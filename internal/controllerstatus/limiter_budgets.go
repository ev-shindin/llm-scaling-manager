package controllerstatus

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// KeyLimiterBudgets is the GPU budget the steady-state engine used in its last
// cycle, JSON (LimiterBudgets): per namespace-scope limiter entry, namespace
// and accelerator, the guaranteed and borrowable GPUs, what is used, and why
// borrowable is 0. Absent until the engine's first cycle with a limiter.
const KeyLimiterBudgets = "limiterBudgets"

// MaxLimiterBudgetEntries bounds the limiterBudgets value: entries past it, in
// (limiter, namespace, accelerator) order, are dropped and counted in
// Truncated, so the ConfigMap stays far below its 1 MiB limit.
const MaxLimiterBudgetEntries = 200

// LimiterBudget is one entry of KeyLimiterBudgets.
type LimiterBudget struct {
	Limiter     string `json:"limiter"`
	Namespace   string `json:"namespace"`
	Accelerator string `json:"accelerator"`
	// Guaranteed is the cap without borrowing; -1 is unlimited.
	Guaranteed int `json:"guaranteed"`
	// Borrowable is what the namespace may draw past Guaranteed now, were no
	// other namespace to draw first.
	Borrowable int `json:"borrowable"`
	Used       int `json:"used"`
	// Reason is why Borrowable is 0, for an entry that reads Kueue borrowing.
	Reason string `json:"reason,omitempty"`
	// AsOf is when the engine first used these figures (RFC3339): it moves
	// only when one of them changes, so a quiet controller rewrites nothing.
	AsOf string `json:"asOf"`
}

// LimiterBudgets is the value of KeyLimiterBudgets.
type LimiterBudgets struct {
	Entries []LimiterBudget `json:"entries"`
	// Truncated is how many entries were dropped past MaxLimiterBudgetEntries.
	Truncated int `json:"truncated,omitempty"`
}

func budgetKey(b LimiterBudget) string {
	return b.Limiter + "\x00" + b.Namespace + "\x00" + b.Accelerator
}

// StampLimiterBudgets sorts cur, keeps each entry's AsOf from prev where its
// figures did not change and sets it to now where they did, and caps the
// result at MaxLimiterBudgetEntries.
func StampLimiterBudgets(prev LimiterBudgets, cur []LimiterBudget, now time.Time) LimiterBudgets {
	before := make(map[string]LimiterBudget, len(prev.Entries))
	for _, b := range prev.Entries {
		before[budgetKey(b)] = b
	}
	out := slices.Clone(cur)
	slices.SortFunc(out, func(a, b LimiterBudget) int {
		return cmp.Or(strings.Compare(a.Limiter, b.Limiter), strings.Compare(a.Namespace, b.Namespace),
			strings.Compare(a.Accelerator, b.Accelerator))
	})
	stamp := now.UTC().Format(time.RFC3339)
	for i := range out {
		out[i].AsOf = stamp
		if p, ok := before[budgetKey(out[i])]; ok {
			asOf := p.AsOf
			p.AsOf = stamp
			if p == out[i] {
				out[i].AsOf = asOf
			}
		}
	}
	res := LimiterBudgets{Entries: out}
	if len(out) > MaxLimiterBudgetEntries {
		res.Entries, res.Truncated = out[:MaxLimiterBudgetEntries], len(out)-MaxLimiterBudgetEntries
	}
	return res
}

// SetLimiterBudgets records the budgets the engine used (StampLimiterBudgets'
// result). Nil entries remove the key: no limiter is in force.
func (p *Publisher) SetLimiterBudgets(b LimiterBudgets) {
	if p == nil {
		return
	}
	if len(b.Entries) == 0 {
		p.update(func(d map[string]string) { delete(d, KeyLimiterBudgets) })
		return
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return
	}
	p.update(func(d map[string]string) { d[KeyLimiterBudgets] = string(raw) })
}
