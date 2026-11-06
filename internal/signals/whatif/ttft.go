package whatif

import (
	"fmt"
	"math"
	"slices"
)

// This file is the TTFT model of the what-if planner (PR P3 of
// docs/proposals/slo-capacity-planner.md; the model is
// docs/proposals/slo-capacity-planner-ttft.md):
//
//	TTFT = W_router + W_engine + own_prefill + T_xfer + W_decode + ITL(k_d)
//
// on a P/D fleet. Aggregated TTFT is not built (M1).
//
// UNVALIDATED: the only caller in the controller is the SLO analyzer's shadow
// pricing, which sizes nothing (slo.SwitchTTFTPrice). The plan
// has P3 depend on P2, the prefill-stress GPU recording kept as a golden; that
// recording is blocked on cluster access, so P3 is built against closed-form
// queueing values and synthetic fleets instead, and every figure carries
// UnvalidatedTTFT. What P2 must check is listed on the TTFT page, "As built
// (P3)". The earlier TTFT = A*T + B fit is not repeated: TTFT includes the
// queue wait, which landed in that fit's intercept (82 s;
// docs/proposals/prefill-ttft-model.md). Here the wait is a queue of its own,
// and the fixed cost a is measured at low load, where there is none.

// Prefill is one prefill role as the TTFT model prices it: Replicas pods,
// each a work-conserving server of prompt tokens at V_P, the busiest of them
// fed f times its share of the arrivals. The figures are P1's
// (internal/signals/prefill; PrefillFromEstimate maps them), or assumed and
// flagged.
type Prefill struct {
	// VP is V_P, one pod's prompt-token throughput while it has work, in
	// tokens per second.
	VP float64
	// VPLowerBound marks a V_P from a source that is a lower bound
	// (VPLowerBound), VPAboveSlopeBound one above the low-load slope's
	// bound, and VPAssumed one the caller supplied (AssumedVP).
	VPLowerBound, VPAboveSlopeBound, VPAssumed bool
	// FixedLatency is a, in seconds: the latency one request pays on a pod
	// besides its prompt's work. It enters the request's own latency only,
	// never the queue's work (busy-minute V_P is already net of it).
	FixedLatency float64
	// Imbalance is f, at least 1 and at most Replicas: the busiest pod's
	// share of the work over the mean share. 0 is unknown, which one pod
	// prices as 1 and more than one refuses (NoImbalance).
	// ImbalanceAssumed marks one the caller supplied (AssumedImbalance).
	Imbalance        float64
	ImbalanceAssumed bool
	// Replicas is N_p.
	Replicas int
	// PromptTokens is E[I_eff], the mean prompt a pod computes (prefix
	// hits are not prefill work).
	PromptTokens float64
	// PromptCV is the coefficient of variation of I_eff, in [0, 10]: the
	// prompt is taken as gamma distributed with it (exponential at 1,
	// constant at 0), and Cs^2 = PromptCV^2. PromptCVAssumed marks one the
	// caller assumed (AssumedPromptSpread).
	PromptCV        float64
	PromptCVAssumed bool
	// ArrivalCV2 is Ca^2, the arrivals' squared coefficient of variation:
	// Kingman's 1 + Cs^2 becomes Ca^2 + Cs^2. 0 means Poisson, 1.
	ArrivalCV2 float64
	// TransferSecondsPerToken is the KV transfer's time per prompt token,
	// kappa/BW. 0 omits T_xfer, flagged TransferOmitted: it is folded into
	// the request's own latency, a + I*(1/V_P + kappa/BW), not the queue's
	// work. With no decode line and the decode engine's TTFT measured
	// (TTFTFleet.DecodeTTFT) it is not priced: that TTFT holds the KV pull.
	TransferSecondsPerToken float64
	// VPPromptTokens is I_vp, the mean effective prompt V_P was read at
	// (prefill.Estimate.VPPromptTokens); 0 is unknown.
	VPPromptTokens float64
	// CurveSlope and CurveCurvature are b and c of P1's low-load curve
	// s0 + b*I + c*I*I (prefill.Estimate.ASlope and ACurve, with AModel
	// quadratic): a prompt of length I costs b + c*I per token in a busy
	// step. Both positive is a curve; otherwise there is none, and V_P is
	// not rescaled (shapedVP).
	CurveSlope, CurveCurvature float64
	// VPMargin is the relative margin taken off V_P near the knee, in
	// [0, MaxVPMargin]: none below MarginRampStart of the worst pod's
	// utilization, all of it from MarginRampFull, linear between
	// (prefillPlan.atRate). PrefillFromEstimate sets DefaultVPMargin; 0 is
	// none.
	VPMargin float64
}

// MaxVPShapeRatio is how far, either way, the mean prompt may be from the one
// V_P was read at before V_P is flagged VPShapeMoved, when there is no curve
// to rescale it by (S1's 1.2x rule, docs/proposals/slo-tiers.md, decision 2).
const MaxVPShapeRatio = 1.2

// DefaultVPMargin is the margin PrefillFromEstimate puts on a learned V_P near
// the knee: 5%, ten times the 0.5% by which run 2's pod under a queue served
// away from its busy-minute V_P at the prompt it was learned on
// (docs/proposals/slo-capacity-planner-ttft.md, "As built (P3 after P2)").
// A stated margin, not a measured one, and it covers only that noise, at
// the prompt length V_P was learned at. It does not cover an error in the
// curve's rescale or in pricing a mix of prompt lengths at its mean (an even
// 8k/52k mix is 32% under its prefill time, "Where it is most wrong").
const DefaultVPMargin = 0.05

