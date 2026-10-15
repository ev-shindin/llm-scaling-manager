# Prometheus integration

The scaling manager reads its inputs (vLLM, EPP) from Prometheus and publishes
its own metrics for Prometheus to scrape. This page covers connecting it to
Prometheus, securing that connection, every metric it publishes, and the shipped
alerts.

## Configuration

The scaling manager supports two methods for configuring Prometheus connectivity:

### 1. Environment variables (recommended)

Set the Prometheus settings as environment variables on the controller
Deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: wva-controller-manager
  namespace: <controller-namespace>
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        # Required: Prometheus server URL
        - name: PROMETHEUS_BASE_URL
          value: "https://prometheus-k8s.monitoring.svc.cluster.local:9091"

        # Optional: TLS configuration
        - name: PROMETHEUS_TLS_INSECURE_SKIP_VERIFY
          value: "false"  # production: verify, with PROMETHEUS_CA_CERT_PATH set.
                          # The shipped Kubernetes config sets "true"; see the table.

        - name: PROMETHEUS_CA_CERT_PATH
          value: "/etc/prometheus-certs/ca.crt"

        - name: PROMETHEUS_CLIENT_CERT_PATH
          value: "/etc/prometheus-certs/client.crt"

        - name: PROMETHEUS_CLIENT_KEY_PATH
          value: "/etc/prometheus-certs/client.key"

        - name: PROMETHEUS_SERVER_NAME
          value: "prometheus-k8s.monitoring.svc.cluster.local"

        # Optional: Bearer token authentication
        - name: PROMETHEUS_BEARER_TOKEN
          valueFrom:
            secretKeyRef:
              name: prometheus-token
              key: token
```

**Environment Variable Reference:**

| Variable | Required | Description | Default |
|----------|----------|-------------|---------|
| `PROMETHEUS_BASE_URL` | Yes | Prometheus server URL (HTTPS only in production) | - |
| `PROMETHEUS_ALLOW_HTTP` | No | Allow a plain `http://` `PROMETHEUS_BASE_URL` (dev/test only; cannot be combined with TLS settings or bearer token auth) | `false` |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | No | Skip TLS certificate verification. The binary defaults to `false`, but the shipped Kubernetes config (`config/base/manager/manager-configmap.yaml`) sets `"true"`; the OpenShift overlay sets `"false"`. In production set `"false"` and point `PROMETHEUS_CA_CERT_PATH` at the CA that signed Prometheus's certificate | `false` (binary); `"true"` (shipped Kubernetes config) |
| `PROMETHEUS_CA_CERT_PATH` | No | Path to CA certificate for TLS verification | - |
| `PROMETHEUS_CLIENT_CERT_PATH` | No | Path to client certificate for mutual TLS | - |
| `PROMETHEUS_CLIENT_KEY_PATH` | No | Path to client private key for mutual TLS | - |
| `PROMETHEUS_SERVER_NAME` | No | Expected server name in TLS certificate | - |
| `PROMETHEUS_BEARER_TOKEN` | No | Bearer token for Prometheus authentication | - |

### 2. ConfigMap configuration

Or set them in the controller's ConfigMap, inside its `config.yaml` key (mounted
at `/etc/wva/config.yaml`). They are read at startup, so restart the controller
after a change.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-manager-config
  namespace: <controller-namespace>
data:
  config.yaml: |
    PROMETHEUS_BASE_URL: "https://prometheus-k8s.monitoring.svc.cluster.local:9091"
    PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"
    PROMETHEUS_CA_CERT_PATH: "/etc/prometheus-certs/ca.crt"
    PROMETHEUS_CLIENT_CERT_PATH: "/etc/prometheus-certs/client.crt"
    PROMETHEUS_CLIENT_KEY_PATH: "/etc/prometheus-certs/client.key"
    PROMETHEUS_SERVER_NAME: "prometheus-k8s.monitoring.svc.cluster.local"
```

Do not put a bearer token in the ConfigMap; use the Secret-backed env variable
above.

**Priority:** environment variables first, then the ConfigMap. With neither
setting `PROMETHEUS_BASE_URL`, the controller fails to start.

### Metrics Endpoint

The controller serves `/metrics` on port 8443 over HTTPS (`--metrics-secure=true`),
behind the Service `wva-controller-manager-metrics-service`. A caller needs a
bearer token whose identity may `get` the non-resource URL `/metrics`; anything
else gets `401`/`403`. To read it by hand, see
[Monitoring](monitoring.md#the-metrics-that-answer-specific-questions).

### ServiceMonitor configuration

The install ships the ServiceMonitor in
[`config/base/monitoring/servicemonitor.yaml`](../../config/base/monitoring/servicemonitor.yaml),
rendered as `wva-controller-manager-metrics-monitor` in the controller's namespace:
```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: wva-controller-manager-metrics-monitor
  namespace: <controller-namespace>
  labels:
    release: kube-prometheus-stack
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
      app.kubernetes.io/name: workload-variant-autoscaler
  endpoints:
  - port: https
    scheme: https
    interval: 10s
    path: /metrics
    bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
    tlsConfig:
      insecureSkipVerify: true
```

Prometheus scrapes with its own ServiceAccount token, so that account needs the
install's metrics-reader ClusterRole (`wva-metrics-reader-<hash>`, `get` on
`/metrics`; the suffix is per install). Pass
`PROMETHEUS_SA=<namespace>/<serviceaccount>` to grant your Prometheus
([configuration](configuration.md)). On Kubernetes the install binds it to the
first of these trusted sources:

1. `PROMETHEUS_SA`, when set;
2. the one non-default account of the Prometheus CRs in the namespace an
   explicitly set `PROMETHEUS_URL` addresses;
3. the kube-prometheus-stack this install deploys (`DEPLOY_PROMETHEUS=true`,
   with the URL in `MONITORING_NAMESPACE`).

A Prometheus the installer only detected is never granted. With none of the
three, it creates no binding and warns; `make check-prereqs` lists the binding
as optional.

Only those sources write or repoint the binding:

- A `make setup-prereqs` re-run with none of them (say an upgrade that left out
  the first install's `PROMETHEUS_SA` or `PROMETHEUS_URL`) **keeps** an existing
  binding and warns. The warning names the bound account, how to repoint it
  (`PROMETHEUS_SA=<namespace>/<serviceaccount>`, printed as the install's own
  `make setup-prereqs` command) and how to remove it
  (`kubectl delete clusterrolebinding wva-metrics-reader-rolebinding-<suffix>`).
- A binding the re-run cannot read is left alone and reported as unknown, with
  a `kubectl get clusterrolebinding <name> -o yaml` to check it.
- The controller-only phase (`make deploy-wva`) never reads, reports or patches
  the binding.



## Security considerations

### TLS configuration

**What ships:** the Kubernetes overlays set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "true"`
in the manager ConfigMap. The kube-prometheus-stack the installer deploys serves
a self-signed certificate that nothing mounts into the controller, so verifying
by default would fail every such install with `x509: certificate signed by
unknown authority`. The OpenShift overlays verify against the service CA and ship
`false`. The installer warns at the end of any install that connects over HTTPS
without verifying.

**Production Deployments:**
- Always use HTTPS endpoints (`https://`)
- Mount the CA that signed the Prometheus certificate into the controller, then point
  the controller at it (steps below)
- Never set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` in production

**Mounting a Prometheus CA.** The controller runs with a read-only root filesystem,
so the CA must arrive as a volume. With the CA in `ca.crt` and the controller in
`$WVA_NS`:

```bash
# Store the CA in a Secret next to the controller
kubectl create secret generic prometheus-ca -n "$WVA_NS" --from-file=ca.crt=ca.crt

# Mount it into the manager container
kubectl patch deployment wva-controller-manager -n "$WVA_NS" --type=json -p='[
  {"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"prometheus-ca","secret":{"secretName":"prometheus-ca"}}},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/-","value":{"name":"prometheus-ca","mountPath":"/etc/prometheus-certs","readOnly":true}}]'
```

Then, in the manager ConfigMap (`wva-manager-config`, key `config.yaml`), set
`PROMETHEUS_CA_CERT_PATH: "/etc/prometheus-certs/ca.crt"` and
`PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"`, and restart the controller
(`kubectl rollout restart deployment/wva-controller-manager -n "$WVA_NS"`).

- **GitOps:** put the same volume and mount in a kustomize patch, as
  `config/components/openshift/deployment-patch.yaml` does for its serving cert.
- **Installer re-runs** re-apply the manager ConfigMap from the overlay and reset
  both keys. Set them again afterwards, or keep them in your own overlay.

**The other direction, the controller's `/metrics`:** the shipped ServiceMonitor
scrapes it with `insecureSkipVerify: true`, because the controller generates a
self-signed serving certificate in memory. To verify that scrape, start the
controller with `--metrics-cert-path` pointing at a mounted certificate and put
its CA in the ServiceMonitor's `tlsConfig`, as the OpenShift overlay does with
the service CA.

**Development/Testing:**
- You may set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` for local clusters
- Example (port-forwarding to Prometheus):
  ```bash
  # Terminal 1: Port forward Prometheus
  kubectl port-forward -n monitoring svc/prometheus-k8s 9091:9091

  # Terminal 2: Set environment for local development
  export PROMETHEUS_BASE_URL=https://127.0.0.1:9091
  export PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true
  ```
- If your Prometheus is reachable only over plain HTTP (e.g. `kube-prometheus-stack`'s
  default in-cluster service), set `PROMETHEUS_ALLOW_HTTP=true` and use an
  `http://` `PROMETHEUS_BASE_URL`. The controller refuses to start if
  `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY`, `PROMETHEUS_SERVER_NAME`, any
  `PROMETHEUS_CA_CERT_PATH`/client cert setting, or bearer token auth is set
  alongside it. Everything travels in cleartext, so use it only on a trusted
  network (dev/test or a secured in-cluster path).

### EPP metrics token

The controller scrapes the EPP's `/metrics` with the token of a dedicated
ServiceAccount, `wva-epp-metrics-reader`, whose only grant is `get` on the
`/metrics` non-resource URL. The token comes from a legacy
`kubernetes.io/service-account-token` Secret, `wva-epp-metrics-token`, mounted at
`/var/run/secrets/epp-metrics/token`. It **does not expire**, and it can read
`/metrics` endpoints and nothing else.

**To rotate it**, delete the Secret and re-apply the install (or `kubectl apply`
the Secret again). The token controller fills in a new token, and the old one
stops working once the Secret is gone. The controller re-reads the file on every
EPP scrape, so it picks up the new token once the kubelet updates the volume
(typically within a minute), with no restart.

**Why not a projected token:** a projected volume issues tokens only for the
Pod's own ServiceAccount. Switching would mean binding the reader role to the
controller's ServiceAccount and dropping the dedicated one, and the documented
`kubectl get secret wva-epp-metrics-token` reads (this page's monitoring
examples, the scale-to-zero and utilization-share guides) and the e2e suite all
use the Secret today.

### PromQL injection prevention

The scaling manager guards its queries against PromQL injection:

