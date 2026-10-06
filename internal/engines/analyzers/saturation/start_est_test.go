package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The per-variant start-time estimate is what the demand floor will project the
// backlog over, so what matters is which figure is in force and when it changes.
func TestReplicaStartEstimate(t *testing.T) {
	const variant = "decode-v"
	const key = "ns|model|" + variant

	replica := func(pod string, startSeconds float64, bridge bool) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 400_000, tracedKv, 10, 1000, 6000)
		rm.Ready = true
		rm.StartSeconds = startSeconds
		rm.FromWarmPool = bridge
		return rm
	}

	t.Run("the seed is in force until something starts", func(t *testing.T) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		if _, stored := a.startSeconds[key]; stored {
			t.Fatalf("an estimate is stored before anything started")
		}
	})

	t.Run("the first measurement replaces the seed outright", func(t *testing.T) {
		// Not averaged with it. The seed is a guess and the measurement is not,
		// so blending them would keep a wrong number in force for several starts.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("d0", 82, false)},
			logr.Discard())
		if got := a.startSeconds[key]; got != 82 {
			t.Fatalf("start estimate = %v, want the measured 82", got)
		}
	})

	t.Run("later measurements move it slowly", func(t *testing.T) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("d0", 67, false)},
			logr.Discard())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("d1", 82, false)},
			logr.Discard())
		// 67 then 82 at alpha 0.3 -> 71.5. Run T's own spread, and the point is
		// that one slow start does not drag the estimate to 82.
		got := a.startSeconds[key]
		if got < 71 || got > 72 {
			t.Fatalf("start estimate = %v, want ~71.5 (0.7*67 + 0.3*82)", got)
		}
	})

	t.Run("a replica is counted once, however many cycles report it", func(t *testing.T) {
		// Every cycle sees the same Ready Pod reporting the same StartSeconds.
		// Folding it in repeatedly would walk the estimate to that one figure and
		// turn the histogram into a count of cycles.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("d0", 67, false)},
			logr.Discard())
		for i := 0; i < 20; i++ {
			a.noteReplicaStart(key, "ns", variant,
				[]domain.ReplicaMetrics{replica("d0", 82, false)},
				logr.Discard())
		}
		if got := a.startSeconds[key]; got != 67 {
			t.Fatalf("start estimate = %v, want it unmoved at 67", got)
		}
	})

	t.Run("a warm-pool bridge is not a start", func(t *testing.T) {
		// A bridged Pod reaching Ready in a second is a wake. Counting it would
		// tell the projection replicas arrive almost instantly, and the fleet
		// would size for a backlog that never stops growing.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("bridge-0", 1, true)},
			logr.Discard())
		if _, stored := a.startSeconds[key]; stored {
			t.Fatalf("a bridge was folded into the estimate; it is a wake, not a start")
		}
	})

	t.Run("an unmeasured replica contributes nothing", func(t *testing.T) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("d0", 0, false)},
			logr.Discard())
		if _, stored := a.startSeconds[key]; stored {
			t.Fatalf("an estimate is stored from a replica that measured nothing")
		}
	})

	t.Run("another variant's replicas are not this variant's starts", func(t *testing.T) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		other := makeReplicaMetrics("o0", "other-v", 400_000, tracedKv, 10, 1000, 6000)
		other.Ready = true
		other.StartSeconds = 300
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{other}, logr.Discard())
		if _, stored := a.startSeconds[key]; stored {
			t.Fatalf("an estimate is stored from a replica that measured nothing")
		}
	})

	t.Run("the seen set is swept, and the estimate dies with its window", func(t *testing.T) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("d0", 67, false)},
			logr.Discard())
		if len(a.startSeenPods) != 1 {
			t.Fatalf("seen pods = %d, want 1", len(a.startSeenPods))
		}
		// Unbounded otherwise: one entry per Pod the process ever sees, on a
		// fleet that scales up and down all day.
		if dropped := a.evictStartSeenPods(a.now().Add(2*time.Hour), time.Hour); dropped != 1 {
			t.Fatalf("dropped %d, want 1", dropped)
		}
		if len(a.startSeenPods) != 0 {
			t.Fatalf("seen pods = %d after the sweep, want 0", len(a.startSeenPods))
		}
	})

	t.Run("many replicas in one cycle are all counted", func(t *testing.T) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		rms := make([]domain.ReplicaMetrics, 0, 4)
		for i := 0; i < 4; i++ {
			rms = append(rms, replica(fmt.Sprintf("d%d", i), 70, false))
		}
		a.noteReplicaStart(key, "ns", variant, rms, logr.Discard())
		if len(a.startSeenPods) != 4 {
			t.Fatalf("seen pods = %d, want 4", len(a.startSeenPods))
		}
	})
}

