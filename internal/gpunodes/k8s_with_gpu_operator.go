package gpunodes

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// K8sWithGpuOperator implements CapacityDiscovery for Kubernetes clusters with GPU Operator
type K8sWithGpuOperator struct {
	Client         client.Client
	metricsEmitter *metrics.MetricsEmitter

	// podReader lists the Pods the usage walk counts; nil reads through Client.
	// See WithPodReader.
	podReader client.Reader
	// physicalLimiterInForce reports whether a physical GPU limiter reads the
	// usage walk right now; nil means always. See WithPhysicalLimiterCheck.
	physicalLimiterInForce func() bool
	// podsDenied remembers how the last Forbidden pod list was reported
	// (podsDeniedNone, podsDeniedInfo or podsDeniedError), so it is logged when
	// it starts or changes rather than on every refresh.
	podsDenied atomic.Int32
}

// How a Forbidden cluster-wide pod list was last reported.
const (
	podsDeniedNone int32 = iota
	podsDeniedInfo
	podsDeniedError
)

// NewK8sWithGpuOperator creates a new K8sWithGpuOperator instance.
func NewK8sWithGpuOperator(client client.Client) *K8sWithGpuOperator {
	return &K8sWithGpuOperator{
		Client:         client,
		metricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// WithPodReader makes the usage walk list Pods through r, outside the
// controller's cache, instead of through Client, and returns d.
//
// A namespace-scoped controller needs it. Its cache holds Pods of the managed
// namespace only, and a list through it answers silently with that subset, so
// GPUs that other tenants' Pods hold on the same nodes read as free and a
// scale-up is admitted onto a full node. Reading uncached, the walk sees every
// Pod on the GPU nodes when RBAC lets it list Pods cluster-wide. The list is
// served from the API server's watch cache, not etcd; see listAllPods. When it does
// not, the walk falls back to the cache and, while a physical limiter reads it,
// reports the undercount (wva_pod_access_denied and an error log); see listPods.
func (d *K8sWithGpuOperator) WithPodReader(r client.Reader) *K8sWithGpuOperator {
	d.podReader = r
	return d
}

// WithPhysicalLimiterCheck sets how listPods learns whether a physical GPU
// limiter is in force, and returns d. The limiter list is live, so inForce is
// asked on every Forbidden list.
//
// Only a physical limiter reads the per-node usage the walk produces, alone or
// beside quota limiters. With quota limiters alone, or none, the undercount a Forbidden list causes bounds
// nothing, so it is not reported as a failure: no error log, and
// wva_pod_access_denied stays 0.
func (d *K8sWithGpuOperator) WithPhysicalLimiterCheck(inForce func() bool) *K8sWithGpuOperator {
	d.physicalLimiterInForce = inForce
	return d
}

// listGPUNodes queries GPU-bearing nodes across all supported vendors
// (NVIDIA, AMD, Intel) and returns a canonical per-node view keyed by node name.
// It queries per vendor because Kubernetes LabelSelectors don't support OR logic
// across different label keys. Multi-vendor nodes (nodes with labels from more
// than one vendor) are merged into a single NodeInfo entry.
//
// This is the single internal node-listing primitive; public methods Discover,
// discoverNodeGPUTypes, and DiscoverNodes project from its result.
func (d *K8sWithGpuOperator) listGPUNodes(ctx context.Context) (map[string]NodeInfo, error) {
	var err error
	defer func() {
		if err != nil {
			metrics.SetGpuDiscoveryUp(0)
		} else {
			metrics.SetGpuDiscoveryUp(1)
		}
	}()

	nodes := make(map[string]NodeInfo)

	// Parse WVA_NODE_SELECTOR once for reuse across vendor queries
	var userRequirements []labels.Requirement
	if selectorStr := os.Getenv("WVA_NODE_SELECTOR"); selectorStr != "" {
		userSelector, parseErr := labels.Parse(selectorStr)
		if parseErr != nil {
			err = fmt.Errorf("invalid WVA_NODE_SELECTOR: %w", parseErr)
			return nil, err
		}
		userRequirements, _ = userSelector.Requirements()
	}

	// Query nodes for each GPU vendor separately.
	// K8s LabelSelectors don't support OR logic across different keys (e.g. nvidia OR amd).
	for _, res := range constants.VendorResources {
		vendor := res.Vendor
		memKey := res.MemoryLabel
		resName := corev1.ResourceName(res.ResourceName)

		// Every key this vendor's product can be published under, canonical
		// first. Only ProductLabel was read here, which is the GPU Feature
		// Discovery spelling: on a cluster that publishes an alias instead --
		// CoreWeave, GKE, EKS Auto Mode, Karpenter -- this discovered NO GPU
		// nodes at all, and the gpu-inventory limiter then bounds a fleet
		// against capacity it cannot see. The aliases were already listed in
		// constants for accelerator naming; this is the inventory reading the
		// same list.
		prodKeys := make([]string, 0, 1+len(res.ProductLabelAliases))
		prodKeys = append(prodKeys, res.ProductLabel)
		prodKeys = append(prodKeys, res.ProductLabelAliases...)

		// The keys cannot be OR-ed in a LabelSelector, and selecting on the
		// canonical one is what dropped the alias-only nodes, so the product
		// filter moves into the loop below. Only the user's sharding
		// requirements stay here.
		selector := labels.NewSelector()

		// Add user requirements for sharding
		for _, userReq := range userRequirements {
			selector = selector.Add(userReq)
		}

		var nodeList corev1.NodeList
		if listErr := d.Client.List(ctx, &nodeList, &client.ListOptions{LabelSelector: selector}); listErr != nil {
			err = fmt.Errorf("failed to list nodes for vendor %s: %w", vendor, listErr)
			return nil, err
		}

		// Process nodes for this vendor
		accelerators := make(map[string]int)
		for _, node := range nodeList.Items {
			// FIRST key the node carries wins, so a cluster publishing both the
			// GFD key and its provider's is counted once rather than twice under
			// two spellings of the same card.
			model := ""
			for _, prodKey := range prodKeys {
				if v, ok := node.Labels[prodKey]; ok && v != "" {
					model = v
					break
				}
			}
			if model == "" {
				continue
			}

			count := 0
			if cap, ok := node.Status.Allocatable[resName]; ok {
				count = int(cap.Value())
			}

			ni, exists := nodes[node.Name]
			if !exists {
				ni = NodeInfo{
					Name:          node.Name,
					Labels:        copyStringMap(node.Labels),
					Accelerators:  make(map[string]AcceleratorModelInfo),
					Unschedulable: refusesNewPods(&node),
					Taints:        schedulingTaints(&node),
				}
			}
			// i915 and xe resources use the same gpu.intel.com/product label, so
			// a node can be selected once for the matching resource and once for the
			// sibling resource that has no allocatable entry. Preserve labeled
			// nodes with no allocatable resource, but skip the duplicate zero-count
			// pass when this node/model was already recorded.
			if _, seen := ni.Accelerators[model]; seen && count == 0 {
				nodes[node.Name] = ni
				continue
			}
			ni.Accelerators[model] = AcceleratorModelInfo{
				Count:  count,
				Memory: node.Labels[memKey],
			}
			nodes[node.Name] = ni
			accelerators[model] += count
		}

		// record metric as soon as accelerators are discovered. For this vendor, record number of GPUs per accelerator type.
		for model, count := range accelerators {
			d.metricsEmitter.RecordAvailableGPUsMetric(vendor, model, accelerator.NormalizeAcceleratorName(model), int32(count))
		}
	}

	return nodes, nil
}

// copyStringMap returns a shallow copy of m, or an empty map if m is nil.
// Used to ensure the labels map returned in NodeInfo is independent of the
// underlying corev1.Node object.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Discover discovers GPU capacity by iterating over nodes and checking GFD labels.
// It queries nodes for each GPU vendor (NVIDIA, AMD, Intel) separately since
// Kubernetes LabelSelectors don't support OR logic across different label keys.
//
// This is a projection of listGPUNodes into the CapacityDiscovery shape
// (per-node accelerator map without labels).
func (d *K8sWithGpuOperator) Discover(ctx context.Context) (map[string]map[string]AcceleratorModelInfo, error) {
	nodes, err := d.listGPUNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]AcceleratorModelInfo, len(nodes))
	for name, n := range nodes {
		out[name] = n.Accelerators
	}
	return out, nil
}

