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
