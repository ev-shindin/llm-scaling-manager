/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalefromzero

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	appsV1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/pod"
	poolreconciler "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	vav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	unittestutil "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	utiltest "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
)

var (
	selector_v1     = map[string]string{"app": "vllm_v1"}
	namespace       = "pool1-ns"
	resourceName    = "resource-name"
	deploymentName  = "deployment-name"
	acceleratorName = "A100"
	modelId         = "unsloth/Meta-Llama-3.1-8B"
	variantCost     = float64(5)
)

// managedSO builds a KEDA ScaledObject targeting the given Deployment, carrying
// WVA's configuration in its trigger metadata. It bears no llm-d.ai/* annotations:
// those are gone, and being managed is decided by KEDA calling the external
// scaler, which registerSO below stands in for.
func managedSO(ns, name, targetDeployment, modelID string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       targetDeployment,
			},
			MinReplicaCount: ptrInt32(0),
			MaxReplicaCount: ptrInt32(2),
			Triggers: []kedav1alpha1.ScaleTriggers{{
				Type: "external-push",
				Name: "wva-external-scaler",
				Metadata: map[string]string{
					registry.ModelIDKey:     modelID,
					registry.VariantCostKey: "5.0",
				},
			}},
		},
	}
}

// registerSO stands in for KEDA calling WVA's external scaler about the object,
// which is what makes a workload WVA's. Enrichment is supplied directly rather
// than read, so these tests need no live API for the ScaledObject.
func registerSO(reg *registry.Registry, so *kedav1alpha1.ScaledObject) {
	reg.Observe(so.Namespace, so.Name, so.Spec.Triggers[0].Metadata)
	reg.SetTarget(so.Namespace, so.Name, registry.Target{
		APIVersion:  so.Spec.ScaleTargetRef.APIVersion,
		Kind:        so.Spec.ScaleTargetRef.Kind,
		Name:        so.Spec.ScaleTargetRef.Name,
		MinReplicas: so.Spec.MinReplicaCount,
		MaxReplicas: so.Spec.MaxReplicaCount,
	})
}

func ptrInt32(i int32) *int32 { return &i }

