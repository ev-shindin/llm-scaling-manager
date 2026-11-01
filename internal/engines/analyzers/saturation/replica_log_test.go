package saturation

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// perReplicaMessages are the lines written once per replica per cycle.
var perReplicaMessages = []string{"replica-capacity-decision", "k2-decision", "derived-mu"}

// cycleLog runs one analyzer cycle over a decode variant of `pods` replicas
// and returns what a controller at -v=verbosity writes for it, in the console
// encoding the controller ships (cmd/main.go: Development: true).
func cycleLog(t *testing.T, verbosity, pods int) string {
	t.Helper()
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(int8(-verbosity)))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))

	rows := make([]domain.ReplicaMetrics, 0, pods)
	for i := range pods {
		rows = append(rows, makeReplicaMetrics(fmt.Sprintf("decode-%d", i), "variant-d", 5000, 16000, 0, 100, 50))
	}
	input := makeAnalyzerInput(rows, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H100",
			CurrentReplicas: pods, GPUsPerReplica: 1},
	})
	_, err := NewSaturationAnalyzer(capacity.NewStore()).Analyze(ctx, input)
	require.NoError(t, err)
	return buf.String()
}

func lineCount(log string) int { return strings.Count(log, "\n") }

func linesWith(log, msg string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "\t"+msg+"\t") {
			n++
		}
	}
	return n
}

// At the shipped verbosity a cycle used to write about 1.9 KB per replica:
// replica-capacity-decision, k2-decision and derived-mu, each once per replica
// (issue #279). They are at perReplicaLogLevel now, and the default carries one
// replica-capacity-summary line per variant, so the number of lines a cycle
// writes no longer grows with the fleet, and each replica adds only its
// summary entry.
func TestCycleLogAtTheShippedVerbosityDoesNotGrowPerReplica(t *testing.T) {
	const small, large = 1, 8
	shipped1, shipped8 := cycleLog(t, logging.DEFAULT, small), cycleLog(t, logging.DEFAULT, large)
	verbose1, verbose8 := cycleLog(t, perReplicaLogLevel, small), cycleLog(t, perReplicaLogLevel, large)

	perReplica := func(a, b string) int { return (len(b) - len(a)) / (large - small) }
	t.Logf("bytes per cycle at -v=%d (shipped): %d with %d replica(s), %d with %d; %d per added replica",
		logging.DEFAULT, len(shipped1), small, len(shipped8), large, perReplica(shipped1, shipped8))
	t.Logf("bytes per cycle at -v=%d: %d with %d replica(s), %d with %d; %d per added replica",
		perReplicaLogLevel, len(verbose1), small, len(verbose8), large, perReplica(verbose1, verbose8))

	// The measurement has something to measure: the per-replica lines exist,
	// one level up.
	for _, msg := range perReplicaMessages {
		assert.GreaterOrEqual(t, linesWith(verbose8, msg), large,
			"%q must still be written once per replica at -v=%d", msg, perReplicaLogLevel)
		assert.Zero(t, linesWith(shipped8, msg),
			"%q is per replica per cycle and must not reach the shipped log", msg)
	}
	assert.Equal(t, lineCount(shipped1), lineCount(shipped8),
		"lines per cycle at the shipped verbosity must not grow with the replica count")
	assert.Less(t, perReplica(shipped1, shipped8), perReplica(verbose1, verbose8),
		"a replica must cost less at the shipped verbosity than with its per-replica lines")

	// What the default keeps: every replica, named, with what it was priced at.
	require.Equal(t, 1, linesWith(shipped8, "replica-capacity-summary"))
	for i := range large {
		assert.Contains(t, shipped8, fmt.Sprintf("decode-%d=", i),
			"replica-capacity-summary must name every priced replica")
	}
}

// summaryReplicas returns the replicas entries of every replica-capacity-summary
// line written for variant at the shipped verbosity.
func summaryReplicas(t *testing.T, logs *observer.ObservedLogs, variant string) []string {
	t.Helper()
	var out []string
	for _, e := range logs.FilterMessage("replica-capacity-summary").All() {
		f := e.ContextMap()
		if f["variant"] != variant {
			continue
		}
		switch entries := f["replicas"].(type) {
		case []string:
			out = append(out, entries...)
		case []any:
			for _, e := range entries {
				s, ok := e.(string)
				require.True(t, ok, "a replicas entry must be a string, got %T", e)
				out = append(out, s)
			}
		default:
			t.Fatalf("replicas must be a list of strings, got %T", f["replicas"])
		}
	}
	return out
}