1. **Parameter Escaping**: All query parameters (namespace, model ID, variant name) are automatically escaped:
   - Backslashes are escaped: `\` → `\\`
   - Double quotes are escaped: `"` → `\"`

2. **Namespace Validation**: Namespace values are validated before use in PromQL queries to prevent malicious label matchers

**Example - Safe Query Construction:**
```go
// User input (potentially malicious)
namespace := `prod",malicious="value`

// the value is escaped automatically
escapedNamespace := EscapePromQLValue(namespace)
// Result: `prod\",malicious=\"value`

// Safe PromQL query
query := fmt.Sprintf(`vllm_kv_cache_usage{namespace="%s"}`, escapedNamespace)
// Result: vllm_kv_cache_usage{namespace="prod\",malicious=\"value"}
// Prometheus treats this as a literal string, preventing injection
```

This stops a crafted value from reading another namespace's metrics or adding
label matchers that change a query's result.



## llm-scaling-manager metrics

The metrics below describe the scaling manager's decisions and its own health.
All are served at `/metrics`.

### Notes on **name_space**s in metrics
On the scaling manager's metrics, `namespace` is the **controller's**
namespace. The workload's namespace is `exported_namespace`. An example, from a
test install whose controller ran in `workload-variant-autoscaler-system`:
```text
{
  "metric": "wva_desired_replicas",
  "labels": {
    "accelerator_type": "A100",
    "container": "manager",
    "endpoint": "https",
    "exported_namespace": "llm-d-sim",    <==== model namespace
    "instance": "10.244.0.73:8443",
    "job": "wva-controller-manager-metrics-service",
    "namespace": "workload-variant-autoscaler-system",  <=== controller namespace
    "pod": "wva-controller-manager-<hash>",
    "service": "wva-controller-manager-metrics-service",
    "variant_name": "workload-variant-autoscaler-va"
  },
  "value": "2"
}
```

### Configuration metrics

### `wva_config_info`
- **Type**: Gauge
- **Description**: The scaling manager configuration information (value is always 1)
- **Labels**:
  - `analyzer_name`: Name of the saturation analyzer in use
  - `limiter_enabled`: Whether the limiter is enabled (`true`, `false`)
  - `scale_to_zero_enabled`: Whether scale-to-zero is enabled (`true`, `false`)
- **Use Case**: Info-style metric to expose the scaling manager configuration via labels for monitoring and debugging
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_config_info",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "limiter_enabled": "false",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "scale_to_zero_enabled": "false",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_config_optimization_interval_seconds`
- **Type**: Gauge
- **Description**: Optimization interval in seconds
- **Labels**: None (global configuration)
- **Use Case**: Track how frequently the optimization loop runs
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_config_optimization_interval_seconds",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778846184.925,
      "30"
    ]
  }
  ```
### Metrics collection observability

### `wva_metrics_collection_duration_seconds`
- **Type**: Histogram
- **Description**: Duration of metrics collection operations in seconds
- **Labels**:
  - `query_type`: Type of metrics query being executed
- **Buckets**: 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5
- **Use Case**: Monitor metrics collection performance and identify slow queries
- ***Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_metrics_collection_duration_seconds_bucket",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "le": "0.001",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "query_type": "cache_config",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### `wva_metrics_collection_errors_total`
- **Type**: Counter
- **Description**: Total number of metrics collection errors
- **Labels**:
  - `query_type`: Type of metrics query that failed. `pod_list` is the controller's per-namespace Pod listing, a Kubernetes API read rather than a Prometheus query
  - `reason`: Reason for the error. For `pod_list`: `forbidden`, `unavailable` or `other`
- **Use Case**: Track metrics collection failures and identify problematic queries. A failed `pod_list` makes the controller treat every Pod in that namespace as Ready and present for the cycle, so a not-Ready Pod's saturated reading can become its variant's k2; a steady rate of `pod_list` errors (`reason="forbidden"` after an RBAC change) is worth an alert
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_metrics_collection_errors_total",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.59:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "query_type": "kv_cache",
      "reason": "bad_data",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778854211.669,
      "3"
    ]
  }
  ```

### `wva_metrics_pods_discovered`
- **Type**: Gauge
- **Description**: Number of pods discovered for a namespace
- **Labels**:
  - `namespace`: Kubernetes namespace
- **Use Case**: Monitor pod discovery to ensure all replicas are being tracked
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_metrics_pods_discovered",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_metrics_freshness_status`
- **Type**: Gauge
- **Description**: Freshness status of metrics for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `status`: Status of metrics freshness (`fresh`, `stale`, `missing`, `unavailable`)
- **Use Case**: Track metric staleness to ensure autoscaling decisions are based on current data
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_metrics_freshness_status",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "status": "fresh",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "2"
    ]
  }
  ```

### `wva_pod_mapping_miss_total`
- **Type**: Counter
- **Description**: Total number of pods whose metrics could not be attributed to a managed scaler — the pod locator's `ownerReferences` walk reached no owning HPA/ScaledObject. Makes the otherwise-silent skip observable.
- **Labels**:
  - `namespace`: Kubernetes namespace of the unattributed pod
  - `reason`: Why the pod was unattributed (currently always `unresolved`)
- **Use Case**: Alert on a rising rate of unattributed pods — usually a scaler missing the `llm-d.ai/managed` annotation, or a pod whose `ownerReferences` chain does not reach one
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_pod_mapping_miss_total",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.59:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "exported_namespace": "llm-d-sim",
      "reason": "unresolved",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778854211.669,
      "2"
    ]
  }
  ```

### KEDA external scaler metrics

### `wva_scaler_undecided_answers_total`
- **Type**: Counter
- **Description**: `GetMetrics` answers the KEDA external scaler gave with no usable decision behind them. Without one the scaler answers the target's current replica count, so the HPA holds; this counts those answers. See [before the first decision](scaledobject-trigger.md#before-the-first-decision-and-the-fallback).
- **Labels**:
  - `namespace`: the ScaledObject's namespace (scraped as `exported_namespace`; see the note on namespaces above)
  - `reason`: `no-decision` (a target the scaling manager plans but has not decided yet, normally the first cycle after a restart or a leader change), `stale` (its last decision is older than the decision TTL, 60 s or three optimization intervals, whichever is longer: no engine is republishing it), or `not-managed` (the trigger metadata does not parse, so no engine will ever decide it)
- **Use Case**: Which reason, and how fast. A counter grows on every poll of every undecided target, so its rate says more about polling than about how many models are frozen: alert on `wva_scaler_undecided_targets`, not on this.

### `wva_scaler_undecided_targets`
- **Type**: Gauge
- **Description**: ScaledObjects the KEDA external scaler is currently answering without a usable decision (with the target's current replica count). A ScaledObject enters on an undecided `GetMetrics` answer and leaves on the next decided one, when it is no longer registered, or two minutes after its last undecided answer (KEDA stops asking about a target parked at zero or deleted). Read at scrape time; a namespace with none has no series.
- **Labels**:
  - `namespace`: the ScaledObject's namespace (scraped as `exported_namespace`)
- **Use Case**: Non-zero for a cycle or two after a restart or leader change is expected. Alert when it stays above 0 for 10 minutes: those models' replica counts are frozen. The shipped rules do, as `WVAScalerTargetsUndecided` (see [Alerting Rules](#alerting-rules)). `wva_scaler_undecided_answers_total` says why. The controller logs `external scaler has no decision for this target` each time a ScaledObject enters the set, naming it, and `external scaler has a decision for this target again` when it leaves on a decision.

  ```yaml
  - alert: WVAScalerTargetsUndecided
    expr: max by (exported_namespace) (wva_scaler_undecided_targets) > 0
    for: 10m
  ```

### `wva_scaler_panics_total`
- **Type**: Counter
- **Description**: Panics in the KEDA external scaler's gRPC handlers that were recovered and answered with `Internal`, so one failing call does not take the controller down. Any non-zero value is a bug; the stack trace is in the controller log under `KEDA external scaler handler panicked`.
- **Labels**:
  - `method`: the full gRPC method, for example `/externalscaler.ExternalScaler/GetMetrics`
- **Use Case**: Alert on any increase.

### Optimization metrics

### `wva_optimization_duration_seconds`
- **Type**: Histogram
- **Description**: Duration of optimization loop cycles in seconds
- **Labels**:
  - `status`: Status of the optimization cycle (`success`, `error`)
- **Buckets**: 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10
- **Use Case**: Monitor optimization loop performance and identify slow optimization cycles
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_optimization_duration_seconds_bucket",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "le": "0.01",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "status": "success"
    },
    "value": [
      1778846184.925,
      "191"
    ]
  }
  ```

### `wva_models_processed`
- **Type**: Gauge
- **Description**: Number of models processed in the last optimization cycle
- **Labels**: None (global metric)
- **Use Case**: Track how many models are being processed per optimization cycle to understand workload
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_models_processed",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### Saturation and capacity metrics

### `wva_saturation_utilization`
- **Type**: Gauge
- **Description**: Per-variant utilization ratio from saturation analysis: TotalDemand / TotalCapacity from the analyzer result — **unbounded above**, where > 1.0 means demand exceeds supply. The value is capacity-weighted, so it stays correct for mixed-capacity replicas.
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Monitor KV cache utilization to understand saturation levels and trigger scaling decisions
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_saturation_utilization",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### `wva_spare_capacity`
- **Type**: Gauge
- **Description**: Spare capacity; >0 indicates scale-down headroom (per-role for P/D-disaggregated models, model-level otherwise). Use the `unit` label to interpret the value: `continuous` → **token surplus** from the **Token-based analyzer** (`max(0, TotalSupply - TotalDemand/scaleDownBoundary)`); empty → a 0.0-1.0 threshold-relative fraction from the **Percentage-based analyzer** (`kvCacheThreshold - avg KV usage`).
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
  - `unit`: `continuous` (Token-based analyzer — a token magnitude) or empty (Percentage-based analyzer — 0.0-1.0 fraction)
- **Use Case**: Track available capacity headroom to prevent saturation and optimize resource allocation
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_spare_capacity",
      "unit": "continuous",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "12000"
    ]
  }
  ```

### `wva_required_capacity`
- **Type**: Gauge
- **Description**: Required capacity; >0 indicates scale-up needed (per-role for P/D-disaggregated models, model-level otherwise). Use the `unit` label to interpret the value: `continuous` → **token demand** from the **Token-based analyzer**; `binary` → 0/1 scale-up signal from the **Percentage-based analyzer**.
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
  - `unit`: `continuous` (Token-based analyzer — a token magnitude) or `binary` (Percentage-based analyzer — 0/1 signal)
- **Use Case**: Identify when additional capacity is needed and understand the magnitude of demand
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_required_capacity",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### `wva_kv_cache_tokens_used`
- **Type**: Gauge
- **Description**: Total KV cache tokens currently in use across all replicas of a variant (sum of vLLM TokensInUse).
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `model_name`: Model name served by the variant
- **Use Case**: Monitor absolute KV cache token usage across variant replicas
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_kv_cache_tokens_used",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### Pipeline stage visibility metrics

