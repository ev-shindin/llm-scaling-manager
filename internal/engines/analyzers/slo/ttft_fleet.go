package slo

import (
	"math"
	"slices"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/prefill"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
)

// What the TTFT model is given for one prefill variant
// (docs/proposals/slo-tiers.md, S1 decisions 1 to 3). One rule runs through
// it: only err toward more replicas. An unknown input takes the value that
// asks for more, and is flagged.

// The analyzer's own flags, added to the TTFT model's on an answer.
const (
	// AssumedPoissonArrivals is every S1 answer: arrivals are taken as
	// Poisson, which over-promises on bursty traffic.
	AssumedPoissonArrivals whatif.Flag = "AssumedPoissonArrivals"
	// PromptSpreadClamped is a measured prompt spread with mass past the
	// histogram's last bound, or above whatif.PromptCVStop: priced at that stop.
	PromptSpreadClamped whatif.Flag = "PromptSpreadClamped"
	// FlowControlMismatch is a declared slo.flowControl the router's series
	// contradict: priced as declared.
	FlowControlMismatch whatif.Flag = "FlowControlMismatch"
	// PromptSpreadAbovePeak is a cv, assumed or measured, above whatif.PromptCVPeak
	// for the objective's percentile: the prompt's own tail shrinks as cv
	// rises from there, so a larger cv prices a shorter floor and can price a
	// higher rate. On S2's no-act list.
	PromptSpreadAbovePeak whatif.Flag = "PromptSpreadAbovePeak"
	// ImbalanceNotSustained is an f at or above whatif.RoutingBoundShare of the pods
	// that has not held over the whole window: chance alone can read that at
	// two pods. Priced at it, the side that asks for more replicas, and not
	// routing-bound.
	ImbalanceNotSustained whatif.Flag = "ImbalanceNotSustained"
)

// Where f came from, on the log line.
const (
	fSourceLearned = "learned"
	fSourceOnePod  = "one-pod"
	fSourceAssumed = "assumed"
)

// Where the prompt's spread came from, on the log line.
const (
	cvSourceMeasured = "measured"
	cvSourceAssumed  = "assumed"
)

// ttftInputs is one prefill variant's figures of the cycle: everything
// ttftFleet prices from, so it is a pure function of the pod count.
type ttftInputs struct {
	// est is P1's estimate, base what the model takes of it
	// (whatif.PrefillFromEstimate).
	est  prefill.Estimate
	base whatif.Prefill
	// estimateAt is when the estimate was stashed; zero with none this cycle.
	estimateAt time.Time
	// pods is N_p, the variant's Ready replicas (prefill.Serving).
	pods int
	// otherPods is the Ready replicas of the model's other prefill variants.
	// The decode role takes the model's whole rate, whichever prefill variant
	// sent it, so a variant is priced at that rate, with its share of it
	// expressed as the replicas that rate spreads over: pods + otherPods at
	// this variant's V_P load each of its pods as its own share would.
	otherPods int
	// percent is the objective's percentile.
	percent int
	// shares is the raw share window.
	shares shareWindow
	// prompt is E[I_eff] and spread its coefficient of variation.
	prompt float64
	spread promptSpread
	// transfer is the KV transfer per prompt token, seconds; 0 unmeasured.
	transfer float64
	// decode is the decode side; nil with no decode line.
	decode *whatif.Decode
	// decodeVariant names the variant behind decode, for the log.
	decodeVariant string
	decodeTTFT    whatif.DecodeTTFT
	flow          whatif.FlowControl
	mismatch      bool
}

// imbalance is f at pods, whether it is assumed, and where it came from.
//
// At the current count: P1's f at that count, from minutes taken at it only;
// with one pod, none (the model prices one pod as 1 exactly); otherwise
// assumed, min(N_p, max(1.5, f_raw)). At another count: min(N', max(1.5, f)),
// so a learned f is never carried unchanged to a new count, and one pod's
// exact 1 never reaches two.
func (in ttftInputs) imbalance(pods int) (f float64, assumed bool, source string) {
	f, assumed, source = in.imbalanceNow()
	if pods == in.pods {
		return f, assumed, source
	}
	f = min(float64(pods), max(whatif.ImbalanceFloor, f))
	return f, pods > 1, fSourceAssumed
}

// imbalanceNow is f at the current count, whether it is assumed, and where it
// came from: P1's f at that count; none on one pod; otherwise the assumed f.
func (in ttftInputs) imbalanceNow() (float64, bool, string) {
	if f, ok := in.est.FByPods[in.pods]; ok {
		return f, false, fSourceLearned
	}
	if in.pods <= 1 {
		return 0, false, fSourceOnePod
	}
	return min(float64(in.pods), max(whatif.ImbalanceFloor, in.shares.largest)), true, fSourceAssumed
}

