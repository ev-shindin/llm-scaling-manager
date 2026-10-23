package allocation

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"
)

// stallPhase is a stretch of constant need per role, in GPUs at the scale-up
// threshold.
type stallPhase struct {
	dur  time.Duration
	need map[string]float64
}

// stallCycle is what one cycle of stallLoop left.
type stallCycle struct {
	at        time.Duration
	held      map[string]int
	swinging  []string
	donorPeak int
	started   int
	// swingNeeds is the plan's SwingNeeds: what each swinging role was
	// planned on, and what it read.
	swingNeeds map[string]ShareSwingNeed
}

// stallLoop runs two roles of 1-GPU replicas in a quota of 4 through phases,
// cycle by cycle, closing the loop as runShareSim does -- a donor's replica
// leaves simRelease after its transfer starts, a receiver is raised the cycle
// its transfer fills -- and with the idle fill the actuator runs after each
// plan (fillIdleShare). With lag, each role's need is read as the mean of the
// samples from 2·simLag to simLag ago, as runShareSim reads it.
func stallLoop(tm ShareTimings, initial map[string]int, lag bool, phases []stallPhase) []stallCycle {
	return stallLoopServed(tm, initial, lag, phases, nil)
}

// stallLoopServed is stallLoop with each role's need read through served,
// given the need its phase states and the GPUs the role holds: what a role
// whose demand is measured from what it serves reads while it is short.
func stallLoopServed(tm ShareTimings, initial map[string]int, lag bool, phases []stallPhase, served func(need float64, held int) float64) []stallCycle {
	return stallLoopSized(tm, 1, initial, lag, phases, served)
}

// stallLoopSized is stallLoopServed with replicas of g GPUs each in a quota
// of four replicas: initial, every need and every split are in GPUs.
func stallLoopSized(tm ShareTimings, g int, initial map[string]int, lag bool, phases []stallPhase,
	served func(need float64, held int) float64) []stallCycle {
	budget := 4 * g
	keys := []string{"A", "B"}
	l := NewShareLedger()
	t0 := time.Unix(1_000_000, 0)
	held := map[string]int{}
	for k, v := range initial {
		held[k] = v
	}
	released, granted := map[string]bool{}, map[string]bool{}
	samples := map[string][]float64{}
	var total time.Duration
	for _, ph := range phases {
		total += ph.dur
	}
	var out []stallCycle
	start, pi := time.Duration(0), 0
	for t := time.Duration(0); t < total; t += simCycle {
		if t >= start+phases[pi].dur {
			start += phases[pi].dur
			pi++
		}
		now := t0.Add(t)
		for _, tr := range l.Transfers() {
			switch {
			case tr.State == ShareReleasing && tr.Donor != "" && !released[tr.ID] && now.Sub(tr.Started) >= simRelease:
				held[tr.Donor] -= tr.DonorGPUs
				released[tr.ID] = true
			case tr.State == ShareFilling && tr.Receiver != "" && !granted[tr.ID]:
				held[tr.Receiver] += tr.GPUs
				granted[tr.ID] = true
			}
		}
		l.Observe(held, now, tm)
		l.TakeReleased()
		in := SharePlanInput{Held: held, Thresholds: map[string]float64{}, DownThresholds: map[string]float64{},
			Budget: budget, Tolerance: 0.15, PhysicalFree: math.MaxInt,
			Headroom: ShareHeadroom{Capped: true, Replicas: 1, ReturnLoad: 0.6}}
		for _, k := range keys {
			need := phases[pi].need[k]
			if served != nil {
				need = served(need, held[k])
			}
			samples[k] = append(samples[k], need)
			if lag {
				s, n := samples[k], int(simLag/simCycle)
				lo, hi := max(0, len(s)-1-2*n), max(0, len(s)-1-n)
				sum := 0.0
				for _, v := range s[lo : hi+1] {
					sum += v
				}
				need = sum / float64(hi-lo+1)
			}
			in.Roles = append(in.Roles, ShareRole{Key: k, Weight: 1, Need: need, Floor: g, MinFloor: g, ReplicaGPUs: g})
			in.Thresholds[k], in.DownThresholds[k] = 0.85, 0.70
		}
		plan := PlanShareTransfers(l, in, now, tm)

		// The idle fill, as fillIdleShare runs it without node information.
		committed := l.Committed(held)
		idle := budget - l.WakeHeld(now) - l.Unreleased()
		for _, c := range committed {
			idle -= c
		}
		integer := map[string]int{}
		for _, v := range plan.Evaluation.Roles {
			integer[v.Key] = v.Integer
		}
		byKey := map[string]ShareRole{}
		for _, r := range in.Roles {
			byKey[r.Key] = r
		}
		var receivers []string
		for _, k := range keys {
			if committed[k] < integer[k] && !l.FillHeld(k, now) && !l.Giving(k) {
				receivers = append(receivers, k)
			}
		}
		z := func(k string) float64 { return byKey[k].Score(float64(committed[k])) }
		slices.SortFunc(receivers, func(a, b string) int { return cmp.Or(cmp.Compare(z(a), z(b)), cmp.Compare(a, b)) })
		for _, rc := range receivers {
			for n := 0; n < ShareFillPace(byKey[rc].Need, committed[rc]) && idle >= g && committed[rc] < integer[rc]; n++ {
				l.StartFill(rc, "v", g, float64(committed[rc]) < byKey[rc].Need, held, now, tm)
				idle -= g
				committed[rc] += g
			}
		}
		l.SetFillShort(slices.DeleteFunc(slices.Clone(receivers), func(rc string) bool { return committed[rc] >= integer[rc] }))

		out = append(out, stallCycle{at: t, held: map[string]int{"A": held["A"], "B": held["B"]},
			swinging: plan.Swinging, donorPeak: plan.Withheld[ShareWithheldDonorPeak], started: len(plan.Started),
			swingNeeds: maps.Clone(plan.SwingNeeds)})
	}
	return out
}

