package learnreplay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
)

// olsFixture is the cluster golden whose load steps k across the ITL band,
// so its window holds the spread a full fit needs.
const olsFixture = "ols-sweep"

// The ols-sweep golden's own replay takes full ITL fits, not only pinned
// ones: the summed path check is met by the in-process mu-paths run alone,
// so without this a cluster recording that lost its OLS fits would pass.
// The counts are the controller's: its log held 101 tier=ols and 94
// tier=pinned-B itl-fit lines over the recording's window. Negative control:
// the same recording cut before the load stepped from k 0.28 to 0.67 fits
// no full line.
func TestGoldens_OLSSweepReplayFitsOLS(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join(goldenDir, olsFixture, RecordingName))
	require.NoError(t, err)
	rec, err := gunzip(gz)
	require.NoError(t, err)

	rep, err := Replay(context.Background(), rec, Options{Mode: Normal})
	require.NoError(t, err)
	require.Empty(t, rep.Failed())
	assert.Equal(t, int64(101), rep.Paths[saturation.PathITLOLS], "the controller logged 101 tier=ols fits")
	assert.Equal(t, int64(94), rep.Paths[saturation.PathITLPinned], "the controller logged 94 tier=pinned-B fits")

	// The load stepped up at 21:59:55Z and the first full fit came at
	// 22:02:17Z; cut at the first cycle that begins after 22:01:00Z.
	cut := time.Date(2026, 11, 1, 22, 1, 0, 0, time.UTC)
	lines := strings.SplitAfter(string(rec), "\n")
	n := 0
	for i, l := range lines {
		var ev struct {
			Now  time.Time
			Kind string
		}
		if l == "" {
			continue
		}
		require.NoError(t, json.Unmarshal([]byte(l), &ev))
		if ev.Kind == "slo.BeginCycle" && !ev.Now.Before(cut) {
			n = i
			break
		}
	}
	require.Positive(t, n, "precondition: the recording runs past the cut")
	head, err := Replay(context.Background(), []byte(strings.Join(lines[:n], "")), Options{Mode: Normal})
	require.NoError(t, err)
	require.Empty(t, head.Failed())
	assert.Zero(t, head.Paths[saturation.PathITLOLS], "before the step, no full fit")
	assert.Positive(t, head.Paths[saturation.PathRowsInstantKV], "precondition: the head offers readings to the window")
}
