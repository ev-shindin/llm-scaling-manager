package learnertest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stepFleet is two replicas of mu 1 hit by a step from 1.2 to 6 requests a
// second at 2 min, whose need is the arrival rate at mu 1, rounded up.
func stepFleet() Fleet {
	return Fleet{Pods: []float64{1, 1}, AddedMu: 1,
		Lambda: func(e time.Duration) float64 {
			if e < 2*time.Minute {
				return 1.2
			}
			return 6
		},
		Need:    Ceil,
		Horizon: 30 * time.Minute}
}

// A truthful step -- it orders need from what the replicas serve and queue
// -- settles at need, and an analyzer that never orders a replica more, or
// orders twice need, is caught. The simulator's own negative controls.
func TestFleet_TheVerdictTellsATruthfulOrderFromAWrongOne(t *testing.T) {
	truthful := stepFleet().Run(func(_ time.Time, _ time.Duration, lambda float64, _ []Replica) int {
		return Ceil(lambda)
	})
	t.Logf("truthful: need %d, final %d, peak %d, under need for %s", truthful.Need, truthful.Final, truthful.Peak, truthful.UnderFor)
	if v := truthful.Verdict(); v != "" {
		t.Errorf("a truthful order was judged: %s", v)
	}
	if truthful.Need != 6 || truthful.Final != 6 {
		t.Errorf("need %d, final %d: want 6 and 6", truthful.Need, truthful.Final)
	}
	// The fleet counts its starting replicas: an order on the cycle the
	// load steps is never under need.
	if truthful.UnderFor != 0 {
		t.Errorf("an order on the step's own cycle was under need for %s", truthful.UnderFor)
	}

	stuck := stepFleet().Run(func(time.Time, time.Duration, float64, []Replica) int { return 2 })
	if v := stuck.Verdict(); !strings.HasPrefix(v, "under-ordered") {
		t.Errorf("a fleet held at 2 against a need of 6 was judged %q", v)
	}
	over := stepFleet().Run(func(_ time.Time, _ time.Duration, lambda float64, _ []Replica) int { return 2 * Ceil(lambda) })
	if v := over.Verdict(); !strings.HasPrefix(v, "over-ordered") || over.OverSince < 20*time.Minute {
		t.Errorf("a fleet ordered at twice need was judged %q, over for %s", v, over.OverSince)
	}
}

// What the step is shown: only Ready replicas, sharing the arrivals, each
// saturated and queueing when its share is over its mu; a scale-down waits
// for the stabilization window. Queue-blind, each is offered an equal share;
// at the shortest-queue limit the shares still sum to the arrivals.
func TestFleet_TheStepSeesTheReadyReplicas(t *testing.T) {
	for _, blind := range []bool{true, false} {
		f := stepFleet()
		f.Horizon = 10 * time.Minute
		f.QueueBlind = blind
		var sawJoiner, sawQueue bool
		res := f.Run(func(now time.Time, e time.Duration, lambda float64, ready []Replica) int {
			offered := 0.0
			for _, r := range ready {
				offered += r.Offered
				if blind && r.Offered != lambda/float64(len(ready)) {
					t.Fatalf("queue-blind: %s offered %g of %g over %d", r.Name, r.Offered, lambda, len(ready))
				}
			}
			if math.Abs(offered-lambda) > 1e-9 {
				t.Fatalf("queue-blind %v: the replicas were offered %g of %g", blind, offered, lambda)
			}
			for _, r := range ready {
				if r.ReadyAt.After(now) {
					t.Fatalf("%s is shown before it is Ready", r.Name)
				}
				if r.Saturated && r.Backlog > 0 {
					sawQueue = true
				}
				if r.Index >= 2 {
					sawJoiner = true
				}
			}
			if e >= 5*time.Minute {
				return 1
			}
			return Ceil(lambda)
		})
		if !sawQueue || !sawJoiner {
			t.Errorf("queue-blind %v: the step never saw a queue (%v) or a joined replica (%v)", blind, sawQueue, sawJoiner)
		}
		// The order fell to 1 at 5 min; the window keeps the fleet at 6 for
		// 5 min more, to the horizon.
		if res.Final != 6 {
			t.Errorf("queue-blind %v: the fleet fell to %d inside the stabilization window: %v", blind, res.Final, res.Trace)
		}
	}
}

