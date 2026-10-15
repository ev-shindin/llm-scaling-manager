package scalingpolicy

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// ShouldCollectClusterInventory reports whether the per-cycle
// collector.CollectInventoryK8S call is appropriate for the active
// configuration: only when a physical limiter is declared, alone or beside quota
// entries. A quota-only deployment stays free of any Node API traffic: it
// deliberately does not consult physical capacity, and listing Nodes here (even
// just for logging) would defeat that contract.
//
// It reads Config.HasPhysicalLimiter so it stays consistent with the limiters
// NewLimiterFromConfig actually builds — both are driven by the same inline
// limiters: list on the saturation "default" config, live. It used to ask for
// the inventory MODE, which a quota entry beside the physical one suppressed.
func ShouldCollectClusterInventory(cfg *config.Config) bool {
	return cfg.HasPhysicalLimiter()
}
