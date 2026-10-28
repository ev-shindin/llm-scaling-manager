package allocation

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"
)

// The planner is written for any number of roles in a group; the §6.7
// scenarios above run three. These run five to sixteen models on the same
// simulated cluster (runShareSim): one GPU per replica, a floor of one, and a
// budget equal to what the models hold, since the simulator has no idle fill.

// manyModels is n models M00..: model i has weight weight(i), starts holding
// initial(i) GPUs and sees demand(i).
func manyModels(n int, weight func(i int) float64, initial func(i int) int,
	demand func(i int) func(time.Duration, *rand.Rand) float64) []simRole {
	out := make([]simRole, 0, n)
	for i := range n {
		out = append(out, simRole{key: simModelKey(i), weight: weight(i), gpus: 1, floor: 1,
			initial: initial(i), demand: demand(i)})
	}
	return out
}

// simModelKey is model i's role key.
func simModelKey(i int) string { return fmt.Sprintf("M%02d", i) }

// equalWeight weighs every model 1.
func equalWeight(int) float64 { return 1 }

// simBudget is what roles hold at the start: the group's whole budget.
func simBudget(roles []simRole) int {
	b := 0
	for _, r := range roles {
		b += r.initial
	}
	return b
}

// shareInvariants checks, every simulated cycle, what must hold however many
// roles a group plans: the committed allocation stays within the budget and
// at or above every floor; no role gives and receives at once; and paced
// moves keep to the pace and the concurrency limit and never reverse a move
// inside the reversal hold. A move that skipped the waits (a hard imbalance,
// unpaced) passes all three by design and is not counted.
type shareInvariants struct {
	t      *testing.T
	label  string
	floors map[string]int
	// strict counts every transfer against the concurrency limit: the run
	// skips no wait (ShareSkipWaits.Off), so none may pass it.
	strict bool
	// maxInFlight is the most transfers counted in flight in any cycle.
	maxInFlight int
	// trace is every start and every cycle's cancel count, in order, without
	// transfer IDs: what two runs of one seed must agree on.
	trace strings.Builder
	// firstStart and served are, per role, the cycle of its first move as a
	// receiver and the first cycle it served at least its target (want).
	firstStart, served map[string]time.Duration
	want               map[string]int
	// lastMove is each donor, receiver pair's last move not cancelled.
	lastMove map[[2]string]simMove
	errs     int
}

// newShareInvariants checks roles; strict when the run skips no wait.
func newShareInvariants(t *testing.T, label string, roles []simRole, strict bool) *shareInvariants {
	floors := map[string]int{}
	for _, r := range roles {
		floors[r.key] = r.floor
	}
	return &shareInvariants{t: t, label: label, floors: floors, strict: strict,
		firstStart: map[string]time.Duration{}, served: map[string]time.Duration{}, lastMove: map[[2]string]simMove{}}
}

// fail reports a broken invariant, at most five per run.
func (s *shareInvariants) fail(at time.Duration, format string, args ...any) {
	s.t.Helper()
	if s.errs++; s.errs <= 5 {
		s.t.Errorf("%s at %v: %s", s.label, at, fmt.Sprintf(format, args...))
	}
}

