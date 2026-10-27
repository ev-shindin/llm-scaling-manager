# Install methods and platform notes

How to run the installer in cases the [guides](../guides/) do not cover: GitOps,
a direct Kustomize apply, or seeing exactly what the script does.

> The guides are the supported paths. This page is reference.

## Deployment methods

### Method 1: Automated Deployment Script (Recommended)

The script sets up:

- the scaling manager controller and its RBAC;
- a Prometheus stack, or a connection to an existing one;
- llm-d infrastructure (Gateway, Scheduler, vLLM);
- KEDA for external metrics (ScaledObject-driven);
- ServiceMonitors for metric collection;
- GPU detection and environment-specific settings.

ScaledObjects are yours to create: they are how a workload reaches the scaling
manager.

#### Quick Start with Make

```bash
# Set required environment variable
export HF_TOKEN="hf_xxxxxxxxxxxxx"

# Deploy to Kubernetes
make deploy-wva

# Deploy to OpenShift
make deploy-wva-on-openshift

# Deploy to Kind (with emulated GPUs)
CREATE_CLUSTER=true make deploy-e2e-infra
```

#### Manual Script Execution

```bash
# Navigate to deploy directory
cd deploy

# Set environment variables
export HF_TOKEN="hf_xxxxxxxxxxxxx"
export ENVIRONMENT="kubernetes"  # or "openshift", "kind-emulator"

# Run deployment script
bash install.sh
```

#### Script Configuration Options

The script takes command-line flags and environment variables.

**Command-line flags** (`deploy/install.sh`):

```text
bash install.sh [OPTIONS]

Options:
  -i, --wva-image IMAGE    WVA container image (default: ghcr.io/ev-shindin/llm-scaling-manager:main)
  -c, --check              Run the prerequisite and permission checks, then exit
  -p, --phase PHASE        prereqs | wva | all (default: all) — see deploy/README.md
  -u, --undeploy           Undeploy WVA, monitoring, and scaler (not llm-d)
  -e, --environment ENV    kubernetes | openshift | kind-emulator
  -h, --help               Show help
```

