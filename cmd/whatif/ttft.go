package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/whatif"
)

// The TTFT half of both questions (docs/proposals/slo-capacity-planner.md,
// "Recording mode, as built"): the fleet is read from the replayed state, by
// the rules the SLO analyzer's shadow pricing follows
// (docs/proposals/slo-tiers.md, "S1 decisions"), and priced by P3
// (internal/signals/whatif, TTFTAt and RateForTTFT). The CLI prices one P/D
// fleet. Aggregated TTFT and per-bucket answers are the planner's M1, and are
// refused.
//
// Every assumed input is named twice: in the flags, and in the words of the
// fleet's assumed list, which the text prints and the JSON carries.

// The words the JSON and the text name a source or a role by.
const (
	sourceLearned    = "learned"
	sideDecodeEngine = "decode engine"
	rolePrefill      = "prefill"
)

// The flags S1 raises beside P3's, under S1's names.
const (
	// assumedPoisson is Poisson arrivals: P3 has no flag for them.
	assumedPoisson whatif.Flag = "AssumedPoissonArrivals"
	// promptSpreadAbovePeak is a cv above whatif.PromptCVPeak for the percentile: the
	// prompt's own tail shrinks as cv rises from there, so a larger cv can
	// price a shorter floor and a higher rate.
	promptSpreadAbovePeak whatif.Flag = "PromptSpreadAbovePeak"
	// routingBound is one pod taking at least whatif.RoutingBoundShare of the work,
	// sustained: more pods would mostly sit idle behind the router. Priced at
	// that f.
	routingBound whatif.Flag = "RoutingBound"
	// imbalanceNotSustained is an f at whatif.RoutingBoundShare of the pods that has
	// not held over the whole window: chance alone can read that at two pods.
	// Priced at it, the side that asks for more replicas.
	imbalanceNotSustained whatif.Flag = "ImbalanceNotSustained"
)

// The closing note's measured halves. The figures are the frozen P2 table's
// (TestTTFTFrozen_TheNotesFigures, learnreplay).
const (
	ttftNoteOpen     = "TTFT is UNVALIDATED until the staircase run (M2) and is not a bound: "
	ttftNoteAssumed  = "at the assumed prompt cv %s it read 1.1x to 4.1x the router's percentile on the P2 recordings; at the sent spread (cv 0.06) up to 34%% under it."
	ttftNoteMeasured = "at the sent spread (cv 0.06) it read up to 34% under the router's percentile on the P2 recordings."
)

// ttftQuestion is the TTFT a command was asked for.
type ttftQuestion struct {
	// q is the percentile in (0, 1), or 0 for the mean.
	q float64
	// seconds is the target, question 1 only.
	seconds float64
}

// label names the figure: "TTFT p90", or "mean TTFT".
func (t ttftQuestion) label() string {
	if t.q == 0 {
		return "mean TTFT"
	}
	// To a millionth of a percent: 0.57 * 100 is 56.99999999999999.
	return "TTFT p" + strconv.FormatFloat(math.Round(1e8*t.q)/1e6, 'f', -1, 64)
}

// checkTTFT validates the TTFT flags both questions share. It changes no
// flag.
func (f *answerFlags) checkTTFT() error {
	switch {
	case f.set["input-bucket"]:
		return usagef("--input-bucket: per-bucket answers are not built (the planner's M1)")
	case f.set["assume-vp"] && !(f.assumeVP > 0 && finite(f.assumeVP)):
		return usagef("--assume-vp %g, want a finite throughput above 0, in prompt tokens per second", f.assumeVP)
	case f.prefill < 0:
		return usagef("--prefill %d, want 0 (the recorded count) or more", f.prefill)
	case f.ttftFlagged() && f.set["headroom"] && f.question == replicasQuestion && !(f.headroom > 0 && f.headroom < 1):
		return usagef("--headroom %g, want a fraction of the knee in (0, 1): at the knee no TTFT exists", f.headroom)
	case f.ttftFlagged() && f.set["headroom"] && !(f.headroom > 0 && f.headroom <= 1):
		return usagef("--headroom %g, want a fraction of the knee in (0, 1]", f.headroom)
	}
	return nil
}

// ttftFlagged reports whether any flag asks about TTFT.
func (f *answerFlags) ttftFlagged() bool {
	return slices.ContainsFunc([]string{"ttft", "ttft-percentile", "assume-vp", "prefill", "prefill-variant"},
		func(name string) bool { return f.set[name] })
}