// observe checks one cycle; it is the run's simOptions.observe.
func (s *shareInvariants) observe(v simCycleView) {
	s.t.Helper()
	committed := v.ledger.Committed(v.held)
	sum := 0
	for k, c := range committed {
		sum += c
		if c < s.floors[k] {
			s.fail(v.at, "%s committed %d, below its floor %d", k, c, s.floors[k])
		}
	}
	if sum > v.in.Budget {
		s.fail(v.at, "committed %d over the budget %d: %v", sum, v.in.Budget, committed)
	}
	giving, receiving := map[string]bool{}, map[string]bool{}
	paced := 0
	for _, tr := range v.ledger.Transfers() {
		if tr.State == ShareReleasing && tr.Donor != "" {
			giving[tr.Donor] = true
		}
		if tr.Receiver != "" {
			receiving[tr.Receiver] = true
		}
		if tr.Donor != "" && !tr.Returned && (s.strict || !tr.SkippedWaits) {
			paced++
		}
	}
	for k := range giving {
		if receiving[k] {
			s.fail(v.at, "%s gives and receives at once", k)
		}
	}
	if paced > ShareMaxConcurrentTransfers {
		s.fail(v.at, "%d paced transfers in flight, over the limit of %d", paced, ShareMaxConcurrentTransfers)
	}
	if s.strict && v.ledger.InFlight() > ShareMaxConcurrentTransfers {
		s.fail(v.at, "%d transfers in flight, over the limit of %d", v.ledger.InFlight(), ShareMaxConcurrentTransfers)
	}
	s.maxInFlight = max(s.maxInFlight, v.ledger.InFlight())
	// A cancelled move never happened: it holds nothing (ShareLedger.unrecord).
	for pair, m := range s.lastMove {
		if slices.Contains(v.plan.Cancelled, m.id) {
			delete(s.lastMove, pair)
		}
	}
	moved := map[string]int{}
	for _, tr := range v.plan.Started {
		fmt.Fprintf(&s.trace, "%v start %s->%s %d\n", v.at, tr.Donor, tr.Receiver, tr.GPUs)
		last, seen := s.lastMove[[2]string{tr.Receiver, tr.Donor}]
		if tr.Receiver != "" && tr.Donor != "" {
			s.lastMove[[2]string{tr.Donor, tr.Receiver}] = simMove{v.at, tr.ID}
		}
		if tr.SkippedWaits && !s.strict {
			continue
		}
		moved[tr.Donor]++
		moved[tr.Receiver]++
		// Rule 4: GPUs do not go back the way they came within the
		// reversal hold, unless the move skipped the waits.
		if seen && v.at-last.at < simTimings().ReversalHold {
			s.fail(v.at, "%s->%s reverses a move of %v ago, inside the reversal hold", tr.Donor, tr.Receiver, v.at-last.at)
		}
	}
	for _, tr := range v.plan.Started {
		if _, ok := s.firstStart[tr.Receiver]; !ok && tr.Receiver != "" {
			s.firstStart[tr.Receiver] = v.at
		}
	}
	for k, n := range moved {
		if k != "" && n > ShareMaxReplicasPerCycle {
			s.fail(v.at, "%s moved %d paced replicas in one cycle, over the pace of %d", k, n, ShareMaxReplicasPerCycle)
		}
	}
	fmt.Fprintf(&s.trace, "%v cancelled %d\n", v.at, len(v.plan.Cancelled))
	for k, w := range s.want {
		if _, ok := s.served[k]; !ok && v.serving[k] >= w {
			s.served[k] = v.at
		}
	}
}

