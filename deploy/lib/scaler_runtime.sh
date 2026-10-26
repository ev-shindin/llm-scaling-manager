#!/usr/bin/env bash
#
# Scaler backend deployment/runtime helpers for deploy/install.sh.
# Requires vars: MONITORING_NAMESPACE, KEDA_NAMESPACE, KEDA_CHART_VERSION.
# Requires funcs: log_info/log_warning/log_success/log_error,
# should_skip_helm_repo_update(), version_at_least() (prereqs.sh), wva_any_pod_ready()/wva_pods_exist() (common.sh).
#

# KEDA_FALLBACK_MIN_VERSION is the oldest KEDA whose ScaledObject has
# `spec.fallback.behavior` (currentReplicasIfHigher), added in KEDA v2.17.0
# ("Add Fallback option `behavior` for dynamic fallback calculation", kedacore
# #6450). It is NOT a minimum for the install: `external-push` is far older
# (2.0), and on an older KEDA the ScaledObjects are written with no fallback
# block at all (so_keda_fallback_probe, scaledobject.sh). The preflight says so
# up front, as a warning, rather than leaving it to the first plan.
KEDA_FALLBACK_MIN_VERSION="${KEDA_FALLBACK_MIN_VERSION:-2.17}"

# KEDA_TESTED_VERSION is the KEDA minor the e2e suites run against: CI's kind
# suites install chart kedacore/keda at install.sh's default KEDA_CHART_VERSION,
# and hack/check-install-flow.sh fails when the two part.
KEDA_TESTED_VERSION="2.21"
# KEDA_SUPPORTED_VERSIONS are the minors reported as supported, in order and
# each once: 2.18, which the benchmarks have run on; 2.19; 2.21, the first
# release with the CVE-2026-77524 fix, which the kind suites have passed on;
# and the tested one, whichever it is. Nothing in the code needs any of them:
# any other version is reported as untested, and the install goes on.
KEDA_SUPPORTED_VERSIONS="$(printf '%s\n' 2.18 2.19 2.21 "$KEDA_TESTED_VERSION" | sort -t. -k1,1n -k2,2n -u | paste -sd' ' -)"
# wva_keda_supported_text echoes the supported minors as prose: "2.18, 2.19 and 2.21".
wva_keda_supported_text() {
    printf '%s\n' $KEDA_SUPPORTED_VERSIONS | paste -sd, - | sed 's/,\([^,]*\)$/ and \1/; s/,/, /g'
}

# CVE-2026-77524 (GHSA-637c-6jxx-4rwm): a tenant who may write a
# TriggerAuthentication can make the KEDA operator send its own ServiceAccount
# token to an address the tenant controls. The advisory lists the operator up to
# 2.20.2; KEDA 2.21.0 fixes it.
KEDA_CVE_ID="CVE-2026-77524"
KEDA_CVE_FIXED_VERSION="2.21.0"

# KEDA_UNWIRED_FIXED_VERSION is the first KEDA that does not leave a new
# ScaledObject's HPA on the CPU default when the scaler is unreachable. Before
# it, KEDA built the HPA from the empty metric spec a failed call returned
# (kedacore/keda#7777 defers it) and kept a broken gRPC connection to the
# scaler for up to two minutes after a restart or outage, so any ScaledObject
# created in that window stayed unwired.
KEDA_UNWIRED_FIXED_VERSION="2.20.0"

# The namespace OpenShift's Custom Metrics Autoscaler runs in, where its
# ClusterServiceVersion carries the version.
KEDA_CMA_NAMESPACE="${KEDA_CMA_NAMESPACE:-openshift-keda}"

