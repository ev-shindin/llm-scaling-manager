package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The two rules that bound one slow pod (mu_slow_pod.go): the unread
// replica's credit (SwitchMuUnreadAtTarget) and the minority's figure
// expiring (SwitchMuMinorityFigureExpires). The closed-loop probe holds them
// together (TestClosedLoopProbe_ASlowReplicaInsideTheServedBoundStaysUnderTheBound);
// these hold each edge alone, each run on and off.

// slowPodRows is n replicas of variant v at 1000 in, 1000 out: the first
// readers queued with a full cache, serving slow, and the rest unqueued at
// fill kv, serving rate.
func slowPodRows(n, readers int, slow, rate, kv float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		if i < readers {
			rows = append(rows, imbRow(fmt.Sprintf("r%d", i), "v", 0.95, 10, 1000, 1000, slow))
			continue
		}
		rows = append(rows, imbRow(fmt.Sprintf("r%d", i), "v", kv, 0, 1000, 1000, rate))
	}
	return rows
}

// slowPodRig is a rig with switches set.
func slowPodRig(t *testing.T, switches map[string]bool) *propRig {
	t.Helper()
	rig := newPropRig()
	require.NoError(t, rig.a.SetSwitchesForReplay(switches))
	return rig
}

// An unread replica under the KV line counts at what it serves over the
// target; one with a full cache, at what it serves; and a target outside
// (0, 1), the switch off, credits nothing.
func TestMuSlowPod_AnUnreadReplicaUnderTheKVLineCountsAtWhatItServesOverTheTarget(t *testing.T) {
	served := servedRates{pods: map[string]servedPod{
		"slow": {rate: 0.4, queued: true, full: true},
		"open": {rate: 0.6},
		"full": {rate: 0.9, full: true},
	}}
	reads := map[string]float64{"slow": 0.4}
	for _, c := range []struct {
		target, want float64
	}{
		{0.85, (0.6/0.85 + 0.9) / 2},
		{0, (0.6 + 0.9) / 2},
		{1, (0.6 + 0.9) / 2},
		{1.2, (0.6 + 0.9) / 2},
	} {
		s := served
		s.target = c.target
		got, n := s.unread(reads)
		require.Equal(t, 2, n)
		require.InDelta(t, c.want, got, 1e-12, "target %v", c.target)
	}
}

// Through Analyze: one replica queued at 0.4 beside three unqueued at half
// their cache serving 0.6. The sample counts the three at 0.6 / 0.85, so mu
// is (0.4 + 3 x 0.706) / 4 = 0.629. With the credit off it counts them at
// 0.6, 0.550: the floor would then charge the target on a rate they were
// only offered.
func TestMuSlowPod_TheCreditReachesTheSample(t *testing.T) {
	for _, c := range []struct {
		on   bool
		want float64
	}{
		{true, (0.4 + 3*0.6/floorTarget) / 4},
		{false, (0.4 + 3*0.6) / 4},
	} {
		rig := slowPodRig(t, map[string]bool{SwitchMuUnreadAtTarget: c.on})
		for range 13 {
			_, err := rig.analyze(slowPodRows(4, 1, 0.4, 0.6, 0.5), aggStates(4), 2.2, propCycle)
			require.NoError(t, err)
		}
		got := rig.measuredMu()
		t.Logf("credit %v: mu %.4f, want %.4f", c.on, got, c.want)
		require.InDelta(t, c.want, got, 1e-3*c.want, "credit %v", c.on)
	}
}

// The writers are a minority when fewer than half of the replicas that read
// the newest sample and the unqueued ones that did not; a queued replica that
// did not read it counts on neither side.
func TestMuSlowPod_TheWritersAreAMinorityOfTheReadAndUnqueued(t *testing.T) {
	queued := servedPod{rate: 1, queued: true, full: true}
	open := servedPod{rate: 1}
	for _, c := range []struct {
		name     string
		pods     map[string]servedPod
		reads    []string
		minority bool
	}{
		{"one of four read", map[string]servedPod{"a": queued, "b": open, "c": open, "d": open}, []string{"a"}, true},
		{"two of four read: half is no minority", map[string]servedPod{"a": queued, "b": queued, "c": open, "d": open}, []string{"a", "b"}, false},
		{"every replica read: saturated together", map[string]servedPod{"a": queued, "b": queued, "c": queued, "d": queued}, []string{"a", "b", "c", "d"}, false},
		{"a queued replica not read counts on neither side", map[string]servedPod{"a": queued, "b": queued, "c": queued, "d": open}, []string{"a"}, false},
	} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		reads := podReads{pods: map[string]float64{}}
		for _, p := range c.reads {
			reads.pods[p] = 1
		}
		a.throughputMuChoice[propMuKey] = &muChoice{}
		a.throughputPodReads[propMuKey] = reads
		a.noteSampleWriters(propMuKey, servedRates{pods: c.pods}, muSampled)
		require.Equal(t, c.minority, a.throughputMuChoice[propMuKey].minority, c.name)
	}
}

