package gpunodes

import (
	"context"
	"fmt"
	"os"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sWithGpuOperator implements CapacityDiscovery for Kubernetes clusters with GPU Operator
type K8sWithGpuOperator struct {
	Client         client.Client
	metricsEmitter *metrics.MetricsEmitter
}

// NewK8sWithGpuOperator creates a new K8sWithGpuOperator instance.
func NewK8sWithGpuOperator(client client.Client) *K8sWithGpuOperator {
	return &K8sWithGpuOperator{
		Client:         client,
		metricsEmitter: metrics.NewMetricsEmitter(),
	}
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
		}
	}
	return byType, byNamespace, out, nil
}

// usageWalk is the one pod walk behind every usage view: GPUs requested by
// pods occupying GPU nodes, per accelerator type, per namespace and per node.
func (d *K8sWithGpuOperator) usageWalk(ctx context.Context, nodeGPUType map[string]string) (map[string]int, map[string]map[string]int, map[string]int, error) {
	var podList corev1.PodList
	if err := d.Client.List(ctx, &podList); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to list pods: %w", err)
	}

	usageByType := make(map[string]int)
	usageByNamespace := make(map[string]map[string]int)
	usageByNode := make(map[string]int)

	for _, pod := range podList.Items {
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

// refusesNewPods reports whether no new pod can land on node: it is cordoned,
// not Ready, or carries a NoSchedule or NoExecute taint. A placement that
// counted on its free GPUs would open a hole nobody can use.
func refusesNewPods(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return true
	}
	for _, t := range node.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
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
