# Preparing a workload to be scaled

Three things decide whether a scale-up helps:

- a new replica gets to Ready quickly (its weights, and the rest of the start);
- an old replica leaves without dropping requests;
- the Deployment says enough about itself for the scaling manager to act on.

None are scaling-manager settings; they are properties of the model server.
`make workload-patch` writes the first two. It also writes the engine compile
cache mount and variables (`VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR`,
`TRITON_CACHE_DIR`), but the storage behind them must exist first: a node
directory from `make engine-cache`, or a claim you already have
(`WVA_ENGINE_CACHE_CLAIM`). Applying that half is its own opt-in
([Engine caches on the node's disk](#engine-caches-on-the-nodes-disk)). The
rest of the start path is a checklist, below.

> Part of the [deployment guide](../../deploy/). For what to watch once it is
> running, see [Watching what the scaling manager decides](monitoring.md).

## What your pod must carry

Check these before registering a model. `<ns>` is the model's namespace,
`<modelID>` the `modelID` you put on the ScaledObject trigger
([trigger reference](scaledobject-trigger.md)). On a LeaderWorkerSet read the
**leader** template (`.spec.leaderWorkerTemplate.leaderTemplate`) where a
Deployment has `.spec.template`.

| # | The pod must carry | Without it | Check |
| --- | --- | --- | --- |
| 1 | `--served-model-name` equal to the trigger's `modelID` (else `--model`) | engine series are filtered on `model_name`; a mismatch reads as no metrics | `kubectl get deploy -n <ns> <name> -o jsonpath='{.spec.template.spec.containers[*].args}'` |
| 2 | Pod-template label `llm-d.ai/inferenceServing: "true"`, or `llm-d.ai/role: prefill`/`decode` | `make scaledobjects-plan` does not list the workload (it can still be registered by hand) | `kubectl get deploy -n <ns> -o custom-columns='NAME:.metadata.name,SERVING:.spec.template.metadata.labels.llm-d\.ai/inferenceServing,ROLE:.spec.template.metadata.labels.llm-d\.ai/role'` |
| 3 | On P/D: `llm-d.ai/role: prefill` or `decode` on each role's template; anything else is `both` | the role is scaled as a whole-request server ([P/D path](../well-lit-paths/pd-disaggregation/)) | same command as row 2 |
| 4 | A GPU-product `nodeSelector` or `nodeAffinity` | `AcceleratorNotResolved` Warning; a GPU limiter charges its GPUs to no pool and it cannot grow | `kubectl get events -n <ns> --field-selector reason=AcceleratorNotResolved` |
| 5 | Engine `/metrics` scraped by Prometheus (vLLM always; SGLang only with `--enable-metrics`) | `MetricsUnavailable` Warning on the ScaledObject; no scaling on load | PromQL `count(vllm:kv_cache_usage_perc{model_name="<modelID>"})` (SGLang: `sglang:token_usage`) is non-empty |
| 6 | The EPP's `flowControl` feature gate, parsed by the running EPP | no router-queue demand signal; a model at zero never wakes | PromQL `count(llm_d_epp_flow_control_queue_size) or count(inference_extension_flow_control_queue_size)` is non-empty |
| 7 | `preStop` hook on the engine container and `terminationGracePeriodSeconds` above it | every scale-down cuts in-flight streams | `make workload-patch NAMESPACE=<ns>` names workloads without a drain ([below](#draining-before-scale-down)) |
| 8 | Weights read from a mounted model cache | every scale-up is a download | `make workload-patch NAMESPACE=<ns>` reports `RE-DOWNLOADS ITS WEIGHTS` ([below](#weights-and-the-model-cache)) |
| 9 | Fast start: image held on the GPU nodes, short probe period | each scale-up pays the pull | `make prepull-status NAMESPACE=<ns>` ([below](#the-rest-of-the-start-path)) |
| 10 | Engine compile caches (`VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR`, `TRITON_CACHE_DIR`) and Python bytecode (`PYTHONPYCACHEPREFIX`, any Python engine) on a path that outlives the pod | every replica recompiles: about 14 s of torch.compile against about 3 s cached, and about 11 s of bytecode | `make engine-cache-status NAMESPACE=<ns>`; `make workload-patch` reports `COMPILES FROM NOTHING` or `RECOMPILES ITS PYTHON BYTECODE` ([below](#engine-caches-on-the-nodes-disk)) |
| 11 | A ScaledObject whose trigger is `external-push` with this `modelID` | nothing scales it — the controller has no watch of its own | `kubectl get scaledobject -n <ns> -o custom-columns='NAME:.metadata.name,MODEL:.spec.triggers[0].metadata.modelID,READY:.status.conditions[0].status'` |

## Weights and the model cache

A replica that must fetch its weights first turns every scale-up into a
download. That is slow, can be rate-limited, and needs Hugging Face reachable
from a cluster that may have no egress on purpose.

llm-d already solves this. The modelservice chart defaults to
`uriProtocol: pvc` and mounts a shared cache at `/model-cache`, so the weights
are fetched once and later pods read them locally. On a stock llm-d install
there is nothing to turn on.

To check, run `make workload-patch`; do not just list the pod's PVCs. A claim
mounted at one path while the engine downloads to another passes that test and
still re-downloads on every scale-up. What matters is where the weights *land*:
`HF_HOME` (or `--download-dir`) under a mounted path. A cache with nothing
pointed at it looks solved and is not.

`make scaledobjects-plan` also flags a workload whose engine downloads outside
every volume it mounts, because registering a ScaledObject is when new replicas
start appearing.

### What it does not buy

**It does not make scale-up fast.** An 8B model still spends tens of seconds
moving weights it already has locally into GPU memory, and a 70B model minutes:
the same order as a cold start.

The disk is not the reason. Measured with direct I/O, the shared RWX PVC reads
at ~1.8 GB/s and local NVMe at ~5.2 GB/s, so an 8B model is a few seconds of
I/O. The cost is the *load*: parsing safetensors, sharding and copying to the
device runs at about 2 GB/s even from page cache.

So the cache removes the *download*, not the *load*, and faster storage does not
remove the load either. That takes a different mechanism: keeping a process
warm, or snapshotting a loaded engine.

The first ships here as the **warm pool**: Pods holding an engine already loaded
on a GPU, which a scaling model borrows instead of paying the load. Whether a
pool pays, and how many models one Pod holds, depend on the cluster; see the
[warm pool guide](../guides/warm-pool/), and run
`deploy/warmpool.sh plan -n <ns>` to see which models could share one.

(An earlier revision gave ~430 MB/s as the PVC's bandwidth. That was an
end-to-end weight-load rate, not storage throughput; the conclusion stands, the
stated reason was wrong.)

## The rest of the start path

Start time is an autoscaling concern, not only an operations one. Until a new
replica is Ready the running ones serve alone; the queue they build meanwhile is
charged as demand, and the ramp is sized by it. A replica that starts twice as
slowly costs not one more replica but as many as the doubled queue implies, and
they arrive after the queue is gone. So take the seconds out of the start before
tuning the scaler. These are pod-template properties (Deployment or
LeaderWorkerSet), with or without a benchmark harness:

| on the way to Ready | what to check |
|---|---|
| **Nothing installed at container start.** | The container's command runs the engine and nothing else: no `apt`, `pip`, `curl` or clone before it. Anything the engine needs is in the image. A start that depends on a package mirror is as slow as the mirror that day, which is the one term that makes start time *vary* between otherwise identical replicas. |
| **`startupProbe` period.** | `periodSeconds` of a few seconds; a probe that fires every 30 s reports a started engine up to 30 s late, every time. Keep the time budget by raising `failureThreshold` (period 5 with threshold 360 is the same 30 minutes as period 30 with threshold 60). The first probes fail on connection refused while the server binds; that is what the threshold is for. |
| **Engine caches that outlive the pod.** | vLLM writes its torch.compile artefacts, the FlashInfer autotune table and Triton's JIT cache under `/tmp` unless told otherwise, so every replica recompiles from nothing. Point `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR` and `TRITON_CACHE_DIR` at a read-write path that outlives the pod and the second replica finds the first one's. The best home is the node's own disk -- [Engine caches on the node's disk](#engine-caches-on-the-nodes-disk), `make engine-cache` -- a shared RWX claim is the other. If the model claim is mounted read-only, use another claim rather than mounting it a second time: a CSI driver publishes a claim once per pod, and the second mount inherits read-only. And make the engine tolerate a cache path that turns out not to be writable -- a storage hiccup must cost one compile, not the replica; vLLM fails hard on a read-only cache directory unless the variable is unset first. The cache is keyed by a hash of the engine config, so a changed model, flag or version misses rather than hits stale. Python bytecode is the same kind of cache, for any Python engine: set `PYTHONPYCACHEPREFIX` beside them, one directory per image ([Python bytecode](#python-bytecode); about 11 s per start). |
| **The image is already on the node.** | An engine image is 10-20 GB; a node that has to pull it adds a minute or more before the container even starts, and the kubelet evicts unused images under disk pressure, so "it was pulled once" does not stay true. `make prepull IMAGES=<image> NAMESPACE=<ns>` holds the image open on every accelerator node (one DaemonSet per image, the image itself asleep, no accelerator requested), and `make prepull-status` says per node whether the kubelet has it -- see [Holding the image on the nodes](#holding-the-image-on-the-nodes). With the image held, a pinned tag should pull `IfNotPresent`: `Always` contacts the registry at every start for a digest that cannot have changed. |
| **The weights** | are the [section above](#weights-and-the-model-cache) -- the download; for a large model, also [Weights on the node's disk](#weights-on-the-nodes-disk) -- the read. |

What remains is the cold process itself (imports, the API server, the
KV-transfer connector, the profile run) and, on a large model, the weight load.
One part of it is not cacheable today, so do not chase it: on vLLM, the
`ll_bf16` router-GEMM kernel warmup (about 7.5 s measured) is built with
`cute.compile` with its cache turned off, and no cache directory keeps it.
Below that floor the only lever is not starting cold: the
[warm pool](../guides/warm-pool/), or a minimum replica count of two.

### Holding the image on the nodes

```bash
# exactly the reference the model server's pod spec names, tag or digest included
make prepull IMAGES=docker.io/vllm/vllm-openai:v0.26.0 NAMESPACE=<ns>
make prepull-status NAMESPACE=<ns>          # per accelerator node: present / absent, and the holder's state
make prepull-delete NAMESPACE=<ns>          # stop holding every image (or IMAGES=<image> for one)
```

**Which nodes.** By default, every node with a known GPU product label (GPU
Feature Discovery's, CoreWeave's, GKE's, EKS's, Karpenter's, the AMD
operator's). The list is in `deploy/lib/accelerator_nodes.sh`, the same one the
controller resolves nodes through. Model servers are placed by their
accelerator *resource* request, not a label, so on a one-vendor cluster these
are the same nodes. On a mixed-vendor cluster the default also holds a CUDA
image on AMD, Intel or Gaudi nodes where it can never run; `prepull-status`
names those nodes and warns. There `PREPULL_NODE_SELECTOR` is required: set it
to what the model servers select on.

**The selector.** One or more `key=value` pairs, comma-separated with no spaces.
A node must carry **every** pair: each pair becomes an equality term of the
holder's `nodeSelector` (no set-based terms, no `!=`, each key once). An
explicit selector **replaces** the default rather than adding to it, so include
the product label among the pairs to stay on GPU nodes. Keys and values follow
Kubernetes label syntax (a key is an optional DNS-subdomain prefix and `/`, then
a name of at most 63 characters; a value is at most 63; both start and end with
a letter or digit). An empty value selects the nodes whose label is empty, not
every node with the key: `gpu.nvidia.com/model=` is CoreWeave's CPU nodes.

Two pairs are for a cluster where no single label picks the right nodes. Say one
GPU node is broken, and the only label that sets the healthy ones apart is also
on the CPU-only nodes. The product label alone takes in the broken node, the
other label alone takes in the CPU nodes, and both together take neither:

```bash
make prepull NAMESPACE=<ns> IMAGES=<image> \
  PREPULL_NODE_SELECTOR=nvidia.com/gpu.product=NVIDIA-H200,example.com/node-state=production
kubectl get nodes -l nvidia.com/gpu.product=NVIDIA-H200,example.com/node-state=production   # the nodes it lands on
```

`WEIGHTS_NODE_SELECTOR`, `--node-selector` on `deploy/prepull.sh`,
`deploy/weights.sh` and `deploy/enginecache.sh`, and the benchmark's node-local
weights take the same form. The model servers must select the same nodes.

**Taints.** `PREPULL_TOLERATIONS=<key>[,<key>]` adds taints beyond
`nvidia.com/gpu`, which is always tolerated. A holder `Pending` on every node
with no reason is a taint it does not tolerate.

**Status.** `prepull-status` lists the nodes each DaemonSet was applied for
(the selector is recorded on it), so it needs no selector of its own. It,
`weights-status` and `engine-cache-status` count only nodes a model server can
use: a cordoned node, or one with a `NoSchedule` or `NoExecute` taint the
DaemonSet does not tolerate, is listed as `excluded` with the reason and not
counted. The command exits 0 when every usable node is ready. It compares the
node's own image list with the reference, so an image named differently from
what the pods pull shows as absent everywhere.

**How it holds.** The holder runs the image itself, asleep, with a memory limit
and no accelerator: a running container protects its image from the kubelet's
garbage collection, an exited one does not. The image must carry `/bin/sh`. One
without it is still pulled (the kubelet fetched it to create the container) and
`status` says `pulled`, not `present`, but nothing holds it. Several images are
a comma-separated list. Nothing in the scaling manager depends on the holder; it
is a start-time measure.

A holder reading `Failed Evicted` means the node is under `DiskPressure`: the
kubelet is evicting pods and garbage-collecting images, a replica there would
pull from scratch, and nothing in the namespace can fix it. (On the first run on
a 17-node cluster, 16 held the image within two minutes; one was that node.)

Three things to know before relying on it:

- `prepull-status` (and the report `prepull` prints after applying) lists
  nodes, which is cluster-scoped. A namespace tenant has no `list nodes` by
  default; the cluster-admin setup grants it to the controller, not to whoever
  runs `make`. Without it the DaemonSets still apply and only the status fails,
  with a Forbidden.
- The holder runs the engine image as that image runs -- as root on
  Kubernetes, since engine images are built that way and `runAsNonRoot`
  would fail the container; as a UID from the project's range on
  OpenShift, where `restricted-v2` assigns one, which `sleep` does not
  mind -- with no privilege, every capability dropped, a read-only root
  filesystem and the runtime's default seccomp profile. (No `runAsUser`
  either way: a fixed one is what a MustRunAsRange SCC rejects.) Engine
  images bake in `NVIDIA_VISIBLE_DEVICES=all`, which the NVIDIA runtime
  honours even from a container that requested no GPU -- on OpenShift's
  GPU operator too, whose toolkit ships that acceptance on; the holder
  sets it to `void`, so no device is injected. That is admitted under Pod
  Security `baseline` and under OpenShift's `restricted-v2` SCC (by
  analysis; not yet run there), and rejected where Pod Security
  `restricted` is *enforced* (it requires `runAsNonRoot`) -- OpenShift's
  label syncer only warns at that level, so there `kubectl apply` prints
  a `runAsNonRoot != true` warning and the apply goes through. When no
  holder appears on any node, `prepull-status` prints the DaemonSet's
  latest `FailedCreate` event, which is where a pod-security, quota, SCC
  or LimitRange rejection is reported.
- With `IfNotPresent` and a *tag*, a node keeps whatever the tag pointed at
  when it pulled; with `Always` the registry's current digest wins at every
  start. For a pinned release tag that is the point. If the tag can move and
  that matters, use a digest (`repo@sha256:...`); the holder, `status` and the
  model server's pod spec all accept one.


### Weights on the node's disk

The shared cache holds the weights, but it is not the fastest place to read
them. Every replica reads the whole model over the network filesystem, and
replicas starting together (a scale-up) share that pipe. For a small model on a
fast class that is seconds and not worth a second copy. For a large one it is
the start path: on one cluster, two nodes reading a 700 GB model from the shared
volume took 1403 s against ~104 s from local NVMe, past the engine's 600 s
startup timeout, so the replica never came up. The load term stays either way;
this removes the read term when it dominates.

```bash
# the engine image is any image carrying huggingface_hub; the claim is printed
make weights WEIGHTS_MODEL=Qwen/Qwen3-32B WEIGHTS_PATH=/mnt/local/models \
     WEIGHTS_IMAGE=docker.io/vllm/vllm-openai:v0.26.0 NAMESPACE=<ns>
# on RHCOS (OpenShift) the directory is under /var, one per project: WEIGHTS_PATH=/var/mnt/weights/<ns>
make weights-status NAMESPACE=<ns>          # per accelerator node: present / downloading, and why not
make weights-delete WEIGHTS_MODEL=<id> NAMESPACE=<ns>  # drop that model's claim, volume and downloader; the files stay
                                            # (without WEIGHTS_MODEL: every model's in the namespace)
```

What it creates:

- one static `hostPath` PersistentVolume, and one claim bound to it and to no
  other (the volume carries a `claimRef`);
- one DaemonSet that downloads the model onto every accelerator node, then
  reports Ready. `weights-status` and the DaemonSet's `numberReady` both mean
  "the download finished on this node". Placement is the image holder's:
  `WEIGHTS_NODE_SELECTOR` / `WEIGHTS_TOLERATIONS` default to the `PREPULL_*`
  values.

The model lands under `<dir>/models/<id>` (the benchmark harness's layout). A
model server mounts it as `pvc://<claim>/models/<id>`, like any claim; each node
reads its own copy, and nothing in the pod spec says hostPath.

- A gated model takes `WEIGHTS_HF_TOKEN_SECRET=<secret>[/<key>]`.
- `WEIGHTS_MEMORY` sets the downloader's memory limit (`16Gi` unless set;
  `--memory` on `deploy/weights.sh`). The request stays 256 MiB, so a node
  reserves nothing. At the earlier 2 GiB limit an 8B model's download was
  OOMKilled on every node.
- Deleting keeps the files; the next apply finds the marker and downloads
  nothing.

What it costs, and what it needs:

- Disk: the model's size on every accelerator node, and one Hugging Face
  download per node (a 60 GB model on 16 nodes is a terabyte of egress, once).
  The directory is cluster-shared, root-writable state that outlives every
  object here. Every namespace pointed at it shares the files and trusts the
  marker (the first writer wins; the others download nothing), and nothing
  charges it to a quota, so a fill from one namespace is `DiskPressure` for
  every pod on the node. Use one directory per trust domain
  (`/mnt/local/weights/<namespace>`), on a disk that is not the node's own
  (`/mnt/local/...` on CoreWeave), never a network mount, and never a system
  path. The script refuses `/etc`, `/var/lib`, `/var/spool`,
  `/tmp`, `/home`, `/opt/bin` and their kind (and where RHCOS keeps them:
  `/var/home`, `/var/roothome`, `/var/usrlocal`, `/sysroot`, `/ostree`),
  `/var/mnt` and `/var/srv` themselves, a top-level directory on its own,
  and `..`.
- Rights to create PersistentVolumes, which are cluster-scoped. A namespace
  tenant lacks them (on OpenShift the `storage-admin` role has them); ask the
  cluster admin to run `make weights` or create the volume.
- The claim's `WEIGHTS_CAPACITY` (1Ti unless set) is charged by a
  `requests.storage` quota, so a project with a storage quota refuses the
  claim; set the capacity near the model's size there.
- The claim cannot be resized: it is bound to a static volume, and Kubernetes
  resizes only dynamically provisioned claims. A re-run keeps an existing
  claim's size when `WEIGHTS_CAPACITY` is unset, and refuses a different
  `WEIGHTS_CAPACITY` before applying anything. To change the size, scale the
  model servers that mount the claim to 0 (a mounted claim stays
  `Terminating`), run `make weights-delete WEIGHTS_MODEL=<id> NAMESPACE=<ns>`
  (the files on the nodes stay; without `WEIGHTS_MODEL` it deletes every
  model's claim in the namespace), and apply again. `--dry-run` reads nothing
  from the cluster, so it shows the set or default size, not the kept one. The
  same holds for `ENGINE_CACHE_CAPACITY` and the `engine-cache` claim, freed
  with `make engine-cache-delete NAMESPACE=<ns>` (one claim per namespace).
- A claim of the model's name on another storage class is not the script's:
  apply refuses it and leaves it alone.
- Behind a proxy to Hugging Face, the downloader needs `HTTPS_PROXY`: the
  cluster does not inject its proxy into arbitrary pods.
- Pod Security `baseline` admits the downloader (it mounts the claim, not a
  hostPath); `restricted` does not. On Kubernetes it runs as root, which
  writing the kubelet-created root-owned directory needs. It runs as its own
  ServiceAccount, `weights-downloader`, so a grant for it does not land on the
  namespace's `default`. But any pod in the namespace may name that
  ServiceAccount, so an SCC bound to it is root for everyone with `pods/create`
  there.

**On OpenShift** -- by analysis; not yet run there, and `make weights`
says so when the cluster has SecurityContextConstraints:

- `restricted-v2` admits the downloader and runs it as the project's range
  UID with GID 0, which cannot write a root-owned directory; and the
  kubelet does not relabel a hostPath, so even root as `container_t` could
  not write a directory labelled `var_t`/`mnt_t`. Both surface as
  `CrashLoopBackOff` with `Permission denied` in the log, which
  `weights-status` names. Neither exists to fix until the first pod has
  run, so prepare the directory on each node **before** `make weights`:
  ```bash
  oc debug node/<node> -- chroot /host sh -c \
    'mkdir -p /var/mnt/weights/<ns> && chgrp 0 /var/mnt/weights/<ns> && chmod 2775 /var/mnt/weights/<ns> && chcon -t container_file_t /var/mnt/weights/<ns>'
  ```
  or, for an extra disk, a MachineConfig mount unit at that path with
  `Options=context=system_u:object_r:container_file_t:s0`, which labels
  the whole disk for containers and survives everything (and puts every
  file at level `s0`, so the per-project isolation below no longer
  applies -- the directory split is then the only separation). No SCC
  grant is needed on this path. The alternative is an SCC that runs the
  downloader as root: `oc adm policy add-scc-to-user <scc> -z
  weights-downloader -n <ns>`, where the SCC has to allow the
  `runtime/default` seccomp profile the pod sets -- stock `anyuid` does
  not, and admission then falls through to `restricted-v2` as if nothing
  had been granted (`oc get pod <p> -o
  jsonpath='{.metadata.annotations.openshift\.io/scc}'` says which SCC
  took the pod). It removes the `chgrp`/`chmod`, not the `chcon` (the
  kubelet still does not relabel the hostPath), and it is root for every
  pod creator in the project.
- The directory is under `/var` on RHCOS (`/var/mnt/<x>`, `/var/srv/<x>`):
  the root is read-only, `/mnt`, `/home`, `/opt`, `/srv` are symlinks into
  `/var`, and `/var/mnt` on its own is the root disk -- the one the
  kubelet's image store and eviction thresholds live on. Mount the NVMe
  there first (a MachineConfig mount unit); the script cannot tell a
  mountpoint from a directory. A new top-level directory fails outright
  (`read-only file system`).
- Files the downloader writes carry the project's SELinux level, so the
  engines in the same project read them and another project's pods
  cannot even see the marker: on OpenShift one directory per project is
  not advice but the only thing that works, unless the disk is mounted
  with `context=` as above.
- A node that joins later has no copy until the DaemonSet reaches it, and a
  replica scheduled there meanwhile reads from a directory that is being
  written. `weights-status` says which nodes are there yet. A cordoned node
  or one under `DiskPressure` still counts for the DaemonSet (its controller
  tolerates both) and keeps evicting the downloader; `weights-status` lists
  it as `excluded` and leaves it out of the count, since no engine can start
  there either. Nothing in the namespace fixes that node.

On node-local NVMe use the engine's default loader (memory-mapped
safetensors); `--safetensors-load-strategy prefetch` is for network filesystems
and buys nothing here. Direct-I/O, many-thread loaders are a further step, not
measured here.

The benchmark standup has the same measure under
`BENCHMARK_MODEL_HOSTPATH=<dir>`; what the harness does with it is in
[Benchmark the scaling manager](../guides/benchmarking/README.md#replica-start-time-in-the-harness).

### Engine caches on the node's disk

The caches in the table above (torch.compile, FlashInfer autotune, Triton JIT,
Python bytecode) pay only if the next replica finds them, and a shared RWX claim can fail that.
Measured on one cluster: nine starts of one decode pod spec, three per node on
three nodes, were repeatable within 2.5 s per node but split by node into
51-56 s and 75-81 s. The 25 s was the cache. On the slow nodes the CSI driver had
published the claim read-only (`ro` in `/proc/mounts`; `touch` fails with
"Read-only file system"), the guard in the engine's command fell back to `/tmp`,
and every start compiled from nothing: 14.4 s of torch.compile against 2.9 s
cached, plus 8 s more in the API server. The node decided the start time, and
nothing in the pod said so.

```bash
# any image with /bin/sh; the engine image is the natural choice. Same directory as the weights.
make engine-cache ENGINE_CACHE_PATH=/mnt/local/weights/<ns> \
     ENGINE_CACHE_IMAGE=docker.io/vllm/vllm-openai:v0.26.0 NAMESPACE=<ns>
# with make weights already run, the path and image default to WEIGHTS_PATH / WEIGHTS_IMAGE
make engine-cache-status NAMESPACE=<ns>     # per accelerator node: prepared / not, and why
make engine-cache-delete NAMESPACE=<ns>     # drop the claim, volume and preparer; the caches stay
```

**What the node directory must be.** `ENGINE_CACHE_PATH` is one path that must
work on every node the preparer selects: by default every node with a known GPU
product label, or, replacing that default, the nodes
`WEIGHTS_NODE_SELECTOR=<key=value>[,<key=value>]` names (every pair must
match). The model servers must select the same nodes; a replica on a node the
preparer missed finds no cache and, with the guard, costs a compile rather than
the replica. The directory must be:

- **absolute, at least two components, no `..`**, no trailing slash
  (`/mnt/local/weights/<ns>`; the whole of a top-level directory is refused);
- **on a disk that is not the node's own, and never a network mount.** A
  hostPath is a bind mount with no driver in its path -- which is the point
  -- and what is written there is charged to no quota, so a fill is
  `DiskPressure` for every pod on the node. On CoreWeave that is
  `/mnt/local/...`; on RHCOS the NVMe mounted at `/var/mnt/<x>` (a
  MachineConfig mount unit) first, since `/var/mnt` on its own is the root
  disk and the script cannot tell a mountpoint from a directory;
- **never a system path**: `/etc`, `/usr`, `/var/lib`, `/var/log`, `/tmp`,
  `/home`, `/opt/bin` and their kind are refused (and where RHCOS keeps them:
  `/var/home`, `/var/roothome`, `/var/usrlocal`, `/sysroot`, `/ostree`), as
  are `/var/mnt` and `/var/srv` themselves;
- **one directory per trust domain**, because what lands there is code every
  engine on the node loads (the paragraph below); on OpenShift one per
  project is the only thing that works;
- **creatable by the preparer**: root on Kubernetes; on OpenShift -- by
  analysis, not yet run there -- `<dir>/engine-cache` prepared on each node
  before `make engine-cache` (`chgrp 0`, `chmod 2775`,
  `chcon -t container_file_t`, below).

You also need rights to create PersistentVolumes, which are cluster-scoped and
which a namespace tenant lacks (`storage-admin` on OpenShift). Ask the cluster
admin to run `make engine-cache`, or to create the volume.

**Without a node directory.** The caches only need a read-write path that
outlives the pod; a shared `ReadWriteMany` claim the namespace already has
works. `make workload-patch` with `WVA_ENGINE_CACHE_CLAIM=<claim>[:<subPath>]`
writes the same variables and mount against that claim. (The benchmark
harness ran this way, on its workload claim under `engine-cache/`, before the
node-local cache existed.) The first start anywhere compiles once, and later
replicas on any node find it. The costs:

- A CSI driver can publish the claim read-only on a node, and every start there
  compiles. So the engine's command needs the guard from the start-path table,
  always. The emitted patch carries it as a comment, because the command
  belongs to the chart.
- It widens who is trusted. The claim holds code every engine loads, so whoever
  can write it (for a benchmark's workload claim, also the load generator and
  results pods) runs code in every engine on every node at once; a write to a
  node directory reaches only that node. Trust the claim like the engine image.

A node selector does not remove the need for a directory; it only picks which
nodes get one. Nor does a `local`/`local-path` StorageClass: such a claim binds
to one node, and a per-node cache shared by every pod on the node is exactly the
static hostPath volume.

What it creates: one static `hostPath` PersistentVolume at
`<dir>/engine-cache`; one claim named `engine-cache` bound to it and to no other
(the volume carries a `claimRef`); and one DaemonSet that creates the four
cache directories (`vllm`, `flashinfer`, `triton`, `pycache`) on every
accelerator node, writes a marker and stays Ready.
The directories are world-writable and sticky: any engine UID can write, and none
removes another's files. `engine-cache-status` and the DaemonSet's `numberReady`
both mean "prepared, and writable by this UID". A model server mounts the claim
read-write and points the variables under it:

```yaml
volumes:
  - name: engine-cache
    persistentVolumeClaim:
      claimName: engine-cache
containers:
  - name: vllm
    volumeMounts:
      - name: engine-cache
        mountPath: /engine-cache
    env:
      - {name: VLLM_CACHE_ROOT,          value: /engine-cache/vllm}
      - {name: FLASHINFER_WORKSPACE_DIR, value: /engine-cache/flashinfer}
      - {name: TRITON_CACHE_DIR,         value: /engine-cache/triton}
      - {name: PYTHONPYCACHEPREFIX,      value: /engine-cache/pycache/<image key>}
```

A hostPath is a bind mount with no storage driver in its path, so no driver
publishes it read-only. (The filesystem under it can still go read-only, from an
NVMe error or an admin's remount; the preparer's readiness and the guard cover
that.) Each node's cache is warm from the second start on that node. The first
start on a node compiles once (measured: 76 s and 81 s on two nodes with an
empty cache, then 55 s and 55 s; 50 s on a third a previous pod had filled).

That is the node-local cache's weak spot: the scheduler spreads replicas across
nodes, so until every node has started an engine once, a scale-up can land on a
cold one (measured: a second replica on an unused node was Ready 96 s after it
was wanted). So **seed it**. `ENGINE_CACHE_SEED_CLAIM=<claim>[:<subPath>]` names
a claim in the namespace, naturally the shared RWX claim the caches lived on
before. The preparer mounts it read-only and merges it into each node's cache,
file by file:

- an entry the node already has is kept; one it lacks is copied (a second engine
  config's key lands beside the node's own);
- a failed copy is removed and retried at the pod's next start;
- a marker records which seed the node has, so a changed seed is merged too;
- a subPath that does not exist yet is nothing to seed, not an error;
- a symbolic link is skipped, and the log names it: vLLM writes none, and a
  link is not the cache;
- a file not every user can read is copied readable by every user, and the
  log counts it: the engine may run as another user;
- every directory it creates is readable and searchable by every user.
  Publish does the same in the seed.

The copies are made as root. Kept at their source modes, vLLM's `600` AOT files
would be unreadable to an engine that does not run as root, on every seeded
node. The cache directory is already world-writable and shared by every engine
on the node, so world-readable copies expose nothing more.

**A node seeded by an earlier version keeps its old copies.** A merge keeps
what a node already has. Earlier versions of the preparer kept the source
modes, so a node they seeded can still hold files only root can read, such as
vLLM's `600` AOT files. An engine that is not root cannot read them, and
`make engine-cache-verify ENGINE_CACHE_UID=<uid>` shows them as `UNREADABLE`.
Fix it once on that node, through its preparer, which runs as root on
Kubernetes:

```bash
# the preparer pod on that node
kubectl get pods -n <ns> -l wva.llmd.ai/engine-cache=engine-cache --field-selector spec.nodeName=<node>
# make every file readable and every directory searchable by every user
kubectl exec -n <ns> <preparer pod> -c prepare -- \
  chmod -R a+rX /engine-cache/vllm /engine-cache/flashinfer /engine-cache/triton
```

Or reseed the node: with no engine running there, delete its cache and the
record of its seed, then restart its preparer, which merges the seed again.
This also drops what the node compiled itself.

```bash
kubectl exec -n <ns> <preparer pod> -c prepare -- \
  rm -rf /engine-cache/vllm /engine-cache/flashinfer /engine-cache/triton /engine-cache/.seeded
kubectl delete pod -n <ns> <preparer pod>
```

Each copy belongs to the user the preparer runs as (root on Kubernetes), not to
the user that compiled it. An engine under another user can read what was
seeded, but cannot change it. Any image with `/bin/sh` works: with GNU `cp` the
copies keep their file times; with busybox `cp` they do not.

With a seed, a node is Ready only once it carries it, so `engine-cache-status`
counts a node whose seed did not complete as not prepared. That holds for the
newest seed: before the preparer merges a new one, it drops the record of the
old one. A node whose newest merge failed is not prepared, even if an older
merge worked. The engines never wait on the preparer.

The seed is code the engines load, so trust it as the cache itself, and more:
one write to it reaches every node at the next prepare, while a write to the
cache reaches only the writer's node. The caches are keyed by a hash of the
engine config, not the node, so one claim per namespace serves every model and
flag set, and a changed engine misses rather than hits stale. A seeded node
starts warm on its first engine. Loading what another node compiled was shown on
one accelerator model, driver and engine build; mixed drivers or engine versions
are untried. Keep the guard: a node the preparer has not reached
(`engine-cache-status` lists it, with the seed) costs a compile, not the replica.

```bash
# the caches this namespace already has on its shared claim, onto every node
make engine-cache ENGINE_CACHE_PATH=/mnt/local/weights/<ns> ENGINE_CACHE_IMAGE=<engine image> \
     ENGINE_CACHE_SEED_CLAIM=<shared claim>[:<subPath>] NAMESPACE=<ns>
```

**A seed starts empty** in a fresh namespace, and the preparer only copies it
*to* the nodes. Each engine configuration (every model, and prefill and decode of
a P/D model, which compile under different keys) then compiles once per node.
Measured on two P/D 8B models: 12-17 s on a node the role had not run on,
against 2-3 s on one it had. Once every configuration has started somewhere,
fill the seed *from* the nodes:

```bash
# every prepared node merges its caches into the seed (what the seed has is
# kept), the seed is stamped, and every preparer restarts and merges it in
make engine-cache-publish NAMESPACE=<ns>
make engine-cache-status NAMESPACE=<ns>     # every usable node prepared again
```

Run it again after adding a model or changing an engine flag: the new stamp
makes every node merge the new keys.

Publish waits up to 10 minutes for every usable node to merge the new stamp.
If a node does not, publish fails and says so. The seed stays stamped, so run
`make engine-cache-status` to see which node, and read that node's preparer log
to see why.

#### Python bytecode

Engine images ship almost no compiled bytecode for their Python packages
(vLLM v0.26.0: 26,613 `.py` files, 412 `.pyc`). So every container compiles its
imports again and loses them with the pod. `PYTHONPYCACHEPREFIX` pointed at
`/engine-cache/pycache/<image key>` keeps them on the node. Measured on vLLM
v0.26.0, it saves about 11 s of every start after the first:

| model | before | after |
|---|---|---|
| Qwen3-32B, TP=1 | 59.8 s | 48.3 s |
| Qwen3-32B, TP=2 | 72.4 s | 61.0 s |
| Qwen3-32B, TP=2, LeaderWorkerSet leader and worker | 108.6 s | 97.7 s |
| Qwen3-30B-A3B (MoE) | 61.8 s | 50.8 s |

The API server's imports went from 13.5 s to 6.2 s, a worker's from 4.6 s to
2.3 s. The first start writes about 150 MB (8,000 files).

- **One directory per image.** `<image key>` is the first 12 hex digits of the
  SHA-256 of the exact image reference, tag or digest included. Python checks a
  `.pyc` only against its source file's time and size, and the files of one
  install share one time. So two images must not share a directory: an image
  patched from another could load the other's bytecode. `make workload-patch`
  writes the key. Run it again when the image changes; it warns when a pod's
  key belongs to another image.
- **Pin the image by digest, or use tags that never move.** A tag pushed again
  keeps its key, and the warning cannot see a rebuilt tag.
- **Any Python engine.** vLLM and SGLang alike; the variable is Python's. For
  SGLang this is new: its namespace needs `make engine-cache` (or
  `WVA_ENGINE_CACHE_CLAIM`) to get it.
- **It needs the guard.** With the variable set, Python reads bytecode only
  from that directory and ignores the image's own `__pycache__` directories. So
  a directory Python cannot write (read-only, created by another user, blocked
  by an SELinux label) makes the start slower than no variable at all. The
  guard in the engine's command (the emitted file has it) unsets the variable
  when the directory is not writable, and logs one line.
- **Skip it** when the pod sets `PYTHONDONTWRITEBYTECODE` (`make workload-patch`
  does not emit it then; check the image's own environment too), and for an
  image that already ships its bytecode compiled.
- **One user per image.** The first engine to start an image creates its key
  directory as its own user, mode `755`. An engine of that image under another
  user cannot write there; the guard then unsets the variable.
- **It is code, like the other caches.** The key comes from the public image
  name and `pycache/` is `1777`, so anyone who can run a pod that mounts the
  same node directory can create a key directory first and put bytecode in it
  that every engine of that image loads. This is the same exposure as `vllm/`,
  `flashinfer/` and `triton/` ([What it costs](#what-it-costs)): keep one
  engine-cache directory per trust domain, or `chown` and `chmod 1770` the
  directories for the engines' one UID.
- **Several engines at once are fine.** Three engines filling one key together
  left a clean cache.
- **Seed and publish carry it**, like the other three directories. Each node
  keeps its own copy. Until publish runs (or each node has started that image
  once), a node starts without it.

**Turning it on where the cache already exists.** On nodes prepared before this
version an engine may already have created `pycache/` as `755`. Run
`make engine-cache` again (the preparer makes `pycache/` `1777`), then
`make workload-patch`, then apply the emitted file. Do the same when the patch
puts the variable under a volume the engine already mounts that was never
prepared.

**Turning it off.** Remove the variable; the files left behind are not read.
Delete `<dir>/engine-cache/pycache` on the nodes to get the space back.

**On a LeaderWorkerSet** `make workload-patch` writes it for the leader and the
worker template, each keyed by its own image
([On a LeaderWorkerSet](#on-a-leaderworkerset)). Each pod reads its own node's
copy.

The guard goes in each container command, before the engine starts (on a
LeaderWorkerSet, the leader's and the worker's):

```bash
for v in VLLM_CACHE_ROOT FLASHINFER_WORKSPACE_DIR TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do
  eval "d=\${$v}"
  [ -n "$d" ] || continue
  if ! mkdir -p "$d" 2>/dev/null || ! touch "$d/.writable" 2>/dev/null; then
    echo "engine cache $v=$d is not writable; using the engine default instead"
    unset $v
  fi
done
```

The loop also covers a `PYTHONPYCACHEPREFIX` you set yourself: it is unset only
when its directory is not writable (then the faster choice), and the check
leaves an empty `.writable` file in that directory.

#### Check the cache without a second start

You can check that every node holds the torch.compile cache without
starting an engine there.

1. Start each engine configuration once.
2. Run `make engine-cache-publish`, so every node gets what it compiled.
3. Ask those pods which key they compiled under, and check the keys on every node:

```bash
# the key(s) the running engine pods used: one per configuration
make engine-cache-keys NAMESPACE=<ns> ENGINE_CACHE_PODS='<decode pod> <prefill pod>'
# every usable node: is each key there, with every rank, and readable
make engine-cache-verify NAMESPACE=<ns> ENGINE_CACHE_KEYS='<key>[,<key>...]'
```

If your engines do not run as root and any node was seeded or published to,
add `ENGINE_CACHE_UID=$(kubectl exec -n <ns> <engine pod> -- id -u)`.
Without it, a key owned by root is judged for root and passes even when your
engines cannot read it: seeded and published copies are readable by every
user, but a key written by a root engine may not be, nor one a node kept from
a seed by an earlier version (see seeding above). The UID is also the
pod's `securityContext.runAsUser` (or the container's), when set.
`ENGINE_CACHE_UID` is one UID for every pod: if prefill and decode run as
different users, run the check once for each.

Pass the keys exactly as `engine-cache-keys` prints them, for example
`torch_compile_cache/3f2a9c1d0e@rank_0_0+rank_1_0`. The part after `@` lists
the rank directories the engine wrote. Each one must be on the node.

Multi-node engines (a LeaderWorkerSet, or data parallel across nodes) need
one more step. Name the leader and every worker pod. Their ranks merge into
one key, for example 16 ranks for two nodes of eight. `engine-cache-verify`
then needs all 16 ranks on every node. Each node compiled only its own ranks,
so this passes only after `make engine-cache-publish`. If you name only the
leader pod, the check covers only the leader's ranks.

`engine-cache-verify` prints one line per node and key:

- `present`: the key is there, with every listed rank, and every file under it
  is readable by the UID the check assumed. The line names that UID.
- `ABSENT`: the key, or one of its ranks, is not on the node. The line names a
  missing rank. It means: if that rank starts on this node, it recompiles
  torch.compile. On a multi-node engine, a node often lacks ranks that never
  run there; publish to fill them in.
- `UNREADABLE`: a file under the key cannot be read, the engine's user could
  not read it or a directory on the way to it (the line names that UID), or it
  is a symbolic link.
- `COULD NOT CHECK`: no Ready preparer on the node, the exec failed, was
  Forbidden, or timed out. This is never reported as absent.

When `ENGINE_CACHE_UID` is not set, each key is judged for the user that owns
it. That is your engines' user only if they wrote the key. A seeded or
published key is owned by root. A key can also belong to another deployment
sharing the cache, or to the engines' old user if the image changed it. So
whenever a key was judged for the owner of its key directory (a `present`
line, or an `UNREADABLE` one for its modes), the summary warns, says how many
node/key checks it judged that way, and names the UIDs it assumed. The last
line names them too: `VERIFIED for root`, `VERIFIED for uid 1000`, or
`VERIFIED for uids 0, 1000`. More than one UID means the keys have different
owners. The engines of one configuration all run
as one UID, so at least one line was judged for the wrong user. These verdicts
still exit zero. Re-run with `ENGINE_CACHE_UID` to check your engines' user;
only then does the last line say plain `VERIFIED`.

It exits non-zero unless every key is present on every node. It needs
`pods/exec` in the namespace, plus the node list `engine-cache-status` needs.
`engine-cache-keys` warns when an engine compiled outside `VLLM_CACHE_ROOT`
(its cache root was not writable); that start did not fill the node cache.

What it does not check: the FlashInfer and Triton caches and the bytecode (`flashinfer/`,
`triton/` and `pycache/` beside `vllm/`). A node can be `present` for every key and still
build those kernels on its next start. Only a real start shows that.

<details>
<summary>Details: what a key is and how the check reads it</summary>

A key is the directory between `VLLM_CACHE_ROOT` and the rank directory in
the engine's log. vLLM writes one of two layouts:

- `torch_compile_cache/<hash>/rank_<r>_<dp>/<prefix>`, logged as `Using cache
  directory: ... for vLLM's torch.compile`.
- `torch_compile_cache/torch_aot_compile/<hash>/rank_<r>_<dp>/model` when
  `VLLM_USE_AOT_COMPILE` is on, logged as `Directly load AOT compilation from
  path ...` or `saved AOT compiled function to ...`.

Every rank of one configuration shares the key; each rank has its own
`rank_<r>_<dp>` directory under it. A key without `@ranks` (or a bare hash,
read as `torch_compile_cache/<hash>`) is present when any rank is there.

The check runs in the preparer pod already on each node (`kubectl exec`, no
GPU). That pod runs as root on Kubernetes, so it can read any file. To catch
files the engine could not read, it also checks the permissions for the
engine's user:

- By default the engine's user is the owner of the key directory: the engine
  that wrote the key. A file it owns needs owner read. A file owned by
  another user needs group read if it is in the key directory's group, and
  read for others if not. The check assumes the engine is in the key
  directory's group. The directories above a key, from the volume mount
  (`/engine-cache`) down, need only search (`x`) the same way: a `711`
  mount is fine. The key directory and every directory under it need read
  and search.
- vLLM writes its AOT files (`.../rank_0_0/model`) as mode `600`. That is
  fine when the engine owns them, which it does when it wrote them. They are
  `present`.
- A seeded or published key is owned by root: the preparer copied it there as
  root, and made every file readable and every directory readable and
  searchable by every user, so an engine that is not root can read it. By
  default it is judged for root, so it is `present (... for uid 0, ...)`,
  the summary warns, and the last line says `VERIFIED for root`.
- Set `ENGINE_CACHE_UID=<uid>` when the engines run as a different user than
  the one that owns the keys. The check then reads the permissions for
  that UID: owner read if it owns the file, read for others if not. Its
  groups are not known, so group permissions are not counted.

```bash
make engine-cache-verify NAMESPACE=<ns> ENGINE_CACHE_KEYS='<key>' ENGINE_CACHE_UID=1001
```

vLLM writes no symbolic links. A link under the key, or a linked directory on
the way to it, is `UNREADABLE` and is never followed. Seeding and publish do
not copy links. An earlier version copied with `cp -a`, which copies links,
so a link found here was made on that node or copied by that version.

`engine-cache-keys` takes no key from a log line saying an AOT save failed
(`unable to save AOT compiled function to ...`), or that a load failed and
the engine compiled again. It warns instead.

The read stops after 300 seconds, and after 280 inside the pod when its
image has `timeout`. The image needs `find` (with `-user`, `-group` and
`-perm`), `ls` and `awk` to read file modes and owners; without them every
key is `COULD NOT CHECK`, and the line names what is missing.

What the mode check cannot see:

- The engine's other groups are not known. A file in one of them with read
  for others but not for the group (mode `604`) is `present`, though the
  engine cannot read it. Under `ENGINE_CACHE_UID` no group is known at all.
- A setgid key directory gives its files its group. If the engine is not in
  that group, a `640` file there is `present` and still unreadable.
- ACLs are not read: only the mode bits are.
- An engine running as root with `CAP_DAC_OVERRIDE` reads any file, whatever
  the modes say.

</details>

#### What it costs

Costs and needs are the weights section's, with two changes:

- **Size:** a few gigabytes per engine config per node, not a model.
- **Contents: code.** torch.compile artefacts, FlashInfer and Triton kernels and Python bytecode
  that every engine on the node loads and runs. The four directories are 1777
  (any engine UID can write; sticky, so no UID removes another's files).
  Whoever can write the directory runs code in every engine on that node. On
  Kubernetes that is anyone with `pods/create` in a namespace that can mount the
  claim: no more than `pods/create` already grants, since such a pod runs as
  root and could overwrite the weights too. In a namespace that separates
  workloads by UID (Pod Security `restricted`, a `runAsUser` per Deployment) it
  is more; the 1770 variant below is for that. Across namespaces it is everyone
  pointed at the same directory.

So the weights rules hold harder here: one directory per trust domain, on a disk
that is not the node's own, never a system path (the script refuses the same
paths). Wipe `<dir>/engine-cache` on the nodes when a namespace or directory
changes hands: `delete` keeps the caches, and a new tenant with the same
namespace name and directory would load the old one's. 1777 is a trade-off so
the engines need no `fsGroup`. A namespace that runs its engines as one UID can
`chown` the four directories to it and `chmod 1770` them on each node.

Nothing cleans the cache. The bytecode alone grows about 150 MB per image per
node, and an old image's key stays after the image is gone: delete
`<dir>/engine-cache/pycache/<key>` on the nodes when you retire an image.

The rest is the same: rights to create PersistentVolumes (the claim's
`ENGINE_CACHE_CAPACITY`, 200Gi unless set, is what a storage quota charges), and
Pod Security `baseline` (the preparer mounts the claim, not a hostPath). On
OpenShift (by analysis, not yet run there) `restricted-v2` runs the preparer and
engines as the project's range UID, and the kubelet does not relabel a hostPath.
Prepare `<dir>/engine-cache` on each node as the weights section says
(`chgrp 0`, `chmod 2775`, `chcon -t container_file_t`, or a `context=` mount).
The preparer then creates the four directories as that UID, owns them, and its
`chmod 1777` takes. The files carry the project's SELinux level, so other
projects' pods cannot read them, unless the disk is mounted with `context=`; then
the directory split is the only separation.

The benchmark standup has the same measure under
`BENCHMARK_ENGINE_CACHE_HOSTPATH=<dir>` (default `BENCHMARK_MODEL_HOSTPATH`).
That, and the harness's own steps on the engine's start path, are in
[Benchmark the scaling manager](../guides/benchmarking/README.md#replica-start-time-in-the-harness).

## The engine's memory budget (`gpuMemoryUtilization`)

**`gpuMemoryUtilization` has to follow the model.** vLLM reads it as a fraction
of the card's **total** memory, not of what is free, and then fills that budget
with KV cache. A 32B model wants 0.95 of an 80GB card. A 0.6B model -- 1.12 GiB
of weights -- does not, and asking for 0.95 anyway makes it claim **73.5 GiB of
KV cache** and then fail:

```
Available KV cache memory: 73.53 GiB
torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 594.00 MiB.
GPU 0 has 79.18 GiB of which 500.69 MiB is free.
```

`make benchmark-standup` sets `GPU_MEM_UTIL` to **0.90** for every model, and
deliberately does *not* lower it for small models. Per-replica capacity IS the
KV cache size, so shrinking KV to make a small model scale sooner does the
autoscaler's job in the wrong layer, and wastes GPU to get what
`kvCacheThreshold` gives for free. Size the card for serving; set when a replica
counts as full in the [scaling policy](scaling-policy.md).

Expect this on a small model. At 0.90 a 0.6B model gets **651,328 tokens** of KV
cache, and one replica takes a lot of traffic without looking full. Measured: 121
concurrent requests, KV utilisation peaking at **10.5%**, and the scaling manager
correctly holding one replica, since nothing was saturated. To make a small model
scale on a small load, lower `kvCacheThreshold`; do not shrink the cache.

**Why it looks intermittent.** 0.95 works on an empty GPU, so the failure comes
and goes. The missing memory belongs to pods the *scheduler cannot see*: FMA
launcher pods request **zero** GPUs while holding the engine. Kubernetes places a
model on a card it thinks is empty, and vLLM measures 77.3 of 79.18 GiB free and
budgets as if it had all of it. Any co-tenant holding memory without requesting a
GPU does the same.

Override it on the standup when you need to:

```bash
make benchmark-standup BENCHMARK_NAMESPACE=<namespace> MODEL_ID=<model> GPU_MEM_UTIL=0.50
```

## Draining before scale-down

A replica removed while streaming takes its open responses with it. The client
gets a truncated body (`ClientPayloadError` from aiohttp, an incomplete stream
elsewhere), and the request is lost after its GPU time was spent.

The scaling manager cannot fix this. It decides the count; the pod spec decides
what happens to a departing pod, and on llm-d that spec belongs to the
modelservice chart (`managed-by: Helm`, release `<model>-ms`). Anything the
installer wrote there would be reverted by the next `helm upgrade`.

Know the cost before turning autoscaling on. Each scale-down causes a burst of
client-side stream failures, from when the replica goes until about a generation
later. Long generations make it worse: a pod is then mid-response for most of
its life.

Two fields on the **model server's** pod template fix it:

```yaml
spec:
  template:
    spec:
      # Long enough for the longest generation you are willing to wait for.
      terminationGracePeriodSeconds: 120
      containers:
      - name: vllm
        lifecycle:
          preStop:
            exec:
              # Stop taking new work, then let what is in flight finish.
              # The endpoint is removed from the Service as soon as the pod goes
              # Terminating, so this only has to outlast the requests already on it.
              command: ["/bin/sh", "-c", "sleep 30"]
```

`sleep` is crude but usually enough, though not for the obvious reason.
Endpoint removal is **not** instant: the pod is marked Terminating at once, but
the withdrawal reaches every kube-proxy and the EPP asynchronously, and they keep
routing to it meanwhile. So the window covers new arrivals still being sent here
as well as generations already running. That is why the emitted patch sleeps 45s,
not the 30s above. If the engine has a drain endpoint, call that instead.

Set `terminationGracePeriodSeconds` above the preStop duration plus the longest
generation. The grace period is the total budget; the kubelet sends SIGKILL when
it expires, whatever the hook is doing.

`make scaledobjects-plan` reports a missing hook per workload, because
registering a ScaledObject is when scale-down becomes possible:

```
note: Scale-down will cut in-flight requests: vllm declares no preStop hook,
      and terminationGracePeriodSeconds is 30. ...
```

### In llm-d modelservice values

The same two fields, where the chart keeps them, once per role.
**UNVERIFIED:** the chart is not vendored here, so these key paths (a per-role
`containers` list passed through to the pod, and a per-role
`terminationGracePeriodSeconds`) are not checked against its templates. Render
with `helm template` and confirm both fields reach the Deployment first.

```yaml
prefill:
  terminationGracePeriodSeconds: 120   # UNVERIFIED key path
  containers:
  - name: vllm
    lifecycle:
      preStop:
        exec:
          command: ["/bin/sh", "-c", "sleep 45"]
decode:
  terminationGracePeriodSeconds: 600   # UNVERIFIED key path; cover the longest generation
  containers:
  - name: vllm
    lifecycle:
      preStop:
        exec:
          command: ["/bin/sh", "-c", "sleep 45"]
```

Decode holds the long-running streams, so its grace period is the one to size
from the longest generation; a prefill request finishes in one forward pass.

**What to check in the chart before relying on the paths above.** Find which
chart and version your release runs, pull that chart, and read the paths off
its Deployment template:

```bash
helm list -n <ns>                                # the CHART column: name and version of <model>-ms
helm pull <repo>/<chart> --version <version> --untar -d /tmp/ms
grep -rn "terminationGracePeriodSeconds\|lifecycle\|preStop" /tmp/ms/*/templates/
```

The `.Values.…` expression each match reads **is** the values path. Per role
(`prefill`, `decode`) find two things: what fills
`spec.template.spec.terminationGracePeriodSeconds`, and how the `vllm`
container's `lifecycle` is filled (a values key rendered into it, or a role-level
`containers` list passed through whole). If neither is templated, the chart
cannot set the field; patch the rendered Deployment instead (reverted by the
next `helm upgrade`). Confirm on the rendered output, then on the live object
after the upgrade:

```bash
helm template <model>-ms <repo>/<chart> --version <version> -f values.yaml \
  | yq 'select(.kind == "Deployment") | [.metadata.name, .spec.template.spec.terminationGracePeriodSeconds, (.spec.template.spec.containers[] | select(.name == "vllm") | .lifecycle.preStop)]'
kubectl get deploy -n <ns> -o custom-columns=NAME:.metadata.name,GRACE:.spec.template.spec.terminationGracePeriodSeconds
```

A null grace period or preStop in either output means the key path is wrong for
your chart version. Last check: `make scaledobjects-plan` drops the
`Scale-down will cut in-flight requests` note once the engine container has a
preStop hook. It ignores the grace period (`so_drain_note` in
`deploy/lib/scaledobject.sh`), so check that with the `kubectl` line above.

### On a LeaderWorkerSet

`make workload-patch` writes the cache halves of a LeaderWorkerSet, not its
drain half.

- **What it writes.** One `LeaderWorkerSet` document: the weights volume, and
  the engine-cache volume with its variables (`PYTHONPYCACHEPREFIX` included),
  under `spec.leaderWorkerTemplate.leaderTemplate` and `.workerTemplate`. Each
  template is judged on its own, with its own engine container and image key.
  With no `leaderTemplate` the leader is built from `workerTemplate`, so only
  that one is written. A worker whose command names no model (a Ray worker)
  takes the leader's model for the weights half. A model passed in a variable
  (`vllm serve $(MODEL)`) is not read; the console says so for each template.
- **Applying it.** `WVA_WORKLOAD_PATCH_APPLY=true` with
  `WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true` or `WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true`
  sends it as a JSON patch (`--type=json`), after the same checks as a
  Deployment, in both templates. It only adds: `env`, `volumeMounts` and
  `volumes` are appended to where they exist. (The API server refuses a
  strategic merge on a custom resource, and `--type=merge` replaces whole
  lists.) With `APPLY` alone nothing is sent; there is no drain half.
- **Applying it restarts every group.** A new template replaces each group
  whole, one group at a time, and by default the old group goes before the new
  one starts. At `replicas: 1` the model serves nothing for one multi-node
  start; with more groups it runs one group short for each. The console names
  the replicas and the group size when it applies.
- **It is applied only to an LWS nothing else controls.** One with an
  `ownerReferences` entry marked `controller: true` is refused, naming the
  owner: the owner would put its own spec back. Put the change there.
- **It cannot land on a changed object.** The checks and the patch use one
  read of the LWS, and the patch tests its `resourceVersion` first. If the LWS
  changed in between, the API server refuses the whole patch and nothing is
  applied; run `make workload-patch` again.
- **The preparer check counts the preparer's nodes**, not the nodes the LWS can
  land on: it does not read the LWS's node selector, tolerations or affinity.
  Make sure the preparer's `WEIGHTS_NODE_SELECTOR` covers them.
- **The guard** goes in the leader command and in the worker command.
- **Every node needs the cache.** Each pod reads its own node's copy. Run
  `make engine-cache` on every node the group can land on, and
  `make engine-cache-publish` after the first start: it gives every node every
  rank's compile cache, and the bytecode.

**Drain is not covered.** Killing a worker aborts the leader's in-flight
generations whatever hook the leader has, so a preStop on each pod does not
drain the group. The console says so for every LWS until both templates have a
preStop and the worker's grace period is at least the leader's. Do it by hand,
in the manifest or chart that owns the LWS:

```yaml
spec:
  leaderWorkerTemplate:
    leaderTemplate:            # if absent, workerTemplate is used for the leader too
      spec:
        terminationGracePeriodSeconds: 600
        containers:
        - name: vllm
          lifecycle:
            preStop:
              exec:
                command: ["/bin/sh", "-c", "sleep 45"]
    workerTemplate:
      spec:
        # At least the leader's: a worker that leaves first ends the group's
        # generations early.
        terminationGracePeriodSeconds: 600
        containers:
        - name: vllm
          lifecycle:
            preStop:
              exec:
                command: ["/bin/sh", "-c", "sleep 45"]
```

To patch a live LWS instead, use a JSON patch (`kubectl patch lws <name> -n <ns>
--type=json -p '[{"op":"add","path":"/spec/leaderWorkerTemplate/workerTemplate/spec/terminationGracePeriodSeconds","value":600}, ...]'`)
— a `--type=merge` patch replaces the whole `containers` list. Both are reverted
by the next `helm upgrade` if a chart owns the object.

## Writing the patch: `make workload-patch`

The sections above share one shape: a pod spec that is fine at a fixed replica
count and costly once the count changes. `make workload-patch` writes the fix
for three of them: the drain hook, the weights volume, and the engine-cache
volume (for vLLM's `VLLM_CACHE_ROOT` and its two siblings, when they sit on no
volume that outlives the pod and every start compiles from nothing; and for
`PYTHONPYCACHEPREFIX`, on any Python engine):

```bash
make workload-patch                       # everything in scope
make workload-patch NAMESPACE=my-models   # one namespace
```

### The whole flow, when it reports all three problems

```bash
# 1. What is missing, and why it costs something. Changes nothing.
make workload-patch NAMESPACE=my-models

# 2. Only if it reported re-downloaded weights: create the shared cache.
#    Run it with no size or class first — it lists the StorageClasses this
#    cluster is already serving ReadWriteMany from, which is the question
#    "which class do I use?" answered from what already works here.
make model-cache NAMESPACE=my-models
make model-cache NAMESPACE=my-models \
    WVA_MODEL_PVC_SIZE=500Gi WVA_MODEL_PVC_CLASS=<an-rwx-class>

# 3. Only if it reported compiling from nothing: prepare the engine cache on
#    the nodes. The emitted file says what <node dir> must be (the list under
#    "Engine caches on the node's disk" above), or set
#    WVA_ENGINE_CACHE_CLAIM=<claim>[:<subPath>] on the next step to use a shared
#    ReadWriteMany claim you already have instead, and skip this. The seed is
#    optional and is what makes the FIRST start on each node warm too.
make engine-cache NAMESPACE=my-models ENGINE_CACHE_PATH=<node dir> ENGINE_CACHE_IMAGE=<engine image>     ENGINE_CACHE_SEED_CLAIM=<a claim holding caches>[:<subPath>]

# 4. Apply. The drain half needs nothing; each storage half needs its claim from
#    step 2 or 3 and its own opt-in, because mounting storage is a bigger change
#    than adding a hook.
make workload-patch NAMESPACE=my-models \
    WVA_WORKLOAD_PATCH_APPLY=true \
    WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true \
    WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true
```

Step 4 patches the live objects, which **replaces their pods**. The durable
way is to copy the emitted file into your modelservice values and let the chart
roll it out; the next `helm upgrade` reverts anything applied directly. Steps 2
and 3 are the same either way: the claims are yours, not the chart's. The patch
cannot carry the guard (an unwritable cache path costs a compile, not the
replica): it lives in the container command, which belongs to the chart. The
emitted file has it as a comment beside the engine-cache half; put it in the
command.

`make model-cache` is safe to re-run: an existing claim is reported and left
alone.

**A storage half is refused, with the reason, when it would break something:**
a volume of that name already exists on the workload (a strategic merge cannot
replace it, so the API server rejects the whole object, drain hook included);
something is already mounted at that path; or the claim does not exist (every
new pod would stay `Pending`). The drain half is still applied, the other
storage half is judged on its own, and the refused half stays in the file.

The output is `wva-workload-patch.yaml`: one document per model server that
needs something, naming the engine container, with a comment saying which of the
three problems it has. A document with two storage halves carries one `env`, one
`volumeMounts` and one `volumes` list for both.

**It writes a file rather than applying it, on purpose.** The pod spec belongs
to the model server's chart. A server-side apply is refused
(`conflict with helm ... .spec.template.spec.terminationGracePeriodSeconds`),
and a patch that does land is silently reverted by the next `helm upgrade`. Put
the contents in your modelservice values, where they survive.

**A missing cache is not a hard failure, but it is reported loudly.** A model
server with no persistent weights autoscales correctly; it is just expensive to
scale. The cost shows only when a replica is added, and looks like the
autoscaler being slow, so it is named per workload on the console:

```text
[WARNING]   ns/qwen-decode RE-DOWNLOADS ITS WEIGHTS on every scale-up: vllm
            fetches Qwen/Qwen3-0.6B from Hugging Face, outside every volume it mounts.
[WARNING] 1 of 1 model server(s) will RE-DOWNLOAD their weights every time a replica is added.
```

On a cluster with no egress it is a failure, not a cost, first seen at the
first scale-up.

The engine cache is reported the same way, per workload (`COMPILES FROM
NOTHING on every start`) and in the summary (`N model server(s) COMPILE FROM
NOTHING every time a replica is added`). It also costs nothing at a fixed count
and tens of seconds per scale-up (measured: 14 s of `torch.compile` against 3 s
warm, and 8 s more before the API server answered). The emitted document names
the claim it mounts (`engine-cache`, or `WVA_ENGINE_CACHE_CLAIM`), says whether
it exists, and if not, what `make engine-cache` needs of a node directory and
how to do without one. Only vLLM has the variable; for another engine the
detector has no opinion, which is not a clean bill.

Python bytecode is reported for any Python engine, vLLM or SGLang
(`RECOMPILES ITS PYTHON BYTECODE on every start`; with the vLLM caches, as part
of that warning). The patch sets `PYTHONPYCACHEPREFIX` to
`<mount>/pycache/<image key>` ([Python bytecode](#python-bytecode)):

- With the vLLM caches, under the same new `engine-cache` mount.
- When the engine already keeps its vLLM cache on a volume, or mounts one at
  `/engine-cache` (`WVA_ENGINE_CACHE_PATH`), under that mount: a variable and
  no new volume. This half is emitted, never applied.
- Otherwise (SGLang, usually) with the `engine-cache` volume, mounted for the
  bytecode alone; create the claim with `make engine-cache` first.

Each of them comes with the guard, which now also covers `PYTHONPYCACHEPREFIX`;
put it in the command. A workload that already sets `PYTHONPYCACHEPREFIX`, by
value or from a ConfigMap or Secret, is left as it is. If its value ends in
`pycache/<key>` and the key belongs to another image, the console says so and
names the right key. A workload that sets `PYTHONDONTWRITEBYTECODE` gets no
bytecode half. A vLLM workload whose `VLLM_CACHE_ROOT` comes from a ConfigMap or
Secret is reported but gets no half either: set `PYTHONPYCACHEPREFIX` beside
your own cache root. On a LeaderWorkerSet it is written for the leader and the
worker, each with its own image key ([On a LeaderWorkerSet](#on-a-leaderworkerset)).

`WVA_WORKLOAD_PATCH_APPLY=true` also applies the **drain half** to the live
objects. Read what it prints first:

- **It replaces pods.** A new pod template rolls every model server it touches,
  *before* the hook exists, so the first rollout still cuts in-flight requests.
  With the default strategy at `replicas: 1` the new pod must schedule before the
  old one goes, so with no spare GPU the rollout stalls with the old pod serving.
- **Each storage half has its own opt-in**: `WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true`
  for the weights volume, `WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true` for the
  engine cache. Mounting storage is a bigger change than a hook: the API server
  can refuse it outright, and it changes where an engine reads weights or writes
  code it will load. With the opt-in a half is applied only where it cannot
  break the workload: no volume of that name, nothing mounted at that path, and
  the claim exists, is `Bound` and is `ReadWriteMany` (a `ReadWriteOnce` claim
  binds to one node, so a second replica could not schedule). Otherwise it
  refuses that half, says why, and applies the rest. The engine-cache half has
  one more check: with the claim `make engine-cache` makes, every accelerator
  node its preparer selects must be prepared (`make engine-cache-status` names
  the others). It is applied **without the guard** (the command belongs to the
  chart), and says so: until the guard is in the chart, a node with an
  unwritable cache path loses the replica, not a compile.

  The volume-name check matters most. `volumes` merges with `retainKeys`, which
  only `kubectl apply` generates, so `kubectl patch --type=strategic` merges
  *into* a same-named volume and the API server rejects the whole object
  (`may not specify more than 1 volume type`), drain hook included.

| variable | default | what it sets |
| --- | --- | --- |
| `WVA_WORKLOAD_PATCH_FILE` | `wva-workload-patch.yaml` | where the patch is written |
| `WVA_WORKLOAD_PATCH_APPLY` | `false` | `true` also applies the drain half to the live workloads |
| `WVA_DRAIN_GRACE_SECONDS` | `120` | `terminationGracePeriodSeconds` in the emitted patch. An existing longer value is kept, never lowered |
| `WVA_DRAIN_SLEEP_SECONDS` | `45` | the preStop sleep — the drain window itself |
| `WVA_MODEL_PVC_NAME` | discovered, else `model-pvc` | the claim the emitted weights volume names. Setting it wins over discovery |
| `WVA_MODEL_VOLUME_NAME` | `model-storage` | the volume name in the emitted patch |
| `WVA_MODEL_CACHE_PATH` | `/model-cache` | where that volume is mounted, and the parent of the emitted `HF_HOME` |
| `WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE` | `false` | `true` also applies the engine-cache half where it cannot break the workload |
| `WVA_ENGINE_CACHE_CLAIM` | `engine-cache` | the claim the emitted engine-cache volume names, `<claim>[:<subPath>]` — the one `make engine-cache` makes, or a shared RWX claim you already have (whoever can write it runs code in every engine; see above) |
| `WVA_ENGINE_CACHE_VOLUME_NAME` | `engine-cache` | the volume name in the emitted patch |
| `WVA_ENGINE_CACHE_PATH` | `/engine-cache` | where that volume is mounted, and the parent of the emitted `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR` and `TRITON_CACHE_DIR` |

The `*_NAME` and `*_PATH` variables affect the emitted file only — without the
opt-in nothing applies a volume.

What it will and will not do:

- **It reuses a claim you already have, when one is unambiguous.** A namespace
  whose cache is `llm-d-model-cache` should not be told to use `model-pvc`, or
  someone provisions a second terabyte for weights already on the cluster. Only
  a **Bound** `ReadWriteMany` claim qualifies (`ReadOnlyMany` would mount
  read-only where the engine downloads). With several candidates it takes the
  one whose name says what it holds; with two equally plausible ones it names
  none and falls back to the default.
- **`workload-patch` does not create the PersistentVolumeClaims.** `make
  model-cache` makes the weights one and `make engine-cache` the engine-cache
  one, as separate steps. For the weights claim two fields decide whether the
  cache helps or breaks scale-up, and neither is guessed:

  - **`accessModes` must be `ReadWriteMany`.** Many replicas reading one copy is
    the point. A cluster's *default* StorageClass is often RWO block storage,
    and a ReadWriteOnce claim binds to one node, so the second replica cannot
    schedule. A claim on the wrong class turns a slow scale-up into a failed
    one, which is worse than no cache.
  - **`storage` depends on how many models share it and their size.** At about
    two bytes per parameter, a 0.6B model is ~1.2 GiB and a 70B ~140 GB; size
    for every model that will share it, plus room.

  ```bash
  make model-cache NAMESPACE=<ns>     # lists classes this cluster serves RWX from
  make model-cache NAMESPACE=<ns> WVA_MODEL_PVC_SIZE=<size> WVA_MODEL_PVC_CLASS=<class>
  ```

  Run bare, it lists the classes this cluster already has Bound RWX claims on,
  rather than a table of provisioner names that would go stale. It is safe to
  re-run: an existing claim is reported, not touched. A `WaitForFirstConsumer`
  class stays `Pending` until a pod mounts it; that is correct, and reported
  rather than waited on.

  It is not part of the install, and `undeploy-wva` does not delete the claim:
  it holds data, and removing it would destroy someone's weights or silently
  leave a terabyte behind. That is the operator's call. The emitted patch also
  carries the claim as a commented manifest, for anyone who prefers to apply
  their own.
- **It writes no drain half for a LeaderWorkerSet, and says so.** A multi-node
  group must drain the worker template too (killing a worker aborts the
  leader's in-flight generations), so a hook on each pod would be a confident,
  wrong answer. The cache halves are written
  ([On a LeaderWorkerSet](#on-a-leaderworkerset)).
- **It never reports a workload it could not read as healthy.** A failed listing
  or `get` is reported and makes the run exit non-zero, as does a missing `jq`
  (or `yq`, needed only when applying). "Nothing to patch" is never said about
  pods nobody could read.

The emitted file opens with a header saying most of this, because it otherwise
looks like something to `kubectl apply -f`, the one thing not to do with it.
The header carries a marker line, and only a file with that marker is deleted
once the workloads stop needing a patch; a file you wrote at your own
`WVA_WORKLOAD_PATCH_FILE` path is left alone.

`make benchmark-standup` and `make benchmark-add-variant` run this with
`APPLY=true`, pinned to the benchmark namespace, and wait for the rollout: those
model servers are ours, and a run whose scale-downs truncate requests measures
the harness, not the autoscaler.