// MaxVPMargin bounds VPMargin at 1 - MarginRampFull, so the margin is whole
// before the margined pod saturates and the knee is (1 - VPMargin) of the
// unmargined one (prefillPlan.knee).
const MaxVPMargin = 1 - MarginRampFull

// MarginRampStart and MarginRampFull are the worst pod's utilization, at the
// unmargined V_P, where the margin starts and where it is whole. Near the
// knee TTFT is most sensitive to V_P: on the M/M/1 sojourn, the model's
// tail at cv 1, a V_P read (1 + e) high puts TTFT at
// (1-rho)/((1+e)(1-rho) + e*rho) of the truth. At e = DefaultVPMargin that
// is 9.1% short at rho 0.5, 10.0% at 0.55, 14.3% at 0.7 and 25% at 0.85: it
// crosses the S1 mark's 10% under-side at 0.55, and the ramp is centred on
// that crossing (TestVPMarginRampIsWhereTTFTTurnsSensitive). Below it the
// error is under the mark on its own, and no margin is spent. The ramp is
// linear, not a step, so TTFT has no jump in the rate.
const (
	MarginRampStart = 0.5
	MarginRampFull  = 0.6
)

// maxPrefillReplicas bounds N_p: the pooled figure's Erlang C recursion
// runs once per pod, as the admission chain runs once per slot.
const maxPrefillReplicas = maxSlots

// maxPromptCV bounds the prompt's coefficient of variation: past 10 the
// gamma's shape is below 0.01 and the model's exponential tail says nothing.
const maxPromptCV = 10

// minPromptCV is the least prompt spread priced as a gamma; below it the
// prompt is priced as constant. The gamma's shape 1/cv^2 is then above 1e8,
// where its incomplete gamma loses its digits to cancellation (cv 1e-7 read
// 8% high); the two figures differ by less than the spread itself.
const minPromptCV = 1e-4

// FlowControlState is what is known of EPP flow control.
type FlowControlState int

const (
	// FlowControlOff is flow control known to be off (the upstream default):
	// the router admits everything, W_router = 0.
	FlowControlOff FlowControlState = iota
	// FlowControlNoSeries is off because its queue series does not exist:
	// priced as off, flagged FlowControlInferredOff.
	FlowControlNoSeries
	// FlowControlOn is the flowControl feature gate on, with the default
	// utilization detector under the static policy, one band.
	FlowControlOn
)

// FlowControl is EPP flow control as the TTFT model prices it. With it on,
// the gate closes when the unweighted mean of the endpoints' scores
// max(queueDepth/QueueThreshold, kvUsage/KVThreshold) reaches 1, per stage
// on P/D.
type FlowControl struct {
	State FlowControlState
	// QueueThreshold and KVThreshold are the utilization detector's
	// (upstream 5 and 0.8). Read only when State is FlowControlOn; not both
	// positive and finite there is FlowControlUnknown.
	QueueThreshold, KVThreshold float64
}

// TTFTFleet is the fleet a TTFT is priced on: the prefill role, the decode
// role it hands to, and flow control.
type TTFTFleet struct {
	Prefill Prefill
	// Decode is the decode role; nil means no decode line: W_decode is not
	// priced, and decode's part of the TTFT is DecodeTTFT, measured, in place
	// of T_xfer + ITL(k_d), flagged DecodeEngineMeasured, or left out,
	// flagged DecodeSideOmitted and DecodeTTFTUnmeasured. The SLO analyzer's
	// shadow pricing of a prefill variant (S1) with flow control off is that
	// case.
	Decode *Decode
	// DecodeTTFT is the decode engine's measured TTFT, read only when Decode
	// is nil. On P/D the router's TTFT is the prefill engine's and then the
	// decode engine's (the KV pull and decode's first step): P2 found the
	// router's median less the two at -7 to +15 ms on 6 of 7 levels. Zero
	// Seconds is unmeasured: the decode term is left out, flagged
	// DecodeTTFTUnmeasured, which no caller acts on.
	DecodeTTFT  DecodeTTFT
	FlowControl FlowControl
}

// DecodeTTFT is the decode engine's mean TTFT, measured: on P/D, the time from
// the request reaching the decode pod to its first token, the KV pull
// included. S1 learns it per decode variant from the R1 field
// domain.ReplicaMetrics.AvgTTFT on the variant's Ready decode rows
// (docs/proposals/slo-tiers.md, decision 3). It is held at the prompt
// scaling below and at the rate it was measured at: RateForTTFT extrapolates
// to other rates with it frozen. Seconds and PromptTokens are one pair, from
// the same cycle.
type DecodeTTFT struct {
	// Seconds is the mean, in seconds; 0 is unmeasured.
	Seconds float64
	// PromptTokens is the mean effective prompt it was measured at. The KV
	// pull grows with the prompt and there is a fixed part besides, so the
	// term is priced at Seconds * max(1, I/PromptTokens): scaled up for a
	// longer prompt (the fixed part makes that high), held for a shorter one
	// (the pull makes that high). Required, at least MinDecodeTTFTPrompt,
	// when Seconds is above 0. It is
	// E[I_eff], the computed prompt, while the KV pull moves the whole
	// prompt, prefix hits included: with prefix hits the ratio I/PromptTokens
	// tracks the pull only as far as the hit rate holds.
	PromptTokens float64
}

// MinDecodeTTFTPrompt is the least prompt, in tokens, a measured decode-engine
// TTFT may be read at: one token. A smaller one is not a measured mean prompt,
// and dividing by it overflows the term.
const MinDecodeTTFTPrompt = 1.0

