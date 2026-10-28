#!/usr/bin/env bash
#
# Executes the installer's decision functions against a STUBBED kubectl/helm and
# asserts what they decide. Nothing here touches a cluster.
#
# Each section covers a review finding that `bash -n` cannot see, because every
# one of them parses and is wrong about a running cluster:
#
#   KEDA        the preflight never looked for KEDA, and the install applied the
#               controller before deploy_keda found it missing -- a half-install.
#
# Sourced through CR-stripped copies, for the reason hack/check-workload-gaps.sh
# gives: a Windows checkout writes these files as CRLF and bash then stops at the
# first function definition.

set -u

# CHECK_ROOT runs the check against another tree -- how its negative control is
# taken: point it at an extracted older deploy/ and the cases must fail.
ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
FAIL=0
PASS=0
# Every case below reports ok or FAIL exactly once. A run that ends early --
# a sourced lib that exits, a section that stops -- otherwise passes having
# checked less than it says. Change this deliberately with the cases.
EXPECT_CASES=98

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh constants.sh prereqs.sh scaler_runtime.sh infra_scaler_backend.sh install_core.sh verify.sh pod_writer.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done

BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
unset WVA_SCOPE WVA_WATCH_NS WVA_LIMITER KEDA_HELM_INSTALL SCALER_BACKEND ENVIRONMENT
WVA_NS="wva-system"
KEDA_CHART_VERSION="2.21.0"
KEDA_NAMESPACE="keda-system"
REQUIRED_TOOLS=(kubectl)
# shellcheck disable=SC1090
for lib in common.sh constants.sh prereqs.sh scaler_runtime.sh infra_scaler_backend.sh install_core.sh verify.sh pod_writer.sh; do
    . "$WORK/$lib"
done

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

# --- the stub -----------------------------------------------------------------
#
# Driven by variables, so each case states the cluster it describes:
#   CRD_MODE      absent | forbidden | old | new | error
#   OP_READY      True | False | none | forbidden    (keda-operator pods)
#   OP_IMAGE      the operator Deployment's image, or forbidden
#   CSV_MODE      ok | forbidden | none    (OpenShift Custom Metrics Autoscaler)
#   KUBECTL_LOG   every call is appended here, for cases that assert on calls
KUBECTL_LOG="$WORK/kubectl.log"
kubectl() {
    printf '%s\n' "$*" >> "$KUBECTL_LOG"
    case "$*" in
        "get crd scaledobjects.keda.sh"*)
            case "${CRD_MODE:-new}" in
                absent)    echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "scaledobjects.keda.sh" not found' >&2; return 1 ;;
                forbidden) echo 'Error from server (Forbidden): customresourcedefinitions.apiextensions.k8s.io "scaledobjects.keda.sh" is forbidden: User "u" cannot get resource' >&2; return 1 ;;
                error)     echo 'Unable to connect to the server: dial tcp: i/o timeout' >&2; return 1 ;;
                old)       echo ''; return 0 ;;
                new)       echo 'string'; return 0 ;;
            esac ;;
        "get pods -A -l app.kubernetes.io/name=keda-operator -o jsonpath={range"*)
            case "${OP_READY:-True}" in
                forbidden) return 1 ;;
                none)      echo ''; return 0 ;;
                *)         echo "${OP_READY}"; return 0 ;;
            esac ;;
        "get pods -A -l app.kubernetes.io/name=keda-operator -o name"*)
            case "${OP_READY:-True}" in
                forbidden) return 1 ;;
                none)      echo ''; return 0 ;;
                *)         echo 'pod/keda-operator-abc'; return 0 ;;
            esac ;;
        "get deployments -A -l app.kubernetes.io/name=keda-operator -o jsonpath={.items[0].spec.template.spec.containers[0].image}"*)
            case "${OP_IMAGE:-}" in
                forbidden) echo 'Error from server (Forbidden): deployments.apps is forbidden: User "u" cannot list resource "deployments" in API group "apps" at the cluster scope' >&2; return 1 ;;
                *)         echo "${OP_IMAGE:-ghcr.io/kedacore/keda:2.21.0}"; return 0 ;;
            esac ;;
        "get csv -n openshift-keda"*)
            case "${CSV_MODE:-ok}" in
                forbidden) echo 'Error from server (Forbidden): clusterserviceversions.operators.coreos.com is forbidden' >&2; return 1 ;;
                none)      echo ''; return 0 ;;
                *)         echo 'custom-metrics-autoscaler.v2.17.2-1 2.17.2-1'; return 0 ;;
            esac ;;
        "get pods -A -l app.kubernetes.io/name=keda-operator -o jsonpath={.items[0].spec.containers[0].image}"*)
            echo "${OP_IMAGE:-ghcr.io/kedacore/keda:2.21.0}"; return 0 ;;
    esac
    return 0
}
helm() { printf 'helm %s\n' "$*" >> "$KUBECTL_LOG"; return 0; }

# keda_case runs check_keda_prerequisite in a subshell (log_error exits) and
# leaves rc / out / state for the assertions.
keda_case() {
    : > "$KUBECTL_LOG"
    out="$( (check_keda_prerequisite; echo "STATE=${WVA_KEDA_STATE:-}") 2>&1 )"
    rc=$?
    state="$(printf '%s\n' "$out" | sed -n 's/^STATE=//p')"
}

# --- KEDA ---------------------------------------------------------------------

if ! declare -F check_keda_prerequisite >/dev/null; then
    fail "KEDA: check_keda_prerequisite is not defined -- the preflight has no KEDA probe"
