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
- `check-controller-ready.sh` runs the controller readiness check against a
  stub `kubectl`. A Deployment at 0 replicas passes `rollout status`, so the
  check must also find a Ready replica.

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

A test that runs a real server on loopback must not take its port from the
kernel's ephemeral range: do not bind `127.0.0.1:0`, close it, and hand the
number to the server. `make test` runs envtest suites in parallel, and their
etcd and kube-apiserver traffic fills that range with `TIME_WAIT` sockets,
most of all under WSL (a 4096-port range). The few free ports are then taken
by the next outgoing connection before the server binds, and the server fails
with `address already in use`. Pick a port below the range and retry on
`EADDRINUSE`, as `startServer` in `internal/scaler/server_test.go` does, and
fail the spec with the server's own error rather than dialling until a
timeout.

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

## Learner Recordings

The learner recorder writes every input the analyzers read, and every result
they return, to a file of JSON lines, so a run can be replayed through the
analyzers later (`internal/engines/analyzers/learnreplay`). The design is in
[the learner proposal](../proposals/analyzer-learner.md), sections 2.4 and 2.5.
It is a test aid, off by default.

### Turn it on

Add the `learner-recorder` component to the overlay you deploy:

```yaml
# kustomization.yaml of an overlay beside config/overlays/cluster-scoped/kubernetes
components:
- ../../../components/learner-recorder
```

It mounts a 600Mi `emptyDir` at `/var/lib/wva/learner-recordings`, sets
`LEARNER_RECORDING_DIR` to it, and adds a small `recordings` sidecar that
mounts the same volume read-only: the manager image has no `tar`, which
`kubectl cp` needs. Without the component, set the directory with
`--learner-recording-dir` (or `LEARNER_RECORDING_DIR`) and mount a volume there
yourself. The controller refuses `/tmp`: it is a 64Mi `emptyDir`, and the
kubelet evicts the pod when it fills.

What it writes:

- One file per controller start, named by its start time:
  `learnrec-20260302T090000.000000000Z.jsonl`. The first line is the start
  (how the analyzers were configured); a replay refuses a file without it.
- At most 256Mi per recording (`--learner-recording-cap-bytes`). At the cap
  the recording keeps what it has, stops writing, and logs how many events it
  dropped.
- At most two recordings at the cap in the directory. A new recording deletes
  the oldest ones first to make room.
- About 1-1.5 KB per replica per cycle: at a 15 s cycle, 8 replicas take about
  five days to reach the cap and 100 replicas about eight hours.

### Copy the recordings out

The `emptyDir` goes with the pod. Copy the files out before you tear the
deployment down:

```bash
NS=wva-system   # the controller's namespace
POD=$(kubectl -n "$NS" get pod -l control-plane=controller-manager -o name | head -1)
kubectl -n "$NS" exec "${POD#pod/}" -c recordings -- ls -l /var/lib/wva/learner-recordings
kubectl -n "$NS" cp -c recordings "${POD#pod/}:/var/lib/wva/learner-recordings" ./learner-recordings
```

A container restart keeps the `emptyDir`, so the recording from before the
restart is still there beside the new one.

### Replay one

`learnreplay.Replay` reads a recording and runs it through new analyzers and a
new capacity store. In `Fidelity` mode, at the commit that recorded it, the
replay must reproduce the recording byte for byte. The acceptance test for
that is `TestLearnerRecording_AReplayAtTheSameCommitReproducesEveryResult`:

