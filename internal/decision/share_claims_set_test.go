package decision

import (
	"testing"
	"time"
)

// Matching backtracks: the decode, larger and so matched first, prefers the
// best-off receiver's transfer X; but X is the only one the prefill's 4-GPU
// pod fits, so the decode must take Y instead. Greedily the pair is refused.
// Control: TestShareClaimStorePrefersTheBestOffReceiver.
func TestShareClaimStoreBacktracks(t *testing.T) {
	t0 := time.Unix(0, 0)
	cluster := ShareGroupKey("", "A100")
	s := claimStore(map[string][]ShareClaimable{cluster: {
		{ID: "X", ReceiverZ: -0.1, DonorGPUs: 8, DonorPodGPUs: []int{4, 4}},
		{ID: "Y", ReceiverZ: -0.5, DonorGPUs: 6, DonorPodGPUs: []int{3, 3}},
	}}, t0)
	pair := []ShareWake{
		{Pods: []int{3, 3}, GPUs: 6, Variant: "ns/dec"},
		{Pods: []int{4}, GPUs: 4, Variant: "ns/pre"},
	}
	claims, outcome := s.ClaimSet("", "A100", pair, -1, "ns/m", t0)
	if outcome != ShareClaimRedirected || len(claims) != 2 {
		t.Fatalf("want the pair claimed, got %q %v", outcome, claims)
	}
	for _, c := range claims {
		if want := map[string]string{"ns/dec": "Y", "ns/pre": "X"}[c.Wake]; c.ID != want {
			t.Fatalf("%s claimed %s, want %s: %v", c.Wake, c.ID, want, claims)
		}
	}
}

// The greedy choice itself, for the record: alone, the decode takes X.
func TestShareClaimStorePrefersTheBestOffReceiver(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := claimStore(map[string][]ShareClaimable{ShareGroupKey("", "A100"): {
		{ID: "X", ReceiverZ: -0.1, DonorGPUs: 8, DonorPodGPUs: []int{4, 4}},
		{ID: "Y", ReceiverZ: -0.5, DonorGPUs: 6, DonorPodGPUs: []int{3, 3}},
	}}, t0)
	claims, _ := s.ClaimSet("", "A100", []ShareWake{{Pods: []int{3, 3}, GPUs: 6, Variant: "ns/dec"}}, -1, "ns/m", t0)
	if len(claims) != 1 || claims[0].ID != "X" {
		t.Fatalf("want X, got %v", claims)
	}
}

// A claim no engine takes -- its group went to shadow or away -- expires, so
// it cannot redirect a transfer restored much later under the same ID.
func TestShareClaimStoreDropsUntakenClaims(t *testing.T) {
	t0 := time.Unix(0, 0)
	cluster := ShareGroupKey("", "A100")
	s := claimStore(map[string][]ShareClaimable{cluster: {{ID: "x", ReceiverZ: 0, DonorGPUs: 1}}}, t0)
	if _, outcome := s.Claim("", "A100", nil, 1, -1, "ns/m", "ns/w", t0); outcome != ShareClaimRedirected {
		t.Fatalf("setup: %q", outcome)
	}
	s.Publish(nil, t0.Add(ShareClaimMaxAge))
	if got := s.Take("", "A100"); len(got) != 1 {
		t.Fatalf("within the bound the claim stays: %v", got)
	}
	s = claimStore(map[string][]ShareClaimable{cluster: {{ID: "y", ReceiverZ: 0, DonorGPUs: 1}}}, t0)
	if _, outcome := s.Claim("", "A100", nil, 1, -1, "ns/m", "ns/w", t0); outcome != ShareClaimRedirected {
		t.Fatalf("setup: %q", outcome)
	}
	s.Publish(nil, t0.Add(ShareClaimMaxAge+time.Second))
	if got := s.Take("", "A100"); len(got) != 0 {
		t.Fatalf("an untaken claim past the bound was kept: %v", got)
	}
}
