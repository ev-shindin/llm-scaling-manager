# Testing Guide

Comprehensive guide for testing the Workload-Variant-Autoscaler (the scaling manager).

## Overview

The scaling manager has a multi-layered testing strategy:

1. **Unit Tests** - Fast, isolated tests for individual packages and functions
2. **Integration Tests** - Tests for component interactions within the controller
3. **E2E Tests** - Environment-agnostic end-to-end tests (Kind emulated or OpenShift), with smoke and full tiers

## Unit Tests

### Running Unit Tests

```bash
# Run all unit tests (also runs check-import-direction and check-doc-comment-owner)
make test

# Run specific package
go test ./internal/engines/analyzers/saturation/...

# Run with verbose output
go test -v ./internal/controller/...

# Coverage. Never `go test ./...`: it also runs test/e2e against your current
# kube context, deleting and restarting things on whatever cluster that is.
go test -coverprofile=coverage.out $(go list ./... | grep -v -e /e2e -e /benchmark)
go tool cover -html=coverage.out -o coverage.html
```

`make test` builds that same package list (it excludes `/e2e` and
`/benchmark`, then adds `./test/e2e/fixtures/`), sets `KUBEBUILDER_ASSETS`
for the envtest suites, and first runs `hack/check-import-direction.sh` and
`hack/check-doc-comment-owner.py`.

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

Check the spec count in the output. A focus that matches nothing runs 0 specs
and exits 0, and e2e specs `Skip()` at runtime on environment flags (for
example the scale-from-zero suites without `SCALE_TO_ZERO_ENABLED=true`). A
skip also reports as success, so always run with `-ginkgo.v` and read what ran.

### Shell checks in `deploy/` and `hack/`

The installer decides what to autoscale by parsing pod specs in shell. That
parsing has no Go test and never touches a cluster, so it is covered by
fixture-executing checks under `hack/`. One target runs them all, and CI runs
it on every PR (`deploy-scripts-check`):

```bash
make lint-deploy-scripts
```

It runs every `hack/check-*` script listed in the target (26 at the time of
writing: 23 shell, 3 Python; parsers, manifests, make recipes, quoted
`jq`/`yq` programs, ...) besides `bash -n` on the deploy scripts. The other
`hack/check-*` scripts (doc links, dashboards, import direction, doc-comment
owners) run from their own targets. They need `jq` and `yq` and fail rather than skip without them,
because a skip is how a green run comes to mean nothing. CI pins yq v4.44.3;
some yq behaviour differs in newer releases, so reproduce a CI failure with
that version.

- `check-install-defaults.sh` -- what a plain install ships: how `IMG` is
  split (registry ports, digests), the pull policy for a reference, the
  floating-tag and unverified-Prometheus-TLS warnings, and which Prometheus
  ServiceAccount is granted the controller's `/metrics`.

Both need `jq` (and the first needs `yq`) and fail rather than skip without
them, because a skip is how a green run comes to mean nothing.

`check-rendered-manifests.sh` needs kustomize as well, so it runs in the
`kustomize-build` CI job rather than under `make lint-deploy-scripts`. It
renders the overlays -- and the overlay the installer wraps around them -- and
asserts what they contain, where the builds alone only prove they render:

```bash
make kustomize && KUSTOMIZE=bin/kustomize bash hack/check-rendered-manifests.sh
```

### Unit Test Structure

Unit tests are co-located with the code they test:

```text
internal/
├── controller/
│   ├── variantautoscaling_controller.go
│   └── variantautoscaling_controller_test.go
├── saturation/
│   ├── analyzer.go
│   └── analyzer_test.go
├── collector/
│   ├── collector.go
│   └── collector_test.go
└── queueing/
    └── analyzer/
        ├── queueanalyzer.go
        └── queueanalyzer_test.go
```

A new fixture check follows the same convention (see AGENTS.md): a
`hack/check-*.sh` script, called from `lint-deploy-scripts`. A check behind
a new make target is one CI never runs.

