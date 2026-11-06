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
It also records each model's composite, with the inputs the engine handed it
([the engine layer](#the-engine-layer)). It is a test aid, off by default.

Before you start:

- Recordings go to the node's local disk, in an `emptyDir`. A new pod has
  a new, empty one, so anything that replaces the pod deletes them: turning
  the recorder on or off, a `kubectl rollout restart`, an upgrade or any
  change to the pod template, an eviction, a node drain, an uninstall. A
  container restart (a crash, an OOM) keeps them. There is no other sink in
  this release.
- Re-running the installer does not turn the recorder off and, when it
  changes nothing else, does not replace the pod: it applies client-side,
  and the fields the patch adds are not in what it applied before. Measured
  against an API server (`test/recorderpatch`): after the installer's apply
  the recorder is still there, and the Deployment's generation has not
  moved.
- A recording starts only when the controller starts. It keeps the head of a
  run: once full, it stops. For a new recording, restart the controller.

### Turn it on

Patch the installed controller. The patch adds an environment variable, a
volume and a small `recordings` sidecar; it changes no image and no other
setting of the manager:

```bash
NS=wva-system   # the controller's namespace
kubectl -n "$NS" patch deployment wva-controller-manager --type strategic --patch-file config/components/learner-recorder/learner-recorder-deployment.yaml
kubectl -n "$NS" rollout status deployment/wva-controller-manager
```

On OpenShift, use
`config/components/learner-recorder-openshift/learner-recorder-openshift-deployment.yaml`
instead: the same patch without the sidecar's fixed `runAsUser`, which the
restricted-v2 SCC refuses (it assigns one). For an overlay you render
yourself, the components `config/components/learner-recorder` and
`config/components/learner-recorder-openshift` apply the same patches.

The patch mounts a 600Mi `emptyDir` at `/var/lib/wva/learner-recordings` and
sets `LEARNER_RECORDING_DIR` to it. The sidecar mounts the same volume
read-only, because the manager image has no `tar`, which `kubectl cp` needs.
Without the patch, set the directory with `--learner-recording-dir` (or
`LEARNER_RECORDING_DIR`) and mount a volume there yourself. Never `/tmp`: it
is a 64Mi `emptyDir`, and the kubelet evicts the pod when it fills. Options a
recording cannot start with (`/tmp`, a cap under 64Ki, a directory ceiling
under two caps, a size that is not a whole number) are logged at startup,
and the controller runs without recording.

To change the sizes on an installed controller, set them in the manager's
environment (in bytes); this replaces the pod, so copy the recordings out
first. Keep the volume's `sizeLimit` (600Mi in the patch) at least the
directory ceiling:

```bash
kubectl -n "$NS" set env deployment/wva-controller-manager -c manager LEARNER_RECORDING_CAP_BYTES=134217728 LEARNER_RECORDING_DIR_BYTES=268435456
```

What it writes:

- One file per controller start, named by its start time:
  `learnrec-20260302T090000.000000000Z.jsonl`. The first line is the start
  (how the analyzers were configured); a replay refuses a file without it.
- At most 256Mi per recording (`LEARNER_RECORDING_CAP_BYTES`, or
  `--learner-recording-cap-bytes`). At the cap the recording keeps what it
  has, stops writing, and logs how many events it dropped.
- At most twice the cap in the directory (`LEARNER_RECORDING_DIR_BYTES`, or
  `--learner-recording-dir-bytes`). To make room, a new recording first
  deletes the tiny ones (under 64Ki) a crash loop leaves. Then, if a full
  cap does not fit, it takes what is left, down to 64Ki, and deletes no
  recording with data. Only when less than 64Ki would be left do the older
  recordings with data go, oldest first, and the newest of them last.
- How long a recording lasts. The in-process acceptance run measured about
  1.7 KB per replica per cycle for each analyzer a model lists (each
  `Analyze` carries its input), before the TTFT inputs
  ([analyzer-learner, 2.9](../proposals/analyzer-learner.md#29-recorder-fields-for-ttft-and-capture))
  were recorded. They add an estimated 4 KB, not yet measured, for about
  5.6 KB per replica per `Analyze`. With vLLM, a 15 s cycle and that
  estimate, the default 256Mi cap fills in:

  | replicas | saturation alone | saturation, slo and throughput |
  | --- | --- | --- |
  | 8 | about 25 hours | about 8 hours |
  | 100 | about 2 hours | about 40 minutes |

  Recording also turns on the queries only it reads: up to 16 more per vLLM
  namespace per cycle, more with SGLang.

### Copy the recordings out

Copy the files out before anything replaces the pod:

```bash
POD=$(kubectl -n "$NS" get pod -l control-plane=controller-manager -o name | head -1)
kubectl -n "$NS" exec "${POD#pod/}" -c recordings -- ls -l /var/lib/wva/learner-recordings
kubectl -n "$NS" cp -c recordings "${POD#pod/}:/var/lib/wva/learner-recordings" ./learner-recordings
```

### Turn it off

Copy the recordings out first: this replaces the pod, and the new one has a
new, empty volume. The same command on OpenShift.

```bash
kubectl -n "$NS" patch deployment wva-controller-manager --type strategic --patch-file config/components/learner-recorder/learner-recorder-off-deployment.yaml
kubectl -n "$NS" set env deployment/wva-controller-manager -c manager LEARNER_RECORDING_CAP_BYTES- LEARNER_RECORDING_DIR_BYTES-
```

The second command is needed only if you set the sizes. Re-running the
installer does not turn the recorder off (see above); uninstalling does.

### Replay one

`learnreplay.Replay` reads a recording and runs it through new analyzers and a
new capacity store. In `Fidelity` mode, at the commit that recorded it, the
replay must reproduce the recording byte for byte. The acceptance test for
that is `TestLearnerRecording_AReplayAtTheSameCommitReproducesEveryResult`:

```bash
go test ./internal/engines/steadystate/ -run TestLearnerRecording -v
go test ./internal/engines/analyzers/learnreplay/ ./internal/signals/learnrec/ -v
```

### Capture a past window

A run that was not recorded can still be made into a recording, from
Prometheus alone, while Prometheus holds its series: `whatif capture` reads
the window's cycle inputs, and `whatif generate` plays them through the
analyzers ([how it works](whatif-capture.md)). The result is an ordinary
recording at the commit of the `whatif` that generated it.

`_bucket` series are often kept for days, not weeks. Preserve a window worth
keeping as a Prometheus snapshot first, and capture it from the snapshot
later. The `ols-sweep-slo` run is the first such window: flow control off,
the slo analyzer listed, streamed load, router TTFT recorded.

1. Snapshot Prometheus while it still holds the run. The snapshot API needs
   Prometheus started with `--web.enable-admin-api`; the snapshot is written
   under its data directory:

   ```bash
   kubectl -n "$PROM_NS" port-forward "svc/$PROM_SVC" 9090:9090 &
   # The answer is {"status":"success","data":{"name":"20261101T120000Z-..."}}.
   NAME=$(curl -s -XPOST http://localhost:9090/api/v1/admin/tsdb/snapshot | jq -r .data.name)
   test -n "$NAME" && test "$NAME" != null || echo "no snapshot: is the admin API on?"
   kubectl -n "$PROM_NS" cp -c prometheus "$PROM_POD:/prometheus/snapshots/$NAME" ./ols-sweep-slo-snapshot
   ```

   `kubectl cp` runs `tar` inside the container, so it needs `tar` in the
   Prometheus image. A distroless image has none, and the copy fails with
   `executable file not found`. Then copy from a helper Pod that mounts the
   same volume and has `tar`: a Pod with an image that has `tar` (from a
   registry you trust), the Prometheus data volume's claim mounted
   read-only at `/prometheus`, then
   `kubectl -n "$PROM_NS" cp "$HELPER_POD:/prometheus/snapshots/$NAME" ./ols-sweep-slo-snapshot`.
   A ReadWriteOnce claim mounts only on Prometheus' own node, so schedule
   the helper there. An ephemeral container works as well where the
   cluster allows them:
   `kubectl debug -n "$PROM_NS" "$PROM_POD" --target=prometheus --image=IMAGE-WITH-TAR -c snap -- sleep 3600`,
   then `kubectl cp -c snap "$PROM_POD:/proc/1/root/prometheus/snapshots/$NAME" ./ols-sweep-slo-snapshot`.

   Without the admin API, `promtool tsdb dump` writes the series out as text,
   and `promtool tsdb create-blocks-from openmetrics` makes blocks of them
   again.

2. Serve the snapshot with a local Prometheus. Give it a retention longer
   than the snapshot's age, or it deletes the old blocks on start:

   ```bash
   prometheus --storage.tsdb.path=./ols-sweep-slo-snapshot --storage.tsdb.retention.time=10y \
     --web.listen-address=:9091 --config.file=/dev/null
   ```

3. Capture the run's window, with the policy it ran under and the engine
   arguments its Deployments had. With kube-state-metrics in the snapshot,
   `--kube-state-metrics` reads Pods, readiness, GPUs and nodes; without its
   role labels (`--metric-labels-allowlist=pods=[llm-d.ai/role]`), add a
   `--role-of` per variant:

   ```bash
   make whatif
   bin/whatif capture --prometheus http://localhost:9091 --namespace "$RUN_NS" \
     --kube-state-metrics --role-of decode=app=ols-decode --role-of prefill=app=ols-prefill \
     --from 2026-11-01T12:00:00Z --to 2026-11-01T13:00:00Z --step 15s \
     --policy ols-sweep-slo-policy.yaml --engine-args "--max-num-seqs=256" \
     -o ols-sweep-slo.capture.jsonl.gz
   ```

   The summary lists each variant and every flag the capture carries:
   engine parameters from flags, roles inferred from Pod names, Pods read as
   Ready without kube-state-metrics, rollouts not seen. Read them before
   trusting the window; the generated recording carries them too.

   Every query is required. One that fails (often `query.max-samples` on a
   histogram over many steps) stops the capture with exit 1 and names the
   query: shorten the window or lengthen `--step`. Each HTTP request may
   take `--timeout` (2m30s by default). Two bounds need `--force` to pass:
   `whatifsource.MaxStepQueries`, 1,000,000 steps times the 80 queries a
   capture with kube-state-metrics registers (12,500 steps, a little over
   two days at 15 s), which bounds the load; and `whatifsource.MaxSamples`,
   20 million samples held in memory, which grows with the Pods as well.
   [What-if capture](whatif-capture.md#load-on-prometheus) gives the load
   a window puts on Prometheus.

4. Generate the recording, and add it as a golden like any recording
   (below):

   ```bash
   bin/whatif generate -in ols-sweep-slo.capture.jsonl.gz -o ols-sweep-slo.jsonl
   make learner-golden-add RECORDING=ols-sweep-slo.jsonl NAME=ols-sweep-slo-capture \
     SOURCE="ols-sweep-slo window, captured from its Prometheus snapshot"
   ```

A capture asks only range queries, one per query over the whole window.
Its tests use the fake Prometheus in `internal/whatifsource/promtest`, never
a live one:

```bash
go test -p 1 ./internal/whatifsource/... ./cmd/whatif/
go test -p 1 ./internal/engines/analyzers/learnreplay/ -run 'TestGenerate|TestMessageWords_AreCurrent'
go test -p 1 ./internal/engines/steadystate/ -run TestWhatifCapture -v
```

### Learner Goldens

A golden is a recording committed as a test fixture, beside the outputs its
replay produces. `make test` replays every golden and fails when an output
has moved. The rule for when an output may move is in
[the learner proposal](../proposals/analyzer-learner.md), section 2.5.

The goldens live in `internal/engines/analyzers/learnreplay/testdata/goldens/`:

- `goldens.yaml` lists every golden and every file of it. The check fails
  when it and the directory disagree: a file it does not list, or a file it
  lists that is missing.
- One directory per golden holds `recording.jsonl.gz`, the exported
  recording, and one CSV file per output: `results.csv` (each result: role
  demand, start seconds and the rest), `variant-capacities.csv`,
  `store-records.csv` (the capacity fields the analyzers write),
  `lines.csv` (ITL lines published and borrowed), `slo-published.csv` and
  `learned-state.csv` (each learned-state document, one row per key).

Each row is keyed by the event kind, its time, the model (or line key, or
learned-state section) and the variant (or key), never by a sequence number.
Two integers must be equal. Two other numbers are equal when they differ by
less than one part in 10^9. So two things that vary between replays of the
same recording do not fail the check: events numbered in another order, and
a float sum whose last bits differ.

The check also fails on a golden that compares nothing. Every analyzer that
analyzed must have results, and the slo analyzer, if it analyzed, must have
published.

- An analyzer analyzed when the recording holds at least one of its
  `Analyze` events.
- The start line names the analyzers the controller built. The controller
  builds the slo analyzer whatever the policy lists, so a run whose models
  list only saturation has slo built and never analyzed.
- Such an analyzer is not compared: its rows in `results.csv` and its
  `slo-published.csv` are empty. The add writes it into the fixture's
  `notRun` field in `goldens.yaml`, and the check prints it as a note, for
  example `note: e1: slo: built, never analyzed (0 Analyze events)`. The
  check fails when a replay disagrees with `notRun`, so a fixture that
  moves into or out of the set is a diff of `goldens.yaml`. An update
  writes the set back and prints a `changed:` line for each fixture whose
  set it changed.
- An analyzer is listed when a policy entry names it as its effective
  type: its `type` when set, else its `name`. An entry with
  `enabled: false` does not list it.
- An analyzer that a recorded policy lists and that has no `Analyze` event
  is refused: the controller was asked to run it and never did.
- An analyzer with even one `Analyze` event analyzed, and a golden that
  compares nothing of it is refused.

Every other output of a fixture must hold rows, unless its recording
allows it to be empty by one of these rules; the check fails on an output
that is empty without one:

- `slo-published.csv`, when the run did not build the slo analyzer, or
  built it and never analyzed it.
- `lines.csv`, when the recording holds no ITL line published or borrowed.
  A variant publishes a line once it fits one of its own, whatever the
  policy says of reuse; a borrow, the other half, happens only when the
  policy turns learned-state reuse on.
- `learned-state.csv`, when the recording holds no learned-state export:
  the controller took no learned-state snapshot in the run.
- `composite-holds.csv`, when the replay held no role (its
  `composite-hold` count is 0), and `composite-release.csv`, when no other
  analyzer's frame bounded a release (`composite-release-bound` is 0), and
  `composite-roles.csv`, when no composite priced a role (its
  `composite-role-capacities` count is 0: the run has no P/D model). Only
  a golden whose recording carries engine events has these two.

The `acceptance` golden is the in-process acceptance run. It covers little
learning: twelve cycles of fixed synthetic rows. The start-time average, the
borrow tolerance and the measured k2 and mu paths barely move in it, so a
change to them can leave it as it is. Its generator (`recRow`) keeps one
Ready time per replica for the day. Until learner PR 4a it stamped a Ready
time 30 min before each cycle, a new Ready time on every cycle, which the
settle tracker reads as an engine restart each time, so no line ever
settled; the `acceptance` and `engine` recordings were taken again with the
fix. The run's controller restarts at its start, so the ITL line waits
`steadyFor` and publishes its first fit on the run's ninth cycle.

The `mu-paths` golden is a second in-process run, for those paths. Its
replicas saturate, so k2 records readings and mu's window takes samples,
each the mean of the replicas read in its minute. Each of mu's three yields
runs on a variant of its own (to the derived figure on a served bound, to
the full siblings, and to the derived figure once stale), and one replica
restarts under its own name. To write it again as a raw recording:

```bash
LEARNREC_MUPATHS_OUT=$HOME/mu-paths.jsonl go test ./internal/engines/steadystate/ -run TestLearnerRecording_TheMuPathsRunReachesEveryLearnerPath -v
```

The `pd-restart` golden is a cluster recording of a P/D model, made with
EPP flow control off, so k2 reads each engine's own queue. The model is the
P/D benchmark scenario: Qwen3-0.6B, one prefill and one decode pod to start.
The load is open loop, 1000 tokens in and 1000 out, in steps of 4, 14, 40
and 4 requests a second. At 40 the decode fleet is at its ceiling of four
replicas, with hundreds of requests queued on each. Mid-run, one decode
pod's engine is killed and its container restarts under the same pod name.
It is the golden that reaches `k2-unsettled-window` (a load step k2 refuses
to learn from), and the only one in which `settle-restarted` comes from a
real restart rather than a generator;
`TestGoldens_PDRestartReplayRestartsForReal` holds its own replay to that.

It cannot be written again in-process. To take it again at a new commit:

1. Stand up the scenario `hack/benchmark/scenarios/guides/pd-disaggregation.yaml`
   with `MODEL_ID=Qwen/Qwen3-0.6B`. Set decode to 16 sequences at most, and
   give the decode ScaledObject 1-4 replicas and the prefill one 1-2.
2. Remove the `flowControl` feature gate and the `flowControl:` block from
   the EPP's `EndpointPickerConfig`, then restart the EPP.
3. Install the controller at that commit. Then turn the recorder on, as the
   last change before any load, so that the controller start that follows
   begins the recording.
4. Run the load above. Partway through the 40 request-a-second step, run
   `kill -9 -1` in the `vllm` container of one Ready decode pod: the
   container restarts, and the Pod keeps its name and UID.
5. Copy the recording out, then add it with `make learner-golden-add` at
   that commit.

The `ols-sweep` golden is the cluster recording whose ITL line comes from
full fits (`itl-ols`), not only from fits with B pinned. A full fit needs
steady readings at least 0.30 of k apart, and a steady load never gives
that: it holds k in a narrow band. The stock P/D decode pod cannot get
there at all. On an H200 its KV holds about 1.16M tokens (about 9080 blocks of 128),
and 16 sequences of at most 16384 tokens fill no more than about 0.23 of
it.

So the run shrinks the decode KV and steps the load. It is the P/D
benchmark scenario with EPP flow control off, and decode pinned at one
replica. The load is closed loop and changes every 4.5 minutes, over
three shapes: 1000 tokens in and 4000 out, 1000 and 500, and 6000 and
1000. The decode pod's k sat near 0.2 on the low levels, and at 0.45 to
0.6 on the high ones. At the commit it was added, its replay took 101 full
fits and 94 pinned ones, the same counts as the controller's own log of
that run. `TestGoldens_OLSSweepReplayFitsOLS` requires at least 80 full
fits, and some pinned ones.

To take it again at a new commit:

1. Stand up the scenario `hack/benchmark/scenarios/guides/pd-disaggregation.yaml`
   with `MODEL_ID=Qwen/Qwen3-0.6B`, as for `pd-restart`, step 1, with these
   changes to the decode Deployment instead of its 16 sequences:
   - set the `vllm` container's environment variable `VLLM_MAX_NUM_SEQ` to
     `64` (the decode command passes it as `--max-num-seq`);
   - keep `VLLM_MAX_MODEL_LEN` at the scenario's `16384` (`--max-model-len`);
   - add `--num-gpu-blocks-override 1024` to the decode `customCommand`,
     after `--max-num-batched-tokens 8192`. At the scenario's block size of
     128 that is 131072 tokens of KV. The engine reports it as
     `num_gpu_blocks="1024"` in `vllm:cache_config_info`.

   Give the decode ScaledObject 1-1 replicas and the prefill one 1-2.
2. Remove EPP flow control and turn the recorder on, as for `pd-restart`,
   steps 2 and 3.
3. Run the load with `hack/benchmark/closed_loop_sweep.py`, from anywhere
   with Python 3, `aiohttp`, and a route to the router service:

   ```bash
   python3 hack/benchmark/closed_loop_sweep.py --url http://ROUTER:80 --model Qwen/Qwen3-0.6B
   ```

   Its default schedule is the one this golden ran. The first level is 10
   requests in flight at 1000 tokens in and 4000 out, for 300 seconds. Then
   come eleven levels of 270 seconds each, as in flight / tokens in / tokens
   out: 24/1000/4000, 10/1000/4000, 24/1000/500, 56/1000/500, 5/6000/1000,
   11/6000/1000, 4/6000/1000, 24/1000/4000, 20/1000/500, 10/6000/1000 and
   8/1000/4000.

   During the first level, read the decode pod's `vllm:kv_cache_usage_perc`.
   It should be about 0.25. If it is not, stop the script and start it
   again with `--scale` set to 0.25 divided by that reading. The script
   multiplies every in-flight count by the factor, rounds it, and keeps it
   between 1 and 64.
4. Copy the recording out, as in [Copy the recordings out](#copy-the-recordings-out).
   Then add it at that commit. `REPLACE=1` replaces the committed golden of
   the same name:

   ```bash
   make learner-golden-add RECORDING=learnrec-<start>.jsonl NAME=ols-sweep REPLACE=1 \
     SOURCE="P/D benchmark scenario, flow control off, decode KV of 1024 blocks and 64 sequences on one decode replica, load sweeps of concurrency and shape, default analyzerTuning"
   ```

   A new recording has new wall-clock times. Move the cut time in
   `TestGoldens_OLSSweepReplayFitsOLS`, and the times in its comment, to
   the new run's first fit.

The `ols-sweep-slo` and `ols-sweep-slo-restart` goldens are one run of the
`ols-sweep` recipe with the slo analyzer listed beside saturation. The slo
analyzer fits its own ITL line, with its own tracker, and judges the model
from the router's latency. Mid-run the leader controller Pod was deleted
under load, so the run left two recordings: `ols-sweep-slo` up to the
restart and `ols-sweep-slo-restart` after it.

At the commit they were added, each replay took the same counts as the
controller's own log of the run, for both analyzers (the saturation
analyzer's `itl-fit` lines and the slo analyzer's `slo-variant` lines):

| Golden | Full fits | Pinned fits | slo `Analyze` calls with router latency | Judged TPOT / TTFT |
| --- | --- | --- | --- | --- |
| `ols-sweep-slo` | 15 | 33 | 79 of 83 | 55 / 55 |
| `ols-sweep-slo-restart` | 57 | 66 | 143 of 143 | 115 / 113 |

A judged objective has a finite observed figure and is not excluded. Most
of the rest came in the first minutes after each controller start, with no
router requests yet or excluded as a transient (`unobserved`). At the end
of the restart file, two TTFT windows had too few requests.

The slo analyzer's fits count as `slo-itl-ols` and `slo-itl-pinned` in a
replay's paths, and the golden check requires both, as it does
saturation's. `TestGoldens_OLSSweepSLOReplayFitsBothLines` requires, in
each golden, a floor of full and of pinned fits of each line, of `Analyze`
calls with router latency (an input of the recording), and of objectives
the replay judged (its output). Its head replay ends with the cycle just
before the first fit and must hold no fit of either tier, so a change that
admits a fit even one cycle earlier fails it. Up to that cycle the pinned
fit declines, and the head must reach that verdict.
`TestGoldens_OLSSweepSLORestartRestoresNothing` requires the restart file
to start with an empty store and to restore no learned state.

To take them again at a new commit, follow the `ols-sweep` steps with
these changes:

1. **The policy.** Add a tier `slo-watch` that lists saturation with its
   current entry (`score: 1.0`, no `parameters:`, as the scenario's
   `default` has it) and `slo`, with a TPOT target of 50ms and a TTFT
   target of 2s at the 95th percentile. Name it as `scalingPolicy:
   slo-watch` in the trigger metadata of both the decode and the prefill
   ScaledObject. See
   [the slo section of the scaling policy reference](../reference/scaling-policy.md#slo-observe-only).

   Write the tier into the map the model actually reads. If the model's
   namespace has its own `wva-scaling-policy-config`, that map replaces
   the global one whole, and a tier written only into the global map
   does not exist for the model: the controller logs
   `Scaling policy not found` and falls back to that map's `default`
   entry, which here has no `slo`.

   Before the load, check that the model's `Effective scaling policy` line
   shows `"scalingPolicy": "slo-watch"` and
   `"slo": "tpot mean<=50ms, ttft p95<=2s (observe)"`. The `(observe)`
   says the slo analyzer is listed. The first cycle after a controller
   start can run before the ConfigMaps load and log `"slo": "off"`; wait
   for the next line. The line does not say whether saturation is listed,
   so also check that the recording holds both `saturation.Analyze` and
   `slo.Analyze` events: both analyzers are listed and ran.
2. **A streamed load.** The router measures TTFT and TPOT only on streamed
   requests. Run the sweep with `--stream`. It sends `stream: true` with
   `stream_options` `include_usage`, and reads each stream to its
   `data: [DONE]`:

   ```bash
   python3 hack/benchmark/closed_loop_sweep.py --url http://ROUTER:80 --model <model> --stream
   ```

   A few minutes into the load, the router's `/metrics` must show
   `llm_d_epp_request_ttft_seconds_count` and
   `llm_d_epp_request_streaming_tpot_seconds_count` above zero for the
   model. Neither exists before the first streamed request.
3. **A controller restart under load.** Keep the learned-state snapshot
   off (the default). 90 seconds into the fifth level (56 in flight, 1000
   tokens in and 500 out), find the Pod that holds the leader lease, copy
   its recordings out, then delete it:

   ```bash
   kubectl -n "$NS" get lease -o custom-columns=NAME:.metadata.name,HOLDER:.spec.holderIdentity
   POD=<the holder, up to the first underscore>
   kubectl -n "$NS" cp -c recordings "$POD:/var/lib/wva/learner-recordings" ./learner-recordings
   kubectl -n "$NS" delete pod "$POD"
   ```

   The new Pod's recorder opens a second file. At the end, copy the new
   Pod's recordings out the same way. Keep both files: the first is the
   one copied before the delete, the second the one the new Pod wrote.
4. **Cross-check the counts** against the controller's own log, taken with
   `kubectl logs --timestamps` from both Pods. Keep each line once, and
   only lines whose timestamp is inside that file's window: from its start
   line to its last event. Count:
   - `itl-fit` lines by their `tier` (`ols`, `pinned-B`), for saturation;
   - `slo-variant` lines by their `lineTier`, for the slo analyzer;
   - `slo-model` lines with a number in `observed` and no `excluded`, per
     `objective`, for the judged windows.

   Each replay must give the same counts. Also check that the second
   file's first line carries `"StoreRecords":0` and that neither file
   holds a `saturation.RestoreLearnedState` or
   `saturation.TakeRestoreOutcomes` event: the restart restored nothing.
5. Add each file under its own name, at that commit:

   ```bash
   SRC="P/D benchmark scenario, flow control off, decode KV of 1024 blocks and 64 sequences on one decode replica, stream load sweeps of concurrency and shape, policy tier with saturation and slo targets on tpot and ttft, default analyzerTuning"
   make learner-golden-add RECORDING=learnrec-<first start>.jsonl NAME=ols-sweep-slo REPLACE=1 SOURCE="$SRC; up to a controller restart"
   make learner-golden-add RECORDING=learnrec-<second start>.jsonl NAME=ols-sweep-slo-restart REPLACE=1 SOURCE="$SRC; after a controller restart under load, snapshot off"
   ```

6. **Move the test to the new run.** In `olsSLOFloors`, for each golden:
   - Set each floor to about four fifths of the new count: full and
     pinned fits of each line, `Analyze` calls with router latency, and
     judged TPOT and TTFT.
   - Set the cut. The first fit is the time of the first row of the
     golden's `lines.csv`. The cut is any moment between the cycle before
     it and the fit itself. The cycles are the `slo.BeginCycle` events of
     the recording, about 15 seconds apart. The dates are that
     recording's wall clock.
   - Write the new counts and times in the comments. The test's own log
     line prints the replay's counts.

The four `ttft-*` goldens are the TTFT track's prefill-stress runs (P2 of
[the what-if plan](../proposals/slo-capacity-planner.md#pr-plan)): the P/D
scenario with Qwen3-0.6B at TP 1 on H200, replicas pinned, EPP flow control
off, the recorder on with every R1 field. Prompts are words of about one
token each, ±10%.

| Golden | Fleet and load | Recorded at | Compressed |
| --- | --- | --- | --- |
| `ttft-three-pods` | 3 prefill + 3 decode; closed loop, 30k tokens in, 250 out, 3 to 72 in flight | 2295aed40 | 1.06 MB |
| `ttft-saturated` | 1 prefill + 2 decode; closed loop to prefill saturation, 8k to 37k in, 16 out | 2295aed40 | 275 KB |
| `ttft-open-loop-long` | 1 + 1; Poisson, prefill occupancy under 0.1, 8k, 30k and 37k in | 324b7da97 | 88 KB |
| `ttft-open-loop-short` | 1 + 1; Poisson, the same, 1k, 2k, 4k and 16k in | 18454b80a | 111 KB |

Each was added whole, at the commit that recorded it, from a worktree of
that commit, and carried over ([a recording made at another
commit](#a-recording-made-at-another-commit)); no output of an earlier
golden moved. Every run lists saturation only, so slo is built and never
analyzed in each. On main they move under two earlier switches, recorded
in `earlierSeams`: `k2-first-reading-waits-for-a-second` (one decode cycle
each in `ttft-saturated` and `ttft-three-pods`) and
`kv-per-request-output-dispersion` (`ttft-three-pods`, the first golden
whose output lengths spread). `ttft_validation_test.go` replays them to
validate the TTFT model ([as built,
P2](../proposals/slo-capacity-planner-ttft.md#as-built-p2)); its tables are
frozen, and a change that moves a figure there fails it. The load
generator, the level windows and the evidence of each run live outside
the repository; the windows are in the test.

The four `ttft-staircase-*` goldens are four of the six phases of the what-if TTFT
staircase, repeat 1
([as built, M2](../proposals/slo-capacity-planner-ttft.md#as-built-m2)): the
P/D scenario with Qwen3-0.6B at TP 1 on H200, 2 prefill and 2 decode replicas
pinned, the recorder on, and slo listed with a TTFT target. They are the first
goldens with the flow-control gate on, and S1 reads on every cycle, as it does on
`ols-sweep-slo` and `ols-sweep-slo-restart`. Each phase has its own controller
start, so its own recording.

| Golden | Load | Flow control | Cycles | Compressed |
| --- | --- | --- | --- | --- |
| `ttft-staircase-a-off` | 1000 in, 1000 out, Poisson, 3 steps | off | 128 | 589 KB |
| `ttft-staircase-b-off` | 8000 in, 200 out, Poisson, 7 steps | off | 289 | 1.37 MB |
| `ttft-staircase-b-on` | the same | on | 289 | 1.49 MB |
| `ttft-staircase-c-on` | B's shape, gaps of SCV 4, 7 steps | on | 292 | 1.52 MB |

Each was added whole at 59772fce2, the commit that recorded it, and carried
over. Off means the gate and the `flowControl:` block were removed from the
`EndpointPickerConfig` and the EPP restarted; on is the block as the scenario
renders it. `b-off` follows an on phase, and its recording holds the wait histogram
for the first minutes; it is not a sign of the gate. The A on and C off phases are
in the validation table only, left out for size and test time; their recordings
are outside the repository.
`ttft_staircase_test.go` freezes what the recordings say of S2's gate and of the
flow-control series. The load driver and the run's other files live outside
the repository.

The `throughput-sweep` and `throughput-sweep-restart` goldens are one run
of the `ols-sweep` scenario with the throughput analyzer listed beside
saturation, for learner PR 4b (the throughput analyzer's ITL line moving
onto the shared tracker). Like the slo pair, the run restarts the
controller under load and leaves two recordings. Take it at a commit with
4b-0 merged: from then on the recording's first line carries the throughput
analyzer's settings (`ThroughputConfig`), and a replay builds the analyzer
as the run did.

The committed run was recorded at 59772fce2, with the KV band at 0.12-0.70
and a tail on the sweep (steps 2 and 4). What its replay reached, by
`throughput.PathNames`:

| Golden | Full fits | Pinned fits | Settling | At the cap | Held | Above band | Compressed |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `throughput-sweep` | 18 | 9 | 5 | 0 | 0 | 0 | 166 KB |
| `throughput-sweep-restart` | 10 | 14 | 8 | 20 | 0 | 0 | 595 KB |

`TestGoldens_ThroughputSweepFloors` requires each non-zero count above zero.
Every settling reading is the wait after a controller start: no joiner is
recorded, since the one decode pod never scales. The test replays with
`throughput.SwitchITLWaitsAfterRestart` off, and with
`throughput.SwitchITLAdmitsAtSequenceCap` off, and requires exactly the
floors each rests on to fail: settling for the first, at-cap and pinned
(`throughput-sweep-restart`) for the second. The held slope ran 0 times: through the 35 minutes of light load the
line was silent with cause `shape-change`, so the held slope stays synthetic.

It needs 2 H200 (one decode, one prefill) for about 2 hours, setup
included. It is a shared cluster: on any failure or abort, go straight to
[Teardown](#teardown-of-the-throughput-sweep), which says where to start
for each step.

The order matters. The controller registers the throughput analyzer only
when it starts, and only if the policy lists it then. Turning the recorder
on restarts the controller, so the policy goes in first and the recorder
last.

`NS` is the run's namespace. The standup installs the controller into it,
so the controller, its policy map and the model all live there.

1. **The scenario and the controller.** Check out the commit, and use the
   controller image built from it, pinned by digest. Apply the decode
   changes of the `ols-sweep` step 1 (64 sequences, `VLLM_MAX_MODEL_LEN`
   at 16384, `--num-gpu-blocks-override 1024`) to
   `hack/benchmark/scenarios/guides/pd-disaggregation.yaml`, then stand it
   up; the standup also installs the controller:

   ```bash
   make benchmark-standup BENCHMARK_NAMESPACE="$NS" BENCHMARK_SPEC=guides/pd-disaggregation \
     MODEL_ID=Qwen/Qwen3-0.6B IMG=ghcr.io/ev-shindin/llm-scaling-manager@sha256:<digest of the commit's image>
   ```

   Give both ScaledObjects 1-1 replicas, decode and prefill. With both
   pinned, listing throughput cannot change the fleet. Then remove EPP flow
   control: delete the `flowControl` feature gate and the `flowControl:`
   block from the EPP's `EndpointPickerConfig`, and restart the EPP. Do
   **not** turn the recorder on yet.
2. **The policy.** Save the map first, for the teardown:

   ```bash
   kubectl -n "$NS" get configmap wva-scaling-policy-config -o yaml > policy-before.yaml
   kubectl -n "$NS" edit configmap wva-scaling-policy-config
   ```

   In the `default` entry, list throughput beside saturation:

   ```yaml
   analyzers:
     - name: saturation
       score: 1.0
     - name: throughput
       score: 1.0
   ```

   In the same entry, set a band that is not the default. The cap levels of
   step 4 sit above its top edge:

   ```yaml
   analyzerTuning:
     itlBand:
       minObservableK: 0.12
       maxObservableK: 0.70
   ```

   This is the controller's global map: the controller registers the
   throughput analyzer only at start, and only when an entry of that map
   lists it. Keep the rest of `analyzerTuning` at its defaults and the
   learned-state snapshot off.
3. **The recorder, last.** It is the change that rolls the controller:

   ```bash
   kubectl -n "$NS" patch deployment wva-controller-manager --type strategic \
     --patch-file config/components/learner-recorder/learner-recorder-deployment.yaml
   kubectl -n "$NS" rollout status deployment/wva-controller-manager
   POD1=$(kubectl -n "$NS" get pod -l control-plane=controller-manager -o name | head -1)
   POD1=${POD1#pod/}
   kubectl -n "$NS" logs "$POD1" -c manager | grep ThroughputAnalyzer
   ```

   It must say `ThroughputAnalyzer registered (enabled in saturation
   config)`. If it says `NOT registered`, or nothing, the map did not list
   throughput when the Pod started. Fix the map, restart the controller
   explicitly, take the new Pod's name, and check again:

   ```bash
   kubectl -n "$NS" rollout restart deployment/wva-controller-manager
   kubectl -n "$NS" rollout status deployment/wva-controller-manager
   ```

   Then check the recording's start line in that Pod:

   ```bash
   kubectl -n "$NS" exec "$POD1" -c recordings -- ls -l /var/lib/wva/learner-recordings
   kubectl -n "$NS" exec "$POD1" -c recordings -- sh -c 'head -1 /var/lib/wva/learner-recordings/learnrec-*.jsonl'
   ```

   `Commit` is the commit you checked out. `Throughput` is `true`, and
   `ThroughputConfig` has `SettleView` and `CapacityStore` `true` and a
   non-zero `SlopeHoldFor` (the bucket retention, 7 days by default:
   `604800000000000`) and `OptimizationInterval` (the controller's
   interval in nanoseconds, `15000000000` at 15 s). A zero in either says
   the analyzer was not built by `Engine.NewThroughputAnalyzer`: stop and
   tear down.
4. **The load.** Start the sweep in the background, with its PID kept for
   the teardown, and run the `--scale` check of the `ols-sweep` step 3 on
   its first level. The schedule is the default one with a tail, so give it
   whole. The tail rises to 80 in flight over 48 s, so the sequences do not
   start in step. It holds 80 for 8 minutes, 64 running and the rest waiting
   at the decode pod's cap, and then 2 in flight for 35 minutes. `--scale`
   would scale the tail too: if the check asks for it, scale the default
   levels by hand and leave the tail as it is.

   ```bash
   TAIL=6:16:1000:1200,6:24:1000:1200,6:32:1000:1200,6:40:1000:1200,6:48:1000:1200,6:56:1000:1200,6:64:1000:1200,6:72:1000:1200,240:80:1000:1200,240:80:1000:1000,2100:2:1000:500
   BASE=300:10:1000:4000,270:24:1000:4000,270:10:1000:4000,270:24:1000:500,270:56:1000:500,270:5:6000:1000,270:11:6000:1000,270:4:6000:1000,270:24:1000:4000,270:20:1000:500,270:10:6000:1000,270:8:1000:4000
   nohup python3 hack/benchmark/closed_loop_sweep.py --url http://ROUTER:80 --model Qwen/Qwen3-0.6B \
     --max-inflight 100 --schedule "$BASE,$TAIL" > sweep.log 2>&1 &
   echo $! > sweep.pid
   ```

   **After the second level (570 s), check the GPS-mismatch clear:**

   ```bash
   kubectl -n "$NS" logs "$POD1" -c manager | grep -c 'GPS mismatch persisted, clearing observation window'
   ```

   The throughput analyzer clears its window after three cycles in a row
   whose generated-token rate disagrees with its line by more than its
   threshold. That rule predates 4b. When it fires every few cycles, the
   window never fills and the run may hold no `T1-ols` at all, which a
   retake does not fix. Note the count, and **keep the recording anyway**:
   it is the evidence 4b-1 needs to decide whether that clear is wrong for
   the line ([analyzer-learner.md, PR 4b](../proposals/analyzer-learner.md#pr-4b-the-throughput-analyzers-itl-line)).
5. **A controller restart under load.** 90 seconds into the fifth level
   (56 in flight, 1000 tokens in and 500 out). The fifth level starts at
   1110 s (300 s, then three levels of 270 s), so this is at 1200 s, 20
   minutes into the sweep. Copy the first Pod's recordings and log out by
   its name, then delete it:

   ```bash
   kubectl -n "$NS" get lease -o custom-columns=NAME:.metadata.name,HOLDER:.spec.holderIdentity
   # POD1 must be the holder, up to the first underscore
   kubectl -n "$NS" cp -c recordings "$POD1:/var/lib/wva/learner-recordings" ./recordings-1
   kubectl -n "$NS" logs --timestamps "$POD1" -c manager > controller-1.log
   kubectl -n "$NS" delete pod "$POD1"
   kubectl -n "$NS" rollout status deployment/wva-controller-manager
   POD2=$(kubectl -n "$NS" get pod -l control-plane=controller-manager -o name | head -1)
   POD2=${POD2#pod/}
   ```

   The new Pod has a new, empty volume and opens its own recording. When
   the sweep ends (`sweep.log` says so, about 98 minutes in), copy its
   files and log out by its name, to a directory of their own:

   ```bash
   kubectl -n "$NS" cp -c recordings "$POD2:/var/lib/wva/learner-recordings" ./recordings-2
   kubectl -n "$NS" logs --timestamps "$POD2" -c manager > controller-2.log
   ```

   Each file is named by the time its recording started
   (`learnrec-<start>.jsonl`). A Pod whose container restarted (a crash, an
   OOM) keeps its volume, so its directory can hold more than one file: the
   one to keep is the one whose start precedes the load (`recordings-1`) or
   follows the delete (`recordings-2`), and the others are reported, not
   used.
6. **Check what was captured.**
   - Each kept file's first line passes the checks of step 3. The second
     file's first line carries `"StoreRecords":0`, and neither file holds a
     `saturation.RestoreLearnedState` or `saturation.TakeRestoreOutcomes`
     event: the restart restored nothing.
   - Each file holds `throughput.Analyze` events for the model.
   - **Count each side the same way**, for each file and its log, over the
     file's window: from its start line's `Now` to one controller interval
     (15 s) past its last event's `Now`. A cycle logs `analyzer-result` after
     its last event, so a window that stops at that event loses the file's
     last line: 19 ms and 15 ms late in the committed run, and at most 0.39 s
     late on any cycle. Take the log straight after the file, so the log holds
     no cycle the file does not.
     - *Log side.* The lines of `controller-N.log` whose message is
       `analyzer-result`, with `"analyzer":"throughput"` and the model's
       `modelID`, and a timestamp inside the window. That is one line a
       cycle the analyzer ran. For each variant, count the lines whose
       `variants` list holds an entry with its `name`, by that entry's
       `reason`: `T1-ols`, `T2-pinned`, `T2-default`. `T2-failed` is the
       lines less those three: a cycle with no line for the variant
       carries no entry for it, and a cycle with no line for any variant
       logs `"variants":[]`.
     - *Replay side.* The `throughput.Analyze` events of the file with an
       empty `Error`: one a cycle. For each variant, count the rows of
       the golden's `variant-capacities.csv` whose `kind` is
       `throughput.Analyze` and whose `item` is the variant, by the
       `Reason` in their `data`. `T2-failed` is the events less those
       rows.

     Both sides must give the same four counts per variant. The first file
     should hold some `T1-ols` (it has three level changes before the
     restart). If it holds none and the GPS check of step 4 counted
     clears, keep it (step 4). If it holds none and there were no clears,
     the `--scale` check was skipped: retake the run.
7. **Add each file** under its own name, at that commit:

   ```bash
   SRC="P/D benchmark scenario, flow control off, decode KV of 1024 blocks and 64 sequences on one decode replica, one prefill replica, load sweeps of concurrency and shape, then 80 in flight at the sequence cap and 35 minutes of 2 in flight, saturation and throughput listed, analyzerTuning KV band 0.12 to 0.70"
   make learner-golden-add RECORDING=recordings-1/learnrec-<first start>.jsonl NAME=throughput-sweep SOURCE="$SRC; up to a controller restart"
   make learner-golden-add RECORDING=recordings-2/learnrec-<second start>.jsonl NAME=throughput-sweep-restart SOURCE="$SRC; after a controller restart under load, snapshot off"
   ```

   The goldens land in a commit of their own, generated at the PR's base,
   before any 4b switch (the golden rule,
   [analyzer-learner.md 2.5](../proposals/analyzer-learner.md#25-replay-and-the-golden-rule)).
   Write the counts of step 6, and the GPS clear count of step 4, in the
   commit message.

#### Teardown of the throughput sweep

Run these in order at the end of the run, and on any failure or abort.
Each is safe to run again, so after an interrupted teardown step, run it
again and carry on from there.

| aborted during | start the teardown at |
| --- | --- |
| step 1 or 2 (no recorder, no load) | T3 |
| step 3 (recorder on, no load) | T2 |
| step 4, 5 or 6 (load running or just ended) | T1 |
| step 7, or a teardown step | the step it stopped in |

- **T1. Stop the sweep.** It holds the load on the GPUs:

  ```bash
  kill "$(cat sweep.pid)" 2>/dev/null; sleep 5; kill -0 "$(cat sweep.pid)" 2>/dev/null && kill -9 "$(cat sweep.pid)"
  ```

- **T2. Save the recordings and logs first,** from every controller Pod
  still there, by name. A Pod's recordings go with the Pod; nothing later
  can bring them back:

  ```bash
  for P in $(kubectl -n "$NS" get pod -l control-plane=controller-manager -o name); do
    P=${P#pod/}
    kubectl -n "$NS" cp -c recordings "$P:/var/lib/wva/learner-recordings" "./recordings-$P"
    kubectl -n "$NS" logs --timestamps "$P" -c manager > "controller-$P.log"
  done
  ```

  Keep what step 5 copied as well.
- **T3. Delete the scenario and its ScaledObjects.** The teardown target
  also removes the controller it installed:

  ```bash
  make benchmark-teardown BENCHMARK_NAMESPACE="$NS" BENCHMARK_SPEC=guides/pd-disaggregation
  kubectl -n "$NS" delete scaledobject --all
  ```

- **T4. Remove the policy entry and the recorder patch,** where the
  teardown left them:

  ```bash
  kubectl -n "$NS" get configmap wva-scaling-policy-config >/dev/null 2>&1 && \
    kubectl -n "$NS" replace --force -f policy-before.yaml
  kubectl -n "$NS" get deployment wva-controller-manager >/dev/null 2>&1 && \
    kubectl -n "$NS" patch deployment wva-controller-manager --type strategic \
      --patch-file config/components/learner-recorder/learner-recorder-off-deployment.yaml
  ```

- **T5. Confirm no GPU Pod is left:**

  ```bash
  kubectl get pods -n "$NS"
  kubectl -n "$NS" get pods -o custom-columns='NAME:.metadata.name,GPU:.spec.containers[*].resources.limits.nvidia\.com/gpu'
  ```

  No Pod may show a GPU count. If one does, delete what owns it (its
  Deployment or LeaderWorkerSet) and check again; do not leave the
  namespace until this reads clean.

#### The start-seed goldens

`start-seed`, `start-seed-restart` and `start-seed-restart-2` are one kind run,
one golden per controller start, as `ols-sweep-slo` and its restart are. They
are the first goldens in which a variant sets `replicaStartSeconds`, and the
first recorded with `LEARNED_STATE_SNAPSHOT=true`, so the restart goldens
replay a real restore. Start time is Pod creation to Ready, so a simulator with
a readiness delay gives real samples and needs no GPUs. Measured:
[analyzer-learner, recording as run](../proposals/analyzer-learner.md#recording-as-run).
`TestGoldens_StartSeedHoldsTheSeedAndTheMeasuredStart` and
`TestGoldens_StartSeedRestartsRestoreBothStartKeys` hold them to what the run
was for. They were recorded before learner PR 5d, so their restores hold no
`startCounted` and no sightings, and replayed as committed they show neither.
`TestGoldens_StartSeedChainRestoresCountsAndFirstSight` splices in what each
run stored on this build, and holds 5d to their rows.

`start-restored` is the same run taken again on a 5d build, the recording
between its two controller restarts. Nothing in it is spliced: it holds a real
`settle.Restore`, and `TestGoldens_StartRestoredRestoresForReal` holds it to no
refold, with each switch off as its control.

To take it again at a new commit:

1. Create a kind cluster of its own (`CLUSTER_NAME`, `CREATE_CLUSTER=true`),
   and `make deploy-e2e-infra` with an image built from that commit. Turn the
   recorder on, set `LEARNED_STATE_SNAPSHOT=true` and add `--v=3`.
2. Deploy one model as two Deployments of the simulator, each with a TCP
   readiness probe: `initialDelaySeconds` 20 for the fast one, 240 for the slow
   one, `periodSeconds` 1. Give them a Service over both and a ServiceMonitor.
3. Give each a ScaledObject, `maxReplicaCount: 5`. The fast variant is cheaper
   (`variantCost` 10 against 30), so a saturated model fills it to its ceiling
   and the order spills to the slow one. Only the slow one sets
   `replicaStartSeconds: "120"`. Keep the scale-down window short (60 s).
4. Wait until one Pod of each is Ready, then saturate the model: 80 closed-loop
   clients on the Service. Wait for at least 4 Ready replicas of each.
5. Copy the recording out, then delete the controller Pod under load. Five
   minutes later, drop to 2 clients; after 2.5 minutes copy the recording out
   and delete the Pod again. When the fleet has scaled down, saturate again
   until a new Pod of each variant is Ready. Copy the last recording out.
6. `make learner-golden-add` each recording with `REPLACE=1`, oldest first,
   then update the switch tables as [when an output moves](#when-an-output-moves)
   says.

#### The engine layer

The analyzer events stop at each `Analyze`. The engine layer (issue #345,
step 0) carries a recording on to the composite, so a replay holds the
composite to a golden as it holds the analyzers. The optimizer's decisions
get a layer of their own, in a later pull request (before #345 step 3).

With a recording open, `collectV2ModelRequest` writes one `engine.Composite`
event per model per cycle, after the model's analyzers
(`internal/engines/steadystate/engine_recording.go`; the event types are in
`internal/engines/enginerec`). It records only what a replay cannot rebuild,
and what the composite built:

- each analyzer entry, by name, with its liveness verdict, which reads the
  engine's history of the analyzer. The result of an analyzer the recording
  replays (saturation, slo, throughput) is not recorded again; an external
  analyzer's is;
- the entries of the listed analyzers inside their staleness grace, also
  read from that history;
- what the capacity-build step reads of the discovery metadata: each
  variant's name, role and current replicas;
- the composite, the reasons it holds each role, and the warm pool's figures
  published from it: each variant's bridge supply and its pressure.

A replay rebuilds each composite with the engine's own functions. It takes
each analyzer's result from its own replayed `Analyze`. From the policy
that `Analyze` received, it derives what the engine derives from the
cycle's policy: each entry's score and thresholds (`cycleEntry`, which runs
the capacity-build step), the entries the composite may size from
(`eligibleEntries`), and saturation's thresholds (`compositeThresholds`).
A change to an analyzer, or to how the engine assembles the composite's
inputs, therefore reaches the composite goldens.

A test holds the composite to these inputs
(`TestEngineRecording_TheCompositeReadsOnlyItsArguments`). It walks, with
type information, every function of the engine, allocation, aggregation,
domain, config, enginerec and learnrec packages the replay's functions
reach. It fails on a method of the engine, a read of any package's
package-level variable (the logger aside), a call to the time package's
clock, and a call into another package of this module the walk does not
follow. The engine replays its own events
(`steadystate.LearnerReplay`); `learnreplay` cannot import the engine, whose
tests import it, so `cmd/learnergolden` and the replay's tests register it
(`learnreplay.RegisterEngine`). A recording that carries engine events is
not a golden without it: the check says so.

A golden whose recording carries engine events has seven more output files:
`composite-results.csv` (each composite at model scope),
`composite-variants.csv`, `composite-roles.csv` (role capacities of a P/D
model), `composite-holds.csv` (each held role with its reasons),
`composite-release.csv` (`ReleaseBound` and `OtherFrames` per role),
`warm-pool-supply.csv` and `warm-pool-pressure.csv`. A golden without engine
events has none of them.

The `engine` golden is the acceptance run with the engine layer on. It adds
discovery metadata, a bridge lent from the warm pool, the P/D model under a
policy that lists the slo analyzer alone (so the composite holds both its
roles), and an external analyzer that fails every third call (its results
are recorded; on the cycles it fails it is inside its grace, and the
composite holds the roles it covers). From the seventh cycle agg-a's scale
target is two replicas below what its rows report, so the capacity-build
step clamps each analyzer's replica count. Every field of its metadata is
set; the recording keeps three, so a replay that matches shows the
capacity-build step reads no other. To write it again as a raw recording:

```bash
LEARNREC_ENGINE_OUT=$HOME/engine.jsonl go test ./internal/engines/steadystate/ -run TestLearnerRecording_TheEngineLayerReplaysByteForByte -v
```

The input builder (`buildAnalyzerInput`) is held by the same engine run.
`TestInputBuilder_TheEngineRunRecordsTheSameEventsInTheSamePhases` asserts
that every forget and scale-target load of a model comes before the model's
saturation `Analyze`. It also compares the count of each event kind per
cycle with `internal/engines/steadystate/testdata/engine-run-event-counts.txt`.
That file is a golden under the same rule as the learner goldens:

- A PR that changes it names the change in its `goldens:` section.
- A PR that claims zero golden change leaves it untouched.

Rewrite it with `ENGINE_RUN_EVENT_COUNTS_WRITE=1` on the test, locally. The
test refuses the write when `CI` is set.

An external analyzer's name is a name someone chose, so the export renames
it to an `analyzer` pseudonym (`analyzer-01`) wherever it stands: the policy,
an entry, a result, a grace entry, and a hold reason
(`analyzer-in-grace:analyzer-01`). The built-in analyzers keep their names.
A pseudonym keeps the order of the external names among themselves, not
against the built-in names, and the composite reads other analyzers' frames
in name order. So an external name that sorts after a built-in one (after
`saturation`, say) can come out of the export in another order, and the
add's byte-for-byte replay then refuses the export. Nothing is committed.

#### Per-path counters

The saturation analyzer counts how often each learner path runs
(`PathCounts`, `internal/engines/analyzers/saturation/path_counts.go`), and a
replay reports the counts (2.5). Each counts where its path does its work,
not where its input arrives:

- k2 readings recorded in the window (P1-obs);
- mu samples started in the window (a reading within the minute folds into
  the sample and is not counted);
- mu's three yields: to the derived figure on a served bound, to the full
  siblings, and to the derived figure once stale;
- ITL lines from a full fit, and from the pinned fit;
- rows whose KV a learner read: a reading the ITL line took (at the KV fill
  averaged over the ITL minute when the collector read it, else the instant
  fill; the counter does not tell the two apart), or a replica demand
  charged at its instant KV;
- rows whose Pod Ready time k2's joined test read;
- k2 readings refused as a joiner because of the Ready time: the restart
  fix (#288), not the first sight, decided the refusal;
- k2's settle refusals by reason: as a joiner (`k2-unsettled-joined`), and
  as a load step (`k2-unsettled-window`).

The names are one table in `path_counts.go`; `PathCounts` and `PathNames`
read it. Every counter is required. `k2-unsettled-window` runs only with
EPP flow control off, so the `pd-restart` golden is the one that reaches
it.

The replay also runs the settle tracker (`internal/signals/settle`) and
counts its verdicts per reason: `settle-joined`, `settle-rejoined` (back
after more than its absence tolerance), `settle-restarted` (a new Ready time under
the same name), `settle-lent` and `settle-rolled-out`. The engine records
each observation (`settle.Observe`: the model and the instant), and the
replay applies it with the rows of the model's next saturation `Analyze`. A
recording made before the engine recorded them is observed at each
saturation `Analyze`'s own clock, at the default optimize interval. A cycle
whose collection failed is recorded as `settle.Hold` and replayed as one.
k2's joined test reads the replay's tracker, as it read the engine's, so a
recording made by driving the analyzer alone must observe a tracker the way
the engine does (`readers_test.go`, `cmd/learnergolden`); an analyzer given
none takes its first cycle's replicas as already watched, and its replay
would read otherwise.
`settle-joined`, `settle-lent` and `settle-restarted` are required. The
`pd-restart` golden reaches `settle-restarted` from a real container restart
on a cluster. The in-process goldens reach it too, from their generators
(rows whose Ready time moves every cycle). The goldens reach `settle-rejoined`
only through a generator (clocks that jump), so requiring it would pin the
generator, not the tracker. No recording has a rollout under a Pod's own
name. The settle suite (`internal/signals/settle`) covers both the rejoin and
the rollout.

The engine's replay counts its own paths (`LearnerReplay.PathCounts`):
composites built, composites of a P/D model priced per role, composites that
hold a role, composites in which another analyzer's frame bounds a release,
composites built with an external analyzer's recorded result, composites
with an analyzer inside its grace, composites in which the capacity-build
step clamped a variant's replica count to its scale target, and composites
with a variant a bridge serves or a variant pushed past its scale-up
threshold. With the engine's replay registered, they are required too.

The committed goldens must run every path at least once between them:
`make test` and `make learner-goldens-check` fail when one runs on no
golden. A golden that never reaches a path pins nothing about it. An add or
an update prints the paths still uncovered and does not fail on them, so a
first golden can be added to an empty directory.

#### What a golden commits

The export renames every name it can place. It refuses a recording that
holds anything it cannot place: nothing it does not know is committed.

- **Renamed**, to a pseudonym such as `variant-03`: namespaces, models,
  variants, pods, images, revisions, forget tokens, engine-argument text and
  unread flags, label and taint keys and values, resource names, priority
  and weight classes, limiter names, and GPU products the code does not
  know. A name is renamed wherever it stands: in a field, a map key, a part
  of a composite key (by its place: in `ns|model|variant|GPU|count` the
  first three parts are names, whatever they look like), or a message, in
  any case (`PAYROLL` is the name `payroll`). The same name always gets the
  same pseudonym, and names keep their sort order. Engine fingerprints are
  recomputed from the renamed arguments.
- **Replaced**: host names, IP addresses (v4 and v6) and URLs in messages
  become `HOST`, `IP` and `URL`. Analyzer queries and decode errors in the
  policy become `redacted`.
- **Kept as they are**: every number and time; the GPU products the code
  names (`knownAccelerators`); label keys and resource names on an exact
  list (`knownLabels`: `nvidia.com/gpu.product`, `topology.kubernetes.io/zone`
  and the like; `payroll.kubernetes.io/team` is renamed); the code's own
  vocabulary (roles, reasons, dtypes); durations. The add prints the kept
  strings; read them before you commit.
- **Messages** are checked whole token by whole token. A token is a run of
  letters, digits and `- _ . / : |`; anything else separates tokens. After
  renaming, each token must be, whole: a word of the code's own messages, a
  pseudonym (alone, or with a pod's suffix), a composite key whose every
  part is allowed, a number, time, duration or fingerprint, or `HOST`, `IP`
  or `URL`. A pod's suffix is only what a controller appends: digits (an
  ordinal), five characters of Kubernetes' random alphabet
  (`bcdfghjklmnpqrstvwxz2456789`), or a ReplicaSet hash of eight to ten of
  them (`ns-01-7c9dfb6d84-x7k2p`). The code's words are the whole tokens of
  the Go string literals (fmt verbs removed; import paths and struct tags
  left out) of the analyzer, engine, signals, config and domain packages, in
  `internal/engines/analyzers/learnreplay/message_words.txt`; a test fails
  when the file is stale, and `make learner-message-words` writes it. A
  name is renamed in any case, wherever it stands in a token: every run of
  the token's segments (split at `- _ . / : |`) that is a recorded name is
  renamed (`llm-inference-7f9c4-dbrx`, `:demand`), and so is a name glued to
  digits (`2payroll`). A token that is none of these (`tenants-production`,
  `ns-01-hsbc`, a host, a word in another script) stops the export. A word
  the code itself uses (`grace`, `production`) passes on its own: the
  export cannot tell it from the code's.
- **Backstop**: after every rule above, every string value and map key, the
  source and the fixture name are lowercased and searched for every
  recorded name of four characters or more, as plain text. Any hit stops
  the export, whatever split or glued the name (`scale@demand`,
  `payroll-kpmgx`), and so do two recorded names that overlap in one
  message (`payroll-team-x7k2p` holding `payroll-team` and `team-x7k2p`).
  The one hit let through is a whole value or token that equals the name
  and is also the code's own word (a role `decode` in a recording whose
  namespace is `decode`); a code word that merely contains a name is not.
  Shorter names are left to the token rules. No refusal names what it
  refused: it names the field, or the position of the word.
- **Refused**: any other string. The export names the field, never the
  value. If the string is the code's own word, add it to `vocabulary` in
  `internal/engines/analyzers/learnreplay/classify.go`; if it is a GPU
  product or a well-known label, add it to `knownAccelerators` or
  `knownLabels` there; if it is a name someone chose, classify its field as
  a rename there.

The numbers are kept on purpose: they are what the replay runs on. A
recording holds every model the controller manages, so a golden commits
every tenant's load, capacity and timing, under pseudonyms. Record on a
cluster where that is agreed, or scope the controller to the models you
mean to commit.

#### Add a recording as a golden

Record from a controller built from the commit you will add it at. The
recording's start line holds the commit its controller was built from
(`make build`, `make docker-build`, `make docker-buildx` and the image
workflows set it; a binary built from a checkout without them reports the
revision Go stamps into it). The add checks the commit first, then
replays the recording and refuses it unless the replay reproduces it byte for
byte. So it refuses a recording made at another commit, before replaying
anything:

```text
the recording was made at <X> and you are at <Y>
```

The check is skipped when either side is `unknown`, for example a binary
built without `make`.

Then run one command:

```bash
make learner-golden-add RECORDING=./learner-recordings/learnrec-20260302T090000.000000000Z.jsonl NAME=e1 SOURCE="E1 benchmark scenario, recorder on, default analyzerTuning"
```

This:

1. Exports the recording (see above). It first cuts the events after the
   last `Analyze`: a recording cut short can end inside a cycle.
2. Refuses it if it is too large (see below), before any replay.
3. Replays the export and refuses it unless the replay reproduces it byte
   for byte, and unless its goldens compare something.
4. Writes `recording.jsonl.gz` and the output files under `NAME/`, then lists
   them in `goldens.yaml`. A refusal or a failure leaves the directory as it
   was. What a killed add left (`.add-NAME-*`) is removed by the next add
   of the same name.
5. Runs the golden check.

`SOURCE` is how the manifest describes the recording: short, and made of
ordinary words, for example
`E1 benchmark scenario, recorder on, default analyzerTuning`. The add
refuses a `SOURCE` that holds a name the recording holds (in any case, and
split by `-` or `_`: `Payroll-team` holds `payroll`), and one that holds a
word that is neither a word of the code's messages nor in
`internal/engines/analyzers/learnreplay/source_words.txt`, naming the
words by their position. Add an ordinary English word to that file,
lowercase, when a source
needs it; never a name. Never write a cluster name in a source.

`NAME` is held the same way: each part between dashes a number, a
scenario name such as `e1`, or an ordinary word, and no part holding a
recorded name (`e1-tuned` passes, `2payroll` does not).

Add `REPLACE=1` to overwrite a golden of the same name.

#### A recording made at another commit

If the controller ran an older commit than your branch:

1. Check out the recording's commit in a worktree:
   `git worktree add ../at-recording <X>`.
2. Run `make learner-golden-add` there.
3. Commit the new golden directory and the `goldens.yaml` entry there, and
   cherry-pick that commit onto your branch.
4. On your branch, `make test` replays the golden. If an output moved
   between the two commits, the check says which; name the change, then run
   `make learner-goldens`.

#### Size

A compressed recording may be at most 4 MiB. A full recording (256Mi at the
default cap) does not fit: the acceptance run compresses about 29 to 1, so
a full one is about twice the limit; at that ratio a head of up to about
100 MiB fits. Cut it to
its head with `HEAD_LINES=N`. When a recording is too large, the add says
which `N` to use: a head that fits, ends on an `Analyze`, and holds an
`Analyze` of every analyzer the whole recording analyzed. Any head
replays. The add refuses a head that drops every `Analyze` of an analyzer
the whole recording analyzed: cut there, the analyzer would read as built
and never analyzed, and no golden would compare it.

To export without adding a golden, for example to share a recording:

```bash
go run ./cmd/learnergolden export -in RAW.jsonl -out exported.jsonl.gz
```

#### When an output moves

A PR with replay switches holds its runs in `TestGoldens_TheSwitchRuns`
(`switch_runs_test.go`, part of `make test`): all off reproduces the base
commit's goldens (an output a switch names, by the base file's digest),
each switch alone moves only the outputs it names, and all on is the
committed goldens. "All off" is the PR's own switches: replace the table
with yours, and leave an earlier PR's switches at their constructor default,
on, as the base goldens were written. An earlier PR's switch whose seam is
still in the code moves to `earlierSeams`, with the outputs its PR named; it
is replayed off alone from all on, and may move only those. Every name
`SetSwitchesForReplay` accepts must have a row in one of the two
(`TestGoldens_EverySwitchHasARow`); a row goes with its seam. A switch that moves no committed
golden is invisible to the runs, so it names the unit tests that hold it
(`heldBy`, checked to exist): mu's three switches are held by
`mu_repeat_test.go`, `mu_unknown_age_test.go` and `mu_latch_restore_test.go`,
and #396's idle and k2-key bands by `flap_band_test.go`, not by the goldens.

Run `make learner-goldens` only when your PR names the change: it rewrites
every output file from a replay at your commit. Never edit a golden by hand.
`make learner-goldens-check` runs the check alone and prints, per output, how
many rows changed and the first change with its sign and size.
`make learner-goldens` does not rewrite the counts and floors that tests write by hand:
`TestGoldens_PDRestartReplayRestartsForReal` and
`TestGoldens_OLSSweepReplayFitsOLS`. If your change moves them, update them in
the test and say why in the PR.

#### Write the acceptance run again

To write the acceptance run as a raw recording:

```bash
LEARNREC_ACCEPTANCE_OUT=$HOME/acceptance.jsonl go test ./internal/engines/steadystate/ -run TestLearnerRecording_AnExportReplaysAndNamesNothing -v
```

The acceptance, mu-paths and engine runs each write the same bytes every
time at one commit. Events with the same timestamp come out in a fixed
order, not in map order. `NoteVariantsPresent` comes out in model order,
with its variants sorted. Each model's `StableAcceleratorFor` and
`FileScaleTarget` come out in variant order. The runs analyse models in
sorted order themselves. `TestLearnerRecording_EachGeneratorIsReproducible`
holds each run to it.

A running controller still analyses models in map order (`optimizeV2`).
Sorting that loop would change the order models are analysed and reach the
optimizer, not only the recording. So a recording from a running controller
keeps the order its cycles ran in, and is never made twice to compare.

An add replays its recording in Fidelity mode, which fails on a recording
made before an input field was added. So the two tests that add a recording,
`TestAddGolden` ("a head cut anywhere is added") and
`TestRun_AddCheckUpdate`, build it in the test. A new input field then does
not force the committed recordings to be written again. Other tests still
read the committed acceptance recording (`rawRecording` in
`cmd/learnergolden`, `fixtureRecording` in `learnreplay`). They only export
it or run the golden check on it, and neither compares it byte for byte
with a replay.

## Learner Property Tests

The [learner goldens](#learner-goldens) pin what the analyzers output on a
recorded run. The property tests check something else: they feed the
learners synthetic inputs and check what they learn. For example: a transient is refused, a figure does not move
with N, and a genuinely slow replica counts by its weight. The classes and
their thresholds are in
[the learner proposal, 2.6](../proposals/analyzer-learner.md#26-property-tests).
They run in `make test` and take about a second. To run them alone:

```bash
go test ./internal/engines/analyzers/saturation/ -run 'TestLearnerProperties|TestProperty' -v
```

Each case runs on many fixed seeds: 100 for cheap cases, 20 for cases that
drive the analyzer for minutes, and 10 for the N-dependence cases. A failure
names the case (`class/learner/case`), the seed and the violation. To rerun
one case on one seed:

```bash
PROPSEED=7 go test ./internal/engines/analyzers/saturation/ -run 'TestLearnerProperties/edges/k2/key-near-output-boundary' -v
```

Each case is judged inside its own subtest, so a filtered `-run` judges only
the cases it ran.

| file (in `internal/engines/analyzers/saturation/`) | what it holds |
| --- | --- |
| `property_test.go` | the runner, the violation kinds and the ledger check |
| `property_itl_test.go`, `property_k2_test.go`, `property_mu_test.go`, `property_floor_test.go`, `property_other_test.go` | the cases, one file per learner |
| `property_gen_test.go` | the generators, including the time-base family, and the declared estimands |
| `property_mutants_test.go` | the 2.7 findings, with each one's status, and the mutants |
| `property_learners_test.go` | the shared and borrowed ITL line, the borrowed k2 lower bound, the fleet shape hold, the decode-saturation memory, and the SLO analyzer's line through `slo.Analyzer` |
| `property_edges_test.go` | one case per threshold of 2.6's table, held at the threshold and either side of it, and at a non-default value where the threshold is a policy key |
| `property_more_test.go` | liveness per topology (8-GPU ranks, a prefill role, the ITL line), scale over GPUs per replica and output length, the time-base cases, and the prefill bound through a P/D `Analyze` |
| `property_policy_test.go` | every policy key a learner reads, each of which some case runs at a non-default value |
| `property_coverage_test.go` | what each learner's cases cover and do not, and the classes with no time-base case |
| `testdata/properties/known-failures.yaml` | the known-failures ledger |
| `testdata/properties/cases.txt` | the registered case ids, pinned |

A new case also goes in `cases.txt` and in the coverage table, or the tests
fail. A case that runs a policy key at a value other than its default names
the key in its `keys`, and every key in `policyKeys` needs such a case: 2.6
does not count a key tested at its default alone. A learner that starts
reading a new key adds it to `policyKeys`.

### Violations and errors

A check returns a violation when the learner breaks the property. A violation
has a kind, such as `transient-admitted`, `over-priced` or `not-learned`. Any
other error means the case could not run, and it fails the subtest whatever
the ledger says. Every check first makes sure the learner learned something:
a figure of zero or NaN is a `not-learned` violation, not a pass.

### The known-failures ledger

Some cases fail on today's code on purpose: they record a known defect. Each
one is a row in `known-failures.yaml` with these fields:

| field | what it holds |
| --- | --- |
| `expect` | the violation kind the case fails with |
| `finding` | the defect |
| `measured` | how often it failed, and the first failure, when the row was written |
| `owner` | what clears it: a PR of the plan (`PR 3`), an issue (`#391`), or `open` |

The test is strict both ways:

- A case in the ledger must fail with its `expect` kind. Failing with another
  kind is a new failure.
- A ledgered case that passes on every seed fails the build, so a PR that
  fixes a listed defect also removes the row. (With `PROPSEED` set, a ledgered
  case that passes on that one seed is not an error.)
- A case not in the ledger must pass.
- A row that names no case is an error.

Keep the rows sorted by `id`. If you change a generator, the seeds that fail
change too: update `measured`. Do not add a row to make a new failure go away.
A new failure is a bug, unless the PR names it as a known defect with an
owner.

### Mutants and the 2.7 accounting

`findings` in `property_mutants_test.go` lists every row of 2.7. Each row is
in exactly one of these states:

- **caught**: a mutant puts the defect back, and its class fails; at least
  one of its mutants is not a toy;
- **caught (toy)**: every mutant of the row is a toy (`toy-learner` or
  `toy-output`), which shows the check is sensitive to the defect and not
  that the production learner would be caught;
- **still escaping**: with the reason.

`TestPropertyMutants` runs each mutant, and checks two more rules: a mutant
never runs a ledgered case, and every caught row has a mutant.

Each mutant states its level:

| level | how the defect is put back | what it proves |
| --- | --- | --- |
| `policy` | a policy key set where the guard stops working | that the production learner is caught |
| `rows` | rows changed to look the way the defect read them | that the production learner is caught |
| `stub` | one fix stubbed through a seam the analyzer already has for tests (its `readySince` func) | that the production learner is caught |
| `toy-learner` | a small learner with the pre-fix estimator | only that the check is sensitive to the defect |
| `toy-output` | the production output edited the way the defect edited it | only that the check reads that output |

If a mutant starts to pass, the check no longer detects its defect. Look for
a change that weakened the learner or the case before you touch the
accounting.

### The closed-loop probe

The replay is open loop: recorded inputs do not depend on what the analyzer
ordered. Holds, latches, the sibling yield and a self-sealing low mu are
closed-loop behaviours, and `closed_loop_probe_test.go` drives the
saturation analyzer with a fleet simulator instead: arrivals split over the
Ready replicas, each serves up to its true mu and queues the rest, the
analyzer's order becomes replicas after a 90 s start time, and a release
waits out a 5-minute stabilization. The verdict compares the fleet the loop
settles at with need, the fleet a truthful analyzer orders once every
replica serves the fleet's mean mu, within 15 % plus one replica. It also
fails a fleet that sits below need at the arrival rate of the moment for
more than 5 minutes in one run, an under-order mid-run. Each passing case
asserts its premise from mu's reports: the load step measured mu and priced
it `measured`, and the slow replica's rate was counted `refused-under-served`.

`TestClosedLoopProbe_KnownFailing_*` is a case today's code fails: one slow
replica at 0.55 of its siblings' rate passes the served bound, becomes mu,
and keeps the fleet over-ordered for good once it stops queueing. The test
asserts the probe sees it, so a change that fixes the defect fails it and
turns it into a passing case. A PR that changes a closed-loop behaviour runs
the probe (`go test ./internal/engines/analyzers/saturation/ -run
TestClosedLoopProbe -v`) and pastes the fleets it logs.

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
- **Monitoring images.** Prometheus comes from kube-prometheus-stack
  (`KUBE_PROMETHEUS_STACK_CHART_VERSION`), with Grafana on, because
  `operational_dashboard_test.go` tests the dashboard. No e2e image may come
  from docker.io (AGENTS.md). Every image the chart pulls is on quay.io,
  registry.k8s.io or ghcr.io except Grafana, which Grafana publishes only on
  Docker Hub, so the installer pulls it from `mirror.gcr.io`
  (`GRAFANA_IMAGE_REGISTRY`) with the chart's tag.
  `mirror.gcr.io` is Google's mirror of Docker Hub: the same images, with
  the same digests, served by Google. It is allowed because the rule exists
  for Docker Hub's anonymous rate limit, and the mirror has none. It only
  holds tags someone has pulled through it, though.
  `hack/check-kps-images.sh`, run by `make lint-deploy-scripts`, renders the
  installed release. It fails on any docker.io or unqualified image, and on
  any `mirror.gcr.io` tag the mirror does not serve, so a chart bump fails
  there rather than as ImagePullBackOff in CI.
- **What `make lint-deploy-scripts` needs.** `helm`, `crane` or `docker` (to
  ask the mirror), and network access to the chart repository and the
  mirror. The CI job installs a pinned helm. Offline, point
  `KPS_CHART_DIR=<dir>` at an untarred kube-prometheus-stack chart of the
  pinned version (`helm pull kube-prometheus-stack --repo
  https://prometheus-community.github.io/helm-charts --version <pin>
  --untar`) to skip the chart download. The mirror check still needs the
  network.
- **Runner disk.** A full disk on the CI runner shows up as a Deployment
  that never becomes ready, not as a disk error. Two PRs failed setup on
  `kube-prometheus-stack-grafana not ready ... Progress deadline exceeded`
  while the runner warned `Free space left: 43 MB`. Every kind e2e job
  starts with `deploy/ci-pr-checks/free-disk.sh`, which removes toolchains
  the e2e does not use and logs `df -h /` before and after. It also samples
  free space for the rest of the job, and `disk-report.sh` prints the low
  point at the end. With `KIND_PRUNE_HOST_IMAGES=true` (CI only), the
  install removes the host Docker copy of the simulator and warm pool images
  once the kind nodes hold their own. A grafana, prometheus or controller
  timeout in setup: read those lines first.
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

### Passive-mode specs (own CI job)

Label `passive`, 19 specs in seven containers, about 45 minutes. They check
[passive mode](../reference/scaling-policy.md#passive) against a real KEDA,
HPA and controller. They carry the `full` label, but the full tier's label
filter excludes them: with them it outgrew its 90-minute CI job. They run in
parallel in the CI job `e2e-tests-passive`, which has the same triggers as
`e2e-tests-full` and the same rule for skipping a PR that touches no compiled
file.

```bash
CREATE_CLUSTER=true USE_SIMULATOR=true SCALE_TO_ZERO_ENABLED=true \
  make test-e2e-passive-with-setup
# or, on a cluster from make deploy-e2e-infra with the same two settings:
USE_SIMULATOR=true SCALE_TO_ZERO_ENABLED=true make test-e2e-passive
```

`USE_SIMULATOR=true` and `SCALE_TO_ZERO_ENABLED=true` are required: without
the simulator's `--fake-metrics` or EPP flow control the specs skip. Check the
summary says `Ran 19 of ... Specs` with no skips. The budget is
`E2E_PASSIVE_TIMEOUT=70m` (ginkgo), `E2E_PASSIVE_GO_TIMEOUT=75m` and a
95-minute job: the 19 specs took 43 minutes on a 22-CPU kind host, and setup
about 15.

- **`passive_mode_test.go`**: hold controller-wide (`WVA_PASSIVE=true`) and per
  model, hold at zero under queued requests, a `kubectl scale` that sticks,
  a controller restart and a deleted leader, passive to active (the control)
  and back mid scale-up, observe beside a `kubernetes-workload` trigger,
  observe-unsafe, and two rollouts at 10 replicas.
- **`passive_mode_pool_test.go`**: a utilization-share group holding a passive
  donor, and a warm pool beside a passive model. Each ends by switching the
  model to active, which must start the transfer or the borrow.

Every hold is a 90 s window, three HPA syncs plus the 30 s scale-down window
the specs set on each ScaledObject. A window fails if `spec.replicas` moves
once (it is sampled every second), and also if `wva_desired_replicas` never
differs from the held count, since a window where the controller wanted
nothing proves nothing. They need Prometheus and fail, not skip, without
it. Lines starting `PASSIVE-` in the `-v` output are the evidence: the
replica timeline, the metrics, the HPA's own `desiredReplicas`, and the log
lines.

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
| `ci-pr-checks.yaml` / `e2e-tests-passive` | `make test-e2e-passive-with-setup` with `SCALE_TO_ZERO_ENABLED=true`, in parallel with `e2e-tests-full`; same triggers, and returns early on the same rule | `CREATE_CLUSTER=true USE_SIMULATOR=true SCALE_TO_ZERO_ENABLED=true make test-e2e-passive-with-setup` |
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