// DiscoverNodes returns per-node info (labels + accelerators) for all GPU-bearing
// nodes. Used by label-aware features such as the namespace-scoped limiter.
func (d *K8sWithGpuOperator) DiscoverNodes(ctx context.Context) (map[string]NodeInfo, error) {
	return d.listGPUNodes(ctx)
}

// DiscoverUsage calculates current GPU usage by summing GPU requests from running pods.
// Returns a map of accelerator type to used GPU count.
//
// A projection of DiscoverUsageByNamespace, so there is one pod walk and the two
// views cannot drift.
func (d *K8sWithGpuOperator) DiscoverUsage(ctx context.Context) (map[string]int, error) {
	byType, _, err := d.DiscoverUsageByNamespace(ctx)
	return byType, err
}

// DiscoverUsageByNamespace sums GPU requests from the pods actually occupying GPU
// nodes, returning the cluster-wide per-type view and the per-namespace one.
//
// Attribution is by the NODE the pod is scheduled to, not by anything the
// workload declares. That is what makes this usable as the sole producer:
//
//   - it does not depend on WVA having discovered the workload. Usage summed from
//     WVA's own population is blind to anything it has not been called about,
//     which under call-driven discovery includes every workload until KEDA first
//     calls — so a wake placed against it can be judged against an empty picture
//     of a full cluster;
//   - it counts GPUs held by workloads WVA does not manage at all — other
//     namespaces, system pods, another autoscaler — which the population sum
//     cannot see and therefore reports as free;
//   - nothing is unattributable. A pod on a GPU node is charged to that node's
//     type whether or not it declared a nodeSelector, so there is no "unknown"
//     bucket to leak capacity through.
//
// Counts REQUESTS, not allocations, and includes pods that are scheduled but not
// yet running (Spec.NodeName set, not Succeeded/Failed): the scheduler has already
// committed those GPUs, so treating them as free would let two wakes place onto
// the same device. Unscheduled pods hold nothing and are skipped.
func (d *K8sWithGpuOperator) DiscoverUsageByNamespace(ctx context.Context) (map[string]int, map[string]map[string]int, error) {
	nodeGPUType, err := d.discoverNodeGPUTypes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to discover node GPU types: %w", err)
	}
	byType, byNamespace, _, err := d.usageWalk(ctx, nodeGPUType)
	return byType, byNamespace, err
}

