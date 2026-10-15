package allocation

import (
	"math"
	"testing"
)

func TestBudgetTotalCountsACoveredSpellingOnce(t *testing.T) {
	const longH = "NVIDIA-H100-80GB-HBM3"
	if got := budgetTotal(map[string]int{"H100": 16, longH: 4}); got != 16 {
		t.Errorf("budgetTotal = %d, want 16: the long key is one of the 16 H100s", got)
	}
	if got := budgetTotal(map[string]int{"H100": 16, "A100": 4}); got != 20 {
		t.Errorf("budgetTotal = %d, want 20 for two different GPUs", got)
	}
	if got := budgetTotal(map[string]int{"H100": math.MaxInt, "A100": 4}); got != math.MaxInt {
		t.Errorf("budgetTotal = %d, want saturation at MaxInt", got)
	}
}

func TestDebitBudgetKeepsSpellingsConsistent(t *testing.T) {
	const longH = "NVIDIA-H100-80GB-HBM3"
	cases := []struct {
		name  string
		start map[string]int
		key   string
		n     int
		want  map[string]int
	}{
		{"long key debits its cover", map[string]int{"H100": 16, longH: 4}, longH, 3, map[string]int{"H100": 13, longH: 1}},
		{"short key clamps the long one", map[string]int{"H100": 16, longH: 4}, "H100", 14, map[string]int{"H100": 2, longH: 2}},
		{"never below zero", map[string]int{"H100": 2, longH: 2}, longH, 5, map[string]int{"H100": 0, longH: 0}},
		{"unbounded stays unbounded", map[string]int{"H100": math.MaxInt}, "H100", 5, map[string]int{"H100": math.MaxInt}},
		{"unbounded long key still debits a finite cover", map[string]int{"H100": 8, longH: math.MaxInt}, longH, 3,
			map[string]int{"H100": 5, longH: 5}},
		{"other GPUs untouched", map[string]int{"H100": 8, "A100": 4}, "H100", 3, map[string]int{"H100": 5, "A100": 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.start
			debitBudget(b, tc.key, tc.n)
			for k, v := range tc.want {
				if b[k] != v {
					t.Errorf("after debit %s -%d: %v, want %v", tc.key, tc.n, b, tc.want)
					break
				}
			}
		})
	}
}