// stallReached is when, at or after from, the loop first held a:b, and whether it
// ever did.
func stallReached(cycles []stallCycle, from time.Duration, a, b int) (time.Duration, bool) {
	for _, c := range cycles {
		if c.at >= from && c.held["A"] == a && c.held["B"] == b {
			return c.at - from, true
		}
	}
	return 0, false
}

// stallHeldAt is the loop's split at the last cycle before at.
func stallHeldAt(cycles []stallCycle, at time.Duration) (int, int) {
	var last stallCycle
	for _, c := range cycles {
		if c.at >= at {
			break
		}
		last = c
	}
	return last.held["A"], last.held["B"]
}

// stallSwap returns phases with the two roles' needs exchanged: the mirror order.
func stallSwap(phases []stallPhase) []stallPhase {
	out := slices.Clone(phases)
	for i, ph := range out {
		out[i].need = map[string]float64{"A": ph.need["B"], "B": ph.need["A"]}
	}
	return out
}

func stallNeeds(a, b float64) map[string]float64 { return map[string]float64{"A": a, "B": b} }

// A donor whose load fell is not held at its peak over the swing window. A
// new group plans with its headroom capped until it has a swing window of
// history, so the first swap, 1:1 to 3:1, goes through. By the swap back the
// peaks are known and fit the quota (2 + 2 of 4), so the spare is spread, and
// the receiver, already at its need, takes no replica that leaves its donor
// below the donor's peak (donor-peak). Judged on its peak over the swing
// window, the donor kept its second replica until that peak aged out: the
// pair sat at 2:2 for 95 minutes. Judged on its peak over its scale-down
// window, the second replica moves once that window has passed. Both orders.
func TestShareSwapBackIsNotHeldOnTheDonorsOldPeak(t *testing.T) {
	const m = time.Minute
	phases := []stallPhase{{60 * m, stallNeeds(1, 1)}, {60 * m, stallNeeds(1.5, 0.5)}, {120 * m, stallNeeds(0.5, 1.5)}}
	for _, order := range []struct {
		name   string
		phases []stallPhase
		first  [2]int
		final  [2]int
	}{
		{"A then B", phases, [2]int{3, 1}, [2]int{1, 3}},
		{"B then A", stallSwap(phases), [2]int{1, 3}, [2]int{3, 1}},
	} {
		cycles := stallLoop(simTimings(), map[string]int{"A": 1, "B": 1}, false, order.phases)
		if a, b := stallHeldAt(cycles, 120*m); a != order.first[0] || b != order.first[1] {
			t.Fatalf("%s, setup: the first swap left %d:%d, want %d:%d", order.name, a, b, order.first[0], order.first[1])
		}
		held := 0
		for _, c := range cycles {
			if c.at >= 120*m {
				held += c.donorPeak
			}
		}
		if held == 0 {
			t.Fatalf("%s, setup: donor-peak never held the swap back; the scenario no longer reaches the rule", order.name)
		}
		after, ok := stallReached(cycles, 120*m, order.final[0], order.final[1])
		if !ok || after > 20*m {
			t.Errorf("%s: the swap back reached %d:%d after %v (reached %v), want within 20m: the donor was held on its old peak",
				order.name, order.final[0], order.final[1], after, ok)
			continue
		}
		t.Logf("%s: swap back reached %d:%d after %v, donor-peak held %d cycles", order.name, order.final[0], order.final[1], after, held)
	}
}

