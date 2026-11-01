package learn

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The list Forget walks names each learner once.
func TestLearnersNamesEachLearnerOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range Learners() {
		if n == "" || seen[n] {
			t.Fatalf("Learners() lists %q twice or empty: %v", n, Learners())
		}
		seen[n] = true
	}
	if !seen[K2] {
		t.Fatalf("Learners() = %v, want %s on it", Learners(), K2)
	}
}

// A clone shares nothing a later write could reach.
func TestReportCloneIsDeep(t *testing.T) {
	r := Report{Counts: map[Reason]int64{"P1-obs": 1}, Forgotten: []Forgotten{{Key: "k", By: ByOperator}},
		LastAdmitted: time.Unix(1, 0)}
	c := r.Clone()
	c.Counts["P1-obs"] = 9
	c.Forgotten[0].By = ByEvicted
	if r.Counts["P1-obs"] != 1 || r.Forgotten[0].By != ByOperator {
		t.Fatalf("the clone wrote through to the original: %+v", r)
	}
}

// learn imports only the standard library (analyzer-learner.md 6.3), so
// domain and internal/metrics can use its types without a cycle.
func TestLearnImportsOnlyTheStandardLibrary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s, outside the standard library", e.Name(), p)
			}
		}
	}
	if files == 0 {
		t.Fatal("no source file read")
	}
}
