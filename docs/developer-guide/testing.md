# Testing Guide

How to run and write tests for the scaling manager.

There are three layers:

1. **Unit tests** - fast tests for one package.
2. **Integration tests** - envtest suites: a real API server, no controllers.
3. **E2E tests** - one suite, run on Kind (emulated GPUs) or OpenShift/Kubernetes, in a smoke tier and a full tier.

## Unit Tests

### Running Unit Tests

```bash
# All unit tests
make test

# One package
go test ./internal/engines/analyzers/saturation/...

# Verbose
go test -v ./internal/controller/...

# Coverage. Never `go test ./...`: it also runs test/e2e against your current
# kube context, deleting and restarting things on whatever cluster that is.
go test -coverprofile=coverage.out $(go list ./... | grep -v -e /e2e -e /benchmark)
go tool cover -html=coverage.out -o coverage.html
```

`make test` runs `hack/check-import-direction.sh` and
`hack/check-doc-comment-owner.py` first. It then tests every package except
`/e2e` and `/benchmark`, plus `./test/e2e/fixtures/`, with `KUBEBUILDER_ASSETS`
set for the envtest suites.

### Run one test

```bash
make setup-envtest
# 1.34 matches k8s.io/api v0.34 in go.mod; the Makefile derives it the same way
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.34 --bin-dir bin -p path)"

# A plain Go test
go test ./internal/engines/steadystate/ -run TestGPUConstraintsAreLogged -v

# One Ginkgo spec: -run selects the suite, -ginkgo.focus the spec
go test ./internal/engines/steadystate/ -run TestSaturationEngine \
  -ginkgo.focus "live GPU limiter rebuild" -ginkgo.v
```

Three packages start an envtest API server and need `KUBEBUILDER_ASSETS`:
`internal/controller`, `internal/actuator` and `internal/engines/steadystate`.

Always read the spec count. A focus that matches nothing runs 0 specs and
exits 0. E2E specs also `Skip()` at runtime on environment flags (for example
the scale-from-zero suites without `SCALE_TO_ZERO_ENABLED=true`), and a skip
reports as success. Run with `-ginkgo.v` and check what ran.

### Shell checks in `deploy/` and `hack/`

The installer parses pod specs in shell. That code has no Go test, so
fixture-driven `hack/check-*` scripts cover it. One target runs them all, and
CI runs it on every PR (`deploy-scripts-check`):

```bash
make lint-deploy-scripts
```

It runs `bash -n` on the deploy scripts and every `hack/check-*` script listed
in the target: parsers, manifests, make recipes, quoted `jq`/`yq` programs and
more. Other `hack/check-*` scripts (doc links, dashboards, import direction,
doc-comment owners) run from their own targets.

- The checks need `jq` and `yq`. They fail, not skip, when either is missing:
  a skip would make a green run meaningless.
- CI pins yq v4.44.3. Newer yq behaves differently in places, so reproduce a
  CI failure with that version.
- `check-install-defaults.sh` covers what a plain install ships: how `IMG` is
  split (registry ports, digests), the pull policy for a reference, the
  floating-tag and unverified-Prometheus-TLS warnings, and which Prometheus
  ServiceAccount may read the controller's `/metrics`.

A new fixture check follows the same convention (see AGENTS.md): a
`hack/check-*.sh` script, called from `lint-deploy-scripts`. A check behind a
new make target is one CI never runs.

`check-rendered-manifests.sh` also needs kustomize, so it runs in the
`kustomize-build` CI job instead. It renders the overlays, and the overlay the
installer wraps around them, and asserts what they contain:

```bash
make kustomize && KUSTOMIZE=bin/kustomize bash hack/check-rendered-manifests.sh
```

### Unit Test Structure

Tests sit next to the code they test. For example:

```text
internal/
├── engines/
│   ├── steadystate/
│   │   ├── engine_v2.go
│   │   ├── engine_v2_test.go
│   │   └── suite_test.go          # Ginkgo suite TestSaturationEngine (envtest)
│   └── analyzers/
│       └── saturation/
│           ├── analyzer.go
│           └── analyzer_test.go
└── collector/
    ├── replica_metrics.go
    └── replica_metrics_test.go
```