### `wva_decisions_limited_total`
- **Type**: Counter
- **Description**: Total number of scaling decisions constrained by the limiter. This tracks how often the limiter prevents scaling actions due to resource constraints.
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `limiter_name`: Name of the limiter that constrained the decision
- **Use Case**: Monitor how frequently resource limiters are constraining scaling decisions to understand capacity bottlenecks
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_decisions_limited_total",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.60:8443",
      "job": "wva-controller-manager-metrics-service",
      "limiter_name": "gpu-limiter",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778855539.730,
      "1"
    ]
  }
  ```

### `wva_gpu_discovery_up`
- **Type**: Gauge
- **Description**: Indicates whether GPU discovery is on (1) or off (0). GPU discovery is enabled when a physical (`gpu-inventory`) limiter is declared in `limiters:`. This metric helps operators understand whether the scaling manager is actively discovering GPU resources.
- **Labels**: None (global metric, optional `controller_instance` label when multi-instance deployment is used)
- **Use Case**: Monitor GPU discovery status to ensure resource discovery is functioning when expected
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_gpu_discovery_up",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_pod_access_denied`
- **Type**: Gauge
- **Description**: `1` while a namespace-scoped controller with a `gpu-inventory` limiter in force (alone or beside quota limiters) may not list Pods cluster-wide, so the GPUs other namespaces hold on shared nodes cannot be counted; `0` otherwise. Under quota limiters alone, or none, nothing reads that count, so it stays `0`. Absent on a cluster-scoped controller.
- **Labels**: optional `controller_instance`
- **Use Case**: while `1`, GPU usage counts only the controller's own namespace, so the physical limiter can admit scale-ups onto nodes other namespaces fill. See [GPU limiter](gpu-limiter.md#when-policy-demands-a-limiter-the-controller-cannot-serve).

### `wva_config_rejected_entries`
- **Type**: Gauge
- **Description**: entries of a scaling-policy ConfigMap rejected at its last load (an invalid value or wrong type). A rejected entry keeps the previous entry under its key, if there was one. The series is removed when the ConfigMap is deleted.
- **Labels**: `namespace`, `configmap`, optional `controller_instance`
- **Use Case**: any value above 0 means the ConfigMap does not say what is running. The `ConfigEntryRejected` event on the ConfigMap names the entry.

### `wva_config_unknown_keys`
- **Type**: Gauge
- **Description**: keys in a scaling-policy ConfigMap's entries that the schema does not define, at its last load. They are ignored; the entries still apply. The series is removed when the ConfigMap is deleted.
- **Labels**: `namespace`, `configmap`, optional `controller_instance`
- **Use Case**: a value above 0 is usually a misspelled key whose setting is not in force. The `ConfigEntryUnknownKeys` event names it and its line.

### `wva_node_access_denied`
- **Type**: Gauge
- **Description**: `1` while a physical GPU limiter declared alone cannot read nodes, `0` when it can. Absent when no physical limiter is configured — nothing is asking for them. Beside a quota it stays `0`: the physical limiter is skipped, the quota still bounds, and `wva_constraint_provider_skipped{reason="node_read_denied"}` reports it.
- **Labels**: optional `controller_instance`
- **Use Case**: the combination that fails with no other symptom. A GPU-aware limiter allocates out of per-accelerator pools, so without nodes every variant is charged to no pool, receives no budget, and stops scaling up — and an unresolved accelerator is otherwise a normal state, so nothing else complains. Alert on `wva_node_access_denied == 1`; the shipped `WVANodeAccessDenied` rule does.

