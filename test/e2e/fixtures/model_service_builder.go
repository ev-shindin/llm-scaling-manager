package fixtures

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
)

// ModelServiceOption adjusts a model-service Deployment before it is applied.
type ModelServiceOption func(*appsv1.Deployment)

// WithRole stamps the P/D disaggregation role on the pod template, which is
// where WVA reads it from (variantmeta.RoleFromScaleTarget). Use
// domain.RolePrefill / domain.RoleDecode; an unlabelled workload reads as
// "both", i.e. non-disaggregated.
func WithRole(role string) ModelServiceOption {
	return func(d *appsv1.Deployment) {
		if d.Spec.Template.Labels == nil {
			d.Spec.Template.Labels = map[string]string{}
		}
		d.Spec.Template.Labels[variantmeta.RoleLabel] = role
	}
}

// WithAcceleratorNodeSelector pins the workload to a GPU product, which is what
// makes its accelerator RESOLVABLE. Without it, WVA cannot attribute the
// workload's GPUs to any pool: its demand is not charged and its usage is not
// counted, so the capacity check has nothing to decide on.
//
// productName is the node's `nvidia.com/gpu.product` value, e.g.
// "NVIDIA-A100-PCIE-80GB".
func WithAcceleratorNodeSelector(productName string) ModelServiceOption {
	return WithAcceleratorNodeSelectorKV("nvidia.com/gpu.product", productName)
}

// WithAcceleratorNodeSelectorKV pins on a given product LABEL KEY, because the
// key is vendor-specific: an emulated cluster may label its nodes
// amd.com/gpu.product or habana.ai/gaudi.product, and a selector on
// nvidia.com/gpu.product matches nothing there -- the pods stay Pending and the
// spec times out somewhere unrelated. Pair with DiscoverAcceleratorProduct.
func WithAcceleratorNodeSelectorKV(key, productName string) ModelServiceOption {
	return func(d *appsv1.Deployment) {
		if d.Spec.Template.Spec.NodeSelector == nil {
			d.Spec.Template.Spec.NodeSelector = map[string]string{}
		}
		d.Spec.Template.Spec.NodeSelector[key] = productName
	}
}

// AllocatableGPUsForProduct reports how many GPUs a single schedulable node
// carrying the given product advertises, or 0 if no such node exists.
//
// For a spec that fills an accelerator: the number has to come from the cluster,
// not from a constant that happens to match today's CLUSTER_GPUS. Reads the
// largest single node rather than a sum, because filling is per-node -- a pod
// cannot straddle two.
func AllocatableGPUsForProduct(ctx context.Context, k8sClient *kubernetes.Clientset, product string) int {
	_, gpus := LargestNodeForProduct(ctx, k8sClient, product)
	return gpus
}

// LargestNodeForProduct is the schedulable node carrying product that
// advertises the most GPUs, and that number; "" and 0 when there is none. A
// cordoned node, or one with a NoSchedule or NoExecute taint, is not
// schedulable: a spec that pins pods to it would leave them Pending.
func LargestNodeForProduct(ctx context.Context, k8sClient *kubernetes.Clientset, product string) (string, int) {
	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", 0
	}
	bestNode, best := "", 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		schedulable := !node.Spec.Unschedulable
		for _, t := range node.Spec.Taints {
			if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
				schedulable = false
				break
			}
		}
		if !schedulable {
			continue
		}
		for _, vendor := range constants.VendorResources {
			labels := append([]string{vendor.ProductLabel}, vendor.ProductLabelAliases...)
			matched := false
			for _, k := range labels {
				if node.Labels[k] == product {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			if q, ok := node.Status.Allocatable[corev1.ResourceName(vendor.ResourceName)]; ok {
				if n := int(q.Value()); n > best {
					bestNode, best = node.Name, n
				}
			}
		}
	}
	return bestNode, best
}

// NoAcceleratorPinAnnotation opts a workload OUT of the default pin below.
//
// For the few specs whose subject IS the unresolved path -- a workload that
// constrains no accelerator, whose GPUs are charged to no pool. Nothing sets it
// today; it exists so such a spec does not have to fight the default.
const NoAcceleratorPinAnnotation = "e2e.llm-d.ai/no-accelerator-pin"

