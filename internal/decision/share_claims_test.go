package decision

import (
	"testing"
	"time"
)

func claimStore(entries map[string][]ShareClaimable, at time.Time) *ShareClaimStore {
	s := &ShareClaimStore{}
	s.Publish(entries, at)
	return s
}

// A wake claims a releasing transfer only when it scores strictly lower than
// the receiver and its replica fits the donor pods; it takes the best-off
// receiver's, and a claimed transfer cannot be claimed twice.
func TestShareClaimStore(t *testing.T) {
	t0 := time.Unix(0, 0)
	cluster := ShareGroupKey("", "A100")
	entries := map[string][]ShareClaimable{cluster: {
		{ID: "low", ReceiverZ: -0.5, DonorGPUs: 8, DonorPodGPUs: []int{8}},
		{ID: "high", ReceiverZ: -0.2, DonorGPUs: 8, DonorPodGPUs: []int{8}},
	}}

	s := claimStore(entries, t0)
	c, outcome := s.Claim("", "A100", []int{8}, 8, -1, "ns/m1", "ns/w1", t0)
	if outcome != ShareClaimRedirected || c.ID != "high" || c.Scope != "" {
		t.Fatalf("want the best-off receiver's transfer redirected, got %q %+v", outcome, c)
	}
	if c, outcome = s.Claim("", "A100", []int{8}, 8, -1, "ns/m2", "ns/w2", t0); c.ID != "low" {
		t.Fatalf("second claim: want the other transfer, got %q %+v", outcome, c)
	}
	if _, outcome = s.Claim("", "A100", []int{8}, 8, -1, "ns/m3", "ns/w3", t0); outcome != ShareClaimNoneReleasing {
		t.Fatalf("both claimed: want none-releasing, got %q", outcome)
	}
	if got := s.Take("", "A100"); len(got) != 2 {
		t.Fatalf("want two claims to take, got %v", got)
	}
	if got := s.Take("", "A100"); len(got) != 0 {
		t.Fatalf("claims are taken once, got %v", got)
	}

	for _, tc := range []struct {
		name    string
		z       float64
		pods    []int
		gpus    int
		at      time.Time
		outcome string
	}{
		{"a tie goes to the receiver", -0.2, []int{8}, 8, t0, ShareClaimRefusedScore},
		{"a better-off wake does not displace", 0, []int{8}, 8, t0, ShareClaimRefusedScore},
		{"two 8-GPU pods do not fit one 8-GPU hole", -1, []int{8, 8}, 16, t0, ShareClaimRefusedFit},
		{"a stale list allows nothing", -1, []int{8}, 8, t0.Add(ShareClaimMaxAge + time.Second), ShareClaimNoneReleasing},
	} {
		s := claimStore(map[string][]ShareClaimable{cluster: {{ID: "x", ReceiverZ: -0.2, DonorGPUs: 8, DonorPodGPUs: []int{8}}}}, t0)
		if _, got := s.Claim("", "A100", tc.pods, tc.gpus, tc.z, "ns/m", "ns/w", tc.at); got != tc.outcome {
			t.Errorf("%s: outcome %q, want %q", tc.name, got, tc.outcome)
		}
	}
}

// A wake claims only in its own group: a namespace with its own quota never
// reaches the cluster group's transfers, however much it would score there.
func TestShareClaimStoreStaysInTheWakesGroup(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := claimStore(map[string][]ShareClaimable{
		ShareGroupKey("", "A100"): {{ID: "cl-t", ReceiverZ: -0.2, DonorGPUs: 1}},
	}, t0)
	if _, outcome := s.Claim("ns", "A100", nil, 1, -1, "ns/m", "ns/w", t0); outcome != ShareClaimNoneReleasing {
		t.Fatalf("a namespace-quota wake reached the cluster group: %q", outcome)
	}
	// Control: a cluster-scoped wake claims it.
	if c, outcome := s.Claim("", "A100", nil, 1, -1, "ns/m", "ns/w", t0); outcome != ShareClaimRedirected || c.ID != "cl-t" {
		t.Fatalf("control: want the cluster transfer claimed, got %q %+v", outcome, c)
	}
}

// One model claims at most once per hold: a woken model stays inactive until
// KEDA acts, and without the hold the 10 Hz wake loop would claim every
// releasing transfer in reach.
func TestShareClaimStoreHoldsAModelAfterAClaim(t *testing.T) {
	t0 := time.Unix(0, 0)
	cluster := ShareGroupKey("", "A100")
	s := claimStore(map[string][]ShareClaimable{cluster: {
		{ID: "a", ReceiverZ: -0.2, DonorGPUs: 1}, {ID: "b", ReceiverZ: -0.2, DonorGPUs: 1},
	}}, t0)
	if _, outcome := s.Claim("", "A100", nil, 1, -1, "ns/m", "ns/w", t0); outcome != ShareClaimRedirected {
		t.Fatalf("first claim: %q", outcome)
	}
	if _, outcome := s.Claim("", "A100", nil, 1, -1, "ns/m", "ns/w", t0.Add(time.Second)); outcome != ShareClaimRefusedHeld {
		t.Fatalf("a second claim by the same model inside the hold: want refused-held, got %q", outcome)
	}
	// Control: another model is not held.
	if _, outcome := s.Claim("", "A100", nil, 1, -1, "ns/other", "ns/w2", t0.Add(time.Second)); outcome != ShareClaimRedirected {
		t.Fatalf("control: another model's claim: %q", outcome)
	}
}