// starts is the trace's starts, for a failure message.
func (s *shareInvariants) starts() string {
	var out []string
	for line := range strings.SplitSeq(s.trace.String(), "\n") {
		if strings.Contains(line, " start ") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// lastServed is the latest cycle a role of want first served its target, and
// whether every one did.
func (s *shareInvariants) lastServed() (time.Duration, bool) {
	var last time.Duration
	for k := range s.want {
		at, ok := s.served[k]
		if !ok {
			return 0, false
		}
		last = max(last, at)
	}
	return last, true
}

// stillShortfall is runShareSim's shortfall for a fleet that never moves: every
// role serves what it starts with. The demand draws the same random sequence
// runShareSim does, so the two compare seed for seed.
func stillShortfall(roles []simRole, seed int64, dur time.Duration) float64 {
	rnd := rand.New(rand.NewSource(seed))
	var short, total float64
	for t := time.Duration(0); t < dur; t += simCycle {
		for _, r := range roles {
			inst := r.demand(t, rnd) / simK
			short += math.Max(0, inst-float64(r.initial))
			total += inst
		}
	}
	return short / total
}

// shareRun runs roles three times on one seed, checking the invariants every
// cycle, and fails unless the three runs plan the same moves: the planner
// ranges over maps, and its result must not depend on their order.
func shareRun(t *testing.T, label string, roles []simRole, budget int, seed int64, tm ShareTimings,
	opt simOptions, want map[string]int) (simResult, *shareInvariants) {
	t.Helper()
	var first simResult
	var inv *shareInvariants
	for run := range 3 {
		s := newShareInvariants(t, label, roles, opt.skipWaits.Off)
		s.want = want
		o := opt
		o.observe = s.observe
		r := runShareSimWith(roles, budget, seed, tm, o)
		if run == 0 {
			first, inv = r, s
			continue
		}
		if s.trace.String() != inv.trace.String() {
			t.Fatalf("%s: run %d planned differently from run 0 on one seed", label, run)
		}
		if fmt.Sprint(r.final) != fmt.Sprint(first.final) {
			t.Fatalf("%s: run %d ended at %v, run 0 at %v", label, run, r.final, first.final)
		}
	}
	return first, inv
}

// manyModelCounts are the group sizes every scenario runs at.
var manyModelCounts = []int{5, 8, 16}

// Steady, noisy load moves nothing, whatever the number of models. Uniform:
// each model needs 2/0.8 = 2.5 GPUs and holds 3, its whole-replica target.
// Mixed: the §6.7 steady scenario's three models (weights 2, 1, 1; needs 5,
// 3.75 and 1.25 GPUs; holding 9, 5 and 2) repeated -- off their whole-replica
// targets but within the band, so only the band keeps them still.
func TestShareManyModelsSteadyLoadMovesNothing(t *testing.T) {
	levels, holds, weights := []float64{4, 3, 1}, []int{9, 5, 2}, []float64{2, 1, 1}
	for _, tc := range []struct {
		name  string
		roles func(n int) []simRole
	}{
		{"uniform", func(n int) []simRole {
			return manyModels(n, equalWeight, func(int) int { return 3 },
				func(int) func(time.Duration, *rand.Rand) float64 { return steady(2) })
		}},
		{"mixed", func(n int) []simRole {
			return manyModels(n, func(i int) float64 { return weights[i%3] }, func(i int) int { return holds[i%3] },
				func(i int) func(time.Duration, *rand.Rand) float64 { return steady(levels[i%3]) })
		}},
	} {
		for _, n := range manyModelCounts {
			t.Run(fmt.Sprintf("%s N=%d", tc.name, n), func(t *testing.T) {
				roles := tc.roles(n)
				for _, seed := range []int64{1, 2, 3} {
					r, _ := shareRun(t, fmt.Sprintf("%s seed %d", t.Name(), seed), roles, simBudget(roles), seed, simTimings(),
						simOptions{dur: 2 * time.Hour}, nil)
					if r.started != 0 {
						t.Errorf("seed %d: %d transfers under steady load, want none; final %v", seed, r.started, r.final)
					}
				}
			})
		}
	}
}

// swingPairs is n models in pairs whose demand swings ±40 % around 3.5
// GPU-equivalents in antiphase (swing), holding 6 and 5; an odd model out is
// steady at 1, holding 1: the §6.7 swap scenario, repeated.
func swingPairs(n int, period time.Duration) []simRole {
	odd := func(i int) bool { return i == n-1 && n%2 == 1 }
	return manyModels(n, equalWeight,
		func(i int) int {
			switch {
			case odd(i):
				return 1
			case i%2 == 0:
				return 6
			}
			return 5
		},
		func(i int) func(time.Duration, *rand.Rand) float64 {
			if odd(i) {
				return steady(1)
			}
			return swing(period, float64(i%2)*math.Pi)
		})
}

// Fast antiphase swings across many pairs waste no move, and keep the
// shortfall within the 1.7x of a still fleet the three-model resonance is
// held to. Measured: 1.2x to 1.35x for N=5, 8 and 16. Without the reversal
// hold the invariants catch moves reversed inside it, and a role giving and
// receiving at once.
func TestShareManyModelsFastSwingsWasteNothing(t *testing.T) {
	const dur = 3 * time.Hour
	for _, n := range manyModelCounts {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			roles := swingPairs(n, 15*time.Minute)
			var with, still float64
			for _, seed := range []int64{1, 2, 3} {
				label := fmt.Sprintf("N=%d seed %d", n, seed)
				r, _ := shareRun(t, label, roles, simBudget(roles), seed, simTimings(), simOptions{dur: dur}, nil)
				if r.wasted != 0 {
					t.Errorf("%s: %d moves wasted, want none", label, r.wasted)
				}
				with += r.shortfall
				still += stillShortfall(roles, seed, dur)
			}
			t.Logf("N=%d: shortfall %.4f, still %.4f (%.2fx)", n, with/3, still/3, with/still)
			if with >= 1.7*still {
				t.Errorf("shortfall %.4f, over 1.7x a still fleet's %.4f", with/3, still/3)
			}
		})
	}
}

