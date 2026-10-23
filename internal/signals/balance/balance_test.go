package balance

import (
	"math"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// t0 is the cycle the Measure tests read at.
var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// row is a replica Ready since well before t0, so its readings span far more
// than IdleAfterReady.
func row(pod, variant string, kv float64, queue int) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: pod, VariantName: variant, KvCacheUsage: kv, QueueLength: queue,
		Ready: true, ReadySince: t0.Add(-time.Hour)}
}

var pd = map[string]string{"pre": domain.RolePrefill, "dec": domain.RoleDecode}

// The measured case (docs/reference/router-configuration.md): 439 requests
// queued on one prefill replica while three sit idle.
func TestMeasureFlagsOneQueuedPrefillBesideIdleOnes(t *testing.T) {
	d := Measure([]domain.ReplicaMetrics{
		row("pre-0", "pre", 0.20, 439),
		row("pre-1", "pre", 0.01, 0),
		row("pre-2", "pre", 0.00, 0),
		row("pre-3", "pre", 0.02, 0),
	}, pd, 5, t0)[domain.RolePrefill]

	if d.Ready != 4 || d.Saturated != 1 || d.Idle != 3 {
		t.Fatalf("ready/saturated/idle = %d/%d/%d, want 4/1/3", d.Ready, d.Saturated, d.Idle)
	}
	if !d.Imbalanced() || d.IdleWhileSaturated() != 3 {
		t.Errorf("Imbalanced=%v IdleWhileSaturated=%d, want true and 3", d.Imbalanced(), d.IdleWhileSaturated())
	}
	if r, ok := d.QueueMaxToMean(); !ok || r != 4 {
		t.Errorf("queue max/mean = %v (ok %v), want 4: every waiting request on one of four", r, ok)
	}
	if d.BusiestPod != "pre-0" {
		t.Errorf("busiest pod %q, want pre-0", d.BusiestPod)
	}
	if want := []string{"pre-1", "pre-2", "pre-3"}; len(d.IdlePods) != 3 || d.IdlePods[0] != want[0] || d.IdlePods[2] != want[2] {
		t.Errorf("idle pods %v, want %v", d.IdlePods, want)
	}
}

// The same total load, spread: nothing saturated, nothing to warn about, and
// both ratios at one.
func TestMeasureReadsAnEvenRoleAsBalanced(t *testing.T) {
	d := Measure([]domain.ReplicaMetrics{
		row("pre-0", "pre", 0.06, 110),
		row("pre-1", "pre", 0.06, 110),
		row("pre-2", "pre", 0.06, 110),
		row("pre-3", "pre", 0.06, 109),
	}, pd, 5, t0)[domain.RolePrefill]
	if d.Imbalanced() || d.Idle != 0 {
		t.Errorf("an evenly queued role read imbalanced: saturated %d idle %d", d.Saturated, d.Idle)
	}
	if r, _ := d.KvMaxToMean(); math.Abs(r-1) > 1e-9 {
		t.Errorf("kv max/mean = %v, want 1", r)
	}
}

// A decode replica's waiting count includes requests waiting for their remote
// KV, so a queue alone does not make it saturated; its KV does.
func TestMeasureJudgesDecodeOnKvAlone(t *testing.T) {
	dists := Measure([]domain.ReplicaMetrics{
		row("dec-0", "dec", 0.40, 50),
		row("dec-1", "dec", 0.10, 0),
	}, pd, 5, t0)
	if d := dists[domain.RoleDecode]; d.Saturated != 0 || d.Imbalanced() {
		t.Errorf("decode queued at 0.40 KV read saturated: %+v", d)
	}
	dists = Measure([]domain.ReplicaMetrics{
		row("dec-0", "dec", 0.95, 0),
		row("dec-1", "dec", 0.10, 0),
	}, pd, 5, t0)
	if d := dists[domain.RoleDecode]; d.Saturated != 1 || !d.Imbalanced() {
		t.Errorf("decode at 0.95 KV beside an idle one not flagged: %+v", d)
	}
}

// A pod of several engine ranks is judged on its busiest rank: one full rank
// among idle ones is saturated, whatever the pod's mean.
func TestMeasureReadsTheBusiestRank(t *testing.T) {
	hot := row("agg-0", "agg", 0.15, 0)
	hot.EngineRanks, hot.MaxRankKvCacheUsage = 8, 0.95
	d := Measure([]domain.ReplicaMetrics{hot, row("agg-1", "agg", 0.05, 0)}, nil, 5, t0)[domain.RoleBoth]
	if d.Saturated != 1 || d.Idle != 1 || d.MaxKvUsage != 0.95 {
		t.Errorf("busiest rank not read: %+v", d)
	}
}

// Not Ready is not routed to, and a KV reading that is not a number is not a
// load: both are left out. An over-range reading is clamped.
func TestMeasureSkipsUnreadyAndBadReadings(t *testing.T) {
	notReady := row("agg-2", "agg", 0.99, 40)
	notReady.Ready = false
	d := Measure([]domain.ReplicaMetrics{
		row("agg-0", "agg", math.NaN(), 0),
		row("agg-1", "agg", 1.7, 0),
		row("agg-3", "agg", math.Inf(1), 0),
		row("agg-4", "agg", -0.2, 0),
		notReady,
	}, nil, 5, t0)[domain.RoleBoth]
	if d.Ready != 1 {
		t.Fatalf("ready %d, want 1 (not-Ready, NaN, Inf and negative left out)", d.Ready)
	}
	if d.MaxKvUsage != 1 || d.MeanKvUsage != 1 {
		t.Errorf("max %v mean %v, want 1 and 1 (1.7 clamped to 1)", d.MaxKvUsage, d.MeanKvUsage)
	}
}