// at is the decode term for a prompt of i tokens: 0 when unmeasured or with no
// prompt to scale by (plan refuses a measured figure without one).
func (d DecodeTTFT) at(i float64) float64 {
	if d.Seconds == 0 || !(d.PromptTokens > 0) {
		return 0
	}
	return d.Seconds * max(1, i/d.PromptTokens)
}

// TTFTTarget is a TTFT service level, end to end.
type TTFTTarget struct {
	// Seconds is the target.
	Seconds float64
	// Percentile is q in (0, 1), or 0 for the mean.
	Percentile float64
}

// prefillPlan is a checked Prefill with its derived figures.
type prefillPlan struct {
	p Prefill
	// f is the imbalance used (1 for one pod with none given).
	f float64
	// cv is the prompt's coefficient of variation as priced (0 below
	// minPromptCV); ca2, cs2 are Ca^2 and Cs^2 = cv^2.
	cv, ca2, cs2 float64
	// vp is V_P at the question's prompt (shapedVP), before the margin;
	// xfer the transfer per token priced in the own latency, 0 where the
	// decode engine's measured TTFT holds it.
	vp, xfer float64
	// margin is VPMargin, and applied the share of it taken at the rate
	// this plan was made for (atRate); 0 on the plan check returns.
	margin, applied float64
	// work is E[S_work] = E[I]/V_P, the queue's work per request; ownRate
	// is the own latency per prompt token, 1/V_P + kappa/BW, each at V_P
	// less the margin applied.
	work, ownRate float64
	flags         []Flag
}

// setVP sets the work and the own latency per token at V_P = vp.
func (pl *prefillPlan) setVP(vp float64) {
	pl.work = pl.p.PromptTokens / vp
	pl.ownRate = 1/vp + pl.xfer
}

// marginAt is the share of V_P taken off at utilization rho0, read at the
// unmargined V_P: 0 up to MarginRampStart, the whole margin from
// MarginRampFull, linear between.
func (pl prefillPlan) marginAt(rho0 float64) float64 {
	return pl.margin * min(max((rho0-MarginRampStart)/(MarginRampFull-MarginRampStart), 0), 1)
}

// atRate is the plan at rate, with the margin on V_P that utilization asks
// for. The worst pod's utilization at the margined V_P,
// rho0/(1 - marginAt(rho0)), rises with rho0, so TTFT still rises with the
// rate; it reaches 1 at rho0 = 1 - margin, the margined knee.
func (pl prefillPlan) atRate(rate float64) prefillPlan {
	m := pl.marginAt(pl.utilization(rate))
	if m == 0 {
		return pl
	}
	q := pl
	q.applied = m
	q.setVP(pl.vp * (1 - m))
	return q
}

// shapedVP is V_P at the question's mean prompt E[I], read at I_vp
// (VPPromptTokens), and the flag it raises.
//
// A busy pod's step of B tokens of prompts of length I costs about
// s0 + (b + c*I)*B (prefill.Estimate.SlopeBound), so
//
//	V_P(I)/V_P(I_vp) = (s0/B + b + c*I_vp) / (s0/B + b + c*I).
//
// With a curve, s0/B unknown, the ratio is taken at s0/B = 0,
// s(I_vp)/s(I) with s(I) = b + c*I, and V_P is rescaled down by it for I
// above I_vp, flagged VPScaledToPrompt. That reads at or below the true V_P
// only if P1's low-load b and c are the busy step's per-token cost, which
// is not measured: on run 2 at 37k it read 1.5% above what the pod served
// (TestVPRescaleOnRun2At37k), and the margin covers that only from rho 0.6
// (MarginRampFull). So VPScaledToPrompt is on S2's no-act list. For I below
// I_vp the ratio would over-read the gain, so V_P is held at the learned
// figure. Without a curve, or with I_vp unknown,
// V_P is held, and a mean prompt more than MaxVPShapeRatio from I_vp either
// way, or an unknown I_vp, is flagged VPShapeMoved. An assumed V_P is the
// caller's for the prompt asked, and is neither rescaled nor flagged.
func (p Prefill) shapedVP() (float64, Flag) {
	if p.VPAssumed {
		return p.VP, ""
	}
	i, ivp := p.PromptTokens, p.VPPromptTokens
	if p.CurveSlope > 0 && p.CurveCurvature > 0 && ivp > 0 {
		if i > ivp {
			s := func(x float64) float64 { return p.CurveSlope + p.CurveCurvature*x }
			return p.VP * s(ivp) / s(i), VPScaledToPrompt
		}
		return p.VP, ""
	}
	if !(ivp > 0) || i > MaxVPShapeRatio*ivp || i*MaxVPShapeRatio < ivp {
		return p.VP, VPShapeMoved
	}
	return p.VP, ""
}