### `wva_policy_namespace_unverified`
- **Type**: Gauge
- **Description**: `1` when the controller took limiters and quotas from its own ConfigMap because reading the Namespaces that decide the policy source (`wva-policy`, or the `wva.llmd.ai/policy-namespace` label on the managed namespace) was Forbidden; `0` otherwise. Set once at startup.
- **Labels**: optional `controller_instance`
- **Use Case**: a `1` means a cluster policy may be published that this controller cannot see, so it may run under a weaker bound than an admin believes. Grant the read with `NAMESPACE=<ns> make setup-prereqs` or `make enable-physical-limiter`. The shipped `WVAPolicyNamespaceUnverified` rule (warning, after 10m) alerts on it, by `namespace`: a controller-level gauge carries no model namespace. See [GPU limiter](gpu-limiter.md#what-the-controller-does-with-that).

### `wva_constraint_provider_skipped`
- **Type**: Gauge
- **Description**: `1` while a declared GPU limiter is left out of scaling decisions. The series is removed when it bounds again, so no series means every declared limiter is in force.
- **Labels**: `provider` (the limiter's name: `gpu-limiter` for the physical one, the entry name for a quota), `reason` (`unobserved`: its GPU usage has not been observed; `failed`: it could not compute its constraints; `no_constraints`; `node_read_denied`: `gpu-inventory` declared beside a quota on a controller that may not list nodes), optional `controller_instance`
- **Use Case**: a skipped limiter lifts its bound. The others still apply, because a decision must fit every limiter that answered, but nothing checks what the skipped one would deny. A short `unobserved` after a restart is normal; anything sustained is not:
  ```promql
  max_over_time(wva_constraint_provider_skipped[10m]) == 1
  ```
  The shipped `WVAConstraintProviderSkipped` rule (warning, after 10m) alerts on it.

### `wva_scale_from_zero_wake_unchecked_total`
- **Type**: Counter
- **Description**: scale-from-zero wakes published with no GPU capacity check although limiters are declared: every limiter was unavailable and no constraints younger than 5 minutes were on hand.
- **Labels**: `namespace` (the model's; read as `exported_namespace`), optional `controller_instance`
- **Use Case**: any increase is a wake that may land on a full accelerator. Read with `wva_constraint_provider_skipped`, which says which limiters were out.

### `wva_scale_from_zero_queue_fallback_active`
- **Type**: Gauge
- **Description**: `1` while the scale-from-zero engine is reading the EPP flow-control queue from Prometheus because the **direct EPP scrape is failing**, `0` while the direct scrape works. The scaling manager reads the wake signal by scraping the EPP pod directly (pod IP, EPP metrics port, projected bearer token) — the one metric path that does not go through Prometheus — so it fails independently of everything else. The fallback keeps models waking; it does not make the direct path healthy.
- **Labels**: `pool` (namespaced InferencePool, e.g. `llm-d-sim/optimized-baseline`), optional `controller_instance`
- **Absence is meaningful**: the series is published on every healthy scrape, so a pool the scaling manager is watching always has one. No series at all means the scaling manager is not reading that pool — a different problem from the fallback being active.
- **Use Case**: Alert on a sustained `1`. Wakes still happen but are slower — bounded by the Prometheus scrape interval instead of the engine's 100 ms loop — and the underlying cause (EPP metrics token, EPP tokenreview RBAC, or a NetworkPolicy blocking pod-IP egress from the scaling manager namespace) will not fix itself. See [troubleshooting](troubleshooting.md#the-epp-scrape-is-failing-but-the-scaling-manager-still-wakes-models-slowly).
- **Example alert**:
  ```promql
  max_over_time(wva_scale_from_zero_queue_fallback_active[10m]) == 1
  ```

### `wva_available_gpus`
- **Type**: Gauge
- **Description**: Number of currently available GPUs grouped by accelerator type (e.g., "H100", "A100"). When `wva_gpu_discovery_up` is 1, this shows the number of currently available GPUs. When `wva_gpu_discovery_up` is 0, this metric shows the number of GPUs that were available at the last successful discovery. Only available in clusters such as OpenShift where the scaling manager can iterate over node objects. There are no exclusions such as tainted nodes or GPUs operating in different modes such as MIG.
- **Labels**:
  - `accelerator_vendor`: Name of the GPU vendor
  - `accelerator_model`: Full name of the accelerator
  - `accelerator_type`: Type of accelerator (short name of the accelerator)
- **Use Case**: Track the number of GPUs discovered by the scaling manager and available for allocation
- **Example**:
  ```json
  {
      "metric": {
        "__name__": "wva_available_gpus",
        "accelerator_model": "NVIDIA-H100-SXM5-80GB",
        "accelerator_type": "H100",
        "accelerator_vendor": "nvidia.com",
        "container": "manager",
        "endpoint": "https",
        "instance": "10.244.2.55:8443",
        "job": "wva-controller-manager-metrics-service",
        "namespace": "workload-variant-autoscaler-system",
        "pod": "wva-controller-manager-<hash>",
        "service": "wva-controller-manager-metrics-service"
      },
      "value": [
        1778847371.083,
        "4"
      ]
    }
  ```

### `wva_enforcer_modifications_total`
- **Type**: Counter
- **Description**: Total number of decision modifications made by the enforcer. The enforcer applies policy constraints (e.g., "scale_to_zero", "minimum_replicas") to scaling decisions.
- **Labels**:
  - `policy_type`: Type of enforcement policy applied
- **Use Case**: Monitor how often the enforcer modifies scaling decisions to enforce policies and understand policy impact
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_enforcer_modifications_total",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.62:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "policy_type": "scale_to_zero",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778859222.859,
      "1"
    ]
  }
  ```

### `wva_optimizer_active`
- **Type**: Gauge
- **Description**: Indicates which optimizer is currently active. Value is 1 for the active optimizer and 0 for inactive optimizers. Only one optimizer should be active at a time.
- **Labels**:
  - `optimizer_name`: Name of the optimizer
- **Use Case**: Track which optimization strategy is currently in use for scaling decisions
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_optimizer_active",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "optimizer_name": "cost-aware",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### Ceilings, wakes, start times and warm pool metrics

Every series below also carries `controller_instance` when `CONTROLLER_INSTANCE`
is set, and is scraped with the workload's namespace as `exported_namespace`.

| metric | type | labels | meaning | read it when |
| --- | --- | --- | --- | --- |
| `wva_variant_at_max_replicas` | gauge | `namespace`, `variant_name` | `1` while the variant's target sits on its own `maxReplicaCount`. Not a GPU shortage (that is `wva_decisions_limited_total`): the operator's ceiling binds, so the fix is a higher `maxReplicaCount`, not more GPUs. | Alone it is normal (a well-sized variant under load). With utilization above the scale-up threshold, demand is going unserved. |
| `wva_unmeasured_queue` | gauge | `namespace`, `model_name` | Requests queued in EPP flow control for a model with **no** attributed replica to act on. `0` is normal, including for an idle model at zero. | Non-zero: traffic queues while the scaling manager is blind to the pods serving it. Usual causes: FMA launcher pods not scraped, a PodMonitor selecting a port the pods do not declare, ownerReferences that reach no scale target. |
| `wva_model_replicas` | gauge | `namespace`, `model_name` | Replicas serving a model, summed across its variants. Exists to join against EPP metrics, which carry `model_name` and no namespace. | `0` with demand is a parked model that did not wake; see the `WVAModelParkedWhileRequestsRefused` alert. |
| `wva_scale_from_zero_wake_seconds` | histogram | `namespace`, `model_name` | Time from the first activation published to the model being observed serving. Buckets 1 s to 300 s. | Wakes are slow. With a warm pool or FMA the distribution is bimodal; a shift toward the slow mode is the regression. |
| `wva_saturation_metrics_up` | gauge | `variant_name`, `namespace` | `1` when the variant got a fresh decision this cycle; `0` when the analyzer knew the variant but its saturation and capacity gauges were not refreshed. | Gate alerts on the saturation gauges with this, rather than trusting a stale value. |
| `wva_replica_start_seconds` | histogram | `variant_name`, `namespace` | Pod creation to Ready, one observation per replica started. Buckets 5 s to 900 s. | Scale-ups land late. It is also an input: the demand floor projects the queue over the start time. |
| `wva_replica_start_seconds_estimate` | gauge | `variant_name`, `namespace`, `source` | The start time the controller is sizing with. `source` is `measured` or `seed`. | It disagrees with the histogram: the controller is sizing on a belief the cluster does not bear out. |
| `wva_warmpool_free_pods` | gauge | `pool` | Warm-pool Pods that can serve a wake now (every instance asleep, none loading). Compare with the pool's `sleepMinSize`. | Below `sleepMinSize` for long. See [operating the warm pool](../guides/warm-pool/operating.md). |
| `wva_warmpool_borrow_total` | counter | `namespace`, `model_name`, `outcome` | Attempts to cover a scale-up from the pool. `outcome`: `hit`, `blocked` (the model was resident but every holding Pod was busy: add Pods), `miss` (resident nowhere: fix the warm set). | The hit rate drops. |
| `wva_warmpool_bridge_seconds` | histogram | `namespace`, `model_name` | How long a borrowed Pod served before ordinary replicas took over. Should track the ordinary start time. | It sits near `maxHoldSeconds`: ordinary scale-ups are failing and the pool is masking it. |
| `wva_unattributed_gpus` | gauge | none | GPUs held by variants whose accelerator could not be resolved. Charged to no accelerator pool, so every pool looks freer than it is. | Any non-zero value: set a GPU product `nodeSelector` or `nodeAffinity` on the workload. |
| `wva_analyzer_demand` | gauge | `analyzer_name`, `namespace`, `model_name`, `role` | Demand D per analyzer, in that analyzer's unit. `role` is empty for a non-P/D model. | A desired count looks wrong: desired is `ceil(demand / target)`. |
| `wva_analyzer_target` | gauge | `analyzer_name`, `namespace`, `model_name`, `variant_name` | Per-replica target P, in the same unit as demand. | Same. |
| `wva_analyzer_observed_replicas` | gauge | `analyzer_name`, `namespace`, `model_name`, `variant_name` | Replicas that reported metrics in the analyzer's last cycle. Compare with the target's **ready** replicas. | Lower than ready: a scrape gap. Higher: something the scale target does not own is serving the model. |

### Utilization-share optimizer metrics

Published when the [utilization-share optimizer](scaling-policy.md#optimizer-cluster-default-only-live)
is selected. The exception is `wva_utilization_share_mode`, which the leader
publishes every cycle from its first, even with no active model.

- **Gauges** are replaced each cycle. A model or group that leaves takes its
  series with it. Switching the optimizer off, a cycle with no quota read, or a
  cycle with no active model deletes them all.
- **Counters and histograms** (`_transfers_total`, `_starts_total`,
  `_withheld_total`, `_claims_total`, `_donors_per_transfer`, `_release_seconds`)
  are never deleted and keep their last values after the optimizer is off. Use
  `rate()` or `increase()` to judge whether it is doing anything now.

The mode gauge:

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `wva_utilization_share_mode` | gauge | `mode`, optional `controller_instance` | `1` for the mode in force, `0` for the other three. `mode`: `off` (no `optimizer:` block, or an empty `type`), `invalid` (the block did not validate, or there is no `limiters:` list to give it a budget; today's optimizer runs and the limiters are unaffected), `shadow` (computes and reports, moves nothing), `active` (moves GPUs). Set by the leader at the start of every optimization cycle, so all four series exist from its first cycle and an alert can match the one it fears without `absent()`. A non-leader replica does not publish it. |

Two label sets for every other series, plus the optional `controller_instance` on both:

- **Role series** carry `namespace`, `model_name` and `role` (`decode`, `prefill`,
  or `both` for an aggregated model). `namespace` is the workload's namespace, so
  once Prometheus scrapes it, it appears as `exported_namespace`, as on every other
  per-model series (see [the note above](#notes-on-name_spaces-in-metrics)).
- **Group series** carry `accelerator_type` and `scope`: the namespace of a
  namespace quota group, or `cluster` for the cluster group. They carry no
  workload namespace; their `namespace` label is the controller's.

Published in shadow mode and when acting. While acting, `_target_gpus`,
`_actionable` and `_replicas_to_move` (and the blocked reasons) come from the
planner, which caps headroom at `maxHeadroomReplicas` only while the quota is
contended (`wva_utilization_share_headroom_capped`). Shadow mode has no
contention test and always caps, so on a quota whose peaks fit, its targets are
lower than what acting would move to:

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `wva_utilization_share_headroom` | gauge | role | The traffic spike the role absorbs before it must scale, as a fraction of its need. Negative when the role is short. Absent for a role with no demand. |
| `wva_utilization_share_target_gpus` | gauge | role | The GPU target the tolerance band is judged against (continuous, not whole replicas). |
| `wva_utilization_share_actionable` | gauge | role | `1` when the role is out of band and off its whole-replica target, so a move could fix it; else `0`. |
| `wva_utilization_share_actual` | gauge | role | The role's utilization at the GPUs it holds, on the scale of its `scaleUpThreshold`. Absent with no demand or no GPUs. |
| `wva_utilization_share_floor_excess_gpus` | gauge | role | GPUs the role's `minReplicaCount` holds above its need. Only the floor the owner configured counts: the last replica the optimizer never takes from a running role is not a floor here. |
| `wva_utilization_share_spare_gpus` | gauge | group | The group's budget minus every role's claim (need, raised to its floor). Negative when the quota is short. |
| `wva_utilization_share_replicas_to_move` | gauge | group | Replicas the whole-replica target would move. In shadow mode, what would be planned. |

Published only while the optimizer acts (`shadow: false`):

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `wva_utilization_share_promised_gpus` | gauge | group | GPUs released for a receiver and not yet held by it. |
| `wva_utilization_share_reserve_debt_gpus` | gauge | group | `reserveGPUs` spent and not yet refilled. |
| `wva_utilization_share_headroom_capped` | gauge | group | `1` while the quota is contended -- its roles' peak needs over the swing window, in whole replicas, do not fit the budget together (a peak counts once two readings in a row reach it; while no role's peak is known -- a new group, or after a restart that restored no stored need history, until a swing window of history exists -- each counts at its `maxReplicas`, or the whole budget; a role joining a group whose peaks are known counts at what it holds) -- and each role's headroom is capped at `maxHeadroomReplicas` times its weight; `0` while the spare is spread by weight. Not published in a cycle the planner did not plan, such as the restart quiet period. The log says when it flips, with its `cause` (`peaks-unknown` or `peaks-exceed-budget`). |
| `wva_utilization_share_swinging` | gauge | role | `1` while the role is planned on its mean need because it reversed direction twice within the swing window. |
| `wva_utilization_share_in_flight` | gauge | group + `state` | Transfers in flight, by `state`: `releasing` (the donor has not released its GPUs yet) or `filling` (released, or an idle fill, and the receiver has not taken the GPUs yet). Both series exist for every active group, `0` when nothing is in flight. |
| `wva_utilization_share_effective_seconds` | gauge | group + `param`, `source` | A derived timing in force, for the group as a whole (from its slowest donor). `param`: `window`, `release-timeout`, `fill-timeout`, `reversal-hold`, `swing-window`. `source`: `measured` (the group's own releases), `scaledobject`, `pod` (termination grace), `default` (the cluster's default for an unset value), `policy` (`reversal-hold` and `swing-window` set by `utilizationShare.stabilization`). |
| `wva_utilization_share_transfers_total` | counter | group + `outcome`, `urgent` | Transfers that ended, by `outcome` (below), idle fills included. `urgent="true"` when the receiver was below its need. Every `outcome` × `urgent` series is published at `0` from a group's first acting cycle, so `increase()` sees the first transfer of each outcome. |
| `wva_utilization_share_starts_total` | counter | group + `kind`, `skipped_waits` | What the optimizer started, by `kind`: `transfer` (a donor's replica moved to a receiver; a donor set counts as one), `fill` (a model raised into the quota's unassigned GPUs), `refill` (a donor lowered to pay back `reserveGPUs`) or `return` (a model's headroom above `maxHeadroomReplicas` given back to the quota). `skipped_waits="true"` on a move that skipped the stabilization waits (`stabilization.skipWaitsWhen`). Every `kind` × `skipped_waits` series is published at `0` from a group's first acting cycle. |
| `wva_utilization_share_withheld_total` | counter | group + `reason` | Transfers not planned: `reversal-hold` (every move a hold withheld: a role that just gave cannot receive, or one that just received cannot give, until the reversal hold passes; a donor → receiver pair whose transfer was cancelled; a donor backing off after an aborted release; a donor that has given all it can) or `not-actionable` (a role out of band with no whole replica to move). Three reasons concern a model holding more than its need plus its headroom: `return-not-calm` (it keeps it because its load would be above `stabilization.returnSpareWhenLoadAtMost` on what it keeps), `return-pod-busy` (its load would let it go, but more than `returnSpareWhenLoadAtMost` x `queueLengthThreshold` requests -- 3 by default -- have been waiting on one of its pods for `consecutiveCyclesBeforeMove` cycles in a row; it keeps it for at most its scale-down window. A P/D decode pod counts only while its KV cache is full; prefill pods are not counted) and `return-role-short` (it keeps it because another model is short of its need). `donor-peak`: while the quota holds every model's peak, a headroom move to a model already at its need that would leave the donor below its own peak over the swing window. `awaiting-return`: a short model left to the idle fill, instead of a transfer or a donor set, because returns still releasing will free a replica of it. `scale-down-band`: a model holding more than its need plus its headroom keeps the replica, because giving it back would take it below its scale-down boundary (its demand would sit above `scaleDownBoundary` on what it keeps), where today's optimizer would not scale it down either. Each is counted once per cycle it applies. |
| `wva_utilization_share_claims_total` | counter | group + `outcome` | Scale-from-zero wakes claiming a releasing transfer's GPUs: `redirected` (claimed), `refused-score` (the waking model does not outrank the receiver), `refused-fit` (the released pods cannot host its replica), `refused-held` (the model claimed one within the last two minutes), `none-releasing` (nothing to claim). A refusal is counted once per change, not per attempt. |
| `wva_utilization_share_donors_per_transfer` | histogram | group | Donor replicas that fund one receiver replica. |
| `wva_utilization_share_release_seconds` | histogram | group | Time from a transfer's start to its donor's GPUs being released. |

Transfer `outcome` values:

| outcome | meaning |
| --- | --- |
| `done` | The receiver holds the released GPUs. Also counts an idle fill (a receiver raised into GPUs the quota had free, with no donor) that landed. |
| `fill-timeout` | The donor released, but the receiver's new pods did not get the GPUs within the fill timeout. If they are still Pending, the receiver's raise is undone and it is not funded again for one release timeout; otherwise it keeps its higher target. |
| `aborted` | The donor did not release within the release timeout. Its replica count is restored, and it backs off before it is asked to give again. |
| `cancelled` | Demand reversed while the release was still in progress, and the transfer was called off. |
| `redirected` | A scale-from-zero wake claimed the GPUs being released; the original receiver is planned again. |
| `wrong-pod` | The donor shrank by a pod other than the one marked, so the GPUs did not come free where the receiver was planned; the receiver is not raised, and the donor stays one replica lower. Detected only for a transfer planned with node information, which names the pods that must go. Without it (a namespace-scoped install, or a transfer planned by count alone) a release is judged by count: another of the donor's pods leaving counts as the release. |

A sustained rise in `aborted`, `fill-timeout` or `wrong-pod` means transfers are
not landing. Alert on `increase()` of those outcomes (see
[monitoring](monitoring.md#the-metrics-that-answer-specific-questions)), and read
the blocked reasons below for which models are affected.

**Limits.** At most two transfers with a donor run per group at once
(`wva_utilization_share_in_flight` counts them, idle fills included), and a role
gives or receives at most two replicas per cycle. Two kinds of move are outside
both limits, so `in_flight{state="releasing"}` can read above `2`:

- **A move that skipped the waits, from its second cycle.** It starts without
  consulting the limit but counts in it, so other moves wait until it lands. When
  a receiver is at its scale-up threshold and a donor stays calm
  (`stabilization.skipWaitsWhen`; the donor judged on the higher of its current
  and peak need, two readings in a row, over its scale-down window, unless the
  receiver's bursts outlast a move), the first cycle moves at the usual pace and
  is limited as any other move. If the imbalance holds a second consecutive
  cycle, the receiver takes every replica it is short of its need in that cycle,
  from one donor or several (`starts_total{skipped_waits="true"}`). Its need
  here is the higher of its current need and, for a model planned on its average
  because its demand swings, that average. The replica of headroom above its
  need is an ordinary, paced move.
- **A spare return** (below). It never counts against the limit.

**No raise while giving.** A model still giving GPUs (a transfer, return or
refill still releasing) is not raised, by a transfer or a fill, until that
release lands. This also stops a move that skipped the waits from sending GPUs
back to a model mid-drain.

**Idle fills** move nothing from another model and are outside the concurrency
limit. A fill raises a model below its need by its whole shortfall at once, and a
model at or above its need by at most two replicas per cycle. The planner counts
what fills will give a receiver (only replicas whose pods fit free GPUs, where
node information exists) before it asks any donor to shrink.

**A spare return** is a model giving what it holds above its need plus its
headroom (`utilizationShare.maxHeadroomReplicas` times its weight) back to the
quota group's unassigned GPUs: a transfer with a donor and no receiver
(`starts_total{kind="return"}`).

- **When.** Only while the quota is contended: it cannot hold every model at its
  peak need over the last `stabilization.flappingWindow` at once
  (`wva_utilization_share_headroom_capped`). While no model's peak is known (a
  new group, or after a restart that finds no stored need history, until a
  window of history exists), each counts at its `maxReplicas`. A model is never
  capped below what the other models' peaks leave of the quota, so a model that
  bursts alone keeps its spare.
- **Start.** Once the model has held the extra for
  `stabilization.consecutiveCyclesBeforeMove` cycles in a row, while no model
  that can grow is short of its need, and while its load would stay at or below
  `stabilization.returnSpareWhenLoadAtMost`. Then all the extra goes in one
  cycle. While more than `returnSpareWhenLoadAtMost` x `queueLengthThreshold`
  requests wait on one of its pods for those cycles in a row, the return waits
  for at most the model's scale-down window (`return-pod-busy`).
- **Release.** It waits the model's scale-down window, as a scale-down does. Its
  GPUs stay assigned until its pod is gone: no model is raised into them before
  that, and the giving model is not raised by a fill meanwhile. It shows in
  `wva_utilization_share_in_flight{state="releasing"}` until the pod is gone and
  never reads as `filling` (there is no receiver). It ends in the cycle that sees
  the release, counted as
  `wva_utilization_share_transfers_total{outcome="done", urgent="false"}`, with
  its release time in `wva_utilization_share_release_seconds`.
- **Called off.** If the model's demand comes back inside the donor's scale-down
  window (short of its target again, or busier than `returnSpareWhenLoadAtMost`
  on what it keeps), the return is called off and counted `outcome="cancelled"`.
- **Restarts.** A return survives one: the mark on its pod records that it is a
  return, and a mark that claims a return with a receiver is rejected.
- **Never handed over.** If another model turns short while a return is
  releasing, the GPUs reach it through the idle fill once the replica is
  released.
- **Logs.** `Utilization share: spare returned to the quota` (with `donor` and
  `gpus`) when released; the `transfer started` line carries `returned` set to
  true.

Transfers are also recorded as Kubernetes Events on the scale targets they move;
see [Events](monitoring.md#events-on-the-models-scale-targets).

### `wva_model_scaling_blocked`

- **Type**: Gauge, `1` for each reason that holds. A series exists only while its reason holds, so no series means nothing is blocking.
- **Labels**: `namespace` (scraped as `exported_namespace`), `model_name`, `reason`, optional `controller_instance`. No `variant_name`, no `role`.
- Several reasons can hold at once.

Every reason the controller sets:

| reason | set by | means | do |
| --- | --- | --- | --- |
| `variant-floor` | scale-to-zero policy | scale-to-zero is on, but a variant has `minReplicaCount > 0`, so the model never parks | set every variant's `minReplicaCount` to 0, or turn scale-to-zero off for the model. Alert: `WVAModelWillNotScaleToZero` |
| `policy-forbids-zero` | scale-to-zero policy | every variant allows 0, but the model's resolved policy disables scale-to-zero | enable `scaleToZero` on the `default` entry (or its `defaultPolicy` tier) or the model's per-model entry, or raise `minReplicaCount`. Alert: `WVAModelWillNotScaleToZero` |
| `engine-unsupported` | scale-to-zero policy | the model runs both vLLM and SGLang, so no single request counter measures idleness; it is not parked | run one engine per model. Not alerted on by design |
| `activation-retention` | scale-to-zero policy | just woken from zero and held for its retention period | nothing; clears by itself |
| `no-wake-signal` | scale-from-zero | EPP exports no flow-control queue metric at all, so a parked model cannot wake | enable the EPP `flowControl` feature gate and restart the EPP. Alert: `WVAModelHasNoWakeSignal` |
| `quiet-period`, `marks-unreadable`, `whole-replica-short`, `awaiting-release`, `quota-short`, `floor-pinned`, `floors-exceed-quota`, `donors-at-floor`, `no-compatible-donor`, `release-timeout`, `release-taken`, `release-shape-mismatch`, `reversal-hold`, `swinging`, `transfer-limit`, `burst-ending`, `fast-swing-capped`, `donor-not-steerable` | utilization-share optimizer, only while it acts | see the table below | see the table below |
| `borrow-released`, `lender-reclaimed`, `borrow-unplaced`, `nominal-lent-out` | Kueue borrowing | see [reasons set by Kueue borrowing](#wva_model_scaling_blocked-reasons-set-by-kueue-borrowing) | same |

For the scale-to-zero reasons, see also
[when a model will not park](../guides/scale-to-zero/README.md#when-a-model-will-not-park).

### `wva_model_scaling_blocked` reasons set by the utilization-share optimizer

- **Type**: Gauge, `1` for each reason that holds; a series exists only while its reason holds.
- **Labels**: `namespace` (scraped as `exported_namespace`), `model_name`, `reason`, optional `controller_instance`.
- Set only while the optimizer acts. In shadow mode nothing is blocked by it, so none of these appear.
- There is no `role` label: a reason on a P/D model may come from either role. Cross-check
  `wva_utilization_share_headroom` and `wva_utilization_share_actionable`, which carry `role`,
  to see which one is short.
- `quiet-period` is set on every planned model of a group for one fill timeout after a controller
  restart, a leader change, or the optimizer starting to act; nothing in the group scales
  meanwhile. `marks-unreadable` freezes the group the same way, but until someone fixes the cause.
- Most reasons hold until something changes. `release-taken` and `release-shape-mismatch` expire
  one release timeout after the fill that set them timed out, and `donor-not-steerable` and
  `release-timeout` last only their back-off, so they come and go; alert on how often they appear
  rather than with a long `for:` (see [monitoring](monitoring.md#the-metrics-that-answer-specific-questions)).

| reason | what it means | what you can do |
| --- | --- | --- |
| `quiet-period` | The group is in the quiet period that follows a controller restart, a leader change, or the optimizer starting to act: for one fill timeout (`wva_utilization_share_effective_seconds{param="fill-timeout"}`, about three minutes with default timings) every planned model holds what it runs, or its restored target, and none scales up or down. A fill in flight before the restart has no mark, and its receiver's pods must not lose their GPUs meanwhile. | Nothing: it clears by itself. Expect it on every upgrade and leader change. |
| `marks-unreadable` | The controller cannot read the transfer marks on the group's donor pods, so it cannot start the group's ledger: every planned model of the group holds what it runs and none scales, up or down, until it can. One namespace it cannot list is enough to freeze the whole group. | Alert on it. The Error log line `could not read transfer marks; retrying next cycle` names the cause, typically a missing RBAC grant to list pods in a model's namespace. |
| `whole-replica-short` | The role holds less than its need although the group's needs add up to less than the quota: whole replicas cannot give every role its need (each need rounded up to its replica size, floors included, does not fit), so moves go to whichever role is worse off, by weight, and this one serves below its need. Common with 8-GPU replicas. | Raise the quota by a replica, lower a floor, or accept it: the role that gave is the one the optimizer judged better off. |
| `awaiting-release` | The model receives a transfer whose donor has not released its GPUs yet, or waits for returns still releasing that will free whole replicas of it (`withheld_total{reason="awaiting-return"}`). | Normally clears within one release. If it persists, check the donor's ScaledObject scale-down window and its pods' termination grace; transfers that time out show as `aborted`. |
| `quota-short` | The role holds less than its need and the whole group is short: no rebalance can cover it. | Raise the quota for that accelerator type, or lower demand. Weights decide who is cut. |
| `floor-pinned` | The model's `minReplicaCount` holds at least one replica more than it needs; GPUs the share would otherwise give to others. Only the configured `minReplicaCount` counts, not the last replica the optimizer keeps on any running role. | Lower `minReplicaCount` if the floor is not deliberate. |
| `floors-exceed-quota` | The configured floors (`minReplicaCount`) of the group's models add up to more than its budget. The last replica the optimizer keeps on a running role is not counted. | Lower floors or raise the quota; until then every model of the group shows this. |
| `donors-at-floor` | The role is short, and no other role holds more than its floor to give. | Lower another model's `minReplicaCount`, or raise the quota. |
| `no-compatible-donor` | The role is short and out of band, but no donor's pods can host one of its replicas: each of its pods needs a donor pod at least as large (for example, it runs 8-GPU pods and every donor runs 2-GPU pods). | Nothing to tune in the optimizer: give the model GPUs another way (a larger quota, or scale a compatible model down). See [troubleshooting](troubleshooting.md#a-model-shows-no-compatible-donor). |
| `release-timeout` | The role's last release as a donor was aborted (it did not scale down within the release timeout), and it is backing off before it is asked to give again. Set only for an aborted release; a donor that could not be marked shows `donor-not-steerable` instead. | Check why it did not scale down in time: a long scale-down window, pods slow to terminate, or a ScaledObject not acting. |
| `donor-not-steerable` | The role was asked to give, but the pod that would go cannot be steered by a fault: a sibling's deletion cost left no room for the mark (a cost a user set at the int32 minimum, or no gap between an earlier transfer's mark and the lowest unmarked sibling), or the pod patch failed. Present only while it backs off (the back-off doubles with each failure in a row, up to 16 release timeouts), then it is asked again. Not set for a donor whose workload is changing -- a rollout (by its status, or pods of two ReplicaSets or LWS revisions), a pod not yet created, scheduled or Ready, an LWS group being replaced -- nor for one whose every pod is already given to transfers in flight: those are held for a release timeout with no back-off, and routine. | Check the deletion costs set on the donor's pods, and RBAC to patch pods. The `UtilizationShareDonorNotSteerable` Event on the donor and the controller log line `could not mark a donor pod` carry the cause. |
| `reversal-hold` | The role is short and a move could fix it, but it gave GPUs within the reversal hold, so it may not receive yet. It is not reported when the role is at its scale-up threshold and some other role would stay calm after giving it a replica (its need at most 0.6 of what it keeps): that move does not wait for the hold, so naming the hold would mislead. A transfer that was cancelled before anything moved holds only that donor → receiver pair from starting again, and sets this reason on neither role. | Usually clears by itself; the hold lasts `wva_utilization_share_effective_seconds{param="reversal-hold"}` from the start of the transfer it gave in, about twice a release. Present for the whole hold means no other role is calm enough to give -- often a model whose burst of long outputs just ended, its draining backlog still read as need. Recurring means load on this model swings faster than transfers land. A model that must not start a burst short needs a floor: see [scaling policy](scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model). |
| `swinging` | The role reversed direction twice within the swing window and is planned on its mean need over that window, not its current one. | Nothing to tune: its load moves faster than the optimizer can follow. Give it headroom by weight or floor if it must not lag. |
| `transfer-limit` | The role is short and a move could fix it, but its group already has the most transfers with a donor in flight (two). Not reported for a role whose moves are currently unpaced: a hard imbalance held for a second cycle, from a calm donor, and its model's burst not expected to end within 1.25 times a move's latency -- including a model with no completed burst on record yet, whose end is not predicted. | Clears as transfers land. Persisting means transfers are slow to release; check `wva_utilization_share_in_flight{state="releasing"}` and the donors' scale-down windows. |
| `burst-ending` | The role is short with its hard imbalance confirmed (two cycles), but its model's burst is expected to end before a move would land and serve: what is left of its last completed burst, after as long as this one has lasted, is shorter than 1.25 times a move's latency. Its prefill and decode are judged together. It receives at the pace -- a replica a cycle, within the two-transfer limit -- not its whole shortfall at once, so its donors are not drained for GPUs that would arrive after the burst. Not set for a need that does not swing across a quarter of its peak, nor once the burst outlasts the last one. | Nothing, normally: it clears when the burst ends or outlasts its history. Persisting through long shortfalls means the model's bursts became longer than its record; it clears by itself once one outlasts it. A model that must serve short bursts in full needs a floor. |
| `fast-swing-capped` | The role's need swings faster than a move can follow (at least two completed bursts, all shorter than a move's latency, over a quarter of its peak) while another model of the group bursts for longer than a move and past its floor. Its headroom under the contended cap is set on its mean need over the swing window, below its current reading, so the idle GPUs it would take at each crest stay free for the long burst. | Nothing to tune: the model serves its crests from what it holds. If it must not lag at a crest, give it a floor or a higher weight. |
| `release-taken` | The role's last transfer timed out filling, and the GPUs its donors released were taken by a pod the scaling manager did not place. Needs node information. Reported for one release timeout after the fill timed out, then cleared; it returns if the next fill fails the same way. | Look for other workloads scheduling onto the same accelerator type. |
| `release-shape-mismatch` | The role's last transfer timed out filling: enough GPUs were free in total, but no node had enough for its largest pod. Needs node information. Reported for one release timeout after the fill timed out, then cleared. | GPUs are fragmented across nodes; the model needs a node with enough GPUs free at once. |

### `wva_model_scaling_blocked` reasons set by Kueue borrowing

Set every cycle by the steady-state engine, only for a namespace-scope Kueue
quota entry that counts borrowing. Each answers "why did this model drop?"; a
model that dropped with none of them present dropped because its demand fell.
See [replicas dropped](troubleshooting.md#replicas-dropped-under-kueue-borrowing).

| reason | what it means | what you can do |
| --- | --- | --- |
| `borrow-released` | The scaling manager is giving borrowed replicas back to a lender ClusterQueue that waits for capacity it lent and cannot reclaim it itself (`reclaimWithinCohort: Never`, or `LowerPriority` below the borrower). Only with `releaseToNonReclaimingLenders: true` on the quota entry; off by default. Present until the release lands. A `BorrowedCapacityReleased` Event on the scale target names the lender. | Nothing, normally. Often: see `WVAKueueBorrowThrashing`. |
| `lender-reclaimed` | Kueue preempted a pod of the model for a lender reclaiming its nominal quota. Present for `borrowCooldown` after the preemption, while the model borrows nothing new. | Raise the model's guaranteed quota if it recurs. |
| `borrow-unplaced` | A variant has replicas that did not place (Unschedulable, or held at Kueue's admission gate). It grows on no borrowed GPUs meanwhile, and borrowed replicas still unplaced after `placementTimeout` are given back. | `kubectl describe pod` for the scheduling message; see `wva_kueue_unplaced_pods`. |
| `nominal-lent-out` | The limiter capped the model this cycle while part of its ClusterQueue's nominal quota is held by a peer it cannot reclaim from, so its guaranteed figure sits below nominal. | Set `reclaimWithinCohort` on the ClusterQueue if the model must reach its nominal quota. |

### Kueue borrowing metrics

Published only for a namespace-scope Kueue quota entry; the borrowing series
only where it counts borrowing. Every other borrowing condition is a reason on
`wva_model_scaling_blocked` (above). `namespace` is scraped as
`exported_namespace`.

| metric | type | labels | value |
| --- | --- | --- | --- |
| `wva_quota_limit_gpus` | gauge | `limiter_name`, `namespace`, `accelerator`, `tier` | A namespace-scope quota entry's cap per namespace and accelerator. `tier="guaranteed"`: the cap without borrowing. `tier="borrowable"`: what the namespace may draw past it now, were no other namespace to draw first -- the smaller of its borrow cap (the queues' `borrowingLimit`, bounded by the entry and the namespace ResourceQuota) and the free GPUs of its cohorts' shared pools for that accelerator, after what is in flight. `0` where borrowing is not counted. No series for an unlimited guaranteed figure. |
| `wva_quota_borrow_withheld` | gauge | `limiter_name`, `namespace`, `accelerator`, `reason` | `1` for the reason borrowable is 0; deleted when the reason changes or clears. Reasons: `stale`, `kueue-controller-down`, `kueue-version-untested`, `no-status`, `hierarchical-cohort`, `hierarchy-unknown`, `effective-quotas`, `fair-sharing`, `topology-aware`, `admission-checks`, `no-cohort`, `inactive`, `malformed`, `not-kueue-managed`, `no-gpu-bound`, `resourcequota-unreadable`, `not-selected` (borrowing not counted), and `no-borrow-cap`, `lender-waiting`, `cohort-pool-empty`, `preemptions-unreadable` (counted, nothing to draw now; the last: Kueue's preemptions in the namespace could not be read this cycle, so none of its models grows on borrowed capacity -- check the controller's `get`/`list` on Workloads). |
| `wva_kueue_unplaced_pods` | gauge | `namespace`, `variant_name` | Pods of a variant unplaced for longer than `placementTimeout`. Absent otherwise. |
| `wva_kueue_preempted_pods_total` | counter | `namespace`, `model_name` | Preempted Workloads (a Workload evicted with reason `Preempted`): one per Workload, so a LeaderWorkerSet or pod group counts once, not once per pod. The name predates this. `WVAKueueBorrowThrashing` reads it. |
| `wva_borrow_releases_total` | counter | `namespace`, `model_name`, `cause` | Variant targets the scaling manager lowered to give borrowed capacity back: one per release record, so a P/D unit counts once per variant it lowers. `cause` is `lender-waiting` or `placement-timeout`. Kueue's preemptions are not counted here (see `wva_kueue_preempted_pods_total`). `WVAKueueBorrowThrashing` reads it. |
| `wva_kueue_borrow_drift_gpus` | gauge | `cluster_queue` | Per ClusterQueue in a cohort, the GPUs by which the borrowed figure the scaling manager computes (usage past nominal) differs from Kueue's `status.flavorsReservation` borrowed figure. 0 while they agree; above 0, Kueue accounts differently from what borrowing assumes. |
| `wva_kueue_lender_waiting_seconds` | gauge | `cohort`, `cluster_queue` | How long a lender ClusterQueue has waited for capacity it lent, published only while a namespace this controller governs still borrows in its cohort. While it is present the cohort borrows nothing new (reason `lender-waiting`); unless `releaseToNonReclaimingLenders` is on, nothing is handed back to a lender Kueue does not reclaim for, so this is how its wait shows. |
| `wva_kueue_never_read` | gauge | `limiter_name` | `1` for a quota entry with `kueue.enabled` that leaves an accelerator type at `-1` in its static caps and has never read Kueue successfully since the controller started: nothing bounds that type. Absent otherwise. `WVAKueueNeverRead` reads it. |

The same budget, per entry, namespace and accelerator, with `used` and an
`asOf` time, is in the `limiterBudgets` key of the `wva-controller-status`
ConfigMap (at most 200 entries; `truncated` counts the rest). `asOf` moves only
when a figure changes.

### Replica management metrics

### `wva_current_replicas`
- **Type**: Gauge
- **Description**: Current number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Monitor current number of replicas per variant
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_current_replicas",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_desired_replicas`
- **Type**: Gauge
- **Description**: Desired number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Expose the desired optimized number of replicas per variant
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_desired_replicas",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_desired_ratio`
- **Type**: Gauge
- **Description**: Ratio of the desired number of replicas and the current number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Compare the desired and current number of replicas per variant, for scaling purposes
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_desired_ratio",
      "accelerator_type": "H100",
      "container": "manager",
      "endpoint": "https",
      "exported_namespace": "llm-d-sim-dual",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_replica_scaling_total`
- **Type**: Counter
- **Description**: Counts the controller's **decisions**, not replica moves. It is incremented once per cycle for each variant whose decision is not "no change" (target differs from current). The HPA and KEDA act on their own schedule, so this is not a count of what the HPA did: a target the HPA is slow to reach counts once per cycle until it gets there.
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `direction`: `scale-up` or `scale-down`
  - `reason`: decision category: `V2` (the normal pipeline), `saturation-only mode`, `scale-from-zero`, `rescale`, `utilization-share`
- **Use Case**: Detect scaling thrashing — backs the `WVAReplicaScalingThrashing` alert — and audit decisions per variant. To see what actually moved, read the HPA (`kubectl describe hpa`) or `wva_current_replicas`.
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_replica_scaling_total",
      "direction": "scale-up",
      "namespace": "workload-variant-autoscaler-system",
      "reason": "V2",
      "variant_name": "smoke-test-va"
    },
    "value": [
      1778846184.925,
      "3"
    ]
  }
  ```

### Error tracking

### `wva_errors_total`
- **Type**: Counter
- **Description**: Total number of errors by component. The components are "collector", "analyzer", "optimizer", "limiter", "enforcer", and "controller". Some of the components currently may not have any `wva_errors_total` metrics. They may be available in future the scaling manager versions.
- **Labels**:
  - `component`: Component where the error occurred
  - `error_type`: Type or category of the error
- **Use Case**: Track error rates across different components to identify problematic areas and monitor system health
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_errors_total",
      "component": "controller",
      "container": "manager",
      "endpoint": "https",
      "error_type": "Failed to parse saturation scaling config entry",
      "instance": "10.244.2.55:8443",
      "job": "wva-controller-manager-metrics-service",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "wva-controller-manager-<hash>",
      "service": "wva-controller-manager-metrics-service"
    },
    "value": [
      1778850096.639,
      "1"
    ]
  }
  ```

### Service rate

What the saturation analyzer priced a variant's service rate on this cycle:
the ITL(k) line, the point it is evaluated at, and the per-replica completion
rate that comes out. They explain `wva_analyzer_target` and the throughput
floor rather than drive anything: these are observations of arithmetic the
analyzer already does, and none of them is a scaling input for KEDA or an HPA
to threshold. All are set from one record per variant per cycle, beside
`wva_analyzer_target`, and a series is deleted when its variant or model stops
being analyzed, and on a cycle whose collection returned no replica rows (a
model scaled to zero, or one whose pods are not scraped), so a parked fleet
does not keep its last rate. A cycle whose metrics query or analysis fails
deletes nothing: these series keep their last values, as `wva_analyzer_target`
does, so during a Prometheus outage they show the last cycle that priced the
model, not the current one.

Labels are the ones `wva_analyzer_target` and `wva_engine_config` carry, so the
three join directly. `namespace` is the **model's** namespace and arrives in
Prometheus as `exported_namespace` (see the note on namespaces above), plus
`controller_instance` when `CONTROLLER_INSTANCE` is set. Every value here is a
variant-level reading; the `derived-mu` log line carries the same quantities
per pod.

### `wva_itl_line_slope`
- **Type**: Gauge
- **Description**: Slope A of the line ITL(k) = A*k + B a decode variant would derive its service rate from, in seconds per unit of KV utilization. Whether the derived rate is the one in use is `wva_service_rate_per_replica`'s `source`: an own measured window with enough readings wins over it. The line is the variant's own fit, or one borrowed from a sibling with the same engine configuration (only when `enableLearnedStateReuse` is on). Absent while the variant has no line, and while none of its replicas reports (a variant parked at zero keeps its window for up to 30 minutes, and publishes none of it).
- **Labels**:
  - `namespace`: the model's namespace (scraped as `exported_namespace`)
  - `model_name`: model ID
  - `variant_name`: variant name
- **Use Case**: Watch the contention term a derived service rate rests on. The series does not say whose line it is: a slope that jumps can be the variant's own line refitted on different load, or the variant switching between its own line and a borrowed one. `wva_service_rate_per_replica{source="borrowed-line"}` tells the two apart while the borrowed line is the one in use.

### `wva_itl_line_intercept`
- **Type**: Gauge
- **Description**: Intercept B of the same line, in seconds: the inter-token latency the line predicts at an empty cache. It is not always a fitted hardware floor: when the readings span too little of k for a free fit, the intercept is pinned to a learned or default value and only the slope is fitted, and a free fit on noisy readings can put it below zero. Present and absent with `wva_itl_line_slope`.
- **Labels**: as `wva_itl_line_slope`
- **Use Case**: Compare a variant's floor across accelerators; an intercept far from the card's usual figure is a fit on too narrow a range of k.

### `wva_itl_seconds_at_pricing_k`
- **Type**: Gauge
- **Description**: The line evaluated at `wva_pricing_k`, in seconds: the inter-token latency a derived service rate would be priced at. Present and absent with `wva_itl_line_slope`. A line that is zero or negative at the pricing k cannot derive a rate, and none of the three line series is published for it; `wva_itl_line_samples` still is.
- **Labels**: as `wva_itl_line_slope`
- **Use Case**: Compare against the ITL the engines report under load. A priced ITL well below the observed one means the derived rate over-states what a replica completes.

### `wva_itl_line_samples`
- **Type**: Gauge
- **Description**: How many (k, ITL) readings a decode variant's **own** ITL window holds, published while it has no line too, zero included, and absent while none of its replicas reports. A line needs enough readings spread far enough apart in k; a variant priced on a borrowed line still reports its own count here.
- **Labels**: as `wva_itl_line_slope`
- **Use Case**: Explain an absent line. A fleet with no line produces no derived service rate, and the reason is almost always this count or the spread of k rather than anything wrong. A fresh fleet takes about 28 cycles from load start to fit.
- **Example**: serving decode variants with no usable line this cycle, their own or a borrowed one:
  ```promql
  wva_itl_line_samples unless on (exported_namespace, model_name, variant_name) wva_itl_line_slope
  ```

### `wva_service_rate_per_replica`
- **Type**: Gauge
- **Description**: The saturated completion rate one replica of a variant is priced at, in requests per second: the median over the variant's own replicas that have one, never a warm pool bridge. One series per variant; `source` names the path that answered for the median replica. With an even replica count the rate averages the central pair, and `source` is the lower reading's, so a variant with one `measured` and one `derived` reading publishes a rate between the two, labelled with the source of the lower. Absent while no replica has a rate.
- **Labels**:
  - `namespace`, `model_name`, `variant_name`: as `wva_itl_line_slope`
  - `role`: `prefill`, `decode` or `both`
  - `source`: `derived` (from the variant's own ITL line at the pricing k), `borrowed-line` (the same, from a line a sibling with the same engine configuration fitted), `measured` (the variant's own saturated window for the output length being served) or `neighbour` (a neighbouring output-length bucket's window, used until the served bucket has its own). The throughput floor may hold a fleet on a `borrowed-line` or `neighbour` rate but not grow one -- nor on a `measured` window with too few readings, which the floor's `heldWhy=single-sample` reports
- **Use Case**: What one replica of the variant is priced to complete at saturation. It is the variant's own figure, not the floor's divisor: the floor works per role, takes the median over the role's replicas that read their own bucket (dropping neighbour and borrowed-line readings whenever one does), and adds the backlog. On a role with one variant and readings of one kind the two agree. A `source` that stays `neighbour` after a shape change means the fleet has not yet measured itself under the new shape.
- **Example**: variants priced on a neighbouring bucket's reading, and for how long:
  ```promql
  wva_service_rate_per_replica{source="neighbour"}
  ```

### `wva_pricing_k`
- **Type**: Gauge
- **Description**: The KV utilization an ITL line is evaluated at to derive a decode replica's service rate, published for every model the saturation analyzer runs on, including those with no decode role: `kvCacheThreshold` x `scaleUpThreshold`, clamped to the range a line is fitted over. Composed, so 0.68 with the shipped defaults, not the 0.85 that `k_sat` suggests.
- **Labels**:
  - `namespace`: the model's namespace (scraped as `exported_namespace`)
  - `model_name`: model ID
- **Use Case**: Settle which utilization the autoscaler targets when pricing a replica, without recomputing it from configuration.

## Example queries

### Basic queries
```promql
# Current replicas by variant
wva_current_replicas