The default image tag is the `newTag` in `config/base/manager/kustomization.yaml`:
`main` on a checkout of `main`, the release tag on a checkout of a release. So
`git checkout v0.10.1` installs `:v0.10.1` without `-i`. See
[Pin the version](upgrade-and-uninstall.md#pin-the-version).

`--check` is what `make check-prereqs` runs. Run it before a real install. It
checks the tools, the cluster connection, and that you may create everything the
install creates; missing rights are the usual reason an install dies halfway.

**llm-d stack** (gateway, EPP, ModelService): deploy it with the [llm-d
guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline). For
an EPP only (llm-d-router-standalone chart + tokenreview RBAC), run
`deploy/install-epp.sh` after `install.sh`.

**Environment variables**: every one the script reads is in the
[Configuration Reference](configuration.md).

#### Script deployment examples

`install.sh` creates no **ScaledObjects**. Create them with `kubectl apply`, or
let your tests or operators manage them. KEDA creates and owns the HPA behind
each one; never create an HPA yourself.

##### Example 1: Base the scaling manager infra + EPP

```bash
./deploy/install.sh -e kubernetes
# EPP (llm-d-router-standalone chart + RBAC):
LLM_D_ROUTER_VERSION=v0.9.0 GAIE_VERSION=v1.5.0 NAMESPACE=llm-d-optimized-baseline \
  ./deploy/install-epp.sh
# Model server: follow llm-d/llm-d guides/optimized-baseline
```

##### Example 2: E2E-style stack (same as `make deploy-e2e-infra`)

```bash
make deploy-e2e-infra ENVIRONMENT=kind-emulator IMG=localhost/llm-scaling-manager:dev
```

##### Example 3: The scaling manager and monitoring only (no llm-d)

```bash
export DEPLOY_WVA=true
export DEPLOY_PROMETHEUS=true
export DEPLOY_OPERATIONAL_DASHBOARD=true
export SCALER_BACKEND=keda
./deploy/install.sh -e kubernetes
```

##### Example 4: Install with LeaderWorkerSet (for full e2e suite)

```bash
export DEPLOY_LWS=true
./deploy/install.sh -e kubernetes
```

### Method 2: Kustomize (GitOps and direct installs)

The overlays are plain Kustomize, so you can apply them yourself: for Argo CD or
Flux, or to review what lands. Scope, namespace and limiter are the same choices
as above; see [Choosing your install](../guides/).

**The overlays alone are not a complete install.** `deploy/install.sh` does four
things around them:

| What the script does | If you skip it |
| --- | --- |
| Writes `PROMETHEUS_URL` into the controller ConfigMap | The controller looks healthy but reads no metrics: it uses the shipped in-cluster default, which is not your Prometheus |
| Renames the ClusterRoles and ClusterRoleBindings per install, with a hash of the namespace | A second install **takes the first install's bindings**, which silently loses all its permissions |
| Checks that no incompatible install already exists | Two controllers scale the same workloads |
| Keeps the Namespace on undeploy, and deletes only this install's renamed ClusterRoles | `kubectl delete -k` removes the **Namespace** (and the model servers in it) and the unrenamed **ClusterRoles**, breaking every other install that binds them |

Use this method when you install **one** scaling manager and wire Prometheus
yourself, or when a GitOps tool owns the manifests. Otherwise use Method 1.

#### Applying the overlays directly

```bash
# Use the manifests of the release you install. A release checkout already
# names its own image, so the image line below only makes it explicit.
git checkout v0.10.1

# Set the controller image: the release tag, or its digest from the release notes
cd config/base/manager
kustomize edit set image controller=ghcr.io/ev-shindin/llm-scaling-manager:v0.10.1
# kustomize edit set image controller=ghcr.io/ev-shindin/llm-scaling-manager@sha256:<digest>

# Apply the overlay for your scope and platform
kubectl apply -k ../../overlays/cluster-scoped/kubernetes
#             ../../overlays/namespace-scoped/kubernetes
#             ../../overlays/cluster-scoped/openshift
#             ../../overlays/namespace-scoped/openshift
```

Then point the controller at your Prometheus. Without this it collects nothing:

```bash
# Set the Prometheus URL and restart (a raw apply -k lands in wva-system; only install.sh rewrites it to $WVA_NS)
kubectl -n wva-system edit configmap wva-manager-config
# under config.yaml, set:
#   prometheus:
#     baseURL: https://<your-prometheus>.<ns>.svc.cluster.local:9090
kubectl -n wva-system rollout restart deployment wva-controller-manager
```

Running **more than one** scaling manager on this cluster? Rename the
ClusterRoles and ClusterRoleBindings in your overlay first, or the second
install takes them from the first. The script's patch set is
`deploy/lib/common.sh:wva_append_crb_name_patches`; the names are in
`WVA_SHARED_CLUSTER_ROLE_BINDINGS` and `WVA_OWNED_CLUSTER_ROLES`.

#### Undeploy

`kubectl delete -k` is **not** the inverse of the apply. The overlay holds a
Namespace and the shared ClusterRoles; deleting them takes the workloads in that
namespace and every other install's permissions. Delete only this install's own
objects:

```bash
kubectl kustomize config/overlays/cluster-scoped/kubernetes \
  | yq 'select(.kind != "Namespace" and .kind != "ClusterRole")' \
  | kubectl delete -f - --ignore-not-found
```

`./deploy/install.sh --undeploy` does exactly this.


## After the controller: warm pools

Installing the scaling manager creates no warm pool, and a namespace does not
need one to autoscale. A pool trades held accelerators for faster scale-up, so it
pays only where a model's load time really costs something. [Weights and the
model cache](workload-preparation.md#weights-and-the-model-cache) explains why
that load is not a storage problem.

To see whether this namespace has models that could share one:

```bash
deploy/warmpool.sh plan -n <namespace>
```

`plan` only reads. It groups the namespace's models by accelerator and devices
per replica (the two things a warm copy cannot change), and prints a `create`
command for each group it can serve. It also lists pools that are declared but
unused, or selected but missing.

The sizing arguments it prints are placeholders. They set the Pod memory limit,
which *is* the warm-set budget, and only you know how many models of what size a
pool must hold at once. `deploy/warmpool.sh sizing -n <namespace>` answers that
against this cluster's nodes.

Reasoning, configuration and removal: the [warm pool guide](../guides/warm-pool/).

## Platform-specific guides

- **[Kubernetes Guide](../../deploy/kubernetes/)**: kube-prometheus-stack setup, GPU operator installation, and ServiceMonitor configuration
- **[OpenShift Guide](../../deploy/openshift/)**: User Workload Monitoring (Thanos), Routes, Security Context Constraints (SCC), and the GPU operator on OpenShift
- **[Kind Guide (Local Testing)](../../deploy/kind-emulator/)**: local development and testing with Kind and emulated GPUs

Each has examples, troubleshooting and quick start commands, and uses the same
[Configuration Reference](configuration.md).
