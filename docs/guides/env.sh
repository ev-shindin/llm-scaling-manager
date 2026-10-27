#!/usr/bin/env bash
#
# Shared environment for the guides. Source it before running guide commands:
#
#     source docs/guides/env.sh
#
# Same shape as llm-d's guides/env.sh: versions and image references live here,
# because they change together and no guide should carry its own copy. The
# NAMESPACE deliberately does not — each guide exports the one it installs into,
# exactly as llm-d's guides do.

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
export REPO_ROOT

# The controller image the guides install.
#
# The default is the image these manifests were released with: the newTag in
# config/base/manager/kustomization.yaml, as deploy/install.sh reads it. On main
# that is :main, a floating build of main. On a checkout of a release tag
# (git checkout v0.10.1) it is that release, so the guides install the
# release and not whatever main last pushed. Upstream WVA images do not accept
# --external-scaler-bind-address, which these manifests pass: pointing the
# guides at ghcr.io/llm-d/...:latest gave a CrashLoopBackOff on "unknown flag",
# which reads like a broken image rather than version skew.
#
# Set this to a build of your own tree when you change controller code:
#     make docker-build docker-push IMG=ghcr.io/<you>/<repo>:<tag>
#
# The variable is IMG, because that is what the make targets read. It was
# exported here as WVA_IMAGE, which nothing anywhere reads: a reader testing an
# unmerged branch set it, saw no effect, and got the CrashLoopBackOff described
# above from the published image they thought they had replaced.
#
# To get the same build a week apart, run the guides from a release checkout,
# or pin the digest from the release notes:
#     export IMG=ghcr.io/ev-shindin/llm-scaling-manager@sha256:<digest>
WVA_GUIDE_IMAGE_TAG=$(awk '$1 == "newTag:" {gsub(/["\r]/, "", $2); print $2; exit}' "$REPO_ROOT/config/base/manager/kustomization.yaml" 2>/dev/null)
export IMG="${IMG:-ghcr.io/ev-shindin/llm-scaling-manager:${WVA_GUIDE_IMAGE_TAG:-main}}"
unset WVA_GUIDE_IMAGE_TAG

# The namespace the scaling manager installs into.
#
# Left unset on purpose. The install uses WVA_NS, then NAMESPACE — llm-d's own
# variable — and if neither is set it FINDS the namespace running llm-d model
# servers. So a reader who has followed an llm-d guide already has this right,
# and everyone else can leave it alone and let the preflight report what it found.
#
#   export NAMESPACE=llm-d-optimized-baseline

# KEDA and Prometheus are prerequisites rather than variables. KEDA must already
# be on the cluster (on Kubernetes, KEDA_HELM_INSTALL=true installs it with Helm;
# on OpenShift, use the platform's KEDA). Prometheus is detected (on OpenShift,
# the platform's Thanos Querier at a fixed address); PROMETHEUS_URL overrides that.
