package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// quotaConstraints is a cluster quota of A100s with free left, and, when nsFree
// is non-negative, a namespace quota for selNS with nsFree left.
func quotaConstraints(free, nsFree int) []*allocation.ResourceConstraints {
	c := &allocation.ResourceConstraints{ProviderName: "quota",
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: 16 - free}}}
	if nsFree >= 0 {
		c.NamespacePools = map[string]map[string]allocation.ResourcePool{
			selNS: {"A100": {Limit: 4, Used: 4 - nsFree}}}
	}
	return []*allocation.ResourceConstraints{c}
}

// publishClaimable makes one transfer claimable in each named scope ("" is
// the cluster group), and clears what the test leaves behind.
func publishClaimable(t *testing.T, scopes ...string) {
	t.Helper()
	clear := func() {
		for _, s := range append(scopes, "", selNS) {
			decision.DefaultShareClaims.Take(s, "A100")
		}
		decision.DefaultShareClaims.Publish(nil, time.Now())
	}
	clear()
	t.Cleanup(clear)
	byGroup := map[string][]decision.ShareClaimable{}
	for _, s := range scopes {
		byGroup[decision.ShareGroupKey(s, "A100")] = []decision.ShareClaimable{{ID: "t-" + s, ReceiverZ: -0.3, DonorGPUs: 1}}
	}
	decision.DefaultShareClaims.Publish(byGroup, time.Now())
}

func wakeCandidates() []Candidate {
	return []Candidate{cand("dec", domain.RoleDecode, "A100", 1, 1)}
}

// A wake claims a releasing transfer only when no capacity can host it: with
// room, it starts on the room and the transfer stays the steady-state engine's.
func TestSelectOrClaimClaimsOnlyWithoutCapacity(t *testing.T) {
	e := &Engine{config: shareConfig(t, false)}

	publishClaimable(t, "")
	set, outcome := e.selectOrClaim(context.Background(), modelGroup{namespace: selNS, modelID: "roomy"},
		wakeCandidates(), coverage{}, quotaConstraints(4, -1))
	if len(set) != 1 || outcome == OutcomeNoCapacity {
		t.Fatalf("setup: with room the wake selects directly, got %v %v", set, outcome)
	}
	if got := decision.DefaultShareClaims.Take("", "A100"); len(got) != 0 {
		t.Fatalf("a wake with room claimed %v", got)
	}

	publishClaimable(t, "")
	set, outcome = e.selectOrClaim(context.Background(), modelGroup{namespace: selNS, modelID: "full"},
		wakeCandidates(), coverage{}, quotaConstraints(0, -1))
	if outcome != OutcomeNoCapacity || len(set) != 1 {
		t.Fatalf("with no capacity the wake should start by a claim, got %v %v", set, outcome)
	}
	if got := decision.DefaultShareClaims.Take("", "A100"); len(got) != 1 {
		t.Fatalf("no claim recorded: %v", got)
	}
}

// A namespace with its own quota claims in its namespace group, never in the
// cluster group.
func TestClaimShareTransferStaysInItsNamespaceGroup(t *testing.T) {
	e := &Engine{config: shareConfig(t, false)}
	publishClaimable(t, "", selNS)
	set, ok := e.claimShareTransfer(context.Background(), modelGroup{namespace: selNS, modelID: "nsm"},
		wakeCandidates(), quotaConstraints(0, 0), coverage{})
	if !ok || len(set) != 1 {
		t.Fatalf("no claim in the namespace group: %v %v", ok, set)
	}
	if got := decision.DefaultShareClaims.Take(selNS, "A100"); len(got) != 1 || got[0].ID != "t-"+selNS {
		t.Fatalf("namespace group claims %v, want t-%s", got, selNS)
	}
	if got := decision.DefaultShareClaims.Take("", "A100"); len(got) != 0 {
		t.Fatalf("the cluster group's transfer was claimed by a namespace-quota model: %v", got)
	}
}

// A refused claim is counted once per change of outcome -- the wake loop runs
// at 10 Hz -- under the scope it was refused in.
func TestClaimShareTransferCountsARefusalOncePerChange(t *testing.T) {
	r := prometheus.NewRegistry()
	if err := metrics.InitMetrics(r); err != nil {
		t.Fatal(err)
	}
	count := func(scope, outcome string) float64 {
		t.Helper()
		mfs, err := r.Gather()
		if err != nil {
			t.Fatal(err)
		}
		sum := 0.0
		for _, mf := range mfs {
			if mf.GetName() != constants.WVAUtilizationShareClaimsTotal {
				continue
			}
			for _, m := range mf.GetMetric() {
				var s, o string
				for _, l := range m.GetLabel() {
					switch l.GetName() {
					case constants.LabelScope:
						s = l.GetValue()
					case constants.LabelOutcome:
						o = l.GetValue()
					}
				}
				if s == scope && o == outcome {
					sum += m.GetCounter().GetValue()
				}
			}
		}
		return sum
	}

	e := &Engine{config: shareConfig(t, false)}
	group := modelGroup{namespace: selNS, modelID: "refused"}
	publishClaimable(t) // nothing releasing
	for range 10 {
		if _, ok := e.claimShareTransfer(context.Background(), group, wakeCandidates(), quotaConstraints(0, 0), coverage{}); ok {
			t.Fatal("claimed with nothing releasing")
		}
	}
	if got := count(selNS, decision.ShareClaimNoneReleasing); got != 1 {
		t.Fatalf("refusal counted %v times under scope %s, want once", got, selNS)
	}
	if got := count(constants.UtilizationShareClusterScope, decision.ShareClaimNoneReleasing); got != 0 {
		t.Fatalf("a namespace-quota refusal counted under the cluster scope (%v)", got)
	}

	// The outcome changes: a releasing transfer whose receiver is worse off.
	decision.DefaultShareClaims.Publish(map[string][]decision.ShareClaimable{
		decision.ShareGroupKey(selNS, "A100"): {{ID: "x", ReceiverZ: -100, DonorGPUs: 1}},
	}, time.Now())
	for range 10 {
		e.claimShareTransfer(context.Background(), group, wakeCandidates(), quotaConstraints(0, 0), coverage{})
	}
	if got := count(selNS, decision.ShareClaimRefusedScore); got != 1 {
		t.Fatalf("the changed refusal counted %v times, want once", got)
	}
}
