package allocation

import (
	"context"
	"errors"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// TypeInventory tracks GPU capacity, usage, and availability per accelerator type (H100, A100, etc.).
//
// Unlike ClusterInventory which maintains a single pool of all GPUs, TypeInventory
// maintains separate pools for each accelerator type. This ensures that:
//   - H100 workloads can only use H100 GPUs
//   - A100 workloads can only use A100 GPUs
//   - Different accelerator types don't compete for the same pool
//
// The inventory tracks three values per accelerator type:
//   - Limit: total capacity discovered from the cluster
//   - Used: GPUs currently in use (discovered from pods or set manually)
//   - Available: Limit - Used (computed)
//
// This is essential for heterogeneous clusters where workloads have specific
// hardware requirements that cannot be satisfied by other accelerator types.
type TypeInventory struct {
	name           string
	discovery      gpunodes.CapacityDiscovery
	usageDiscovery gpunodes.UsageDiscovery // Optional: if set, RefreshAll will auto-discover usage

	mu sync.RWMutex
	// limitByType maps accelerator type (e.g., "H100", "A100") to total GPU capacity
	limitByType map[string]int
	// usedByType maps accelerator type to currently used GPU count
	usedByType map[string]int
	// totalLimit is the sum of all GPU capacity across types
	totalLimit int
	// totalUsed is the sum of all used GPUs across types
	totalUsed int
}

// NewTypeInventory creates a TypeInventory that tracks GPUs per accelerator type.
//
// Parameters:
//   - name: identifier for logging/metrics
//   - disc: interface to discover accelerator capacity from the cluster
//
// For automatic usage discovery, use NewTypeInventoryWithUsage instead.
func NewTypeInventory(name string, disc gpunodes.CapacityDiscovery) *TypeInventory {
	return &TypeInventory{
		name:        name,
		discovery:   disc,
		limitByType: make(map[string]int),
		usedByType:  make(map[string]int),
	}
}

// NewTypeInventoryWithUsage creates a TypeInventory with automatic usage discovery.
//
// Parameters:
//   - name: identifier for logging/metrics
//   - disc: interface implementing both CapacityDiscovery and UsageDiscovery
//
// When using this constructor, call RefreshAll() to update both limits and usage
// in a single operation.
func NewTypeInventoryWithUsage(name string, disc gpunodes.FullDiscovery) *TypeInventory {
	return &TypeInventory{
		name:           name,
		discovery:      disc,
		usageDiscovery: disc,
		limitByType:    make(map[string]int),
		usedByType:     make(map[string]int),
	}
}

// Name returns the inventory identifier.
func (i *TypeInventory) Name() string {
	return i.name
}

// RefreshAll updates both limits (capacity) and usage in a single operation.
//
// This is the preferred method when using NewTypeInventoryWithUsage.
// It discovers GPU capacity from nodes and calculates current usage from pods.
//
// Returns an error if usage discovery is not configured (use Refresh + SetUsed instead).
func (i *TypeInventory) RefreshAll(ctx context.Context) error {
	if i.usageDiscovery == nil {
		return errors.New("usage discovery not configured; use SetUsed() or NewTypeInventoryWithUsage()")
	}

	// Refresh limits first
	if err := i.Refresh(ctx); err != nil {
		return err
	}

	// Discover current usage
	usedByType, err := i.usageDiscovery.DiscoverUsage(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover GPU usage: %w", err)
	}

	// Update usage
	i.SetUsed(usedByType)

	return nil
}

// Refresh updates the inventory limits from the cluster using the discovery interface.
//
// This aggregates GPU capacity across all nodes for each accelerator type.
// Accelerator names are normalized from full model names (e.g., "NVIDIA-A100-PCIE-80GB")
// to short names (e.g., "A100") to match VA label conventions.
// Should be called before reading pools to ensure fresh data.
// Note: This only updates limits; call SetUsed or RefreshAll to update usage.
func (i *TypeInventory) Refresh(ctx context.Context) error {
	// Discover node -> accelerator type -> count
	nodeInventory, err := i.discovery.Discover(ctx)
	if err != nil {
		// A physical limiter that cannot read nodes is the one misconfiguration
		// that costs nothing at install time and everything afterwards: every
		// variant is charged to no accelerator pool, gets no budget, and stops
		// scaling up. Nothing else reports it, because an unresolved accelerator is
		// a perfectly normal state when no limiter is configured.
		//
		// Node permission is optional until someone turns this limiter on — which
		// a cluster admin can do long after the install, by editing the ConfigMap.
		// So say it loudly and publish it, rather than returning an error that the
		// caller logs at DEBUG once a cycle.
		if apierrors.IsForbidden(err) {
			metrics.SetNodeAccessDenied(true)
			log.FromContext(ctx).Error(err, "A GPU limiter is configured but this controller cannot read nodes. "+
				"Every variant will be charged to no accelerator pool, receive no GPU budget, and STOP SCALING UP. "+
				"Grant the node read, or remove the limiters: entry from the scaling-policy ConfigMap.",
				"limiter", i.name, "metric", constants.WVANodeAccessDenied)
		}
		return fmt.Errorf("failed to discover accelerator capacity: %w", err)
	}
	metrics.SetNodeAccessDenied(false)

	// Aggregate by accelerator type across all nodes
	// Normalize full model names to short names for matching with VA labels
	byType := make(map[string]int)
	total := 0

	for _, accelerators := range nodeInventory {
		for fullModelName, info := range accelerators {
			// Normalize "NVIDIA-A100-PCIE-80GB" -> "A100"
			shortName := accelerator.NormalizeAcceleratorName(fullModelName)
			byType[shortName] += info.Count
			total += info.Count
		}
	}

	i.mu.Lock()
	i.limitByType = byType
	i.totalLimit = total
	i.mu.Unlock()

	return nil
}

// SetUsed updates the used GPU counts per accelerator type.
// This should be called with current usage (e.g., from replica counts) before reading pools.
//
// Incoming keys are reconciled against the discovered limit keys, because the two
// sides are written in different vocabularies. Limits are keyed by NORMALIZED
// short names ("A100"); callers key usage by the name a WORKLOAD declares, which
// for the common nodeSelector deployment is the raw product label
// ("NVIDIA-A100-PCIE-80GB"). Without reconciliation such a key never matches a
// limit key, GetResourcePools reports Used = 0 for it, and every pool claims its
// full complement is free however much is actually running — so both the
// GPU-aware optimizer and the scale-from-zero placement check see an empty
// cluster and over-allocate.
//
// Usage that matches no discovered type is dropped rather than stored: it cannot
// be charged to a pool, so counting it in totalUsed would leave the aggregate
// contradicting the sum of the pools. Callers must therefore Refresh (which
// populates the limit keys) before SetUsed; ComputeConstraints does.
func (i *TypeInventory) SetUsed(usedByType map[string]int) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.usedByType = make(map[string]int, len(usedByType))
	total := 0
	soleType, homogeneous := i.soleAcceleratorType()
	for declared, count := range usedByType {
		key, ok := resolveAcceleratorKey(i.limitByType, declared)
		if !ok {
			// No discovered type owns this name — an unresolved accelerator, or a
			// type that has since left the cluster.
			//
			// On a HOMOGENEOUS cluster that is still attributable: if exactly one
			// accelerator type was discovered, whatever is holding these GPUs can
			// only be running on it, whether or not the workload declared so. That
			// closes the over-provisioning gap outright for single-type clusters,
			// which is the common shape.
			//
			// Otherwise it stays unattributed and uncounted, and the budget check
			// remains permissive — deliberately. Denying on unattributable usage
			// would let one mislabelled variant block scaling for every other
			// workload in the cluster. Operators who want a bound on it can give a
			// quota limiter an explicit "unknown" entry, which the quota inventory
			// honours as its own pool. The amount is reported either way
			// (wva_unattributed_gpus).
			if !homogeneous {
				continue
			}
			key = soleType
		}
		i.usedByType[key] += count
		total += count
	}
	i.totalUsed = total
}

