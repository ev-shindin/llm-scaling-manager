package saturation

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Uneven routing: the router loads one replica of a role while a sibling of
// the same role sits idle. What the analyzer LEARNS must not move with it. What
// it prices is a sum over the role's replicas and charges every queue in full
// wherever it stands: uneven routing is reported, not compensated, and where
// it moves the demand it raises it. A fleet that really is short, every
// replica busy, is checked beside each case.
//
// The fixtures use the shipped test policy (makeAnalyzerInput: kvCacheThreshold
// 0.8, queueLengthThreshold 5, scale-up 0.85) and a 1,162,240-token cache, the
// figures of the load-imbalance audit (2026-10-23) these tests came from.

const imbKv = int64(1_162_240)

func imbAnalyzer() (*SaturationAnalyzer, *time.Time) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	return a, &clock
}

// imbRow is one replica at KV fraction frac (its one-minute peak and, unless
// the test sets it apart, its instant reading) with queue waiting.
func imbRow(pod, variant string, frac float64, queue int, in, out, reqRate float64) domain.ReplicaMetrics {
	rm := makeReplicaMetrics(pod, variant, int64(frac*float64(imbKv)), imbKv, queue, in, out)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = frac, true
	rm.RequestRate = reqRate
	rm.GenerationTokenRate = reqRate * out
	rm.PrefillComputedTokenRate = reqRate * in
	return rm
}

func imbVariant(t *testing.T, r *domain.AnalyzerResult, v string) domain.VariantCapacity {
	t.Helper()
	for _, vc := range r.VariantCapacities {
		if vc.VariantName == v {
			return vc
		}
	}
	t.Fatalf("no variant %s in the result", v)
	return domain.VariantCapacity{}
}

// imbK2Windows counts the k2 windows holding a reading for role ("" for any).
func imbK2Windows(a *SaturationAnalyzer, role string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for k, ra := range a.computeCapacityHistory {
		if (role == "" || strings.Contains(k, "|"+role+"|")) && ra.Len() > 0 {
			n++
		}
	}
	return n
}

// One decode replica full and queued is decode saturated for what prefill
// LEARNS, whatever its siblings do: KV prefilled for it waits on it, and a
// prefill k2 learned from that wait is never corrected. Only prefill's DEMAND
// hold relaxes, and only once a decode sibling has read idle for
// IdleSettledFor. Every decode replica full and queued, or one full among
// busy siblings, holds as well; a balanced decode that is not full does
// neither.
func TestImbalanceDecodeGateKeepsPrefillFromLearningBackPressure(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
		{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	roles := map[string]string{"decode-v": domain.RoleDecode, "prefill-v": domain.RolePrefill}
	decode := func(fracs []float64, queues []int) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, 0, len(fracs)+1)
		for i, f := range fracs {
			rows = append(rows, imbRow("decode-"+string(rune('0'+i)), "decode-v", f, queues[i], 6000, 1000, 0.3))
		}
		return rows
	}
	run := func(name string, rows []domain.ReplicaMetrics, settle, wantSaturated, wantHeld bool) {
		t.Helper()
		a, clock := imbAnalyzer()
		rows = append(rows, imbRow("prefill-0", "prefill-v", 0.35, 30, 6000, 1, 4.77))
		sat := roleSaturated(rows, roles, domain.RoleDecode, 5, 0.8)
		var r *domain.AnalyzerResult
		if settle {
			r = imbSettle(t, a, clock, makeAnalyzerInput(rows, states))
		} else {
			// Two cycles: a prefill reading under a deep queue is admitted
			// the cycle after it first shows (k2_settle.go).
			for range 2 {
				var err error
				if r, err = a.Analyze(context.Background(), makeAnalyzerInput(rows, states)); err != nil {
					t.Fatal(err)
				}
				*clock = clock.Add(imbCycle)
			}
		}
		_, held := r.HeldRoleDemand[domain.RolePrefill]
		n := imbK2Windows(a, domain.RolePrefill)
		t.Logf("%s: decodeSaturated=%v prefillK2Windows=%d prefillHeld=%v prefillDemand=%.0f",
			name, sat, n, held, r.RoleDemand[domain.RolePrefill])
		if sat != wantSaturated {
			t.Errorf("%s: decode saturated = %v, want %v", name, sat, wantSaturated)
		}
		if wantSaturated && n != 0 {
			t.Errorf("%s: decode has a full, queued replica, yet prefill recorded %d k2 windows from its back-pressure", name, n)
		}
		if !wantSaturated && n != 1 {
			t.Errorf("%s: decode is not saturated, yet prefill recorded %d k2 windows (want 1)", name, n)
		}
		if held != wantHeld {
			t.Errorf("%s: prefill demand held = %v, want %v", name, held, wantHeld)
		}
	}
	imbalanced := []float64{0.95, 0.02, 0.02, 0.02}
	run("imbalanced, one cycle", decode(imbalanced, []int{20, 0, 0, 0}), false, true, true)
	run("imbalanced, siblings idle for long enough", decode(imbalanced, []int{20, 0, 0, 0}), true, true, false)
	run("uniformly saturated", decode([]float64{0.95, 0.95, 0.95, 0.95}, []int{20, 20, 20, 20}), false, true, true)
	run("full among busy", decode([]float64{0.95, 0.5, 0.5, 0.5}, []int{20, 0, 0, 0}), true, true, true)
	run("balanced, not full", decode([]float64{0.2525, 0.2525, 0.2525, 0.2525}, []int{0, 0, 0, 0}), false, false, false)
}

