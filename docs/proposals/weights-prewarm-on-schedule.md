# Reading a model's weights into memory when its pod is scheduled

- **Status:** proposed. Nothing here is built.
- **Problem:** on a prepared node the weights are on local disk, but not in
  memory. The engine's weight load then waits on the disk. Measured on
  Qwen3-32B, the load takes 19.9 s cold and 10.0 s when the files are already
  in the page cache.
- **Decision:** when a pod that mounts a model's node-local weights is bound to
  a node, the weights DaemonSet that `make weights` already runs on that node
  reads the files that pod will need into the page cache. The engine's own load
  then reads from memory. Off by default; turned on with
  `WEIGHTS_PREWARM=on-schedule`.

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
weight files only were dropped from the page cache with
`posix_fadvise(DONTNEED)` and confirmed 0 % resident with `mincore`.

| | weight load | to `/health` | runs |
| --- | ---: | ---: | --- |
| weight files cold | 19.9 s | 53.7 s | 2 |
| weight files read once beforehand | 10.0 s | 43.6 s | 2 |
| weight files warm from an earlier start | 8.9 s | 42.3 s | 2 |

The pre-read itself, measured: one full read of the files took 21.7 s
(about 3.0 GB/s) and left 65.5 GB in the page cache.

The gain is **about 10 s of 44-54 s**, if the read finishes before the engine
starts loading. Is there time? Measured on the same prepared node, from pod
creation to the start of the weight load:

| step | seconds |
| --- | ---: |
| pod created to container started | 2.3 |
| Python and vLLM imports | ~6 |
| engine process spawn | ~7 |
| engine init to first CUDA context | 4.6 |
| **pod creation to weight load** | **~20** |

