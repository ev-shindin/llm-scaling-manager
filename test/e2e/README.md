# E2E test suite

End-to-end tests for llm-scaling-manager. They install nothing themselves: they
run against a cluster that already has the controller, Prometheus, KEDA and an
llm-d EPP, and each spec creates and deletes its own model servers and
ScaledObjects.

```bash
CREATE_CLUSTER=true make deploy-e2e-infra   # kind cluster with emulated GPUs + infra
make test-e2e-smoke
```

The suite checks deterministic behaviour — wiring, decisions, invariants. Load
tests, latency and throughput belong in the [benchmarks](../../docs/guides/benchmarking/),
not here. The [testing guide](../../docs/developer-guide/testing.md) covers the
whole test setup, including unit and envtest suites.

## Read the spec counts, not the exit code

**A skipped spec reports as success.** Many specs call `Skip()` at run time when
an environment flag or a cluster capability is missing, and a run in which they
all skip still exits 0. The make targets already pass `-v -ginkgo.v`; compare the
summary (`Ran N of M Specs`, then the `Skipped` count) against a known-good run,
and read each `[SKIPPED]` reason.

| Skips unless | Specs |
| --- | --- |
| `SCALE_TO_ZERO_ENABLED=true` at **both** `deploy-e2e-infra` and the test run (default `false`; it turns on EPP flow control) | every scale-from-zero and scale-to-zero spec, and the scaling-blocked-reason spec |
| `USE_SIMULATOR=true` (the default) | KEDA external scaler, saturation, scaling policy tier, conceded supply, throughput analyzer, warm-pool attribution and borrow, warm-pool retained, SGLang |
| `USE_SIMULATOR=false` on a GPU cluster | warm-pool drain (label `gpu`, in neither tier) |
| LeaderWorkerSet is served (`DEPLOY_LWS=true`) | LeaderWorkerSet scale-from-zero, warm-pool group |
| Kueue CRDs are served | Kueue quota limiter |
| A Kueue controller is running (`KUEUE_CONTROLLER=true`, the default on kind); `REQUIRE_KUEUE_E2E=true` makes its absence a failure | Kueue cohort borrowing |
| InferenceObjective CRD is served | scale-from-zero and park-and-wake specs |
| Grafana is deployed | operational dashboard |
| Prometheus is reachable at `https://localhost:9090` | metric assertions in many specs; the make targets start this port-forward themselves |
| `ENVIRONMENT=kind-emulator` | multi-controller |
| A node advertises a GPU product label (enough GPUs on one node, for some) | utilization share |

`ENVIRONMENT=openshift` also forces `SCALE_TO_ZERO_ENABLED` off.

## Running

| Target | Ginkgo label filter | CI |
| --- | --- | --- |
| `make test-e2e-smoke` | `smoke` | every PR into `main` or `dev` that changes something besides `docs/**`, `README.md`, `CONTRIBUTING.md`, `LICENSE`, `OWNERS` or `PROJECT`, with `SCALE_TO_ZERO_ENABLED=false` |
| `make test-e2e-full` | `full && !smoke && !flaky && !kueue-borrow` | PRs that touch code, config or tests, with `SCALE_TO_ZERO_ENABLED=true` and `DEPLOY_LWS=true` |
| `make test-e2e-multi-controller` | `multi-controller` specs | — |
| `make test-e2e-kueue-borrow` / `test-e2e-kueue-borrow-with-setup` | `kueue-borrow` (the first needs `make install-kueue-e2e`) | job `e2e-tests-kueue-borrow`: PRs touching borrowing, label `ok-to-test-kueue`, nightly; `REQUIRE_KUEUE_E2E=true` |
| `make test-e2e-smoke-with-setup` / `test-e2e-full-with-setup` | deploy the infra first, then run the tier | what CI runs |

Smoke and full are **disjoint**: the full target excludes `smoke`, so run both
for complete coverage.

CI runs these only on pull requests whose base is `main` or `dev`
(`ci-pr-checks.yaml`). A PR stacked on another branch gets no e2e, build, test or
lint run at all.