// Why a saturated reading was not recorded, and why a decode replica has no
// derived mu, used to be said only by k2-decision and derived-mu, which are at
// perReplicaLogLevel now. Raising the level restarts the controller, which
// clears the in-memory state being diagnosed, so the default carries both.
func TestSummarySaysWhyAReadingWasNotRecorded(t *testing.T) {
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	analyzer := NewSaturationAnalyzer(capacity.NewStore())

	// A saturated prefill behind a saturated decode: its reading is decode's
	// saturation seen from prefill, so it is not recorded (P1-obs-downstream).
	prefill := makeReplicaMetrics("pod-p", "variant-p", 357_800, 1_149_312, 30, 6000, 1)
	decode := makeReplicaMetrics("pod-d", "variant-d", 1_039_474, 1_162_240, 81, 6000, 1000)
	prefill.Ready, decode.Ready = true, true
	_, err := analyzeAfterHold(ctx, analyzer, makeAnalyzerInput([]domain.ReplicaMetrics{prefill, decode},
		[]domain.VariantReplicaState{
			{VariantName: "variant-p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
			{VariantName: "variant-d", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
		}))
	require.NoError(t, err)

	p := summaryReplicas(t, logs, "variant-p")
	require.Len(t, p, 1)
	assert.Contains(t, p[0], "/"+k2ReasonObsDownstream, "the prefill entry must say its reading was not recorded, and why")
	d := summaryReplicas(t, logs, "variant-d")
	require.Len(t, d, 1)
	assert.NotContains(t, d[0], "/P1-obs-", "decode's reading was recorded; nothing to say")
	// A fresh analyzer has fitted no ITL line, so decode has no derived mu.
	assert.Contains(t, d[0], "/mu:no-line", "the decode entry must say why it has no derived mu")
	assert.NotContains(t, p[0], "/mu:", "prefill derives no mu, so it has nothing missing to report")
}

func TestReplicaSummaryEntry(t *testing.T) {
	rc := capacity.ReplicaCapacity{PodName: "p", MemoryBoundCapacity: 100, ComputeBoundCapacity: 80,
		EffectiveCapacity: 80, K2Priority: capacity.K2SrcHistorical}
	assert.Equal(t, "p=80/k2/P2-hist", replicaSummary(rc, ""))
	rc.K2NotRecorded, rc.FromWarmPool = k2ReasonObsUnsettled, true
	assert.Equal(t, "p=80/k2/P2-hist/P1-obs-unsettled/mu:no-line/bridge", replicaSummary(rc, "no-line"))
	// A tie is k1, as replica-capacity-decision's boundBy says.
	tie := capacity.ReplicaCapacity{PodName: "q", MemoryBoundCapacity: 100, ComputeBoundCapacity: 100,
		EffectiveCapacity: 100, K2Priority: capacity.K2SrcFallback}
	assert.Equal(t, "q=100/k1/P4-k1", replicaSummary(tie, ""))
}

// containerd and CRI-O split a log line longer than criMaxLogLineBytes, and a
// grep for the variant then misses the pods in the second half. A variant
// with many replicas is written over several lines, each whole, none lost.
func TestSummaryStaysWithinTheCRILineLimit(t *testing.T) {
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	logger := zapr.NewLogger(zap.New(zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(-logging.DEFAULT))))

	// Real pod names: <deployment>-<replicaset hash>-<suffix>.
	const replicas = 1000
	caps := make([]capacity.ReplicaCapacity, 0, replicas)
	for i := range replicas {
		caps = append(caps, capacity.ReplicaCapacity{
			PodName:     fmt.Sprintf("llama-3-70b-instruct-decode-7f9c6d5b8-%05d", i),
			VariantName: "decode", MemoryBoundCapacity: 1_152_000, ComputeBoundCapacity: 1_000_000,
			EffectiveCapacity: 1_000_000, K2Priority: capacity.K2SrcHistorical,
		})
	}
	logReplicaCapacitySummary(logger, "m", "ns", caps, nil)

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	require.Greater(t, len(lines), 1, "a thousand replicas do not fit one line under the CRI limit")
	for i, line := range lines {
		assert.LessOrEqual(t, len(line)+1, criMaxLogLineBytes, "line %d is %d bytes", i, len(line))
		assert.Contains(t, line, `"priced": 1000`, "every part carries the variant's whole count")
		assert.Contains(t, line, fmt.Sprintf(`"part": %d, "parts": %d`, i+1, len(lines)))
	}
	for i := range replicas {
		assert.Contains(t, buf.String(), fmt.Sprintf(`"llama-3-70b-instruct-decode-7f9c6d5b8-%05d=`, i))
	}
}