// A minority's figure with no line gives way to the fleet's served bound
// once nothing has read saturated in agreement with it for lowHoldFor, not a
// cycle before; and never with a line (the hold already releases it to the
// derived figure), at or over the bound (a steering router's minority read
// what every replica does), with the fleet's figure, with no choice on
// record (a restore: the safe side), for a borrowed reading, or with the
// switch off. It logs on the first released cycle only.
func TestMuSlowPod_AMinorityFigureExpiresOnlyPastTheHold(t *testing.T) {
	tn := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	hold := tn.MuChoice.LowHoldFor.Duration()
	at := propStart
	reading := throughputReading{rate: 0.55, sampledAt: at, writtenAt: at}
	// The fleet: two unqueued replicas at half their cache serving 0.6, so
	// at the 0.85 target the bound is 0.706, and one queued at 0.55.
	fleet := servedRates{target: floorTarget, pods: map[string]servedPod{
		"a": {rate: 0.6}, "b": {rate: 0.6}, "c": {rate: 0.55, queued: true},
	}}
	bound := (2*0.6/floorTarget + 0.55) / 3
	type state struct {
		on, minority, choice, derivedOK bool
		reading                         throughputReading
		served                          servedRates
	}
	base := state{on: true, minority: true, choice: true, reading: reading, served: fleet}
	expired := func(s state, now time.Time) minorityRelease {
		a := NewSaturationAnalyzer(capacity.NewStore())
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuMinorityFigureExpires: s.on}))
		if s.choice {
			a.throughputMuChoice[propMuKey] = &muChoice{minority: s.minority, saturatedAt: at}
		}
		return a.minorityFigureExpired(propMuKey, s.reading, s.served, s.derivedOK, tn, now)
	}
	with := func(edit func(*state)) state { s := base; edit(&s); return s }
	late := at.Add(2 * hold)

	require.False(t, expired(base, at.Add(hold-propCycle)).released, "inside the hold")
	rel := expired(base, at.Add(hold))
	require.True(t, rel.released, "past the hold")
	require.InDelta(t, bound, rel.bound, 1e-12, "released to the fleet's served bound, not to no mu")
	require.True(t, rel.first, "the first released cycle logs")
	require.False(t, expired(with(func(s *state) { s.derivedOK = true }), late).released, "a line prices it")
	require.False(t, expired(with(func(s *state) { s.minority = false }), late).released, "the fleet wrote it")
	require.False(t, expired(with(func(s *state) { s.choice = false }), late).released, "no choice on record")
	require.False(t, expired(with(func(s *state) { s.on = false }), late).released, "the switch off")
	borrowed := reading
	borrowed.borrowed = true
	require.False(t, expired(with(func(s *state) { s.reading = borrowed }), late).released, "a borrowed reading is no window of the key's")
	high := reading
	high.rate = bound
	require.False(t, expired(with(func(s *state) { s.reading = high }), late).released, "at the bound the figure is kept")

	// Logged once per run of released cycles, per key.
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.throughputMuChoice[propMuKey] = &muChoice{minority: true, saturatedAt: at}
	firsts := 0
	for i := range 3 {
		for range 4 { // four replicas of the key on one cycle
			if r := a.minorityFigureExpired(propMuKey, reading, fleet, false, tn, late.Add(time.Duration(i)*propCycle)); r.first {
				firsts++
			}
		}
	}
	require.Equal(t, 1, firsts, "one line for the whole run, not one per replica per cycle")

	// A run that ends on any path, the hold not met or an early return,
	// logs again when the next run starts: release, not released, release
	// gives two first lines, whichever path ended the first run.
	ends := map[string]func() minorityRelease{
		"the hold not met": func() minorityRelease {
			return a.minorityFigureExpired(propMuKey, reading, fleet, false, tn, at)
		},
		"a line": func() minorityRelease {
			return a.minorityFigureExpired(propMuKey, reading, fleet, true, tn, late)
		},
		"a borrowed reading": func() minorityRelease {
			return a.minorityFigureExpired(propMuKey, borrowed, fleet, false, tn, late)
		},
		"the fleet's figure": func() minorityRelease {
			a.throughputMuChoice[propMuKey].minority = false
			r := a.minorityFigureExpired(propMuKey, reading, fleet, false, tn, late)
			a.throughputMuChoice[propMuKey].minority = true
			return r
		},
	}
	for name, end := range ends {
		a = NewSaturationAnalyzer(capacity.NewStore())
		a.throughputMuChoice[propMuKey] = &muChoice{minority: true, saturatedAt: at}
		r1 := a.minorityFigureExpired(propMuKey, reading, fleet, false, tn, late)
		require.False(t, end().released, "%s: premise: not released", name)
		r2 := a.minorityFigureExpired(propMuKey, reading, fleet, false, tn, late)
		require.True(t, r1.first && r2.first, "%s: release, not released, release logs twice", name)
	}
}

