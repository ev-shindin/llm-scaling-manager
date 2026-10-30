package saturation

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// Property tests for the analyzers' learners (docs/proposals/analyzer-learner.md,
// 2.6 and 2.7). Each case is a synthetic input run against one learner on
// many seeds. A check returns a *propViolation when the learner breaks the
// property, and any other error when the case itself could not run; the
// latter never counts as a known failure.
//
// A case that fails on today's code by design is a row of the known-failures
// ledger (testdata/properties/known-failures.yaml), with the violation kind
// it is expected to fail with. Each subtest judges its own case against the
// ledger, so a filtered -run judges only what it ran:
//   - a case not in the ledger fails its subtest on any violation;
//   - a ledgered case must fail with its expected kind, and fails its subtest
//     if it fails with another kind, or (on a full seed run) not at all.
//
// The cases are in property_<learner>_test.go, the generators in
// property_gen_test.go, the mutants and the 2.7 accounting in
// property_mutants_test.go, and the coverage table in
// property_coverage_test.go.

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

// allPropClasses is every class.
var allPropClasses = []propClass{
	classTransient, classFirstSample, classStale, classMissing, classHeterogeneity,
	classRedistribution, classNDependence, classKnownTruth, classScale, classLiveness,
	classMonotonicity, classEdges,
}

// propFamily is an input family. Every class runs on the time-base family as
// well as on direct inputs (2.6), unless property_coverage_test.go exempts
// it with a reason.
type propFamily string

const (
	familyDirect   propFamily = "direct"
	familyTimeBase propFamily = "time-base"
)

// propTuning names the analyzerTuning a case runs under.
type propTuning string

const (
	tunedDefault    propTuning = "default"
	tunedNonDefault propTuning = "steadyFor 10m, carry 15m, idleRows 5"
	// tunedPolicy is a case that runs the policy keys it lists (propCase.keys)
	// at values other than their defaults.
	tunedPolicy propTuning = "non-default policy keys"
)

// Violation kinds. A ledger row names the kind its case is expected to fail
// with; a case failing with another kind is a new failure.
const (
	kindNotLearned  = "not-learned"        // the learner learned nothing: the check would be vacuous
	kindTransient   = "transient-admitted" // a transient set or moved a figure
	kindAdoptedRaw  = "adopted-raw"        // one reading set a figure a second one moves
	kindStale       = "stale"              // a figure did not converge on identifying data
	kindMoved       = "moved"              // a figure moved where the property says it stays
	kindBiased      = "biased"             // a figure is off its declared estimand or truth
	kindOverPriced  = "over-priced"        // priced capacity over truth x (1 + epsilon)
	kindNDependent  = "n-dependent"        // an expectation moved with N
	kindFlipped     = "flipped"            // a verdict or key reversed more than the path allows
	kindFell        = "fell"               // demand fell along a rising trajectory
	kindUnbounded   = "unbounded"          // demand over what the fleet can drain
	kindNotDetected = "not-detected"       // a refusal the property needs did not happen
	kindWrongSide   = "wrong-side"         // a verdict on the wrong side of its threshold
)

// propViolation is a broken property: the kind, and what was measured.
type propViolation struct {
	kind, msg string
}

func (v *propViolation) Error() string { return v.kind + ": " + v.msg }

// violation returns a *propViolation of kind.
func violation(kind, format string, args ...any) error {
	return &propViolation{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// holds is nil when cond holds, and a violation of kind otherwise.
func holds(cond bool, kind, format string, args ...any) error {
	if cond {
		return nil
	}
	return violation(kind, format, args...)
}

// propCase is one property case: a class, the learner it runs on, and a check
// that returns the violation it found for a seed, or nil.
type propCase struct {
	class   propClass
	learner string
	name    string
	family  propFamily
	tuning  propTuning
	// policy is the policy the case runs, set where it sets any key away
	// from its default, nil otherwise. The check reads the same value, so
	// the keys it sets (nonDefaultKeys) are the keys the case runs at a
	// non-default value: 2.6 requires one such case per key a learner reads
	// (policyKeys).
	policy *config.ScalingPolicy
	// seeds is how many seeds the case runs on; zero is defaultSeeds.
	seeds int
	check func(seed uint64) error
}

// id is the ledger's key for the case: class/learner/name.
func (c propCase) id() string { return string(c.class) + "/" + c.learner + "/" + c.name }

// Seed counts. A cheap case runs on cheapSeeds; one that drives the analyzer
// for minutes of simulated time on defaultSeeds. Changing a generator's draws
// moves which seeds fail: a ledger row's measured rate is restated then.
const (
	defaultSeeds = 20
	cheapSeeds   = 100
)

// propSeedEnv runs every case on the one seed it names, to reproduce a
// failure: PROPSEED=7 go test -run 'TestLearnerProperties/<id>'. A ledgered
// case that passes on that one seed is not an error then.
const propSeedEnv = "PROPSEED"

func seedsFor(c propCase) (seeds []uint64, full bool, err error) {
	if s := os.Getenv(propSeedEnv); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("%s=%q: %w", propSeedEnv, s, err)
		}
		return []uint64{n}, false, nil
	}
	n := c.seeds
	if n == 0 {
		n = defaultSeeds
	}
	seeds = make([]uint64, n)
	for i := range seeds {
		seeds[i] = uint64(i + 1)
	}
	return seeds, true, nil
}

