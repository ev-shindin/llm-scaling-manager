# llm-scaling-manager documentation

llm-scaling-manager decides how many replicas of each LLM model server should
run on Kubernetes. It reads how loaded the servers are, sizes every model for
its load, keeps the total inside a GPU budget if you set one, and hands the
number to KEDA, which scales the workloads. It works with
[llm-d](https://llm-d.ai) model servers running vLLM or SGLang.

![How it fits together](concepts/figures/how-it-fits.svg)

The picture, explained: [how the scaling manager fits together](concepts/architecture.md).
Evaluating it? [Why llm-scaling-manager](why.md) is the one-page case: what it
was measured to do, what it costs, and how mature each part is.

**Start here.** Pick your scenario in the [well-lit paths](well-lit-paths/)
index, then follow the guide that path links to. Nothing installed yet? The
[three steps below](#getting-started-in-three-steps) come first.

## Terms you will meet

| Term | Meaning |
| --- | --- |
| **model** | what a client asks for by name, e.g. `meta-llama/Llama-3.1-8B-Instruct` |
| **variant** | one way of serving a model (a GPU type, a serving configuration): one KEDA ScaledObject and the workload it scales. Variants with the same `modelID` are sized together |
| **replica** | one Pod of a variant |
| **EPP** | llm-d's endpoint picker: the router in front of a model's replicas. Its queue is a demand signal, and the only one at zero replicas |
| **saturation** | how full a replica is: KV-cache use and waiting requests. The default scaling signal |
| **cycle** | the timer on which every model is re-sized: 15 s by default |
| **tier** | a named set of scaling thresholds a ScaledObject selects with `scalingPolicy:` |
| **limiter** | an optional GPU budget every decision must fit: free GPUs, a quota, or a Kueue quota |
| **P/D** | prefill/decode disaggregation: a model's prefill and decode run as separate, separately scaled variants |

## Getting started in three steps

1. **Have a model to scale.** llm-d serving in a namespace, with the EPP's
   `flowControl` feature gate on, plus KEDA and Prometheus on the cluster.
   Nothing serving yet? [Install a small llm-d model](guides/install-small-model/) first.
2. **Install the scaling manager** into that namespace. A cluster admin runs
   the one-time setup; the namespace owner runs the rest. That default suits a
   single team or a trusted cluster. On a shared cluster, the admin runs the
   controller out of the tenant's reach: `WVA_NS=wva-<ns> WVA_WATCH_NS=<ns>`
   on `setup-prereqs` and `deploy-wva`
   ([cluster-admin setup](guides/admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach)).
   Either way, the bound that holds is a GPU ResourceQuota on the namespace
   ([bound at admission](guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).

   ```bash
   export NAMESPACE=<the namespace running llm-d>
   make check-prereqs          # read-only: your permissions, the namespace, Prometheus
   make setup-prereqs          # once per namespace, by a cluster admin
   make deploy-wva             # the scaling manager itself
   ```

3. **Prepare and register your model servers.** Nothing is scaled until it has a ScaledObject:

   ```bash
   make workload-patch         # writes a patch (drain hook, weights, engine cache); apply it through your model-server chart
   make scaledobjects-plan     # lists your model servers, writes an editable plan
   make scaledobjects-apply    # creates the ScaledObjects
   ```

   With the controller out of your reach, add `WVA_NS=wva-<ns> WVA_WATCH_NS=<ns>`
   to both, so every trigger names the controller in `wva-<ns>`.

The full walk-through, with what each step checks:
[install the scaling manager in a namespace](guides/install-in-namespace/).

### What success looks like

```bash
kubectl get scaledobject,hpa -n $NAMESPACE
```

Each ScaledObject shows `READY True`, and its HPA's `TARGETS` shows a number
such as `1/1 (avg)`. `cpu: <unknown>/80%` means KEDA could not reach the
scaling manager: [first-line troubleshooting](reference/operations.md#first-line-troubleshooting).
After a cycle or two, the controller's log shows its decisions (its namespace
is `$NAMESPACE`, or `wva-$NAMESPACE` when the admin keeps it out of your reach;
then ask the admin, or read `wva_desired_replicas` in Prometheus):

```bash
kubectl logs -n $NAMESPACE deploy/wva-controller-manager | grep scaling-decision
```

## Where to go next

| You want to | Start here |
| --- | --- |
| Solve a specific problem | **[Well-lit paths](well-lit-paths/)**: one page per scenario |
| Look something up | **[Reference](#reference--what-you-set-and-read)** |
| Understand a decision it made | **[Concepts](#concepts--how-the-scaling-manager-decides)** |
| Change the scaling manager itself | **[Developing the scaling manager](#developing-the-scaling-manager)** |

## Well-lit paths — start from the problem

Recipes for one scenario each, meant to be documented, tested and benchmarked;
not every path clears all three yet. Each page names the suites and the
benchmark scenario behind it, and says which leg is short when one is.

| Path | For |
| --- | --- |
| [Scale a model on saturation](well-lit-paths/scale-on-saturation/) | the default: replica counts that follow real serving load |
| [Scale to zero, and get back](well-lit-paths/scale-to-zero/) | releasing an idle model's accelerators, and waking it |
| [Bridge a scale-up with a warm pool](well-lit-paths/warm-pool-bridge/) | spikes that arrive faster than a replica loads |
| [Hold several large models on one set of GPUs](well-lit-paths/retained-pool/) | more large models than hardware to run them — **experimental** |
| [Bound a fleet by real GPUs](well-lit-paths/bound-by-gpus/) | a shared cluster where `maxReplicaCount` is not a real ceiling |
| [Cap what each tenant may take](well-lit-paths/tenant-gpu-quotas/) | per-namespace or cluster-wide GPU allowances |
| [Bound tenants by the quotas Kueue already holds](well-lit-paths/kueue-bounded-quotas/) | the allowance is Kueue's nominal quota, followed live — **experimental** |
| [Share one GPU quota between models](well-lit-paths/utilization-share/) | several models under one quota, each given what its load needs and the rest shared — **experimental** |
| [Different behaviour per workload class](well-lit-paths/workload-classes/) | interactive and batch, tuned as classes rather than per model |
| [One model, two accelerator variants](well-lit-paths/accelerator-variants/) | letting the optimizer pick the cost-efficient hardware |
| [A P/D-disaggregated model](well-lit-paths/pd-disaggregation/) | prefill and decode scaled apart — **experimental** |

## Guides — the steps

Every guide runs one task from nothing to working, in the same shape: *Overview →
Prerequisites → Installation → Verification → Cleanup → Configuration*. The
commands are generated from each guide's `guide.yaml`, so they cannot drift from
what they document. Index and conventions: **[guides/](guides/)**.

**Installing** — [in a namespace](guides/install-in-namespace/) (the common
case) · [cluster-wide](guides/install-cluster-wide/) · [a small llm-d model
first](guides/install-small-model/), if nothing is serving yet ·
[coming from llm-d's workload-variant-autoscaler](guides/from-upstream-wva/)

**Administering** — [cluster-admin setup for a
namespace](guides/admin-cluster-setup/) · [bound every install by real
GPUs](guides/admin-gpu-bounding/)

**Exercising** — [scale to zero](guides/scale-to-zero/) · [borrow from a
Kueue cohort](guides/kueue-borrowing/) · [warm
pool](guides/warm-pool/) · [test against a full llm-d
stack](guides/testing-with-llm-d/) · [benchmark the scaling manager](guides/benchmarking/)

## Reference — what you set and read

- **[Configuration](reference/configuration.md)** — every variable the installer reads, and which settings need a restart
- **[After the install](reference/operations.md)** — verifying it worked, and first-line troubleshooting
- **[Watching what the scaling manager decides](reference/monitoring.md)** — the dashboard, who owns it, and the metrics that answer specific questions
- **[The cycle log](reference/cycle-log.md)** — the two lines the scaling manager emits per cycle, their fields and reason codes: the page that answers "why did it scale?"
- **[Scaling policy](reference/scaling-policy.md)** — thresholds, tiers, scale-to-zero, limiters, the utilization-share optimizer
- **[Preparing a workload](reference/workload-preparation.md)** — the model cache, draining before scale-down, `make workload-patch`
- **[Replica start time](reference/start-time.md)** — every measured start time, what each preparation step and the warm pool buy, and what each costs
- **[ScaledObject trigger metadata](reference/scaledobject-trigger.md)** — every key the trigger carries (`modelID`, `variantCost`, `scalingPolicy`, `defaultOutputTokens`, warm-pool keys), its default and validation, and the recommended HPA behavior
- **[Install methods](reference/install-methods.md)** — installer, kustomize, and per-platform entry points
- **[KEDA](reference/keda.md)**: supported versions, a KEDA the cluster already has, and what the installer prints about it
- **[The GPU limiter](reference/gpu-limiter.md)** and **[the quota limiter](reference/quota-limiter.md)** — bounding the scaling manager by real accelerators, and by declared caps
- **[Security model](reference/security-model.md)** — identities, every permission by install scope, what it writes, the ports it opens, and what fails open
- **[Metrics and health](reference/metrics.md)** · **[Prometheus integration](reference/prometheus.md)**
- **[Router configuration](reference/router-configuration.md)** — the two router settings that decide what the scaling manager sees: flow control bounds the queue it reads as demand, and the scheduling profiles decide whether the replicas it orders get used
- **[SGLang backend](reference/sglang-backend.md)** — auto-detected per variant; nothing to configure
- **[Troubleshooting](reference/troubleshooting.md)**
- **[Upgrade and uninstall](reference/upgrade-and-uninstall.md)** — pinning a version, what a restart costs, and what an uninstall leaves behind

## Concepts — how the scaling manager decides

- **[How the scaling manager fits together](concepts/architecture.md)**: the parts, one decision cycle, what gets installed. Start here
- **[The steady-state engine](concepts/steady-state-engine.md)** — what it measures, and how a measurement becomes a replica count
- **[GPU capacity accounting](concepts/gpu-capacity-accounting.md)** — what each limiter bounds, how they combine, node fit, Kueue borrowing, and where the figures can still be wrong
- **[Modeling and optimization](concepts/modeling-and-optimization.md)** — what is modelled, what is not, and how per-variant demands become replica counts
- **[llm-d autoscaling](https://llm-d.ai/docs/architecture/advanced/autoscaling)** — upstream's page, describing the autoscaler this project forked from; kept for context, not as a description of this one

## Developing the scaling manager

- **[Development setup](developer-guide/development.md)** · **[Testing](developer-guide/testing.md)** · **[Debugging](developer-guide/debugging.md)**
- **[Multi-analyzer pipeline](developer-guide/multi-analyzer-pipeline.md)** — how analyzers are registered, run and scored
- **[Saturation analyzer](developer-guide/saturation-analyzer.md)** — how it decides, with every formula and where each one lives
- **[Which component owns which number](developer-guide/signal-dataflow.md)** — the chain from a metric scrape to a replica count, and where every parameter in the arithmetic comes from
- **[Throughput analyzer](developer-guide/throughput-analyzer.md)** · **[saturation demand floor](developer-guide/saturation-demand-floor.md)** · **[pod scraping source](developer-guide/pod-scraping-source.md)**
- **[slo analyzer and the ITL tracker](developer-guide/slo-analyzer.md)** — one cycle of the observe-only SLO analyzer, its engine hooks, and the line fit it shares with saturation
- **[Analyzer checklists](developer-guide/analyzer-checklists.md)** — what a new analyzer must show before it graduates
- **[Analyzer evidence](developer-guide/analyzer-evidence.md)** — what each scaling signal was measured to do, and on which run
- **[Benchmark internals](developer-guide/benchmark-guide.md)** · **[two-variant benchmark](developer-guide/two-variant-wva-benchmark.md)** · **[recorded results](developer-guide/benchmark-results.md)** · **[an example k2 decision report](developer-guide/benchmark-k2-decisions-example.md)**
- **[How the limiter set is built](developer-guide/limiter-composition.md)** — reading, building and rebuilding the declared limiters, skipping an unavailable one, the scale-from-zero snapshot
- **[Kueue borrowing internals](developer-guide/kueue-borrowing.md)** — what is read from Kueue, where borrowing is counted, spent and given back, and how it is tested
- **[Anatomy of a replica start](developer-guide/replica-start-anatomy.md)** — where the seconds of a vLLM start go, the levers ruled out, and the experimental snapshot restore
- **[Release process](developer-guide/release-process.md)** · **[Contributing](../CONTRIBUTING.md)**

## What this project is

- **[Project proposal](project-proposal.md)** — the case for an analytical autoscaler: what it does, what is measured, what it does not cover, mapped against the llm-d autoscaling problem space
- **[blog/](blog/)** — posts written for people outside the repository

## Design notes

- **[proposals/](proposals/)** — work not built yet, or built and still moving. The warm pool's argument and implementation design live here, as does the [FMA post-mortem](proposals/fma-post-mortem.md) explaining what the pool inherited.
- **[roadmap.md](roadmap.md)** — planned work that changes what an operator can rely on, each item linked to its GitHub issue.
- **[comparison/](comparison/)** — how the scaling manager's autoscaling compares with [KEDA alone (llm-d's recipes)](comparison/keda-alone.md), Dynamo, Mooncake, SGLang and the hosted platforms, and how [sharing a GPU budget between models](comparison/gpu-sharing.md) compares with llm-d's Kueue rebalancing.
- **[plans/](plans/)** — agent plans and the specs behind them.

## Elsewhere in the repo

- [Main README](../) · [Kubernetes deployment](../deploy/kubernetes/) · [OpenShift deployment](../deploy/openshift/) · [kind emulator, for local development](../deploy/kind-emulator/)

## Need help?

- [Troubleshooting](reference/troubleshooting.md) first, then [after the install](reference/operations.md)
- [Open an issue](https://github.com/ev-shindin/llm-scaling-manager/issues)
