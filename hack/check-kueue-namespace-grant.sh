#!/usr/bin/env bash
#
# Executes the preflight that refuses a controller-only install whose controller
# could not read the Namespace a Kueue quota needs, against a stubbed kubectl.
#
# The Kueue reader GETs each LocalQueue's Namespace to evaluate a ClusterQueue
# namespaceSelector. A kueue-reader role created before that read existed lacks
# it, an UPGRADE in the controller-only phase (INSTALL_PHASE=wva) does not touch
# it, and the new controller then bounds the namespace at nothing -- it cannot
# scale up. Nothing about that parses or applies differently, so `bash -n` and a
# green install both miss it.
#
# WRITING A CASE HERE: a function that does not exist refuses nothing. The
# guard below refuses to run unless it is defined, and EXPECT_CASES refuses a run
# that ended early.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v yq >/dev/null 2>&1 || { printf 'FATAL: yq is required.\n' >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh prereqs.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/prereqs.sh"
for fn in wva_check_kueue_namespace_read wva_kueue_source_configured check_permissions; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done

EXPECT_CASES=33
RAN=0
FAILED=0

check() {
    if [ "$2" = "$3" ]; then
        RAN=$((RAN + 1))
        printf 'ok    %s\n' "$1"
    else
        FAILED=$((FAILED + 1))
        printf 'FAIL  %s\n        want [%s]\n        got  [%s]\n' "$1" "$2" "$3" >&2
    fi
}

POLICY_WITH_KUEUE='limiters:
- name: q
  type: quota
  kueue:
    enabled: true'

# stub_kubectl answers from the STUB_* variables below. Errors go to stderr,
# where the API server puts them, and STUB_WARN=1 adds a warning there on every
# call while still SUCCEEDING -- what a deprecated API or a webhook does. A
# warning is never data: no verdict may change with it.
stub_kubectl() {
    echo "$*" >> "$WORK/kubectl.log"
    [ -z "${STUB_WARN:-}" ] || echo 'Warning: the kubectl client is older than the server' >&2
    case "$*" in
        "auth can-i create "*)
            # check_permissions: the caller's own create rights.
            case " ${STUB_DENY_CREATE:-} " in
                *" $4 "*) echo no; return 1 ;;
            esac
            echo yes; return 0
            ;;
        "auth can-i "*)
            case "${STUB_CANI:-no}" in
                yes) echo yes; return 0 ;;
                no) echo no; return 1 ;;
                *) echo 'Error from server (Forbidden): users "x" is forbidden: cannot impersonate' >&2; return 1 ;;
            esac
            ;;
        "api-resources "*)
            printf '%s\n' \
                'configmaps cm v1 true ConfigMap' \
                'services svc v1 true Service' \
                'serviceaccounts sa v1 true ServiceAccount' \
                'secrets  v1 true Secret' \
                'deployments deploy apps/v1 true Deployment' \
                'networkpolicies netpol networking.k8s.io/v1 true NetworkPolicy' \
                'roles  rbac.authorization.k8s.io/v1 true Role' \
                'rolebindings  rbac.authorization.k8s.io/v1 true RoleBinding' \
                'clusterroles  rbac.authorization.k8s.io/v1 false ClusterRole' \
                'clusterrolebindings  rbac.authorization.k8s.io/v1 false ClusterRoleBinding'
            ;;
        "get namespace "*)
            case "${STUB_LABEL:-}" in
                FORBIDDEN) echo 'Error from server (Forbidden): namespaces is forbidden' >&2; return 1 ;;
                *) printf '%s' "${STUB_LABEL:-}" ;;
            esac
            ;;
        "get configmap wva-scaling-policy-config -n "*)
            local cns="${5:-}" body=""
            case " ${STUB_CM_FORBIDDEN:-} " in
                *" $cns "*) echo 'Error from server (Forbidden): configmaps is forbidden' >&2; return 1 ;;
            esac
            case "$cns" in
                wva-policy) body="${STUB_POLICY:-}" ;;
                "${STUB_LABEL:-none}") body="${STUB_LABELLED_POLICY:-}" ;;
                "$WVA_NS") body="${STUB_OWN:-}" ;;
            esac
            [ -n "$body" ] || { echo 'Error from server (NotFound): configmaps "wva-scaling-policy-config" not found' >&2; return 1; }
            printf '%s\n' "$body" ;;
        *) return 0 ;;
    esac
}

