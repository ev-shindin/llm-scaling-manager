package controllerstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestStampLimiterBudgetsKeepsAsOfUntilAFigureChanges(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	a := LimiterBudget{Limiter: "kq", Namespace: "a", Accelerator: "H100", Guaranteed: 8, Borrowable: 4, Used: 10}
	b := LimiterBudget{Limiter: "kq", Namespace: "b", Accelerator: "H100", Guaranteed: 0, Reason: "not-kueue-managed"}

	first := StampLimiterBudgets(LimiterBudgets{}, []LimiterBudget{b, a}, t0)
	if len(first.Entries) != 2 || first.Entries[0].Namespace != "a" {
		t.Fatalf("want two entries sorted by namespace, got %+v", first.Entries)
	}
	for _, e := range first.Entries {
		if e.AsOf != "2026-10-15T12:00:00Z" {
			t.Fatalf("asOf = %q, want RFC3339 of the first use", e.AsOf)
		}
	}

	b.Borrowable, b.Reason = 2, ""
	second := StampLimiterBudgets(first, []LimiterBudget{a, b}, t0.Add(time.Minute))
	if got := second.Entries[0].AsOf; got != "2026-10-15T12:00:00Z" {
		t.Errorf("unchanged entry: asOf moved to %q", got)
	}
	if got := second.Entries[1].AsOf; got != "2026-10-15T12:01:00Z" {
		t.Errorf("changed entry: asOf = %q, want the time of the change", got)
	}
}

func TestStampLimiterBudgetsCapsTheEntries(t *testing.T) {
	cur := make([]LimiterBudget, 0, MaxLimiterBudgetEntries+5)
	for i := range MaxLimiterBudgetEntries + 5 {
		cur = append(cur, LimiterBudget{Limiter: "kq", Namespace: fmt.Sprintf("ns-%04d", i), Accelerator: "H100"})
	}
	got := StampLimiterBudgets(LimiterBudgets{}, cur, time.Unix(0, 0))
	if len(got.Entries) != MaxLimiterBudgetEntries || got.Truncated != 5 {
		t.Fatalf("got %d entries, truncated %d; want %d and 5", len(got.Entries), got.Truncated, MaxLimiterBudgetEntries)
	}
	raw, _ := json.Marshal(got)
	if len(raw) > 64<<10 {
		t.Errorf("limiterBudgets is %d bytes at the cap; keep it far below the ConfigMap limit", len(raw))
	}
}

// Review finding (on-call): the cap kept the first 200 entries in key order, so
// on a large cluster the withheld namespace on-call was looking for, or the one
// that just changed, could be the one left out.
func TestStampLimiterBudgetsKeepsWithheldAndChangedOverTheCap(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	cur := make([]LimiterBudget, 0, MaxLimiterBudgetEntries+10)
	for i := range MaxLimiterBudgetEntries + 10 {
		cur = append(cur, LimiterBudget{Limiter: "kq", Namespace: fmt.Sprintf("ns-%04d", i), Accelerator: "H100", Borrowable: 2})
	}
	first := StampLimiterBudgets(LimiterBudgets{}, cur, t0)

	last := len(cur) - 1
	// The last entry is withheld, the one before it changed: both are past
	// the cap in key order.
	cur[last].Borrowable, cur[last].Reason = 0, "not-kueue-managed"
	cur[last-1].Borrowable = 3
	second := StampLimiterBudgets(first, cur, t0.Add(time.Minute))
	names := map[string]bool{}
	for _, e := range second.Entries {
		names[e.Namespace] = true
	}
	if len(second.Entries) != MaxLimiterBudgetEntries || second.Truncated != 10 {
		t.Fatalf("got %d entries, truncated %d", len(second.Entries), second.Truncated)
	}
	if !names[cur[last].Namespace] {
		t.Errorf("the withheld entry %s was dropped by the cap", cur[last].Namespace)
	}
	if !names[cur[last-1].Namespace] {
		t.Errorf("the entry that just changed, %s, was dropped by the cap", cur[last-1].Namespace)
	}
	for i := 1; i < len(second.Entries); i++ {
		if second.Entries[i-1].Namespace > second.Entries[i].Namespace {
			t.Fatalf("kept entries are not in key order at %d", i)
		}
	}

	third := StampLimiterBudgets(second, cur, t0.Add(2*time.Minute))
	for _, e := range third.Entries {
		if e.Namespace == "ns-0000" && e.AsOf != t0.Format(time.RFC3339) {
			t.Errorf("an entry the cap dropped and kept again read as changed: asOf %q", e.AsOf)
		}
	}
}

func TestSetLimiterBudgetsWritesAndRemovesTheKey(t *testing.T) {
	w := &writes{}
	p, c := newTestPublisher(t, w)
	p.SetLimiterBudgets(StampLimiterBudgets(LimiterBudgets{}, []LimiterBudget{
		{Limiter: "kq", Namespace: "a", Accelerator: "H100", Guaranteed: 8, Borrowable: 4, Used: 10},
	}, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)))
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := `{"entries":[{"limiter":"kq","namespace":"a","accelerator":"H100","guaranteed":8,"borrowable":4,"used":10,"asOf":"2026-10-15T12:00:00Z"}]}`
	if got := stored(t, c).Data[KeyLimiterBudgets]; got != want {
		t.Fatalf("limiterBudgets =\n%s\nwant\n%s", got, want)
	}

	p.SetLimiterBudgets(LimiterBudgets{})
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored(t, c).Data[KeyLimiterBudgets]; ok {
		t.Error("no limiter in force: the key must go")
	}
}
