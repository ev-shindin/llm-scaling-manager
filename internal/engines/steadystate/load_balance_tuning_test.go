package steadystate

import (
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
)

// The diagnostic reads the model's loadImbalance block, and with none set it
// runs on the constants it was calibrated with.
func TestLoadBalanceThresholdsReadThePolicy(t *testing.T) {
	p := config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5}
	want := balance.Thresholds{KvUsage: 0.8, Queue: 5, IdleKvUsage: balance.IdleKvUsage,
		IdleAfterReady: balance.IdleAfterReady, SustainFor: balance.SustainFor, ClearAfter: balance.ClearAfter}
	if got := loadBalanceThresholds(p); got != want {
		t.Errorf("defaults: %+v, want %+v", got, want)
	}
	p.LoadImbalance = config.LoadImbalance{
		IdleKvUsage: 0.2, IdleAfterReady: config.PolicyDuration(3 * time.Minute),
		SustainFor: config.PolicyDuration(5 * time.Minute), ClearAfter: config.PolicyDuration(4 * time.Minute),
	}
	want = balance.Thresholds{KvUsage: 0.8, Queue: 5, IdleKvUsage: 0.2,
		IdleAfterReady: 3 * time.Minute, SustainFor: 5 * time.Minute, ClearAfter: 4 * time.Minute}
	got := loadBalanceThresholds(p)
	if got != want {
		t.Errorf("tuned: %+v, want %+v", got, want)
	}
	// The warning states the windows in force, not the defaults.
	if msg := loadImbalancedWarning(got); !strings.Contains(msg, "at least 3m0s") || !strings.Contains(msg, "for 5m0s") {
		t.Errorf("warning does not state the tuned windows: %s", msg)
	}
	if msg := loadImbalancedWarning(balance.Thresholds{}); !strings.Contains(msg, "at least 2m0s") {
		t.Errorf("warning does not state the default window: %s", msg)
	}
}