# run_check [VAR=value...] runs wva_check_kueue_namespace_read and echoes
# REFUSED, WARNED or OK, then the can-i question asked ("-" for none).
#   STUB_CANI      yes | no | unknown (impersonation denied)
#   STUB_POLICY    the cluster policy default entry in wva-policy ("" = none)
#   STUB_OWN       the default entry of the controller's own ConfigMap ("" = none)
#   STUB_LABEL     the managed Namespace's policy-namespace label, or FORBIDDEN
#   STUB_LABELLED_POLICY  the default entry in the namespace that label names
#   STUB_CM_FORBIDDEN     namespaces whose policy ConfigMap read is Forbidden
run_check() {
    : > "$WORK/kubectl.log"
    : > "$WORK/out"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { echo "WARNED" >> "$WORK/out"; }
        log_error() { echo "REFUSED" >> "$WORK/out"; printf '%s\n' "$1" > "$WORK/refusal"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        WVA_NS=wva-ctl WVA_WATCH_NS='' WVA_SCOPE=namespace INSTALL_PHASE=wva
        WVA_LIMITER=quota WVA_QUOTA_KUEUE=true STUB_CANI=no STUB_POLICY=''
        local kv
        for kv in "$@"; do eval "$kv"; done
        wva_check_kueue_namespace_read
    )
    local verdict asked
    verdict="$(head -1 "$WORK/out")"
    asked="$(grep '^auth can-i ' "$WORK/kubectl.log" | head -1 | awk '{print $3, $4}')"
    printf '%s %s' "${verdict:-OK}" "${asked:--}"
}

check 'upgrade, Kueue configured, Namespace get denied -> refused' \
    'REFUSED get namespaces/wva-ctl' "$(run_check)"
check 'the refusal names the remedy' '1' \
    "$(run_check >/dev/null; grep -c 'make setup-prereqs' "$WORK/refusal")"
check 'granted -> proceeds' \
    'OK get namespaces/wva-ctl' "$(run_check STUB_CANI=yes)"
check 'WVA_WATCH_NS: the managed namespace is the one asked about' \
    'REFUSED get namespaces/tenant-a' "$(run_check WVA_WATCH_NS=tenant-a)"
check 'cluster scope asks for the unrestricted read' \
    'REFUSED get namespaces' "$(run_check WVA_SCOPE=cluster)"
check 'impersonation denied -> warned, not refused' \
    'WARNED get namespaces/wva-ctl' "$(run_check STUB_CANI=unknown)"
# The prereqs and all phases APPLY the role that grants it.
check 'INSTALL_PHASE=all is not refused (its apply grants the read)' \
    'OK -' "$(run_check INSTALL_PHASE=all)"
check 'no Kueue source -> nothing asked' \
    'OK -' "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false)"
check 'WVA_QUOTA_KUEUE without the quota limiter -> nothing asked' \
    'OK -' "$(run_check WVA_LIMITER=gpu-inventory)"
check 'a cluster policy with kueue.enabled is a configured source' \
    'REFUSED get namespaces/wva-ctl' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false "STUB_POLICY='$POLICY_WITH_KUEUE'")"
check 'a cluster policy without kueue is not' \
    'OK -' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false "STUB_POLICY='limiters: [{name: g, type: gpu-inventory}]'")"

# The policy the controller ACTUALLY reads, resolved the way the controller
# resolves it (pl_policy_resolution), and Forbidden is unknown, not none.
check 'its own ConfigMap with kueue is a source: an upgrade need not repeat WVA_QUOTA_KUEUE' \
    'REFUSED get namespaces/wva-ctl' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false "STUB_OWN='$POLICY_WITH_KUEUE'")"
check 'admin-owned (manages another namespace): kueue only in wva-policy, which it never reads -> nothing asked' \
    'OK -' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false WVA_WATCH_NS=tenant-a "STUB_POLICY='$POLICY_WITH_KUEUE'")"
check 'a label-named policy namespace with kueue is a source' \
    'REFUSED get namespaces/wva-ctl' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_LABEL=platform-policy "STUB_LABELLED_POLICY='$POLICY_WITH_KUEUE'")"
check 'an unreadable policy is unknown: warned, not silently no source' \
    'WARNED get namespaces/wva-ctl' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_CM_FORBIDDEN=wva-policy)"
check 'unknown source, read already granted -> quiet' \
    'OK get namespaces/wva-ctl' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_CM_FORBIDDEN=wva-policy STUB_CANI=yes)"

