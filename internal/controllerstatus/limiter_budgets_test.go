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
