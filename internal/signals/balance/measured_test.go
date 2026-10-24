package balance

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// e8Sample is one controller cycle of a measured run (Qwen3-32B-FP8, one
// decode variant of four H200 replicas, no prefill variant, max-num-seqs 256,
// queueLengthThreshold 5): the KV utilization and waiting queue the collector
// read on each pod, from the controller log at --v=4. Phase 1 sent every
// request to 69x4l, which ran 256 sequences with 768 waiting at 6.2 req/s
// while its three siblings held nothing; phase 2 spread the same model's load
// evenly.
type e8Sample struct {
	at string // HH:MM:SS, UTC
	kv [4]float64
	q  [4]int
}

// e8Pods are the replicas in the order e8Sample lists them.
var e8Pods = [4]string{"69x4l", "dkmkt", "gh2dm", "vcgxr"}

// e8Samples is every sixth cycle of the run: the log's cycles are 15s apart,
// so each sample stands for the 90s after it.
var e8Samples = []e8Sample{
	{"19:52:18", [4]float64{0, 0, 0, 0}, [4]int{0, 0, 0, 0}},
	{"19:53:48", [4]float64{0.9130, 0, 0, 0}, [4]int{180, 0, 0, 0}},
	{"19:55:18", [4]float64{0.8886, 0, 0, 0}, [4]int{765, 0, 0, 0}},
	{"19:56:48", [4]float64{0.8825, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"19:58:18", [4]float64{0.8869, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"19:59:48", [4]float64{0.8756, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"20:01:18", [4]float64{0.8746, 0, 0, 0}, [4]int{767, 0, 0, 0}},
	{"20:02:48", [4]float64{0.8828, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"20:04:18", [4]float64{0.8674, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"20:05:48", [4]float64{0.8634, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"20:07:18", [4]float64{0.8717, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"20:08:48", [4]float64{0.8623, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"20:10:18", [4]float64{0.8609, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	{"20:11:48", [4]float64{0.8663, 0, 0, 0}, [4]int{768, 0, 0, 0}},
	// Phase 2 starts at 20:13:12.
	{"20:13:18", [4]float64{0.0757, 0.0770, 0.0774, 0.0473}, [4]int{0, 0, 0, 0}},
	{"20:14:49", [4]float64{0.0571, 0.0655, 0.0604, 0.0332}, [4]int{0, 0, 0, 0}},
}

const e8Phase2 = "20:13:12"

func (s e8Sample) time(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.TimeOnly, s.at)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(2026, 10, 23, at.Hour(), at.Minute(), at.Second(), 0, time.UTC)
}

// rows are the sample's replicas, Ready since before the run's load started.
func (s e8Sample) rows(t *testing.T) []domain.ReplicaMetrics {
	readySince := e8Samples[0].time(t).Add(-5 * time.Minute)
	out := make([]domain.ReplicaMetrics, 0, len(e8Pods))
	for i, pod := range e8Pods {
		out = append(out, domain.ReplicaMetrics{PodName: pod, VariantName: "qwen32b-decode",
			KvCacheUsage: s.kv[i], QueueLength: s.q[i], Ready: true, ReadySince: readySince})
	}
	return out
}

// Measured (E8): a decode variant with no prefill beside it, one replica
// capped by max-num-seqs with 768 waiting at KV 0.86-0.89 while three
// siblings idled for twenty minutes. Judged on KV alone at a fixed 0.90 it
// read saturated on one cycle at KV 0.913 and never warned; a
// second run warned on that cycle and logged "load balanced again" two
// minutes later with 768 still waiting. Its queue is a backlog, so it must
// warn once the idle replicas' grace and the sustain have run, name 69x4l,
// hold through phase 1, and clear in phase 2.
//
// The replay runs at kvCacheThreshold 0.90, which every sample after the
// first is under, so phase 1 holds on the queue rule alone: under the default
// 0.80 the KV would carry it and the queue rule would go untested.
func TestMeasuredDecodeBacklogWithoutPrefillReadsImbalanced(t *testing.T) {
	queueOnly := Thresholds{KvUsage: 0.90, Queue: policy.Queue}
	roles := map[string]string{"qwen32b-decode": domain.RoleDecode}
	tr := NewTracker()
	tr.SetInterval(cycle)
	start := e8Samples[0].time(t)
	warnAfter := start.Add(IdleAfterReady + SustainFor)
	var warnedAt, clearedAt time.Time
	for _, s := range e8Samples {
		rows := s.rows(t)
		phase2 := s.at >= e8Phase2
		for i := range int(90 * time.Second / cycle) {
			now := s.time(t).Add(time.Duration(i) * cycle)
			rs := tr.Observe("evgensh-sg1/qwen", tr.Measure("evgensh-sg1/qwen", rows, roles, queueOnly, now), now)
			r := only(t, rs)
			if r.Changed && r.Sustained {
				warnedAt = now
			}
			if r.Changed && !r.Sustained {
				if !phase2 {
					t.Fatalf("%s: logged balanced again in phase 1 with queue %v beside %d idle: %+v",
						now.Format(time.TimeOnly), r.MaxQueue, r.Idle, r.Distribution)
				}
				clearedAt = now
			}
			if !phase2 && r.Sustained && r.BusiestPod != "69x4l" {
				t.Errorf("%s: busiest pod %q, want 69x4l", now.Format(time.TimeOnly), r.BusiestPod)
			}
			if !phase2 && !warnedAt.IsZero() && !r.Sustained {
				t.Fatalf("%s: phase-1 imbalance stopped being reported: %+v", now.Format(time.TimeOnly), r.Distribution)
			}
		}
	}
	if warnedAt.IsZero() {
		t.Fatal("never reported imbalanced through phase 1")
	}
	if warnedAt.Before(warnAfter) {
		t.Errorf("warned at %s, before the grace and sustain ran out at %s",
			warnedAt.Format(time.TimeOnly), warnAfter.Format(time.TimeOnly))
	}
	if clearedAt.IsZero() {
		t.Error("the report never cleared in phase 2, where the load is even")
	}
}

// A decode replica of a disaggregated model counts requests waiting for their
// remote KV among its waiting ones, so a deep queue at KV under the policy's
// kvCacheThreshold is not saturated there, and the busiest pod is the fuller
// one, not the one with more waiting. A prefill variant scaled to zero still
// makes the model disaggregated: transfers it began can still be pending.
func TestMeasureDisaggregatedDecodeIgnoresDeferredWaiting(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		row("dec-0", "dec", 0.74, 768),
		row("dec-1", "dec", 0.78, 0),
		row("dec-2", "dec", 0.00, 0),
	}
	d := Measure(rows, pd, policy, t0)[domain.RoleDecode]
	if d.Saturated != 0 || d.Imbalanced() {
		t.Errorf("P/D decode queued for remote KV read saturated: %+v", d)
	}
	if d.BusiestPod != "dec-1" {
		t.Errorf("P/D decode busiest pod %q, want dec-1 (KV 0.78)", d.BusiestPod)
	}
	alone := map[string]string{"dec": domain.RoleDecode}
	if d := Measure(rows, alone, policy, t0)[domain.RoleDecode]; d.Saturated != 1 || !d.Imbalanced() || d.BusiestPod != "dec-0" {
		t.Errorf("decode with no prefill variant, 768 waiting, not judged on its queue: %+v", d)
	}
}
