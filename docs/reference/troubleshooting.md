# Troubleshooting


## Deployment Not Scaling Up

**Symptom**: Deployment remains at 0 replicas despite pending requests.

**Possible Causes**:

1. **InferencePool datastore is empty**:
   ```bash
   # Check if InferencePool exists and is reconciled
   kubectl get inferencepool
   ```
   
   the scaling manager watches a single InferencePool API group (`inference.networking.k8s.io` or `inference.networking.x-k8s.io`). If the cluster's pools use the other group, the datastore stays empty and scale-from-zero never gets a recommendation.
   
   **Solution**: Ensure InferencePool is created and reconciled before the workload's ScaledObject. When using **`make deploy-e2e-infra`**, `deploy/install-epp.sh` installs the GAIE standalone chart which creates the InferencePool after the EPP starts.

2. **Labels mismatch**:
   ```bash
   # Check deployment labels
   kubectl get deployment llama-8b-deployment -o jsonpath='{.spec.template.metadata.labels}'
   
   # Check InferencePool selector
   kubectl get inferencepool llama-pool -o jsonpath='{.spec.selector}'
   ```
   
   **Solution**: Ensure deployment labels match InferencePool selector.

3. **EPP metrics source not available**:
   ```bash
   # Check if EPP service exists
   kubectl get svc | grep epp
   ```
   
   **Solution**: Verify EndpointPicker service is running and metrics are being collected.

4. **No pending requests in queue**:

   Extract the Bearer token from the EPP metrics reader secret:
   ```bash
   TOKEN=$(kubectl -n workload-variant-autoscaler-system get secret wva-epp-metrics-token -o jsonpath='{.data.token}' | base64 --decode)
   ```

   Port-forward the EPP metrics service to localhost:9090:

   ```bash
   kubectl port-forward svc/epp 9090:9090
   ```

   In a separate terminal, query the metrics endpoint:
   ```bash
   curl -H "Authorization: Bearer $TOKEN" localhost:9090/metrics | grep inference_extension_flow_control_queue_size
   ```

   **Solution**: Verify requests are being sent to the correct model endpoint.

   The metric family was renamed: llm-d's EPP exports
   `llm_d_epp_flow_control_queue_size` and upstream gateway-api-inference-extension
   still exports `inference_extension_flow_control_queue_size`. Grep for both — the scaling manager
   reads whichever exists.

### The EPP scrape is failing but the scaling manager still wakes models (slowly)

**Symptom**: the log carries

```text
Scale-from-zero: EPP metrics scrape failing; reading the flow-control queue from
Prometheus instead. Wakes still work but are slower and bounded by the scrape
interval — fix the direct path.
```

and `wva_scale_from_zero_queue_fallback_active{pool="..."}` is `1`.

**What it means**: The scaling manager reads the flow-control queue by scraping the EPP pod
**directly** — pod IP, EPP metrics port, bearer token projected at
`/var/run/secrets/epp-metrics/token`. Every other metric it consumes comes from
Prometheus, so this one path can fail on its own. When it does, the scaling manager falls back to
reading the same metric from Prometheus so models still wake. Nothing looks broken
from the outside; wakes are just slower — bounded by the Prometheus scrape interval
rather than the engine's 100 ms loop — and a sample older than 90 s is ignored, so
a queue that has already drained cannot wake anything.

This is a degraded state, not a supported one. Check, in order:

1. **The token.** `Failed to read EPP metrics token` in the log means the projected
   volume is missing; the scaling manager then scrapes unauthenticated and the EPP rejects it.
   ```bash
   kubectl -n workload-variant-autoscaler-system exec deploy/wva-controller-manager --      ls -l /var/run/secrets/epp-metrics/token
   ```
2. **The EPP's tokenreview RBAC**, which authorizes the scaling manager's token. It is bound per
   release name, so a renamed or reinstalled EPP release leaves the scaling manager's binding
   pointing at nothing.
   ```bash
   kubectl get clusterrolebinding | grep epp-tokenreview
   ```
