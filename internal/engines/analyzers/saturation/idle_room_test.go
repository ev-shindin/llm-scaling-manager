package saturation

import (
	"context"
	"math"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// A replica that has only just turned Ready reads idle: its one-minute peak
// covers the seconds since it came up. A fleet scaling up lands replicas one
// at a time, so it nearly always has one, and the short fleet's backlog must
// not go with it. Every original replica is full and queued here; the fresh
// fourth adds nothing and takes nothing away.
func TestIdleRoomFreshReplicaKeepsTheBacklog(t *testing.T) {
	full := []float64{0.95, 0.95, 0.95}
	f0 := imbFloorFleetAt(t, imbFleet(full, []int{60, 60, 60}, []float64{1, 1, 1}), 5, false)
	f1 := imbFloorFleetAt(t, imbFleet(append(full, 0), []int{60, 60, 60, 0}, []float64{1, 1, 1, 0}), 5, false)
	f2 := imbFloorFleetAt(t, imbFleet(append(full, 0.31), []int{60, 60, 60, 0}, []float64{1, 1, 1, 0.3}), 5, false)
	t.Logf("implied replicas at lambda 5, mu ~1: three full queued 60 %.2f; plus a fourth just Ready %.2f; plus a fourth at 0.31 KV %.2f",
		f0, f1, f2)
	if f1 < f0-0.01 {
		t.Errorf("a replica Ready for one cycle dropped the short fleet's order from %.2f to %.2f replicas", f0, f1)
	}

	// The occupancy path, with no mu on record: the fresh replica's queue of
	// nothing must not take the others' queues out of their charge.
	occ := func(fracs []float64, queues []int) float64 {
		t.Helper()
		a, _ := imbAnalyzer()
		rows := make([]domain.ReplicaMetrics, 0, len(fracs))
		for i := range fracs {
			rows = append(rows, imbRow("pod-"+string(rune('a'+i)), "v", fracs[i], queues[i], 2000, 500, 1.0))
		}
		states := []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: len(rows), GPUsPerReplica: 1}}
		r, err := a.Analyze(context.Background(), makeAnalyzerInput(rows, states))
		if err != nil {
			t.Fatal(err)
		}
		return r.TotalDemand
	}
	a0 := occ(full, []int{40, 40, 40})
	a1 := occ(append(full, 0), []int{40, 40, 40, 0})
	t.Logf("occupancy demand: three full queued 40 %.0f; plus a fourth just Ready %.0f", a0, a1)
	if a1 != a0 {
		t.Errorf("a replica Ready for one cycle moved the fleet's demand %.0f -> %.0f", a0, a1)
	}
}

// A sibling idle for long enough takes only what it has room for. One idle
// prefill replica beside a queue of 439 prompts (2.6M tokens) has room for
// its effective capacity, and the rest of the queue is still charged.
func TestIdleRoomBoundsWhatIsDropped(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 3, GPUsPerReplica: 1},
	}
	const prompt = 6000.0
	a, clock := imbAnalyzer()
	rows := []domain.ReplicaMetrics{
		imbRow("decode-0", "decode-v", 0.25, 0, prompt, 1000, 0.5),
		imbRow("decode-1", "decode-v", 0.25, 0, prompt, 1000, 0.5),
		imbRow("prefill-0", "prefill-v", 0.20, 439, prompt, 1, 1.0),
		imbRow("prefill-1", "prefill-v", 0, 0, prompt, 1, 0),
		imbRow("prefill-2", "prefill-v", 0.35, 1, prompt, 1, 1.0),
	}
	r := imbSettle(t, a, clock, makeAnalyzerInput(rows, states))
	tokens := func(f float64) float64 { return float64(int64(f * float64(imbKv))) }
	resident := tokens(0.20) + tokens(0.35)
	queue := (439 + 1) * prompt
	// Every prefill replica is priced at one capacity (the variant's k2 or
	// k1), and the idle one holds nothing: its room is that capacity.
	idleP := imbVariant(t, r, "prefill-v").PerReplicaCapacity
	want := resident + queue - min(idleP, queue)
	got := r.RoleDemand[domain.RolePrefill]
	t.Logf("prefill demand %.0f: resident %.0f, queue %.0f, the idle replica's room %.0f", got, resident, queue, idleP)
	if math.Abs(got-want) > 2 {
		t.Errorf("prefill demand %.0f, want %.0f: the resident KV and the queue less the one idle replica's room", got, want)
	}
}

// A warm-pool bridge idle beside a queued replica is no room the role keeps:
// the queue is charged in full, as with no idle replica at all.
func TestIdleRoomBridgeIsNoRoom(t *testing.T) {
	states := []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
	run := func(bridge bool) float64 {
		t.Helper()
		a, clock := imbAnalyzer()
		rows := []domain.ReplicaMetrics{imbRow("own-0", "v", 0.95, 40, 2000, 500, 1.0)}
		if bridge {
			b := imbRow("bridge-0", "v", 0, 0, 2000, 500, 0)
			b.FromWarmPool = true
			rows = append(rows, b)
		}
		return imbSettle(t, a, clock, makeAnalyzerInput(rows, states)).TotalDemand
	}
	alone, withBridge := run(false), run(true)
	t.Logf("demand: one queued replica %.0f; beside an idle bridge %.0f", alone, withBridge)
	if withBridge < alone {
		t.Errorf("an idle warm-pool bridge took the queue out of demand: %.0f -> %.0f", alone, withBridge)
	}
}