else
    ENVIRONMENT=kubernetes; SCALER_BACKEND=keda; KEDA_HELM_INSTALL=false
    CRD_MODE=absent; OP_READY=none; keda_case
    if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'KEDA_HELM_INSTALL=true'; then
        ok "KEDA absent on Kubernetes refuses, and says KEDA_HELM_INSTALL=true"
    else
        fail "KEDA absent on Kubernetes must refuse with KEDA_HELM_INSTALL advice (rc=$rc)" "$out"
    fi

    ENVIRONMENT=openshift; CRD_MODE=absent; keda_case
    if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'Custom Metrics Autoscaler'; then
        ok "KEDA absent on OpenShift refuses, and names the Custom Metrics Autoscaler operator"
    else
        fail "KEDA absent on OpenShift must name the operator to install (rc=$rc)" "$out"
    fi

    ENVIRONMENT=kubernetes; CRD_MODE=forbidden; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = unknown ] && printf '%s' "$out" | grep -qi 'cannot verify'; then
        ok "KEDA Forbidden is 'cannot verify', not absent"
    else
        fail "KEDA Forbidden must warn and continue as unknown (rc=$rc state=$state)" "$out"
    fi

    CRD_MODE=error; keda_case
    if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'could not check'; then
        ok "KEDA probe error (not NotFound, not Forbidden) refuses as 'could not check'"
    else
        fail "an unreadable CRD must refuse as unknown-error, not pass (rc=$rc)" "$out"
    fi

    KEDA_HELM_INSTALL=true; CRD_MODE=absent; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = absent-installing ]; then
        ok "KEDA absent with KEDA_HELM_INSTALL=true passes: this run installs it"
    else
        fail "KEDA_HELM_INSTALL=true must accept an absent KEDA (rc=$rc state=$state)" "$out"
    fi
    # The controller-only phase never runs deploy_keda, so KEDA_HELM_INSTALL=true
    # there installs nothing. Accepting an absent KEDA on it went on to report
    # "KEDA: installed by this run" about a KEDA nobody installed.
    INSTALL_PHASE=wva; CRD_MODE=absent; keda_case
    if [ "$rc" -ne 0 ] && [ "$state" != absent-installing ] && printf '%s' "$out" | grep -q 'never installs KEDA'; then
        ok "KEDA absent with INSTALL_PHASE=wva refuses even with KEDA_HELM_INSTALL=true"
    else
        fail "INSTALL_PHASE=wva must refuse an absent KEDA: that phase never installs it (rc=$rc state=$state)" "$out"
    fi
    unset INSTALL_PHASE
    KEDA_HELM_INSTALL=false

    # The advice is the command to run, in this install's shape.
    CRD_MODE=absent; keda_case
    if printf '%s' "$out" | grep -q 'KEDA_HELM_INSTALL=true NAMESPACE=wva-system make setup-prereqs'; then
        ok "KEDA absent prints the setup-prereqs command that installs it"
    else
        fail "KEDA absent must print the make setup-prereqs command with KEDA_HELM_INSTALL=true" "$out"
    fi

    # SKIP_CHECKS=true skips the preflight that sets WVA_KEDA_STATE; deploy_keda
    # confirmed the CRD and the summary still said "not verified by this run".
    CRD_MODE=new
    : > "$KUBECTL_LOG"
    out="$( (WVA_KEDA_STATE=''; deploy_keda; echo "STATE=${WVA_KEDA_STATE:-}") 2>&1 )"
    if [ "$(printf '%s\n' "$out" | sed -n 's/^STATE=//p')" = present ]; then
        ok "deploy_keda records a KEDA it confirmed, for the summary (SKIP_CHECKS=true)"
    else
        fail "deploy_keda must set WVA_KEDA_STATE=present when it finds the CRD" "$out"
    fi

    # Older than 2.17 is supported: the plan writes ScaledObjects with no
    # fallback (so_keda_fallback_probe). The preflight must say so and go on,
    # never refuse what the install itself accepts.
    CRD_MODE=old; OP_READY=True; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = present ] && printf '%s' "$out" | grep -q 'NO fallback'; then
        ok "KEDA CRD without fallback.behavior warns (no fallback) and continues"
    else
        fail "a CRD without fallback.behavior must warn and continue as present (rc=$rc state=$state)" "$out"
    fi

    WVA_SO_FALLBACK=false; keda_case
    if [ "$rc" -eq 0 ] && ! printf '%s' "$out" | grep -q 'NO fallback'; then
        ok "KEDA without fallback.behavior and WVA_SO_FALLBACK=false does not warn about a fallback nothing writes"
    else
        fail "WVA_SO_FALLBACK=false must not warn about the fallback (rc=$rc)" "$out"
    fi
    unset WVA_SO_FALLBACK

    CRD_MODE=new; OP_READY=True; OP_IMAGE=ghcr.io/kedacore/keda:2.16.1; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = present ] && printf '%s' "$out" | grep -q 'operator is 2.16.1.*NO fallback'; then
        ok "KEDA operator 2.16.1 warns (no fallback before ${KEDA_FALLBACK_MIN_VERSION:-2.17}) and continues"
    else
        fail "an operator older than ${KEDA_FALLBACK_MIN_VERSION:-2.17} must warn and continue (rc=$rc state=$state)" "$out"
    fi

    OP_IMAGE=ghcr.io/kedacore/keda:2.21.0; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = present ]; then
        ok "KEDA 2.21.0 with a Ready operator is present"
    else
        fail "KEDA 2.21.0 Ready must pass as present (rc=$rc state=$state)" "$out"
    fi

    # The version of an existing KEDA: reported, never blocking. 2.18, 2.19 and 2.21
    # are supported (CI installs the default chart; 2.18 is what the benchmarks ran
    # on). other_warnings prints every [WARNING] line but the CVE advice, which
    # is the one warning a supported version may get.
    other_warnings() { printf '%s\n' "$out" | grep '\[WARNING\]' | grep -v "CVE-2026-77524" || true; }
    for v in 2.18.1 2.19.0; do
        OP_IMAGE=ghcr.io/kedacore/keda:$v; keda_case
        if [ "$rc" -eq 0 ] && [ "$state" = present ] \
            && printf '%s' "$out" | grep -q "KEDA $v: supported" \
            && [ -z "$(other_warnings)" ]; then
            ok "VERSION: KEDA $v is supported, rc 0, no warning but the CVE advice"
        else
            fail "KEDA $v must be reported supported with rc 0 and no other warning (rc=$rc state=$state)" "$out"
        fi
    done

    OP_IMAGE=ghcr.io/kedacore/keda:2.19.0; keda_case
    if printf '%s' "$out" | grep -q "carries CVE-2026-77524.*Advice only: consider upgrading KEDA to 2.21.0"; then
        ok "VERSION: KEDA 2.19.0 gets the CVE-2026-77524 advice, as advice"
    else
        fail "KEDA 2.19.0 must get the CVE-2026-77524 advice (rc=$rc)" "$out"
    fi

    # 2.21 is supported, and the CVE advice stops at the release that fixes it.
    OP_IMAGE=ghcr.io/kedacore/keda:2.21.0; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'KEDA 2.21.0: supported (KEDA 2.18, 2.19 and 2.21' \
        && ! printf '%s' "$out" | grep -q "CVE-2026-77524" && [ -z "$(other_warnings)" ]; then
        ok "VERSION: KEDA 2.21.0 is supported, with no CVE advice and no warning"
    else
        fail "KEDA 2.21.0 must be supported with rc 0 and no CVE advice (rc=$rc)" "$out"
    fi

    # Before 2.20 KEDA can leave a new ScaledObject's HPA on the CPU default
    # after any scaler restart or outage (kedacore/keda#7777). One warning
    # carries both that and the CVE, and names the remedies; 2.20 gets only the
    # CVE advice, 2.21 neither.
    for v in 2.18.1 2.19.0; do
        OP_IMAGE=ghcr.io/kedacore/keda:$v; keda_case
        if [ "$rc" -eq 0 ] && [ "$(printf '%s\n' "$out" | grep -c '\[WARNING\]')" -eq 1 ] \
            && printf '%s' "$out" | grep -q "KEDA $v carries CVE-2026-77524.*bug fixed in 2.20.0: a ScaledObject created or changed within about two minutes of a WVA controller restart, rollout or outage can stay unwired, its HPA left on the CPU default" \
            && printf '%s' "$out" | grep -q "run  NAMESPACE=wva-system make scaledobjects-rewire  after every WVA controller restart (or restart the KEDA operator).*consider upgrading KEDA to 2.21.0"; then
            ok "VERSION: KEDA $v gets one warning: unwired after a scaler restart, the rewire remedy, and the CVE"
        else
            fail "KEDA $v must get one warning naming the unwired-HPA bug, make scaledobjects-rewire, a KEDA restart and the upgrade (rc=$rc)" "$out"
        fi
    done
    OP_IMAGE=ghcr.io/kedacore/keda:2.20.0; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "KEDA 2.20.0 carries CVE-2026-77524" \
        && ! printf '%s' "$out" | grep -qi "unwired"; then
        ok "VERSION: KEDA 2.20.0 gets the CVE advice and no unwired-HPA warning"
    else
        fail "KEDA 2.20.0 must get the CVE advice and no unwired-HPA warning (rc=$rc)" "$out"
    fi
    OP_IMAGE=ghcr.io/kedacore/keda:2.21.0; keda_case
    if [ "$rc" -eq 0 ] && ! printf '%s' "$out" | grep -qi "unwired" \
        && ! printf '%s' "$out" | grep -q "CVE-2026-77524"; then
        ok "VERSION: KEDA 2.21.0 gets neither the unwired-HPA warning nor the CVE advice"
    else
        fail "KEDA 2.21.0 must get no unwired-HPA warning and no CVE advice (rc=$rc)" "$out"
    fi
    # The rewire command carries this install's shape: a bare one finds no
    # ScaledObject on a cluster-scoped install and exits 0.
    WVA_SCOPE=cluster; OP_IMAGE=ghcr.io/kedacore/keda:2.19.0; keda_case; unset WVA_SCOPE
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "run  make scaledobjects-rewire SCOPE=cluster WVA_NS=wva-system  after every WVA controller restart"; then
        ok "VERSION: on a cluster-scoped install the rewire remedy names SCOPE=cluster and WVA_NS"
    else
        fail "the rewire remedy must carry SCOPE=cluster WVA_NS=wva-system on a cluster-scoped install (rc=$rc)" "$out"
    fi

    # The advice says how to upgrade a KEDA that is there: re-running the
    # installer never does.
    OP_IMAGE=ghcr.io/kedacore/keda:2.20.2; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'KEDA 2.20.2: untested, and not refused' \
        && printf '%s' "$out" | grep -q "carries CVE-2026-77524" \
        && printf '%s' "$out" | grep -q "never upgrades a KEDA that is there.*helm upgrade keda kedacore/keda -n .* --version 2.21.0 --reuse-values"; then
        ok "VERSION: KEDA 2.20.2 is untested, carries the CVE advice, and is given the helm upgrade"
    else
        fail "KEDA 2.20.2 must be untested, with the CVE advice and the helm upgrade command (rc=$rc)" "$out"
    fi

    OP_IMAGE=ghcr.io/kedacore/keda:2.16.1; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = present ] && printf '%s' "$out" | grep -q 'KEDA 2.16.1: untested, and not refused' \
        && [ "$(printf '%s\n' "$out" | grep -c 'NO fallback')" -eq 1 ]; then
        ok "VERSION: KEDA 2.16.1 (too old for a fallback) is untested, warned once about the fallback, and not refused"
    else
        fail "KEDA 2.16.1 must be untested with one fallback warning and rc 0 (rc=$rc state=$state)" "$out"
    fi

    OP_IMAGE=forbidden; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = present ] \
        && printf '%s' "$out" | grep -q 'could not check its version (listing Deployments cluster-wide is not permitted' \
        && ! printf '%s' "$out" | grep -q ': supported'; then
        ok "VERSION: a Forbidden operator Deployment is 'could not check', never supported"
    else
        fail "a Forbidden version read must warn it could not check, and not pass as supported (rc=$rc)" "$out"
    fi

    OP_IMAGE=ghcr.io/kedacore/keda:latest; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'could not check its version (the operator image ghcr.io/kedacore/keda:latest has no version tag (latest))' \
        && ! printf '%s' "$out" | grep -q ': supported'; then
        ok "VERSION: a non-semver tag (latest) is 'could not check', never supported"
    else
        fail "a non-semver tag must warn it could not check (rc=$rc)" "$out"
    fi

    OP_IMAGE=ghcr.io/kedacore/keda@sha256:0123abcd; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'pinned by digest, which names no version'; then
        ok "VERSION: a digest-pinned image is 'could not check'"
    else
        fail "a digest-pinned image must warn it could not check (rc=$rc)" "$out"
    fi

    OP_IMAGE=registry.local:5000/keda; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'has no version tag (none)'; then
        ok "VERSION: a registry port is not read as a version tag"
    else
        fail "registry.local:5000/keda must have no version tag (rc=$rc)" "$out"
    fi
    OP_IMAGE=ghcr.io/kedacore/keda:2.19.0

    # Reported once: the preflight and deploy_keda both reach it in one run.
    : > "$KUBECTL_LOG"
    out="$( (check_keda_prerequisite; deploy_keda) 2>&1 )"; rc=$?
    if [ "$rc" -eq 0 ] && [ "$(printf '%s\n' "$out" | grep -c 'KEDA 2.19.0: supported')" -eq 1 ]; then
        ok "VERSION: the preflight and deploy_keda report the version once"
    else
        fail "the version must be reported exactly once per run (rc=$rc)" "$out"
    fi

    # SKIP_CHECKS=true skips the preflight; deploy_keda still reports it.
    out="$( (deploy_keda) 2>&1 )"; rc=$?
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'KEDA 2.19.0: supported'; then
        ok "VERSION: deploy_keda reports an existing KEDA's version without the preflight"
    else
        fail "deploy_keda must report the version of the KEDA it finds (rc=$rc)" "$out"
    fi

    # The chart this run installs gets the same judgement: the default chart
    # is supported and carries no CVE advice; an older pinned one gets it.
    KEDA_HELM_INSTALL=true; CRD_MODE=absent; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'KEDA chart 2.21.0 (this run installs it): supported' \
        && ! printf '%s' "$out" | grep -q 'CVE-2026-77524'; then
        ok "VERSION: the default chart KEDA_HELM_INSTALL=true installs is reported supported, with no CVE advice"
    else
        fail "the default chart this run installs must be reported supported without CVE advice (rc=$rc)" "$out"
    fi
    KEDA_CHART_VERSION=2.19.0; keda_case; KEDA_CHART_VERSION=2.21.0
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'KEDA chart 2.19.0 (this run installs it): supported' \
        && printf '%s' "$out" | grep -q 'consider KEDA_CHART_VERSION=2.21.0'; then
        ok "VERSION: a pinned 2.19.0 chart is reported supported, with the CVE advice"
    else
        fail "a pinned 2.19.0 chart must be reported with the CVE advice (rc=$rc)" "$out"
    fi
    # A v-prefixed chart version is judged as the version it names, not as
    # something too odd to compare (and so new enough).
    KEDA_CHART_VERSION=v2.19.0; keda_case; KEDA_CHART_VERSION=2.21.0
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'KEDA chart v2.19.0 (this run installs it) carries CVE-2026-77524.*can stay unwired'; then
        ok "VERSION: a v-prefixed chart (v2.19.0) gets the unwired-HPA and CVE warning"
    else
        fail "KEDA_CHART_VERSION=v2.19.0 must be judged as 2.19.0 (rc=$rc)" "$out"
    fi
    KEDA_HELM_INSTALL=false; CRD_MODE=new

    # OpenShift: the Custom Metrics Autoscaler's own version, not compared.
    ENVIRONMENT=openshift; CSV_MODE=ok; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'Custom Metrics Autoscaler 2.17.2-1, untested' \
        && printf '%s' "$out" | grep -q 'If it bundles a KEDA below 2.20.0, upgrade the operator; until then run  NAMESPACE=wva-system make scaledobjects-rewire  after every WVA controller restart (or restart its KEDA operator)'; then
        ok "VERSION: OpenShift reports the Custom Metrics Autoscaler version, untested"
    else
        fail "OpenShift must report the CMA version (rc=$rc)" "$out"
    fi
    CSV_MODE=forbidden; keda_case
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'could not check the Custom Metrics Autoscaler version (reading ClusterServiceVersions'; then
        ok "VERSION: an unreadable Custom Metrics Autoscaler version is 'could not check'"
    else
        fail "a Forbidden CSV read must warn it could not check (rc=$rc)" "$out"
    fi
    ENVIRONMENT=kubernetes; CSV_MODE=ok

    # The tested minor is the one CI installs: install.sh's default chart.
    chart_default="$(sed -n 's/^KEDA_CHART_VERSION=\${KEDA_CHART_VERSION:-\([0-9.]*\)}.*/\1/p' "$ROOT/deploy/install.sh" | tr -d '\r')"
    if [ -n "$chart_default" ] && [ "${chart_default%.*}" = "${KEDA_TESTED_VERSION:-}" ]; then
        ok "VERSION: KEDA_TESTED_VERSION ($KEDA_TESTED_VERSION) is the minor of install.sh's default chart ($chart_default)"
    else
        fail "KEDA_TESTED_VERSION (${KEDA_TESTED_VERSION:-unset}) must match install.sh's default KEDA_CHART_VERSION (${chart_default:-not found})"
    fi

    OP_READY=False; keda_case
    if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'none is Ready'; then
        ok "KEDA operator pods present and none Ready refuses"
    else
        fail "operator pods with none Ready must refuse (rc=$rc)" "$out"
    fi

    OP_READY=forbidden; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = present ]; then
        ok "KEDA operator unlistable (Forbidden) does not refuse a CRD that is new enough"
    else
        fail "an unlistable operator must not refuse (rc=$rc state=$state)" "$out"
    fi
    OP_READY=True

    SCALER_BACKEND=none; keda_case
    if [ "$rc" -eq 0 ] && [ "$state" = skipped ] && [ ! -s "$KUBECTL_LOG" ]; then
        ok "SCALER_BACKEND=none skips the KEDA probe without calling the cluster"
    else
        fail "SCALER_BACKEND=none must skip the probe (rc=$rc state=$state)" "$out"
    fi
    SCALER_BACKEND=keda
