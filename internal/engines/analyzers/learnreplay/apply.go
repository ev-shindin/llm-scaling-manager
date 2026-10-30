package learnreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// checkKinds are made inside a replayed call, never applied: the replay
// records its own, and Fidelity compares the two.
var checkKinds = map[string]bool{
	saturation.KindPublishLine: true,
	saturation.KindBorrowLine:  true,
}

// apply replays one event on the analyzers, at the event's clock.
func (r *replayer) apply(ctx context.Context, ev learnrec.RawEvent) error {
	if checkKinds[ev.Kind] {
		return nil
	}
	r.clock.SetTime(ev.Now)
	r.rep.Applied++
	switch ev.Kind {
	case capacity.KindFileScaleTarget:
		var d capacity.FileScaleTargetEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.store.FileScaleTarget(d.Namespace, d.ModelID, d.Variant, d.Accelerator, d.GpuCount, d.Params, d.RollingOut, ev.Now)
	case capacity.KindEvictStale:
		var d capacity.EvictStaleEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		// Recomputed from the replay's own records; what the run removed is
		// a check, which only a Fidelity replay holds it to.
		if n := r.store.EvictStale(ev.Now, d.Timeout); n != d.Removed {
			r.mismatch(ev, "evict", fmt.Sprintf("removed %d records, the run removed %d", n, d.Removed))
		}

	case saturation.KindAnalyze:
		var d saturation.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		input, err := decodeInput(d.Input)
		if err != nil {
			return err
		}
		if err := r.checkStore(ev, input, d.Records); err != nil {
			return err
		}
		r.rep.Analyzes[ev.Kind]++
		_, _ = r.sat.Analyze(ctx, input)
	case saturation.KindNoteVariantsPresent:
		var d saturation.PresentEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.sat.NoteVariantsPresent(d.Namespace, d.ModelID, d.Variants)
	case saturation.KindEvictStaleHistory:
		r.sat.EvictStaleHistory()
	case saturation.KindForgetVariant:
		var d saturation.ForgetVariantEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.sat.ForgetVariant(d.Namespace, d.ModelID, d.Variant, d.Token)
	case saturation.KindForgottenToken:
		var d saturation.ForgottenTokenEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.sat.ForgottenToken(d.Namespace, d.Variant)
	case saturation.KindHoldLowerBounds:
		var d saturation.HoldEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.sat.HoldLowerBounds(d.Held)
	case saturation.KindExportLearnedState:
		r.sat.ExportLearnedState()
	case saturation.KindRestoreLearnedState:
		var d saturation.RestoreEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		var s saturation.LearnedState
		if err := d.State.Decode(&s); err != nil {
			return err
		}
		r.sat.RestoreLearnedState(s)
	case saturation.KindTakeRestoreOutcomes:
		r.sat.TakeRestoreOutcomes()
	case saturation.KindForgetLearnedState:
		r.sat.ForgetLearnedState()
	case saturation.KindStableAcceleratorFor:
		var d saturation.AcceleratorEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.sat.StableAcceleratorFor(d.Namespace, d.Variant, d.Accelerator)

	case slo.KindAnalyze:
		if r.slo == nil {
			return errNoSLO
		}
		var d slo.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		input, err := decodeInput(d.Input)
		if err != nil {
			return err
		}
		if err := r.checkStore(ev, input, d.Records); err != nil {
			return err
		}
		if d.RouterCalled && r.router == nil {
			return errors.New("the run asked a router, and the start says it had none")
		}
		if r.router != nil {
			r.router.latency, r.router.called = d.RouterLatency, false
		}
		r.rep.Analyzes[ev.Kind]++
		_, _ = r.slo.Analyze(ctx, input)
		if r.router != nil && r.router.called != d.RouterCalled {
			r.mismatch(ev, "router", fmt.Sprintf("the replay asked the router: %v, the run: %v", r.router.called, d.RouterCalled))
		}
	case slo.KindEvictStale:
		if r.slo == nil {
			return errNoSLO
		}
		var d slo.EvictStaleEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.slo.EvictStale(d.Now)
	case slo.KindBeginCycle:
		if r.slo == nil {
			return errNoSLO
		}
		r.slo.BeginCycle()
	case slo.KindNoteVariantsPresent:
		if r.slo == nil {
			return errNoSLO
		}
		var d slo.PresentEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		r.slo.NoteVariantsPresent(d.Namespace, d.ModelID, d.Variants)

	case throughput.KindAnalyze:
		if r.tp == nil {
			return errNoThroughput
		}
		var d throughput.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		input, err := decodeInput(d.Input)
		if err != nil {
			return err
		}
		r.rep.Analyzes[ev.Kind]++
		_, _ = r.tp.Analyze(ctx, input)
	case throughput.KindObserve:
		if r.tp == nil {
			return errNoThroughput
		}
		var d throughput.ObserveEvent
		if err := ev.DecodeData(&d); err != nil {
			return err
		}
		var rows []domain.ReplicaMetrics
		if err := d.Metrics.Decode(&rows); err != nil {
			return err
		}
		r.tp.Observe(ctx, d.Now, d.ModelID, d.Namespace, rows)

	default:
		return errors.New("an event the replay does not know")
	}
	return nil
}

// policyInput is learnrec.Input with its Config read back as the engine's
// type: the fields carry the same names, so it decodes what InputOf wrote.
type policyInput struct {
	ModelID        string
	Namespace      string
	ReplicaMetrics []domain.ReplicaMetrics
	VariantStates  []domain.VariantReplicaState
	Config         *config.ScalingPolicy
	ConfigType     string
	SchedulerQueue *domain.SchedulerQueueMetrics
	ArrivalRate    float64
}

