# Share GPUs between namespaces with a namespace-scoped install

llm-d serves models in several namespaces. This guide puts them under one GPU
budget, with a scaling manager that has no cluster-wide rights on workloads.
It runs in a namespace of its own and manages only the namespaces that run
llm-d. When one model is busy, it takes GPUs that a model in another of those
namespaces is not using. It gives them back when its load drops.
**Experimental.**

**Which install do I need?**

- **This guide**: the scaling manager may read and change workloads only in
  the llm-d namespaces. It gets a Role in each of them, and nothing
  cluster-wide on workloads.
- **[One install for the whole cluster](../utilization-share-cluster-wide/)**:
  the scaling manager may read every namespace. Pick that one if you want the
  cluster's free GPUs, with no quota, as the budget (option B there). Here the
  budget is always a quota; option B adds the cluster's GPUs for placement.
- **One namespace only**: see [Turn on the utilization-share optimizer,
  safely](../utilization-share/).

To learn what sharing costs first, see [the well-lit
path](../../well-lit-paths/utilization-share/).

## Before you begin

You need:

- A Kubernetes cluster, and cluster-admin rights on it for step 9.
- llm-d serving in two or more namespaces. Each namespace has its model
  servers, an EPP and an InferencePool. A fresh llm-d install is enough: the
  installer adds the monitoring llm-d leaves out.
- GPU nodes. The models that share must run on one GPU product. Every model
  server must ask for its GPUs (`nvidia.com/gpu`) and name the product in its
  `nodeSelector` or `nodeAffinity` (`nvidia.com/gpu.product`).
- For option C only: Kueue, with a ClusterQueue that holds a GPU quota, and
  a LocalQueue on it in each model namespace. Step 8 checks them.
- `kubectl`, `helm`, `git`, `jq`, `yq`, `make` and `openssl`.
- A checkout of this repository. Run every command from its top directory, in
  one shell: steps 6 and 8 set variables that the later steps read.

KEDA and Prometheus are optional. If the cluster has none, the installer adds
them.

**Time.** Steps 1 to 14 take about 20 minutes. Then leave the optimizer in
shadow for at least one busy period of each model, typically a day. Turning it
on and seeing the first move takes about 10 minutes more.

**What this changes in your cluster.**

- A new namespace for the scaling manager, `workload-variant-autoscaler-system`
  unless step 6 names another.
- In each llm-d namespace: a Role and RoleBinding for the scaling manager, a
  PodMonitor and a ServiceMonitor where nothing scrapes the model servers or
  the EPP yet, and one ScaledObject for each model server.
- A few cluster roles that no Role can express. None of them changes
  workloads:
  - node read, to resolve the GPU product;
  - Namespace read, limited by name to the llm-d namespaces;
  - list on Kueue ClusterQueues, Cohorts and ResourceFlavors, which cannot be
    limited by name;
  - the metrics authentication;
  - on OpenShift, `cluster-monitoring-view`, so the scaling manager can query
    the platform's Thanos;
  - option B only: list on Pods in every namespace, so the GPU count sees the
    GPUs other namespaces hold on the same nodes.
- KEDA, in `keda-system`, if the cluster has none. A Prometheus, if the
  cluster has none.
- No model server restarts. KEDA now sets each model's replica count.

Every step can be run again. Step 19 turns sharing off. Step 20 removes
everything the installer added, except KEDA and Prometheus.

## Check the cluster

### Step 1: Check your tools

<!-- guide:steps.tools start -->
```bash
# Prints nothing when every tool is there.
for t in kubectl helm git jq yq make openssl; do command -v $t >/dev/null || echo "missing: $t"; done
```
<!-- guide:steps.tools end -->

You should see nothing. If a tool is listed, install it.

### Step 2: Check you are a cluster admin

<!-- guide:steps.admin start -->
```bash
# Prints yes for a cluster admin.
kubectl auth can-i '*' '*' --all-namespaces
```
<!-- guide:steps.admin end -->

You should see `yes`. If you see `no`, you can still run every read-only
step. For step 9, render it for a cluster admin instead: step 9 says how.
Then go on from step 10 once they have run it.

### Step 3: Check llm-d serves in each namespace

<!-- guide:steps.pools start -->
```bash
# One InferencePool in each model namespace.
kubectl get inferencepools -A
```
<!-- guide:steps.pools end -->

You should see one pool in each model namespace:

```text
NAMESPACE   NAME                 AGE
team-a      optimized-baseline   14m
team-b      optimized-baseline   13m
```

If a namespace is missing, install llm-d there first.

### Step 4: Check each model pod asks for GPUs

<!-- guide:steps.podgpus start -->
```bash
# The GPUs each model pod asks for, and the node it runs on.
kubectl get pods -A -l 'llm-d.ai/role in (decode,prefill)' \
  -o custom-columns='NAMESPACE:.metadata.namespace,POD:.metadata.name,GPUS:.spec.containers[*].resources.limits.nvidia\.com/gpu,NODE:.spec.nodeName'
```
<!-- guide:steps.podgpus end -->

You should see a number in `GPUS` on every model pod:

```text
NAMESPACE   POD                               GPUS   NODE
team-a      model-a-decode-65988cc5bc-czdb8   1      nsm-kind-worker
team-a      model-a-decode-65988cc5bc-lfnrn   1      nsm-kind-worker
team-b      model-b-decode-56b7ff596b-g8pkf   1      nsm-kind-worker
team-b      model-b-decode-56b7ff596b-vpnn6   1      nsm-kind-worker
```

If `GPUS` is empty, add `nvidia.com/gpu` to that model's resource limits.

### Step 5: Find the GPU product

<!-- guide:steps.products start -->
```bash
# The GPU product of each node.
kubectl get nodes -L nvidia.com/gpu.product
```
<!-- guide:steps.products end -->

You should see a product on the nodes from step 4:

```text
NAME                     STATUS   ROLES           AGE   VERSION   GPU.PRODUCT
nsm-kind-control-plane   Ready    control-plane   14m   v1.31.2
nsm-kind-worker          Ready    <none>          14m   v1.31.2   NVIDIA-A100-PCIE-80GB
```

Write down the product of the nodes the models run on. Step 8 needs it.

## Install

### Step 6: Say which namespaces it manages

`WVA_WATCH_NS=auto` means every namespace that runs llm-d model servers or an
InferencePool. The installer finds them once, here, and writes the list into
the scaling manager. It skips a namespace annotated
`wva.llmd.ai/exclude: "true"`, and one another scaling manager already
manages, and warns about each.

`CONTROLLER_NAMESPACE` is where the scaling manager runs. The default is
`workload-variant-autoscaler-system`; to use another, set it here, for example
`export CONTROLLER_NAMESPACE=llm-scaling`. Every later step reads it, from the
install to the uninstall. (`WVA_NS` is the same setting; set one of them.)

<!-- guide:steps.names start -->
```bash
# Manage every namespace that runs llm-d, from the scaling manager's own namespace. Every later step reads both.
export WVA_WATCH_NS=auto
export CONTROLLER_NAMESPACE=workload-variant-autoscaler-system
```
<!-- guide:steps.names end -->

This prints nothing. To manage only some of them, name them instead:
`export WVA_WATCH_NS=team-a,team-b`.

### Step 7: Run the preflight

This command only reads. It checks each namespace the scaling manager will
manage.

<!-- guide:steps.check start -->
```bash
# Read-only. Checks the tools, your rights, KEDA, Prometheus, and each namespace it will manage.
make check-prereqs KEDA_HELM_INSTALL=true
```
<!-- guide:steps.check end -->

You should see the two namespaces it found, each passing, and `Preflight
passed` at the end:

```text
[INFO] Namespace: workload-variant-autoscaler-system  (the default)
[INFO]   It runs in workload-variant-autoscaler-system and manages team-a, team-b (WVA_WATCH_NS=auto: every namespace with llm-d).
[INFO] Policy: this controller manages team-a, team-b from workload-variant-autoscaler-system, so it reads its limiters from its own ConfigMap there (wva-scaling-policy-config), which this install writes.
...
[INFO] Checking the 2 namespace(s) this controller manages (WVA_WATCH_NS):

[INFO] Namespace team-a
[WARNING]   EPP flow control: off on optimized-baseline-epp. Not refused: scaling works without it. Two things do not:
...
[SUCCESS]   team-a: passed (setup adds the EPP ServiceMonitor, EPP flow control off: no scale from zero).
...
[INFO] Namespace summary:
[SUCCESS]   team-a: passed (setup adds the EPP ServiceMonitor, EPP flow control off: no scale from zero)
[SUCCESS]   team-b: passed (setup adds the EPP ServiceMonitor, EPP flow control off: no scale from zero)
[SUCCESS] Namespaces: all 2 passed.
...
[SUCCESS] Preflight passed for ENVIRONMENT=kubernetes, WVA_SCOPE=namespace, WVA_LIMITER=<unset: keeps the live limiter>
```

