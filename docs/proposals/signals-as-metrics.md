# Every signal as a metric

**Status:** proposed; stage 3 (service rate and the ITL line) built. Names and
label keys below are the thing to argue about; they are the part that becomes a
compatibility promise the moment anything scrapes them.

Today a scaling decision can only be reconstructed from the controller log. The
numbers that produce it — `KVreq`, `k1`, `k2`, `mu`, `kPrice`, the ITL line,
the arrival rate, the projected backlog, `replicasImplied` — are emitted as log
fields and nowhere else. WVA exposes 41 metrics and all but a handful are
*outcomes* (`wva_desired_replicas`, `wva_saturation_utilization`) or *health*
(`wva_metrics_freshness_status`). The chain in between is invisible to a
dashboard.

This proposes exposing the chain. It also draws a line that this repository has
already argued once, and does not reopen.

## The line: observability, not actuation

"WVA as a metric shop" was proposed and rejected. The sentence that settles it is:

> A raw Prometheus metric is not enough. Right scaling needs *measured
> per-variant capacity + drift tracking*, not *metric + static threshold*. The
> metric shop ships the metric and leaves the hard half unsolved.

So, explicitly:

- **In scope.** Publishing signals so a human, a dashboard, an alert or a
  post-mortem can see *why* WVA decided what it decided, without grepping a
  40,000-line log.
- **Out of scope.** Publishing a signal for KEDA or an HPA to threshold
  *instead of* WVA computing the target. That is the rejected design, and
  nothing here should make it look available. `wva_floor_replicas_implied` is a
  diagnostic, not a scaling input — if it becomes one, the capacity model moves
  out of WVA by accident.

A second thing stays out of scope: **these metrics do not replace the log
lines.** `derived-mu` carries fifteen fields in one record precisely so that a
mu wrong by 8x can be attributed to the output length, the sequence count, the
ITL line or the pricing point. A gauge carries one number with no companions.
The log is for attribution; the metric is for a time series. Both, not either.

## The rule this must not break

`wva_model_scaling_blocked` already establishes how a *condition* is published:
a `reason` label, present only while the reason holds, each reason naming a
contradiction rather than a setting. That rule exists so a new diagnostic does
not become a new gauge nobody discovers.

So the split below is deliberate:

- a **number** that varies continuously gets a gauge;
- a **state** out of a small fixed set gets a `reason`- or `source`-style label
  on one gauge, set to 1 for the member in force;
- a **condition** that either holds or does not joins
  `wva_model_scaling_blocked` rather than growing a family.

## Label keys

One label vocabulary for everything proposed here. Getting this wrong is worse
than missing a metric, because a renamed label breaks every query written
against it.

| key | values | why |
| --- | --- | --- |
| `namespace` | the **model's** namespace | Set as `namespace`, as every per-variant WVA series is, and *scraped* as `exported_namespace`: the scrape takes `namespace` for the controller's own. So a query reads `exported_namespace`. Alerts that keyed on `namespace` matched the controller and silently returned nothing. |
| `model_name` | model ID | the key `wva_analyzer_target` and `wva_engine_config` carry, so these series join them without a `label_replace` |
| `variant_name` | variant name | the same; omitted on model-level series |
| `role` | `prefill`, `decode`, `both` | the canonical role, never the raw label |
| `window` | `1m`, `5m` | only where a signal genuinely exists at two timescales |
| `basis` | `fleet`, `priced` | the fleet's own reading vs the figure a decision used |
| `source` | `measured`, `derived`, `borrowed-line`, `neighbour`, `observed`, `historical`, `fallback`, `seed` | which path answered |
| `bound` | `k1`, `k2` | which capacity bound |

`pod` appears on **nothing** proposed here. See cardinality.

An earlier draft keyed these `model` and `variant`. Every per-variant family
WVA already publishes uses `model_name` and `variant_name`, and a new family
on different keys could not be joined to `wva_analyzer_target` -- the series it
exists to explain -- without a `label_replace` in every query. Every metric
block below uses these keys, built or not.

`source` gained `neighbour` and `borrowed-line` in stage 3. `neighbour` is a
service rate read from a neighbouring output-length bucket's window because the
bucket being served has no reading of its own yet; `borrowed-line` is one
derived from a line another variant fitted. The floor may hold a fleet on
either but not grow one, so each is a source of its own rather than a qualifier
on `measured` or `derived`.

## The metrics

### The shape

