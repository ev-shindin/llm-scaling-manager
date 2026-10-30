package saturation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Property tests for the analyzers' learners (docs/proposals/analyzer-learner.md,
// 2.6 and 2.7). Each case is a synthetic input run against one learner, on
// propSeeds; a case passes only when every seed does. A case that fails on
// today's code by design is a row of the known-failures ledger
// (testdata/properties/known-failures.yaml): it must still fail, and the
// build breaks once it passes, until its row is removed. Every case not in the
// ledger must pass. Nothing is skipped, and no tolerance is widened to pass a
// defect.
//
// Each mapping row of 2.7 has a mutant in property_mutants_test.go: the same
// check, run on a learner with the past defect put back. The class must fail
// on it.

// propClass is one of the twelve property classes of 2.6.
type propClass string

// The twelve classes, as 2.6 names them.
const (
	classTransient      propClass = "transient"
	classFirstSample    propClass = "first-sample"
	classStale          propClass = "stale-value"
	classMissing        propClass = "missing-duplicated"
	classHeterogeneity  propClass = "contamination-heterogeneity"
	classRedistribution propClass = "redistribution"
	classNDependence    propClass = "n-dependence"
	classKnownTruth     propClass = "known-truth"
	classScale          propClass = "scale"
	classLiveness       propClass = "liveness"
	classMonotonicity   propClass = "monotonicity"
	classEdges          propClass = "edges"
)

// allPropClasses is every class; TestPropertyEveryClassHasACase holds the
// registry to it.
var allPropClasses = []propClass{
	classTransient, classFirstSample, classStale, classMissing, classHeterogeneity,
	classRedistribution, classNDependence, classKnownTruth, classScale, classLiveness,
	classMonotonicity, classEdges,
}

// Input families. Every class runs on the time-base family as well as on
// direct inputs (2.6), and a policy key runs at a non-default value as well
// as at its default.
const (
	familyDirect   = "direct"
	familyTimeBase = "time-base"
)

// propCase is one property case: a class, the learner it runs on, and a check
// that returns the violation it found for a seed, or nil.
type propCase struct {
	class   propClass
	learner string
	name    string
	family  string
	// tuning is "default", or the non-default analyzerTuning it runs under.
	tuning string
	check  func(seed uint64) error
}

// id is the ledger's key for the case: class/learner/name.
func (c propCase) id() string { return string(c.class) + "/" + c.learner + "/" + c.name }

// propSeeds are the generator seeds every case runs on. A failure names its
// seed, so `go test -run 'TestLearnerProperties/<id>'` reproduces it.
var propSeeds = []uint64{1, 2, 3, 4, 5}

// runProp runs check on every seed and returns the first violation, seed
// named.
func runProp(check func(seed uint64) error) error {
	for _, s := range propSeeds {
		if err := check(s); err != nil {
			return fmt.Errorf("seed %d: %w", s, err)
		}
	}
	return nil
}

// ledgerRow is one known failure: (class, learner, finding, measured result,
// the PR that clears it or "open"), keyed by the case it names.
type ledgerRow struct {
	ID       string `json:"id"`
	Class    string `json:"class"`
	Learner  string `json:"learner"`
	Finding  string `json:"finding"`
	Measured string `json:"measured"`
	Owner    string `json:"owner"`
}

const ledgerPath = "testdata/properties/known-failures.yaml"

func loadLedger(t *testing.T) []ledgerRow {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(ledgerPath))
	if err != nil {
		t.Fatalf("read the known-failures ledger: %v", err)
	}
	var rows []ledgerRow
	if err := yaml.UnmarshalStrict(b, &rows); err != nil {
		t.Fatalf("parse %s: %v", ledgerPath, err)
	}
	return rows
}

// propOutcome is what one case did: its violation, nil when it passed.
type propOutcome struct {
	c   propCase
	err error
}

// reconcileLedger holds the outcomes to the ledger and returns every breach:
// a ledgered case that passed, a case outside the ledger that failed, a row
// naming no case, a row whose class or learner disagrees with its case, a row
// missing a field, and a row listed twice.
func reconcileLedger(outcomes []propOutcome, rows []ledgerRow) []error {
	var errs []error
	byID := map[string]ledgerRow{}
	for _, r := range rows {
		if _, dup := byID[r.ID]; dup {
			errs = append(errs, fmt.Errorf("ledger: %s is listed twice", r.ID))
		}
		byID[r.ID] = r
		if r.Finding == "" || r.Measured == "" || r.Owner == "" {
			errs = append(errs, fmt.Errorf("ledger: %s needs a finding, a measured result and an owner (a PR, or \"open\")", r.ID))
		}
	}
	seen := map[string]bool{}
	for _, o := range outcomes {
		id := o.c.id()
		seen[id] = true
		r, listed := byID[id]
		switch {
		case listed && (r.Class != string(o.c.class) || r.Learner != o.c.learner):
			errs = append(errs, fmt.Errorf("ledger: %s says class %q learner %q; the case is %q %q",
				id, r.Class, r.Learner, o.c.class, o.c.learner))
		case listed && o.err == nil:
			errs = append(errs, fmt.Errorf("%s passes, and the ledger says it fails (%s, owner %s): remove its row",
				id, r.Finding, r.Owner))
		case !listed && o.err != nil:
			errs = append(errs, fmt.Errorf("%s [%s, tuning %s]: %v", id, o.c.family, o.c.tuning, o.err))
		}
	}
	for _, r := range rows {
		if !seen[r.ID] {
			errs = append(errs, fmt.Errorf("ledger: %s names no property case: a row with no failing case is an error", r.ID))
		}
	}
	return errs
}