// WithoutAcceleratorPin leaves the workload unpinned, so its accelerator resolves
// the way an unconstrained production workload's does.
func WithoutAcceleratorPin() ModelServiceOption {
	return func(d *appsv1.Deployment) {
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[NoAcceleratorPinAnnotation] = "true"
	}
}

// pinToDiscoveredAccelerator gives a pod spec a GPU product nodeSelector unless it
// already has one, and reports what it pinned to.
//
// Applied by default to every model service and warm pool this package creates,
// because the alternative turned out to be a source of failures rather than a
// neutral default. A workload that constrains nothing has its accelerator observed
// from the nodes its pods landed on, and that observation requires agreement: on a
// heterogeneous cluster -- which this suite's kind emulator is -- a scale-up puts
// the second replica on another product, the variant reads unresolved, its k2
// history key moves, capacity jumps, and it scales back down. Pinning is also what
// a real deployment does; WVA's own AcceleratorNotResolved warning asks for it.
//
// Pools are pinned for the matching reason: a pool Pod's accelerator is a property
// of its node, and a borrow requires the pool and the workload to agree. Leaving
// pools to the scheduler while workloads are pinned would break every borrow spec
// on a heterogeneous cluster.
func pinToDiscoveredAccelerator(ctx context.Context, k8sClient *kubernetes.Clientset, podSpec *corev1.PodSpec) {
	for _, k := range accelerator.GetProductKeys() {
		if _, already := podSpec.NodeSelector[k]; already {
			return
		}
	}
	key, product, resourceName, ok := discoverAccelerator(ctx, k8sClient)
	if !ok {
		return
	}
	if podSpec.NodeSelector == nil {
		podSpec.NodeSelector = map[string]string{}
	}
	podSpec.NodeSelector[key] = product

	// And claim one of that vendor's GPUs, unless the caller already asked for
	// some. WVA's physical usage picture is summed from pod GPU REQUESTS
	// (gpunodes.getPodGPURequests, feeding gpuusage.Refresher, whose whole point
	// is "every GPU held on a GPU node, whoever holds it"), so a simulator pod
	// that asks for nothing leaves that picture empty however many replicas run --
	// the placement checks and the inventory limiter were being exercised against
	// a cluster that always looked idle. The managed view (replicas x
	// GPUs-per-replica, from the scaler's metadata) did see them, which is why the
	// two never disagreed loudly enough to notice.
	//
	// The resource comes from the SAME discovery as the nodeSelector for a reason
	// that is not theoretical: they must name one vendor. A spec needing a
	// different count sets WithGPURequest, which is why an existing claim wins.
	res := corev1.ResourceName(resourceName)
	for i := range podSpec.Containers {
		c := &podSpec.Containers[i]
		if _, already := c.Resources.Requests[res]; already {
			continue
		}
		// A LIMIT is a claim too, and pool containers state their devices as one:
		// the count there is the pool spec's, and capacityOf reads it as the warm
		// unit's size. Overwriting it with this default's single device would
		// quietly resize every group the suite builds.
		if _, already := c.Resources.Limits[res]; already {
			continue
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		one := *resource.NewQuantity(1, resource.DecimalSI)
		c.Resources.Requests[res] = one
		c.Resources.Limits[res] = one
	}
}

// DiscoverAcceleratorProduct returns a GPU product label present on a schedulable
// node, so a spec can pin its workload to ONE accelerator.
//
// Why a spec should: a workload that constrains no accelerator has its type
// observed from the nodes its pods landed on, and that observation requires
// agreement -- two pods on two products report "no single answer" and the variant
// reads unresolved. On a heterogeneous emulator (this suite's kind cluster labels
// one node NVIDIA-H100-SXM5-80GB and another NVIDIA-A100-PCIE-80GB) a scale-up is
// therefore enough to un-resolve the accelerator, which is not the condition most
// specs mean to test and which produced a self-sustaining 1<->2 oscillation.
//
// Nodes carrying NoSchedule taints are skipped: kind taints its control-plane,
// and pinning to a product that only exists there parks every replica in Pending.
// Sorted for determinism, so a rerun pins the same way. Returns ok=false on a
// cluster with no product labels at all, where the caller should simply not pin.
func DiscoverAcceleratorProduct(ctx context.Context, k8sClient *kubernetes.Clientset) (key, product string, ok bool) {
	key, product, _, ok = discoverAccelerator(ctx, k8sClient)
	return key, product, ok
}

// DiscoverAccelerator is DiscoverAcceleratorProduct plus the vendor's
// resource name, for a caller that requests the GPUs itself (WithLWSGPUs).
func DiscoverAccelerator(ctx context.Context, k8sClient *kubernetes.Clientset) (key, product, resourceName string, ok bool) {
	return discoverAccelerator(ctx, k8sClient)
}

// discoverAccelerator also reports the vendor's RESOURCE name, which a caller
// needs to ask for one of these GPUs: the product label and the resource are a
// pair (amd.com/gpu.product-name goes with amd.com/gpu), and mixing them across
// vendors yields a Pod that can never be scheduled.
func discoverAccelerator(ctx context.Context, k8sClient *kubernetes.Clientset) (key, product, resourceName string, ok bool) {
	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", "", false
	}
	items := make([]corev1.Node, len(nodes.Items))
	copy(items, nodes.Items)
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	// Chosen by CAPACITY, not by name order. Every workload and pool this package
	// creates lands on the product picked here, so the choice sets the suite's
	// whole GPU budget: taking the first node alphabetically put everything on a
	// 4-GPU control-plane while an equally labelled 4-GPU worker sat unused, which
	// is an arbitrary ceiling rather than a decision. Ties break on the label
	// string so a rerun pins identically.
	type candidate struct {
		key, product, resource string
		gpus                   int64
	}
	totals := map[string]*candidate{}
	for i := range items {
		node := &items[i]
		schedulable := true
		for _, t := range node.Spec.Taints {
			if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
				schedulable = false
				break
			}
		}
		if !schedulable {
			continue
		}
		for _, vendor := range constants.VendorResources {
			labels := append([]string{vendor.ProductLabel}, vendor.ProductLabelAliases...)
			for _, k := range labels {
				v, found := node.Labels[k]
				if !found || v == "" {
					continue
				}
				id := k + "=" + v
				if totals[id] == nil {
					totals[id] = &candidate{key: k, product: v, resource: vendor.ResourceName}
				}
				if q, ok := node.Status.Allocatable[corev1.ResourceName(vendor.ResourceName)]; ok {
					totals[id].gpus += q.Value()
				}
				break
			}
		}
	}

	best := ""
	for id, c := range totals {
		if best == "" {
			best = id
			continue
		}
		if c.gpus > totals[best].gpus || (c.gpus == totals[best].gpus && id < best) {
			best = id
		}
	}
	if best == "" {
		return "", "", "", false
	}
	return totals[best].key, totals[best].product, totals[best].resource, true
}

