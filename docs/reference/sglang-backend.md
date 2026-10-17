# SGLang Backend Support

## Overview

llm-scaling-manager works with both [vLLM](https://github.com/vllm-project/vllm)
and [SGLang](https://github.com/sgl-project/sglang). It reads each engine's
Prometheus metrics to estimate load and capacity, then scales through HPA/KEDA
the same way for both.

The engine is **detected per variant**; you do not configure it. A deployment
with no SGLang signal is treated as vLLM, so vLLM deployments are unaffected.

## How detection works

The scaling manager reads the pod template of the variant's scale target (the
Deployment or LeaderWorkerSet the ScaledObject points at). It is SGLang when
**either**:

- a container image reference contains `sglang` (e.g. `lmsysorg/sglang:...`), or
- a container command or args invoke `sglang.launch_server` / `sglang serve`
  (also inside a `/bin/sh -c "..."` wrapper).

Otherwise it is **vLLM** (the default).

**Any container counts, sidecars included** (`internal/inferenceengine/engine.go`).
A vLLM engine with an `sglang-router` sidecar (image `lmsysorg/sglang-router:...`,
or any reference containing `sglang`) is detected as SGLang. The scaling manager
then looks for `sglang:*` series the vLLM engine never emits, so the model gets
no saturation metrics: a `MetricsUnavailable` Warning event and no scaling
decision. There is no override field. To fix it, keep the `sglang` signal out
of the scale target's pod template:

- run the router as its own Deployment, not as a sidecar of the engine; or
- pull its image under a reference without `sglang`, and launch it with a
  command without `sglang.launch_server` or `sglang serve`.

Nothing else is engine-specific. Register the workload with a KEDA ScaledObject,
exactly as for vLLM; the same ScaledObject works for either engine. Register it
with [step 4 of the namespace install](../guides/install-in-namespace/README.md#4-register-the-workloads).
Every trigger key is in [ScaledObject trigger metadata](scaledobject-trigger.md),
and what the pod must carry is in [workload preparation](workload-preparation.md).

## Metrics

Both engines must provide the same signals. SGLang names them `sglang:*`,
labelled with `model_name`, which the scaling manager filters on:

| Signal | vLLM | SGLang |
|--------|------|--------|
| KV-cache utilization (0–1) | `vllm:kv_cache_usage_perc` | `sglang:token_usage` |
| Queue depth | `vllm:num_requests_waiting` | `sglang:num_queue_reqs` |
| Total KV-cache token capacity | derived from `vllm:cache_config_info` | `sglang:max_total_num_tokens` |
| Time to first token | `vllm:time_to_first_token_seconds` | `sglang:time_to_first_token_seconds` |
| Inter-token latency | `vllm:inter_token_latency_seconds` | `sglang:inter_token_latency_seconds` |
| Prompt tokens / request | `vllm:request_prompt_tokens_*` | `sglang:prompt_tokens_histogram_*` |
| Generation tokens / request | `vllm:request_generation_tokens_*` | `sglang:generation_tokens_histogram_*` |
| Prefix-cache hit rate | `vllm:prefix_cache_hits_total / vllm:prefix_cache_queries_total` | `sglang:cached_tokens_total / sglang:prompt_tokens_total` |
| Request count (scale-to-zero) | `vllm:request_success_total` | `sglang:num_requests_total` |

> Your Prometheus (or VictoriaMetrics) must scrape the SGLang pods' `/metrics`,
> just as it scrapes vLLM. The scaling manager only uses metrics that are in
> your monitoring backend.

## Serving-flag mapping

A variant with no live metrics yet (new, or scaled to zero) gets a capacity
estimate from its serving flags. SGLang flags map onto the same parameters as
vLLM's:

| Parameter | vLLM flag | SGLang flag |
|-----------|-----------|-------------|
| GPU memory fraction | `--gpu-memory-utilization` | `--mem-fraction-static` |
| Max concurrent requests | `--max-num-seqs` | `--max-running-requests` |
| KV page/block size | `--block-size` | `--page-size` |
| Tensor parallelism | `--tensor-parallel-size` | `--tp-size` / `--tp` |
| Total KV token capacity | (n/a) | `--max-total-tokens` |
| Context length | `--max-model-len` | `--context-length` |
| Eager / no CUDA graph | `--enforce-eager` | `--disable-cuda-graph` |

## Caveats

- **vLLM is the default.** Anything without an SGLang signal is vLLM. The signal
  is read from every container, so an SGLang sidecar beside a vLLM engine flips
  the pod to SGLang; see [How detection works](#how-detection-works).
- **One engine per variant.** Mixing vLLM and SGLang containers in one variant's
  pod is not supported.
- **Metrics need `--enable-metrics`.** SGLang exposes no `sglang:*` series
  without this server argument. (This repo's e2e uses an SGLang emulator, so the
  flag itself is not exercised here.) Then check Prometheus has your model:
  `count(sglang:token_usage{model_name="<modelID>"})` must be non-zero.
- **Scale-to-zero reads the SGLang counter.** Idleness comes from
  `sglang:num_requests_total`: the detected engine is passed to the enforcer,
  which picks the series (`CollectModelRequestCountForEngine`). Earlier releases
  always asked for `vllm:request_success_total`, so an SGLang model looked
  permanently idle and was refused parking. That gate is gone;
  `test/e2e/scale_to_zero_sglang_test.go` covers the round trip.

## See also

- Design proposal: [`docs/proposals/sglang-backend.md`](../proposals/sglang-backend.md)
- SGLang server arguments: <https://docs.sglang.io/advanced_features/server_arguments.html>