### Writing Unit Tests

A Ginkgo suite looks like this:

```go
package solver_test

import (
    "testing"
    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
)

func TestSolver(t *testing.T) {
    RegisterFailHandler(Fail)
    RunSpecs(t, "Solver Suite")
}

var _ = Describe("Solver", func() {
    Context("when optimizing single variant", func() {
        It("should calculate optimal replicas", func() {
            Expect(result).To(Equal(expected))
        })
    })
})
```

Good unit tests:

- use tables for many scenarios;
- mock external dependencies (Kubernetes API, Prometheus);
- cover edge cases (zero, negative, nil);
- run in milliseconds;
- have names that say what they check.

## Integration Tests

Integration tests use controller-runtime's envtest: a real Kubernetes API
server with no controllers. The project installs no CRD, so the suites load
none. See `internal/controller/suite_test.go` for the setup.

```bash
# Included in make test
make test

# Only the controller suite (needs KUBEBUILDER_ASSETS; see Run one test)
go test ./internal/controller/... -v
```

## End-to-End Tests

One suite in `test/e2e/` runs on Kind with emulated GPUs, or on
OpenShift/Kubernetes. Environment variables select the environment. The tests
create their own ScaledObjects, HPAs and model services.

- **Smoke** (~5-10 min): run on every PR.
- **Full** (~15-25 min): the deeper correctness checks.

E2E is a **deterministic correctness signal**: resource wiring,
reconciliation and stable invariants (ScaledObjects become Ready, KEDA creates
the HPA, scalers point at the right targets). Traffic and performance
scenarios belong outside `test/e2e/`.

### Quick Start

```bash
# Kind: create a cluster, deploy infra, run the smoke tier
CREATE_CLUSTER=true make test-e2e-smoke-with-setup

# Existing Kind cluster: deploy infra, then run the full tier
make deploy-e2e-infra
make test-e2e-full

# Smoke tier against infra that is already deployed
make test-e2e-smoke

# OpenShift
export KUBECONFIG=/path/to/openshift/kubeconfig
export ENVIRONMENT=openshift
make test-e2e-smoke          # or make test-e2e-full

# One spec by name (a Describe or It text)
FOCUS="Basic Autoscaling" make test-e2e-smoke
```

- `test-e2e-smoke` runs label `smoke`; `test-e2e-full` runs
  `full && !smoke && !flaky`. The tiers are disjoint, so run both for full
  coverage. Specs labelled `flaky` run in neither.
- A `FOCUS` must name a spec in the tier you run. Check `Ran N of M Specs`:
  a focus that matches nothing runs 0 specs and exits green.
- Both targets port-forward Prometheus to `https://localhost:9090` for the
  run. Without it, the metric assertions skip.
- The `-with-setup` targets never delete the cluster. Delete it with
  `kind delete cluster --name kind-wva-gpu-cluster` (or your `CLUSTER_NAME`).

See the [E2E Test Suite README](../../test/e2e/) for every option.

### Infra the suite needs

`make deploy-e2e-infra` deploys the scaling manager, Prometheus, KEDA and the
llm-d EPP (`deploy/install-epp.sh`). It creates no ScaledObject or HPA; the
tests do. The controller goes into `CONTROLLER_NAMESPACE`, which defaults to
`llm-d-sim` on every environment.

- **Scale to zero.** With `SCALE_TO_ZERO_ENABLED=true`, `install-epp.sh`
  enables the EPP flowControl feature gate, so it exposes
  `inference_extension_flow_control_queue_size`. The scale-from-zero tests
  create the `e2e-default` InferenceObjective themselves.