// The shape-swap replay's prefill reading (queue 30 at 357 800 resident)
// while decode-1 is full with 81 waiting and decode-0 sits at 26 % of its
// cache with nothing waiting: the reading is decode-1's back-pressure, and
// prefill must not record it as its k2, on any cycle.
func TestImbalanceDecodeGateHoldsBesideAnUnderloadedSibling(t *testing.T) {
	a, clock := imbAnalyzer()
	const prefillVariant = "prefill-v"
	decode := func(pod string, tok int64, q int) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, decodeVariant, tok, runKvCapacity, q, 6000, 1000)
		rm.RequestRate = 4.73
		rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
		return rm
	}
	prefill := makeReplicaMetrics("prefill-0", prefillVariant, 357_800, 1_149_312, 30, 6000, 1)
	prefill.RequestRate = 4.77
	prefill.GenerationTokenRate = prefill.RequestRate
	states := []domain.VariantReplicaState{
		{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: prefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{
		prefill,
		decode("decode-0", 300_000, 0),
		decode("decode-1", 1_039_474, 81),
	}, states)
	in.ArrivalRate = runLambda
	imbSettle(t, a, clock, in)
	if n := imbK2Windows(a, domain.RolePrefill); n != 0 {
		t.Errorf("prefill recorded %d k2 windows from decode-1's back-pressure while decode-1 was full and queued", n)
	}
}

// replicaIdle is the balance package's idle: Ready, busiest-rank KV under 0.30,
// nothing waiting.
func TestImbalanceReplicaIdleMatchesBalance(t *testing.T) {
	row := func(frac float64, queue int) domain.ReplicaMetrics {
		return imbRow("p", "v", frac, queue, 100, 100, 0)
	}
	for _, c := range []struct {
		name string
		rm   domain.ReplicaMetrics
		want bool
	}{
		{"empty", row(0, 0), true},
		{"just under", row(0.2999, 0), true},
		{"at the line", row(0.30, 0), false},
		{"queued at low KV", row(0.05, 1), false},
		{"not Ready", func() domain.ReplicaMetrics { r := row(0, 0); r.Ready = false; return r }(), false},
		{"busiest rank over the line", func() domain.ReplicaMetrics {
			r := row(0.1, 0)
			r.EngineRanks, r.MaxRankKvCacheUsage = 2, 0.4
			return r
		}(), false},
		{"tokens with no utilization", func() domain.ReplicaMetrics {
			r := row(0.5, 0)
			r.KvCacheUsage = 0
			return r
		}(), false},
	} {
		if got := replicaIdle(c.rm); got != c.want {
			t.Errorf("%s: idle = %v, want %v", c.name, got, c.want)
		}
	}
}