// A replica that drains its queue while offered less than its mu serves its
// share and its queue, up to its mu: offered nothing, it still serves at its
// limit, and it never serves less than the lower of its share and its mu.
func TestFleet_ADrainingReplicaServesItsQueue(t *testing.T) {
	f := Fleet{Pods: []float64{1, 1}, AddedMu: 1, QueueBlind: false, Need: Ceil, Horizon: 20 * time.Minute,
		Lambda: func(e time.Duration) float64 {
			if e < 2*time.Minute {
				return 4
			}
			return 1
		}}
	var sawDrain bool
	f.Run(func(_ time.Time, _ time.Duration, _ float64, ready []Replica) int {
		for _, r := range ready {
			if r.Served > r.Mu+1e-12 || r.Served < min(r.Offered, r.Mu)-1e-12 {
				t.Fatalf("%s served %g, offered %g at mu %g", r.Name, r.Served, r.Offered, r.Mu)
			}
			if r.Offered < r.Mu && r.Backlog > 0 {
				sawDrain = true
				if r.Served != r.Mu || !r.Saturated {
					t.Fatalf("%s drains a queue of %g offered %g and served %g (saturated %v)", r.Name, r.Backlog, r.Offered, r.Served, r.Saturated)
				}
			}
		}
		return 2
	})
	if !sawDrain {
		t.Fatal("premise: no replica drained a queue")
	}
}

// A step that holds keeps the fleet as it is, starting replicas included:
// it neither grows it under a step nor shrinks it under light load.
func TestFleet_AHoldKeepsTheFleet(t *testing.T) {
	f := stepFleet()
	f.Horizon = 10 * time.Minute
	res := f.Run(func(_ time.Time, e time.Duration, _ float64, _ []Replica) int {
		if e < time.Minute {
			return 4
		}
		return Hold
	})
	for i, n := range res.Trace {
		if n != 4 {
			t.Fatalf("cycle %d: the fleet is %d under a hold of 4: %v", i, n, res.Trace)
		}
	}
	if v := res.Verdict(); !strings.HasPrefix(v, "under-ordered") {
		t.Errorf("a fleet held at 4 against a need of 6 was judged %q", v)
	}
}