// caseRun is what a case did over its seeds.
type caseRun struct {
	seeds    int
	failures []seedFailure
}

type seedFailure struct {
	seed uint64
	v    *propViolation
}

func (r caseRun) String() string {
	if len(r.failures) == 0 {
		return fmt.Sprintf("passes on %d/%d seeds", r.seeds, r.seeds)
	}
	f := r.failures[0]
	return fmt.Sprintf("fails on %d/%d seeds; first, seed %d: %v", len(r.failures), r.seeds, f.seed, f.v)
}

// runCase runs check on every seed. A non-violation error is the case failing
// to run: it is returned as err and never counted as a violation.
func runCase(check func(uint64) error, seeds []uint64) (caseRun, error) {
	run := caseRun{seeds: len(seeds)}
	for _, s := range seeds {
		err := check(s)
		var v *propViolation
		switch {
		case err == nil:
		case errors.As(err, &v):
			run.failures = append(run.failures, seedFailure{seed: s, v: v})
		default:
			return run, fmt.Errorf("seed %d: the case did not run: %w", s, err)
		}
	}
	return run, nil
}

// judge holds one case's run to its ledger row (nil when it has none). full
// says every seed ran; a ledgered case that passes is an error only then.
func judge(c propCase, row *ledgerRow, run caseRun, full bool) []error {
	var errs []error
	if row == nil {
		for i, f := range run.failures {
			if i == 3 {
				errs = append(errs, fmt.Errorf("... and %d more seeds", len(run.failures)-3))
				break
			}
			errs = append(errs, fmt.Errorf("%s [%s, %s] seed %d: %v", c.id(), c.family, c.tuning, f.seed, f.v))
		}
		if len(errs) > 0 {
			errs = append(errs, fmt.Errorf("%s: a new failure. Fix the learner; only a known defect with an owning PR becomes a ledger row (docs/developer-guide/testing.md)", c.id()))
		}
		return errs
	}
	if len(run.failures) == 0 && full {
		errs = append(errs, fmt.Errorf("%s passes on all %d seeds, and the ledger says it fails (%s, owner %s). If the learner was fixed, remove the row; if the case changed, check it still tests the finding",
			c.id(), run.seeds, row.Finding, row.Owner))
	}
	for _, f := range run.failures {
		if f.v.kind != row.Expect {
			errs = append(errs, fmt.Errorf("%s seed %d fails with %q, and its ledger row expects %q: a new failure: %v",
				c.id(), f.seed, f.v.kind, row.Expect, f.v))
			break
		}
	}
	return errs
}

// ledgerRow is one known failure: (class, learner, finding, measured result,
// the PR that clears it or "open"), keyed by the case it names, with the
// violation kind the case fails with.
type ledgerRow struct {
	ID       string `json:"id"`
	Class    string `json:"class"`
	Learner  string `json:"learner"`
	Expect   string `json:"expect"`
	Finding  string `json:"finding"`
	Measured string `json:"measured"`
	Owner    string `json:"owner"`
}

const ledgerPath = "testdata/properties/known-failures.yaml"

func loadLedger(t *testing.T) map[string]*ledgerRow {
	t.Helper()
	rows := readLedger(t)
	out := make(map[string]*ledgerRow, len(rows))
	for i := range rows {
		out[rows[i].ID] = &rows[i]
	}
	return out
}

func readLedger(t *testing.T) []ledgerRow {
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

// checkLedger holds the ledger's rows to the registered cases: each names a
// case, agrees with its class and learner, carries every field, is listed
// once, and the rows are sorted by id.
func checkLedger(rows []ledgerRow, cases []propCase) []error {
	byID := make(map[string]propCase, len(cases))
	for _, c := range cases {
		byID[c.id()] = c
	}
	var errs []error
	seen := map[string]bool{}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		if seen[r.ID] {
			errs = append(errs, fmt.Errorf("ledger: %s is listed twice", r.ID))
		}
		seen[r.ID] = true
		if r.Expect == "" || r.Finding == "" || r.Measured == "" || r.Owner == "" {
			errs = append(errs, fmt.Errorf("ledger: %s needs an expected kind, a finding, a measured result and an owner (a PR, or \"open\")", r.ID))
		}
		c, ok := byID[r.ID]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("ledger: %s names no property case: a row with no failing case is an error", r.ID))
		case r.Class != string(c.class) || r.Learner != c.learner:
			errs = append(errs, fmt.Errorf("ledger: %s says class %q learner %q; the case is %q %q", r.ID, r.Class, r.Learner, c.class, c.learner))
		}
	}
	if !slices.IsSorted(ids) {
		errs = append(errs, errors.New("ledger: rows are not sorted by id"))
	}
	return errs
}

