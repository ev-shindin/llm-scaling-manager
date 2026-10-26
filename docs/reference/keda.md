# KEDA

The scaling manager hands each model's replica count to KEDA through a
ScaledObject, and KEDA does the scaling. This page covers which KEDA versions
are supported, what the installer does with a KEDA the cluster already has, and
when it installs one.

## Supported versions

- **KEDA 2.21**: supported. It is the first release with the
  CVE-2026-77524 fix (below). The kind e2e suites pass on it. It supports
  Kubernetes 1.34 to 1.36.
- **KEDA 2.19**: supported. It supports Kubernetes 1.32 to 1.34.
- **KEDA 2.18**: supported. The benchmarks have run on 2.18.1.
- **Which one CI tests**: the installer's default `KEDA_CHART_VERSION`. The
  kind e2e suites install chart `kedacore/keda` at that version, and
  `make check-prereqs` prints it, as `CI tests <version>`.
- **Any other version**: untested. Nothing in the code needs a minimum
  version, so the installer reports the version and goes on.
- **Before 2.17**: ScaledObjects get no fallback, because
  `spec.fallback.behavior` arrived in KEDA 2.17. The installer warns about it.
- **OpenShift**: the Custom Metrics Autoscaler operator. CI pins no version of
  it. The installer reads that operator's own version and does not map it to a
  KEDA version.

The code builds against the KEDA v2.21.0 Go module.

### CVE-2026-77524

