# Prometheus Integration

The scaling manager integrates with Prometheus to collect metrics from different sources such as vLLM inference servers, and expose internal as well as custom autoscaling metrics. This guide covers Prometheus configuration, metric collection, and security best practices.

## Configuration

The scaling manager supports two methods for configuring Prometheus connectivity:

### 1. Environment Variables (Recommended)

Set Prometheus configuration via environment variables in the scaling manager deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
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
          value: "false"  # Set to "true" only for testing/development

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
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | No | Skip TLS certificate verification (dev/test only) | `false` |
| `PROMETHEUS_CA_CERT_PATH` | No | Path to CA certificate for TLS verification | - |
| `PROMETHEUS_CLIENT_CERT_PATH` | No | Path to client certificate for mutual TLS | - |
| `PROMETHEUS_CLIENT_KEY_PATH` | No | Path to client private key for mutual TLS | - |
| `PROMETHEUS_SERVER_NAME` | No | Expected server name in TLS certificate | - |
| `PROMETHEUS_BEARER_TOKEN` | No | Bearer token for Prometheus authentication | - |

### 2. ConfigMap Configuration

Alternatively, configure Prometheus via the controller's ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-manager-config
  namespace: workload-variant-autoscaler-system
data:
  PROMETHEUS_BASE_URL: "https://prometheus-k8s.monitoring.svc.cluster.local:9091"
  PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"
  PROMETHEUS_CA_CERT_PATH: "/etc/prometheus-certs/ca.crt"
  PROMETHEUS_CLIENT_CERT_PATH: "/etc/prometheus-certs/client.crt"
  PROMETHEUS_CLIENT_KEY_PATH: "/etc/prometheus-certs/client.key"
  PROMETHEUS_SERVER_NAME: "prometheus-k8s.monitoring.svc.cluster.local"
  PROMETHEUS_BEARER_TOKEN: "your-bearer-token"  # Not recommended - use Secret instead
```
**Configuration Priority:**
1. Environment variables (checked first)
2. ConfigMap values (fallback)
3. Error if neither provides `PROMETHEUS_BASE_URL`
   
### Metrics Endpoint
The metrics are exposed at the `/metrics` endpoint on port 8080 (HTTP).

### ServiceMonitor Configuration

The scaling manager metrics are exposed on port 8080 (HTTP):
```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: workload-variant-autoscaler
  namespace: workload-variant-autoscaler-system
  labels:
    release: kube-prometheus-stack
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
  endpoints:
  - port: http
    scheme: http
    interval: 30s
    path: /metrics
```



## Security Considerations

### TLS Configuration

**Production Deployments:**
- Always use HTTPS endpoints (`https://`)
- Provide CA certificate via `PROMETHEUS_CA_CERT_PATH`
- Never set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` in production

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
- If your Prometheus is only reachable over plain HTTP (e.g. `kube-prometheus-stack`'s
  default in-cluster service), set `PROMETHEUS_ALLOW_HTTP=true` and use an
  `http://` `PROMETHEUS_BASE_URL` instead of standing up a TLS-terminating proxy.
  This cannot be combined with `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY`,
  `PROMETHEUS_SERVER_NAME`, any `PROMETHEUS_CA_CERT_PATH`/client cert settings, or
  bearer token auth — the scaling manager refuses to start if those are set alongside a plain HTTP
  URL. Note that credentials and metrics are sent in cleartext, so use this only
  on a trusted network (dev/test or a secured in-cluster path).

### PromQL Injection Prevention

The scaling manager implements security measures to prevent PromQL injection attacks:

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

**Why This Matters:**
- Prevents unauthorized access to metrics from other namespaces
- Blocks label injection attacks that could manipulate query results
- Ensures multi-tenant deployments remain isolated



## llm-scaling-manager Metrics

The scaling manager exposes metrics providing insights into autoscaling behavior and optimization performance. These metrics are exposed via Prometheus at the `/metrics` endpoint.

### Notes on **name_space**s in metrics
With the scaling manager metrics, the value for the label `namespace` is the scaling manager controller namespace, not the VA's namespace. The VA namespace has the label `exported_namespace`. Here's an example:
```text
{
  "metric": "wva_desired_replicas",
  "labels": {
    "accelerator_type": "A100",
    "container": "manager",
    "endpoint": "https",
    "exported_namespace": "llm-d-sim",    <==== VA namespace
    "instance": "10.244.0.73:8443",
    "job": "workload-variant-autoscaler-metrics",
    "namespace": "workload-variant-autoscaler-system",  <=== controller namespace
    "pod": "workload-variant-autoscaler-controller-manager-75b45dd7c-89g5s",
    "service": "workload-variant-autoscaler-metrics",
    "variant_name": "workload-variant-autoscaler-va"
  },
  "value": "2"
}
```