// slowPodPriced is what Analyze priced one replica's saturated throughput
// at, and the bucket it named, from its replica-capacity-decision line.
type slowPodPriced struct {
	rate   float64
	bucket string
}

// slowPodEpisode runs a fleet of four through Analyze: readers queued at
// 0.55 beside the rest unqueued at 0.6 for 3 min, then all four unqueued at
// 0.4 of their cache serving 0.6 for 40 min, and returns when the minority's
// figure first expired, from the end of the saturated part (negative when it
// never did), the mu on record at the end, and what the last cycle priced
// each replica at.
func slowPodEpisode(t *testing.T, readers int, switches map[string]bool) (time.Duration, float64, []slowPodPriced) {
	t.Helper()
	rig := slowPodRig(t, switches)
	for range 12 {
		_, err := rig.analyze(slowPodRows(4, readers, 0.55, 0.6, 0.5), aggStates(4), 2.4, propCycle)
		require.NoError(t, err)
	}
	ctx, logs := observedCtx(t)
	rig.ctx = ctx
	quietFrom := rig.clock
	first := time.Duration(-1)
	for rig.clock.Sub(quietFrom) < 40*time.Minute {
		elapsed := rig.clock.Sub(quietFrom)
		logs.TakeAll()
		_, err := rig.analyze(slowPodRows(4, 0, 0, 0.6, 0.4), aggStates(4), 2.4, propCycle)
		require.NoError(t, err)
		if first < 0 && muCount(rig.a, muReasonMinorityExpired) > 0 {
			first = elapsed
		}
	}
	entries := logs.FilterMessage("replica-capacity-decision").All()
	priced := make([]slowPodPriced, 0, len(entries))
	for _, e := range entries {
		f := e.ContextMap()
		rate, _ := f["saturatedThroughput"].(float64)
		bucket, _ := f["saturatedThroughputBucket"].(string)
		priced = append(priced, slowPodPriced{rate: rate, bucket: bucket})
	}
	return first, rig.measuredMu(), priced
}

// One replica of four wrote the figure: it expires 30 min (lowHoldFor) after
// its last saturated reading, and with the switch off it never does.
func TestMuSlowPod_OneReplicasFigureExpiresAfterTheHold(t *testing.T) {
	hold := tuningDefault.WithDefaults().MuChoice.LowHoldFor.Duration()
	first, mu, priced := slowPodEpisode(t, 1, nil)
	t.Logf("one reader: expired %s after the fleet stopped queueing; mu on record %.3f; priced %v", first, mu, priced)
	require.Positive(t, mu, "premise: the slow replica wrote a figure")
	require.GreaterOrEqual(t, first, hold-propCycle, "not before the hold")
	require.Less(t, first, hold+time.Minute, "within a minute of it")
	// What Analyze priced, not the window: every replica at the quiet
	// fleet's served bound, four unqueued at 0.6 under the KV line, credited
	// over the target, and named as such.
	bound := 0.6 / floorTarget
	require.Len(t, priced, 4)
	for _, p := range priced {
		require.InDelta(t, bound, p.rate, 1e-9, "released to the fleet's served bound")
		require.Equal(t, servedBoundBucket, p.bucket)
	}
	require.Greater(t, bound, mu, "premise: the figure on record sits under the bound")

	off, _, kept := slowPodEpisode(t, 1, map[string]bool{SwitchMuMinorityFigureExpires: false})
	require.Negative(t, off, "with the switch off a minority's figure stands")
	for _, p := range kept {
		require.InDelta(t, mu, p.rate, 1e-9, "with the switch off the figure on record prices")
	}
}

// A fleet that saturated together keeps its figure: every replica read it,
// so it is no minority's, and stands however long nothing queues.
func TestMuSlowPod_AFleetThatSaturatedTogetherKeepsItsFigure(t *testing.T) {
	first, mu, _ := slowPodEpisode(t, 4, nil)
	t.Logf("four readers: first expiry %s; mu on record %.3f", first, mu)
	require.Positive(t, mu, "premise: the fleet wrote a figure")
	require.Negative(t, first, "the fleet's own figure never expires")
}

