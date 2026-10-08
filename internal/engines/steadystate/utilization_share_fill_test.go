package steadystate

import (
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// The idle fill places a receiver with an exclusive topology in one domain:
// two 4-GPU pods with 4 free GPUs on each of two racks fit only across them,
// so the receiver is not filled. Control: the same receiver without a domain
// is filled.
func TestFillIdleSharePlacesADomainReceiverInOneDomain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		domains map[string]string
		want    int
	}{
		{"exclusive topology: no rack holds both pods", map[string]string{"B": "rack"}, 0},
		{"no domain (control)", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{}
			e.utilizationShare.desired = map[string]int{}
			g := allocation.ShareGroup{
				AcceleratorType: "A100", Budget: 16, PhysicalFree: math.MaxInt,
				Roles: []allocation.ShareRole{{Key: "B", Weight: 1, Need: 16, Ceiling: 16, ReplicaGPUs: 8}},
				Grow:  map[string]allocation.ShareVariant{"B": {Name: "b", GPUs: 8, PodGPUs: []int{4, 4}}},
			}
			ev := allocation.ShareEvaluation{Roles: []allocation.ShareRoleVerdict{{Key: "B", Integer: 16}}}
			nodes := map[string]allocation.ShareNode{
				"n1": {Free: 4, Labels: map[string]string{"rack": "r1"}},
				"n2": {Free: 4, Labels: map[string]string{"rack": "r2"}},
			}
			key := func(role, variant string) string { return role + "/" + variant }
			e.fillIdleShare(logr.Discard(), allocation.NewShareLedger(), g, ev, map[string]int{"B": 8}, nodes,
				tc.domains, key, time.Unix(0, 0), allocation.ShareTimings{FillTimeout: time.Minute})
			if got := e.utilizationShare.desired["B/b"]; got != tc.want {
				t.Fatalf("B raised by %d, want %d", got, tc.want)
			}
		})
	}
}