// imbCycle is the analyzer's cycle in these specs (GLOBAL_OPT_INTERVAL).
const imbCycle = 15 * time.Second

// imbSettle runs in every imbCycle until a replica that reads idle on every
// row has done so for IdleSettledFor, and returns the last result: the
// steady state of a fleet that stays as in describes. A window and two
// cycles over, since a prefill replica's idleness needs a rate on record,
// which is written only once a saturated reading has settled
// (K2SettleLookback).
func imbSettle(t *testing.T, a *SaturationAnalyzer, clock *time.Time, in domain.AnalyzerInput) *domain.AnalyzerResult {
	t.Helper()
	start := *clock
	for {
		r, err := a.Analyze(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if clock.Sub(start) >= IdleSettledFor+K2SettleLookback+2*imbCycle {
			return r
		}
		*clock = clock.Add(imbCycle)
	}
}

// imbFloorFleet is an aggregated variant of replicas whose measured mu is
// about 1 req/s, learned on four replicas over four saturated cycles a minute
// apart, and the analyzer handed rows at lambda: for one cycle, or, with
// settle, until an idle replica among them has read idle for IdleSettledFor.
func imbFloorFleet(t *testing.T, rows []domain.ReplicaMetrics) (implied float64) {
	t.Helper()
	return imbFloorFleetAt(t, rows, 2.0, true)
}

// imbFloorFleetAt is imbFloorFleet at an arrival rate, settled or not.
func imbFloorFleetAt(t *testing.T, rows []domain.ReplicaMetrics, lambda float64, settle bool) (implied float64) {
	t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "agg-v", AcceleratorName: "H200", CurrentReplicas: len(rows), GPUsPerReplica: 1},
	}
	a, clock := imbAnalyzer()
	for i := 0; i < 4; i++ {
		seed := make([]domain.ReplicaMetrics, 0, 4)
		for _, p := range []string{"r0", "r1", "r2", "r3"} {
			// Distinct readings per cycle, so each counts as a sample.
			seed = append(seed, imbRow(p, "agg-v", 0.95, 10, 1000, 1000, []float64{1.0, 1.02, 0.98, 1.01}[i]))
		}
		seedStates := []domain.VariantReplicaState{
			{VariantName: "agg-v", AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
		}
		in := makeAnalyzerInput(seed, seedStates)
		in.ArrivalRate = 4.0
		if _, err := a.Analyze(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		*clock = clock.Add(ThroughputSampleSpacing + time.Second)
	}
	in := makeAnalyzerInput(rows, states)
	in.ArrivalRate = lambda
	var r *domain.AnalyzerResult
	if settle {
		r = imbSettle(t, a, clock, in)
	} else {
		var err error
		if r, err = a.Analyze(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	return r.TotalDemand / (0.85 * imbVariant(t, r, "agg-v").PerReplicaCapacity)
}

// imbFleet is four agg-v replicas r0..r3 at the given KV fractions, queues and
// request rates.
func imbFleet(fracs []float64, queues []int, rates []float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, len(fracs))
	for i := range fracs {
		rows = append(rows, imbRow("r"+string(rune('0'+i)), "agg-v", fracs[i], queues[i], 1000, 1000, rates[i]))
	}
	return rows
}

// A queue the router pinned on one replica, while its siblings idle, is the
// role's backlog: the floor prices requests to drain at mu, and an idle
// sibling's KV room drains none of them. Pinned beside idle siblings or
// standing beside busy ones, the same queue at the same lambda is the same
// backlog -- up to what its replica drains in the drain time, mu x T = 60 at
// mu ~1, since added replicas cannot take a request waiting inside that
// engine (localQueueBacklog). Beside busy siblings the order may be higher
// still, by their resident KV.
func TestImbalanceFloorBacklogCountsAPinnedQueue(t *testing.T) {
	light := []float64{0.10, 0.10, 0.10}
	balanced := imbFloorFleet(t, imbFleet([]float64{0.3125, 0.3125, 0.3125, 0.3125}, []int{0, 0, 0, 0}, []float64{0.5, 0.5, 0.5, 0.5}))
	q100 := imbFloorFleet(t, imbFleet(append([]float64{0.95}, light...), []int{100, 0, 0, 0}, []float64{1.0, 0.33, 0.33, 0.34}))
	busyQ0 := imbFloorFleet(t, imbFleet([]float64{0.95, 0.95, 0.95, 0.95}, []int{0, 0, 0, 0}, []float64{0.5, 0.5, 0.5, 0.5}))
	busyQ100 := imbFloorFleet(t, imbFleet([]float64{0.95, 0.95, 0.95, 0.95}, []int{100, 100, 100, 100}, []float64{0.5, 0.5, 0.5, 0.5}))
	hotAmongBusy := imbFloorFleet(t, imbFleet([]float64{0.95, 0.5, 0.5, 0.5}, []int{100, 0, 0, 0}, []float64{1.0, 0.33, 0.33, 0.34}))
	t.Logf("implied replicas: balanced %.2f; queued 100 on one beside idle siblings %.2f, beside busy ones %.2f",
		balanced, q100, hotAmongBusy)
	t.Logf("implied replicas: every replica full, queue 0 / 100 each %.2f / %.2f", busyQ0, busyQ100)
	// lambda 2 plus the pinned replica's own queue drained in T: (2 + 60/60) / 1.
	drained := (2.0 + 1) / 0.85
	for name, got := range map[string]float64{"beside idle siblings": q100, "beside busy ones": hotAmongBusy} {
		if got < drained-0.05 {
			t.Errorf("a queue of 100 pinned %s implies %.2f replicas, under the %.2f lambda and the replica's own drain need: the backlog was cut for where it stands",
				name, got, drained)
		}
	}
	if !(q100 > balanced+0.5) {
		t.Errorf("a queue of 100 implies %.2f replicas, no more than balanced with none, %.2f", q100, balanced)
	}
	if !(busyQ100 > busyQ0+1) {
		t.Errorf("every replica full and queued 100 implies %.2f replicas, no more than queue 0's %.2f: the backlog of a short fleet was dropped",
			busyQ100, busyQ0)
	}
}

// A queue the router pinned on one prefill replica, while its siblings have
// idled for minutes, is charged as resident prompt in full, as it is beside
// busy siblings. Review probe (round 2): with the three idle replicas' room
// taken off it, the 439 waiting priced 9.3 replicas against the 12.3 the
// resident KV and the queue need.
func TestImbalanceResidencyChargesAPinnedQueue(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
	}
	const prompt = 6000.0
	run := func(fracs []float64, queues []int) float64 {
		t.Helper()
		a, clock := imbAnalyzer()
		rows := make([]domain.ReplicaMetrics, 0, 2+len(fracs))
		rows = append(rows,
			imbRow("decode-0", "decode-v", 0.25, 0, prompt, 1000, 0.5),
			imbRow("decode-1", "decode-v", 0.25, 0, prompt, 1000, 0.5),
		)
		for i := range fracs {
			rate := 1.0
			if fracs[i] == 0 && queues[i] == 0 {
				rate = 0
			}
			rows = append(rows, imbRow("prefill-"+string(rune('0'+i)), "prefill-v", fracs[i], queues[i], prompt, 1, rate))
		}
		return imbSettle(t, a, clock, makeAnalyzerInput(rows, states)).RoleDemand[domain.RolePrefill]
	}
	resident := func(fracs []float64) float64 {
		var s float64
		for _, f := range fracs {
			s += float64(int64(f * float64(imbKv)))
		}
		return s
	}
	hot := []float64{0.20, 0, 0, 0}
	pinned := run(hot, []int{439, 0, 0, 0})
	busy := []float64{0.35, 0.35, 0.35, 0.35}
	busyQ := run(busy, []int{100, 100, 100, 100})
	amongBusy := run(busy, []int{439, 0, 0, 0})
	t.Logf("prefill demand: 439 queued on one, three idle %.0f; every replica 0.35 KV queued 100 %.0f; 439 on one beside busy siblings %.0f",
		pinned, busyQ, amongBusy)
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"439 queued on one beside three idle", pinned, resident(hot) + 439*prompt},
		{"every replica busy and queued 100", busyQ, resident(busy) + 4*100*prompt},
		{"439 queued on one beside busy siblings", amongBusy, resident(busy) + 439*prompt},
	} {
		if c.got != c.want {
			t.Errorf("%s: prefill demand %.0f, want the resident KV plus the whole queue, %.0f", c.name, c.got, c.want)
		}
	}
}