// check refuses a Prefill the model cannot price, naming the first input
// that fails, in the order an operator fixes them: the question, then the
// capacity, the fixed latency, the imbalance and the shape.
func (p Prefill) check() (prefillPlan, *Unanswerable) {
	var u Unanswerable
	switch {
	case p.Replicas < 1:
		u = refuse(NoPrefillReplicas, fmt.Sprintf("prefill replicas %d", p.Replicas))
	case p.Replicas > maxPrefillReplicas:
		u = refuse(InvalidInput, fmt.Sprintf("%d prefill replicas, above the %d priced", p.Replicas, maxPrefillReplicas))
	case !Finite(p.VP) || !(p.VP > 0):
		u = refuse(NoPrefillCapacity, fmt.Sprintf("V_P %g tokens/s", p.VP))
	case !Finite(p.FixedLatency) || !(p.FixedLatency > 0):
		u = refuse(NoFixedLatency, fmt.Sprintf("a %g s", p.FixedLatency))
	case p.Imbalance == 0 && p.Replicas > 1:
		u = refuse(NoImbalance, fmt.Sprintf("no f for %d prefill pods, learned or assumed", p.Replicas))
	case p.Imbalance != 0 && !(p.Imbalance >= 1 && p.Imbalance <= float64(p.Replicas)):
		u = refuse(InvalidInput, fmt.Sprintf("f %g, want 1 <= f <= %d (the pods)", p.Imbalance, p.Replicas))
	case !Finite(p.PromptTokens) || !(p.PromptTokens > 0):
		u = refuse(NoTraffic, fmt.Sprintf("prompt %g tokens", p.PromptTokens))
	case !(p.PromptCV >= 0 && p.PromptCV <= maxPromptCV):
		// The spread is assumed or read from a histogram; a value outside
		// [0, 10] is the caller's mistake, not the traffic's.
		u = refuse(InvalidInput, fmt.Sprintf("prompt cv %g, want 0 <= cv <= %d", p.PromptCV, maxPromptCV))
	case !Finite(p.ArrivalCV2) || p.ArrivalCV2 < 0:
		u = refuse(InvalidInput, fmt.Sprintf("Ca^2 %g, want a finite value of at least 0 (0 is Poisson)", p.ArrivalCV2))
	case !Finite(p.TransferSecondsPerToken) || p.TransferSecondsPerToken < 0:
		u = refuse(InvalidInput, fmt.Sprintf("transfer %g s per token", p.TransferSecondsPerToken))
	case !Finite(p.VPPromptTokens) || p.VPPromptTokens < 0:
		u = refuse(InvalidInput, fmt.Sprintf("V_P read at %g prompt tokens", p.VPPromptTokens))
	case !Finite(p.CurveSlope) || p.CurveSlope < 0 || !Finite(p.CurveCurvature) || p.CurveCurvature < 0:
		u = refuse(InvalidInput, fmt.Sprintf("curve b %g, c %g, want finite values of at least 0", p.CurveSlope, p.CurveCurvature))
	case !(p.VPMargin >= 0 && p.VPMargin <= MaxVPMargin):
		u = refuse(InvalidInput, fmt.Sprintf("V_P margin %g, want 0 <= margin <= %g", p.VPMargin, MaxVPMargin))
	default:
		cv := p.PromptCV
		if cv < minPromptCV {
			cv = 0
		}
		pl := prefillPlan{p: p, f: max(p.Imbalance, 1), ca2: p.ArrivalCV2, cv: cv, cs2: cv * cv,
			xfer: p.TransferSecondsPerToken, margin: p.VPMargin}
		if pl.ca2 == 0 {
			pl.ca2 = 1
		}
		vp, shape := p.shapedVP()
		pl.vp = vp
		pl.setVP(vp)
		pl.flags = []Flag{UnvalidatedTTFT}
		for _, c := range []struct {
			on bool
			f  Flag
		}{
			{p.VPAssumed, AssumedVP}, {p.VPLowerBound, VPLowerBound}, {p.VPAboveSlopeBound, VPAboveSlopeBound},
			{shape != "", shape},
			{p.ImbalanceAssumed, AssumedImbalance}, {p.PromptCVAssumed, AssumedPromptSpread},
			{p.TransferSecondsPerToken == 0, TransferOmitted},
		} {
			if c.on {
				pl.flags = append(pl.flags, c.f)
			}
		}
		return pl, nil
	}
	return prefillPlan{}, &u
}

// knee is the prefill knee, N_p * V_P * (1 - VPMargin) / (f * E[I_eff]), V_P
// at the question's prompt: the rate at which the worst pod saturates. The
// margin is whole there (MaxVPMargin).
func (pl prefillPlan) knee() float64 {
	return float64(pl.p.Replicas) * pl.vp * (1 - pl.margin) / (pl.f * pl.p.PromptTokens)
}

// utilization is rho_w, the worst pod's: f * rate * E[I_eff] / (N_p * V_P),
// at the plan's V_P (unmargined on a checked plan, margined after atRate).
func (pl prefillPlan) utilization(rate float64) float64 {
	return pl.f * rate * pl.work / float64(pl.p.Replicas)
}

// waitScale is theta, E[S_work](Ca^2 + Cs^2) / (2(1 - rho)).
func (pl prefillPlan) waitScale(rho float64) float64 {
	return pl.work * (pl.ca2 + pl.cs2) / (2 * (1 - rho))
}

// own is the request's own latency less a: I * (1/V_P + kappa/BW).
func (pl prefillPlan) own() ownTime {
	return newOwnTime(pl.p.PromptTokens*pl.ownRate, pl.cv)
}

