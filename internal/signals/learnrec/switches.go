package learnrec

import (
	"fmt"
	"sort"
	"strings"
)

// UnknownSwitches is what a package's SetSwitchesForReplay returns for a set
// of switch names when it has none of them: nil for an empty set, and an
// error naming every name, sorted, otherwise. A replay switch is a test seam
// for one named change (docs/proposals/analyzer-learner.md, 2.5); no package
// has one yet.
func UnknownSwitches(pkg string, switches map[string]bool) error {
	if len(switches) == 0 {
		return nil
	}
	names := make([]string, 0, len(switches))
	for n := range switches {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("%s: no replay switch %s", pkg, strings.Join(names, ", "))
}