### Configuration Metrics

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
      "job": "workload-variant-autoscaler-metrics",
      "limiter_enabled": "false",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "scale_to_zero_enabled": "false",
      "service": "workload-variant-autoscaler-metrics"
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "30"
    ]
  }
  ```
### Metrics Collection Observability

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
      "job": "workload-variant-autoscaler-metrics",
      "le": "0.001",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "query_type": "cache_config",
      "service": "workload-variant-autoscaler-metrics"
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
  - `query_type`: Type of metrics query that failed
  - `reason`: Reason for the error
- **Use Case**: Track metrics collection failures and identify problematic queries
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_metrics_collection_errors_total",
      "container": "manager",
      "endpoint": "https",
      "instance": "10.244.2.59:8443",
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-f9fdb95df-wvd9p",
      "query_type": "kv_cache",
      "reason": "bad_data",
      "service": "workload-variant-autoscaler-metrics"
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-f9fdb95df-wvd9p",
      "exported_namespace": "llm-d-sim",
      "reason": "unresolved",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778854211.669,
      "2"
    ]
  }
  ```

### Optimization Metrics

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
      "job": "workload-variant-autoscaler-metrics",
      "le": "0.01",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### Saturation and Capacity Metrics

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
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "model_name": "unsloth/Meta-Llama-3.1-8B",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
      "variant_name": "smoke-test-dual-shared-va"
    },
    "value": [
      1778846184.925,
      "0"
    ]
  }
  ```

### Pipeline Stage Visibility Metrics

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
      "job": "workload-variant-autoscaler-metrics",
      "limiter_name": "gpu-limiter",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-659d5c9dcf-w6cw7",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### `wva_node_access_denied`
- **Type**: Gauge
- **Description**: `1` while a configured physical GPU limiter cannot read nodes, `0` when it can. Absent when no physical limiter is configured — nothing is asking for them.
- **Labels**: optional `controller_instance`
- **Use Case**: the combination that fails with no other symptom. A GPU-aware limiter allocates out of per-accelerator pools, so without nodes every variant is charged to no pool, receives no budget, and stops scaling up — and an unresolved accelerator is otherwise a normal state, so nothing else complains. Alert on `wva_node_access_denied == 1`; the shipped `WVANodeAccessDenied` rule does.

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
        "job": "workload-variant-autoscaler-metrics",
        "namespace": "workload-variant-autoscaler-system",
        "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
        "service": "workload-variant-autoscaler-metrics"
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-c8b9c74b5-82t2c",
      "policy_type": "scale_to_zero",
      "service": "workload-variant-autoscaler-metrics"
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "optimizer_name": "cost-aware",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778846184.925,
      "1"
    ]
  }
  ```

### Utilization-Share Optimizer Metrics