func TestSingleInactiveVariant(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeInferencePool("pool1").
		Namespace(namespace).
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef("epp-pool1-svc").ObjRef()
	pool1.SetGroupVersionKind(gvk)

	tests := []struct {
		name             string
		pool             *v1.InferencePool
		resourceReplicas int32
		labels           map[string]string
		datastoreSize    int
		wantErr          bool
	}{
		{
			name:             "one inactiveVariant: successful scalefromzero optimization",
			pool:             pool1,
			labels:           map[string]string{"app": "vllm_v1"},
			datastoreSize:    1,
			resourceReplicas: 0,
		},
		{
			name:             "zero inactiveVariant: skipped scalefromzero optimization",
			pool:             pool1,
			labels:           map[string]string{"app": "vllm_v1"},
			datastoreSize:    1,
			resourceReplicas: 1,
		},
		{
			name:             "Skip variant when labels of inferencePool and deployment don't match",
			pool:             pool1,
			labels:           map[string]string{"vllm": "test"},
			datastoreSize:    1,
			resourceReplicas: 0,
			wantErr:          false, // Changed: now returns nil to avoid blocking retry loop
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool1 := tt.pool

			va := unittestutil.CreateVariantAutoscalingResource(namespace, resourceName, deploymentName, modelId, acceleratorName, variantCost)
			dp := unittestutil.MakeDeployment(deploymentName, namespace, tt.resourceReplicas, tt.labels)
			svc := unittestutil.MakeService("epp-pool1-svc", namespace)

			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1alpha2.Install(scheme)
			_ = v1.Install(scheme)
			_ = vav1alpha1.AddToScheme(scheme)
			_ = kedav1alpha1.AddToScheme(scheme)
			_ = appsV1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)
			fakeClientInitialObjs := []client.Object{pool1, dp, va, svc}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(fakeClientInitialObjs...).
				Build()

			// Create a request for the existing resource.
			namespacedName := types.NamespacedName{Name: pool1.Name, Namespace: pool1.Namespace}
			gknn := common.GKNN{
				NamespacedName: namespacedName,
				GroupKind: schema.GroupKind{
					Group: pool1.GroupVersionKind().Group,
					Kind:  pool1.GroupVersionKind().Kind,
				},
			}

			req := ctrl.Request{NamespacedName: namespacedName}
			ctx := context.Background()

			ds := datastore.NewDatastore(nil)
			inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

			// (1) Reconcile inferencePool and store generated endpointPool in the datastore
			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Errorf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Check the size of the datastore
			assert.Equal(t, len(ds.PoolList()), tt.datastoreSize, "There should be one EndpointPool in the datastore")

			// (2) Create scalefromzero engine loop
			fakeRecorder := record.NewFakeRecorder(100)

			engine := &Engine{
				client:         fakeClient,
				executor:       nil,
				recorder:       fakeRecorder,
				Datastore:      ds,
				maxConcurrency: 30,
			}

			// Call the optimize function.
			err := engine.optimize(ctx)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMultipleInactiveVariants(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeInferencePool("pool1").
		Namespace(namespace).
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef("epp-pool1-svc").ObjRef()
	pool1.SetGroupVersionKind(gvk)

	// Variants are discovered from annotated HPAs targeting each Deployment.
	so1 := managedSO(namespace, "resource-1", "resource-1-deployment", "model-1")
	so2 := managedSO(namespace, "resource-2", "resource-2-deployment", "model-2")
	so3 := managedSO(namespace, "resource-3", "resource-3-deployment", "model-3")

	dp1 := unittestutil.MakeDeployment("resource-1-deployment", namespace, 0, selector_v1)
	dp2 := unittestutil.MakeDeployment("resource-2-deployment", namespace, 0, selector_v1)
	dp3 := unittestutil.MakeDeployment("resource-3-deployment", namespace, 0, selector_v1)
	svc := unittestutil.MakeService("epp-pool1-svc", namespace)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha2.Install(scheme)
	_ = v1.Install(scheme)
	_ = vav1alpha1.AddToScheme(scheme)
	_ = kedav1alpha1.AddToScheme(scheme)
	_ = appsV1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClientInitialObjs := []client.Object{pool1, dp1, dp2, dp3, so1, so2, so3, svc}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fakeClientInitialObjs...).
		Build()

	namespacedName := types.NamespacedName{Name: pool1.Name, Namespace: pool1.Namespace}
	gknn := common.GKNN{
		NamespacedName: namespacedName,
		GroupKind: schema.GroupKind{
			Group: pool1.GroupVersionKind().Group,
			Kind:  pool1.GroupVersionKind().Kind,
		},
	}

	req := ctrl.Request{NamespacedName: namespacedName}
	ctx := context.Background()

	ds := datastore.NewDatastore(nil)
	inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

	if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
		t.Errorf("Unexpected InferencePool reconcile error: %v", err)
	}

	fakeRecorder := record.NewFakeRecorder(100)

	// KEDA calling about each ScaledObject is what makes it WVA's.
	reg := registry.New(0)
	for _, so := range []*kedav1alpha1.ScaledObject{so1, so2, so3} {
		registerSO(reg, so)
	}

	engine := &Engine{
		client:         fakeClient,
		executor:       nil,
		recorder:       fakeRecorder,
		Datastore:      ds,
		maxConcurrency: 30,
		Variants:       reg,
	}

	// Get all inactive VAs
	inactiveVAs, scaleTargets, err := utils.InactiveVariantAutoscaling(ctx, fakeClient, reg)
	require.NoError(t, err)
	assert.Equal(t, 3, len(inactiveVAs), "Should have 3 inactive VAs")

	// Verify scale targets map is populated correctly
	assert.NotNil(t, scaleTargets, "ScaleTargets map should not be nil")
	assert.Equal(t, 3, len(scaleTargets), "Should have 3 scale targets in the map")

	// Verify each scale target is keyed by namespace/deploymentName
	expectedScaleTargets := []string{
		namespace + "/resource-1-deployment",
		namespace + "/resource-2-deployment",
		namespace + "/resource-3-deployment",
	}
	for _, expectedKey := range expectedScaleTargets {
		scaleTarget, found := scaleTargets[expectedKey]
		assert.True(t, found, "ScaleTarget with key %s should be in the map", expectedKey)
		assert.NotNil(t, scaleTarget, "ScaleTarget should not be nil for key %s", expectedKey)
		if scaleTarget != nil {
			assert.Equal(t, namespace, scaleTarget.GetNamespace(), "ScaleTarget namespace should match")
			assert.Equal(t, int32(0), *scaleTarget.GetReplicas(), "ScaleTarget should have 0 replicas (inactive)")
		}
	}

	// Run optimize - it should handle multiple VAs concurrently
	err = engine.optimize(ctx)
	// The EPP Service selects no pod, so the queue could not be read: that is
	// an error for each model, not an empty queue. This used to assert no
	// error, which was the answer that kept the Prometheus fallback off.
	require.ErrorIs(t, err, pod.ErrNoReadyPods)
}