// Idle is measured against the replica's own capacity, min(k1, k2), not
// against its cache. A compute-bound fleet whose k2 is 25 % of the cache has
// replicas at their limit under the cache's 30 % line; one with nothing
// waiting for a minute is busy, not room, and one request more or less on it
// must not move the whole role's demand.
func TestIdleRoomIsMeasuredAgainstTheReplicasCapacity(t *testing.T) {
	const cache = int64(1_000_000)
	states := []domain.VariantReplicaState{{VariantName: "both-v", AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1}}
	row := func(pod string, tok int64, q int) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, "both-v", tok, cache, q, 6000, 1000)
		rm.RequestRate = 2
		rm.GenerationTokenRate = 2000
		rm.KvUsageInstant = float64(tok) / float64(cache)
		return rm
	}
	// demand seeds k2 = 250 000 on four queued replicas, then holds r3 at
	// r3Tokens with r3Queue waiting: for one cycle, or until it has read idle
	// for long enough to count.
	demand := func(r3Tokens int64, r3Queue int, settle bool) float64 {
		t.Helper()
		a, clock := imbAnalyzer()
		for range 3 {
			in := makeAnalyzerInput([]domain.ReplicaMetrics{
				row("r0", 250_000, 20), row("r1", 250_000, 20), row("r2", 250_000, 20), row("r3", 250_000, 20),
			}, states)
			if _, err := a.Analyze(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			*clock = clock.Add(2 * imbCycle)
		}
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			row("r0", 250_000, 20), row("r1", 250_000, 20), row("r2", 250_000, 20), row("r3", r3Tokens, r3Queue),
		}, states)
		if settle {
			return imbSettle(t, a, clock, in).TotalDemand
		}
		r, err := a.Analyze(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		return r.TotalDemand
	}
	const oneRequest = 7000
	for _, c := range []struct {
		name   string
		tokens int64
		settle bool
	}{
		{"r3 at 29 % of its cache, over its k2, one cycle", 290_000, false},
		{"r3 at 29 % of its cache, over its k2, settled", 290_000, true},
		{"r3 at 80 % of its k2 (20 % of its cache), settled", 200_000, true},
	} {
		busy, quiet := demand(c.tokens, 1, c.settle), demand(c.tokens, 0, c.settle)
		t.Logf("%s: demand with one request waiting on r3 %.0f, with none %.0f", c.name, busy, quiet)
		if busy-quiet > oneRequest {
			t.Errorf("%s: one request fewer on r3 dropped demand by %.0f tokens, more than the request's own %d: its siblings' queues went with it",
				c.name, busy-quiet, oneRequest)
		}
	}
}

// A P/D prefill replica holds KV only until decode pulls it, so a busy one
// reads low KV with nothing waiting. Completing at the rate its siblings do,
// it is not room for their queues, however long it reads that way.
func TestIdleRoomPrefillIsJudgedByItsRate(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 4, GPUsPerReplica: 1},
	}
	const prompt = 6000.0
	run := func(lowRate float64) float64 {
		t.Helper()
		a, clock := imbAnalyzer()
		rows := []domain.ReplicaMetrics{
			imbRow("decode-0", "decode-v", 0.25, 0, prompt, 1000, 0.5),
			imbRow("decode-1", "decode-v", 0.25, 0, prompt, 1000, 0.5),
			imbRow("prefill-0", "prefill-v", 0.20, 30, prompt, 1, 1.0),
			imbRow("prefill-1", "prefill-v", 0.20, 30, prompt, 1, 1.0),
			imbRow("prefill-2", "prefill-v", 0.20, 30, prompt, 1, 1.0),
			imbRow("prefill-3", "prefill-v", 0.05, 0, prompt, 1, lowRate),
		}
		return imbSettle(t, a, clock, makeAnalyzerInput(rows, states)).RoleDemand[domain.RolePrefill]
	}
	busy, idle := run(1.0), run(0)
	tokens := func(f float64) float64 { return float64(int64(f * float64(imbKv))) }
	full := 3*tokens(0.20) + tokens(0.05) + 3*30*prompt
	t.Logf("prefill demand: the low-KV replica completing at its siblings' rate %.0f, completing nothing %.0f; every queue charged %.0f",
		busy, idle, full)
	if busy != full {
		t.Errorf("a prefill replica at 5 %% KV completing 1 req/s took its siblings' queues out of demand: %.0f, want %.0f", busy, full)
	}
	if !(idle < full) {
		t.Errorf("a prefill replica at 5 %% KV completing nothing for minutes is room, yet demand stayed %.0f", idle)
	}
}