// engineWait is the worst pod's wait tail at utilization rho, and its decay
// rate r. The rule errs only toward a longer tail, and so more replicas:
//
//   - Poisson arrivals (Ca^2 = 1), cv >= 1: rho * exp(-r t), r the smaller of
//     Kingman's 1/theta and the Cramer-Lundberg root gamma of
//     lambda_w (M_S(gamma) - 1) = gamma (M_S the work's moment generating
//     function), the exact asymptotic decay of an M/G/1 wait. At cv 1 the two
//     are equal (mu - lambda) and the tail is exact (M/M/1). Above 1 the root
//     is the smaller: 1/theta alone read a p99 up to 10% short of a Lindley
//     simulation.
//   - Poisson arrivals, cv < 1: min(rho, C exp(-gamma t)), Taylor's upper
//     bound on an M/G/1 wait: C = sup_x P(S > x) / E[exp(gamma (S - x)); S > x].
//     Gamma work at cv <= 1 has an increasing failure rate, so the ratio
//     rises with x and its sup is the limit, C = 1 - gamma/b with b the
//     gamma's rate k/E[S] (1 for constant work, which is the Lundberg
//     inequality). At cv 1, C = rho, so the bound meets the exact M/M/1 tail
//     and TTFT does not jump at the boundary. An exponential body behind the
//     atom under-states the wait of light-tailed work just past the atom:
//     against Erlang's exact M/D/1 wait it read a p90 sojourn 10% short at
//     rho 0.2.
//   - Otherwise (Kingman's Ca^2 + Cs^2): rho * exp(-r t), r = 1/theta, never
//     above the work's own tail rate b = 1/(cv^2 E[S]): a wait cannot thin
//     out faster than one request's work does. Not checked against a
//     simulation; bursty arrivals lengthen the tail, so Kingman's 1/theta is
//     the conservative side of it.
//
// A rate that is not finite (no Cramer-Lundberg root) comes back as such;
// price refuses it.
func (pl prefillPlan) engineWait(rho float64) (waitTail, float64) {
	r := 1 / pl.waitScale(rho)
	if pl.ca2 == 1 {
		g := lundbergRoot(rho/pl.work, pl.work, pl.cv)
		if pl.cv < 1 {
			c := 1.0
			if pl.cv > 0 {
				c = 1 - g*pl.cv*pl.cv*pl.work // 1 - g/b, b = 1/(cv^2 E[S])
			}
			return boundWait(rho, c, g), g
		}
		r = min(r, g)
		return expWait(rho, r), r
	}
	if pl.cv > 0 {
		r = min(r, 1/(pl.cv*pl.cv*pl.work))
	}
	return expWait(rho, r), r
}

// queueLength is a pod's mean waiting count at utilization rho,
// rho^2 (Ca^2 + Cs^2) / (2(1 - rho)) (Little's law on the mean wait).
func (pl prefillPlan) queueLength(rho float64) float64 {
	return rho * rho * (pl.ca2 + pl.cs2) / (2 * (1 - rho))
}

// meanScore is the prefill stage's mean endpoint score at rate: the worst
// pod at f, the other N_p - 1 sharing the rest evenly, each scoring its mean
// waiting count over the threshold. Prefill KV usage is not in it: a prefill
// pod's queue binds long before its KV does (run PK held 730 waiting at 25%
// KV).
func (pl prefillPlan) meanScore(rate, threshold float64) float64 {
	pl = pl.atRate(rate)
	n := float64(pl.p.Replicas)
	rw := pl.utilization(rate)
	if rw >= 1 {
		return math.Inf(1)
	}
	sum := pl.queueLength(rw)
	if n > 1 {
		sum += (n - 1) * pl.queueLength(rate*pl.work*(n-pl.f)/(n*(n-1)))
	}
	return sum / n / threshold
}

// ttftPlan is a checked fleet.
type ttftPlan struct {
	pf prefillPlan
	d  *Decode
	// decodeTTFT is the decode engine's measured TTFT, set only with no
	// decode line.
	decodeTTFT DecodeTTFT
	fc         FlowControl
	onsets     Onsets
	flags      []Flag
}

// plan checks a fleet and places its flow-control onsets.
func (fl TTFTFleet) plan() (ttftPlan, *Unanswerable) {
	pf, u := fl.Prefill.check()
	if u != nil {
		return ttftPlan{}, u
	}
	if fl.Decode != nil {
		if u := fl.Decode.check(); u != nil {
			return ttftPlan{}, u
		}
	}
	pl := ttftPlan{pf: pf, d: fl.Decode, fc: fl.FlowControl}
	if fl.Decode == nil {
		dt := fl.DecodeTTFT
		switch {
		case !Finite(dt.Seconds) || dt.Seconds < 0:
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s", dt.Seconds))
			return ttftPlan{}, &u
		case dt.Seconds == 0:
			pf.flags = append(pf.flags, DecodeTTFTUnmeasured, DecodeSideOmitted)
		case !Finite(dt.PromptTokens) || !(dt.PromptTokens >= MinDecodeTTFTPrompt):
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s measured at %g prompt tokens: a measured TTFT (Seconds > 0) requires a prompt of at least %g tokens",
				dt.Seconds, dt.PromptTokens, MinDecodeTTFTPrompt))
			return ttftPlan{}, &u
		case !Finite(dt.at(pf.p.PromptTokens)):
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s at %g prompt tokens overflows at the prompt asked, %g tokens",
				dt.Seconds, dt.PromptTokens, pf.p.PromptTokens))
			return ttftPlan{}, &u
		default:
			// The decode engine's TTFT holds the KV pull: pricing T_xfer
			// beside it would count the pull twice.
			pl.decodeTTFT = dt
			pf.xfer = 0
			pf.setVP(pf.vp)
			pf.flags = slices.DeleteFunc(pf.flags, func(f Flag) bool { return f == TransferOmitted })
			pf.flags = append(pf.flags, DecodeEngineMeasured)
		}
	}
	pl.pf, pl.flags = pf, pf.flags
	switch fl.FlowControl.State {
	case FlowControlOff:
	case FlowControlNoSeries:
		pl.flags = append(pl.flags, FlowControlInferredOff)
	case FlowControlOn:
		qt, kt := fl.FlowControl.QueueThreshold, fl.FlowControl.KVThreshold
		if !Finite(qt) || !(qt > 0) || !Finite(kt) || !(kt > 0) {
			u := refuse(FlowControlUnknown, fmt.Sprintf("queue threshold %g, KV threshold %g", qt, kt))
			return ttftPlan{}, &u
		}
		if fl.Decode == nil {
			pl.flags = append(pl.flags, DecodeOnsetUnchecked)
		}
		on, u := pl.placeOnsets()
		if u != nil {
			return ttftPlan{}, u
		}
		pl.onsets = on
	default:
		u := refuse(InvalidInput, fmt.Sprintf("flow-control state %d", fl.FlowControl.State))
		return ttftPlan{}, &u
	}
	return pl, nil
}