# Scaling decisions per second (decisions, not HPA moves)
rate(wva_replica_scaling_total[5m])

# Desired replicas by variant
wva_desired_replicas
```

### Advanced queries
```promql
# Scale-up decisions per second
rate(wva_replica_scaling_total{direction="scale-up"}[5m])

# Replica count mismatch
abs(wva_desired_replicas - wva_current_replicas)

# Scaling decisions by reason
sum by (reason) (rate(wva_replica_scaling_total[5m]))

# Optimization duration 95th percentile
histogram_quantile(0.95, rate(wva_optimization_duration_seconds_bucket[5m]))

# Saturation utilization by variant
wva_saturation_utilization

# KV cache utilization percentage
(wva_kv_cache_tokens_used / wva_kv_cache_tokens_capacity) * 100

# Variants requiring scale-up (token deficit)
wva_required_capacity{unit="continuous"} > 0

# High utilization — scale-up likely
wva_saturation_utilization > 0.85

# Little scale-down headroom left. wva_spare_capacity is an absolute token
# count, not a 0-1 ratio, so compare it against the utilization above rather
# than a fixed fraction.
wva_spare_capacity{unit="continuous"} == 0

# Models processed over time
wva_models_processed

# Metrics collection duration 99th percentile by query type
histogram_quantile(0.99, rate(wva_metrics_collection_duration_seconds_bucket[5m])) by (query_type)

