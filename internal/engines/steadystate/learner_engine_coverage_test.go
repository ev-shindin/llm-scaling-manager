package steadystate

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The engine layer's coverage test (issue #345, step 0; the analyzers' is
// learnreplay's coverage_test.go). The analyzers are objects whose exported
// methods are their inputs; the composite is a function, and its inputs are
// its arguments. So the test holds the composite to its arguments: the
// functions a replay calls to rebuild a composite read nothing but what they
// are passed -- no engine state, no package variable, no wall clock -- and
// composeSignal takes exactly the arguments the composite event records. A
// new input fails here until the event records it.

// replayRoots are the functions LearnerReplay calls to rebuild a composite.
var replayRoots = []string{"composeSignal", "namedResultScored", "metadataByVariant", "warmPoolSupply",
	"variantPressure", "compositeEvent"}

// composeSignalParams are composeSignal's parameters, each recorded by the
// composite event: the entries (Entries, rebuilt by namedResultScored),
// the eligible ones (Eligible and Grace), and saturation's thresholds
// (ScaleUp, ScaleDown). The context carries the logger only.
var composeSignalParams = []string{"ctx", "namedResults", "eligibleAnalyzers", "scaleUp", "scaleDown"}

func TestEngineRecording_TheCompositeReadsOnlyItsArguments(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	funcs := map[string]*ast.FuncDecl{}
	methods := map[string][]*ast.FuncDecl{}
	vars := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					funcs[d.Name.Name] = d
				} else {
					methods[d.Name.Name] = append(methods[d.Name.Name], d)
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					for _, n := range s.(*ast.ValueSpec).Names {
						if n.Name != "_" {
							vars[n.Name] = true
						}
					}
				}
			}
		}
	}
	require.NotEmpty(t, vars, "precondition: the package has variables to find")

	reached := map[*ast.FuncDecl]bool{}
	var problems []string
	var visit func(d *ast.FuncDecl)
	visit = func(d *ast.FuncDecl) {
		if reached[d] {
			return
		}
		reached[d] = true
		name := d.Name.Name
		if d.Recv != nil {
			recv := d.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if id, ok := recv.(*ast.Ident); ok && id.Name == "Engine" {
				problems = append(problems, name+" is a method of the engine: it reads engine state the recording does not carry")
				return
			}
		}
		ast.Inspect(d.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == "time" && n.Sel.Name == "Now" {
					problems = append(problems, name+" reads the wall clock")
				}
			case *ast.Ident:
				if vars[n.Name] {
					problems = append(problems, name+" reads the package variable "+n.Name)
				}
			case *ast.CallExpr:
				switch fn := n.Fun.(type) {
				case *ast.Ident:
					if callee, ok := funcs[fn.Name]; ok {
						visit(callee)
					}
				case *ast.SelectorExpr:
					for _, m := range methods[fn.Sel.Name] {
						visit(m)
					}
				}
			}
			return true
		})
	}
	for _, root := range replayRoots {
		d, ok := funcs[root]
		require.True(t, ok, "the replay root %s is gone", root)
		visit(d)
	}
	assert.Empty(t, problems, "the composite reads an input the composite event does not record")
	assert.GreaterOrEqual(t, len(reached), 25, "the walk reached only %d functions: it is not following the calls", len(reached))
	t.Logf("%d functions reached from the replay's roots", len(reached))

	params := make([]string, 0, len(composeSignalParams))
	for _, f := range funcs["composeSignal"].Type.Params.List {
		for _, n := range f.Names {
			params = append(params, n.Name)
		}
	}
	assert.Equal(t, composeSignalParams, params,
		"composeSignal's arguments changed: record the new one in enginerec.CompositeEvent and replay it")
}

// Every engine event kind is one LearnerReplay applies.
func TestEngineRecording_TheReplayOwnsEveryEngineKind(t *testing.T) {
	r := NewLearnerReplay()
	for _, k := range enginerec.Kinds() {
		assert.True(t, r.Owns(k), k)
	}
	assert.False(t, r.Owns("saturation.Analyze"))
	// The composite event's fields are pinned: one added is a reviewed
	// change, replayed and covered by the export's tables.
	typ := reflect.TypeFor[enginerec.CompositeEvent]()
	fields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Name)
	}
	assert.Equal(t, []string{"Namespace", "ModelID", "Entries", "Eligible", "Grace", "Metadata", "ScaleUp", "ScaleDown",
		"Composite", "HoldReasons", "Supply", "Pressure"}, fields)
}

// skewedEngine is the engine's replay with every analyzer result it reads
// doubled in demand: a negative control for the chain from the analyzers'
// replayed results to the composite.
type skewedEngine struct{ *LearnerReplay }

func (s skewedEngine) Apply(ctx context.Context, ev learnrec.RawEvent,
	results func(analyzer, namespace, modelID string) (*domain.AnalyzerResult, bool), rec *learnrec.Recorder) error {
	skewed := func(a, ns, m string) (*domain.AnalyzerResult, bool) {
		res, ok := results(a, ns, m)
		if ok {
			c := *res
			c.TotalDemand *= 2
			c.VariantCapacities = slices.Clone(c.VariantCapacities)
			for i := range c.VariantCapacities {
				c.VariantCapacities[i].TotalDemand *= 2
			}
			res = &c
		}
		return res, ok
	}
	return s.LearnerReplay.Apply(ctx, ev, skewed, rec)
}

// Negative controls for the fidelity check: the composite is rebuilt from
// the analyzers' replayed results, not copied from the recording, so a
// skewed result shows in every composite; and a recorded input edited by
// hand shows in the composite it feeds.
func TestEngineRecording_FidelityCatchesAMovedComposite(t *testing.T) {
	recording, _ := recordEngineRun(t, recStart, true)
	n := kindCounts(t, recording)[enginerec.KindComposite]

	rep, err := learnreplay.Replay(context.Background(), recording,
		learnreplay.Options{Mode: learnreplay.Fidelity, Engine: skewedEngine{NewLearnerReplay()}})
	require.NoError(t, err)
	moved := 0
	for _, m := range rep.Mismatches {
		if m.Kind == enginerec.KindComposite {
			moved++
		}
	}
	assert.Equal(t, n, moved, "a composite the skewed results did not move")

	// One composite's saturation scale-up threshold edited in the recording.
	lines := strings.SplitAfter(string(recording), "\n")
	edited := -1
	for i, l := range lines {
		if strings.Contains(l, `"Kind":"`+enginerec.KindComposite+`"`) && strings.Contains(l, `,"ScaleUp":0.85,"ScaleDown":0.7,"Composite"`) {
			lines[i] = strings.Replace(l, `,"ScaleUp":0.85,"ScaleDown":0.7,"Composite"`, `,"ScaleUp":0.5,"ScaleDown":0.7,"Composite"`, 1)
			edited = i
			break
		}
	}
	require.GreaterOrEqual(t, edited, 0, "precondition: a composite at the 0.85 threshold")
	rep, err = learnreplay.Replay(context.Background(), []byte(strings.Join(lines, "")),
		learnreplay.Options{Mode: learnreplay.Fidelity, Engine: NewLearnerReplay()})
	require.NoError(t, err)
	require.Len(t, rep.Mismatches, 1, mismatchText(rep.Mismatches))
	assert.Equal(t, enginerec.KindComposite, rep.Mismatches[0].Kind)
	assert.Equal(t, "line", rep.Mismatches[0].What)
}
