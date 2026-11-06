package learnreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learn"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
)

// The what-if planner's replay (docs/proposals/slo-capacity-planner.md,
// "Data per mode", Recording): replay a recording through the real
// analyzers, stop after the cycle asked for, and read what they had learned
// then. The answer is what the controller knew at that cycle.

// StateQuery names the point of a recording a what-if question is asked at.
type StateQuery struct {
	// Namespace and ModelID name the model. An empty ModelID takes the
	// recording's only model; an empty Namespace matches any, and a model
	// found in two namespaces is an error.
	Namespace, ModelID string
	// Cycle is the model's cycle to stop after, counting its saturation
	// Analyze calls from 1; 0 is its last.
	Cycle int
}

// QueryError is a StateQuery the recording cannot answer: a model it does
// not hold or names ambiguously, or a cycle past its last. It is the
// caller's question that is wrong, not the replay: every other error of
// StateAt is a recording that did not replay.
type QueryError struct{ msg string }

func (e *QueryError) Error() string { return "learnreplay: " + e.msg }

func queryErrorf(format string, args ...any) error {
	return &QueryError{msg: fmt.Sprintf(format, args...)}
}

// State is what the analyzers had learned of one model when the replay
// stopped.
type State struct {
	Namespace, ModelID string
	// Cycle is the model's cycle the replay stopped after, and Cycles how
	// many the recording holds.
	Cycle, Cycles int
	// FirstAt is the clock of the model's first cycle and At that of Cycle.
	FirstAt, At time.Time
	// Policy is the scaling policy the cycle was analyzed under; nil when
	// the recorded config was no policy.
	Policy *config.ScalingPolicy
	// Variants is each variant the cycle priced, by name.
	Variants []VariantState
	// TPOTWindows is the router's per-request TPOT histogram of every cycle
	// up to Cycle that recorded one, oldest first, as bucket rates: the
	// input to whatif.TPOTRatioFromWindows. Empty in a recording made before
	// the recorder carried it.
	TPOTWindows []whatif.TPOTWindow
	// Prefill is each prefill variant at the cycle, by name, as a TTFT
	// question reads it (state_ttft.go).
	Prefill []PrefillState
	// FlowControlSeries says the router's flow-control queue series exists for
	// the model at the cycle, by S1's rule (domain.AnalyzerInput.FlowControlSeries).
	// False is no queue series, which a scrape gap reads as too.
	FlowControlSeries bool
}

// VariantState is one variant's learned state at the cycle: everything the
// decode planner prices from.
type VariantState struct {
	Variant, Role string
	// Replicas is the variant's replica count as the cycle priced it.
	Replicas int
	// Line is the ITL line the variant was priced on, zero with none; Tier
	// is where it came from (whatif.TierOLS and the rest, "" with no line)
	// and BorrowedFrom the donor of a borrowed one (namespace/variant).
	Line         itl.Model
	Tier         string
	BorrowedFrom string
	// PricingKey is the ITL key the line was fitted under, "" for a line
	// the variant did not fit, and Readings that key's windows.
	PricingKey string
	Readings   itl.KeyReadings
	// LineReadings is the readings the line was fitted from, found by
	// making the tracker's choice of fit again on Readings (itl.Refit, or
	// itl.RefitAboveBand for an above-band line); Refitted says the refit
	// gave the line back. A line it does not give back (one priced per
	// token or by shape, say) has none, and is not banded.
	LineReadings []itl.Observation
	Refitted     bool
	// Baseline is the intercept the tracker's pinned fit pins for the key,
	// and MinSettled the steady gate's MinSettledSamples: what a refit takes.
	Baseline   float64
	MinSettled int
	// OneSided is how the line was priced one-sided
	// (itl.Admission.OneSidedAboveKSat, issue #467), the zero Admission for
	// a line that was not: priced at the prior on B, B unlearned and
	// Baseline itl.DefaultBaselineSec. A refit takes it (whatif.Refit.OneSided)
	// to price the line the controller priced. analyzerOneSided is the
	// admission the analyzer last priced the key with, and analyzerRaised
	// whether that line was priced at the prior: refit takes the admission on
	// a raised line only.
	OneSided         itl.Admission
	analyzerOneSided itl.Admission
	analyzerRaised   bool
	// CapacityTokens is C, the replica's whole KV capacity
	// (whatif.CapacityTokens).
	CapacityTokens float64
	// BatchCap is the cap per engine rank the analyzer prices the variant
	// at, BatchCapSource where it came from (flags, declared, learned,
	// default; "" with none), and Ranks the engine ranks per replica.
	BatchCap       int64
	BatchCapSource string
	Ranks          int
	// Shape is the traffic the variant served (shapeOf).
	Shape MeasuredShape
	// ReadingsFrom and ReadingsTo are the first and the last cycle of the
	// LineReadings, 0 with none.
	ReadingsFrom, ReadingsTo int
	// DecodeTTFT is what the variant's rows say of a TTFT's decode side; the
	// zero value on a variant that is not decode (state_ttft.go).
	DecodeTTFT DecodeTTFTState
}

