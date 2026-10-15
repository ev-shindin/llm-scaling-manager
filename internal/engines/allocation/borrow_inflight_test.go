package allocation

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

func TestBlockCohorts(t *testing.T) {
	twoCohorts := func() *ResourceConstraints {
		c := borrowQuota(8, nsBorrow{ns: "a", cap: config.QuotaUnlimited})
		c.SharedPools = append(c.SharedPools, SharedPoolChunk{Cohort: "d", Flavor: "h100", Resource: "nvidia.com/gpu", Free: 6})
		return c
	}
	free := func(c *ResourceConstraints) map[string]int {
		out := map[string]int{}
		for _, p := range c.SharedPools {
			out[p.Cohort] = p.Free
		}
		return out
	}

	t.Run("blocks the listed cohort only, on a copy", func(t *testing.T) {
		in := twoCohorts()
		phys := physicalH100(16)
		got := BlockCohorts([]*ResourceConstraints{phys, nil, in}, map[string]bool{"c": true})
		if len(got) != 3 || got[0] != phys || got[1] != nil {
			t.Fatalf("a constraint without shared pools, or nil, must pass through as is: %v", got)
		}
		if f := free(got[2]); f["c"] != 0 || f["d"] != 6 {
			t.Errorf("blocked pools = %v, want c: 0 and d untouched at 6", f)
		}
		if f := free(in); f["c"] != 8 || f["d"] != 6 {
			t.Errorf("the input was mutated: %v", f)
		}
		if got[2] == in {
			t.Error("a blocked constraint must be a copy")
		}
	})

	t.Run("no cohort to block returns the input", func(t *testing.T) {
		in := []*ResourceConstraints{twoCohorts()}
		for _, cohorts := range []map[string]bool{nil, {}} {
			if got := BlockCohorts(in, cohorts); &got[0] != &in[0] {
				t.Errorf("BlockCohorts(%v) copied the slice; want the input", cohorts)
			}
		}
	})

	t.Run("a cycle without borrowing returns the input", func(t *testing.T) {
		in := []*ResourceConstraints{physicalH100(16)}
		if got := BlockCohorts(in, map[string]bool{"c": true}); &got[0] != &in[0] {
			t.Error("a pass with no borrowing must be returned as it is")
		}
	})

	t.Run("an unknown cohort blocks nothing", func(t *testing.T) {
		got := BlockCohorts([]*ResourceConstraints{twoCohorts()}, map[string]bool{"zz": true})
		if f := free(got[0]); f["c"] != 8 || f["d"] != 6 {
			t.Errorf("pools = %v, want both untouched", f)
		}
	})

	t.Run("a nil pool list stays nil", func(t *testing.T) {
		c := borrowQuota(8, nsBorrow{ns: "a", cap: config.QuotaUnlimited})
		c.SharedPools = nil
		got := BlockCohorts([]*ResourceConstraints{c}, map[string]bool{"c": true})
		if got[0] != c || got[0].SharedPools != nil {
			t.Errorf("a borrowing constraint with no pools must pass through: %+v", got[0])
		}
	})
}

// Entries is read every cycle to deduct in-flight draws and to log them; its
// order must not depend on map iteration, including between draws that share
// every key field but one.
func TestBorrowInFlightEntriesOrderIsStable(t *testing.T) {
	draws := make([]BorrowDraw, 0, 32)
	for _, ns := range []string{"b", "a"} {
		for _, v := range []string{"v2", "v1"} {
			for _, cohort := range []string{"d", "c"} {
				for _, flavor := range []string{"h200", "h100"} {
					for _, res := range []string{"nvidia.com/gpu", "amd.com/gpu"} {
						draws = append(draws, BorrowDraw{Namespace: ns, Variant: v, Cohort: cohort, Flavor: flavor, Resource: res, GPUs: 1, Replicas: 1, Target: 1})
					}
				}
			}
		}
	}
	key := func(d BorrowDraw) string {
		return fmt.Sprintf("%s/%s/%s/%s/%s", d.Namespace, d.Variant, d.Cohort, d.Flavor, d.Resource)
	}
	var first []string
	for i := range 20 {
		b := NewBorrowInFlight()
		// Insert in a rotated order each time.
		b.Record(append(append([]BorrowDraw{}, draws[i%len(draws):]...), draws[:i%len(draws)]...))
		entries := b.Entries()
		got := make([]string, 0, len(entries))
		for _, d := range entries {
			got = append(got, key(d))
		}
		if len(got) != len(draws) {
			t.Fatalf("Entries returned %d draws, want %d", len(got), len(draws))
		}
		for j := 1; j < len(got); j++ {
			if got[j-1] >= got[j] {
				t.Fatalf("Entries not sorted at %d: %s before %s", j, got[j-1], got[j])
			}
		}
		if first == nil {
			first = got
		} else if !reflect.DeepEqual(got, first) {
			t.Fatalf("Entries order changed between runs:\n%v\n%v", first, got)
		}
	}
	if e := (*BorrowInFlight)(nil).Entries(); e != nil {
		t.Errorf("a nil record returns %v, want nil", e)
	}
}

func TestCompareInFlightKeys(t *testing.T) {
	base := inFlightKey{"a", "v", "c", "f", "r"}
	if compareInFlightKeys(base, base) != 0 {
		t.Error("a key compares unequal to itself")
	}
	later := []inFlightKey{
		{"b", "a", "a", "a", "a"},
		{"a", "w", "a", "a", "a"},
		{"a", "v", "d", "a", "a"},
		{"a", "v", "c", "g", "a"},
		{"a", "v", "c", "f", "s"},
	}
	for _, k := range later {
		if compareInFlightKeys(base, k) >= 0 || compareInFlightKeys(k, base) <= 0 {
			t.Errorf("%v must order after %v, field by field", k, base)
		}
	}
}

// With no limiter at all, the engine publishes "nothing bounds you" so a warm
// pool waiting for a snapshot newer than its own charge can grow.
func TestPublishUnboundedHeadroom(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	now := time.Now()

	PublishUnboundedHeadroom(7, now)
	if _, state := decision.GPUHeadroomReading("any-ns", "H100", 7, time.Minute, now); state != decision.HeadroomUnbounded {
		t.Errorf("a namespace reads %v, want unbounded", state)
	}
	if _, state := decision.GPUHeadroomReading("any-ns", "H100", 8, time.Minute, now); state != decision.HeadroomUnknown {
		t.Errorf("a pool charged after the snapshot reads %v, want unknown", state)
	}
	h, ok := decision.DefaultHeadroom.Get()
	if !ok || h.Cluster != nil || len(h.Free) != 0 {
		t.Errorf("snapshot = %+v, want no namespace and no cluster bound", h)
	}
}
