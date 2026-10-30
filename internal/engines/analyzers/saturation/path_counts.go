package saturation

import (
	"sync/atomic"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The per-path counters a replay reads (docs/proposals/analyzer-learner.md,
// 2.5): how often each learner path ran. A replay that touched none of a
// path's runs proves nothing about it, so the golden check requires each to
// be above zero over the committed goldens.
const (
	// PathK2ObsAdmitted counts saturated readings k2 recorded (P1-obs).
	PathK2ObsAdmitted = "k2-obs-admitted"
	// PathMuSampleWritten counts saturated rates written to mu's window.
	PathMuSampleWritten = "mu-sample-written"
	// PathMuYieldServed counts cycles a measured mu gave way to the derived
	// one because a Ready replica served more than maxRatio times it.
	PathMuYieldServed = "mu-yield-served"
	// PathMuYieldSibling counts cycles a measured mu gave way to what its
	// full siblings serve (siblingYield).
	PathMuYieldSibling = "mu-yield-sibling"
	// PathMuYieldStale counts cycles a stale measured mu gave way to the
	// derived one.
	PathMuYieldStale = "mu-yield-stale"
	// PathITLOLS and PathITLPinned count the cycles a variant's ITL line came
	// from a full fit, or from the pinned fit (a held slope included).
	PathITLOLS    = "itl-ols"
	PathITLPinned = "itl-pinned"
	// PathRowsInstantKV counts input rows that carry an instant KV reading.
	PathRowsInstantKV = "rows-instant-kv"
	// PathRowsReadySince counts input rows that carry their Pod's Ready time.
	PathRowsReadySince = "rows-ready-since"
	// PathJoinedRefusedReadySince counts saturated readings k2 refused as a
	// replica that joined under a rate window ago, where the Pod's Ready time
	// was known: the restart fix (#288) at work.
	PathJoinedRefusedReadySince = "joined-refused-ready-since"
)

// pathNames is every path, in the order PathCounts reports them.
var pathNames = []string{
	PathK2ObsAdmitted, PathMuSampleWritten, PathMuYieldServed, PathMuYieldSibling, PathMuYieldStale,
	PathITLOLS, PathITLPinned, PathRowsInstantKV, PathRowsReadySince, PathJoinedRefusedReadySince,
}

// PathNames is every per-path counter PathCounts reports.
func PathNames() []string { return append([]string(nil), pathNames...) }

// pathCounters is the analyzer's per-path counts. Atomic, so a path counts
// without taking a.mu.
type pathCounters struct {
	k2Obs, muWritten, yieldServed, yieldSibling, yieldStale atomic.Int64
	itlOLS, itlPinned, instantKV, readySince, joinedReady   atomic.Int64
}

// PathCounts is how often each learner path ran since the analyzer was made,
// keyed by the Path* names. Read by the replay; nothing in the controller
// reads it.
func (a *SaturationAnalyzer) PathCounts() map[string]int64 {
	p := &a.paths
	return map[string]int64{
		PathK2ObsAdmitted:           p.k2Obs.Load(),
		PathMuSampleWritten:         p.muWritten.Load(),
		PathMuYieldServed:           p.yieldServed.Load(),
		PathMuYieldSibling:          p.yieldSibling.Load(),
		PathMuYieldStale:            p.yieldStale.Load(),
		PathITLOLS:                  p.itlOLS.Load(),
		PathITLPinned:               p.itlPinned.Load(),
		PathRowsInstantKV:           p.instantKV.Load(),
		PathRowsReadySince:          p.readySince.Load(),
		PathJoinedRefusedReadySince: p.joinedReady.Load(),
	}
}

// countInputRows counts the input rows that carry an instant KV reading and
// a Ready time.
func (p *pathCounters) countInputRows(rows []domain.ReplicaMetrics) {
	for _, rm := range rows {
		if rm.KvUsageInstantKnown {
			p.instantKV.Add(1)
		}
		if !rm.ReadySince.IsZero() {
			p.readySince.Add(1)
		}
	}
}