// routing reads f at the current count against whatif.RoutingBoundShare of the pods:
// bound when it holds, which a learned f has shown over its minutes and a raw
// share must show over the whole window; transient when f reads there and has
// not held. One pod has no router to blame: f / N_p is 1 by construction.
func (in ttftInputs) routing() (bound, transient bool) {
	f, _, source := in.imbalanceNow()
	pods := float64(in.pods)
	if in.pods < 2 || f/pods < whatif.RoutingBoundShare {
		return false, false
	}
	if source == fSourceLearned || in.shares.full && in.shares.least/pods >= whatif.RoutingBoundShare {
		return true, false
	}
	return false, true
}

// ttftFleet is the fleet the TTFT model prices at pods prefill replicas, and
// the analyzer's own flags on it. S1 calls it at the current count; S2's
// search calls it at each other count.
func ttftFleet(in ttftInputs, pods int) (whatif.TTFTFleet, []whatif.Flag) {
	p := in.base
	p.Imbalance, p.ImbalanceAssumed, _ = in.imbalance(pods)
	if pods > 0 {
		p.Replicas = pods + in.otherPods
	}
	// One pod's exact f, with other variants' pods beside it in Replicas.
	if p.Imbalance == 0 && p.Replicas > 1 {
		p.Imbalance = 1
	}
	p.PromptTokens = in.prompt
	p.PromptCV, p.PromptCVAssumed = in.spread.cv, in.spread.assumed
	p.TransferSecondsPerToken = in.transfer
	fl := whatif.TTFTFleet{Prefill: p, Decode: in.decode, FlowControl: in.flow}
	if in.decode == nil {
		fl.DecodeTTFT = in.decodeTTFT
	}
	flags := []whatif.Flag{AssumedPoissonArrivals}
	if in.spread.clamped {
		flags = append(flags, PromptSpreadClamped)
	}
	if in.mismatch {
		flags = append(flags, FlowControlMismatch)
	}
	if in.spread.cv > whatif.PromptCVPeak(float64(in.percent)/100) {
		flags = append(flags, PromptSpreadAbovePeak)
	}
	if _, transient := in.routing(); transient && pods == in.pods {
		flags = append(flags, ImbalanceNotSustained)
	}
	return fl, flags
}

// flowControlState is what the tier declares, or else what the router's series
// say, and whether the two disagree. seriesSeen is whether the router's
// flow-control queue series exist for the model. Declared enabled with no
// series counts as a mismatch, the conservative reading; whether an on gate
// publishes its queue series while empty is not verified.
func flowControlState(p *config.SLOPolicy, seriesSeen bool) (whatif.FlowControl, bool) {
	on := whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: whatif.FlowControlQueueThreshold,
		KVThreshold: whatif.FlowControlKVThreshold}
	switch {
	case p != nil && p.FlowControl == config.SLOFlowControlEnabled:
		return on, !seriesSeen
	case p != nil && p.FlowControl == config.SLOFlowControlDisabled:
		return whatif.FlowControl{State: whatif.FlowControlOff}, seriesSeen
	case seriesSeen:
		return on, false
	}
	return whatif.FlowControl{State: whatif.FlowControlNoSeries}, false
}

// promptSpread is the coefficient of variation of the effective prompt, and
// how it was had.
type promptSpread struct {
	cv float64
	// assumed is no histogram read; clamped is PromptSpreadClamped.
	assumed, clamped bool
}

// readPromptSpread reads the spread from the computed-token histogram of the
// variant's serving rows, pooled: each bucket's rate is summed over the pods
// and cv computed once. mean is the exact E[I_eff].
//
// The second moment uses each bucket's upper bound, so cv reads high, never
// low: that costs replicas, not safety. Mass past the last finite bound has no
// upper bound, so cv is the top and flagged, as is a cv above it. With no
// histogram it is whatif.AssumedPromptCV, flagged by the model.
func readPromptSpread(rows []domain.ReplicaMetrics, mean float64) promptSpread {
	assumed := promptSpread{cv: whatif.AssumedPromptCV, assumed: true}
	pooled := map[float64]float64{}
	for _, rm := range rows {
		if !prefill.Serving(rm) {
			continue
		}
		for _, b := range rm.ComputedTokensBuckets {
			if !math.IsNaN(b.UpperBound) && whatif.Finite(b.Rate) && b.Rate >= 0 {
				pooled[b.UpperBound] += b.Rate
			}
		}
	}
	total, ok := pooled[math.Inf(1)]
	if !ok || !(total > 0) || !(mean > 0) {
		return assumed
	}
	bounds := make([]float64, 0, len(pooled))
	for b := range pooled {
		if !math.IsInf(b, 1) {
			bounds = append(bounds, b)
		}
	}
	slices.Sort(bounds)
	var second, below float64
	for _, u := range bounds {
		// Rates over a window can dip between bounds; a dip is no mass.
		n := max(pooled[u]-below, 0)
		second += n * u * u
		below = max(below, pooled[u])
	}
	if total > below {
		return promptSpread{cv: whatif.PromptCVStop, clamped: true}
	}
	cv := math.Sqrt(max(second/total/(mean*mean)-1, 0))
	if !whatif.Finite(cv) || cv > whatif.PromptCVStop {
		return promptSpread{cv: whatif.PromptCVStop, clamped: true}
	}
	return promptSpread{cv: cv}
}

