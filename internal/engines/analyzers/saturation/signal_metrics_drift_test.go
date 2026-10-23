package saturation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// "A signal in one of those three log lines and absent from the metric
// registry is a failure" -- docs/proposals/signals-as-metrics.md, "Making it
// not rot". Scoped to derived-mu, the line stage 3 publishes from; the other
// two (replica-capacity-decision, throughput-demand-floor) join this table
// when their families are built.
//
// derived-mu is logged per POD and the metrics are per VARIANT, so a mapping
// here says the field's variant-level counterpart is published, not that the
// two numbers are equal: rate on one pod is one reading of the median
// wva_service_rate_per_replica publishes.
//
// Every key of the log call must appear below, either mapped to a metric that
// InitMetrics registers or excluded with its reason. A key added to the log
// with no entry fails, and so does an entry for a key the log no longer has.
type derivedMuField struct {
	metric   string // the metric carrying the field's variant-level counterpart
	excluded string // or why no metric carries it
}

var derivedMuMetricFields = map[string]derivedMuField{
	"variant":         {metric: constants.WVAServiceRatePerReplica}, // the variant_name label every series carries
	"namespace":       {metric: constants.WVAServiceRatePerReplica}, // the namespace label every series carries
	"ok":              {metric: constants.WVAServiceRatePerReplica}, // a derived rate in use shows as source=derived or borrowed-line; ok=true alone does not mean it was used (an own measured window can win)
	"rate":            {metric: constants.WVAServiceRatePerReplica},
	"kPrice":          {metric: constants.WVAPricingK},
	"itlAtKPrice":     {metric: constants.WVAITLSecondsAtPricingK},
	"itlA":            {metric: constants.WVAITLLineSlope},
	"itlB":            {metric: constants.WVAITLLineIntercept},
	"itlZero":         {metric: constants.WVAITLLineSamples},        // no line is the slope's absence; the count explains it
	"maxNumSeqs":      {metric: constants.WVAEngineConfig},          // the max_num_seqs label
	"replicaKvTokens": {metric: constants.WVAKvCacheTokensCapacity}, // summed over the variant's pods

	"pod":              {excluded: "per-pod is the log's granularity; a pod label scales the series with the fleet being autoscaled"},
	"seqs":             {excluded: "per-pod intermediate of rate (k*C/KVreq capped at maxNumSeqs); rate carries the result"},
	"tokenSec":         {excluded: "per-pod intermediate of rate (rate = tokenSec / muDivisor)"},
	"engineRanks":      {excluded: "per-pod: the data-parallel ranks the row reports, which scale maxNumSeqs; the configured size is the data_parallel_size label of wva_engine_config"},
	"lineBorrowed":     {metric: constants.WVAServiceRatePerReplica}, // source=borrowed-line
	"lineBorrowedFrom": {excluded: "a lender variant name: a label value that changes with the lender, published as donor on wva_learned_state_borrowed (PR #152), not here"},
	"avgOutputTokens":  {excluded: "the shape family (stage 1, wva_shape_output_tokens), not built yet"},
	"muDivisor":        {excluded: "the shape family (stage 1, wva_shape_output_tokens{window}), not built yet"},
	"avgInputTokens":   {excluded: "the shape family (stage 1, wva_shape_input_tokens), not built yet"},
	"muInputTokens":    {excluded: "the shape family (stage 1, wva_shape_input_tokens{window}), not built yet"},
	"muShortWindow":    {excluded: "the shape family (stage 1, the window label), not built yet"},
	"kvReqPerSeq":      {excluded: "the shape family (stage 1, wva_shape_kv_tokens_per_request{basis=priced}), not built yet"},
	"kvReqFleet":       {excluded: "the shape family (stage 1, wva_shape_kv_tokens_per_request{basis=fleet}), not built yet"},
}

// derivedMuLogKeys parses analyze.go and returns the string-literal keys of
// the Info("derived-mu", ...) call.
func derivedMuLogKeys(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "analyze.go", nil, 0)
	require.NoError(t, err)

	var calls int
	var keys []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Info" {
			return true
		}
		if msg, ok := stringLit(call.Args[0]); !ok || msg != "derived-mu" {
			return true
		}
		calls++
		// Keys and values alternate after the message. A non-literal key is a
		// key this test cannot check, so it fails rather than being skipped.
		for i := 1; i < len(call.Args); i += 2 {
			key, ok := stringLit(call.Args[i])
			require.True(t, ok, "derived-mu argument %d is not a string-literal key at %s",
				i, fset.Position(call.Args[i].Pos()))
			keys = append(keys, key)
		}
		return true
	})
	require.Equal(t, 1, calls, "expected exactly one Info(\"derived-mu\", ...) call in analyze.go")
	// An empty extraction would compare clean against anything.
	require.NotEmpty(t, keys, "found the derived-mu call but extracted no keys")
	return keys
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// namesRegistry records the fully-qualified name of every metric registered
// through it.
type namesRegistry struct {
	names map[string]bool
}

var fqNamePattern = regexp.MustCompile(`fqName: "([^"]+)"`)

func (r *namesRegistry) Register(c prometheus.Collector) error {
	ch := make(chan *prometheus.Desc, 64)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	for d := range ch {
		// Desc exposes its name only through String().
		if m := fqNamePattern.FindStringSubmatch(d.String()); m != nil {
			r.names[m[1]] = true
		}
	}
	return nil
}

func (r *namesRegistry) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		_ = r.Register(c)
	}
}

func (r *namesRegistry) Unregister(prometheus.Collector) bool { return false }

func TestDerivedMuFieldsArePublished(t *testing.T) {
	keys := derivedMuLogKeys(t)

	reg := &namesRegistry{names: map[string]bool{}}
	require.NoError(t, metrics.InitMetrics(reg))
	// The pattern match must find names at all, or every mapping below would
	// fail for the wrong reason -- and an exclusion-only table would pass.
	require.True(t, reg.names[constants.WVAAnalyzerTarget],
		"the registry recorder read no names; the Desc format may have changed")

	inLog := make(map[string]bool, len(keys))
	for _, key := range keys {
		inLog[key] = true
		f, ok := derivedMuMetricFields[key]
		if !ok {
			t.Errorf("derived-mu logs %q, which no metric publishes and no exclusion names: "+
				"publish it, or add it to derivedMuMetricFields with the reason it is not", key)
			continue
		}
		switch {
		case f.metric != "" && f.excluded != "":
			t.Errorf("%q is both mapped to %s and excluded", key, f.metric)
		case f.metric != "":
			if !reg.names[f.metric] {
				t.Errorf("derived-mu field %q maps to %s, which InitMetrics does not register", key, f.metric)
			}
		case f.excluded == "":
			t.Errorf("%q has neither a metric nor a reason", key)
		}
	}

	var stale []string
	for key := range derivedMuMetricFields {
		if !inLog[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale, "derivedMuMetricFields names keys derived-mu no longer logs")
}
