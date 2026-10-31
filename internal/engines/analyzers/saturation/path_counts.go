package saturation

import (
	"sync/atomic"
)

// The per-path counters a replay reads (docs/proposals/analyzer-learner.md,
// 2.5): how often each learner path did its work. A replay that touched none
// of a path's runs proves nothing about it, so the golden check requires each
// to be above zero over the committed goldens. Each counts where the path
// acts, not where its input arrives.

// PathK2ObsAdmitted counts saturated readings k2 recorded in its window
// (P1-obs).
const PathK2ObsAdmitted = "k2-obs-admitted"

// PathMuSampleWritten counts new samples a replica's saturated rate started
// in mu's window: a sample a minute; a reading within the minute folds into
// the sample already there and is not counted.
const PathMuSampleWritten = "mu-sample-written"

// PathMuYieldServed counts cycles a measured mu gave way to the derived one
// because a Ready replica served more than maxRatio times it.
const PathMuYieldServed = "mu-yield-served"

// PathMuYieldSibling counts cycles a measured mu gave way to what its full
// siblings serve (siblingYield).
const PathMuYieldSibling = "mu-yield-sibling"

// PathMuYieldStale counts cycles a stale measured mu gave way to the derived
// one.
const PathMuYieldStale = "mu-yield-stale"

// PathITLOLS counts the cycles a variant's ITL line came from a full fit.
const PathITLOLS = "itl-ols"

// PathITLPinned counts the cycles a variant's ITL line came from the pinned
// fit, a held slope included.
const PathITLPinned = "itl-pinned"

// PathRowsInstantKV counts rows whose instant KV a learner read: a reading
// the ITL line took at its instant k, or a replica demand charged at its
// instant KV rather than its one-minute peak.
const PathRowsInstantKV = "rows-instant-kv"

// PathRowsReadySince counts rows whose Pod Ready time k2's joined test read.
const PathRowsReadySince = "rows-ready-since"

// PathJoinedRefusedReadySince counts saturated readings k2 refused as a
// replica that joined under a rate window ago because of its Pod's Ready
// time: the restart fix (#288) deciding the refusal, not the first sight.
const PathJoinedRefusedReadySince = "joined-refused-ready-since"

// learnerPath indexes the counters.
type learnerPath int

const (
	pathK2Obs learnerPath = iota
	pathMuWritten
	pathYieldServed
	pathYieldSibling
	pathYieldStale
	pathITLOLS
	pathITLPinned
	pathInstantKV
	pathReadySince
	pathJoinedReady
	numPaths
)

// pathNames is the one table of the counters' names, by index.
var pathNames = [numPaths]string{
	pathK2Obs:        PathK2ObsAdmitted,
	pathMuWritten:    PathMuSampleWritten,
	pathYieldServed:  PathMuYieldServed,
	pathYieldSibling: PathMuYieldSibling,
	pathYieldStale:   PathMuYieldStale,
	pathITLOLS:       PathITLOLS,
	pathITLPinned:    PathITLPinned,
	pathInstantKV:    PathRowsInstantKV,
	pathReadySince:   PathRowsReadySince,
	pathJoinedReady:  PathJoinedRefusedReadySince,
}

// PathNames is every per-path counter PathCounts reports, in order.
func PathNames() []string { return append([]string(nil), pathNames[:]...) }

// pathCounters is the analyzer's per-path counts. Atomic, so a path counts
// without taking a.mu.
type pathCounters [numPaths]atomic.Int64

// add counts n runs of path p.
func (c *pathCounters) add(p learnerPath, n int64) { c[p].Add(n) }

// PathCounts is how often each learner path ran since the analyzer was made,
// keyed by PathNames. Read by the replay; nothing in the controller reads it.
func (a *SaturationAnalyzer) PathCounts() map[string]int64 {
	out := make(map[string]int64, numPaths)
	for p := learnerPath(0); p < numPaths; p++ {
		out[pathNames[p]] = a.paths[p].Load()
	}
	return out
}