// replicasTTFT reads question 2's TTFT flags, before any replay, so that a
// bad flag is refused at once: nil when none asks about TTFT, and q 0 with no
// --ttft-percentile, for policyPercentile to fill.
func (f *answerFlags) replicasTTFT() (*ttftQuestion, error) {
	if !f.ttftFlagged() {
		return nil, nil
	}
	if f.ttft != "" {
		return nil, usagef("--ttft is a target, for slo-to-rate; replicas-to-slo takes --ttft-percentile")
	}
	if f.ttftPercentile == "" {
		return &ttftQuestion{}, nil
	}
	q, err := parseTTFTPercent("--ttft-percentile", f.ttftPercentile)
	if err != nil {
		return nil, err
	}
	return &ttftQuestion{q: q}, nil
}

// policyPercentile sets a percentile not asked for to the policy's, else 90.
func (t *ttftQuestion) policyPercentile(st *learnreplay.State) {
	if t == nil || t.q != 0 {
		return
	}
	pct := config.DefaultTTFTPercentile
	if st.Policy != nil && st.Policy.SLO != nil && st.Policy.SLO.TTFT != nil {
		pct = st.Policy.SLO.TTFTPercentile()
	}
	t.q = float64(pct) / 100
}

// rateTTFT reads question 1's TTFT flags: --ttft, a mean or with @p and a
// percentile, or --ttft-percentile beside a plain target. Nil when none asks
// about TTFT.
func (f *answerFlags) rateTTFT() (*ttftQuestion, error) {
	if f.ttft == "" {
		if f.ttftFlagged() {
			return nil, usagef("--ttft-percentile, --assume-vp, --prefill and --prefill-variant need a --ttft target on slo-to-rate")
		}
		return nil, nil
	}
	value, pct, hasPct := strings.Cut(f.ttft, "@")
	d, err := time.ParseDuration(value)
	if err != nil {
		return nil, usagef("--ttft %q: %v", f.ttft, err)
	}
	if d <= 0 {
		return nil, usagef("--ttft %q: want a target above 0", f.ttft)
	}
	t := &ttftQuestion{seconds: d.Seconds()}
	switch {
	case hasPct && f.ttftPercentile != "":
		return nil, usagef("--ttft %q and --ttft-percentile both name a percentile; give one", f.ttft)
	case hasPct && !strings.HasPrefix(pct, "p"):
		return nil, usagef("--ttft %q: a percentile is written @p90", f.ttft)
	case hasPct:
		t.q, err = parseTTFTPercent("--ttft", pct)
	case f.ttftPercentile != "":
		t.q, err = parseTTFTPercent("--ttft-percentile", f.ttftPercentile)
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// parseTTFTPercent reads a TTFT percentile in percent, refusing one outside
// (0, 100) here: a question asked wrongly outranks what the recording says.
func parseTTFTPercent(flagName, s string) (float64, error) {
	q, err := parsePercent(s)
	if err != nil {
		return 0, usagef("%s %q: %v", flagName, s, err)
	}
	if !(q > 0 && q < 1) {
		return 0, usagef("%s %q: want a percentile above 0 and below 100", flagName, s)
	}
	return q, nil
}

// pricedFleet is one P/D fleet as a TTFT question prices it: the prefill role
// and the decode variant it hands to.
type pricedFleet struct {
	// pv is the decode variant; nil with none in the recording.
	pv *pricedVariant
	// refused is why the fleet cannot be priced before the library is asked:
	// an input with no source.
	refused *whatif.Unanswerable
	fleet   whatif.TTFTFleet
	// variant names the prefill variant, and q is the percentile asked.
	variant string
	q       float64
	// assumedVP is a V_P --assume-vp supplied.
	assumedVP bool
	// flags are the fleet's own, beside P3's.
	flags []whatif.Flag
	// inputsText is where the fleet's inputs came from, for the human
	// output; in the same for the JSON. in.Assumed names each assumed input.
	inputsText string
	in         wireTTFT
}

// label names a fleet's rows, when more than one decode variant is priced.
func (pf *pricedFleet) label(role string, fleets int) string {
	if fleets > 1 && pf.pv != nil {
		return role + "/" + pf.pv.state.Variant
	}
	return role
}

// prefillState is the prefill variant the question prices: the recording's
// only one, or the one --prefill-variant names. Nil with none. Several, with
// none named, is a usage error: the CLI prices one fleet, and the whole rate
// on one of several would read wrong.
func (f *answerFlags) prefillState(st *learnreplay.State) (*learnreplay.PrefillState, error) {
	if f.prefillVariant != "" {
		for i := range st.Prefill {
			if st.Prefill[i].Variant == f.prefillVariant {
				return &st.Prefill[i], nil
			}
		}
		return nil, usagef("--prefill-variant %q: no prefill variant of that name at this cycle", f.prefillVariant)
	}
	switch len(st.Prefill) {
	case 0:
		return nil, nil
	case 1:
		return &st.Prefill[0], nil
	}
	names := make([]string, len(st.Prefill))
	for i := range st.Prefill {
		names[i] = st.Prefill[i].Variant
	}
	return nil, usagef("the model has %d prefill variants (%s); name one with --prefill-variant", len(names), strings.Join(names, ", "))
}

// ttftFleets is the fleet of each decode variant, in order; with no decode
// variant, one fleet with no decode side.
func (f *answerFlags) ttftFleets(st *learnreplay.State, q ttftQuestion, variants []pricedVariant) ([]*pricedFleet, error) {
	ps, err := f.prefillState(st)
	if err != nil {
		return nil, err
	}
	if len(variants) == 0 {
		return []*pricedFleet{f.fleetFor(st, ps, q, nil)}, nil
	}
	decodes := 0
	for _, vs := range st.Variants {
		if domain.CanonicalRole(vs.Role) != domain.RolePrefill {
			decodes++
		}
	}
	fleets := make([]*pricedFleet, len(variants))
	for i := range variants {
		pf := f.fleetFor(st, ps, q, &variants[i])
		if pf.refused == nil && decodes > 1 {
			// As S1 prices decode: the model's whole rate.
			pf.assume("the whole rate on decode variant %s (the model has %d)", variants[i].state.Variant, decodes)
		}
		fleets[i] = pf
	}
	return fleets, nil
}

// noPrefillRole is the refusal for a fleet with no prefill variant: aggregated
// TTFT is M1, and a role the recording does not hold has no a to price.
func noPrefillRole(st *learnreplay.State) *whatif.Unanswerable {
	for _, vs := range st.Variants {
		if domain.CanonicalRole(vs.Role) == domain.RoleBoth {
			return aggregatedRefusal(vs.Variant)
		}
	}
	return &whatif.Unanswerable{Reason: whatif.NoPrefillReplicas,
		Detail: fmt.Sprintf("no prefill variant at cycle %d of %s/%s", st.Cycle, st.Namespace, st.ModelID)}
}

// aggregatedRefusal refuses TTFT on an aggregated variant.
func aggregatedRefusal(variant string) *whatif.Unanswerable {
	return &whatif.Unanswerable{Reason: whatif.NoPrefillReplicas,
		Detail: fmt.Sprintf("variant %s is aggregated: aggregated TTFT is not built (the planner's M1); the model prices P/D fleets only", variant)}
}

// fleetFor reads one fleet, and every input's source, from the state.
func (f *answerFlags) fleetFor(st *learnreplay.State, ps *learnreplay.PrefillState, q ttftQuestion, pv *pricedVariant) *pricedFleet {
	pf := &pricedFleet{pv: pv, q: q.q, flags: []whatif.Flag{assumedPoisson},
		in: wireTTFT{Percentile: q.q, TargetSeconds: q.seconds, Assumed: []string{}}}
	if pv != nil {
		pf.in.DecodeVariant = pv.state.Variant
		if domain.CanonicalRole(pv.state.Role) == domain.RoleBoth {
			pf.refused = aggregatedRefusal(pv.state.Variant)
			return pf
		}
	}
	if ps == nil {
		pf.refused = noPrefillRole(st)
		return pf
	}
	pf.variant = ps.Variant
	p, refused := f.prefillInputs(pf, ps, st.Cycle)
	if refused != nil {
		pf.refused = refused
		return pf
	}
	pf.fleet.Prefill = p
	f.prefillShape(pf, ps)
	pf.decodeSide()
	pf.flowControl(st)
	pf.assume("Poisson arrivals")
	pf.in.ArrivalsSource = shapeAssumed
	if f.prefillVariant != "" && len(st.Prefill) > 1 {
		pf.assume("the whole rate on prefill variant %s (--prefill-variant; the model has %d)", ps.Variant, len(st.Prefill))
	}
	pf.raiseFlags(ps)
	pf.describe()
	return pf
}

// raiseFlags adds S1's flags on the spread and the imbalance. The routing
// flags read f at the recorded count, where the window was read.
func (pf *pricedFleet) raiseFlags(ps *learnreplay.PrefillState) {
	p := pf.fleet.Prefill
	if pf.q > 0 && p.PromptCV > whatif.PromptCVPeak(pf.q) {
		pf.flags = append(pf.flags, promptSpreadAbovePeak)
	}
	if p.Replicas != ps.Pods {
		return
	}
	pods := float64(p.Replicas)
	switch {
	case p.Replicas < 2 || p.Imbalance/pods < whatif.RoutingBoundShare:
	case !p.ImbalanceAssumed, ps.RawShareFull && ps.RawShareLeast/pods >= whatif.RoutingBoundShare:
		pf.flags = append(pf.flags, routingBound)
	default:
		pf.flags = append(pf.flags, imbalanceNotSustained)
	}
}

// assume names one assumed input.
func (pf *pricedFleet) assume(format string, args ...any) {
	pf.in.Assumed = append(pf.in.Assumed, fmt.Sprintf(format, args...))
}

// prefillInputs is V_P, a, f and N_p. V_P is --assume-vp, as every --assume-*
// flag overrides the recording, with P1's figure printed beside it; else P1's,
// read through the replay; else NoPrefillCapacity: it is never defaulted. a
// has no assumption to fall back on: with none, NoFixedLatency.
func (f *answerFlags) prefillInputs(pf *pricedFleet, ps *learnreplay.PrefillState, cycle int) (whatif.Prefill, *whatif.Unanswerable) {
	est := ps.Estimate
	learned := ps.EstimateRead && est.VP > 0 && est.VPRefused == ""
	switch {
	case f.set["assume-vp"]:
		if learned {
			vp := est.VP
			pf.in.VPLearned = &vp
		}
		est.VP, est.VPRefused, est.VPLowerBound, est.VPAboveSlopeBound = f.assumeVP, "", false, false
		est.VPPromptTokens = 0
		pf.assumedVP = true
		pf.in.VPSource = shapeAssumed
		pf.assume("V_P %s tok/s (--assume-vp)", num(f.assumeVP))
	case learned:
		pf.in.VPSource, pf.in.VPOrigin = sourceLearned, string(est.VPSource)
	default:
		why := fmt.Sprintf("no P1 estimate of %s at cycle %d", ps.Variant, cycle)
		if ps.EstimateRead {
			why = fmt.Sprintf("P1 refused V_P of %s: %s", ps.Variant, est.VPRefused)
		}
		return whatif.Prefill{}, &whatif.Unanswerable{Reason: whatif.NoPrefillCapacity,
			Detail: why + "; pass --assume-vp for a flagged answer"}
	}
	if !ps.EstimateRead {
		return whatif.Prefill{}, &whatif.Unanswerable{Reason: whatif.NoFixedLatency,
			Detail: fmt.Sprintf("no P1 estimate of %s at cycle %d", ps.Variant, cycle)}
	}
	p, refused := whatif.PrefillFromEstimate(est)
	if refused != nil {
		return whatif.Prefill{}, refused
	}
	if pf.assumedVP {
		// The caller's figure, for the prompt asked: neither rescaled nor
		// margined (Prefill.shapedVP).
		p.VPAssumed, p.VPMargin, p.CurveSlope, p.CurveCurvature = true, 0, 0, 0
	}
	pf.in.VP, pf.in.VPPromptTokens = p.VP, p.VPPromptTokens
	pf.in.FixedLatencySeconds, pf.in.FixedLatencySource = p.FixedLatency, sourceLearned

	p.Replicas = ps.Pods
	pf.in.Replicas, pf.in.ReplicasFrom = ps.Pods, "recording"
	if f.prefill > 0 {
		p.Replicas, pf.in.ReplicasFrom = f.prefill, "--prefill"
		pf.in.Replicas = f.prefill
	}
	p.Imbalance, p.ImbalanceAssumed, pf.in.ImbalanceSource = imbalanceAt(ps, p.Replicas)
	pf.in.Imbalance = max(p.Imbalance, 1)
	if p.ImbalanceAssumed {
		pf.assume("f %s on %d pods (none learned at that count)", num(p.Imbalance), p.Replicas)
	}
	return p, nil
}

// imbalanceAt is f at pods, by S1's decision 1: P1's own at that count, else
// nothing on one pod, which P3 prices as exactly 1, else an assumed
// min(pods, max(1.5, f)), f the learned figure at the recorded count or the
// raw share. A learned f is never carried unchanged to another count, and one
// pod's exact 1 never reaches two.
func imbalanceAt(ps *learnreplay.PrefillState, pods int) (f float64, assumed bool, source string) {
	if v, ok := ps.Estimate.FByPods[pods]; ok && v >= 1 {
		return v, false, sourceLearned
	}
	if pods == 1 {
		return 0, false, "one pod"
	}
	base := ps.RawShare
	if v, ok := ps.Estimate.FByPods[ps.Pods]; ok && v >= 1 {
		base = v
	}
	return min(float64(pods), max(whatif.ImbalanceFloor, base)), true, shapeAssumed
}

// prefillShape is the prompt: --assume-input-tokens, else what the prefill
// pods computed, else the decode variant's effective prompt; and its spread
// and the KV transfer per token. The transfer is read at the measured prompt,
// whatever the prompt asked: it is a figure of the traffic that moved it.
func (f *answerFlags) prefillShape(pf *pricedFleet, ps *learnreplay.PrefillState) {
	p := &pf.fleet.Prefill
	pv := pf.pv
	switch {
	case f.set["assume-input-tokens"]:
		p.PromptTokens = pf.effectivePrompt(f.inputTokens)
		pf.in.PromptSource = shapeAssumed
		pf.assume("prompt %s tokens (--assume-input-tokens)", num(round(p.PromptTokens)))
	case ps.PromptTokens > 0:
		p.PromptTokens, pf.in.PromptSource = ps.PromptTokens, shapeMeasured
		pf.in.PromptFromCycle, pf.in.PromptToCycle = ps.PromptFromCycle, ps.PromptToCycle
	case pv != nil && pv.shape.ILeff > 0:
		p.PromptTokens, pf.in.PromptSource = pv.shape.ILeff, "decode shape"
		pf.assume("prompt %s tokens (the decode variant's; no computed-token rate on the prefill rows)", num(round(p.PromptTokens)))
	}
	pf.in.PromptTokens = p.PromptTokens

	p.PromptCV, p.PromptCVAssumed = whatif.AssumedPromptCV, true
	pf.in.PromptCV, pf.in.PromptCVSource = whatif.AssumedPromptCV, shapeAssumed
	pf.assume("prompt cv %s (no computed-prompt histogram)", num(whatif.AssumedPromptCV))

	if pv != nil && pv.state.DecodeTTFT.TransferSeconds > 0 && ps.PromptTokens > 0 {
		p.TransferSecondsPerToken = pv.state.DecodeTTFT.TransferSeconds / ps.PromptTokens
	}
}

// effectivePrompt is an assumed prompt less the prefix hits of the decode
// variant's shape; with none, the prompt as given.
func (pf *pricedFleet) effectivePrompt(il float64) float64 {
	if pf.pv == nil {
		return il
	}
	return pf.pv.shape.ILeff
}

// decodeSide is decode's part of the TTFT: the decode variant's line when
// it prices, else the decode engine's measured TTFT, which holds the KV pull
// (TTFTFleet.DecodeTTFT), else nothing, flagged by P3.
func (pf *pricedFleet) decodeSide() {
	pf.in.TransferSecondsPerToken = pf.fleet.Prefill.TransferSecondsPerToken
	pf.in.TransferSource = shapeMeasured
	if pf.in.TransferSecondsPerToken == 0 {
		pf.in.TransferSource = "omitted"
	}
	if pf.pv != nil {
		if _, ok := whatif.KneeRate(pf.pv.decode).(whatif.Answer); ok {
			d := pf.pv.decode
			pf.fleet.Decode = &d
			pf.in.DecodeSide = "line"
			return
		}
		pf.fleet.DecodeTTFT = pf.pv.state.DecodeTTFT.EngineTTFT
	}
	if pf.fleet.DecodeTTFT.Seconds > 0 {
		pf.in.TransferSecondsPerToken, pf.in.TransferSource = 0, sideDecodeEngine
		pf.in.DecodeSide = sideDecodeEngine
		pf.in.DecodeTTFTSeconds = pf.fleet.DecodeTTFT.Seconds
		pf.in.DecodeTTFTPromptTokens = pf.fleet.DecodeTTFT.PromptTokens
		return
	}
	pf.in.DecodeSide = "omitted"
}

// flowControl is flow control inferred from its series: S1's rule with
// slo.flowControl unset. The CLI does not read that field, so a tier that
// declares the state is still inferred here. On carries the upstream
// thresholds.
func (pf *pricedFleet) flowControl(st *learnreplay.State) {
	if st.FlowControlSeries {
		pf.fleet.FlowControl = whatif.FlowControl{State: whatif.FlowControlOn,
			QueueThreshold: whatif.FlowControlQueueThreshold, KVThreshold: whatif.FlowControlKVThreshold}
		pf.in.FlowControl = "on"
		pf.assume("flow-control thresholds %s and %s (upstream defaults)", num(whatif.FlowControlQueueThreshold), num(whatif.FlowControlKVThreshold))
		return
	}
	pf.fleet.FlowControl = whatif.FlowControl{State: whatif.FlowControlNoSeries}
	pf.in.FlowControl = "off, inferred"
	pf.assume("flow control off (no queue series)")
}

// describe is the fleet's inputs, in a line.
func (pf *pricedFleet) describe() {
	p := pf.fleet.Prefill
	var b strings.Builder
	fmt.Fprintf(&b, "V_P %s ", num(round(p.VP)))
	switch {
	case pf.assumedVP && pf.in.VPLearned != nil:
		fmt.Fprintf(&b, "ASSUMED, learned %s", num(round(*pf.in.VPLearned)))
	case pf.assumedVP:
		b.WriteString("ASSUMED")
	case p.VPPromptTokens > 0:
		fmt.Fprintf(&b, "learned (%s, at %s tokens)", pf.in.VPOrigin, num(round(p.VPPromptTokens)))
	default:
		fmt.Fprintf(&b, "learned (%s)", pf.in.VPOrigin)
	}
	fmt.Fprintf(&b, "; a %.2f ms learned; f %.4g (%s); N_p %d (%s)", 1000*p.FixedLatency, pf.in.Imbalance, pf.in.ImbalanceSource,
		p.Replicas, pf.in.ReplicasFrom)
	fmt.Fprintf(&b, "; prompt %s tokens (%s", num(round(p.PromptTokens)), pf.in.PromptSource)
	if pf.in.PromptFromCycle > 0 {
		fmt.Fprintf(&b, ", cycles %d-%d", pf.in.PromptFromCycle, pf.in.PromptToCycle)
	}
	b.WriteString(")")
	switch pf.in.DecodeSide {
	case "line":
		b.WriteString("; decode by its line")
	case sideDecodeEngine:
		fmt.Fprintf(&b, "; decode engine TTFT %.1f ms at %s tokens, measured", 1000*pf.fleet.DecodeTTFT.Seconds, num(round(pf.fleet.DecodeTTFT.PromptTokens)))
	default:
		b.WriteString("; decode side omitted")
	}
	pf.inputsText = b.String()
}

// verdict is the words every TTFT figure prints beside it.
func (pf *pricedFleet) verdict() string {
	if pf.assumedVP {
		return "UNVALIDATED; V_P ASSUMED"
	}
	return "UNVALIDATED"
}

// footerNote is the fleets' closing note. The direction turns on the spread:
// a measured cv (the collector's Q1) has only the sent spread's row to go by.
func footerNote(fleets []*pricedFleet) string {
	note := ttftNoteOpen + fmt.Sprintf(ttftNoteAssumed, num(whatif.AssumedPromptCV))
	if slices.ContainsFunc(fleets, func(pf *pricedFleet) bool { return pf.refused == nil && !pf.fleet.Prefill.PromptCVAssumed }) {
		note = ttftNoteOpen + ttftNoteMeasured
	}
	if slices.ContainsFunc(fleets, func(pf *pricedFleet) bool { return pf.assumedVP }) {
		note += " V_P ASSUMED."
	}
	return note
}

// priced is the fleet's Result from price, or the refusal of an input with no
// source.
func (pf *pricedFleet) priced(price func(whatif.TTFTFleet) whatif.Result) whatif.Result {
	if pf.refused != nil {
		return *pf.refused
	}
	return pf.qualify(price(pf.fleet))
}

// qualify adds what the fleet says of an answer: the decode variant's flags
// where its line priced it, an assumed shape in any case, and the fleet's own.
func (pf *pricedFleet) qualify(r whatif.Result) whatif.Result {
	add := slices.Clone(pf.flags)
	if pf.pv != nil {
		if pf.fleet.Decode != nil {
			r = pf.pv.qualify(r)
			add = append(add, pf.pv.flags...)
		}
		if pf.pv.assumed() {
			add = append(add, whatif.AssumedShape)
		}
	}
	return withFlags(r, add)
}

// withFlags is r with each of add it does not carry; a refusal has none.
func withFlags(r whatif.Result, add []whatif.Flag) whatif.Result {
	merged := func(flags []whatif.Flag) []whatif.Flag {
		flags = slices.Clone(flags)
		for _, fl := range add {
			if !slices.Contains(flags, fl) {
				flags = append(flags, fl)
			}
		}
		return flags
	}
	switch v := r.(type) {
	case whatif.Answer:
		v.Flags = merged(v.Flags)
		return v
	case whatif.TTFT:
		v.Flags = merged(v.Flags)
		return v
	}
	return r
}

// knee is the prefill knee, or the refusal of its inputs.
func (pf *pricedFleet) knee() whatif.Result {
	if pf.refused != nil {
		return *pf.refused
	}
	return whatif.PrefillKnee(pf.fleet.Prefill)
}

// operatingTTFT is question 2's TTFT on one fleet.
type operatingTTFT struct {
	fleet *pricedFleet
	q     ttftQuestion
	// knee is the prefill knee, and ttft the answer at rate.
	knee, ttft whatif.Result
	// prefillBinds says the prefill knee set the operating rate: it is the
	// lower of the two, and no rate was given.
	prefillBinds bool
	rate         float64
}

// atOperatingPoint prices the fleet at --rate, or at the headroom times the
// lower of the roles' knees. It leaves query as it is unless the prefill knee
// binds, and then it asks for that rate: the fleet's operating point is one
// rate, and the TPOT is read at it.
func (pf *pricedFleet) atOperatingPoint(q ttftQuestion, query whatif.ReplicasQuery, given *float64, headroom float64, decodeKnee whatif.Result) (operatingTTFT, whatif.ReplicasQuery) {
	o := operatingTTFT{fleet: pf, q: q, knee: pf.knee()}
	pk, ok := o.knee.(whatif.Answer)
	if !ok && given == nil {
		o.ttft = o.knee
		return o, query
	}
	if given != nil {
		o.rate = *given
		pf.in.RateSource = "--rate"
	} else {
		lowest := pk.Value
		dk, decoded := decodeKnee.(whatif.Answer)
		o.prefillBinds = !decoded || pk.Value < dk.Value
		pf.in.RateSource = sourcePrefillKnee
		if !o.prefillBinds {
			lowest = dk.Value
			pf.in.RateSource = "headroom x decode knee"
		}
		o.rate = headroom * lowest
		if o.prefillBinds {
			query.Rate, query.Headroom = o.rate, 0
		}
	}
	pf.in.Rate = &o.rate
	o.ttft = pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, o.rate, q.q) })
	return o, query
}