The `EPP flow control: off` warning is expected on a fresh llm-d install. It
does not stop the install; see [Details](#details).

If not:

- **A namespace `failed` with `No llm-d found`.** You named a namespace with
  no model servers. Leave it out of `WVA_WATCH_NS`.
- **`WVA_WATCH_NS=auto found no namespace with llm-d`.** Install llm-d first,
  or name the namespaces.
- **`includes workload-variant-autoscaler-system`.** The scaling manager must
  run outside the namespaces it manages. Leave that namespace out of the list.
- **A namespace `failed` for another reason.** The lines under it say what is
  missing. Fix it, then run step 7 again.

### Step 8: Choose the budget

The budget is a GPU count: how many GPUs of one product the models in all the
managed namespaces may hold together. Pick one option. Steps 9 and 10 then
have a part for each, and option C adds a check here; every other step is
the same.

- **Option A: quota.** The GPU count alone. A replica that fits the count is
  allowed, wherever the scheduler then puts it.
- **Option B: quota plus the cluster's GPUs.** The same count, and the
  scaling manager also reads the nodes (`gpu-inventory`), so it adds a
  replica only where a node has a free GPU of that product. Pick it when other
  work holds GPUs on the same nodes. It needs two more read-only grants:
  node read and list on Pods in every namespace.
- **Option C: Kueue.** The count is the GPU quota of Kueue's ClusterQueues.
  Pick it when the cluster already uses Kueue to share GPUs. It needs a
  ClusterQueue with a GPU quota (`nominalQuota` on `nvidia.com/gpu`), a
  LocalQueue on it in each managed namespace, and each model server on that
  LocalQueue: the label `kueue.x-k8s.io/queue-name: <LocalQueue>` on its pod
  template. It needs no more grants than option A: the Kueue reads are
  already there, limited to the managed namespaces. Change the quota in
  Kueue, and the budget follows within a minute.

The cluster's GPUs alone, with no quota, is not offered here: every
namespace-scoped scaling manager would plan the same free GPUs. For that one,
use [the cluster-wide install](../utilization-share-cluster-wide/).

For options A and B, set the GPU product from step 5, and the GPU count:

<!-- guide:steps.vars start -->
```bash
# The GPU product from step 5, and how many of its GPUs the models may hold together.
export ACCELERATOR=<gpu product>
export GPUS=<gpus>
```
<!-- guide:steps.vars end -->

This prints nothing.

For option C, set the GPU product from step 5. Kueue gives the number:

<!-- guide:steps.varskueue start -->
```bash
# Option C: Kueue. The GPU product from step 5. Kueue's ClusterQueues give the number.
export ACCELERATOR=<gpu product>
```
<!-- guide:steps.varskueue end -->

This prints nothing. Then check the queues. This only reads:

<!-- guide:steps.checkkueue start -->
```bash
# Option C: Kueue. Read-only. Checks each managed namespace has a LocalQueue on a GPU ClusterQueue, with its model servers on it.
make check-prereqs KEDA_HELM_INSTALL=true \
  WVA_LIMITER=quota WVA_QUOTA_SCOPE=cluster WVA_QUOTAS="${ACCELERATOR}=-1" WVA_QUOTA_KUEUE=true
```
<!-- guide:steps.checkkueue end -->

You should see each namespace on its LocalQueue, the budget Kueue gives, and
`Preflight passed`:

```text
[INFO] Checking the 2 namespace(s) this controller manages (WVA_WATCH_NS):

[INFO] Namespace team-a
[SUCCESS]   Kueue budget: LocalQueue serving -> ClusterQueue inference (4 GPUs).
[SUCCESS]   Kueue budget: the model servers in team-a are on LocalQueue serving.
[SUCCESS]   team-a: passed (setup adds the model-server PodMonitor, setup adds the EPP ServiceMonitor, EPP flow control off: no scale from zero, on Kueue LocalQueue serving).
...
[SUCCESS] Kueue budget: 4 GPUs, the GPU quota of ClusterQueue(s) inference, shared by every managed namespace; WVA_QUOTAS can only lower it.
[SUCCESS] Namespaces: all 2 passed.
[SUCCESS] Preflight passed for ENVIRONMENT=kubernetes, WVA_SCOPE=namespace, WVA_LIMITER=quota
```

If not:

- **`no LocalQueue in team-a`.** Create one on the GPU ClusterQueue. The line
  under it prints the `kubectl create` command.
- **`carries no kueue.x-k8s.io/queue-name label`.** Add the label to the model
  server's pod template, with the `kubectl patch` it prints, or in your llm-d
  values. Its pods restart.
- **`run outside Kueue`.** The pods started before the label. The patch above
  replaces them.
- **`which declares no GPU resource`** or **`does not admit team-a`.** Point
  the LocalQueue at a ClusterQueue with a GPU quota whose `namespaceSelector`
  selects the namespace.
- **`cannot tell whether team-a has a LocalQueue`.** You may not list
  LocalQueues there. Run it as a cluster admin.

### Step 9: Install the prerequisites

This is the cluster-admin step. It takes about 3 minutes. It creates the
scaling manager's namespace, a Role in each managed namespace, the few cluster
roles listed above, the monitors the preflight said were missing, and KEDA if
the cluster has none.

**Option A: quota.**

<!-- guide:steps.setup start -->
```bash
# Option A: quota. Cluster admin, once. A Role in each managed namespace, the monitors, and KEDA only if the cluster has none.
make setup-prereqs KEDA_HELM_INSTALL=true
```
<!-- guide:steps.setup end -->

You should see the monitors and the Role in each namespace, and KEDA:

```text
[INFO] Adding the monitors llm-d did not install (DEPLOY_LLMD_MONITORS=false skips this):
[SUCCESS]   team-a: added PodMonitor wva-modelserver-metrics, ServiceMonitor wva-epp-metrics for optimized-baseline-epp
[SUCCESS]   team-b: added PodMonitor wva-modelserver-metrics, ServiceMonitor wva-epp-metrics for optimized-baseline-epp
...
[INFO] Granting the controller its permissions in team-a (it runs in workload-variant-autoscaler-system)
[SUCCESS]   Role/wva-manager-role-1d8cfc15 and RoleBinding/wva-manager-rolebinding-1d8cfc15 in team-a
[INFO] Granting the controller its permissions in team-b (it runs in workload-variant-autoscaler-system)
[SUCCESS]   Role/wva-manager-role-1d8cfc15 and RoleBinding/wva-manager-rolebinding-1d8cfc15 in team-b
...
==========================================
 Prerequisites ready for workload-variant-autoscaler-system
==========================================
  ...
    KEDA: installed by this run (Helm)
    Monitors added in team-a: PodMonitor wva-modelserver-metrics, ServiceMonitor wva-epp-metrics for optimized-baseline-epp
    Monitors added in team-b: PodMonitor wva-modelserver-metrics, ServiceMonitor wva-epp-metrics for optimized-baseline-epp
```

**Option B: quota plus the cluster's GPUs.**

<!-- guide:steps.setupphysical start -->
```bash
# Option B: quota plus the cluster's GPUs. As option A, and the read-only Pod list and node read the GPU count needs.
make setup-prereqs KEDA_HELM_INSTALL=true \
  WVA_LIMITER=gpu-inventory,quota WVA_QUOTA_SCOPE=cluster WVA_QUOTAS="${ACCELERATOR}=${GPUS}"
```
<!-- guide:steps.setupphysical end -->

You should see what option A shows, and the Pod list:

```text
[SUCCESS]   Role/wva-manager-role-1d8cfc15 and RoleBinding/wva-manager-rolebinding-1d8cfc15 in team-b
[SUCCESS]   ClusterRole/wva-pod-list-role-1d8cfc15: list Pods in every namespace, so gpu-inventory sees the GPUs other namespaces hold on the same nodes
...
 Prerequisites ready for workload-variant-autoscaler-system
```

**Option C: Kueue.**

<!-- guide:steps.setupkueue start -->
```bash
# Option C: Kueue. As option A, and the Kueue reads, granted for the managed namespaces only.
make setup-prereqs KEDA_HELM_INSTALL=true \
  WVA_LIMITER=quota WVA_QUOTA_SCOPE=cluster WVA_QUOTAS="${ACCELERATOR}=-1" WVA_QUOTA_KUEUE=true
```
<!-- guide:steps.setupkueue end -->

You should see what option A shows, and the Kueue reads:

```text
[SUCCESS] Kueue budget: 4 GPUs, the GPU quota of ClusterQueue(s) inference, shared by every managed namespace; WVA_QUOTAS can only lower it.
...
[SUCCESS]   Role/wva-manager-role-1d8cfc15 and RoleBinding/wva-manager-rolebinding-1d8cfc15 in team-b
...
 Prerequisites ready for workload-variant-autoscaler-system
  ...
    ClusterRole/wva-kueue-reader-role-1d8cfc15
  ...
    ClusterRoleBinding/wva-kueue-reader-rolebinding-1d8cfc15
```

No ClusterRole to read Workloads in every namespace is created: one budget
for every namespace counts no cohort borrowing.

**If you are not a cluster admin, render this step for one.** This only
reads, so read rights are enough. It writes the step to `admin-setup/` and
applies nothing. For option B or C, add the same settings as that option's
command above:

<!-- guide:steps.render start -->
```bash
# Not a cluster admin? Option A, read-only. Writes the step to admin-setup/ for an admin to run, and applies nothing. For option B or C, add that option's settings.
make setup-prereqs KEDA_HELM_INSTALL=true RENDER=admin-setup
```
<!-- guide:steps.render end -->

It ends with what it wrote and the command for the admin:

```text
[SUCCESS] Rendered the cluster-admin phase for workload-variant-autoscaler-system into /home/you/admin-setup. Nothing was applied.
[INFO]   admin.yaml: 54 objects
...
[INFO] Give both files to a cluster admin, who reviews them and runs:
[INFO]     bash /home/you/admin-setup/admin.sh
```

`admin.yaml` holds every object this step creates, the Role in each managed
namespace among them. `admin.sh` holds the Helm installs and anything else
that is not a plain object, each with a comment saying why, then applies
`admin.yaml`. Send both files to the admin. They need `kubectl` and `helm`,
not this repository:

<!-- guide:steps.adminrun start -->
```bash
# The cluster admin, in the directory you gave them. No checkout needed.
bash admin-setup/admin.sh
```
<!-- guide:steps.adminrun end -->

`admin.yaml` holds private keys (the scaler's certificates), so send it
privately. Render again after anything on the cluster changes: the render
describes the cluster as it was when you ran it.

With KEDA already on the cluster, the line reads `KEDA: already on the
cluster`. Where something already scrapes a namespace, no monitor is added
there.

If not:

- **It cannot tell where KEDA runs.** Run it again with
  `KEDA_NAMESPACE=<the namespace KEDA runs in>`.
- **`no Prometheus Operator CRDs, so no monitors can be added`.** Your
  Prometheus does not use the Prometheus Operator. Scrape the model servers
  and the EPP your own way, then run step 7 again.

### Step 10: Install the scaling manager

It takes about 2 minutes. `WVA_QUOTA_SCOPE=cluster` makes the namespaces
share one budget.

**Option A: quota.**

<!-- guide:steps.install start -->
```bash
# Option A: quota. The scaling manager, with one GPU budget for every namespace it manages.
make deploy-wva INSTALL_PHASE=wva \
  WVA_LIMITER=quota WVA_QUOTA_SCOPE=cluster WVA_QUOTAS="${ACCELERATOR}=${GPUS}"
```
<!-- guide:steps.install end -->

You should see each namespace pass, the namespaces it manages, the quota, and
`Deployment on kubernetes complete!`:

```text
[INFO] Checking the 2 namespace(s) this controller manages (WVA_WATCH_NS):
...
[SUCCESS] Namespaces: all 2 passed.
...
[INFO] Controller runs in workload-variant-autoscaler-system and manages team-a, team-b
...
[WARNING] Scaling is now bounded by every limiter in 'quota' (declared in workload-variant-autoscaler-system/wva-scaling-policy-config):
    - name: install-quota
      type: quota
      scope: cluster
      quotas:
        NVIDIA-A100-PCIE-80GB: 4
...
  Manages:      team-a, team-b (it runs in workload-variant-autoscaler-system; its cache holds those and its own)
...
[SUCCESS] Deployment on kubernetes complete!
```

**Option B: quota plus the cluster's GPUs.**

<!-- guide:steps.installphysical start -->
```bash
# Option B: quota plus the cluster's GPUs. The same budget, and a replica goes only where a node has a free GPU.
make deploy-wva INSTALL_PHASE=wva \
  WVA_LIMITER=gpu-inventory,quota WVA_QUOTA_SCOPE=cluster WVA_QUOTAS="${ACCELERATOR}=${GPUS}"
```
<!-- guide:steps.installphysical end -->

You should see each namespace pass, both limiters, and `Deployment on
kubernetes complete!`:

```text
[SUCCESS] Namespaces: all 2 passed.
...
[WARNING] Scaling is now bounded by every limiter in 'gpu-inventory quota' (declared in workload-variant-autoscaler-system/wva-scaling-policy-config):
    - type: gpu-inventory
    - name: install-quota
      type: quota
      scope: cluster
      quotas:
        NVIDIA-A100-PCIE-80GB: 4
...
[SUCCESS] Deployment on kubernetes complete!
```

**Option C: Kueue.** `-1` lets Kueue set the number. A finite number there
caps the budget below Kueue's.

<!-- guide:steps.installkueue start -->
```bash
# Option C: Kueue. The scaling manager, with the GPU quota of Kueue's ClusterQueues as one budget for every namespace it manages.
make deploy-wva INSTALL_PHASE=wva \
  WVA_LIMITER=quota WVA_QUOTA_SCOPE=cluster WVA_QUOTAS="${ACCELERATOR}=-1" WVA_QUOTA_KUEUE=true
```
<!-- guide:steps.installkueue end -->

You should see each namespace on its LocalQueue, the quota read from Kueue,
and `Deployment on kubernetes complete!`:

```text
[SUCCESS] The controller can get namespaces/team-a, which the Kueue quota source (WVA_QUOTA_KUEUE=true) needs
[SUCCESS] The controller can get namespaces/team-b, which the Kueue quota source (WVA_QUOTA_KUEUE=true) needs
...
[SUCCESS]   team-a: passed (EPP flow control off: no scale from zero, on Kueue LocalQueue serving)
[SUCCESS]   team-b: passed (EPP flow control off: no scale from zero, on Kueue LocalQueue serving)
[SUCCESS] Kueue budget: 4 GPUs, the GPU quota of ClusterQueue(s) inference, shared by every managed namespace; WVA_QUOTAS can only lower it.
[SUCCESS] Namespaces: all 2 passed.
...
[INFO] Controller runs in workload-variant-autoscaler-system and manages team-a, team-b
...
[WARNING] Scaling is now bounded by every limiter in 'quota' (declared in workload-variant-autoscaler-system/wva-scaling-policy-config):
    - name: install-quota
      type: quota
      scope: cluster
      kueue:
        enabled: true
      quotas:
        NVIDIA-A100-PCIE-80GB: -1
...
[SUCCESS] Deployment on kubernetes complete!
```

If not:

- **It says the prerequisites are missing.** Run step 9 first.
- **A Role is missing in one namespace.** Step 9 ran with a different
  `WVA_WATCH_NS`. Run step 9 again in this shell.
- **It warns that the quota key matches no node.** Use the exact product from
  step 5.
- **Option B: the Pod list is missing.** Step 9 ran as option A. Check it
  with `kubectl auth can-i list pods -A --as=system:serviceaccount:${CONTROLLER_NAMESPACE}:wva-controller-manager`;
  it answers `yes` after step 9 for option B. If it answers `no`, run step 9
  for option B.
- **`gpu-inventory alone is refused`.** Keep the quota beside it, as above.
- **Option C: a namespace `failed` with a Kueue reason.** Fix what the
  `Kueue budget:` lines under it say, as in step 8.
- **Option C: `the shared budget is the GPU quota of EVERY ClusterQueue`.**
  A ClusterQueue for other work counts too. Set `WVA_QUOTAS` to the number it
  prints instead of `-1`.

## Register the models

### Step 11: Write the model plan

<!-- guide:steps.plan start -->
```bash
# Read-only. Lists the model servers in the managed namespaces and writes a plan. Applies nothing.
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=share-plan.yaml
```
<!-- guide:steps.plan end -->

You should see every model server in the managed namespaces, and `Plan
written to share-plan.yaml`:

```text
    APPLY  NAMESPACE  KIND        NAME            MODELID  MIN  MAX  COST  POLICY  FALLBACK
    yes    team-a     Deployment  model-a-decode  model-a  1    10   10.0  -       (+fallback)
    yes    team-b     Deployment  model-b-decode  model-b  1    10   10.0  -       (+fallback)

[SUCCESS] Plan written to share-plan.yaml — nothing applied.
```

The plan lets each model grow to `MAX` replicas. Together the models can want
more GPUs than the budget, so there is something to share. To change `MIN` or
`MAX`, edit `share-plan.yaml`. Keep `MIN` at 1 or more.

If not:

- **A model is missing.** Its pods need the label `llm-d.ai/role: decode` (or
  `prefill`), and its namespace must be one the scaling manager manages.
- **`# inferencePool: (none)`** in the plan. No InferencePool selects those
  pods. Make the pool's selector match the pod labels.

### Step 12: Check the model servers drain safely

<!-- guide:steps.workloads start -->
```bash
# Read-only. Checks that each model server can be scaled safely. Applies nothing.
make workload-patch
```
<!-- guide:steps.workloads end -->

You should see:

```text
[SUCCESS] 2 model server(s) already drain on scale-down and keep their weights on a volume; no vLLM among them compiles from nothing.
```

If it prints a patch, apply it where the model server's pod spec is owned,
usually your llm-d values. When GPUs move, a pod is removed, and without a
drain hook its requests are cut off.

### Step 13: Register the models

<!-- guide:steps.register start -->
```bash
# Applies the plan. One ScaledObject for each model server.
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=share-plan.yaml
```
<!-- guide:steps.register end -->

You should see one ScaledObject for each model:

```text
[SUCCESS]   team-a/model-a-decode (Deployment) -> ScaledObject model-a-decode-wva (modelID: model-a, 1-10) (+fallback)
[SUCCESS]   team-b/model-b-decode (Deployment) -> ScaledObject model-b-decode-wva (modelID: model-b, 1-10) (+fallback)
[SUCCESS] ScaledObjects: 2 created, 0 adopted, 0 not applied
```

If a model shows `adopt`, it already has a ScaledObject. Keep `adopt`, so the
model does not get a second one.

If you installed the scaling manager here before (step 20, then again), you
may see a warning that KEDA's operator still holds its connection to the old
scaler. Restart KEDA's operator with the command it prints. That briefly
pauses scaling for everything else on the cluster that uses KEDA, so it is
your call.

## Turn on sharing

> **A passive model puts its share group in shadow.** If one model of a group
> is [passive](../../reference/scaling-policy.md#passive), the whole group
> plans and moves nothing, and `wva_utilization_share_passive_shadow` is 1.
> Make the model active, or keep it out of the group, to share again.

### Step 14: Check the budget

Wait a minute after step 13, then run:

<!-- guide:steps.budget start -->
```bash
# Read-only. Is there a budget to share, and does the GPU product resolve.
make utilization-share-check CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE}
```
<!-- guide:steps.budget end -->

You should see the namespaces it manages, the quota, the GPU product, and no
`[FAIL]`. Option C reads the same as option A:

```text
  the controller in workload-variant-autoscaler-system manages team-a, team-b
  policy namespace: workload-variant-autoscaler-system (from workload-variant-autoscaler-system/wva-controller-status)
  policy: workload-variant-autoscaler-system/wva-scaling-policy-config
[OK]   limiter: quota; the optimizer shares each quota group
  ...
  accelerators the controller resolves: NVIDIA-A100-PCIE-80GB
[OK]   no AcceleratorNotResolved events
  optimizer block: none (today's optimizer). Next: shadow
```

With option B, the limiter line reads:

```text
[OK]   limiters: gpu-inventory and quota, both in force; the share's cluster budget is the cluster-scope quota, and the GPUs that exist bound it too
```

If not:

- **`the controller has logged no accelerator yet`.** Wait another minute and
  run it again.
- **`AcceleratorNotResolved event(s)`.** A model server does not name its GPU
  product. Add `nvidia.com/gpu.product: <product from step 5>` to its
  `nodeSelector`. The event stays for an hour after the fix; delete it with
  `kubectl delete events -n <namespace> --field-selector reason=AcceleratorNotResolved`
  and run step 14 again.
- **The quota has no key for a product.** Run steps 8 and 10 again with that
  exact name.

### Step 15: Turn on shadow

In shadow, the optimizer reports what it would move. It moves nothing.

<!-- guide:steps.shadow start -->
```bash
# Shadow: it reports what it would move, and moves nothing.
make utilization-share-shadow CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE}
```
<!-- guide:steps.shadow end -->

You should see:

```text
  wrote the optimizer block (shadow) into workload-variant-autoscaler-system/wva-scaling-policy-config
[OK]   the controller applied version 4500: utilization share is shadow
```

### Step 16: Read what it would do

Leave it in shadow through each model's busy period. Then run:

<!-- guide:steps.status start -->
```bash
# Read-only. The mode and the recent decisions.
make utilization-share-status CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE}
```
<!-- guide:steps.status end -->

You should see `mode shadow`, both namespaces counted, and no warnings:

```text
  policy: workload-variant-autoscaler-system/wva-scaling-policy-config; optimizer block: shadow
  controller: mode shadow, limiter quota/install-quota, policy version 4500 accepted=true, ...
  events counted in the namespaces of this budget: team-a team-b
  UtilizationShareGiving events: 0
  UtilizationShareReceiving events: 0
  UtilizationShareReceived events: 0
[OK]   no UtilizationShare warnings
  pods marked for a transfer: none
```

During a busy period the `log:` lines show what it would move, such as
`Utilization share: would rebalance (shadow)`.

### Step 17: Activate

<!-- guide:steps.activate start -->
```bash
# Let it act in every namespace it manages.
make utilization-share-activate CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE} GRANT_SHARED=true
```
<!-- guide:steps.activate end -->

You should see `active`:

```text
  the controller in workload-variant-autoscaler-system manages team-a, team-b
  policy namespace: workload-variant-autoscaler-system (from workload-variant-autoscaler-system/wva-controller-status)
[OK]   the controller in workload-variant-autoscaler-system can already patch pods in team-a
  wrote the optimizer block (active) into workload-variant-autoscaler-system/wva-scaling-policy-config
[OK]   the controller applied version 4564: utilization share is active
  for two to three minutes nothing in the group scales (quiet-period); the same follows every controller restart
```

The Role from step 9 already lets the scaling manager mark pods in each
managed namespace, so nothing new is granted, and a namespace that needs
nothing prints nothing. A namespace it may not mark pods in is named with a
warning. Nothing scales for two to three
minutes after this. That is expected.

### Step 18: See GPUs move

Wait about five minutes after step 17, while one model is busy and another is
not. Then run:

<!-- guide:steps.events start -->
```bash
# GPUs moving between namespaces, oldest first.
kubectl get events -A --sort-by=.lastTimestamp | grep UtilizationShare
```
<!-- guide:steps.events end -->

You should see `UtilizationShareGiving` on the model that gives a GPU, and
`UtilizationShareReceiving` on the model that takes it:

```text
team-b   11m     Normal   UtilizationShareGiving      deployment/model-b-decode   Utilization share: giving one replica (1 GPUs) to a model in another namespace of its quota group ...
team-a   11m     Normal   UtilizationShareReceiving   deployment/model-a-decode   Utilization share: receiving 1 GPUs from a model in another namespace of its quota group ...
team-a   5m52s   Normal   UtilizationShareReceived    deployment/model-a-decode   Utilization share: received 1 GPUs from a model in another namespace of its quota group ...
```

Step 16 now counts them, across both namespaces:

```text
  events counted in the namespaces of this budget: team-a team-b
  UtilizationShareGiving events: 1 (team-b 1)
  UtilizationShareReceiving events: 1 (team-a 1)
  UtilizationShareReceived events: 1 (team-a 1)
```

The replicas move with it: in this example model-a went from 2 to 3 and model-b from 2 to 1, and `wva_desired_replicas` in Prometheus shows the same. A move waits for the giver's scale-down window, 300 seconds by default. If
you see nothing after ten minutes, see [the troubleshooting
list](../../reference/troubleshooting.md#the-optimizer-evaluates-but-never-moves-anything).

The scaling manager's log has the same moves:

<!-- guide:steps.log start -->
```bash
# The optimizer's decisions, from the controller's log.
kubectl logs -n ${CONTROLLER_NAMESPACE} deploy/wva-controller-manager --tail=-1 \
  | grep -E 'Utilization share: (rebalancing|transfer (started|ended|cancelled))'
```
<!-- guide:steps.log end -->

You should see a `transfer started` line with `"scope": "cluster"`, a donor
in one namespace and a receiver in the other:

```text
... Utilization share: transfer started {"accelerator": "NVIDIA-A100-PCIE-80GB", "scope": "cluster", ..., "donor": "team-b/model-b/decode", "receiver": "team-a/model-a/decode", ...
... Utilization share: transfer ended {"accelerator": "NVIDIA-A100-PCIE-80GB", "scope": "cluster", ..., "outcome": "done", "donor": "team-b/model-b/decode", "receiver": "team-a/model-a/decode"}
```

## Undo

### Step 19: Turn the optimizer off

This is the rollback. The scaling manager keeps scaling each model on its own;
the budget stays.

<!-- guide:steps.disable start -->
```bash
# Turn the optimizer off. The budget stays.
make utilization-share-off CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE}
```
<!-- guide:steps.disable end -->

You should see `utilization share is off`.

### Step 20: Remove the scaling manager

The uninstall reads the namespaces it managed from the scaling manager
itself, and finds every namespace an earlier list set up by its label, so it
needs only the scaling manager's namespace from step 6.

<!-- guide:steps.uninstall start -->
```bash
# Remove the scaling manager. The models keep serving; KEDA and Prometheus stay.
make undeploy-wva CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE}
```
<!-- guide:steps.uninstall end -->

You should see the Role removed in each namespace, each ScaledObject removed,
and `Undeployment complete!`:

```text
[INFO] Removed the controller's permissions in team-a (Role/wva-manager-role-1d8cfc15, RoleBinding/wva-manager-rolebinding-1d8cfc15)
[INFO] Removed the controller's permissions in team-b (Role/wva-manager-role-1d8cfc15, RoleBinding/wva-manager-rolebinding-1d8cfc15)
...
[INFO] Removing the ScaledObjects this installer created:
[SUCCESS]   team-a/model-a-decode-wva removed
[SUCCESS]   team-b/model-b-decode-wva removed
[INFO]   KEDA restores each workload to its pre-autoscaling replica count as these go.
[SUCCESS] WVA uninstalled
[INFO] Removed the monitors setup added in team-a (label llm-d.ai/applied-by=wva-setup-prereqs).
[INFO] Removed the monitors setup added in team-b (label llm-d.ai/applied-by=wva-setup-prereqs).
...
[SUCCESS] Undeployment complete!
```

The models keep serving. KEDA returns each one to the replica count it had
before step 13. KEDA and Prometheus stay. To remove them too, add
`UNDEPLOY_SHARED=true`.

## Details

- **What the scaling manager may do.** In each managed namespace: what its
  Role grants, the same as a single-namespace install. Outside them: list
  nodes, get the managed Namespaces by name, list Kueue queues, and
  authenticate metrics; with option B, list Pods too. It cannot change
  workloads anywhere else, or list them outside the managed namespaces. Check
  it with
  `kubectl auth can-i list deployments -A --as=system:serviceaccount:${CONTROLLER_NAMESPACE}:wva-controller-manager`,
  which answers `no`.
- **A namespace it may no longer read.** If its RoleBinding is deleted, or the
  namespace is, the scaling manager keeps managing the others. It logs the
  namespace, reports it as `wva_watch_namespace_unreadable{namespace,reason}`,
  and asks again every minute; once it may read it again, it restarts to
  manage it. Run step 9 again to restore the RoleBinding.
- **Taking a namespace out of the list.** Run steps 9 and 10 again with the
  shorter `WVA_WATCH_NS`. They remove the Role, RoleBinding and monitors this
  install added in the namespace you left out, found by their labels, and
  nothing else there.
- **`WVA_WATCH_NS=auto` is read once.** A namespace that starts running llm-d
  later is not managed until you run steps 9 and 10 again. The scaling manager
  never widens its own rights.
- **The budget is a quota, `scope: cluster`.** It is charged only what this
  scaling manager's models hold. Back it with a Kubernetes `ResourceQuota`
  where other work must be bounded too. Without `WVA_QUOTA_SCOPE=cluster`,
  each namespace gets its own budget of `GPUS` and nothing is shared.
- **Kueue as the budget (option C).** The budget is the GPU `nominalQuota` of
  every ClusterQueue, read every 30 seconds, and the smaller of that and
  `WVA_QUOTAS`. Kueue still admits each pod; the scaling manager only stops
  asking for replicas Kueue would hold. The Kueue reads are cluster-scoped
  lists of ClusterQueues, Cohorts and ResourceFlavors, and get on the managed
  Namespaces by name; LocalQueues are read through each namespace's Role. See
  [Kueue as a quota
  source](../../reference/quota-limiter.md#kueue-as-a-quota-source).
- **Option C limit: every ClusterQueue counts.** A ClusterQueue for other
  work adds to the budget. Cap it with a finite `WVA_QUOTAS`; step 10 prints
  the number.
- **Option C limit: one budget per ClusterQueue, not per namespace.**
  Namespaces on one queue share it, and two that grow in the same minute can
  ask for more than it admits; Kueue holds the extra pods.
- **Option C limit: GPUs Kueue granted are never taken back by the
  optimizer.** Kueue preempts what it must
  ([the scaling policy](../../reference/scaling-policy.md#optimizer-cluster-default-only-live)).
- **Option C limit: no cohort borrowing.** One budget for every namespace
  counts only nominal quota. For borrowing, use [Let models borrow from a
  Kueue cohort](../kueue-borrowing/).
- **Why not the cluster's free GPUs alone.** Every namespace-scoped scaling
  manager would plan the same free GPUs, so `gpu-inventory` without a quota
  is refused here and `physicalGroups` is ignored. Option B keeps the quota
  as the budget and uses the nodes only for placement. Use [the cluster-wide
  install](../utilization-share-cluster-wide/) for the free GPUs alone.
- **Where the policy lives.** In `wva-scaling-policy-config` in the scaling
  manager's own namespace, which only the cluster admin who ran step 9 can
  edit. The teams in the managed namespaces cannot change the budget.
- **What the preflight checks in each namespace.** That llm-d runs there, that
  something scrapes the model servers and the EPP, and whether the EPP has
  flow control on.
- **The monitors step 9 adds.** A PodMonitor for the model servers, and a
  ServiceMonitor for the EPP. Only where nothing scrapes them yet. Each is
  labelled `llm-d.ai/applied-by=wva-setup-prereqs`; step 20 removes exactly
  those.
- **EPP flow control.** llm-d ships it off. Sharing works without it. Without
  it, a model at zero replicas never wakes, and the arrival rate reads low
  while a model is saturated. To turn it on, add `featureGates: [flowControl]`
  to the EPP's `EndpointPickerConfig` and re-run your llm-d router install.
- **Upgrading.** Re-running step 10 drops the optimizer block. Run steps 15
  and 17 again after an upgrade.
- **OpenShift.** Install the Custom Metrics Autoscaler operator instead of
  KEDA, and drop `KEDA_HELM_INSTALL=true`.

## Next

- [Turn on the utilization-share optimizer, safely](../utilization-share/):
  weights, floors and turning it off.
- [Share GPUs between models in different
  namespaces](../utilization-share-cluster-wide/): the cluster-wide install,
  with the cluster's free GPUs as a second budget option.
- [The well-lit path](../../well-lit-paths/utilization-share/): what it costs
  and what was measured.