// MeasuredShape is the shape a variant served over the cycles it had
// traffic in.
type MeasuredShape struct {
	// InputTokens, OutputTokens and PrefixHitRate are the means.
	InputTokens, OutputTokens, PrefixHitRate float64
	// FromCycle and ToCycle are the first and the last cycle they were
	// taken over, and Cycles how many of the cycles between had traffic;
	// all 0 when no cycle up to the one asked had any.
	FromCycle, ToCycle, Cycles int
}

// shapeCycles is how many of a variant's latest cycles with traffic its
// shape is taken over: five minutes at the default 15 s cycle.
const shapeCycles = 20

// shapeSample is one cycle's shape of a variant, and its request rate.
type shapeSample struct {
	cycle            int
	il, ol, hit, req float64
}

// StateAt replays recording through the analyzers (in Normal mode, whatever
// opts.Mode says) and returns the state of the model q names after q's
// cycle. A *QueryError is a model the recording does not hold or names
// ambiguously, or a cycle past its last; any other error is a recording
// that does not replay.
//
// The model and its cycle are resolved first, from the recording's
// saturation Analyze events alone, so the last cycle is a cycle number like
// any other: the replay stops there and the state is read once.
func StateAt(ctx context.Context, recording []byte, opts Options, q StateQuery) (*State, error) {
	if q.Cycle < 0 {
		return nil, queryErrorf("cycle %d, want 1 or more, or 0 for the last", q.Cycle)
	}
	ns, model, total, err := resolveModel(recording, q)
	if err != nil {
		return nil, err
	}
	cycle := q.Cycle
	if cycle == 0 {
		cycle = total
	}
	if cycle > total {
		return nil, queryErrorf("cycle %d asked for, and the recording holds %d of %s/%s", cycle, total, ns, model)
	}
	opts.Mode = Normal
	var (
		st       *State
		stateErr error
		n        int
		firstAt  time.Time
		windows  []whatif.TPOTWindow
		shapes   = map[string][]shapeSample{}
		ttft     = newTTFTSamples()
		cycleAt  = map[time.Time]int{}
	)
	opts.stop = func(r *replayer, ev learnrec.RawEvent) bool {
		if ev.Kind != saturation.KindAnalyze {
			return false
		}
		var d saturation.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			stateErr = err
			return true
		}
		in, err := decodeInput(d.Input)
		if err != nil {
			stateErr = err
			return true
		}
		if in.Namespace != ns || in.ModelID != model {
			return false
		}
		n++
		if n == 1 {
			firstAt = ev.Now
		}
		cycleAt[ev.Now] = n
		if w, ok := tpotWindow(in.RouterLatency); ok {
			windows = append(windows, w)
		}
		noteShapes(shapes, in.ReplicaMetrics, n)
		ttft.note(in, n)
		if n == cycle {
			st = r.stateOf(in, n, ev.Now, shapes)
			r.fillTTFT(st, in, ttft)
			return true
		}
		return false
	}
	if _, err := Replay(ctx, recording, opts); err != nil {
		return nil, err
	}
	if stateErr != nil {
		return nil, fmt.Errorf("learnreplay: %w", stateErr)
	}
	if st == nil {
		return nil, fmt.Errorf("learnreplay: the replay ended after %d of the %d cycles of %s/%s", n, cycle, ns, model)
	}
	st.FirstAt, st.TPOTWindows, st.Cycles = firstAt, windows, total
	for i := range st.Variants {
		st.Variants[i].ReadingsFrom, st.Variants[i].ReadingsTo = cyclesSpanned(st.Variants[i].LineReadings, cycleAt)
	}
	return st, nil
}

// resolveModel is the model q names and how many saturation cycles the
// recording holds of it, read from its Analyze events without replaying
// them.
func resolveModel(recording []byte, q StateQuery) (ns, model string, cycles int, err error) {
	read, err := learnrec.Read(recording)
	if err != nil {
		return "", "", 0, err
	}
	counts := map[[2]string]int{}
	for _, ev := range read.Events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		// Two fields of the input are all the scan reads: encoding/json skips
		// the rest, where learnrec's own decoder would hold every field to
		// the type (the replay decodes the event in full).
		var d struct {
			Input struct{ Namespace, ModelID string }
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return "", "", 0, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		id := d.Input
		if q.ModelID != "" && id.ModelID != q.ModelID || q.Namespace != "" && id.Namespace != q.Namespace {
			continue
		}
		counts[[2]string{id.Namespace, id.ModelID}]++
	}
	keys := make([]string, 0, len(counts))
	for k, c := range counts {
		ns, model, cycles = k[0], k[1], c
		keys = append(keys, k[0]+"/"+k[1])
	}
	slices.Sort(keys)
	switch {
	case len(keys) == 0 && q.ModelID == "":
		return "", "", 0, queryErrorf("the recording holds no saturation cycle")
	case len(keys) == 0:
		return "", "", 0, queryErrorf("the recording holds no cycle of model %q", q.ModelID)
	case len(keys) > 1:
		return "", "", 0, queryErrorf("the recording holds %s; name one with its namespace and model", strings.Join(keys, ", "))
	}
	return ns, model, cycles, nil
}