3. **Network reachability to the EPP pod IP.** Prometheus scrapes the EPP too, so a
   NetworkPolicy admitting the monitoring namespace but not the scaling manager's breaks exactly
   this path and no other.

The gauge returns to `0` and the log reports the scrape recovered once the direct
path works again.

### E2E and infra-only deploys

For e2e-style deploys, **`deploy/install-epp.sh`** enables EPP flow control when `ENABLE_SCALE_TO_ZERO=true` (adds the `flowControl` feature gate to the GAIE standalone chart). The **InferenceObjective** `e2e-default` is created by the scale-from-zero e2e tests (`test/e2e/fixtures`), not by the install scripts. See [deploy/install-epp.sh](../../deploy/install-epp.sh).

## Slow Scale-Up Response

**Symptom**: Deployment takes too long to scale up from zero.

**Possible Causes**:

1. **High concurrent processing load**:
   
   This can happen when there are many variants that are scaled down to zero, causing the scale-from-zero engine to process multiple scaling decisions simultaneously.
   
   **Solution**: Increase `SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY`:

   Add the environment variable to the scaling manager controller deployment:

   ```yaml
   apiVersion: apps/v1
   kind: Deployment
   metadata:
      name: controller-manager
      namespace: workload-variant-autoscaler-system
   spec:
      template:
         spec:
            containers:
            - name: manager
            env:
            - name: SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY
              value: "50"  # Increase for larger clusters
   ```

2. **Inference gateway not receiving requests**:
   
   **Solution**: Verify that requests are being routed through the inference gateway and not directly to model server endpoints.

## Utilization-Share Optimizer

