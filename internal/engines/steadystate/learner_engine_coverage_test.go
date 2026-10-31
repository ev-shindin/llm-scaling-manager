package steadystate

import (
	"context"
	"go/ast"
	"go/types"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The engine layer's coverage test (issue #345, step 0; the analyzers' is
// learnreplay's coverage_test.go). The analyzers are objects whose exported
// methods are their inputs; the composite is a function, and its inputs are
// its arguments. So the test holds the composite to its arguments: the
// functions a replay calls to rebuild a composite, and every function of
// this module they reach, read nothing but what they are passed -- no
// engine state, no package-level variable of any package, no wall clock --
// and composeSignal takes exactly the arguments the replay rebuilds. A new
// input fails here until the replay rebuilds it or the event records it.

// modulePath is this module's import path.
const modulePath = "github.com/llm-d/llm-d-workload-variant-autoscaler/"

// walkedPackages are the packages whose functions the walk follows. A call
// into another package of this module fails the test until it is added
// here, so the walk cannot shrink without notice.
var walkedPackages = []string{
	modulePath + "internal/engines/steadystate",
	modulePath + "internal/engines/allocation",
	modulePath + "internal/engines/aggregation",
	modulePath + "internal/domain",
	modulePath + "internal/config",
	modulePath + "internal/engines/enginerec",
	modulePath + "internal/signals/learnrec",
}

// loggerOnly are the package variables the walk lets a function read: each
// holds a function that hands back the logger, and what is logged is no
// input.
var loggerOnly = map[string]bool{"sigs.k8s.io/controller-runtime.LoggerFrom": true}

// replayRoots are the functions LearnerReplay calls to rebuild a composite.
var replayRoots = []string{"composeSignal", "cycleEntry", "eligibleEntries", "compositeThresholds",
	"metadataByVariant", "warmPoolSupply", "variantPressure", "compositeEvent"}

// wallClock are the time package's functions that read the clock.
var wallClock = map[string]bool{"Now": true, "Since": true, "Until": true, "After": true, "AfterFunc": true,
	"Tick": true, "NewTicker": true, "NewTimer": true, "Sleep": true}

// composeSignalParams are composeSignal's parameters, each one the replay
// rebuilds: the entries (cycleEntry, from the replayed results, the
// recorded policy and liveness), the eligible ones (eligibleEntries, and the
// recorded grace entries), and saturation's thresholds
// (compositeThresholds). The context carries the logger only.
var composeSignalParams = []string{"ctx", "namedResults", "eligibleAnalyzers", "scaleUp", "scaleDown"}

// funcIndex is every function and method declared in the walked packages,
// by types.Func.FullName, with the type information of its package.
type funcIndex map[string]struct {
	decl *ast.FuncDecl
	info *types.Info
}

func loadWalked(t *testing.T) (funcIndex, map[string]*packages.Package) {
	t.Helper()
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
		packages.NeedTypesInfo | packages.NeedImports}
	pkgs, err := packages.Load(cfg, walkedPackages...)
	require.NoError(t, err)
	require.Len(t, pkgs, len(walkedPackages))
	index := funcIndex{}
	byPath := map[string]*packages.Package{}
	for _, p := range pkgs {
		require.Empty(t, p.Errors, "loading %s", p.PkgPath)
		byPath[p.PkgPath] = p
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func)
				require.True(t, ok, "precondition: %s has a type", fd.Name.Name)
				index[obj.FullName()] = struct {
					decl *ast.FuncDecl
					info *types.Info
				}{fd, p.TypesInfo}
			}
		}
	}
	return index, byPath
}

// impurities walks every function the roots reach and names each read of
// engine state, of a package-level variable, or of the wall clock, and each
// call into a package of this module the walk does not follow. reached is
// how many functions it walked.
func impurities(index funcIndex, roots []string) (problems []string, reached int) {
	seen := map[string]bool{}
	add := func(p string) {
		if !slices.Contains(problems, p) {
			problems = append(problems, p)
		}
	}
	var visit func(name string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		fn := index[name]
		if fn.decl.Recv != nil && strings.Contains(name, "steadystate.Engine)") {
			add(name + " is a method of the engine: it reads engine state the recording does not carry")
			return
		}
		ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			switch obj := fn.info.Uses[id].(type) {
			case *types.Var:
				if obj.Pkg() != nil && !obj.IsField() && obj.Parent() == obj.Pkg().Scope() &&
					!loggerOnly[obj.Pkg().Path()+"."+obj.Name()] {
					add(name + " reads the package variable " + obj.Pkg().Path() + "." + obj.Name())
				}
			case *types.Func:
				if obj.Pkg() == nil {
					return true
				}
				if obj.Pkg().Path() == "time" && wallClock[obj.Name()] {
					add(name + " reads the wall clock (time." + obj.Name() + ")")
					return true
				}
				callee := obj.Origin().FullName()
				if _, ok := index[callee]; ok {
					visit(callee)
					return true
				}
				sig, _ := obj.Type().(*types.Signature)
				dynamic := sig != nil && sig.Recv() != nil && types.IsInterface(sig.Recv().Type())
				if strings.HasPrefix(obj.Pkg().Path(), modulePath) && !dynamic {
					add(name + " calls " + callee + ", in a package the walk does not follow: add it to walkedPackages")
				}
			}
			return true
		})
	}
	for _, r := range roots {
		visit(modulePath + "internal/engines/steadystate." + r)
	}
	sort.Strings(problems)
	return problems, len(seen)
}

