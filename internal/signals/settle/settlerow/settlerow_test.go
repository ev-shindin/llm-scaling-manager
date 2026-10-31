package settlerow

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// A row keeps the fields the tracker reads, its role from discovery, and its
// ITL as the three states the presence bits give.
func TestOfReadsRolesAndITLStates(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rows := []domain.ReplicaMetrics{
		{PodName: "p", VariantName: "pre", Ready: true, ReadySince: at, AvgITLEmpty: true,
			RequestRate: 0.5, RequestRateKnown: true},
		{PodName: "d", VariantName: "dec", FromWarmPool: true, LentSince: at, Revision: "r",
			KvCacheUsage: 0.4, KvCacheUsageKnown: true, AvgITL: 0.02, AvgITLKnown: true,
			GenerationTokenRate: 100, GenerationTokenRateKnown: true},
		{PodName: "u", VariantName: "unlisted"},
	}
	got := Of(rows, map[string]string{"pre": domain.RolePrefill, "dec": domain.RoleDecode})
	want := []settle.Row{
		{Key: settle.Key{Pod: "p", Variant: "pre"}, Prefill: true, Ready: true, ReadySince: at, ITL: settle.ITLEmpty,
			RequestRate: 0.5, RequestRateKnown: true},
		{Key: settle.Key{Pod: "d", Variant: "dec", Bridge: true}, LentSince: at, Revision: "r",
			KV: 0.4, KVKnown: true, ITL: settle.ITLFinite, GenerationRate: 100, GenerationRateKnown: true},
		{Key: settle.Key{Pod: "u", Variant: "unlisted"}, ITL: settle.ITLAbsent},
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