fi

# An admin re-run asking for KEDA (KEDA_HELM_INSTALL=true) keeps both phases
# while KEDA is not there: only the prereqs half installs it. Resolved to `wva`
# because the RBAC prereqs existed, it skipped deploy_keda and then failed the
# KEDA check it had asked to satisfy.
# resolved_phase <CRD_MODE> <KEDA_HELM_INSTALL> prints the phase chosen.
resolved_phase() {
    (
        CRD_MODE="$1" KEDA_HELM_INSTALL="$2" SCALER_BACKEND=keda ENVIRONMENT=kubernetes
        INSTALL_PHASE_EXPLICIT='' INSTALL_PHASE=all WVA_SCOPE=namespace
        wva_missing_prereqs() { :; }
        log_info() { :; }
        wva_resolve_install_phase
        printf '%s' "$INSTALL_PHASE"
    )
}
if [ "$(resolved_phase absent true)" = all ] && [ "$(resolved_phase forbidden true)" = all ] \
    && [ "$(resolved_phase new true)" = wva ] && [ "$(resolved_phase absent false)" = wva ]; then
    ok "PHASE: KEDA_HELM_INSTALL=true keeps both phases until KEDA is confirmed installed"
else
    fail "PHASE: KEDA_HELM_INSTALL=true with KEDA absent or unreadable must stay all; present, or not asked for, resolves to wva (absent/true $(resolved_phase absent true), forbidden/true $(resolved_phase forbidden true), new/true $(resolved_phase new true), absent/false $(resolved_phase absent false))"
fi

# The ORDER of the cluster-admin phase: the scaler backend before the controller.
if ! declare -F wva_run_prereqs_phase >/dev/null; then
    fail "KEDA: wva_run_prereqs_phase is not defined -- the prereqs order is not testable"
else
    order="$(
        ORDER=""
        create_namespaces()        { ORDER="$ORDER namespaces"; }
        deploy_monitoring_stack()  { ORDER="$ORDER monitoring"; }
        deploy_scaler_backend()    { ORDER="$ORDER scaler"; }
        deploy_wva_prerequisites() { ORDER="$ORDER prerequisites"; }
        deploy_wva_controller()    { ORDER="$ORDER controller"; }
        pl_grant_published_policy_read() { ORDER="$ORDER policy-read"; }
        wva_install_scope()        { echo namespace; }
        DEPLOY_WVA=true
        wva_run_prereqs_phase all
        echo "$ORDER"
    )"
    case "$order" in
        *scaler*controller*) ok "prereqs phase installs the scaler backend before the controller ($order)" ;;
        *) fail "the scaler backend must come before the controller, got:$order" ;;
    esac
    case "$order" in
        *monitoring*scaler*) ok "prereqs phase installs monitoring before the scaler backend (KEDA's chart creates ServiceMonitors)" ;;
        *) fail "monitoring must come before the scaler backend, got:$order" ;;
    esac
    # A namespace install grants the published policy read BEFORE the
    # controller is applied, or its first pod races the RoleBinding.
    case "$order" in
        *policy-read*controller*) ok "prereqs phase grants the published policy read before the controller" ;;
        *) fail "the published policy read must be granted before the controller, got:$order" ;;
    esac
fi

