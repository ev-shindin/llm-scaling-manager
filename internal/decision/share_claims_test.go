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
	c, outcome := s.Claim("ns", "A100", []int{8}, 8, -1, -1, "ns/w", t0)
	if outcome != ShareClaimRedirected || c.ID != "high" || c.Scope != "" {
		t.Fatalf("want the best-off receiver's transfer redirected, got %q %+v", outcome, c)
	}
	if c, outcome = s.Claim("ns", "A100", []int{8}, 8, -1, -1, "ns/w2", t0); c.ID != "low" {
		t.Fatalf("second claim: want the other transfer, got %q %+v", outcome, c)
	}
	if _, outcome = s.Claim("ns", "A100", []int{8}, 8, -1, -1, "ns/w3", t0); outcome != ShareClaimNoneReleasing {
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
		if _, got := s.Claim("ns", "A100", tc.pods, tc.gpus, tc.z, tc.z, "ns/w", tc.at); got != tc.outcome {
			t.Errorf("%s: outcome %q, want %q", tc.name, got, tc.outcome)
		}
	}
}

// A model in a namespace with its own quota group claims there, with its
// score there; the cluster group is tried only after.
func TestShareClaimStoreTriesTheNamespaceGroupFirst(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := claimStore(map[string][]ShareClaimable{
		ShareGroupKey("ns", "A100"): {{ID: "ns-t", ReceiverZ: -0.2, DonorGPUs: 1}},
		ShareGroupKey("", "A100"):   {{ID: "cl-t", ReceiverZ: -0.2, DonorGPUs: 1}},
	}, t0)
	c, outcome := s.Claim("ns", "A100", nil, 1, -1, -1, "ns/w", t0)
	if outcome != ShareClaimRedirected || c.ID != "ns-t" || c.Scope != "ns" {
		t.Fatalf("want the namespace group's transfer, got %q %+v", outcome, c)
	}
}