### Writing Unit Tests

Example unit test structure:

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
            // Test implementation
            Expect(result).To(Equal(expected))
        })
    })
})
```

### Unit Test Best Practices

- **Use table-driven tests** for testing multiple scenarios
- **Mock external dependencies** (Kubernetes API, Prometheus, etc.)
- **Test edge cases** (zero values, negative numbers, nil pointers, etc.)
- **Keep tests fast** - unit tests should run in milliseconds
- **Use descriptive test names** - clearly state what is being tested
- **Follow AAA pattern** - Arrange, Act, Assert

## Integration Tests

Integration tests validate component interactions within the controller using envtest.

### Running Integration Tests

```bash
# Run integration tests (included in make test)
make test

# Run only controller integration tests
go test ./internal/controller/... -v
```

### envtest Setup

Integration tests use controller-runtime's envtest, a real Kubernetes API
server with no controllers. The project installs no CRD, so the suites load
none; see `internal/controller/suite_test.go` for the setup, and
[Run one test](#run-one-test) for running a suite by hand.

## End-to-End Tests

The scaling manager provides a **single consolidated E2E suite** that runs on multiple environments (Kind with emulated GPUs, or OpenShift/kubernetes with real infrastructure). Tests are environment-agnostic and parameterized via environment variables; they create ScaledObjects, HPAs and model services dynamically as part of the test workflow.

- **Location**: `test/e2e/`
- **Environments**: Kind (emulated), OpenShift, or generic Kubernetes
- **Tiers**: Smoke (~5–10 min) for PRs; full suite (~15–25 min) for comprehensive validation

### Scope

E2E is intended to be a **deterministic correctness signal**: resource wiring, reconciliation, and stable invariants (e.g., ScaledObjects become Ready, KEDA creates the HPA, scalers are created and point at the right targets/metrics). Traffic generation and performance/benchmarking scenarios should live outside `test/e2e/`.

### Controller state between runs

`BeforeSuite` restarts the scaling manager controller before anything else runs. This is required for determinism, not hygiene.

The saturation analyzer's capacity knowledge store is **in-memory and lives for the process lifetime**. It is keyed by `(namespace, model, variant)`, and the suites all reuse the same model ID, so a per-replica capacity learned by one suite — or by an earlier run against the same cluster — survives into the next and is preferred over live observation (the `P2-hist` capacity source in the `analyzer-result` log line).

Without the restart, results depend on run history rather than on the code. A degenerate learned capacity has been observed to pin utilization at `1.0`, at which point the engine *correctly* refuses to scale down and a scale-down spec fails against code that is fine — reproducibly on a reused pod, and not at all on a fresh one.

Two consequences worth knowing:

- Re-running the suite against an already-deployed controller is only trustworthy because of that restart. If you invoke specs some other way and skip `BeforeSuite`, expect history-dependent results.
- Redeploying with the **same image tag** does not restart the pod (`imagePullPolicy: IfNotPresent` plus an unchanged pod spec), so rebuilt code silently does not run. Bump the tag when redeploying — check the pod's name/age, not just the image string.

The restart is best-effort: if patching the Deployment is not permitted, the suite warns and continues rather than failing.

### E2E shared fixtures

Code lives under `test/e2e/fixtures`. The `fixtures` package holds reusable helpers to create, ensure (idempotent setup), and delete Kubernetes objects used by the e2e suite (KEDA ScaledObjects, HPAs, model services, Services, ServiceMonitors, InferenceObjective, etc.). Package-level documentation and naming conventions (`Create*` / `Ensure*` / `Delete*`, `baseName` vs full resource names) live in the package doc:

```bash
go doc ./test/e2e/fixtures
```

After changing fixture APIs or generated object shape, compile e2e without running specs:

```bash
go test ./test/e2e/... -run TestDoesNotExist
```

### Infra-Only Setup (Required Before Running Tests)

Tests expect **the scaling manager + monitoring + scaler + llm-d EPP/gateway** to be deployed; they create ScaledObjects, HPAs and model workloads themselves. Use **`make deploy-e2e-infra`** (runs `deploy/install.sh` then `deploy/install-epp.sh`) or invoke those scripts with the same environment variables the Makefile sets.

This deploys:
- The scaling manager controller (via Kustomize)
- llm-d EPP (GAIE standalone chart) via `deploy/install-epp.sh`
- Prometheus stack and KEDA
- **No** ScaledObject or HPA (tests create these)

When `ENABLE_SCALE_TO_ZERO=true` (set by `make deploy-e2e-infra` when `SCALE_TO_ZERO_ENABLED=true`), **`install-epp.sh`** enables the **flowControl feature gate** on the EPP so it exposes `inference_extension_flow_control_queue_size`. The **InferenceObjective** `e2e-default` is created by the scale-from-zero tests (`test/e2e/fixtures`), not by the install scripts.

**Kueue (full suite on kind).** Two specs read Kueue:

- `kueue_quota_test.go` (label `kueue-quota`) proves a quota entry bounded by
  Kueue against a real API server. It needs only the three Kueue **CRDs**
  (ClusterQueue, LocalQueue, ResourceFlavor): the reader lists objects and
  never talks to Kueue.
- `kueue_borrow_test.go` (label `kueue-borrow`) needs Kueue's **controller**.
  It runs a managed, loaded simulator model on one queue of a two-queue cohort
  (2 GPUs each) and checks borrowing as the scaling manager applies it:
  - a GPU pod in the namespace that Kueue does not manage withholds
    borrowing: the model holds at its nominal 2;
  - with that pod gone the model grows to 4, every pod placed, and no further;
  - a `Never` lender with a pending workload gets exactly one replica back,
    and its workload is admitted;
  - the lender, switched to `Any`, asks for more: Kueue preempts a borrowed
    pod, the scaling manager lowers the target so no gated replacement
    lingers, and the model borrows nothing until `borrowCooldown` ends.

  The policy sets short timings on the Kueue block (`placementTimeout: 30s`,
  `borrowCooldown: 4m`, `lenderDebounce: 10s`) and declares the physical
  limiter, which borrowing needs as its GPU bound. The spec needs 6 free GPUs
  of the suite's GPU product and prints `MEASURE` lines: replica start, the
  release cycle and the reclaim cycle.

`make test-e2e-full-with-setup` installs Kueue on kind through
`make install-kueue-e2e`. By default (`KUEUE_CONTROLLER=true`) that runs
`deploy/ci-pr-checks/install-kueue.sh`, which:

- downloads the pinned release manifest (`KUEUE_VERSION`, default `v0.20.0`,
  image from `registry.k8s.io`), checks its sha256, then applies it. Another
  version needs `KUEUE_MANIFEST_SHA256` as well;
- waits for the rollout, then for the webhook to answer;
- narrows Kueue's job webhooks to namespaces labelled `llm-d.ai/kueue-e2e`
  (`KUEUE_WEBHOOK_NAMESPACE_LABEL`). Upstream they cover every namespace with
  `failurePolicy: Fail`, so a restarting Kueue would fail pod creation for the
  whole suite. Only the webhooks are narrowed, so a queue-labelled pod in an
  unlabelled namespace runs unmanaged: fine on a throwaway kind cluster, never
  on one whose quotas matter.

With `KUEUE_CONTROLLER=false` it runs `install-kueue-crds.sh` instead, which
installs the CRDs alone. The quota spec passes either way; the borrowing spec
then fails.

On kind both specs **fail rather than skip** when what they need is missing, so
a missing setup step is never read as coverage. On any other cluster (the
OpenShift CI included) nothing installs Kueue: each spec runs when the cluster
has what it needs and skips, saying so in the `-v` output, when it does not.
Both scripts refuse to change a Kueue somebody else installed.

To run the borrowing spec alone on a kind cluster from `make deploy-e2e-infra`:

```bash
make install-kueue-e2e
make test-e2e-kueue-borrow
```

Check the summary says `Ran 4 of ... Specs` with no skips.

**Utilization-share optimizer (full suite).** `utilization_share_test.go`
(label `utilization-share`) runs the optimizer of
[the proposal](../proposals/utilization-share-optimizer.md) against a real
ReplicaSet, HPA and KEDA. It needs nothing beyond the standard infra.
- **Setup:** one namespace has a 4-GPU quota on the discovered accelerator. An
  idle model A holds 3 GPUs, and model B, under load, holds 1.
- **Shadow phase, the negative control:** the group must be evaluated (the
  `wva_utilization_share_*` series appear, with B short), and no pod is marked.
- **Active phase:**
  - an A pod gets the donor mark;
  - the ReplicaSet removes exactly that pod;
  - B is raised only once the marked pod starts terminating;
  - the transfer ends `done`.
- **Duration:** about 7 minutes. Run it alone with
  `FOCUS="Utilization share" make test-e2e-full`.

Two more specs share the label and the `FOCUS`. All three skip when no
schedulable node carries a GPU product label, and the two below also skip when
no node has enough GPUs of one product. A skip reports as success, so read the
`-v` output: on the kind emulator all three must run.
- **`utilization_share_pd_test.go`, a P/D model on LeaderWorkerSets.**
  - Setup: a 7-GPU quota. Idle model A has four 1-GPU replicas. Model B's
    decode LWS runs one group of two 1-GPU pods, and its prefill LWS one pod.
  - B's decode grows by a group. Two of A's replicas fund it as one donor set,
    and the decode LWS is raised only after both have released.
  - The set is read from the controller's log (`transfer started`, `released,
    raising the receiver`, `transfer ended`), because it can finish within
    about 30 seconds, faster than a poll reliably catches the donor marks.
- **`utilization_share_nodes_test.go`, node-aware donor sets.**
  - Setup: a 6-GPU quota. Idle model A has four 1-GPU replicas, pinned to the
    product's largest node so that two of them share it, and model B one
    2-GPU replica.
  - Every other free GPU on the product's nodes is taken by a
    `registry.k8s.io/pause` pod in a separate namespace, outside WVA.
  - No single A pod holds B's pod, so only the node-aware search can fund it:
    the controller must log a set of two A pods with `"planned": true`, raise
    B only after both have released, and B's second pod must run.
  - The fill is what makes the node search matter. On an emulator with spare
    GPUs, the receiver's pods would fit free GPUs and the search would defer to
    the node-blind one.

**Install script tuning (optional, same variables as `deploy/install.sh`):**

- **`SKIP_HELM_REPO_UPDATE`**: When set to **`true`**, `helm repo update` is skipped during installs (faster, less network churn). Default runs `helm repo update` to refresh repo indexes.

Alternatively, use the Makefile to deploy infra and run tests in one go:

```bash
# Kind: create cluster, deploy infra, run smoke tests
make test-e2e-smoke-with-setup