// placeOnsets finds each stage's onset by bisection on the rate: the least
// rate at which the stage's mean endpoint score reaches 1, below the stage's
// knee. A stage whose score stays under 1 up to its knee has its knee as the
// onset.
func (pl ttftPlan) placeOnsets() (Onsets, *Unanswerable) {
	qt, kt := pl.fc.QueueThreshold, pl.fc.KVThreshold
	var on Onsets
	on.Prefill = onset(pl.pf.knee(), func(r float64) float64 { return pl.pf.meanScore(r, qt) })
	if pl.d == nil {
		return on, nil
	}
	d := *pl.d
	if d.full().slots > maxSlots {
		u := refuse(TooManySlots, fmt.Sprintf("%g slots on one replica, above the %d the chain holds", d.full().slots, maxSlots))
		return Onsets{}, &u
	}
	on.Decode = onset(d.kneeRate(), func(r float64) float64 { return decodeScore(d, r, qt, kt) })
	return on, nil
}

// decodeScore is a decode pod's score at rate, the decode pods balanced:
// max(Lq/QueueThreshold, fill/KVThreshold) from the admission chain, with Lq
// the mean waiting count, lambda_rep times the mean admission wait (Little),
// and fill the chain's mean fill. +Inf where the chain has no stationary
// state.
func decodeScore(d Decode, rate, qt, kt float64) float64 {
	adm, ok := DecodeAdmission(d, rate).(Admission)
	if !ok {
		return math.Inf(1)
	}
	lq := rate / float64(d.Replicas) * adm.MeanWait()
	return max(lq/qt, adm.Basis.Fill/kt)
}

// onset is the least rate in [0, knee) at which score reaches 1, or knee if
// it never does there. score is non-decreasing in the rate.
func onset(knee float64, score func(float64) float64) float64 {
	_, hi := bisectRate(knee, func(r float64) bool { return score(r) < 1 })
	return hi
}

