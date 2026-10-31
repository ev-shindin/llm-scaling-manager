package settle

import (
	"testing"
	"time"
)

// The settle suite (analyzer-learner.md, section 8): every case the four
// rules before the tracker were fixed for, at the tracker.

const cycle = 15 * time.Second

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

const model = "ns/m"

// busy is a decode replica Ready an hour before t0, serving.
func busy(pod string) Row {
	return Row{Key: Key{Pod: pod, Variant: "dec"}, Ready: true, ReadySince: t0.Add(-time.Hour),
		KV: 0.5, KVKnown: true, ITL: ITLFinite, GenerationRate: 900, GenerationRateKnown: true,
		RequestRate: 2, RequestRateKnown: true}
}

// idle is busy with nothing in the cache and an empty ITL window.
func idle(pod string) Row {
	r := busy(pod)
	r.KV, r.ITL, r.GenerationRate, r.RequestRate = 0, ITLEmpty, 0, 0
	return r
}

// blind is replica d with every gauge unknown: a failed scrape, or a
// Prometheus brownout with the Pod still listed.
func blind() Row {
	r := busy("d")
	r.KVKnown, r.ITL, r.GenerationRateKnown, r.RequestRateKnown = false, ITLAbsent, false, false
	return r
}

// run observes one row of pod per cycle from t0 (nil: the pod is missing
// that cycle) and returns the tracker and the instant of the last cycle.
func run(rows ...*Row) (*Tracker, time.Time) {
	tr := NewTracker()
	now := t0
	for i, r := range rows {
		now = t0.Add(time.Duration(i) * cycle)
		var obs []Row
		if r != nil {
			obs = []Row{*r}
		}
		tr.Observe(model, obs, now)
	}
	return tr, now
}

func ptr(r Row) *Row { return &r }

// repeat is n copies of r.
func repeat(r Row, n int) []*Row {
	out := make([]*Row, n)
	for i := range out {
		out[i] = ptr(r)
	}
	return out
}

func replicaOf(t *testing.T, tr *Tracker, pod string) Replica {
	t.Helper()
	r, ok := tr.Replica(model, Key{Pod: pod, Variant: "dec"})
	if !ok {
		t.Fatalf("the tracker does not know %s", pod)
	}
	return r
}

