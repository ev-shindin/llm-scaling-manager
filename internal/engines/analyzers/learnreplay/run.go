package learnreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// Mismatch is one place a replay diverged from its recording.
type Mismatch struct {
	// Seq and Kind are the recorded event the divergence is at (Seq 0 for a
	// recording-wide one, such as a count of events).
	Seq  uint64
	Kind string
	// What says what diverged: "store" for the check before an Analyze,
	// "line" for an event the replay recorded differently, "evict" for a
	// store eviction that removed another count, "router" for a router asked
	// when the run did not ask it, or the reverse.
	What string
	// Detail is a readable account: the differing fields, or the two lines
	// around their first difference.
	Detail string
	// Fatal is whether the mismatch fails the replay in its mode. An evict
	// count is a check only a Fidelity replay is held to (2.4): elsewhere
	// LearnedAt follows the replay's own writes, and it is reported only.
	Fatal bool
}

// String is the mismatch on one line: where, what, and the detail.
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
	// Ignored counts, per kind, the events of owners this replay does not
	// drive (the engine-level layer #345 adds to the same recording, say):
	// skipped, and left out of the line comparison.
	Ignored map[string]int
	// TrimmedChecks is how many check events at the recording's end were
	// left out: a cap or a crash cut off the Analyze they belonged to.
	TrimmedChecks int
	// TornTail is a last line cut off mid-write, dropped (learnrec.Read).
	TornTail string
	// StoreChecks is how many store checks ran (one per recorded variant
	// record, before each Analyze that reads the store).
	StoreChecks int
	// Recording is the replay's own recording.
	Recording []byte
	// Mismatches is every divergence found, in sequence order.
	Mismatches []Mismatch
	// Paths is how often each learner path ran in the replay, by
	// saturation.PathNames (analyzer-learner.md, 2.5).
	Paths map[string]int64
}

// Failed is the mismatches that fail the replay in its mode.
func (r *Report) Failed() []Mismatch {
	var out []Mismatch
	for _, m := range r.Mismatches {
		if m.Fatal {
			out = append(out, m)
		}
	}
	return out
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

// owners is the kinds' owners a replay drives, by the prefix of their kind.
var owners = []string{"saturation.", "slo.", "throughput.", "store."}

// driven reports whether a replay applies, or checks, events of kind.
func driven(kind string) bool {
	if kind == learnrec.KindStart {
		return true
	}
	for _, o := range owners {
		if strings.HasPrefix(kind, o) {
			return true
		}
	}
	return false
}

// Replay replays recording. An error is a recording that cannot be replayed
// at all (no start, an unknown event of an owner it drives, an event for an
// analyzer the run did not have, a cancelled ctx); a divergence is a
// Mismatch in the report. In Fidelity mode the replay's own recording is
// compared with the original event by event.
func Replay(ctx context.Context, recording []byte, opts Options) (*Report, error) {
	read, err := learnrec.Read(recording)
	if err != nil {
		return nil, err
	}
	events := read.Events
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
	r.rep.TornTail = read.TornTail

	var kept []learnrec.RawEvent
	for _, ev := range events {
		if !driven(ev.Kind) {
			r.rep.Ignored[ev.Kind]++
			continue
		}
		kept = append(kept, ev)
	}
	// Check events are written inside the Analyze they belong to, before it.
	// At the end of a recording, with no Analyze after them, a cap or a crash
	// cut that Analyze off; the replay cannot make them, so they go.
	for len(kept) > 1 && checkKinds[kept[len(kept)-1].Kind] {
		kept = kept[:len(kept)-1]
		r.rep.TrimmedChecks++
	}
	for _, ev := range kept[1:] {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("learnreplay: %w", err)
		}
		if err := r.apply(ctx, ev); err != nil {
			return nil, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
	}
	if err := rec.Err(); err != nil {
		return nil, fmt.Errorf("learnreplay: the replay's own recording failed: %w", err)
	}
	r.rep.Recording = buf.Bytes()
	r.rep.Paths = r.sat.PathCounts()
	if opts.Mode == Fidelity {
		r.rep.Mismatches = append(r.rep.Mismatches, compareEvents(kept, r.rep.Recording)...)
	}
	return r.rep, nil
}