// The defects review found in the first cut of this, each with the scenario that
// produced it.
func TestReplicaStartEstimateIdentityAndOutliers(t *testing.T) {
	const variant = "decode-v"
	const key = "ns|model|" + variant

	replica := func(ns, pod string, startSeconds float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 400_000, tracedKv, 10, 1000, 6000)
		rm.Ready = true
		rm.Namespace = ns
		rm.StartSeconds = startSeconds
		return rm
	}

	t.Run("the same pod name in two namespaces is two replicas", func(t *testing.T) {
		// One release deployed into two namespaces gives both a pod named
		// <release>-decode-0. Keyed on the bare name, whichever was analyzed
		// first recorded it and the other never measured at all -- while the
		// first namespace looked correct, so nothing appeared wrong.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns-a", variant,
			[]domain.ReplicaMetrics{replica("ns-a", "rel-decode-0", 67)}, logr.Discard())
		a.noteReplicaStart("ns-b|model|"+variant, "ns-b", variant,
			[]domain.ReplicaMetrics{replica("ns-b", "rel-decode-0", 300)}, logr.Discard())
		if got := a.startSeconds[key]; got != 67 {
			t.Fatalf("ns-a estimate = %v, want 67", got)
		}
		if got := a.startSeconds["ns-b|model|"+variant]; got != 300 {
			t.Fatalf("ns-b estimate = %v, want 300 -- it was suppressed by ns-a", got)
		}
	})

	t.Run("a pod that stops reporting is forgotten", func(t *testing.T) {
		// This is what bounds the map in practice. EvictStaleHistory does now
		// run every cycle, but on a 24h timeout and only as a backstop for a
		// variant that disappears entirely -- a set keyed per pod cannot be
		// left to that.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("ns", "d0", 67), replica("ns", "d1", 70)},
			logr.Discard())
		if len(a.startSeenPods) != 2 {
			t.Fatalf("seen = %d, want 2", len(a.startSeenPods))
		}
		// d1 goes away; the fleet scaled down.
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("ns", "d0", 67)}, logr.Discard())
		if len(a.startSeenPods) != 1 {
			t.Fatalf("seen = %d after d1 left, want 1", len(a.startSeenPods))
		}
	})

	t.Run("one variant's cycle does not forget another's pods", func(t *testing.T) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		other := "ns|model|other-v"
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("ns", "d0", 67)}, logr.Discard())
		rm := makeReplicaMetrics("o0", "other-v", 400_000, tracedKv, 10, 1000, 6000)
		rm.Ready = true
		rm.Namespace = "ns"
		rm.StartSeconds = 80
		a.noteReplicaStart(other, "ns", "other-v", []domain.ReplicaMetrics{rm}, logr.Discard())
		if len(a.startSeenPods) != 2 {
			t.Fatalf("seen = %d, want 2 -- the prune is scoped by key prefix", len(a.startSeenPods))
		}
	})

	t.Run("a readiness blip is rejected, not folded in", func(t *testing.T) {
		// The Ready condition's LastTransitionTime is re-stamped on every
		// False->True transition. A probe blip 45 minutes in reports 2700 s,
		// which at alpha 0.3 would move a 67 s estimate to about 857 s.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("ns", "d0", 67)}, logr.Discard())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("ns", "d1", 2700)}, logr.Discard())
		if got := a.startSeconds[key]; got != 67 {
			t.Fatalf("estimate = %v, want it unmoved at 67", got)
		}
	})

	t.Run("but a persistent change is admitted", func(t *testing.T) {
		// Otherwise the guard becomes the bug: a variant redeployed onto
		// genuinely slower hardware would have every sample rejected forever
		// against an estimate that no longer describes it.
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.noteReplicaStart(key, "ns", variant,
			[]domain.ReplicaMetrics{replica("ns", "d0", 67)}, logr.Discard())
		// N rejections, then admitted on the sighting after them.
		for i := 0; i <= maxConsecutiveStartOutliers; i++ {
			a.noteReplicaStart(key, "ns", variant,
				[]domain.ReplicaMetrics{replica("ns", "slow", 300)}, logr.Discard())
		}
		if got := a.startSeconds[key]; got <= 67 {
			t.Fatalf("estimate = %v, want it to have admitted the new hardware", got)
		}
	})
}