// WithGPUs requests count GPUs of the named vendor resource on every container,
// as a request and a limit. Pass the resource the accelerator pin discovered
// (DiscoverAccelerator), so it matches the pin's nodeSelector.
func WithGPUs(resourceName string, count int64) ModelServiceOption {
	return func(d *appsv1.Deployment) {
		qty := *resource.NewQuantity(count, resource.DecimalSI)
		for i := range d.Spec.Template.Spec.Containers {
			c := &d.Spec.Template.Spec.Containers[i]
			if c.Resources.Requests == nil {
				c.Resources.Requests = corev1.ResourceList{}
			}
			if c.Resources.Limits == nil {
				c.Resources.Limits = corev1.ResourceList{}
			}
			c.Resources.Requests[corev1.ResourceName(resourceName)] = qty
			c.Resources.Limits[corev1.ResourceName(resourceName)] = qty
		}
	}
}

// WithGPURequest sets the pod's nvidia.com/gpu request/limit, which is how WVA
// derives GPUs-per-replica (scaletarget.GetTotalGPUsPerReplica). On a node of
// another vendor use WithGPUs with the discovered resource name.
func WithGPURequest(count int64) ModelServiceOption {
	return WithGPUs("nvidia.com/gpu", count)
}

// CreateModelService creates the model-server Deployment only (name + "-decode").
// It does not create a Kubernetes Service; callers must use CreateService or EnsureService
// (typically naming the Service name + "-service") to expose the deployment.
// collector can attribute pod metrics to the right annotated scaler. Pass "" to omit
// the label (no WVA-managed scaler targets this deployment).
func CreateModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int) error {
	return CreateModelServiceWithExtraArgs(ctx, k8sClient, namespace, name, poolName, modelID, useSimulator, maxNumSeqs, nil)
}