// The collector's KV reading is each pod's one-minute peak. A router that
// moves its hot pod within the minute leaves a full peak on every pod it
// visited, and summed they read as that many full pods. A pod idle now beside
// a sibling busy now is charged what it holds now; with no sibling busy now
// every peak stands.
func TestImbalanceDemandDoesNotSumAMovedPeak(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "agg-v", AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
	}
	const peak = 0.95
	// run prices four pods peaking at peak with the given instant readings;
	// a negative one is a pod whose instant series is missing.
	run := func(instants []float64) float64 {
		t.Helper()
		a, _ := imbAnalyzer()
		rows := make([]domain.ReplicaMetrics, 0, len(instants))
		for i, k := range instants {
			rm := imbRow("r"+string(rune('0'+i)), "agg-v", peak, 0, 1000, 1000, 0.5)
			rm.KvUsageInstant, rm.KvUsageInstantKnown = max(k, 0), k >= 0
			rows = append(rows, rm)
		}
		r, err := a.Analyze(context.Background(), makeAnalyzerInput(rows, states))
		if err != nil {
			t.Fatal(err)
		}
		return r.TotalDemand
	}
	tokens := func(f float64) float64 { return float64(int64(f * float64(imbKv))) }
	allPeaks := 4 * tokens(peak)
	moved := run([]float64{0.95, 0.02, 0.02, 0.02})
	busy := run([]float64{0.90, 0.85, 0.92, 0.88})
	stopped := run([]float64{0.02, 0.02, 0.02, 0.02})
	missing := run([]float64{-1, -1, -1, -1})
	missingBesideBusy := run([]float64{0.95, -1, -1, -1})
	t.Logf("demand, four pods each peaking at %.2f: moved (one full now) %.0f; busy everywhere %.0f; stopped %.0f; no instant reading %.0f; three missing beside one busy %.0f; sum of peaks %.0f",
		peak, moved, busy, stopped, missing, missingBesideBusy, allPeaks)
	if want := tokens(peak) + 3*math.Round(0.02*float64(imbKv)); moved != want {
		t.Errorf("a load moved among four pods within the minute priced %.0f, want the busy pod's peak plus what the idle three hold now, %.0f",
			moved, want)
	}
	for name, got := range map[string]float64{
		"busy everywhere": busy, "stopped": stopped, "no instant reading": missing,
		"three instant series missing beside one busy pod": missingBesideBusy,
	} {
		if got != allPeaks {
			t.Errorf("%s: demand %.0f, want every pod's peak, %.0f", name, got, allPeaks)
		}
	}
}