func TestEmptyInactiveVariants(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool1 := utiltest.MakeInferencePool("pool1").
		Namespace(namespace).
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef("epp-pool1-svc").ObjRef()
	pool1.SetGroupVersionKind(gvk)

	// Create VA with non-zero replicas (active)
	va := unittestutil.CreateVariantAutoscalingResource(namespace, resourceName, resourceName+"-deployment", modelId, acceleratorName, variantCost)
	dp := unittestutil.MakeDeployment(resourceName+"-deployment", namespace, 1, selector_v1) // 1 replica = active
	svc := unittestutil.MakeService("epp-pool1-svc", namespace)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha2.Install(scheme)
	_ = v1.Install(scheme)
	_ = vav1alpha1.AddToScheme(scheme)
	_ = kedav1alpha1.AddToScheme(scheme)
	_ = appsV1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClientInitialObjs := []client.Object{pool1, dp, va, svc}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fakeClientInitialObjs...).
		Build()

	namespacedName := types.NamespacedName{Name: pool1.Name, Namespace: pool1.Namespace}
	gknn := common.GKNN{
		NamespacedName: namespacedName,
		GroupKind: schema.GroupKind{
			Group: pool1.GroupVersionKind().Group,
			Kind:  pool1.GroupVersionKind().Kind,
		},
	}

	req := ctrl.Request{NamespacedName: namespacedName}
	ctx := context.Background()

	ds := datastore.NewDatastore(nil)
	inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{Client: fakeClient, Datastore: ds, PoolGKNN: gknn}

	if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
		t.Errorf("Unexpected InferencePool reconcile error: %v", err)
	}

	fakeRecorder := record.NewFakeRecorder(100)

	engine := &Engine{
		client:         fakeClient,
		executor:       nil,
		recorder:       fakeRecorder,
		Datastore:      ds,
		maxConcurrency: 30,
	}

	// Should complete without error when no inactive VAs exist
	err := engine.optimize(ctx)
	assert.NoError(t, err, "Should not error when no inactive VAs exist")
}

