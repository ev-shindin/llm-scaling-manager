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

// MaxLimiterBudgetEntries bounds the limiterBudgets value: past it, entries
// are dropped and counted in Truncated, so the ConfigMap stays far below its
// 1 MiB limit. What stays is what on-call looks for first: entries whose
// borrowing is withheld (a reason), then the most recently changed, then the
// rest in (limiter, namespace, accelerator) order.
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
	// all is every entry stamped, the dropped ones included, so an entry the
	// cap left out keeps its asOf and does not read as changed next cycle.
	all []LimiterBudget
}

func budgetKey(b LimiterBudget) string {
	return b.Limiter + "\x00" + b.Namespace + "\x00" + b.Accelerator
}

// StampLimiterBudgets sorts cur, keeps each entry's AsOf from prev where its
// figures did not change and sets it to now where they did, and caps the
// result at MaxLimiterBudgetEntries, keeping withheld and recently changed
// entries over the others. The kept entries are in key order.
func StampLimiterBudgets(prev LimiterBudgets, cur []LimiterBudget, now time.Time) LimiterBudgets {
	known := prev.all
	if known == nil {
		known = prev.Entries
	}
	before := make(map[string]LimiterBudget, len(known))
	for _, b := range known {
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
	res := LimiterBudgets{Entries: out, all: out}
	if len(out) > MaxLimiterBudgetEntries {
		notWithheld := func(b LimiterBudget) int {
			if b.Reason != "" {
				return 0
			}
			return 1
		}
		ranked := slices.Clone(out)
		slices.SortStableFunc(ranked, func(a, b LimiterBudget) int {
			// Withheld first; then, RFC3339 in UTC sorting as time, the newest
			// change first; the stable sort keeps key order within a tie.
			return cmp.Or(cmp.Compare(notWithheld(a), notWithheld(b)), strings.Compare(b.AsOf, a.AsOf))
		})
		kept := ranked[:MaxLimiterBudgetEntries]
		slices.SortFunc(kept, func(a, b LimiterBudget) int { return strings.Compare(budgetKey(a), budgetKey(b)) })
		res.Entries, res.Truncated = kept, len(out)-MaxLimiterBudgetEntries
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