// The activity of a row, role by role (4.3's table).
func TestActivityReadsPositiveEvidenceOnly(t *testing.T) {
	prefill := func(edit func(*Row)) Row {
		r := idle("pre-0")
		r.Prefill = true
		edit(&r)
		return r
	}
	for _, tc := range []struct {
		name string
		row  Row
		want Activity
	}{
		{"decode serving", busy("d"), Load},
		{"decode idle: KV 0, ITL window empty", idle("d"), Idle},
		{"decode idle: KV 0, generation rate known 0", func() Row { r := idle("d"); r.ITL = ITLAbsent; r.GenerationRateKnown = true; return r }(), Idle},
		{"KV 0 with a finite ITL is a reading, not idle", func() Row { r := busy("d"); r.KV = 0; return r }(), Load},
		{"k > 0 with no ITL is busy with its ITL missing", func() Row { r := blind(); r.KV, r.KVKnown = 0.4, true; return r }(), Unknown},
		{"k == 0 because KV was not read (#273 A4)", func() Row { r := idle("d"); r.KVKnown = false; return r }(), Unknown},
		{"KV 0, ITL absent, no generation rate", func() Row { r := idle("d"); r.ITL = ITLAbsent; r.GenerationRateKnown = false; return r }(), Unknown},
		{"every gauge unknown: a failed scrape", blind(), Unknown},
		{"not Ready", func() Row { r := idle("d"); r.Ready = false; return r }(), Unknown},
		{"prefill under light load: KV 0, ITL empty, requests arriving", prefill(func(r *Row) { r.RequestRate = 0.2 }), Load},
		{"prefill idle: KV 0, request rate known 0", prefill(func(*Row) {}), Idle},
		{"prefill with no request rate", prefill(func(r *Row) { r.RequestRateKnown = false }), Unknown},
		{"prefill's empty ITL is not evidence", prefill(func(r *Row) { r.RequestRateKnown, r.GenerationRateKnown = false, true }), Unknown},
	} {
		if got := tc.row.Activity(); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// 2.3's negative control, #273 A4: a busy replica whose KV and ITL queries
// both fail for two cycles reads k == 0 with no ITL. The ITL trace counted
// those rows as idle, so the next good row restarted the replica's settle as
// load returned after idle. At the tracker they are unknown: no idle run, no
// return, the settle stands.
func TestUnknownGaugesDoNotUnsettleABusyReplica(t *testing.T) {
	k0 := idle("d")
	k0.KVKnown, k0.ITL, k0.GenerationRateKnown = false, ITLAbsent, false
	rows := append(repeat(busy("d"), 10), ptr(k0), ptr(k0))
	tr, _ := run(append(rows, repeat(busy("d"), 4)...)...)
	r := replicaOf(t, tr, "d")
	if !r.Since.Equal(t0) || r.Reason != Joined {
		t.Errorf("settle %v %s, want the join at %v", r.Since, r.Reason, t0)
	}
	if _, ok := r.ReturnedAfter(1); ok || len(r.Returns) != 0 {
		t.Errorf("returns %v: two rows of unknown KV and ITL read as an idle run", r.Returns)
	}
	if got := tr.Verdicts(); got[Joined] != 1 || got[Rejoined]+got[Restarted]+got[Lent]+got[RolledOut] != 0 {
		t.Errorf("verdicts %v, want one join", got)
	}
}

// 2.3's negative control, 273-r3 #2: a single missed scrape dropped the
// replica's trace, and it came back as a joiner. A sighting outlives an
// absence of up to Period; past it the replica rejoins.
func TestAMissedScrapeKeepsTheSettle(t *testing.T) {
	for _, tc := range []struct {
		missing int
		want    Reason
	}{{1, Joined}, {int(Period/cycle) - 1, Joined}, {int(Period / cycle), Rejoined}} {
		rows := repeat(busy("d"), 5)
		rows = append(rows, make([]*Row, tc.missing)...)
		tr, last := run(append(rows, ptr(busy("d")))...)
		r := replicaOf(t, tr, "d")
		if r.Reason != tc.want {
			t.Errorf("missing %d cycles: settle %s at %v, want %s", tc.missing, r.Reason, r.Since, tc.want)
		}
		if tc.want == Rejoined && !r.Since.Equal(last) {
			t.Errorf("missing %d cycles: rejoined at %v, want the return %v", tc.missing, r.Since, last)
		}
	}
}

// Section 8, the brownout trade, as PR 1 picks it: a Prometheus brownout
// longer than a minute with every Pod still listed keeps every settle, an
// idle-and-return inside it included. The rows are sightings; what the
// replica did under them was not read, so no idle run and no return is
// made of them. The cost side: a fill transient after it is admitted.
func TestABrownoutKeepsTheSettle(t *testing.T) {
	rows := repeat(busy("d"), 8)
	rows = append(rows, repeat(blind(), int(3*time.Minute/cycle))...)
	tr, _ := run(append(rows, repeat(busy("d"), 2)...)...)
	r := replicaOf(t, tr, "d")
	if !r.Since.Equal(t0) || r.Reason != Joined || len(r.Returns) != 0 {
		t.Errorf("after a 3-minute brownout: settle %s at %v, returns %v; want the join at %v and none",
			r.Reason, r.Since, r.Returns, t0)
	}

	// Idle seen before the brownout, load after it: the run ended at the
	// gap, so it is no return.
	rows = append(repeat(busy("d"), 2), repeat(idle("d"), 4)...)
	rows = append(rows, repeat(blind(), int(3*time.Minute/cycle))...)
	tr, _ = run(append(rows, ptr(busy("d")))...)
	if r := replicaOf(t, tr, "d"); len(r.Returns) != 0 {
		t.Errorf("idle before a 3-minute brownout, load after: returns %v, want none", r.Returns)
	}
}

// An idle run and the load that returns after it. The run's length is the
// reader's to judge (steadyGate.idleRows); a run continues across unknown
// rows, but not across a gap longer than Period between two idle rows.
func TestLoadReturnsAfterAnIdleRun(t *testing.T) {
	rows := append(repeat(busy("d"), 2), repeat(idle("d"), 5)...)
	tr, last := run(append(rows, ptr(busy("d")))...)
	r := replicaOf(t, tr, "d")
	if at, ok := r.ReturnedAfter(5); !ok || !at.Equal(last) {
		t.Errorf("after 5 idle rows: return %v %v, want one at %v", at, ok, last)
	}
	if _, ok := r.ReturnedAfter(6); ok {
		t.Error("a run of 5 idle rows read as one of 6")
	}

	// idleRows 5 at a 15 s cycle spans 75 s: the limit is per gap, not on
	// the run's span.
	if r.Returns[0].IdleRows != 5 || last.Sub(r.Returns[0].IdleFrom) != 5*cycle {
		t.Errorf("return %+v, want a run of 5 rows from %v", r.Returns[0], last.Add(-5*cycle))
	}

	// Unknown rows inside the run neither count nor break it.
	rows = append(repeat(busy("d"), 2), ptr(idle("d")), ptr(blind()), ptr(idle("d")), ptr(blind()), ptr(idle("d")))
	tr, _ = run(append(rows, ptr(busy("d")))...)
	if r := replicaOf(t, tr, "d"); len(r.Returns) != 1 || r.Returns[0].IdleRows != 3 {
		t.Errorf("idle, unknown, idle, unknown, idle, load: returns %v, want one after 3 idle rows", r.Returns)
	}

	// Two idle rows more than Period apart, only unknown rows between, are
	// two runs.
	rows = append(repeat(busy("d"), 2), ptr(idle("d")))
	rows = append(rows, repeat(blind(), int(Period/cycle))...)
	rows = append(rows, ptr(idle("d")))
	tr, _ = run(append(rows, ptr(busy("d")))...)
	if r := replicaOf(t, tr, "d"); len(r.Returns) != 1 || r.Returns[0].IdleRows != 1 {
		t.Errorf("idle rows %v apart: returns %v, want one after a run of 1", Period+cycle, r.Returns)
	}

	// Idle is a run of at least idleRows rows whose last is at most Period
	// old.
	tr, last = run(ptr(busy("d")), ptr(idle("d")), ptr(idle("d")))
	r = replicaOf(t, tr, "d")
	if !r.Idle(2, last) || r.Idle(3, last) || r.Idle(2, last.Add(Period+time.Second)) {
		t.Errorf("a run of 2 idle rows ending %v: Idle(2) %v, Idle(3) %v, Idle(2) past the period %v; want true, false, false",
			last, r.Idle(2, last), r.Idle(3, last), r.Idle(2, last.Add(Period+time.Second)))
	}
}

// A restart under the same name gives a new Ready time; a rollout a new
// revision; a warm-pool Pod lent again a new lend time. Each starts a settle,
// and resets the idle run before it.
func TestEventsUnderTheSameKeyStartASettle(t *testing.T) {
	restarted := busy("d")
	restarted.ReadySince = t0.Add(4 * cycle)
	rolled := busy("d")
	rolled.Revision = "rev-2"
	first := busy("d")
	first.Revision = "rev-1"
	tr, _ := run(ptr(first), ptr(idle("d")), ptr(idle("d")), ptr(first), ptr(restarted))
	if r := replicaOf(t, tr, "d"); r.Reason != Restarted || !r.Since.Equal(t0.Add(4*cycle)) || len(r.Returns) != 0 {
		t.Errorf("a new Ready time: settle %s at %v, returns %v; want restarted at %v, none", r.Reason, r.Since, r.Returns, t0.Add(4*cycle))
	}
	rolled.ReadySince = restarted.ReadySince
	tr, _ = run(ptr(first), ptr(rolled))
	if r := replicaOf(t, tr, "d"); r.Reason != RolledOut || !r.Since.Equal(t0.Add(cycle)) {
		t.Errorf("a new revision: settle %s at %v, want rolled out at %v", r.Reason, r.Since, t0.Add(cycle))
	}

	// A Ready time, a lend time or a revision that is unknown on a row
	// changes nothing.
	unknown := busy("d")
	unknown.ReadySince, unknown.Revision = time.Time{}, ""
	tr, _ = run(ptr(first), ptr(unknown), ptr(first))
	if r := replicaOf(t, tr, "d"); r.Reason != Joined || !r.ReadySince.Equal(first.ReadySince) {
		t.Errorf("unknown fields: settle %s, Ready since %v; want the join and the Ready time kept", r.Reason, r.ReadySince)
	}
}

// A bridge is a lend from its first sight. Lent again to the same variant
// inside Period, it keeps its key and its Ready time; the new lend time
// starts a settle. Missing one cycle under the same lend does not.
func TestABridgeLentAgainSettlesAgain(t *testing.T) {
	bridge := busy("pool-0")
	bridge.Bridge = true
	bridge.LentSince = t0.Add(-time.Hour)
	key := bridge.Key
	again := bridge
	again.LentSince = t0.Add(3 * cycle)

	tr, _ := run(ptr(bridge), ptr(bridge), nil, ptr(bridge))
	r, _ := tr.Replica(model, key)
	if r.Reason != Lent || !r.Since.Equal(t0) || !r.LentSeen.Equal(t0) {
		t.Errorf("one missed cycle under the same lend: settle %s at %v, lend seen %v; want the lend at %v", r.Reason, r.Since, r.LentSeen, t0)
	}
	tr, _ = run(ptr(bridge), ptr(bridge), nil, ptr(again))
	r, _ = tr.Replica(model, key)
	if r.Reason != Lent || !r.Since.Equal(t0.Add(3*cycle)) || !r.LentSeen.Equal(t0.Add(3*cycle)) {
		t.Errorf("lent again: settle %s at %v, lend seen %v; want a new lend at %v", r.Reason, r.Since, r.LentSeen, t0.Add(3*cycle))
	}
	if got := tr.Verdicts()[Lent]; got != 2 {
		t.Errorf("%d lends counted, want 2", got)
	}

	// The same Pod as another variant's bridge, or as its own replica, is
	// another key.
	other := bridge
	other.Variant = "dec2"
	tr, _ = run(ptr(bridge), ptr(other))
	if r, ok := tr.Replica(model, other.Key); !ok || !r.Since.Equal(t0.Add(cycle)) {
		t.Errorf("lent to another variant: %+v %v, want a new replica at %v", r, ok, t0.Add(cycle))
	}
}

// The Ready time a reader waits from is the later of the Pod's and the
// first sight of it: a node clock 90 s behind the controller's, a Pod Ready
// a moment ago, an LWS group whose pods became Ready at different times.
func TestReadyFromIsTheLaterOfTheReadyTimeAndItsFirstSight(t *testing.T) {
	behind := busy("lag")
	behind.ReadySince = t0.Add(-90 * time.Second)
	ahead := busy("lead")
	ahead.ReadySince = t0.Add(30 * time.Second)
	leader, worker := busy("grp-0"), busy("grp-0-1")
	leader.ReadySince, worker.ReadySince = t0.Add(-3*time.Minute), t0.Add(cycle+5*time.Second)
	tr := NewTracker()
	tr.Observe(model, []Row{behind, ahead}, t0)
	tr.Observe(model, []Row{leader, worker}, t0.Add(cycle))
	for _, tc := range []struct {
		pod  string
		want time.Time
	}{{"lag", t0}, {"lead", t0.Add(30 * time.Second)}, {"grp-0", t0.Add(cycle)}, {"grp-0-1", t0.Add(cycle + 5*time.Second)}} {
		if got := replicaOf(t, tr, tc.pod).ReadyFrom(); !got.Equal(tc.want) {
			t.Errorf("%s: ready from %v, want %v", tc.pod, got, tc.want)
		}
	}
	notReady := busy("new")
	notReady.Ready, notReady.ReadySince = false, time.Time{}
	tr.Observe(model, []Row{notReady}, t0.Add(2*cycle))
	if got := replicaOf(t, tr, "new").ReadyFrom(); !got.IsZero() {
		t.Errorf("a Pod never seen Ready: ready from %v, want unknown", got)
	}
	ready := busy("new")
	ready.ReadySince = t0.Add(2*cycle + 5*time.Second)
	tr.Observe(model, []Row{ready}, t0.Add(3*cycle))
	if r := replicaOf(t, tr, "new"); !r.ReadyFrom().Equal(t0.Add(3*cycle)) || r.Reason != Joined {
		t.Errorf("Ready after its first sight: ready from %v, settle %s; want %v and the join", r.ReadyFrom(), r.Reason, t0.Add(3*cycle))
	}
}

// Back compares against the row one period back, not one cycle back, and the
// trace keeps TraceHorizon. A replica absent for TraceHorizon is forgotten,
// and so is a model Retain is not given.
func TestBackTraceAndForgetting(t *testing.T) {
	tr, last := run(repeat(busy("d"), int(10*time.Minute/cycle))...)
	key := Key{Pod: "d", Variant: "dec"}
	s, ok := tr.Back(model, key, Period, last)
	if !ok || !s.At.Equal(last.Add(-Period)) || s.Activity != Load {
		t.Errorf("one period back from %v: %v %v, want the row at %v", last, s.At, ok, last.Add(-Period))
	}
	trace := tr.Trace(model, key)
	if len(trace) == 0 || last.Sub(trace[0].At) > TraceHorizon {
		t.Errorf("trace from %v to %v, want at most %v", trace[0].At, last, TraceHorizon)
	}
	if _, ok := tr.Back(model, key, 20*time.Minute, last); ok {
		t.Error("a row older than the trace was found")
	}

	tr.Observe(model, []Row{busy("other")}, last.Add(TraceHorizon+cycle))
	if _, ok := tr.Replica(model, key); ok {
		t.Error("a replica absent for longer than TraceHorizon is still known")
	}
	tr.Retain(map[string]bool{"ns/kept": true})
	if _, ok := tr.Replica(model, Key{Pod: "other", Variant: "dec"}); ok {
		t.Error("Retain kept a model it was not given")
	}
	tr.Observe(model, []Row{busy("d")}, last)
	tr.Forget(model)
	if _, ok := tr.Replica(model, key); ok {
		t.Error("Forget kept the model")
	}
}

// One Observe per cycle is one sighting per replica: rows of several
// replicas are tracked apart, and a View copy is not the tracker's.
func TestReplicasAreTrackedApartAndCopied(t *testing.T) {
	tr := NewTracker()
	tr.Observe(model, []Row{busy("a"), idle("b")}, t0)
	tr.Observe(model, []Row{busy("a"), busy("b")}, t0.Add(cycle))
	b := replicaOf(t, tr, "b")
	if len(b.Returns) != 1 || len(replicaOf(t, tr, "a").Returns) != 0 {
		t.Fatalf("returns a %v, b %v; want one on b only", replicaOf(t, tr, "a").Returns, b.Returns)
	}
	b.Returns[0].IdleRows = 99
	if replicaOf(t, tr, "b").Returns[0].IdleRows == 99 {
		t.Error("a Replica read shares its returns with the tracker")
	}
}