```bash
go test ./internal/engines/steadystate/ -run TestLearnerRecording -v
go test ./internal/engines/analyzers/learnreplay/ ./internal/signals/learnrec/ -v
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
  - the only lender is on `Never`: nothing it lends is counted, so the model
    holds at its nominal 2, counted at 0 as
    `lender-cannot-reclaim`;
  - a GPU pod in the namespace that Kueue does not manage withholds
    borrowing; the lender is then patched to `Any`, and the model still holds
    at its nominal 2;
  - chaos, the lender still on `Any`: the Kueue controller is scaled to 0, and
    once its leader Lease reads expired that pod goes; the model must not be
    ordered past its nominal 2 meanwhile. Kueue is restored on every exit
    path;
  - with Kueue back, the model grows to 4, every pod placed, and no further;
  - the lender asks for its nominal: Kueue preempts the borrowed pods, the
    scaling manager lowers the target so no gated replacement lingers, and
    the model borrows nothing until `borrowCooldown` ends;
  - the lender goes back to `Never` and asks again: nothing is handed back
    while `releaseToNonReclaimingLenders` is unset; once it is set, the model
    drops to its nominal 2 (cause `lender-waiting`) and Kueue admits the
    lender.

  The policy sets short Kueue timings (`placementTimeout: 30s`,
  `borrowCooldown: 4m`, `lenderDebounce: 10s`) and declares `gpu-inventory`,
  which borrowing needs as its GPU bound. It needs 6 free GPUs of the suite's
  GPU product and prints `MEASURE` lines for replica start and the reclaim
  cycle. Each of the first three steps holds the model at nominal by one
  condition alone, so each fails if that condition's handling is removed.
  `make test-e2e-kueue-borrow` passes `-ginkgo.fail-on-empty`: a label filter
  that matches no spec fails instead of passing with 0 specs run.

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

### Whole-node utilization-share specs (own cluster shape)

Seven specs, label `utilization-share-wholenode`, put a large model whose
replica is a whole 8-GPU node (or a LeaderWorkerSet group of two) beside
1-GPU models. They need nodes of exactly 8 GPUs of one product, which the
default cluster (one product per 16-GPU node) does not have. They are not in
the full tier and run on their own cluster:

```bash
CREATE_CLUSTER=true CLUSTER_NAME=ushare-large make test-e2e-ushare-wholenode-with-setup
# or, on a cluster that already has that shape:
make test-e2e-ushare-wholenode            # FOCUS=... narrows it
```

- The cluster is `CLUSTER_GPU_TYPE=nvidia CLUSTER_GPUS=8 CLUSTER_NODES=6`
  (`USHARE_WHOLENODE_CLUSTER_*`). Kind leaves the control-plane schedulable
  here, so all six nodes take pods. The setup also installs LeaderWorkerSet and
  Kueue's controller.
- `REQUIRE_WHOLENODE_E2E=true` (the target's default) turns every skip -- too
  few 8-GPU nodes, no Prometheus, no Kueue -- into a failure. The target passes
  `-ginkgo.fail-on-empty`. Check `Ran N of M Specs`.
- About 70 minutes on a 22-CPU workstation; the budget is `USHARE_WHOLENODE_TIMEOUT=200m`. No CI job runs
  them yet.
- Each spec closes every GPU it does not use with `registry.k8s.io/pause` pods
  (`fixtures.FillNodeGPUs`) and creates its models in an order that decides
  where each pod lands. Do not run them on a shared cluster.
- `fixtures.WithStartDelay` holds a replica not Ready for a while after it is
  scheduled (a readiness probe's initial delay), to emulate a large model's
  load. `fixtures.QueueGPUPod` makes a pod outside WVA wait for GPUs on one
  node.

The specs:

- **Lending a node** (`utilization_share_wholenode_test.go`). G has two 8-GPU
  replicas; S1 is loaded. G gives a whole node and S1 grows onto it. Then G is
  loaded and gets a node back.
- **Funded by small models.** G (one replica, 3-minute start) beside eight
  1-GPU pods of S1 and S2 on one node. G grows by a donor set that empties
  that node. Also checks G's start estimate.
- **A fragmented cluster** (`utilization_share_wholenode_fragment_test.go`).
  The small pods share their nodes with pause pods, so no node can be
  emptied. G must get no transfer and report `no-node-fits` in every reading
  once it first does, while the small models return their spare. Then,
  with the optimizer off and room in the quota, today's optimizer orders a G
  replica that stays Pending.
- **A released node taken** (issue #234). Same layout as "funded", plus a
  pause pod waiting for one GPU on the small models' node, and G at a
  `PriorityClass` the spec creates. The pause pod takes the first GPU freed;
  G's raised pod preempts it, the transfer ends `done`, and G runs two
  replicas. Without the class it ends `fill-timeout` with `release-taken`.
  "Funded" checks the controller's warning naming G, which has no class;
  this spec checks it does not name G.
- **A two-node LeaderWorkerSet** (`utilization_share_wholenode_lws_test.go`).
  G has two groups of two 8-GPU pods. It lends its highest group to S1, then
  gets a group back.
- **A namespace-scoped controller** (`utilization_share_wholenode_scope_test.go`).
  The "funded" layout, with the controller's `WATCH_NAMESPACE` set for this
  spec only. Without node data the small pods cannot fund G:
  `no-compatible-donor`, no transfer. The spec restores cluster scope after.
- **Under Kueue.** G and S on one ClusterQueue (nominal 9), a second queue
  lends 8. With the optimizer G stays at one replica and nothing is borrowed;
  without it, today's optimizer borrows 8 GPUs for a second G replica. It also
  logs whether Kueue's `waitForPodsReady` is on.

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
- After a restart, wait before creating a ScaledObject. `restartWVAController`
  returns only once the scaler Service routes to the new pod alone. A
  ScaledObject created while KEDA cannot reach the scaler gets a CPU-only HPA
  that KEDA never fixes. Specs that create one after a restart call
  `waitForKEDAExternalMetricWired`, which recreates it once if that happened.

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

**Helpers that reuse the controller's labels need their own NetworkPolicy
allow.** The e2e installs the controller in the model namespace, and the
shipped `wva-external-scaler` NetworkPolicy selects every Pod there that
carries the controller's labels. It admits only `:8443`, `:8081` and `:9090`.
A test Pod that borrows those labels (the warm-pool relays made with
`fixtures.ControllerDriverLabels`) is cut off on any other port. On kind it
only shows on a worker node, because traffic from the control-plane node
itself is not filtered, so the failure is intermittent: `dial tcp ...: i/o
timeout`. `fixtures.CreateHTTPDriver` adds a policy that admits the relay port
on that one Pod. Do the same for any new helper; do not widen the shipped
policy.

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
| `E2E_EXPECT_SCALER_TLS` | unset | `true` fails the run when the controller serves the external scaler in plaintext. Without it, a plaintext install just skips the scaler TLS spec, which reads as a pass. CI sets it |

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
| `ci-pr-checks.yaml` / `lint-and-test` | golangci-lint v2.9.0, `make build`, `make test` | `make lint build test` |
| `ci-pr-checks.yaml` / `deploy-scripts-check` | `make lint-deploy-scripts`, with yq pinned to v4.44.3 | `make lint-deploy-scripts`, with yq v4.44.3 on `PATH` |
| `ci-pr-checks.yaml` / `docs-check` | `make guides-check` (guide blocks vs `guide.yaml`, plus `hack/check-doc-links.py`), `make warmpool-check`, `make dashboards-check` | the same three targets (needs Python with PyYAML) |
| `ci-pr-checks.yaml` / `kustomize-build` | `kustomize build` of the cluster-scoped kubernetes, namespace-scoped kubernetes and namespace-scoped openshift overlays | `make kustomize`, then `bin/kustomize build config/overlays/<scope>/<env> > /dev/null` |
| `ci-pr-checks.yaml` / `e2e-tests-smoke` | `make test-e2e-smoke-with-setup` on a fresh Kind cluster, then the release's blocking Trivy scan (fixable CRITICAL) of the image it built; skipped when a PR changes only docs or metadata | `CREATE_CLUSTER=true make test-e2e-smoke-with-setup` |
| `ci-pr-checks.yaml` / `e2e-tests-full` | `make test-e2e-full-with-setup` with `SCALE_TO_ZERO_ENABLED=true`; returns early when no compiled file (Go, Makefile, `config/`, `test/`, workflows) changed | `CREATE_CLUSTER=true SCALE_TO_ZERO_ENABLED=true make test-e2e-full-with-setup` |
| `ci-signed-commits.yaml` / `signed-commits` | fails if any commit in the PR lacks a verified signature | `git log --show-signature origin/main..HEAD` |

On a push to `main`, only `lint-and-test`, `deploy-scripts-check`, `docs-check`
and `kustomize-build` run; the e2e jobs are PR-only.

`ci-e2e-openshift.yaml` (the GPU OpenShift run) is manual
(`workflow_dispatch` only); its PR and `/ok-to-test` triggers are disabled.
When they are enabled, a fork PR runs only on `/ok-to-test <sha>` or
`/retest <sha>`, where `<sha>` is the full 40-character SHA of the head commit
you reviewed. The run is refused if the head has moved since; review the new
commits and approve the new SHA.

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
