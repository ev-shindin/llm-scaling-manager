package learnreplay

import (
	"maps"
	"slices"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/prefill"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
)

// What a recording says of a TTFT question at a cycle: the what-if planner's
// TTFT half (docs/proposals/slo-capacity-planner.md, "Recording mode, as
// built"). P1's estimate is read from the saturation analyzer's stash, the
// same handle the slo analyzer reads it through, and the rest from the
// cycles' rows by the rules of docs/proposals/slo-tiers.md, "S1 decisions".
// All of it is read, none of it is fed back: the analyzers run as they do
// without it, so it needs no replay switch.

// PrefillState is one prefill variant at the cycle, as a TTFT question reads
// it.
type PrefillState struct {
	Variant string
	// Pods is N_p, the serving pods at the cycle (prefill.Serving), the rule
	// P1 counts its per-count f by.
	Pods int
	// Estimate is P1's estimate of the variant, written at this cycle;
	// EstimateRead is false when none was (the observation was off, or the
	// cycle gave it no rows).
	Estimate     prefill.Estimate
	EstimateRead bool
	// PromptTokens is E[I_eff], computed tokens over requests, over the
	// variant's latest shapeCycles cycles with traffic, so that a recording
	// that ends idle is priced at the traffic it last served; 0 with none.
	// PromptFromCycle, PromptToCycle and PromptCycles say which cycles.
	PromptTokens                                 float64
	PromptFromCycle, PromptToCycle, PromptCycles int
	// RawShare is f_raw, the busiest pod's share of the computed-token rate
	// over the mean share, not de-biased: the largest over the latest
	// whatif.TTFTWindowCycles cycles that read one at the cycle's pod count. A pod with
	// no rate counts as a pod that computed nothing. 0 with none, or one pod.
	// RawShareLeast is the window's least, and RawShareFull says it holds
	// whatif.TTFTWindowCycles readings: what the routing-bound flag reads. The
	// slo-tiers rule reads settled rows; here a pod still warming up reads
	// low, which raises the share, the side that rule permits.
	RawShare, RawShareLeast float64
	RawShareFull            bool
}

// DecodeTTFTState is what a decode variant's rows say of the decode side of
// a TTFT, read from its serving rows.
type DecodeTTFTState struct {
	// EngineTTFT is the decode engine's TTFT: of the latest whatif.TTFTWindowCycles
	// cycles, the largest, each a mean over the rows weighted by their
	// request rate, with the mean effective prompt of the same cycle (the
	// pair is never split). A cycle with no reading holds an empty slot, so a
	// figure ages out when the rows stop carrying one. Zero when unread.
	EngineTTFT whatif.DecodeTTFT
	// TransferSeconds is the mean KV transfer time over the rows that read
	// one, over the latest shapeCycles cycles with a reading; 0 with none.
	TransferSeconds float64
}

// promptSample is one cycle's computed tokens and requests of a prefill
// variant, per second.
type promptSample struct {
	cycle             int
	computed, request float64
}

// shareWindow is a prefill variant's raw shares at its current pod count. A
// new count restarts it; a cycle that reads no share leaves it as it is, so
// an idle tail forgets nothing.
type shareWindow struct {
	pods   int
	shares []float64
}

// note folds in a cycle's share at pods.
func (w *shareWindow) note(pods int, share float64, ok bool) {
	if w.pods != pods {
		w.pods, w.shares = pods, nil
	}
	if ok {
		w.shares = keepLast(w.shares, share, whatif.TTFTWindowCycles)
	}
}

// read is the window's largest and least, and whether it is full.
func (w *shareWindow) read() (largest, least float64, full bool) {
	for i, s := range w.shares {
		largest = max(largest, s)
		if i == 0 || s < least {
			least = s
		}
	}
	return largest, least, len(w.shares) == whatif.TTFTWindowCycles
}

