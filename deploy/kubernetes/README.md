# Installing on Kubernetes

What `make deploy-wva` does on a Kubernetes cluster, and the Kubernetes-only
settings. For the full walk-through start with a guide —
[install in a namespace](../../docs/guides/install-in-namespace/) is the common
case — and for every variable see the
[configuration reference](../../docs/reference/configuration.md).

```bash
export NAMESPACE=<namespace where llm-d already serves>
make check-prereqs ENVIRONMENT=kubernetes     # read-only
make deploy-wva   ENVIRONMENT=kubernetes
```

`ENVIRONMENT` is detected when unset (a cluster that serves `route.openshift.io`
is OpenShift; anything else is Kubernetes). `make deploy-wva-on-k8s` is the same
install with `ENVIRONMENT=kubernetes` fixed.

## Before you start

- **llm-d is already running in `NAMESPACE`.** The scaling manager does not deploy
  models or llm-d; it scales what is there. The preflight refuses to install
  when it cannot find model-server and EPP metrics (`SKIP_CHECKS=true` overrides).
- **KEDA is installed on the cluster.** On Kubernetes the installer does **not**
  install KEDA unless you ask: set `KEDA_HELM_INSTALL=true` to have it install
  KEDA with Helm into `keda-system`.
- **Tools:** `kubectl`, `helm`, `git`, `jq`, and go-yq v4.
- **Rights:** a namespace install splits in two. A cluster admin runs
  `make setup-prereqs` once per namespace; the namespace owner then runs
  `make deploy-wva`, which installs only the controller when the admin half is
  already there. See [cluster-admin setup](../../docs/guides/admin-cluster-setup/).

## What the install creates

| Component | Where | Notes |
| --- | --- | --- |
| Controller `wva-controller-manager` | `NAMESPACE` (namespace scope, the default); `workload-variant-autoscaler-system` with `SCOPE=cluster` | 1 replica (`WVA_REPLICAS`); extras are leader-election standbys |
| External scaler Service `wva-external-scaler` | same namespace, port 9090 (gRPC) | what every ScaledObject trigger names |
| ConfigMaps `wva-manager-config`, `wva-scaling-policy-config` | same namespace | |
| kube-prometheus-stack | `workload-variant-autoscaler-monitoring` | only when the cluster has no Prometheus of its own; otherwise the existing one is used (`PROMETHEUS_URL`, `PROMETHEUS_FORCE_INSTALL`) |
| Grafana dashboard ConfigMap | the monitoring namespace | `DEPLOY_OPERATIONAL_DASHBOARD=false` to skip |
| KEDA | `keda-system` | only with `KEDA_HELM_INSTALL=true` |
| LeaderWorkerSet CRD | `lws-system` | only with `DEPLOY_LWS=true` |

It creates **no ScaledObject** unless asked. Without one, nothing is scaled: the
scaling manager learns a workload exists only when KEDA calls it about one.

```bash
make scaledobjects-plan      # lists your model servers; applies nothing
make scaledobjects-apply
```

or pass `WVA_DEFAULT_SO=plan|edit|true` to the install. See
[Default ScaledObjects](../../docs/reference/configuration.md#default-scaledobjects).

## Settings you are most likely to change

| Variable | Default | Meaning |
| --- | --- | --- |
| `NAMESPACE` | none (required in namespace scope) | where llm-d serves; the controller is installed there too |
| `SCOPE` | `namespace` | `cluster` installs one controller for every namespace (work in progress) |
| `IMG` | `ghcr.io/ev-shindin/llm-scaling-manager:main` on `main`; the release tag on a release checkout (`git checkout v0.10.2`) | controller image; [pin a release](../../docs/reference/upgrade-and-uninstall.md#pin-the-version) for anything beyond development |
| `PROMETHEUS_URL` | detected | the Prometheus the controller reads |
| `KEDA_HELM_INSTALL` | `false` | install KEDA with Helm |
| `WVA_LIMITER` | `none` | `none` leaves scaling unbounded except by `maxReplicaCount`; `gpu-inventory,quota` bounds by both; see [bounding GPU usage](../../docs/guides/admin-gpu-bounding/) |
| `SKIP_CHECKS` | `false` | skip the preflight |

`deploy/install.sh --help` prints every variable the installer reads.

## Check it

```bash
make verify-deployment
kubectl get scaledobject,hpa -n "$NAMESPACE"
kubectl logs -n "$NAMESPACE" deploy/wva-controller-manager -f
```

KEDA creates and owns the HPA behind each ScaledObject (`keda-hpa-<name>`); do
not edit it by hand. The scaling manager's decision reaches KEDA over gRPC, so
it does not appear on the external-metrics API; read it from Prometheus as
`wva_desired_replicas`. For a workload that does not scale, see
[troubleshooting](../../docs/reference/troubleshooting.md).

## Remove it

```bash
make undeploy-wva ENVIRONMENT=kubernetes
```

Pass the same `NAMESPACE` and `SCOPE` you installed with. Prometheus, KEDA and
the EPP are left in place unless `UNDEPLOY_SHARED=true`; namespaces are kept
unless `DELETE_NAMESPACES=true`.
