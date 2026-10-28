package cycleclock

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// learnedStatePackages are the packages whose learned state is stamped and
// aged on the cycle clock, relative to this one.
var learnedStatePackages = []string{
	"../capacity",
	"../../engines/analyzers/saturation",
	"../../engines/analyzers/throughput",
	"../../engines/analyzers/slo",
}

// wallClockAllowed is every read of the wall clock those packages may keep,
// keyed "<package dir>/<file>:<func>", with the reason it is not learned state.
// Empty: a new entry needs a reason a reviewer can check.
var wallClockAllowed = map[string]string{}

// TestNoWallClockInLearnedState fails on any time.Now, time.Since or
// time.Until -- called, or taken as a function value -- in the non-test files
// of the learned-state packages. One such read is enough to make a replay
// depend on when it ran: the stamp it takes, or the age it measures, is wall
// time while every other one is the cycle's.
func TestNoWallClockInLearnedState(t *testing.T) {
	var found []string
	for _, dir := range learnedStatePackages {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		scanned := 0
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			scanned++
			found = append(found, wallClockReads(t, dir, path)...)
		}
		// An empty directory scans nothing and passes: a moved package must
		// fail here instead.
		if scanned == 0 {
			t.Fatalf("no Go files in %s: the package moved, and this test no longer reads it", dir)
		}
	}
	var unexpected []string
	for _, f := range found {
		if _, ok := wallClockAllowed[strings.SplitN(f, " ", 2)[0]]; !ok {
			unexpected = append(unexpected, f)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("%d wall-clock reads in learned-state code; read the analyzer's clock, or take now from the caller:\n%s",
			len(unexpected), strings.Join(unexpected, "\n"))
	}
}

// wallClockReads returns each time.Now, time.Since and time.Until in the file
// at path, as "<dir>/<file>:<func> <line>: time.<name>".
func wallClockReads(t *testing.T, dir, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	timeName := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "time" {
			timeName = "time"
			if imp.Name != nil {
				timeName = imp.Name.Name
			}
		}
	}
	if timeName == "" || timeName == "_" {
		return nil
	}
	var out []string
	for _, decl := range f.Decls {
		fn := "<package>"
		if d, ok := decl.(*ast.FuncDecl); ok {
			fn = d.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || x.Name != timeName {
				return true
			}
			switch sel.Sel.Name {
			case "Now", "Since", "Until":
				key := filepath.Base(dir) + "/" + filepath.Base(path) + ":" + fn
				out = append(out, key+" "+strconv.Itoa(fset.Position(sel.Pos()).Line)+": time."+sel.Sel.Name)
			}
			return true
		})
	}
	return out
}

// TestWallClockReadsFindsEachForm pins the scanner itself: a scanner that
// finds nothing passes TestNoWallClockInLearnedState on any code.
func TestWallClockReadsFindsEachForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	for _, tc := range []struct {
		name, imp, pkg string
	}{
		{name: "plain import", imp: `"time"`, pkg: "time"},
		{name: "aliased import", imp: `tm "time"`, pkg: "tm"},
	} {
		src := "package x\n\nimport " + tc.imp + "\n\nvar clock = PKG.Now\n\nfunc f(t PKG.Time) {\n" +
			"\t_ = PKG.Now()\n\t_ = PKG.Since(t)\n\t_ = PKG.Until(t)\n\t_ = PKG.Duration(0)\n}\n"
		src = strings.ReplaceAll(src, "PKG", tc.pkg)
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(dir) + "/x.go:"
		want := []string{
			base + "<package> 5: time.Now",
			base + "f 8: time.Now",
			base + "f 9: time.Since",
			base + "f 10: time.Until",
		}
		if got := wallClockReads(t, dir, path); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: got\n%s\nwant\n%s", tc.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}
