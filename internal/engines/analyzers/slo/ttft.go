package slo

import (
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/prefill"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// TTFT in shadow (S1 of docs/proposals/slo-tiers.md, "Phase 2: TTFT"). Each
// prefill variant of a P/D model whose policy sets a TTFT target is priced by
// the model in internal/signals/whatif: the TTFT predicted at the current rate,
// and the rate per replica at which the bounded TTFT meets the target. Both go
// to wva_slo_* series and the cycle log, and nothing else: the analyzer's result
// is unchanged, a prefill variant stays no-data, so no replica count moves.
// Aggregated variants wait for the what-if M1.
//
// The prediction is not a bound, and which way it errs depends on the prompt
// spread. On the prefill-stress recordings' light-load points, at p90 and p95
// (docs/proposals/slo-capacity-planner-ttft.md, "As built (P3 after P2)"): at
// the assumed cv 2 it reads high, over the router at 13 of 14, and at the
// spread the load generator sent it reads low, under at 9 of 14.
// constants.SLOTTFTPredictionCaveat has the worst of each, and a test holds it
// to the frozen table.

// SwitchTTFTPrice is the replay switch for the shadow TTFT pricing: on, each
// prefill variant is priced against the TTFT target; off, as before, none is
// and the snapshot's TTFT stays nil.
const SwitchTTFTPrice = "slo-ttft-price"

const (
	// decodeBoundRatio is how far the prefill side's own rate must exceed
	// the full rate for decode to count as setting it: any decode wait lowers
	// the full rate a little, and reading that as a hold would stop S2
	// growing prefill on almost every P/D fleet. Stated, not measured.
	decodeBoundRatio = 1.1
	// kneeTolerance is how close a rate must be to a knee to stand at it.
	kneeTolerance = 1e-9
)

// ttftReasons maps the model's refusals to the published reason, in kebab
// case. FlowControlUnknown is not among them: the thresholds are always passed.
var ttftReasons = map[whatif.Reason]string{
	whatif.NoPrefillCapacity: constants.SLOTTFTReasonNoPrefillCapacity,
	whatif.NoFixedLatency:    constants.SLOTTFTReasonNoFixedLatency,
	whatif.NoImbalance:       constants.SLOTTFTReasonNoImbalance,
	whatif.NoPrefillReplicas: constants.SLOTTFTReasonNoPrefillReplicas,
	whatif.NoDecodeReplicas:  constants.SLOTTFTReasonNoDecodeReplicas,
	whatif.NoCapacity:        constants.SLOTTFTReasonNoCapacity,
	whatif.NoBatchCap:        constants.SLOTTFTReasonNoBatchCap,
	whatif.AboveDecodeOnset:  constants.SLOTTFTReasonAboveDecodeOnset,
	whatif.Saturated:         constants.SLOTTFTReasonSaturated,
	whatif.TargetBelowFloor:  constants.SLOTTFTReasonTargetBelowFloor,
	whatif.NoTraffic:         constants.SLOTTFTReasonNoTraffic,
	whatif.TooManySlots:      constants.SLOTTFTReasonTooManySlots,
	whatif.InvalidInput:      constants.SLOTTFTReasonInputInvalid,
}

// ttftReason is the published reason of a refusal; input-invalid for one that
// has no reason of its own, so it never reads as priced.
func ttftReason(u whatif.Unanswerable) string {
	if r, ok := ttftReasons[u.Reason]; ok {
		return r
	}
	return constants.SLOTTFTReasonInputInvalid
}

// ttftPricing is one prefill variant's two answers, and what they publish.
type ttftPricing struct {
	reason string
	// rate is RateForTTFT's answer at the current count, which sets the
	// reason; at is TTFTAt's at the current rate, the shadow prediction. Each
	// is nil when the call was refused (rateRefused, atRefused) or not made.
	rate        *whatif.Answer
	rateRefused *whatif.Unanswerable
	at          *whatif.TTFT
	atRefused   *whatif.Unanswerable
	// predicted and perReplica are NaN when absent.
	predicted, perReplica float64
	unmet                 bool
	// f is the imbalance priced at.
	f     float64
	flags []whatif.Flag
}

// priceTTFT prices one variant at its current count and the model's whole
// rate (ttftInputs.otherPods says why whole). RateForTTFT sets the reason and the per-replica rate. TTFTAt is scored
// against the router and says whether the target is met now: a refusal there
// (at or over the prefill knee, at or over the decode onset) only blanks the
// prediction, since the rate is past what the fleet serves at any TTFT, which
// is a miss and not a hold.
func priceTTFT(in ttftInputs, rate float64, target whatif.TTFTTarget) ttftPricing {
	out := ttftPricing{predicted: math.NaN(), perReplica: math.NaN()}
	fl, own := ttftFleet(in, in.pods)
	out.f = fl.Prefill.Imbalance
	switch r := whatif.RateForTTFT(fl, target).(type) {
	case whatif.Answer:
		out.rate = &r
	case whatif.Unanswerable:
		out.rateRefused = &r
	}
	switch r := whatif.TTFTAt(fl, rate, target.Percentile).(type) {
	case whatif.TTFT:
		out.at = &r
		out.unmet = r.Seconds > target.Seconds || r.HasFlag(whatif.AboveFlowControlOnset)
	case whatif.Unanswerable:
		out.atRefused = &r
		out.unmet = r.Reason == whatif.Saturated || r.Reason == whatif.AboveDecodeOnset
	}

	var flags []whatif.Flag
	switch {
	case out.rate != nil:
		out.reason = answeredReason(fl, *out.rate, in)
		flags = out.rate.Flags
		if out.reason == constants.SLOTTFTReasonPriced {
			out.perReplica = out.rate.Value / float64(fl.Prefill.Replicas)
		}
		if out.at != nil {
			out.predicted = out.at.Seconds
		}
	case out.rateRefused != nil:
		out.reason = ttftReason(*out.rateRefused)
		if out.at != nil {
			flags = out.at.Flags
		}
		// No fleet meets the target, so it is missed at any rate, whatever
		// the prediction reads there.
		if out.rateRefused.Reason == whatif.TargetBelowFloor {
			out.unmet = true
		}
	}
	if out.rate != nil || out.at != nil {
		out.flags = append(slices.Clone(flags), own...)
	}
	return out
}

// answeredReason is the reason of a rate the model gave: decode-bound when
// decode sets it, routing-bound when one pod takes the work, priced otherwise.
func answeredReason(fl whatif.TTFTFleet, rate whatif.Answer, in ttftInputs) string {
	if decodeBound(fl, rate) {
		return constants.SLOTTFTReasonDecodeBound
	}
	if bound, _ := in.routing(); bound {
		return constants.SLOTTFTReasonRoutingBound
	}
	return constants.SLOTTFTReasonPriced
}

// decodeBound reports whether decode sets the rate: the decode onset binds, the
// prefill side alone allows more than decodeBoundRatio times the rate, or the
// prefill side alone stands at a decode knee below the prefill knee, where it
// cannot show more.
func decodeBound(fl whatif.TTFTFleet, rate whatif.Answer) bool {
	if slices.Contains(rate.Basis.BoundBy, whatif.BoundDecodeOnset) {
		return true
	}
	t := rate.Basis.TTFT
	if t == nil {
		return false
	}
	if t.PrefillOnlyRate > decodeBoundRatio*rate.Value {
		return true
	}
	knee, ok := whatif.PrefillKnee(fl.Prefill).(whatif.Answer)
	return ok && fl.Decode != nil && t.KneeRate < knee.Value*(1-kneeTolerance) &&
		t.PrefillOnlyRate >= t.KneeRate*(1-kneeTolerance)
}

// publishedFlags is the flags as the snapshot holds them: in
// constants.SLOTTFTFlags order, so a golden does not depend on the order
// they were raised in.
func publishedFlags(flags []whatif.Flag) []string {
	var out []string
	for _, name := range constants.SLOTTFTFlags {
		if slices.Contains(flags, whatif.Flag(name)) {
			out = append(out, name)
		}
	}
	return out
}

// vpSource is where V_P came from: P1's source, or none when it refused.
func vpSource(est prefill.Estimate) string {
	if est.VP > 0 && est.VPRefused == "" && slices.Contains(constants.SLOTTFTVPSources, string(est.VPSource)) {
		return string(est.VPSource)
	}
	return constants.SLOTTFTVPNone
}

// ttftDecodeLine is a decode variant as its pricing left it.
type ttftDecodeLine struct {
	state  domain.VariantReplicaState
	detail pricingDetail
}

// ttftDecode is the decode side of the model's prefill variants: the model's
// one decode variant, when its line prices. Nil with several decode variants,
// which one a prefill variant hands to not being known, or with a line that is
// not measured.
func (a *Analyzer) ttftDecode(input domain.AnalyzerInput, fleet fleetReading, lines []ttftDecodeLine) *whatif.Decode {
	if len(lines) != 1 {
		return nil
	}
	v, d := lines[0].state.VariantName, lines[0].detail
	if d.source != constants.SLOSourceOwn && d.source != constants.SLOSourcePinned {
		return nil
	}
	line := d.line.Model
	if !itl.ValidModel(line.A, line.B) || !whatif.Finite(line.B) || !(line.B > 0) {
		return nil
	}
	params := a.engineParams(input.Namespace, input.ModelID, v)
	batchCap, _ := a.pricedBatchCap(params)
	return &whatif.Decode{
		Line: line, Band: d.band, CapacityTokens: fleet.capacityTokens(v, params),
		KVPerRequest: fleet.shape.KVreq, OutputTokens: fleet.shape.AvgOutputTokens,
		ReplicaCap: batchCap * fleet.engineRanks(v), Replicas: fleet.ready[v], Tier: d.line.Tier,
	}
}

// rowsOf is the rows of the named variants.
func rowsOf(rows []domain.ReplicaMetrics, variants []string) []domain.ReplicaMetrics {
	var out []domain.ReplicaMetrics
	for _, rm := range rows {
		if slices.Contains(variants, rm.VariantName) {
			out = append(out, rm)
		}
	}
	return out
}

// priceTTFTVariants prices every prefill variant of the model against the TTFT
// target and logs each; nil with no target or the switch off.
//
// The estimate is read before any lock of this analyzer is taken, and the
// windows lock a.mu for themselves (prefill_estimates.go, the lock order).
func (a *Analyzer) priceTTFTVariants(logger logr.Logger, input domain.AnalyzerInput, cfg *config.ScalingPolicy,
	fleet fleetReading, lines []ttftDecodeLine, now time.Time) []metrics.SLOTTFTReading {
	if !a.ttftPrice || cfg.SLO == nil || cfg.SLO.TTFT == nil {
		return nil
	}
	var prefills, decodes []string
	readyPrefill := 0
	for _, vs := range fleet.variants {
		switch domain.CanonicalRole(vs.Role) {
		case domain.RolePrefill:
			prefills = append(prefills, vs.VariantName)
			readyPrefill += fleet.ready[vs.VariantName]
		case domain.RoleDecode:
			decodes = append(decodes, vs.VariantName)
		}
	}
	if len(prefills) == 0 {
		return nil
	}
	target := whatif.TTFTTarget{Seconds: cfg.SLO.TTFT.Target.Seconds(), Percentile: float64(cfg.SLO.TTFTPercentile()) / 100}
	statistic := "p" + strconv.Itoa(cfg.SLO.TTFTPercentile())
	flow, mismatch := flowControlState(cfg.SLO, input.FlowControlSeries())
	if mismatch {
		logger.V(logging.VERBOSE).Info("slo.flowControl contradicts the router's flow-control series; priced as declared",
			"modelID", input.ModelID, "namespace", input.Namespace, "declared", string(cfg.SLO.FlowControl),
			"seriesPresent", flow.State == whatif.FlowControlOff)
	}
	decode := a.ttftDecode(input, fleet, lines)
	decodeVariant := "none"
	if decode != nil {
		decodeVariant = lines[0].state.VariantName
	}

	prefillRows := rowsOf(fleet.rows, prefills)
	decodeRows := rowsOf(fleet.rows, decodes)
	modelPrompt, _ := prefill.MeanEffectivePrompt(prefillRows, a.sglang(input, prefills))
	// One decode variant's measured TTFT, with no decode line to price:
	// several behind one prefill variant are not told apart.
	var measured whatif.DecodeTTFT
	if len(decodes) == 1 {
		var cycle whatif.DecodeTTFT
		if s, ok := decodeEngineTTFT(decodeRows); ok && modelPrompt > 0 {
			cycle = whatif.DecodeTTFT{Seconds: s, PromptTokens: modelPrompt}
		}
		measured = a.noteDecodeTTFT(ttftWindowKey(input.Namespace, input.ModelID, decodes[0]), cycle, now)
	}
	arrival := input.ArrivalRate
	if !(arrival > 0) || !whatif.Finite(arrival) {
		arrival = fleet.requestRate
	}

	view := &prefill.Settle{View: a.settleView(), Model: utils.GetNamespacedKey(input.Namespace, input.ModelID)}
	var out []metrics.SLOTTFTReading
	for _, v := range prefills {
		pods := fleet.ready[v]
		rows := rowsOf(fleet.rows, []string{v})
		est, fresh := a.prefillEstimate(input.Namespace, input.ModelID, v, now)
		var estimateAt time.Time
		if fresh {
			estimateAt = now
		}
		share, shareOK := rawShare(rows, view, now)
		prompt, _ := prefill.MeanEffectivePrompt(rows, a.sglang(input, []string{v}))
		in := ttftInputs{
			est: est, estimateAt: estimateAt, pods: pods, otherPods: readyPrefill - pods, percent: cfg.SLO.TTFTPercentile(),
			prompt: prompt, spread: readPromptSpread(rows, prompt),
			shares:   a.noteShare(ttftWindowKey(input.Namespace, input.ModelID, v), pods, share, shareOK, now),
			transfer: transferPerToken(decodeRows, prompt), decode: decode, decodeVariant: decodeVariant,
			decodeTTFT: measured, flow: flow, mismatch: mismatch,
		}
		p := gateOrPrice(&in, fresh, arrival, target)
		out = append(out, metrics.SLOTTFTReading{
			Variant: v, Statistic: statistic, Predicted: p.published(), PerReplica: p.perReplica,
			Unmet: p.unmet, VPSource: vpSource(est), Reason: p.reason, Refused: p.currentRefusal(),
			Flags: publishedFlags(p.flags),
		})
		logTTFT(logger, input, v, in, p, arrival, target)
	}
	return out
}

// currentRefusal is the model's refusal of the current rate because the rate
// is past what the fleet serves, one of constants.SLOTTFTRefusedReasons, or ""
// with no such refusal. It stands behind an absent prediction with unmet set;
// the reason stays the answer at the target.
func (p ttftPricing) currentRefusal() string {
	if p.atRefused == nil {
		return ""
	}
	if r := ttftReason(*p.atRefused); slices.Contains(constants.SLOTTFTRefusedReasons, r) {
		return r
	}
	return ""
}

// published is the prediction as the series holds it: only with a reason that
// has an answer behind it, so a refusal reads as a reason and not as a number.
func (p ttftPricing) published() float64 {
	if p.rate == nil {
		return math.NaN()
	}
	return p.predicted
}

// gateOrPrice is the variant's pricing, or S1's own reason not to price: no
// estimate from this cycle, or flow control on with no decode side, above whose
// onset TTFT is not priced at all. A variant with no Ready replica goes to the
// model, which names that.
func gateOrPrice(in *ttftInputs, fresh bool, rate float64, target whatif.TTFTTarget) ttftPricing {
	gated := ttftPricing{predicted: math.NaN(), perReplica: math.NaN()}
	if in.pods > 0 {
		switch {
		case !fresh:
			gated.reason = constants.SLOTTFTReasonNoEstimate
			return gated
		case in.flow.State == whatif.FlowControlOn && in.decode == nil:
			gated.reason = constants.SLOTTFTReasonNoDecodeSide
			return gated
		}
		base, refused := whatif.PrefillFromEstimate(in.est)
		if refused != nil {
			gated.reason = ttftReason(*refused)
			gated.rateRefused = refused
			return gated
		}
		in.base = base
	}
	return priceTTFT(*in, rate, target)
}

// sglang reports whether any of the named variants runs SGLang, whose prompt
// P1 reads another way.
func (a *Analyzer) sglang(input domain.AnalyzerInput, variants []string) bool {
	for _, v := range variants {
		if p := a.engineParams(input.Namespace, input.ModelID, v); p != nil && p.Engine == inferenceengine.EngineSGLang {
			return true
		}
	}
	return false
}

// ttftWindowKey is a variant's key in the TTFT windows: namespace|model|variant.
func ttftWindowKey(namespace, modelID, variant string) string {
	return namespace + "|" + modelID + "|" + variant
}

// flowControlName is the flow-control state on the log line.
func flowControlName(s whatif.FlowControlState) string {
	switch s {
	case whatif.FlowControlOn:
		return "on"
	case whatif.FlowControlNoSeries:
		return "no-series"
	}
	return "off"
}

// logTTFT is the variant's line in the cycle log: what was priced, from what,
// and both calls' refusals.
func logTTFT(logger logr.Logger, input domain.AnalyzerInput, variant string, in ttftInputs, p ttftPricing, rate float64, target whatif.TTFTTarget) {
	log := logger.V(logging.VERBOSE)
	if !log.Enabled() {
		return
	}
	nan := math.NaN()
	bound, independent, pooled, floor, beta, rho, theta := nan, nan, nan, nan, nan, nan, nan
	knee, onsetPrefill, onsetDecode := nan, nan, nan
	if t := p.at; t != nil {
		bound, independent, pooled, floor, beta = t.Seconds, t.Independent, t.Pooled, t.Floor, t.DecodeShare
		rho, theta, knee = t.PrefillUtilization, t.WaitScale, t.PrefillKnee
		onsetPrefill, onsetDecode = t.Onsets.Prefill, t.Onsets.Decode
	}
	fleetRate, prefillRate := nan, nan
	var boundBy []whatif.Bound
	if r := p.rate; r != nil {
		fleetRate, boundBy = r.Value, r.Basis.BoundBy
		if r.Basis.TTFT != nil {
			prefillRate = r.Basis.TTFT.PrefillOnlyRate
			if math.IsNaN(knee) {
				knee = r.Basis.TTFT.KneeRate
			}
		}
	}
	refusal := func(u *whatif.Unanswerable) string {
		if u == nil {
			return ""
		}
		return u.Error()
	}
	_, _, fSource := in.imbalance(in.pods)
	cvSource := cvSourceMeasured
	if in.spread.assumed {
		cvSource = cvSourceAssumed
	}
	log.Info("slo-ttft-variant",
		"modelID", input.ModelID, "namespace", input.Namespace, "variant", variant,
		"reason", p.reason, "target", target.Seconds, "percentile", target.Percentile,
		"rate", rate, "pods", in.pods, "unmet", p.unmet,
		// The model's figures.
		"bound", bound, "independent", independent, "pooled", pooled, "floor", floor, "beta", beta,
		"rho", rho, "theta", theta, "fleetRate", fleetRate, "perReplicaRate", p.perReplica,
		"prefillOnlyRate", prefillRate, "boundBy", boundBy, "knee", knee,
		"onsetPrefill", onsetPrefill, "onsetDecode", onsetDecode, "flags", p.flags,
		// The inputs.
		"vp", in.est.VP, "vpSource", vpSource(in.est), "vpPromptTokens", in.est.VPPromptTokens,
		"estimateAt", in.estimateAt, "a", in.est.A, "f", p.f, "fSource", fSource, "fRaw", in.shares.largest,
		"fRawLeast", in.shares.least, "fRawWindowFull", in.shares.full, "otherPrefillPods", in.otherPods,
		"prompt", in.prompt, "cv", in.spread.cv, "cvSource", cvSource, "transferPerToken", in.transfer,
		"decodeVariant", in.decodeVariant, "decodeTTFT", in.decodeTTFT.Seconds,
		"decodeTTFTPrompt", in.decodeTTFT.PromptTokens, "flowControl", flowControlName(in.flow.State),
		// Both calls' refusals.
		"rateRefused", refusal(p.rateRefused), "ttftRefused", refusal(p.atRefused))
}