// CreateModelServiceWithExtraArgs is like CreateModelService but appends additional
// CLI args to the model-server (simulator or vLLM) container. Used by tests that
// need to inject engine-specific configuration such as --fake-metrics.
//
// The caller is responsible for ensuring extraArgs are valid for the chosen
// runtime — this function performs no flag validation. Engine-specific flags
// (e.g., --fake-metrics, which exists only on llm-d-inference-sim) will cause
// the container to crash-loop if passed to the wrong runtime. Tests that use
// simulator-only flags should gate their suite on `cfg.UseSimulator` and Skip
// otherwise.
func CreateModelServiceWithExtraArgs(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int, extraArgs []string, opts ...ModelServiceOption) error {
	deployment := buildModelServiceDeployment(namespace, name, poolName, modelID, useSimulator, maxNumSeqs, extraArgs)
	for _, opt := range opts {
		opt(deployment)
	}
	applyAcceleratorPin(ctx, k8sClient, deployment)
	_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	return err
}

// applyAcceleratorPin pins the Deployment unless it opted out, and removes the
// marker so it does not travel to the cluster as a stray annotation.
func applyAcceleratorPin(ctx context.Context, k8sClient *kubernetes.Clientset, d *appsv1.Deployment) {
	if d.Annotations[NoAcceleratorPinAnnotation] == "true" {
		delete(d.Annotations, NoAcceleratorPinAnnotation)
		return
	}
	pinToDiscoveredAccelerator(ctx, k8sClient, &d.Spec.Template.Spec)
}

// DeleteModelService deletes the model service deployment. Idempotent; ignores NotFound.
func DeleteModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string) error {
	deploymentName := name + decodeNameSuffix
	err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete model service deployment %s: %w", deploymentName, err)
	}
	return nil
}

// EnsureModelService creates or replaces the model-server Deployment only (name + "-decode").
// It does not create a Kubernetes Service; pair with EnsureService for a ClusterIP Service.
// collector can attribute pod metrics to the right annotated scaler. Pass "" to omit.
// Options adjust the built Deployment before it is applied; see WithRole.
func EnsureModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int, opts ...ModelServiceOption) error {
	appLabel := name + decodeNameSuffix
	deploymentName := appLabel
	desiredDeployment := buildModelServiceDeployment(namespace, name, poolName, modelID, useSimulator, maxNumSeqs, nil)
	for _, opt := range opts {
		opt(desiredDeployment)
	}
	applyAcceleratorPin(ctx, k8sClient, desiredDeployment)

	existingDeployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing deployment %s: %w", deploymentName, err)
		}
	} else {
		if existingDeployment.Status.ReadyReplicas > 0 && modelServiceDeploymentMatchesDesired(*existingDeployment, *desiredDeployment) {
			return nil
		}
		propagationPolicy := metav1.DeletePropagationForeground
		deleteErr := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) && !errors.IsConflict(deleteErr) {
			return fmt.Errorf("delete existing deployment %s: %w", deploymentName, deleteErr)
		}
		if err := WaitUntilDeploymentDeleted(ctx, k8sClient, namespace, deploymentName, 2*time.Minute); err != nil {
			return fmt.Errorf("timeout waiting for deployment %s to be deleted: %w", deploymentName, err)
		}
	}

	_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, desiredDeployment, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		propagationPolicy := metav1.DeletePropagationForeground
		_ = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if waitErr := WaitUntilDeploymentDeleted(ctx, k8sClient, namespace, deploymentName, 2*time.Minute); waitErr != nil {
			return fmt.Errorf("timeout waiting for deployment %s to be deleted before recreate: %w", deploymentName, waitErr)
		}
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, desiredDeployment, metav1.CreateOptions{})
	}
	return err
}