// learnertest is for tests: no non-test file of the module imports it.
func TestOnlyTestsImportLearnertest(t *testing.T) {
	const self = "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/internal/learnertest"
	root := filepath.Join("..", "..", "..", "..", "..")
	files, importers := 0, 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path != self {
				continue
			}
			importers++
			if !strings.HasSuffix(p, "_test.go") {
				t.Errorf("%s imports learnertest, and is not a test", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 500 || importers < 2 {
		t.Fatalf("the walk read %d Go files and found %d importers: it did not read the module", files, importers)
	}
}

// A router's weights split the arrivals by share; weights that sum to zero,
// or to no number, split them equally rather than offer NaN.
func TestFleet_WeightsSplitTheArrivals(t *testing.T) {
	for _, c := range []struct {
		name   string
		weight func(i int, _ time.Duration) float64
		want   []float64
	}{
		{"three to one", func(i int, _ time.Duration) float64 { return float64(3 - 2*i) }, []float64{3, 1}},
		{"all zero", func(int, time.Duration) float64 { return 0 }, []float64{2, 2}},
		{"negative", func(int, time.Duration) float64 { return -1 }, []float64{2, 2}},
	} {
		f := Fleet{Pods: []float64{10, 10}, AddedMu: 10, Lambda: func(time.Duration) float64 { return 4 },
			Weight: c.weight, Need: Ceil, Horizon: Cycle}
		f.Run(func(_ time.Time, _ time.Duration, _ float64, ready []Replica) int {
			for i, r := range ready {
				if r.Offered != c.want[i] {
					t.Errorf("%s: %s offered %g, want %g", c.name, r.Name, r.Offered, c.want[i])
				}
			}
			return Hold
		})
	}
}

// The router fills the shortest queues first: with no queue it splits by
// weight; a replica whose queue is over the level the arrivals fill the
// others to gets none; one whose queue is under it gets the difference.
func TestFleet_TheRouterFillsTheShortestQueues(t *testing.T) {
	for _, c := range []struct {
		name      string
		lambda    float64
		w, q, out []float64
	}{
		{"no queue, equal", 6, []float64{1, 1, 1}, []float64{0, 0, 0}, []float64{2, 2, 2}},
		{"no queue, weighted", 4, []float64{3, 1}, []float64{0, 0}, []float64{3, 1}},
		{"a long queue gets none", 6, []float64{1, 1, 1}, []float64{50, 0, 0}, []float64{0, 3, 3}},
		{"a short queue is filled to the level", 9, []float64{1, 1, 1}, []float64{3, 0, 0}, []float64{1, 4, 4}},
		{"weight scales the level", 9, []float64{2, 1}, []float64{3, 0}, []float64{5, 4}},
		{"weight zero gets nothing", 6, []float64{0, 1}, []float64{0, 0}, []float64{0, 6}},
		{"no arrivals", 0, []float64{1, 1}, []float64{0, 4}, []float64{0, 0}},
		{"equal queues split equally", 6, []float64{1, 1, 1}, []float64{2, 2, 2}, []float64{2, 2, 2}},
		{"a queue tied with the level gets none", 8, []float64{1, 1, 1}, []float64{4, 0, 0}, []float64{0, 4, 4}},
		{"negative weight gets nothing", 6, []float64{-1, 1}, []float64{0, 0}, []float64{0, 6}},
		{"NaN weight gets nothing", 6, []float64{math.NaN(), 1}, []float64{0, 0}, []float64{0, 6}},
		{"infinite weight gets nothing", 6, []float64{math.Inf(1), 1}, []float64{0, 0}, []float64{0, 6}},
		{"NaN queue gets nothing", 6, []float64{1, 1}, []float64{math.NaN(), 0}, []float64{0, 6}},
		{"no queue a number: split by weight", 6, []float64{1, 2}, []float64{math.NaN(), math.Inf(1)}, []float64{2, 4}},
		{"a negative queue counts as none", 6, []float64{1, 1}, []float64{-5, 0}, []float64{3, 3}},
	} {
		got := route(c.lambda, c.w, c.q)
		for i := range got {
			if math.Abs(got[i]-c.out[i]) > 1e-9 {
				t.Errorf("%s: offers %v, want %v", c.name, got, c.out)
				break
			}
		}
	}
}

// The router on random fleets: the offers sum to the arrivals and none is
// negative; every replica offered something ends at one level of queue plus
// offer per unit of weight, and every replica offered nothing already holds
// that level or more.
func TestFleet_TheRouterFillsToOneLevel(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 2000 {
		n := 1 + rng.IntN(8)
		w, q := make([]float64, n), make([]float64, n)
		for i := range n {
			w[i] = 0.1 + 3*rng.Float64()
			if rng.IntN(3) > 0 {
				q[i] = 50 * rng.Float64()
			}
		}
		lambda := 20 * rng.Float64()
		offers := route(lambda, w, q)
		sum, level := 0.0, math.NaN()
		for i, o := range offers {
			if !(o >= 0) {
				t.Fatalf("trial %d: offer %g to replica %d (w %v, q %v, lambda %g)", trial, o, i, w, q, lambda)
			}
			sum += o
			if o > 0 {
				l := (q[i] + o) / w[i]
				if math.IsNaN(level) {
					level = l
				} else if math.Abs(l-level) > 1e-9*max(1, level) {
					t.Fatalf("trial %d: replica %d fills to %g, another to %g (w %v, q %v, lambda %g, offers %v)", trial, i, l, level, w, q, lambda, offers)
				}
			}
		}
		if math.Abs(sum-lambda) > 1e-9*max(1, lambda) {
			t.Fatalf("trial %d: offers sum to %g of %g", trial, sum, lambda)
		}
		for i, o := range offers {
			if o == 0 && lambda > 0 && q[i]/w[i] < level-1e-9*max(1, level) {
				t.Fatalf("trial %d: replica %d holds %g under the level %g and was offered nothing", trial, i, q[i]/w[i], level)
			}
		}
	}
}

// The queue a slow start leaves on the first replicas: four replicas at mu 1
// under 12 requests a second for a 463 s start queue 2 a second each, about
// 960 requests apiece by the time the thirteen joiners are Ready. The joiners
// take the arrivals (12 over 13, under their mu), so the four drain at mu, in
// about 16 min. A queue-blind router feeds them 12/17 and drains them at 0.29
// a second, in about 54 min: the negative control.
func TestFleet_TheRouterDrainsAStartsQueueAtMu(t *testing.T) {
	drain := func(blind bool) (peak float64, landed, cleared time.Duration) {
		f := Fleet{Pods: []float64{1, 1, 1, 1}, AddedMu: 1, Lambda: func(time.Duration) float64 { return 12 },
			Need: Ceil, Horizon: 90 * time.Minute, StartTime: 463 * time.Second, QueueBlind: blind}
		landed, cleared = -1, -1
		f.Run(func(_ time.Time, e time.Duration, _ float64, ready []Replica) int {
			var queued float64
			for _, r := range ready {
				if r.Index < 4 {
					queued = max(queued, r.Backlog)
				}
			}
			peak = max(peak, queued)
			if len(ready) == 17 && landed < 0 {
				landed = e
			}
			if landed >= 0 && queued == 0 && cleared < 0 {
				cleared = e
			}
			return 17
		})
		return peak, landed, cleared
	}
	peak, landed, cleared := drain(false)
	_, blindLanded, blindCleared := drain(true)
	atMu := time.Duration(peak * float64(time.Second))
	t.Logf("each first replica queued %.0f; the joiners landed at %s; the queue cleared %s later, %s later queue-blind",
		peak, landed, cleared-landed, blindCleared-blindLanded)
	if landed < 0 || cleared < 0 || cleared-landed > atMu+2*Cycle {
		t.Fatalf("the start's queue of %.0f cleared %s after the joiners landed, want about %s at mu", peak, cleared-landed, atMu)
	}
	if blindCleared >= 0 && blindCleared-blindLanded < 2*atMu {
		t.Fatalf("premise: queue-blind, the queue cleared %s after the joiners landed, not slower than at mu (%s)",
			blindCleared-blindLanded, atMu)
	}
}
