package accelerator

import "testing"

const (
	hbm3 = "NVIDIA-H100-80GB-HBM3"
	sxm5 = "NVIDIA-H100-SXM5-80GB"
	pcie = "NVIDIA-H100-PCIE-80GB"
)

func TestFamilyTree(t *testing.T) {
	for _, c := range []struct {
		name   string
		family string
		isFam  bool
	}{
		{"H100", "H100", true},
		{"h100", "h100", true},
		{hbm3, "H100", false},
		{"nvidia-h100-80gb", "h100", false},
		{"nvidia-tesla-t4", "t4", false},
		{"Intel-Gaudi-2-96GB", "Gaudi-2", false},
	} {
		if got := Family(c.name); got != c.family {
			t.Errorf("Family(%q) = %q, want %q", c.name, got, c.family)
		}
		if got := IsFamily(c.name); got != c.isFam {
			t.Errorf("IsFamily(%q) = %v, want %v", c.name, got, c.isFam)
		}
	}
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		key, name string
		want      bool
	}{
		{"H100", "H100", true},
		{"H100", "h100", true},
		{"H100", hbm3, true}, // a family covers its products
		{"h100", "nvidia-h100-80gb", true},
		{hbm3, "H100", false}, // a product never covers its family
		{hbm3, sxm5, false},   // nor a sibling product
		{hbm3, "nvidia-h100-80gb-hbm3", true},
		{"H100", "NVIDIA-A100-SXM4-80GB", false},
		{"A100", hbm3, false},
	} {
		if got := Covers(c.key, c.name); got != c.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", c.key, c.name, got, c.want)
		}
	}
}

func TestFindBudgetKey(t *testing.T) {
	for _, c := range []struct {
		desc    string
		known   map[string]int
		name    string
		wantKey string
		wantOK  bool
	}{
		{"exact", map[string]int{"H100": 1}, "H100", "H100", true},
		{"case fold", map[string]int{"H100": 1}, "h100", "H100", true},
		{"a product falls back to its family", map[string]int{"H100": 1}, hbm3, "H100", true},
		{"a listed product is its own key", map[string]int{"H100": 1, hbm3: 1}, hbm3, hbm3, true},
		{"an unlisted product beside a listed sibling is denied",
			map[string]int{"H100": 5, hbm3: 4, sxm5: 4}, pcie, "", false},
		{"products only: an unlisted product is denied",
			map[string]int{hbm3: 4, sxm5: 4}, pcie, "", false},
		{"a family name reaches a product key", map[string]int{sxm5: 4, hbm3: 4}, "H100", hbm3, true},
		{"another family is not found", map[string]int{"H100": 1}, "A100", "", false},
		{"GKE's Tesla spelling", map[string]int{"nvidia-tesla-t4": 1}, "t4", "nvidia-tesla-t4", true},
		{"two Tesla products never meet", map[string]int{"nvidia-tesla-t4": 1}, "nvidia-tesla-a100", "", false},
	} {
		key, ok := FindBudgetKey(c.known, c.name)
		if key != c.wantKey || ok != c.wantOK {
			t.Errorf("%s: FindBudgetKey(%v, %q) = (%q, %v), want (%q, %v)",
				c.desc, c.known, c.name, key, ok, c.wantKey, c.wantOK)
		}
	}
}
