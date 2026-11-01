package decision

import (
	"maps"
	"testing"
	"time"
)

func TestPromisedHolesTotalAndMerge(t *testing.T) {
	a := PromisedHoles{Nodes: map[string]int{"n0": 8, "n1": 1}, Unplaced: 2}
	b := PromisedHoles{Nodes: map[string]int{"n1": 3, "n2": 4}, Unplaced: 1}
	if got := a.Total(); got != 11 {
		t.Fatalf("Total = %d, want 11", got)
	}
	m := a.Merge(b)
	if !maps.Equal(m.Nodes, map[string]int{"n0": 8, "n1": 4, "n2": 4}) || m.Unplaced != 3 || m.Total() != 19 {
		t.Fatalf("Merge = %+v, want n0=8 n1=4 n2=4 unplaced 3", m)
	}
	m.Nodes["n0"] = 0
	if a.Nodes["n0"] != 8 || b.Nodes["n1"] != 3 {
		t.Fatalf("Merge aliased a side: a %v, b %v", a.Nodes, b.Nodes)
	}
}

// What LatestHoles returns is the caller's: changing it, or what was
// published, does not change the store.
func TestSharePromisedStoreCopies(t *testing.T) {
	var s SharePromisedStore
	now := time.Unix(100, 0)
	in := map[string]map[string]PromisedHoles{"": {"H100": {Nodes: map[string]int{"n0": 8}, Unplaced: 1}}}
	s.Publish(in, now)
	in[""]["H100"].Nodes["n0"] = 0
	out := s.LatestHoles(time.Minute, now)
	out[""]["H100"].Nodes["n0"] = 0
	out[""]["H100"] = PromisedHoles{}
	got := s.LatestHoles(time.Minute, now)[""]["H100"]
	if got.Nodes["n0"] != 8 || got.Unplaced != 1 {
		t.Fatalf("store holds %+v after its input and output were changed, want n0=8 unplaced 1", got)
	}
	if c := s.Latest(time.Minute, now)[""]["H100"]; c != 9 {
		t.Fatalf("Latest = %d, want 9", c)
	}
	if s.LatestHoles(time.Minute, now.Add(2*time.Minute)) != nil {
		t.Fatal("a snapshot older than maxAge was returned")
	}
}