# The handover text must not promise a KEDA install that never happens.
summary="$(
    wva_rendered_prereq_objects() { echo "ClusterRole wva-manager-role-abc"; }
    wva_install_scope() { echo namespace; }
    ENVIRONMENT=openshift WVA_KEDA_STATE=present print_prereqs_summary 2>&1
)"
if printf '%s' "$summary" | grep -q 'if this cluster had none'; then
    fail "print_prereqs_summary still claims KEDA is installed 'if this cluster had none'"
elif printf '%s' "$summary" | grep -q 'KEDA: already on the cluster'; then
    ok "print_prereqs_summary reports the measured KEDA state"
else
    fail "print_prereqs_summary must report the measured KEDA state" "$summary"
fi

# --- POD WRITES ---------------------------------------------------------------
#
# This release moves `patch` on pods out of the controller's ClusterRole into
# role/wva-pod-writer, made per namespace (deploy/lib/pod_writer.sh). An upgrade
# took the old grant away and nothing made the new one: an existing warm pool
# stopped lending, and an active utilization-share optimizer could not clear
# the marks it had written. And an uninstall left every grant behind.
#
# The stub, driven by variables:
#   PW_SOS            ScaledObject triggers as the go-template renders them
#   PW_MARKED         namespaces holding a pod with the transfer mark
#   PW_RBS            "namespace name" of each labelled RoleBinding
#   PW_*_FORBIDDEN    that call is refused (a list of namespaces for apply,
#                     delete and the per-namespace RoleBinding list)
pw_kubectl() {
    printf '%s\n' "$*" >> "$KUBECTL_LOG"
    local body ns
    case "$*" in
        "get scaledobject -A"*)
            [ -z "${PW_SO_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): scaledobjects.keda.sh is forbidden: User "u" cannot list resource "scaledobjects" at the cluster scope' >&2; return 1; }
            [ -z "${PW_SO_NOTYPE:-}" ] || { echo "error: the server doesn't have a resource type \"scaledobject\"" >&2; return 1; }
            printf '%s\n' "${PW_SOS:-}" ;;
        # The optimizer mode: PW_STATUS is what the controller publishes
        # (FORBIDDEN refuses the read; unset is no status ConfigMap), PW_POLICY
        # the default entry of its policy (unset is no policy ConfigMap).
        "get configmap wva-controller-status "*)
            case "${PW_STATUS:-}" in
                "") echo 'Error from server (NotFound): configmaps "wva-controller-status" not found' >&2; return 1 ;;
                FORBIDDEN) echo 'Error from server (Forbidden): configmaps "wva-controller-status" is forbidden: User "u" cannot get resource "configmaps"' >&2; return 1 ;;
                *) printf '%s' "$PW_STATUS" ;;
            esac ;;
        "get configmap wva-scaling-policy-config "*)
            [ -n "${PW_POLICY:-}" ] || { echo 'Error from server (NotFound): configmaps "wva-scaling-policy-config" not found' >&2; return 1; }
            printf '%s\n' "$PW_POLICY" ;;
        # May the controller patch pods? PW_CANI yes, no, or (unset) this user
        # may not impersonate it. PW_HAS_RB / PW_HAS_ROLE: the two halves of
        # the grant exist.
        "auth can-i "*)
            case "${PW_CANI:-}" in
                yes) echo yes ;;
                no) echo no; return 1 ;;
                *) echo 'Error from server (Forbidden): users "u" is forbidden: User "u" cannot impersonate resource "serviceaccounts"' >&2; return 1 ;;
            esac ;;
        "get rolebinding wva-pod-writer-"*)
            [ -n "${PW_HAS_RB:-}" ] || { echo 'Error from server (NotFound): rolebindings.rbac.authorization.k8s.io not found' >&2; return 1; } ;;
        "get role wva-pod-writer "*)
            [ -n "${PW_HAS_ROLE:-}" ] || { echo 'Error from server (NotFound): roles.rbac.authorization.k8s.io "wva-pod-writer" not found' >&2; return 1; }
            # Its rules, as the jsonpath prints them. PW_ROLE_RULES overrides.
            printf '%s\n' "${PW_ROLE_RULES:-[\"\"] [\"pods\"] [\"patch\"]}" ;;
        "get pods -A"*)
            [ -z "${PW_PODS_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): pods is forbidden: User "u" cannot list resource "pods" at the cluster scope' >&2; return 1; }
            printf '%s\n' "${PW_MARKED:-}" ;;
        "apply -f -")
            body="$(cat)"
            ns="$(printf '%s\n' "$body" | sed -n 's/^  namespace: //p' | sed -n 1p)"
            case " ${PW_APPLY_FORBIDDEN:-} " in
                *" $ns "*) echo "Error from server (Forbidden): roles.rbac.authorization.k8s.io \"wva-pod-writer\" is forbidden: User \"u\" cannot create resource \"roles\" in the namespace \"$ns\"" >&2; return 1 ;;
            esac
            echo "APPLIED $ns" >> "$KUBECTL_LOG" ;;
        "get rolebinding -A"*)
            [ -z "${PW_RB_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): rolebindings.rbac.authorization.k8s.io is forbidden: User "u" cannot list resource "rolebindings" at the cluster scope' >&2; return 1; }
            printf '%s\n' "${PW_RBS:-}" ;;
        "get rolebinding -n "*)
            case " ${PW_RB_NS_FORBIDDEN:-} " in
                *" $4 "*) echo "Error from server (Forbidden): rolebindings is forbidden in the namespace \"$4\"" >&2; return 1 ;;
            esac
            printf '%s\n' "${PW_RBS:-}" | awk -v n="$4" '$1 == n' ;;
        "delete rolebinding "*|"delete role "*)
            case " ${PW_DELETE_FORBIDDEN:-} " in
                *" $5 "*) echo "Error from server (Forbidden): $2 is forbidden in the namespace \"$5\"" >&2; return 1 ;;
            esac ;;
    esac
    return 0
}

# pw_case <function> <args...> -- runs it against the stub, in a subshell;
# leaves out (its log) and the stub's call log in KUBECTL_LOG.
pw_case() {
    : > "$KUBECTL_LOG"
    out="$( (kubectl() { pw_kubectl "$@"; }; "$@") 2>&1 )"
}
applied() { grep '^APPLIED ' "$KUBECTL_LOG" | cut -d' ' -f2 | sort | tr '\n' ' ' | sed 's/ $//'; }

PW_ADDR="wva-external-scaler.wva-system.svc.cluster.local:9090"
PW_OTHER="wva-external-scaler.other.svc.cluster.local:9090"
PW_SUFFIX="$(printf '%s' wva-system | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-8)"

if ! declare -F wva_regrant_pod_writers >/dev/null; then
    fail "POD WRITES: wva_regrant_pod_writers is not defined -- an upgrade takes patch on pods away and grants it nowhere"
else
    # A pool in ns-p and a marked model in ns-m call this controller. ns-q
    # holds a model with no mark; ns-x holds a mark of another controller's.
    PW_SOS="ns-p $PW_ADDR pool-a
ns-m $PW_ADDR -
ns-q $PW_ADDR -
ns-x $PW_OTHER -"
    PW_MARKED="ns-m
ns-x"
    pw_case wva_regrant_pod_writers wva-system
    if [ "$(applied)" = "ns-m ns-p" ]; then
        ok "POD WRITES: an upgrade grants in the warm-pool and marked namespaces of this controller, and no others"
    else
        fail "POD WRITES: an upgrade must grant in ns-m and ns-p only, got [$(applied)]" "$out"
    fi

    PW_APPLY_FORBIDDEN="ns-p" pw_case wva_regrant_pod_writers wva-system
    if [ "$(applied)" = "ns-m" ] && printf '%s' "$out" | grep -q 'pod_writer.sh ns-p wva-system | kubectl apply -f -'; then
        ok "POD WRITES: a namespace the installer may not grant in is named, with the command an admin runs"
    else
        fail "POD WRITES: a refused grant must name ns-p and the admin command" "$out"
    fi

    PW_SO_FORBIDDEN=1 pw_case wva_regrant_pod_writers wva-system
    if [ -z "$(applied)" ] && printf '%s' "$out" | grep -q 'UNKNOWN'; then
        ok "POD WRITES: unreadable ScaledObjects are UNKNOWN and warned, not taken for none"
    else
        fail "POD WRITES: an unreadable ScaledObject list must warn as unknown" "$out"
    fi

    PW_PODS_FORBIDDEN=1 pw_case wva_regrant_pod_writers wva-system
    if [ "$(applied)" = "ns-p" ] && printf '%s' "$out" | grep -q 'transfer marks are UNKNOWN' \
        && printf '%s' "$out" | grep -q 'ns-m ns-q'; then
        ok "POD WRITES: unreadable pods still grant the pools, and name the model namespaces to check"
    else
        fail "POD WRITES: unreadable pods must warn and list the model namespaces" "$out"
    fi

    # An ACTIVE optimizer moves the deletion cost of every model's pods, marked
    # or not. Before, a namespace with no mark yet lost patch on the upgrade,
    # and nothing was printed.
    PW_MARKED="" PW_STATUS=active pw_case wva_regrant_pod_writers wva-system
    if [ "$(applied)" = "ns-m ns-p ns-q" ]; then
        ok "POD WRITES: an active optimizer with no marks yet is granted in every namespace of its models"
    else
        fail "POD WRITES: an active optimizer must be granted in ns-m, ns-p and ns-q, got [$(applied)]" "$out"
    fi
    PW_MARKED="" PW_POLICY='optimizer:
  type: utilizationShare
  utilizationShare:
    shadow: false' pw_case wva_regrant_pod_writers wva-system
    if [ "$(applied)" = "ns-m ns-p ns-q" ]; then
        ok "POD WRITES: a controller with no status is read from its policy: active is granted everywhere"
    else
        fail "POD WRITES: an active block in the policy must grant in ns-m, ns-p and ns-q, got [$(applied)]" "$out"
    fi
    PW_STATUS=FORBIDDEN pw_case wva_regrant_pod_writers wva-system
    if [ "$(applied)" = "ns-m ns-p" ] && printf '%s' "$out" | grep -q 'Could not tell whether the utilization-share optimizer is active' \
        && printf '%s' "$out" | grep -q 'ns-m ns-q'; then
        ok "POD WRITES: an unreadable optimizer mode is warned with the model namespaces, never taken for off"
    else
        fail "POD WRITES: an unreadable mode must warn and list ns-m ns-q" "$out"
    fi

    # The short Service name (wva-external-scaler.<ns>:9090) calls this
    # controller too. An address naming no controller is warned, not guessed.
    PW_SOS="ns-c wva-external-scaler.wva-system:9090 pool-c -