// A dip inside the donor's one busy stretch does not make its peak a
// recurring one. The swap back of TestShareSwapBackIsNotHeldOnTheDonorsOldPeak,
// with the busy model's need falling to the other's for a minute, twice, in
// its busy hour: counted as three bursts, the donor kept its peak over the
// swing window, and the swap back waited over an hour for it to age out.
// Both orders.
func TestShareSwapBackIsNotHeldOnADipInTheDonorsBurst(t *testing.T) {
	const m = time.Minute
	busy, dip := stallNeeds(1.5, 0.5), stallNeeds(0.5, 1.5)
	plain := []stallPhase{{60 * m, stallNeeds(1, 1)}, {60 * m, busy}, {120 * m, dip}}
	dipped := []stallPhase{{60 * m, stallNeeds(1, 1)}, {20 * m, busy}, {m, dip}, {19 * m, busy}, {m, dip}, {19 * m, busy}, {120 * m, dip}}
	for _, order := range []struct {
		name          string
		plain, dipped []stallPhase
		final         [2]int
	}{
		{"A then B", plain, dipped, [2]int{1, 3}},
		{"B then A", stallSwap(plain), stallSwap(dipped), [2]int{3, 1}},
	} {
		heldAfter := func(cycles []stallCycle) int {
			held := 0
			for _, c := range cycles {
				if c.at >= 120*m {
					held += c.donorPeak
				}
			}
			return held
		}
		want := heldAfter(stallLoop(simTimings(), map[string]int{"A": 1, "B": 1}, false, order.plain))
		cycles := stallLoop(simTimings(), map[string]int{"A": 1, "B": 1}, false, order.dipped)
		if held := heldAfter(cycles); held > want {
			t.Errorf("%s: donor-peak held the swap back %d cycles with dips in the donor's busy hour, %d without", order.name, held, want)
		}
		after, ok := stallReached(cycles, 120*m, order.final[0], order.final[1])
		if !ok || after > 45*m {
			t.Errorf("%s: the swap back reached %d:%d after %v (reached %v), want within 45m: the donor was held on its old peak",
				order.name, order.final[0], order.final[1], after, ok)
		}
	}
}

// A load that moves for good is not planned on its mean. Two models swap
// 1:3 for 3:1, back, and again within a swing window: the third swap is each
// role's third change of direction, and the swing rule plans both on their
// mean need. On their means the split is 2:2, and the busy model sat there
// past its threshold -- a need of 3 on 2 GPUs -- for 85 minutes. Its need
// has stayed well above its mean, and the other's well below, for longer than
// a move takes and longer than any half of the swaps before it (30 minutes):
// a step, so each is planned on its current need again. Both orders. (Before
// a swing took three changes of direction, a single swap back reached the
// rule, and this scenario had one swap fewer.)
func TestShareStepAfterASwapIsNotPlannedOnTheMean(t *testing.T) {
	const m = time.Minute
	tm, _ := DeriveShareTimings(ShareTimingInputs{Cycle: simCycle}, nil)
	phases := []stallPhase{{240 * m, stallNeeds(2, 2)}, {30 * m, stallNeeds(1, 3)}, {30 * m, stallNeeds(3, 1)},
		{30 * m, stallNeeds(1, 3)}, {180 * m, stallNeeds(3, 1)}}
	const step = 330 * time.Minute
	for _, order := range []struct {
		name   string
		phases []stallPhase
		final  [2]int
	}{
		{"A then B", phases, [2]int{3, 1}},
		{"B then A", stallSwap(phases), [2]int{1, 3}},
	} {
		cycles := stallLoop(tm, map[string]int{"A": 2, "B": 2}, true, order.phases)
		swung := false
		for _, c := range cycles {
			if c.at >= step && len(c.swinging) > 0 {
				swung = true
			}
		}
		if !swung {
			t.Fatalf("%s, setup: the step never started under the swing rule; the scenario no longer reaches it", order.name)
		}
		after, ok := stallReached(cycles, step, order.final[0], order.final[1])
		if !ok || after > 45*m {
			t.Errorf("%s: the step reached %d:%d after %v (reached %v), want within 45m: it was planned on its mean",
				order.name, order.final[0], order.final[1], after, ok)
			continue
		}
		t.Logf("%s: step reached %d:%d after %v", order.name, order.final[0], order.final[1], after)
	}
}