// The two ledger cases #495 cleared, each failing with its switch off, as
// they did on the parent: the slow reading refused (SwitchMuWeighsFleet) and
// the yield to the full siblings' median (SwitchMuYieldCappedAtMeasurement).
func TestMuSlowPod_TheClearedLedgerCasesFailWithTheirSwitchOff(t *testing.T) {
	for _, c := range []struct {
		name  string
		check func(map[string]bool) func(uint64) error
		off   string
	}{
		{"half-served-max-slow-pod", checkMuHalfServedWith, SwitchMuWeighsFleet},
		{"sibling-yield-slow-pod", checkMuSiblingYieldWith, SwitchMuYieldCappedAtMeasurement},
	} {
		for seed := uint64(1); seed <= 3; seed++ {
			require.NoError(t, c.check(nil)(seed), "%s seed %d with the switch on", c.name, seed)
			err := c.check(map[string]bool{c.off: false})(seed)
			var v *learnertest.Violation
			require.ErrorAs(t, err, &v, "%s seed %d with %s off", c.name, seed, c.off)
			require.Equal(t, kindOverPriced, v.Kind)
			t.Logf("%s, %s off, seed %d: %s", c.name, c.off, seed, v.Msg)
		}
	}
}

// The sibling yield never prices mu above the measurement it replaces
// (SwitchMuYieldCappedAtMeasurement). The fixture is the ttft-three-pods
// cycle the uncapped fleet yield moved: a variant measured at 3.39 whose full
// replicas served 5.18 unqueued. Capped, there is no yield; uncapped, the
// negative control, it priced 5.18, 53 % over the measurement.
func TestMuSlowPod_TheYieldNeverPricesAboveTheMeasurement(t *testing.T) {
	tn := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	served := servedRates{max: 5.18, saturated: 5.18, unqueued: true}
	for _, capped := range []bool{true, false} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuYieldCappedAtMeasurement: capped}))
		rate, yielded := a.siblingYield(propMuKey, throughputReading{rate: 3.39}, served, tn)
		t.Logf("capped %v: yielded %v at %.2f against a measured 3.39", capped, yielded, rate)
		if capped {
			require.False(t, yielded, "a yield priced above the measurement")
		} else {
			require.True(t, yielded, "premise: uncapped, the yield prices the siblings")
			require.Greater(t, rate, 3.39)
		}
	}

	// Through Analyze, the weighing off so the yield alone decides: p0 queued
	// at 3.39, two full siblings unqueued at 5.18.
	bucket := func(switches map[string]bool) string {
		a := newE1Analyzer(t, false)
		require.NoError(t, a.SetSwitchesForReplay(switches))
		var b map[string]string
		for i := range 20 {
			rows := make([]e1Row, 0, 3)
			rows = append(rows, e1Row{"p0", 30, 330_000, 3.39, 3.39*498 + float64(i%3)})
			for _, p := range []string{"p1", "p2"} {
				rows = append(rows, e1Row{p, 0, 330_000, 5.18, 5.18*498 + float64(i%3)})
			}
			b, _ = tunedE1(t, a, e1Start.Add(time.Duration(i)*15*time.Second), rows, nil)
		}
		return b["p0"]
	}
	require.NotEqual(t, servedBucket, bucket(map[string]bool{SwitchMuWeighsFleet: false}), "capped, p0 priced its siblings")
	require.Equal(t, servedBucket, bucket(map[string]bool{SwitchMuWeighsFleet: false, SwitchMuYieldCappedAtMeasurement: false}),
		"premise: uncapped, p0 yields to its siblings")
	require.NotEqual(t, servedBucket, bucket(nil), "all on, p0 priced its siblings")
}

