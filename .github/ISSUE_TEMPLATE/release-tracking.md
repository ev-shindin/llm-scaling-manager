---
name: New Release
about: Propose a new release
title: Release vX.Y
labels: kind/release
assignees: ''

---

- [Introduction](#introduction)
- [Prerequisites](#prerequisites)
- [Release Process](#release-process)
- [After the Release](#after-the-release)
- [Announce the Release](#announce-the-release)

## Introduction

This document defines the process for releasing llm-scaling-manager.

## Prerequisites

1. Permissions to push tags to the llm-scaling-manager repository and to publish
   GitHub Releases.

1. Set the version you are releasing:

   ```shell
   export VERSION=v0.9.0-rc1   # a release candidate; v0.9.0 for a final release
   ```

1. If needed, clone the llm-scaling-manager [repo].

   ```shell
   git clone -o origin git@github.com:ev-shindin/llm-scaling-manager.git
   ```

## Release Process

Follow [the release process](https://github.com/ev-shindin/llm-scaling-manager/blob/main/docs/developer-guide/release-process.md).
There is no release branch: only `main` can be released, and the
[CI release workflow] refuses a tag that is not on `main` or whose commit does
not pin `newTag: ${VERSION}`.

- [ ] 1. Release PR to `main`: `newTag: ${VERSION}` in
      `config/base/manager/kustomization.yaml`; merge it.
- [ ] 2. Tag the merge commit of the release PR, check it, push the tag
      ([step 1](https://github.com/ev-shindin/llm-scaling-manager/blob/main/docs/developer-guide/release-process.md#1-push-the-tag)).
- [ ] 3. Wait for the [CI release workflow] to succeed.
- [ ] 4. Reset PR to `main`: `newTag: main` again, only after the run succeeded.
- [ ] 5. Publish the GitHub Release with the release notes ([new release]);
      mark a release candidate as a pre-release. Verify the signature.
- [ ] 6. Docs PR to `main`: point the install, upgrade and benchmark docs at
      `${VERSION}` and its digest
      ([step 5](https://github.com/ev-shindin/llm-scaling-manager/blob/main/docs/developer-guide/release-process.md#5-point-the-docs-at-the-release)).
- [ ] 7. Run the [install guide](https://github.com/ev-shindin/llm-scaling-manager/tree/main/docs/guides/install-in-namespace)
      against the released image.

If the run fails, see
[If the release fails](https://github.com/ev-shindin/llm-scaling-manager/blob/main/docs/developer-guide/release-process.md#if-the-release-fails).
If you find a bug in this process, create an [issue].

## After the Release

Unpin any dependencies that were pinned for the release, and update the
release tracking issue with the final release link.

> Upstream's template also opened a pull request against `llm-d/llm-d` to
> update the autoscaling guides for the new version. That step is not
> available here and has been removed: this is an independent fork with no
> push access to that repository, and it publishes its own guides under
> `docs/guides/`.

## Announce the Release

Use the following steps to announce the release.

1. Generate the announcement email content by running the following block in your terminal (make sure `${VERSION}` is set in your current shell):

   ```shell
   cat <<EOF
   Subject: [ANNOUNCE] llm-scaling-manager ${VERSION} is released

   Hi all,

   We are pleased to announce the release of llm-scaling-manager ${VERSION}!

   ### Container Images
   * Controller: ghcr.io/ev-shindin/llm-scaling-manager:${VERSION}

   ### Release Notes
   For more details, please see the GitHub release notes: https://github.com/ev-shindin/llm-scaling-manager/releases/tag/${VERSION}
   EOF
   ```

1. Copy the generated subject and body and send it wherever this project
   announces releases. (Upstream's template mailed
   `llm-d-contributors@googlegroups.com`; that is their list, not this
   project's, so it is not the default here.)

1. Add a link to the final release in this issue.

1. Close this issue.

[repo]: https://github.com/ev-shindin/llm-scaling-manager
[ghcr registry]: https://github.com/users/ev-shindin/packages?repo_name=llm-scaling-manager
[new release]: https://github.com/ev-shindin/llm-scaling-manager/releases/new
[issue]: https://github.com/ev-shindin/llm-scaling-manager/issues/new/choose
[CI release workflow]: https://github.com/ev-shindin/llm-scaling-manager/actions/workflows/ci-release.yaml
