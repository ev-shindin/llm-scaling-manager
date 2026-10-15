#!/usr/bin/env bash
# Installs the Kueue CONTROLLER -- CRDs, webhooks, the manager Deployment --
# from the pinned release manifest, for the live-Kueue e2e (kueue_borrow_test.go).
#
# Its sibling install-kueue-crds.sh installs the three CRDs only, which is all
# the quota-reader spec (kueue_quota_test.go) needs: that reader lists objects
# and never talks to Kueue. The borrowing spec needs Kueue itself -- admission,
# ClusterQueue status, cohort borrowing and reclaim preemption are what it
# checks the reader against. The Makefile picks one of the two with
# KUEUE_CONTROLLER (default true); the quota-reader spec passes against either.
#
# A throwaway kind cluster is the only target:
#   * Kueue already running in kueue-system at KUEUE_VERSION: re-checked, not
#     re-applied (the run is idempotent).
#   * Kueue running at another version, or a Kueue Deployment in any other
#     namespace (an operator-managed install): refused. A server-side apply of
#     a pinned release over somebody else's Kueue would be an up- or downgrade
#     they did not ask for.
#   * CRDs present with no controller (install-kueue-crds.sh ran first): the
#     release is applied over them. Those CRDs came from this repo's script on
#     this throwaway cluster, under the same field manager.
#
# KUEUE_WEBHOOK_NAMESPACE_LABEL, when set, narrows Kueue's job-integration
# webhooks (pods, Deployments, StatefulSets, LeaderWorkerSets, Jobs, ...) to
# namespaces carrying that label. Upstream they cover every namespace but
# kube-system and kueue-system with failurePolicy Fail, so a Kueue manager that
# is restarting or slow fails pod creation cluster-wide -- in every other e2e
# spec too. The borrowing spec labels the namespaces it creates; nothing else
# goes through Kueue. Webhooks whose namespaceSelector is absent or empty (the
# cluster-scoped kinds: ClusterQueue, ResourceFlavor, ...) are left as they are.
set -euo pipefail

KUEUE_VERSION="${KUEUE_VERSION:-v0.20.0}"
KUEUE_NAMESPACE=kueue-system
KUEUE_DEPLOYMENT=kueue-controller-manager
KUEUE_IMAGE="registry.k8s.io/kueue/kueue:${KUEUE_VERSION}"
MANIFEST_URL="https://github.com/kubernetes-sigs/kueue/releases/download/${KUEUE_VERSION}/manifests.yaml"
ROLLOUT_TIMEOUT="${KUEUE_ROLLOUT_TIMEOUT:-300s}"
WEBHOOK_TIMEOUT_SECONDS="${KUEUE_WEBHOOK_TIMEOUT_SECONDS:-180}"
WEBHOOK_LABEL="${KUEUE_WEBHOOK_NAMESPACE_LABEL:-}"

die() { echo "install-kueue: $*" >&2; exit 1; }

# Any Kueue manager elsewhere is somebody else's install.
foreign="$(kubectl get deploy -A -l app.kubernetes.io/name=kueue,control-plane=controller-manager \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | grep -v "^${KUEUE_NAMESPACE}/${KUEUE_DEPLOYMENT}$" || true)"
if [ -n "$foreign" ]; then
    die "a Kueue controller is already installed outside ${KUEUE_NAMESPACE} (${foreign}); refusing to install a second one"
fi

current=""
if kubectl -n "$KUEUE_NAMESPACE" get deploy "$KUEUE_DEPLOYMENT" >/dev/null 2>&1; then
    current="$(kubectl -n "$KUEUE_NAMESPACE" get deploy "$KUEUE_DEPLOYMENT" \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')"
fi

if [ -z "$current" ]; then
    echo "Installing Kueue ${KUEUE_VERSION} (${MANIFEST_URL})"
    # Server-side: the CRD schemas are far past the client-side annotation limit.
    kubectl apply --server-side -f "$MANIFEST_URL" >/dev/null
elif [ "$current" = "$KUEUE_IMAGE" ]; then
    echo "Kueue ${KUEUE_VERSION} already installed; checking it is ready"
else
    die "Kueue is already installed with image ${current}, not ${KUEUE_IMAGE}; refusing to change its version"
fi

kubectl -n "$KUEUE_NAMESPACE" rollout status "deploy/${KUEUE_DEPLOYMENT}" --timeout="$ROLLOUT_TIMEOUT" \
    || { kubectl -n "$KUEUE_NAMESPACE" get pods -o wide >&2 || true
         kubectl -n "$KUEUE_NAMESPACE" logs "deploy/${KUEUE_DEPLOYMENT}" --tail=50 >&2 || true
         die "the Kueue controller did not become available within ${ROLLOUT_TIMEOUT}"; }

if [ -n "$WEBHOOK_LABEL" ]; then
    # The added term is an Exists match on the label; it is added once, so a
    # re-run leaves the configuration as it is.
    for kind in mutatingwebhookconfiguration validatingwebhookconfiguration; do
        name="kueue-${kind%webhookconfiguration}-webhook-configuration"
        for attempt in 1 2 3 4 5; do
            kubectl get "$kind" "$name" -o json \
                | jq --arg label "$WEBHOOK_LABEL" '
                    .webhooks |= map(
                      if ((.namespaceSelector.matchExpressions // []) | length) == 0
                         and ((.namespaceSelector.matchLabels // {}) | length) == 0 then .
                      elif ([.namespaceSelector.matchExpressions[]? | select(.key == $label)] | length) > 0 then .
                      else .namespaceSelector.matchExpressions += [{"key": $label, "operator": "Exists"}]
                      end)' \
                | kubectl replace -f - >/dev/null && break
            # The manager rotates its certificate into the caBundle; a replace
            # that raced it is a conflict, and the next attempt re-reads.
            [ "$attempt" -lt 5 ] || die "could not narrow ${kind} ${name} to namespaces labelled ${WEBHOOK_LABEL}"
            sleep 2
        done
    done
    echo "Kueue job webhooks narrowed to namespaces labelled ${WEBHOOK_LABEL}"
fi

# An available Deployment is not a serving webhook: the certificate is rotated
# in after the pod starts, and until then every Kueue object create fails. Probe
# with a server-side dry-run of a ResourceFlavor -- it goes through the webhook
# and persists nothing.
probe="$(mktemp)"
trap 'rm -f "$probe" "$probe.err"' EXIT
printf '%s\n' 'apiVersion: kueue.x-k8s.io/v1beta2' 'kind: ResourceFlavor' 'metadata:' \
    '  name: install-kueue-webhook-probe' > "$probe"
deadline=$(( $(date +%s) + WEBHOOK_TIMEOUT_SECONDS ))
until kubectl create --dry-run=server -f "$probe" >/dev/null 2>"$probe.err"; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
        cat "$probe.err" >&2
        die "the Kueue webhook did not answer within ${WEBHOOK_TIMEOUT_SECONDS}s"
    fi
    sleep 2
done

served="$(kubectl get crd clusterqueues.kueue.x-k8s.io \
    -o jsonpath='{range .spec.versions[?(@.served==true)]}{.name} {end}')"
image="$(kubectl -n "$KUEUE_NAMESPACE" get deploy "$KUEUE_DEPLOYMENT" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')"
echo "Kueue controller ready: image ${image}; ClusterQueue versions served: ${served}"
