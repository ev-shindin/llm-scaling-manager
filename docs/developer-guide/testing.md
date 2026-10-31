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
- Measured on the in-process acceptance run: about 1.7 KB per replica per
  cycle for each analyzer a model lists (each `Analyze` carries its input).
  At a 15 s cycle, with saturation alone, 8 replicas fill 256Mi in about 3.4
  days and 100 replicas in about 6.5 hours; with saturation, slo and
  throughput listed, in about 27 hours and 2.1 hours.

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
  `notRun` field in `goldens.yaml`, and the check prints it, for example
  `e1: slo: built, never analyzed (0 Analyze events)`. The check fails
  when a replay disagrees with `notRun`, so a fixture that moves into or
  out of the set is a diff of `goldens.yaml`.
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

The `acceptance` golden is the in-process acceptance run. It covers little
learning: twelve cycles of fixed synthetic rows. The start-time average, the
borrow tolerance and the measured k2 and mu paths barely move in it, so a
change to them can leave it as it is.

The `mu-paths` golden is a second in-process run, for those paths. Its
replicas saturate, so k2 records readings and mu's window takes samples,
each the mean of the replicas read in its minute. Each of mu's three yields
runs on a variant of its own (to the derived figure on a served bound, to
the full siblings, and to the derived figure once stale), and one replica
restarts under its own name. To write it again as a raw recording:

```bash
LEARNREC_MUPATHS_OUT=$HOME/mu-paths.jsonl go test ./internal/engines/steadystate/ -run TestLearnerRecording_TheMuPathsRunReachesEveryLearnerPath -v
```

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
  fix (#288), not the first sight, decided the refusal.

The names are one table in `path_counts.go`; `PathCounts` and `PathNames`
read it.

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

Run `make learner-goldens` only when your PR names the change: it rewrites
every output file from a replay at your commit. Never edit a golden by hand.
`make learner-goldens-check` runs the check alone and prints, per output, how
many rows changed and the first change with its sign and size.

#### Write the acceptance run again

To write the acceptance run as a raw recording:

```bash
LEARNREC_ACCEPTANCE_OUT=$HOME/acceptance.jsonl go test ./internal/engines/steadystate/ -run TestLearnerRecording_AnExportReplaysAndNamesNothing -v
```

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
