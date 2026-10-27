# Anatomy of a replica start

Where the seconds of a vLLM start go, phase by phase, and what was measured and
ruled out. The operator's comparison of options is
[Replica start time](../reference/start-time.md); this page is the detail behind it.

Measured on an H200 cluster, 2026-10: Qwen3-32B bf16 (61 GiB), one H200,
`--max-model-len 8192 --gpu-memory-utilization 0.90`, vLLM v0.26.0, in-pod
timestamps, median of 2 to 3 starts after one start that fills the caches.

## Phases of the 59.8 s prepared start

"Prepared" is `make prepull`, `make weights` on the node's disk and a seeded
`make engine-cache`, with the weight files already in the page cache.
"Unprepared" is weights on a network ReadWriteMany claim and no engine cache.
Seconds:

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

- About 27.5 s of the prepared start happens before the engine creates its
  first CUDA context.
- `VLLM_CACHE_ROOT` also caches the API server's architecture check: 11.4 s to
  0.0 s.
- The bytecode cache (`PYTHONPYCACHEPREFIX`) takes the imports from 13.4 to
  6.2 s and the engine init from 4.6 to 2.3 s.
- The one-file AOT compile (`VLLM_USE_MEGA_AOT_ARTIFACT=1`) takes torch.compile
  from 5.1 to 0.8 s. On a hit vLLM 0.26 otherwise re-splits the graph and loads
  each piece; the log's "Dynamo bytecode transform" line there is deserialize
  and source verification, not a trace.
- On v0.32.0 the memory profile phase is 9.2-9.4 s: Model Runner V2 captures
  every CUDA graph once to measure memory (`profile_cudagraph_memory` in
  `gpu_worker.py`). `--kv-cache-memory-bytes` skips it (to 1.0 s).
- Weight load from the page cache vs from disk, v0.32.0: 10.0 s vs 19.9 s.

## Not levers

Measured, and not worth chasing:

- **The `ll_bf16` kernel warmup** (~7.5 s on v0.26.0). vLLM compiles CuTe-DSL
  router GEMMs for DeepSeek-style shapes on any SM 90+ GPU, whatever the model.
  `cute.compile` is called with `no_cache=True`, so `CUTE_DSL_CACHE_DIR` is never
  read or written. From v0.27 the warmup runs only for models with a bf16 MoE
  router (`_ll_bf16_router_shapes_from_model` in
  `vllm/model_executor/warmup/kernel_warmup.py`).
- **Trimming CUDA-graph capture sizes.** It changes how the engine serves. On
  the 32B the default sizes already stop at 512, below `max_num_seqs` (1024 on
  an H200); on GLM-5.2 capture is ~3 s.
- **Running the engine in one process.** It saves the engine spawn and changes
  how the engine serves.
- **The older model runner on v0.32.0** (`VLLM_USE_V2_MODEL_RUNNER=0`). 40.4 s,
  but a different runner with a different KV size. `--kv-cache-memory-bytes`
  gets 42.3 s with the same serving.
- **FlashInfer autotune.** Its table is already under `VLLM_CACHE_ROOT`. On
  Qwen3-32B (FLASH_ATTN backend) it finds nothing to tune; at world size above 1
  vLLM tunes every start and keeps no file.

## Snapshot restore (experimental)

Not built and not supported. Qwen3-32B, TP=1, vLLM v0.26.0, one H200.

The source engine starts with sleep mode, is put to sleep at level 2 (GPU
memory 133 GB to 2.7 GB), locked and checkpointed with `cuda-checkpoint`, and
dumped with CRIU: a 6.2 GB image in about 5 s. A new Pod restores it, wakes it,
and reloads the weights.

| step | seconds |
| --- | ---: |
| container start | 2.3 |
| unpack `/tmp`, `/dev/shm`, `/root/.cache` | 0.3 |
| CRIU restore | 2.8 |
| CUDA restore and unlock | 1.5 |
| wake | 0.5 |
| reload weights | 7.9 |
| **to `/health`** | **15.1** (16.1 with the image out of page cache) |

- Against 59.8 s prepared and 44.0 s for the best prepared start. Output was
  byte-identical; restores worked onto another GPU index.
- The source start pays for it: sleep mode adds ~2 s, and changes the compile
  cache key, so the first sleep-mode start on a node misses (~75 s).
- A full snapshot with the weights in it (140 GB) restored in 76-78 s, slower
  than a cold start.
- It needs a privileged Pod, CRIU and `cuda-checkpoint` (not in the v0.26.0
  image), and environment set at the original start: `UV_USE_IO_URING=0`,
  `USE_LIBUV=0`, loopback for vLLM, Gloo and NCCL sockets, usage stats off.
  `--device-map` needed a full permutation of the node's GPUs.
- Multi-rank engines could not be checkpointed (GLM-5.2, 2026-09: DP/EP hung,
  TP=8 errored).