// rawShare is f_raw's reading of one cycle: the busiest pod's share of the
// variant's computed-token rate over the mean share, over its Ready, settled
// rows. A row with no rate counts as a pod that computed nothing, which reads
// high, the permitted side. False with fewer than two such rows or no rate on
// any.
func rawShare(rows []domain.ReplicaMetrics, view *prefill.Settle, now time.Time) (float64, bool) {
	var sum, busiest float64
	n := 0
	for _, rm := range rows {
		if !prefill.Serving(rm) || !view.Settled(rm, now) {
			continue
		}
		n++
		if r := rm.PrefillComputedTokenRate; rm.PrefillComputedTokenRateKnown && whatif.Finite(r) && r > 0 {
			sum += r
			busiest = max(busiest, r)
		}
	}
	if n < 2 || !(sum > 0) {
		return 0, false
	}
	return busiest * float64(n) / sum, true
}

// imbalanceWindow is a prefill variant's raw shares over the last
// whatif.TTFTWindowCycles cycles at its current Ready count.
type imbalanceWindow struct {
	pods   int
	shares []float64
	seen   time.Time
}

// shareWindow is what f reads of an imbalance window.
type shareWindow struct {
	// largest and least are over the window; 0 with none.
	largest, least float64
	// full is a window of whatif.TTFTWindowCycles readings.
	full bool
}

// noteShare folds the cycle's raw share into the variant's window, reset when
// the Ready count changes, and returns what it holds.
func (a *Analyzer) noteShare(key string, pods int, share float64, ok bool, now time.Time) shareWindow {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.imbalance[key]
	if w == nil || w.pods != pods {
		w = &imbalanceWindow{pods: pods}
		a.imbalance[key] = w
	}
	w.seen = now
	if ok {
		w.shares = append(w.shares, share)
		if len(w.shares) > whatif.TTFTWindowCycles {
			w.shares = w.shares[1:]
		}
	}
	out := shareWindow{full: len(w.shares) == whatif.TTFTWindowCycles}
	for i, s := range w.shares {
		out.largest = max(out.largest, s)
		if i == 0 || s < out.least {
			out.least = s
		}
	}
	return out
}

// decodeTTFTWindow is a decode variant's measured TTFT over the last
// whatif.TTFTWindowCycles cycles, each with the prompt of its cycle. A cycle with no
// reading holds an empty slot, so a figure ages out when the rows stop
// carrying one.
type decodeTTFTWindow struct {
	cycles []whatif.DecodeTTFT
	seen   time.Time
}

// noteDecodeTTFT folds the cycle's decode-engine TTFT into the variant's
// window and returns the largest, as a pair from one cycle; zero with none.
func (a *Analyzer) noteDecodeTTFT(key string, cycle whatif.DecodeTTFT, now time.Time) whatif.DecodeTTFT {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.decodeTTFT[key]
	if w == nil {
		w = &decodeTTFTWindow{}
		a.decodeTTFT[key] = w
	}
	w.seen = now
	w.cycles = append(w.cycles, cycle)
	if len(w.cycles) > whatif.TTFTWindowCycles {
		w.cycles = w.cycles[1:]
	}
	var largest whatif.DecodeTTFT
	for _, c := range w.cycles {
		if c.Seconds > largest.Seconds {
			largest = c
		}
	}
	return largest
}

// decodeEngineTTFT is the decode rows' mean AvgTTFT, weighted by request rate
// (equally when no row read one), and whether any row carries it. rows are one
// decode variant's.
func decodeEngineTTFT(rows []domain.ReplicaMetrics) (float64, bool) {
	var weighted, weight, sum float64
	n := 0
	for _, rm := range rows {
		if !prefill.Serving(rm) || !rm.AvgTTFTKnown || !whatif.Finite(rm.AvgTTFT) || !(rm.AvgTTFT > 0) {
			continue
		}
		sum += rm.AvgTTFT
		n++
		if rr := rm.RequestRate; whatif.Finite(rr) && rr > 0 {
			weighted += rr * rm.AvgTTFT
			weight += rr
		}
	}
	switch {
	case weight > 0:
		return weighted / weight, true
	case n > 0:
		return sum / float64(n), true
	}
	return 0, false
}

// transferPerToken is the mean AvgKVTransferTime over the serving rows that
// carry it, per token of the mean prompt; 0 where none does, or with no prompt.
func transferPerToken(rows []domain.ReplicaMetrics, prompt float64) float64 {
	var sum float64
	n := 0
	for _, rm := range rows {
		if prefill.Serving(rm) && rm.AvgKVTransferTimeKnown && whatif.Finite(rm.AvgKVTransferTime) && rm.AvgKVTransferTime > 0 {
			sum += rm.AvgKVTransferTime
			n++
		}
	}
	if n == 0 || !(prompt > 0) {
		return 0
	}
	return sum / float64(n) / prompt
}
