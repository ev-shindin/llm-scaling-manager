# Replica start time: what each option buys

A new replica serves nothing until it is Ready, so its start time decides how
long a scale-up leaves the running replicas alone with the queue. This page
compares the options for shortening that time, or not paying it at all: what
each buys and what each costs. For the steps, follow the links.

Every number says what it was measured on. Numbers from different models or
GPUs sit in different tables; do not read across them.

## Which to use

Preparation is never the wrong first step: it costs disk, not accelerators.

| model and traffic | use | what it adds |
| --- | --- | --- |
| Fits one GPU, load ramps slowly | preparation only | disk: model size + a few GB of caches per node; rights to create PersistentVolumes (or an admin runs it once) |
| Several GPUs: TP=2 in one Pod, or a LeaderWorkerSet | preparation only, on every node the group can land on | the same, per node. On Qwen3-32B TP=2 starts ~12 s slower than TP=1, and a two-Pod LeaderWorkerSet ~49 s slower |
| A node that has never run the model | `make prepull` and `make weights` before it is needed | without them, at least 67-80 s of image pull, then a download |
| Several models, bursts out of phase | preparation + a warm pool shared by them | the pool's GPUs all the time; host RAM per resident model; a Role in the pool's namespace |
| One model, always busy, sharp spikes | preparation + a floor (`minReplicaCount: 2`) | one replica held all the time |
| Scale to zero, first request may wait | preparation only | 0 GPUs while idle; the first request waits a full start (~44-60 s for Qwen3-32B below, plus scheduling) |
| Scale to zero, first request must be fast | preparation + a warm pool | the pool's GPUs, shared across every parked model it holds |
| GLM class, one node or more | preparation (the JIT cache is not optional) + a retained pool or a floor | whole nodes held; host RAM may rule warming out |