# Kind: deploy infra only (if cluster already exists), then run full suite
make deploy-e2e-infra
make test-e2e-full
```

See the [E2E Test Suite README](../../test/e2e/) for full configuration options and examples.

### Quick Start

```bash
# Smoke tests (recommended for every PR)
make test-e2e-smoke

# Full suite (on-demand)
make test-e2e-full

# OpenShift: point at cluster and run
export KUBECONFIG=/path/to/openshift/kubeconfig
export ENVIRONMENT=openshift
make test-e2e-smoke
# or make test-e2e-full

# Run a specific test by name (a Describe or It text, e.g. in smoke_keda_test.go)
FOCUS="Basic Autoscaling" make test-e2e-smoke
```

`test-e2e-smoke` filters on label `smoke` and `test-e2e-full` on
`full && !smoke && !flaky`, so a `FOCUS` must name a spec in the tier you run. Either
way, check `Ran N of M Specs` in the output: a focus that matches nothing runs
0 specs and exits green.

### What the Suite Validates

- **Smoke (label `smoke`)**: Infrastructure readiness, basic autoscaling through the KEDA external scaler, error handling
- **Full (label `full`, run as `full && !smoke && !flaky`)**: Deterministic correctness checks (scale-from-zero, limiter, pod scraping, etc.). The tiers are disjoint: `test-e2e-full` does not re-run the smoke specs, and specs labelled `flaky` run in neither. Run both targets for complete coverage.

### Configuration

Key environment variables (see [E2E Test Suite README](../../test/e2e/) for the full list):

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `kind-emulator` | `kind-emulator`, `openshift`, or `kubernetes` |
| `USE_SIMULATOR` | `true` | Emulated GPUs (true) or real vLLM (false) |
| `SCALE_TO_ZERO_ENABLED` | `false` | Enable scale-to-zero tests (Kind supports both enabled and disabled) |
| `SCALER_BACKEND` | `keda` | `keda` (ScaledObject) or `none` (skip install, use pre-installed backend) |
| `POD_READY_TIMEOUT` / `SCALE_UP_TIMEOUT` | `300` / `600` | Model ready vs longest scale/job waits (seconds) |
| `E2E_EVENTUALLY_STANDARD`, etc. | see README | Optional `Eventually` timeouts and poll intervals (`E2E_EVENTUALLY_*`, `E2E_EVENTUALLY_POLL*`) |

Deploy-time knobs: `SKIP_HELM_REPO_UPDATE`, optional `SCALE_UP_THRESHOLD` / `SCALE_DOWN_BOUNDARY` (Makefile patches the `wva-scaling-policy-config` ConfigMap when set) — see **Install script tuning** above.

For running multiple test runs in parallel, a controller can be given a
`CONTROLLER_INSTANCE` name: it then manages only workloads whose ScaledObject
carries `wva.llmd.ai/controller-instance` with that name. It is set by an env var on
the manager container, so it needs its own overlay — see the secondary overlay the
multi-controller suite uses (`WVA_E2E_SECONDARY_OVERLAY_PATH`). It is a **test
isolation** mechanism, not an install option: `install.sh` does not offer it, because
two controllers on one cluster still allocate from one pool of GPUs (see
`deploy/lib/single_install.sh`).

## Test Comparison Matrix

| Aspect | Unit Tests | Integration Tests | E2E Consolidated (Kind emulated) | E2E Consolidated (OpenShift) |
|--------|-----------|-------------------|----------------------------------|------------------------------|
| **Speed** | Fast (<1min) | Fast (1-3min) | Smoke 5-10min / Full 15-25min | Smoke 5-10min / Full 15-25min |
| **Isolation** | Complete | Partial | Complete (Kind) | Shared cluster |
| **GPU Required** | No | No | No (emulated) | Yes (real) |
| **Infrastructure** | None | envtest | Kind + infra-only deploy | OpenShift + infra-only deploy |
| **Realism** | Low | Medium | High (emulated) | Production-like |
| **CI-Friendly** | Yes | Yes | Yes | Requires cluster |
| **Local Dev** | Yes | Yes | Yes | Cluster access needed |

## Continuous Integration

Two workflows gate a pull request. Every job maps to a make target you can run
locally; run the same target before you push.

`ci-pr-checks.yaml` runs only on pull requests whose base is `main` or `dev`
(plus pushes to `main` and manual dispatch). A PR stacked on another branch gets
no build, test, lint or e2e at all, so retarget it to `main` before review.

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

`ci-e2e-openshift.yaml` (the GPU OpenShift run) is currently manual
(`workflow_dispatch` only); its PR and `/ok-to-test` triggers are disabled.

Every commit also needs a DCO sign-off (`git commit -s`); see
[CONTRIBUTING.md](../../CONTRIBUTING.md#commits).

## Testing Best Practices

### General Guidelines

1. **Write tests first** (TDD approach) - helps design better APIs
2. **Test behavior, not implementation** - tests should survive refactoring
3. **Keep tests independent** - tests should not depend on each other
4. **Use meaningful assertions** - prefer specific matchers over generic equality
5. **Clean up resources** - always clean up in AfterEach/AfterAll blocks
6. **Document complex tests** - add comments explaining non-obvious test logic

### Ginkgo/Gomega Patterns

#### Use Descriptive Test Names

```go
// ✅ Good
It("should recommend scale-up when KV cache exceeds 70% threshold", func() {
    // ...
})

