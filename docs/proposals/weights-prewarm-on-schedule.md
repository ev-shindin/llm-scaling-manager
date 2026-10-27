# Reading a model's weights into memory while its engine starts

- **Status:** proposed, second revision after review (PR #362). Nothing here
  is built.
- **Problem:** on a prepared node the weights are on local disk, but not in
  memory. The engine's weight load then waits on the disk. Measured on
  Qwen3-32B, the load takes 19.9 s cold and 10.0 s when the files are already
  in the page cache.
- **Decision:** a background reader, started by the engine container's own
  command just before it `exec`s the engine, reads the pod's weight files into
  the page cache while the engine imports and initialises. Same container, same
  UID, same memory cgroup. It reads every file, because the engine does. Off by
  default.

This builds on [Weights on the node's disk](../reference/workload-preparation.md#weights-on-the-nodes-disk).
It does not replace the [warm pool](warm-pool-lws.md) or
[peer weight transfer](warm-pool-weight-transfer.md); §9 says how they relate.

## 1. The problem, and what it is worth

A replica on a prepared node still reads every byte from disk, unless an
earlier start left the files in memory. So the same pod spec starts in 44 s on
one node and 54 s on another, and nothing in the spec says which.

### Small model: Qwen3-32B, one GPU

Measured: Qwen3-32B bf16 (61 GiB on disk), one H200, vLLM v0.32, prepared node
(image held, weights on local disk, engine caches warm). "Cold" means the
weight files only were dropped with `posix_fadvise(DONTNEED)` and checked 0 %
resident with `mincore`.

| | weight load | to `/health` | runs |
| --- | ---: | ---: | --- |
| weight files cold | 19.9 s | 53.7 s | 2 |
| weight files read once beforehand | 10.0 s | 43.6 s | 2 |
| weight files warm from an earlier start | 8.9 s | 42.3 s | 2 |

Also measured: one buffered read of all the files took 21.7 s (~3.0 GB/s) and
left 65.5 GB in the page cache. The cold engine load moves the same 65.5 GB in
19.9 s (~3.3 GB/s), so **the cold load is disk-bound**.

From the same kind of node, the head start a reader gets if it starts with the
container:

| step | seconds |
| --- | ---: |
| pod created to container started | 2.3 |
| Python and vLLM imports | ~6 |
| engine process spawn | ~7 |
| engine init to first CUDA context | 4.6 |
| **container start to weight load** | **~18** |

### Big model: GLM-5.2-FP8

Measured on **one** 8xH200 node, DP=8 with expert parallelism, compile caches
warm. No two-node group has been measured.

| | seconds |
| --- | ---: |
| `vllm serve` to ready, real weights | 192 |
| same, `--load-format dummy` (reads no weights) | 152 |
| the weight step in the real run | 26.7 read + 37.0 load |
| process and rank construction before any weight is touched | ~60-90 |

The 26.7 s read is faster than one NVMe can deliver ~744 GB, so the page cache
probably carried part of it. It is not a cold baseline.

### 1.3 How much a head start buys

Symbols: `B` bytes to read; `R` the engine's cold rate; `W = B / T_warm` the
engine's rate from memory; `C` the reader's rate; `H` the head start;
`P = H x C` the bytes read before the engine starts loading.

From `H` on, the engine consumes warm pages at `W` while the reader keeps
adding at `C`. The engine reaches the reader's front after `P / (W - C)`
seconds, having read `P x W / (W - C)` bytes from memory instead of disk. So:

```
gain = P x (W - R) / (R x (W - C)),   at most T_cold - T_warm
```

| case | B | R | W | C | H | gain |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Qwen3-32B, no cap (default) | 65.5 GB | 3.3 | 6.55 | 3.0 | 18 s | 15.0 s, capped: **9.9 s** |
| Qwen3-32B, cap 2 GB/s | 65.5 GB | 3.3 | 6.55 | 2.0 | 18 s | **7.8 s** |
| GLM class per node, one disk, no cap | 744 GB | 3.0 | 20.1 | 3.0 | 60-90 s | **60-90 s** |
| GLM class, cap 2 GB/s | 744 GB | 3.0 | 20.1 | 2.0 | 60-90 s | **38-57 s** |
| GLM class, striped NVMe | 744 GB | ~20 | 20.1 | ~20 | 60-90 s | **~0** (`T_cold ~ T_warm`) |

Rates in GB/s. Every gain is an **estimate** from the formula. Qwen3-32B
inputs are the measurements above; the full 9.9 s needs `C` of about
2.4 GB/s at `H` = 18 s. GLM: `B` is the whole checkpoint, because every
expert-parallel rank reads every file (§5); `R` and `C` are the Qwen3-32B
buffered rates, assumed for the same class of disk; `T_warm` is the measured
37 s; `H` the measured rank construction; the cap `T_cold - T_warm`
(~248 - 37 s) does not bind. 5.2 GB/s, measured elsewhere for one local NVMe,
is a direct-I/O figure and is not used. Measurement B (§10) replaces the GLM
rows.

## 2. Mechanism: a background reader in the engine container

`make workload-patch` already emits a guard for the engine-cache variables,
to go in the engine container's command before the engine starts. The command
belongs to the model server's chart, so the guard is emitted, not patched live.
The reader is three more lines in the same place:

```sh
if [ "${WVA_WEIGHTS_PREWARM:-off}" = "on" ]; then
  ( nice -n 19 python3 -c "<reader, emitted by make workload-patch>" "$MODEL_DIR" & )
fi
exec vllm serve "$MODEL_DIR" ...
```

- **No new object.** No container, ConfigMap, probe, token or RBAC. The pod's
  QoS class, requests and limits do not change.
- **Same memory cgroup.** The pages are charged to the engine container,
  which would have been charged for them a few seconds later anyway (§4).
- **Same UID.** The reader can read exactly what the engine can, and its
  residency check is as truthful as it can be for that UID (§3).
- **Python** is in the vLLM and SGLang images. The reader is stdlib only. It
  contains no `$(`: the kubelet expands `$(VAR)` in a command.
- **The switch is an env var.** `WVA_WEIGHTS_PREWARM=on` is env, which `make
  workload-patch` can apply live; the lines in the command do nothing without
  it.

### Process lifetime

- `( ... & )` forks the reader from a subshell that exits at once, and the
  shell reaps the subshell. The reader is orphaned and reparented to the PID
  namespace's init: the shell, which then `exec`s and becomes the engine, so
  the engine is the reader's parent. (`pid_namespaces(7)`: an orphan "is
  reparented to the init process in the PID namespace of its parent".)
- **It never delays termination.** The kubelet signals PID 1, the engine. When
  PID 1 exits, "the kernel terminates all of the processes in the namespace via
  a SIGKILL signal" (`pid_namespaces(7)`). The reader also exits on its own
  `SIGTERM`. vLLM's shutdown kills its own process tree, which includes the
  reader; that is harmless.
- **It exits after the read.** Nothing lingers but one zombie entry: Python
  does not reap children it did not start, so the reader's exit status stays
  in the process table until the container ends. It holds no memory or CPU.
  Measurement A counts `<defunct>` entries to confirm there is exactly one.
  `setsid` or a double fork would not avoid it: every orphan in the container
  ends up under the same PID 1.
- **CPU.** One reader thread, `nice 19`, competing a little with the engine's
  imports. On a static CPU-manager pod it runs on the engine's pinned CPUs.

## 3. Check what is already in memory, and when to trust it

Before reading, the reader measures residency with `cachestat(2)` (Linux 6.5
and later), else `mmap` plus `mincore(2)` through `ctypes`. Neither tells the
truth to every caller:

- `mm/filemap.c` `can_do_cachestat()`: true if the file is open for write, or
  `file_owner_or_capable()` (owner, or `CAP_FOWNER` over the inode), or write
  permission. Otherwise `cachestat` returns `EPERM`.
- `mm/mincore.c` `can_do_mincore()`: the same owner-or-write test. Otherwise
  `mincore_vma()` does `memset(vec, 1, pages)`: **every page reported
  resident**. (Kernels from before that rule reported only pages mapped by the
  caller.)

On Kubernetes the downloader and the engine both run as root, so the engine
owns the files and the check is truthful. The reader tests the rule first
(`st_uid == geteuid()` or `os.access(W_OK)`). **If it cannot trust the check,
it fails closed:** it treats every file as cold and reads it all. Rereading a
cached page costs CPU, not disk.

It reads only non-resident ranges (gaps under 8 MiB merged) and skips a file
99 % resident or more. If everything is resident it logs `skip
reason=resident` and exits.

## 4. Memory

Sources: the kernel's [cgroup v2 documentation](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-ownership),
Kubernetes [node-pressure eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/)
and [pod QoS](https://kubernetes.io/docs/concepts/workloads/pods/pod-qos/).

- **Charge.** "A memory area is charged to the cgroup which instantiated it
  and stays charged to the cgroup until the area is released." The reader is a
  process in the engine container, so the engine container is charged, as in a
  cold start. `posix_fadvise(WILLNEED)` would be charged the same way; the
  reader uses plain `read()` so it can see progress.
- **Working set.** The kubelet's working set excludes `inactive_file` only. A
  page read once sits on the inactive list; when the engine maps and uses it,
  it may be activated, and then counts. That is where a cold start puts the
  same pages, in the same container. Eviction ranks pods by whether usage
  exceeds requests, then priority, then usage relative to requests, so the
  engine pod's exposure is unchanged.
- **A container limit.** A Guaranteed engine pod has a memory limit. If the
  model and the engine's own memory do not fit under it, a full pre-read would
  reclaim its own earlier pages before the engine reaches them. So the reader
  also reads `memory.max` and `memory.current` and never reads more than the
  room left under the limit minus 2 GiB; it reads the front of the file order
  up to that point. A cold load in the same container has the same limit.

### The node's budget

The budget is **`MemAvailable` minus a headroom** (the larger of 5 % of
`MemTotal` and 16 GiB). Not `MemFree`: once anything is cached Linux keeps
`MemFree` near the low watermark, so a `MemFree` test would skip on every
busy node.

The argument for counting reclaimable cache as available: in read-all mode
the reader reads exactly the bytes the engine's cold load reads seconds later.
Whatever cache it evicts, the engine would have evicted too. It never reads a
byte the engine will not read. It does not start when `need` (the non-resident
bytes) exceeds the budget, and re-checks every 1 GiB, stopping below the
headroom (`stop reason=memory`). When the pod terminates, the reader is
killed with the engine (§2).

On a busy node whose page cache is full of other files, the budget still
allows the read, because that cache is reclaimable. Measurement A runs on such
a node.

## 5. What to read

### Every file, by default

Checked in the vLLM v0.26.0 and v0.32.0 sources
(`vllm/model_executor/model_loader/`):

- `weight_utils.py` `safetensors_weights_iterator` (v0.26.0 line 840, v0.32.0
  line 1079) sorts the files with `_natural_sort_key`, opens each with
  `safe_open` and calls `get_tensor` on every key that `should_skip_weight`
  does not skip.
- `default_loader.py` `_init_ep_weight_filter` (v0.26.0 line 351, v0.32.0
  line 421) sets the local expert ids only for an MoE model with
  `enable_expert_parallel` **and** `enable_ep_weight_filter`, and returns early
  when `enable_eplb` is set. `enable_ep_weight_filter` defaults to `False`
  (`vllm/config/parallel.py`).

So by default every rank reads every file, expert-parallel or not, and
**read-all is the correct default** for one pod and for each pod of a
LeaderWorkerSet group. The reader reads every `*.safetensors` file in the same
natural-sorted order, so it stays ahead of the engine, plus the small files
read first (`config.json`, tokenizer files, the index).

### This node's share, only when the engine filters (beta)

With `--enable-ep-weight-filter`, EPLB off and the default loader, the reader
mirrors `ep_weight_filter.py` exactly:

- `ep_size = dp_size x pcp_size x tp_size`;
  `ep_rank = dp_rank x pcp_size x tp_size + pcp_rank x tp_size + tp_rank`, for
  every rank on this node (the node's DP ranks from
  `--data-parallel-start-rank` and `--data-parallel-size-local`, or the
  LeaderWorkerSet worker index times the local size);
- local experts by linear placement (`compute_local_expert_ids`);
- `should_skip_weight`: skip a tensor only if its name has `.experts.<id>.`,
  the id is not local to **any** of this node's ranks, and it ends in `.weight`
  (v0.26.0) or `.weight` / `.weight_packed` (v0.32.0). Scale and metadata
  tensors of every expert are still read; so is a fused expert tensor with no
  id.

It reads those tensors' **byte ranges**, from each file's safetensors header,
not whole files.

Read-all regardless, because the engine reads whole files or ignores the
filter: `--expert-placement-strategy round_robin` (until mirrored),
`--load-format fastsafetensors` (called without `local_expert_ids`),
`--model-loader-extra-config '{"enable_multithread_load": true}'` (also called
without them), `--safetensors-load-strategy eager` (reads each whole file with
`f.read()` before filtering) and `prefetch` (`_prefetch_all_checkpoints` reads
whole files, split across ranks). Also read-all for an argument the reader
cannot resolve (a shell variable, an env from a ConfigMap): never a guess.

### Pace

- One reader, 16 MiB blocks into a reused buffer, `posix_fadvise(SEQUENTIAL)`.
- **No rate cap by default.** The previous revision defaulted to 2 GB/s, to
  leave disk for others. That did not hold: during the head start the engine
  is not reading the disk, so a cap only shrinks `P` (7.8 s instead of 9.9 s on
  Qwen3-32B); once the engine reaches the front, both are disk-bound whatever
  the cap. `WVA_WEIGHTS_PREWARM_RATE` (MB/s) stays as an option for a device
  shared with I/O-sensitive tenants. `ionice` does nothing on NVMe under the
  `none` or `mq-deadline` schedulers, so it is not used.

### The engine catching up

When the engine reaches the reader's front, both read the same pages. A
second reader of a folio being read waits on its lock, so each byte should
come from disk once and the overlap be no worse than cold. That follows from
how the page cache works and is **not measured**; measurement A checks it.

## 6. Races and concurrency

| case | behaviour |
| --- | --- |
| pod deleted or rescheduled | the reader dies with the container; pages stay cached and reclaimable. |
| container restart | the command runs again, so the reader runs again; it finds the files resident and exits. |
| engine Ready before the read ends | in read-all the engine has read every byte by then, so what is left is cached and the reader finishes from memory. |
| two pods of one model start on one node | both read. The first charges the pages; the second mostly hits cache and costs CPU. A page is charged to whichever container read it first, as today. |
| several models start on one node | each reader takes part of the device (no cap), so each `P` shrinks and each gain is smaller. No start is slower than cold, since the engines would contend for the same device moments later. |
| download not finished | `skip reason=not-downloaded` (the marker is missing). |

## 7. Configuration and operator experience

```bash
make workload-patch NAMESPACE=<ns> WVA_WORKLOAD_PATCH_APPLY=true \
     WVA_WORKLOAD_PATCH_APPLY_PREWARM=true
```

- `make workload-patch` emits the reader lines beside the engine-cache guard,
  for the chart's command (leader and worker on a LeaderWorkerSet), and offers
  it only where the weights volume is a claim made by `make weights`.
- `WVA_WORKLOAD_PATCH_APPLY_PREWARM=true` applies `WVA_WEIGHTS_PREWARM=on` live.
  Applying rolls the workload, like any pod-template change.
- Options, as env on the engine container: `WVA_WEIGHTS_PREWARM_RATE` (MB/s,
  unset = no cap), `WVA_WEIGHTS_PREWARM_HEADROOM` (default max(5 % RAM,
  16Gi)), `WVA_WEIGHTS_PREWARM_LAYOUT` (`all`; `auto` from beta).
- **Removing it:** set `WVA_WEIGHTS_PREWARM=off`, or drop the env. The lines
  in the command then do nothing; remove them at the next chart change.
- **Drift.** A live env patch is reverted by the next `helm upgrade`, and
  Argo CD or Flux see it as drift and may revert it too. Put both the lines and
  the env in the chart's values to keep them.

**Logs only**, on the engine container's output, one key=value line per
event:

```
prewarm start layout=all files=25 bytes=65.5G resident_before=0.0% memavailable=1.7T trusted=yes
prewarm done  read=65.5G seconds=21.7 rate=3.0GB/s resident_after=100.0% cgroup_memory=66.1G
prewarm skip  reason=resident|memory|cgroup-limit|not-downloaded
prewarm stop  reason=memory read=31.2G
```

No metrics: a port on the engine container would need a scrape object, and
the engine's own port is the chart's. A metric is a GA question.

**How an operator checks it worked:** the `done` line comes before the
engine's `Model loading took` line, and that line is near the warm figure (10 s
for Qwen3-32B, not 20 s). `fincore` on the weight files, as root, shows the
residency.

**OpenShift: out of scope for alpha.** The reader inherits the engine's
`securityContext`, so admission is unchanged. Unverified: whether the
project's UID owns the files (if not, the reader fails closed and reads
everything).

## 8. Security and multi-tenancy

- **Files and privilege.** The reader reads the engine's own weights volume as
  the engine's UID. No new object, privilege or capability.
- **Shared RAM.** In read-all mode it adds no page cache beyond what the
  engine's load creates, in the same container; it creates it sooner.
- **Disk.** Unthrottled, one start takes the device for the length of one
  cold read, as the engine's load does. `WVA_WEIGHTS_PREWARM_RATE` is the knob
  for a device shared with I/O-sensitive tenants.

## 9. Alternatives considered

| alternative | why not |
| --- | --- |
| **A native sidecar in the engine pod** (the previous revision) | A sidecar with no memory limit removes the pod-level limit and turns a Guaranteed pod Burstable: a different OOM score, eviction standing and no static CPU-manager pinning. Request equal to limit keeps Guaranteed but reserves the model's size. A namespace CPU quota rejects a container without CPU settings. A missing ConfigMap is `CreateContainerConfigError` for the whole pod. A sidecar's readiness gates pod Ready, and a PID 1 that ignores `SIGTERM` delays deletion. A pod-level `spec.resources` limit below engine plus model makes it reclaim its own pages. |
| **The weights DaemonSet watching pods bound to its node** | Needs a token and a Role to watch pods in a DaemonSet that has neither. Its page cache is charged to a 256 MiB-request pod, the first eviction target under pressure, and the limit it needs collides with quotas, LimitRanges and OpenShift overrides. |
| **Timed re-read** | Holds the model in page cache on every prepared node all the time (65 GB per node for 32B, ~744 GB for GLM class); dropped under pressure, when a scale-up is likely. |
| **Trigger on the autoscaler's decision** | 11-23 s earlier (measured KEDA/HPA lag), but the node is unknown; reading on 16 candidates for one 32B replica is ~1 TB. |
| **A pod holding the weights in RAM** (`mlock`) | Unevictable memory equal to the model, per node per model, forever. |
| **Peer weight transfer** | Complementary: a replica filled from a serving peer over NCCL reads no files ([§4e](warm-pool-weight-transfer.md#4e-it-works-on-a-plain-scale-up-too-with-no-pool-at-all), measured byte-identical). Needs a same-layout peer and engine changes. |
| **Snapshot of a loaded engine** | Measured: restores in ~15 s, TP=1 only today. Where it applies it removes much more than the read. |
| **vLLM's own `--safetensors-load-strategy prefetch`** | Reads whole files into page cache, but inside the load, after the head start; on node-local NVMe the [reference](../reference/workload-preparation.md#weights-on-the-nodes-disk) says it buys nothing. |
| **Warm pool** | Removes the whole start, at the cost of an idle engine. |

## 10. Measurement plan before building past alpha

Cold means: weight files dropped with `posix_fadvise(DONTNEED)`, checked 0 %
resident as root. Not `drop_caches`, which affects every tenant.

**A. Qwen3-32B, TP=1, one H200, prepared node whose page cache is already
full of other files** (fill it with an unrelated read first). Three starts per
arm: cold (reader off); reader on, files cold; reader on with
`WVA_WEIGHTS_PREWARM_RATE=2000`; warm (reader off, files warm). Record per
start:

- pod created to `/health`; the engine's weight-load seconds;
- the reader's `done` line, and the time the engine began loading;
- `MemAvailable` and `MemFree` before and after; `/proc/diskstats`;
- `container_memory_working_set_bytes` for the engine container and pod,
  every 2 s from start to 60 s after `/health`;
- `<defunct>` entries in the container after the read.

**B. GLM class, two 8xH200 nodes, LeaderWorkerSet, DP=16 / EP.** First measure
`R` on the node (a buffered read of one cold weight file). Then two arms, two
starts each: cold; reader on, read-all. Record per node what A records, plus
peak `Cached`. Confirm `--enable-ep-weight-filter` is absent and, after the
cold start, every file resident on both nodes.

**Acceptance (alpha to beta):**

- A: reader-on weight load within 2 s of the warm arm's median, and `/health`
  at least 7 s faster than cold, on all three starts.
- A: the capped arm's gain within 30 % of the formula's 7.8 s.
- A: no reader-on start slower than the slowest cold start (overlap no worse).
- A: a start with files resident logs `skip reason=resident` within 2 s.
- A: no change in the pod's QoS class, requests or limits; one `<defunct>` at most.

**Acceptance (beta to GA):** B: a gain of at least 20 s, and within 30 % of
§1.3's prediction for the measured `R`. A kind e2e (no GPU): stand-in
safetensors files, the command lines, residency asserted after start.

## 11. Rollout and open questions

| stage | scope |
| --- | --- |
| **alpha** | the emitted lines and the env switch; Deployments and LeaderWorkerSets; read-all only; residency check failing closed; `MemAvailable` and cgroup budget; logs. Measurement A. |
| **beta** | the share layout for `--enable-ep-weight-filter`; measurement B. |
| **GA** | both met; the kind e2e in CI; the user-facing section in [workload-preparation](../reference/workload-preparation.md#weights-on-the-nodes-disk). Still off by default. |

Open questions:

1. **The disk rate on GLM-class nodes.** If they stripe several devices, §1.3
   predicts almost no gain there.
2. **SGLang.** Its loader's file order and whether it reads every file are not
   checked. Alpha is vLLM only.
3. **Python and zombies.** That vLLM's API server does not reap the reader is
   expected from how CPython handles children, not checked; measurement A
   checks it.
