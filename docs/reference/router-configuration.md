# Router configuration

What an operator sets on the llm-d inference router (the Endpoint Picker, EPP)
so that the signals the scaling manager reads mean what it assumes they mean.

The scaling manager does not configure the router and does not route anything.
It *reads* the router, and two of the router's settings decide what it sees:

- **flow control** bounds the gateway queue, and the scaling manager reads that
  queue as demand;
- **scheduling profiles** decide which replica each request goes to, and so
  whether replicas the scaling manager orders are actually used.

Neither is wrong by default. Both have defaults that suit a short-prompt
workload and behave differently under long prompts, and neither announces that
it is the thing limiting you.

## Flow control bounds the demand signal

`internal/collector/registration/saturation.go` and `throughput_analyzer.go`
both read

```
llm_d_epp_flow_control_queue_size or inference_extension_flow_control_queue_size
```

as the model's queued demand. Requests sitting in that queue have reached no
engine yet; they are what `estimateSchedulerQueueDemand` charges to the roles,
and on a disaggregated fleet they are most of what sizes prefill. **However the
router is configured to bound that queue is the ceiling on the demand the
scaling manager can see.**

Ask the router what it resolved, rather than reading the ConfigMap — defaults
are filled in at load and the ConfigMap does not show them:

```bash
kubectl -n <ns> logs deploy/<model>-router-epp -c epp \
  | grep -o 'FlowControlConfig:{.*}' | head -1
```

An unconfigured `llm-d-router-endpoint-picker:v0.9.0` answers with, in
substance:

```
Controller:{DefaultRequestTTL:0s ExpiryCleanupInterval:1s ...}
Registry:{MaxBytes:0 MaxRequests:0
          PriorityBands:[{Priority:0 Queue:ListQueue MaxBytes:1000000000 MaxRequests:0}]}
```

Read that as three facts:

| setting | value here | meaning |
|---|---|---|
| `DefaultRequestTTL` | `0s` | **no expiry.** A queued request waits until the client gives up. |
| `MaxRequests` (band) | `0` | no count bound applied on this version. |
| `MaxBytes` (band) | `1000000000` | 1 GB — the only bound that actually applies. |

**Check your own version rather than copying this table.** These are the values
*this* router resolved with no `flowControl` block configured; defaults differ
between router versions, and a newer one defaults `defaultRequestTTL` to 60s
and gives each band a count bound. The command above prints what yours
actually resolved, which is the only answer that is true for your deployment.

The practical consequence is that the queue is bounded by *bytes*, and prompt
length decides how many requests that is. Measured on a 30,000-token prompt
trace at 12 req/s against a fleet that could not keep up:

| | observed |
|---|---|
| queue depth, peak | **4,772 requests** |
| queue bytes, peak | **814 MB — 81% of the 1 GB band cap** |
| mean time a request spent queued | **181 s** |
| requests evicted, all as `client disconnected: request evicted from queue: request context cancelled` | 246 |

At ~170 KB per queued request the byte cap is reached at roughly 6,000
requests, so on this workload *bytes* bind first and request count never does.
On a 500-token prompt the same cap is tens of thousands of requests, and the
queue is effectively unbounded. The same configuration therefore behaves
completely differently depending on prompt length, which is the part worth
knowing before an incident rather than during one.

Two consequences for the scaling manager:

1. **An unbounded queue is an unbounded demand signal.** The scaling manager
   will keep ordering replicas for work that is queued, including work whose
   client has already timed out. The 246 evictions above were all
   `client disconnected` — not the router shedding by policy, because with
   `DefaultRequestTTL: 0s` it never does.
2. **A request queued for 181 seconds is not demand you can serve.** If your
   service objective is a 30-second time-to-first-token, a request that has
   been queued for three minutes is already a failure; scaling for it buys
   nothing and costs GPUs.

### A TTL has to cover two different kinds of waiting

There are two reasons a request cannot dispatch, and they want opposite
budgets:

| regime | meaning | what waiting buys | right budget |
|---|---|---|---|
| **saturated** | endpoints exist, all busy | a slot frees in seconds | short — keep TTFT inside the objective |
| **empty pool** | no endpoints at all | a pod starts: image pull + weight load | long — minutes |