# A warning kubectl prints on stderr while succeeding is not data. Merged into
# the answer with 2>&1, it became the label ("Warning: ..."), and a `yes` that
# starts with "Warning" was no longer a yes.
check 'stderr warning: the label still names the policy namespace' \
    'REFUSED get namespaces/wva-ctl' \
    "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_WARN=1 STUB_LABEL=platform-policy "STUB_LABELLED_POLICY='$POLICY_WITH_KUEUE'")"
check 'stderr warning: a granted read is still granted' \
    'OK get namespaces/wva-ctl' "$(run_check STUB_WARN=1 STUB_CANI=yes)"
check 'stderr warning: a NotFound policy is still no source' \
    'OK -' "$(run_check WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_WARN=1)"

# --- check_permissions in the controller-only phase asks only for what that
# phase applies.
#
# NetworkPolicy/wva-external-scaler is the admin's by NAME (WVA_PREREQ_NAMED)
# and the render's only NetworkPolicy. Filtering by kind name left
# NetworkPolicy in the tenant's list, so INSTALL_PHASE=wva and --check
# demanded `create networkpolicies` of a tenant for an object that phase never
# applies. The render is stubbed to the shapes that matter: a tenant ConfigMap
# beside the admin's named one, the admin's NetworkPolicy, and admin kinds.
RENDER_FIXTURE='apiVersion: v1
kind: ConfigMap
metadata: {name: wva-manager-config}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: wva-metrics-server-ca}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: wva-controller-manager}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: wva-external-scaler}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: wva-manager-role}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: wva-leader-election-role}'
# run_perm <INSTALL_PHASE> [VAR=value...]: echoes the create rights asked for
# (sorted), then REFUSED if check_permissions refused on a create right.
run_perm() {
    local phase="$1"
    shift
    : > "$WORK/kubectl.log"
    : > "$WORK/refusal"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { :; }
        log_error() { printf '%s\n' "$1" > "$WORK/refusal"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        wva_render_manifests() { printf '%s\n' "$RENDER_FIXTURE"; }
        WVA_NS=wva-ctl WVA_WATCH_NS='' WVA_SCOPE=namespace INSTALL_PHASE="$phase"
        WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_CANI=yes STUB_DENY_CREATE=networkpolicies
        local kv
        for kv in "$@"; do eval "$kv"; done
        check_permissions
    ) >/dev/null 2>&1
    printf '%s' "$(grep '^auth can-i create ' "$WORK/kubectl.log" | awk '{print $4}' | sort -u | tr '\n' ' ')"
    grep -q 'You cannot create everything' "$WORK/refusal" && printf 'REFUSED'
}
check 'INSTALL_PHASE=wva: no create networkpolicies asked, not refused' \
    'configmaps deployments ' "$(run_perm wva)"
check 'INSTALL_PHASE=all still asks it, and a denial refuses (the stub can tell)' \
    'clusterroles configmaps deployments networkpolicies roles REFUSED' "$(run_perm all)"
# A render that comes back empty falls back to a fixed kind list. That list
# carried the admin kinds (Role, RoleBinding, Secret, ServiceAccount,
# ServiceMonitor) into the controller-only phase, and a tenant denied them was
# refused. ServiceMonitor is not served by the stub, so it is never asked.
ADMIN_KINDS_DENIED='roles rolebindings secrets serviceaccounts'
check 'unrendered fallback, INSTALL_PHASE=wva: only the tenant kinds asked, not refused' \
    'configmaps deployments services ' \
    "$(run_perm wva RENDER_FIXTURE= "STUB_DENY_CREATE='$ADMIN_KINDS_DENIED'")"
check 'unrendered fallback, INSTALL_PHASE=all: the admin kinds still asked, a denial refuses' \
    'configmaps deployments rolebindings roles secrets serviceaccounts services REFUSED' \
    "$(run_perm all RENDER_FIXTURE= "STUB_DENY_CREATE='$ADMIN_KINDS_DENIED'")"
# WVA_LIMITER is a list: one naming quota beside gpu-inventory still reads Kueue.
check 'a list naming quota with Kueue is a configured source' \
    'REFUSED get namespaces/wva-ctl' "$(run_check WVA_LIMITER=gpu-inventory,quota)"

