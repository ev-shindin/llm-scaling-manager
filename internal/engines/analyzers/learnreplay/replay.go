// Package learnreplay replays a learner recording (internal/signals/learnrec)
// through a fresh set of analyzers and a capacity store of their own: the
// test half of docs/proposals/analyzer-learner.md, 2.4 and 2.5.
//
// A replay builds the analyzers from the recording's start line and applies
// every event in sequence order, all analyzers together: the slo analyzer
// reads the store the saturation analyzer's Update just wrote. Router latency
// is served from the recording, never queried. The rows are recorded raw, so
// the P/D decode rewrite runs again inside the replayed Analyze.
//
// The replay records itself with a recorder of its own. At the recording's
// own commit (Fidelity) that recording must equal the original byte for
// byte: every result, every store record read before an Analyze, every check
// event. Test support only; nothing in the controller imports it.
package learnreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// Mode is how strictly a replay holds its store to the recording.
type Mode int

const (
	// Fidelity is a replay at the recording's own commit: before each
	// Analyze the replay's store must hold exactly the records the run's
	// did, and the replay's own recording must equal the original byte for
	// byte.
	Fidelity Mode = iota
	// Normal is every other replay (all switches off, each alone, all on):
	// the store is held only to the fields the engine supplied, by the
	// store's own rules, where the two records share a writer
	// (capacity.EngineSuppliedDiff). Outputs may move; they are the goldens'
	// business, not the replay's.
	Normal
)

// The packages a replay sets switches in (Options.Switches).
const (
	PackageSaturation = "saturation"
	PackageSLO        = "slo"
	PackageThroughput = "throughput"
	PackageCapacity   = "capacity"
)

// Options configure a replay.
type Options struct {
	Mode Mode
	// Switches is, per package, the replay switches to set
	// (SetSwitchesForReplay). Nil leaves every switch at its constructor
	// default.
	Switches map[string]map[string]bool
}

// Mismatch is one place a replay diverged from its recording.
type Mismatch struct {
	// Seq and Kind are the recorded event the divergence is at.
	Seq  uint64
	Kind string
	// What says what diverged: "store" for the check before an Analyze,
	// "line" for an event the replay recorded differently, "evict" for a
	// store eviction that removed another count.
	What string
	// Detail is a readable account: the differing fields, or the two lines
	// around their first difference.
	Detail string
}

func (m Mismatch) String() string {
	return fmt.Sprintf("seq %d %s: %s: %s", m.Seq, m.Kind, m.What, m.Detail)
}

// Report is what a replay did.
type Report struct {
	// Events is how many recorded events were read, the start included, and
	// Applied how many the replay called (check events are not applied).
	Events, Applied int
	// Analyzes counts the Analyze calls replayed, per kind.
	Analyzes map[string]int
	// StoreChecks is how many store checks ran (one per recorded variant
	// record, before each Analyze that reads the store).
	StoreChecks int
	// Recording is the replay's own recording.
	Recording []byte
	// Mismatches is every divergence found, in sequence order.
	Mismatches []Mismatch
}

// replayer holds one replay's analyzers and their shared clock.
type replayer struct {
	opts   Options
	clock  *clocktesting.FakePassiveClock
	store  *capacity.Store
	sat    *saturation.SaturationAnalyzer
	slo    *slo.Analyzer
	router *recordedRouter
	tp     *throughput.ThroughputAnalyzer
	rep    *Report
}