**This matters more here than on most deployments, because the scaling manager
scales to zero.** Every scale-from-zero is the empty-pool regime, and the wait
is a pod start. Measured on this stack, pod startup is **60-64 seconds**. A TTL
chosen for a time-to-first-token objective — 30 seconds, say — sheds every
scale-from-zero request about half a minute before the pod it was waiting for
becomes ready, and the autoscaler looks like it failed to scale when it was
the router that gave up.

v0.11.0 and newer split the two (`defaultRequestTTL` for the saturated regime,
`noEndpointRequestTTL` for the empty pool, each charged from the later of
enqueue time and the last regime change, so a regime change restarts the
budget). **v0.9.0 does not**: its resolved configuration carries
`DefaultRequestTTL` and no empty-pool budget at all, so one number serves both
regimes and the only safe choice is the longer one. That is why the scenarios
in this repo pin v0.11.0.

### Which router version you need

The two-budget split is the difference between a scale-to-zero deployment that
works and one that sheds its own cold starts, so it decides the version. Taken
from the shipped binaries rather than release notes:

| flow-control feature | v0.9.0 | v0.11.0 |
|---|---|---|
| `noEndpointRequestTTL` — the scale-from-zero waiting room | **absent** | present |
| `enableEviction` — in-flight eviction | absent | present |
| `priority-holdback-policy`, `soft-reflective-ceiling-policy` | absent | present |
| `sheddable-eviction-filter` | absent | present |
| `slo-deadline`, `edf-ordering`, round-robin and program-aware fairness | present | present |

**On v0.9.0 there is no value of `defaultRequestTTL` that serves a
scale-to-zero deployment.** Short enough for a time-to-first-token objective
kills cold starts; long enough for a cold start abandons the objective
whenever the pool is merely busy. Upgrade, or accept one of those.

#### Migrating to v0.11.0: three changes

A P/D config does not start on v0.11.0 unmodified. Each of these is a startup
failure, found by deploying and reading the error, and none of them is in a
release note:

**1. `disagg-headers-handler` is gone.**

```
configuration validation failed: plugin type 'disagg-headers-handler' is not registered
```

Upstream `runner.go` registers only `disagg-profile-handler` — now
`disagg.HandlerFactory`, via `RegisterWithPluginDependencies` — and the three
PD deciders. The two v0.9.0 types `disagg.HeadersHandler` and
`disagg.PdProfileHandler` collapsed into one `disagg.Handler` taking a
`StageOrder`. So the handler is **absorbed, not renamed**: delete the line,
do not look for a replacement. (`header-profile-handler` exists in v0.11.0 and
is *not* it — that is an unrelated Alpha plugin that picks a profile from a
header.)

**2. `deciderPluginName` was removed.**

```
failed to parse parameters of the disagg-profile-handler
  json: unknown field "deciderPluginName"
```

Use `deciders.prefill`. v0.9.0 had been warning about this at every startup:
*"Deprecated parameter 'deciderPluginName', use 'deciders.prefill' instead"* —
worth grepping your current router's log for deprecation warnings before any
upgrade, since they are the migration notes.

```yaml
- type: disagg-profile-handler
  parameters:
    deciders:
      prefill: always-disagg-pd-decider
```

**3. The metrics data source has to be declared.** This one does not crash, and
is the dangerous one.

v0.9.0's framework injected a working `metrics-data-source`; v0.11.0's
auto-created producer does not reach the engines. With no engine metrics every
endpoint reads stale, and the utilization detector scores a stale endpoint as
**fully saturated** — it fails closed. The router starts, reports healthy, and
then a single request against an idle fleet takes 19-30 s or sheds at the TTL
with a 429. Declare it explicitly:

```yaml
- type: metrics-data-source
  parameters:
    scheme: "http"
    path: "/metrics"
    insecureSkipVerify: true
- type: core-metrics-extractor
```

After those three, the `flowControl` block parses and resolves as written:
`DefaultRequestTTL: 30s, NoEndpointRequestTTL: 3m0s, PriorityBands:
[{Priority: 0, MaxBytes: 1073741824, MaxRequests: 2000}]`.

**Verify the P/D split afterwards, not just that it starts.** A wrong answer in
the disaggregation plugins does not crash — it quietly routes everything to one
role, and a benchmark still produces plausible numbers. Drive a dozen
completions and require *both* roles' `vllm:request_success_total` to move:

```bash
# before and after driving traffic, per role
kubectl -n <ns> exec <pod> -c vllm -- \
  sh -c 'curl -s localhost:$VLLM_INFERENCE_PORT/metrics | grep ^vllm:request_success_total'
```

