# Reading a model's weights into memory while its engine starts

- **Status:** proposed, revised after review (PR #362). Nothing here is built.
- **Problem:** on a prepared node the weights are on local disk, but not in
  memory. The engine's weight load then waits on the disk. Measured on
  Qwen3-32B, the load takes 19.9 s cold and 10.0 s when the files are already
  in the page cache.
- **Decision:** a native sidecar in the engine pod reads that pod's weight
  files into the page cache while the engine imports and initialises. By
  default it reads every file, because that is what the engine reads. `make
  workload-patch` adds it, for Deployments and both LeaderWorkerSet templates.
  Off by default.

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

Measured: one full buffered read of the files took 21.7 s (~3.0 GB/s) and left
65.5 GB in the page cache. The cold engine load reads the same 65.5 GB in
19.9 s, ~3.3 GB/s: **the cold load is disk-bound**, at the same rate as a
plain read.

Measured on the same kind of node, from pod creation:

| step | seconds |
| --- | ---: |
| pod created to container started | 2.3 |
| Python and vLLM imports | ~6 |
| engine process spawn | ~7 |
| engine init to first CUDA context | 4.6 |
| **container start to weight load** | **~18** |

A sidecar starts with the pod, so it has a head start of about 18 s. §1.3
shows that is enough for the whole ~10 s.

### Big model: GLM-5.2-FP8

Measured on **one** 8xH200 node, DP=8 with expert parallelism, compile caches
warm. No two-node group has been measured.

| | seconds |
| --- | ---: |
| `vllm serve` to ready, real weights | 192 |
| same, `--load-format dummy` (reads no weights) | 152 |
| the weight step in the real run | 26.7 read + 37.0 load |
| process and rank construction before any weight is touched | ~60-90 |

The 26.7 s read is faster than one NVMe can deliver for ~744 GB, so the page
cache probably carried part of it. It is not a cold baseline.

### 1.3 How much a head start buys

Let `B` be the bytes to read, `R` the disk's buffered read rate, `H` the head
start, `T_warm` the load time with every byte in memory, and `C` the
sidecar's rate cap (`C <= R`). Both readers walk the files in the same order
(§5), so the engine reads warm pages until it reaches the sidecar's front,
then both move at disk speed. Then:

```
T_cold = B / R                       (the load is disk-bound)
gain   = min(H * C / R, T_cold - T_warm)
```

The gain is the head start, scaled down by the cap, and never more than the
whole disk term.

| case | B | R | C | H | T_cold | T_warm | gain |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Qwen3-32B, measured inputs | 65.5 GB | 3.3 GB/s | 2.0 GB/s | 18 s | 19.9 s | 10.0 s | **9.9 s** (estimate) |
| GLM class, one disk, per node | 744 GB | 3.0 GB/s | 2.0 GB/s | 60-90 s | ~248 s | 37 s | **40-60 s** (estimate) |
| GLM class, same, no cap | 744 GB | 3.0 GB/s | 3.0 GB/s | 60-90 s | ~248 s | 37 s | **60-90 s** (estimate) |
| GLM class, striped NVMe | 744 GB | ~20 GB/s | 2.0 GB/s | 60-90 s | ~37 s | 37 s | **~0** (estimate) |

Sources: `B`, `R` and `T_warm` for 32B are measured above. For GLM, `B` is the
whole checkpoint because every EP rank reads every file (§5); `R` is the 32B
buffered rate, assumed for the same class of disk; `T_warm` is the measured
37 s load; `H` is the measured rank construction. 5.2 GB/s is the
direct-I/O rate of one local NVMe measured on another cluster; buffered reads
ran at ~3.0-3.3 GB/s, which is the rate used here. On a node with several
devices striped the disk is no longer the bottleneck and there is nothing to
hide. Measurement B (§10) replaces the GLM rows.

## 2. Mechanism: a native sidecar in the engine pod

`make workload-patch` adds an `initContainers` entry with
`restartPolicy: Always` (a native sidecar: Kubernetes 1.29 and later, on by
default; GA in 1.33). The kubelet starts it before the engine container and
keeps it running beside it.

- It mounts the same weights volume, read-only, at the same path.
- It runs as the engine container's UID and GID (copied from its
  `securityContext`), with all capabilities dropped and no privilege
  escalation.
- It uses the engine image, which has Python. The tool is one stdlib-only
  Python file, shipped in a ConfigMap by `make workload-patch`.
- It needs no ServiceAccount token, no RBAC and no new privilege. It reads
  files the engine can already read.