ns-u 10.0.0.9:9090 - llama"
    PW_MARKED="" pw_case wva_regrant_pod_writers wva-system
    if [ "$(applied)" = "ns-c" ]; then
        ok "POD WRITES: a pool addressed by the short Service name (wva-external-scaler.<ns>:port) is granted"
    else
        fail "POD WRITES: wva-external-scaler.wva-system:9090 must count as this controller, got [$(applied)]" "$out"
    fi
    if printf '%s' "$out" | grep -q 'does not say which controller' && printf '%s' "$out" | grep -q 'ns-u (10.0.0.9:9090)'; then
        ok "POD WRITES: a pool or model whose address names no controller is warned about"
    else
        fail "POD WRITES: ns-u, addressed by IP, must be named as unattributable" "$out"
    fi

    # SCALER_BACKEND=none: no ScaledObject kind at all is none, not unknown.
    PW_SO_NOTYPE=1 pw_case wva_regrant_pod_writers wva-system
    if [ -z "$(applied)" ] && ! printf '%s' "$out" | grep -q 'UNKNOWN'; then
        ok "POD WRITES: a cluster without the ScaledObject kind has nothing to grant, and says nothing"
    else
        fail "POD WRITES: a missing ScaledObject kind must read as none, not UNKNOWN" "$out"
    fi
    unset PW_SOS PW_MARKED

    # Asked without impersonation, the grant counts only when both halves
    # exist: a RoleBinding whose Role was deleted grants nothing.
    if [ "$( (kubectl() { pw_kubectl "$@"; }; PW_HAS_RB=1 PW_HAS_ROLE=1 wva_pod_writes_allowed ns-a wva-system) )" = yes ] \
        && [ "$( (kubectl() { pw_kubectl "$@"; }; PW_CANI=yes wva_pod_writes_allowed ns-a wva-system) )" = yes ]; then
        ok "POD WRITES: the RoleBinding and the Role together read as already allowed"
    else
        fail "POD WRITES: a whole grant, or a can-i yes, must read as yes"
    fi
    if [ "$( (kubectl() { pw_kubectl "$@"; }; PW_HAS_RB=1 wva_pod_writes_allowed ns-a wva-system) )" = unknown ]; then
        ok "POD WRITES: a RoleBinding without its Role is not already allowed"
    else
        fail "POD WRITES: a RoleBinding whose Role is gone must read as unknown, so the grant is made again"
    fi
    # A Role of that name that does not grant patch on pods grants nothing.
    if [ "$( (kubectl() { pw_kubectl "$@"; }; PW_HAS_RB=1 PW_HAS_ROLE=1 PW_ROLE_RULES='[""] ["pods"] ["get","list"]' \
            wva_pod_writes_allowed ns-a wva-system) )" = unknown ] \
        && [ "$( (kubectl() { pw_kubectl "$@"; }; PW_HAS_RB=1 PW_HAS_ROLE=1 PW_ROLE_RULES='["*"] ["*"] ["*"]' \
            wva_pod_writes_allowed ns-a wva-system) )" = yes ]; then
        ok "POD WRITES: the Role counts only when a rule grants patch on pods (wildcards count)"
    else
        fail "POD WRITES: a wva-pod-writer Role without patch on pods must read as unknown"
    fi
    # A rule limited by resourceNames grants patch on those pods only. Was read
    # as the whole grant, so the grant was never made again.
    if [ "$( (kubectl() { pw_kubectl "$@"; }; PW_HAS_RB=1 PW_HAS_ROLE=1 PW_ROLE_RULES='[""] ["pods"] ["patch"] ["one-pod"]' \
            wva_pod_writes_allowed ns-a wva-system) )" = unknown ] \
        && [ "$( (kubectl() { pw_kubectl "$@"; }; PW_HAS_RB=1 PW_HAS_ROLE=1 PW_ROLE_RULES='[""] ["pods"] ["patch"] []' \
            wva_pod_writes_allowed ns-a wva-system) )" = yes ]; then
        ok "POD WRITES: a rule restricted by resourceNames is not the whole grant"
    else
        fail "POD WRITES: a patch rule with resourceNames must read as unknown (an empty list still counts)"
    fi
    # Pods are in the core group. A rule on apps/pods grants nothing on them.
    if [ "$( (kubectl() { pw_kubectl "$@"; }; PW_HAS_RB=1 PW_HAS_ROLE=1 PW_ROLE_RULES='["apps"] ["pods"] ["patch"]' \
            wva_pod_writes_allowed ns-a wva-system) )" = unknown ]; then
        ok "POD WRITES: a patch rule in another API group is not the grant"
    else
        fail "POD WRITES: a patch rule on apps/pods must read as unknown: pods are in the core group"
    fi

    # Before the apply that removes the old grant, and only where the
    # installer applies cluster RBAC.
    iw="$(tr -d '\r' < "$ROOT/deploy/lib/infra_wva.sh")"
    regrant_at="$(printf '%s\n' "$iw" | grep -nE '^[[:space:]]+wva_regrant_pod_writers ' | head -1 | cut -d: -f1)"
    apply_at="$(printf '%s\n' "$iw" | grep -nE 'kubectl apply -k "\$tmp_overlay"' | head -1 | cut -d: -f1)"
    if [ -n "$regrant_at" ] && [ -n "$apply_at" ] && [ "$regrant_at" -lt "$apply_at" ] \
        && tr -d '\r' < "$ROOT/deploy/install.sh" | grep -q 'source "$DEPLOY_LIB_DIR/pod_writer.sh"'; then
        ok "POD WRITES: deploy_wva_controller re-grants before its apply, and install.sh sources pod_writer.sh"
    else
        fail "POD WRITES: wva_regrant_pod_writers must run before the overlay apply (line ${regrant_at:-none} vs ${apply_at:-none}), from an install.sh that sources it"
    fi
    # And only where the installer applies cluster RBAC: the phases that carry
    # the bindings (all, prereqs), for a cluster-scoped controller. The three
    # lines above the call, whitespace aside.
    guard=""
    if [ -n "$regrant_at" ] && [ "$regrant_at" -gt 3 ]; then
        guard="$(printf '%s\n' "$iw" | sed -n "$((regrant_at - 3)),${regrant_at}p" | sed 's/^[[:space:]]*//' | tr '\n' '|')"
    fi
    want_guard='case "${WVA_APPLY_SCOPE:-all}" in|all|prereqs)|if [ "$(wva_install_scope)" = "cluster" ]; then|wva_regrant_pod_writers "$WVA_NS"|'
    if [ "$guard" = "$want_guard" ]; then
        ok "POD WRITES: the re-grant sits inside the all|prereqs and cluster-scope guards"
    else
        fail "POD WRITES: wva_regrant_pod_writers must sit directly inside the all|prereqs and cluster-scope guards" "got:  $guard
want: $want_guard"
    fi
fi