# The physical limiter needs node read, which only the admin setup phase grants.
# Composed with a quota, a controller that cannot list nodes is refused with
# that remedy, rather than installed into NotReady.
# run_node_check [VAR=value...] echoes REFUSED, WARNED or OK for
# wva_check_limiter_node_read over the set wva_limiter_set resolves.
run_node_check() {
    : > "$WORK/out"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { echo "WARNED" >> "$WORK/out"; }
        log_error() { echo "REFUSED" >> "$WORK/out"; printf '%s\n' "$1" > "$WORK/refusal"; exit 1; }
        kubectl() {
            case "${STUB_CANI:-no}" in
                yes) echo yes; return 0 ;;
                no) echo no; return 1 ;;
                *) echo 'Error from server (Forbidden): cannot impersonate'; return 1 ;;
            esac
        }
        WVA_SCOPE=cluster WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_CANI=no
        local kv
        for kv in "$@"; do eval "$kv"; done
        wva_check_limiter_node_read "$(wva_limiter_set)" "system:serviceaccount:wva-ctl:wva-controller-manager"
    )
    printf '%s' "$(head -1 "$WORK/out")"
    [ -s "$WORK/out" ] || printf 'OK'
}
check 'gpu-inventory,quota without node read -> refused' \
    'REFUSED' "$(run_node_check WVA_LIMITER=gpu-inventory,quota)"
check 'the node-read refusal names make setup-prereqs' '1' \
    "$(run_node_check WVA_LIMITER=gpu-inventory,quota >/dev/null; grep -c 'make setup-prereqs' "$WORK/refusal")"
check 'a Kueue quota alone adds no physical limiter: only a warning' \
    'WARNED' "$(run_node_check WVA_LIMITER=quota WVA_QUOTA_KUEUE=true)"
check 'gpu-inventory,quota with node read -> proceeds' \
    'OK' "$(run_node_check WVA_LIMITER=gpu-inventory,quota STUB_CANI=yes)"
check 'a named composed set without node read says the physical limiter would be skipped' '1' \
    "$(run_node_check WVA_LIMITER=gpu-inventory,quota >/dev/null; grep -c 'SKIPS the physical limiter' "$WORK/refusal")"

# WVA_LIMITER unset keeps the LIVE policy's limiters, so the node read is
# checked against them (check_permissions passes the live set). An upgrade
# over a live gpu-inventory,quota policy without node read used to read
# "no limiter is configured"; it is warned, with the remedy, not refused.
run_live_node_check() {
    : > "$WORK/out"; : > "$WORK/warning"
    (
        log_info() { :; }
        log_success() { :; }
        log_warning() { echo "WARNED" >> "$WORK/out"; printf '%s\n' "$1" >> "$WORK/warning"; }
        log_error() { echo "REFUSED" >> "$WORK/out"; exit 1; }
        kubectl() { echo no; return 1; }
        WVA_NS=wva-ctl WVA_SCOPE=cluster
        wva_check_limiter_node_read "$1" "system:serviceaccount:wva-ctl:wva-controller-manager" live
    )
    printf '%s' "$(head -1 "$WORK/out")"
    [ -s "$WORK/out" ] || printf 'OK'
}
check 'a live gpu-inventory,quota policy without node read -> warned, not refused' \
    'WARNED' "$(run_live_node_check 'gpu-inventory quota')"
check 'the live warning names make setup-prereqs and the physical skip' '2' \
    "$(run_live_node_check 'gpu-inventory quota' >/dev/null; grep -c 'setup-prereqs\|SKIPS the physical limiter' "$WORK/warning")"

# Wired into check_permissions -- which --check and the install both run --
# BEFORE the tenant early-return, or the namespace-scoped default never reaches it.
body="$(tr -d '\r' < "$ROOT/deploy/lib/prereqs.sh" | sed -n '/^check_permissions() {/,/^}/p')"
call_line="$(printf '%s\n' "$body" | grep -n '^[[:space:]]*wva_check_kueue_namespace_read$' | head -1 | cut -d: -f1)"
tenant_line="$(printf '%s\n' "$body" | grep -n 'if wva_scope_is_tenant; then' | tail -1 | cut -d: -f1)"
check 'check_permissions calls it' 'yes' "$([ -n "$call_line" ] && echo yes || echo no)"
check 'before the tenant early-return' 'yes' \
    "$([ -n "$call_line" ] && [ -n "$tenant_line" ] && [ "$call_line" -lt "$tenant_line" ] && echo yes || echo no)"

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d Kueue namespace-grant assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nKueue namespace-grant checks passed (%d cases).\n' "$RAN"
