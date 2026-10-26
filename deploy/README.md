# Deploying the scaling manager

The scaling manager is a KEDA **external scaler**: it decides how many replicas each llm-d model
needs and hands that to KEDA, which owns the HPA and does the scaling.

**Start with the [guides](../docs/guides/).** Each is a complete path:

| | |
| --- | --- |
| [Install the scaling manager in a namespace](../docs/guides/install-in-namespace/) | the common case, new or existing llm-d |
| [Install the scaling manager for the whole cluster](../docs/guides/install-cluster-wide/) | one controller for everything |
| [Cluster-admin setup](../docs/guides/admin-cluster-setup/) | enabling a namespace's owner |
| [Bound every install by real GPUs](../docs/guides/admin-gpu-bounding/) | the GPU budget |
| [Test against a full llm-d stack](../docs/guides/testing-with-llm-d/) | kind, emulated GPUs |
| [Benchmark the scaling manager](../docs/guides/benchmarking/) | load and comparison |

## The four commands

| | |
| --- | --- |
| `make check-prereqs` | read-only: rights, namespace, Prometheus |
| `make setup-prereqs` | cluster admin, once per namespace |
| `make deploy-wva` | install; `INSTALL_PHASE=wva` for the controller alone |
| `make undeploy-wva` | remove |

`SCOPE=namespace` (default) or `cluster`. `ENVIRONMENT` is detected; set
`kubernetes` or `openshift` to force it. Everything else is optional —
[Configuration reference](../docs/reference/configuration.md).

## Two things every path shares

**Nothing scales until a ScaledObject exists.** The scaling manager has no watch and no listing:
it learns a workload exists only when KEDA calls it about one.

```bash
make scaledobjects-plan     # lists your model servers; applies nothing
make scaledobjects-apply
```

**Without a limiter, scaling is unbounded** — bounded only by each workload's
`maxReplicaCount`. [Bounding GPU usage](../docs/guides/admin-gpu-bounding/)
is the one command that changes that, and it is an admin's to run.

> **Coming from llm-d's docs?** The scaling manager ships **no CustomResourceDefinition**. A
> workload is registered by a KEDA ScaledObject whose `external-push` trigger names the
> scaling manager's external scaler: `wva-external-scaler.<controller-namespace>.svc:9090`
> with an `authenticationRef` on a TLS install (the default), or
> `wva-external-scaler.<controller-namespace>.svc.cluster.local:9090` on a plaintext one
> ([external scaler mutual TLS](../docs/reference/configuration.md#external-scaler-mutual-tls)). Instructions to
> apply a `VariantAutoscaling` CRD, or to point an HPA or Prometheus Adapter at a
> `wva_` metric, describe upstream WVA and do not apply here.
> Moving an upstream install: [Coming from llm-d's workload-variant-autoscaler](../docs/guides/from-upstream-wva/).