# --- WIRING -------------------------------------------------------------------
#
# The TLS and token helpers are tested on their own in check-install-defaults.sh;
# these prove deploy_wva_controller and the uninstall still call them, in order.
# Line numbers inside the function body, whitespace aside.
body_of() { # body_of <file> <function>: the function's lines, numbered from its opening line
    # Depth-counted: deploy_wva_controller defines functions inside itself.
    tr -d '\r' < "$1" | awk -v f="$2() {" '
        $0 == f {on=1}
        on && /^[A-Za-z_][A-Za-z0-9_]*\(\) \{$/ {depth++}
        on {print}
        on && $0 == "}" {depth--; if (depth == 0) exit}' | sed 's/^[[:space:]]*//' | cat -n
}
line_of() { printf '%s\n' "$1" | awk -v p="$2" 'index($0, p) {print $1; exit}'; }
dwc="$(body_of "$ROOT/deploy/lib/infra_wva.sh" deploy_wva_controller)"
read_at="$(line_of "$dwc" 'wva_read_live_prometheus_tls "$WVA_NS"')"
guard_at="$(line_of "$dwc" 'if [ "${WVA_APPLY_SCOPE:-all}" != "prereqs" ] && [ -z "${WVA_LIVE_PROM_TLS:-}" ]; then')"
refuse_at="$(line_of "$dwc" 'wva_refuse_unknown_prometheus_tls')"
render_at="$(line_of "$dwc" 'wva_render_manager_config_patch "$tmp_overlay"')"
apply_at="$(line_of "$dwc" 'kubectl apply -k "$tmp_overlay"')"
write_at="$(line_of "$dwc" 'wva_write_prometheus_settings')"
token_at="$(line_of "$dwc" 'all|prereqs) wva_delete_legacy_controller_token "$WVA_NS" ;;')"
if [ -n "$read_at" ] && [ -n "$guard_at" ] && [ "$((guard_at + 1))" = "$read_at" ] \
    && [ -n "$refuse_at" ] && [ -n "$render_at" ] && [ -n "$apply_at" ] \
    && [ "$read_at" -lt "$refuse_at" ] && [ "$refuse_at" -lt "$render_at" ] && [ "$render_at" -lt "$apply_at" ]; then
    ok "WIRING: the live TLS settings are read (not in the prereqs phase, not twice), checked and rendered before the apply"
else
    fail "WIRING: deploy_wva_controller must read the live TLS settings under the != prereqs, not-yet-read guard, then refuse-if-unknown, then render, all before the apply (guard ${guard_at:-none}, read ${read_at:-none}, refuse ${refuse_at:-none}, render ${render_at:-none}, apply ${apply_at:-none})"
fi
# The refusal says "Nothing was changed", so main reads and judges the live
# settings before EITHER phase runs. Inside deploy_wva_controller it came after
# INSTALL_PHASE=all had already applied the prereqs phase.
mn="$(body_of "$ROOT/deploy/lib/install_core.sh" main)"
m_read_at="$(line_of "$mn" 'wva_read_live_prometheus_tls "$WVA_NS"')"
m_refuse_at="$(line_of "$mn" 'wva_refuse_unknown_prometheus_tls')"
m_prereqs_at="$(line_of "$mn" 'wva_run_prereqs_phase "$phase"')"
m_wva_at="$(line_of "$mn" 'WVA_APPLY_SCOPE=controller deploy_wva_controller')"
if [ -n "$m_read_at" ] && [ -n "$m_refuse_at" ] && [ -n "$m_prereqs_at" ] && [ -n "$m_wva_at" ] \
    && [ "$m_read_at" -lt "$m_refuse_at" ] && [ "$m_refuse_at" -lt "$m_prereqs_at" ] && [ "$m_refuse_at" -lt "$m_wva_at" ]; then
    ok "WIRING: main reads and judges the live TLS settings before either phase writes anything"
else
    fail "WIRING: main must read the live TLS settings and refuse-if-unknown before the prereqs and controller phases (read ${m_read_at:-none}, refuse ${m_refuse_at:-none}, prereqs ${m_prereqs_at:-none}, controller ${m_wva_at:-none})"
fi
# The controller reads its config once, at startup: a changed Prometheus
# setting needs a restart, after every patch to that config.
restart_at="$(line_of "$dwc" 'wva_restart_on_prometheus_change "$(kubectl get configmap wva-manager-config')"
limiter_at="$(line_of "$dwc" 'case "$limiter_set" in')"
wait_at="$(line_of "$dwc" '# Wait for WVA to be ready.')"
if [ -n "$restart_at" ] && [ -n "$write_at" ] && [ -n "$limiter_at" ] && [ -n "$wait_at" ] \
    && [ "$restart_at" -gt "$write_at" ] && [ "$restart_at" -gt "$limiter_at" ] && [ "$restart_at" -lt "$wait_at" ]; then
    ok "WIRING: the controller is restarted on a Prometheus change after the last config patch, before the wait"
else
    fail "WIRING: wva_restart_on_prometheus_change must follow the Prometheus write and the limiter patch, before the readiness wait (restart ${restart_at:-none}, write ${write_at:-none}, limiter ${limiter_at:-none}, wait ${wait_at:-none})"
fi
if [ -n "$write_at" ] && [ -n "$apply_at" ] && [ "$write_at" -gt "$apply_at" ]; then
    ok "WIRING: the Prometheus settings are written into the live config after the apply"
else
    fail "WIRING: wva_write_prometheus_settings must run after the apply (write ${write_at:-none}, apply ${apply_at:-none})"
fi
if [ -n "$token_at" ] && [ -n "$apply_at" ] && [ "$token_at" -gt "$apply_at" ]; then
    ok "WIRING: the legacy controller token is deleted after the apply, in the all|prereqs phases"
else
    fail "WIRING: wva_delete_legacy_controller_token must sit in the all|prereqs case after the apply (token ${token_at:-none}, apply ${apply_at:-none})"
fi
if [ -n "$(line_of "$(body_of "$ROOT/deploy/lib/cleanup.sh" undeploy_wva_controller)" 'wva_delete_legacy_controller_token "$WVA_NS"')" ]; then
    ok "WIRING: the uninstall deletes the legacy controller token"
else
    fail "WIRING: undeploy_wva_controller must call wva_delete_legacy_controller_token"
fi

if ! declare -F wva_remove_pod_writers >/dev/null; then
    fail "POD WRITES: wva_remove_pod_writers is not defined -- an uninstall leaves role/wva-pod-writer in every workload namespace"
else
    # Ours in ns-a alone, ours beside another controller's in ns-b, only
    # another's in ns-c.
    PW_RBS="ns-a wva-pod-writer-$PW_SUFFIX
ns-b wva-pod-writer-$PW_SUFFIX
ns-b wva-pod-writer-0000abcd
ns-c wva-pod-writer-0000abcd"
    pw_case wva_remove_pod_writers wva-system
    dels="$(grep '^delete' "$KUBECTL_LOG" | sort | tr '\n' ';')"
    want="delete role wva-pod-writer -n ns-a --ignore-not-found;delete rolebinding wva-pod-writer-$PW_SUFFIX -n ns-a --ignore-not-found;delete rolebinding wva-pod-writer-$PW_SUFFIX -n ns-b --ignore-not-found;"
    if [ "$dels" = "$want" ]; then
        ok "POD WRITES: uninstall removes this controller's bindings, and the Role only where no other controller binds it"
    else
        fail "POD WRITES: uninstall deleted the wrong things" "got:  $dels
want: $want"
    fi

    PW_RB_FORBIDDEN=1 pw_case wva_remove_pod_writers wva-system
    if ! grep -q '^delete' "$KUBECTL_LOG" && printf '%s' "$out" | grep -q 'Could not list RoleBindings in: cluster-wide'; then
        ok "POD WRITES: an unreadable RoleBinding list is reported, with how to find the leftovers"
    else
        fail "POD WRITES: an unreadable list must be reported, not passed over" "$out"
    fi

    PW_RB_FORBIDDEN=1 pw_case wva_remove_pod_writers wva-system ns-a ns-z
    if grep -q "^delete rolebinding wva-pod-writer-$PW_SUFFIX -n ns-a" "$KUBECTL_LOG" \
        && ! printf '%s' "$out" | grep -q 'Could not list'; then
        ok "POD WRITES: a namespace-scoped uninstall searches the namespaces it was given"
    else
        fail "POD WRITES: refused cluster-wide, the given namespaces must be searched" "$out"
    fi

    PW_DELETE_FORBIDDEN="ns-a" pw_case wva_remove_pod_writers wva-system
    if printf '%s' "$out" | grep -q "rolebinding/wva-pod-writer-$PW_SUFFIX in ns-a"; then
        ok "POD WRITES: a grant the uninstall may not delete is listed"
    else
        fail "POD WRITES: a refused delete must be listed" "$out"
    fi
    # Listed WITH the command, not "a cluster admin deletes them by hand".
    if printf '%s' "$out" | grep -q "kubectl delete rolebinding wva-pod-writer-$PW_SUFFIX -n ns-a"; then
        ok "POD WRITES: each grant the uninstall may not delete comes with its delete command"
    else
        fail "POD WRITES: a refused delete must print kubectl delete rolebinding <b> -n <ns>" "$out"
    fi
    PW_RB_FORBIDDEN=1 pw_case wva_remove_pod_writers wva-system
    if printf '%s' "$out" | grep -q "kubectl get rolebinding -A --field-selector metadata.name=wva-pod-writer-$PW_SUFFIX"; then
        ok "POD WRITES: an unreadable list names the exact listing command for the leftovers"
    else
        fail "POD WRITES: an unreadable list must print the field-selector listing" "$out"
    fi
    unset PW_RBS

    if tr -d '\r' < "$ROOT/deploy/lib/cleanup.sh" | grep -qE '^[[:space:]]+wva_remove_pod_writers '; then
        ok "POD WRITES: undeploy_wva_controller removes the grants"
    else
        fail "POD WRITES: cleanup.sh never calls wva_remove_pod_writers"
    fi
    # After the delete, and gated on the Deployment being gone: removed first, a
    # delete that then failed left a running controller without patch on pods.
    cl="$(tr -d '\r' < "$ROOT/deploy/lib/cleanup.sh")"
    remove_at="$(printf '%s\n' "$cl" | grep -nE '^[[:space:]]+wva_remove_pod_writers ' | head -1 | cut -d: -f1)"
    delete_at="$(printf '%s\n' "$cl" | grep -nE 'kubectl delete -f - --ignore-not-found' | head -1 | cut -d: -f1)"
    gate_at="$(printf '%s\n' "$cl" | grep -nE 'get deployment wva-controller-manager -n "\$WVA_NS"' | head -1 | cut -d: -f1)"
    if [ -n "$remove_at" ] && [ -n "$delete_at" ] && [ -n "$gate_at" ] \
        && [ "$delete_at" -lt "$gate_at" ] && [ "$gate_at" -lt "$remove_at" ]; then
        ok "POD WRITES: uninstall removes the grants after the delete, once the Deployment is gone"
    else
        fail "POD WRITES: wva_remove_pod_writers must follow the delete and a Deployment-gone check (delete ${delete_at:-none}, check ${gate_at:-none}, remove ${remove_at:-none})"
    fi
fi

# --- GO -----------------------------------------------------------------------
#
# The install targets must apply the committed manifests. A prerequisite on
# `manifests` (controller-gen, a type-check of ./..., a rewrite of a tracked
# RBAC file) or `kustomize` makes Go an undeclared install prerequisite.
for target in setup-prereqs deploy-wva deploy-wva-on-k8s deploy-wva-on-openshift; do
    prereqs="$(tr -d '\r' < "$ROOT/Makefile" | awk -v t="$target" '
        index($0, t ":") == 1 { line = substr($0, length(t) + 2); sub(/##.*/, "", line); print line; exit }')"
    if printf '%s' "$prereqs" | grep -qwE 'manifests|generate|controller-gen|kustomize|build'; then
        fail "make $target depends on a code-generation/build step:$prereqs"
    else
        ok "make $target applies the committed manifests (prerequisites:${prereqs:- none})"
    fi
done

# --- TOKEN --------------------------------------------------------------------
#
# The OpenShift overlay used to mint a non-expiring token Secret for the
# controller's own ServiceAccount. It is no longer rendered, so nothing but a
# delete by name removes it from an existing install.
if ! declare -F wva_delete_legacy_controller_token >/dev/null; then
    fail "TOKEN: wva_delete_legacy_controller_token is not defined -- an upgraded install keeps the controller token Secret"
else
    token_case() {
        : > "$KUBECTL_LOG"
        out="$(
            kubectl() {
                printf '%s\n' "$*" >> "$KUBECTL_LOG"
                case "$1 $2" in
                    "get secret")
                        case "$TOKEN_SA" in
                            absent)    echo 'Error from server (NotFound): secrets "wva-controller-manager-token" not found'; return 1 ;;
                            forbidden) echo 'Error from server (Forbidden): secrets "wva-controller-manager-token" is forbidden'; return 1 ;;
                            *)         printf '%s' "$TOKEN_SA" ;;
                        esac ;;
                esac
                return 0
            }
            wva_delete_legacy_controller_token wns 2>&1
        )"
    }
    TOKEN_SA=wva-controller-manager token_case
    if grep -qx 'delete secret wva-controller-manager-token -n wns' "$KUBECTL_LOG"; then
        ok "TOKEN: the controller token Secret is deleted"
    else
        fail "TOKEN: the controller token Secret must be deleted" "$(cat "$KUBECTL_LOG")"
    fi
    TOKEN_SA=someone-else token_case
    if grep -q '^delete' "$KUBECTL_LOG"; then
        fail "TOKEN: a Secret of that name for another account is not ours to delete" "$(cat "$KUBECTL_LOG")"
    else
        ok "TOKEN: a Secret of that name for another account is left alone"
    fi
    TOKEN_SA=forbidden token_case
    if ! grep -q '^delete' "$KUBECTL_LOG" && printf '%s' "$out" | grep -q 'Could not check'; then
        ok "TOKEN: Forbidden is reported, not taken for absent"
    else
        fail "TOKEN: Forbidden must warn, not pass silently" "$out"
    fi