// TestLearnerProperties runs every property case and holds the outcomes to
// the known-failures ledger.
func TestLearnerProperties(t *testing.T) {
	cases := propertyCases()
	outcomes := make([]propOutcome, 0, len(cases))
	for _, c := range cases {
		var err error
		t.Run(c.id(), func(t *testing.T) {
			err = runProp(c.check)
			if err != nil {
				t.Logf("fails [%s, tuning %s]: %v", c.family, c.tuning, err)
			} else {
				t.Logf("passes [%s, tuning %s]", c.family, c.tuning)
			}
		})
		outcomes = append(outcomes, propOutcome{c: c, err: err})
	}
	for _, e := range reconcileLedger(outcomes, loadLedger(t)) {
		t.Error(e)
	}
}

// Every class has a case, on each family, and some case runs a non-default
// analyzerTuning; ids are unique.
func TestPropertyEveryClassHasACase(t *testing.T) {
	cases := propertyCases()
	ids := map[string]bool{}
	families := map[propClass]map[string]bool{}
	nonDefault := 0
	for _, c := range cases {
		if ids[c.id()] {
			t.Errorf("property case %s is registered twice", c.id())
		}
		ids[c.id()] = true
		if families[c.class] == nil {
			families[c.class] = map[string]bool{}
		}
		families[c.class][c.family] = true
		if c.tuning != "default" {
			nonDefault++
		}
	}
	for _, cl := range allPropClasses {
		if len(families[cl]) == 0 {
			t.Errorf("class %s has no property case", cl)
		}
	}
	if nonDefault == 0 {
		t.Error("no property case runs a non-default analyzerTuning")
	}
	timeBase := 0
	for _, f := range families {
		if f[familyTimeBase] {
			timeBase++
		}
	}
	if timeBase == 0 {
		t.Error("no property case runs on the time-base family")
	}
}

// The ledger's own negative controls: a row removed from the ledger, a passing
// case added to it, a row for no case, and a row with no owner each break the
// reconciliation.
func TestPropertyLedgerReconcileNegativeControls(t *testing.T) {
	fail := propCase{class: classEdges, learner: "x", name: "fails"}
	pass := propCase{class: classEdges, learner: "x", name: "passes"}
	outcomes := []propOutcome{{c: fail, err: errors.New("violated")}, {c: pass}}
	good := []ledgerRow{{ID: fail.id(), Class: "edges", Learner: "x", Finding: "f", Measured: "m", Owner: "open"}}
	if errs := reconcileLedger(outcomes, good); len(errs) != 0 {
		t.Fatalf("a matching ledger reconciles: %v", errs)
	}
	for name, rows := range map[string][]ledgerRow{
		"a failing case's row removed": nil,
		"a passing case added": append(append([]ledgerRow{}, good...),
			ledgerRow{ID: pass.id(), Class: "edges", Learner: "x", Finding: "f", Measured: "m", Owner: "open"}),
		"a row naming no case": append(append([]ledgerRow{}, good...),
			ledgerRow{ID: "edges/x/gone", Class: "edges", Learner: "x", Finding: "f", Measured: "m", Owner: "open"}),
		"a row with no owner":        {{ID: fail.id(), Class: "edges", Learner: "x", Finding: "f", Measured: "m"}},
		"a row on the wrong learner": {{ID: fail.id(), Class: "edges", Learner: "y", Finding: "f", Measured: "m", Owner: "open"}},
	} {
		if errs := reconcileLedger(outcomes, rows); len(errs) == 0 {
			t.Errorf("%s: the reconciliation passed", name)
		}
	}
}

// The committed ledger names only registered cases, and is sorted by id so a
// diff of it reads in order.
func TestPropertyLedgerIsSorted(t *testing.T) {
	rows := loadLedger(t)
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("ledger rows are not sorted by id:\n%s", strings.Join(ids, "\n"))
	}
}