// DiscoverUsageWithNodes returns DiscoverUsageByNamespace's two views and, from
// the same pod walk, each GPU node's accelerator, capacity, requested GPUs and
// labels. The utilization-share optimizer places a receiver's pods into the
// holes donors open on particular nodes, which needs the per-node picture
// (docs/proposals/utilization-share-optimizer.md, section 6.5).
func (d *K8sWithGpuOperator) DiscoverUsageWithNodes(ctx context.Context) (map[string]int, map[string]map[string]int, map[string]decision.NodeGPU, error) {
	nodes, err := d.listGPUNodes(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to discover GPU nodes: %w", err)
	}
	nodeGPUType := nodeGPUTypesOf(nodes)
	byType, byNamespace, byNode, err := d.usageWalk(ctx, nodeGPUType)
	if err != nil {
		return nil, nil, nil, err
	}
	out := make(map[string]decision.NodeGPU, len(nodeGPUType))
	for name, model := range nodeGPUType {
		out[name] = decision.NodeGPU{
			Accelerator:   model,
			Capacity:      nodes[name].Accelerators[model].Count,
			Used:          byNode[name],
			Labels:        nodes[name].Labels,
			Unschedulable: nodes[name].Unschedulable,
			Taints:        nodes[name].Taints,
		}
	}
	return byType, byNamespace, out, nil
}

