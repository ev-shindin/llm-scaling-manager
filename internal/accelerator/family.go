package accelerator

import (
	"slices"
	"strings"
)

// GPU names form a two-level tree: a FAMILY ("H100") and the PRODUCTS under it
// ("NVIDIA-H100-80GB-HBM3", "NVIDIA-H100-SXM5-80GB", GKE's "nvidia-h100-80gb").
// A budget keyed by a family bounds the SUM of what every product under it
// spends; a budget keyed by a product bounds that product alone. Spellings that
// differ only in case are one node of the tree.
//
// The family of a name is its short name (NormalizeAcceleratorName). A name is a
// family when it is its own short name. That is a heuristic for vendor
// spellings it does not know -- an already-short "Gaudi-2" normalizes to "2" and
// so reads as a product of a family "2" -- and such a name simply never covers
// anything but its own spelling, as it never did.

// IsFamily reports whether name is a family (short) name, such as "H100", rather
// than a product name such as "NVIDIA-H100-80GB-HBM3".
func IsFamily(name string) bool {
	return NormalizeAcceleratorName(name) == name
}

// Family returns the family name of name: name itself when it is a family, else
// its short name.
func Family(name string) string {
	return NormalizeAcceleratorName(name)
}

// IsProductOf reports whether product is a product name under the family name
// family, ignoring case. A family is never a product of itself.
func IsProductOf(product, family string) bool {
	return IsFamily(family) && !IsFamily(product) && strings.EqualFold(Family(product), family)
}

// Covers reports whether a budget keyed key bounds what is spent under name:
// the same spelling ignoring case, or key is the family name is a product of.
// A product key never covers its family, nor another product.
func Covers(key, name string) bool {
	return strings.EqualFold(key, name) || IsProductOf(name, key)
}

// FindBudgetKey returns the key of a budget map that what a workload declares
// as name is spent from, and whether the map holds one.
//
// It is FindKey with one difference, which is what makes a merged budget a
// closed allowlist: a product name that is not a key does not fall back to its
// family key when the map lists another product of that family. Such a map was
// built from an allowlist that names products -- a quota granting
// "NVIDIA-H100-80GB-HBM3" and "NVIDIA-H100-SXM5-80GB" -- and its family key, if
// any, is the bound on their sum, not a grant of every other H100 product.
// FindKey would hand an "NVIDIA-H100-PCIE-80GB" workload that family key.
//
// The cost is that an allowlist naming a family AND some of its products (x:
// {H100: 8, NVIDIA-H100-80GB-HBM3: 2}) denies the products it does not name.
// Denying is the safe side of an ambiguity the map cannot resolve.
//
// Order: the exact key; a key equal ignoring case; for a family name, the
// lexically smallest of its product keys (as FindKey); for a product name, its
// family key when no sibling product is listed. Ties resolve to the lexically
// smallest key so the answer does not depend on map order.
func FindBudgetKey[V any](known map[string]V, name string) (string, bool) {
	if _, ok := known[name]; ok {
		return name, true
	}
	var caseEqual, products, families []string
	sibling := false
	for key := range known {
		switch {
		case strings.EqualFold(key, name):
			caseEqual = append(caseEqual, key)
		case IsProductOf(key, name):
			products = append(products, key)
		case IsProductOf(name, key):
			families = append(families, key)
		case !IsFamily(name) && !IsFamily(key) && strings.EqualFold(Family(key), Family(name)):
			sibling = true
		}
	}
	switch {
	case len(caseEqual) > 0:
		return slices.Min(caseEqual), true
	case len(products) > 0:
		return slices.Min(products), true
	case len(families) > 0 && !sibling:
		return slices.Min(families), true
	}
	return "", false
}