- **Kueue.** Two specs, see [Kueue specs](#kueue-specs): the quota spec in
  the full tier, the borrowing spec in a CI job of its own.
- **Helm repo updates.** `SKIP_HELM_REPO_UPDATE=true` skips `helm repo update`
  during the install. It is faster, but the repo indexes must already exist.

### Kueue specs

- **`kueue_quota_test.go`** (label `kueue-quota`) checks a quota entry bounded
  by Kueue against a real API server. It needs only the three Kueue CRDs
  (ClusterQueue, LocalQueue, ResourceFlavor), not Kueue's controller.
- **`kueue_borrow_test.go`** (label `kueue-borrow`) needs Kueue's controller.
  It is excluded from the full tier's label filter and runs in its own CI job,
  `e2e-tests-kueue-borrow`. A loaded simulator model runs on one queue of a
  two-queue cohort (2 GPUs each):
  - a GPU pod in the namespace that Kueue does not manage withholds
    borrowing, so the model holds at its nominal 2;
  - chaos: the Kueue controller is scaled to 0 for 60 seconds while that pod
    goes; the model must not be ordered past its nominal 2 meanwhile. Kueue is
    restored on every exit path;
  - with that pod gone the model grows to 4, every pod placed, and no further;
  - a `Never` lender with a pending workload gets nothing back while
    `releaseToNonReclaimingLenders` is unset (the default); once the spec sets
    it, the lender gets exactly one replica back and its workload is admitted;
  - the lender, switched to `Any`, asks for more: Kueue preempts a borrowed
    pod, the scaling manager lowers the target so no gated replacement
    lingers, and the model borrows nothing until `borrowCooldown` ends.

  The policy sets short Kueue timings (`placementTimeout: 30s`,
  `borrowCooldown: 4m`, `lenderDebounce: 10s`) and declares `gpu-inventory`,
  which borrowing needs as its GPU bound. It needs 6 free GPUs of the suite's
  GPU product and prints `MEASURE` lines for replica start, the release cycle
  and the reclaim cycle.

`make test-e2e-full-with-setup` installs Kueue on Kind with
`make install-kueue-e2e`; CI's full job sets `KUEUE_CONTROLLER=false`, the CRDs
alone, since only the quota spec runs there. `make
test-e2e-kueue-borrow-with-setup` installs the controller
(`KUEUE_CONTROLLER=true`, the default), which runs
`deploy/ci-pr-checks/install-kueue.sh`. That script refuses a kubectl context
not named `kind-*` unless `KUEUE_ALLOW_NON_KIND=true`, and:

- downloads the pinned release manifest (`KUEUE_VERSION`, default `v0.20.0`,
  image from `registry.k8s.io`), checks its sha256, and applies it. Another
  version needs `KUEUE_MANIFEST_SHA256` too;
- waits for the rollout, then for the webhook to answer;
- narrows Kueue's job webhooks to namespaces labelled `llm-d.ai/kueue-e2e`
  (`KUEUE_WEBHOOK_NAMESPACE_LABEL`). Upstream they cover every namespace with
  `failurePolicy: Fail`, so a restarting Kueue would fail pod creation for the
  whole suite. A queue-labelled pod in an unlabelled namespace then runs
  unmanaged: fine on a throwaway Kind cluster, never on one whose quotas
  matter.

`KUEUE_CONTROLLER=false` runs `install-kueue-crds.sh` instead, which installs
the CRDs alone; the quota spec passes, the borrowing spec fails.

On Kind both specs **fail** when what they need is missing, so a missing setup
step is never read as coverage. On other clusters nothing installs Kueue, and
each spec skips (and says so in `-v` output) when the cluster lacks what it
needs; `REQUIRE_KUEUE_E2E=true` turns the borrowing spec's skips into failures
anywhere, and its CI job sets it. Both scripts refuse to change a Kueue
somebody else installed.

The CI job `e2e-tests-kueue-borrow` runs on a PR that touches
`internal/kueue/`, `internal/engines/allocation/borrow*`,
`internal/engines/steadystate/borrowing*`, `test/e2e/kueue*`,
`test/e2e/fixtures/kueue*`, `deploy/ci-pr-checks/install-kueue.sh` or the
workflow; on a PR labelled `ok-to-test-kueue` (add the label, then push); every
night; and on a manual dispatch with `run_kueue_tests`. Its budget is
`E2E_TIMEOUT=45m` (ginkgo), `E2E_GO_TIMEOUT=50m` and an 80-minute job.

To run the borrowing spec alone on Kind:

```bash
make test-e2e-kueue-borrow-with-setup REQUIRE_KUEUE_E2E=true
# or, on a cluster from make deploy-e2e-infra:
make install-kueue-e2e
make test-e2e-kueue-borrow REQUIRE_KUEUE_E2E=true
```

Check the summary says `Ran 5 of ... Specs` with no skips.

### Utilization-share specs (full tier)

All three share the label `utilization-share`. Run them alone with
`FOCUS="Utilization share" make test-e2e-full`. All three skip when no
schedulable node has a GPU product label; the last two also skip when no node
has enough GPUs of one product. On the Kind emulator all three must run, so
read the `-v` output.

- **`utilization_share_test.go`** runs the optimizer of
  [the proposal](../proposals/utilization-share-optimizer.md) against a real
  ReplicaSet, HPA and KEDA. About 7 minutes.
  - Setup: one namespace has a 4-GPU quota. Idle model A holds 3 GPUs; model
    B, under load, holds 1.
  - Shadow phase (the negative control): the `wva_utilization_share_*` series
    appear with B short, and no pod is marked.
  - Active phase: an A pod gets the donor mark, the ReplicaSet removes exactly
    that pod, B rises only once that pod starts terminating, and the transfer
    ends `done`.
- **`utilization_share_pd_test.go`**, a P/D model on LeaderWorkerSets.
  - Setup: a 7-GPU quota. Idle A has four 1-GPU replicas. B's decode LWS runs
    one group of two 1-GPU pods; its prefill LWS runs one pod.
  - B's decode grows by a group. Two A replicas fund it as one donor set, and
    decode rises only after both have released.
  - The test reads the set from the controller log (`transfer started`,
    `released, raising the receiver`, `transfer ended`). The transfer can
    finish in about 30 seconds, too fast to poll for the donor marks.
- **`utilization_share_nodes_test.go`**, node-aware donor sets.
  - Setup: a 6-GPU quota. Idle A has four 1-GPU replicas pinned to the
    product's largest node, so two share it. B has one 2-GPU replica.
  - `registry.k8s.io/pause` pods in another namespace take every other free
    GPU on the product's nodes. Without that fill, B's pods would fit free GPUs
    and the node-aware search would never run.
  - No single A pod frees enough for B, so the controller must log a set of two
    A pods with `"planned": true`, raise B only after both release, and B's
    second pod must run.

### Controller state between runs

`BeforeSuite` restarts the controller first. This is needed for determinism.

The saturation analyzer keeps learned capacity **in memory for the life of
the process**, keyed by `(namespace, model, variant)`. All suites use the same
model ID, so capacity learned by one suite, or by an earlier run, carries into
the next and wins over live observation (the `P2-hist` capacity source in the
`analyzer-result` log line). A bad learned capacity has pinned utilization at
`1.0`, so the engine correctly refused to scale down and a scale-down spec
failed against good code.

- Without the restart, results depend on run history. If you run specs in a
  way that skips `BeforeSuite`, expect that.
- Redeploying the **same image tag** does not restart the pod
  (`IfNotPresent` and an unchanged pod spec), so rebuilt code does not run.
  Bump the tag, and check the pod's age, not just its image.
- The restart is best-effort: if the suite may not patch the Deployment, it
  warns and continues.

### E2E shared fixtures

`test/e2e/fixtures` holds helpers that create, ensure (idempotent) and delete
the objects the suite uses: ScaledObjects, HPAs, model services, Services,
ServiceMonitors, InferenceObjective. The naming rules (`Create*` / `Ensure*` /
`Delete*`, `baseName` vs full names) are in the package doc:

```bash
go doc ./test/e2e/fixtures

# Compile the e2e package without running specs
go test ./test/e2e/... -run TestDoesNotExist
```

### Configuration

The main variables (full list in the [E2E README](../../test/e2e/)):

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `kind-emulator` | `kind-emulator`, `openshift`, or `kubernetes` |
| `USE_SIMULATOR` | `true` | Emulated GPUs (true) or real vLLM (false) |
| `SCALE_TO_ZERO_ENABLED` | `false` | Enable scale-to-zero tests |
| `SCALER_BACKEND` | `keda` | `keda` (ScaledObject) or `none` (use a pre-installed backend) |
| `POD_READY_TIMEOUT` / `SCALE_UP_TIMEOUT` | `300` / `600` | Model ready vs longest scale/job waits (seconds) |
| `E2E_EVENTUALLY_*`, `E2E_EVENTUALLY_POLL*` | see README | Optional `Eventually` timeouts and poll intervals |

Deploy-time knobs: `SKIP_HELM_REPO_UPDATE`, and `SCALE_UP_THRESHOLD` /
`SCALE_DOWN_BOUNDARY` (when set, the Makefile patches the
`wva-scaling-policy-config` ConfigMap).

**Parallel runs.** A controller given a `CONTROLLER_INSTANCE` name manages only
workloads whose ScaledObject carries `wva.llmd.ai/controller-instance` with
that name. It is an env var on the manager container, so it needs its own
overlay; see the one the multi-controller suite uses
(`WVA_E2E_SECONDARY_OVERLAY_PATH`). It is for **test isolation** only:
`install.sh` does not offer it, because two controllers on one cluster still
share one pool of GPUs (see `deploy/lib/single_install.sh`).

## Test Comparison Matrix

| Aspect | Unit | Integration | E2E (Kind emulated) | E2E (OpenShift) |
|--------|------|-------------|---------------------|-----------------|
| **Speed** | <1 min | 1-3 min | Smoke 5-10 / Full 15-25 min | Smoke 5-10 / Full 15-25 min |
| **Isolation** | Complete | Partial | Complete (Kind) | Shared cluster |
| **GPU required** | No | No | No (emulated) | Yes |
| **Infrastructure** | None | envtest | Kind + infra deploy | OpenShift + infra deploy |

## Continuous Integration

Two workflows gate a pull request. Each job maps to a make target; run it
before you push.

`ci-pr-checks.yaml` runs only on PRs whose base is `main` or `dev` (plus pushes
to `main` and manual dispatch). A PR stacked on another branch gets no build,
test, lint or e2e, so retarget it to `main` before review.

| Workflow / job | What it runs | Reproduce locally |
|----------------|--------------|-------------------|
| `ci-pr-checks.yaml` / `lint-and-test` | golangci-lint v2.8.0, `make build`, `make test` | `make lint build test` |
| `ci-pr-checks.yaml` / `deploy-scripts-check` | `make lint-deploy-scripts`, with yq pinned to v4.44.3 | `make lint-deploy-scripts`, with yq v4.44.3 on `PATH` |
| `ci-pr-checks.yaml` / `docs-check` | `make guides-check` (guide blocks vs `guide.yaml`, plus `hack/check-doc-links.py`), `make warmpool-check`, `make dashboards-check` | the same three targets (needs Python with PyYAML) |
| `ci-pr-checks.yaml` / `kustomize-build` | `kustomize build` of the cluster-scoped kubernetes, namespace-scoped kubernetes and namespace-scoped openshift overlays | `make kustomize`, then `bin/kustomize build config/overlays/<scope>/<env> > /dev/null` |
| `ci-pr-checks.yaml` / `e2e-tests-smoke` | `make test-e2e-smoke-with-setup` on a fresh Kind cluster; skipped when a PR changes only docs or metadata | `CREATE_CLUSTER=true make test-e2e-smoke-with-setup` |
| `ci-pr-checks.yaml` / `e2e-tests-full` | `make test-e2e-full-with-setup` with `SCALE_TO_ZERO_ENABLED=true`; returns early when no compiled file (Go, Makefile, `config/`, `test/`, workflows) changed | `CREATE_CLUSTER=true SCALE_TO_ZERO_ENABLED=true make test-e2e-full-with-setup` |
| `ci-signed-commits.yaml` / `signed-commits` | fails if any commit in the PR lacks a verified signature | `git log --show-signature origin/main..HEAD` |

On a push to `main`, only `lint-and-test`, `deploy-scripts-check`, `docs-check`
and `kustomize-build` run; the e2e jobs are PR-only.

`ci-e2e-openshift.yaml` (the GPU OpenShift run) is manual
(`workflow_dispatch` only); its PR and `/ok-to-test` triggers are disabled.

Every commit also needs a DCO sign-off (`git commit -s`); see
[CONTRIBUTING.md](../../CONTRIBUTING.md#commits).

## Ginkgo/Gomega Patterns

Test behavior, not implementation; keep tests independent; clean up in
`AfterEach`/`AfterAll`.

```go
// Name says what it checks
It("should recommend scale-up when KV cache exceeds 70% threshold", func() { /* ... */ })

// Eventually: wait for an async condition (a bare Get-then-Expect is racy)
Eventually(func(g Gomega) {
    dep := &appsv1.Deployment{}
    g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
    g.Expect(*dep.Spec.Replicas).To(BeNumerically(">=", 2))
}, timeout, interval).Should(Succeed())

// Consistently: a state holds for a period (here 30s)
Consistently(func(g Gomega) {
    dep := &appsv1.Deployment{}
    g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
    g.Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
}, 30*time.Second, 5*time.Second).Should(Succeed())

// Ordered: specs run in order and share state
var _ = Describe("Scale-up workflow", Ordered, func() {
    It("should create resources", func() { /* ... */ })
    It("should detect saturation", func() { /* ... */ })
    It("should scale up", func() { /* ... */ })
})
```

## Debugging Tests

### Unit tests

```bash
# Ginkgo trace
go test -v ./internal/engines/steadystate/ -ginkgo.v -ginkgo.trace

# Debugger (delve)
dlv test ./internal/controller/... -- -ginkgo.v
```

### E2E tests

Run through the make targets; they set the namespaces and the Prometheus
port-forward the suite needs:

```bash
FOCUS="Basic Autoscaling" make test-e2e-smoke
FOCUS="Utilization share" make test-e2e-full
```

A bare `go test ./test/e2e/` needs the same environment by hand:
`WVA_NAMESPACE=llm-d-sim` (and the other variables the target sets) plus
`kubectl port-forward -n <monitoring-ns> svc/kube-prometheus-stack-prometheus 9090:9090`.

Inspect the cluster:

```bash
kubectl get pods -A
kubectl logs -n "${CONTROLLER_NAMESPACE:-llm-d-sim}" deploy/wva-controller-manager
```

### Common failures

| Symptom | Likely causes | Check |
|---------|---------------|-------|
| Test times out | controller stuck, HPA not reading metrics, Prometheus not scraping, quota blocks pods | `kubectl get events -A --sort-by='.lastTimestamp'`; `kubectl describe scaledobject -n <ns>`; controller logs |
| No external metric | KEDA operator down, bad labels | `kubectl describe hpa -n <ns> keda-hpa-<scaledobject>` |
| HPA wants replicas, Deployment does not grow | CPU/memory/GPU short, node capacity, PDB | `kubectl describe deploy -n <ns>`; events; `kubectl top nodes` |

KEDA serves `external.metrics.k8s.io` per ScaledObject. `wva_desired_replicas`
is a Prometheus gauge, not an external metric: read it from Prometheus, or read
the HPA KEDA drives from it.

## Coverage and contributing tests

Targets: 70%+ unit coverage, integration tests for controller operations, e2e
for critical user workflows. Before pushing, run `make test` and
`make test-e2e-smoke` (or `make test-e2e-full`).

```bash
# Never ./...: that runs test/e2e on your kube context
go test -coverprofile=coverage.out $(go list ./... | grep -v -e /e2e -e /benchmark)
go tool cover -func=coverage.out
```

## Related Documentation

- [Development Guide](development.md) - development environment setup
- [E2E Test Suite README](../../test/e2e/) - every e2e option
- [Contributing Guide](../../CONTRIBUTING.md) - contribution guidelines