Measured here after the migration: completions in 27-120 ms (v0.9.0 was
44-113 ms), and 12 requests incremented both roles by exactly 12.

And **pin the router version** rather than inheriting it from a benchmark
harness's anchor. An unpinned router changes how requests are spread and how
the demand queue is bounded between one run and the next, with nothing in the
output saying so.

### What to set

```yaml
# EndpointPickerConfig  (v0.11.0 or newer)
flowControl:
  defaultRequestTTL: 30s        # saturated: your TTFT objective
  noEndpointRequestTTL: 180s    # empty pool: pod start + margin.
                                # Size this from YOUR measured startup --
                                # 60-64 s on this stack, so 180 s is ~3x.
  priorityBands:
    - priority: 0
      maxRequests: 2000         # a COUNT bound, so behaviour stops depending
                                # on prompt length
      maxBytes: 1Gi
```

On v0.9.0, with no split available, the only safe single value is one that
clears pod start — `defaultRequestTTL: 180s` — and the TTFT objective goes
unserved while the pool is busy.

Two things that are easy to get wrong:

**A TTL only acts if the caller waits at least as long.** Pair it with a
gateway request timeout no shorter than the TTL. Otherwise the client
disconnects first and the request is evicted as a cancelled context rather than
shed by policy — which is exactly what the 246 evictions above were, every one
of them the same message as above, ending `request context cancelled`. The
router never got
to apply a budget because it had none, and the caller supplied the only bound.

**Set the count bound explicitly.** With only a byte bound, the depth at which
the router starts rejecting moves by an order of magnitude when prompt length
does, and nothing in the configuration says so. Note that on some router
versions a per-band `0` means "unset, take the default" rather than "no limit",
so leaving it at `0` does not reliably mean unbounded — read the resolved
configuration from the log rather than assuming either reading.

### The prerequisite that fails closed

Dispatch depends on **fresh** model-server metrics. The default saturation
detector scores an endpoint whose metrics are older than its staleness
threshold as *fully saturated* — it fails closed, not open. If the router loses
its scrape path to every endpoint (a NetworkPolicy, a port change, TLS, a
starved refresh loop), saturation pins high, dispatch stalls, and queued
requests leave at their TTL — or, with no TTL, never.

Keep the metrics refresh interval comfortably inside the staleness threshold.
Watch `inference_extension_flow_control_pool_saturation` alongside queue size:
on the run above it peaked at 28.8 while the queue was 4,772 deep, so
saturation was being reported — the queue simply was not bounded to act on it.

## Scheduling profiles decide whether ordered replicas get used

A P/D router carries one profile per role. The stock prefill profile is:

```yaml
- name: prefill
  plugins:
    - pluginRef: prefill-filter
    - pluginRef: prefix-cache-scorer
      weight: 3
    - pluginRef: queue-scorer
      weight: 2
    - pluginRef: kv-cache-utilization-scorer
      weight: 2
```

Scores are normalised to `[0,1]` and weighted-summed, so **the largest weight
wins ties it should not**. At `prefix-cache-scorer: 3` against
`queue-scorer: 2`, a prefix hit contributes up to 3 while the entire spread
between an idle replica and a saturated one contributes at most 2: a replica
that has seen the prefix attracts the request regardless of what it is already
holding.

Measured on the same trace, prefill side, at 8-10 ready replicas:

- the busiest replica held **65-100% of the waiting queue** at the median,
  where even sharing would be ~15%
- fewer than 40% of ready replicas had any queued request at all
- work per replica spread from 34% of the total down to 3%

The scaling manager ordered ten prefill replicas, roughly four times the
capacity the offered load required, and the queue still ran past 1,300. **When
a fleet is scaled out and latency does not improve, measure the spread before
scaling further** — routing can bound latency in a way no replica count
reaches.

### What to check first

Whether the affinity you are paying for exists at all. If the engines run
without prefix caching, the prefix scorer is steering toward KV that was never
retained:

```bash
# on an engine pod: is prefix caching even on?
kubectl -n <ns> get deploy <model>-prefill -o json \
  | jq -r '.spec.template.spec.containers[]|select(.name=="vllm")|.args|join(" ")' \
  | tr ' ' '\n' | grep -i prefix

# and is it being used?  0.0 everywhere means it is not
kubectl -n <ns> exec <engine-pod> -- \
  curl -s localhost:8000/metrics | grep '^vllm:prefix_cache_queries_total'
```