// A load that keeps swinging is still planned on its mean. The two models
// swap every 8 minutes, faster than a move lands (about 13 minutes): the need
// never stays on one side of its mean for a move's latency, so the swing rule
// holds once it starts, and the transfers stop chasing each swap. Most
// transfers it starts are called off within the release window, before
// anything moves, and each cancel counts as a change of direction: counting
// only the moves that landed, the rule never started, and a transfer started
// and was cancelled at every swap. Control: with the swing window at zero --
// the rule off -- the same load starts more transfers. With 1-GPU replicas
// and with whole 8-GPU nodes.
func TestShareFastSwapsStayPlannedOnTheMean(t *testing.T) {
	const m = time.Minute
	tm, _ := DeriveShareTimings(ShareTimingInputs{Cycle: simCycle}, nil)
	for _, g := range []int{1, 8} {
		x := float64(g)
		phases := []stallPhase{{240 * m, stallNeeds(2*x, 2*x)}}
		for i := range 30 {
			if i%2 == 0 {
				phases = append(phases, stallPhase{8 * m, stallNeeds(x, 3*x)})
			} else {
				phases = append(phases, stallPhase{8 * m, stallNeeds(3*x, x)})
			}
		}
		run := func(tm ShareTimings) (swinging, cycles, started int) {
			for _, c := range stallLoopSized(tm, g, map[string]int{"A": 2 * g, "B": 2 * g}, true, phases, nil) {
				if c.at < 300*m {
					continue
				}
				cycles++
				if len(c.swinging) > 0 {
					swinging++
				}
				for _, r := range c.swinging {
					if _, ok := c.swingNeeds[r]; !ok {
						t.Fatalf("%d-GPU replicas: at %v %s was planned on its mean with no SwingNeeds entry", g, c.at, r)
					}
				}
				started += c.started
			}
			return swinging, cycles, started
		}
		swinging, cycles, started := run(tm)
		if swinging*3 < cycles {
			t.Errorf("%d-GPU replicas: swaps every 8 minutes were planned on the mean in %d of %d cycles, want at least a third",
				g, swinging, cycles)
		}
		off := tm
		off.SwingWindow = 0
		_, _, startedOff := run(off)
		if started*2 > startedOff {
			t.Errorf("%d-GPU replicas: the swing rule started %d transfers, without it %d; want at most half", g, started, startedOff)
		}
		t.Logf("%d-GPU replicas: swinging %d of %d cycles; %d transfers started, %d without the rule",
			g, swinging, cycles, started, startedOff)
	}
}

// stallEvery is one phase per cycle over total, the needs f gives at each.
func stallEvery(total time.Duration, f func(t time.Duration) (a, b float64)) []stallPhase {
	var out []stallPhase
	for t := time.Duration(0); t < total; t += simCycle {
		a, b := f(t)
		out = append(out, stallPhase{simCycle, stallNeeds(a, b)})
	}
	return out
}

// A load that swings slower than a move lands is still planned on its mean.
// The two models swap every 18 minutes, longer than a move takes (about 13):
// each half stays on one side of its mean for longer than a move, but no
// longer than the halves before it, so it is the swing, not a step. Counted
// as a step at the end of every half, it was chased: a move decided 13
// minutes into a half landed in the next one, and the loop started nearly
// three times the transfers. Control: with the swing window at zero -- the
// rule off -- the same load starts more transfers.
func TestShareSwingSlowerThanAMoveStaysPlannedOnTheMean(t *testing.T) {
	const m = time.Minute
	tm, _ := DeriveShareTimings(ShareTimingInputs{Cycle: simCycle}, nil)
	const half = 18 * m
	if tm.MoveLatency >= half {
		t.Fatalf("setup: a move takes %v, not less than the %v half period", tm.MoveLatency, half)
	}
	warm := 240 * m
	phases := stallEvery(warm+8*time.Hour, func(at time.Duration) (float64, float64) {
		switch {
		case at < warm:
			return 2, 2
		case int((at-warm)/half)%2 == 0:
			return 1, 3
		}
		return 3, 1
	})
	run := func(tm ShareTimings) (swinging, cycles, started int) {
		for _, c := range stallLoop(tm, map[string]int{"A": 2, "B": 2}, true, phases) {
			if c.at < warm {
				continue
			}
			cycles++
			if len(c.swinging) > 0 {
				swinging++
			}
			started += c.started
		}
		return swinging, cycles, started
	}
	swinging, cycles, started := run(tm)
	off := tm
	off.SwingWindow = 0
	_, _, startedOff := run(off)
	if swinging*2 < cycles {
		t.Errorf("swaps every 18 minutes were planned on the mean in %d of %d cycles, want at least half", swinging, cycles)
	}
	if started*2 > startedOff {
		t.Errorf("the swing rule started %d transfers, without it %d; want at most half: the swing was chased as a step", started, startedOff)
	}
	t.Logf("swinging %d of %d cycles; %d transfers started, %d without the rule", swinging, cycles, started, startedOff)
}