// Replay replays recording. An error is a recording that cannot be replayed
// at all (no start, an unknown event, an event for an analyzer the run did
// not have); a divergence is a Mismatch in the report. In Fidelity mode the
// replay's own recording is compared with the original line by line.
func Replay(ctx context.Context, recording []byte, opts Options) (*Report, error) {
	events, err := learnrec.Read(recording)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 || events[0].Kind != learnrec.KindStart {
		return nil, errors.New("learnreplay: the recording has no start line; a replay cannot build its analyzers without one")
	}
	var start learnrec.Start
	if err := events[0].DecodeData(&start); err != nil {
		return nil, fmt.Errorf("learnreplay: start: %w", err)
	}
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	r, err := build(start, events[0].Now, opts, rec)
	if err != nil {
		return nil, err
	}
	rec.Record(learnrec.KindStart, events[0].Now, start)
	r.rep.Events = len(events)
	for _, ev := range events[1:] {
		if err := r.apply(ctx, ev); err != nil {
			return nil, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
	}
	if err := rec.Err(); err != nil {
		return nil, fmt.Errorf("learnreplay: the replay's own recording failed: %w", err)
	}
	r.rep.Recording = buf.Bytes()
	if opts.Mode == Fidelity {
		r.rep.Mismatches = append(r.rep.Mismatches, compareLines(events, r.rep.Recording)...)
	}
	return r.rep, nil
}

// build makes the analyzers and the store a recording's start describes.
func build(start learnrec.Start, now time.Time, opts Options, rec *learnrec.Recorder) (*replayer, error) {
	if start.StoreRecords != 0 {
		return nil, fmt.Errorf("learnreplay: the recording started with %d capacity records; a recording starts at a controller start, with an empty store", start.StoreRecords)
	}
	r := &replayer{
		opts:  opts,
		clock: clocktesting.NewFakePassiveClock(now),
		store: capacity.NewStore().WithRecorder(rec),
		rep:   &Report{Analyzes: map[string]int{}},
	}
	h := capacity.Horizons{EpisodeGap: start.Saturation.EpisodeGap, VariantTimeout: start.Saturation.VariantTimeout,
		BucketRetention: start.Saturation.BucketRetention}
	sat, replaced := saturation.NewSaturationAnalyzer(r.store).WithClock(r.clock).WithHorizons(h)
	if len(replaced) > 0 {
		return nil, fmt.Errorf("learnreplay: the recorded horizons are unusable: %v", replaced)
	}
	// BorrowTickGap is max(BorrowedK2TickGap, 3 x interval): a third of it
	// sets it back exactly.
	r.sat = sat.WithOptimizationInterval(start.Saturation.BorrowTickGap / 3).WithRecorder(rec)
	if got := r.sat.BorrowTickGap(); got != start.Saturation.BorrowTickGap {
		return nil, fmt.Errorf("learnreplay: borrow tick gap %v, the recording has %v", got, start.Saturation.BorrowTickGap)
	}
	if s := start.SLO; s != nil {
		var src slo.RouterLatencySource
		if s.Judging {
			r.router = &recordedRouter{}
			src = r.router
		}
		r.slo = slo.New(src, r.store).WithClock(r.clock).WithStateTimeout(s.StateTimeout).
			WithSlopeHoldFor(s.SlopeHoldFor).WithRecorder(rec)
	}
	if start.Throughput {
		r.tp = throughput.NewThroughputAnalyzer().WithClock(r.clock).WithRecorder(rec)
	}
	if err := r.setSwitches(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *replayer) setSwitches() error {
	for pkg, sw := range r.opts.Switches {
		var err error
		switch pkg {
		case PackageSaturation:
			err = r.sat.SetSwitchesForReplay(sw)
		case PackageSLO:
			if r.slo == nil {
				return errors.New("learnreplay: switches for the slo analyzer, which the run did not have")
			}
			err = r.slo.SetSwitchesForReplay(sw)
		case PackageThroughput:
			if r.tp == nil {
				return errors.New("learnreplay: switches for the throughput analyzer, which the run did not have")
			}
			err = r.tp.SetSwitchesForReplay(sw)
		case PackageCapacity:
			err = r.store.SetSwitchesForReplay(sw)
		default:
			return fmt.Errorf("learnreplay: no package %q has replay switches", pkg)
		}
		if err != nil {
			return fmt.Errorf("learnreplay: %w", err)
		}
	}
	return nil
}

// recordedRouter serves the slo analyzer the latency the run read.
type recordedRouter struct {
	// latency is the next answer; called whether it was asked for it.
	latency *domain.RouterLatency
	called  bool
}

// CollectRouterLatency implements slo.RouterLatencySource from the recording.
func (r *recordedRouter) CollectRouterLatency(context.Context, string, string) *domain.RouterLatency {
	r.called = true
	return r.latency
}
