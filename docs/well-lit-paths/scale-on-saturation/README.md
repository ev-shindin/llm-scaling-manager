# Scale a model on saturation

The scaling manager reads what your inference engines are doing (KV-cache use,
queues, token rates) and tells KEDA (the Kubernetes event-driven autoscaler)
how many replicas each model needs. This is
the default path for anyone serving llm-d models; every other path builds on it.

**Maturity:** Stable.

## When not to take it

- The bottleneck is outside the engine: a gateway, a tokenizer service, the
  network. The scaling manager only sees engine and EPP metrics.
- Nothing scrapes vLLM/SGLang and EPP (the llm-d request router) metrics into
  Prometheus. It has no opinion it did not read from a metric.

## Start here

You need llm-d model servers already serving (none yet?
[install a small model](../../guides/install-small-model/)), Prometheus
scraping them and their EPP, the EPP's `flowControl` feature gate, and a
cluster admin for KEDA. Every step is in
[Install the scaling manager in a namespace](../../guides/install-in-namespace/).

1. Namespace owner: [check the prerequisites](../../guides/install-in-namespace/README.md#prerequisites). The check is read-only.
2. Cluster admin, once per namespace: [prepare the namespace](../../guides/install-in-namespace/README.md#1-cluster-admin-prepare-the-namespace).
3. Namespace owner (the admin, if the controller runs in `wva-<ns>`): [install the controller](../../guides/install-in-namespace/README.md#2-install-the-controller).
4. Namespace owner: [make the workloads safe to scale](../../guides/install-in-namespace/README.md#3-make-the-workloads-safe-to-scale). The patch it writes changes nothing until you apply it.
5. Namespace owner: [register the workloads](../../guides/install-in-namespace/README.md#4-register-the-workloads). Nothing scales before this step.
6. Whoever ran step 3: confirm only KEDA can reach the scaler port with `make verify-deployment` ([details](#details-the-scaler-port)).
7. [Check it works](../../guides/install-in-namespace/README.md#verification): ScaledObjects (KEDA's per-workload objects) show `READY True`, and decisions appear in a few minutes.

To undo, see [Cleanup](../../guides/install-in-namespace/README.md#cleanup). For
one controller serving every namespace, a cluster admin can
[install cluster-wide](../../guides/install-cluster-wide/) instead.

## What it costs

- No accelerators. The controller is a small deployment.
- One set of Prometheus queries per namespace each cycle, not one per pod.

## Evidence

No benchmark has been run on this engine yet.

- A [July 2026 study](../../comparison/keda-alone.md) compared five scenarios
  against a KEDA threshold, one run each, on an older controller.
- Benchmark scenario: `hack/benchmark/scenarios/guides/workload-autoscaling.yaml`,
  run with [Benchmark the scaling manager](../../guides/benchmarking/).

## Details

### What it needs

- KEDA on the cluster, and Prometheus scraping the model servers and the EPP.
- A KEDA `ScaledObject` per workload. The controller has no watch and no
  listing, so until a ScaledObject exists it runs and does nothing.
- Nothing else. The scaling manager holds no accelerators of its own.
- `make check-prereqs` and `make deploy-wva` refuse to run without the EPP's
  `featureGates: [flowControl]` in its `EndpointPickerConfig`.

### When the controller is out of your reach

The admin may run the controller in `wva-<ns>`, a namespace the tenant cannot
edit. Then add `WVA_NS=wva-<ns> WVA_WATCH_NS=<ns>` to `make scaledobjects-plan`
and `make scaledobjects-apply`. The admin runs `make deploy-wva`,
`make verify-deployment` and `make undeploy-wva`, and the controller log is
the admin's to read. The controller runs in `$NAMESPACE` in the default
install and in `workload-variant-autoscaler-system` (by default) cluster-wide.

`make undeploy-wva` leaves the model servers running. ScaledObjects you adopted
stay in place and are listed for you to repoint or remove. Cluster-scoped
objects the admin created stay until the admin removes them
([what an uninstall leaves behind](../../guides/install-in-namespace/README.md#what-an-uninstall-run-by-you-leaves-behind)).

### Is my model scaled right?

Four checks, cheapest first. `<modelID>` is the trigger's `modelID`, which must
equal the engine's `--served-model-name`. `WVA_NS` is the controller's namespace.

1. **The engine series exist for this model.** Everything else depends on it:

   ```promql
   count by (model_name) (vllm:kv_cache_usage_perc{model_name="<modelID>"})
   # SGLang: count by (model_name) (sglang:token_usage{model_name="<modelID>"})
   ```

   An empty result means the scaling manager cannot see this model. It reports
   a `MetricsUnavailable` Warning Event on the ScaledObject
   (`kubectl get events -n "$NAMESPACE" --field-selector reason=MetricsUnavailable`).
   Fix the scrape (a ServiceMonitor or PodMonitor reaching `/metrics`) or the
   name (`--served-model-name` vs `modelID`) before tuning anything.
2. **Utilization sits inside the band.** Read the decision's reason:

   ```bash
   kubectl logs -n "$WVA_NS" deploy/wva-controller-manager | grep analyzer-result
   ```

   Compare `util` with `scaleUpThreshold` and `scaleDownBoundary` on the same
   line. Under steady load `util` should settle between them. A `util` stuck
   above the threshold while replicas do not grow points at a ceiling
   (`maxReplicaCount`, a limiter), not at the analyzer.
3. **The target matches what runs.**

   ```promql
   wva_desired_replicas{exported_namespace="<your-namespace>", variant_name="<scaledobject>"}
     -
   wva_current_replicas{exported_namespace="<your-namespace>", variant_name="<scaledobject>"}
   ```

   A gap that lasts longer than the HPA's scale-down window (300 s as
   generated) or a pod's start time means KEDA is not acting. Check
   `kubectl get scaledobject,hpa -n "$NAMESPACE"` for `READY`/`ACTIVE` and the
   HPA's conditions.
4. **Latency is what you wanted.** TTFT (time to first token) from the
   engine's own histogram:

   ```promql
   histogram_quantile(0.9, sum by (le) (rate(
     vllm:time_to_first_token_seconds_bucket{model_name="<modelID>"}[5m])))
   ```

   TTFT too high with `util` inside the band means the band is too high for
   this model: move it to a lower [tier](../workload-classes/#choosing-a-tier).
   TTFT well under target with many replicas means the band can go up.

A target lower than demand implies is usually a limiter at work, and the
reason is reported; see [bound a fleet by real GPUs](../bound-by-gpus/). PromQL
takes the namespace literally. `exported_namespace` is the workload's namespace;
plain `namespace` is the controller's
([why alerts group by the former](../../reference/monitoring.md)). Log fields
are described in [the cycle log](../../reference/cycle-log.md).

### How it is tested

- End-to-end: `test/e2e/saturation_v2_test.go` (`Saturation V2 engine`),
  `saturation_analyzer_path_test.go`, `saturation_config_test.go`,
  `external_scaler_keda_test.go`, `smoke_keda_test.go`.
- The benchmark harness's own start-path settings are in
  [Replica start time in the harness](../../guides/benchmarking/README.md#replica-start-time-in-the-harness).

### Tuning it

Thresholds, analyzer choice and tiers are in the
[scaling policy reference](../../reference/scaling-policy.md). How a
measurement becomes a replica count is in
[the steady-state engine](../../concepts/steady-state-engine.md).

### Details: the scaler port

The scaler port `:9090` is plaintext. `make verify-deployment` reads the
`wva-external-scaler` NetworkPolicy that `make setup-prereqs` writes when it
knows KEDA's namespace. A tenant usually cannot read that policy in `wva-<ns>`,
so the admin runs the check there (adding `WVA_NS=wva-<ns> WVA_WATCH_NS=<ns>`).
A policy you cannot read is reported as not checked, not as open.

If it prints `No NetworkPolicy: scaler port open`, the cluster admin re-runs
`make setup-prereqs` in the install's shape with
`KEDA_NAMESPACE=<namespace KEDA runs in>` (the warning prints the exact
command). Or serve the scaler over mutual TLS
([network exposure](../../reference/security-model.md#network-exposure)).
