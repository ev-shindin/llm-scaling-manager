# Replica start time: what each option buys

A new replica serves nothing until it is Ready, so its start time decides how
long a scale-up leaves the running replicas alone with the queue. This page puts
every measured start time in one place: what each preparation step takes off,
what a warm pool takes off, and what each costs. It compares; it does not
explain how. For the steps, follow the links.

Every number says what it was measured on. Numbers from different models or
GPUs sit in different tables; do not read across them.

## Qwen3-32B on one H200

Qwen3-32B bf16 (61 GiB of weights), one H200, `--max-model-len 8192
--gpu-memory-utilization 0.90`, vLLM defaults otherwise (torch.compile and CUDA
graphs on). Seconds from Pod creation to `/health` 200; the first token follows
within 0.1 s. Measured on an H200 cluster, 2026-10, from in-pod timestamps,
median of 2 to 3 starts after one start that fills the caches.

Each row adds to the one above it.

| | vLLM | to `/health` | what it takes |
| --- | --- | ---: | --- |
| Unprepared: weights on a network ReadWriteMany claim, no engine cache, image on the node | v0.26.0 | 102.6 s | nothing |
| ... and the node does not have the image | v0.26.0 | + 67-80 s pull | nothing; this is what an unprepared node pays |
| Prepared: [`make prepull`](workload-preparation.md#holding-the-image-on-the-nodes), [`make weights`](workload-preparation.md#weights-on-the-nodes-disk) on the node's disk, seeded [`make engine-cache`](workload-preparation.md#engine-caches-on-the-nodes-disk) | v0.26.0 | 59.8 s | 62 GB of disk and ~200 MB of caches per node; rights to create PersistentVolumes |
| + [Python bytecode cache](workload-preparation.md#python-bytecode) | v0.26.0 | 48.3 s | ~150 MB per image per node |
| + one-file AOT compile (`VLLM_USE_MEGA_AOT_ARTIFACT=1`, which `make workload-patch` sets on 0.26.x) | v0.26.0 | 44.0-44.2 s | one recompile per node when first turned on |
| The same preparation on a newer engine, defaults | v0.32.0 | 50.4 s | nothing more |
| v0.32.0 + `--kv-cache-memory-bytes` from a previous start (`make workload-patch` proposes it, never applies it) | v0.32.0 | 42.3 s | a value you must derive again after any model, GPU, flag or version change |

- **Why v0.32 is slower by default.** Its Model Runner V2 sizes the KV cache by
  capturing every CUDA graph once more (~9 s of the start). A fixed KV size
  skips that, with the same KV cache, output and decode speed. Too large a value
  runs out of memory; see [Depends on the vLLM version](workload-preparation.md#depends-on-the-vllm-version).
- **Bytecode on v0.32 buys nothing.** The image ships its own; the guard in
  `make workload-patch` unsets the variable there.
- **The bytecode gain holds across shapes** (vLLM v0.26.0, same node class):

  | | prepared | + bytecode |
  | --- | ---: | ---: |
  | Qwen3-32B, TP=2, one Pod | 72.4 s | 61.0 s |
  | Qwen3-32B, TP=2, LeaderWorkerSet leader + worker | 108.6 s | 97.7 s |
  | Qwen3-30B-A3B (MoE), TP=1 | 61.8 s | 50.8 s |

  TP=2 does not load faster than TP=1 (weights 9.0 s against 7.8 s).

### Where the prepared start goes

The 59.8 s prepared start on v0.26.0, phase by phase, beside the unprepared one.

| phase | unprepared | prepared |
| --- | ---: | ---: |
| Pod created to container started (incl. volume mount) | 9.2 | 2.3 |
| Python and vLLM imports | 13.4 | 13.4 |
| API server: model config and architecture check | 11.4 | 0.0 |
| engine process spawn and imports | 7.0 | 7.2 |
| engine init to first CUDA context | 2.4 | 4.6 |
| weight load | 12.1 | 7.8 |
| torch.compile (miss / hit) | 23.4 | 5.1 |
| memory profile and KV sizing | 2.6 | 1.8 |
| kernel warmup and CUDA-graph capture (capture 6.0) | 13.8 | 14.1 |
| engine done to `/health` | 7.0 | 3.1 |
| other | 0.3 | 0.4 |
| **total** | **102.6** | **59.8** |

About 27.5 s of the prepared start happens before the engine touches the GPU.
The bytecode cache halves the imports (13.4 to 6.2 s) and the engine init
(4.6 to 2.3 s). The one-file AOT compile takes torch.compile from 5.1 to 0.8 s.
What is left is mostly GPU work: warmup, graph capture and the weight load.

**The page cache matters as much as the disk.** On v0.32.0, prepared, the weight
load took 19.9 s with the weight files evicted from memory (53.7 s to `/health`),
and 10.0 s when they had been read once beforehand (43.6 s). A node that ran the
model recently has them in memory; a fresh node does not. Recorded in
[Reading a model's weights into memory](../proposals/weights-prewarm-on-schedule.md#small-model-qwen3-32b-one-gpu).

## The warm pool

A [warm pool](../guides/warm-pool/) holds Pods with models already loaded and
asleep, and lends one to a model that is scaling up. It does not shorten a
start; it serves while the start happens.

These numbers are **8B-class models on H100 and H200**, not the 32B above.
Measured on an H100 test cluster, 2026-08
([fast model loading §2.7](../proposals/fast-model-loading.md#27-the-pool-was-built-and-measured-2026-08-21)),
and on an H200 cluster, 2026-09 ([measured.md](../well-lit-paths/warm-pool-bridge/measured.md)).

| | without a pool | with a pool |
| --- | ---: | ---: |
| time to serve, two models resident in a pool Pod (H100) | ~41 s cold start | 435-440 ms model switch (four switches) |
| TTFT at p95 during a rise, two 8B models bursting out of phase (H200) | 5.2-8.8 s | 0.11-0.83 s |

**Count the same costs on both sides.** Neither number includes the scale-up
decision. A cold replica waits for it, and so does a lend: in the benchmark the
bridge was first seen lent 33-143 s after each traffic rise.

What a pool holds:

| | held | measured |
| --- | --- | --- |
| GPUs | every pool Pod's accelerators, all the time, lending or idle | a one-Pod pool for two models: 14,138 GPU-seconds against 12,129 for autoscaling alone (+17 %) and 16,080 for a floor of two replicas (+33 %) |
| host RAM, sleep level 1 (what the pool implements) | per resident model | about 2.6 GiB + 1.4x its weights; an 8B model 23.4 GiB, so about five 8B models in a 128 GiB Pod ([sizing](../guides/warm-pool/sizing.md#how-many-models-fit-in-one-pod)) |
| GPU memory | per sleeping model | about 1.4 GiB, taken from the awake model's KV cache |
| host RAM, sleep level 2 (not implemented) | megabytes | wake re-reads the weights: 37 s on a shared claim ([sleep levels](../proposals/warm-pool-sleep-levels.md)) |

**When it pays.** More than one model per pool Pod, with bursts that do not all
arrive at once; or scale-to-zero that must answer its first request fast. A pool
Pod holding one model is an idle replica. A model that is always bursting is
cheaper on a floor (`minReplicaCount: 2`).

## Larger models: GLM-5.2-FP8

GLM-5.2-FP8 (744B MoE) on one 8xH200 node, DP=8 with expert parallelism,
weights on the node's disk. Measured 2026-09, recorded in
[Reading a model's weights into memory](../proposals/weights-prewarm-on-schedule.md#big-model-glm-52-fp8)
and the [launch post](../blog/sub-second-scale-ups-on-llm-d.md#why-a-scale-up-is-slow-and-not-for-the-reason-people-assume).

| | `vllm serve` to ready |
| --- | ---: |
| JIT and compile caches warm, real weights | 192 s |
| caches warm, no weights read (`--load-format dummy`) | 152 s |
| JIT cache cold, no weights read | 463 s |

- **Preparation buys the 311 s.** A cold JIT cache costs five minutes of kernel
  compiles. Keep the engine caches on the node and seed them
  ([engine caches](workload-preparation.md#engine-caches-on-the-nodes-disk)).
- **The weights are 40 s of 192.** Faster storage or a peer transfer can buy at
  most that.
- **About 60-90 s is process and rank construction** before any weight is
  read. No cache removes it. Only a replica that is already running does: a
  [retained pool](../well-lit-paths/retained-pool/), a pool of
  [groups](../guides/warm-pool/groups.md), or a floor. Check first that host RAM
  can hold a sleeper at all: `deploy/warmpool.sh sizing --params 744B --dtype fp8`.

## Experimental, not built

Nothing here is supported. They are recorded so the numbers are not lost.

- **Sparse snapshot restore.** Qwen3-32B, TP=1, vLLM v0.26.0, one H200: the
  engine put to sleep (level 2), checkpointed with `cuda-checkpoint` and CRIU
  (a 6.2 GB image), and restored into a new Pod: 15.1 s to `/health`, against
  59.8 s prepared, same output. It needs a privileged Pod and tools the engine
  image does not ship, and works for a single rank only: multi-rank engines hung
  in the driver. Measured on an H200 cluster, 2026-10, median of 3 restores.
  A snapshot that includes the weights (140 GB) restored in 76-78 s, slower than
  a cold start.
- **Reading the weights while the engine starts.** A background reader in the
  engine container fills the page cache during imports. Estimated, not measured:
  up to ~9.9 s on Qwen3-32B, 38-90 s per node on GLM class with one disk
  ([proposal](../proposals/weights-prewarm-on-schedule.md#13-how-much-a-head-start-buys)).

## Which to use

| model and traffic | use | what it adds |
| --- | --- | --- |
| Fits one GPU or one node, load ramps slowly | preparation only | disk: model size + a few GB of caches per node; rights to create PersistentVolumes (or an admin runs it once) |
| Several models, bursts out of phase | preparation + a warm pool shared by them | the pool's GPUs all the time; host RAM per resident model; a Role in the pool's namespace |
| One model, always busy, sharp spikes | preparation + a floor (`minReplicaCount: 2`) | one replica held all the time |
| Scale to zero, first request may wait | preparation only | 0 GPUs while idle; the first request waits a full start (~44-60 s for the 32B above, plus scheduling) |
| Scale to zero, first request must be fast | preparation + a warm pool | the pool's GPUs, shared across every parked model it holds |
| GLM class, one node or more | preparation (the JIT cache is not optional) + a retained pool or a floor | whole nodes held; host RAM may rule warming out |

Preparation is never the wrong first step: it costs disk, not accelerators.

## Not levers

Measured, and not worth chasing:

- **The `ll_bf16` kernel warmup** (~7.5 s on vLLM v0.26.0). It runs with its
  cache turned off, so no cache directory keeps it. From vLLM 0.27 it runs only
  for MoE models with a bf16 router; a dense model no longer pays it.
- **Trimming CUDA-graph capture sizes.** It changes how the engine serves. On
  the 32B the defaults already stop at 512, below the batch limit; on GLM capture
  is ~3 s.
- **Running the engine in one process.** It saves the engine spawn, and it
  changes how the engine serves.
- **Going back to the older model runner on v0.32.0**
  (`VLLM_USE_V2_MODEL_RUNNER=0`). Faster to start (40.4 s), but a different
  runner with a different KV size: it changes serving. Use
  `--kv-cache-memory-bytes` instead (42.3 s).
- **FlashInfer autotune.** Already cached with the engine caches; on Qwen3-32B
  there is nothing to tune.