The read takes ~22 s, so it finishes about as the load starts. Most of the
10 s is reachable; the last files may still be in flight. Before the pod
exists there are 11-23 s more (measured: the KEDA and HPA lag between the
autoscaler's decision and pod creation), but the node is not known then (§2).

### Big model: GLM-5.2-FP8, two 8xH200 nodes

Measured on one 8xH200 node, DP=8 with expert parallelism, compile caches warm:

| | seconds |
| --- | ---: |
| `vllm serve` to ready, real weights | 192 |
| same, `--load-format dummy` (reads no weights) | 152 |
| the weight step in the real run | 26.7 read + 37.0 load |
| process and rank construction before any weight is touched | ~60-90 |

The 26.7 s read is faster than one NVMe can deliver for this model, so the page
cache probably carried part of it. That run is not a cold baseline.

The gain on the two-node group is an **estimate**, derived as follows:

| input | value | source |
| --- | ---: | --- |
| bytes one node needs (half the routed experts, all shared weights) | ~400 GB | estimate; exact from the checkpoint index (§5) |
| local NVMe read rate | 5.2 GB/s | measured with direct I/O, one device, another cluster |
| cold read of one node's share | ~77 s | 400 / 5.2 |
| load once the bytes are in memory | 37 s | measured (the run above) |
| time available before the load starts | 60-90 s | measured (rank construction) |

So a cold weight step of ~77 s or more becomes ~37 s: **an estimated
25-60 s of a 150-250 s start**. The low end is a node with several NVMe
devices striped, where the cold read is already fast. The high end is one
device. The measurement plan in §10 replaces this estimate.

Faster storage alone does not remove this: one rank's load is parse-bound at
about `1.08 GB/s x TP^0.34` (measured), so node-local NVMe is worth ~1.55x
over a shared volume for a 744 GB model, not 5x. Reading ahead hides the disk
behind work the engine does anyway.

## 2. Trigger: a pod bound to this node

Each weights DaemonSet pod watches the pods bound to its own node:

```
GET /api/v1/namespaces/<ns>/pods?watch=1&fieldSelector=spec.nodeName=<this node>
```

`<this node>` comes from the downward API (`spec.nodeName`). The API server
filters, so each watcher sees only its node's pods.

A pod starts a pre-read when all of these hold:

- one of its volumes is the DaemonSet's claim
  (`persistentVolumeClaim.claimName`), or a `hostPath` under the DaemonSet's
  node directory (the benchmark harness mounts the weights that way);
- it is not being deleted (`deletionTimestamp` unset);
- its engine container is not Ready.

The watch reacts to a bind, not to a reason for one. So it covers every way a
pod reaches a node: an autoscaler scale-up, a manual `kubectl scale`, a
container restart (a rising `restartCount` on a not-Ready pod), and a
rescheduled pod after a node drain. After the watcher itself restarts, it lists
the node's pods and handles any not-Ready one it finds.

The autoscaler's decision comes 11-23 s earlier, but no node is chosen yet, so
it would read on every candidate node. A timed re-read holds the model in
memory on every node all the time. §9 has both.

### RBAC

The DaemonSet runs as `weights-downloader` with no token today
(`automountServiceAccountToken: false`). The pre-read needs one:

- a Role in the DaemonSet's namespace: `get`, `list`, `watch` on `pods`;
- a RoleBinding to `weights-downloader`;
- a projected token mounted into the pre-read container only, not the
  downloader.

The claim is namespaced, so the engines that mount it are in the same
namespace. A namespace-scoped Role is enough. A ClusterRole is needed only when
pods in other namespaces read the same node directory through a `hostPath`;
`make weights` does not create that and this proposal does not add it.

## 3. Check what is already in memory first

Before reading, the tool measures how much of each file is resident:

- `cachestat(2)` where the kernel has it (Linux 6.5 and later): resident page
  counts for a file range, without mapping the file;
- otherwise `mmap` plus `mincore(2)` through Python `ctypes`. Mapping a file
  faults nothing in; `mincore` only reports. The check walks the file in 1 GiB
  windows, so its vector stays at 256 KiB.

Both run in the engine image the DaemonSet already uses (it has Python); no
new image. `fincore` (util-linux) makes the same calls and is the operator's
check (§7). `vmtouch` is not in the engine images and is not needed.

Partial residency: the tool reads only the non-resident ranges. Gaps under
8 MiB between them are merged, so it does not issue thousands of small reads.
A file that is 99 % resident or more is skipped whole. When everything is
resident the pre-read ends there and logs `skipped: resident`. This is the
common case for a restart on the same node, and it costs well under a second
(estimate; measured in §10).

## 4. Memory budget

### The page cache is charged to the reader's cgroup

Linux (cgroup v2) charges a page-cache page to the memory cgroup of the task
whose read brought it in. A page that is already cached is not charged again
when another cgroup reads it, and the charge does not move. This holds for
`read()`, for `mmap` faults, and for `posix_fadvise(WILLNEED)` and
`readahead(2)`: readahead runs in the caller's context, so it is charged to the
caller too. **`WILLNEED` is not a way around the charge.** It is also
asynchronous, gives no completion signal and can be dropped under pressure. The
tool uses plain `read()`.

So the pre-read container's memory limit caps how much it can make resident.
At the downloader's 16 GiB limit, reading a 61 GiB model would end with the
container's cgroup reclaiming its own earlier pages to make room for later
ones. Nothing is OOM-killed (clean page cache is reclaimed, not killed for),
but only the last 16 GiB stay. The pre-read would run to completion and leave
the model mostly cold.

**Decision.** The pre-read runs as a second container in the weights
DaemonSet pod, with:

- memory request 256 MiB, like the downloader, so a node reserves nothing;
- memory limit `WEIGHTS_PREWARM_MEMORY`, defaulting to the claim's
  `WEIGHTS_CAPACITY` (1Ti unless set). The capacity is already the operator's
  statement of the model's size. A limit above the node's RAM is allowed and
  reserves nothing; it only stops the cgroup from evicting its own reads.

At run time the tool reads its own `memory.max`. When the bytes it needs exceed
`memory.max` minus 1 GiB it does not start, and logs `skipped: cgroup-limit`.

The engine is then not charged for those pages, so an engine with a tight
memory limit loads more easily, not less. When the pre-read container restarts,
its pages stay cached and usable; the kernel reclaims them like any clean cache.

### The node's free memory

The cgroup limit is not the budget. The node's RAM is. Before reading, and
after every 1 GiB, the tool reads `MemAvailable` from `/proc/meminfo`:

- **headroom** = `WEIGHTS_PREWARM_HEADROOM`, default the larger of 10 % of
  `MemTotal` and 16 GiB;
- start only if `MemAvailable - bytes still to read >= headroom`; otherwise log
  `skipped: memory`;
- stop at once if `MemAvailable` falls below headroom during the read; log
  `stopped: memory`.

The rule is never to read through reclaim. Reclaim takes the least recently
used pages first, and on a node where an engine started a minute ago those may
be its own weights, read once and inactive. A pre-read that forces reclaim can
evict the files of the engine next to it. Stopping at headroom means the
pre-read only ever uses memory that was free.

What this adds in steady state: nothing. The engine's default loader maps the
same files, so a cold start puts the same bytes in the page cache anyway. The
pre-read puts them there ~20 s (small) or ~60-90 s (big) earlier. The extra
cost is a pre-read for a pod that is then deleted before it starts.

## 5. What to read

### One pod, one node: read every file

A single-pod engine (any TP, any DP, on one node) reads the whole checkpoint.
Read all `*.safetensors` files of the model directory, plus the small files
the loader opens first (`config.json`, the tokenizer files, the index).

### A group across nodes: read this node's share

On a LeaderWorkerSet, each node holds some ranks. Reading every file on every
node doubles the disk reads and the page cache on a 2-node expert-parallel
group, and on GLM class (~750 GB) that is the difference between fitting in the
60-90 s window and not (§1). So the tool derives this node's ranks from the
pod:

| fact | where it comes from |
| --- | --- |
| this pod's node index | label `leaderworkerset.sigs.k8s.io/worker-index` (0 is the leader), or env `LWS_WORKER_INDEX` |
| nodes in the group | annotation `leaderworkerset.sigs.k8s.io/size` |
| TP | `--tensor-parallel-size` / `-tp` |
| PP | `--pipeline-parallel-size` / `-pp` |
| DP, DP per node, first DP rank | `--data-parallel-size`, `--data-parallel-size-local`, `--data-parallel-start-rank` (default: node index x DP per node) |
| expert parallel | `--enable-expert-parallel` |
| experts, layers | the model's `config.json` (`n_routed_experts` or `num_experts` or `num_local_experts`; `num_hidden_layers`) |
| tensor to file | `model.safetensors.index.json` (`weight_map`) |

The arguments are read from the engine container's `command` and `args`, after
Kubernetes' own `$(VAR)` expansion against the container's literal `env`. A
value the tool cannot resolve (a shell variable inside `sh -c`, an env from a
ConfigMap) means **read every file**, never a guess. This repository has
already been bitten once by a fingerprint that read unexpanded `$VLLM_*`
variables as values.

Mapping, per tensor name in the index:

- **Routed expert** (`...mlp.experts.<e>....`) with expert parallelism: the EP
  group is `DP x TP` ranks; vLLM's default linear placement gives rank `r`
  experts `[r*E/EP, (r+1)*E/EP)`, remainder to the low ranks. The node needs
  the experts of its own ranks.
- **Layer under pipeline parallelism**: the node needs the layers of its
  stages, from vLLM's even split (or `VLLM_PP_LAYER_PARTITION` when set); the
  embedding goes to the first stage, the final norm and `lm_head` to the last.
- **Everything else** (attention, dense MLP, norms, embeddings without PP):
  every node needs it.

The node reads every file holding at least one tensor it needs. Fall back to
every file when: the index is missing (single-file checkpoint), experts are
stored fused in one tensor, EPLB is on (`--enable-eplb`: placement moves at
run time), a non-default expert placement is set, or any argument above is
unresolved. On the 2-node EP group that fallback reads 2x what the node needs.

**The gate for this section** (open question 1): the mapping is only right if
the engine itself touches only those files. Whether vLLM's loader on an EP rank
reads expert tensors it then discards has not been measured. If it does, every
node reads every file regardless and read-all is the correct pre-read. §10
measures it before shard-aware reading is built.

### Order and pace

- **Order.** vLLM's default loader walks the checkpoint files in sorted name
  order (to be confirmed per version; open question 2). The pre-read uses the
  same order. If the engine starts loading before the pre-read ends, it finds
  the front of the list in memory and the pre-read stays ahead of it.
- **Readers.** `WEIGHTS_PREWARM_READERS` threads, default 4, taking files from
  the front of the ordered list. Python releases the GIL in `read()`. One file
  per thread at a time, read in 16 MiB blocks into a reused buffer, with
  `posix_fadvise(SEQUENTIAL)`.
- **Priority.** `nice 19` for CPU. I/O priority is set to idle with
  `ioprio_set`, but that only has an effect under the BFQ scheduler; NVMe
  devices usually run `none`, where it does nothing. The real control is the
  rate cap.
- **Rate cap.** `WEIGHTS_PREWARM_RATE` in MB/s, default 0 (no cap), applied as
  a token bucket across all readers. With no cap the pre-read can take a whole
  device; set a cap where another tenant's I/O matters more than this start.

## 6. Cancellation and races

| case | behaviour |
| --- | --- |
| pod deleted, or `deletionTimestamp` set | stop between blocks; log `stopped: pod-deleted`. Pages already read stay cached and are reclaimable. |
| pod rescheduled elsewhere | the old node's watcher sees the delete and stops; the new node's watcher sees the bind and starts. |
| engine already reading | both continue. A page being read is locked once in the page cache, so the second reader waits for the same I/O instead of issuing a second one. The engine is parse-bound (1-3 GB/s) below the disk (5 GB/s), so they share bandwidth with little contention. Same order means the pre-read stays ahead. Worst case is the cold case plus the CPU of one reader. |
| engine Ready before the read ends | stop; log `stopped: ready`. |
| several pods of one model on one node (two TP=4 replicas on 8 GPUs) | one pre-read per model per node: the second pod joins the running one. |
| two models on one node | each model's DaemonSet handles its own files. Pre-reads on one node are serialised with `flock` on `<node dir>/.prewarm.lock`, so each budget check sees the other's pages. |
| disk pressure | the pre-read writes nothing. A node under `DiskPressure` is excluded by `weights-status` already, and no engine starts there. |
| memory pressure | §4: stop at headroom. |
| download not finished | no pre-read until the marker exists; the engine would be reading a half-written directory anyway. |

## 7. Configuration and operator experience

Turned on per model, with `make weights`:

```bash
make weights WEIGHTS_MODEL=Qwen/Qwen3-32B WEIGHTS_PATH=/mnt/local/weights/<ns> \
     WEIGHTS_IMAGE=<engine image> NAMESPACE=<ns> WEIGHTS_PREWARM=on-schedule
```

| variable (`deploy/weights.sh` flag) | default | meaning |
| --- | --- | --- |
| `WEIGHTS_PREWARM` (`--prewarm`) | `off` | `on-schedule` adds the pre-read container, Role, RoleBinding and ConfigMap |
| `WEIGHTS_PREWARM_LAYOUT` (`--prewarm-layout`) | `auto` | `auto`: shard-aware when derivable (beta); `all`: always every file |
| `WEIGHTS_PREWARM_MEMORY` (`--prewarm-memory`) | the claim's capacity | the pre-read container's memory limit |
| `WEIGHTS_PREWARM_HEADROOM` (`--prewarm-headroom`) | max(10 % of RAM, 16Gi) | free memory the pre-read never reads into |
| `WEIGHTS_PREWARM_READERS` (`--prewarm-readers`) | 4 | reader threads |
| `WEIGHTS_PREWARM_RATE` (`--prewarm-rate`) | 0 (no cap) | MB/s across all readers |

The tool is one Python file, `deploy/lib/weights_prewarm.py`, shipped in a
ConfigMap the way the downloader's script is shipped in the pod spec. It uses
only the standard library: the watch is a streamed HTTPS GET with the projected
token and the cluster CA. No Kubernetes client library is needed in the engine
image.

**Logs.** One line per event, key=value, from the `prewarm` container:

```
prewarm start pod=<ns>/<pod> layout=all files=25 bytes=65.5G resident_before=0.0%
prewarm done  pod=<ns>/<pod> read=65.5G seconds=21.7 rate=3.0GB/s resident_after=100.0%
prewarm skip  pod=<ns>/<pod> reason=resident|memory|cgroup-limit|not-downloaded
prewarm stop  pod=<ns>/<pod> reason=pod-deleted|ready|memory read=31.2G
```

**Metrics** (beta), on a port of the `prewarm` container, prefix
`wva_weights_prewarm_`: `bytes_read_total`, `seconds` (histogram),
`resident_ratio{phase="before|after"}` (last value per model),
`runs_total{result="done|skipped|stopped",reason}`. Labels: model and node
only, no pod names.

**How an operator checks it worked:**

1. `make weights-status NAMESPACE=<ns>` prints the last `prewarm` line per node
   next to the download state.
2. The engine's own `Model loading took` line: with the pre-read done, it
   should be near the warm figure (10 s for Qwen3-32B, not 20 s).
3. On the node, from a debug pod: `fincore <dir>/models/<id>/*.safetensors`
   shows the resident bytes per file.

## 8. Security and multi-tenancy

- **Files.** The pre-read reads only the model directory of the claim its
  DaemonSet already mounts, read-only. It does not open any path a pod spec
  names; the directory is fixed at apply time.
- **New privilege.** `list`/`watch` on pods in one namespace. Pod specs can
  carry plain `env` values, so this lets the ServiceAccount read them. Any pod
  in the namespace may name `weights-downloader` (the same caveat
  [workload-preparation](../reference/workload-preparation.md#weights-on-the-nodes-disk)
  states for an SCC grant), so anyone with `pods/create` there gains pod read.
  The token is mounted only into the `prewarm` container. Off by default for
  this reason as well.
- **Shared RAM.** The page cache is node memory every tenant uses. The pre-read
  only fills memory that is free beyond the headroom (§4), stops before reclaim
  starts, and adds nothing in steady state beyond what the engine's own load
  puts there. Pages it reads are clean and reclaimable; another tenant needing
  memory gets it back, at the cost of this model's warmth.
- **Disk.** A full-speed read can take a whole NVMe device for 20 s (small) or
  over a minute (big). On a node shared with I/O-sensitive tenants, set
  `WEIGHTS_PREWARM_RATE`.
- **OpenShift.** Same SELinux situation as the downloader. Not run there.

## 9. Alternatives considered

| alternative | why not |
| --- | --- |
| **Timed re-read** (touch the files every N minutes) | Holds the model in page cache on every prepared node all the time: 65 GB per node for a 32B model, ~400-750 GB for GLM class. The kernel drops it under pressure, which is when a scale-up is likely. |
| **Trigger on the autoscaler's decision** | 11-23 s earlier (measured), but the node is unknown. Reading on all candidate nodes multiplies reads and page cache by the candidate count; on 16 prepared nodes a 32B scale-up of one would read ~1 TB. It also misses manual scale, restarts and rescheduling. |
| **A pod holding the weights in RAM** (`mlock`, `vmtouch -l`) | Unevictable memory equal to the model per node per model, charged to a pod, forever. That is a warm pool without the GPU part, and a worse trade. |
| **Prepull-style permanent residency** | The same cost as the timed re-read, with a DaemonSet that must notice when the kernel drops the files. |
| **Peer weight transfer** | Complementary. A replica filled from a serving peer over NCCL reads no files at all (measured byte-identical, [§4e](warm-pool-weight-transfer.md#4e-it-works-on-a-plain-scale-up-too-with-no-pool-at-all)). It needs a running peer of the same layout and changes to the engine; the pre-read needs neither. Where transfer is used, the pre-read is not needed for that replica. |
| **Snapshot of a loaded engine** | Measured: restores in ~15 s, but only at TP=1 today. Where it applies it removes much more than the read; the pre-read covers the multi-GPU and multi-node cases it does not. |
| **Warm pool** | Removes the whole start, at the cost of an idle engine. The pre-read is for the starts the pool does not cover. |

## 10. Measurement plan before building past alpha

Cold always means: weight files dropped with `posix_fadvise(DONTNEED)` and
checked 0 % resident with `mincore`. Not `drop_caches`, which affects every
tenant and needs privilege.

**A. Qwen3-32B, TP=1, one H200, prepared node.** Three starts per arm, the
same node:

| arm | what |
| --- | --- |
| cold | trigger off, files cold |
| on-schedule | trigger on, files cold |
| warm | trigger off, files warm from an earlier start |

Record per start: pod-created to `/health`, the engine's weight-load seconds,
the pre-read's own seconds and whether it finished before the load began,
`MemAvailable` before and after, disk read throughput (`/proc/diskstats`).

**B. GLM class on two 8xH200 nodes, LeaderWorkerSet, DP=16 / EP.**

1. Access pattern first (gates §5): one cold start with the trigger off, then
   `mincore` on every file on both nodes. If each node has every expert file
   resident, the engine reads everything and shard-aware reading is dropped.
2. Three arms, two starts each: cold; read-all pre-read; shard-aware pre-read.
   Record per node: ready time, weight step, bytes pre-read, peak page cache
   (`Cached`), `MemAvailable` minimum, disk throughput.

**Acceptance (alpha to beta):**

- A: on-schedule weight load within 2 s of the warm arm's median, and `/health`
  at least 7 s faster than cold, on all three starts.
- A: a start with files already resident logs `skipped: resident` within 2 s.
- No run takes `MemAvailable` below the configured headroom.

**Acceptance (beta to GA):**

- B: shard-aware gains at least 20 s of ready time over cold, and reads at most
  60 % of read-all's bytes per node; or, if step 1 shows the engine reads
  everything, read-all gains at least 20 s.
- An e2e check on kind (no GPU): small stand-in safetensors files, a pod
  mounting the claim, residency asserted after the bind.

## 11. Rollout and open questions

| stage | scope |
| --- | --- |
| **alpha** | `WEIGHTS_PREWARM=on-schedule`; single-pod engines; `layout=all` only; residency check; memory budget; logs; `weights-status` line. Measurement A. |
| **beta** | shard-aware layout for LeaderWorkerSet groups, behind measurement B step 1; metrics; rate cap. Measurement B. |
| **GA** | both measurements met; the kind e2e in CI; the user-facing section in [workload-preparation](../reference/workload-preparation.md#weights-on-the-nodes-disk) and `make workload-patch` naming the option when it finds node-local weights without it. Still off by default. |

Open questions:

1. **Does an EP rank read only its own experts?** If vLLM's loader touches
   every tensor and discards the ones it does not own, the engine reads every
   file on every node, and shard-aware pre-reading would leave the rest cold.
   §10 B step 1 answers it.
2. **The loader's file order.** Sorted by name in the versions read so far;
   to be confirmed for each vLLM version in use, and for SGLang.
3. **Charges after the container restarts.** Pages stay usable either way;
   whether their charge moves to the parent cgroup depends on the kernel.
   Record the kernel version in §10.
4. **Non-default expert placement and EPLB.** Read-all for now. Revisit if a
   deployment relies on them.
5. **The GLM gain itself.** §1's 25-60 s is an estimate from one node's run
   with a probably part-warm cache. Measurement B replaces it.
