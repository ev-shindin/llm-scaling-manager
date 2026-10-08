package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// A P/D wake's two claims are applied each on its own. With both transfers
// still releasing, both are redirected. Once one has released -- its receiver
// already raised -- that one cannot be, but the other still is: the wake has
// happened, its pod will take the hole that opens, and the books must say so
// rather than raise the original receiver into a pod that stays Pending.
func TestUtilizationShareAppliesEachClaimItCan(t *testing.T) {
	for _, tc := range []struct {
		name       string
		releaseOne bool
		want       float64
	}{
		{"both still releasing (control)", false, 2},
		{"one already released", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := freshMetrics(t)
			f := newShareFleet()
			c := sharePods(t, f)
			se := newShareEngine(t, f, c, time.Unix(0, 0))
			for range 20 {
				if se.cycle(); len(markedPods(t, c)) == 2 {
					break
				}
			}
			ids := markIDs(t, c)
			if len(ids) != 2 {
				t.Fatalf("setup: want two transfers in flight, got %d", len(ids))
			}
			claimable := make([]decision.ShareClaimable, 0, len(ids))
			for id := range ids {
				claimable = append(claimable, decision.ShareClaimable{ID: id, ReceiverZ: 0, DonorGPUs: 1})
			}
			if tc.releaseOne {
				f.current["A"]-- // the first transfer releases; its receiver is raised
				se.cycle()
			}
			saved := decision.DefaultShareClaims
			decision.DefaultShareClaims = &decision.ShareClaimStore{}
			t.Cleanup(func() { decision.DefaultShareClaims = saved })
			// Both listed, as a wake would have seen them a moment earlier.
			decision.DefaultShareClaims.Publish(map[string][]decision.ShareClaimable{
				decision.ShareGroupKey("", "A100"): claimable}, se.clock)
			if _, outcome := decision.DefaultShareClaims.ClaimSet("", "A100", []decision.ShareWake{
				{GPUs: 1, Variant: "ns/W-dec"}, {GPUs: 1, Variant: "ns/W-pre"}}, -1, "ns/W", se.clock); outcome != decision.ShareClaimRedirected {
				t.Fatalf("setup: the pair was not claimed: %q", outcome)
			}
			se.cycle() // applies the claims

			redirected := 0.0
			for _, m := range family(t, reg, constants.WVAUtilizationShareTransfersTotal) {
				if label(m, constants.LabelOutcome) == "redirected" {
					redirected += m.GetCounter().GetValue()
				}
			}
			if redirected != tc.want {
				t.Fatalf("redirected %v transfers, want %v", redirected, tc.want)
			}
		})
	}
}

// The warm pools' published carve reaches the group's budget: the spare the
// group reports falls by it.
func TestUtilizationShareCarvesTheWarmPoolsTarget(t *testing.T) {
	spare := func(carve int) float64 {
		reg := freshMetrics(t)
		saved := decision.DefaultWarmPoolUnheld
		decision.DefaultWarmPoolUnheld = &decision.WarmPoolUnheldStore{}
		t.Cleanup(func() { decision.DefaultWarmPoolUnheld = saved })
		f := newShareFleet()
		se := newShareEngine(t, f, sharePods(t, f), time.Unix(0, 0))
		if carve > 0 {
			decision.DefaultWarmPoolUnheld.Publish("pools", map[string]int{"A100": carve}, se.clock.Add(30*time.Second))
		}
		se.cycle()
		for _, m := range family(t, reg, constants.WVAUtilizationShareSpareGPUs) {
			return m.GetGauge().GetValue()
		}
		t.Fatal("no spare series published")
		return 0
	}
	if without, with := spare(0), spare(3); without-with != 3 {
		t.Fatalf("spare %v without the carve, %v with a 3-GPU carve: want 3 less", without, with)
	}
}