# wva_keda_operator_version sets WVA_KEDA_VERSION to the KEDA operator's version
# (no leading v), read from the image tag of the operator Deployment found
# cluster-wide by KEDA_OPERATOR_LABEL_SELECTOR. When it cannot be read it is
# empty, and WVA_KEDA_VERSION_WHY says why: Forbidden is "could not check",
# never absent and never a pass.
wva_keda_operator_version() {
    local out rc=0 image tag
    WVA_KEDA_VERSION=""
    WVA_KEDA_VERSION_WHY=""
    out="$(kubectl get deployments -A -l "$KEDA_OPERATOR_LABEL_SELECTOR" \
        -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>&1)" || rc=$?
    if [ "$rc" -ne 0 ]; then
        if printf '%s' "$out" | grep -qi 'forbidden'; then
            WVA_KEDA_VERSION_WHY="listing Deployments cluster-wide is not permitted for you"
        else
            WVA_KEDA_VERSION_WHY="reading the operator Deployment failed: $(printf '%s\n' "$out" | head -1)"
        fi
        return 0
    fi
    image="$(printf '%s' "$out" | tr -d '[:space:]')"
    if [ -z "$image" ]; then
        WVA_KEDA_VERSION_WHY="no Deployment labelled $KEDA_OPERATOR_LABEL_SELECTOR was found (other distributions label it differently)"
        return 0
    fi
    case "$image" in
        *@*)
            WVA_KEDA_VERSION_WHY="the operator image $image is pinned by digest, which names no version"
            return 0 ;;
    esac
    # The tag is after the last ':' of the last path segment, so a registry
    # port (host:5000/keda) is not read as one.
    tag="${image##*/}"
    case "$tag" in
        *:*) tag="${tag##*:}" ;;
        *)   tag="" ;;
    esac
    tag="${tag#v}"
    if ! [[ "$tag" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
        WVA_KEDA_VERSION_WHY="the operator image $image has no version tag (${tag:-none})"
        return 0
    fi
    WVA_KEDA_VERSION="$tag"
}

# wva_keda_version_judge <version> <what> <advice> reports a KEDA version:
# supported, or untested; too old for a fallback; carrying KEDA_CVE_ID, and,
# before KEDA_UNWIRED_FIXED_VERSION, the bug that leaves a ScaledObject unwired
# (one warning for both). It is information only and always returns 0: KEDA's
# external-push trigger is far older than any of these, so no version is
# refused.
wva_keda_version_judge() {
    local v="$1" what="$2" advice="$3" minor
    minor="$(printf '%s' "$v" | cut -d. -f1-2)"
    case " ${KEDA_SUPPORTED_VERSIONS} " in
        *" ${minor} "*)
            log_info "${what}: supported (KEDA $(wva_keda_supported_text); CI tests ${KEDA_TESTED_VERSION})." ;;
        *)
            log_warning "${what}: untested, and not refused. Supported: KEDA $(wva_keda_supported_text) (CI tests ${KEDA_TESTED_VERSION}). The install goes on." ;;
    esac
    if [ "${WVA_KEDA_FALLBACK_WARNED:-false}" != true ] && ! version_at_least "$v" "$KEDA_FALLBACK_MIN_VERSION"; then
        WVA_KEDA_FALLBACK_WARNED=true
        wva_keda_old_warning "the operator is ${v}"
    fi
    if ! version_at_least "$v" "$KEDA_UNWIRED_FIXED_VERSION"; then
        log_warning "${what} carries ${KEDA_CVE_ID}: a tenant who may write a TriggerAuthentication can obtain the KEDA operator's ServiceAccount token. It also has a KEDA bug fixed in ${KEDA_UNWIRED_FIXED_VERSION}: a ScaledObject created soon after the scaler restarts or is unreachable can stay unwired, its HPA left on the CPU default. Until KEDA is upgraded, after every WVA restart or rollout run  make scaledobjects-rewire  (or restart the KEDA operator). Advice only: ${advice}"
    elif ! version_at_least "$v" "$KEDA_CVE_FIXED_VERSION"; then
        log_warning "${what} carries ${KEDA_CVE_ID}: a tenant who may write a TriggerAuthentication can obtain the KEDA operator's ServiceAccount token. Advice only: ${advice}"
    fi
    return 0
}