// A claimed transfer is not published again before the engine takes the
// claim, or a second wake could claim the same hole.
func TestShareClaimStoreDoesNotRepublishAClaimedTransfer(t *testing.T) {
	t0 := time.Unix(0, 0)
	cluster := ShareGroupKey("", "A100")
	list := map[string][]ShareClaimable{cluster: {{ID: "x", ReceiverZ: -0.2, DonorGPUs: 1}}}
	s := claimStore(list, t0)
	if _, outcome := s.Claim("", "A100", nil, 1, -1, "ns/m1", "ns/w1", t0); outcome != ShareClaimRedirected {
		t.Fatalf("first claim: %q", outcome)
	}
	s.Publish(list, t0) // the engine's next pass still lists it
	if _, outcome := s.Claim("", "A100", nil, 1, -1, "ns/m2", "ns/w2", t0); outcome != ShareClaimNoneReleasing {
		t.Fatalf("the claimed transfer was claimable again: %q", outcome)
	}
}

// A P/D wake claims one transfer per replica, all or none: a decode and a
// prefill each need a hole of their own, larger replicas are matched first so
// a small one cannot take the only large hole, and a pair that does not fully
// fit claims nothing and leaves both transfers claimable.
func TestShareClaimStoreClaimsASetAllOrNone(t *testing.T) {
	t0 := time.Unix(0, 0)
	cluster := ShareGroupKey("", "A100")
	pair := []ShareWake{{Pods: []int{2}, GPUs: 2, Variant: "ns/pre"}, {Pods: []int{8}, GPUs: 8, Variant: "ns/dec"}}

	s := claimStore(map[string][]ShareClaimable{cluster: {
		// The small hole has the best-off receiver: taken first for the 8-GPU
		// replica it would be, were it large enough.
		{ID: "big", ReceiverZ: -0.5, DonorGPUs: 8, DonorPodGPUs: []int{8}},
		{ID: "small", ReceiverZ: -0.2, DonorGPUs: 8, DonorPodGPUs: []int{8}},
	}}, t0)
	claims, outcome := s.ClaimSet("", "A100", pair, -1, "ns/m", t0)
	if outcome != ShareClaimRedirected || len(claims) != 2 {
		t.Fatalf("want both replicas claimed, got %q %v", outcome, claims)
	}
	if claims[0].ID == claims[1].ID {
		t.Fatalf("two replicas claimed one hole: %v", claims)
	}
	if got := s.Take("", "A100"); len(got) != 2 {
		t.Fatalf("want two claims to take, got %v", got)
	}

	// Only one transfer: neither replica is claimed, and it stays claimable.
	s = claimStore(map[string][]ShareClaimable{cluster: {{ID: "only", ReceiverZ: -0.2, DonorGPUs: 8, DonorPodGPUs: []int{8}}}}, t0)
	if claims, outcome = s.ClaimSet("", "A100", pair, -1, "ns/m", t0); outcome != ShareClaimRefusedFit || len(claims) != 0 {
		t.Fatalf("want a half pair refused, got %q %v", outcome, claims)
	}
	if got := s.Take("", "A100"); len(got) != 0 {
		t.Fatalf("a refused pair recorded claims: %v", got)
	}
	if c, outcome := s.Claim("", "A100", []int{8}, 8, -1, "ns/other", "ns/w", t0); outcome != ShareClaimRedirected || c.ID != "only" {
		t.Fatalf("the refused pair's transfer must stay claimable, got %q %+v", outcome, c)
	}

	// Largest first: the 8-GPU replica takes the only 8-GPU hole even though
	// the 2-GPU replica is listed first.
	s = claimStore(map[string][]ShareClaimable{cluster: {
		{ID: "two", ReceiverZ: -0.5, DonorGPUs: 2, DonorPodGPUs: []int{2}},
		{ID: "eight", ReceiverZ: -0.2, DonorGPUs: 8, DonorPodGPUs: []int{8}},
	}}, t0)
	claims, outcome = s.ClaimSet("", "A100", pair, -1, "ns/m", t0)
	if outcome != ShareClaimRedirected {
		t.Fatalf("want the pair claimed, got %q", outcome)
	}
	for _, c := range claims {
		if (c.Wake == "ns/dec") != (c.ID == "eight") {
			t.Fatalf("decode must take the 8-GPU hole: %v", claims)
		}
	}
}

// Claim is ClaimSet for a wake of one replica: the tests' shorthand.
func (s *ShareClaimStore) Claim(scope, accelerator string, wakePods []int, wakeGPUs int,
	z float64, model, wake string, now time.Time) (ShareClaim, string) {
	claims, outcome := s.ClaimSet(scope, accelerator, []ShareWake{{Pods: wakePods, GPUs: wakeGPUs, Variant: wake}},
		z, model, now)
	if len(claims) == 0 {
		return ShareClaim{}, outcome
	}
	return claims[0], outcome
}
