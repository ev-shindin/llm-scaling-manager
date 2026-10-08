package gpunodes

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// CapacityDiscovery defines the interface for discovering accelerator capacity in the cluster.
type CapacityDiscovery interface {
	// Discover returns a map of node names to their accelerator inventory.
	// The inner map is keyed by accelerator model name (e.g. "NVIDIA-A100").
	// Returns detailed per-node info needed for capacity planning and node selection.
	Discover(ctx context.Context) (map[string]map[string]AcceleratorModelInfo, error)
}

// UsageDiscovery defines the interface for discovering current GPU usage in the cluster.
type UsageDiscovery interface {
	// DiscoverUsage returns a map of accelerator type to used GPU count.
	// This is calculated by summing GPU requests from all running pods.
	// Returns aggregated counts (not per-node) since limiter only needs cluster-wide totals.
	// Note: The return type differs from Discover() intentionally - usage tracking needs
	// simple aggregated counts, while capacity discovery needs detailed per-node info.
	DiscoverUsage(ctx context.Context) (map[string]int, error)
}

// NamespacedUsageDiscovery discovers GPU usage in both views the constraint
// providers consume — cluster-wide per accelerator type, and per namespace per
// type — from a single observation.
//
// Both views come back from ONE call on purpose. They are consumed together
// (ComputeConstraints takes them as a pair, and a namespace's cap is bounded by
// the cluster pool), so deriving them from two separate walks would let a pod
// that started in between appear in one view and not the other, and a consumer
// comparing them would see a cluster that does not add up.
type NamespacedUsageDiscovery interface {
	// DiscoverUsageByNamespace returns (usage by accelerator type, usage by
	// namespace by accelerator type).
	DiscoverUsageByNamespace(ctx context.Context) (map[string]int, map[string]map[string]int, error)
}

// NodeDiscovery defines the interface for discovering per-node information including
// labels and accelerator capacity. Enables label-aware features that need to match
// node selectors against cluster nodes (e.g., namespace-scoped inventory).
type NodeDiscovery interface {
	// DiscoverNodes returns per-node info keyed by node name. Only nodes with at
	// least one discovered accelerator are included. The returned map and all
	// nested maps are freshly allocated and safe to mutate.
	DiscoverNodes(ctx context.Context) (map[string]NodeInfo, error)
}

// FullDiscovery combines capacity, usage, and node discovery for complete inventory tracking.
type FullDiscovery interface {
	CapacityDiscovery
	UsageDiscovery
	NodeDiscovery
}

// NodeUsageDiscovery additionally returns the per-node view from the same pod
// walk as the namespaced one. Optional: a discovery that cannot provide it
// leaves the per-node consumers without node information, which they handle.
type NodeUsageDiscovery interface {
	DiscoverUsageWithNodes(ctx context.Context) (map[string]int, map[string]map[string]int, map[string]decision.NodeGPU, error)
}