// usageWalk is the one pod walk behind every usage view: GPUs requested by
// pods occupying GPU nodes, per accelerator type, per namespace and per node.
func (d *K8sWithGpuOperator) usageWalk(ctx context.Context, nodeGPUType map[string]string) (map[string]int, map[string]map[string]int, map[string]int, error) {
	pods, err := d.listPods(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	usageByType := make(map[string]int)
	usageByNamespace := make(map[string]map[string]int)
	usageByNode := make(map[string]int)

	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		gpuType, ok := nodeGPUType[pod.Spec.NodeName]
		if !ok {
			// Node doesn't have GPUs, skip
			continue
		}

		gpuCount := resources.PodGPURequests(&pod)
		if gpuCount <= 0 {
			continue
		}
		usageByType[gpuType] += gpuCount
		usageByNode[pod.Spec.NodeName] += gpuCount
		perType, seen := usageByNamespace[pod.Namespace]
		if !seen {
			perType = make(map[string]int)
			usageByNamespace[pod.Namespace] = perType
		}
		perType[gpuType] += gpuCount
	}

	return usageByType, usageByNamespace, usageByNode, nil
}

// listPods returns every Pod the usage walk considers: through Client, or,
// when a pod reader is set, through it (listAllPods).
//
// A Forbidden list through the pod reader falls back to Client, which counts
// the Pods this controller's cache holds and no others, and says so. While a
// physical limiter is in force, the wva_pod_access_denied gauge reads 1 and an
// error is logged when the denial starts. Otherwise the undercount bounds
// nothing: the gauge reads 0 and the denial is logged once at info. The fallback is a deliberate choice over publishing nothing. An
// absent usage view sends the optimizer to its unlimited posture
// (GPUUsageViews.MissingBasis), which bounds less than a known-low count does;
// a reported undercount keeps the cap at the nodes' capacity and is visible.
func (d *K8sWithGpuOperator) listPods(ctx context.Context) ([]corev1.Pod, error) {
	if d.podReader != nil {
		pods, err := listAllPods(ctx, d.podReader)
		if err == nil {
			metrics.SetPodAccessDenied(false)
			d.podsDenied.Store(podsDeniedNone)
			return pods, nil
		}
		if !apierrors.IsForbidden(err) {
			return nil, fmt.Errorf("failed to list pods cluster-wide: %w", err)
		}
		d.reportPodsDenied(ctx, err)
	}
	var list corev1.PodList
	if err := d.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	return list.Items, nil
}

// ResetPodAccessDenied clears a reported pod-list denial: wva_pod_access_denied
// reads 0, and the next Forbidden list is logged again as if it were the first.
//
// Call it when the walk stops because no physical limiter reads it. Nothing
// else would clear the gauge then, and without clearing the remembered level,
// a physical limiter turned off and on again would not log the denial again.
func (d *K8sWithGpuOperator) ResetPodAccessDenied() {
	metrics.SetPodAccessDenied(false)
	d.podsDenied.Store(podsDeniedNone)
}

// reportPodsDenied reports a Forbidden cluster-wide pod list: as a failure
// while a physical limiter is in force, and as information otherwise.
func (d *K8sWithGpuOperator) reportPodsDenied(ctx context.Context, err error) {
	physical := d.physicalLimiterInForce == nil || d.physicalLimiterInForce()
	metrics.SetPodAccessDenied(physical)
	level := podsDeniedInfo
	if physical {
		level = podsDeniedError
	}
	if d.podsDenied.Swap(level) == level {
		return
	}
	logger := log.FromContext(ctx)
	if physical {
		logger.Error(err, "Cannot list Pods cluster-wide. GPU usage counts only the Pods in "+
			"this controller's own namespace, so GPUs other namespaces hold on the same nodes read as FREE "+
			"and the physical limiter can admit scale-ups onto full nodes. Grant list on pods cluster-wide, "+
			"or drop gpu-inventory from the limiters and bound with a quota limiter alone: a quota "+
			"beside gpu-inventory leaves the physical limiter reading this count.",
			"metric", constants.WVAPodAccessDenied)
		return
	}
	logger.Info("Cannot list Pods cluster-wide, so GPU usage counts only this controller's own namespace. "+
		"No physical GPU limiter is in force, so this bounds nothing. A physical limiter would need list "+
		"on pods cluster-wide.", "error", err.Error())
}