# Metrics collection error rate
rate(wva_metrics_collection_errors_total[5m])

# Pods discovered per namespace
wva_metrics_pods_discovered

# Stale metrics by variant
wva_metrics_freshness_status{status="stale"}

# Fresh metrics ratio
sum(wva_metrics_freshness_status{status="fresh"}) / sum(wva_metrics_freshness_status)

# The scaling manager configuration info
wva_config_info

# Check if limiter is enabled
wva_config_info{limiter_enabled="true"}

# Optimization loop interval
wva_config_optimization_interval_seconds

# Total errors by component
wva_errors_total

# Error rate over time
rate(wva_errors_total[5m])

# Error rate by component
rate(wva_errors_total[5m]) by (component)

# Error rate by error type
rate(wva_errors_total[5m]) by (error_type)

# Decisions limited by limiter
wva_decisions_limited_total

# Decisions limited rate over time
rate(wva_decisions_limited_total[5m])

# Decisions limited by variant
rate(wva_decisions_limited_total[5m]) by (variant_name)

# Decisions limited by limiter type
rate(wva_decisions_limited_total[5m]) by (limiter_name)

# GPU discovery status
wva_gpu_discovery_up

# Check if GPU discovery is enabled
wva_gpu_discovery_up == 1

# Available GPUs by accelerator type
wva_available_gpus