func TestNamespacedMetricsSourceLookup(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}

	tests := []struct {
		name          string
		poolNamespace string
		poolName      string
		vaNamespace   string
		expectSkip    bool
		skipReason    string
	}{
		{
			name:          "pool found in same namespace - processes variant",
			poolNamespace: "test-ns",
			poolName:      "pool1",
			vaNamespace:   "test-ns",
			expectSkip:    false,
		},
		{
			name:          "pool not found in different namespace - skips gracefully",
			poolNamespace: "pool-ns",
			poolName:      "pool1",
			vaNamespace:   "different-ns",
			expectSkip:    true,
			skipReason:    "pool not found in VA's namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create pool in its namespace
			pool := utiltest.MakeInferencePool(tt.poolName).
				Namespace(tt.poolNamespace).
				Selector(selector_v1).
				TargetPorts(8080).
				EndpointPickerRef("epp-svc").ObjRef()
			pool.SetGroupVersionKind(gvk)

			// Create VA in its namespace (might be different)
			va := unittestutil.CreateVariantAutoscalingResource(
				tt.vaNamespace,
				resourceName,
				deploymentName,
				modelId,
				acceleratorName,
				variantCost,
			)

			// Create deployment with matching labels in VA's namespace
			dp := unittestutil.MakeDeployment(deploymentName, tt.vaNamespace, 0, selector_v1)

			// Create service in pool's namespace
			svc := unittestutil.MakeService("epp-svc", tt.poolNamespace)

			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1alpha2.Install(scheme)
			_ = v1.Install(scheme)
			_ = vav1alpha1.AddToScheme(scheme)
			_ = kedav1alpha1.AddToScheme(scheme)
			_ = appsV1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			fakeClientInitialObjs := []client.Object{pool, dp, va, svc}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(fakeClientInitialObjs...).
				Build()

			ctx := context.Background()
			ds := datastore.NewDatastore(nil)

			// Reconcile the pool to add it to datastore
			namespacedName := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
			gknn := common.GKNN{
				NamespacedName: namespacedName,
				GroupKind: schema.GroupKind{
					Group: pool.GroupVersionKind().Group,
					Kind:  pool.GroupVersionKind().Kind,
				},
			}
			req := ctrl.Request{NamespacedName: namespacedName}
			inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{
				Client:    fakeClient,
				Datastore: ds,
				PoolGKNN:  gknn,
			}

			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Fatalf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Verify pool is in datastore
			poolList := ds.PoolList()
			require.Equal(t, 1, len(poolList), "Pool should be in datastore")

			// Verify metrics source is registered under the namespaced key (namespace/name)
			// This is critical for processInactiveVariant which calls PoolGetMetricsSource(namespace/name)
			namespacedPoolName := tt.poolNamespace + "/" + tt.poolName
			metricsSource := ds.PoolGetMetricsSource(namespacedPoolName)
			if tt.expectSkip {
				// For cross-namespace test, metrics source won't be found (different namespace)
				// This is expected and will be handled gracefully
			} else {
				// For same-namespace test, metrics source must be registered
				require.NotNil(t, metricsSource, "Metrics source should be registered under namespaced key %s", namespacedPoolName)
			}

			fakeRecorder := record.NewFakeRecorder(100)

			engine := &Engine{
				client:         fakeClient,
				executor:       nil,
				recorder:       fakeRecorder,
				Datastore:      ds,
				maxConcurrency: 30,
			}

			scaleTargets := map[string]scaletarget.ScaleTargetAccessor{
				tt.vaNamespace + "/" + deploymentName: scaletarget.NewDeploymentAccessor(dp),
			}

			// Resolve the variant into a selection candidate. This is the step
			// that looks the EPP pool up from the scale target's pod labels, so
			// it is where a namespace mismatch shows up: a variant whose pool
			// does not resolve is dropped from the candidates rather than
			// failing the model.
			group := modelGroup{namespace: tt.vaNamespace, modelID: modelId, variants: []vav1alpha1.VariantAutoscaling{*va}}
			candidates, resolvedPool, err := engine.buildCandidates(ctx, scaleTargets, group)

			if tt.expectSkip {
				// When pool is not found (different namespace), we expect nil error (skip)
				assert.NoError(t, err, "Expected no error (skip) for: %s, but got: %v", tt.skipReason, err)
				assert.Empty(t, candidates, "Variant should be skipped when its pool is in another namespace: %s", tt.skipReason)
				assert.Nil(t, resolvedPool)
			} else {
				if err != nil && errors.Is(err, datastore.ErrPoolNotSynced) {
					// When pool is found, we expect it to proceed
					t.Errorf("Should have found pool in same namespace, but got: %v", err)
				}
				assert.NoError(t, err)
				assert.Len(t, candidates, 1, "Variant in the pool's namespace should become a candidate")
				assert.NotNil(t, resolvedPool, "Pool should resolve for a same-namespace variant")
			}
		})
	}
}