On a stack started with `--no-enable-prefix-caching`, those counters read `0.0`
for the life of the run on every replica, and the highest-weighted signal on
the prefill path is optimising for a cache that does not exist.

### What is NOT established

Swapping the two weights (`queue-scorer: 3`, `prefix-cache-scorer: 2`) was
measured on this workload and **did not improve prefill latency**: phase-2
time-to-first-token went from 54 s to 80 s and the busiest replica's queue
share went from 65% to 100%. That single run is not evidence the swap is
harmful either — two runs with the weights *unchanged* scored 100% and 65% on
the same measure, so the run-to-run spread is wider than the effect being
looked for.

Treat the weights as something to measure on your own workload with repeated
runs, not as a setting with a known-good value. The defensible statement today
is narrower: *if* your prefill replicas are unevenly loaded, the scorer weights
are where to look, and the spread is measurable before you change anything.

## Which plugins to add — usually none

The router ships ordering policies, fairness policies, usage-limit policies and
a saturation detector, and most of them are driven by something the *client*
sends. Adding one the traffic cannot drive changes nothing and makes the
configuration harder to read. Check the input before the plugin:

| plugin | needs | inert without it |
|---|---|---|
| `round-robin-fairness-policy`, `program-aware-fairness` | `x-llm-d-inference-fairness-id` per request | every request lands in one `default-flow` and there is nothing to rotate between |
| `slo-deadline-ordering-policy` | a TTFT SLO header, in milliseconds | all requests get a far-future deadline and sort identically |
| sheddable-band tuning (`defaultNegativePriorityBand`) | an `InferenceObjective` with `priority < 0` | unclassified traffic defaults to priority 0, which is **non-sheddable**, so the negative band is never used |
| `priority-holdback-policy` | several priorities *and* `--allow-experimental-plugins` | nothing to hold back |

A benchmark harness sending plain OpenAI completions drives none of these, and
nor does most single-tenant production traffic. The flow-control block above is
the configuration that matters; the plugin surface is for when you have
genuinely distinct classes of traffic to separate, and then the first step is
creating the `InferenceObjective`s that classify them.

One that is worth knowing about even unconfigured: sheddability is derived
purely from `InferenceObjective.spec.priority < 0`. There is no `sheddable:
true` field, and traffic with no matching objective is non-sheddable. So a
cluster with no `InferenceObjective`s at all cannot shed anything by priority,
whatever the bands say.

## Changing router configuration

**The router reads its configuration once, at startup.** It is passed
`--config-file /config/<name>.yaml` and there is no reload or watch. The
`/config` volume is mounted without `subPath`, so an edited ConfigMap *does*
appear in the pod within about a minute — but nothing re-reads it, so the
change has no effect until the process restarts.

**Restarting the router is a brief outage, not a rolling update.** On a stock
deployment:

- `replicas: 1`
- `strategy: Recreate` — the old pod is terminated *before* the new one starts
- the pod carries both the Envoy proxy and the EPP, and the model Service
  points at it, so it is the data plane and not a sidecar

There is no overlap and no PodDisruptionBudget. Every in-flight request is
dropped and new ones are refused until the readiness probe passes, typically
10-30 seconds. **Change router configuration while the fleet is idle.** During
a benchmark or a production window it will corrupt the run or drop traffic.

```bash
kubectl -n <ns> patch cm <model>-router-epp --type merge --patch-file patch.json
kubectl -n <ns> rollout restart deploy/<model>-router-epp
kubectl -n <ns> rollout status  deploy/<model>-router-epp --timeout=180s
```

Then confirm what the router actually loaded, not what the ConfigMap says —
defaults and plugin instantiation happen at load, and only the log shows the
effective result:

```bash
kubectl -n <ns> logs deploy/<model>-router-epp -c epp \
  | grep -o 'prefill:{Filters.*Scorers: \[[^]]*\]'
```

Expect the weights you set, as floats:

```
prefill:{Filters: [prefill-filter/by-label], Scorers: [prefix-cache-scorer: 3.000000, queue-scorer: 2.000000, ...]
```

## See also

- [`metrics.md`](metrics.md) — the full metric surface the scaling manager reads
- [`scaling-policy.md`](scaling-policy.md) — thresholds the queue feeds into
- [`troubleshooting.md`](troubleshooting.md) — symptoms and where to look
