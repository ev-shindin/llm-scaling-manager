package allocation

import (
	"fmt"
	"maps"
	"math"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"
)

// shareInvariants checks, every simulated cycle, what must hold however many
// roles a group plans: the committed allocation stays within the budget and
// at or above every floor; no role gives and receives at once; and paced
// moves keep to the pace and the concurrency limit, and never take a role
// that gave within the reversal hold as a receiver, or one that received as
// a donor (rule 4). A move that skipped the waits (a hard imbalance, unpaced)
// passes all three by design and is not counted.
//
// It collects what it finds and reports it through report, from the test, so
// a failure names the test's line rather than the simulator's.
type shareInvariants struct {
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
	// moves is each role's moves not cancelled, as the ledger keeps them for
	// rule 4.
	moves map[string][]simMove
	// exempt is, per transfer started, whether it skipped the waits; open is
	// the transfers in flight at the last cycle; landed is every transfer
	// that ended done, in order.
	exempt map[string]bool
	open   map[string]ShareTransfer
	landed []simLanding
	errs   []string
}

// simMove is one move of a role: when it started, which transfer, and
// whether the role received.
type simMove struct {
	at       time.Duration
	id       string
	received bool
}

// simLanding is a transfer that ended done: when, its roles, and whether it
// skipped the waits.
type simLanding struct {
	at              time.Duration
	donor, receiver string
	exempt          bool
}

// newShareInvariants checks roles; strict when the run skips no wait.
func newShareInvariants(label string, roles []simRole, strict bool) *shareInvariants {
	floors := map[string]int{}
	for _, r := range roles {
		floors[r.key] = r.floor
	}
	return &shareInvariants{label: label, floors: floors, strict: strict,
		firstStart: map[string]time.Duration{}, served: map[string]time.Duration{},
		moves: map[string][]simMove{}, exempt: map[string]bool{}, open: map[string]ShareTransfer{}}
}

// fail records a broken invariant.
func (s *shareInvariants) fail(at time.Duration, format string, args ...any) {
	s.errs = append(s.errs, fmt.Sprintf("%s at %v: %s", s.label, at, fmt.Sprintf(format, args...)))
}

// report fails t with what the run broke, at most five.
func (s *shareInvariants) report(t *testing.T) {
	t.Helper()
	for i, e := range s.errs {
		if i == 5 {
			t.Errorf("%s: %d more", s.label, len(s.errs)-5)
			break
		}
		t.Error(e)
	}
}

// heldBy reports whether role's last move the other way -- having given, when
// it is to receive -- is within the reversal hold at now.
func (s *shareInvariants) heldBy(role string, receive bool, now time.Duration) (time.Duration, bool) {
	var last time.Duration
	seen := false
	for _, m := range s.moves[role] {
		if m.received != receive && (!seen || m.at > last) {
			last, seen = m.at, true
		}
	}
	return now - last, seen && now-last < simTimings().ReversalHold
}

// observe checks one cycle; it is the run's simOptions.observe.
func (s *shareInvariants) observe(v simCycleView) {
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
	now := map[string]ShareTransfer{}
	for _, tr := range v.ledger.Transfers() {
		now[tr.ID] = tr
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
	// A transfer gone from the ledger after it was filling landed.
	for _, id := range slices.Sorted(maps.Keys(s.open)) {
		if tr := s.open[id]; tr.State == ShareFilling && tr.Donor != "" && tr.Receiver != "" {
			if _, ok := now[id]; !ok {
				s.landed = append(s.landed, simLanding{v.at, tr.Donor, tr.Receiver, s.exempt[id]})
			}
		}
	}
	s.open = now
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
	for role, ms := range s.moves {
		s.moves[role] = slices.DeleteFunc(ms, func(m simMove) bool { return slices.Contains(v.plan.Cancelled, m.id) })
	}
	moved := map[string]int{}
	for _, tr := range v.plan.Started {
		fmt.Fprintf(&s.trace, "%v start %s->%s %d\n", v.at, tr.Donor, tr.Receiver, tr.GPUs)
		s.exempt[tr.ID] = tr.SkippedWaits
		if tr.Receiver != "" {
			if _, ok := s.firstStart[tr.Receiver]; !ok {
				s.firstStart[tr.Receiver] = v.at
			}
		}
		paced := !tr.SkippedWaits || s.strict
		if paced {
			moved[tr.Donor]++
			moved[tr.Receiver]++
			// Rule 4: a role that gave does not receive within the reversal
			// hold, and one that received does not give.
			if ago, held := s.heldBy(tr.Receiver, true, v.at); tr.Receiver != "" && held {
				s.fail(v.at, "%s->%s: %s gave %v ago, inside the reversal hold", tr.Donor, tr.Receiver, tr.Receiver, ago)
			}
			if ago, held := s.heldBy(tr.Donor, false, v.at); tr.Donor != "" && held {
				s.fail(v.at, "%s->%s: %s received %v ago, inside the reversal hold", tr.Donor, tr.Receiver, tr.Donor, ago)
			}
		}
		// What the ledger records (ShareLedger.Start): every move not entitled
		// holds its donor -- a donor-set contributor too -- and its receiver.
		if !tr.Entitled {
			if tr.Donor != "" {
				s.moves[tr.Donor] = append(s.moves[tr.Donor], simMove{v.at, tr.ID, false})
			}
			if tr.Receiver != "" {
				s.moves[tr.Receiver] = append(s.moves[tr.Receiver], simMove{v.at, tr.ID, true})
			}
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

// wasted is runShareSim's wasted moves, as pairs of landings: a transfer and
// the one that, landing within RELEASE after it, sends GPUs back into its
// donor or takes them out of its receiver.
func (s *shareInvariants) wasted() [][2]simLanding {
	var out [][2]simLanding
	for i, a := range s.landed {
		for _, b := range s.landed[i+1:] {
			if b.at-a.at <= simRelease && (b.receiver == a.donor || b.donor == a.receiver) {
				out = append(out, [2]simLanding{a, b})
				break
			}
		}
	}
	return out
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
		s := newShareInvariants(label, roles, opt.skipWaits.Off)
		s.want = want
		o := opt
		o.observe = s.observe
		r := runShareSimWith(roles, budget, seed, tm, o)
		if run == 0 {
			first, inv = r, s
			s.report(t)
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