# wva_keda_cma_version_report reports OpenShift's Custom Metrics Autoscaler
# version from its ClusterServiceVersion. Its versions are Red Hat's and CI pins
# none, so it is reported and not compared with KEDA's.
wva_keda_cma_version_report() {
    local out rc=0 v
    out="$(kubectl get csv -n "$KEDA_CMA_NAMESPACE" \
        -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.version}{"\n"}{end}' 2>&1)" || rc=$?
    if [ "$rc" -ne 0 ]; then
        if printf '%s' "$out" | grep -qi 'forbidden'; then
            log_warning "KEDA: could not check the Custom Metrics Autoscaler version (reading ClusterServiceVersions in $KEDA_CMA_NAMESPACE is not permitted for you)."
        else
            log_warning "KEDA: could not check the Custom Metrics Autoscaler version: $(printf '%s\n' "$out" | head -1)"
        fi
        return 0
    fi
    v="$(printf '%s\n' "$out" | awk '$1 ~ /^custom-metrics-autoscaler/ { print $2; exit }')"
    if [ -z "$v" ]; then
        log_warning "KEDA: could not check the Custom Metrics Autoscaler version (no custom-metrics-autoscaler ClusterServiceVersion in $KEDA_CMA_NAMESPACE)."
        return 0
    fi
    log_warning "KEDA: Custom Metrics Autoscaler ${v}, untested: CI pins no version of it, and this install does not map it to a KEDA version. Check its release notes for fallback.behavior (KEDA ${KEDA_FALLBACK_MIN_VERSION}), the unwired-HPA fix (KEDA ${KEDA_UNWIRED_FIXED_VERSION}) and ${KEDA_CVE_ID} (fixed in KEDA ${KEDA_CVE_FIXED_VERSION})."
}

# wva_keda_version_report reports the version of a KEDA that is already on the
# cluster, once per run: the preflight and deploy_keda both call it, and
# SKIP_CHECKS=true skips the first.
wva_keda_version_report() {
    [ "${WVA_KEDA_VERSION_REPORTED:-false}" = true ] && return 0
    WVA_KEDA_VERSION_REPORTED=true
    if [ "${ENVIRONMENT:-kubernetes}" = openshift ]; then
        wva_keda_cma_version_report
        return 0
    fi
    wva_keda_operator_version
    if [ -z "$WVA_KEDA_VERSION" ]; then
        log_warning "KEDA: could not check its version (${WVA_KEDA_VERSION_WHY}). The install goes on. Supported: KEDA $(wva_keda_supported_text) (CI tests ${KEDA_TESTED_VERSION}); a fallback needs ${KEDA_FALLBACK_MIN_VERSION} or later, before ${KEDA_UNWIRED_FIXED_VERSION} a ScaledObject can stay unwired after a scaler restart, and ${KEDA_CVE_ID} is fixed in ${KEDA_CVE_FIXED_VERSION}. Check by hand: kubectl get deploy -A -l ${KEDA_OPERATOR_LABEL_SELECTOR} -o wide"
        return 0
    fi
    wva_keda_version_judge "$WVA_KEDA_VERSION" "KEDA ${WVA_KEDA_VERSION}" "consider upgrading KEDA to ${KEDA_CVE_FIXED_VERSION} or later. Re-running this installer never upgrades a KEDA that is there; a Helm-installed one upgrades with: helm upgrade ${KEDA_RELEASE_NAME:-keda} kedacore/keda -n ${KEDA_NAMESPACE:-keda-system} --version ${KEDA_CVE_FIXED_VERSION} --reuse-values"
}

# wva_keda_chart_version_report is the same judgement for the chart this run
# installs, once per run.
wva_keda_chart_version_report() {
    [ "${WVA_KEDA_VERSION_REPORTED:-false}" = true ] && return 0
    WVA_KEDA_VERSION_REPORTED=true
    wva_keda_version_judge "${KEDA_CHART_VERSION:-}" "KEDA chart ${KEDA_CHART_VERSION:-} (this run installs it)" \
        "consider KEDA_CHART_VERSION=${KEDA_CVE_FIXED_VERSION} or later."
}

