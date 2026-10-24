package main

import (
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// capacityLogVerbosity is the V() level the saturation analyzer logs its
// per-variant capacity lines at (replica-capacity-summary, itl-window,
// itl-fit). The shipped deployment passes no -v, so if defaultLogVerbosity ever
// drops below this they vanish from the default log with nothing failing. The
// per-replica lines are one level higher on purpose (perReplicaLogLevel in the
// saturation package), and TestCycleLogAtTheShippedVerbosityDoesNotGrowPerReplica
// keeps them out of the default.
const capacityLogVerbosity = logging.DEFAULT

func TestDefaultVerbosityKeepsCapacityLogsVisible(t *testing.T) {
	assert.GreaterOrEqual(t, defaultLogVerbosity, capacityLogVerbosity,
		"the shipped -v must stay at or above V(%d), where the saturation analyzer logs "+
			"what each variant's replicas were priced at",
		capacityLogVerbosity)
}

// The level arithmetic between -v and zap is easy to get backwards (InitLogging
// negates), so assert on an actual sink rather than on the constants alone: at
// the shipped -v a V(capacityLogVerbosity) line is recorded, and one level more
// verbose is not.
func TestDefaultVerbosityLevelArithmetic(t *testing.T) {
	core, logs := observer.New(zapcore.Level(int8(-1 * defaultLogVerbosity)))
	logger := zapr.NewLogger(uberzap.New(core))

	logger.V(capacityLogVerbosity).Info("capacity-line")
	logger.V(capacityLogVerbosity + 1).Info("too-verbose-line")

	require.Len(t, logs.FilterMessage("capacity-line").All(), 1,
		"the analyzer's capacity lines must reach the logs at the shipped -v")
	assert.Empty(t, logs.FilterMessage("too-verbose-line").All(),
		"one level past the shipped -v must be suppressed, or the gate buys nothing")

	// Guard the direction of the negation: a logger built for -v=0 must drop it.
	quietCore, quietLogs := observer.New(zapcore.Level(0))
	quiet := zapr.NewLogger(uberzap.New(quietCore))
	quiet.V(capacityLogVerbosity).Info("capacity-line")
	assert.Empty(t, quietLogs.FilterMessage("capacity-line").All(),
		"-v=0 must silence the per-variant capacity lines")
}