# Total available GPUs across all types
sum(wva_available_gpus)

# Enforcer modifications total
wva_enforcer_modifications_total

# Enforcer modification rate over time
rate(wva_enforcer_modifications_total[5m])

# Enforcer modifications by policy type
rate(wva_enforcer_modifications_total[5m]) by (policy_type)

# Active optimizer
wva_optimizer_active

# Currently active optimizer (filter for value = 1)
wva_optimizer_active == 1
```

## Alerting rules

The scaling manager ships Prometheus alerting rules as a kustomize component,
[`config/components/prometheus-alerts`](../../config/components/prometheus-alerts/prometheusrule.yaml):
one PrometheusRule, `controller-manager-alerts`, group `wva.rules`, in the
controller's namespace. They are off by default.

### The shipped alerts

T = troubleshooting runbook on [Troubleshooting](troubleshooting.md). Each alert
also carries its runbook link in a `runbook_url` annotation, so Alertmanager
notifications link to it.

| alert | severity | for | fires when | what to do | runbook |
| --- | --- | --- | --- | --- | --- |
| `WVANodeAccessDenied` | critical | 2m | a `gpu-inventory` limiter is declared alone and the controller may not list nodes. **No variant scales up.** Beside a quota, `WVAConstraintProviderSkipped` fires instead | grant the controller's ServiceAccount get/list/watch on nodes (cluster admin: `make enable-physical-limiter` grants every controller it finds and verifies when it runs; re-run it after a new install), or remove the limiter | [T: controller not Ready](troubleshooting.md#the-controller-is-not-ready-or-crash-looping) |
| `WVAModelParkedWhileRequestsRefused` | critical | 5m | a model has 0 replicas and EPP returns `ServiceUnavailable` for it: traffic is being dropped | read `wva_model_scaling_blocked` for the model first | [T: will not wake](troubleshooting.md#a-model-will-not-wake-from-zero) |
| `WVAModelHasNoWakeSignal` | warning | 10m | blocked reason `no-wake-signal`: EPP exports no flow-control queue, so a parked model cannot wake | enable the EPP `flowControl` feature gate, then restart the EPP Deployment (it reads its config once at start) | [T: will not wake](troubleshooting.md#a-model-will-not-wake-from-zero) |
| `WVAScalerTargetsUndecided` | warning | 10m | `wva_scaler_undecided_targets` above 0 in a namespace: the scaler answers those ScaledObjects their current count, so their replica counts are frozen | `wva_scaler_undecided_answers_total` says why; the log line `external scaler has no decision for this target` names each one | [T: ScaledObject](troubleshooting.md#scaledobject-ready-false-or-targets-unknown) |
| `WVAHighErrorRate` | warning | 5m | `wva_errors_total` above 6/min for one `component`/`error_type` | grep the controller log for the `error_type` text | [Logs](monitoring.md#the-logs) |
| `WVAMetricsCollectionFailing` | warning | 5m | `wva_metrics_collection_errors_total` above 5/min for one `query_type`/`reason`: decisions run on stale data | check Prometheus reachability and the controller's Prometheus settings | [T: metrics stale](troubleshooting.md#metrics-stale-or-missing) |
| `WVAPolicyNamespaceUnverified` | warning | 10m | the controller may not read Namespaces, so it cannot tell whether an admin published cluster policy for it. It runs on its own ConfigMap, which may be weaker than the admin intends | (cluster admin) `NAMESPACE=<ns> make setup-prereqs`, then restart the controller: the value is set once at startup | [T: policy namespace unverified](troubleshooting.md#cluster-policy-is-not-applied-policy-namespace-unverified) |
| `WVAConstraintProviderSkipped` | warning | 10m | a declared limiter has been left out of decisions (`provider`, `reason`); the others still bound, so models scale past what it would allow | `node_read_denied`: (cluster admin) `make setup-prereqs`, or drop `gpu-inventory`. `failed`: the `GPU constraint provider skipped` log line says why | [T: GPUs held](troubleshooting.md#gpus-held-or-quota-spent) |
| `WVAGPUResourceExhausted` | warning | 5m | GPU-labeled nodes report zero allocatable GPUs of a type. Only with a `gpu-inventory` limiter | (cluster admin) check the nodes' allocatable GPUs and the device-plugin pods. No alert does not mean GPUs are free: the metric exists only with that limiter | [T: GPUs held](troubleshooting.md#gpus-held-or-quota-spent) |
| `WVAReplicaScalingThrashing` | warning | 10m | more than 2 cycles per minute for one variant whose decision was not `no-change`. The counter rises on every such cycle, so it fires on flapping **and** on a scale-up the workload never catches up with: actuation stuck, Pods Pending, slow replica starts | read the model's `scaling-decision` lines: `tgt` alternating is flapping; `tgt` steady above `curr` is actuation not landing | [T: flapping](troubleshooting.md#a-model-is-flapping), [T: actuation stuck](troubleshooting.md#desired-is-not-current-actuation-stuck) |
| `WVAModelWillNotScaleToZero` | info | 15m | blocked reason `variant-floor` or `policy-forbids-zero`: the configuration asks to park and forbids it | make the two halves agree | [will not park](../guides/scale-to-zero/README.md#when-a-model-will-not-park) |
| `WVAKueueReaderStale` | warning | 5m | `wva_quota_borrow_withheld{reason="stale"}` for a limiter: the Kueue read failed or is older than twice the refresh interval. Borrowing is frozen: nothing new is borrowed, nothing is given back | check the log for `external quota source unreadable` and the controller's Kueue RBAC; to hand capacity back meanwhile, lower the borrowing model's `maxReplicaCount` on its ScaledObject | [T: replicas dropped](troubleshooting.md#replicas-dropped-under-kueue-borrowing) |
| `WVAKueueLenderWaitingPastCooldown` | warning | 2m | `wva_kueue_lender_waiting_seconds` above 600, the default `borrowCooldown`. No metric carries the configured value: if you change `borrowCooldown`, change this threshold to match. A lender waits while a governed namespace still borrows in its cohort, and neither Kueue nor (by default) the scaling manager hands it capacity back | set `reclaimWithinCohort: Any` on the lender, lower the borrower's `maxReplicaCount`, or opt in with `releaseToNonReclaimingLenders: true`; with the opt-in, read `borrowReleaseLedger` in `wva-controller-status` (`release-not-actuated` means the lowered target did not land) | [T: replicas dropped](troubleshooting.md#replicas-dropped-under-kueue-borrowing) |
| `WVAKueueNeverRead` | warning | 10m | `wva_kueue_never_read` is 1: a Kueue-backed entry with a `-1` cap has never read Kueue, so that type is unbounded | the log line `external quota source unreadable and never read`; the Kueue CRDs; the controller's RBAC on ClusterQueues | [T: never read](troubleshooting.md#a-kueue-backed-quota-was-never-read) |
| `WVAKueuePodsUnplaced` | warning | 5m | `wva_kueue_unplaced_pods` above 0: pods unplaced past `placementTimeout` | `kubectl describe pod` for the scheduling message, and the Workload's admission | [T: replicas dropped](troubleshooting.md#replicas-dropped-under-kueue-borrowing) |
| `WVAKueueBorrowThrashing` | warning | 1m | `wva_borrow_releases_total` plus `wva_kueue_preempted_pods_total`, increased over 1h, above 3 for one model: it lost borrowed capacity more than 3 times in an hour, given back by the scaling manager or preempted by Kueue (with an `Any` lender, preemption is the usual cause) | raise the model's `nominalQuota`, lengthen `borrowCooldown`, or turn borrowing off for it | [T: replicas dropped](troubleshooting.md#replicas-dropped-under-kueue-borrowing) |
| `WVAOptimizationLoopStalled` | info | 15m | `wva_models_processed` is 0 or absent for 10 minutes | expected when no ScaledObject names this scaler. Otherwise check the controller pod, its log and Prometheus | [T: controller not Ready](troubleshooting.md#the-controller-is-not-ready-or-crash-looping) |

There is no alert on `engine-unsupported` on purpose: it is a standing property
of a model and would never clear.

### Installing the alerts

Set `DEPLOY_ALERTING_RULES=true` on the controller install. The installer adds
the `prometheus-alerts` component to the overlay it renders:

```bash
DEPLOY_ALERTING_RULES=true make deploy-wva NAMESPACE=<namespace>
```

Set it on **every** later install and upgrade: a re-run with
`DEPLOY_ALERTING_RULES=false` (the default) deletes `controller-manager-alerts`.

Prerequisites:

- the `PrometheusRule` CRD (Prometheus Operator);
- rights to create a PrometheusRule in the controller's namespace;
- a Prometheus that selects it. The rule carries `release:
  kube-prometheus-stack`; a different `ruleSelector` or `ruleNamespaceSelector`
  will not load it. On OpenShift, user-workload monitoring loads
  PrometheusRules from user namespaces.

To apply the rule by hand into an existing install instead:

```bash
kubectl apply -n "$NS" -f config/components/prometheus-alerts/prometheusrule.yaml
```

### Checking they loaded

```bash
kubectl get prometheusrule -n "$NS" controller-manager-alerts
```

Then open the Prometheus UI's `/alerts` page and look for the `wva.rules` group:

```bash
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-prometheus 9090:9090
# http://localhost:9090/alerts
```

![wva.rules](./wva.rules.png)

A firing rule, through the API:

```bash
curl -s http://localhost:9090/api/v1/alerts | jq '.data.alerts[] | select(.labels.alertname | startswith("WVA"))'
```

### Consider adding

These are **not shipped**. They catch silent failures the shipped rules miss.
Tune each `for:` to your cycle and start times.

```promql
# Requests queue for a model the scaling manager cannot see (for: 5m).
wva_unmeasured_queue > 0