// oneDonor is one model, M00, whose demand drops at an hour while every other
// model's triples: M00 holds 2 GPUs for each other model, which hold 2 each.
func oneDonor(n int) []simRole {
	return manyModels(n, equalWeight,
		func(i int) int {
			if i == 0 {
				return 2*(n-1) + 2
			}
			return 2
		},
		func(i int) func(time.Duration, *rand.Rand) float64 {
			if i == 0 {
				return stepAt(float64(2*(n-1)), 1, time.Hour)
			}
			return stepAt(1, 3, time.Hour)
		})
}

// pacedBound is the latest a demand step at stepAt is served when moves
// keep to the concurrency limit: the step is seen 2·LAG late and confirmed
// over ShareConfirmCycles; then moves go ShareMaxConcurrentTransfers at a time,
// each holding its slot from its start until the cycle after its donor
// releases, when the receiver is scheduled and the ledger sees it done; the
// last receiver serves FILL after that. One cycle of slack per stage covers
// sampling on the 30 s grid.
func pacedBound(stepAt time.Duration, moves int) time.Duration {
	waves := (moves + ShareMaxConcurrentTransfers - 1) / ShareMaxConcurrentTransfers
	slot := simRelease + 2*simCycle
	return stepAt + 2*simLag + (ShareConfirmCycles+1)*simCycle + time.Duration(waves)*slot + simFill + simCycle
}

// stepCase is a demand step at an hour on n models: who gives, who receives,
// what each ends at, and how many replicas move.
type stepCase struct {
	roles     []simRole
	receivers map[string]int // receiver -> the GPUs it ends at and serves
	final     map[string]int
	moves     int
}

// runStep runs c paced (every move waits: the concurrency limit binds) and
// unpaced (the default hard-imbalance band), and checks both: every receiver
// serves its target, no move is cancelled or wasted, exactly c.moves start,
// and the last receiver is served within pacedBound. The unpaced run, which
// may pass the limit for a hard imbalance, is never slower than the paced one.
func runStep(t *testing.T, c stepCase) {
	t.Helper()
	var pacedLast time.Duration
	for _, paced := range []bool{true, false} {
		label := fmt.Sprintf("%s paced=%v", t.Name(), paced)
		r, inv := shareRun(t, label, c.roles, simBudget(c.roles), 1, simTimings(),
			simOptions{dur: 6 * time.Hour, skipWaits: ShareSkipWaits{Off: paced}}, c.receivers)
		if fmt.Sprint(r.final) != fmt.Sprint(c.final) {
			t.Errorf("%s: final %v, want %v", label, r.final, c.final)
		}
		if r.cancelled != 0 || r.wasted != 0 {
			t.Errorf("%s: %d cancelled, %d wasted: want none", label, r.cancelled, r.wasted)
		}
		if r.started != c.moves {
			t.Errorf("%s: %d transfers, want %d: one per replica moved\n%s", label, r.started, c.moves, inv.starts())
		}
		last, ok := inv.lastServed()
		if !ok {
			t.Fatalf("%s: a receiver never served its target: served at %v", label, inv.served)
		}
		bound := pacedBound(time.Hour, c.moves)
		t.Logf("%s: %d moves, at most %d in flight; last receiver served %v after the step, bound %v",
			label, r.started, inv.maxInFlight, last-time.Hour, bound-time.Hour)
		if last > bound {
			t.Errorf("%s: the last receiver served %v after the step, over the bound of %v",
				label, last-time.Hour, bound-time.Hour)
		}
		if paced {
			pacedLast = last
		} else if last > pacedLast {
			t.Errorf("unpaced, the last receiver served at %v, later than paced (%v)", last, pacedLast)
		}
	}
}