**One-time setup per node.** Measured for Qwen3-32B: `make weights` downloaded
the 62 GB in ~25 s on that cluster's network; the first start that fills the
engine cache, and copying it to the seed, took ~2.5 min more. Allow 3-4.5 min
per node in all, once. How to run each:
[`make prepull`](workload-preparation.md#holding-the-image-on-the-nodes),
[`make weights`](workload-preparation.md#weights-on-the-nodes-disk),
[`make engine-cache`](workload-preparation.md#engine-caches-on-the-nodes-disk).

## Words used here

| term | meaning |
| --- | --- |
| AOT compile | vLLM saves its torch.compile result and loads it on the next start instead of compiling again |
| Model Runner V2 | the newer vLLM model runner (the part that runs the model on the GPU); the default for every model from vLLM 0.29, by its source |
| CUDA graphs | GPU work recorded once at start so serving replays it fast; recording takes seconds |
| page cache | file contents the node's kernel keeps in RAM after a read; a second read skips the disk |
| JIT cache | GPU kernels compiled at first use and kept on disk for the next start |
| TP / DP / EP | tensor, data and expert parallelism: how one engine is split over GPUs |
| sleep level 1 / 2 | a sleeping engine keeps its weights in host RAM (1) or drops them (2); either frees the GPU |
| floor | `minReplicaCount` held above zero, so a replica is always running |
| lend / bridge | a warm-pool Pod serving for a model while that model's own new replica starts |
| warming | loading a model into a pool Pod and putting it to sleep, ready to lend |
| parked | a model scaled to zero, held asleep in a pool Pod |
| retained pool | a pool that keeps what it lends: several large models loaded on one set of GPUs (a Pod or a whole group), one awake at a time ([retained pool](../well-lit-paths/retained-pool/)) |

## Qwen3-32B on one H200

Qwen3-32B bf16 (61 GiB of weights), one H200, `--max-model-len 8192
--gpu-memory-utilization 0.90`, vLLM defaults otherwise. Seconds from Pod
creation to `/health` 200; the first token follows within 0.1 s. Measured on an
H200 cluster, 2026-10, from in-pod timestamps, median of 2 to 3 starts after
one start that fills the caches.

Each row adds to the one above it. The prepared rows have the weight files
already in the node's page cache; a fresh node adds ~10 s (see below).

| | vLLM | to `/health` | what it takes |
| --- | --- | ---: | --- |
| Unprepared: weights on a network ReadWriteMany claim, no engine cache, image on the node | v0.26.0 | 102.6 s | nothing |
| ... and the node does not have the image | v0.26.0 | + at least 67-80 s pull | nothing; this is what an unprepared node pays |
| Prepared: [`make prepull`](workload-preparation.md#holding-the-image-on-the-nodes), [`make weights`](workload-preparation.md#weights-on-the-nodes-disk) on the node's disk, seeded [`make engine-cache`](workload-preparation.md#engine-caches-on-the-nodes-disk) | v0.26.0 | 59.8 s | 62 GB of disk and ~200 MB of caches per node; rights to create PersistentVolumes |
| + [Python bytecode cache](workload-preparation.md#python-bytecode) | v0.26.0 | 48.3 s | ~150 MB per image per node |
| + one-file AOT compile (`VLLM_USE_MEGA_AOT_ARTIFACT=1`, which `make workload-patch` sets on 0.26.x) | v0.26.0 | 44.0-44.2 s | one recompile per node when first turned on |
| The same preparation on a newer engine, defaults | v0.32.0 | 50.4 s | nothing more |
| v0.32.0 + `--kv-cache-memory-bytes` from a previous start (`make workload-patch` proposes it, never applies it) | v0.32.0 | 42.3 s | a value you must derive again after any model, GPU, flag or version change |

- **Not every row ran on the same node, and the pull is a lower bound**; see
  [measurement conditions](../developer-guide/replica-start-anatomy.md#measurement-conditions).
- **Why v0.32 is slower by default.** Model Runner V2 sizes the KV cache by
  recording every CUDA graph once more (~9 s). A fixed KV size skips that, with
  the same KV cache, output and decode speed. Too large a value runs out of
  memory; see [Depends on the vLLM version](workload-preparation.md#depends-on-the-vllm-version).
- **Bytecode on v0.32 buys nothing.** The image ships its own; the guard in
  `make workload-patch` unsets the variable there.
- **The bytecode gain holds across shapes** (v0.26.0):

  | | prepared | + bytecode |
  | --- | ---: | ---: |
  | Qwen3-32B, TP=2, one Pod | 72.4 s | 61.0 s |
  | Qwen3-32B, TP=2, LeaderWorkerSet leader + worker | 108.6 s | 97.7 s |
  | Qwen3-30B-A3B (MoE), TP=1 | 61.8 s | 50.8 s |

**A fresh node is ~10 s slower.** On v0.32.0, prepared, the weight load took
19.9 s with the weight files out of memory (53.7 s to `/health`) and 10.0 s when
they had been read once beforehand (43.6 s). A node that ran the model recently
has them in memory; a fresh node does not. Recorded in
[Reading a model's weights into memory](../proposals/weights-prewarm-on-schedule.md#small-model-qwen3-32b-one-gpu).

About half of the prepared start happens before the engine touches the GPU, and
most of the rest is CUDA-graph recording, kernel warmup and the weight load:
phase by phase in [Anatomy of a replica start](../developer-guide/replica-start-anatomy.md),
with the levers measured and ruled out.

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
| GPUs | every pool Pod's accelerators, all the time, lending or idle | a one-Pod pool for two models: +17 % GPU-seconds over autoscaling alone; 12 % fewer than a floor of two |
| host RAM, sleep level 1 (what the pool implements) | per resident model | about 2.6 GiB + 1.4x its weights; an 8B model 23.4 GiB, so about five 8B models in a 128 GiB Pod ([sizing](../guides/warm-pool/sizing.md#how-many-models-fit-in-one-pod)) |
| GPU memory | per sleeping model | about 1.4 GiB, taken from the awake model's KV cache |
| host RAM, sleep level 2 (not implemented) | megabytes | wake re-reads the weights: 37 s on a shared claim ([sleep levels](../proposals/warm-pool-sleep-levels.md)) |

**When it pays.** More than one model per pool Pod, with bursts that do not all
arrive at once; or scale-to-zero that must answer its first request fast. A pool
Pod holding one model is an idle replica. A model that is always bursting is
cheaper on a floor.

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
- **The weights were 40 s of 192 in this run.** The 26.7 s read in it was
  helped by the page cache, so it is not a cold baseline; from a cold disk the
  weights cost more.
- **About 60-90 s is process and rank construction** before any weight is
  read. No cache removes it. Only a replica that is already running does: a
  [retained pool](../well-lit-paths/retained-pool/), a pool of
  [groups](../guides/warm-pool/groups.md), or a floor. Check first that host RAM
  can hold a sleeper at all: `deploy/warmpool.sh sizing --params 744B --dtype fp8`.

## Experimental, not built

Nothing here is supported. **Do not plan capacity on these.**

- **Snapshot restore.** Qwen3-32B, TP=1, vLLM v0.26.0, one H200: a sleeping
  engine checkpointed and restored into a new Pod reached `/health` in 15.1 s,
  against 44.0 s for the best prepared start. It needs a privileged Pod, and
  multi-rank engines could not be checkpointed. Measured on an H200 cluster,
  2026-10, median of 3 restores; detail in
  [Anatomy of a replica start](../developer-guide/replica-start-anatomy.md#snapshot-restore-experimental).
- **Reading the weights while the engine starts.** A background reader in the
  engine container fills the page cache during imports. Estimated, not measured:
  up to ~9.9 s on Qwen3-32B, 38-90 s per node on GLM class with one disk
  ([proposal](../proposals/weights-prewarm-on-schedule.md#13-how-much-a-head-start-buys)).