// results is the Results the exit code reads.
func (o operatingTTFT) results() []whatif.Result { return []whatif.Result{o.knee, o.ttft} }

// onDecode is a decode figure read at the operating rate. A rate the prefill
// knee set rests on V_P, so a figure read at it carries the flags the knee
// does: it is no more validated than that rate.
func (o operatingTTFT) onDecode(r whatif.Result) whatif.Result {
	if k, ok := o.knee.(whatif.Answer); ok && o.prefillBinds {
		return withFlags(r, k.Flags)
	}
	return r
}

// decodeNote is what a decode row read at the operating rate adds to its
// basis: nothing unless the prefill knee set that rate.
func (o *operatingTTFT) decodeNote() string {
	if o == nil || !o.prefillBinds {
		return ""
	}
	return "; at the prefill knee's rate; " + o.fleet.verdict()
}

// fleetRate is question 1's fleet row: the smaller of the roles' rates and
// the operating point of the lower knee. decode is nil when no TPOT was
// asked. A role's refusal is the fleet's.
func fleetRate(q ttftQuestion, headroom float64, decode, ttft whatif.Result) whatif.Result {
	if u, ok := decode.(whatif.Unanswerable); ok {
		return u
	}
	t, ok := ttft.(whatif.Answer)
	if !ok {
		return ttft
	}
	type bound struct {
		rate float64
		by   whatif.Bound
	}
	bounds := []bound{{headroom * t.Basis.TTFT.KneeRate, whatif.BoundOperatingPoint},
		{t.Value, whatif.Bound(q.label() + ", end to end")}}
	flags := slices.Clone(t.Flags)
	if d, ok := decode.(whatif.Answer); ok {
		bounds = append(bounds, bound{d.Value, whatif.Bound("decode " + boundText(d.Basis.BoundBy))})
		for _, fl := range d.Flags {
			if !slices.Contains(flags, fl) {
				flags = append(flags, fl)
			}
		}
	}
	lowest := bounds[0].rate
	for _, b := range bounds[1:] {
		lowest = min(lowest, b.rate)
	}
	a := whatif.Answer{Value: lowest, Flags: flags}
	for _, b := range bounds {
		if b.rate == lowest {
			a.Basis.BoundBy = append(a.Basis.BoundBy, b.by)
		}
	}
	return a
}