fi

# --- CHART --------------------------------------------------------------------
#
# kube-prometheus-stack is one release shared by every WVA on the cluster. With
# no --version, each install took whatever the repository served that day and a
# re-run upgraded everyone's Prometheus to it.
tr -d '\r' < "$ROOT/deploy/lib/deploy_prometheus_kube_stack.sh" > "$WORK/deploy_prometheus_kube_stack.sh"
kps_default="$(tr -d '\r' < "$ROOT/deploy/install.sh" | sed -n 's/^KUBE_PROMETHEUS_STACK_CHART_VERSION=\${KUBE_PROMETHEUS_STACK_CHART_VERSION:-\([^}]*\)}$/\1/p')"
# KPS_LIST is what `helm list -o json` answers: the releases already installed.
KPS_LIST='[]'
kps_run() {
    : > "$KUBECTL_LOG"
    (
        openssl() { return 0; }
        helm() {
            printf 'helm %s\n' "$*" >> "$KUBECTL_LOG"
            case "$1" in
                list)
                    if [ "$KPS_LIST" = FAIL ]; then
                        echo 'Error: Kubernetes cluster unreachable: Get "https://api:6443/version": dial tcp: i/o timeout' >&2
                        return 1
                    fi
                    printf '%s\n' "$KPS_LIST" ;;
            esac
            return 0
        }
        install_operational_dashboard() { :; }
        MONITORING_NAMESPACE=mon PROMETHEUS_SECRET_NAME=s PROMETHEUS_PORT=9090 PROMETHEUS_URL=u \
            DEPLOY_OPERATIONAL_DASHBOARD=false SKIP_HELM_REPO_UPDATE=true
        # shellcheck disable=SC1091
        . "$WORK/deploy_prometheus_kube_stack.sh"
        deploy_prometheus_kube_stack
    ) > "$WORK/kps.out" 2>&1
}
KUBE_PROMETHEUS_STACK_CHART_VERSION="$kps_default" kps_run
kps_helm="$(grep '^helm upgrade --install kube-prometheus-stack' "$KUBECTL_LOG")"
if [ -n "$kps_default" ] && printf '%s' "$kps_helm" | grep -q -- "--version $kps_default"; then
    ok "kube-prometheus-stack is installed at the pinned chart version ($kps_default)"
else
    fail "kube-prometheus-stack must be installed with --version (install.sh default: '${kps_default}')" "$kps_helm"
fi
KUBE_PROMETHEUS_STACK_CHART_VERSION="" kps_run
if grep -q '^helm upgrade --install kube-prometheus-stack' "$KUBECTL_LOG"; then
    fail "an empty KUBE_PROMETHEUS_STACK_CHART_VERSION must refuse, not install unpinned" "$(cat "$WORK/kps.out")"
else
    ok "an empty KUBE_PROMETHEUS_STACK_CHART_VERSION refuses rather than installing unpinned"
fi
# The release is shared. A newer one another install put there is kept, not
# downgraded to this install's default pin -- unless the pin was set on purpose.
kps_version_used() { grep '^helm upgrade --install kube-prometheus-stack' "$KUBECTL_LOG" | sed -n 's/.*--version \([^ ]*\).*/\1/p'; }
KPS_LIST='[{"name":"kube-prometheus-stack","chart":"kube-prometheus-stack-99.1.0"}]'
KUBE_PROMETHEUS_STACK_CHART_VERSION="$kps_default" KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT='' kps_run
if [ "$(kps_version_used)" = 99.1.0 ] && grep -q 'newer than this install' "$WORK/kps.out"; then
    ok "a newer installed kube-prometheus-stack is kept, with a warning, not downgraded"
else
    fail "a newer installed kube-prometheus-stack must be kept (want --version 99.1.0 and a warning)" "$(kps_version_used); $(cat "$WORK/kps.out")"
fi
KUBE_PROMETHEUS_STACK_CHART_VERSION="$kps_default" KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT="$kps_default" kps_run
if [ "$(kps_version_used)" = "$kps_default" ]; then
    ok "an explicit KUBE_PROMETHEUS_STACK_CHART_VERSION is honoured over a newer installed release"
else
    fail "an explicit chart version must win (want $kps_default)" "$(kps_version_used)"
fi
KPS_LIST='[{"name":"kube-prometheus-stack","chart":"kube-prometheus-stack-80.0.0"}]'
KUBE_PROMETHEUS_STACK_CHART_VERSION="$kps_default" KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT='' kps_run
if [ "$(kps_version_used)" = "$kps_default" ] && ! grep -q 'newer than this install' "$WORK/kps.out"; then
    ok "an older installed kube-prometheus-stack is upgraded to the pin, quietly"