func modelServiceDeploymentMatchesDesired(existing, desired appsv1.Deployment) bool {
	return apiequality.Semantic.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) &&
		apiequality.Semantic.DeepEqual(existing.Spec.Template.Labels, desired.Spec.Template.Labels) &&
		apiequality.Semantic.DeepEqual(existing.Spec.Template.Spec, desired.Spec.Template.Spec)
}

func buildModelServiceDeployment(namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int, extraArgs []string) *appsv1.Deployment {
	appLabel := name + decodeNameSuffix
	image := defaultModelServiceSimulatorImage
	if !useSimulator {
		image = defaultModelServiceRuntimeImage
	}
	args := buildModelServerArgs(modelID, useSimulator, maxNumSeqs, extraArgs)
	labels := map[string]string{
		"app":                          appLabel,
		"llm-d.ai/inferenceServing":    defaultLabelValueTrue,
		"llm-d.ai/model":               defaultModelServiceLabelValue,
		"llm-d.ai/model-pool":          poolName,
		"test-resource":                defaultTestResourceLabelValue,
		"llm-d.ai/guide":               defaultGuideLabelValue,
		"llm-d.ai/inference-serving":   defaultLabelValueTrue,
		"llm-d.ai/accelerator-variant": defaultAcceleratorVariantValue,
	}

	envVars := []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
	}
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	if !useSimulator {
		envVars = append(envVars,
			corev1.EnvVar{Name: "HF_HOME", Value: "/model-cache"},
			corev1.EnvVar{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: defaultHFTokenSecretName},
					Key:                  defaultHFTokenSecretKey,
				},
			}},
		)
		volumes = []corev1.Volume{
			{Name: "model-storage", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resourcePtr("100Gi")}}},
			{Name: "torch-compile-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "metrics-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "triton-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
		volumeMounts = []corev1.VolumeMount{
			{Name: "model-storage", MountPath: "/model-cache"},
			{Name: "torch-compile-cache", MountPath: "/.cache"},
			{Name: "metrics-volume", MountPath: "/.config"},
			{Name: "triton-cache", MountPath: "/.triton"},
		}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appLabel,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                          appLabel,
					"llm-d.ai/inferenceServing":    defaultLabelValueTrue,
					"llm-d.ai/model":               defaultModelServiceLabelValue,
					"llm-d.ai/model-pool":          poolName,
					"llm-d.ai/guide":               defaultGuideLabelValue,
					"llm-d.ai/inference-serving":   defaultLabelValueTrue,
					"llm-d.ai/accelerator-variant": defaultAcceleratorVariantValue,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            appLabel,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            args,
							Ports: []corev1.ContainerPort{
								{Name: defaultServicePortName, ContainerPort: defaultModelServiceContainerPort, Protocol: corev1.ProtocolTCP},
							},
							Env:          envVars,
							Resources:    buildModelServiceResources(useSimulator),
							VolumeMounts: volumeMounts,
						},
					},
					Volumes:       volumes,
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

func resourcePtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// buildModelServiceResources returns resource requirements appropriate for the
// deployment mode. Real vLLM requires a GPU to detect the device type at startup;
// the simulator runs on CPU.
//
// The simulator's GPU claim is NOT here, deliberately: which resource to ask for
// depends on the vendor of the node the workload is pinned to, and this function
// cannot know that. It is added by pinToDiscoveredAccelerator, alongside the
// nodeSelector, from the same discovery -- see the note there for why the claim
// exists at all. Naming nvidia.com/gpu here instead cost a CI run: on a mixed
// cluster the pin chose the AMD node and every pod asked it for an NVIDIA
// resource it does not advertise, so nothing scheduled and five suites failed in
// BeforeAll waiting for pods that could never start.
func buildModelServiceResources(useSimulator bool) corev1.ResourceRequirements {
	if useSimulator {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		}
	}
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("1"),
		},
	}
}