// One donor funds every other model: n-1 receivers queue for its replicas,
// two at a time when paced. Measured, paced, the last is served 30.5, 50 and
// 102 minutes after the step for N=5, 8 and 16, against bounds of 35, 56 and
// 112; unpaced, 23 minutes at every N.
func TestShareManyModelsOneDonorManyReceivers(t *testing.T) {
	for _, n := range manyModelCounts {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			c := stepCase{roles: oneDonor(n), receivers: map[string]int{},
				final: map[string]int{simModelKey(0): 2}, moves: 2 * (n - 1)}
			for i := 1; i < n; i++ {
				c.receivers[simModelKey(i)] = 4 // 3/0.8 = 3.75 GPUs
				c.final[simModelKey(i)] = 4
			}
			runStep(t, c)
		})
	}
}

// manyDonors is the converse of oneDonor: M00 holds 2 and its demand rises
// at an hour to 1.48n, a need of 1.85n GPUs, just under the 2n it can hold
// once every other model -- holding 4, dropping to 1 -- is down to 2.
func manyDonors(n int) []simRole {
	return manyModels(n, equalWeight,
		func(i int) int {
			if i == 0 {
				return 2
			}
			return 4
		},
		func(i int) func(time.Duration, *rand.Rand) float64 {
			if i == 0 {
				return stepAt(1, 1.48*float64(n), time.Hour)
			}
			return stepAt(3, 1, time.Hour)
		})
}

// Every other model funds one: the receiver takes a replica from each donor
// in turn, two transfers at a time when paced.
func TestShareManyModelsManyDonorsOneReceiver(t *testing.T) {
	for _, n := range manyModelCounts {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			// Each donor keeps 2 (1/0.8 = 1.25 GPUs, whole); M00 takes the
			// rest, which covers its need however the 5 % noise reads it.
			c := stepCase{roles: manyDonors(n), receivers: map[string]int{simModelKey(0): 2 + 2*(n-1)},
				final: map[string]int{simModelKey(0): 2 + 2*(n-1)}, moves: 2 * (n - 1)}
			for i := 1; i < n; i++ {
				c.final[simModelKey(i)] = 2
			}
			runStep(t, c)
		})
	}
}

// rotating is n models bursting in turn, each for its own 30-minute slot of
// an n-slot period, from 1 to 5 GPU-equivalents. M00 starts with 8 GPUs, the
// rest with 2: GPUs must follow the burst from model to model.
func rotating(n int) []simRole {
	const slot = 30 * time.Minute
	period := slot * time.Duration(n)
	return manyModels(n, equalWeight,
		func(i int) int {
			if i == 0 {
				return 8
			}
			return 2
		},
		func(i int) func(time.Duration, *rand.Rand) float64 {
			return func(t time.Duration, rnd *rand.Rand) float64 {
				v := 1.0
				if ph := t % period; ph >= slot*time.Duration(i) && ph < slot*time.Duration(i+1) {
					v = 5
				}
				return v * (1 + rnd.Float64()*0.1 - 0.05)
			}
		})
}