// cyclesSpanned is the first and the last cycle readings were taken at: a
// reading is stamped with the clock of the Analyze that offered it.
func cyclesSpanned(readings []itl.Observation, cycleAt map[time.Time]int) (from, to int) {
	for _, o := range readings {
		c, ok := cycleAt[o.Timestamp]
		if !ok {
			continue
		}
		if from == 0 || c < from {
			from = c
		}
		to = max(to, c)
	}
	return from, to
}

// noteShapes keeps, per variant, the shape of each of its latest
// shapeCycles cycles with traffic: a row serving requests, or generating
// tokens, at the cycle.
func noteShapes(shapes map[string][]shapeSample, rows []domain.ReplicaMetrics, cycle int) {
	type acc struct{ n, il, ol, hit, req float64 }
	byVariant := map[string]*acc{}
	traffic := map[string]bool{}
	for _, rm := range rows {
		a := byVariant[rm.VariantName]
		if a == nil {
			a = &acc{}
			byVariant[rm.VariantName] = a
		}
		if rm.RunningRequests > 0 || rm.RequestRate > 0 || rm.GenerationTokenRate > 0 {
			traffic[rm.VariantName] = true
		}
		if rm.AvgOutputTokens > 0 {
			a.n++
			a.il += rm.AvgInputTokens
			a.ol += rm.AvgOutputTokens
			a.hit += rm.PrefixCacheHitRate
		}
		if rm.RequestRateKnown && rm.RequestRate > 0 {
			a.req += rm.RequestRate
		}
	}
	for v, a := range byVariant {
		if !traffic[v] || a.n == 0 {
			continue
		}
		s := shapes[v]
		s = append(s, shapeSample{cycle: cycle, il: a.il / a.n, ol: a.ol / a.n, hit: a.hit / a.n, req: a.req})
		if len(s) > shapeCycles {
			s = s[len(s)-shapeCycles:]
		}
		shapes[v] = s
	}
}

// shapeOf is a variant's measured shape: the mean of its latest shapeCycles
// cycles with traffic, each weighted by its request rate (equally when no
// cycle read one). The rows' own lengths are means over a window already;
// weighting by rate keeps a cycle of a light trickle from counting as much
// as one at load, and taking cycles with traffic only means a recording
// that ends idle is priced at the traffic it last served rather than at
// none.
func shapeOf(samples []shapeSample) MeasuredShape {
	if len(samples) == 0 {
		return MeasuredShape{}
	}
	var total float64
	for _, s := range samples {
		total += s.req
	}
	var out MeasuredShape
	for _, s := range samples {
		w := 1 / float64(len(samples))
		if total > 0 {
			w = s.req / total
		}
		out.InputTokens += w * s.il
		out.OutputTokens += w * s.ol
		out.PrefixHitRate += w * s.hit
	}
	out.FromCycle, out.ToCycle, out.Cycles = samples[0].cycle, samples[len(samples)-1].cycle, len(samples)
	return out
}

// tpotWindow is the router's per-request TPOT histogram of one cycle, as the
// what-if library reads it: bucket rates for counts, which give the same
// quantile and mean.
func tpotWindow(rl *domain.RouterLatency) (whatif.TPOTWindow, bool) {
	if rl == nil || len(rl.TPOTBuckets) == 0 {
		return whatif.TPOTWindow{}, false
	}
	w := whatif.TPOTWindow{Sum: rl.TPOTSumRate, Count: rl.TPOTCountRate,
		Buckets: make([]whatif.HistogramBucket, len(rl.TPOTBuckets))}
	for i, b := range rl.TPOTBuckets {
		w.Buckets[i] = whatif.HistogramBucket{UpperEdge: b.UpperBound, Count: b.Rate}
	}
	return w, true
}

