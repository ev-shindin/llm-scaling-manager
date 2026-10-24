package constants

// The SLO analyzer's metrics. See docs/proposals/slo-tiers.md, "Observability".
const (
	// WVASLOTargetSeconds is the latency target a model's policy declares.
	// Labels: namespace, model_name, objective, statistic.
	WVASLOTargetSeconds = "wva_slo_target_seconds"
	// WVASLOObservedSeconds is the latency the router observed over its window:
	// the mean for TPOT, the configured percentile for TTFT. Absent when the
	// router exported too few requests to judge.
	// Labels: namespace, model_name, objective, statistic.
	WVASLOObservedSeconds = "wva_slo_observed_seconds"
	// WVASLOAttainmentRatio is the share of requests whose TTFT met the target.
	// Absent below the sample minimum, never 0 or 1 by default.
	// Labels: namespace, model_name, objective, statistic.
	WVASLOAttainmentRatio = "wva_slo_attainment_ratio"
	// WVASLOObjectiveRatio is the share of requests a TTFT objective promises
	// within its target: 0.90 for p90. The attainment alert compares the
	// request-weighted attainment with it, since the percentile is otherwise
	// only in the statistic label's text. Published for TTFT only.
	// Labels: namespace, model_name, objective, statistic.
	WVASLOObjectiveRatio = "wva_slo_objective_ratio"
	// WVASLOFill is the KV fill, as a fraction of physical KV, a variant's SLO
	// capacity is priced at: where its mean inter-token latency meets the TPOT
	// target, clamped to the policy's ITL band ([0.15, 0.80] by default) and lowered where max_num_seqs binds
	// first, so the line's ITL there is at or below the target. Before any
	// scale-up headroom. Absent when the variant could not be priced.
	// Labels: namespace, model_name, variant_name, objective.
	WVASLOFill = "wva_slo_fill"
	// WVASLOPredictedITLSeconds is the mean inter-token latency the variant's
	// line predicts at the fill the fleet would actually run at once the
	// scale-up headroom is applied: the margin an operator really has.
	// Labels: namespace, model_name, variant_name.
	WVASLOPredictedITLSeconds = "wva_slo_predicted_itl_seconds"
	// WVASLOSource is 1 for the source a variant's SLO capacity came from and 0
	// for every other, like wva_replica_capacity_source.
	// Labels: namespace, model_name, variant_name, source.
	WVASLOSource = "wva_slo_source"

	// WVASLOWindowRequests is how many requests the router recorded for the
	// objective over its window. Present whenever the router reported anything,
	// so an absent wva_slo_observed_seconds beside it means "too few requests to
	// judge", and both absent means the router reported nothing.
	// Labels: namespace, model_name, objective, statistic.
	WVASLOWindowRequests = "wva_slo_window_requests"

	// WVASLOExcluded is 1 for the reason a model's router window is not being
	// judged this cycle and 0 for the others; all 0 while it is judged. A
	// window is excluded while it covers a wake from zero or a replica
	// starting, and for a grace period after, because those requests waited out
	// capacity no steady-state sizing could have had ready; and after cycles the
	// analyzer did not see (a skipped model, a controller restart). Per
	// objective: a prefill replica starting excludes TTFT and not TPOT.
	// Labels: namespace, model_name, objective, reason.
	WVASLOExcluded = "wva_slo_excluded"

	// LabelObjective names an SLO objective: "ttft" or "tpot".
	LabelObjective = "objective"
	// LabelStatistic names the statistic an objective is judged on: "mean", or
	// "p" followed by the percentile ("p90").
	LabelStatistic = "statistic"
)

// SLO objective label values.
const (
	SLOObjectiveTTFT = "ttft"
	SLOObjectiveTPOT = "tpot"
)

// SLO source label values: where a variant's SLO capacity came from, or why
// there is none.
const (
	// SLOSourceOwn: the variant's own ordinary least-squares ITL fit.
	SLOSourceOwn = "own"
	// SLOSourcePinned: a fit with the intercept pinned to this variant's last
	// full fit, because the fleet's readings did not span enough of k to fit
	// both coefficients, or the full fit was not a valid line. A balanced fleet
	// often sits here indefinitely.
	SLOSourcePinned = "pinned"
	// SLOSourcePinnedDefault: a pinned fit whose intercept is the built-in
	// floor, not one this variant was ever measured at. Not a measurement of
	// the hardware: on a large accelerator the real intercept is several times
	// the floor, and the slope fitted against it is too steep.
	SLOSourcePinnedDefault = "pinned-default"
	// SLOSourceNoOutputLength: no replica has reported an output length, so
	// neither the demand nor the per-request KV footprint is known.
	SLOSourceNoOutputLength = "no-output-length"
	// SLOSourceNoLine: too few readings in the window to fit any line: a new
	// variant warming up, replicas that report no ITL, or replicas outside the
	// band -- below a fill of 0.15 (an idle fleet) or above 0.80 (an overloaded
	// one), which can last indefinitely. The slo-variant log line counts which.
	SLOSourceNoLine = "no-line"
	// SLOSourceDeclined: the window held enough readings and the pinned fit
	// refused them, most often because the variant's ITL sits below the pinned
	// intercept (a fast card under the built-in floor) and the slope came out
	// negative. A steady state, not a warm-up.
	SLOSourceDeclined = "declined"
	// SLOSourceInfeasible: no fill in the line's range meets the target.
	SLOSourceInfeasible = "infeasible"
	// SLOSourceInputInvalid: an input was missing, non-finite or non-positive.
	SLOSourceInputInvalid = "input-invalid"
)

// Reasons a model's router window is excluded from judgement.
const (
	SLOExcludedZeroReplicas = "zero-replicas"
	SLOExcludedStarting     = "starting"
	// SLOExcludedUnobserved: the analyzer did not see the model's previous
	// cycles -- it was skipped, as a waking model whose Pods have no metrics
	// yet is, or the controller restarted -- so the window may hold a
	// transient nothing here saw.
	SLOExcludedUnobserved = "unobserved"
)

// SLOExcludedReasons is every exclusion reason, in the order they are published.
var SLOExcludedReasons = []string{SLOExcludedZeroReplicas, SLOExcludedStarting, SLOExcludedUnobserved}

// SLOSources is every SLO source value, in the order they are published.
var SLOSources = []string{
	SLOSourceOwn, SLOSourcePinned, SLOSourcePinnedDefault, SLOSourceNoLine, SLOSourceDeclined,
	SLOSourceNoOutputLength, SLOSourceInfeasible, SLOSourceInputInvalid,
}