// bisectRate brackets the edge of a predicate that holds below some rate in
// [0, limit) and fails above it: lo is the largest rate found where it holds
// and hi the least where it fails, within a relative 1e-12. When it holds
// just under limit, both are limit.
func bisectRate(limit float64, holds func(float64) bool) (lo, hi float64) {
	if holds(limit * (1 - 1e-12)) {
		return limit, limit
	}
	lo, hi = 0, limit
	for range 200 {
		mid := lo + (hi-lo)/2
		if holds(mid) {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-12*hi {
			break
		}
	}
	return lo, hi
}

// PrefillKnee is the prefill knee, N_p * V_P / (f * E[I_eff]), in requests
// per second over the fleet (question 2, step 1): the rate at which the
// busiest prefill pod saturates. Basis.TTFT.KneeRate repeats it.
func PrefillKnee(p Prefill) Result {
	pl, u := p.check()
	if u != nil {
		return *u
	}
	k := pl.knee()
	return Answer{Value: k, Flags: pl.flags, Basis: Basis{BoundBy: []Bound{BoundPrefillKnee}, TTFT: &TTFTBasis{KneeRate: k}}}
}

// FlowControlOnsets are the rates at which EPP flow control's gate starts
// holding requests (docs/proposals/slo-capacity-planner-ttft.md, "The router
// queue"): zero with flow control off. The decode onset is an extra knee:
// RateForTTFT caps at it and TTFTAt refuses above it.
func FlowControlOnsets(fl TTFTFleet) Result {
	pl, u := fl.plan()
	if u != nil {
		return *u
	}
	return pl.onsets
}

// lowestKnee is the lower of the two roles' knees.
func (pl ttftPlan) lowestKnee() float64 {
	k := pl.pf.knee()
	if pl.d != nil {
		k = min(k, pl.d.kneeRate())
	}
	return k
}

// priced is one TTFT at one rate, before it becomes a TTFT result.
type priced struct {
	out TTFT
	// u is set when the rate cannot be priced.
	u *Unanswerable
}

// price is the TTFT at rate and percentile q (0 for the mean). With
// withDecodeWait false the decode admission wait is left out: the prefill
// side alone, which RateForTTFT compares against.
func (pl ttftPlan) price(rate, q float64, withDecodeWait, extras bool) priced {
	pk := pl.pf.knee()
	out := TTFT{PrefillKnee: pk, Onsets: pl.onsets, Flags: append([]Flag(nil), pl.flags...)}
	if rate >= pk {
		u := refuse(Saturated, fmt.Sprintf("%g r/s at or above the prefill knee %g r/s", rate, pk))
		return priced{u: &u}
	}
	if pl.fc.State == FlowControlOn {
		if pl.d != nil && rate >= pl.onsets.Decode {
			u := refuse(AboveDecodeOnset, fmt.Sprintf("%g r/s at or above the decode gate's onset %g r/s", rate, pl.onsets.Decode))
			return priced{u: &u}
		}
		if rate >= pl.onsets.Prefill {
			out.Flags = append(out.Flags, AboveFlowControlOnset)
		}
	}

	pf := pl.pf.atRate(rate)
	out.VPMarginApplied = pf.applied
	rho := pf.utilization(rate)
	theta := pf.waitScale(rho)
	wait, tail := pf.engineWait(rho)
	if rho > 0 && (!Finite(tail) || !(tail > 0)) {
		u := refuse(Saturated, fmt.Sprintf("no finite wait tail at %g r/s (decay rate %g)", rate, tail))
		return priced{u: &u}
	}
	own := pf.own()
	a := pl.pf.p.FixedLatency
	b1 := block{wait: wait, own: own}
	out.PrefillUtilization, out.WaitScale = rho, 1/tail
	if len(wait.s) > 0 {
		out.WaitOffset = wait.s[0]
	}
	out.Parts.EngineWait = rho * theta
	out.Parts.OwnPrefill = a + own.mean

	var dec waitTail
	var first, floorStep float64
	if pl.d != nil {
		d := *pl.d
		out.DecodeKnee, out.Tier = d.kneeRate(), d.Tier
		// The admission chain is the costly part, and the prefill side
		// alone (withDecodeWait false, RateForTTFT's second bisection)
		// needs none of it: its first step comes from the closed-form fill,
		// which refuses past the knee as the chain does.
		var adm Admission
		if withDecodeWait || extras {
			switch v := DecodeAdmission(d, rate).(type) {
			case Admission:
				adm = v
			case Unanswerable:
				return priced{u: &v}
			}
		}
		k, ok := d.fillAtReplicaRate(rate / float64(d.Replicas))
		if !ok {
			u := refuse(Saturated, fmt.Sprintf("no decode fill carries %g r/s", rate))
			return priced{u: &u}
		}
		if k < d.Band.Lo {
			k = d.Band.Lo
			out.Flags = addFlags(out.Flags, BelowFittedBand)
		}
		first, floorStep = d.Line.ITLAt(k), d.Line.ITLAt(d.Band.Lo)
		out.Flags = addFlags(out.Flags, adm.Flags...)
		out.DecodeWaitProbability = adm.WaitProbability
		if withDecodeWait {
			dec = expWait(adm.WaitProbability, adm.Decay)
			out.Parts.DecodeWait = adm.MeanWait()
		}
	} else {
		// No decode line: decode's part is the decode engine's TTFT,
		// measured (0, flagged DecodeTTFTUnmeasured, when it is not). It is
		// a mean, added at its mean to every percentile: its own spread is
		// not priced.
		first = pl.decodeTTFT.at(pl.pf.p.PromptTokens)
		if !Finite(first) {
			// plan refuses an overflowing term; kept so that no infinite
			// figure is ever answered as a TTFT.
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s at %g prompt tokens", first, pl.pf.p.PromptTokens))
			return priced{u: &u}
		}
		floorStep = first
		out.Parts.DecodeEngine = first
	}

	if pl.d != nil {
		out.Parts.FirstStep = first
	}

	if q == 0 {
		mean := out.Parts.EngineWait + out.Parts.OwnPrefill + out.Parts.DecodeWait + first
		if !Finite(mean) {
			u := refuse(Saturated, fmt.Sprintf("no finite mean TTFT at %g r/s", rate))
			return priced{u: &u}
		}
		out.Seconds, out.Independent = mean, mean
		out.Floor = a + pl.pf.own().mean + floorStep
		if extras {
			out.Pooled = pf.pooledWait(rate).mean() + out.Parts.OwnPrefill + out.Parts.DecodeWait + first
		}
		return priced{out: out}
	}
	v, beta := split(b1, dec, q)
	if !Finite(v) {
		// Only within rounding of a knee, where the wait's scale overflows.
		u := refuse(Saturated, fmt.Sprintf("no finite TTFT at %g r/s", rate))
		return priced{u: &u}
	}
	if !Finite(a + v + first) {
		u := refuse(InvalidInput, fmt.Sprintf("no finite TTFT at %g r/s: a %g s, decode %g s", rate, a, first))
		return priced{u: &u}
	}
	out.Seconds, out.DecodeShare = a+v+first, beta

	out.Floor = a + pl.pf.own().quantile(q) + floorStep
	out.Independent = out.Seconds
	if extras {
		// The two waits always sum (a single-piece wait and the decode
		// chain's unshifted one); were they ever not to, the union bound
		// stands for the independent figure, which it never undercuts.
		if both, ok := b1.wait.sum(dec); ok && len(dec.w) > 0 {
			out.Independent = a + block{wait: both, own: own}.quantile(1-q) + first
		}
		pv, _ := split(block{wait: pf.pooledWait(rate), own: own}, dec, q)
		out.Pooled = a + pv + first
	}
	return priced{out: out}
}

// pooledWait is the optimistic wait: the N_p pods as one M/G/N_p queue with
// no imbalance (Allen-Cunneen), P(W > t) = C * exp(-t/theta_p), C Erlang C at
// the offered load and theta_p = E[S](Ca^2 + Cs^2) / (2 N_p (1 - rho)).
func (pl prefillPlan) pooledWait(rate float64) waitTail {
	n := pl.p.Replicas
	load := rate * pl.work
	rho := load / float64(n)
	thetaP := pl.work * (pl.ca2 + pl.cs2) / (2 * float64(n) * (1 - rho))
	return expWait(erlangCProb(n, load), 1/thetaP)
}

// checkPercentile refuses a TTFT percentile outside (0, 1); 0 is the mean.
func checkPercentile(q float64) *Unanswerable {
	if q == 0 || (q > 0 && q < 1) {
		return nil
	}
	u := refuse(InvalidInput, fmt.Sprintf("TTFT percentile %g, want 0 < q < 1, or 0 for the mean", q))
	return &u
}

// TTFTAt is the end-to-end TTFT at rate requests per second over the fleet,
// at percentile q, or the mean for q = 0 (question 2's TTFT): the union bound
// with its parts, the independent figure beside it, the pooled optimistic
// figure and the floor.
//
// It refuses (Saturated) at or above the lower knee, and, with flow control
// on, at or above the decode gate's onset (AboveDecodeOnset); at or above the
// prefill gate's onset it answers, flagged AboveFlowControlOnset.
func TTFTAt(fl TTFTFleet, rate, q float64) Result {
	// The question first: a usage error outranks what the fleet's data says,
	// and placing the onsets is not free.
	if u := checkRate(rate); u != nil {
		return *u
	}
	if u := checkPercentile(q); u != nil {
		return *u
	}
	pl, u := fl.plan()
	if u != nil {
		return *u
	}
	p := pl.price(rate, q, true, true)
	if p.u != nil {
		return *p.u
	}
	return p.out
}

// RateForTTFT is question 1 for TTFT: the largest rate, in requests per
// second over the fleet, whose TTFT at the target's percentile (or mean) is at
// most the target, decode admission priced at the same rate
// (docs/proposals/slo-capacity-planner.md, "Question 1").
//
// TTFT rises with the rate, so the rate is bisected below the lower knee and,
// with flow control on, below the decode gate's onset, which caps it
// (BoundDecodeOnset). When the prefill side alone would allow more (by more
// than a relative 1e-6), the rate is the one decode admission holds it to
// (BoundBy TTFT then BoundDecodeAdmission, flagged HeldByDecodeAdmission);
// Basis.TTFT.PrefillOnlyRate is the prefill side's own rate. If
// the TTFT at no load, the floor, already reaches the target, it refuses
// (TargetBelowFloor). The operating-point cap is not applied: the fleet
// carries the smaller of the roles' rates, then the cap, as the caller
// combines them.
//
// Basis.TTFT carries the lower knee, the TTFT floor and the prefill side's
// own rate; the TPOT fields (KneeRate, Floor, Uncapped) stay 0. Basis.Fill is
// the decode fill at the answer and Basis.Tier the decode line's tier (both
// empty with no decode side). Band is nil: the TTFT model is not banded yet.
//
// The cap at the decode onset is not conservative: the onset is where the
// MEAN endpoint score reaches 1, while the gate reads the current score, and
// E[max] >= max E, so the gate closes for part of the time below it.
func RateForTTFT(fl TTFTFleet, t TTFTTarget) Result {
	if !Finite(t.Seconds) || !(t.Seconds > 0) {
		return refuse(InvalidInput, fmt.Sprintf("TTFT target %g s", t.Seconds))
	}
	if u := checkPercentile(t.Percentile); u != nil {
		return *u
	}
	pl, u := fl.plan()
	if u != nil {
		return *u
	}
	zero := pl.price(0, t.Percentile, true, false)
	if zero.u != nil {
		return *zero.u
	}
	floor := zero.out.Floor
	if floor >= t.Seconds {
		return refuse(TargetBelowFloor, fmt.Sprintf("target %.4g s at or below the TTFT floor %.4g s", t.Seconds, floor))
	}
	limit, limitBy := pl.lowestKnee(), Bound("")
	if pl.fc.State == FlowControlOn && pl.d != nil && pl.onsets.Decode < limit {
		limit, limitBy = pl.onsets.Decode, BoundDecodeOnset
	}
	full := pl.bisect(t, limit, true)
	if !(full > 0) {
		// The floor is under the target, but by less than the bisection
		// resolves: no positive rate is known to meet it.
		return refuse(TargetBelowFloor, fmt.Sprintf("target %.4g s: no positive rate meets it (TTFT floor %.4g s)", t.Seconds, floor))
	}
	alone := pl.bisect(t, limit, false)

	var by []Bound
	flags := append([]Flag(nil), pl.flags...)
	switch {
	case limitBy != "" && full >= limit:
		by = []Bound{limitBy}
	case alone > full*(1+1e-6):
		// The target sets the rate; decode admission's share of the tail
		// holds it below what the prefill side alone allows.
		by = []Bound{BoundTTFT, BoundDecodeAdmission}
		flags = append(flags, HeldByDecodeAdmission)
	default:
		by = []Bound{BoundTTFT}
	}
	at := pl.price(min(full, limit*(1-1e-12)), t.Percentile, true, false)
	if at.u != nil {
		return *at.u
	}
	for _, f := range at.out.Flags {
		if f == BelowFittedBand || f == KVBoundSlots || f == AboveFlowControlOnset {
			flags = addFlags(flags, f)
		}
	}
	var fill float64
	var tier string
	if pl.d != nil {
		fill, _ = pl.d.fillAtReplicaRate(full / float64(pl.d.Replicas))
		tier = pl.d.Tier
	}
	return Answer{Value: full, Flags: flags, Basis: Basis{
		Fill: fill, BoundBy: by, Tier: tier,
		TTFT: &TTFTBasis{Floor: floor, KneeRate: pl.lowestKnee(), PrefillOnlyRate: alone},
	}}
}

// bisect is the largest rate below limit whose TTFT meets the target, or
// limit itself when the TTFT just under it still does (only an onset: at a
// knee the TTFT is unbounded).
func (pl ttftPlan) bisect(t TTFTTarget, limit float64, withDecodeWait bool) float64 {
	lo, _ := bisectRate(limit, func(rate float64) bool {
		p := pl.price(rate, t.Percentile, withDecodeWait, false)
		return p.u == nil && p.out.Seconds <= t.Seconds
	})
	return lo
}

// addFlags appends each flag not already in flags.
func addFlags(flags []Flag, add ...Flag) []Flag {
	for _, f := range add {
		if !slices.Contains(flags, f) {
			flags = append(flags, f)
		}
	}
	return flags
}