// msText is a duration in seconds, in milliseconds.
func msText(s float64) string { return fmt.Sprintf("%.1f ms", 1000*s) }

// ttftBoundText names what set a TTFT rate, the target by its percentile.
func ttftBoundText(q ttftQuestion, by []whatif.Bound) string {
	parts := make([]string, len(by))
	for i, b := range by {
		parts[i] = string(b)
		if b == whatif.BoundTTFT {
			parts[i] = q.label()
		}
	}
	return strings.Join(parts, ", ")
}

// writeInputs prints the fleet's inputs and every assumed one, under an
// indent.
func (pf *pricedFleet) writeInputs(b *strings.Builder, inputsLabel, assumedLabel string) {
	if pf.inputsText != "" {
		fmt.Fprintf(b, "%s%s\n", inputsLabel, pf.inputsText)
	}
	if len(pf.in.Assumed) > 0 {
		fmt.Fprintf(b, "%s%s\n", assumedLabel, strings.Join(pf.in.Assumed, "; "))
	}
}

// kneeBasis spells the prefill knee: N_p x V_P x (1 - margin) / (f x prompt),
// V_P at the prompt asked, read back from the knee, so it is the V_P priced
// (rescaled along P1's curve, where it was) and not the one learned.
func (pf *pricedFleet) kneeBasis(knee float64) string {
	p := pf.fleet.Prefill
	vp := knee * pf.in.Imbalance * p.PromptTokens / float64(p.Replicas) / (1 - p.VPMargin)
	margin := ""
	if p.VPMargin > 0 {
		margin = fmt.Sprintf(" x (1 - %s margin)", num(p.VPMargin))
	}
	return fmt.Sprintf("N_p %d x V_P %s%s / (f %.4g x %s tokens); %s", p.Replicas, num(round(vp)), margin,
		pf.in.Imbalance, num(round(p.PromptTokens)), pf.verdict())
}