func TestPoolGetFromLabelsWithNamespace(t *testing.T) {
	tests := []struct {
		name          string
		poolNamespace string
		poolName      string
		poolSelector  map[string]string
		queryNs       string
		queryLabels   map[string]string
		expectFound   bool
	}{
		{
			name:          "finds pool in same namespace with matching labels",
			poolNamespace: "ns1",
			poolName:      "pool1",
			poolSelector:  map[string]string{"app": "test"},
			queryNs:       "ns1",
			queryLabels:   map[string]string{"app": "test", "version": "v1"},
			expectFound:   true,
		},
		{
			name:          "does not find pool in different namespace even with matching labels",
			poolNamespace: "ns1",
			poolName:      "pool1",
			poolSelector:  map[string]string{"app": "test"},
			queryNs:       "ns2",
			queryLabels:   map[string]string{"app": "test", "version": "v1"},
			expectFound:   false,
		},
		{
			name:          "does not find pool with non-matching labels in same namespace",
			poolNamespace: "ns1",
			poolName:      "pool1",
			poolSelector:  map[string]string{"app": "test"},
			queryNs:       "ns1",
			queryLabels:   map[string]string{"app": "different"},
			expectFound:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gvk := schema.GroupVersionKind{
				Group:   v1.GroupVersion.Group,
				Version: v1.GroupVersion.Version,
				Kind:    "InferencePool",
			}

			pool := utiltest.MakeInferencePool(tt.poolName).
				Namespace(tt.poolNamespace).
				Selector(tt.poolSelector).
				TargetPorts(8080).
				EndpointPickerRef("epp-svc").ObjRef()
			pool.SetGroupVersionKind(gvk)

			svc := unittestutil.MakeService("epp-svc", tt.poolNamespace)

			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1alpha2.Install(scheme)
			_ = v1.Install(scheme)
			_ = vav1alpha1.AddToScheme(scheme)
			_ = kedav1alpha1.AddToScheme(scheme)
			_ = corev1.AddToScheme(scheme)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(pool, svc).
				Build()

			ctx := context.Background()
			ds := datastore.NewDatastore(nil)

			// Reconcile pool to add to datastore
			namespacedName := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
			gknn := common.GKNN{
				NamespacedName: namespacedName,
				GroupKind: schema.GroupKind{
					Group: pool.GroupVersionKind().Group,
					Kind:  pool.GroupVersionKind().Kind,
				},
			}
			req := ctrl.Request{NamespacedName: namespacedName}
			inferencePoolReconciler := &poolreconciler.InferencePoolReconciler{
				Client:    fakeClient,
				Datastore: ds,
				PoolGKNN:  gknn,
			}

			if _, err := inferencePoolReconciler.Reconcile(ctx, req); err != nil {
				t.Fatalf("Unexpected InferencePool reconcile error: %v", err)
			}

			// Query with namespace
			foundPool, err := ds.PoolGetFromLabels(tt.queryNs, tt.queryLabels)

			if tt.expectFound {
				require.NoError(t, err, "Expected to find pool but got error")
				require.NotNil(t, foundPool, "Expected to find pool but got nil")
				assert.Equal(t, tt.poolNamespace, foundPool.Namespace, "Pool namespace should match")
				assert.Equal(t, tt.poolName, foundPool.Name, "Pool name should match")
			} else {
				require.Error(t, err, "Expected error (not found) but got none")
				assert.ErrorIs(t, err, datastore.ErrPoolNotSynced, "Error should be ErrPoolNotSynced")
				assert.Nil(t, foundPool, "Pool should be nil when not found")
			}
		})
	}
}