```bash
# one Describe or It by its text
FOCUS="KEDA Smoke Tests - Basic Autoscaling" make test-e2e-smoke
# leave some out
SKIP="Warm pool" make test-e2e-full
```

`FOCUS` cannot un-skip a spec that skips at run time; set the flag it needs.

To reproduce the CI full run on kind:

```bash
SCALE_TO_ZERO_ENABLED=true CREATE_CLUSTER=true make test-e2e-full-with-setup
```

## Where things are

`make deploy-e2e-infra` installs the controller **cluster-scoped into
`llm-d-sim`**, the same namespace as the model servers, and the make targets
pass `WVA_NAMESPACE=llm-d-sim` to the tests to match. Running `go test` directly
defaults `WVA_NAMESPACE` to `workload-variant-autoscaler-system`; set it.

Never run `go test ./...` from the repository root: it includes this suite and
runs it against your current kube context.

## Configuration

Read from the environment by [`config.go`](config.go) and
[`test/testconfig`](../testconfig/config.go).

| Variable | Default | Meaning |
| --- | --- | --- |
| `ENVIRONMENT` | `kind-emulator` | `kind-emulator`, `kubernetes` or `openshift` |
| `USE_SIMULATOR` | `true` | emulated model servers, or real vLLM |
| `SCALE_TO_ZERO_ENABLED` | `false` | run the scale-to/from-zero specs; must match the infra |
| `WVA_NAMESPACE` | `workload-variant-autoscaler-system` (make: `llm-d-sim`) | the controller's namespace |
| `LLMD_NAMESPACE` | `llm-d-sim` | where specs create model servers |
| `MONITORING_NAMESPACE` | `workload-variant-autoscaler-monitoring` | Prometheus |
| `MODEL_ID` | `e2ewva/dummy-model` | model name the specs serve |
| `ACCELERATOR_TYPE` | `H100` | accelerator the specs request |
| `MAX_NUM_SEQS` | `5` | engine batch size; lower saturates sooner |
| `POD_READY_TIMEOUT` | `300` | seconds to wait for model pods |
| `SCALE_UP_TIMEOUT` | `600` | seconds for the longest scale and job waits |
| `E2E_EVENTUALLY_SHORT` / `MEDIUM` / `STANDARD` / `LONG` / `EXTENDED` | `30` / `60` / `120` / `180` / `300` | `Eventually` timeouts, seconds |
| `E2E_EVENTUALLY_POLL` / `POLL_QUICK` / `POLL_SLOW` / `POLL_VERY_SLOW` | `5` / `2` / `10` / `15` | poll intervals, seconds |
| `WARMPOOL_PROXY_IMAGE` | the published proxy | warm-pool proxy under test |

On a real GPU cluster, set `ENVIRONMENT`, `USE_SIMULATOR=false`, `MODEL_ID` and
`ACCELERATOR_TYPE` to match it, and raise the timeouts; the simulator-only specs
then skip.

## Writing a spec

- Shared builders and ensure/delete helpers are in [`fixtures/`](./fixtures/)
  (`go doc ./test/e2e/fixtures`). Resources they create carry the label
  `test-resource=true`.
- Label it `smoke` or `full` (and `flaky` to keep it out of CI), make it
  `Ordered`, and clean up in `AfterAll`. Mark it `Serial` if it changes
  cluster-wide state or shares a model with another spec.
- If it needs a flag or a capability, `Skip()` with a message that names it.
- Container images must come from a fully qualified registry, never Docker Hub.

## Troubleshooting

```bash
kubectl get pods -n llm-d-sim
kubectl get scaledobject,hpa -n llm-d-sim -o wide
kubectl logs -n llm-d-sim deploy/wva-controller-manager --tail=200
```

A ScaledObject with `READY False` usually names a scaler address where no
controller runs. Leftovers from an interrupted run:

```bash
kubectl delete scaledobject,deployment,job -n llm-d-sim -l test-resource=true
```