// Bursts that rotate through every model leave less demand unserved than a
// fleet that never moves, with no move cancelled or wasted. Measured: 0.47 to
// 0.57 of the still fleet's shortfall for N=5, 8 and 16; the bound is 0.7.
func TestShareManyModelsRotatingBursts(t *testing.T) {
	const dur = 6 * time.Hour
	for _, n := range manyModelCounts {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			roles := rotating(n)
			for _, seed := range []int64{1, 2, 3} {
				label := fmt.Sprintf("N=%d seed %d", n, seed)
				r, _ := shareRun(t, label, roles, simBudget(roles), seed, simTimings(), simOptions{dur: dur}, nil)
				still := stillShortfall(roles, seed, dur)
				t.Logf("%s: %d moves, %d cancelled, %d wasted; shortfall %.4f, still %.4f (%.2fx)",
					label, r.started, r.cancelled, r.wasted, r.shortfall, still, r.shortfall/still)
				if r.started == 0 {
					t.Errorf("%s: nothing moved", label)
				}
				if r.cancelled != 0 || r.wasted != 0 {
					t.Errorf("%s: %d cancelled, %d wasted: want none", label, r.cancelled, r.wasted)
				}
				if r.shortfall >= 0.7*still {
					t.Errorf("%s: shortfall %.4f, not below 0.7 of a still fleet's %.4f", label, r.shortfall, still)
				}
			}
		})
	}
}

// A heavier model short of its need is funded first, and the lighter ones
// after it are funded too: weight orders the queue, it starves nobody. M00
// gives; M01 weighs 3 and every other model 1; all of them step up together.
// Paced, the concurrency limit makes them queue.
func TestShareManyModelsWeightOrdersReceivers(t *testing.T) {
	for _, n := range []int{5, 8} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			roles := oneDonor(n)
			roles[1].weight = 3
			receivers := map[string]int{}
			for i := 1; i < n; i++ {
				receivers[simModelKey(i)] = 4
			}
			r, inv := shareRun(t, t.Name(), roles, simBudget(roles), 1, simTimings(),
				simOptions{dur: 6 * time.Hour, skipWaits: ShareSkipWaits{Off: true}}, receivers)
			heavy := simModelKey(1)
			t.Logf("final %v; first moves %v; served %v", r.final, inv.firstStart, inv.served)
			for i := 2; i < n; i++ {
				k := simModelKey(i)
				if inv.firstStart[heavy] > inv.firstStart[k] {
					t.Errorf("%s (weight 3) first funded at %v, after %s (weight 1) at %v",
						heavy, inv.firstStart[heavy], k, inv.firstStart[k])
				}
				if inv.served[heavy] > inv.served[k] {
					t.Errorf("%s (weight 3) served at %v, after %s (weight 1) at %v", heavy, inv.served[heavy], k, inv.served[k])
				}
			}
			last, ok := inv.lastServed()
			if !ok {
				t.Fatalf("a light model starved: served at %v", inv.served)
			}
			if bound := pacedBound(time.Hour, r.started); last > bound {
				t.Errorf("the last model served %v after the step, over the bound of %v", last-time.Hour, bound-time.Hour)
			}
		})
	}
}

// pdModels is n P/D models, each a prefill and a decode role of one model
// (SharePlanInput.Origins). Model 0's two roles drop at an hour; of every
// other model, one role steps up -- decode for odd models, prefill for even
// ones -- and the other stays put.
func pdModels(n int) ([]simRole, map[string]ShareRoleOrigin, map[string]int) {
	var roles []simRole
	origins := map[string]ShareRoleOrigin{}
	final := map[string]int{}
	for i := range n {
		for _, role := range []string{"prefill", "decode"} {
			key := fmt.Sprintf("P%02d/%s", i, role)
			origins[key] = ShareRoleOrigin{Namespace: "ns", ModelID: fmt.Sprintf("P%02d", i), Role: role}
			r := simRole{key: key, weight: 1, gpus: 1, floor: 1, initial: 2, demand: steady(1.2)}
			final[key] = 2
			switch rising := (i%2 == 1) == (role == "decode"); {
			case i == 0 && role == "prefill":
				r.initial = 2*(n-1) + 2
				r.demand = stepAt(float64(2*(n-1)), 1, time.Hour)
			case rising:
				r.demand = stepAt(1, 3, time.Hour)
				final[key] = 4
			}
			roles = append(roles, r)
		}
	}
	return roles, origins, final
}