# Pods decided on with stale or missing metrics (for: 10m).
wva_metrics_freshness_status{status!="fresh"} > 0

# A variant whose saturation gauges stopped refreshing (for: 10m).
wva_saturation_metrics_up == 0

# At its own ceiling and still over the scale-up threshold: demand unserved
# (for: 15m). Replace 0.85 with your scaleUpThreshold.
(wva_variant_at_max_replicas == 1)
  and on (exported_namespace, variant_name)
  (max by (exported_namespace, variant_name) (wva_saturation_utilization) > 0.85)

# The decision is not being carried out (for: 15m): KEDA, the HPA or the
# workload is stuck. Longer than your replica start time plus the HPA windows.
wva_desired_replicas != wva_current_replicas
```

### Alerting rules e2e test

Two layers test the rules, and both read the alert list from
`config/components/prometheus-alerts/prometheusrule.yaml`, so a new alert needs no
test edit:

- `test/alerts` (run by `make test`, no cluster) checks that every expression names
  only metrics the controller defines, or a short list of EPP metrics.
- `test/e2e/prometheus_alerts_test.go` checks the rule **as deployed**: it exists,
  holds exactly the alerts in the manifest, each is well formed, and, when
  Prometheus is reachable, every rule is loaded and healthy. It runs only with
  `DEPLOY_ALERTING_RULES=true`; `make test-e2e-smoke-with-setup` sets that up. See
  [Testing](../developer-guide/testing.md).