KEDA up to 2.20.2 has
[CVE-2026-77524](https://github.com/kedacore/keda/security/advisories/GHSA-637c-6jxx-4rwm).
A tenant who may write a TriggerAuthentication can make the KEDA operator send
its own ServiceAccount token to an address the tenant controls. KEDA 2.21.0
fixes it.

KEDA 2.18 and 2.19 are affected. For any version below 2.21.0, the installer
prints this as advice and does not stop. From 2.21.0 on it prints nothing. Read
KEDA 2.21's upgrade notes before you upgrade: the release lists breaking
changes.

### Upgrading a KEDA the cluster already has

Re-running the installer never upgrades a KEDA that is already there, even
with `KEDA_HELM_INSTALL=true`. If this installer (or Helm) installed it,
upgrade it with Helm:

```bash
helm repo update kedacore
helm upgrade keda kedacore/keda -n keda-system --version 2.21.0 --reuse-values
```

Use your release name and namespace if they differ (`helm list -A | grep keda`).
On OpenShift, upgrade the Custom Metrics Autoscaler operator instead.

## What the installer prints

`make check-prereqs`, `make setup-prereqs` and `make deploy-wva` print one KEDA
version line. None of these lines stops the install.

- `KEDA 2.21.0: supported (KEDA 2.18, 2.19 and 2.21; CI tests <version>).`
- `KEDA 2.20.2: untested, and not refused.` It names the supported versions.
- `KEDA 2.19.0 carries CVE-2026-77524 ... Advice only: consider upgrading KEDA
  to 2.21.0 or later.` It also prints the `helm upgrade` command above.
- `KEDA: could not check its version (...)`. The version is unknown, which is
  not a pass. The reason is one of these:
  - Listing Deployments cluster-wide is not permitted for you.
  - No Deployment is labelled `app.kubernetes.io/name=keda-operator`.
  - The image is pinned by digest.
  - The image tag is not a version, such as `latest`.

  To check by hand, run
  `kubectl get deploy -A -l app.kubernetes.io/name=keda-operator -o wide`.
- With `KEDA_HELM_INSTALL=true` and no KEDA on the cluster, the same judgement
  covers the chart this run installs: `KEDA chart <version> (this run installs
  it): supported`.
- On OpenShift: `KEDA: Custom Metrics Autoscaler <version>, untested`, read
  from its ClusterServiceVersion in `openshift-keda`. If that cannot be read,
  the line says so.

The version comes from the image tag of the operator Deployment.

## A KEDA the cluster already has

Leave `KEDA_HELM_INSTALL` off, which is the default. The installer then uses the
KEDA it finds:

- **Kubernetes, `KEDA_HELM_INSTALL` off:** the installer never installs KEDA.
  It looks for the `scaledobjects.keda.sh` CRD:
  - If the CRD is found, the installer uses this KEDA.
  - If reading the CRD is Forbidden, the installer warns and assumes KEDA is
    there.
  - If the CRD is not found, the installer stops before it applies anything,
    and prints the command that installs KEDA.
- **Kubernetes, `KEDA_HELM_INSTALL=true`:** the installer skips Helm in two
  cases. One is when the CRD, a Ready operator and the external-metrics
  APIService are all there. The other is when the CRD is there and the
  `keda-operator` ClusterRole is not managed by Helm. Otherwise it installs chart
  `kedacore/keda` at `KEDA_CHART_VERSION` into `KEDA_NAMESPACE` (default
  `keda-system`).
- **OpenShift:** the installer never installs KEDA. Install the Custom Metrics
  Autoscaler operator and create its KedaController in `openshift-keda` first.

### The scaler's TLS in KEDA's namespace

The cluster-admin phase (`make setup-prereqs`) writes KEDA's side of the
scaler's mutual TLS:

- a copy of the client certificate Secret, in the namespace where KEDA reads
  Secrets
- a ClusterTriggerAuthentication that points at that copy

It finds where KEDA runs from the first of these:

1. `KEDA_NAMESPACE`, when you set it.
2. The namespace of the Service behind the `v1beta1.external.metrics.k8s.io`
   APIService, when that Service is KEDA's.
3. The namespace this run installs KEDA into.

If KEDA's operator sets `KEDA_CLUSTER_OBJECT_NAMESPACE`, the installer puts the
copy there instead.

If the installer cannot tell where KEDA runs, it warns and the scaler stays on
plaintext. With `WVA_SCALER_TLS=true`, it refuses instead. Set
`KEDA_NAMESPACE=<the namespace KEDA runs in>` and run the command again.

Sometimes the installer writes KEDA's side for the first time while KEDA is
already running. It then prints a warning and the command to restart KEDA's
operator. On a reinstall, KEDA keeps its old connection to the scaler until its operator restarts, and every call is refused. The installer never restarts KEDA's operator on its own, because other workloads share it. `make scaledobjects-apply`, `make deploy-wva` and `make verify-deployment` detect the stale connection, list the ScaledObjects that stay unwired, and print the restart command. With `KEDA_RESTART=true` they restart KEDA, and so does `make undeploy-wva` once its ScaledObjects are gone.

KEDA asks the scaler for a ScaledObject's metric spec only when the ScaledObject changes, and does not retry a failed call. The HPA then stays on the CPU default. The controller re-wires such a ScaledObject on its own: once it is a minute old and its HPA lacks the scaler's External metric, it changes the trigger key `wvaRewire`, with a backoff of 1 minute doubling to 30 (`--scaledobject-rewire=false` turns this off). `make scaledobjects-apply` also re-wires such ScaledObjects after it applies them, and `make scaledobjects-rewire` does it at any time: it changes the trigger key `wvaRewire`, which the scaler ignores, so KEDA asks again. `DRY_RUN=true` only lists them.

Both cover every ScaledObject naming this scaler in the namespaces the install watches, including hand-written and GitOps-owned ones. A GitOps tool shows the `wvaRewire` key as drift; leave it, or add it to the source. Both also name the KEDA operator restart when every HPA is wired but KEDA's calls are refused. `make scaledobjects-rewire` exits 1 while one is unwired or KEDA is refused, 2 when the ScaledObjects cannot be listed or `DRY_RUN` is not `true` or `false`, and 3 when one could not be read. The first wait before a re-wire is `WVA_VERIFY_WIRE_WAIT` seconds (default 30).

## Uninstall

`make undeploy-wva` never removes KEDA. It removes only the
ClusterTriggerAuthentication and Secret copy it wrote, which carry its label.

`UNDEPLOY_SHARED=true` also removes shared infrastructure. On Kubernetes, it
removes KEDA only when `KEDA_HELM_INSTALL=true` is also set: it uninstalls the
Helm release and deletes `KEDA_NAMESPACE`. Pass both only if this installer
installed KEDA. On OpenShift it never removes KEDA.
