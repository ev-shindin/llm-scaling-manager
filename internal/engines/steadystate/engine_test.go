/*
Copyright 2025.

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

package steadystate

import (
	"context"
	"fmt"
	"strings"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	utils "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// newManagedScaledObject builds a KEDA ScaledObject in the default namespace
// targeting the given Deployment, carrying WVA's configuration in its trigger
// metadata. It bears no
// llm-d.ai/* annotations: those are gone, and a workload becomes WVA's by KEDA
// calling the external scaler about it (see wvaDiscovery).
func newManagedScaledObject(name, targetDeployment, modelID string) *kedav1alpha1.ScaledObject {
	maxReplicas := int32(2)
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       targetDeployment,
			},
			MaxReplicaCount: &maxReplicas,
			// Required by the CRD (minItems: 1), and WVA's whole configuration
			// surface. KEDA delivers this verbatim as
			// scalerMetadata on every external-scaler call, and that call is what
			// registers the workload — see registerScaledObjects.
			Triggers: []kedav1alpha1.ScaleTriggers{{
				Type: "external-push",
				Name: "wva-external-scaler",
				Metadata: map[string]string{
					"scalerAddress":         "wva-external-scaler.wva-system.svc:9090",
					registry.ModelIDKey:     modelID,
					registry.VariantCostKey: "10.0",
				},
			}},
		},
	}
}

// wvaDiscovery stands in for KEDA calling WVA's external scaler about every
// ScaledObject in the default namespace, where every spec here creates them,
// and returns the registry plus an enricher that resolves each one against the
// live test API server — the same pair the engine is wired with in production.
func wvaDiscovery(ctx context.Context) (*registry.Registry, *registry.Enricher) {
	GinkgoHelper()
	reg := registry.New(0)

	var soList kedav1alpha1.ScaledObjectList
	Expect(k8sClient.List(ctx, &soList, client.InNamespace("default"))).To(Succeed())
	for i := range soList.Items {
		so := &soList.Items[i]
		if len(so.Spec.Triggers) == 0 {
			continue
		}
		reg.Observe(so.Namespace, so.Name, so.Spec.Triggers[0].Metadata)
	}

	enricher := registry.NewEnricher(k8sClient, reg, 0)
	enricher.Refresh(ctx)
	return reg, enricher
}

var _ = Describe("Saturation Engine", func() {

	// Use config.SystemNamespace() instead of local function

	// CreateServiceClassConfigMap creates a service class ConfigMap for testing
	var CreateServiceClassConfigMap = func(controllerNamespace string, models ...string) *v1.ConfigMap {
		data := map[string]string{}

		// Build premium.yaml with all models
		var premiumBuilder, freemiumBuilder strings.Builder

		for _, model := range models {
			fmt.Fprintf(&premiumBuilder, "  - model: %s\n    slo-tpot: 24\n    slo-ttft: 500\n", model)
			fmt.Fprintf(&freemiumBuilder, "  - model: %s\n    slo-tpot: 200\n    slo-ttft: 2000\n", model)
		}
		premiumModels := premiumBuilder.String()
		freemiumModels := freemiumBuilder.String()

		data["premium.yaml"] = "name: Premium\npriority: 1\ndata:\n" + premiumModels

		data["freemium.yaml"] = "name: Freemium\npriority: 10\ndata:\n" + freemiumModels

		return &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "service-classes-config",
				Namespace: controllerNamespace,
			},
			Data: data,
		}
	}

	Context("When handling multiple VariantAutoscalings", func() {
		const totalVAs = 3
		const configMapName = "wva-manager-config"
		var configMapNamespace = config.SystemNamespace()

		BeforeEach(func() {
			logging.NewTestLogger()

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			// Use custom configmap creation function
			modelNames := make([]string, 0, totalVAs)
			for i := range totalVAs {
				modelNames = append(modelNames, fmt.Sprintf("model-%d-model-%d", i, i))
			}
			configMap := CreateServiceClassConfigMap(ns.Name, modelNames...)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("Creating VariantAutoscaling resources and Deployments")
			for i := range totalVAs {
				modelID := fmt.Sprintf("model-%d-model-%d", i, i)
				name := fmt.Sprintf("multi-test-resource-%d", i)

				d := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: utils.Ptr(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": name},
						},
						Template: v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": name},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:  "test-container",
										Image: "registry.k8s.io/pause:3.9",
										Ports: []v1.ContainerPort{{ContainerPort: 80}},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, d)).To(Succeed())

				// Variants are discovered from the ScaledObject KEDA calls about.
				Expect(k8sClient.Create(ctx, newManagedScaledObject(name, name, modelID))).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			var soList kedav1alpha1.ScaledObjectList
			err = k8sClient.List(ctx, &soList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list ScaledObjects")

			var deploymentList appsv1.DeploymentList
			err = k8sClient.List(ctx, &deploymentList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

			// Clean up all deployments
			for i := range deploymentList.Items {
				deployment := &deploymentList.Items[i]
				if strings.HasPrefix(deployment.Spec.Template.Labels["app"], "multi-test-resource") {
					err = k8sClient.Delete(ctx, deployment)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete deployment")
				}
			}

			// Clean up all managed ScaledObjects
			for i := range soList.Items {
				err = k8sClient.Delete(ctx, &soList.Items[i])
				Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete ScaledObject")
			}
		})

		It("should run the optimization loop over annotation-discovered variants", func() {
			By("Using a working mock Prometheus API with sample data")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Add default responses for common queries
				},
				QueryErrors: map[string]error{},
			}

			// Initialize MetricsCollector with mock Prometheus API
			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck
			// Create minimal test config with saturation config
			testConfig := config.NewTestConfig()
			testConfig.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
				"default": {},
			})
			fakeRecorder := record.NewFakeRecorder(100)
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), fakeRecorder, sourceRegistry, testConfig, allocation.NewNoOpLimiter("test"))
			engine.Variants, engine.VariantEnricher = wvaDiscovery(ctx)

			By("Performing optimization loop")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Prometheus was queried for the discovered variants")
			// Each annotated HPA is synthesized into an in-memory variant; the engine
			// queries Prometheus for saturation metrics for each. Non-empty QueryCallCounts
			// proves the variants were discovered and fed through the saturation pipeline.
			Expect(mockPromAPI.QueryCallCounts).NotTo(BeEmpty(),
				"engine should have queried Prometheus for at least one discovered variant")
		})

		It("stamps every variant that exists, active or parked, before the sweep", func() {
			// Analyze stamps only what it analyses, and only after the sweep, so
			// the engine stamps every variant that still exists first:
			//  - ACTIVE: the three models from BeforeEach. Stamped before the
			//    sweep, a fleet waking after a park longer than the timeout keeps
			//    its state on the cycle it wakes. They also keep optimize from
			//    returning early on "No active VariantAutoscalings".
			//  - PARKED: a Deployment that wants zero replicas is inactive and in
			//    no model group, so no Analyze call would ever stamp it.
			const parkedName, parkedModel = "multi-test-resource-parked", "parked-model"
			parked := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: parkedName, Namespace: "default"},
				Spec: appsv1.DeploymentSpec{
					Replicas: utils.Ptr(int32(0)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": parkedName}},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": parkedName}},
						Spec: v1.PodSpec{Containers: []v1.Container{{
							Name: "test-container", Image: "registry.k8s.io/pause:3.9",
						}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, parked)).To(Succeed())
			Expect(k8sClient.Create(ctx, newManagedScaledObject(parkedName, parkedName, parkedModel))).To(Succeed())

			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{},
				QueryErrors:  map[string]error{},
			}
			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck
			testConfig := config.NewTestConfig()
			testConfig.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
				"default": {},
			})
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), record.NewFakeRecorder(100), sourceRegistry, testConfig, allocation.NewNoOpLimiter("test"))
			engine.Variants, engine.VariantEnricher = wvaDiscovery(ctx)
			spy := &presenceSpy{
				SaturationAnalyzer: saturation.NewSaturationAnalyzer(capacity.NewStore()),
				noted:              map[string][]string{},
			}
			engine.setSaturation(spy)

			Expect(engine.optimize(ctx)).To(Succeed())

			for i := range totalVAs {
				modelID := fmt.Sprintf("model-%d-model-%d", i, i)
				Expect(spy.noted).To(HaveKeyWithValue("default/"+modelID,
					ConsistOf(fmt.Sprintf("multi-test-resource-%d", i))),
					"an active model was not stamped as present")
			}
			Expect(spy.noted).To(HaveKeyWithValue("default/"+parkedModel, ConsistOf(parkedName)),
				"a model parked at zero was not stamped as present")
			Expect(spy.calls).To(ContainElement("sweep"), "precondition: the cycle swept")
			last := len(spy.calls) - 1
			for i, c := range spy.calls {
				if c == "note" {
					last = i
				}
			}
			Expect(spy.calls[last+1:]).To(ContainElement("sweep"),
				"a stamp landed after the sweep, so the sweep judged a variant on its stale stamp")
		})
	})

	Context("Source Infrastructure Optimization Tests", func() {
		const totalVAs = 3
		const configMapName = "wva-manager-config"
		var configMapNamespace = config.SystemNamespace()
		var sourceRegistry *source.SourceRegistry
		var mockPromAPI *testutils.MockPromAPI

		BeforeEach(func() {
			logging.NewTestLogger()

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("Using a working mock Prometheus API with no data")
			mockPromAPI = &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Add default responses for common queries
				},
				QueryErrors: map[string]error{},
			}

			By("creating the source registry with mock Prometheus API")
			sourceRegistry = source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck

			By("creating the required configmaps")
			// Use custom configmap creation function
			modelNames := make([]string, 0, totalVAs)
			for i := range totalVAs {
				modelNames = append(modelNames, fmt.Sprintf("v2-model-%d-model-%d", i, i))
			}
			configMap := CreateServiceClassConfigMap(ns.Name, modelNames...)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("Creating VariantAutoscaling resources and Deployments for source infrastructure tests")
			for i := range totalVAs {
				modelID := fmt.Sprintf("v2-model-%d-model-%d", i, i)
				name := fmt.Sprintf("v2-test-resource-%d", i)

				d := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: utils.Ptr(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": name},
						},
						Template: v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": name},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:  "test-container",
										Image: "registry.k8s.io/pause:3.9",
										Ports: []v1.ContainerPort{{ContainerPort: 80}},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, d)).To(Succeed())

				// Variants are discovered from the ScaledObject KEDA calls about.
				Expect(k8sClient.Create(ctx, newManagedScaledObject(name, name, modelID))).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			var soList kedav1alpha1.ScaledObjectList
			err = k8sClient.List(ctx, &soList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list ScaledObjects")

			var deploymentList appsv1.DeploymentList
			err = k8sClient.List(ctx, &deploymentList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

			// Clean up all deployments created by v2 tests
			for i := range deploymentList.Items {
				deployment := &deploymentList.Items[i]
				if strings.HasPrefix(deployment.Spec.Template.Labels["app"], "v2-test-resource") {
					err = k8sClient.Delete(ctx, deployment)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete deployment")
				}
			}

			// Clean up all managed ScaledObjects created by v2 tests
			for i := range soList.Items {
				so := &soList.Items[i]
				if strings.HasPrefix(so.Name, "v2-test-resource") {
					err = k8sClient.Delete(ctx, so)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete ScaledObject")
				}
			}

		})

		It("should successfully run optimization with source infrastructure", func() {

			// Initialize legacy MetricsCollector for non-saturation metrics
			// Create minimal test config with saturation config
			testConfig := config.NewTestConfig()
			testConfig.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
				"default": {},
			})
			fakeRecorder := record.NewFakeRecorder(100)
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), fakeRecorder, sourceRegistry, testConfig, allocation.NewNoOpLimiter("test"))
			engine.Variants, engine.VariantEnricher = wvaDiscovery(ctx)

			By("Performing optimization loop with source infrastructure")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the annotated variants were discovered")
			// Variants are synthesized from the annotated ScaledObjects; confirm all
			// test objects are present so the loop had the expected discovery surface.
			var soList kedav1alpha1.ScaledObjectList
			err = k8sClient.List(ctx, &soList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())

			testVariants := 0
			for _, so := range soList.Items {
				if strings.HasPrefix(so.Name, "v2-test-resource") && so.DeletionTimestamp.IsZero() {
					testVariants++
				}
			}
			Expect(testVariants).To(Equal(totalVAs), "Expected all test variants to be present")
		})

	})

	Context("Optimizer selection based on the declared limiters", func() {
		const testName = "optimizer-toggle-test"
		const modelID = "test-model"

		BeforeEach(func() {
			logging.NewTestLogger()

			By("Creating test deployment and VA")
			d := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: utils.Ptr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": testName},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": testName},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "test-container",
									Image: "registry.k8s.io/pause:3.9",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, d)).To(Succeed())

			// Variant is discovered from an annotated HPA targeting the Deployment.
			Expect(k8sClient.Create(ctx, newManagedScaledObject(testName, testName, modelID))).To(Succeed())
		})

		AfterEach(func() {
			By("Cleaning up test resources")
			d := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: "default",
				},
			}
			err := k8sClient.Delete(ctx, d)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			so := &kedav1alpha1.ScaledObject{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: "default",
				},
			}
			err = k8sClient.Delete(ctx, so)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should update e.optimizer when the declared limiters change", func() {
			By("Creating engine with no limiters declared")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{},
				QueryErrors:  map[string]error{},
			}
			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck

			// The declared limiters are now the whole answer: a declared limiter IS
			// the request to be limited. There is no enable flag that could leave a
			// declared limiter inert, which is what let a quota be configured and
			// silently not enforced.
			testConfig := config.NewTestConfig()
			withLimiters := func(limiters ...config.QuotaLimiterConfig) {
				testConfig.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
					"default": {
						AnalyzerName: domain.SaturationAnalyzerName, // V2 path
						Limiters:     limiters,
					},
				})
			}
			withLimiters()
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig, allocation.NewNoOpLimiter("test"))
			engine.Variants, engine.VariantEnricher = wvaDiscovery(ctx)

			By("Running optimize() with no limiters")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(engine.optimizer.Name()).To(Equal("cost-aware"),
				"Expected CostAwareOptimizer when no limiter is declared")

			By("Declaring a gpu-inventory limiter")
			withLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"})

			err = engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(engine.optimizer.Name()).To(Equal("greedy-by-score"),
				"Expected GreedyByScoreOptimizer once a limiter is declared")

			By("Declaring a quota limiter — also limited, with no second flag to set")
			withLimiters(config.QuotaLimiterConfig{
				Name: "q", Type: "quota", Scope: config.QuotaScopeCluster,
				ClusterQuotas: map[string]int{"A100": 4},
			})

			err = engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(engine.optimizer.Name()).To(Equal("greedy-by-score"),
				"a declared quota must be enforced without a separate enable flag")

			By("Removing the limiters again")
			withLimiters()

			err = engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(engine.optimizer.Name()).To(Equal("cost-aware"),
				"Expected CostAwareOptimizer when the limiters are removed")
		})
	})

})

// presenceSpy is the real saturation analyzer with NoteVariantsPresent and the
// sweep recorded, so a spec can see which variants the engine stamped, and
// whether before the sweep, without reaching into the analyzer's unexported
// maps. noted is keyed namespace/model; calls is the order of both.
type presenceSpy struct {
	*saturation.SaturationAnalyzer
	noted map[string][]string
	calls []string
}

func (s *presenceSpy) NoteVariantsPresent(namespace, modelID string, variantNames []string) {
	s.noted[namespace+"/"+modelID] = append(s.noted[namespace+"/"+modelID], variantNames...)
	s.calls = append(s.calls, "note")
	s.SaturationAnalyzer.NoteVariantsPresent(namespace, modelID, variantNames)
}

func (s *presenceSpy) EvictStaleHistory() saturation.Evicted {
	s.calls = append(s.calls, "sweep")
	return s.SaturationAnalyzer.EvictStaleHistory()
}
