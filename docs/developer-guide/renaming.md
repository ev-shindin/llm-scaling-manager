# Renaming the project

`hack/rename` renames the project throughout the tree. It runs once on a later
`main`, after the open pull requests are in. A second run changes nothing.

Nothing is upgraded across this rename. There are no compatibility paths, so the
old names stop working the moment it lands. Test installs are reinstalled.

## Before the rename

Uninstall every test install with the **old** tree, while its names still match
what is on the cluster. The renamed tree looks for the new names and would leave
the old objects behind:

```bash
make undeploy-wva NAMESPACE=<namespace>
```

Learned state, tuned policy, labels on namespaces and ScaledObject addresses are
not carried over. Install again from the renamed tree and re-create what you need.

The tree no longer removes what older installs left behind, because those objects
are not rendered any more and no code looks for them. If you reinstall on a cluster
that had an older install and did not uninstall it with the old tree, delete these
two by hand:

```bash
kubectl delete secret wva-controller-manager-token -n <namespace>
kubectl delete clusterrolebinding wva-prometheus-cluster-monitoring-view-<suffix>
```

## Run order

```bash
# Dry run: writes bin/rename-report.md and changes nothing.
go run ./hack/rename

# Rewrite the tree. Refused while anything is unclassified.
go run ./hack/rename --apply

# Regenerate what the rename left out of date.
LEARNREPLAY_WRITE_MESSAGE_WORDS=1 go test -p 1 -run TestMessageWords_AreCurrent ./internal/engines/analyzers/learnreplay/
make guides-render warmpool-render

# Fail if a run would still change anything, or a hit has no reason.
go run ./hack/rename --expect-noop --sweep
```

The report lists counts and samples per rule and path class, every kept and held
name with its reason, and every occurrence no rule covers. `--apply` needs that
last list empty, or `--allow-unclassified`. Its final sweep looks in every tracked
file and path, in any case, and gives each remaining hit a reason by file type.

Push the renamed tree to `llm-d-incubation/llm-d-scaling-manager`, let CI publish
the images there, then run `go run ./hack/rename --code-moved --apply` and land
that.

## The two switches

- `Targets.Module` in `rules.go` (or `--module`) is the Go module path. A build
  inside the tree does not care that the path does not resolve yet. `go get` and
  `go install` from outside fail until the repository there has a `go.mod`.
- `--code-moved` turns on the held rows: repository links, image references and
  clone commands, which resolve only once the code and images are in the new home.
  Pull request and issue links stay on the old repository, because their numbers
  do not carry over.

## What stays

- Binary and compressed files, symlinks, submodules and untracked files.
- Other projects' names: upstream repository links and issues, other forks,
  `ig-wva`, and text about the other product.
- llm-d-benchmark's own names: its scenario `wva` and `variantAutoscaling` keys, `context.wva_namespace`,
  and the text `patch_harness.sh` anchors on in its source.
- The two old-release learned-state documents, and their writer in the test that
  reads them.
- Images published under the old owner, until they are republished.

## How Go is rewritten

Go goes through its syntax tree, never a pattern over the source. Regular
expressions over Go test fixtures cost this repository a reverted refactor.

- Identifiers map by whole name through `identRules`, so every use of a name
  lands on the same new name.
- Comments, strings, struct tags and import paths each go through the text rules
  in their own context. A struct tag is touched only by a rule that names it.
- Edits are spliced into the original bytes, and gofmt then realigns the columns
  and re-sorts the imports.
- A positional check then holds the Nth token of the result against the Nth
  token of the input, in document order, because fixtures reuse names and a check
  keyed by name would hide a drift. It proves the edit did what the table says and
  nothing else: no token appeared, vanished or moved, and a non-string token is
  unchanged. It does not prove the table is right, because it recomputes each
  string with the same table. Comments are outside it, and imports are compared
  as a set because gofmt sorts them. Whether a renamed string is still correct is
  for the tests to say: they found four fixtures whose expected order changed with
  the name.

Strings in test fixtures take the same rules as production strings, because most
are expected values that have to move with the code they check.

## Length limits

The rendered overlays stay inside every limit: the longest Service name is 50
characters and the longest port name 10.

- **Scaler common name:** an X.509 common name holds 64 characters. The new
  Service name would leave room for a namespace of 28, so the tool shortens the
  common name to `llmsm-external-scaler.<ns>.svc`, which fits a namespace of 38.
  Verification reads the SANs. A tenant's controller namespace takes the short
  form too: `llmsm-<ns>` fits an `<ns>` of 32, where `scaling-manager-<ns>` would
  fit 22.
- **ScaledObject names:** KEDA names the HPA `keda-hpa-<ScaledObject>` and refuses
  one over 63, so a ScaledObject name holds 54. The generated names use the short
  form, so a workload name holds 48 with `<workload>-llmsm`, and a pool name 38 with
  `llmsm-warm-pool-<pool>`. Past that, `llmsm_check_so_name` stops an install or a
  pool creation with the name in the message. A ScaledObject that names its own HPA
  with `spec.advanced.horizontalPodAutoscalerConfig.name` is not bound by this.