```
wva_shape_input_tokens{namespace,model_name,role,window}      gauge
wva_shape_output_tokens{namespace,model_name,role,window}     gauge
wva_shape_prefix_hit_rate{namespace,model_name,role}          gauge
wva_shape_kv_tokens_per_request{namespace,model_name,role,basis}  gauge
wva_shape_change_active{namespace,model_name}                 gauge  0|1
```

`wva_shape_kv_tokens_per_request` with `basis` is the one to build first. It is
`KVreq`, the quantity everything that asks "how many requests fit" divides by,
and the `fleet` / `priced` pair is exactly the divergence that cost seven
decode replicas for seven minutes on a 6000/1000 → 1000/4000 swap: priced 3000
while the fleet reading still said 5896. On a dashboard that is two lines
separating and rejoining; in the log it was findable only once someone
suspected it.

`window` carries `1m` and `5m` because the short window is a real second
reading, not an implementation detail — the pairing of the two is what the
analyzer prices mu at during a shape change.

### Per-replica capacity

```
wva_replica_capacity_bound_tokens{namespace,model_name,variant_name,role,bound}  gauge
wva_replica_capacity_tokens{namespace,model_name,variant_name,role}              gauge
wva_replica_capacity_source{namespace,model_name,variant_name,role,source}      gauge  0|1
```

`bound` publishes `k1` and `k2` as two series; `wva_replica_capacity_tokens` is
the effective figure, so "which bound is binding" is a comparison rather than a
string. `wva_replica_capacity_source` follows the reason-label pattern for k2's
priority order (`observed`, `historical`, `derived`, `fallback`) — 1 for the one
that answered.

### The service rate

```
wva_service_rate_per_replica{namespace,model_name,variant_name,role,source}  gauge
wva_pricing_k{namespace,model_name}                                          gauge
wva_itl_line_slope{namespace,model_name,variant_name}                        gauge
wva_itl_line_intercept{namespace,model_name,variant_name}                    gauge
wva_itl_seconds_at_pricing_k{namespace,model_name,variant_name}              gauge
wva_itl_line_samples{namespace,model_name,variant_name}                      gauge
```