// A donor whose peak comes back keeps it. B peaks for 10 minutes every 30
// and A sits at its need: the peaks fit the quota, so the spare is spread,
// and A, at its need, takes no replica that leaves B below its peak
// (donor-peak). Judged on its peak over its scale-down window alone, B gave
// its spare to A between peaks and waited a release at every one.
func TestShareRecurringDonorPeakIsKept(t *testing.T) {
	const m = time.Minute
	tm, _ := DeriveShareTimings(ShareTimingInputs{Cycle: simCycle}, nil)
	warm := 240 * m
	phases := stallEvery(warm+8*time.Hour, func(at time.Duration) (float64, float64) {
		if at%(30*m) < 10*m {
			return 1, 2.6
		}
		return 1, 0.9
	})
	held, started, short := 0, 0, 0
	for i, c := range stallLoop(tm, map[string]int{"A": 2, "B": 2}, true, phases) {
		if c.at < warm {
			continue
		}
		held += c.donorPeak
		started += c.started
		if float64(c.held["B"]) < math.Ceil(phases[i].need["B"]) {
			short++
		}
	}
	if held == 0 {
		t.Fatal("setup: donor-peak never held B; the scenario no longer reaches the rule")
	}
	if started != 0 || short != 0 {
		t.Errorf("B, peaking every 30 minutes, gave its spare away: %d transfers started, B short in %d cycles", started, short)
	}
	t.Logf("donor-peak held %d cycles", held)
}

// A single swap back is not a swing (issue #269). Two models swap a load of
// 2.9 GPUs for 0.17 after 25 minutes, then 0.7 for 2.9 for 20, then swap
// back for good, as the real-GPU run did; a short model reads at most 1.5
// GPUs per GPU it holds, its demand measured from what it serves. Counting
// the move that first raised it, the swap back is each model's second change
// of direction within the flapping window. When two marked a swing, both
// were planned on their mean need: the first replica moved (1:3 to 2:2) and
// the second waited for the busy model's mean to catch up -- 2:2 for eleven
// minutes, 3:1 after 25 (the run: 28), against 9 with the rule off. A swing
// now takes three changes of direction, so the swap back is planned on the
// current need and reaches 3:1 as soon as with the rule off. With 1-GPU
// replicas and with whole 8-GPU nodes, both orders.
func TestShareSingleSwapBackIsNotHeldOnTheMean(t *testing.T) {
	const m = time.Minute
	tm, _ := DeriveShareTimings(ShareTimingInputs{Cycle: simCycle, ReplicaStart: 240 * time.Second}, nil)
	const back = singleSwapBackAt
	for _, g := range []int{1, 8} {
		phases, served := singleSwapBack(g)
		initial := map[string]int{"A": g, "B": g}
		for _, order := range []struct {
			name   string
			phases []stallPhase
			busy   string
			split  func(busy, idle int) (int, int)
		}{
			{"A then B", phases, "A", func(b, i int) (int, int) { return b * g, i * g }},
			{"B then A", stallSwap(phases), "B", func(b, i int) (int, int) { return i * g, b * g }},
		} {
			name := fmt.Sprintf("%d-GPU replicas, %s", g, order.name)
			cycles := stallLoopSized(tm, g, initial, true, order.phases, served)
			a1, b1 := order.split(1, 3)
			if x, y := stallHeldAt(cycles, back); x != a1 || y != b1 {
				t.Fatalf("%s, setup: the first swap left %d:%d, want %d:%d", name, x, y, a1, b1)
			}
			a3, b3 := order.split(3, 1)
			off := tm
			off.SwingWindow = 0
			free, ok := stallReached(stallLoopSized(off, g, initial, true, order.phases, served), back, a3, b3)
			if !ok || free > 12*m {
				t.Fatalf("%s, control: with the swing rule off the swap back reached %d:%d after %v (reached %v), want within 12m",
					name, a3, b3, free, ok)
			}
			done, ok := stallReached(cycles, back, a3, b3)
			if !ok || done > free {
				t.Errorf("%s: the swap back reached %d:%d after %v (reached %v), want no later than with the rule off (%v)",
					name, a3, b3, done, ok, free)
			}
			for _, c := range cycles {
				if c.at >= back && slices.Contains(c.swinging, order.busy) {
					t.Errorf("%s: at %v the busy role was planned on its mean after a single swap back (%+v)",
						name, c.at, c.swingNeeds[order.busy])
					break
				}
			}
			t.Logf("%s: swap back reached %d:%d after %v; rule off: %v", name, a3, b3, done, free)
		}
	}
}