Published when the [utilization-share optimizer](scaling-policy.md#optimizer-cluster-default-only-live)
is selected, except `wva_utilization_share_mode`, which the leader publishes
every cycle, from its first, including cycles with no active model. The
**gauges** are replaced each cycle, so a model or group that leaves takes its
gauge series with it, and switching the optimizer off, a cycle with no quota
read, or a cycle with no active model deletes them all. The **counters and histograms** (`_transfers_total`,
`_withheld_total`, `_claims_total`, `_donors_per_transfer`, `_release_seconds`)
are never deleted: they keep their last values after the optimizer is switched
off, as counters do. Use `rate()` or `increase()` on them, not their raw value, to
judge whether the optimizer is doing anything now.

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

Published in shadow mode as well as when acting:

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
| `wva_utilization_share_swinging` | gauge | role | `1` while the role is planned on its mean need because it reversed direction twice within the swing window. |
| `wva_utilization_share_in_flight` | gauge | group + `state` | Transfers in flight, by `state`: `releasing` (the donor has not released its GPUs yet) or `filling` (released, or an idle fill, and the receiver has not taken the GPUs yet). Both series exist for every active group, `0` when nothing is in flight. |
| `wva_utilization_share_effective_seconds` | gauge | group + `param`, `source` | A derived timing in force, for the group as a whole (from its slowest donor). `param`: `window`, `release-timeout`, `fill-timeout`, `reversal-hold`, `swing-window`. `source`: `measured` (the group's own releases), `scaledobject`, `pod` (termination grace), `default` (the cluster's default for an unset value). |
| `wva_utilization_share_transfers_total` | counter | group + `outcome`, `urgent` | Transfers that ended, by `outcome` (below), idle fills included. `urgent="true"` when the receiver was below its need. Every `outcome` × `urgent` series is published at `0` from a group's first acting cycle, so `increase()` sees the first transfer of each outcome. |
| `wva_utilization_share_withheld_total` | counter | group + `reason` | Transfers not planned: `reversal-hold` (a role that just gave cannot receive, or one that just received cannot give, until the hold passes) or `not-actionable` (a role out of band with no whole replica to move). |
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
not landing; alert on `increase()` of those outcomes (see
[monitoring](monitoring.md#the-metrics-that-answer-specific-questions)), and read
the blocked reasons below for which models are affected.

At most two transfers with a donor run per group at once
(`wva_utilization_share_in_flight` counts them with idle fills included). Idle
fills move nothing from another model and are not limited by that cap; they are
paced only by two replicas per role per cycle.

Transfers are also recorded as Kubernetes Events on the scale targets they move;
see [Events](monitoring.md#events-on-the-models-scale-targets).

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
| `awaiting-release` | The model receives a transfer whose donor has not released its GPUs yet. | Normally clears within one release. If it persists, check the donor's ScaledObject scale-down window and its pods' termination grace; transfers that time out show as `aborted`. |
| `quota-short` | The role holds less than its need and the whole group is short: no rebalance can cover it. | Raise the quota for that accelerator type, or lower demand. Weights decide who is cut. |
| `floor-pinned` | The model's `minReplicaCount` holds at least one replica more than it needs; GPUs the share would otherwise give to others. Only the configured `minReplicaCount` counts, not the last replica the optimizer keeps on any running role. | Lower `minReplicaCount` if the floor is not deliberate. |
| `floors-exceed-quota` | The configured floors (`minReplicaCount`) of the group's models add up to more than its budget. The last replica the optimizer keeps on a running role is not counted. | Lower floors or raise the quota; until then every model of the group shows this. |
| `donors-at-floor` | The role is short, and no other role holds more than its floor to give. | Lower another model's `minReplicaCount`, or raise the quota. |
| `no-compatible-donor` | The role is short and out of band, but no donor's pods can host one of its replicas: each of its pods needs a donor pod at least as large (for example, it runs 8-GPU pods and every donor runs 2-GPU pods). | Nothing to tune in the optimizer: give the model GPUs another way (a larger quota, or scale a compatible model down). See [troubleshooting](troubleshooting.md#a-model-shows-no-compatible-donor). |
| `release-timeout` | The role's last release as a donor was aborted (it did not scale down within the release timeout), and it is backing off before it is asked to give again. Set only for an aborted release; a donor that could not be marked shows `donor-not-steerable` instead. | Check why it did not scale down in time: a long scale-down window, pods slow to terminate, or a ScaledObject not acting. |
| `donor-not-steerable` | The role was asked to give, but the pod that would go cannot be steered by a fault: a sibling's deletion cost left no room for the mark (a cost a user set at the int32 minimum, or no gap between an earlier transfer's mark and the lowest unmarked sibling), or the pod patch failed. Present only while it backs off (the back-off doubles with each failure in a row, up to 16 release timeouts), then it is asked again. Not set for a donor whose workload is changing -- a rollout (by its status, or pods of two ReplicaSets or LWS revisions), a pod not yet created, scheduled or Ready, an LWS group being replaced -- nor for one whose every pod is already given to transfers in flight: those are held for a release timeout with no back-off, and routine. | Check the deletion costs set on the donor's pods, and RBAC to patch pods. The `UtilizationShareDonorNotSteerable` Event on the donor and the controller log line `could not mark a donor pod` carry the cause. |
| `reversal-hold` | The role is short and a move could fix it, but it gave GPUs within the reversal hold, so it may not receive yet. It applies to a role below its need too, unless the role holds less than three quarters of its need and a donor would keep its own need after giving: then the hold is lifted for that move. A transfer that was cancelled before anything moved holds only its own direction, so it never sets this on its donor. | Usually clears by itself; the hold lasts `wva_utilization_share_effective_seconds{param="reversal-hold"}` from the start of the transfer it gave in, about twice a release. Persisting means load on this model swings faster than transfers land. |
| `swinging` | The role reversed direction twice within the swing window and is planned on its mean need over that window, not its current one. | Nothing to tune: its load moves faster than the optimizer can follow. Give it headroom by weight or floor if it must not lag. |
| `transfer-limit` | The role is short and a move could fix it, but its group already has the most transfers with a donor in flight (two). | Clears as transfers land. Persisting means transfers are slow to release; check `wva_utilization_share_in_flight{state="releasing"}` and the donors' scale-down windows. |
| `release-taken` | The role's last transfer timed out filling, and the GPUs its donors released were taken by a pod the scaling manager did not place. Needs node information. Reported for one release timeout after the fill timed out, then cleared; it returns if the next fill fails the same way. | Look for other workloads scheduling onto the same accelerator type. |
| `release-shape-mismatch` | The role's last transfer timed out filling: enough GPUs were free in total, but no node had enough for its largest pod. Needs node information. Reported for one release timeout after the fill timed out, then cleared. | GPUs are fragmented across nodes; the model needs a node with enough GPUs free at once. |

### Replica Management Metrics

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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics",
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
- **Description**: Total number of replica scaling operations performed, labeled by direction and reason
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `direction`: Scaling direction (`up`, `down`)
  - `reason`: Reason for the scaling operation
- **Use Case**: Detect scaling thrashing — backs the `WVAReplicaScalingThrashing` alert — and audit scaling activity per variant
- **Example**:
  ```json
  {
    "metric": {
      "__name__": "wva_replica_scaling_total",
      "direction": "up",
      "namespace": "workload-variant-autoscaler-system",
      "reason": "saturation",
      "variant_name": "smoke-test-va"
    },
    "value": [
      1778846184.925,
      "3"
    ]
  }
  ```

### Error Tracking

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
      "job": "workload-variant-autoscaler-metrics",
      "namespace": "workload-variant-autoscaler-system",
      "pod": "workload-variant-autoscaler-controller-manager-6ddfbddf57-l5ptf",
      "service": "workload-variant-autoscaler-metrics"
    },
    "value": [
      1778850096.639,
      "1"
    ]
  }
  ```

## Example Queries

### Basic Queries
```promql
# Current replicas by variant
wva_current_replicas

# Scaling frequency
rate(wva_replica_scaling_total[5m])

# Desired replicas by variant
wva_desired_replicas
```

### Advanced Queries
```promql
# Scaling frequency by direction
rate(wva_replica_scaling_total{direction="scale_up"}[5m])

# Replica count mismatch
abs(wva_desired_replicas - wva_current_replicas)

# Scaling frequency by reason
rate(wva_replica_scaling_total[5m]) by (reason)

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

## Alerting Rules

The scaling manager pre-defined a number of Prometheus alerting rules which can be optionally installed. These rules are defined in `config/components/prometheus-alerts/prometheusrule.yaml`.
### Alerting Rules Installation
- To install alerting rules, set environment variable `DEPLOY_ALERTING_RULES` to `true`, and run the installation, for example:
  ```bash
  export DEPLOY_ALERTING_RULES=true
  make deploy-e2e-infra
  ```
- To remove alerting rules, set environment variable `DEPLOY_ALERTING_RULES` to `false`, and run the installation, for example:
  ```bash
  export DEPLOY_ALERTING_RULES=false
  make deploy-e2e-infra
  ```

### Alerting Rules Verification
Once installed, you can verify as follows:
- Check that PrometheusRule resource has been created:
  ```bash
  kubectl get PrometheusRule -n workload-variant-autoscaler-system controller-manager-alerts
  NAME                        AGE
  controller-manager-alerts   40h
  ```

- Check Prometheus:
  - Forward Prometheus service to local host:
    ```bash
    kubectl port-forward -n workload-variant-autoscaler-monitoring svc/kube-prometheus-stack-prometheus 9090:9090
    ```

  - Browse to https://localhost:9090/alerts
  - You should see `wva.rules`: ![wva.rules](./wva.rules.png)
  - Here's an example of a triggered rule: ![wva.rules-firing](./wva.rules-firing.png)

- Get fired-rule details:
    From https://localhost:9090/alerts, if there's a rule has been fired, you can get the details as follows. Here's an example:
    ```bash
    curl -k https://localhost:9090/api/v1/alerts | jq .

    {
        "labels": {
          "alertname": "WVAHighErrorRate",
          "component": "controller",
          "error_type": "Config is nil in ConfigMapReconciler bootstrap",
          "severity": "warning"
        },
        "annotations": {
          "description": "Controller component 'controller' error_type 'Config is nil in ConfigMapReconciler bootstrap' rate is 0.03/sec (>6/min threshold) sustained for 5+ minutes. Check controller logs for error patterns.",
          "summary": "llm-scaling-manager error rate elevated in controller"
        },
        "state": "pending",
        "activeAt": "2026-07-02T19:35:06.642428048Z",
        "value": "3.1034482758620693e-02"
    }
    ```

### Alerting Rules Notes
- For `WVAGPUResourceExhausted` rule with `wva_available_gpus` metric, as described in [wva_available_gpus](#wva_available_gpus) this metric is not always available in which case this rule will not trigger. In other words, if there's no alert for this rule, it does not mean GPUs are available.
  
### Alerting Rules E2E Test
E2E tests for alerting rules are in `test/e2e/prometheus_alerts_test.go`. The tests cover basic install and validation. Here are some scenarios:
- should create PrometheusRule with the scaling manager alert rules
- should have all expected alert rules defined
- should have valid alert rule structure 
- should only reference known the scaling manager metrics in alert expressions

How to execute E2e Test:
```bash
export DEPLOY_ALERTING_RULES=true
make deploy-e2e-infra
make test-e2e-smoke FOCUS="PrometheusAlerts"
```