// A replica full and queued, whose traffic the router then moves off and
// hands back, reads a fraction of its batch under a deep queue the next
// cycle. That is a replica filling, not one at a limit, and must not lower
// the variant's k2: every replica of it is priced there, and a k2 too low is
// never corrected.
func TestImbalanceK2RefusesAFallUnderAQueue(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "agg-v", AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
	}
	a, clock := imbAnalyzer()
	full := make([]domain.ReplicaMetrics, 0, 4)
	for _, p := range []string{"r0", "r1", "r2", "r3"} {
		full = append(full, imbRow(p, "agg-v", 270_539.0/float64(imbKv), 10, 1000, 1000, 6.0))
	}
	in := makeAnalyzerInput(full, states)
	in.ArrivalRate = 24
	if _, err := a.Analyze(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	before := imbK2Average(a)
	*clock = clock.Add(ThroughputSampleSpacing + time.Second)
	step := []domain.ReplicaMetrics{
		imbRow("r0", "agg-v", 31_713.0/float64(imbKv), 160, 1000, 1000, 0.0019),
		imbRow("r1", "agg-v", 0, 0, 1000, 1000, 0),
		imbRow("r2", "agg-v", 0, 0, 1000, 1000, 0),
		imbRow("r3", "agg-v", 0, 0, 1000, 1000, 0),
	}
	in = makeAnalyzerInput(step, states)
	in.ArrivalRate = 0.8
	r, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	after := imbK2Average(a)
	t.Logf("k2 window: %.0f before; %.0f after one replica read 31 713 under 160 queued; priced at %.0f",
		before, after, imbVariant(t, r, "agg-v").PerReplicaCapacity)
	if after != before {
		t.Errorf("a fall under a deep queue moved the variant's k2 %.0f -> %.0f", before, after)
	}
}