Configuration: [`optimizer` in the scaling policy](scaling-policy.md#optimizer-cluster-default-only-live).
Metrics and blocked reasons: [utilization-share metrics](prometheus.md#utilization-share-optimizer-metrics).

### The optimizer evaluates but never moves anything

Work down the list in order.

1. **Is it selected at all?** `wva_utilization_share_mode` says: `off`,
   `invalid`, `shadow` or `active` is `1`. For `invalid` or `off`, the log says
   why: `Utilization share: invalid optimizer block; keeping today's optimizer`
   (a misspelled key, a value out of range, an empty `clusterNamespaces` entry,
   or `needs a limiters: list`, no budget to share). A block in a
   namespace-local scaling-policy map is ignored:
   `ignoring optimizer blocks in namespace-local scaling-policy maps`.
2. **Is it in shadow mode?** `wva_utilization_share_mode{mode="shadow"} == 1`;
   the log says `Utilization share: would rebalance (shadow)` rather than
   `Utilization share: rebalancing`, and `wva_utilization_share_promised_gpus` is
   absent: it is published only while the optimizer acts. Set `shadow: false`
   (or remove the key) to act. Both lines are written when the set of actionable
   roles changes, not every cycle, so a quiet log is not a stopped optimizer.
3. **Is the model planned?** The two lines above, and at `--v=4` the per-cycle
   `Utilization share: evaluation`, carry a `frozen` field listing the
   group's models left to today's optimizer this cycle, with the reason: no live
   analyzer result, a role with demand but no measured capacity, a
   namespace-local scaling-policy map (not planned in the cluster group), or a
   namespace not in `clusterNamespaces`. A model whose variants run on more than
   one accelerator type, one in a namespace with
   `enabled: false`, or one under only a `gpu-inventory` limiter without
   `physicalGroups: true` is in no group at all and publishes no role series.
4. **Is any role actionable?** `wva_utilization_share_actionable` is `1` only
   for a role outside its tolerance band *and* off its whole-replica target. All
   `0` means the fleet is already where the optimizer wants it, or every
   imbalance is smaller than one replica; `wva_utilization_share_withheld_total{reason="not-actionable"}`
   counts the latter.
5. **Does it stay actionable?** A role must be actionable on **two consecutive
   cycles** before a transfer is planned for it, so a role that flickers in and
   out of band never moves. At most two replicas move per role per cycle and two
   transfers run per group at once.
6. **Is it held?** `withheld_total{reason="reversal-hold"}` rising, or the
   model's blocked reason `reversal-hold`, means a role that just gave is being
   kept from receiving (or the reverse) until the hold in
   `wva_utilization_share_effective_seconds{param="reversal-hold"}` passes. That
   is the anti-oscillation rule working, not a fault. Its worst case: a role that
   gave cannot receive for the whole hold, about twice a release time from the
   start of the transfer it gave in, **even when it is now below its need**
   (urgent receivers are held too). With a 300 s scale-down window that is on the
   order of 12 minutes.
7. **Is it swinging, or at the transfer limit?** Blocked reason `swinging`
   (`wva_utilization_share_swinging == 1`) means the role reversed direction
   twice within the swing window and is planned on its mean need, not its
   current one, so it may look short and still not receive. `transfer-limit`
   means the group already runs two transfers with a donor; it clears as they
   land.
8. **Did the controller just restart?** After a restart -- and after the
   optimizer starts acting, or a group first appears -- every planned model of
   the group holds what it runs (or its restored target) for one fill timeout,
   about three minutes with default timings, and nothing in it scales:
   a fill in flight before the restart has no mark, and its receiver's pods
   must not lose their GPUs meanwhile. Expect this freeze on every upgrade or
   leader change. The log line `Utilization share: ledger started` carries
   `planningFrom`.
9. **Could it not mark the donor?** `Utilization share: could not mark a donor pod;
   transfer not started` means the donor had a pod that was not Ready (on a
   Deployment) or not yet scheduled (the ReplicaSet removes such a pod before it
   reads the deletion cost, so the choice of pod cannot be steered), had no pod
   left to give, or the patch failed. The donor backs off, shows the blocked
   reason `donor-not-steerable`, and is retried after the back-off.

If transfers do start but the model still does not grow, read its
`wva_model_scaling_blocked` reasons.

### A model shows `no-compatible-donor`

**Symptom**: `wva_model_scaling_blocked{reason="no-compatible-donor"} == 1` on a
model that is short, while other models on the same accelerator type hold more
than they need.

**What it means**: every one of the model's pods needs a donor pod at least as
large to take its place: a 4-GPU pod cannot be funded by two 2-GPU pods, since
nothing shows the two share a node. Here no donor in the group has pods large
enough, so no transfer can be planned for this model.

**What to do**: the optimizer has no setting that changes this. Give the model
GPUs another way: raise the quota for that accelerator type, or lower a model
whose pods are at least as large. If the model has pods of more than one size
(P/D roles with different GPU counts), each role is judged on its own pods; check
which role is short with `wva_utilization_share_headroom < 0`.

### Transfer marks left on pods after a downgrade

**Symptom**: after moving to a version without the utilization-share optimizer,
some pods still carry `llm-d.ai/utilization-share-transfer` and
`controller.kubernetes.io/pod-deletion-cost: "-1000"`, so the ReplicaSet removes
them first on every scale-down.

**Prevention**: before downgrading, set `shadow: true` (or remove the `optimizer:`
block) and wait one optimization cycle. The controller removes the marks it wrote
and restores each pod's previous deletion cost. Check that none are left:

```bash
kubectl get pods -A -o json | jq -r '.items[]
  | select(.metadata.annotations["llm-d.ai/utilization-share-transfer"] != null)
  | .metadata.namespace + "/" + .metadata.name'
```

**Cleanup after the fact**: the mark records the pod's deletion cost from before
the transfer as `prevCost`; restore it, or remove the cost when the mark has
none, and remove the mark:

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
instances that were downgraded, by adding that field to the `select`.