# wva_keda_installs_itself reports whether THIS run installs KEDA (so its absence
# now is expected, not a fault): kind-emulator always goes the Helm path, and
# Kubernetes does when KEDA_HELM_INSTALL=true. Never the controller-only phase
# (INSTALL_PHASE=wva): KEDA is installed by the prerequisites phase, which that
# phase does not run, so KEDA_HELM_INSTALL=true there installed nothing while
# the summary said "installed by this run".
wva_keda_installs_itself() {
    [ "${INSTALL_PHASE:-all}" != "wva" ] || return 1
    [ "${ENVIRONMENT:-kubernetes}" = "kind-emulator" ] && return 0
    [ "${ENVIRONMENT:-kubernetes}" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" = "true" ]
}

# wva_keda_absent_advice echoes what to do about a cluster with no KEDA, per
# platform. One copy, used by the preflight and by deploy_keda.
wva_keda_absent_advice() {
    if [ "${ENVIRONMENT:-kubernetes}" = "openshift" ]; then
        printf '%s\n' "On OpenShift this installer does not install KEDA: it uses the platform's." \
            "Install the Custom Metrics Autoscaler operator (OperatorHub) and create its" \
            "KedaController (namespace openshift-keda), then re-run this command."
        return 0
    fi
    if [ "${INSTALL_PHASE:-all}" = "wva" ]; then
        printf '%s\n' "This is the controller-only phase (INSTALL_PHASE=wva), which never installs KEDA." \
            "Install KEDA on the cluster, or have a cluster admin let this installer do it:"
    else
        printf '%s\n' "Install KEDA on the cluster, or let this installer do it (cluster admin):"
    fi
    printf '%s\n' "    $(wva_make_cmd setup-prereqs KEDA_HELM_INSTALL=true)" \
        "  It installs chart kedacore/keda ${KEDA_CHART_VERSION:-<pinned>} into ${KEDA_NAMESPACE:-keda-system}. Then re-run this command."
}

# wva_keda_old_warning warns that this KEDA predates fallback.behavior, in the
# terms so_keda_fallback_probe uses when the plan finds the same thing: older
# KEDA is supported, its ScaledObjects just get no fallback.
wva_keda_old_warning() {
    local what="$1"
    if [ "${WVA_SO_FALLBACK:-true}" = "false" ]; then
        log_info "KEDA: ${what}; WVA_SO_FALLBACK=false, so nothing here needs fallback.behavior."
        return 0
    fi
    log_warning "KEDA: ${what} -- fallback.behavior arrived in KEDA ${KEDA_FALLBACK_MIN_VERSION}, so the ScaledObjects this install writes get NO fallback. An older KEDA would prune the field and apply a static fallback that forces minReplicaCount. Upgrade KEDA to ${KEDA_FALLBACK_MIN_VERSION} or later to get one."
}

# check_keda_prerequisite answers "is there a KEDA to act on the ScaledObjects
# WVA needs?" BEFORE anything is created.
#
# It used to be answered only by deploy_keda, which ran after the controller had
# been applied -- so a cluster with no KEDA passed `make check-prereqs`, then got
# namespaces, RBAC and a running controller, and only then exit 1.
#
# Three readings, never two:
#   present     the ScaledObject CRD exists and (when the operator can be seen)
#               the operator is Ready. Older than KEDA 2.17 is still present, with
#               a warning: its ScaledObjects get no fallback.
#   absent      NotFound: an error, with what to do on this platform
#   unknown     Forbidden or unreadable: a warning. FORBIDDEN IS NOT ABSENT -- a
#               namespace admin cannot read CRDs, and refusing them for a KEDA that
#               is there all along sent them to an admin for nothing.
#
# Sets WVA_KEDA_STATE (present|absent-installing|unknown|skipped) for the summary,
# so that line reports what was measured rather than what was assumed.
check_keda_prerequisite() {
    WVA_KEDA_STATE=unknown
    if [ "${SCALER_BACKEND:-keda}" != "keda" ]; then
        WVA_KEDA_STATE=skipped
        log_info "KEDA: not checked (SCALER_BACKEND=${SCALER_BACKEND:-}). WVA needs KEDA to actuate; something else must provide it."
        return 0
    fi

    local probe rc=0
    # || rc=$?, never ; rc=$? -- under set -e the plain form exits on the very
    # NotFound this exists to explain.
    probe=$(kubectl get crd scaledobjects.keda.sh -o jsonpath='{.spec.versions[*].schema.openAPIV3Schema.properties.spec.properties.fallback.properties.behavior.type}' 2>&1) || rc=$?
    if [ $rc -ne 0 ]; then
        if printf '%s' "$probe" | grep -qi 'forbidden'; then
            log_warning "KEDA: cannot verify (reading CustomResourceDefinitions is not permitted for you), so this install assumes KEDA is present."
            log_warning "  If it is not, ScaledObjects stay unready and nothing scales. A cluster admin can confirm: kubectl get crd scaledobjects.keda.sh"
            wva_keda_version_report
            return 0
        fi
        if printf '%s' "$probe" | grep -qiE 'notfound|not found'; then
            if wva_keda_installs_itself; then
                WVA_KEDA_STATE=absent-installing
                log_info "KEDA: not on the cluster yet; this run installs it (chart kedacore/keda ${KEDA_CHART_VERSION:-})."
                wva_keda_chart_version_report
                return 0
            fi
            WVA_KEDA_STATE=absent
            log_error "KEDA is not installed (no scaledobjects.keda.sh CRD). WVA cannot actuate without it, and nothing has been applied yet.

$(wva_keda_absent_advice)"
        fi
        log_error "KEDA: could not check for the scaledobjects.keda.sh CRD:
    ${probe}
Nothing has been applied. Fix cluster access, or skip the check with SKIP_CHECKS=true."
    fi

    # The CRD decides whether fallback.behavior is admitted or pruned. Missing,
    # it is an older KEDA: a warning, never a refusal (see KEDA_FALLBACK_MIN_VERSION).
    local old=""
    if [ -z "$(printf '%s' "$probe" | tr -d '[:space:]')" ]; then
        old="its ScaledObject CRD has no spec.fallback.behavior"
    fi

    # The operator. Its pod labels differ between distributions (OpenShift's
    # Custom Metrics Autoscaler is one), so only a definite answer is fatal:
    # operator pods that exist and are none of them Ready.
    local ready_rc=0
    wva_any_pod_ready -A "$KEDA_OPERATOR_LABEL_SELECTOR" || ready_rc=$?
    case "$ready_rc" in
        0)
            WVA_KEDA_STATE=present
            log_success "KEDA: ScaledObject CRD present, operator Ready"
            ;;
        1)
            if wva_pods_exist -A "$KEDA_OPERATOR_LABEL_SELECTOR"; then
                if wva_keda_installs_itself; then
                    WVA_KEDA_STATE=absent-installing
                    log_warning "KEDA: operator pods exist and none is Ready; this run's Helm install will try to repair it."
                    return 0
                fi
                WVA_KEDA_STATE=absent
                log_error "KEDA: the operator pods ($KEDA_OPERATOR_LABEL_SELECTOR) exist and none is Ready, so no ScaledObject would be reconciled. Nothing has been applied.
    kubectl get pods -A -l $KEDA_OPERATOR_LABEL_SELECTOR"
            fi
            WVA_KEDA_STATE=present
            log_warning "KEDA: the CRD is present, but no operator pod labelled $KEDA_OPERATOR_LABEL_SELECTOR was found to confirm it is running (other distributions label it differently)."
            ;;
        *)
            WVA_KEDA_STATE=present
            log_info "KEDA: the CRD is present; whether its operator is Ready cannot be checked (listing pods cluster-wide is not permitted for you)."
            ;;
    esac
    if [ -n "$old" ]; then
        WVA_KEDA_FALLBACK_WARNED=true
        wva_keda_old_warning "$old"
    fi
    # The version of the KEDA this install will use. A version it cannot read
    # is a warning that says so, never a pass.
    wva_keda_version_report
    return 0
}

