# Contributing to the scaling manager

Thanks for contributing to llm-scaling-manager.

Issues and pull requests go to
[github.com/ev-shindin/llm-scaling-manager](https://github.com/ev-shindin/llm-scaling-manager).

## Setting Up

### Prerequisites

- Go 1.25.0+
- Docker, Make, git
- kubectl 1.24+ (the installer refuses older), or `oc` on OpenShift
- Helm 3.8+
- Kind, for a local cluster
- `jq` 1.6+ and `yq` v4 (mikefarah's Go yq; CI pins v4.44.3)
- `python3` with PyYAML, for the doc and dashboard checks

### Set up your environment

```bash
# Clone your fork
git clone https://github.com/<your-username>/llm-scaling-manager.git
cd llm-scaling-manager

# Add this project (not llm-d) as upstream
git remote add upstream https://github.com/ev-shindin/llm-scaling-manager.git

# Download Go modules
go mod download

# Create a Kind cluster with emulated GPUs and deploy the infra
CREATE_CLUSTER=true make deploy-e2e-infra

# Run the unit tests
make test
```

The [Developer Guide](docs/developer-guide/development.md) has the full setup
and a [package map](docs/developer-guide/development.md#project-structure).

## Commits

Every commit needs two things; CI checks the second:

- **A DCO sign-off.** Commit with `git commit -s`. It adds a `Signed-off-by:`
  line with your git name and email. Do not type the line by hand; it must
  match the commit's author.
- **A signature.** The `signed-commits` check
  (`.github/workflows/ci-signed-commits.yaml`) fails a PR with any commit that
  GitHub cannot verify. Set up [GPG or SSH commit signing](https://docs.github.com/en/authentication/managing-commit-signature-verification)
  and commit with `git commit -S -s` (or set `commit.gpgsign=true`).

Commit messages follow [conventional commits](https://www.conventionalcommits.org/).

## Testing Your Changes

```bash
# Unit tests
make test

# Lint, and fix what the linter can
make lint
make lint-fix

# E2E on Kind (smoke tier, then full tier)
make test-e2e-smoke
make test-e2e-full

# E2E on OpenShift (KUBECONFIG set)
ENVIRONMENT=openshift make test-e2e-smoke

# One spec by name
FOCUS="Basic Autoscaling" make test-e2e-smoke
```

A `FOCUS` that matches nothing runs 0 specs and still passes. Check that the
`Ran N of M Specs` line has N above 0.

What CI runs, and how to run each job locally:
[Testing: Continuous Integration](docs/developer-guide/testing.md#continuous-integration).

### Modifying generated manifests

The project installs no CustomResourceDefinition; a KEDA ScaledObject registers
a workload, so there is no API type to version. The only generated file is the
manager's ClusterRole:

```bash
# After changing +kubebuilder:rbac markers; rewrites config/base/rbac/manager-clusterrole.yaml
make manifests
git diff config/base/rbac
```

`internal/variant/deepcopy.go` is hand-maintained. No type has a
`+kubebuilder:object` marker, so `make generate` produces nothing; update
`deepcopy.go` by hand when you change a struct in `internal/variant`.

### Building and Deploying

```bash
# Build the binary
make build

# Run the controller on your machine against the KUBECONFIG cluster
make run

# Build an image
make docker-build IMG=<your-registry>/wva-controller:tag

# Deploy it (Kubernetes, or OpenShift)
make deploy-wva-on-k8s IMG=<your-registry>/wva-controller:tag
make deploy-wva-on-openshift IMG=<your-registry>/wva-controller:tag

# Deploy it with the e2e infra (EPP, Prometheus, KEDA)
make deploy-e2e-infra IMG=<your-registry>/wva-controller:tag
```

A controller run with `make run` gets no KEDA calls, so it manages nothing until
a ScaledObject points at it; see [Debugging](docs/developer-guide/debugging.md).

### Debugging

```bash
# Controller logs
kubectl logs -n "$WVA_NS" deploy/wva-controller-manager -f

# What KEDA was told
kubectl describe scaledobject <name> -n <namespace>

# The HPA KEDA drives from the decision
kubectl describe hpa -n <namespace> keda-hpa-<scaledobject-name>
```

### Cleaning Up

```bash
# Delete the Kind cluster
make destroy-kind-cluster

# Remove the install (pass the ENVIRONMENT, SCOPE and namespace you installed with)
make undeploy-wva
```

## Documentation

[AGENTS.md](AGENTS.md#documentation) says where a page goes. CI enforces three
conventions:

- **Guides are generated.** The bash blocks in `docs/guides/<task>/README.md`
  come from its `guide.yaml`. Edit the YAML, run `make guides-render`, then
  `make guides-check` (which also checks every doc link).
- **Config file names** under `config/` follow `(<app>-)?<kind>.yaml`, e.g.
  `manager-clusterrole.yaml`.
- **Shell checks are fixtures.** A new check is a `hack/check-*.sh` wired into
  `make lint-deploy-scripts`.

## Code Guidelines

- Log through `ctrl.Log` (see [AGENTS.md](AGENTS.md)); verbosity levels are in
  `internal/logging`.
- Use backoff retries for Kubernetes API calls (`GetResourceWithBackoff` in
  `internal/resources`).
- Emit metrics for what an operator needs to see.
- **Analyzers** (`internal/engines/analyzers/`): keep the math correct, test it
  thoroughly, check it against real workload data when you can, and document
  assumptions and limits.
- **Optimizers** (`internal/engines/allocation/`): mind computational cost,
  test edge cases (zero load, overload), check feasibility, and document the
  algorithm choice.

## Pre-Submission Checklist

- [ ] `make test` passes
- [ ] `make lint` passes
- [ ] `make test-e2e-smoke` passes if controller logic changed (`make test-e2e-full` for the full tier)
- [ ] Docs updated for user-facing changes
- [ ] Every commit is signed off (`-s`) and signed
- [ ] Commit messages follow [conventional commits](https://www.conventionalcommits.org/)
- [ ] The PR description explains the change

For help, read the [Developer Guide](docs/developer-guide/development.md) or
search [GitHub Issues](https://github.com/ev-shindin/llm-scaling-manager/issues).

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