func TestEngineRecording_TheCompositeReadsOnlyItsArguments(t *testing.T) {
	index, byPath := loadWalked(t)
	for _, r := range replayRoots {
		_, ok := index[modulePath+"internal/engines/steadystate."+r]
		require.True(t, ok, "the replay root %s is gone", r)
	}
	problems, reached := impurities(index, replayRoots)
	assert.Empty(t, problems, "the composite reads an input the replay does not rebuild")
	assert.GreaterOrEqual(t, reached, 60, "the walk reached only %d functions: it is not following the calls", reached)
	t.Logf("%d functions reached from the replay's roots, across %d packages", reached, len(walkedPackages))

	steady := byPath[walkedPackages[0]]
	fn, ok := steady.Types.Scope().Lookup("composeSignal").(*types.Func)
	require.True(t, ok)
	sig := fn.Type().(*types.Signature)
	params := make([]string, 0, sig.Params().Len())
	for i := range sig.Params().Len() {
		params = append(params, sig.Params().At(i).Name())
	}
	assert.Equal(t, composeSignalParams, params,
		"composeSignal's arguments changed: rebuild the new one in the replay, or record it in enginerec.CompositeEvent")
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
	assert.Equal(t, []string{"Namespace", "ModelID", "Entries", "Grace", "Metadata",
		"Composite", "HoldReasons", "Supply", "Pressure"}, fields)
}

// skewed is a replay's reproduced state with every analyzer result doubled
// in demand: a negative control for the chain from the analyzers' replayed
// results to the composite.
type skewed struct{ enginerec.Replayed }

func (s skewed) Result(a, ns, m string) (*domain.AnalyzerResult, bool) {
	res, ok := s.Replayed.Result(a, ns, m)
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

// lowered is a replay's reproduced state with saturation's scale-up
// threshold lowered in every policy: a negative control for the thresholds
// the replay derives from the recorded policy.
type lowered struct{ enginerec.Replayed }

func (l lowered) Policy(ns, m string) (*config.ScalingPolicy, bool) {
	p, ok := l.Replayed.Policy(ns, m)
	if ok {
		c := *p
		c.ScaleUpThreshold = 0.5
		p = &c
	}
	return p, ok
}

// wrapped is the engine's replay with what the replay reproduced bent by
// bend.
type wrapped struct {
	*LearnerReplay
	bend func(enginerec.Replayed) enginerec.Replayed
}

func (w wrapped) Apply(ctx context.Context, ev learnrec.RawEvent, replayed enginerec.Replayed, rec *learnrec.Recorder) error {
	return w.LearnerReplay.Apply(ctx, ev, w.bend(replayed), rec)
}

// Negative controls for the fidelity check: the composite is rebuilt from
// the analyzers' replayed results and from the recorded policy, not copied
// from the recording, so a skewed result or a lowered threshold shows in
// every composite; and a recorded input edited by hand shows in the
// composite it feeds.
func TestEngineRecording_FidelityCatchesAMovedComposite(t *testing.T) {
	recording, _ := recordEngineRun(t, recStart, true)
	n := kindCounts(t, recording)[enginerec.KindComposite]
	moved := func(e EngineReplayFor) int {
		rep, err := learnreplay.Replay(context.Background(), recording, learnreplay.Options{Mode: learnreplay.Fidelity, Engine: e})
		require.NoError(t, err)
		k := 0
		for _, m := range rep.Mismatches {
			if m.Kind == enginerec.KindComposite {
				k++
			}
		}
		return k
	}
	assert.Equal(t, n, moved(wrapped{NewLearnerReplay(), func(r enginerec.Replayed) enginerec.Replayed { return skewed{r} }}),
		"a composite the skewed results did not move")
	assert.Equal(t, n, moved(wrapped{NewLearnerReplay(), func(r enginerec.Replayed) enginerec.Replayed { return lowered{r} }}),
		"a composite the lowered threshold did not move")

	// One composite's recorded metadata edited: agg-a clamped to one replica.
	lines := strings.SplitAfter(string(recording), "\n")
	const was, now = `{"VariantName":"agg-a","Role":"","CurrentReplicas":10}`, `{"VariantName":"agg-a","Role":"","CurrentReplicas":1}`
	edited := -1
	for i, l := range lines {
		if strings.Contains(l, `"Kind":"`+enginerec.KindComposite+`"`) && strings.Contains(l, was) {
			lines[i] = strings.Replace(l, was, now, 1)
			edited = i
			break
		}
	}
	require.GreaterOrEqual(t, edited, 0, "precondition: a composite with agg-a at 10 replicas")
	rep, err := learnreplay.Replay(context.Background(), []byte(strings.Join(lines, "")),
		learnreplay.Options{Mode: learnreplay.Fidelity, Engine: NewLearnerReplay()})
	require.NoError(t, err)
	require.Len(t, rep.Mismatches, 1, mismatchText(rep.Mismatches))
	assert.Equal(t, enginerec.KindComposite, rep.Mismatches[0].Kind)
	assert.Equal(t, "line", rep.Mismatches[0].What)
}

// EngineReplayFor is learnreplay.EngineReplayer, named here for the tests.
type EngineReplayFor = learnreplay.EngineReplayer