// imbK2Average is the mean of the one k2 window the analyzer holds.
func imbK2Average(a *SaturationAnalyzer) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var avg float64
	for _, ra := range a.computeCapacityHistory {
		if ra.Len() > 0 {
			avg = ra.Average()
		}
	}
	return avg
}

// A replica with requests waiting has not handed its load away, whatever its
// instant KV reads: that dips between prefill batches -- a P/D prefill
// replica's KV is transient until decode pulls it -- and on preemption. A
// fleet saturated and queued everywhere is charged its peaks however its
// instant readings scatter.
func TestImbalanceAQueuedReplicaKeepsItsPeak(t *testing.T) {
	agg := func(instants []float64) float64 {
		t.Helper()
		a, _ := imbAnalyzer()
		rows := make([]domain.ReplicaMetrics, 0, len(instants))
		for i, k := range instants {
			rm := imbRow("pod-"+string(rune('a'+i)), "v", 0.95, 20, 2000, 500, 1.0)
			rm.KvUsageInstant = k
			rows = append(rows, rm)
		}
		states := []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: len(rows), GPUsPerReplica: 1}}
		r, err := a.Analyze(context.Background(), makeAnalyzerInput(rows, states))
		if err != nil {
			t.Fatal(err)
		}
		return r.TotalDemand
	}
	even := agg([]float64{0.9, 0.9, 0.9, 0.9})
	dipped := agg([]float64{0.9, 0.2, 0.2, 0.2})
	t.Logf("four pods peaking at 0.95, 20 queued each: instants 0.9 everywhere %.0f; 0.9/0.2/0.2/0.2 %.0f", even, dipped)
	if dipped != even {
		t.Errorf("queued replicas whose instant KV dipped were charged at the dip: demand %.0f, %.0f with the instants even", dipped, even)
	}

	states := []domain.VariantReplicaState{
		{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
	}
	prefill := func(instants []float64) float64 {
		t.Helper()
		a, _ := imbAnalyzer()
		rows := make([]domain.ReplicaMetrics, 0, 2+len(instants))
		rows = append(rows,
			imbRow("d0", "decode-v", 0.25, 0, 6000, 1000, 0.5),
			imbRow("d1", "decode-v", 0.25, 0, 6000, 1000, 0.5),
		)
		for i, k := range instants {
			rm := imbRow("p"+string(rune('0'+i)), "prefill-v", 0.6, 30, 6000, 1, 1.0)
			rm.KvUsageInstant = k
			rows = append(rows, rm)
		}
		r, err := a.Analyze(context.Background(), makeAnalyzerInput(rows, states))
		if err != nil {
			t.Fatal(err)
		}
		return r.RoleDemand[domain.RolePrefill]
	}
	pEven := prefill([]float64{0.4, 0.4, 0.4, 0.4})
	pDipped := prefill([]float64{0.4, 0.1, 0.1, 0.1})
	t.Logf("P/D prefill, four pods peaking at 0.6 with 30 queued: instants 0.4 everywhere %.0f; 0.4/0.1/0.1/0.1 %.0f", pEven, pDipped)
	if pDipped != pEven {
		t.Errorf("queued prefill replicas between batches were charged at their instant KV: demand %.0f, %.0f with the instants even", pDipped, pEven)
	}
}