// ❌ Bad
It("should work", func() {
    // ...
})
```

#### Use Eventually for Async Operations

```go
// ✅ Good - waits for condition to become true
Eventually(func(g Gomega) {
    dep := &appsv1.Deployment{}
    err := k8sClient.Get(ctx, key, dep)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(*dep.Spec.Replicas).To(BeNumerically(">=", 2))
}, timeout, interval).Should(Succeed())

// ❌ Bad - may fail due to timing
dep := &appsv1.Deployment{}
k8sClient.Get(ctx, key, dep)
Expect(*dep.Spec.Replicas).To(BeNumerically(">=", 2))
```

#### Use Consistently for Stable State

```go
// Verify replicas remain stable for 30 seconds
Consistently(func(g Gomega) {
    deploy := &appsv1.Deployment{}
    err := k8sClient.Get(ctx, key, deploy)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
}, 30*time.Second, 5*time.Second).Should(Succeed())
```

#### Use Ordered for Sequential Tests

```go
var _ = Describe("Scale-up workflow", Ordered, func() {
    // These tests run in order and share state
    It("should create resources", func() { /* ... */ })
    It("should detect saturation", func() { /* ... */ })
    It("should scale up", func() { /* ... */ })
})
```

### Test Organization

#### Use Contexts for Grouping

```go
var _ = Describe("Optimizer", func() {
    Context("with single variant", func() {
        It("should optimize for cost", func() { /* ... */ })
        It("should meet SLO requirements", func() { /* ... */ })
    })

    Context("with multiple variants", func() {
        It("should prefer cheaper variant", func() { /* ... */ })
        It("should distribute load evenly", func() { /* ... */ })
    })
})
```

#### Use BeforeEach/AfterEach for Setup/Teardown

```go
var _ = Describe("Controller", func() {
    var (
        namespace string
        cleanup   func()
    )

    BeforeEach(func() {
        namespace = "test-" + randomString()
        // Setup test resources
    })

    AfterEach(func() {
        // Clean up test resources
        if cleanup != nil {
            cleanup()
        }
    })

    It("should reconcile resources", func() {
        // Test implementation
    })
})
```

## Debugging Tests

### Debugging Unit Tests

```bash
# Run with verbose output
go test -v ./internal/engines/allocation/...

