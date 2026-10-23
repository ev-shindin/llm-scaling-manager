package metrics

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/balance"
)

func prefillReading(sustained bool) balance.Reading {
	return balance.Reading{
		Distribution: balance.Distribution{
			Role: domain.RolePrefill, Ready: 4, MaxKvUsage: 0.2, MeanKvUsage: 0.05,
			MaxQueue: 439, MeanQueue: 109.75, Saturated: 1, Idle: 3,
		},
		Sustained: sustained,
	}
}

func TestPublishLoadBalanceSetsEverySeries(t *testing.T) {
	_, registry := engineConfigEmitter(t)
	PublishLoadBalance("ns", "m", []balance.Reading{prefillReading(true)})

	l := map[string]string{constants.LabelNamespace: "ns", constants.LabelModelName: "m", constants.LabelRole: "prefill"}
	for name, want := range map[string]float64{
		constants.WVALoadReadyReplicas:              4,
		constants.WVALoadKvUsageMax:                 0.2,
		constants.WVALoadKvUsageMean:                0.05,
		constants.WVALoadKvMaxToMeanRatio:           4,
		constants.WVALoadQueueMaxToMeanRatio:        4,
		constants.WVALoadIdleReplicasWhileSaturated: 3,
		constants.WVALoadImbalanced:                 1,
	} {
		if got, ok := gaugeAt(t, registry, name, l); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", name, got, ok, want)
		}
	}
}

// A ratio that says nothing is absent, not 0; a role that leaves, and a model
// that leaves, take their series with them.
func TestPublishLoadBalanceDeletesWhatNoLongerHolds(t *testing.T) {
	_, registry := engineConfigEmitter(t)
	decode := balance.Reading{Distribution: balance.Distribution{Role: domain.RoleDecode, Ready: 1, MaxKvUsage: 0.3, MeanKvUsage: 0.3}}
	PublishLoadBalance("ns", "m", []balance.Reading{prefillReading(false), decode})

	dec := map[string]string{constants.LabelModelName: "m", constants.LabelRole: "decode"}
	if _, ok := gaugeAt(t, registry, constants.WVALoadKvMaxToMeanRatio, dec); ok {
		t.Error("a kv ratio over one replica was published")
	}
	if got, ok := gaugeAt(t, registry, constants.WVALoadImbalanced, dec); !ok || got != 0 {
		t.Errorf("imbalanced = %v (present %v), want 0", got, ok)
	}

	PublishLoadBalance("ns", "m", []balance.Reading{decode})
	pre := map[string]string{constants.LabelModelName: "m", constants.LabelRole: "prefill"}
	if _, ok := gaugeAt(t, registry, constants.WVALoadReadyReplicas, pre); ok {
		t.Error("a role with no ready replica kept its series")
	}

	PublishLoadBalance("ns", "other", []balance.Reading{prefillReading(true)})
	RetainLoadBalanceModels(func(_, model string) bool { return model == "other" })
	if _, ok := gaugeAt(t, registry, constants.WVALoadReadyReplicas, dec); ok {
		t.Error("RetainLoadBalanceModels kept a model it was not told to keep")
	}
	other := map[string]string{constants.LabelModelName: "other"}
	if _, ok := gaugeAt(t, registry, constants.WVALoadImbalanced, other); !ok {
		t.Error("RetainLoadBalanceModels dropped a model it was told to keep")
	}
	DeleteLoadBalanceModel("ns", "other")
	if _, ok := gaugeAt(t, registry, constants.WVALoadImbalanced, other); ok {
		t.Error("DeleteLoadBalanceModel left a series")
	}
}