// P/D models move GPUs role by role: only the roles whose demand rose
// receive, only model 0's roles give, and the invariants hold every cycle
// with 2n roles in the group.
func TestShareManyModelsPrefillDecode(t *testing.T) {
	for _, n := range []int{3, 5} {
		t.Run(fmt.Sprintf("models=%d", n), func(t *testing.T) {
			roles, origins, final := pdModels(n)
			gave, got := map[string]bool{}, map[string]bool{}
			receivers := map[string]int{}
			for k, v := range final {
				if v == 4 {
					receivers[k] = v
				}
			}
			for _, paced := range []bool{true, false} {
				label := fmt.Sprintf("%s paced=%v", t.Name(), paced)
				inv := newShareInvariants(t, label, roles, paced)
				inv.want = receivers
				r := runShareSimWith(roles, simBudget(roles), 1, simTimings(), simOptions{
					dur: 6 * time.Hour, origins: origins, skipWaits: ShareSkipWaits{Off: paced},
					observe: func(v simCycleView) {
						inv.observe(v)
						for _, tr := range v.plan.Started {
							gave[tr.Donor], got[tr.Receiver] = true, true
						}
					}})
				if fmt.Sprint(r.final) != fmt.Sprint(final) {
					t.Errorf("%s: final %v, want %v", label, r.final, final)
				}
				if r.cancelled != 0 || r.wasted != 0 {
					t.Errorf("%s: %d cancelled, %d wasted: want none", label, r.cancelled, r.wasted)
				}
				if _, ok := inv.lastServed(); !ok {
					t.Errorf("%s: a rising role never served its target: %v", label, inv.served)
				}
				// Determinism over 2n roles and their origins.
				again := newShareInvariants(t, label, roles, paced)
				runShareSimWith(roles, simBudget(roles), 1, simTimings(), simOptions{
					dur: 6 * time.Hour, origins: origins, skipWaits: ShareSkipWaits{Off: paced}, observe: again.observe})
				if again.trace.String() != inv.trace.String() {
					t.Errorf("%s: a second run planned differently", label)
				}
			}
			for k := range gave {
				if !strings.HasPrefix(k, "P00/") {
					t.Errorf("%s gave GPUs; only model 0's roles should", k)
				}
			}
			for k := range got {
				if _, ok := receivers[k]; !ok {
					t.Errorf("%s received GPUs; only the roles whose demand rose should", k)
				}
			}
		})
	}
}