# Enable Ginkgo trace
go test -v ./internal/engines/steadystate/ -ginkgo.v -ginkgo.trace

# Run with debugger (delve)
dlv test ./internal/controller/... -- -ginkgo.v
```

### Debugging E2E Tests

#### View Test Logs

```bash
# Consolidated E2E suite (smoke or full)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter="smoke"
go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter="full && !smoke && !flaky" -timeout 35m
```

#### Access Test Cluster

```bash
# For Kind E2E tests (default cluster name: kind-wva-gpu-cluster or from CLUSTER_NAME)
export KUBECONFIG=~/.kube/config   # or path from kind get kubeconfig
kubectl get pods -A
kubectl logs -n llm-d-sim deploy/wva-controller-manager

# For OpenShift E2E tests (same CONTROLLER_NAMESPACE, default llm-d-sim)
oc get pods -A
oc logs -n "${CONTROLLER_NAMESPACE:-llm-d-sim}" deploy/wva-controller-manager
```

`deploy-e2e-infra` installs the controller in `CONTROLLER_NAMESPACE`, which
defaults to `llm-d-sim` on every environment, not in
`workload-variant-autoscaler-system`.

#### Keep Cluster Alive After Failure

```bash
# Run tests; on failure, cluster is kept by default (DELETE_CLUSTER=false)
make test-e2e-smoke-with-setup
# Inspect: kubectl get all -A
# To delete cluster after: DELETE_CLUSTER=true make test-e2e-smoke-with-setup
# Or manually: kind delete cluster --name <CLUSTER_NAME>
```

### Common Test Failures

#### Test Times Out

**Symptoms**: Test hangs or exceeds timeout

**Possible causes**:
- Controller stuck in reconciliation loop
- HPA not reading metrics
- Prometheus not scraping metrics
- Resource quotas preventing pod creation

**Debugging steps**:
```bash
kubectl get events -A --sort-by='.lastTimestamp'
kubectl describe scaledobject -n <namespace>
kubectl logs -n llm-d-sim deploy/wva-controller-manager   # Kind e2e; else the controller's namespace
```

#### Metrics Not Available

**Symptoms**: External metrics API returns empty or error

**Possible causes**:
- KEDA operator not running
- Metrics not being scraped
- Incorrect metric labels or selectors

**Debugging steps**:
```bash
# Check external metrics API
# KEDA owns external.metrics.k8s.io and serves per-ScaledObject metrics there;
# wva_desired_replicas is a Prometheus gauge, not an external metric. Read it via
# Prometheus, or read the HPA KEDA drives from it.
kubectl describe hpa -n <namespace> keda-hpa-<scaledobject-name>