func buildModelServerArgs(modelID string, useSimulator bool, maxNumSeqs int, extraArgs []string) []string {
	return append(buildModelServerBaseArgs(modelID, useSimulator, maxNumSeqs), extraArgs...)
}

func buildModelServerBaseArgs(modelID string, useSimulator bool, maxNumSeqs int) []string {
	if useSimulator {
		// Simulator is configured to be deliberately slow so that Prometheus
		// can observe non-zero KV-cache and queue metrics between scrapes (every 15s).
		// With TTFT=2000ms + ITL=100ms and ~250 output tokens, each request takes ~27s.
		// With max-num-seqs=5, the 5 slots fill quickly and incoming requests queue,
		// producing visible num_requests_waiting and kv_cache_usage_perc metrics.
		//
		// KV cache sizing is critical: the simulator uses reference-counted unique
		// block hashes, so all requests with the same prompt share a single block.
		// The burst load prompt (~8 tokens) with blockSize=8 produces exactly
		// 1 block (8/8 = 1). With kv-cache-size=1 (max 1 block), usage = 1/1 = 100%,
		// which exceeds the WVA saturation spare trigger threshold and fires scale-up.
		// IMPORTANT: The load generator must use /v1/completions (text completion),
		// NOT /v1/chat/completions — the simulator only tracks KV cache for the
		// text completion API.
		// Note: blockSize must be one of {8, 16, 32, 64, 128} per simulator validation.
		const (
			simulatorKVCacheSize = 1        // minimal cache: 1 unique block / 1 max block = 100% usage during load
			simulatorBlockSize   = 8        // minimum valid block size; 8 tokens / 8 = 1 block per request
			simulatorMaxModelLen = 512      // must exceed prompt tokens + max_tokens (burst load uses ~9 + 400 = 409)
			simulatorTTFT        = "2000ms" // time-to-first-token (slow to hold KV cache)
			simulatorITL         = "100ms"  // inter-token latency (slow to keep requests active)
		)
		return []string{
			"--model", modelID,
			"--port", "8000",
			"--time-to-first-token=" + simulatorTTFT,
			"--inter-token-latency=" + simulatorITL,
			"--mode=random",
			"--enable-kvcache",
			fmt.Sprintf("--kv-cache-size=%d", simulatorKVCacheSize),
			fmt.Sprintf("--block-size=%d", simulatorBlockSize),
			"--max-num-seqs", strconv.Itoa(maxNumSeqs),
			"--max-model-len", strconv.Itoa(simulatorMaxModelLen),
		}
	}
	return []string{
		"--model", modelID,
		"--max-num-seqs", strconv.Itoa(maxNumSeqs),
		"--max-model-len", "1024",
		"--served-model-name", modelID,
		"--disable-log-requests",
	}
}

// WithPoolGuide overrides the `llm-d.ai/guide` label that decides which
// InferencePool selects this workload's pods.
//
// Needed to place a workload OUTSIDE the pool under test. The project's contract
// is one model to one InferencePool, but every fixture defaults to the same
// guide value, so two fixtures serving different models would still land in one
// pool — and a running workload there supplies ready endpoints, which stops
// requests queueing and makes the scale-from-zero scenario unreachable.
func WithPoolGuide(guide string) ModelServiceOption {
	return func(d *appsv1.Deployment) {
		if d.Spec.Template.Labels == nil {
			d.Spec.Template.Labels = map[string]string{}
		}
		d.Spec.Template.Labels["llm-d.ai/guide"] = guide
		// The guide label is part of the Deployment's selector, and the API
		// server rejects a template whose labels do not match it.
		if d.Spec.Selector != nil && d.Spec.Selector.MatchLabels != nil {
			if _, selects := d.Spec.Selector.MatchLabels["llm-d.ai/guide"]; selects {
				d.Spec.Selector.MatchLabels["llm-d.ai/guide"] = guide
			}
		}
	}
}