// singleSwapBackAt is when the scenario of singleSwapBack swaps back for good.
const singleSwapBackAt = 50 * time.Minute

// singleSwapBack is the swap and swap back of issue #269 with g-GPU replicas
// (TestShareSingleSwapBackIsNotHeldOnTheMean), and the demand a short model
// is measured at: at most 1.5 GPUs per GPU it holds.
func singleSwapBack(g int) ([]stallPhase, func(need float64, held int) float64) {
	const m = time.Minute
	x := float64(g)
	phases := []stallPhase{{5 * m, stallNeeds(0.1*x, 0.1*x)}, {25 * m, stallNeeds(2.9*x, 0.17*x)},
		{20 * m, stallNeeds(0.7*x, 2.9*x)}, {60 * m, stallNeeds(2.9*x, 0.65*x)}}
	served := func(need float64, held int) float64 { return math.Min(need, 1.5*float64(held)) }
	return phases, served
}

// stabilization.swingReversals sets how many changes of direction a swing
// takes. At 2 -- the rule before the default became three -- the single swap
// back of issue #269 is a swing again: the busy model is planned on its mean
// after it, and reaches 3:1 later than with the rule off. At 3, set
// explicitly, it is not held, as with the default. Both orders, 1-GPU
// replicas and whole 8-GPU nodes.
func TestShareSwingReversalsSetsWhatASwingTakes(t *testing.T) {
	derived, _ := DeriveShareTimings(ShareTimingInputs{Cycle: simCycle, ReplicaStart: 240 * time.Second}, nil)
	const back = singleSwapBackAt
	for _, g := range []int{1, 8} {
		phases, served := singleSwapBack(g)
		initial := map[string]int{"A": g, "B": g}
		for _, order := range []struct {
			name   string
			phases []stallPhase
			busy   string
			final  [2]int
		}{
			{"A then B", phases, "A", [2]int{3 * g, g}},
			{"B then A", stallSwap(phases), "B", [2]int{g, 3 * g}},
		} {
			off := derived
			off.SwingWindow = 0
			free, ok := stallReached(stallLoopSized(off, g, initial, true, order.phases, served), back, order.final[0], order.final[1])
			if !ok {
				t.Fatalf("%d-GPU replicas, %s, control: with the swing rule off the swap back never reached %v", g, order.name, order.final)
			}
			for _, n := range []int{2, 3} {
				name := fmt.Sprintf("%d-GPU replicas, %s, swingReversals %d", g, order.name, n)
				tm := derived
				tm.SwingReversals = n
				cycles := stallLoopSized(tm, g, initial, true, order.phases, served)
				held := 0
				for _, c := range cycles {
					if c.at >= back && slices.Contains(c.swinging, order.busy) {
						held++
					}
				}
				done, reached := stallReached(cycles, back, order.final[0], order.final[1])
				switch {
				case n == 2 && held == 0:
					t.Errorf("%s: the busy role was never planned on its mean after the swap back; two reversals must make a swing", name)
				case n == 2 && reached && done <= free:
					t.Errorf("%s: the swap back reached %v after %v, no later than with the rule off (%v); it was not held",
						name, order.final, done, free)
				case n == 3 && held > 0:
					t.Errorf("%s: the busy role was planned on its mean for %d cycles after a single swap back", name, held)
				case n == 3 && (!reached || done > free):
					t.Errorf("%s: the swap back reached %v after %v (reached %v), want no later than with the rule off (%v)",
						name, order.final, done, reached, free)
				}
				t.Logf("%s: held on the mean %d cycles; reached %v after %v (reached %v); rule off: %v",
					name, held, order.final, done, reached, free)
			}
		}
	}
}
