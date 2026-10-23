# Troubleshooting

From a symptom to a fix. Every command uses `$NS` for the namespace the
**controller** runs in (see [Verifying the install](operations.md#verifying-the-install)),
and `<ns>` for the model's namespace, which is the same for a namespace-scoped
install. Steps marked **(cluster admin)** need rights a namespace owner does not
have.

Most runbooks start from the controller's two per-cycle log lines
([cycle log](cycle-log.md)). Set this once:

```bash
LOGS="kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler --tail=-1"
```

## Symptom index

| symptom | first signal | likely causes | runbook |
| --- | --- | --- | --- |
| will not scale up | `scaling-decision` shows `tgt` equal to `curr` under load; `limitedBy` in it names a limiter that held it back; `wva_model_scaling_blocked{reason="no-composite-signal"}` (it blocks scaling only when `saturation` is not listed) | utilization below `scaleUpThreshold` by the controller's measure, at its own max, a limiter, unscraped pods, stale metrics, no listed analyzer able to size it | [will not scale up](#a-model-will-not-scale-up) |
| one role (often prefill) never moves | `composite holds a role` in the controller log; no blocked reason is set | a policy without `saturation` whose analyzers do not size that role, or a listed analyzer inside its staleness grace | [will not scale up](#a-model-will-not-scale-up), step 3 under `no-composite-signal` |
| will not wake from zero | `WVAModelParkedWhileRequestsRefused`; `wva_model_scaling_blocked{reason=~"no-wake-signal\|wake-no-capacity\|wake-nominal-lent-out"}`; `wva_model_replicas == 0` with requests queued | EPP flow control off or EPP not restarted, the direct EPP scrape failing, InferencePool API group mismatch, a limiter with no room for the wake, a borrowing cooldown | [will not wake](#a-model-will-not-wake-from-zero) |
| will not park at zero | `wva_model_scaling_blocked{reason=~"variant-floor\|policy-forbids-zero\|activation-retention\|engine-unsupported"}`; `WVAModelWillNotScaleToZero` | a variant floor, a policy that forbids zero, a just-woken model, a model running both vLLM and SGLang, an idle counter that does not exist yet | [will not park](#a-model-will-not-park-at-zero) |
| flapping, or a scale-up that never lands | `WVAReplicaScalingThrashing` (it counts every cycle that is not `no-change`); `tgt` alternating in `scaling-decision`, or `tgt` steady above `curr` | utilization crossing a narrow band, short stabilization windows, utilization-share swings; or actuation stuck, Pods Pending, slow replica starts | [flapping](#a-model-is-flapping); [actuation stuck](#desired-is-not-current-actuation-stuck) |
| a lightly loaded model reads busy | `util` in `analyzer-result` far above what its load implies; `eppQueueSize` above 0 in its `scheduler-queue-demand` line while its own router queues nothing | v0.10.0-rc2 and earlier: another namespace serving the same model name, whose router queue was read as this model's | [reads busy](#a-lightly-loaded-model-reads-busy) |
| pinned at max | `wva_variant_at_max_replicas == 1`; `"atMax":true` in `scaling-decision` | `maxReplicaCount` too low for the load | [pinned at max](#a-model-is-pinned-at-max) |
| one replica full while others idle | `WVALoadImbalanced`; `wva_load_imbalanced == 1`; log `WARNING: load imbalanced` | the router's scorer weights (`prefix-cache-scorer` above `queue-scorer`, `active-request-scorer`, `kv-cache-utilization-scorer`), prefix caching off on the engines | [uneven load](#one-replica-is-saturated-while-others-idle) |
| desired is not current | `wva_desired_replicas != wva_current_replicas` for many cycles | HPA stabilization window, KEDA not reaching the scaler, Pods Pending (GPUs, ResourceQuota), slow starts | [actuation stuck](#desired-is-not-current-actuation-stuck) |
| replicas dropped under Kueue borrowing | fewer replicas with no scale-down asked for; `wva_model_scaling_blocked{reason=~"lender-reclaimed\|borrow-unplaced\|released-to-lender"}`; `BorrowedCapacityReleased` Event | Kueue preempted, a borrowed replica did not place and was released, a waiting lender was handed it back (opt-in), demand fell | [replicas dropped](#replicas-dropped-under-kueue-borrowing) |
| a lender's replica Pending after Kueue preempted | the lender's new pod `Pending` with no node; a borrower's Workload evicted with reason `Preempted`; later `reason="borrow-unplaced"` | Kueue preempts by priority, not by node: the GPUs it freed form no node the replica fits | [replicas dropped](#replicas-dropped-under-kueue-borrowing) |
| a Kueue-backed quota was never read | `WVAKueueNeverRead`; `wva_kueue_never_read == 1`; log `external quota source unreadable and never read` | Kueue CRDs absent, the controller may not list ClusterQueues, Kueue down at start | [never read](#a-kueue-backed-quota-was-never-read) |
| Kueue borrowing withheld | `wva_quota_borrow_withheld == 1`; its `reason` names the condition; `reason` in `limiterBudgets`; `withheldByNamespace` in the borrowing log line | a borrowing condition does not hold (withheld), Kueue cannot be trusted right now (frozen), or there is nothing to draw (counted, but 0) | [borrowing is withheld](#borrowing-is-withheld) |
| scaled past nominal | a model holds more GPUs than its ClusterQueue's `nominalQuota`; `used` above `guaranteed` in `limiterBudgets`; `wva_kueue_borrowed_gpus > 0` | expected: borrowing is on by default for a Kueue-backed namespace quota | [past nominal](#a-model-runs-past-its-kueue-nominal-quota) |
| a Kueue borrowing alert fires | `WVAKueueReaderStale`, `WVAKueueReaderStaleNamespace`, `WVAKueueControllerDown`, `WVAKueueBorrowDrift`, `WVAKueueVersionUnknown`, `WVAKueueVersionUntested`, `WVAKueueLenderWaitingPastCooldown` | a read is failing, the Kueue controller is not running, Kueue accounts differently, its version cannot be read or is untested, a lender is not getting its capacity back | [Kueue borrowing alerts](#kueue-borrowing-alerts) |
| GPUs held / quota spent | `ResourceConstrained` Event; `wva_decisions_limited_total` rising; `wva_model_scaling_blocked{reason="quota-short"}` | the quota or GPU inventory is used up, unresolved accelerators, a lowered quota (it does not evict) | [GPUs held](#gpus-held-or-quota-spent) |
| EPP `/metrics` answers 500 | `returned status 500` for an EPP pod in the controller log; `tokenreviews.authentication.k8s.io is forbidden` in the EPP's log | the EPP's own ServiceAccount may not create tokenreviews and subjectaccessreviews | [EPP 500](#epp-metrics-returns-500) |
| metrics stale or missing | `MetricsUnavailable` Event; `wva_metrics_freshness_status{status!="fresh"} > 0`; `WVAMetricsCollectionFailing`; `wva_unmeasured_queue > 0` | ServiceMonitor or PodMonitor not selecting the pods, Prometheus unreachable, FMA launcher pods not scraped | [metrics](#metrics-stale-or-missing) |
| controller not Ready or crash-looping | pod `0/1` or `CrashLoopBackOff`; log `refusing to start unbounded`; readiness check `gpu-policy` failing | cluster policy unreadable, an invalid `default` entry in the ConfigMap the limiters come from, a `gpu-inventory` limiter declared alone with no node read, a Prometheus setting it refuses, an image older than the manifests | [controller not Ready](#the-controller-is-not-ready-or-crash-looping) |
| `x509: certificate is valid for ..., not ...` | the controller log at startup | a Prometheus address its certificate does not name | [certificate name mismatch](#prometheus-certificate-name-mismatch) |
| cluster policy not applied | `WVAPolicyNamespaceUnverified`; `wva_policy_namespace_unverified == 1` | the controller may not read Namespaces, so it runs on its own ConfigMap | [policy namespace unverified](#cluster-policy-is-not-applied-policy-namespace-unverified) |
| ScaledObject `READY False` or `TARGETS <unknown>` | `kubectl get scaledobject,hpa -n <ns>` | `scalerAddress` names a namespace with no scaler, `modelID` missing, the controller not Ready, KEDA backing off or keeping the old client certificate after a reinstall | [ScaledObject](#scaledobject-ready-false-or-targets-unknown) |
| capacity priced high after a restart | TTFT worse after an upgrade or leader change; `P3-k2`/`P4-k1` in `replica-capacity-summary` (or `k2-decision` at `--v=3`) where `P1-obs` was before | `LEARNED_STATE_SNAPSHOT` off, the Role missing, an engine configuration that changed or could not be read | [after a restart](#capacity-priced-high-after-a-restart) |
| one variant held larger than its load needs | `replica-capacity-summary` (or `k2-decision` at `--v=3`) keeps `P2-hist`, `P2-prior` or `P2-borrow` for one variant while its replicas run light | `P2-hist`: a k2 learned from a bad reading. `P2-prior`: the variant's own k2 from before its engine arguments changed; expected after a rollout that changes them, held for 30 minutes of serving, then relaxed over 6 hours of serving. `P2-borrow`: a sibling's figure that does not fit this variant | `P2-hist` or `P2-prior`: [reset one variant](#reset-one-variants-learned-state) (`disableK2Borrow` does not affect `P2-prior`). `P2-borrow`: [`disableK2Borrow`](scaling-policy.md#disablek2borrow) |
| a new decode variant reads low utilization at light load | `itl-line-none` with `derivedMu=false`; `itl-window` shows `offered: 0` and `belowBand` equal to `replicas` | expected: below `analyzerTuning.itlBand.minObservableK` (0.15 of the KV cache) a replica teaches its ITL line nothing, so a variant that has never run in that band has no derived mu and is sized by KV occupancy alone, which at light load reads below the throughput share served. Once a line is learned it is kept through light load, up to `LEARNED_STATE_BUCKET_RETENTION` after it was fitted, until a controller restart or a change of engine arguments; `derived-mu` (at `--v=3`) then logs `lineHeld=true` | run the variant in the band once, or lower [`analyzerTuning.itlBand.minObservableK`](scaling-policy.md#advanced-tuning-analyzertuning-and-loadimbalance) |
| a saturated replica's k2 is not recorded | the replica's `replica-capacity-summary` entry ends in `/P1-obs-unsettled` (`k2-decision` at `--v=3` says more) while the replica's queue is over `queueLengthThreshold` | expected for about a minute at a load step, while a replica's batch is still filling, or when a replica first reaches a limit at low occupancy: the queue is deeper than everything the replica holds, so the reading waits until the replica completes at the rate on record while holding most of its k2 on record, or holds the queue for a minute with flat residency. A noisy KV gauge can stretch that to a few minutes. Every cycle at steady load means the queue keeps out-running a batch that is still growing | nothing to do for a few cycles; see [`P1-obs-unsettled`](cycle-log.md#reason-values-reason-field). For the first minute after the controller restarts, every replica's saturated reading reads `P1-obs-unsettled`: each waits a minute from the controller's first sight of it, and [`learner-start`](cycle-log.md#learner-start) says when that ends |
| P/D prefill grows only after its queue builds | no `throughput-demand-floor` line with `role=prefill`; no `wva_service_rate_per_replica{role="prefill"}` series | prefill has no rate yet: it never queued past `queueLengthThreshold`, or its engine does not publish `vllm:request_prefill_kv_computed_tokens` (SGLang), or its engine arguments changed. Prefill queued only behind a full decode is priced only with `enablePrefillThroughputBound` on, which is off by default | expected until prefill queues on its own once. To size prefill from the router's arrival rate sooner, turn on [`enablePrefillThroughputBound`](scaling-policy.md#enableprefillthroughputbound), and check it in the `Effective scaling policy` line |
| P/D prefill held larger than its load needs | `throughput-demand-floor` with `role=prefill` and `lowerBound=true`; `wva_service_rate_per_replica{role="prefill",source="lower-bound"}` | prefill is sized on a lower bound learned while it queued behind a full decode, which reads its capacity low | expected. It ends only when prefill queues on its own with decode healthy, which a prefill held this large may never do, so it can last. If it costs too much, [reset the variant's learned state](#reset-one-variants-learned-state), or remove [`enablePrefillThroughputBound`](scaling-policy.md#enableprefillthroughputbound) from the entry that turned it on (a per-model entry cannot turn off what the `default:` entry turned on) |
| misses its SLO, or is never judged | `WVAModelSLOAttainmentLow`; `WVAModelSLOMissed`; `WVAModelSLOUnjudged`; an empty SLO dashboard row | tier thresholds too loose for the SLO (observe only), a fleet that never holds still, too little traffic, the router not scraped or not streaming | [SLO](#a-model-misses-or-is-never-judged-against-its-slo) |
| utilization-share optimizer does nothing | `wva_utilization_share_mode`, `wva_utilization_share_actionable` | shadow mode, not planned, held, swinging | [optimizer](#the-optimizer-evaluates-but-never-moves-anything) |

Alerts that point here are listed in [Prometheus: the shipped alerts](prometheus.md#the-shipped-alerts).
Events are listed in [Monitoring: Events](monitoring.md#events).

## A model will not scale up

**Symptom**: load rises, latency rises, the replica count does not.

Read the model's last decisions:

```bash
$LOGS | grep scaling-decision | grep '"modelID": "<model>"' | tail -5
$LOGS | grep analyzer-result  | grep '"modelID": "<model>"' | tail -5
```

| what the lines show | means | go to |
| --- | --- | --- |
| `tgt` above `curr`, for several cycles | the decision is right; it is not being carried out | [actuation stuck](#desired-is-not-current-actuation-stuck) |
| `"atMax":true` | the variant is at its own `maxReplicaCount` | [pinned at max](#a-model-is-pinned-at-max) |
| `util` below `scaleUpThreshold` | by the controller's measure the model is not saturated; check the measure next | the metrics below |
| no line for the model | the controller is not deciding for it: no ScaledObject names this scaler, or its metrics are missing | [ScaledObject](#scaledobject-ready-false-or-targets-unknown), [metrics](#metrics-stale-or-missing) |

Then check the silent causes, in this order:

```promql
wva_model_scaling_blocked{model_name="<model>"}           # any reason holding it back
wva_node_access_denied == 1                               # gpu-inventory alone, nodes unreadable: nothing scales up
wva_constraint_provider_skipped == 1                      # a declared limiter is left out; the others still bound
increase(wva_decisions_limited_total[15m]) > 0            # a limiter capped it
wva_unmeasured_queue{model_name="<model>"} > 0            # requests queue for pods it cannot see
wva_metrics_freshness_status{status!="fresh"} > 0         # decided on stale or missing data
```

`wva_node_access_denied` is covered in [controller not Ready](#the-controller-is-not-ready-or-crash-looping);
a limiter in [GPUs held](#gpus-held-or-quota-spent); the last two in
[metrics](#metrics-stale-or-missing).

**`no-composite-signal`**: no analyzer the policy lists could size any variant
this cycle. With `saturation` listed, the model is sized on saturation's own
result, as before the composite existed; without it, the model holds where it
is, up and down. The
dashboard's *Models blocked from scaling* panel counts it.

1. Check each listed analyzer reports the model:
   `wva_analyzer_target{model_name="<model>"}`, one `analyzer_name` per listed
   analyzer.
2. Run the controller at `--v=3` and read the `composite-contributors` line: per
   variant, which analyzers sized it (`contributors`) and the replicas they
   needed (`totalReplicas`).
3. Look for `composite holds a role` in the log. Its `reason` says why the
   role is held: no listed analyzer can size it, the other P/D role cannot
   grow, a listed analyzer's missed reading (up to three cycles), or a
   saturation hold another analyzer's demand replaced
   ([cycle log](cycle-log.md)). It is logged for a single held role too,
   which sets no blocked reason.
4. Look for the line that names the cause:
   - `listed analyzer reports no per-role demand, so it cannot size this P/D
     model`: the analyzer reports model-level demand only, so on P/D it sizes
     neither role.
   - `no listed analyzer can size this model this cycle, so it holds where it
     is`: the policy leaves `saturation` out and lists nothing else that
     sizes, for example `slo` alone.
   - `a listed analyzer's need is dropped: saturation has no per-replica
     capacity for the variant`: saturation has no capacity estimate for the
     variant yet, so another analyzer's need cannot be expressed for it.

## A model will not wake from zero

**Symptom**: the workload stays at 0 replicas while requests are pending.

First check whether a limiter refused the wake: the model sees its queue,
and nothing fits.

```bash
$LOGS | grep 'Scale-from-zero: no variant woken' | grep '"modelID": "<model>"' | tail -3
```

While the wake is refused, `wva_model_scaling_blocked{reason="wake-no-capacity"}`
is set for the model. The log line's `reason` is the outcome (`no-capacity`,
`prefill-required`, `nominal-lent-out`), `gpuBudgets` what the wake was measured
against, and `demand` what it asked for. Free GPUs or raise the quota, as in
[GPUs held](#gpus-held-or-quota-spent). It is also set while the model is in a
borrowing cooldown (`borrowCooldown`) after a borrowed replica of it was
released: until the cooldown ends, the wake places only into a free node hole
and borrows none. `wake-nominal-lent-out` in its place
means the ClusterQueue lent its nominal to a peer it cannot reclaim from: see
[past nominal](#a-model-runs-past-its-kueue-nominal-quota).

With no such line and no such reason, work through these in order. Do not run
`hack/verify_scale_from_zero.sh` during an incident: it parks the model first.

1. **The InferencePool.**
   ```bash
   kubectl get inferencepool -n <ns>
   ```
   The scaling manager watches a single InferencePool API group
   (`inference.networking.k8s.io` or `inference.networking.x-k8s.io`). If the
   cluster's pools use the other group, the datastore stays empty and
   scale-from-zero never gets a recommendation. Create the InferencePool, and
   let it be reconciled, before the workload's ScaledObject.

2. **Labels.** The workload's pod labels must match the InferencePool selector:
   ```bash
   kubectl get deployment <deployment> -n <ns> -o jsonpath='{.spec.template.metadata.labels}'
   kubectl get inferencepool <pool> -n <ns> -o jsonpath='{.spec.selector}'
   ```

3. **The EPP.** It must exist and export its flow-control queue:
   ```bash
   kubectl get svc -n <ns> | grep epp
   ```
   A model with no queue metric at all shows
   `wva_model_scaling_blocked{reason="no-wake-signal"}`. That reason means the
   EPP answered without the metric; an EPP that cannot be scraped, or has no
   Ready pod, does not set it (see [the EPP scrape is failing](#the-epp-scrape-is-failing-but-the-scaling-manager-still-wakes-models-slowly)). Enable the EPP's
   `flowControl` feature gate, then restart the EPP: it reads its config once
   at start.

4. **The queue itself.** Read it straight from the EPP, with the token the
   controller uses. The EPP's metrics port varies by release; find it first
   and use it in place of `9090` below:
   ```bash
   kubectl get svc <epp-service> -n <ns> -o jsonpath='{.spec.ports}'; echo
   TOKEN=$(kubectl -n "$NS" get secret wva-epp-metrics-token -o jsonpath='{.data.token}' | base64 --decode)
   kubectl port-forward -n <ns> svc/<epp-service> 9090:9090 &
   curl -s -H "Authorization: Bearer $TOKEN" localhost:9090/metrics \
     | grep -E 'llm_d_epp_flow_control_queue_size|inference_extension_flow_control_queue_size'
   kill %1
   ```
   llm-d's EPP exports `llm_d_epp_flow_control_queue_size`; upstream
   gateway-api-inference-extension exports
   `inference_extension_flow_control_queue_size`. The scaling manager reads
   whichever exists. A queue that stays at 0 means requests are not reaching the
   EPP: send them through the inference gateway, not straight to model pods.

### The EPP scrape is failing but the scaling manager still wakes models (slowly)

**Symptom**: the log carries

```text
Scale-from-zero: EPP metrics scrape failing; reading the flow-control queue from
Prometheus instead. Wakes still work but are slower and bounded by the scrape
interval — fix the direct path.
```

and `wva_scale_from_zero_queue_fallback_active{pool="..."}` is `1`.

The scrape failure itself is logged once, when every EPP pod starts failing:

```text
EPP metrics scrape failing on every pod; logged once until a scrape succeeds
  {"namespace": "<ns>", "service": "<epp-service>", "pods": 2, "error": "pod <epp-pod> returned status 401, body \"Unauthorized\""}
```

The error quotes the first 200 bytes of the EPP's answer. A `500` with body
`Authentication failed` or `Authorization for user ... failed` comes from the
EPP's own TokenReview or SubjectAccessReview call: see
[EPP /metrics returns 500](#epp-metrics-returns-500). While every pod keeps failing, the scaling manager does not
repeat the failure as `Optimization error` on every retry. It still does when
the Prometheus fallback below fails too, and when another model fails in the
same cycle.

`EPP metrics scrape recovered` follows once a scrape succeeds again. The
per-pod line, `Failed to scrape pod` with `namespace`, `pod` and `error`, is
logged only at `-v=3`. The fallback needs every pod to fail: while one EPP pod
still answers, the scaling manager reads the queue from that pod.

An EPP with no Ready pod (crash loop, rollout, readiness probe failing, or
scaled to zero) also counts as a failed scrape, and the fallback engages the
same way. There is no once-per-change line for it; `kubectl get pods -n <ns>`
on the EPP shows it. With no EPP serving, the queue in Prometheus is the last
one the EPP reported, so a model can wake on demand up to 90 s old. That is
deliberate: a wrong wake costs one replica until scale to zero parks it again,
and a missed wake parks a model with real demand for as long as the EPP is down.

**What it means**: the scaling manager reads the flow-control queue by scraping
the EPP pod **directly** (pod IP, EPP metrics port, bearer token projected at
`/var/run/secrets/epp-metrics/token`). Only this path bypasses Prometheus, so it
can fail alone. Then the scaling manager reads the same metric from Prometheus:
models still wake, but at the scrape interval instead of a 100 ms loop. A sample
older than 90 s is ignored, so a drained queue cannot wake anything.

Check, in order:

1. **The token.** `Failed to read EPP metrics token` in the log means the
   projected volume is missing, so the unauthenticated scrape is rejected. The
   image has no shell, so check the Secret has a token and the Deployment
   mounts it:
   ```bash
   kubectl get secret wva-epp-metrics-token -n "$NS" -o jsonpath='{.data.token}' | wc -c
   kubectl get deploy wva-controller-manager -n "$NS" \
     -o jsonpath='{.spec.template.spec.volumes[?(@.name=="epp-metrics-token")]}'; echo
   ```
   A count of `0` means the token controller never filled the Secret; an
   empty second line means the volume is missing from the Deployment.
2. **The EPP's own ServiceAccount** (cluster admin). A `500` means it may not
   create tokenreviews or subjectaccessreviews; see
   [EPP /metrics returns 500](#epp-metrics-returns-500).
3. **Network reachability to the EPP pod IP.** A NetworkPolicy that admits the
   monitoring namespace but not the controller's breaks this path only.

Once the direct path works, the gauge returns to `0` and the log says the scrape
recovered.

### Slow wakes

**Symptom**: the model wakes, but late. `wva_scale_from_zero_wake_seconds`
shows the time from activation to serving.

- **Many models waking at once.** The scale-from-zero engine processes at most
  `SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY` decisions at a time (default `10`).
  Raise it for a cluster with many parked models:
  ```bash
  kubectl set env -n "$NS" deploy/wva-controller-manager SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY=50
  ```
  This restarts the controller. A later `make deploy-wva` re-applies the
  Deployment, so check it is still set.
- **The replica itself starts slowly.** Compare with `wva_replica_start_seconds`;
  see [Preparing a workload](workload-preparation.md).
- **Requests bypass the gateway.** Requests sent straight to model pods never
  queue in the EPP and never wake anything.

### E2E and infra-only deploys

- [`deploy/install-epp.sh`](../../deploy/install-epp.sh) turns on EPP flow
  control when `ENABLE_SCALE_TO_ZERO=true` (it adds the `flowControl` feature
  gate to the GAIE standalone chart).
- The **InferenceObjective** `e2e-default` comes from the scale-from-zero e2e
  tests (`test/e2e/fixtures`), not from the install scripts.

## A model will not park at zero

**First signal**: `wva_model_scaling_blocked` for the model:

```promql
wva_model_scaling_blocked{model_name="<model>"}
```

| reason | fix |
| --- | --- |
| `variant-floor` | a variant still has `minReplicaCount > 0`; set every variant to 0 |
| `policy-forbids-zero` | the model's resolved policy has `scaleToZero` off; turn it on for every tier the model's variants name (the `default` entry or its `defaultPolicy` tier when they name none), or on the model's per-model entry, or accept that it stays up |
| `activation-retention` | just woken and held for its retention period; clears by itself |
| `engine-unsupported` | the model runs both vLLM and SGLang; run one engine per model |

No reason, and still not parked? A model that has never served a request has
no request counter yet, so the controller keeps it where it is. Send one
request. Full runbook: [scale-to-zero
guide](../guides/scale-to-zero/README.md#when-a-model-will-not-park).

## A lightly loaded model reads busy

**Symptom**: two namespaces serve the same model name. One carries a fraction
of the other's load, yet its `util` in `analyzer-result` is close to the busy
one's.

v0.10.0-rc2 and earlier read the router's queue (`llm_d_epp_flow_control_queue_size`)
by model name alone, so each namespace was charged the sum of both routers'
queues. The quiet model carried its busy twin's queue as its own demand.

1. **Read the queue the controller charged** to the quiet model:
   ```bash
   $LOGS | grep scheduler-queue-demand | grep '"namespace": "<ns>"' | tail -5
   ```
2. **Read the queue its own router holds**, in Prometheus:
   ```promql
   sum by (namespace, model_name, target_model_name) (llm_d_epp_flow_control_queue_size)
   ```
   If `eppQueueSize` in step 1 matches the other namespace's queue and not this
   one's, this is the cause. Upgrade the controller. It reads each namespace's
   router queue on its own.

The controller tells the namespaces apart by the `namespace` label Prometheus
adds when it scrapes the router. ServiceMonitors and PodMonitors add it. A
custom scrape job that drops it leaves the queue unattributed, and every
namespace reads it as before. A router deployed outside its model's namespace
is read by no model.

## A model is flapping

**Symptom**: replicas go up and down every few minutes. `WVAReplicaScalingThrashing`
fires at more than two decisions a minute for one variant.

That alert is not proof of flapping. Its counter, `wva_replica_scaling_total`,
rises on every cycle that is not `no-change`. A scale-up that is asked for and
never lands fires it too (actuation stuck, Pods Pending, slow starts). Step 1
tells them apart: if `tgt` is steady above `curr`, go to [actuation
stuck](#desired-is-not-current-actuation-stuck).

1. **Read what the controller decided**, cycle by cycle:
   ```bash
   $LOGS | grep scaling-decision | grep '"modelID": "<model>"' | tail -40
   ```
   `tgt` alternating between two values is the controller flapping. `tgt`
   steady while the replica count moves is the HPA or something else moving
   the workload: see [actuation stuck](#desired-is-not-current-actuation-stuck).
2. **Read why**:
   ```bash
   $LOGS | grep analyzer-result | grep '"modelID": "<model>"' | tail -40
   ```
   Compare `util` with `scaleUpThreshold` and `scaleDownBoundary` on the same
   line. `util` crossing back and forth over one of them is load sitting on the
   edge of the band. Widen the band in the scaling policy (lower
   `scaleDownBoundary`): see [Changing policy safely](operations.md#changing-policy-safely)
   and [scaling policy](scaling-policy.md).
3. **Check the HPA's stabilization windows.** KEDA's HPA acts on the
   controller's number through the ScaledObject's
   `advanced.horizontalPodAutoscalerConfig.behavior`:
   ```bash
   kubectl get scaledobject <name> -n <ns> \
     -o jsonpath='{.spec.advanced.horizontalPodAutoscalerConfig.behavior}'
   ```
   The generated ScaledObjects use scale-up `stabilizationWindowSeconds: 0` and
   scale-down `300`, set at generation by `WVA_SO_SCALE_UP_STABILIZATION` and
   `WVA_SO_SCALE_DOWN_STABILIZATION` ([configuration](configuration.md)). A
   scale-down window shorter than the model's load cycle releases a replica it
   will need again. Raise it on the object:
   ```bash
   kubectl patch scaledobject <name> -n <ns> --type=merge -p \
     '{"spec":{"advanced":{"horizontalPodAutoscalerConfig":{"behavior":{"scaleDown":{"stabilizationWindowSeconds":600}}}}}}'
   ```
   or set `WVA_SO_SCALE_DOWN_STABILIZATION` before `make scaledobjects-apply`.
   If the ScaledObject is managed by GitOps, change it there.
4. **Under the utilization-share optimizer**, a model whose load swings faster
   than a transfer lands shows `swinging` or `fast-swing-capped`; see
   [the optimizer runbook](#the-optimizer-evaluates-but-never-moves-anything).

## A model is pinned at max

**Symptom**: `wva_variant_at_max_replicas == 1`, or `"atMax":true` in
`scaling-decision`, and latency is high.

At max is normal for a well-sized variant under load. It is a problem only when
`util` in `analyzer-result` is still above `scaleUpThreshold`: demand goes
unserved, and more GPUs will not help. Raise the ceiling.

```bash
kubectl get scaledobject <name> -n <ns> -o jsonpath='{.spec.maxReplicaCount}'
kubectl patch scaledobject <name> -n <ns> --type=merge -p '{"spec":{"maxReplicaCount":<n>}}'
```

Check for free GPUs first; without them a higher ceiling gives Pending pods
([GPUs held](#gpus-held-or-quota-spent)). A benchmark pinned at its ceiling
measures the ceiling, not the autoscaler.

## One replica is saturated while others idle

**Symptom**: `WVALoadImbalanced` fires, or `wva_load_imbalanced == 1`; the
controller log has `WARNING: load imbalanced across a role's replicas` naming
a `busiestPod` and `idlePods`; latency stays high however many replicas the
role has.

The router (EPP) picks the replica for each request. When its scorers keep
choosing one replica, that replica fills and queues while its siblings idle.
A replica count is unlikely to fix this: the warning needs an idle replica
that has been Ready for at least 2 minutes (by default; `loadImbalance.idleAfterReady`), so the role already has capacity
the router is not using, and new replicas are routed to no more than the idle
ones are. A scale-up in progress does not raise it: a replica is not counted
idle until 2 minutes after it became Ready and the controller first saw it
Ready. A warm-pool Pod lent to the role counts from when it was lent. After a
controller restart every replica waits those 2 minutes again. The usual cause is the role's scheduling profile, where
the largest weight wins ties it should not. With `prefix-cache-scorer: 3`
against `queue-scorer: 2`, the replica that saw a prefix gets the request
however busy it is. One measured prefill fleet held 439 queued requests on one
replica while three of its four sat idle.

1. Measure the spread, per role:

   ```promql
   wva_load_kv_max_to_mean_ratio
   wva_load_queue_max_to_mean_ratio
   wva_load_idle_replicas_while_saturated
   ```

   A ratio near the replica count means one replica holds almost everything.

2. Read the scheduling profile the router loaded for that role, and look at
   the weights of `queue-scorer`, `active-request-scorer` and
   `kv-cache-utilization-scorer` against `prefix-cache-scorer`. How the
   weights combine is in
   [scheduling profiles](router-configuration.md#scheduling-profiles-decide-whether-ordered-replicas-get-used);
   how to print and patch the profile is in
   [changing router configuration](router-configuration.md#changing-router-configuration).

3. Check that the affinity you pay for exists: with prefix caching off on the
   engines, the prefix scorer steers toward KV that was never kept
   ([what to check first](router-configuration.md#what-to-check-first)).

4. Change one weight, restart the router, and measure the spread again over
   repeated runs. No weight set is known to be right for every workload.
   **Restarting the router drops traffic**: it runs as one replica with the
   `Recreate` strategy, so requests fail for 10 to 30 s while it restarts
   ([changing router configuration](router-configuration.md#changing-router-configuration)).
   Do it in a quiet window, not in the middle of the incident that paged you.

What the scaling manager does meanwhile: capacity is learned per replica from
that replica's own readings. But a deep queue pinned on one replica still
counts as demand, so the role may grow while the imbalance lasts, by replicas
that the router then leaves idle.

## Desired is not current (actuation stuck)

**Symptom**: `wva_desired_replicas` differs from `wva_current_replicas` for many
cycles; `scaling-decision` shows `tgt` not equal to `curr` cycle after cycle.

The controller decided; KEDA, the HPA or the workload did not follow. Check the
chain in order:

1. **The scale-down window.** A scale-down waits out the HPA's
   `stabilizationWindowSeconds` (300 s by default). Several minutes of
   `tgt < curr` is the design.
2. **KEDA got the number.**
   ```bash
   kubectl get scaledobject,hpa -n <ns>
   kubectl describe hpa keda-hpa-<name> -n <ns>
   ```
   `READY True` and a numeric `TARGETS` mean KEDA reads the controller. Otherwise
   go to [ScaledObject](#scaledobject-ready-false-or-targets-unknown). In the
   HPA's conditions, `ScalingLimited` means it hit `minReplicaCount` or
   `maxReplicaCount`.
3. **The Pods.**
   ```bash
   kubectl get pods -n <ns> --field-selector=status.phase=Pending
   kubectl get events -n <ns> --field-selector reason=FailedScheduling
   kubectl get events -n <ns> --field-selector reason=FailedCreate
   ```
   `FailedScheduling` with insufficient `nvidia.com/gpu` is a full cluster.
   `FailedCreate` with `exceeded quota` is a Kubernetes ResourceQuota
   (`kubectl describe resourcequota -n <ns>`), which the scaling manager does
   not see.
4. **Slow starts.** Pods created but not Ready for minutes: compare
   `wva_replica_start_seconds` with what you expect, and see
   [Preparing a workload](workload-preparation.md).

## GPUs held or quota spent

**Symptom**: the controller wants more replicas and does not get them, or a
model cannot grow while others hold GPUs.

| signal | means |
| --- | --- |
| `ResourceConstrained` Warning Event on the ScaledObject | a limiter capped this cycle's decision |
| `wva_decisions_limited_total` rising | same, as a counter per `limiter_name` |
| `wva_available_gpus` | GPUs the `gpu-inventory` limiter sees per accelerator type |
| `wva_unattributed_gpus > 0` | GPUs held by workloads whose accelerator did not resolve, charged to no pool, so every pool looks freer than it is |
| `wva_model_scaling_blocked{reason="quota-short"}` | under the utilization-share optimizer, the whole group is short |
| A Kueue-backed namespace stops below its ClusterQueue's `nominalQuota`; a non-zero `charge` on its rows in the `Kueue borrowing applied` log line | other workloads Kueue admitted in the queue (a training job, another namespace on a shared queue) hold that much of it, and the figure is charged with it. Expected: Kueue would hold more replicas pending. See [What a quota is charged](quota-limiter.md#what-a-quota-is-charged) |
| `GPU constraint provider skipped; the other limiters still bound scaling` in the log, `wva_constraint_provider_skipped == 1` | a declared limiter is left out of decisions; the others still bound, but nothing checks what it would deny. `reason`: `unobserved`: its GPU usage is not observed yet. A Kueue-backed quota entry that has never read Kueue is not skipped: it still bounds with its finite caps, and shows as `wva_kueue_never_read` instead (see [never read](#a-kueue-backed-quota-was-never-read)). Other reasons: `failed` (it could not compute, e.g. a Kueue read or a node list failed; the log line says why), `node_read_denied` (`gpu-inventory` beside a quota, no node read: grant it with `make setup-prereqs`), `no_constraints` (the limiter answered with no figures at all; the shipped limiters always answer, so report it as a bug with the log line). `GPU constraint provider bounds scaling again` ends it |
| `Physical limiter: replicas are not fitted to nodes this cycle` | the physical limiter bounds scaling but no node snapshot younger than 2 minutes exists, so replicas are fitted to the pool only, as before node fit; `reason` says why. Usually the GPU usage observation is failing (the same cause skips the provider soon after). `Physical limiter: replicas are fitted to nodes again` ends it. See [A replica must fit one node](gpu-limiter.md#a-replica-must-fit-one-node) |
| A model with free GPUs in `wva_available_gpus` does not grow; `ResourceConstrained` on its ScaledObject ends `(limited by node-fit)`; `wva_decisions_limited_total{limiter_name="node-fit"}` rising | the free GPUs are split across nodes in holes smaller than one replica (or one pod of it), on nodes tainted against it, or held for a replica still starting (the in-flight hold: up to 5 minutes, or a Kueue flavor's `placementTimeout` when longer, tripled while a cluster-autoscaler scale-up is pending): with the physical limiter a replica must fit one node. A P/D model needs a node for a prefill **and** a decode replica. Nothing to fix in the scaling manager; free a node, or use smaller replicas. See [A replica must fit one node](gpu-limiter.md#a-replica-must-fit-one-node) |
| A new group of an exclusive-topology LeaderWorkerSet stays Pending (`FailedScheduling` on pod affinity), and `wva_pod_access_denied` is 1 | a namespace-scoped controller that may not list Pods cluster-wide sees only its own namespace's Pods, so it misses other namespaces' LeaderWorkerSet groups. A topology domain where only such a group runs looks free, and a group is ordered into it. Grant `list` on `pods` cluster-wide. See [When policy demands a limiter the controller cannot serve](gpu-limiter.md#when-policy-demands-a-limiter-the-controller-cannot-serve) |
| `ResourceConstrained` ends `(limited by pd-pair-borrows-whole)`; `wva_decisions_limited_total{limiter_name="pd-pair-borrows-whole"}` rising | Kueue borrowing: the model's next P/D pair would put one role on guaranteed and the other on borrowed capacity, and a pair borrows only whole. The GPUs are there; the rule holds the pair back, because Kueue could reclaim one half alone. It grows once a whole pair fits borrowed capacity, or guaranteed capacity holds both roles. See [Cohort borrowing](quota-limiter.md#cohort-borrowing) |
| A Job's pods fail to create (`FailedCreate`, `exceeded quota`) in a namespace where serving pods wait at Kueue's admission gate | a `ResourceQuota` counts gated pods, Kueue does not. A serving replica ordered past what Kueue admits holds the namespace's `ResourceQuota` while it waits, and the Job Kueue did admit cannot create its pods. Find the gated pods with `kubectl get pods -n <ns> -o jsonpath='{range .items[?(@.spec.schedulingGates)]}{.metadata.name}{"\n"}{end}'`, and lower the serving model's `maxReplicaCount` so it stops ordering past Kueue's grant |
| `Upgrade note (transitional): this policy declares a physical GPU limiter and quotas, and both now bound scaling` | expected once after upgrading a policy that declares both kinds: the listed variants get a different target than the quota alone gave them, because the GPUs that exist now bound them too. Nothing to fix unless the physical bound is not wanted; then drop the `gpu-inventory` entry. Logged only when the set of variants changes, and removed in the next release |

A **fully** spent quota shows the same signals: a model the last GPUs did not
reach gets the `ResourceConstrained` Event and counts in
`wva_decisions_limited_total`, named after the limiter that ran out, though its
decision is `no-change`. The `scaling-decision` line carries the same name as
`limitedBy`. To see where the GPUs went, compare the quota with what the models
hold. First find where the limiters come
from: when a policy namespace governs this controller (`wva-policy` by default),
the limiters there apply and any in the controller's own ConfigMap are ignored:

```bash
$LOGS | grep -E 'Cluster policy comes from|Loaded cluster policy from the policy namespace|Ignoring limiters in the controller.s own ConfigMap' | tail -5
kubectl get configmap wva-scaling-policy-config -n "$NS" -o yaml | grep -A10 limiters
kubectl get configmap wva-scaling-policy-config -n wva-policy -o yaml | grep -A10 limiters   # (cluster admin) the policy namespace, if the log names another, use that
kubectl get pods -n <ns> -o custom-columns='NAME:.metadata.name,GPUS:.spec.containers[*].resources.limits.nvidia\.com/gpu'
```

What to do:

- Raise the quota, or free GPUs by lowering another model's `minReplicaCount`
  or `maxReplicaCount`. Lowering a quota does not evict: models move back under
  it only as they scale down.
- Resolve accelerators: set a GPU product `nodeSelector` or `nodeAffinity` on
  each workload (`AcceleratorNotResolved` Event names them).
- A `gpu-inventory` limiter counts allocatable GPUs on nodes, not GPUs free
  for scheduling.
- `WVAGPUResourceExhausted` (zero allocatable GPUs of a type): **(cluster admin)**
  check what the nodes advertise and that the device plugin runs on them. A node
  that is NotReady, or has no running device plugin, advertises zero.
  ```bash
  kubectl get nodes -L nvidia.com/gpu.product \
    -o custom-columns='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,GPUS:.status.allocatable.nvidia\.com/gpu'
  kubectl get pods -A -o wide | grep -i device-plugin
  ```
  On AMD nodes read `amd\.com/gpu` instead.

See [the GPU limiter](gpu-limiter.md) and [the quota limiter](quota-limiter.md).

## Replicas dropped under Kueue borrowing

**Symptom**: a model on a Kueue quota, usually one that counts borrowing, went
from, say, 6 to 4 replicas, and nobody lowered it.

There are four causes, five with `releaseToNonReclaimingLenders`. Check them
in this order; the first that matches is
the answer.

| cause | how to tell |
| --- | --- |
| 1. Kueue preempted pods | `wva_model_scaling_blocked{reason="lender-reclaimed"}` for the model; `wva_kueue_preempted_pods_total` rising; a Workload evicted with reason `Preempted` |
| 2. A replica did not place | `reason="borrow-unplaced"`; `wva_kueue_unplaced_pods` for the variant; a `BorrowedCapacityReleased` Event with cause `placement-timeout`. A replica a lender grew into its own lent-out nominal, ordered without a free node because Kueue was to preempt a borrower for it, is released the same way when it never places. On an entry that counts no borrowing (`countBorrowable: false`, a queue in no cohort), a replica Kueue held at its admission gate, waiting for quota, past `placementTimeout` is released too: its Event has cause `kueue-gate`, and `wva_borrow_releases_total{cause="kueue-gate"}` counts it. If Kueue was preempting lower-priority work in the queue for it, raise `placementTimeout` above that work's termination grace period |
| 3. Another model here reclaimed the nominal it borrowed | `$LOGS \| grep 'Kueue reclaim: a model of this controller grows'` names the variant (`replicasByVariant`). A model of a lending namespace in the same cohort grew into nominal its queue lent out, and Kueue preempts a borrowed replica to admit it, so the borrower gives the replica back in the same cycle. When a wake from zero grew it, the borrower gives it back in the next cycle. Expected; the borrower borrows again when the cohort has free GPUs |
| 3b. Handed back to a waiting lender | `reason="released-to-lender"` while it is in flight; a Normal `BorrowedCapacityReleased` Event with cause `lender-waiting` naming the lender's ClusterQueue; `wva_borrow_releases_total{cause="lender-waiting"}`. Only with `releaseToNonReclaimingLenders` on: a lender Kueue does not reclaim for waited for what this model borrowed. Expected; the model borrows nothing new in that cohort for `borrowCooldown` |
| 4. Demand fell | none of the above; the model's `scaling-decision` line shows `tgt` below `curr` at low `util` ([cycle log](cycle-log.md)) |

Commands, in the same order:

```bash
kubectl get workloads.kueue.x-k8s.io -n <ns> -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Evicted")].reason}{"\n"}{end}'
kubectl get events -n <ns> --field-selector reason=BorrowedCapacityReleased
$LOGS | grep -E "Releasing borrowed capacity|Releasing replicas held at Kueue|Kueue preempted a pod" | tail
kubectl get pods -n <ns> --field-selector status.phase=Pending
```

To see the budget the controller used, per quota entry, namespace and
accelerator (`guaranteed`, `borrowable`, `used`, and the `reason` borrowable is
0):

```bash
kubectl get configmap wva-controller-status -n "$NS" -o jsonpath='{.data.limiterBudgets}'
```

What to do:

- **Preempted or released, often** (`WVAKueueBorrowThrashing`): the model
  keeps growing onto capacity its lenders take back. Raise its ClusterQueue's
  `nominalQuota`, lengthen `borrowCooldown`, or set `countBorrowable: false` on
  the quota entry.
- **Did not place**: a replica waiting for a node the cluster autoscaler is
  adding counts as unplaced. The placement timeout waits up to three times as
  long while the autoscaler reports `TriggeredScaleUp` on the pod. If new nodes
  take longer than that on your cluster, raise `placementTimeout`, or borrowed
  replicas are given back while their node is still coming.
- **A lender's replica stays Pending after Kueue preempted a borrower**:
  Kueue picks the borrowers to preempt by priority, not by node. A lender
  growing into the nominal it lent out gets its quota back, but the GPUs
  freed may not form a node its replica fits: two 4-GPU borrowers preempted
  on two different nodes free no 8-GPU node, and a borrower that was itself
  `Pending` frees none. The scaling manager orders such a replica without a
  free node, so it watches it under the placement timeout: if it never
  places, it is released (`borrow-unplaced`), and the model's growth into
  lent-out nominal needs a free node until `borrowCooldown` ends. To avoid it,
  keep borrowed replicas whole-node in a cohort of whole-node lenders, or
  leave a node free for the lender.
- **A prefill stays unplaced after a partner was released**: when a P/D
  model's last prefill does not place, the scaling manager releases a decode
  to free GPUs for it. The freed GPUs may sit on nodes the prefill cannot use,
  for example a prefill pinned to RDMA nodes. It then stays `Pending`, and
  `WVAKueuePodsUnplaced` fires. Check the pod's scheduling message with
  `kubectl describe pod`; free or add a node it can use.
- **Borrowing is frozen as `stale`**: see
  [WVAKueueReaderStale](#wvakueuereaderstale).
- **The Kueue controller is down** (reason `kueue-controller-down`): see
  [WVAKueueControllerDown](#wvakueuecontrollerdown).
- **A lender waits past the cooldown**: see
  [WVAKueueLenderWaitingPastCooldown](#wvakueuelenderwaitingpastcooldown).
- **Pods refused by a ResourceQuota**: no shipped alert covers this. No
  scaling-manager metric carries the refusal, and the shipped alerts depend on
  no kube-state-metrics series (`kube_resourcequota` would). Check by hand:
  `kubectl get events -n <ns> --field-selector reason=FailedCreate | grep -i quota`.

Timings are in [configuration](configuration.md#kueue-borrowing-timings); the
series in [Prometheus](prometheus.md#kueue-borrowing-metrics).

## A Kueue-backed quota was never read

**Symptom**: `WVAKueueNeverRead` fires; `wva_kueue_never_read{limiter_name}`
is 1.

The quota entry has `kueue.enabled: true` and caps at least one accelerator
type at `-1`, so Kueue is meant to supply that figure. No Kueue read has
succeeded since the controller started, so a type at `-1` is bounded only by
the other limiters, and borrowing is not counted. The entry's finite caps still
bound, so it is not reported as a skipped provider.

This is not fail-closed, and it is not over-allocation. The other limiters
still bound, and Kueue still gates every pod past its quota at admission, so
replicas ordered past Kueue's quota wait as gated pods. The cost is churn:
replicas created, held Pending at Kueue's gate, and their GPUs not delivered.

```bash
$LOGS | grep 'external quota source unreadable and never read' | tail -3
kubectl get crd clusterqueues.kueue.x-k8s.io localqueues.kueue.x-k8s.io resourceflavors.kueue.x-k8s.io
kubectl auth can-i list clusterqueues.kueue.x-k8s.io \
  --as=system:serviceaccount:<controller-namespace>:<controller-serviceaccount>
```

What to do:

- **CRDs absent**: Kueue is not installed. Install it, or set
  `kueue.enabled: false` and give the type a number.
- **Forbidden**: re-run the install so the controller's ClusterRole carries
  the Kueue reads (see [the quota limiter](quota-limiter.md)).
- **Meanwhile**: replace `-1` with a number in the entry to bound the type.

The scaling manager does not refuse to scale while the entry is unread, by
design: see above.

## Borrowing is withheld

**Symptom**: a model on a Kueue quota stops at its nominal quota (or its
guaranteed figure) while its cohort has idle GPUs;
`wva_quota_borrow_withheld == 1` names a reason.

```bash
kubectl get configmap wva-controller-status -n "$NS" -o jsonpath='{.data.limiterBudgets}' \
  | jq -c '.entries[] | select(.reason) | {namespace, accelerator, reason}'
# the line reads "applied", or "frozen at the last figures" while stale or Kueue is down
$LOGS | grep -E 'Kueue borrowing applied|borrowing frozen at the last figures' | tail -1
```

Each namespace shows one reason: the first in this table that applies. Every
reason below is published on `wva_quota_borrow_withheld`. "Re-run the admin
phase" means a cluster admin runs `NAMESPACE=<ns> make setup-prereqs` with the
variables the install used, adding `KUEUE_NAMESPACE=<kueue-ns>` where Kueue
runs elsewhere than `kueue-system`. `<controller SA>` is
`system:serviceaccount:$NS:wva-controller-manager`; `<kueue-ns>` is where
Kueue runs.

**Withheld**: borrowable is 0, and the namespace is capped at its nominal
quota, as with `countBorrowable: false`. One that holds borrowed capacity now
is frozen at its guaranteed figure instead, until it scales down.

| reason | what it means | what to do |
| --- | --- | --- |
| `not-selected` | the entry's `borrowNamespaces` is set and does not list this namespace | add it to `borrowNamespaces`, or remove the list to borrow everywhere ([pilot](../guides/kueue-borrowing/#pilot-on-a-few-namespaces)) |
| `lenders-unreadable` | the controller cannot list Kueue Workloads in every namespace (a namespace-scoped install whose admin phase predates the grant or whose policy did not count borrowing when it ran, a Forbidden, a failed list), so it could not see a lender wait. A read revoked while the controller runs shows here within about 5 minutes. Granted again, borrowing resumes at the next check, while the Workload cache may still hold what it last listed before the revoke: up to about 30 more seconds, until its watch re-lists, a lender's Workload created in the gap is not seen | **(cluster admin)** re-run the admin phase with the install's `WVA_LIMITER` and `WVA_QUOTA_KUEUE`: it grants the cluster-wide Workload read when that policy counts borrowing. `kubectl auth can-i list workloads.kueue.x-k8s.io --all-namespaces --as=<controller SA>` must then say yes. An install that must not read other namespaces cannot borrow safely: set `countBorrowable: false` |
| `not-kueue-managed` | a GPU pod in the namespace is not backed by a Workload with quota reserved, owned by a kind a Kueue integration manages; or the controller may not read the pod's owner | find it: `kubectl get workloads.kueue.x-k8s.io -n <ns>` against `kubectl get pods -n <ns>`. Put the workload behind a LocalQueue (`kueue.x-k8s.io/queue-name`), or move it out of the namespace. For a Forbidden owner read, re-run the admin phase |
| `resourcequota-unreadable` | the namespace's ResourceQuotas cannot be read: Forbidden, or failing for five refresh intervals | `kubectl auth can-i list resourcequotas -n <ns> --as=<controller SA>`; re-run the admin phase |
| `no-gpu-bound` | nothing checks that borrowed GPUs exist: no `gpu-inventory` limiter, and no GPU `ResourceQuota` at most nominal plus `borrowingLimit` | declare one ([prerequisites](../guides/kueue-borrowing/#prerequisites)) |
| `multi-queue-namespace` | more than one of the namespace's GPU ClusterQueues grants this accelerator type (a flavor without a product label grants every type). The controller does not read which one a variant uses, so the type is capped at the smallest single queue's figure, not at their sum. A type one queue grants is not affected | `kubectl get localqueues -n <ns>` lists them. Move serving to a namespace with one LocalQueue, or point every GPU LocalQueue of the namespace at one ClusterQueue |
| `kueue-version-unknown` | the `kueue-controller-manager` Deployment was read, but its image carries no version tag (a digest-only image, say) | check the image: `kubectl get deploy kueue-controller-manager -n <kueue-ns> -o jsonpath='{.spec.template.spec.containers[*].image}'`. If it runs a [tested version](quota-limiter.md#supported-kueue-versions), set `kueue.assumeTestedVersion: true` on the entry |
| `kueue-version-untested` | Kueue runs a release outside the tested range | run a [tested version](quota-limiter.md#supported-kueue-versions); the nominal cap applies meanwhile |
| `no-cohort` | the namespace's ClusterQueue is in no cohort, so there is nothing to borrow from | set `spec.cohortName` on the ClusterQueue, if borrowing is wanted |
| `inactive` | the ClusterQueue admits nothing: `stopPolicy` is `Hold` or `HoldAndDrain`, or it names a flavor that does not exist | `kubectl get clusterqueue <cq> -o yaml`: read its `Active` condition |
| `malformed` | a `nominalQuota`, `borrowingLimit` or `lendingLimit` of the queue does not parse | fix the quantity on the ClusterQueue |
| `no-status` | a queue in the cohort has no reservation status yet | wait one refresh; if it stays, check that Kueue's controller is reconciling (its log) |
| `hierarchical-cohort` | the cohort has a parent or child Cohort | not supported: flatten the cohort, or set `countBorrowable: false` |
| `hierarchy-unknown` | Cohort objects cannot be read, so a nested cohort cannot be ruled out | `kubectl auth can-i list cohorts.kueue.x-k8s.io --as=<controller SA>`; re-run the admin phase |
| `effective-quotas` | a queue in the cohort uses `status.effectiveQuotas` | not supported |
| `fair-sharing` | Kueue Fair Sharing is on | not supported |
| `topology-aware` | a GPU flavor in the cohort has a `topologyName` (Topology-Aware Scheduling) | not supported |
| `admission-checks` | a queue in the cohort has admission checks, MultiKueue included | not supported |
| `borrow-within-cohort` | a queue in the cohort sets `preemption.borrowWithinCohort` other than `Never`, so a borrower could preempt another queue's work | set `borrowWithinCohort.policy: Never` (or remove it) on every queue in the cohort |
| `flavor-unpinned` | more than one flavor of the queue offers the GPU, and a variant's pod template does not select the flavor's `nodeLabels`, so Kueue could admit it on another flavor than the one counted. A queue with one GPU flavor never shows it | add the flavor's `nodeLabels` to the variant's `nodeSelector` |
| `co-resource-bound` | the GPU flavor's resource group also covers other resources (CPU, memory) and a variant's per-replica request for one of them cannot be read from its pod template | set requests for every covered resource on the variant's containers |

**Frozen**: nothing new is borrowed, the last figures are kept, and nothing
borrowed is taken away.

| reason | what it means | what to do |
| --- | --- | --- |
| `stale` | the last Kueue read failed, or is older than twice `refreshInterval`; or one namespace's ResourceQuota read is failing (for five refresh intervals at most; then `resourcequota-unreadable`) | `$LOGS \| grep 'external quota source unreadable'` names the read; usually RBAC or a missing CRD. Alerts `WVAKueueReaderStale` (per entry) and `WVAKueueReaderStaleNamespace` |
| `kueue-controller-down` | Kueue's leader Lease was not renewed within twice its `leaseDuration` (with no Lease, the Deployment has no ready replica), or the controller cannot read the Deployment or the Lease at all: never found, or Forbidden for want of the Role in the Kueue namespace | see [WVAKueueControllerDown](#wvakueuecontrollerdown): check Kueue runs, then the controller's read of its Deployment and Lease in `<kueue-ns>`; re-run the admin phase. Kueue elsewhere: set `kueue.controllerNamespace` too |

**Counted, but 0 this cycle**: borrowing is on, and there is nothing to draw.

| reason | what it means | what to do |
| --- | --- | --- |
| `lender-cannot-reclaim` | the cohort's only unused capacity belongs to lenders Kueue would not reclaim for: `reclaimWithinCohort: Never`, or `LowerPriority` whose Workloads do not outrank this model's. A `LowerPriority` lender with no GPU Workloads at all (idle) has no priority to compare, so it counts as cannot-reclaim too, even though its capacity is free. The namespace stays counted, at its guaranteed figure | set `reclaimWithinCohort: Any` on the lender's ClusterQueue, if serving may use its capacity; or give the serving model a lower `WorkloadPriorityClass` than the lender; or, experimental, set `releaseToNonReclaimingLenders: true` on the quota entry so the scaling manager counts that capacity and gives it back when the lender waits |
| `lender-waiting` | a lender waits for capacity it lent. What is borrowed stays until Kueue preempts it; nothing new is borrowed in the cohort, until `borrowCooldown` after the wait ends | nothing: Kueue reclaims for the lender. If it lasts, see [WVAKueueLenderWaitingPastCooldown](#wvakueuelenderwaitingpastcooldown) |
| `cohort-pool-empty` | the cohort's free pool holds no GPUs, after what is in flight and held at Kueue's gate | nothing: the cohort is in use |
| `no-borrow-cap` | `borrowingLimit`, the static entry or the namespace ResourceQuota stops the namespace at guaranteed | raise the one that binds, if borrowing is wanted |
| `preemptions-unreadable` | Kueue's preemptions in the namespace could not be read this cycle | transient; if it repeats, check the controller's `get`/`list`/`watch` on Workloads and pods |

Two more labels you may meet:

- `nominal-lent-out` is not a withheld reason. It qualifies a guaranteed
  figure below nominal: a peer holds part of the queue's nominal and need not
  give it back. On `wva_model_scaling_blocked`, it marks a serving model
  capped for that reason, and `wake-nominal-lent-out` a parked model that
  cannot wake for it. To get the nominal back, set `reclaimWithinCohort:
  Any` on the queue.
- `borrow-namespaces-split` is a warning in the log: two Kueue entries govern
  one cohort and only one sets `borrowNamespaces`. Nothing is withheld; set
  the list on both entries, or on neither.

## A model runs past its Kueue nominal quota

**Symptom**: a model holds more GPUs than its ClusterQueue's `nominalQuota`.

This is borrowing, on by default for a namespace-scope quota entry with
`kueue.enabled`. Confirm it:

```bash
kubectl get configmap wva-controller-status -n "$NS" -o jsonpath='{.data.limiterBudgets}' \
  | jq -c '.entries[] | select(.namespace == "<ns>") | {accelerator, guaranteed, borrowable, used}'
kubectl get clusterqueue <cq> -o jsonpath='{.status.flavorsReservation}'
```

`used` above `guaranteed`, and Kueue's own `borrowed` above 0, is a model on
borrowed capacity. `wva_kueue_borrowed_gpus` shows what this controller holds
borrowed, per cohort and flavor.

What to do, if it is not wanted:

- Set `countBorrowable: false` on the entry, or pilot with
  `borrowNamespaces` ([pilot](../guides/kueue-borrowing/#pilot-on-a-few-namespaces),
  [turn off](../guides/kueue-borrowing/#cleanup)). This
  evicts nothing: the cap only stops growth, and models move back under
  nominal as they scale down.
- To hand GPUs back now, lower the model's `maxReplicaCount` on its
  ScaledObject.
- To stop a queue borrowing for anyone, set its `borrowingLimit` to 0 in
  Kueue.

## Kueue borrowing alerts

Runbooks for the alerts on Kueue borrowing. `WVAKueuePodsUnplaced` and
`WVAKueueBorrowThrashing` point at
[replicas dropped](#replicas-dropped-under-kueue-borrowing);
`WVAKueueNeverRead` at [never read](#a-kueue-backed-quota-was-never-read).
In the commands, `<kueue-ns>` is where Kueue runs: `kueue-system`, or the
entry's `kueue.controllerNamespace`.

### WVAKueueReaderStale

**Fires when** borrowing has been frozen as `stale` for 5 minutes. Nothing new
is borrowed and nothing is given back. Two alerts, so one fault pages once:

- **`WVAKueueReaderStale`**, one per quota entry, while two or more of its
  namespaces are stale. Its value is how many. The entry's last Kueue read
  failed, or is older than twice `refreshInterval`.
- **`WVAKueueReaderStaleNamespace`**, one per namespace, only while it is the
  entry's only stale one. Either the entry covers that namespace alone and
  its Kueue read failed, or the namespace's ResourceQuota read is failing.
  A failing ResourceQuota read becomes `resourcequota-unreadable` after five
  refresh intervals, so at the default 30s it never reaches this alert; it
  does only with a `refreshInterval` above one minute.

`wva_quota_borrow_withheld{reason="stale"}` lists the frozen namespaces.

```bash
$LOGS | grep 'external quota source unreadable' | tail -3
kubectl auth can-i list resourcequotas -n <namespace> --as=system:serviceaccount:$NS:wva-controller-manager
```

The log line names the read that failed; usually RBAC or a missing CRD. To
hand capacity back by hand meanwhile, lower the borrowing model's
`maxReplicaCount` on its ScaledObject, or set `countBorrowable: false` on the
entry, which caps the namespace at nominal.

### WVAKueueControllerDown

**Fires when** borrowing has been frozen as `kueue-controller-down` for 5
minutes: Kueue's API answers, but no running Kueue controller is seen.

Nothing new is borrowed, nothing is given back, and nothing borrowed is taken
away. Kueue itself admits and preempts nothing while it is down.

```bash
kubectl get deploy kueue-controller-manager -n <kueue-ns>
kubectl get lease c1f6bfd2.kueue.x-k8s.io -n <kueue-ns> -o jsonpath='{.spec.renewTime}{"\n"}'
kubectl auth can-i get deployments/kueue-controller-manager -n <kueue-ns> \
  --as=system:serviceaccount:$NS:wva-controller-manager
kubectl auth can-i get leases.coordination.k8s.io/c1f6bfd2.kueue.x-k8s.io -n <kueue-ns> \
  --as=system:serviceaccount:$NS:wva-controller-manager
```

- **Kueue is not running**, or its Lease is not renewed: fix Kueue.
- **`no` above**: a namespace-scoped install needs the Role in the Kueue
  namespace. A cluster admin runs `NAMESPACE=<namespace>
  KUEUE_NAMESPACE=<kueue-ns> make setup-prereqs`, with the other variables the
  install used.
- **Kueue runs elsewhere than `kueue-system`**: set
  `kueue.controllerNamespace` on the entry.
- **Kueue runs with a custom leader-election ID**: the scaling manager reads
  only the default Lease, `c1f6bfd2.kueue.x-k8s.io`, so such a Kueue always
  reads as down. Borrowing cannot be counted there; set
  `countBorrowable: false`.

### WVAKueueBorrowDrift

**Fires when** `wva_kueue_borrow_drift_gpus` is above 0 for 15 minutes: for a
ClusterQueue, the borrowed GPUs the scaling manager computes (usage past
nominal) differ from Kueue's own `status.flavorsReservation` borrowed figure.

Borrowing still runs. The figures it counts are from a model of Kueue's
accounting, and that model no longer matches this cohort, so borrowable may be
too high or too low.

```bash
kubectl get clusterqueue <cluster_queue> -o jsonpath='{.status.flavorsReservation}'
kubectl get configmap wva-controller-status -n "$NS" -o jsonpath='{.data.limiterBudgets}' | jq -c '.entries[]'
$LOGS | grep -E 'Kueue borrowing applied|borrowing frozen at the last figures' | tail -1     # kueueBorrowed per queue row
```

Usual causes: a Kueue feature borrowing does not detect (`waitForPodsReady`
with `blockAdmission`), or a Kueue release whose accounting changed. Brief
drift while Kueue admits or preempts is normal; 15 minutes is not. If it
persists, set `countBorrowable: false` on the entry and report the two
figures.

### WVAKueueVersionUnknown

**Fires when** `wva_kueue_controller_version_known` has been 0 for 10 minutes,
per controller instance: the controller cannot tell which Kueue release runs,
and withholds borrowing as `kueue-version-unknown`.

The gauge is 1 once the version is read, or when the entry sets
`kueue.assumeTestedVersion: true`. It is published only while some quota
entry counts borrowing, so a cluster with borrowing off never fires. A
Deployment that cannot be read also sets it to 0, but freezes borrowing as
`kueue-controller-down`; this alert then stays quiet and
[WVAKueueControllerDown](#wvakueuecontrollerdown) pages instead.

```bash
kubectl get deploy kueue-controller-manager -n <kueue-ns> \
  -o jsonpath='{.spec.template.spec.containers[*].image}{"\n"}'
kubectl auth can-i get deployments/kueue-controller-manager -n <kueue-ns> \
  --as=system:serviceaccount:$NS:wva-controller-manager
```

- **The image has no version tag** (a digest only): if the release is a
  [tested one](quota-limiter.md#supported-kueue-versions), set
  `kueue.assumeTestedVersion: true` on the entry. The alert clears.
- **The Deployment cannot be read** (`no` above, or not found): borrowing is
  frozen as `kueue-controller-down`, which `assumeTestedVersion` does not
  lift. See [WVAKueueControllerDown](#wvakueuecontrollerdown).

### WVAKueueVersionUntested

**Fires when** (severity `info`) Kueue has run a release outside the
[tested range](quota-limiter.md#supported-kueue-versions) for 30 minutes.

Borrowing is withheld (`kueue-version-untested`): every namespace stays at
its nominal quota, and nothing is evicted. Run a tested release to count
borrowing again. `assumeTestedVersion` does not lift this reason: it only
stands in for a version that cannot be read.

### WVAKueueLenderWaitingPastCooldown

**Fires when** a lender ClusterQueue has waited for capacity it lent longer than
`wva_kueue_borrow_cooldown_seconds` (the configured `borrowCooldown`), while a
namespace this controller governs still borrows in its cohort. In a cohort
whose quota entries set `releaseToNonReclaimingLenders`, it waits also for
that cohort's `wva_kueue_borrow_release_latency_seconds`, the time a release
there takes to land (at most 30 minutes); every other cohort pages at the
cooldown.

By default the scaling manager counts only what Kueue would reclaim for its
lender, so Kueue is expected to preempt for it. New borrowing in the cohort
stopped at the first sighting (reason `lender-waiting`), and the scaling
manager hands nothing back itself.

With `releaseToNonReclaimingLenders` on, it hands a lender Kueue does not
reclaim for (`Never`, or `LowerPriority` below the model) what it waits for.
Check that it did:

```bash
$LOGS | grep 'Releasing borrowed capacity' | grep 'lender-waiting'
kubectl get events -n <borrowing ns> --field-selector reason=BorrowedCapacityReleased
kubectl get events -n <borrowing ns> --field-selector reason=LenderReleaseBlocked
```

A `LenderReleaseBlocked` Event, or
`wva_kueue_lender_release_blocked{cluster_queue="<lender>"}`, says why
nothing was handed back:

- `unservable`: what this controller can give back on the lender's flavor,
  in whole units and never below `minReplicas` or a guaranteed figure, would
  not admit the lender's next Workload -- the rest of what it waits for is
  held by a borrower this controller does not govern, or a model's smallest
  unit is far larger than the need. Nothing is handed back, so nothing sits
  idle. Free the rest by hand, or set `reclaimWithinCohort: Any` on the
  lender.
- `taken-by-another-borrower`: a release landed, but another borrower took
  the GPUs before Kueue admitted the lender. Nothing more is handed to it
  while it waits.

A release line and no block, but the lender still waits: the release has not
landed yet (the model's scale-down window and termination grace).

No line of either kind: the lender's wait has not triggered a release yet
(two reads in a row, or a Workload waiting over a minute), its Workload is in
Kueue's requeue backoff, or it waits behind a `StrictFIFO` head that does not
fit. A ledger that cannot be read also holds every release
(`wva_kueue_borrow_ledger_unread`).

```bash
kubectl get workloads.kueue.x-k8s.io -n <lender namespace> \
  -o custom-columns='NAME:.metadata.name,PRIORITY:.spec.priority,ADMITTED:.status.conditions[?(@.type=="Admitted")].status'
kubectl describe workload <pending workload> -n <lender namespace>
```

A long wait usually means:

- the lender's pending Workload does not outrank the borrowers'
  (`LowerPriority`), so Kueue will not preempt for it;
- the GPUs it waits for are held by a borrower this controller does not
  govern;
- the pending Workload does not fit where the borrowed GPUs are (another
  flavor, or CPU or memory short).

To free capacity by hand, lower the borrowing model's `maxReplicaCount` on its
ScaledObject, or set `countBorrowable: false` on the quota entry. For a lender
on `Never` or a `LowerPriority` lender below the model, the opt-in
`releaseToNonReclaimingLenders: true` does it for you
([quota limiter](quota-limiter.md#lenders-kueue-does-not-reclaim-for)).

## EPP /metrics returns 500

**Symptom**: the controller log names an EPP pod that `returned status 500,
body "Authentication failed"`, every cycle. The EPP's own log says why:

```text
Failed to make webhook authenticator request: tokenreviews.authentication.k8s.io is forbidden:
User "system:serviceaccount:<ns>:<epp-sa>" cannot create resource "tokenreviews" ...
```

**Cause**: the EPP checks every `/metrics` scrape by asking the API server who
the caller is (a TokenReview) and whether it may read `/metrics` (a
SubjectAccessReview). It asks as its **own** ServiceAccount. When that
ServiceAccount may not create those reviews, the EPP cannot answer and returns
500, to the scaling manager and to Prometheus alike. A caller without the right
gets 401 or 403, never 500. llm-d's router chart grants this; an EPP installed
another way may not have it.

**Check** (`make check-prereqs` and `make verify-deployment` warn about it too):

```bash
kubectl auth can-i create tokenreviews.authentication.k8s.io --as=system:serviceaccount:<ns>:<epp-sa>
kubectl auth can-i create subjectaccessreviews.authorization.k8s.io --as=system:serviceaccount:<ns>:<epp-sa>
```

**Fix (cluster admin)**: grant it to the EPP of every InferencePool the
scaling manager manages. `DRY_RUN=true` prints the plan and the commands and
changes nothing.

```bash
make epp-metrics-auth NAMESPACE=<ns> DRY_RUN=true
make epp-metrics-auth NAMESPACE=<ns>
```

The EPP caches review results, so the 500 can last up to about 5 minutes after
the grant. Restarting the EPP pod clears it at once.

It finds each pool's endpoint picker Service, the Deployment behind it and the
ServiceAccount that Deployment runs as. It binds the ServiceAccounts that are
not already allowed, and those allowed only through another install's binding,
which that install's uninstall would remove. `make setup-prereqs` does the same
(`WVA_EPP_METRICS_AUTH=false` skips it), and `make undeploy-wva` removes what
either made.

- An EPP running as the namespace's `default` ServiceAccount is not bound: every
  pod there that names none would get the grant too. Give the EPP its own
  ServiceAccount, or pass `WVA_EPP_METRICS_AUTH_DEFAULT_SA=true`.
- A pool whose endpoint picker is not a Service is skipped with a warning;
  check its EPP by hand.

The script exits 1 while an EPP is left ungranted (for example, a pool whose
Service does not exist), including a `DRY_RUN=true` run that has something to
grant, as `scaledobjects-rewire` does. It exits 2 when the pools cannot be
listed, and 3 when one could not be checked. `make` reports any of these as
`Error <code>` and itself exits 2.

By hand, the same grant:

```bash
kubectl create clusterrole wva-epp-metrics-auth --verb=create \
  --resource=tokenreviews.authentication.k8s.io,subjectaccessreviews.authorization.k8s.io
kubectl create clusterrolebinding wva-epp-metrics-auth-<ns>-<epp-sa> \
  --clusterrole=wva-epp-metrics-auth --serviceaccount=<ns>:<epp-sa>
```

A binding made by hand has no label, so `make undeploy-wva` leaves it.

## Metrics stale or missing

**Symptom**: decisions look wrong or absent; a `MetricsUnavailable` Warning Event
on the ScaledObject; `WVAMetricsCollectionFailing` firing.

1. **Is Prometheus reachable?** `wva_metrics_collection_errors_total` rising, or
   Prometheus errors in the controller log, mean the query fails. Check the
   Prometheus URL and TLS settings ([Prometheus](prometheus.md#configuration)).
2. **Are the model pods scraped?** `wva_metrics_pods_discovered` should be above
   0 per model, and `wva_metrics_freshness_status{status="fresh"}` equal to the
   pod count.
   ```bash
   make verify-deployment
   kubectl get servicemonitor,podmonitor -n <ns>
   ```
   Then confirm the targets are up on Prometheus' `/targets` page.
3. **Requests queue for pods it cannot see?** `wva_unmeasured_queue > 0`, or an
   `UnattributedReadyPods` Warning Event (`<variant> has N ready pod(s) but none
   attributed`):
   ```bash
   kubectl get events -n <ns> --field-selector reason=UnattributedReadyPods
   ```
   The serving pods are not scraped, or their ownerReferences do not reach the
   scale target. On FMA, see [FMA launcher pods](operations.md#fma-launcher-pods).

While a variant's metrics are unavailable it keeps its current replica count.
Details: [Metrics health](metrics.md).

## Capacity priced high after a restart

A controller that restarts forgets what it measured, and prices each replica
from the deployment-derived capacity until the model saturates again; that is
higher than the measured figure, so the fleet is smaller and TTFT worse for a
while after every upgrade or leader change. `LEARNED_STATE_SNAPSHOT=true`
carries the measured state across ([configuration](configuration.md#learned-state-across-a-restart)).
With it on, `wva_learned_state_restores_total{outcome}` counts the variants by
how their state was restored, `wva_learned_state_snapshot_errors_total{operation}` the
failed reads and writes, and the log says why (logger `learned-state`):

- `Learned state: no snapshot to restore` -- the first start with the store,
  or after a flush. Expected once.
- `Learned state: snapshot restored` -- read back; `pendingVariants` wait for
  their first cycle, and `aged`, `unusable` and `unattributable` count entries
  not restored. Then, per variant on its first cycle, one of:
  - `learned state restored`;
  - `learned state restored without the ITL baseline: the engine image
    changed, the arguments did not` -- a new release rolled out while the
    controller was down. Capacity, throughput and start time are kept; the
    ITL line, and so the derived mu, is fitted again from fresh readings;
  - `learned state restored as lower-only figures: the variant's engine
    arguments changed, so its k2 windows become previous figures (P2-prior)
    for the buckets they were measured in` -- its flags changed while the
    controller was down. The old capacity is used only where it is lower, and
    expires (30 minutes of serving from a bucket's first use, then a 6-hour
    relaxation). The old service rate comes back too, and holds the
    throughput floor hold-only -- it may keep the fleet, never grow it -- for
    the same 30 minutes plus 6 hours of serving at most, until the new flags
    measure their own. A saturated throughput already measured under the
    flags it runs now -- a rollback made while the controller was down -- is
    kept as its own;
  - `learned state restored under the unread configuration: ...` -- a flag
    such as `--max-num-seqs $VLLM_MAX_NUM_SEQS` whose value comes from an
    `envFrom`. Its windows are its own only if the variant runs the same
    unread configuration, and lower-only figures otherwise (with the old
    service rate holding the floor hold-only, as above).
    `wva_engine_config`'s `unresolved` label names the flag;
  - `learned state restored as lower-only figures: the document recorded no
    engine arguments and its configuration differs, ...` -- a snapshot
    written by an older release. Expect it for every variant after an upgrade
    from a release that did not record engine arguments, or one with another
    `fingerprint_version` label on `wva_engine_config`. Only the k2 is kept;
    the old service rate is not;
  - `learned state restored as a hold-only service rate: no k2 window was
    recent enough, ...` -- a lower-only case where no k2 could be kept, but
    the old saturated throughput could: it holds the throughput floor,
    hold-only, until the new arguments measure their own. Nothing to do;
  - `learned state not restored: the variant's engine configuration changed,
    and no k2 window was recent enough, on the hardware it runs now, to keep
    as a lower-only figure` -- one of the two lower-only cases above, with
    nothing to keep: the windows were old, or were measured on another
    accelerator or GPU count;
  - `learned state not restored: the variant's engine configuration could not
    be read completely, and no k2 window was recent enough, on the hardware
    it runs now, to keep` -- the unread-configuration case with nothing to
    keep, for the same reasons. Nothing to do; the variant learns again;
  - `learned state not restored: ... and the snapshot held no k2 window of
    the variant's own to keep` (either case) -- the variant never measured a
    k2 of its own before the restart: it had borrowed one, or had not
    saturated. Nothing was lost. Nothing to do;
  - `learned state not restored: the variant's engine configuration could not
    be read completely, and the snapshot recorded no engine arguments to check
    its windows against` -- an unread configuration and a snapshot from a
    release that did not record arguments: nothing can be matched, so nothing
    is kept. To restore it next time, make the configuration readable: give
    flags literal values in the container's `args` or `env` rather than from
    an `envFrom` or another source the controller cannot resolve
    (`wva_engine_config`'s `unresolved` label names them).

  Each of these lines carries an `outcome` field: `whole`, `args`, `prior`,
  `incomplete` or `differs`.
- `Learned state: could not read the snapshot; nothing restored, and nothing
  will be written until a read succeeds` -- the read failed; the error says
  why. Not a missing `wva-learned-state-role`: in every install mode the
  manager Role reads ConfigMaps in the controller's namespace. Look for an
  API server or network error, or a `wva-manager-role` or its binding in the
  controller's namespace that was edited or removed (re-run
  `NAMESPACE=<controller-namespace> make setup-prereqs` to restore it).
  Retried every minute;
  the stored state is not overwritten meanwhile, and `Learned state: the
  snapshot can be read again` marks recovery. For the first 10 minutes
  lower bounds (`P2-borrow`, `P2-prior`) are held at full strength and none
  starts; after that they start as usual, and a read that succeeds later
  keeps those over the stored ones -- a borrow the snapshot had as spent
  runs its hold and relaxation again. Fix the cause within 10 minutes to
  avoid it.
- `Learned state: could not write the snapshot` -- typically Forbidden: the
  Role `wva-learned-state-role` or its binding is missing. The snapshot is
  still read and restored -- the manager Role reads it -- but nothing learned
  since is kept for the next restart, which restores the snapshot as it was
  when writing stopped. Ask an admin to re-run
  `NAMESPACE=<controller-namespace> make setup-prereqs`. Logged once, not
  every minute; `Learned state: the snapshot can be written again` marks
  recovery.
- `Learned state: snapshot not restored` -- the document names no writer, or
  a controller in another namespace wrote it (a ConfigMap copied by hand).
- `Learned state: the snapshot was over its size bound` -- the least recently
  used entries were left out; they are relearned after a restart.

To flush it -- after a model's engine or hardware changed in a way the
configuration check cannot see, say -- delete the ConfigMap:

```bash
kubectl delete configmap wva-learned-state -n <controller-namespace>
```

The running controller notices within a minute or two and forgets everything
it learned too, rather than writing it straight back (`Learned state: the
snapshot was deleted; everything learned is forgotten`). Every model then
prices from the derived capacity until it saturates again, as after a restart
with no store. To forget one variant instead, see the next section.

## Reset one variant's learned state

Use this when one variant is priced from its own history you no longer trust:
it holds more replicas than its load needs, and `replica-capacity-summary` (or
`k2-decision` at `--v=3`) shows `P2-hist` or `P2-prior` for it. It forgets that variant only; every other variant keeps
what it has learned.

- `P2-hist` is the variant's own measured k2.
- `P2-prior` is its own k2 from before its engine arguments changed. It is
  expected after a rollout that changes them, and wears off by itself: held
  for 30 minutes of serving, then relaxed over 6 hours of serving. Forget it
  only to end it sooner; `disableK2Borrow` does not affect it.
- `P2-borrow` is a sibling's figure. Forgetting ends a running borrow, but the
  variant can borrow again on new evidence. To stop borrowing, set
  [`disableK2Borrow`](scaling-policy.md#disablek2borrow) for the model instead.

1. Annotate the variant's scale target -- the Deployment or LeaderWorkerSet
   itself, not its pod template, so nothing restarts. The value is any string;
   a timestamp is convenient:

   ```bash
   kubectl annotate deployment <name> -n <namespace> --overwrite \
     llm-d.ai/forget-learned-state="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
   ```

2. Check the controller log on the variant's next cycle:
   `Learned state forgotten for one variant, on request`, with the variant and
   the number of `entries` dropped.

What is forgotten: the variant's measured k2 and throughput, its ITL line
(including one it lent to siblings), its replica start estimate, and its
capacity record. Also its previous k2 (`P2-prior`), the old service rate that
holds the throughput floor after a change of arguments, and the windows set
aside under earlier arguments for a rollback. The variant then prices as a new variant would until it
saturates again: from its deployment-derived figure, or from a lower measured
k2 of a compatible sibling where one exists (the k2 borrow), which orders more
replicas, not fewer. Expect more replicas or a higher price for a while,
depending on which figure was wrong.

A k2 borrow the variant was running is ended, not dropped: it does not start
again unless the sibling's figure or the variant's configuration changes. A
dropped one would have started again at full strength on the next cycle.

- **Acted on once per value.** Leaving the annotation in place does nothing
  more, across controller restarts too when `LEARNED_STATE_SNAPSHOT` is on.
  To forget again, set a new value.
- **A variant parked at zero** is forgotten on the first cycle it is analysed
  again, once it has a replica.
- **To stop a variant borrowing** a sibling's figure rather than forget what it
  has, set [`disableK2Borrow`](scaling-policy.md#disablek2borrow) for the model.
- **Remove the annotation before rolling the controller back** to a release
  without this feature. That release rewrites the snapshot without the
  record of which value was acted on, so upgrading again with the annotation
  still in place forgets the variant a second time.

## The controller is not Ready or crash-looping

```bash
kubectl get pods -n "$NS" -l app.kubernetes.io/name=workload-variant-autoscaler
kubectl describe pod -n "$NS" -l app.kubernetes.io/name=workload-variant-autoscaler
kubectl logs -n "$NS" deploy/wva-controller-manager --previous    # after a crash
```

A NotReady controller has no endpoints on the `wva-external-scaler` Service, so
KEDA cannot reach it and **no** model scales. To see which readiness check fails:

```bash
kubectl port-forward -n "$NS" deploy/wva-controller-manager 8081:8081 &
curl -s 'localhost:8081/readyz?verbose'
kill %1
```

| failing check, or log line | means | fix |
| --- | --- | --- |
| `gpu-policy`: `cluster policy declares a gpu-inventory limiter but this controller cannot confirm it may list nodes` | a `gpu-inventory` limiter, declared alone, is in cluster policy and the node list failed. Kept NotReady on purpose: otherwise every variant would silently stop scaling up. For `forbidden`, `wva_node_access_denied` and `WVANodeAccessDenied` report the same; `Unauthorized` and `no answer yet` do not set them. The error after the colon says which: `forbidden` is missing node read; `Unauthorized` means the API server rejected the controller's credentials; `no answer yet` means the API server has not answered since the pod started (2s timeout per try) | Forbidden: (cluster admin) grant node read: `make enable-physical-limiter` grants every controller it finds and verifies when it runs, so re-run it after a new install. Or remove the limiter. Unauthorized: check the controller's ServiceAccount token, then restart the pod. No answer yet: check the API server; no action unless it persists ([details](gpu-limiter.md#when-policy-demands-a-limiter-the-controller-cannot-serve)) |
| `Cluster policy declares gpu-inventory beside quota limiters, but this controller may not list nodes` (an error line; the pod stays Ready) | the physical limiter is skipped and only the quota bounds; `wva_constraint_provider_skipped{reason="node_read_denied"}` is 1. Only a definite answer (`forbidden` or `Unauthorized`) sets it: a node list that gets no answer keeps what was last reported | (cluster admin) grant node read: `make setup-prereqs` for the install, or `make enable-physical-limiter`. Or drop `gpu-inventory` to keep the quota alone |
| `readyz`: `initial ConfigMap bootstrap not complete`, with `cannot read cluster policy from <ns>/<name>, refusing to start unbounded` in the log | the controller cannot read the cluster policy ConfigMap in the policy namespace (`wva-policy` by default). It refuses to run without its limits | (cluster admin) grant it get/list/watch on ConfigMaps there; `make enable-physical-limiter` does this for every controller it finds and verifies when it runs (re-run it after a new install) |
| `CrashLoopBackOff`, log `refusing to start unbounded: the "default" entry of ConfigMap <ns>/<name> is invalid` | the `default` entry of the ConfigMap the limiters come from has an invalid value. It carries the GPU limits, so the controller will not start without it. The entry was often edited while the controller ran: then the old entry stayed in force, and a `ConfigEntryRejected` event warned that the next restart would fail | fix the entry the log names, then let the pod restart. The line before it gives the reason: `Scaling-policy entry rejected: entry "default" rejected (<reason>)` |
| `external-scaler` | the gRPC scaler KEDA calls is not serving | read the log around startup for the bind error |
| pod restarts (`CrashLoopBackOff`) at startup, log `Shutdown requested during startup` after Prometheus connection errors, exit code 143 (read it with `kubectl get pod <pod> -n "$NS" -o jsonpath='{.status.containerStatuses[0].lastState.terminated.exitCode}'`) | the controller cannot reach Prometheus at startup and keeps retrying. Its health endpoint is not served yet, so the startup probe kills it after about 60s and the pod restarts | make Prometheus reachable: check the URL, TLS settings and network policy ([Prometheus](prometheus.md#configuration)). The restarts stop once it connects |
| `CrashLoopBackOff`, log names a Prometheus setting | a plain `http://` URL without `PROMETHEUS_ALLOW_HTTP=true`, or TLS settings combined with plain HTTP | fix the Prometheus settings ([Prometheus](prometheus.md#configuration)) |
| `CrashLoopBackOff`, `unknown flag` | the image is older than the manifests | deploy an image built from the same release as the manifests ([Upgrade](upgrade-and-uninstall.md)) |
| `CrashLoopBackOff`, `x509: certificate signed by unknown authority` | verification is on by default now, and a private CA signed the Prometheus certificate | re-run the install: it copies the CA from the Prometheus TLS Secret and mounts it. If it cannot read that Secret it names the command an admin runs ([Prometheus with TLS](prometheus.md#prometheus-with-tls-eg-kube-prometheus-stack-on-a-managed-gpu-cloud)) |
| `CrashLoopBackOff`, `x509: certificate is valid for <names>, not <host>` | the controller trusts the certificate, but it was issued for other names than the host in `PROMETHEUS_BASE_URL` | [Prometheus certificate name mismatch](#prometheus-certificate-name-mismatch) |

### Prometheus certificate name mismatch

**Symptom**: the controller exits at startup with

```text
x509: certificate is valid for llmd-kube-prometheus-stack-prometheus.llm-d-monitoring.svc, ..., not prometheus-operated.llm-d-monitoring.svc.cluster.local
```

The CA is fine; the address is not. A kube-prometheus-stack issues its
certificate for its own Service, `<release>-kube-prometheus-stack-prometheus`,
and not for `prometheus-operated`, which installers before this fix picked.

**Fix**: re-run the install. It now picks the Service the certificate names and
checks the name before it reports success
([how the address is chosen](configuration.md#how-the-prometheus-address-is-chosen)).
To choose the address yourself, pass one of the names from the error:

```bash
PROMETHEUS_URL=https://llmd-kube-prometheus-stack-prometheus.llm-d-monitoring.svc:9090 \
  NAMESPACE=<namespace> make deploy-wva
```

Add every other variable you installed with. The re-run restarts the
controller. The installer stops with the same names if you pass a
`PROMETHEUS_URL` the certificate does not name.

## Cluster policy is not applied (policy namespace unverified)

**Symptom**: `WVAPolicyNamespaceUnverified` firing, or
`wva_policy_namespace_unverified == 1`.

The controller may not read Namespaces, so it cannot see whether a cluster
admin published policy for it. It runs on the ConfigMap in its own namespace,
and any limiters or quotas an admin published are **not** in force for it.

```bash
$LOGS | grep -E 'Cannot tell whether a cluster admin published policy|Could not read the managed Namespace' | tail -3
```

Fix **(cluster admin)**: grant the Namespace read, then restart the controller.
The gauge is set once at startup.

```bash
NAMESPACE=<controller namespace> make setup-prereqs
kubectl rollout restart deployment/wva-controller-manager -n "$NS"
```

After the restart, the log says where policy comes from:

| log line | policy in force |
| --- | --- |
| `Cluster policy comes from the namespace this one is labelled with` | the admin's, from the namespace in the `wva.llmd.ai/policy-namespace` label |
| `Cluster policy comes from the well-known namespace` | the admin's, from `wva-policy` |
| `Cluster policy comes from this controller's own namespace` | this controller's own ConfigMap; no admin policy exists. Logged at `-v=1`, which the default `-v=2` shows |

## ScaledObject READY False or TARGETS `<unknown>`

```bash
kubectl get scaledobject,hpa -n <ns>
kubectl get scaledobject <name> -n <ns> -o jsonpath='{.spec.triggers[0].metadata}'
kubectl get endpoints -n "$NS" wva-external-scaler
```

| sign | cause | fix |
| --- | --- | --- |
| `scalerAddress` names a namespace where no scaler runs | the ScaledObject points at another or a removed install | `make scaledobjects-repoint` |
| `make verify-deployment` names the ScaledObject as on the plaintext address or without the `authenticationRef` | the scaler serves mutual TLS, and the trigger uses the plaintext address or lacks KEDA's `authenticationRef`. It may be Ready now, riding another ScaledObject's connection | `make deploy-wva WVA_SCALER_TLS=true WVA_SCALER_TLS_MIGRATE=true`, or edit the trigger ([what it needs](configuration.md#external-scaler-mutual-tls)) |
| Every ScaledObject on the TLS address stopped after a KEDA operator restart; the controller logs `refused a TLS handshake` | a ScaledObject on that address without the `authenticationRef` connected first, so KEDA's one connection for the address has no client certificate | fix that ScaledObject (`make verify-deployment` names it in the namespaces it watches), then (cluster admin) `kubectl rollout restart deployment/keda-operator -n <KEDA namespace>` |
| Every ScaledObject not-Ready after the scaler's CA changed (reinstall, provider switch, OpenShift service CA rotation) | KEDA keeps the TLS of a connection until it restarts | (cluster admin) `make setup-prereqs`, then `kubectl rollout restart deployment/keda-operator -n <KEDA namespace>` |
| The install or `make verify-deployment` says `Cannot tell which CA KEDA trusts` | KEDA's side was written by an older installer, which did not record the CA | (cluster admin) `make setup-prereqs` |
| `make verify-deployment` says KEDA's side is not usable | the cluster admin has not run `make setup-prereqs` since TLS was turned on, or KEDA's copy of the client certificate expired | (cluster admin) `make setup-prereqs` with the install's variables |
| the trigger has no `modelID` | the controller cannot tell which model to answer for | set `modelID` to what the container serves; `make verify-scaledobjects` |
| `wva-external-scaler` has no endpoints | the controller is not Ready | [controller not Ready](#the-controller-is-not-ready-or-crash-looping) |
| `make verify-deployment` warns that the external scaler NetworkPolicy admits `:9090` ONLY from a namespace KEDA does not run in | the NetworkPolicy admits the wrong namespace, so KEDA's calls are dropped while the controller reads healthy | (cluster admin) `KEDA_NAMESPACE=<namespace KEDA runs in> make setup-prereqs` with the install's other variables ([details](upgrade-and-uninstall.md#the-external-scaler-networkpolicy)) |
| `TARGETS` reads `cpu: <unknown>/80%` | KEDA never got a metric spec and fell back to CPU | one of the above |
| After an uninstall and reinstall in the same namespace: the ScaledObject reads Ready but not Active, the HPA carries only `cpu`, `status.externalMetricNames` is empty. KEDA logs `tls: unknown certificate authority` and `got empty metric spec`; the controller logs `refused a TLS handshake` with `certificate signed by unknown authority` from KEDA's Pod IP, about once a minute. `make verify-deployment` says `KEDA's operator still presents an OLD client certificate` | KEDA keeps one connection per scaler address and never closes it. It still presents the client certificate of the old install, which the new CA rejects. Changing the ScaledObject (`wvaRewire`) does not help | (cluster admin) `kubectl rollout restart deployment/keda-operator -n <KEDA namespace>` |
| `make verify-deployment` says KEDA did not wire the scaler's metric onto the HPA; the ScaledObject reads Ready, the HPA carries only `cpu` | KEDA asks for the metric spec only when the spec changes. The call failed then (the scaler was restarting or moving TLS mode), and KEDA does not retry. On KEDA 2.19 and older this happens to any ScaledObject created up to two minutes after a controller restart or outage, a KEDA bug fixed in 2.20 ([details](keda.md#unwired-hpa-before-keda-220)) | upgrade KEDA to 2.21. On older KEDA, once the scaler answers, run `make scaledobjects-rewire` or restart the KEDA operator. Run it with the same `NAMESPACE`, `WVA_NS` and `SCOPE` as `verify-deployment`; `DRY_RUN=true` only lists them. It changes the trigger key `wvaRewire`, which the scaler ignores, and never deletes a ScaledObject. It exits non-zero while one stays unwired |
| the replica count never moves, and `wva_scaler_undecided_targets` stays above 0 for the namespace | the scaler answers the current count because the model has no usable decision. For a cycle or two after a controller restart or leader change that is normal. `wva_scaler_undecided_answers_total` says why: `reason="not-managed"`, or `no-decision` that never stops, means the model is never planned, usually a trigger value that does not parse; `stale` means its decision stopped being republished, so read that model's `scaling-decision` lines | [a bad value freezes the model](scaledobject-trigger.md#model-keys); `external scaler has no decision for this target` names the ScaledObject, and the controller log at `--v=4` names the key |
| the `Fallback` condition `True`, or `KEDAScalerFailed` that persists | KEDA cannot reach the scaler: the controller is down or not Ready, or the Service has no endpoints | [controller not Ready](#the-controller-is-not-ready-or-crash-looping) |
| `no children to pick from` in KEDA's log, after an uninstall and reinstall | KEDA's gRPC client is still backing off on a name that did not resolve | (cluster admin) restart the KEDA operator ([details](operations.md#no-children-to-pick-from-after-a-reinstall)) |

More rows: [First-line troubleshooting](operations.md#first-line-troubleshooting).
Removing or reinstalling the controller: [Upgrade and uninstall](upgrade-and-uninstall.md).

## A model misses, or is never judged against, its SLO

**Symptom**: `WVAModelSLOAttainmentLow` or `WVAModelSLOMissed` (warning), or `WVAModelSLOUnjudged` (info)
fires, or the SLO dashboard row stays empty for a model whose policy sets
targets. All three carry `component: slo`, so they can be routed apart from
pages.

**On this release the slo analyzer only observes**, so neither alert means the
autoscaler failed: the fleet is sized by the saturation analyzer, and a miss
says that tier's thresholds do not meet the SLO. It is a tuning finding to act
on in working hours, not an outage. Set up in
[`slo` (observe only)](scaling-policy.md#slo-observe-only).

The `wva_slo_*` series carry the model's namespace as `exported_namespace`
(see [the labels](prometheus.md#slo-analyzer-metrics)); filter on that, not on
`namespace`.

### `WVAModelSLOAttainmentLow` or `WVAModelSLOMissed`

- `WVAModelSLOAttainmentLow` (TTFT): over the last 30 minutes fewer requests
  met the target than the objective promises (90% for `p90`), every judged
  request counted once. It needs at least 15 judged minutes, holding at least
  half the half hour's requests: at p99 a window needs 1000 requests to be
  judged, and a model judged only in its busy bursts is not judged at all.
- `WVAModelSLOMissed` (TPOT): the mean was over target in at least half of the
  judged minutes of the last 30, and in at least 7.

**1. Check the target can be met at all.** If it cannot, more replicas do not
help: loosen the target, or move the model to faster hardware.

```promql
# tpot: 1 for "infeasible" -- even an almost empty replica is over the target
wva_slo_source{model_name="<model>", source="infeasible"} == 1
# ttft: the objective's percentile of prefill time alone (vLLM), queue left
# out; at or above the TTFT target, no replica count meets it. Use the same
# percentile as the objective (0.90 for p90): a mean under the target hides a
# tail of long prompts that no replica count can make faster.
histogram_quantile(0.90, sum by (le) (rate(vllm:request_prefill_time_seconds_bucket{namespace="<ns>", model_name="<model>"}[5m])))
```

The prefill buckets are coarse above 2s (2.5s, then 5s), so a reading in the
bucket that holds the target is inconclusive. On P/D, read the prefill pods
alone (decode pods report almost no prefill time and pull the percentile
down), and the KV transfer to decode is extra time on top.

`infeasible` read from a `pinned-default` line is not reliable: that line's
intercept is a default for no particular card.

**2. If it can, mitigate:** give the model more capacity than saturation asks
for:

- raise its floor: `minReplicaCount` on each variant's ScaledObject
  (`kubectl edit scaledobject <so> -n <ns>`; where a tool generates the
  ScaledObject, change it there, or the next apply undoes it), or
- lower the tier's `scaleUpThreshold` (and write the tier's other fields out,
  see [Turn it on](scaling-policy.md#slo-observe-only), step 3).

**3. Then find the cause**, per objective:

```promql
# tpot: the mean ITL the variant would show sized for the target, against the target
wva_slo_predicted_itl_seconds{model_name="<model>"}
wva_slo_target_seconds{model_name="<model>", objective="tpot"}
# and the fill the target allows, before headroom: below kvCacheThreshold
# (0.80 by default), the target needs more replicas than the tier gives
wva_slo_fill{model_name="<model>"}
# ttft: the engine's queue wait against its prefill time, per pod (vLLM)
rate(vllm:request_queue_time_seconds_sum{namespace="<ns>"}[5m]) / rate(vllm:request_queue_time_seconds_count{namespace="<ns>"}[5m])
rate(vllm:request_prefill_time_seconds_sum{namespace="<ns>"}[5m]) / rate(vllm:request_prefill_time_seconds_count{namespace="<ns>"}[5m])
```

A queue that grows while prefill time stays flat is a capacity miss; a prefill
time that grows is a slow prefill (long prompts, a cold prefix cache). The
router's TTFT pools every priority band, so where its flow control holds a
low-priority band back on purpose, that wait counts as a miss too. Only
completed requests count, so under shedding the alert can resolve while
requests are refused: read `llm_d_epp_request_error_total` too.

`WVAModelSLOAttainmentLow` resolves once a judged reading of the last 5
minutes meets the objective. A window excluded while a replica starts does not
resolve it, so the miss that brought the replica on stays one alert. It also
ends when too few minutes are judged to hold half the requests.
`WVAModelSLOMissed` resolves once the judged minutes that meet the target
outnumber the misses in the last 30, so it stays up for a while after a fix.
Changing the tier's TTFT percentile clears the old percentile's alert at once;
the new one needs 15 judged minutes before it can fire.

### `WVAModelSLOUnjudged`, or nothing judged at all

A target set for 55 of the last 60 minutes, and no window judged in the hour. The alert
needs `wva_slo_target_seconds`, which exists only when the policy is right, so
**for the alert, skip to the router side below**. For an empty dashboard row
with no alert, start with the policy side -- the model's last
`Effective scaling policy` line in the controller log (logged when the policy
changes, so on a long-running controller it may have rotated out; restarting
the controller logs it again):

```bash
$LOGS | grep 'Effective scaling policy' | grep '"modelID": "<model>"' | tail -1
```

| the line shows | means |
| --- | --- |
| `"scalingPolicy"` is not the tier you set | the model's variants do not all name it (the alphabetically first name wins), or the tier is defined only in the global map and the namespace has its own map, which replaces it whole |
| `"slo": "off"` | the tier it reached has no `slo` block, and does not list the analyzer |
| `"slo"` ends `(analyzer not listed)` | targets set, `slo` not under `analyzers:` (also logged as `SLO policy is half-configured`) |
| `"slo"` ends `(observe)` | the policy is right: read the router side below |

Then the router side:

```promql
wva_slo_window_requests{model_name="<model>"}   # requests the router recorded, per objective
wva_slo_excluded{model_name="<model>"} == 1     # the reason a window is not judged
# the share of the last day it was not excluded (for either objective)
1 - avg_over_time(max by (exported_namespace, model_name) (wva_slo_excluded{model_name="<model>"})[1d:5m])
```

| what you see | means | fix |
| --- | --- | --- |
| `excluded` 1 for `starting` or `zero-replicas`, most of the time | the fleet never holds still for the window plus its grace: constant scale-ups, or a replica that keeps starting | check the starts are real (`kubectl get pods -n <ns>`). If the replica count goes up and down (`wva_desired_replicas`), the fleet flaps, which is a tuning question for the tier's band, not for the SLO. If it only climbs, it is growing, and the model is judged once it settles. Silence the alert meanwhile |
| `excluded` 1 for `unobserved` | the analyzer did not analyze the model on the cycle before: just after a controller restart, or the engine keeps skipping the model | if it persists, the model's replicas lack metrics on some cycles: [metrics](#metrics-stale-or-missing) |
| requests present but low, nothing excluded | too little traffic: a p90 needs 100 requests in the 5-minute window, a p95 200, a p99 1000, a TPOT mean 50 | nothing to do: the alert does not fire for this, and the model is judged once traffic returns |
| `tpot` requests `0` while the router serves streaming requests | clients do not send `stream_options.include_usage`; or the router does not publish `llm_d_epp_request_streaming_tpot_seconds`; or the router's TPOT query fails (`wva_metrics_collection_errors_total{query_type="router_latency"}` rising) | have clients send it, or run vLLM with `--enable-force-include-usage`; check the router publishes the series (`count(llm_d_epp_request_streaming_tpot_seconds_count{namespace="<ns>"})`); for a failing query, [metrics](#metrics-stale-or-missing) |
| no `window_requests` series at all | the router's histograms are not read | below |

When there is no `window_requests` series, check:

```promql
# Does Prometheus have the router's histogram for the model, and where?
sum by (namespace, model_name, target_model_name, streaming) (rate(llm_d_epp_request_ttft_seconds_count[5m]))
```

Here `namespace` is right: it is the router's own scrape label.

- **No series anywhere**: the router (EPP) is not scraped, or is too old to
  publish `llm_d_epp_*`. Check its ServiceMonitor / PodMonitor and version.
- **A series in another namespace**: the EPP runs outside the model's
  namespace. That is not supported: the analyzer reads the router in the
  model's namespace only, and nothing configures another. Run the EPP in the
  model's namespace.
- **Only `streaming="false"`**: the clients do not stream; only streaming
  requests are counted.
- **A series under another name**: the analyzer takes the model from
  `target_model_name`, or `model_name` where that is empty; clients that address
  an alias or an objective name show up under it, not under the model ID the
  controller logs. So do requests to a LoRA adapter: they are counted under the
  adapter's name, never the base model's.

## Utilization-Share Optimizer

Configuration: [`optimizer` in the scaling policy](scaling-policy.md#optimizer-cluster-default-only-live).
Metrics and blocked reasons: [utilization-share metrics](prometheus.md#utilization-share-optimizer-metrics).

### The optimizer evaluates but never moves anything

Work down the list in order.

1. **Is it selected at all?** Exactly one `wva_utilization_share_mode` series
   is `1`: `off`, `invalid`, `shadow` or `active`.
   - For `invalid` (alert `WVAUtilizationShareModeInvalid`) or `off`, the log says why: `Utilization share: invalid
     optimizer block; keeping today's optimizer` (a misspelled key, a value out
     of range, an empty `clusterNamespaces` entry, or `needs a limiters: list`,
     no budget to share).
   - A block in a namespace-local scaling-policy map is ignored: `ignoring
     optimizer blocks in namespace-local scaling-policy maps`.
   - A namespace-scoped install that reads cluster policy from a separate policy
     namespace reads an edit there only at start. Restart it after the change.
2. **Is it in shadow mode?** `wva_utilization_share_mode{mode="shadow"} == 1`.
   The log says `Utilization share: would rebalance (shadow)`, not `Utilization
   share: rebalancing`, and `wva_utilization_share_promised_gpus` is absent (it
   is published only while acting). Set `shadow: false` (or remove the key) to
   act. Both lines are written when the set of actionable roles changes, not
   every cycle, so a quiet log is not a stopped optimizer.
3. **Is the model planned?** The two lines above, and at `--v=4` the per-cycle
   `Utilization share: evaluation`, carry a `frozen` field. It lists the
   group's models left to today's optimizer this cycle, with the reason: no live
   analyzer result, a role with demand but no measured capacity, a
   namespace-local scaling-policy map (not planned in the cluster group), or a
   namespace not in `clusterNamespaces`. These models are in no group at all
   and publish no role series: variants on more than one accelerator type, a
   namespace with `enabled: false`, only a `gpu-inventory` limiter without
   `physicalGroups: true`, or a namespace a namespace quota lists in `exclude`
   with no finite cluster bound for its accelerator type (logged once as
   `Utilization share: not sharing models no quota bounds`).
4. **Is any role actionable?** `wva_utilization_share_actionable` is `1` only
   for a role outside its tolerance band *and* off its whole-replica target. All
   `0` means the fleet is where the optimizer wants it, or every imbalance is
   under one replica (counted by
   `wva_utilization_share_withheld_total{reason="not-actionable"}`).
5. **Does it stay actionable?** A role must be actionable on **two consecutive
   cycles** before a transfer is planned, so a role that flickers never moves.
   A hard imbalance (step 6) does not wait. At most two replicas move per role
   per cycle, and two transfers run per group at once.
6. **Is it held?** `withheld_total{reason="reversal-hold"}` rising, or the
   blocked reason `reversal-hold`, means a role that just gave is kept from
   receiving (or the reverse) until the hold in
   `wva_utilization_share_effective_seconds{param="reversal-hold"}` passes. This
   is the anti-oscillation rule working, not a fault.
   - The hold lasts about twice a release time from the start of the transfer
     it gave in: about 12 minutes with a 300 s scale-down window.
   - It is lifted for a **hard imbalance**: a role at 0.9 or more of its
     scale-up threshold receives at once from a role at 0.6 or less of its own
     after giving (`utilizationShare.stabilization.skipWaitsWhen`). The log
     shows `skippedWaits=true`, and the role does not show `reversal-hold`
     while such a donor exists. A role held short for the whole hold means no
     other role is that lightly loaded.
   - A cancelled transfer holds only its own donor -> receiver pair; each role
     can still move with any other role.
   - A transfer is called off on its donor while its replica is still
     draining if the donor's demand comes back so that, after the move, it
     would be one whole replica or more under its target: the move is
     exactly reversed. A busy model whose load dips for a cycle or two and
     comes back keeps its replica, with whole 8-GPU replicas as with 1-GPU
     ones. Once the donor's pod has gone, the move is no longer called off.
   - The counter also counts moves withheld by a donor's back-off after an
     aborted release, or by a donor that has given all it can.
   - A donor's calm is judged on the higher of its current and peak need (two
     readings in a row) over its scale-down window, so a model in a trough of
     its normal demand is not "lightly loaded". For a receiver whose bursts
     outlast a move, the donor is judged on its current need alone: the move
     lands inside the burst.

   Three more reasons on the same counter are not holds:
   - `donor-peak`: while the quota holds every model's peak, a model already at
     its need takes no headroom that would leave its donor below the donor's
     own recent peak (its peak over its scale-down window, or over the swing
     window once it has peaked twice within it, more than a scale-down window
     apart), by a single move or a donor set.
   - `awaiting-return`: a short model is credited with returns still releasing
     whose pods hold its pods, in whole replicas; only the rest is funded by
     draining other models. The model shows blocked reason `awaiting-release`.
   - `scale-down-band`: a return the model did not make because it would go
     lower than today's optimizer scales down to (demand a quarter of the way
     from `scaleDownBoundary` to `scaleUpThreshold`). A return is today's
     scale-down, and goes no lower.
7. **Is it swinging, paced, or at the transfer limit?** Blocked reasons:
   - `swinging` (`wva_utilization_share_swinging == 1`): the role reversed
     direction `stabilization.swingReversals` times (three by default)
     within the swing window (a transfer called off on a reversal counts as
     the move it would have made; one called off because the donor's own
     demand came back is a change of direction only the second time the
     donor gave and wanted the replica back within the window) and is
     planned on its mean need, so it
     may look short and still not receive. The controller logs
     `Utilization share: planned on its mean need` with the role,
     `meanNeedGPUs`, `currentNeedGPUs`, the `flappingWindow` and
     `swingReversals` when the rule takes a role, and
     `planned on its current need again` when it lets go.
     With the default, two models that swap loads and swap back once are not
     a swing: the swap back follows current demand. A load that swaps a
     second time within the window is: the move that follows waits until the
     busy model's mean catches up, or its need has stayed high for longer
     than the earlier halves did. A shorter `stabilization.flappingWindow`
     shortens this; a higher `stabilization.swingReversals` lets more swaps
     through before it starts.
   - `transfer-limit`: the group already runs two transfers with a donor;
     clears as they land.
   - `burst-ending`: the role is short with a hard imbalance, but its model's
     burst should end (by the length of its last completed burst) before a
     transfer lands, so it receives one replica a cycle instead of its whole
     shortfall.
   - `fast-swing-capped`: the role swings faster than a move while another model
     bursts for longer than one; its headroom is capped on its mean need, and it
     serves each crest from what it holds.

   `burst-ending` and `fast-swing-capped` are passing states; give the model a
   floor if it must not lag.
8. **Did the controller just restart?** After a restart, after the optimizer
   starts acting, or when a group first appears, every planned model of the
   group holds what it runs (or its restored target) for one fill timeout, about
   three minutes by default, and nothing in it scales. A fill in flight before
   the restart has no mark, and its receiver's pods must not lose their GPUs
   meanwhile. Expect this on every upgrade or leader change.
   - While it lasts, every planned model shows blocked reason `quiet-period`,
     and the log line `Utilization share: ledger started` carries
     `planningFrom`.
   - A freeze that does not end shows `marks-unreadable` (alert
     `WVAUtilizationShareMarksUnreadable`): the controller cannot read the
     transfer marks on the donors' pods. The Error line `Utilization
     share: could not read transfer marks; retrying next cycle` says why,
     typically a missing RBAC grant to list pods in one model's namespace.

   **The need history.** The models' demand history survives a restart in the
   ConfigMap `wva-utilization-share-history` in the controller's namespace.
   - `Utilization share: need history restored` says it was read back.
     `rolesKnownNow` counts roles whose peaks are known at once; `peaksKnownAt`
     is when the first restored role's peak is known (in the past when some are
     known now).
   - With no usable history (none stored, or one ending more than ten minutes
     before the restart), the headroom cap is on until a
     `stabilization.flappingWindow` of history exists and the peaks are known
     ([a model shrinks while no other model grows](#a-model-shrinks-while-no-other-model-grows)).
     `wva_utilization_share_headroom_capped` is not published during the quiet
     period, when the planner does not plan.
   - A history shorter than today's `flappingWindow` (the window was raised
     since) is restored, but its peaks are known only once today's window is
     covered. `Utilization share: stored need history not used` says why an
     entry was ignored (`reason`; `older than its window` when every sample has
     aged out).
   - `Utilization share: could not read the stored need history; the models'
     peaks are unknown for a swing window and their headroom is capped
     meanwhile. Retrying every minute; nothing is written until a read
     succeeds` (with the `group`): the read failed with something other than
     NotFound, typically Forbidden because the Role
     `wva-utilization-share-history-role` or its binding is missing. Nothing is
     restored or written until a read succeeds, so the stored history is not
     overwritten. `Utilization share: could not store the need history; a
     restart now forgets the models' peaks` is the same for the write. Both are
     logged per group, once per change of the error; `Utilization share: the
     need history store works again` marks recovery.
   - On an install upgraded from before the store existed, the controller-only
     `make deploy-wva` reports the Role and binding as optional and installs
     without them. Ask an admin to re-run `make setup-prereqs` in the install's
     shape: `NAMESPACE=<ns> make setup-prereqs`; out of reach
     `NAMESPACE=<managed ns> make setup-prereqs WVA_NS=wva-<managed ns> WVA_WATCH_NS=<managed ns>`;
     cluster-scoped `make setup-prereqs SCOPE=cluster WVA_NS=<controller ns>`.
   - The ConfigMap stays below the API server's 1 MiB limit: at most 128 roles
     per group, and an equal share of 900 KiB per group written. A group over
     either bound leaves out the roles with the smallest stored peaks, which
     count at their `maxReplicas` after a restart. `Utilization share: need
     history truncated; the roles left out count at their ceiling after a
     restart` names them (`dropped`, the first ten), once per change.

   To flush the history (say a model's load changed for good and its old peaks
   keep the cap on), delete the ConfigMap:
   ```bash
   kubectl delete configmap wva-utilization-share-history -n <controller-namespace>
   ```
   The controller notices on its next read or write (within a minute or so) and
   forgets its in-memory history too, instead of writing it back: `Utilization
   share: the need history ConfigMap was deleted; every model's need history is
   forgotten`. For one window the cap then treats every model as after a
   restart with nothing stored. Editing one group's key by hand does not flush
   it; the next write replaces it from memory. Entries the controller cannot
   parse are removed by its next write, and entries of another format version
   once a week old. `make undeploy-wva` deletes the ConfigMap.
9. **Could it not mark the donor?** `Utilization share: could not mark a donor pod;
   transfer not started` means a fault stopped the donor's pod being steered: a
   sibling's deletion cost leaves no room for the mark (see [which pod
   leaves](scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model)),
   or the patch failed. The donor backs off, shows blocked reason
   `donor-not-steerable`, gets a `UtilizationShareDonorNotSteerable` Event naming
   the cause and the end of the back-off, and is retried after it.

   Two donor states are holds, not failures. In both, the receiver tries another
   donor, and the donor counts no abort, does not back off, and gets no Event:
   - **Its workload is changing:** mid-rollout (by its status, pods of two
     ReplicaSets, or LWS groups of two revisions at or above its partition), a
     pod not yet created, scheduled or Ready, or an LWS group missing or being
     replaced. Logged as `Utilization share: donor's workload is changing;
     transfer not started`, then `donor held while its workload is changing`
     every cycle, with the cause. The donor and the model whose transfer it
     refused show blocked reason `donor-changing`. The hold ends once the workload
     reads stable for `stabilization.consecutiveCyclesBeforeMove` cycles, and
     after one release timeout at the latest. A paused Deployment or a held LWS
     canary rollout stays "changing", and does not give, until resumed or
     finished.
   - **It has nothing left:** every pod (or LWS group) is already given to
     transfers in flight. It is held until one of those releases lands, or for
     one release timeout, and is not counted as `reversal-hold`. At `--v=4`:
     `Utilization share: donor has nothing left to give; transfer not started`.

Other cases:

- **`whole-replica-short`:** the model serves below its need because the quota
  covers the needs in fractions but not in whole replicas (with 8-GPU replicas,
  a quota a few GPUs short of every need rounded up to whole nodes). Moves go to
  the model worse off by weight, and this one gave. Raise the quota by a
  replica, lower a floor, or accept the trade.
- **A P/D model with a blocked reason:** `wva_model_scaling_blocked` has no
  `role` label. Read `wva_utilization_share_headroom` and
  `wva_utilization_share_actionable` by `role`.
- **A lowered quota does not shrink the fleet:** the optimizer does not evict.
  A group above its quota plans over what it holds and moves back under only as
  models scale down.
- **Transfers start but the model does not grow:** read its
  `wva_model_scaling_blocked` reasons.

### A model shrinks while no other model grows

**Symptom**: under the utilization-share optimizer a model loses one or more
replicas, at least a few cycles plus one scale-down window after its load fell, and no other
model in the quota group grows to take the GPUs.

**What it means**: the model gave headroom back to the quota. This happens only
while the quota is **contended**.

- **Contended** means the quota cannot hold every model at its peak need over
  the last `stabilization.flappingWindow` at once (a peak counts once two
  readings in a row reach it). While the peaks fit, the whole spare stays spread
  by weight and nothing is given back.
- **Unknown peaks count high.** While no model's peak is known (a new group, or
  every model after a restart with no usable stored history, see step 8 above),
  each counts at its `maxReplicas` (the whole quota without one) until a
  `flappingWindow` of history shows the peaks fit. A model that joins a group
  whose peaks are known counts at what it holds, and does not turn the cap on
  by itself.
- **The cap.** While contended, each model (each role of a P/D model) keeps at
  most `maxHeadroomReplicas` (default `1`) times its weight, rounded up, above
  its need. The rest of the quota stays unassigned, so a model whose demand
  rises can start pods at once instead of waiting for another model's pods to
  finish their requests.
- **The return.** A model that held more than its need plus headroom for
  `stabilization.consecutiveCyclesBeforeMove` cycles in a row (two by default),
  typically after a burst ended, gives all the extra back in one cycle. The
  release then waits its scale-down window, as today's scale-down would. It
  returns only while:
  - no model in the group that can grow is short of its need;
  - its load would stay at or below `stabilization.returnSpareWhenLoadAtMost`
    (default `0.6`) on what it keeps, not counting headroom.

  While more than 0.6 x `queueLengthThreshold` requests (3 by default) wait on
  one of its pods for `consecutiveCyclesBeforeMove` cycles in a row, the return
  also waits, for at most its scale-down window. A decode pod of a P/D model
  counts only while its KV cache is full; prefill pods are not counted.
- **The floor.** It never goes below its need (the higher of current and
  average need while it swings) plus headroom, below what the other models'
  recent peaks leave of the quota in whole replicas, its `minReplicaCount`, or
  its last replica. A model that bursts alone keeps its spare; GPUs are returned
  only when another model's recent peak could use them.
- **Calling it off.** If, during the scale-down window, the model is short of
  its target again or busier than that load on what it keeps, the return is
  called off and it keeps the replica. If another model turns short while the
  return is still releasing, the returned GPUs go to the unassigned GPUs as
  usual, and the idle fill raises that model into them once the replica is
  released.

**How to tell**: the donor's scale target carries a `UtilizationShareGiving`
Event that names "the quota group's unassigned GPUs" and says "because it holds more headroom than maxHeadroomReplicas gives it while no model is short, and stays calm without it; the replica drains after its scale-down window, and is kept if its demand comes back before then; a model whose demand rises takes the GPUs at once". A return called off carries a
`UtilizationShareCancelled` Event: "this model's demand came back, so it keeps
the replica it was giving back". The controller log has
`Utilization share: transfer started` with `returned` set to true and no
`receiver`, then `Utilization share: spare returned to the quota` when the
replica is released. `wva_utilization_share_starts_total{kind="return"}`
counts returns per quota group. A model that holds more than its headroom and
is not giving it back shows why in `wva_utilization_share_withheld_total`:
`return-not-calm` (its load is above `returnSpareWhenLoadAtMost`),
`return-pod-busy` (its load would let the return go, but more than
`returnSpareWhenLoadAtMost` x `queueLengthThreshold` requests are waiting on
one of its pods; counted until the scale-down window bound passes),
`return-role-short` (another model is short of its need) or
`return-receiver-unplaceable` (the only models below their target are ones no
node can hold a new replica of, so the GPUs would sit unassigned).

Whether the cap is on: `wva_utilization_share_headroom_capped == 1` for the
group (published only while the optimizer acts, not in shadow mode, and not
during the restart quiet period, when the planner does not plan). The log says
when it flips, with `peakClaimGPUs` (the models' peaks summed in whole
replicas), `budget` and a `cause`. It turns on with one of:

- `cause=peaks-unknown`: `Utilization share: the models' peaks are not known
  yet -- ...`, with `unknownPeaks` naming the roles counted at their
  `maxReplicas`. This is the new-group rule above, or the restart rule when
  no `need history restored` line preceded it: it turns off
  once a `flappingWindow` of history exists
  (`wva_utilization_share_effective_seconds{param="swing-window"}`), if the
  recorded peaks fit.
- `cause=peaks-exceed-budget`: `Utilization share: the models' recent peaks do
  not fit the quota together; each keeps at most maxHeadroomReplicas of
  headroom, and the rest is returned`. The recorded peaks really do not fit.

It turns off with `Utilization share: the models' recent peaks fit the quota
together again; the spare is spread by weight`. Each line carries `scope` as
the metrics label it (`cluster` for the cluster group), so it joins the
group's series.

```bash
kubectl get events -n <model-namespace> --field-selector reason=UtilizationShareGiving | grep unassigned
kubectl logs -n <controller-namespace> -l app.kubernetes.io/name=workload-variant-autoscaler --tail=-1 | grep 'spare returned to the quota'
kubectl logs -n <controller-namespace> -l app.kubernetes.io/name=workload-variant-autoscaler --tail=-1 | grep -E 'recent peaks|peaks are not known'
```

**What to do**: nothing, if the model still serves within its targets. The GPUs
are free for whichever model needs them next, and this model takes them back the
same way when its demand rises. Otherwise:

- **More spare on each model:** raise `utilizationShare.maxHeadroomReplicas`. A
  value as large as the models' `maxReplicas` spreads the whole spare by weight
  even while contended and leaves little to return, but a model whose demand
  rises must then wait for another model's pods to drain.
- **Keep replicas longer after demand falls:** lower
  `stabilization.returnSpareWhenLoadAtMost`.
- **Protect one model:** raise its `minReplicaCount`. A higher `weightClass`
  gives it more headroom replicas (weight times `maxHeadroomReplicas`, rounded
  up) but does not shield them from another model's need.

See the [scaling policy reference](scaling-policy.md#optimizer-cluster-default-only-live).

### A role is held by floors or by a release that failed

Read the role's reason first:

```promql
wva_model_scaling_blocked{reason=~"floor-pinned|floors-exceed-quota|donors-at-floor|release-timeout|release-taken|release-shape-mismatch"} == 1
```

| reason | means | what to do |
| --- | --- | --- |
| `floor-pinned` | the model's `minReplicaCount` holds at least one replica more than it needs; the share cannot give those GPUs to others | lower `minReplicaCount` if the floor is not deliberate |
| `floors-exceed-quota` | the floors of the group's models add up to more than its budget; every model of the group shows it | lower a floor or raise the quota |
| `donors-at-floor` | the role is short, and no other role holds more than its floor to give | lower another model's `minReplicaCount`, or raise the quota |
| `release-timeout` | the role, as a donor, did not scale down within the release timeout; the release was aborted and it backs off before it is asked again | check the donor's ScaledObject scale-down window, its pods' termination grace, and that KEDA acts on it |
| `release-taken` | the role's last fill timed out: the GPUs its donors released were taken by a pod the scaling manager did not place. Cleared after one release timeout | look for other workloads scheduling onto that accelerator; a `ResourceQuota` or a dedicated node pool keeps them off. For a whole-node model, a `priorityClassName` above the other GPU pods lets its Pending pod evict the one that took the GPU, at the cost of evicting lower-priority pods whenever it waits (see [whole-node models](scaling-policy.md)) |
| `release-shape-mismatch` | the role's last fill timed out: enough GPUs were free in total, but on no node enough for its largest pod. Cleared after one release timeout | the GPUs are fragmented; free a whole node, or use smaller replicas |

The reasons and their timing are in
[utilization-share metrics](prometheus.md#utilization-share-optimizer-metrics).

### A model shows `no-compatible-donor`

**Symptom**: `wva_model_scaling_blocked{reason="no-compatible-donor"} == 1` on a
model that is short, while other models on the same accelerator type hold more
than they need.

**What it means**: each of the model's pods needs a donor pod at least as large.
A 4-GPU pod cannot be funded by two 2-GPU pods, since nothing shows they share a
node. No donor in the group has pods large enough.

**What to do**: no optimizer setting changes this. Raise the quota for that
accelerator type, or lower a model whose pods are at least as large. With pods
of more than one size (P/D roles with different GPU counts), each role is judged
on its own pods; find the short one with `wva_utilization_share_headroom < 0`.

### A large model cannot be placed

**Symptom**: a model with large replicas (often one whole 8-GPU node each)
stays short, and no transfer toward it starts. It shows one of these reasons:

```promql
wva_model_scaling_blocked{reason=~"no-node-fits|no-compatible-donor|release-shape-mismatch|release-taken"} == 1
```

All four mean the same thing for an operator: the optimizer cannot get a
replica of the model onto a node. Alert on all four together (see
[monitoring](monitoring.md#the-metrics-that-answer-specific-questions)).

- `no-node-fits`: no node can hold one replica, even if every pod the other
  models in its group could give on that node went. The rest of each node is
  held by pods the scaling manager does not plan, or by a held role's pods
  (then it clears when the hold ends). It is set every cycle while the model
  is short. It needs node information (a cluster-scoped install, or the
  `gpu-inventory` limiter).
- `no-compatible-donor`: no donor pod, or set of donor pods on one node, can
  host a replica. Without node information, fragmentation shows as this.
- `release-shape-mismatch` and `release-taken`: a transfer's GPUs came free,
  but the replica still did not land. Each expires one release timeout later.

**What it means**: the GPUs are fragmented. The quota may have room, but no
node has a whole replica's GPUs free. While this model is the only one below
its target, the other models keep their spare rather than give it back to
GPUs that would sit unassigned; each cycle this happens is counted as
`return-receiver-unplaceable` in `wva_utilization_share_withheld_total`.
Without the optimizer, the same quota would order a replica that stays
Pending.

**What to do**: keep the large model's nodes whole. Put it on its own node pool
or behind a taint it alone tolerates, or use a bin-packing scheduler profile so
small pods fill nodes before they spread. Lowering another model's floor does
not help with `no-node-fits`: it already counts every pod those models could
give.

### Transfer marks left on pods after a downgrade

**Symptom**: after moving to a version without the utilization-share optimizer,
some pods still carry `llm-d.ai/utilization-share-transfer` and a low
`controller.kubernetes.io/pod-deletion-cost` (`-1000`, or lower when a sibling's
cost was lower), so the ReplicaSet removes them first on every scale-down. The
same happens after renaming a controller's `CONTROLLER_INSTANCE`: the controller
removes only marks carrying its current instance, so those written under the old
name stay.

**Prevention**: before downgrading (or renaming `CONTROLLER_INSTANCE`), set
`shadow: true` (or remove the `optimizer:` block) and wait one optimization
cycle. The controller removes its marks and restores each pod's previous
deletion cost. A namespace-scoped install that reads cluster policy from a
separate policy namespace needs a restart for the edit: edit, restart, wait a
cycle. Then check none are left. Both commands below read every namespace
(cluster admin); a tenant uses `-n <ns>` instead of `-A`:

```bash
kubectl get pods -A -o json | jq -r '.items[]
  | select(.metadata.annotations["llm-d.ai/utilization-share-transfer"] != null)
  | .metadata.namespace + "/" + .metadata.name'
```

**Cleanup after the fact**: the mark records the pod's deletion cost from before
the transfer as `prevCost`; restore it, or remove the cost when the mark has
none, and remove the mark (cluster admin as written; `-n <ns>` for a tenant):

```bash
kubectl get pods -A -o json \
  | jq -r '.items[]
      | select(.metadata.annotations["llm-d.ai/utilization-share-transfer"] != null)
      | [.metadata.namespace, .metadata.name,
         ((.metadata.annotations["llm-d.ai/utilization-share-transfer"] | fromjson? | .prevCost) // "none")]
      | @tsv' \
  | while read -r ns pod prev; do
      if [ "$prev" = "none" ]; then
        kubectl annotate pod -n "$ns" "$pod" \
          llm-d.ai/utilization-share-transfer- \
          controller.kubernetes.io/pod-deletion-cost-
      else
        kubectl annotate pod -n "$ns" "$pod" --overwrite \
          llm-d.ai/utilization-share-transfer- \
          "controller.kubernetes.io/pod-deletion-cost=$prev"
      fi
    done
```

A mark that is not valid JSON is treated as having no previous cost. If several
controllers with different `CONTROLLER_INSTANCE` values share the cluster, each
mark names its writer in its `instance` field; run the cleanup only for the
instances that were downgraded (or renamed), by adding that field to the
`select`. A controller with `CONTROLLER_INSTANCE` unset writes no `instance`
field at all, so match it with `== null`, not `== ""`:

```jq
# marks of the instance named old-name
select((.metadata.annotations["llm-d.ai/utilization-share-transfer"] | fromjson? | .instance) == "old-name")
# marks of a controller with CONTROLLER_INSTANCE unset
select((.metadata.annotations["llm-d.ai/utilization-share-transfer"] | fromjson? | .instance) == null)
```
