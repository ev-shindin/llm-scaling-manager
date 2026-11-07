# Comparisons

How the scaling manager's autoscaling compares with the other systems in this space. Written to
be checkable: each claim is about a named mechanism, not a marketing position,
and where a competitor's decision logic is closed the comparison says so rather
than guessing.

Our side was last checked on 2026-10-25, against v0.10.0 (rc4 and the fixes
merged after it). The competitor sections keep the dates they give.

- **[KEDA alone](keda-alone.md)** — the one most operators weigh first: llm-d's
  own KEDA recipes against llm-scaling-manager, need by need, with one July 2026
  study of five scenarios, one run each,
  [what has changed since](keda-alone.md#what-changed-since-the-july-study),
  and the runs measured since.
- **[Sharing a GPU budget between models](gpu-sharing.md)** — moving GPUs from
  a model whose load fell to one whose load rose: what llm-d offers today
  (its experimental Kueue guide: floors, borrowing and preemption) and what
  the utilization-share optimizer adds, and when the two work together.
- **[NVIDIA Dynamo, Mooncake and SGLang](autoscaling-nvidia-mooncake-sglang-vs-wva.md)**
  — the detailed comparison. Dynamo's Planner is the only true peer autoscaler
  of the three, and the only one with forecasting; Mooncake is a load-shedding
  scheduler rather than an autoscaler; SGLang ships no native autoscaler and is
  driven by external HPA/KEDA rules.
- **[Dynamo, Fireworks and Together, on one page](autoscaling-one-pager-dynamo-fireworks-together.md)**
  — the same ground against the hosted platforms, compressed: which signals each
  one scales on, and how many of them you are allowed to use at once.

The Dynamo, Mooncake and SGLang pages benchmark nothing. The only measured
head-to-head is against KEDA thresholds: one July 2026 study of five scenarios,
one run each, on an older controller
and recipe; [KEDA alone](keda-alone.md#measured-against-keda-alone) says what
still holds. Since then the warm pool, the P/D path and quota sharing between
models have been measured on their own, not against KEDA; the
[table](keda-alone.md#what-was-measured-and-what-failed) lists each run, with
the one that failed.

These pages compare mechanisms, not projects.