// TotalLimit returns total GPU capacity across all types.
func (i *TypeInventory) TotalLimit() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.totalLimit
}

// TotalUsed returns total GPUs currently in use across all types.
func (i *TypeInventory) TotalUsed() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.totalUsed
}

// TotalAvailable returns total available GPUs (Limit - Used) across all types.
func (i *TypeInventory) TotalAvailable() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	available := i.totalLimit - i.totalUsed
	if available < 0 {
		return 0
	}
	return available
}

// LimitByType returns the GPU capacity limit for a specific accelerator type.
func (i *TypeInventory) LimitByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.limitByType[accType]
}

// UsedByType returns the used GPU count for a specific accelerator type.
func (i *TypeInventory) UsedByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.usedByType[accType]
}

// AvailableByType returns available GPUs (Limit - Used) for a specific accelerator type.
func (i *TypeInventory) AvailableByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	available := i.limitByType[accType] - i.usedByType[accType]
	if available < 0 {
		return 0
	}
	return available
}

// GetResourcePools returns per-type resource availability as ResourcePool structs.
func (i *TypeInventory) GetResourcePools() map[string]ResourcePool {
	i.mu.RLock()
	defer i.mu.RUnlock()

	pools := make(map[string]ResourcePool, len(i.limitByType))
	for accType, limit := range i.limitByType {
		used := i.usedByType[accType]
		pools[accType] = ResourcePool{
			Limit: limit,
			Used:  used,
		}
	}
	return pools
}

// AcceleratorTypes returns all known accelerator types.
func (i *TypeInventory) AcceleratorTypes() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	types := make([]string, 0, len(i.limitByType))
	for t := range i.limitByType {
		types = append(types, t)
	}
	return types
}

// PoolsClosed reports true: a type no node carries has no GPUs, so physical
// inventory denies every type it did not discover.
func (i *TypeInventory) PoolsClosed() bool {
	return true
}

// Ensure TypeInventory implements Inventory interface
var _ Inventory = (*TypeInventory)(nil)

// Ensure TypeInventory declares its pools closed
var _ ClosedPoolsInventory = (*TypeInventory)(nil)

// soleAcceleratorType returns the only discovered accelerator type, and whether
// the cluster is homogeneous in that sense. Callers must already hold the lock.
//
// "Exactly one type" is the whole condition: with a single type there is nowhere
// else unattributed usage could be, so charging it there is a deduction rather
// than a guess. With two or more it would be a guess, and a wrong one silently
// starves whichever pool it is charged to.
func (i *TypeInventory) soleAcceleratorType() (string, bool) {
	if len(i.limitByType) != 1 {
		return "", false
	}
	for t := range i.limitByType {
		return t, true
	}
	return "", false
}