// writeRows prints question 2's TTFT rows.
func (o operatingTTFT) writeRows(b *strings.Builder) {
	pf := o.fleet
	if k, ok := o.knee.(whatif.Answer); ok {
		row(b, "prefill knee", rateText(k.Value), "", pf.kneeBasis(k.Value))
	}
	switch v := o.ttft.(type) {
	case whatif.TTFT:
		row(b, o.q.label(), msText(v.Seconds), "", "end to end; "+pf.verdict())
		if msText(v.Independent) != msText(v.Seconds) {
			fmt.Fprintf(b, "%-16s(%s if prefill and decode waits were independent)\n", "", msText(v.Independent))
		}
		fmt.Fprintf(b, "%-16s(floor %s, pooled %s, worst pod utilization %.3f)\n", "", msText(v.Floor), msText(v.Pooled), v.PrefillUtilization)
	case whatif.Unanswerable:
		fmt.Fprintf(b, "%-16s%s\n", o.q.label(), refusedText(v))
	}
	pf.writeInputs(b, fmt.Sprintf("%-16s", "ttft inputs"), fmt.Sprintf("%-16s", "assumed"))
}

// writeRateRows prints question 1's prefill and fleet rows, for layout's
// columns.
func writeRateRows(b *strings.Builder, layout string, pf *pricedFleet, fleets int, q ttftQuestion, ttft, fleet whatif.Result) {
	role := pf.label(rolePrefill, fleets)
	switch r := ttft.(type) {
	case whatif.Answer:
		fmt.Fprintf(b, layout, role, rateText(r.Value), ttftBoundText(q, r.Basis.BoundBy), pf.verdict())
	case whatif.Unanswerable:
		fmt.Fprintf(b, "%-9s%s\n", role, refusedText(r))
	}
	pf.writeInputs(b, fmt.Sprintf("%-9sinputs ", ""), fmt.Sprintf("%-9sassumed ", ""))
	role = pf.label("fleet", fleets)
	switch r := fleet.(type) {
	case whatif.Answer:
		fmt.Fprintf(b, layout, role, rateText(r.Value), boundText(r.Basis.BoundBy), pf.verdict())
	case whatif.Unanswerable:
		fmt.Fprintf(b, "%-9s%s\n", role, refusedText(r))
	}
}
