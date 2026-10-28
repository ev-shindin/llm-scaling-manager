# Release Process

How to cut a release of the llm-scaling-manager: what to merge to `main`, how
to tag it, and what to do when the release workflow fails.

## Quick reference

A release from `main`, `-rc` ones included, takes **three PRs to `main`** and
one tag. A patch to an older line follows [Patch releases](#patch-releases)
instead.

| Step | Where | What |
|------|--------|------|
| 1. Release PR | PR to `main` | `newTag: vX.Y.Z`; merge it |
| 2. Tag | Your clone | Tag the merge commit of the release PR, check it, push the tag |
| 3. Wait | GitHub Actions | The release run must succeed |
| 4. Reset PR | PR to `main` | `newTag: main` again, only after the run succeeded |
| 5. Publish | GitHub | Publish the GitHub Release with the release notes; verify the signature |
| 6. Docs PR | PR to `main` | Point the install, upgrade and benchmark docs at the release and its digest |

---

## Scope of a release

1. **A version tag** in this repository, `vMAJOR.MINOR.PATCH` or
   `vMAJOR.MINOR.PATCH-PRERELEASE` (e.g. `v0.6.0`, `v0.6.0-rc1`).
2. **A container image** at `ghcr.io/<repository owner>/llm-scaling-manager:<tag>`,
   i.e. `ghcr.io/ev-shindin/llm-scaling-manager:<tag>` for this repository.
3. **Release notes**, in the GitHub Release. They are not kept as a file in
   this repository.

A tag is released only from `main`, or from its own line's
`release-MAJOR.MINOR` branch (a `v0.10.2` from `release-0.10`) once that line
has a tag on `main`, and only when `newTag` on the tagged commit equals the tag. See [Patch releases](#patch-releases).

---

## Prerequisites

- [ ] You may push tags to this repository and publish GitHub Releases.
- [ ] The GHCR package `llm-scaling-manager` gives this repository **Write**
  access ([first release](#the-first-release-to-a-new-package)).
- [ ] The commit you will tag passed its PR checks, including the e2e runs. The
  release reruns the PR checks, not the e2e suites.

The workflow checks where a tag is, not who reviewed it. Review on `main` and
`release-*` is enforced only by the repository's branch rules, if set.

---

## Pre-release checklist

1. **Open the release PR to `main`.** It sets `newTag: vX.Y.Z` in
   `config/base/manager/kustomization.yaml`. The release fails before it builds
   anything if `newTag` is not the tag.
2. **Bump dependency pins**, if the release moves one (for example
   [llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim)): the
   [Makefile](../../Makefile), `test/e2e/fixtures/model_service_builder.go` and
   `test/utils/resources/llmdsim.go`. Same PR.
3. **Draft the release notes** for the GitHub Release: user-facing and notable
   changes, and the upgrade steps. You paste them when you publish.
4. **Merge the PR, then tag right away.** From the merge until the release run
   pushes the image, an install from `main` pulls `:vX.Y.Z`, which does not
   exist yet.

---

## Creating the release

### 1. Push the tag

Tag the merge commit of the release PR, not the tip of `main` (another PR may
have merged since) and not your local branch. The command checks that the tagged
commit pins the release and is on `main`, and pushes only then.

`REMOTE` is the name of your remote for `ev-shindin/llm-scaling-manager`
(`git remote -v` lists them). In a clone of upstream WVA with this repository
added as a second remote, it is not `origin`.

```bash
REMOTE=<remote for ev-shindin/llm-scaling-manager>
git fetch "$REMOTE"
git tag -m "llm-scaling-manager vX.Y.Z" vX.Y.Z <merge commit of the release PR>
git show vX.Y.Z:config/base/manager/kustomization.yaml | grep -x '  newTag: vX.Y.Z' \
  && git merge-base --is-ancestor vX.Y.Z "$REMOTE/main" && git push "$REMOTE" vX.Y.Z
```

`-m` makes an annotated tag, and with `tag.gpgSign` set a signed one; without
`-m`, a signing setup opens an editor for the tag message. The workflow reads
the commit the tag points at, so a lightweight, annotated or signed tag all
work. Push the tag by name: `git push --tags` would also push any upstream WVA
tags your clone holds.

The push starts [`ci-release.yaml`](../../.github/workflows/ci-release.yaml).
Watch it in the **Actions** tab.

### 2. Wait for the run to succeed

Do not merge anything that changes `newTag` yet. If the run fails, see
[If the release fails](#if-the-release-fails).

### 3. Set `newTag` back to `main`

Once the run has succeeded, open a second PR to `main` that sets `newTag: main`
again, and merge it. Until it merges, an install from `main` deploys the release
image instead of `:main`. Merged earlier, a re-tag after a failed run would fail
the pin check.

### 4. Publish the GitHub Release

In **Releases**, choose the tag, paste the release notes, mark it as a
pre-release if the tag has a prerelease suffix, and publish.

### 5. Point the docs at the release

Open a docs PR to `main` that replaces the previous version with the new one in
the install, upgrade and benchmark docs, pins the new multi-arch index digest
from the run's summary where the docs pin one, and adds the upgrade section for
this release to `docs/reference/upgrade-and-uninstall.md`.

### If the release fails

Find the failed step in the run.

**It failed before "Build and push the multi-arch image".** Nothing was
published.

- A flaky check: open the run and use **Re-run failed jobs**.
- Anything else: delete the tag, fix the problem with a PR to `main` (for a
  patch, to its release branch) that keeps
  `newTag: vX.Y.Z`, tag that PR's merge commit as in step 1, and repeat:
  ```bash
  git push "$REMOTE" --delete vX.Y.Z && git tag -d vX.Y.Z
  ```

**It failed at or after "Build and push the multi-arch image".** An image may
be public under that tag. Do not move or delete the tag.

- The cause is outside the code (for example, the package access below): fix
  it, then use **Re-run failed jobs**. A re-run uses the workflow and the code
  at the tag.
- The fix needs a change to the code or the workflow: release `vX.Y.(Z+1)`
  instead.

---

## Patch releases

An urgent fix for a released line ships from that line's branch while `main`
moves on: `v0.10.2` from `release-0.10`. The workflow releases a tag from a
`release-MAJOR.MINOR` branch only if the tag is of that line and the line
already has a tag on `main` (an `-rc` tag counts).

**Creating a line's branch** (once per line). Start it at the line's latest
release and add the release-branch CI change, which a tag runs from the tagged
commit:

```bash
REMOTE=<remote for ev-shindin/llm-scaling-manager>
git fetch --tags "$REMOTE"
git push "$REMOTE" 'v0.10.1^{commit}:refs/heads/release-0.10'
```

Then open a PR to `release-0.10` that cherry-picks the commit adding
release-branch support to `ci-release.yaml`, `ci-pr-checks.yaml` and
`non-main-gatekeeper.yml`. Merge it first: until it is on the branch, the
branch's tags are refused.

**Each patch:**

1. **Fix on `main` first**, then open a PR to `release-0.10` that cherry-picks
   the merged commit (`git cherry-pick -x <sha>`). If `main` no longer has the
   bug, open the fix against the release branch alone and say why in the PR.
   PRs to `release-*` run the same checks as PRs to `main`.
2. **Release PR to `release-0.10`**, not to `main`. It sets
   `newTag: v0.10.2` and moves every version reference on the branch to
   `v0.10.2`; list them with
   `git grep -n 'v0\.10\.[0-9]' -- docs deploy hack/benchmark`. It adds
   "Upgrading from v0.10.1 to v0.10.2" to
   `docs/reference/upgrade-and-uninstall.md`. Pre-release checklist items 2
   and 3 apply.
3. **Tag its merge commit**, as in [step 1](#1-push-the-tag), with the release
   branch in place of `main`:
   ```bash
   REMOTE=<remote for ev-shindin/llm-scaling-manager>
   git fetch "$REMOTE"
   git tag -m "llm-scaling-manager v0.10.2" v0.10.2 <merge commit>
   git show v0.10.2:config/base/manager/kustomization.yaml | grep -x '  newTag: v0.10.2' \
     && git merge-base --is-ancestor v0.10.2 "$REMOTE/release-0.10" && git push "$REMOTE" v0.10.2
   ```
4. **No reset PR.** The branch keeps the pin; `main` never had it.
5. **Publish** as in [step 4](#4-publish-the-github-release). Untick
   **Set as the latest release** unless the patch is the highest final release.
6. **Docs PR to `main`**: add the upgrade section. Point `main`'s install docs
   at the patch only if it is the highest final release.

If the run fails, recover as in [If the release fails](#if-the-release-fails),
with the fix PR going to the release branch instead of `main`.

The `latest` image tag moves only for the highest final release; a `v0.10.2`
after `v0.11.0` leaves it on `v0.11.0`.

---

## The first release to a new package

The package `llm-scaling-manager` already exists for this repository: it is
public, and this repository has Write access to it. Nothing needs doing unless
that changes, for example after a move to another owner.

The release pushes with the built-in `GITHUB_TOKEN`. No `GHCR_TOKEN` secret is
used. If the repository has no Write access to the package, the run fails at
**Build and push the multi-arch image** with:

```
denied: permission_denied: write_package
```

To fix it:

1. Open the package page: **Packages** on the repository owner's profile, then
   `llm-scaling-manager`.
2. **Package settings** → **Manage Actions access** → **Add repository**.
   Pick this repository and give it **Write**.
3. In the failed run, choose **Re-run failed jobs**. No new tag is needed.

A package GitHub creates on the first push is **private**. Users then get
`401 Unauthorized` when they pull. In **Package settings**, set its visibility
to **Public**.

Make the first release after this workflow lands an `-rc1` tag. It tests the
release run end to end without moving `latest`.

---

## Verifying a release

Replace the owner `ev-shindin` with yours if you release from a fork. The
workflow signs with cosign v3; verify with cosign v3 or later, because cosign v2
reports `no signatures found` for these images. With no cosign installed, run
its image: `docker run --rm ghcr.io/sigstore/cosign/cosign:v3.0.6 verify ...`.

Record the index digest in the release notes, so users can deploy by digest:
`docker buildx imagetools inspect "$IMAGE:vX.Y.Z"` prints it as `Digest:`.

```bash
IMAGE=ghcr.io/ev-shindin/llm-scaling-manager
cosign verify "$IMAGE:vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/ev-shindin/llm-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/vX.Y.Z"

# SBOM and provenance attached to the index
docker buildx imagetools inspect "$IMAGE:vX.Y.Z" --format '{{ json .SBOM }}'
docker buildx imagetools inspect "$IMAGE:vX.Y.Z" --format '{{ json .Provenance }}'
```

Deploy by digest (`IMG=$IMAGE@sha256:...`) when you need the exact bytes that
were verified; a tag can be moved after the fact, a digest cannot.

Installing a release needs no `IMG`: `git checkout vX.Y.Z && make deploy-wva`
deploys `:vX.Y.Z`, pulled `IfNotPresent` because a release tag does not move.

---

## Enabling others to cut releases

Grant them permission to push tags and to publish Releases. No registry secret
needs sharing. Point them at this page.

---

## Details

### What the workflow does

[`ci-release.yaml`](../../.github/workflows/ci-release.yaml) runs once per tag
push. Releases run one at a time. GitHub keeps one run waiting: a third tag
pushed while one runs and one waits cancels the waiting run, which you then
re-run from the **Actions** tab.

1. **Gate.** Calls [`ci-pr-checks.yaml`](../../.github/workflows/ci-pr-checks.yaml)
   on the tagged commit: `golangci-lint`, `make build`, `make test`,
   `make lint-deploy-scripts`, the docs checks and the kustomize builds.
2. **Validate.** Refuses a tag whose commit is on neither `main` nor the tag's
   `release-MAJOR.MINOR` branch, a tag that is not
   `vMAJOR.MINOR.PATCH[-PRERELEASE]`, and a commit whose `newTag` is not the tag.
3. **Scan before push.** Builds the `linux/amd64` image locally and scans it
   with Trivy. A **fixable CRITICAL** vulnerability fails the release. A CRITICAL
   with no upstream fix is reported but does not block. A second, non-blocking
   scan prints every HIGH and CRITICAL finding. The same blocking scan runs in
   the PR checks' `e2e-tests-smoke` job, so a finding fails the PR that brings
   it in before it can fail a tag.
4. **Build and push once.** One `linux/amd64,linux/arm64` build, pushed with an
   SBOM and a max-mode SLSA provenance attestation.
5. **Tags.** Always `:<version>`. `:latest` moves only for the highest final
   release; a prerelease (`-rc1`) never moves it. `hack/check-release-latest.sh`
   tests this.
6. **Sign.** Keyless `cosign sign` of the pushed digest, with the workflow's
   OIDC identity as the signer. The run summary prints the digest and the
   verify command.