// stateOf reads the analyzers' learned state of the model in after its
// saturation Analyze.
func (r *replayer) stateOf(in domain.AnalyzerInput, cycle int, now time.Time, shapes map[string][]shapeSample) *State {
	st := &State{Namespace: in.Namespace, ModelID: in.ModelID, Cycle: cycle, At: now}
	st.Policy, _ = in.Config.(*config.ScalingPolicy)
	res := r.results[resultKey(domain.SaturationAnalyzerName, in.Namespace, in.ModelID)]
	if res == nil {
		return st
	}
	reports := map[string]learn.Report{}
	for _, rep := range r.sat.LearnerReports()[in.Namespace+"|"+in.ModelID] {
		if rep.Learner == learn.ITL {
			reports[rep.Variant] = rep
		}
	}
	learning := st.Policy != nil && st.Policy.EnableBatchCapLearning
	minSettled := st.Policy.ITLSteadiness().MinSettledSamples
	for _, vc := range res.VariantCapacities {
		vs := VariantState{Variant: vc.VariantName, Role: vc.Role, Replicas: vc.ReplicaCount, MinSettled: minSettled}
		if sr := vc.ServiceRate; sr != nil && sr.HasLine {
			vs.Line = itl.Model{A: sr.LineSlope, B: sr.LineIntercept}
		}
		for _, b := range vc.Borrowed {
			if b.Quantity == domain.BorrowedITLLine {
				vs.BorrowedFrom = b.DonorNamespace + "/" + b.Donor
			}
		}
		rep := reports[vc.VariantName]
		if !vs.Line.IsZero() {
			vs.Tier = saturation.ITLTier(rep.Source, vs.BorrowedFrom != "")
		}
		if rep.PricingKey != "" {
			vs.PricingKey = rep.PricingKey
			vs.Readings = r.sat.ITLReadings(rep.PricingKey)
			vs.analyzerOneSided, vs.analyzerRaised = r.sat.ITLOneSided(rep.PricingKey)
		}
		vs.refit()
		var params *capacity.EngineParams
		var live int64
		if rec := r.store.Get(in.Namespace, in.ModelID, vc.VariantName); rec != nil {
			if rec.EngineParams != nil {
				p := *rec.EngineParams
				params = &p
			}
			live = rec.TotalKvCapacityTokens
		}
		for _, rm := range in.ReplicaMetrics {
			if rm.VariantName == vc.VariantName {
				vs.Ranks = max(vs.Ranks, rm.EngineRanks)
			}
		}
		if vs.Ranks < 1 {
			vs.Ranks = 1
			if params != nil && params.DataParallelSize > 1 {
				vs.Ranks = params.DataParallelSize
			}
		}
		vs.Shape = shapeOf(shapes[vc.VariantName])
		vs.CapacityTokens = whatif.CapacityTokens(params, vs.Ranks, live)
		vs.BatchCap, vs.BatchCapSource = r.sat.PricingBatchCap(in.Namespace, in.ModelID, vc.VariantName, params, learning)
		st.Variants = append(st.Variants, vs)
	}
	slices.SortFunc(st.Variants, func(a, b VariantState) int { return strings.Compare(a.Variant, b.Variant) })
	return st
}

// sameLine reports two lines equal to a part in a billion: a refit of the
// readings a line was fitted from gives it back to the last bits.
func sameLine(a, b itl.Model) bool {
	near := func(x, y float64) bool { return math.Abs(x-y) <= 1e-9*max(math.Abs(x), math.Abs(y)) }
	return near(a.A, b.A) && near(a.B, b.B)
}

// refit finds the readings the line was fitted from, by making the tracker's
// choice of fit again on the key's windows: in the band, at the line's B,
// and, when that does not give the line back, the above-band fallback, which
// marks the line TierAboveBand. A held or borrowed line was fitted from no
// readings of the variant's own here.
//
// A pinned line the analyzer priced at the prior on B (issue #467; the
// recorded itl.Reading.PinnedAtBPrior) is refit with the analyzer's
// one-sided admission, which the state keeps for the bands (OneSided), so a
// resample prices as the controller did.
func (vs *VariantState) refit() {
	if vs.Tier != whatif.TierOLS && vs.Tier != whatif.TierPinnedB {
		return
	}
	vs.Baseline = vs.Line.B
	var adm itl.Admission
	if vs.Tier == whatif.TierPinnedB && vs.analyzerRaised {
		adm = vs.analyzerOneSided
	}
	if got := itl.Refit(vs.Readings.All, vs.Readings.Steady, vs.Baseline, vs.MinSettled, adm); got.Tier != "" && sameLine(got.Model, vs.Line) {
		vs.Tier, vs.LineReadings, vs.Refitted, vs.OneSided = got.Tier, got.Read, true, adm
		return
	}
	if got := itl.RefitAboveBand(vs.Readings.Above, vs.Readings.AboveSteady, vs.MinSettled); got.Tier != "" && sameLine(got.Model, vs.Line) {
		vs.Tier, vs.LineReadings, vs.Refitted = whatif.TierAboveBand, got.Read, true
	}
}