// The credit's target is the one the floor charges on mu: the saturation
// analyzer's own scaleUpThreshold (config.AnalyzerThresholds), not the
// policy-wide one. A policy at 0.7 with the analyzer overridden to 0.95:
// one replica queued at 0.4 beside three unqueued at half their cache
// serving 0.6 reads (0.4 + 3 x 0.6 / 0.95) / 4 = 0.574. Negative control:
// credited at the policy-wide 0.7, as first built, it read
// (0.4 + 3 x 0.6 / 0.7) / 4 = 0.743, 29 % over, priced over what the floor
// then takes back off at 0.95.
func TestMuSlowPod_TheCreditUsesTheSaturationAnalyzersTarget(t *testing.T) {
	rig := slowPodRig(t, nil)
	up := 0.95
	rig.policy = func(p *config.ScalingPolicy) {
		p.ScaleUpThreshold = 0.7
		p.Analyzers = []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName, ScaleUpThreshold: &up}}
	}
	for range 13 {
		_, err := rig.analyze(slowPodRows(4, 1, 0.4, 0.6, 0.5), aggStates(4), 2.2, propCycle)
		require.NoError(t, err)
	}
	got := rig.measuredMu()
	want, global := (0.4+3*0.6/up)/4, (0.4+3*0.6/0.7)/4
	t.Logf("mu %.4f; at the analyzer's 0.95 %.4f, at the policy's 0.7 %.4f", got, want, global)
	require.InDelta(t, want, got, 1e-3*want, "the credit's target is the saturation analyzer's")
}

// The weighing's hold (muChoice.unread): the unread replicas count at the
// lower of two consecutive samples' rates at most, held while the readers sit
// under the tolerance of them. One queued replica at 0.4 beside three
// unqueued at half their cache serving 0.6, then one minute's drain burst at
// 0.8, then the three falling to 0.55, 0.5 and 0.45 as a growing fleet would
// share the load. The burst lifts its own sample and no other, and the
// falling minutes hold the 0.6 the three showed. Read fresh (h := mean in
// weighUnreadReplicas) the falling minutes fall with them, which orders more,
// which lowers them again: the 0.3 probe peaked at 28 that way, against 24.
func TestMuSlowPod_TheWeighingHoldsAFallingFleetAndCarriesNoBurst(t *testing.T) {
	rig := slowPodRig(t, nil)
	served := []float64{0.6, 0.6, 0.6, 0.6, 0.8, 0.6, 0.55, 0.5, 0.45}
	for _, s := range served {
		for range 4 { // a minute, one sample
			_, err := rig.analyze(slowPodRows(4, 1, 0.4, s, 0.5), aggStates(4), 0.4+3*s, propCycle)
			require.NoError(t, err)
		}
	}
	var samples []float64
	rig.a.mu.Lock()
	for _, ra := range rig.a.saturatedThroughput {
		samples = ra.Snapshot().Values
	}
	rig.a.mu.Unlock()
	t.Logf("samples %.4f", samples)
	require.GreaterOrEqual(t, len(samples), 5, "premise: the window holds the burst and the falling minutes")
	held := (0.4 + 3*0.6/floorTarget) / 4
	over := 0
	for _, v := range samples {
		if v > held*(1+1e-9) {
			over++
		}
	}
	require.Equal(t, 1, over, "the burst lifts its own sample only")
	for _, v := range samples[len(samples)-3:] {
		require.InDelta(t, held, v, 1e-9, "a falling fleet's samples hold what it showed")
	}
}

// How the weighing moves demand through the floor (analyzer-learner.md, "One
// slow pod, as built"), pinned on the start-seed-restart-2 golden's cycle at
// 21:22:34, the replay's own numbers: occupancy's demand 8193 tokens, of
// which 48 resident, 2.177 req/s arriving, and the floor's term at mu 0.3506
// with the weighing on and 0.3657 off. The floor's tokens rise as mu falls
// (53.2 against 51.1), but the trust blend reads lambda x P / mu against the
// resident KV, which a lower mu raises, so more of the queue's residency
// charge is swapped for the floor's drain price: 699 against 1527.
// localQueueBacklog is not the term: the capped backlog is 55.99 requests
// either way, and in replicas the cap is min(q / (mu x T), 1) per replica,
// which never falls as mu does.
func TestMuSlowPod_TheFloorTrustIsHowALowerMuLowersDemand(t *testing.T) {
	tn := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	const measured, resident, lambda = 8193.0, 48.0, 2.1772658136774585
	at := func(mu, perReplica, replicas float64) float64 {
		tokens := replicas * perReplica
		return tn.flooredDemand(measured, resident, tokens, lambda*perReplica/mu, true)
	}
	on := at(0.3506359494741492, 6.0000310445465095, 8.87084602889296)
	off := at(0.3657028708413113, 6.011312673169893, 8.505368067863964)
	t.Logf("floored demand %.2f with the weighing, %.2f without", on, off)
	require.InDelta(t, 699.28948785588, on, 0.01)
	require.InDelta(t, 1527.275814274085, off, 0.01)
	// Without the swap, the same terms floor at occupancy's demand either way.
	require.Equal(t, measured, tn.flooredDemand(measured, resident, 53.2, lambda*6/0.35, false))
}
