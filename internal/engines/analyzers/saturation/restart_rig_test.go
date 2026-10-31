package saturation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle/settlerow"
)

// restartRig is the saturation analyzer as the engine runs it: the engine's
// settle tracker, at a 15 s interval, observed with each cycle's rows before
// Analyze, and the analyzer reading it (WithSettleView). A new rig is a
// controller start.
type restartRig struct {
	t   *testing.T
	a   *SaturationAnalyzer
	tr  *settle.Tracker
	now time.Time
}

// p1Obs is the k2 source of a saturated reading k2 recorded.
const p1Obs = "P1-obs"

// restartCycle is the interval the rig's cycles are apart.
const restartCycle = 15 * time.Second

func newRestartRig(t *testing.T, start time.Time) *restartRig {
	t.Helper()
	tr := settle.NewTracker()
	tr.SetInterval(restartCycle)
	r := &restartRig{t: t, tr: tr, now: start}
	r.a = NewSaturationAnalyzer(capacity.NewStore()).WithSettleView(tr)
	r.a.now = func() time.Time { return r.now }
	return r
}

// cycle observes rows into the tracker at the rig's clock, as the engine
// does at the top of its cycle, runs one Analyze, moves the clock a cycle
// on, and returns, in row order, what k2 made of each saturated reading:
// the P1-obs decisions (P1-obs, or why it was not recorded), not the chain
// a refused reading then falls through to.
func (r *restartRig) cycle(rows []domain.ReplicaMetrics, states []domain.VariantReplicaState) []string {
	r.t.Helper()
	ctx, logs := observedCtx(r.t)
	r.cycleCtx(ctx, rows, states)
	var out []string
	for _, e := range logs.FilterMessage("k2-decision").All() {
		if p := stringField(r.t, e.ContextMap(), "priority"); strings.HasPrefix(p, p1Obs) {
			out = append(out, p)
		}
	}
	return out
}

// cycleCtx is one cycle as cycle runs it, logging to ctx's logger.
func (r *restartRig) cycleCtx(ctx context.Context, rows []domain.ReplicaMetrics, states []domain.VariantReplicaState) {
	r.t.Helper()
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = vs.Role
	}
	r.tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(rows, roles), r.now)
	_, err := r.a.Analyze(ctx, makeAnalyzerInput(rows, states))
	require.NoError(r.t, err)
	r.now = r.now.Add(restartCycle)
}