// policyType is the Config type the engine passes, as InputOf names it.
var policyType = fmt.Sprintf("%T", &config.ScalingPolicy{})

func decodeInput(raw learnrec.Raw) (domain.AnalyzerInput, error) {
	// The type first: a config of another type does not decode as a policy.
	var loose map[string]any
	if err := raw.Decode(&loose); err != nil {
		return domain.AnalyzerInput{}, err
	}
	if typ, _ := loose["ConfigType"].(string); typ != "" && typ != policyType {
		return domain.AnalyzerInput{}, fmt.Errorf("the recorded config is a %s, which a replay cannot rebuild", typ)
	}
	var in policyInput
	if err := raw.Decode(&in); err != nil {
		return domain.AnalyzerInput{}, err
	}
	out := domain.AnalyzerInput{
		ModelID:        in.ModelID,
		Namespace:      in.Namespace,
		ReplicaMetrics: in.ReplicaMetrics,
		VariantStates:  in.VariantStates,
		SchedulerQueue: in.SchedulerQueue,
		ArrivalRate:    in.ArrivalRate,
	}
	if in.Config != nil {
		out.Config = in.Config
	}
	return out, nil
}

// checkStore holds the replay's store to the records the run's Analyze read,
// before the replay's Analyze reads its own: the whole record in Fidelity, the
// engine-supplied fields otherwise (capacity.EngineSuppliedDiff).
func (r *replayer) checkStore(ev learnrec.RawEvent, input domain.AnalyzerInput, recorded learnrec.Raw) error {
	var want map[string]*capacity.Record
	if err := recorded.Decode(&want); err != nil {
		return err
	}
	names := learnrec.VariantNames(input)
	got := capacity.RecordsOf(r.store, input.Namespace, input.ModelID, names)
	for _, v := range names {
		r.rep.StoreChecks++
		if r.opts.Mode == Fidelity {
			g, err := learnrec.Marshal(got[v])
			if err != nil {
				return err
			}
			w, err := learnrec.Marshal(want[v])
			if err != nil {
				return err
			}
			if !bytes.Equal(g, w) {
				r.mismatch(ev, "store", fmt.Sprintf("variant %s: %s", v, firstDiff(string(w), string(g))))
			}
			continue
		}
		if diff := capacity.EngineSuppliedDiff(got[v], want[v]); diff != "" {
			r.mismatch(ev, "store", fmt.Sprintf("variant %s: %s", v, diff))
		}
	}
	return nil
}

// mismatch reports a divergence at ev. Every kind fails the replay except an
// evict count outside Fidelity, which is reported only (2.4).
func (r *replayer) mismatch(ev learnrec.RawEvent, what, detail string) {
	fatal := what != "evict" || r.opts.Mode == Fidelity
	r.rep.Mismatches = append(r.rep.Mismatches, Mismatch{Seq: ev.Seq, Kind: ev.Kind, What: what, Detail: detail, Fatal: fatal})
}

// compareEvents holds a replay's recording to the events of the original it
// drives, event by event: the kind, the clock and the data, byte for byte.
// Sequence numbers are not compared, so events of owners the replay does not
// drive, left out of want, do not shift every line after them.
func compareEvents(want []learnrec.RawEvent, got []byte) []Mismatch {
	read, err := learnrec.Read(got)
	if err != nil {
		return []Mismatch{{What: "line", Fatal: true, Detail: fmt.Sprintf("the replay's recording does not read back: %v", err)}}
	}
	gotEvents := read.Events
	var out []Mismatch
	n := min(len(want), len(gotEvents))
	for i := 0; i < n; i++ {
		w, g := want[i], gotEvents[i]
		if w.Kind != g.Kind || !w.Now.Equal(g.Now) || !bytes.Equal(w.Data, g.Data) {
			out = append(out, Mismatch{Seq: w.Seq, Kind: w.Kind, What: "line", Fatal: true, Detail: describe(w, g)})
		}
	}
	if len(want) != len(gotEvents) {
		out = append(out, Mismatch{What: "line", Fatal: true, Detail: fmt.Sprintf("the run recorded %d events, the replay %d%s",
			len(want), len(gotEvents), extraKinds(want, gotEvents, n))})
	}
	return out
}

func describe(want, got learnrec.RawEvent) string {
	switch {
	case want.Kind != got.Kind:
		return fmt.Sprintf("the replay recorded %s here", got.Kind)
	case !want.Now.Equal(got.Now):
		return fmt.Sprintf("the replay read the clock at %s, the run at %s", got.Now, want.Now)
	}
	return firstDiff(string(want.Data), string(got.Data))
}

func extraKinds(want, got []learnrec.RawEvent, from int) string {
	rest := want
	who := "run"
	if len(got) > len(want) {
		rest, who = got, "replay"
	}
	var kinds []string
	for i := from; i < len(rest) && len(kinds) < 5; i++ {
		kinds = append(kinds, rest[i].Kind)
	}
	return fmt.Sprintf("; the %s's extra events begin %s", who, strings.Join(kinds, ", "))
}

// firstDiff shows two encodings around the first byte they differ at.
func firstDiff(want, got string) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	from := max(0, i-60)
	clip := func(s string) string {
		end := min(len(s), i+60)
		if from >= end {
			return ""
		}
		return s[from:end]
	}
	return fmt.Sprintf("at byte %d: run …%s… replay …%s…", i, clip(want), clip(got))
}

var (
	errNoSLO        = errors.New("an slo event, and the start says the run had no slo analyzer")
	errNoThroughput = errors.New("a throughput event, and the start says the run had no throughput analyzer")
)