// Probe (review r1): a Ready replica whose KV figure is unreadable was read
// as 0 and so as idle, and beside a saturated sibling the role read as
// imbalanced, blaming the router for a scrape gap.
func TestMeasureDoesNotReadAnUnreadableKvAsIdle(t *testing.T) {
	d := Measure([]domain.ReplicaMetrics{
		row("d-0", "dec", 0.95, 0),
		row("d-1", "dec", math.NaN(), 0),
	}, pd, 5, t0)[domain.RoleDecode]
	if d.Imbalanced() || d.Idle != 0 || len(d.IdlePods) != 0 {
		t.Errorf("an unreadable KV counted as idle: idle %d %v", d.Idle, d.IdlePods)
	}
}

// Probe (review r1): prefill and aggregated roles are judged saturated on
// their queue, and the busiest pod was picked by KV alone. With the queued
// pod holding less KV than an idle sibling, the warning named the idle pod as
// busiest, and listed it among the idle pods too.
func TestMeasureNamesTheQueuedPodBusiest(t *testing.T) {
	d := Measure([]domain.ReplicaMetrics{
		row("pre-0", "pre", 0.10, 439),
		row("pre-1", "pre", 0.25, 0),
		row("pre-2", "pre", 0.00, 0),
	}, pd, 5, t0)[domain.RolePrefill]
	if d.Saturated != 1 || d.Idle != 2 {
		t.Fatalf("saturated/idle = %d/%d, want 1/2", d.Saturated, d.Idle)
	}
	if d.BusiestPod != "pre-0" {
		t.Errorf("busiest pod %q, want pre-0, the one with 439 waiting", d.BusiestPod)
	}
	for _, p := range d.IdlePods {
		if p == d.BusiestPod {
			t.Errorf("busiest pod %q is also listed as idle", p)
		}
	}
}

// Decode is judged on KV, so its busiest pod is the fullest one even when a
// sibling has more waiting; and with every replica idle there is no busiest.
func TestMeasureBusiestFollowsTheRolesSignal(t *testing.T) {
	d := Measure([]domain.ReplicaMetrics{
		row("dec-0", "dec", 0.95, 0),
		row("dec-1", "dec", 0.50, 30),
	}, pd, 5, t0)[domain.RoleDecode]
	if d.BusiestPod != "dec-0" {
		t.Errorf("decode busiest pod %q, want dec-0 (KV 0.95)", d.BusiestPod)
	}
	idle := Measure([]domain.ReplicaMetrics{row("a", "agg", 0, 0), row("b", "agg", 0.1, 0)}, nil, 5, t0)[domain.RoleBoth]
	if idle.BusiestPod != "" {
		t.Errorf("busiest pod %q among replicas that are all idle", idle.BusiestPod)
	}
}

// Probe (review r1): a scale-up of a saturated role. The new replica has just
// passed readiness and the router has not filled it yet: KV 0, nothing
// waiting. It is not idle until it has been Ready for IdleAfterReady, nor is
// a replica whose Ready time is unknown.
func TestMeasureDoesNotReadAJustReadyReplicaAsIdle(t *testing.T) {
	fresh := row("d-2", "dec", 0, 0)
	fresh.ReadySince = t0.Add(-30 * time.Second)
	unknown := row("d-3", "dec", 0, 0)
	unknown.ReadySince = time.Time{}
	rows := []domain.ReplicaMetrics{row("d-0", "dec", 0.95, 0), row("d-1", "dec", 0.93, 0), fresh, unknown}
	if d := Measure(rows, pd, 5, t0)[domain.RoleDecode]; d.Imbalanced() || d.Idle != 0 {
		t.Errorf("a replica Ready for 30s, or since an unknown time, read idle: %v", d.IdlePods)
	}
	d := Measure(rows, pd, 5, t0.Add(IdleAfterReady))[domain.RoleDecode]
	if d.Idle != 1 || d.IdlePods[0] != "d-2" {
		t.Errorf("once Ready for IdleAfterReady the empty replica is idle: idle %d %v", d.Idle, d.IdlePods)
	}
}

func TestRatiosAbsentWhereTheySayNothing(t *testing.T) {
	one := Measure([]domain.ReplicaMetrics{row("a", "agg", 0.5, 3)}, nil, 5, t0)[domain.RoleBoth]
	if _, ok := one.KvMaxToMean(); ok {
		t.Error("a ratio over one replica was published")
	}
	idle := Measure([]domain.ReplicaMetrics{row("a", "agg", 0, 0), row("b", "agg", 0, 0)}, nil, 5, t0)[domain.RoleBoth]
	if _, ok := idle.KvMaxToMean(); ok {
		t.Error("a kv ratio over a mean of zero was published")
	}
	if _, ok := idle.QueueMaxToMean(); ok {
		t.Error("a queue ratio with nothing waiting was published")
	}
}

func TestQueueThresholdOffDisablesTheQueueTest(t *testing.T) {
	d := Measure([]domain.ReplicaMetrics{row("a", "agg", 0.2, 100), row("b", "agg", 0.0, 0)}, nil, 0, t0)[domain.RoleBoth]
	if d.Saturated != 0 {
		t.Errorf("saturated on its queue with queueLengthThreshold 0: %+v", d)
	}
}
