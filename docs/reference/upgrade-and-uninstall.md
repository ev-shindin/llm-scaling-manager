# Upgrade and uninstall

How to upgrade the controller safely, what a restart costs, and what an
uninstall removes and leaves behind. `$NS` is the namespace the controller runs
in.

> Part of the [deployment guide](../../deploy/). Day-to-day checks are in
> [After the install](operations.md); symptoms in [Troubleshooting](troubleshooting.md).

## Pin the version

An install from a checkout of `main` deploys
`ghcr.io/ev-shindin/llm-scaling-manager:main`. `:main` is a
**floating** tag, rebuilt on every push to `main`, and the installer pulls it
with `imagePullPolicy: Always`. Any pod restart (a node drain, an eviction, a
leader change) can start different code from what you tested. Do not run
production on it.

Pin a release tag or a digest, and deploy with the manifests from the **same**
release. An image older than the manifests crash-loops on flags it does not
know. Releases are listed on the
[Releases page](https://github.com/ev-shindin/llm-scaling-manager/releases);
the first full release is `v0.10.0`, after release candidates `-rc1` to
`-rc4`, and the latest is `v0.10.2`. A checked-out release deploys its own tag,
so `IMG_TAG` below only makes that explicit. Each release's notes list its image
digest; a digest names one build, and a tag could in principle be re-pushed.

```bash
git checkout v0.10.2
make deploy-wva NAMESPACE=<namespace> IMG_TAG=v0.10.2
# or by digest (v0.10.2's, from its release notes)
make deploy-wva NAMESPACE=<namespace> \
  IMG=ghcr.io/ev-shindin/llm-scaling-manager@sha256:08b640117764fdcc768fc66e6ad67d4410b275e3ae9a6d7023b8e6cdcfc25a76
# out-of-reach shape (the cluster admin runs it): the controller runs in wva-<namespace>
NAMESPACE=<namespace> make deploy-wva WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace> IMG_TAG=v0.10.2
```

`IMG_TAG` sets the tag of the default `IMG`; `IMG` replaces the whole reference.
The pull policy follows the reference: `IfNotPresent` for a digest or a `vX.Y.Z`
tag, `Always` for anything else. `WVA_IMAGE_PULL_POLICY` overrides it. Confirm
what runs:

```bash
kubectl get deploy wva-controller-manager -n "$NS" \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"  "}{.spec.template.spec.containers[0].imagePullPolicy}{"\n"}'
```

Pass every install-time variable you used the first time (`SCOPE`,
`ENVIRONMENT`, `DEPLOY_ALERTING_RULES`, ...) on every upgrade. An upgrade
re-renders the install from them. For example, leaving `DEPLOY_ALERTING_RULES`
at its default `false` deletes the alert rules. The full list is in
[Configuration](configuration.md).

The limiter is the exception. A re-run without `WVA_LIMITER` keeps the limiter
the controller's `wva-scaling-policy-config` already declares; pass `WVA_LIMITER`
only to change it. Limiters an admin published in a policy namespace
(`make enable-physical-limiter`) are never touched by `make deploy-wva`.

## Upgrade

1. **Back up your policy.** The install owns `wva-scaling-policy-config`; keep a
   copy of what you tuned
   ([Changing policy safely](operations.md#changing-policy-safely)).
2. **Check prerequisites** against the new release. It is read-only:
   ```bash
   make check-prereqs NAMESPACE=<namespace>
   # out-of-reach shape: the controller runs in wva-<namespace>
   NAMESPACE=<namespace> make check-prereqs WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>
   ```
   A new release can need a kind or a permission the old one did not. If it
   reports missing prerequisites, a cluster admin runs, once:
   ```bash
   NAMESPACE=<namespace> make setup-prereqs        # (cluster admin)
   # out-of-reach shape
   NAMESPACE=<namespace> make setup-prereqs WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>
   ```
   A missing **optional** prerequisite does not stop the install: the controller
   runs without the feature that needs it, and says so. For this release that
   is not enough when you come from an older build: see
   [Upgrading from before v0.10.0-rc1](#upgrading-from-before-v0100-rc1).
3. **Deploy** the pinned image, as above, in the install's shape.
   - Out-of-reach install: the cluster admin runs the
     `WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>` form. The plain
     `make deploy-wva NAMESPACE=<namespace>` would install a second controller
     in `<namespace>` instead of upgrading the one in `wva-<namespace>`.
   - The re-run keeps the declared limiter (see above) and re-applies the rest
     of `wva-scaling-policy-config` from the release. That is why step 1 backs
     it up.
4. **Verify**, naming the namespace as you did for the install. Without it, both
   commands look in the default `workload-variant-autoscaler-system`:
   ```bash
   NAMESPACE=<namespace> make verify-deployment
   NAMESPACE=<namespace> make verify-scaledobjects
   # out-of-reach shape: the controller runs in wva-<namespace>
   NAMESPACE=<namespace> WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace> make verify-deployment
   ```
   If a ScaledObject reads Ready but its HPA carries only `cpu`, KEDA left it
   unwired. Re-wire it (`DRY_RUN=true` only lists them). In the out-of-reach
   shape pass `WVA_NS` too; without it the command finds no ScaledObject
   naming that scaler, warns with the namespace they do name, and exits 3:
   ```bash
   NAMESPACE=<namespace> make scaledobjects-rewire
   # out-of-reach shape: the controller runs in wva-<namespace>
   NAMESPACE=<namespace> WVA_NS=wva-<namespace> make scaledobjects-rewire
   ```

### Upgrading from v0.10.1 to v0.10.2

Follow [Upgrade](#upgrade) above: back up the policy first, check out
`v0.10.2`, and re-run the same `make deploy-wva` command. No cluster-admin
step is needed. Three changes can show after the upgrade:

1. **A variant that sets `--max-num-active-seqs` below `--max-num-seqs` can
   run more replicas.** vLLM runs at most the active cap at once; v0.10.1
   priced a replica at `--max-num-seqs` and so ordered too few replicas for
   it. The `slo-variant` and `derived-mu` log lines gain a `maxNumActiveSeqs`
   field.
2. **Utilization share can fund a second whole-node receiver.** v0.10.1 took
   a promised GPU off the freest node instead of the node it is on, so a node
   free enough for an 8-GPU pod could look one GPU short.
3. **The install can pick a different Prometheus.** It now takes the first
   Prometheus resource that scrapes the model namespace, not the first one
   listed, and warns when none is known to. An install that passes
   `PROMETHEUS_URL` is unchanged.

If you set up the utilization-share optimizer, re-run its steps after
`make deploy-wva`, as for v0.10.1 below. The full list is in the
[v0.10.2 release notes](https://github.com/ev-shindin/llm-scaling-manager/releases/tag/v0.10.2).

### Upgrading from v0.10.0 to v0.10.1

Follow [Upgrade](#upgrade) above: back up the policy first, check out
`v0.10.1`, and re-run the same `make deploy-wva` command. Also:

1. **Let the EPPs authenticate `/metrics`, once (cluster admin).** If an EPP
   answers 500 to `/metrics` ([why](troubleshooting.md#epp-metrics-returns-500)),
   `make deploy-wva` alone does not fix it. A cluster admin runs
   `make epp-metrics-auth` (or re-runs `make setup-prereqs`, which now includes
   it; `WVA_EPP_METRICS_AUTH=false` skips it) with the same variables as the
   install. `make verify-deployment` names the EPPs that need it. The EPP
   caches review results, so the 500 can last up to about 5 minutes after the
   grant; restarting the EPP pod clears it at once.
2. **Upgrade KEDA older than 2.21 to 2.21, before `make deploy-wva` if you
   can.** On KEDA before 2.20 a controller restart, such as the upgrade's own
   rollout, or an outage can leave ScaledObjects unwired
   ([Unwired HPA before KEDA 2.20](keda.md#unwired-hpa-before-keda-220)); if
   KEDA is upgraded after the deploy, run `make scaledobjects-rewire` once it
   is. KEDA 2.20 carries CVE-2026-77524, fixed in 2.21.0. The installer now
   warns about both.
3. **Log parsers.** The `itl-window` log line's `maxNumSeqs` field is now
   `batchCap`: `max_num_seqs`, lowered by `--max-num-active-seqs` when the
   engine sets it. Only that line changed: the saturation and SLO analyzers'
   capacity lines still log `maxNumSeqs`.
4. **Utilization-share optimizer.** If you set it up, re-run its steps after
   `make deploy-wva`: the install drops the optimizer block. Cluster-wide:
   [steps 15 and 16](../guides/utilization-share-cluster-wide/README.md#step-15-turn-on-shadow);
   namespaced:
   [steps 15 and 17](../guides/utilization-share-namespaced/README.md#step-15-turn-on-shadow).

The full list is in the
[v0.10.1 release notes](https://github.com/ev-shindin/llm-scaling-manager/releases/tag/v0.10.1).

### Upgrading from v0.10.0-rc4 to v0.10.0

Follow [Upgrade](#upgrade) above: back up the policy first, because the re-run
re-applies `wva-scaling-policy-config`. `make setup-prereqs` is not needed:
v0.10.0 adds no grant to rc4. From rc4 also:

1. **Rename reserved quota entries first.** A `quota` entry named
   `gpu-limiter`, `node-fit`, `pd-pair-borrows-whole` or
   `lender-cannot-reclaim` is now an error, not a warning: the `default` policy
   entry holding it is rejected and a controller starting on it does not start.
   `make deploy-wva` checks before it applies anything. How to find and rename
   them: [below](#other-changes-that-need-an-action).
2. **Check out `v0.10.0` and re-run the same `make deploy-wva` command, adding
   what changed.** `WVA_REPLICAS=<count>` if you scaled the controller by
   hand: a re-install now sets it to `WVA_REPLICAS` (default 1).
   `WVA_VERBOSITY=3` if you read the per-replica log lines or use
   `hack/benchmark/dump_k2_decisions.py`. If you set up the utilization-share
   optimizer, re-run its steps afterwards: the install drops the optimizer
   block.
3. **GitOps.** A re-wire (`make scaledobjects-rewire`, and the one
   `make scaledobjects-apply` now runs after applying) sets the trigger key
   `wvaRewire` on a ScaledObject KEDA left unwired. A GitOps tool shows it as
   drift; leave it, or add it to the source. The scaler ignores the key.
4. **Alerts.** An alert on `wva_model_scaling_blocked` reasons may want the new
   `donor-changing`.

The full list is in the
[v0.10.0 release notes](https://github.com/ev-shindin/llm-scaling-manager/releases/tag/v0.10.0).

### Upgrading from before v0.10.0-rc1

This release adds two cluster-scoped grants (`wva-policy-namespace-reader-*`,
`wva-kueue-reader-*`), a third made only where a policy counts Kueue borrowing
(`wva-kueue-borrow-reader-*`), and a namespaced Role (`wva-controller-status-role`, for
the status ConfigMap). It also moves the external-scaler NetworkPolicy into the
admin phase. `make check-prereqs` reports all three grants as **optional**, so a
tenant's upgrade goes ahead without them and the controller runs without what
they enable. Do this, in order:

1. **A cluster admin re-runs `make setup-prereqs` first**, for every
   namespace-scoped install, with the variables that install used, before the
   namespace owner runs `make deploy-wva`:
   ```bash
   NAMESPACE=<namespace> make setup-prereqs        # (cluster admin)
   # out-of-reach shape
   NAMESPACE=<namespace> make setup-prereqs WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>
   ```
   Also pass:
   - `KEDA_NAMESPACE=<namespace KEDA runs in>`, unless the installer can find
     KEDA itself. If it cannot, **no** NetworkPolicy is applied and `:9090` is
     open to every Pod ([the scaler NetworkPolicy](#the-external-scaler-networkpolicy)).
   - `PROMETHEUS_SA=<namespace>/<serviceaccount>` (or the `PROMETHEUS_URL` the
     install first used). Without one, the re-run keeps the existing
     `metrics-reader` binding unchanged and warns which account it names
     ([Prometheus](prometheus.md)).

   Before this step, save the controller's `Effective scaling policy` log
   lines, to compare after the upgrade
   ([Policy that starts to apply](#policy-that-starts-to-apply)).
2. **Policy may move.** After its next restart, a controller that manages its
   own namespace can read Namespaces, so it finds a published cluster policy:
   the namespace its `wva.llmd.ai/policy-namespace` label names, else
   `wva-policy` if it exists. Its limiters and optimizer selection then come
   from there, and the `limiters:` in its own `wva-scaling-policy-config`
   **stop applying**. Thresholds, tiers and per-model entries still come from
   its own ConfigMap.
3. **`make enable-physical-limiter` and `make disable-limiter` restart
   every namespace-scoped controller whose policy lives outside the namespace it
   manages**, on every run. That is a self-managed controller reading
   `wva-policy` (or its labelled namespace), or an out-of-reach one reading its
   own ConfigMap in `wva-<namespace>`. Such a controller caches only the
   namespace it manages and reads other ConfigMaps once, at startup. The plan
   the command prints marks them `WILL BE RESTARTED`. Run it when a restart is
   acceptable ([what a restart costs](#after-every-upgrade-or-restart)).
4. **What now fails closed**:
   - A Kueue ClusterQueue with a `namespaceSelector` grants a namespace its
     quota only when the selector admits it (no selector admits nothing, `{}`
     admits every namespace). If the controller cannot read the namespace's
     labels, that queue's grant is withheld, so the namespace is bounded at
     nothing by it.
   - With a Kueue quota source configured, `make deploy-wva` (the controller-only
     phase) **refuses** when the controller cannot `get` its managed Namespace,
     and names the `make setup-prereqs` run that fixes it. When it cannot tell
     (you may not impersonate the controller, or cannot read whether a Kueue
     source is configured), it **warns** with the same fix and goes ahead.
   - A NetworkPolicy that admits a namespace KEDA does not run in drops every
     KEDA call: every ScaledObject naming this scaler goes not-Ready and nothing
     scales, while the controller reports healthy.
5. **Check**:
   - `wva_policy_namespace_unverified` is `0`. `1` means the controller could
     not read its Namespace and runs on its own ConfigMap
     (alert `WVAPolicyNamespaceUnverified`).
   - `NAMESPACE=<namespace> make verify-deployment` (add
     `WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>` for the out-of-reach
     shape) prints no NetworkPolicy warning: not "wrong KEDA namespace", not
     "port open". "Not checked" means you may not read NetworkPolicies there;
     run it as someone who can.
   - The [`wva-controller-status` ConfigMap](operations.md#what-the-controller-is-running-with)
     names the policy namespace, limiter and acceptance the controller runs with:
     ```bash
     kubectl get cm wva-controller-status -n "$NS" \
       -o jsonpath='{.data.policyNamespace} {.data.limiter} {.data.policyAccepted}{"\n"}'
     ```
     It prints, for example, `wva-policy inventory+quota/gpu-inventory,tenant-quota true`:
     the limiter types joined by `+`, then the name of every entry
     ([what changed in this format](#limiters-and-kueue-borrowing)).

#### Limiters and Kueue borrowing

> **ACTION REQUIRED before upgrade.** A Kueue-backed namespace quota starts
> counting cohort borrowing **without a config change**. Borrowing is
> experimental but on by default. Before the upgrade:
>
> 1. **(cluster admin) Re-run `make setup-prereqs`** for every
>    namespace-scoped install ([step 1 above](#upgrading-from-before-v0100-rc1)).
>    It adds a Role in the Kueue namespace to read the
>    `kueue-controller-manager` Deployment and its leader Lease and, only
>    where a limiter policy of the install counts borrowing, the cluster-wide
>    Workload read (ClusterRole `wva-kueue-borrow-reader-role-<hash>`).
>    Where Kueue runs elsewhere than `kueue-system`, add
>    `KUEUE_NAMESPACE=<kueue-ns>`, or set `kueue.controllerNamespace` on the
>    entry, which a re-run then uses. Without that Role, borrowing stops with
>    `kueue-controller-down`; without the Workload read, with
>    `lenders-unreadable`. The Workload read reaches every tenant's Workloads,
>    pod templates included ([security model](security-model.md#kueue-borrowing)),
>    so it is revoked when no policy counts borrowing: on a re-run with
>    `WVA_QUOTA_KUEUE_BORROW=false`, on `make disable-limiter
>    WVA_LIMITER_TYPE=quota`, and on uninstall.
>
>    **A cluster-scoped install** re-applies its manifests instead; its
>    manager ClusterRole gains every read borrowing needs, Workloads in every
>    namespace included, whether or not borrowing is counted:
>    ```bash
>    make setup-prereqs SCOPE=cluster WVA_NS=<controller namespace>   # (cluster admin)
>    ```
> 2. **Check the node read** for every policy that declares `gpu-inventory`
>    beside `quota` (below).
> 3. **Choose, per Kueue-backed entry**:
>    - **borrow everywhere**: nothing to do, where the conditions hold;
>    - **pilot**: set `borrowNamespaces` on the entry to the namespaces that
>      may borrow ([how](../guides/kueue-borrowing/#pilot-on-a-few-namespaces));
>    - **opt out**: set `countBorrowable: false` on the entry
>      (`WVA_QUOTA_KUEUE_BORROW=false` on the install), which keeps the
>      Kueue cap at nominal exactly as before.
> 4. **Give borrowing a GPU bound** where you borrow: a `gpu-inventory` entry,
>    or a GPU `ResourceQuota` whose `hard` is **at most nominal plus
>    `borrowingLimit`**. The installer adds neither; without one borrowing is
>    withheld as `no-gpu-bound`.
> 5. **After the upgrade**, check that borrowing is counted where you want it,
>    and read the reason where it is not:
>    ```promql
>    wva_quota_limit_gpus{tier="borrowable"} > 0
>    sum by (reason) (wva_quota_borrow_withheld)
>    ```
>    A missing permission shows as the reason of the read it blocks, never as
>    "nothing to borrow": `lenders-unreadable` (Workloads),
>    `not-kueue-managed` (a GPU pod's owner), `resourcequota-unreadable`,
>    `hierarchy-unknown` (Cohorts), `kueue-controller-down` (the Role in the
>    Kueue namespace). [What each reason means](troubleshooting.md#borrowing-is-withheld).
>
> The capacity of lenders that do **not** reclaim
> (`reclaimWithinCohort: Never`, or `LowerPriority` that does not outrank the
> borrowers) is not counted by default (`lender-cannot-reclaim`). Counting it,
> and handing it back when they wait, is an opt-in, experimental setting:
> `kueue.releaseToNonReclaimingLenders: true`
> ([how it works](quota-limiter.md#lenders-kueue-does-not-reclaim-for)). Nothing
> changes until you set it. Set, it adds `released-to-lender` on
> `wva_model_scaling_blocked`, cause `lender-waiting` on
> `wva_borrow_releases_total` and the `BorrowedCapacityReleased` Event, and
> the gauges `wva_kueue_borrow_release_latency_seconds{cohort}` (which
> `WVAKueueLenderWaitingPastCooldown` adds to its threshold, in that cohort
> only), `wva_kueue_lender_release_blocked` and
> `wva_kueue_borrow_ledger_unread`, and the Warning Event
> `LenderReleaseBlocked`. The release ledger in `wva-controller-status` gains
> a `version` field; a controller older than this release ignores the fields
> it does not know, so a rollback hands nothing back but keeps the cooldowns.
>
> **Before turning it off or rolling back**, lower each borrowing model's
> `maxReplicaCount` to what its guaranteed figure holds: capacity borrowed
> from a `Never` lender stays borrowed (Kueue never reclaims it), the lender
> waits, and the alert pages at the cooldown alone. A release in flight keeps
> its targets for up to twice its latency. Raise `maxReplicaCount` again
> afterwards.

**Before you upgrade**, list the limiter types every policy declares, and
every Kueue-backed entry with its borrowing settings. A cluster admin runs
this; a namespace owner runs the inner commands for their own namespace:

```bash
for ns in $(kubectl get cm -A --field-selector metadata.name=wva-scaling-policy-config \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{end}'); do
  printf '%s: ' "$ns"
  kubectl get cm wva-scaling-policy-config -n "$ns" -o jsonpath='{.data.default}' \
    | yq -r '[.limiters[]?.type] | join(",")'
  # Kueue entries: null countBorrowable means the default, true;
  # null borrowNamespaces means every governed namespace.
  kubectl get cm wva-scaling-policy-config -n "$ns" -o jsonpath='{.data.default}' \
    | yq -o=json -I=0 '.limiters[]? | select(.kueue.enabled == true) | {"name": .name, "countBorrowable": .kueue.countBorrowable, "borrowNamespaces": .kueue.borrowNamespaces}'
done
```

For each line that lists `gpu-inventory` (or `inventory`, the same limiter)
**and** `quota`:

- **Check that the controller reading it can list nodes**:
  ```bash
  kubectl auth can-i list nodes --as=system:serviceaccount:<controller-namespace>:wva-controller-manager
  ```
  `no` is typical for a namespace-scoped install. The controller stays Ready
  after the upgrade, skips the physical limiter and keeps bounding by the quota,
  as it did before; it logs `Cluster policy declares gpu-inventory beside quota
  limiters, but this controller may not list nodes` and sets
  `wva_constraint_provider_skipped{provider="gpu-limiter",reason="node_read_denied"}`
  to 1. To have both bound, a cluster admin runs `make setup-prereqs` for that
  install (or `make enable-physical-limiter`); to keep the quota alone, drop the
  `gpu-inventory` entry. A policy that lists `gpu-inventory` **alone** still
  takes a controller without node read NotReady, as before.
- **Check that every entry lists every GPU type its models use.** A type one
  entry does not list is now denied even when another entry lists it, so a
  model can lose a type it scaled on before. Print the entries and compare their
  types with the `accelerator` the controller logs for each model:
  ```bash
  kubectl get cm wva-scaling-policy-config -n <ns> -o jsonpath='{.data.default}' | yq '.limiters'
  ```
  The physical limiter lists the GPUs the nodes advertise
  (`wva_available_gpus`).

What changes without a config change:

- **A policy that declares both `gpu-inventory` and `quota` is bounded by
  both.** Before, the quota alone applied and the physical entry was never
  built. This can be tighter where other workloads hold GPUs. For this release
  only, the controller also computes what the quota alone would have decided, at
  most once every 10 cycles, and logs `Upgrade note (transitional)` naming the
  variants that differ. That note is removed in the next release.
- **A type one limiter entry denies stays denied** when another entry lists
  it. Before, entries listing different types allowed the union (bug fix).
- **Budgets follow the GPU family and product names.** A family key such as
  `H100` bounds the sum of its products (`NVIDIA-H100-80GB-HBM3`, ...), so two
  models, or the two roles of one P/D model, spelled differently can no longer
  order the same GPUs twice. An allowlist that names only products denies the
  products it does not name; one that names the family too admits them up to
  the family's figure. A workload that names the bare family is bounded by the
  family's total (bug fix).
- **A P/D pair commits only what fits together.** Before, a budget of 8 with
  two 8-GPU roles ordered 16. A P/D wake, and two wakes in one tick, fit the
  family's figure together too (bug fix).
- **With the physical limiter, a replica must fit one node.** Free GPUs split
  across nodes in holes smaller than a replica no longer count: 16 free as
  4+4+8 order one 8-GPU replica, not two, and a P/D model grows only when both
  roles find a node. Scale-from-zero and the warm pool check the same way; a
  Pending pod nominated for a node now holds it. See
  [A replica must fit one node](gpu-limiter.md#a-replica-must-fit-one-node).
- **The warm pool's headroom is published after the optimizer's pass**, net
  of the replicas the pass ordered. Before, it was the figure from before the
  pass, so a pool could grow into GPUs just ordered for a model.
- **A namespace-scope quota bounds only the namespaces it governs**: those it
  lists, and with a `default` key every other namespace it does not exclude.
  A namespace it excludes is no longer bounded by the sum of the other tenants' caps: it is
  bounded by the cluster bounds (the physical limiter or a cluster-scope quota),
  and unbounded when there are none.
- **A namespace allowance that is unlimited (`-1`) for every type now scales**
  when nothing else bounds the cluster. It never did.
- **A namespace that lists a GPU family now gets GPUs when the cluster quota
  lists only products of it** (bug fix). A namespace at `{H100: -1}` under a
  cluster quota of `{NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 8}` was
  often held at 0, and its warm pool always was. It now gets up to 8 of each
  product, in every cycle. Its warm pool reads each product at that product's
  figure. Clusters whose cluster quota lists the family itself, or that have
  no cluster quota, are not affected.
- **A family is bounded by what fits under every cluster quota, product by
  product** (bug fix). This gives fewer GPUs to one setup only: two or more
  `scope: cluster` quotas that list products of one family in different
  numbers. `{NVIDIA-H100-80GB-HBM3: 16, NVIDIA-H100-PCIE-80GB: 8}` beside
  `{NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 16}` allowed 24 H100s; only
  16 fit under both, and 16 is the bound now. A workload that names the bare
  family, in a namespace that lists only products of it, likewise gets only
  what the cluster allows on those products: `{NVIDIA-H100-SXM5-80GB: -1}`
  under a cluster quota with no SXM5 now gets 0, where it could get the
  cluster's other H100s.
- **A namespace that lists a family and denies one of its products gets the
  family only through its other products** (bug fix). This gives fewer GPUs
  to one setup only: a workload that names the bare family, such as `H100`,
  in a namespace like `{H100: -1, NVIDIA-H100-SXM5-80GB: 0}`, under a cluster
  quota that lists only products. With `{NVIDIA-H100-80GB-HBM3: 8,
  NVIDIA-H100-SXM5-80GB: 24}` it could get 32 H100s, and a wake from zero
  passed on them. It now gets 8, the HBM3 GPUs.
- **`LimitedBy` names the cluster quotas when only their joint bound holds.**
  With two quotas that list products of one family in different numbers, a
  decision held at what fits under both used to name the physical limiter
  first and the quotas beside it. It now names the quotas. The
  `wva_decisions_limited_total` metric and the `ResourceConstrained` event
  carry the new name. No GPU figure changes.
- **Two utilization-share namespace groups on one GPU type split the free
  cluster GPUs** rather than each counting all of them.
- **A limiter that cannot be consulted in a cycle is skipped; the others still
  bound.** Before, one missing input lifted every limiter. A skip sets
  `wva_constraint_provider_skipped` and is logged when it starts and ends.
- **Scale-from-zero debits each wake against its fallback snapshot**, so wakes
  during a limiter outage cannot all pass on the same free GPUs. A wake with no
  check at all counts in `wva_scale_from_zero_wake_unchecked_total`.
- **A Kueue-bounded namespace quota counts cohort borrowing**, on by default,
  where it is safe, and only capacity Kueue would take back for its lender
  (`lender-cannot-reclaim` otherwise). Kueue takes borrowed replicas back for
  that lender, and the scaling manager lowers a borrowed replica that did not
  place within `placementTimeout`. While a declared `gpu-inventory` limiter
  is skipped (no node read, say), no new borrowing starts. See the box above
  and [the guide](../guides/kueue-borrowing/). Borrowing state is kept in
  `wva-controller-status`, so it survives a restart.
- **vLLM's prefix-cache hit rate is read** (bug fix). The query used the
  counter names without the `_total` suffix vLLM exposes, so the hit rate was
  always 0. With prefix caching on, a cached part of the prompt is no longer
  charged: the KV each request needs, the router's queued demand and
  prefill's work are all discounted by the hit rate, so a workload with shared
  prefixes can get fewer replicas than before. A prefill variant's learned
  throughput is keyed on the prompt net of cache hits, so it may start a new
  window once. SGLang is not affected.
- **A vLLM pod running several data-parallel ranks behind one API server now
  gets its real capacity, so fewer replicas are ordered for it.** Before, each
  per-pod figure took the busiest rank: its cache use, its latency and its
  queue stood for the whole pod, and `--max-num-seqs` capped the whole pod at
  one rank's batch. A pod of eight ranks could be ordered up to about eight
  times the replicas it needs. Now its use is the mean over its ranks, its
  queue is the sum, its latencies are traffic-weighted means, and its batch cap
  is the ranks times `--max-num-seqs`. A pod with one rank (DP=1) is priced as
  before. A pod with one API server per rank changes too: its ranks are
  counted, so its batch cap and its derived capacity are the ranks times one
  engine's, and its ITL is weighted by the tokens each rank generated
  (bug fix). See
  [Data-parallel pods](../developer-guide/signal-dataflow.md#data-parallel-pods).
- **A hot rank is judged on its own.** Whether a pod is saturated is read from
  its busiest rank: a queue on one rank at `queueLengthThreshold`, and that
  rank's cache at `kvCacheThreshold`. Eight ranks with one request waiting
  each are not saturated, though their queues add up to eight. A pod with one
  full rank keeps its own capacity: the other pods' spare can still be given
  back, but not that pod's. The rest of the role is not held, and no replica
  is ordered for it. Once kept, the pod is kept until that rank drops 5 points
  below `kvCacheThreshold`, so a rank hovering at the threshold does not
  switch it on and off every cycle. On a P/D model this applies to decode, and
  to prefill while decode is not saturated.
- **Learned capacity saved before the upgrade.** With the learned-state
  snapshot turned on (`LEARNED_STATE_SNAPSHOT`, off by default), the k2
  figures it carries across the upgrade were measured the old way, from the
  busiest rank. For a data-parallel pod they can be off until new readings
  replace them or they age out. To drop them for one variant, see
  [Reset one variant's learned state](troubleshooting.md#reset-one-variants-learned-state).
  With the snapshot off, nothing carries over.

New reads, for borrowing: `cohorts` and `workloads` (Kueue; `workloads`
cluster-wide, watched through an informer), the `kueue-controller-manager`
Deployment and its leader Lease in the Kueue namespace, and in the managed
namespace `resourcequotas`, `events`, and the owners of GPU pods
(`replicasets`, `statefulsets`, `jobs`, `leaderworkersets`). A cluster-scoped
install gets them from the install itself, Workloads in every namespace
included. **A namespace-scoped install needs a cluster admin to re-run
`make setup-prereqs`** (step 1 above), which grants the cluster-wide Workload
read only while a policy of the install counts borrowing. Until then borrowing is withheld or
frozen, with the reasons the box lists, and the nominal cap applies.

**Rolling back past this release**: first turn borrowing off
(`countBorrowable: false`, or `WVA_QUOTA_KUEUE_BORROW=false`), and wait for
models to drop back under nominal. Turning it off evicts nothing: a lowered
cap only stops growth, and models move back as they scale down (lower a
model's `maxReplicaCount` to hurry it). Check that none is still past it: no
entry may show `used` above `guaranteed`.

```bash
kubectl get configmap wva-controller-status -n "$NS" -o jsonpath='{.data.limiterBudgets}' \
  | jq -c '(.entries | length), (.entries[] | select(.guaranteed >= 0 and .used > .guaranteed) | {namespace, accelerator, guaranteed, used})'
```

The first line is the number of entries; it must be above 0, or the check
read nothing. No line after it means every namespace is back within nominal.
If the utilization-share
optimizer is on, also set `shadow: true` and wait one cycle before the
downgrade, so it removes its marks from the pods
([why](troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade)).
Then install the older release. It
ignores the `borrowReleaseLedger` and `limiterBudgets` keys in
`wva-controller-status`. The RBAC borrowing added stays behind; it grants
reads only. Remove it by re-applying the older release's roles, or leave it.

What you read changes:

- **`wva-controller-status` `.data.limiter`** was `<mode>/<names>`, and a policy
  declaring both kinds read `quota/<quota names>`. It is now the limiter types
  joined by `+`, then every entry's name: `inventory+quota/gpu,tenant-quota`. An
  unnamed physical entry is named after its type:
  `inventory+quota/gpu-inventory,tenant-quota`. The guides' `jsonpath` recipes
  print this form.
- **Log keys.** `GPU limiter constructed` and `GPU limiter (re)built from config`
  carry `limiterTypes` (was `type`), in the same `+` form; `Loaded cluster
  policy from the policy namespace` carries `limiterTypes` (was `limiterMode`).
  A query on the old keys finds nothing.
- **A second physical entry** logs `Ignoring physical (gpu-inventory) limiters
  entries after the first` (was `WARNING: limiters declares more than one
  physical (gpu-inventory) entry`).
- **New metrics**: `wva_constraint_provider_skipped` and
  `wva_scale_from_zero_wake_unchecked_total` ([Prometheus](prometheus.md)),
  and for Kueue: the borrowing series ([Prometheus](prometheus.md#kueue-borrowing-metrics)),
  `wva_kueue_borrowed_gpus`, `wva_kueue_controller_version_known`,
  `wva_kueue_borrow_cooldown_seconds` and `wva_kueue_never_read`, with the
  alerts `WVAKueueNeverRead`, `WVAKueueVersionUnknown`, `WVAKueueBorrowDrift`
  and the others in [Kueue borrowing alerts](troubleshooting.md#kueue-borrowing-alerts).
- **`scaling-decision`** lines carry `borrowed`, the GPUs a model holds past
  its guaranteed figure.

**Rolling back** to an older controller reads a policy that declares both kinds
as quota-only again: the physical limiter stops bounding, and that release only
warns about it (`notEnforced`).

Installer:

- `WVA_LIMITER` takes a list: `WVA_LIMITER=gpu-inventory,quota`. Left unset, it
  keeps the live policy's limiters, and the prerequisite check now asks for node
  read against those: it warns, with the remedy, when a kept `gpu-inventory`
  entry has none.
- `make enable-physical-limiter` keeps a limiter of the other kind already in
  the policy.
- `make disable-limiter` (new; `make disable-physical-limiter` is the same
  target under its older name) removes the kind
  `WVA_LIMITER_TYPE` names: `gpu-inventory` (the default), `quota`, or `all`.
  Exit codes are in [Uninstall](#policy-namespace-and-limiter-leftovers-cluster-admin).
- `WVA_QUOTA_KUEUE_BORROW=false`, beside `WVA_QUOTA_KUEUE=true`, keeps the
  Kueue cap at nominal (`countBorrowable: false`).
- A re-run of the install, or of `make enable-physical-limiter`, keeps the
  quota entry's whole `kueue:` block (`borrowNamespaces`,
  `assumeTestedVersion`, the timings), and rewrites only what its variables
  set.
- With `WVA_QUOTA_KUEUE=true` the installer adds no `gpu-inventory` entry.
  Name it yourself (`WVA_LIMITER=gpu-inventory,quota`) to give borrowing its
  GPU bound.

#### Other changes that need an action

- **A namespace-scoped controller ignores `utilizationShare.physicalGroups`.**
  Only a cluster-scoped controller plans physical groups. Under a quota the
  setting changed nothing before and changes nothing now. Under `gpu-inventory`
  alone, a namespace-scoped controller now plans no group: it logs a warning
  and sets `utilizationShareNote` in `wva-controller-status`. The optimizer
  mode is unchanged. Check every tenant controller that reads a shared policy:
  ```bash
  kubectl get cm wva-controller-status -n "$NS" -o jsonpath='{.data.utilizationShareNote}{"\n"}'
  ```
  If it prints a note, give the namespace a quota, or remove `physicalGroups`
  from the shared policy.
- **OpenShift: a cluster admin runs `make setup-prereqs`.** The ServiceMonitor
  now scrapes as `wva-metrics-scraper`, and the old non-expiring token Secret
  `wva-controller-manager-token` is deleted. Only the admin phase does both.
  `make deploy-wva INSTALL_PHASE=wva` alone keeps scraping with the old token.
  ```bash
  NAMESPACE=<namespace> make setup-prereqs ENVIRONMENT=openshift   # (cluster admin)
  # out-of-reach shape: the controller runs in wva-<namespace>
  NAMESPACE=<namespace> make setup-prereqs ENVIRONMENT=openshift WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>
  # cluster scope
  make setup-prereqs ENVIRONMENT=openshift SCOPE=cluster WVA_NS=<controller namespace>
  ```
- **The install stops when KEDA is absent**, before it creates anything. It
  prints the command that installs KEDA
  (`KEDA_HELM_INSTALL=true NAMESPACE=<namespace> make setup-prereqs`). The
  controller-only phase (`INSTALL_PHASE=wva`) never installs KEDA, so there it
  always stops.
- **kube-prometheus-stack is pinned** (`KUBE_PROMETHEUS_STACK_CHART_VERSION`,
  default `92.7.0`). A newer release already installed is kept, with a
  warning. Set the variable to install that exact version, including an older
  one.
- **KEDA's chart is now 2.21.0** (`KEDA_CHART_VERSION`, was `2.19.0`). KEDA up
  to 2.20.2 has CVE-2026-77524 (GHSA-637c-6jxx-4rwm): a TriggerAuthentication
  can make the KEDA operator send its own ServiceAccount token to an address
  its author controls. A new install with `KEDA_HELM_INSTALL=true` gets 2.21.0.
  A re-run does **not** upgrade a KEDA that is already running: it finds KEDA
  Ready and skips Helm. Upgrade it yourself, with the flags the installer uses:
  ```bash
  helm upgrade keda kedacore/keda --version 2.21.0 -n keda-system \
    --set prometheus.metricServer.enabled=true --set prometheus.operator.enabled=true \
    --wait --timeout=5m
  ```
  Existing ScaledObjects and the scaler's mutual TLS keep working across it;
  KEDA's client certificate lives in a Secret the chart does not own. Before
  upgrading a KEDA that also serves other tenants, read
  [KEDA's 2.21 migration notes](https://keda.sh/docs/2.21/migration/): Vault
  Kubernetes authentication and `boundServiceAccountToken` triggers now need an
  explicit token audience. This project uses neither.
- **`wva_learned_state_restores_total` has new `outcome` values.** `installed`
  and `rejected` are gone; the label is now `whole`, `args`, `prior`,
  `incomplete` or `differs`
  ([the metric](prometheus.md#wva_learned_state_restores_total)). `installed`
  is `whole` plus `args`; `rejected` is `prior`, `incomplete` and `differs`.
  Rewrite queries and alerts:
  ```promql
  # was: wva_learned_state_restores_total{outcome="installed"}
  sum without (outcome) (wva_learned_state_restores_total{outcome=~"whole|args"})
  # was: wva_learned_state_restores_total{outcome="rejected"}
  sum without (outcome) (wva_learned_state_restores_total{outcome=~"prior|incomplete|differs"})
  ```
- **Rename a quota entry named `gpu-limiter`, `node-fit`,
  `pd-pair-borrows-whole` or `lender-cannot-reclaim`, before upgrading.**
  These names are reserved for what a held-back decision is reported under
  when no quota entry held it ([reserved names](quota-limiter.md#reserved-names)).
  This release rejects a `quota` entry with one: the `default` entry holding
  it is rejected (`policyAccepted: "false"`, and `policyError` says
  `quota entry name "<name>" is reserved`), and a controller starting on it
  refuses to start: `kubectl logs` of the new pod says
  `quota entry name "<name>" is reserved ... rename the entry`, and the
  rollout stalls with the old pod still serving. `make deploy-wva` checks
  first and stops before applying anything, with the same message, when the
  policy it keeps (`WVA_LIMITER` unset) or the one in `wva-policy` has such an
  entry. It cannot check a policy it is not allowed to read, such as
  `wva-policy` from a namespace-scoped install, or a policy namespace other
  than `wva-policy`. v0.10.0-rc1 to rc4 only warned (the log line
  `WARNING: Quota limiter entry has a reserved name` and the `limiterWarnings`
  status key); an rc install showing either has such an entry. Read the
  policy itself rather than the status: the controller's own
  `wva-scaling-policy-config`, and the one in `wva-policy` if that namespace
  governs the install. Each line prints how many entry names it read before
  the reserved ones, so a policy that was not read is not taken for a pass:
  ```bash
  for ns in <controller namespace> wva-policy; do
    if ! entry=$(kubectl get configmap wva-scaling-policy-config -n "$ns" -o jsonpath='{.data.default}'); then
      echo "$ns: not read"; continue
    fi
    names=$(printf '%s\n' "$entry" | grep -E '^[[:space:]-]*name:' | sed -E 's/.*name:[[:space:]]*"?([^"[:space:]]*)"?.*/\1/')
    reserved=$(printf '%s\n' "$names" | grep -xE 'gpu-limiter|node-fit|pd-pair-borrows-whole|lender-cannot-reclaim')
    echo "$ns: $(printf '%s' "$names" | grep -c .) entry names read, $(printf '%s' "$reserved" | grep -c .) reserved"
    printf '%s\n' "$reserved"
  done
  ```
  `wva-policy: not read` is expected when no policy namespace exists. Only a
  `quota` entry needs renaming; a `gpu-inventory` entry may keep any name.
  Renaming an entry renames its `limiter_name` series and the name in its
  Events: update any alert or query that selects it.
- **A parked model refused a wake has new reasons on
  `wva_model_scaling_blocked`.** `wake-no-capacity` when no serving set fits
  the GPU budget or the nodes, and `wake-nominal-lent-out` when the
  ClusterQueue lent its nominal to a peer it cannot reclaim from. A parked
  model used to show `nominal-lent-out` for the second;
  `nominal-lent-out` now marks only a capped serving model. Point any selector
  on `reason="nominal-lent-out"` meant for parked models at
  `reason="wake-nominal-lent-out"`
  ([reasons](prometheus.md#wva_model_scaling_blocked)).
- **Before rolling the controller back to an earlier release, remove the
  `llm-d.ai/forget-learned-state` annotation** from every scale target that
  carries it. The earlier release rewrites the learned-state snapshot without
  the record of which value was acted on, so upgrading again with the
  annotation still in place forgets the variant a second time
  ([reset one variant](troubleshooting.md#reset-one-variants-learned-state)):
  ```bash
  kubectl annotate deployment <name> -n <namespace> llm-d.ai/forget-learned-state-
  ```

#### Prometheus TLS verification is on by default

Earlier releases skipped TLS verification for every Prometheus. This release
verifies the certificate of any `https://` Prometheus, except the one the
installer deploys itself, which has no CA to verify with.

A re-run keeps what the controller runs with now:

- **A CA path, same Prometheus.** `PROMETHEUS_CA_CERT_PATH` is kept, and so is
  the verification setting it had. If verification was off, it stays off, and
  the installer warns that the CA path is unused. A volume and mount you added
  with `kubectl patch` survive re-runs. With GitOps, keep them in your overlay.
- **A CA path, different `PROMETHEUS_URL`.** The CA path is dropped, because
  the new Prometheus may use another CA. The installer warns, and prints the
  re-run that keeps it.
- **Verification off, no CA, same Prometheus.** It stays off, and the
  installer warns
  `TLS verification for <url> stays OFF, as it was before this upgrade`.
  Turning it on with no CA would stop the controller connecting.
- **Verification off, but a different `PROMETHEUS_URL`.** The new Prometheus
  is verified, and the installer says so. Pass
  `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` to keep it off.

An explicit `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` or `PROMETHEUS_CA_CERT_PATH`
always wins.

To turn verification on, do one of these:

- Mount the CA that signed the Prometheus certificate, and re-run with
  `PROMETHEUS_CA_CERT_PATH=<file>` ([TLS configuration](prometheus.md#tls-configuration)).
- Re-run with `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=false`, if a public CA signed
  the certificate.

When a Prometheus setting changes, the installer restarts the controller. The
controller reads these settings only when it starts.

If the installer cannot read the controller's current config, it stops before
changing anything, and asks you to re-run with
`PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` or `=false`. It does not stop when
you pass `PROMETHEUS_CA_CERT_PATH`, or for the Prometheus it deploys itself.

The re-run commands the installer prints carry `PROMETHEUS_URL`, `IMG`,
`DEPLOY_PROMETHEUS=false` and `ENVIRONMENT` when you set them. Add every other
variable you installed with.

#### Patch on pods moves to Role `wva-pod-writer`

The controller's ClusterRole no longer lets it patch pods. Each namespace that
needs it gets Role `wva-pod-writer` instead. Only warm pools and the
utilization-share optimizer patch pods. A namespace-scoped controller keeps
`patch` in its own Role, so nothing changes for it.

For a cluster-scoped controller, the upgrade grants `wva-pod-writer` before it
removes the old grant. It does so in each namespace that holds:

- a warm pool that calls this controller,
- a pod with a utilization-share transfer mark, of a model that calls this
  controller, or
- any model that calls this controller, when the utilization-share optimizer
  is active (not shadow). The re-run also removes the `optimizer:` block
  ([Re-running the installer resets the default entry](scaling-policy.md#re-running-the-installer-resets-the-default-entry)),
  so run these again, in order:
  ```bash
  make utilization-share-shadow NAMESPACE=<models namespace> CONTROLLER_NAMESPACE=<controller namespace>
  make utilization-share-activate NAMESPACE=<models namespace> CONTROLLER_NAMESPACE=<controller namespace>
  ```

It prints each namespace it granted in. Then read its warnings:

- **`Could not grant the controller patch on pods in <namespace>`**: that warm
  pool stops lending, or the optimizer cannot clear its marks there. A cluster
  admin runs the command the warning prints.
- **`... are UNKNOWN`**: the installer could not list ScaledObjects or pods
  across the cluster. Re-run the upgrade as a cluster admin, or grant by hand in
  each namespace with a warm pool or an active optimizer, from the repository
  root:
  ```bash
  bash deploy/lib/pod_writer.sh <namespace> <controller namespace> | kubectl apply -f -
  ```
- **`Could not tell whether the utilization-share optimizer is active`**: the
  installer could not read the controller's status or policy. If the optimizer
  is active, run the command above in each namespace the warning lists.
- **`... does not say which controller they call`**: a ScaledObject declares a
  warm pool or a model, but its `scalerAddress` is not
  `wva-external-scaler.<namespace>...`. If it calls this controller, run the
  command above in its namespace.

A cluster without the ScaledObject kind (`SCALER_BACKEND=none`) has no warm
pools or models to grant for, and the upgrade says nothing.

#### Policy that starts to apply

Before you upgrade, save what the controller resolves today:

```bash
kubectl logs -n "$NS" deploy/wva-controller-manager | grep 'Effective scaling policy' > before.txt
```

Each line names a model and its `scaleUpThreshold`, `scaleDownBoundary`,
`kvCacheThreshold` and `priority`. It does not show `queueLengthThreshold` or
`analyzers`. A line is logged once per change, not every cycle, so a
controller whose log has rotated may print nothing. If `before.txt` is empty,
keep a copy of your ConfigMaps instead.

Some settings in named tiers and per-model entries were ignored or replaced
before. They now apply. Before you upgrade, read your
`wva-scaling-policy-config` **and** every namespace-local scaling-policy
ConfigMap, and work through this list.

1. **Fields a tier or per-model entry leaves out.** They now come from the
   layer below (the tier, then `default`), not from built-in values.

   | field the entry leaves out | tier before | per-model before | now |
   | --- | --- | --- | --- |
   | `kvCacheThreshold` | 0.80 | 0.80 | the layer below |
   | `queueLengthThreshold` | 5 | 5 | the layer below |
   | `priority` | 1.0 | 1.0 | the layer below |
   | `scaleUpThreshold`, `scaleDownBoundary` (entry sets `analyzerName: saturation` or `analyzers`) | 0.85, 0.70 | 0.85, 0.70 | the layer below |
   | `analyzers` (entry sets `analyzerName: saturation`) | `[saturation]` | `[saturation]` | the layer below |

   To keep the old behaviour, set the old value in the entry explicitly, for
   example `kvCacheThreshold: 0.80`.

2. **Settings that were ignored in a tier or per-model entry.**

   | field | tier before | per-model before | now |
   | --- | --- | --- | --- |
   | `scaleToZero` | ignored (except the tier `default.defaultPolicy` names) | applied | applied; a model parks only if every tier its variants name allows it, and waits for the longest `retentionPeriod` |
   | `scaleFromZero.requirePrefill` | ignored | ignored | applied; a model waits for prefill if any of its tiers requires it |
   | `shapeChangeHoldSeconds` | ignored | ignored | applied |
   | `disableShapeChangeHold` | ignored | ignored | applied |

   Nothing reports these the first time they take effect. To keep the old
   behaviour, remove the field from the tier (from a per-model entry, only for
   the rows marked ignored there).

   Consequences to check:

   - A tier that enables `scaleToZero` now parks its models after
     `retentionPeriod`, even when `default` disables it. The first request
     after that waits for a cold start. Disable `scaleToZero` on the tier to
     keep them up.
   - A tier that disables `scaleToZero` now keeps its models up, even when
     `default` enables it. `WVAModelWillNotScaleToZero` may start firing for
     those models (reason `policy-forbids-zero`). The controller log line
     `Scale-to-zero is disabled by the scaling policy of a tier` lists the
     tiers in `disabledBy`. Enable `scaleToZero` on each, or on the model's
     per-model entry, or accept the alert. If the per-model entry is what
     disables it, the line says so (`disabledByPerModelEntry=true`).
   - A tier or per-model `requirePrefill: true` now applies. A P/D model whose
     prefill cannot be placed stays at zero rather than waking decode alone.

   A variant that names no tier follows the variants of its model that do. If
   only the decode of a P/D model names a tier, the prefill now follows that
   tier too.

3. **`scaleToZero.retentionPeriod` values that cannot be used.** An unusable
   value (not a duration, such as `5 minutes`) used to fall back to 10m. It now
   falls back to the value of the layer below (the tier, then `default`, then
   10m), so the model may wait longer or shorter than before. Values under 30s
   are now unusable too. Fix the value. A skipped value is reported as a
   `ConfigFieldIgnored` event on the ConfigMap, in `wva_config_ignored_fields`,
   in the log, and for the `default` entry in the `policyIgnored` key of the
   [`wva-controller-status` ConfigMap](operations.md#what-the-controller-is-running-with).
   A negative `shapeChangeHoldSeconds` is skipped and reported the same way.
   So are a non-finite number (`.inf`, `-.inf`, `.nan`) in `priority`, the
   thresholds, the scaling band, or an analyzer's `score` or band; a negative
   analyzer `score`; and a `priority` or `score` above 1000000. An entry that
   was rejected before only because of such a value (for example
   `scaleUpThreshold: .inf`) now applies, with that field from the layer below.
   A `priority: .nan` used to act as 1 in the fair share, and under rescale
   gave the model nothing above its floor; it now takes the layer below's
   value, so under rescale such a model can grow and take GPUs from the others
   in its budget.

4. **`terminationGracePeriodSeconds` patches below 40.** The controller now
   takes up to 40s to stop: it finishes the running cycle, then releases its
   lease. Raise the patch to 40, or remove it. A patch left at 10 is no worse
   than before; it only loses the faster failover
   ([details](operations.md#stopping-and-leader-handover)).

5. **Analyzers listed beside or instead of `saturation` now size the model.**
   Before, only saturation sized it and the others were published and ignored.
   Read every `analyzers:` list in every scaling-policy ConfigMap, per-model
   and tier entries included ([details](scaling-policy.md#multi-analyzer-registration)):
   - beside `saturation`, the model is sized to the larger need;
   - without `saturation`, the others size it alone, and a role none of them
     can size holds where it is. A list of `slo` alone holds the model.

   The `Effective scaling policy` line does not show the list, so the check
   below cannot catch this. After the upgrade:
   - a held model reports `wva_model_scaling_blocked{reason="no-composite-signal"} == 1`
     or logs `composite holds a role`;
   - a model another analyzer now sizes above saturation shows a composite
     demand above saturation's:
     `wva_analyzer_demand{analyzer_name="CompositeSignal"} > on (exported_namespace, model_name, role) wva_analyzer_demand{analyzer_name="saturation"}`.

   Dashboards and alerts on `wva_analyzer_*` must filter on `analyzer_name`:
   the series gain `analyzer_name="CompositeSignal"`.

After the upgrade, compare the controller's `Effective scaling policy` log
lines with the ones you saved in `before.txt`:

```bash
kubectl logs -n "$NS" deploy/wva-controller-manager | grep 'Effective scaling policy' > after.txt
diff before.txt after.txt
```

Other changes you may notice:

- **The TTFT SLO alert is `WVAModelSLOAttainmentLow`.** `WVAModelSLOMissed`
  now covers the TPOT mean only. Move any Alertmanager route or silence that
  matches `WVAModelSLOMissed` for TTFT to the new name, or TTFT misses go
  unrouted, or page where they were silenced.
- A scale-from-zero wake records one `ScaledUp` Event. It used to record one
  on every 100ms tick until KEDA scaled the target, so a dashboard that counts
  `ScaledUp` Events will show far fewer.
- A SIGTERM during startup, for example from the startup probe after about 60s
  while Prometheus is unreachable, is now logged as
  `Shutdown requested during startup` and exits 143. Previously the process was
  killed by the signal with no log line; the exit code was 143 as well.
- A scale-from-zero wake claims a utilization-share transfer with the weight of
  the model's tier, as the planner does. It used to use the `default` weight.
- The `gpu-policy` readiness check no longer goes NotReady on a passing error
  when listing nodes, such as a timeout. It is not ready until the API server
  first answers. When it is not ready, its message now reads
  `cannot confirm it may list nodes` (it read `may not list nodes`).
- A P/D model whose idle role's pool is empty may now scale up its other role.
  The idle role used to block it.
- A decode-only model with `requirePrefill: true` now claims utilization-share
  transfers for its decode. It used to claim none.

### External scaler TLS on upgrade

Mutual TLS on the scaler port is now the default. Most installs already have
ScaledObjects, and a plain upgrade leaves those on plaintext and lists them.
To move to TLS, run:

```bash
make deploy-wva WVA_SCALER_TLS=true WVA_SCALER_TLS_MIGRATE=true
```

It moves the controller and its ScaledObjects together. The ScaledObjects move
only once the new controller Pods alone serve the scaler: the rollout is done,
the old Pods are gone from the Service, and a new Pod holds the leader lease.
This can take a minute or two. KEDA's calls for each ScaledObject may fail for
a few seconds while it moves.

After the move, the installer checks that KEDA wired each HPA: the HPA carries
the scaler's external metric. KEDA asks for that metric only when a
ScaledObject's spec changes. If that one call fails, the HPA stays on the CPU
default, and the ScaledObject still reads Ready. So when an HPA is not wired,
the installer changes one trigger metadata key, `wvaRewire`, which makes KEDA
ask again. It tries for about three minutes. It never deletes a ScaledObject
and never restarts KEDA. If the HPA is still not wired, the install and
`make verify-deployment` name the ScaledObject and print the command to run.

After a move, KEDA keeps trying the old address, about once a minute or two.
The scaler refuses it and logs `refused a TLS handshake` at a limited rate.
This is expected and harmless: no ScaledObject uses that connection. KEDA
keeps one connection per address and closes none until its operator restarts.
The installer cannot close it without restarting KEDA, so it does not.

**Reinstall in the same namespace: restart KEDA's operator.** After
`make undeploy-wva` and a new install in the same namespace, KEDA still holds
its connection to the same scaler address, with the old client certificate.
The new CA rejects it, so the HPA stays on `cpu` and the ScaledObject reads
Ready but not Active. KEDA logs `tls: unknown certificate authority` and
`got empty metric spec`. The controller logs `refused a TLS handshake` with
`certificate signed by unknown authority`, from KEDA's Pod IP, about once a
minute. Changing the ScaledObject (`wvaRewire`) does not help. Only a restart
does:

```bash
kubectl rollout restart deployment/keda-operator -n <KEDA namespace>
```

The installer cannot tell a reinstall from a first install: the uninstall
removed everything it wrote. So `make setup-prereqs` warns on every first
write of KEDA's side while KEDA runs. `make verify-deployment`, and the
installer after it moves ScaledObjects, read the controller's log. When they
find those refusals from a KEDA Pod, they name this cause and the restart.

- **Split install:** KEDA's side must be in place first. A cluster admin runs
  `make setup-prereqs`; then the tenant runs the command above. A tenant's
  `make deploy-wva` cannot write KEDA's side, and stays on plaintext until the
  admin has.
- **GitOps:** run the command, then commit the
  [new trigger](configuration.md#external-scaler-mutual-tls) (the TLS address
  and the `authenticationRef`) to git right away. Otherwise GitOps puts the old
  one back and those ScaledObjects stop reaching the scaler.

To stay on plaintext, set `WVA_SCALER_TLS=false`. Check the result with
`make verify-deployment`.

### The external-scaler NetworkPolicy

`wva-external-scaler` admits `:9090` only from KEDA's namespace. The admin phase
(`make setup-prereqs`) writes it, and only when it **knows** where KEDA runs. In
order, it takes:

1. `KEDA_NAMESPACE`, set explicitly;
2. the namespace serving the external-metrics APIService, when that is KEDA's;
3. `KEDA_NAMESPACE`'s default with `KEDA_HELM_INSTALL=true`, when that
   APIService does not exist yet (this install puts KEDA there);
4. the namespace an existing `wva-external-scaler` policy already admits.

So a re-run with `KEDA_NAMESPACE` corrects a policy that names the wrong
namespace; with nothing to go on, a wrong policy stays wrong. When none answers,
no policy is written, the installer warns that `:9090` is open, and
`make verify-deployment` prints `No NetworkPolicy: scaler port open`.

```bash
KEDA_NAMESPACE=<keda ns> NAMESPACE=<namespace> make setup-prereqs
# out-of-reach shape
KEDA_NAMESPACE=<keda ns> NAMESPACE=<namespace> make setup-prereqs WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>
# cluster-scoped install
KEDA_NAMESPACE=<keda ns> make setup-prereqs SCOPE=cluster WVA_NS=<controller ns>
```

What the installer leaves alone, and reports as `left as it is, state UNKNOWN`
(with `kubectl get networkpolicy wva-external-scaler -n <ns> -o yaml` to check
it):

- a policy it cannot read;
- a policy that admits `:9090` from anything other than exactly one namespace;
- any policy, when the controller-only phase (`INSTALL_PHASE=wva`) runs with
  `WVA_SCALER_NETWORKPOLICY=false`. Only the admin phase removes one.

`make check-prereqs` and the controller-only phase list the policy as optional.

- Run `make verify-deployment` with the same `NAMESPACE` (and `WVA_NS`,
  `WVA_WATCH_NS` for the out-of-reach shape) and read its NetworkPolicy lines. A
  namespace you cannot see KEDA from is reported as not confirmed, not as
  correct, and a policy you cannot read as not checked, not as open.
- Earlier versions of these docs had you apply a policy by hand, named
  `wva-external-scaler-from-keda`. NetworkPolicies add up, so it still admits
  whatever namespace it names to `:9090`, and the installer neither reads nor
  updates it. Delete it once the shipped one is in place:
  ```bash
  kubectl delete networkpolicy wva-external-scaler-from-keda -n "$NS" --ignore-not-found
  ```

## After every upgrade or restart

A new controller pod starts with no decision in memory. Until it has one:

- **Until its first cycle completes** (one `GLOBAL_OPT_INTERVAL`, 15 s by
  default, plus leader election), the scaler answers KEDA's `GetMetrics` with
  each target's current replica count. The HPA holds, and KEDA sees no error.
  `wva_scaler_undecided_answers_total{reason="no-decision"}` grows meanwhile
  and stops once the first cycle publishes.
- **While no controller pod is serving**, KEDA cannot reach the scaler. The
  generated `fallback` covers that: after more than `failureThreshold` failed
  reads in a row (3, at about one read every 15 s) KEDA holds the current
  count. A short rollout may log a `KEDAScalerFailed` Event or two and never
  reach the fallback. See [before the first
  decision](scaledobject-trigger.md#before-the-first-decision-and-the-fallback).
- **Under the utilization-share optimizer**, every planned model holds what it
  runs for one fill timeout, about three minutes with default timings, and
  shows the blocked reason `quiet-period`. Expect it on every upgrade and leader
  change.
- **On KEDA 2.19 and older, a ScaledObject created or changed during the
  outage, or within about two minutes after it, can stay unwired**: Ready, but
  its HPA carries only `cpu`. It is a KEDA bug fixed in 2.20
  ([details](keda.md#unwired-hpa-before-keda-220)); upgrading KEDA to 2.21
  stops it. On older KEDA run
  `NAMESPACE=<namespace> make scaledobjects-rewire`, adding
  `WVA_NS=wva-<namespace>` in the out-of-reach shape; without it the command
  finds nothing and exits 0
  ([#340](https://github.com/ev-shindin/llm-scaling-manager/issues/340)).
- **What it learned is held in memory by default.** Learned capacity and
  throughput are re-learned after every restart or leader change, so a request
  shape it had already priced is sized from occupancy again until it saturates
  once more. `LEARNED_STATE_SNAPSHOT=true` carries it across instead
  ([learned state across a restart](configuration.md#learned-state-across-a-restart)).
  A snapshot written by v0.10.0 or earlier has no ITL slope, so on the first
  restart after upgrading from it each decode variant fits its ITL line, and
  so mu, from fresh readings; from the next restart on the line can come
  back ([on these terms](configuration.md#learned-state-across-a-restart)).

Do not judge an upgrade by the first few minutes of decisions.

## Uninstall

Pass the **same** `ENVIRONMENT`, `SCOPE` and namespace you installed with; the
uninstall renders the same overlay and deletes what it renders.

```bash
make undeploy-wva NAMESPACE=<namespace>
```

If the controller was installed in a namespace of its own (`WVA_NS`), as in the
out-of-reach shape where it runs in `wva-<namespace>` and manages `<namespace>`,
pass both, or the uninstall looks for the controller in the wrong place:

```bash
make undeploy-wva NAMESPACE=<namespace> WVA_NS=wva-<namespace>
```

### What it removes

| object | note |
| --- | --- |
| the controller Deployment, its ServiceAccounts, Services and ServiceMonitor | |
| `wva-manager-config` and `wva-scaling-policy-config` | back up your policy first |
| the install's ClusterRoles and ClusterRoleBindings | suffixed per install, so other installs keep theirs. Cluster-scoped: a namespace owner gets Forbidden on these, and the uninstall says which are left for a cluster admin |
| the controller's Role and RoleBinding in a namespace it managed from outside (`WVA_WATCH_NS`) | |
| the EPP `/metrics` authentication ClusterRoleBindings this install made (`llm-d.ai/applied-for=$NS`), and ClusterRole `wva-epp-metrics-auth` with the last of them | cluster-scoped: a namespace owner cannot read them, and the uninstall prints the cluster admin's `kubectl delete` command |
| RoleBinding `wva-pod-writer-<suffix>` in every workload namespace, and Role `wva-pod-writer` where no other controller's binding uses it | made by `deploy/warmpool.sh create`, utilization-share activate, and the upgrade to this release |
| `wva-utilization-share-history`, `wva-controller-status` and `wva-learned-state` | written by the controller, not rendered, so deleted by name. An uninstall and reinstall therefore starts learned state cold; upgrade in place to keep it |
| the ScaledObjects **this installer created** | KEDA restores each workload to its pre-autoscaling replica count as they go. Keep them with `UNDEPLOY_SCALEDOBJECTS=false` |

### What it leaves

| left behind | why | clean up |
| --- | --- | --- |
| the namespace `$NS` | it may hold your model servers | `kubectl delete namespace "$NS"` only if nothing else lives there |
| ScaledObjects you **adopted** (written by you or GitOps, pointed at this scaler) | not the installer's to delete. Their trigger now calls a scaler that is gone, and **a model parked at zero never wakes**: activation only comes from the scaler | see below |
| Prometheus, KEDA, the EPP | shared; this install may not have created them | `UNDEPLOY_SHARED=true` removes them too, for everything on the cluster |
| KEDA's connection to the scaler's TLS address, with the old client certificate | KEDA keeps it until its operator restarts. It matters only if you install the scaler in the same namespace again: the HPA then stays on `cpu` until KEDA restarts. The uninstall warns when a ScaledObject used that address, and `make verify-deployment` names it after the reinstall ([details](#external-scaler-tls-on-upgrade)) | (cluster admin) after the reinstall, `kubectl rollout restart deployment/keda-operator -n <KEDA namespace>` |
| the PrometheusRule `controller-manager-alerts` | the uninstall does not render the alerts component | `kubectl delete prometheusrule controller-manager-alerts -n "$NS"` |
| the dashboard ConfigMap `wva-operation-dashboard` | shared by every install on the cluster | (cluster admin, if it is in a shared monitoring namespace) `kubectl delete configmap wva-operation-dashboard -n <dashboard-namespace>` |
| cluster policy in the policy namespace (`wva-policy`, or the one a `wva.llmd.ai/policy-namespace` label names): the ConfigMap, Role `wva-policy-reader` and this install's RoleBinding `wva-policy-reader-<suffix>` | the ConfigMap is written by `make enable-physical-limiter`; the Role and RoleBinding by it and by `make setup-prereqs` (for a controller that manages its own namespace, when the policy namespace exists) | (cluster admin) see below |
| ClusterRoles and bindings from an install older than per-install suffixes | they may be bound by another install | (cluster admin) delete by hand once no install uses them |
| Role `wva-pod-writer` and RoleBinding `wva-pod-writer-<suffix>` the uninstall could not read or delete | it may not list RoleBindings across the cluster, or delete them there. It lists each one with its `kubectl delete` command, or says it could not look | (cluster admin) see [Leftover pod-write grants](#leftover-pod-write-grants) |
| Role `wva-pod-writer` in a namespace another controller also uses | that controller's binding still needs it | nothing; it goes with that controller |
| Role `wva-pod-writer` and RoleBinding `wva-pod-writer-<suffix>` everywhere, when the controller Deployment is still there | the delete failed, or its result could not be read. A running controller still needs the grant | remove the Deployment, then run the uninstall again |
| utilization-share transfer marks on pods | written while the optimizer acted | [Transfer marks left on pods](troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade); set `shadow: true` and wait a cycle **before** uninstalling to avoid them |

### Leftover pod-write grants

List the pod-write bindings, and the controller namespace each one is for:

```bash
kubectl get rolebinding -A -l app.kubernetes.io/name=workload-variant-autoscaler \
  -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,ROLE:.roleRef.name,CONTROLLER:.subjects[0].namespace' \
  | grep -E '^NAMESPACE|wva-pod-writer'
```

For each row whose `CONTROLLER` is the namespace you uninstalled:

```bash
kubectl delete rolebinding <NAME> -n <NAMESPACE>
```

Then delete the Role in that namespace, once no other `wva-pod-writer-*`
binding is left there:

```bash
kubectl delete role wva-pod-writer -n <NAMESPACE>
```

### Adopted ScaledObjects

The uninstall lists them. To find them later:

```bash
kubectl get scaledobject -n <ns> \
  -o custom-columns=NAME:.metadata.name,ADDR:.spec.triggers[0].metadata.scalerAddress \
  | grep "wva-external-scaler.$NS"
```

Then, for each:

- **Another install serves the model**: `make scaledobjects-repoint` rewrites
  `scalerAddress` on objects that name a namespace where no scaler runs.
- **Going back to what scaled it before**: change its trigger, in GitOps if it
  is managed there.
- **No autoscaling any more**: delete it, then set the replica count you want.
  KEDA restores the original count only when the object has
  `advanced.restoreToOriginalReplicaCount: true`; otherwise the workload stays
  where it is, which for a parked model is zero.
  ```bash
  kubectl delete scaledobject <name> -n <ns>
  kubectl scale deployment <name> -n <ns> --replicas=1
  ```

### Policy namespace and limiter leftovers (cluster admin)

The per-install suffix is the first eight hex digits of the SHA-256 of the
controller's namespace (`wva-<namespace>` in the out-of-reach shape). The policy
namespace is the `policyNamespace` in the controller's `wva-controller-status`
ConfigMap, else `wva-policy`. The uninstall removes that ConfigMap, so read it
first; afterwards, the managed namespace's `wva.llmd.ai/policy-namespace` label
names it, if it is set:

```bash
POLICY_NS=$(kubectl get cm wva-controller-status -n "$NS" \
  -o jsonpath='{.data.policyNamespace}' 2>/dev/null)
POLICY_NS="${POLICY_NS:-wva-policy}"
SUFFIX=$(printf '%s' "$NS" | sha256sum | cut -c1-8)
kubectl delete rolebinding "wva-policy-reader-$SUFFIX" -n "$POLICY_NS"
kubectl get clusterrole,clusterrolebinding | grep -- "-$SUFFIX"   # anything the uninstall could not remove
```

Only when no scaling-manager install remains on the cluster:

```bash
make disable-limiter WVA_LIMITER_TYPE=all   # every limiter, from wva-policy and every policy ConfigMap a controller reads
kubectl delete namespace "$POLICY_NS"
```

`WVA_LIMITER_TYPE=all` removes every limiter, of either kind, and exits 0 also
where none is declared, so it is safe in an uninstall script.

With `WVA_LIMITER_TYPE=gpu-inventory` (the default) or `quota` it removes that
kind only and keeps the other. Run on a policy that holds only the other kind,
it removes nothing and exits non-zero, naming the kind that stays: `make` prints
`make: *** [Makefile:<line>: disable-limiter] Error 1` and itself exits
2.

`disable-limiter` leaves the read-only RBAC in place on purpose; the
namespace delete removes it. Like `enable-physical-limiter`, it needs `kubectl`,
`jq` and `yq`. Where no limiter is declared it changes nothing and prints
`No limiter declared in <ns>/wva-scaling-policy-config; nothing to remove.`,
then warns `Nothing removed`, and exits 0. A
failed edit stops with an error naming the step, and never leaves an emptied
ConfigMap.

If the uninstall ran as a namespace owner and reported Forbidden objects, a
cluster admin re-runs the same `make undeploy-wva` with the same variables.