Built (stage 3). `source` is `derived`, `borrowed-line`, `measured` or
`neighbour`; the rate is
the median over the variant's own replicas, never a bridge, and the series is
absent while none has a rate. The ITL series are decode-only, because only
decode fits a line; `wva_itl_line_samples` counts the variant's *own* window
even when it is priced on a borrowed line. See
[the reference](../reference/prometheus.md#service-rate).

`wva_pricing_k` is worth its own series despite being derived from
configuration, because it is *composed* — `kvCacheThreshold × scaleUpThreshold`,
0.68 with the shipped defaults, not the 0.85 that `k_sat` suggests. Publishing
it stops the recurring argument about which number the autoscaler targets.

`wva_itl_line_samples` is the one that explains an *absence*: a fleet with no
fitted line produces no derived mu and therefore no floor, and the reason is
almost always the sample count or the `k` spread rather than anything wrong.

### The throughput floor

```
wva_floor_arrival_rate{namespace,model_name,role}                   gauge
wva_floor_backlog_requests{namespace,model_name,role,basis}         gauge
wva_floor_replicas_implied{namespace,model_name,role}               gauge
wva_floor_per_replica_capacity{namespace,model_name,role}           gauge
wva_floor_held{namespace,model_name,role,reason}                    gauge  0|1
```

`basis` here is `observed` and `projected` — the queue now, and the queue a
replica ordered now would land into (`backlog + λT − μ(ready·T + credit)`).
The gap between them is the whole argument for ordering on a backlog, and it is
currently a log field nobody plots.

`wva_floor_held` takes a `reason` (`shape-change`, `insufficient-samples`,
`borrowed-only`) rather than becoming three gauges.

### Demand attribution

```
wva_demand_tokens{namespace,model_name,role,source}  gauge
```

`source` is `replica` (what running pods hold) and `scheduler_queue` (what the
router holds and no pod has started). On a P/D split those are charged to
different roles — prefill owes the discounted prompt, decode owes the
generation — and that attribution has been the subject of four separate fixes.
One metric with a `source` label makes it visible.

## Cardinality

The thing that kills a proposal like this. Worst case per model:

```
roles            3   (prefill, decode, both -- in practice 2)
variants         2   (one per role, typically)
windows          2   (only on 2 of the shape metrics)
bound/basis/src  <= 4
```

Shape: 2 metrics × 2 roles × 2 windows + 3 × 2 roles ≈ **14 series/model**.
Capacity: 3 × 2 variants × (2 bounds or 4 sources) ≈ **16**.
Service rate: 6 × 2 variants ≈ **12**, plus 1 model-level.
Floor: 5 × 2 roles, with `basis` and `reason` ≈ **16**.
Demand: 2 roles × 2 sources = **4**.

**About 60 new series per model**, against roughly 40 today. For a controller
managing 20 models that is 1,200 series — unremarkable for Prometheus, and
bounded by *configuration* rather than by traffic, which is the property that
matters.

What makes it unbounded, and is therefore excluded:

- **`pod` as a label.** Per-replica series scale with the fleet, which is the
  thing being autoscaled — a 10× scale-up would 10× the series. Per-replica
  figures are already aggregated to the variant before a decision uses them, so
  the variant is the honest granularity. Per-pod belongs in the log.
- **The output-length bucket as a label.** `short`/`medium`/`long`/`xlong`/
  `vlong` × roles × variants multiplies for a key that changes only when the
  workload shape changes. `wva_shape_output_tokens` already carries the number
  the bucket is derived from.
- **Per-reason gauges.** Hence the label rule above.

## Making it not rot

Thirty hand-written `Set` calls scattered through the analyzer will drift from
the signals they describe — the same way three copies of one weighted mean did.
The mechanism matters as much as the list:

Declare each signal once, beside the computation that produces it, and have the
emitting be a consequence of computing it rather than a second call someone can
forget. Concretely: the analyzer already assembles every one of these numbers
into a log record at a single point per stage (`derived-mu`,
`replica-capacity-decision`, `throughput-demand-floor`). Those three points are
where the metrics should be set, from the same struct, so a field that reaches
the log reaches the metric by construction.

That also gives the test: **a signal in one of those three log lines and absent
from the metric registry is a failure.** A table test over the field names, so
adding a log field without a metric fails rather than being noticed later.

## An opt-out, not an opt-in

Default **on**. A diagnostic that has to be enabled is not available in the
incident that needs it, which is the lesson `LOG_LEVEL` already taught here —
the knob reached nothing and the real verbosity was `-v`, so the one run that
needed detail did not have it.

For the operator who disagrees, one knob — `metricsDetail: outcomes | full`,
default `full`, where `outcomes` publishes only today's 41. Not per-family
switches: a dozen booleans is a configuration surface nobody can reason about,
and the cardinality above does not justify one.

## Staging

1. **The shape family**, `wva_shape_*`. Smallest, and it carries
   `kv_tokens_per_request{basis}`, the highest-value single series here.
2. **The floor family.** Second because `replicas_implied` against
   `wva_desired_replicas` is the most common question asked of a scaling
   decision after the fact.
3. **Service rate and the ITL line.** Includes `wva_itl_line_samples`, which
   explains absences. **Built**, with the drift test below scoped to
   `derived-mu` (`TestDerivedMuFieldsArePublished` in the saturation analyzer
   package): every key of that log line is mapped to a registered metric or
   excluded with a reason, and a key added without either fails.
4. **Capacity and demand attribution.**
5. **The drift test** above, which should arguably be first — but it needs one
   family in place to be written against. Started with stage 3 for
   `derived-mu`; `replica-capacity-decision` and `throughput-demand-floor`
   join it as their families are built.

Each stage is independently useful, and none changes a decision. A stage that
changes a decision has a bug: these are observations of arithmetic that already
happens.

## What this does not solve

- **Thresholds and drift.** Publishing `mu` does not make `mu` right. The
  metric shop's unsolved half stays unsolved; this just makes the current
  answer visible.
- **The dashboards.** Series are not panels. `make dashboards-check` exists and
  the panels are a separate piece of work.
- **Historical comparison across a restart.** Every signal here is in-memory
  and starts empty; a restart shows a gap, and the ITL families stay absent for
  the ~28 cycles a fit takes, measured from load start (an earlier ~51 was
  counted from the controller's boot; the correction is PR #116's). This
  document called that gap honest rather than fixable. It is fixable, but not
  by reading these series back: a `wva_` series has no writer identity (the
  ServiceMonitor drops `pod`) and no honest age (`client_golang` cannot publish
  a historical timestamp), so publishing a signal is safe and reading back one
  WVA itself wrote is not. The learned state is carried across a restart in a
  ConfigMap snapshot instead; see
  [learned state across a restart](learned-state-snapshot.md).
  `wva_itl_line_samples` still makes the gap legible where it remains.