// ttftSamples is the windows of the rows a TTFT question reads, by variant.
type ttftSamples struct {
	prompts    map[string][]promptSample
	shares     map[string]*shareWindow
	engineTTFT map[string][]whatif.DecodeTTFT
	transfers  map[string][]float64
}

func newTTFTSamples() *ttftSamples {
	return &ttftSamples{prompts: map[string][]promptSample{}, shares: map[string]*shareWindow{},
		engineTTFT: map[string][]whatif.DecodeTTFT{}, transfers: map[string][]float64{}}
}

// keepLast is s with v appended, holding the last n.
func keepLast[T any](s []T, v T, n int) []T {
	s = append(s, v)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// rolesOf is each variant's role, as the cycle states it.
func rolesOf(in domain.AnalyzerInput) map[string]string {
	roles := map[string]string{}
	for _, vs := range in.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}

// decodeRows is one decode variant's readings in one cycle.
type decodeRows struct {
	ttftWeighted, ttftWeight, ttftPlain float64
	ttftRows                            int
	transfer                            float64
	transferRows                        int
}

// prefillRows is one prefill variant's readings in one cycle.
type prefillRows struct {
	pods                  int
	sum, busiest          float64
	computedRate, reqRate float64
}

// positive reports a reading above 0 that is finite: a zero is no reading of
// a latency, and pulls a mean low.
func positive(x float64) bool { return whatif.Finite(x) && x > 0 }

// note takes one cycle's rows.
func (t *ttftSamples) note(in domain.AnalyzerInput, cycle int) {
	roles := rolesOf(in)
	pre := map[string]*prefillRows{}
	decode := map[string]*decodeRows{}
	for v, role := range roles {
		switch domain.CanonicalRole(role) {
		case domain.RolePrefill:
			pre[v] = &prefillRows{}
		case domain.RoleDecode:
			decode[v] = &decodeRows{}
		}
	}
	for _, rm := range in.ReplicaMetrics {
		if !prefill.Serving(rm) {
			continue
		}
		v := rm.VariantName
		if p := pre[v]; p != nil {
			p.pods++
			if !rm.PrefillComputedTokenRateKnown || !positive(rm.PrefillComputedTokenRate) {
				continue
			}
			p.sum += rm.PrefillComputedTokenRate
			p.busiest = max(p.busiest, rm.PrefillComputedTokenRate)
			if rm.RequestRateKnown && rm.RequestRate > 0 {
				p.computedRate += rm.PrefillComputedTokenRate
				p.reqRate += rm.RequestRate
			}
			continue
		}
		d := decode[v]
		if d == nil {
			continue
		}
		if rm.AvgTTFTKnown && positive(rm.AvgTTFT) {
			d.ttftRows++
			d.ttftPlain += rm.AvgTTFT
			if rm.RequestRateKnown && rm.RequestRate > 0 {
				d.ttftWeighted += rm.AvgTTFT * rm.RequestRate
				d.ttftWeight += rm.RequestRate
			}
		}
		if rm.AvgKVTransferTimeKnown && positive(rm.AvgKVTransferTime) {
			d.transfer += rm.AvgKVTransferTime
			d.transferRows++
		}
	}
	var allComputed, allRequests float64
	for v, p := range pre {
		w := t.shares[v]
		if w == nil {
			w = &shareWindow{}
			t.shares[v] = w
		}
		share, ok := rawShare(p.pods, p.sum, p.busiest)
		w.note(p.pods, share, ok)
		if p.computedRate > 0 {
			allComputed += p.computedRate
			allRequests += p.reqRate
			t.prompts[v] = keepLast(t.prompts[v], promptSample{cycle: cycle, computed: p.computedRate, request: p.reqRate}, shapeCycles)
		}
	}
	for v, d := range decode {
		if d.transferRows > 0 {
			t.transfers[v] = keepLast(t.transfers[v], d.transfer/float64(d.transferRows), shapeCycles)
		}
		var slot whatif.DecodeTTFT
		if d.ttftRows > 0 && allComputed > 0 {
			slot = whatif.DecodeTTFT{Seconds: d.ttftPlain / float64(d.ttftRows), PromptTokens: allComputed / allRequests}
			if d.ttftWeight > 0 {
				slot.Seconds = d.ttftWeighted / d.ttftWeight
			}
		}
		t.engineTTFT[v] = keepLast(t.engineTTFT[v], slot, whatif.TTFTWindowCycles)
	}
}

// rawShare is one cycle's f_raw: the busiest of pods over the mean, a pod with
// no rate counting as one that computed nothing, which reads high, the
// permitted side. False with fewer than two pods or no rate on any.
func rawShare(pods int, sum, busiest float64) (float64, bool) {
	if pods < 2 || !(sum > 0) {
		return 0, false
	}
	return busiest * float64(pods) / sum, true
}

// promptOf is a prefill variant's mean effective prompt over its samples,
// and the cycles they span: each cycle weighs by its requests, which is
// computed tokens over requests across them.
func promptOf(samples []promptSample) (tokens float64, from, to, cycles int) {
	if len(samples) == 0 {
		return 0, 0, 0, 0
	}
	var computed, requests float64
	for _, s := range samples {
		computed += s.computed
		requests += s.request
	}
	return computed / requests, samples[0].cycle, samples[len(samples)-1].cycle, len(samples)
}

// fillTTFT sets what the state says of a TTFT question, from the samples
// through the cycle in and from P1's estimates.
func (r *replayer) fillTTFT(st *State, in domain.AnalyzerInput, t *ttftSamples) {
	roles := rolesOf(in)
	pods := map[string]int{}
	for _, rm := range in.ReplicaMetrics {
		if prefill.Serving(rm) {
			pods[rm.VariantName]++
		}
	}
	for _, v := range slices.Sorted(maps.Keys(roles)) {
		if domain.CanonicalRole(roles[v]) != domain.RolePrefill {
			continue
		}
		ps := PrefillState{Variant: v, Pods: pods[v]}
		if w := t.shares[v]; w != nil {
			ps.RawShare, ps.RawShareLeast, ps.RawShareFull = w.read()
		}
		ps.PromptTokens, ps.PromptFromCycle, ps.PromptToCycle, ps.PromptCycles = promptOf(t.prompts[v])
		ps.Estimate, ps.EstimateRead = estimateAt(r.sat.PrefillEstimate, in.Namespace, in.ModelID, v, st.At)
		st.Prefill = append(st.Prefill, ps)
	}
	for i := range st.Variants {
		vs := &st.Variants[i]
		vs.DecodeTTFT.EngineTTFT = largestTTFT(t.engineTTFT[vs.Variant])
		if xs := t.transfers[vs.Variant]; len(xs) > 0 {
			sum := 0.0
			for _, x := range xs {
				sum += x
			}
			vs.DecodeTTFT.TransferSeconds = sum / float64(len(xs))
		}
	}
	st.FlowControlSeries = in.FlowControlSeries()
}

// estimateAt is a variant's estimate read through read, when it was stashed
// at now: one left from an earlier cycle, which did observe the variant, is
// not this cycle's.
func estimateAt(read func(namespace, modelID, variant string) (prefill.Estimate, time.Time, bool),
	namespace, modelID, variant string, now time.Time) (prefill.Estimate, bool) {
	est, at, ok := read(namespace, modelID, variant)
	if !ok || !at.Equal(now) {
		return prefill.Estimate{}, false
	}
	return est, true
}

// largestTTFT is the sample with the largest TTFT, as a pair: S1 takes the
// largest and never the largest of each.
func largestTTFT(samples []whatif.DecodeTTFT) whatif.DecodeTTFT {
	var top whatif.DecodeTTFT
	for _, d := range samples {
		if d.Seconds > top.Seconds {
			top = d
		}
	}
	return top
}