// listAllPods lists every Pod through r in one request, with resourceVersion
// "0".
//
// "0" lets the API server answer from its watch cache instead of reading etcd,
// which matters because this runs every refresh interval and lists every Pod in
// the cluster. The answer may be slightly behind etcd, by as much as the watch
// cache is; the usage walk is a periodic observation and tolerates that.
//
// It does not page. A paged list from the watch cache is not supported on every
// API server version, and paging through etcd instead adds a failure this
// path would need its own fallback for: a continue token that expires between
// pages (410 Gone) on a large cluster.
func listAllPods(ctx context.Context, r client.Reader) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.List(ctx, &list, podListFromWatchCache()); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// podListFromWatchCache is the list option listAllPods sends. A new one per
// call: the client writes into Raw while building the request, so a shared one
// would race between concurrent refreshes.
func podListFromWatchCache() *client.ListOptions {
	return &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: "0"}}
}

// discoverNodeGPUTypes returns a map of node name to GPU type (model name).
// For multi-vendor nodes (nodes labeled for more than one GPU vendor), the model
// from the LAST resource with matching productLabel wins.
//
// This preserves the pre-refactor behavior: the original implementation iterated
// VendorResources in order and assigned `nodeGPUType[node] = model` on each match,
// causing later assignments to overwrite earlier ones. Changing this would
// silently affect usage attribution for multi-vendor nodes, so the refactor
// keeps the exact same tie-break semantics.
//
// This is a projection of listGPUNodes into a single-model-per-node shape.
func (d *K8sWithGpuOperator) discoverNodeGPUTypes(ctx context.Context) (map[string]string, error) {
	nodes, err := d.listGPUNodes(ctx)
	if err != nil {
		return nil, err
	}
	return nodeGPUTypesOf(nodes), nil
}

// nodeGPUTypesOf projects listed nodes onto one accelerator model per node,
// with discoverNodeGPUTypes' tie-break.
func nodeGPUTypesOf(nodes map[string]NodeInfo) map[string]string {
	out := make(map[string]string, len(nodes))
	for name, n := range nodes {
		// Iterate vendor resources in REVERSE order and break on first match so
		// the last item in `VendorResources` wins (Intel > AMD > NVIDIA).
		// Relies on the listGPUNodes invariant that n.Accelerators[model]
		// exists whenever n.Labels[productLabel] == model.
		for i := len(constants.VendorResources) - 1; i >= 0; i-- {
			res := constants.VendorResources[i]
			// Same key precedence listGPUNodes uses, or this projection resolves
			// nothing on a cluster that publishes only an alias -- and a node
			// with no entry here is attributed to no accelerator at all.
			prodKeys := make([]string, 0, 1+len(res.ProductLabelAliases))
			prodKeys = append(prodKeys, res.ProductLabel)
			prodKeys = append(prodKeys, res.ProductLabelAliases...)

			model := ""
			for _, prodKey := range prodKeys {
				if v, ok := n.Labels[prodKey]; ok && v != "" {
					model = v
					break
				}
			}
			if model != "" {
				out[name] = model
				break
			}
		}
	}
	return out
}

// Ensure K8sWithGpuOperator implements FullDiscovery
var _ FullDiscovery = (*K8sWithGpuOperator)(nil)

// nodeRemovalTaints mark a node an autoscaler is draining for removal. No
// workload tolerates them.
var nodeRemovalTaints = map[string]bool{
	"ToBeDeletedByClusterAutoscaler": true,
	"karpenter.sh/disrupted":         true,
	"karpenter.sh/disruption":        true,
}

// refusesNewPods reports whether no new pod can land on node: it is cordoned,
// not Ready, or carries a NoSchedule or NoExecute node-condition taint
// (node.kubernetes.io/...: not-ready, unreachable, a pressure) or an
// autoscaler's removal taint. A placement
// that counted on its free GPUs would open a hole nobody can use. Other taints
// are not read: a GPU pool's own taint is tolerated by the pods it exists for,
// and matching a receiver's tolerations is not built.
// schedulingTaints is the node's taints that keep a pod off it unless the pod
// tolerates them: NoSchedule and NoExecute.
func schedulingTaints(node *corev1.Node) []corev1.Taint {
	var out []corev1.Taint
	for _, t := range node.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			out = append(out, t)
		}
	}
	return out
}

func refusesNewPods(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return true
	}
	for _, t := range node.Spec.Taints {
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if strings.HasPrefix(t.Key, "node.kubernetes.io/") || nodeRemovalTaints[t.Key] {
			return true
		}
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status != corev1.ConditionTrue
		}
	}
	return false
}
