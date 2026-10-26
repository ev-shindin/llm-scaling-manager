# Roadmap

Planned work that changes what an operator can rely on, after v0.10.0. Each
item links the GitHub issue that tracks it, with the detail and the current
state. The gaps themselves are summarized in [known gaps](why.md#known-gaps).

## GPU budgets

- **Model Kueue more closely.** Kueue is read per namespace today, and cohort
  borrowing is counted only on a flat cohort without Fair Sharing or
  `effectiveQuotas`. Planned: read Kueue per ClusterQueue
  ([#213](https://github.com/ev-shindin/llm-scaling-manager/issues/213)), a
  claims ledger ([#212](https://github.com/ev-shindin/llm-scaling-manager/issues/212)),
  closing the known limits of borrowing and hand-back
  ([#214](https://github.com/ev-shindin/llm-scaling-manager/issues/214)), and
  rescale with mixed replica sizes, which rounds per role and fills greedily
  ([#219](https://github.com/ev-shindin/llm-scaling-manager/issues/219)).
  · today: [Kueue quotas](well-lit-paths/kueue-bounded-quotas/), [the guide](guides/kueue-borrowing/)

## Scaling

- **Scale to a latency target.** Tiers set how full a replica may run, not a
  TTFT or TPOT target; an operator with an SLO translates it into thresholds by
  hand. A tier can already declare a latency target and see it judged
  ([observe only](reference/scaling-policy.md#slo-observe-only)). Planned: the
  controller derives the fill that meets it from measured latency, keeping
  variants, P/D pairing and the GPU budget.
  [#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141)
- **Judge each analyzer on its own band.** Today every analyzer's need is
  judged against saturation's thresholds
  ([#305](https://github.com/ev-shindin/llm-scaling-manager/issues/305)).
  Open: whether one analyzer's score should weigh a model across models
  ([#226](https://github.com/ev-shindin/llm-scaling-manager/issues/226)).
- **Size prefill when only decode saturates.** v0.10.0 ships an opt-in lower
  bound for prefill
  ([`enablePrefillThroughputBound`](reference/scaling-policy.md#enableprefillthroughputbound),
  [#263](https://github.com/ev-shindin/llm-scaling-manager/pull/263)).
  Planned: cluster evidence for it, and for a decode at its sequence cap,
  before either is relied on.
  [#320](https://github.com/ev-shindin/llm-scaling-manager/issues/320)
- **Price a replica more exactly.** Speculative decoding
  ([#274](https://github.com/ev-shindin/llm-scaling-manager/issues/274)), mixed
  prefill and decode pods ([#275](https://github.com/ev-shindin/llm-scaling-manager/issues/275)),
  output length ([#276](https://github.com/ev-shindin/llm-scaling-manager/issues/276)),
  prefix-cache hits ([#277](https://github.com/ev-shindin/llm-scaling-manager/issues/277)),
  KV-landed P/D requests ([#307](https://github.com/ev-shindin/llm-scaling-manager/issues/307))
  and `--max-num-active-seqs` ([#308](https://github.com/ev-shindin/llm-scaling-manager/issues/308)).
- **Utilization share.** Call off a one-replica move when the donor's demand
  comes back ([#285](https://github.com/ev-shindin/llm-scaling-manager/issues/285)),
  and close the known limits of in-flight holds
  ([#215](https://github.com/ev-shindin/llm-scaling-manager/issues/215)).

## Resilience

- **Re-wire an unwired ScaledObject without an operator.** On KEDA 2.19 and
  older, a ScaledObject created soon after the controller restarts can keep its
  HPA on the CPU default. It is a KEDA bug, fixed in KEDA 2.20; upgrade KEDA to
  2.21. On older KEDA, `make scaledobjects-rewire` fixes it
  ([KEDA](reference/keda.md#unwired-hpa-before-keda-220)). Considered: the
  controller re-wires it on its own on older KEDA.
  [#315](https://github.com/ev-shindin/llm-scaling-manager/issues/315)
- **Make surviving a restart the default.** By default what the controller
  learns about a model's capacity lives in memory, so a restart or a leader
  hand-off re-learns it. `LEARNED_STATE_SNAPSHOT=true` keeps it in the
  ConfigMap `wva-learned-state`; it is off by default
  ([learned state across a restart](reference/configuration.md#learned-state-across-a-restart)).
  Before it can be the default: restore a held ITL line
  ([#316](https://github.com/ev-shindin/llm-scaling-manager/issues/316)), speed
  up mu without `enableLearnedStateReuse`
  ([#317](https://github.com/ev-shindin/llm-scaling-manager/issues/317)), and run
  the cases not yet run on GPUs
  ([#228](https://github.com/ev-shindin/llm-scaling-manager/issues/228)), test
  the rules no test reaches
  ([#229](https://github.com/ev-shindin/llm-scaling-manager/issues/229)), and
  measure the ITL window and `wva_engine_config` on a real fleet
  ([#230](https://github.com/ev-shindin/llm-scaling-manager/issues/230)).
  [#227](https://github.com/ev-shindin/llm-scaling-manager/issues/227)

## Evidence

- **Prove variant choice, and measure more.** An end-to-end spec that the
  cheaper variant grows, a benchmark across two GPU types, repeated runs with
  their spread, larger models, and a head-to-head with llm-d's recipes on their
  own published setups.
  [#142](https://github.com/ev-shindin/llm-scaling-manager/issues/142)
- **Nightly end-to-end on real GPUs.** CI runs the suites on kind with a
  simulator; a scheduled run on a GPU cluster would catch real-engine
  regressions (metrics, sleep and wake, drain, P/D transfer).
  [#143](https://github.com/ev-shindin/llm-scaling-manager/issues/143)

Other open issues and known issues in v0.10.0, such as the router scrape that
returns HTTP 500
([#318](https://github.com/ev-shindin/llm-scaling-manager/issues/318)), are in
the [release notes](https://github.com/ev-shindin/llm-scaling-manager/releases/tag/v0.10.0).
