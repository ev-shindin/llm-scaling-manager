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
for fn in wva_check_kueue_namespace_read wva_check_kueue_cohort_read wva_kueue_source_configured check_permissions; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done

EXPECT_CASES=31
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

# run_check [VAR=value...] runs wva_check_kueue_namespace_read (or CHECK_FN) and echoes
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
        kubectl() {
            echo "$*" >> "$WORK/kubectl.log"
            case "$*" in
                "auth can-i "*)
                    case "${STUB_CANI:-no}" in
                        yes) echo yes; return 0 ;;
                        no) echo no; return 1 ;;
                        *) echo 'Error from server (Forbidden): users "x" is forbidden: cannot impersonate'; return 1 ;;
                    esac
                    ;;
                "get namespace "*)
                    case "${STUB_LABEL:-}" in
                        FORBIDDEN) echo 'Error from server (Forbidden): namespaces is forbidden'; return 1 ;;
                        *) printf '%s' "${STUB_LABEL:-}" ;;
                    esac
                    ;;
                "get configmap wva-scaling-policy-config -n "*)
                    local cns="${5:-}" body=""
                    case " ${STUB_CM_FORBIDDEN:-} " in
                        *" $cns "*) echo 'Error from server (Forbidden): configmaps is forbidden'; return 1 ;;
                    esac
                    case "$cns" in
                        wva-policy) body="${STUB_POLICY:-}" ;;
                        "${STUB_LABEL:-none}") body="${STUB_LABELLED_POLICY:-}" ;;
                        "$WVA_NS") body="${STUB_OWN:-}" ;;
                    esac
                    [ -n "$body" ] || { echo 'Error from server (NotFound): configmaps "wva-scaling-policy-config" not found'; return 1; }
                    printf '%s\n' "$body" ;;
                *) return 0 ;;
            esac
        }
        WVA_NS=wva-ctl WVA_WATCH_NS='' WVA_SCOPE=namespace INSTALL_PHASE=wva
        WVA_LIMITER=quota WVA_QUOTA_KUEUE=true STUB_CANI=no STUB_POLICY=''
        local kv
        for kv in "$@"; do eval "$kv"; done
        "${CHECK_FN:-wva_check_kueue_namespace_read}"
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

# WVA_LIMITER is a list: one naming quota beside gpu-inventory still reads Kueue.
check 'a list naming quota with Kueue is a configured source' \
    'REFUSED get namespaces/wva-ctl' "$(run_check WVA_LIMITER=gpu-inventory,quota)"

# The physical limiter needs node read, which only the admin setup phase grants.
# Composed with a quota -- named, or added beside a Kueue-bounded quota on a
# cluster-scoped install -- a controller that cannot list nodes is refused with
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
check 'a Kueue quota on a cluster-scoped install adds gpu-inventory, so node read is required' \
    'REFUSED' "$(run_node_check WVA_LIMITER=quota WVA_QUOTA_KUEUE=true)"
check 'WVA_LIMITER_PHYSICAL=false leaves it out: only a warning' \
    'WARNED' "$(run_node_check WVA_LIMITER=quota WVA_QUOTA_KUEUE=true WVA_LIMITER_PHYSICAL=false)"
check 'gpu-inventory,quota with node read -> proceeds' \
    'OK' "$(run_node_check WVA_LIMITER=gpu-inventory,quota STUB_CANI=yes)"
# The Cohort list (borrowing). Missing it only stops borrowing from being
# counted, so it warns and never refuses.
check 'cohort list denied -> warned, not refused' \
    'WARNED list cohorts.kueue.x-k8s.io' "$(run_check CHECK_FN=wva_check_kueue_cohort_read)"
check 'cohort list granted -> proceeds' \
    'OK list cohorts.kueue.x-k8s.io' "$(run_check CHECK_FN=wva_check_kueue_cohort_read STUB_CANI=yes)"
check 'cohort list unverifiable -> warned' \
    'WARNED list cohorts.kueue.x-k8s.io' "$(run_check CHECK_FN=wva_check_kueue_cohort_read STUB_CANI=unknown)"
check 'cohort list: INSTALL_PHASE=all asks nothing' \
    'OK -' "$(run_check CHECK_FN=wva_check_kueue_cohort_read INSTALL_PHASE=all)"
check 'cohort list: no Kueue source asks nothing' \
    'OK -' "$(run_check CHECK_FN=wva_check_kueue_cohort_read WVA_LIMITER=none WVA_QUOTA_KUEUE=false)"
check 'cohort list: an unreadable policy asks nothing (the namespace check warns)' \
    'OK -' "$(run_check CHECK_FN=wva_check_kueue_cohort_read WVA_LIMITER=none WVA_QUOTA_KUEUE=false STUB_CM_FORBIDDEN=wva-policy)"

# Wired into check_permissions -- which --check and the install both run --
# BEFORE the tenant early-return, or the namespace-scoped default never reaches it.
body="$(tr -d '\r' < "$ROOT/deploy/lib/prereqs.sh" | sed -n '/^check_permissions() {/,/^}/p')"
call_line="$(printf '%s\n' "$body" | grep -n '^[[:space:]]*wva_check_kueue_namespace_read$' | head -1 | cut -d: -f1)"
tenant_line="$(printf '%s\n' "$body" | grep -n 'if wva_scope_is_tenant; then' | tail -1 | cut -d: -f1)"
check 'check_permissions calls it' 'yes' "$([ -n "$call_line" ] && echo yes || echo no)"
check 'before the tenant early-return' 'yes' \
    "$([ -n "$call_line" ] && [ -n "$tenant_line" ] && [ "$call_line" -lt "$tenant_line" ] && echo yes || echo no)"
cohort_line="$(printf '%s\n' "$body" | grep -n '^[[:space:]]*wva_check_kueue_cohort_read$' | head -1 | cut -d: -f1)"
check 'check_permissions calls the cohort check before the tenant early-return' 'yes' \
    "$([ -n "$cohort_line" ] && [ -n "$tenant_line" ] && [ "$cohort_line" -lt "$tenant_line" ] && echo yes || echo no)"

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