deploy_keda() {
    log_info "Deploying KEDA (scaler backend)..."
    # Deploy flow is non-interactive: missing/failed scaler backend is a hard failure.

    # OpenShift: KEDA is cluster-managed (OLM/operator); never Helm-install — avoids
    # ClusterRole/release conflicts with an existing platform KEDA.
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: assuming platform-managed KEDA — skipping Helm install"
        if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
            WVA_KEDA_STATE=present
            log_success "KEDA ScaledObject CRD is available on the cluster"
            wva_keda_version_report
        else
            log_error "OpenShift: scaledobjects.keda.sh CRD not found (SCALER_BACKEND=keda).

$(wva_keda_absent_advice)"
        fi
        return
    fi

    # Kubernetes (e.g. CKS, shared clusters): assume cluster-managed KEDA; never Helm unless opted in.
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: assuming cluster-managed KEDA — skipping Helm (set KEDA_HELM_INSTALL=true to install via Helm)"
        # Distinguish "absent" from "not allowed to look". A namespace admin cannot
        # read cluster-scoped CRDs, and treating that Forbidden as NotFound aborted
        # their install — for a KEDA that was present all along, and only AFTER the
        # controller had been applied, leaving it half-installed.
        # || keda_rc=$?, not ; keda_rc=$?. The plain form aborts the whole
        # install at this assignment the moment the CRD is absent, which is the
        # ONE case the message below exists to explain -- so the operator got
        # a bare exit 2 and a half-applied prerequisite set instead of being
        # told to set KEDA_HELM_INSTALL=true.
        local keda_probe keda_rc=0
        keda_probe=$(kubectl get crd scaledobjects.keda.sh 2>&1) || keda_rc=$?
        if [ $keda_rc -eq 0 ]; then
            # Measured here, so the summary says so even when SKIP_CHECKS=true
            # skipped the preflight that would otherwise have set it.
            WVA_KEDA_STATE=present
            log_success "KEDA ScaledObject CRD is available on the cluster"
            wva_keda_version_report
        elif printf '%s' "$keda_probe" | grep -qi 'forbidden'; then
            log_warning "Cannot check for the KEDA CRD (reading cluster-scoped CRDs is not permitted for you), so this install assumes KEDA is present."
            log_warning "  If it is not, ScaledObjects will stay unready and nothing will scale. Confirm with whoever administers the cluster."
            wva_keda_version_report
        else
            log_error "Kubernetes: scaledobjects.keda.sh CRD not found.

$(wva_keda_absent_advice)"
        fi
        return
    fi

    # Skip install if KEDA is already fully operational on the cluster.
    # Check CRD + operator pods + external metrics APIService to avoid false positives
    # from stale CRDs left behind after a prior uninstall.
    if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
        # Readiness, not `grep -q Running`. This decides whether to install KEDA
        # at all, so a "0/1 Running" operator answering yes here skips the install
        # that would have fixed it and leaves the cluster with a KEDA that never
        # reconciles anything.
        if wva_any_pod_ready -A "$KEDA_OPERATOR_LABEL_SELECTOR"; then
            if kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" >/dev/null 2>&1; then
                WVA_KEDA_STATE=present
                log_success "KEDA CRD, operator, and metrics APIService detected — skipping helm install"
                wva_keda_version_report
                return
            fi
        fi
        # Shared clusters (e.g. CKS) often pre-install KEDA without the exact pod label / APIService
        # shape our probe expects, but ClusterRole keda-operator already exists without Helm metadata.
        # Helm install then fails with ownership errors — skip Helm when that pattern is present.
        if kubectl get clusterrole keda-operator >/dev/null 2>&1; then
            keda_cr_managed_by=$(kubectl get clusterrole keda-operator -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null || true)
            if [ "$keda_cr_managed_by" != "Helm" ]; then
                WVA_KEDA_STATE=present
                log_info "KEDA CRD present and ClusterRole keda-operator is not Helm-managed — skipping Helm install (pre-installed KEDA)"
                wva_keda_version_report
                return
            fi
        fi
        log_warning "KEDA ScaledObject CRD found but operator or metrics APIService not detected; proceeding with helm install"
    fi

    wva_keda_chart_version_report
    kubectl create namespace "$KEDA_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
    if [ "$(should_skip_helm_repo_update)" = "true" ]; then
        log_info "Skipping helm repo update for KEDA (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    if ! helm upgrade -i "$KEDA_RELEASE_NAME" kedacore/keda \
        --version "$KEDA_CHART_VERSION" \
        -n "$KEDA_NAMESPACE" \
        --set prometheus.metricServer.enabled=true \
        --set prometheus.operator.enabled=true \
        --wait \
        --timeout=5m; then
        log_error "KEDA Helm installation failed (SCALER_BACKEND=keda)"
    else
        WVA_KEDA_STATE=absent-installing
        log_success "KEDA deployed in $KEDA_NAMESPACE"
    fi
}