else
    fail "an older installed release must be upgraded to the pin $kps_default" "$(kps_version_used); $(cat "$WORK/kps.out")"
fi
# Versions compare as versions, not as text: 92.10.0 is newer than 92.9.0.
KPS_LIST='[{"name":"kube-prometheus-stack","chart":"kube-prometheus-stack-92.10.0"}]'
KUBE_PROMETHEUS_STACK_CHART_VERSION=92.9.0 KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT='' kps_run
if [ "$(kps_version_used)" = 92.10.0 ] && grep -q 'newer than this install' "$WORK/kps.out"; then
    ok "an installed 92.10.0 is newer than a 92.9.0 pin: kept"
else
    fail "92.10.0 installed against a 92.9.0 pin must be kept (a text sort calls it older)" "$(kps_version_used); $(cat "$WORK/kps.out")"
fi
KPS_LIST='[{"name":"kube-prometheus-stack","chart":"kube-prometheus-stack-92.9.0"}]'
KUBE_PROMETHEUS_STACK_CHART_VERSION=92.10.0 KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT='' kps_run
if [ "$(kps_version_used)" = 92.10.0 ] && ! grep -q 'newer than this install' "$WORK/kps.out"; then
    ok "an installed 92.9.0 is older than a 92.10.0 pin: upgraded, quietly"
else
    fail "92.9.0 installed against a 92.10.0 pin must be upgraded to the pin" "$(kps_version_used); $(cat "$WORK/kps.out")"
fi
# A list that failed is not "no release". Read as one, a transient error or a
# Forbidden list installed the pin over a newer shared release.
KPS_LIST=FAIL
KUBE_PROMETHEUS_STACK_CHART_VERSION="$kps_default" KUBE_PROMETHEUS_STACK_CHART_VERSION_EXPLICIT='' kps_run
if ! grep -q '^helm upgrade --install kube-prometheus-stack' "$KUBECTL_LOG" \
    && grep -q 'cluster unreachable' "$WORK/kps.out" && grep -q 'Could not list the Helm releases' "$WORK/kps.out"; then
    ok "a failed helm list refuses, with helm's error, rather than installing the pin"
else
    fail "a failed helm list must refuse with its error, not install $kps_default" "$(cat "$WORK/kps.out")"
fi
KPS_LIST='[]'

# --- REGISTRATIONS AT CLUSTER SCOPE ------------------------------------------
#
# A cluster-scoped controller registers workloads in every namespace, never in
# its own. The preflight counted its own namespace, said "none yet" beside a
# cluster full of ScaledObjects, and printed plan/apply commands without
# SCOPE=cluster, which scan only the controller's namespace and find nothing.
#
# so_report <scope> echoes what wva_report_scaledobjects logged, against a
# stub with two WVA ScaledObjects in team-a and team-b and none in wva-system.
SO_TWO='{"items":[
 {"metadata":{"namespace":"team-a","name":"a"},"spec":{"triggers":[{"metadata":{"scalerAddress":"wva-external-scaler.wva-system.svc:9090"}}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"namespace":"team-b","name":"b"},"spec":{"triggers":[{"metadata":{"scalerAddress":"wva-external-scaler.wva-system.svc:9090"}}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}'
so_report() {
    (
        WVA_SCOPE="$1"
        kubectl() {
            case "$*" in
                "get scaledobject -A -o json")
                    if [ -n "${SO_LIST:-}" ]; then printf '%s' "$SO_LIST"
                    elif [ -n "${SO_NONE:-}" ]; then echo '{"items":[]}'
                    else printf '%s' "$SO_TWO"; fi ;;
                "get scaledobject -n "*) echo '{"items":[]}' ;;
                *) return 1 ;;
            esac
        }
        log_info() { printf 'INFO %s\n' "$1"; }
        log_success() { printf 'OK %s\n' "$1"; }
        log_warning() { printf 'WARN %s\n' "$1"; }
        wva_report_scaledobjects
    ) 2>&1
}
out="$(so_report cluster)"
if printf '%s' "$out" | grep -q '2 workload(s) on the cluster already registered'; then
    ok "cluster scope: ScaledObjects in other namespaces are counted"
else
    fail "cluster scope must count ScaledObjects in every namespace" "$out"
fi
out="$(SO_NONE=1 so_report cluster)"
if printf '%s' "$out" | grep -q 'make scaledobjects-plan SCOPE=cluster ' \
    && printf '%s' "$out" | grep -q 'make scaledobjects-apply SCOPE=cluster '; then
    ok "cluster scope: the registration commands carry SCOPE=cluster"
else
    fail "cluster scope: the printed plan/apply commands must carry SCOPE=cluster" "$out"
fi
out="$(so_report namespace)"
if printf '%s' "$out" | grep -q 'none in wva-system registered with this scaling manager yet' && ! printf '%s' "$out" | grep -q 'SCOPE=cluster'; then
    ok "namespace scope: only the controller's namespace is read"
else
    fail "namespace scope must read only its own namespace" "$out"
fi

# The printed commands must run as printed. Without WVA_NS, apply looks for the
# scaler in the default controller namespace, finds none and exits 2 (#358).
out="$(SO_NONE=1 so_report cluster)"
if printf '%s' "$out" | grep -q 'make scaledobjects-plan SCOPE=cluster WVA_NS=wva-system WVA_DEFAULT_SO_PLAN=' \
    && printf '%s' "$out" | grep -q 'make scaledobjects-apply SCOPE=cluster WVA_NS=wva-system WVA_DEFAULT_SO_PLAN='; then
    ok "cluster scope: the registration commands carry WVA_NS"
else
    fail "cluster scope: the printed plan/apply commands must carry WVA_NS=wva-system" "$out"
fi
out="$(SO_LIST='{"items":[{"metadata":{"namespace":"team-a","name":"a"},"spec":{"triggers":[{"metadata":{"scalerAddress":"wva-external-scaler.wva-system.svc:9090"}}]},"status":{"conditions":[{"type":"Ready","status":"False","message":"stale"}]}}]}' so_report cluster)"
if printf '%s' "$out" | grep -q 'make scaledobjects-apply SCOPE=cluster WVA_NS=wva-system WVA_DEFAULT_SO_PLAN=<plan>'; then
    ok "cluster scope: the not-Ready recreate command carries WVA_NS"
else
    fail "cluster scope: the not-Ready recreate command must carry SCOPE=cluster WVA_NS=wva-system" "$out"
fi

# The count is ScaledObjects naming this scaler. Upstream ones on the same
# workloads are not "none on the cluster": say what was counted, and name them.
out="$(SO_LIST='{"items":[{"metadata":{"namespace":"team-a","name":"up"},"spec":{"triggers":[{"type":"prometheus","metadata":{"serverAddress":"http://prom:9090"}}]}}]}' so_report cluster)"
if printf '%s' "$out" | grep -q 'none on the cluster registered with this scaling manager yet' \
    && printf '%s' "$out" | grep -q '1 other ScaledObject(s) on the cluster use another scaler' \
    && ! printf '%s' "$out" | grep -q 'none on the cluster yet'; then
    ok "cluster scope: an upstream ScaledObject is named, not reported as none on the cluster"
else
    fail "cluster scope: with only an upstream ScaledObject, say none registered with this scaling manager and name the other one" "$out"
fi
out="$(SO_NONE=1 so_report cluster)"
if ! printf '%s' "$out" | grep -q 'other ScaledObject'; then
    ok "cluster scope: no other ScaledObjects, no line about them"
else
    fail "cluster scope: with no ScaledObjects at all, nothing may be said about other ones" "$out"
fi

# The node read, with no physical limiter: the admin phase grants it, so a run
# that includes that phase must not warn that the controller cannot list nodes.
node_note() {
    (
        WVA_SCOPE=cluster INSTALL_PHASE="$1"
        can_i_as() { echo no; }
        log_info() { printf 'INFO %s\n' "$1"; }
        log_success() { printf 'OK %s\n' "$1"; }
        log_warning() { printf 'WARN %s\n' "$1"; }
        log_error() { printf 'ERROR %s\n' "$1"; }
        wva_check_limiter_node_read "" "system:serviceaccount:wva-system:wva-controller-manager"
    ) 2>&1
}
if ! node_note all | grep -q '^WARN'; then
    ok "no limiter, INSTALL_PHASE=all: the node read the admin phase grants is not warned about"
else
    fail "INSTALL_PHASE=all must not warn about a node read its admin phase grants" "$(node_note all)"
fi
if node_note wva | grep -q '^WARN.*cannot list nodes'; then
    ok "no limiter, INSTALL_PHASE=wva: a missing node read is still warned about"
else
    fail "INSTALL_PHASE=wva must still warn about a missing node read" "$(node_note wva)"
fi

echo ""
echo "check-install-flow: $PASS passed, $FAIL failed"
if [ $((PASS + FAIL)) -ne "$EXPECT_CASES" ]; then
    echo "check-install-flow: $((PASS + FAIL)) case(s) ran, $EXPECT_CASES expected -- the run did not finish, or a case was added without updating EXPECT_CASES"
    exit 1
fi
[ "$FAIL" -eq 0 ]