What it does, in order: check residency (§3), check memory (§4), read (§5),
then sleep until the pod ends. It does **not** exit after the read. A native
sidecar that exits is restarted, and its cgroup would be removed (§4).

Why a sidecar rather than a DaemonSet that watches for pods: the sidecar is
inside the pod, so it covers every start (autoscaler, manual scale, restart,
reschedule) with no watch, and its memory is charged inside the pod that uses
it. §9 has the watcher and why it lost.

On a LeaderWorkerSet, `make workload-patch` patches the leader and the worker
templates (since #361), so each pod in a group reads on its own node.

## 3. Check what is already in memory, and when the answer can be trusted

Before reading, the tool measures how much of each file is resident:
`cachestat(2)` where the kernel has it (6.5 and later), else `mmap` plus
`mincore(2)` through `ctypes`. `fincore` (util-linux) makes the same calls and
is the operator's check (§7).

Both calls only tell the truth to some callers:

- `mm/filemap.c` `can_do_cachestat()`: the file must be open for write, or the
  caller must own it or be capable over it (`inode_owner_or_capable`), or have
  write permission. Otherwise `cachestat` returns `EPERM`.
- `mm/mincore.c` `can_do_mincore()`: the same test on the mapping. Otherwise,
  on current kernels, `mincore` reports **every page resident**; on old ones
  it reported only pages mapped by the caller.

A false "all resident" would skip the read on a cold cache. So:

- The sidecar runs as the engine's UID. On Kubernetes the downloader and the
  engine both run as root, so root owns the files and the check is truthful.
- Before trusting the check, the tool tests the same rule itself: owner
  (`st_uid == geteuid()`) or write permission (`os.access(W_OK)`).
- **If it cannot, it fails closed:** it treats every file as cold and reads it
  all. Reading cached pages costs CPU, not disk.

Partial residency: the tool reads only non-resident ranges, merging gaps under
8 MiB. A file 99 % resident or more is skipped. If everything is resident, it
logs `skip reason=resident` and sleeps.

## 4. Memory

### Whose cgroup the pages are charged to

- **Charge at first touch.** A page-cache folio is charged to the memory cgroup
  of the task that adds it to the cache (`filemap_add_folio()` charges the
  current task's memcg). A later reader in another cgroup is not charged and
  the charge does not move. `posix_fadvise(WILLNEED)` and `readahead(2)` run
  in the caller's context too, so they are no way around it. The tool uses
  plain `read()`.
- **So the sidecar is charged.** Its container cgroup holds the pages; the pod
  cgroup above it counts them, as it would have counted the engine's own cold
  read. The pod's total is the same as today.
- **A small limit defeats the read.** If the sidecar's limit is below the bytes
  it reads, its cgroup reclaims its own earlier pages to make room for later
  ones. Nothing is OOM-killed, and the model ends mostly cold.
- **Working set.** The kubelet's working set is usage minus `inactive_file`
  (cAdvisor). A page read once sits on the inactive list. When the engine maps
  and touches it, it is activated, on the **sidecar's** LRU, and then counts
  in the sidecar's and the pod's working set. The pod's working set ends near
  where a cold start puts it: the model's bytes, in the pod that uses them.
  Under node memory pressure the kubelet ranks pods by working set above their
  requests, so the engine pod is as exposed to eviction as it is today, no
  more.
- **If the sidecar's cgroup goes away** (it is restarted or killed), its pages
  stay cached on the dying memcg and are not recharged when the engine touches
  them; they leave only when reclaimed. They are still usable. That is why the
  sidecar sleeps instead of exiting.

### The sidecar's limit

- **Request** 64 MiB. It is added to the pod's request.
- **Limit:** none by default. A container without a limit leaves the pod
  without a pod-level limit, as it is when the engine has none (the usual case
  for vLLM). The page cache is reclaimable, so an unlimited reader is bounded by
  the budget below, not by OOM.
- When the namespace has a `ResourceQuota` on `limits.memory`, or a
  `LimitRange` that injects a default limit, `make workload-patch` requires
  `WVA_WEIGHTS_PREWARM_MEMORY=<size>` and sets it: the bytes to read plus
  2 GiB. It refuses to write a sidecar with a default limit it did not choose.
  That limit counts against the quota.
- At run time the tool reads `memory.max`. If the bytes it needs exceed it
  minus 1 GiB, it logs `skip reason=cgroup-limit` and reads nothing.

### The node's budget

The budget uses **`MemFree`**, not `MemAvailable`. `MemAvailable` counts other
tenants' clean page cache as available; reading into it would evict their
files. `MemFree` is memory nobody is using.

- `need` = this pod's bytes that are not yet resident (§3). Pages of this model
  already cached cost nothing and are not counted.
- **Headroom** = the larger of 5 % of `MemTotal` and 16 GiB
  (`WVA_WEIGHTS_PREWARM_HEADROOM`).
- Start only if `MemFree - need >= headroom`; otherwise
  `skip reason=memory`.
- Re-read `MemFree` every 1 GiB; stop when it falls below headroom
  (`stop reason=memory`). The tool never reads through reclaim.

What this adds: nothing in steady state. The engine maps the same files, so a
cold start puts the same bytes in memory anyway, ~18 s (small) or 60-90 s
(big) later.

## 5. What to read

### Every file, by default

vLLM's default loader reads every file and every tensor on every rank,
including on expert-parallel ranks (read in the v0.26.0 and v0.32.0 sources):

- `safetensors_weights_iterator` (`model_executor/model_loader/weight_utils.py`,
  ~lines 1079-1213) natural-sorts the file list, opens each with `safe_open`
  and calls `get_tensor` on every key.
- The default loader (`default_loader.py`, ~420-482) skips tensors only when
  `--enable-ep-weight-filter` is set (`model_loader/ep_weight_filter.py`). It
  defaults to off, and EPLB turns it off.

So **read-all is the correct default**, for one pod and for every pod of a
LeaderWorkerSet group. It is what the engine will read on that node anyway.
Files: every `*.safetensors` of the model directory, plus the small files read
first (`config.json`, tokenizer files, the index).

### This node's share, only when the engine filters too

When `--enable-ep-weight-filter` is set and EPLB is off, the engine reads only
the dense and shared tensors plus its local experts. Then the sidecar reads the
same **byte ranges**, from each file's safetensors header (tensor offsets), not
whole files:

| fact | where it comes from |
| --- | --- |
| node index in the group | label `leaderworkerset.sigs.k8s.io/worker-index`, env `LWS_WORKER_INDEX` |
| TP, PCP, DP, DP per node, first DP rank | the engine's arguments (`--tensor-parallel-size`, prefill context parallel size, `--data-parallel-size`, `--data-parallel-size-local`, `--data-parallel-start-rank`) |
| EP size | `DP x PCP x TP`; an EP rank includes its PCP rank |
| local experts | linear placement over EP ranks, as `ep_weight_filter.py` computes it |
| experts, tensor offsets | `config.json`; each file's safetensors header |

Anything else means read-all: `--expert-placement-strategy round_robin`, EPLB,
any non-default `--load-format` (fastsafetensors ignores the filter; the
multithreaded loader reads out of order), fused expert tensors, or an argument
the tool cannot resolve. Arguments are read after Kubernetes' `$(VAR)`
expansion against literal `env`; a shell variable inside `sh -c` or an env from
a ConfigMap is unresolved, never guessed.

### Order and pace

- **Order.** The natural-sorted file order the iterator uses, so the sidecar
  stays ahead of the engine.
- **Readers.** 2 threads by default (`WVA_WEIGHTS_PREWARM_READERS`), from the
  front of the list; 16 MiB blocks into a reused buffer;
  `posix_fadvise(SEQUENTIAL)`; `nice 19`.
- **Rate cap.** `WVA_WEIGHTS_PREWARM_RATE`, default **2000 MB/s**, a token
  bucket across readers. I/O priority (`ionice`) does nothing on NVMe under the
  `none` or `mq-deadline` schedulers, so the cap is the only control. The
  default is about two thirds of the measured buffered rate (3.0-3.3 GB/s),
  leaving a third of the device to everyone else. By §1.3 it still buys the
  whole 9.9 s on Qwen3-32B. Raise it on a node whose disk is the model's alone;
  lower it where other tenants' I/O matters more. Pick it from a buffered read
  of one cold weight file on the node.

### The engine already reading

If the engine catches up with the sidecar, both read the same pages. Each byte
is read from disk once: a second reader of a folio that is being read waits on
its lock. So overlap should be no worse than cold, apart from the sidecar's
CPU. That is plausible from how the page cache works, and **not measured**;
measurement A records it.

## 6. Races and concurrency

| case | behaviour |
| --- | --- |
| pod deleted or rescheduled | the sidecar dies with it; pages read stay cached and reclaimable. The new pod's sidecar starts on the new node. |
| container restart | the sidecar keeps running and does not re-read. A restarted engine finds the pages it left. |
| sidecar restart | runs the check again; finds the files resident and skips. |
| engine Ready before the read ends | the read continues to the end; it is reading what the engine already has, from cache. |
| two pods of one model start on one node | both sidecars read. The first charges the pages; the second mostly hits cache and costs CPU. The second engine's use then activates pages on the first pod's sidecar cgroup, so the first pod's working set carries both. That is how a shared cache is charged today too. |
| several models start at once | each sidecar checks `MemFree` against its own need. Two can pass the start check together; the re-check every 1 GiB stops whichever brings `MemFree` to the headroom. |
| download not finished | no read; logs `skip reason=not-downloaded` (the marker is missing). |

## 7. Configuration and operator experience

Same opt-in shape as the other halves of `make workload-patch`:

```bash
make workload-patch NAMESPACE=<ns> WVA_WORKLOAD_PATCH_APPLY=true \
     WVA_WORKLOAD_PATCH_APPLY_PREWARM=true
```

| variable | default | meaning |
| --- | --- | --- |
| `WVA_WORKLOAD_PATCH_APPLY_PREWARM` | `false` | add the sidecar and its ConfigMap |
| `WVA_WEIGHTS_PREWARM_LAYOUT` | `all` | alpha: `all` only. Beta adds `auto` (share when the engine filters) |
| `WVA_WEIGHTS_PREWARM_MEMORY` | unset (no limit) | required where a quota or LimitRange needs a limit (§4) |
| `WVA_WEIGHTS_PREWARM_HEADROOM` | max(5 % RAM, 16Gi) | free memory the read never goes below |
| `WVA_WEIGHTS_PREWARM_READERS` | 2 | reader threads |
| `WVA_WEIGHTS_PREWARM_RATE` | 2000 (MB/s) | cap across readers |

It applies only to a workload that mounts a claim made by `make weights`;
on a shared network claim it is not offered. Applying it rolls the workload,
like any pod-template change, and `make workload-patch` says so. The next
`helm upgrade` of the model server's chart reverts it, as it reverts every
half.

**Removing it.** `WVA_WORKLOAD_PATCH_REMOVE_PREWARM=true` writes and applies a
patch that deletes the sidecar by name (a strategic merge `$patch: delete` on a
Deployment, a JSON patch `remove` on each LeaderWorkerSet template).

**Logs**, one key=value line per event from the `weights-prewarm` container:

```
prewarm start layout=all files=25 bytes=65.5G resident_before=0.0% memfree=1.8T trusted=yes
prewarm done  read=65.5G seconds=32.8 rate=2.0GB/s resident_after=100.0% cgroup_memory=65.6G
prewarm skip  reason=resident|memory|cgroup-limit|not-downloaded
prewarm stop  reason=memory read=31.2G
```

**Metrics** (beta), on a port of the sidecar, prefix `wva_weights_prewarm_`:
`bytes_read_total`, `seconds`, `resident_ratio{phase="before|after"}`,
`cgroup_memory_bytes`, `runs_total{result="done|skipped|stopped",reason}`.

**How an operator checks it worked:**

1. `kubectl logs <pod> -c weights-prewarm`: the `done` line, and whether it
   came before the engine's weight load.
2. The engine's `Model loading took` line: near the warm figure (10 s for
   Qwen3-32B, not 20 s).
3. `fincore` on the weight files from a debug pod, as root.

**OpenShift: out of scope for alpha.** The sidecar copies the engine's
`securityContext` and runs as the project's range UID, so `restricted-v2`
should admit it. Unverified: whether that UID owns the files the downloader
wrote (if not, the residency check fails closed and the sidecar reads
everything), and the SELinux level of the files.

## 8. Security and multi-tenancy

- **Files.** The sidecar reads the model directory of the volume the engine
  already mounts, read-only, as the engine's UID. It can read nothing the
  engine cannot.
- **Privilege.** None added: no token, no RBAC, no capability.
- **Shared RAM.** The budget uses `MemFree`, so the sidecar only fills memory
  nobody is using, and stops before reclaim. In steady state it adds nothing
  beyond what the engine's own load puts in memory, charged to the same pod.
- **Disk.** The 2000 MB/s default leaves about a third of one NVMe to other
  tenants; set it lower where their I/O matters more.

## 9. Alternatives considered

| alternative | why not |
| --- | --- |
| **The weights DaemonSet watching pods bound to its node** (the first version of this proposal) | Needs a ServiceAccount token and a Role to list and watch pods in a DaemonSet that has neither, and any pod naming that ServiceAccount gets pod read. The page cache is charged to the DaemonSet pod, whose request is 256 MiB: under node pressure it becomes the first eviction target, and the engine's pages leave with its cgroup. The limit it needs (the model's size) collides with `limits.memory` quotas, LimitRange maxima and OpenShift overrides. It starts ~2 s earlier than a sidecar, which is not worth that. |
| **Timed re-read** | Holds the model in page cache on every prepared node all the time (65 GB per node for 32B, ~744 GB for GLM class). The kernel drops it under pressure, which is when a scale-up is likely. |
| **Trigger on the autoscaler's decision** | 11-23 s earlier (measured KEDA/HPA lag), but the node is unknown. Reading on all candidates multiplies reads and page cache: one 32B replica on 16 prepared nodes would read ~1 TB. Misses manual scale and restarts. |
| **A pod holding the weights in RAM** (`mlock`, `vmtouch -l`) | Unevictable memory equal to the model, per node per model, forever. A warm pool without the GPU part. |
| **Peer weight transfer** | Complementary: a replica filled from a serving peer over NCCL reads no files ([§4e](warm-pool-weight-transfer.md#4e-it-works-on-a-plain-scale-up-too-with-no-pool-at-all), measured byte-identical). Needs a same-layout peer and engine changes. Where it is used, the sidecar is not needed. |
| **Snapshot of a loaded engine** | Measured: restores in ~15 s, TP=1 only today. Where it applies it removes much more than the read. |
| **Warm pool** | Removes the whole start, at the cost of an idle engine. The sidecar is for the starts the pool does not cover. |

## 10. Measurement plan before building past alpha

Cold always means: weight files dropped with `posix_fadvise(DONTNEED)` and
checked 0 % resident, as root. Not `drop_caches`, which affects every tenant.

**A. Qwen3-32B, TP=1, one H200, prepared node.** Three starts per arm, same
node: cold (sidecar off); sidecar on, files cold; warm (sidecar off, files warm
from an earlier start). Record per start:

- pod created to `/health`; the engine's weight-load seconds;
- the sidecar's read seconds, and whether it finished before the load began;
- `MemFree` before and after; disk throughput (`/proc/diskstats`);
- `container_memory_working_set_bytes` for the engine container, the sidecar
  and the pod, sampled every 2 s from container start to 60 s after `/health`.

**B. GLM class on two 8xH200 nodes, LeaderWorkerSet, DP=16 / EP.**

1. Confirm the loader reads every file: one cold start, `mincore` (as root) on
   both nodes afterwards, every file resident on each, and
   `--enable-ep-weight-filter` absent from the engine's arguments.
2. Two arms, two starts each: cold; sidecar, read-all. Record per node: ready
   time, weight step, bytes read, peak `Cached`, `MemFree` minimum, disk
   throughput, working set as in A. Run one start with the rate cap off.
3. Beta only: a third arm with `--enable-ep-weight-filter` and the share
   layout, if anyone deploys with that flag.

**Acceptance (alpha to beta):**

- A: sidecar-on weight load within 2 s of the warm arm's median, and `/health`
  at least 7 s faster than cold, on all three starts.
- A: overlap is no worse than cold: no sidecar-on start slower than the
  slowest cold start.
- A: a start with files resident logs `skip reason=resident` within 2 s.
- No run takes `MemFree` below the headroom.

**Acceptance (beta to GA):** B: the sidecar gains at least 20 s of ready time
over cold, and §1.3's formula predicts the measured gain within 30 %. An e2e
check on kind (no GPU): small stand-in safetensors files, a pod with the
sidecar, residency asserted after start.

## 11. Rollout and open questions

| stage | scope |
| --- | --- |
| **alpha** | `WVA_WORKLOAD_PATCH_APPLY_PREWARM`; Deployments and LeaderWorkerSets; read-all only; residency check failing closed; `MemFree` budget; logs; remove switch. Measurement A. |
| **beta** | metrics; the share layout for `--enable-ep-weight-filter`; measurement B. |
| **GA** | both measurements met; the kind e2e in CI; the user-facing section in [workload-preparation](../reference/workload-preparation.md#weights-on-the-nodes-disk). Still off by default. |

Open questions:

1. **The disk rate on GLM-class nodes.** If they stripe several NVMe devices,
   §1.3 predicts almost no gain there. Measure `R` on the node before running B.
2. **SGLang.** Its loader's file order and whether it reads every file are not
   checked. Alpha is vLLM only.
3. **Kernel behaviour on a dying memcg.** Pages stay usable; whether a kernel
   in use reparents them changes only accounting. Record the kernel in §10.
4. **Native sidecar availability.** Kubernetes 1.29 or later. On an older
   cluster `make workload-patch` refuses the half rather than fall back to a
   plain init container, which would hold the engine's start for the read.