// Two whole-node receivers and four 1-GPU models on six full 8-GPU nodes,
// the quota committed in full:
//
//	n1  R1 x1 (8)   n2  R2 x1 (8)
//	n3  A x4, B x4  n4  C x4, D x4
//	n5  A x4, B x4  n6  A x4, B x4
//
// R1 and R2 each need a second 8-GPU replica; A to D need less than one GPU
// each and keep one. A 1-GPU pod funds no 8-GPU one, so each receiver needs a
// node emptied by a set of 1-GPU donor pods. A and B have pods to spare after
// emptying one node, so nothing but the node picture keeps the second set
// from being planned into the node the first emptied. Between cycles a
// planned pod is marked, and a marked pod is no longer offered
// (shareDonorUnits).
func TestShareManyModelsTwoWholeNodeReceivers(t *testing.T) {
	eight := ShareVariant{Name: "v", GPUs: 8, PodGPUs: []int{8}}
	one := ShareVariant{Name: "v", GPUs: 1, PodGPUs: []int{1}}
	nodeOf := map[string]string{}
	all := map[string][]ShareUnit{}
	place := func(role, node string, n int) {
		for range n {
			name := fmt.Sprintf("ns/%s-%d", role, len(all[role]))
			nodeOf[name] = node
			all[role] = append(all[role], ShareUnit{Pods: []SharePod{{Name: name, Node: node, GPUs: 1}}})
		}
	}
	for _, n := range []string{"n3", "n5", "n6"} {
		place("A", n, 4)
		place("B", n, 4)
	}
	place("C", "n4", 4)
	place("D", "n4", 4)
	roles := []ShareRole{
		{Key: "R1", Weight: 1, Need: 14, Floor: 8, Ceiling: 64, ReplicaGPUs: 8},
		{Key: "R2", Weight: 1, Need: 14, Floor: 8, Ceiling: 64, ReplicaGPUs: 8},
	}
	held := map[string]int{"R1": 8, "R2": 8}
	thresholds := map[string]float64{"R1": 0.8, "R2": 0.8}
	give := map[string]ShareVariant{}
	grow := map[string]ShareVariant{"R1": eight, "R2": eight}
	for _, r := range []string{"A", "B", "C", "D"} {
		roles = append(roles, ShareRole{Key: r, Weight: 1, Need: 0.8, Floor: 1, Ceiling: 64, ReplicaGPUs: 1})
		held[r], thresholds[r], give[r], grow[r] = len(all[r]), 0.8, one, one
	}
	nodes := map[string]ShareNode{"n1": {}, "n2": {}, "n3": {}, "n4": {}, "n5": {}, "n6": {}}
	var trace string
	for run := range 3 { // three runs: the result must not depend on map order
		units := map[string][]ShareUnit{}
		for r, u := range all {
			units[r] = slices.Clone(u)
		}
		in := SharePlanInput{Roles: roles, Held: held, Thresholds: thresholds, Budget: 48, Tolerance: 0.15,
			Give: give, Grow: grow, Nodes: nodes, PhysicalFree: 0}
		l := NewShareLedger()
		var started []ShareTransfer
		var b strings.Builder
		for i := range 6 {
			in.DonorUnits = units
			for _, tr := range PlanShareTransfers(l, in, time.Unix(int64(30*i), 0), simTimings()).Started {
				fmt.Fprintf(&b, "cycle %d: %s->%s %v\n", i, tr.Donor, tr.Receiver, tr.PlannedPods)
				started = append(started, tr)
				// The planned pods are marked: offered to no later set.
				units[tr.Donor] = slices.DeleteFunc(units[tr.Donor], func(u ShareUnit) bool {
					return slices.Contains(tr.PlannedPods, u.Pods[0].Name)
				})
			}
		}
		if run == 0 {
			trace = b.String()
			t.Logf("planned:\n%s", trace)
		} else if b.String() != trace {
			t.Fatalf("run %d planned differently from run 0:\n%s", run, b.String())
		}
		// Each receiver's set: its primary and the contributors linked to it.
		receiverOf := map[string]string{}
		for _, tr := range started {
			if tr.Receiver != "" {
				receiverOf[tr.ID] = tr.Receiver
			}
		}
		holes := map[string]map[string]int{} // receiver -> node -> GPUs its planned pods free
		gave := map[string]int{}
		for _, tr := range started {
			gave[tr.Donor] += tr.DonorGPUs
			rc := tr.Receiver
			if rc == "" {
				rc = receiverOf[tr.SetID]
			}
			if rc == "" {
				t.Fatalf("a transfer funds no receiver: %+v", tr)
			}
			if holes[rc] == nil {
				holes[rc] = map[string]int{}
			}
			for _, pod := range tr.PlannedPods {
				holes[rc][nodeOf[pod]]++
			}
		}
		var used []string
		for _, rc := range []string{"R1", "R2"} {
			full := ""
			for n, g := range holes[rc] {
				if g >= 8 {
					full = n
				}
			}
			if full == "" {
				t.Fatalf("%s has no node its 8-GPU pod fits: planned holes %v", rc, holes[rc])
			}
			used = append(used, full)
		}
		if used[0] == used[1] {
			t.Errorf("R1 and R2 were both planned into %s", used[0])
		}
		for r, g := range gave {
			if held[r]-g < 1 {
				t.Errorf("%s gave %d of %d, below its floor of 1", r, g, held[r])
			}
		}
		committed := 0
		for _, c := range l.Committed(held) {
			committed += c
		}
		if committed > in.Budget {
			t.Errorf("committed %d over the budget %d", committed, in.Budget)
		}
	}
}

// simMove is when a transfer started, and which.
type simMove struct {
	at time.Duration
	id string
}