// TestLearnerProperties runs every property case, each in its own subtest
// judged against its ledger row.
func TestLearnerProperties(t *testing.T) {
	ledger := loadLedger(t)
	for _, c := range propertyCases() {
		t.Run(c.id(), func(t *testing.T) {
			seeds, full, err := seedsFor(c)
			if err != nil {
				t.Fatal(err)
			}
			run, err := runCase(c.check, seeds)
			if err != nil {
				t.Fatalf("%s: %v", c.id(), err)
			}
			row := ledger[c.id()]
			if row != nil {
				t.Logf("[%s, %s] ledgered (expect %s, owner %s): %s; measured when listed: %s", c.family, c.tuning, row.Expect, row.Owner, run, row.Measured)
			} else {
				t.Logf("[%s, %s] %s", c.family, c.tuning, run)
			}
			for _, e := range judge(c, row, run, full) {
				t.Error(e)
			}
		})
	}
}

// TestPropertyLedgerNamesRegisteredCases checks the ledger file against the
// registered cases, whatever a -run filter selected.
func TestPropertyLedgerNamesRegisteredCases(t *testing.T) {
	for _, e := range checkLedger(readLedger(t), propertyCases()) {
		t.Error(e)
	}
}

// The judgement's negative controls: an unledgered failing case, a ledgered
// case passing on a full run, a ledgered case failing with another kind, a
// ledger row for no case or missing a field, and an infrastructure error each
// break it; a ledgered case passing on a filtered seed does not.
func TestPropertyJudgementNegativeControls(t *testing.T) {
	c := propCase{class: classEdges, learner: "x", name: "y"}
	row := &ledgerRow{ID: c.id(), Class: "edges", Learner: "x", Expect: kindFlipped, Finding: "f", Measured: "m", Owner: "open"}
	failing := caseRun{seeds: 2, failures: []seedFailure{{seed: 1, v: &propViolation{kind: kindFlipped, msg: "m"}}}}
	other := caseRun{seeds: 2, failures: []seedFailure{{seed: 1, v: &propViolation{kind: kindNotLearned, msg: "m"}}}}
	passing := caseRun{seeds: 2}
	for name, tc := range map[string]struct {
		row   *ledgerRow
		run   caseRun
		full  bool
		fails bool
	}{
		"ledgered and failing with its kind":         {row, failing, true, false},
		"unledgered and passing":                     {nil, passing, true, false},
		"ledgered and passing on one filtered seed":  {row, passing, false, false},
		"unledgered and failing":                     {nil, failing, true, true},
		"ledgered and passing on every seed":         {row, passing, true, true},
		"ledgered and failing with another kind":     {row, other, true, true},
		"ledgered, another kind, on a filtered seed": {row, other, false, true},
	} {
		if got := len(judge(c, tc.row, tc.run, tc.full)) > 0; got != tc.fails {
			t.Errorf("%s: judged failing %v, want %v", name, got, tc.fails)
		}
	}
	cases := []propCase{c}
	for name, rows := range map[string][]ledgerRow{
		"a row naming no case":     {*row, {ID: "edges/x/gone", Class: "edges", Learner: "x", Expect: "k", Finding: "f", Measured: "m", Owner: "open"}},
		"a row with no expect":     {{ID: c.id(), Class: "edges", Learner: "x", Finding: "f", Measured: "m", Owner: "open"}},
		"a row on another learner": {{ID: c.id(), Class: "edges", Learner: "z", Expect: "k", Finding: "f", Measured: "m", Owner: "open"}},
		"a row listed twice":       {*row, *row},
	} {
		if len(checkLedger(rows, cases)) == 0 {
			t.Errorf("%s: the ledger check passed", name)
		}
	}
	if errs := checkLedger([]ledgerRow{*row}, cases); len(errs) != 0 {
		t.Errorf("a matching ledger is clean: %v", errs)
	}
	if _, err := runCase(func(uint64) error { return errors.New("no rig") }, []uint64{1}); err == nil {
		t.Error("an infrastructure error was counted as a violation")
	}
}

// The registered case ids are pinned in testdata/properties/cases.txt, so a
// case dropped or renamed shows in the diff.
func TestPropertyCaseListIsPinned(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash("testdata/properties/cases.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields(string(b))
	got := map[string]bool{}
	for _, c := range propertyCases() {
		if got[c.id()] {
			t.Errorf("property case %s is registered twice", c.id())
		}
		got[c.id()] = true
	}
	if ids := slices.Sorted(maps.Keys(got)); !slices.Equal(ids, want) {
		t.Errorf("registered cases differ from testdata/properties/cases.txt; registered:\n%s", strings.Join(ids, "\n"))
	}
}