# Check Prometheus
kubectl port-forward -n workload-variant-autoscaler-monitoring svc/prometheus-operated 9090:9090
# Query: wva_desired_replicas{variant_name="..."}
```

#### Deployment Not Scaling

**Symptoms**: HPA shows desired replicas but deployment doesn't scale

**Possible causes**:
- Resource constraints (CPU/memory/GPU)
- Node capacity exceeded
- PDB preventing scale-up
- Deployment controller issues

**Debugging steps**:
```bash
kubectl describe hpa -n <namespace>
kubectl describe deploy -n <namespace>
kubectl get events -n <namespace> --sort-by='.lastTimestamp'
kubectl top nodes
```

## Performance / Benchmarking

Performance and benchmarking scenarios (traffic generation, throughput/latency measurement, scale-up latency, etc.) are intentionally **out of scope** for `test/e2e/` so that e2e remains deterministic. Use the project’s dedicated benchmarking tooling/workflows instead.

## Test Coverage Goals

Current coverage targets:
- **Unit tests**: 70%+ code coverage
- **Integration tests**: All controller operations
- **E2E tests**: Critical user workflows

### Checking Coverage

```bash
# Generate coverage report (never ./...: that runs test/e2e on your kube context)
go test -coverprofile=coverage.out $(go list ./... | grep -v -e /e2e -e /benchmark)

# View summary
go tool cover -func=coverage.out

# Generate HTML report
go tool cover -html=coverage.out -o coverage.html

# View in browser
open coverage.html  # macOS
xdg-open coverage.html  # Linux
```

## Contributing Tests

When contributing, please ensure:

1. ✅ **All new code has unit tests** - aim for 70%+ coverage
2. ✅ **Critical paths have integration tests** - especially controller logic
3. ✅ **New features have E2E tests** - validate end-to-end behavior
4. ✅ **Tests are documented** - explain what is being tested and why
5. ✅ **Tests follow naming conventions** - use descriptive names
6. ✅ **Tests clean up resources** - no resource leaks in tests
7. ✅ **Tests pass locally before pushing** - run `make test` and `make test-e2e-smoke` (or `make test-e2e-full`)

## Related Documentation

- [Development Guide](development.md) - Development environment setup
- [E2E Test Suite README](../../test/e2e/) - Consolidated E2E tests (Kind, OpenShift, infra-only setup)
- [Contributing Guide](../../CONTRIBUTING.md) - Contribution guidelines
