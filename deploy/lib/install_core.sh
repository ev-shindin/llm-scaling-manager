#!/usr/bin/env bash
#
# Core install orchestration for deploy/install.sh.
# Requires vars: ENVIRONMENT, SCRIPT_DIR, SKIP_CHECKS, deployment toggles.
# Requires funcs sourced by install.sh: parse_args(), check_prerequisites(),
# set_wva_logging_level(), create_namespaces(), deploy_*(), verify_deployment(), print_summary().
# llm-d install: see llm-d project guides or deploy/install-epp.sh for kind EPP setup.
#

# wva_missing_prereqs echoes the admin-owned objects that do not exist yet, one
# per line. Empty output means the cluster-admin phase has been run for this
# namespace. Quiet: the caller decides whether that is an error or a decision.
wva_missing_prereqs() {
    local kind name expected out rc
    expected="$(wva_rendered_prereq_objects)"
    [ -n "$expected" ] || { echo "UNRENDERABLE"; return 0; }
    while read -r kind name; do
        [ -n "$kind" ] && [ -n "$name" ] || continue
        # Cluster-scoped kinds are the exception; everything else is looked up in
        # WVA_NS. Naming only ServiceMonitor as namespaced was a bug: Role and
        # RoleBinding fell through to a lookup with no -n, which resolves against
        # the CALLER'S CURRENT CONTEXT namespace rather than the install
        # namespace. With a context pointing anywhere else, four objects that
        # exist were reported missing -- and the phase that reports it is the
        # tenant's, so it sent them to ask an admin to redo work already done.
        # That is the exact failure the Forbidden handling below was added to
        # prevent, arriving by a different route.
        # `out=$(...) || rc=$?`, never `out=$(...); rc=$?`. Under `set -e` the
        # plain form takes the shell down AT THE ASSIGNMENT when kubectl fails,
        # so rc is never read and the Forbidden handling below never runs. Here
        # that happens inside a command substitution, so the damage is quieter:
        # the subshell dies, the caller reads empty output, and empty means "no
        # prereqs are missing" -- the reassuring answer, not the true one.
        rc=0
        case "$kind" in
            Namespace|ClusterRole|ClusterRoleBinding|CustomResourceDefinition)
                out=$(kubectl get "$kind" "$name" 2>&1) || rc=$? ;;
            *)
                out=$(kubectl get "$kind" "$name" -n "$WVA_NS" 2>&1) || rc=$? ;;
        esac
        [ $rc -eq 0 ] && continue
        # A denied read is not an absent object, and cluster-scoped RBAC is
        # exactly what a tenant may not read. Reporting Forbidden as missing sent
        # them to ask an admin for objects that admin had already created.
        if printf '%s' "$out" | grep -qi 'forbidden'; then
            echo "UNVERIFIABLE $kind/$name"
            continue
        fi
        # Some objects serve one optional feature and arrived after installs
        # already existed. Refusing every upgrade until an admin re-runs prereqs,
        # for a feature the tenant may never use, would send them back to the
        # admin for nothing; the controller runs fine without them and logs
        # Forbidden where the feature needs them. Each is reported with the
        # feature it serves:
        #   wva-kueue-reader-*  the Kueue read, used only by a quota limiter
        #                       entry with kueue.enabled.
        #   wva-utilization-share-history-*  the utilization-share need history
        #                       store; without it a restart forgets the models'
        #                       peaks and caps their headroom for a window.
        #   wva-controller-status-*  the controller-status ConfigMap; without
        #                       it the controller scales as before and only
        #                       the published status is missing.
        #   wva-policy-namespace-reader-*  the Namespace get that finds a
        #                       published cluster policy. Without it the
        #                       controller runs on its own ConfigMap and reports
        #                       wva_policy_namespace_unverified=1.
        #   wva-learned-state-*  the learned-state snapshot
        #                       (LEARNED_STATE_SNAPSHOT); without it a restart
        #                       starts every model's learned capacity cold.
        case "$name" in
            wva-kueue-reader-*)
                echo "OPTIONAL $kind/$name|the Kueue quota source (a quota limiter entry with kueue.enabled)"
                continue ;;
            wva-policy-namespace-reader-*)
                echo "OPTIONAL $kind/$name|finding a cluster policy published in wva-policy (make enable-physical-limiter); without it that policy is NOT applied to this controller"
                continue ;;
            wva-utilization-share-history-role|wva-utilization-share-history-rolebinding)
                echo "OPTIONAL $kind/$name|the utilization-share need history, kept across controller restarts"
                continue ;;
            wva-learned-state-role|wva-learned-state-rolebinding)
                echo "OPTIONAL $kind/$name|the learned-state snapshot (LEARNED_STATE_SNAPSHOT), kept across controller restarts"
                continue ;;
            wva-controller-status-role|wva-controller-status-rolebinding)
                echo "OPTIONAL $kind/$name|the controller-status ConfigMap (the policy version and modes the controller is running with)"
                continue ;;
        esac
        # Two more that an admin phase may leave out ON PURPOSE, so a phase that
        # sees differently (a tenant cannot read KEDA's APIService or a
        # Prometheus CR) must not call them missing -- that reported them
        # MISSING on every --check and refused INSTALL_PHASE=wva forever:
        #   NetworkPolicy wva-external-scaler  written only when the admin
        #       phase KNOWS where KEDA runs (wva_resolve_scaler_keda_namespace).
        #   wva-metrics-reader-rolebinding-*  written only for a Prometheus
        #       account the install was told about (wva_prometheus_scraper_sa).
        case "$kind/$name" in
            NetworkPolicy/wva-external-scaler)
                echo "OPTIONAL $kind/$name|admitting only KEDA to the external scaler (:9090); without it the port is reachable by any Pod ($(wva_make_cmd setup-prereqs "KEDA_NAMESPACE=<ns>") writes it)"
                continue ;;
            ClusterRoleBinding/wva-metrics-reader-rolebinding-*)
                echo "OPTIONAL $kind/$name|Prometheus reading the controller's /metrics ($(wva_make_cmd setup-prereqs 'PROMETHEUS_SA=<ns>/<sa>') grants it)"
                continue ;;
        esac
        case "$kind" in
            Namespace|ClusterRole|ClusterRoleBinding|CustomResourceDefinition)
                echo "$kind/$name" ;;
            *)  echo "$kind/$name (in $WVA_NS)" ;;
        esac
    done <<< "$expected"

    # The manager Role and RoleBinding the prereqs phase writes in each managed
    # namespace, when the controller runs outside them (wva_apply_watch_namespace_rbac).
    [ -n "${WVA_WATCH_NS:-}" ] || return 0
    local role binding ns
    read -r role binding <<< "$(wva_watch_rbac_names)"
    for ns in $(wva_watch_words); do
        [ "$ns" != "$WVA_NS" ] || continue
        for kind in Role RoleBinding; do
            name="$role"; [ "$kind" = Role ] || name="$binding"
            rc=0
            out=$(kubectl get "$kind" "$name" -n "$ns" 2>&1) || rc=$?
            [ $rc -eq 0 ] && continue
            if printf '%s' "$out" | grep -qi 'forbidden'; then
                echo "UNVERIFIABLE $kind/$name in $ns"
            else
                echo "$kind/$name (in $ns)"
            fi
        done
    done
}

# wva_resolve_install_phase decides which half of the install is left to do,
# when the caller did not say.
#
# `all` as a fixed default made the tenant path carry INSTALL_PHASE=wva — a
# parameter whose only job was to state that the admin had already done their
# half, which is a question the cluster can answer. It runs BEFORE the permission
# check, because that check asks whether the caller can create everything the
# phase creates: a phase resolved after it would be checked against work nobody
# was going to do, which is how a namespace owner came to be told they could not
# install, for lacking rights to create objects that already existed.
wva_resolve_install_phase() {
    [ -z "${INSTALL_PHASE_EXPLICIT:-}" ] || return 0
    [ "$(wva_install_scope)" = "namespace" ] || return 0

    # Capability first, not existence: a tenant cannot READ a ClusterRole either,
    # so an existence check answers Forbidden for the very caller this serves,
    # which reads as "missing" and puts them back on the phase they cannot run.
    # Whether you may CREATE one is a SelfSubjectAccessReview — allowed for
    # everybody — and it answers the question that decides the phase: which half
    # is yours to do.
    if ! kubectl auth can-i create clusterroles >/dev/null 2>&1; then
        INSTALL_PHASE=wva
        export INSTALL_PHASE
        log_info "Installing the controller only: creating cluster-scoped objects is not permitted for you, and that is the half a cluster admin runs once with 'make setup-prereqs'."
        return 0
    fi

    # KEDA_HELM_INSTALL=true asks THIS run to install KEDA, and only the
    # prereqs half does that. Resolved to `wva`, an admin re-run whose RBAC was
    # already in place skipped deploy_keda and then failed the KEDA check it
    # had asked to satisfy. Kept unless the KEDA CRD is confirmed present; an
    # unreadable one is not taken for absent or present -- `all` is the phase
    # that can act either way, and its KEDA check says which it is.
    if [ "${SCALER_BACKEND:-keda}" = "keda" ] && [ "${KEDA_HELM_INSTALL:-false}" = "true" ] \
        && [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] \
        && ! kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
        log_info "Running both phases: KEDA_HELM_INSTALL=true and KEDA is not confirmed installed, and only the cluster-admin phase installs it."
        return 0
    fi

    [ -z "$(wva_missing_prereqs)" ] || return 0
    INSTALL_PHASE=wva
    export INSTALL_PHASE
    log_info "The cluster-admin prerequisites for $WVA_NS are already in place — installing the controller only. (INSTALL_PHASE=all redoes them.)"
}

# require_prereqs_present refuses a controller-only install when the admin phase
# has not run for this namespace.
#
# It names every missing object rather than letting `kubectl apply` fail on the
# first one: the person running this phase is, by design, the person who CANNOT
# create them, so the useful output is the list to hand to an admin — not a
# Forbidden on one object with the rest unknown.
require_prereqs_present() {
    local line missing=() unverifiable=() optional=()
    while read -r line; do
        [ -n "$line" ] || continue
        case "$line" in
            UNRENDERABLE)
                log_error "Could not render the install overlay, so whether the cluster-admin prerequisites exist could not be checked. Fix the overlay (try 'kubectl kustomize $(wva_overlay_dir)') and re-run; nothing has been applied." ;;
            "UNVERIFIABLE "*) unverifiable+=("${line#UNVERIFIABLE }") ;;
            "OPTIONAL "*) optional+=("${line#OPTIONAL }") ;;
            *) missing+=("$line") ;;
        esac
    done <<< "$(wva_missing_prereqs)"

    if [ ${#optional[@]} -ne 0 ]; then
        log_info "Optional cluster-admin prerequisites are absent; the controller installs without them:"
        local entry
        for entry in "${optional[@]}"; do
            log_info "  - ${entry%%|*}: needed only by ${entry#*|}"
        done
        log_info "  To add them, ask an admin to re-run '$(wva_make_cmd setup-prereqs)'."
    fi

    if [ ${#missing[@]} -ne 0 ]; then
        log_error "The cluster-admin prerequisites for $WVA_NS are not in place. Missing:
$(printf '  - %s\n' "${missing[@]}")

Ask a cluster admin to run, once, for this namespace:

    $(wva_make_cmd setup-prereqs)

after which this phase needs no cluster-scoped rights and you can re-run it, and
every later upgrade, yourself."
    fi

    if [ ${#unverifiable[@]} -ne 0 ]; then
        # Proceed: the install itself does not need these objects to be readable,
        # only to exist, and a controller whose RBAC is genuinely absent fails
        # visibly at readiness rather than silently.
        log_info "Cannot read some cluster-admin prerequisites (reading them is not permitted for you), so this cannot confirm they exist: ${unverifiable[*]}"
        log_info "  If they are missing, the controller will install and then fail to become ready; ask an admin to run '$(wva_make_cmd setup-prereqs)'."
        return 0
    fi
    log_success "Prerequisites are in place for $WVA_NS"
}


# print_prereqs_summary tells the admin what they just created and what to hand
# over. The handover is the point of the phase, so it is stated rather than left
# to be inferred from a list of objects.
print_prereqs_summary() {
    local scope platform
    scope="$(wva_install_scope)"
    platform=$([ "$ENVIRONMENT" = openshift ] && echo openshift || echo k8s)
    echo ""
    echo "=========================================="
    echo " Prerequisites ready for $WVA_NS"
    echo "=========================================="
    echo ""
    echo "  Created (or already present):"
    # Not the NetworkPolicy when this phase left it out: it was not created, and
    # the warning above says why and how to add it.
    wva_rendered_prereq_objects \
        | if [ "${WVA_SCALER_NP_APPLIED:-true}" = "false" ]; then grep -vx 'NetworkPolicy wva-external-scaler' || true; else cat; fi \
        | sed 's|^\([^ ]*\) |    \1/|'
    # What happened to KEDA, as measured -- not "if this cluster had none", which
    # promised an install the platform paths never do.
    case "${WVA_KEDA_STATE:-unknown}" in
        present)           echo "    KEDA: already on the cluster" ;;
        absent-installing) echo "    KEDA: installed by this run (Helm)" ;;
        skipped)           echo "    KEDA: not managed by this install (SCALER_BACKEND=${SCALER_BACKEND:-})" ;;
        *)                 echo "    KEDA: could not be verified (see the warning above)" ;;
    esac
    # The monitors added in the llm-d namespaces (wva_apply_llmd_monitors).
    local added
    for added in ${WVA_LLMD_MONITORS_ADDED[@]+"${WVA_LLMD_MONITORS_ADDED[@]}"}; do
        echo "    Monitors added in $added"
    done
    if [ "${WVA_SCALER_NP_APPLIED:-true}" = "false" ]; then
        echo ""
        case "${WVA_SCALER_NP_STATE:-}" in
            unknown|unrecognised)
                # Left as it is: one may exist, so "open" would be a guess.
                echo "  NOT written: NetworkPolicy/wva-external-scaler. Its state is UNKNOWN"
                echo "  (left as it is; see the warning above). Check:"
                echo "      kubectl get networkpolicy wva-external-scaler -n $WVA_NS"
                ;;
            *)
                echo "  NOT created: NetworkPolicy/wva-external-scaler, so the external scaler"
                echo "  port (:9090) is open to any Pod. See the warning above."
                ;;
        esac
    fi
    echo ""
    echo "  NOT created: the controller itself. Whoever owns $WVA_NS installs it,"
    echo "  and every later upgrade, with no cluster-scoped rights:"
    echo ""
    echo "      $(wva_make_cmd deploy-wva)"
    echo ""
    echo "  Re-run this phase only when the WVA version changes what it needs"
    echo "  cluster-wide (new RBAC rules), not for an ordinary controller upgrade."
    echo ""
}

# wva_set_monitors_planned sets WVA_MONITORS_PLANNED=true when this run (or,
# for the preflight, the install it previews) includes the admin phase and
# that phase adds missing monitors. The checks then report a missing monitor as
# work the phase does rather than refusing over it. Call it after
# wva_resolve_install_phase.
wva_set_monitors_planned() {
    WVA_MONITORS_PLANNED=false
    wva_llmd_monitors_wanted || return 0
    case "${INSTALL_PHASE:-all}" in
        all|prereqs) WVA_MONITORS_PLANNED=true ;;
    esac
    return 0
}

# wva_run_prereqs_phase runs the cluster-admin half, in dependency order.
#
# The scaler backend comes BEFORE the controller. It used to come last, so a
# cluster without KEDA reached deploy_keda's refusal with the controller already
# applied and running -- a half-install, and exactly the state the preflight
# exists to prevent. After the monitoring stack, because KEDA's Helm install
# creates ServiceMonitors and needs their CRD.
wva_run_prereqs_phase() {
    local phase="$1"
    create_namespaces

    deploy_monitoring_stack

    # The model-server and EPP monitors llm-d did not install, in each llm-d
    # namespace. After the monitoring stack, whose CRDs they need; before the
    # controller, which reads what they scrape.
    if [ "$DEPLOY_WVA" = "true" ]; then
        wva_apply_llmd_monitors
    fi

    # The EPPs of the managed pools authenticate /metrics scrapes as their own
    # ServiceAccount; without the cluster-scoped grant they answer 500
    # (deploy/lib/epp_auth.sh).
    if [ "$DEPLOY_WVA" = "true" ]; then
        wva_apply_epp_metrics_auth
    fi

    deploy_scaler_backend

    # Environment-specific WVA prerequisites (on OpenShift: the SA token
    # Secret and the service-ca bundle the ServiceMonitor authenticates with).
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_prerequisites

        # A cluster policy published BEFORE this install granted its read to
        # the controllers that existed then, not to this one -- which would
        # resolve to that policy and refuse to start for want of a RoleBinding.
        #
        # BEFORE deploy_wva_controller: with INSTALL_PHASE=all that call also
        # applies the Deployment, and a grant made after it raced the first
        # pod, which could resolve the policy, find no RoleBinding and refuse
        # to start. A RoleBinding may name a ServiceAccount that does not exist
        # yet, so nothing here needs the controller to be applied first.
        if [ "$(wva_install_scope)" = "namespace" ]; then
            pl_grant_published_policy_read "$WVA_NS" "${WVA_WATCH_NS:-$WVA_NS}"
            # The Kueue controller's Deployment and leader Lease, by name, in
            # the Kueue namespace: a namespace-scoped controller cannot read
            # them otherwise, and Kueue borrowing then freezes
            # (kueue-controller-down). A Role in another namespace, so not
            # part of the overlay (pl_grant_kueue_controller_read says why).
            # In KUEUE_NAMESPACE, else the kueue.controllerNamespace the
            # policy keeps (wva_kueue_borrow_wanted reads it).
            wva_kueue_borrow_wanted
            pl_grant_kueue_controller_read "$WVA_NS" wva-controller-manager "$WVA_KUEUE_KEPT_NS" \
                || log_warning "Could not grant the Kueue controller read in ${KUEUE_NAMESPACE:-${WVA_KUEUE_KEPT_NS:-kueue-system}}; Kueue borrowing, if counted, freezes as kueue-controller-down until it is granted."
            # Workloads and LocalQueues in every namespace, only when a policy
            # of this install counts cohort borrowing: the read reaches every
            # tenant's Workloads. Revoked when none does.
            pl_sync_kueue_borrow_read "$WVA_NS" wva-controller-manager \
                "$WVA_KUEUE_BORROW_WANTED" "${WVA_KUEUE_BORROW_UNREAD:-}" || true
            # Another controller granted the same read may borrow from the
            # same cohort; each counts only its own borrowing in flight.
            if [ "$WVA_KUEUE_BORROW_WANTED" = yes ]; then
                pl_warn_shared_cohort_borrowers "$WVA_NS/wva-controller-manager ${WVA_WATCH_NS:-$WVA_NS}
$(pl_kueue_borrowers_besides "$WVA_NS")"
            fi
        fi
        WVA_APPLY_SCOPE=$([ "$phase" = "all" ] && echo all || echo prereqs) \
            deploy_wva_controller

        # KEDA's side of the scaler TLS, when the certificates were written
        # before KEDA's API was served.
        if declare -F wva_scaler_tls_after_keda >/dev/null; then
            wva_scaler_tls_after_keda
        fi
    fi
}

main() {
    parse_args "$@"

    # RENDER=<dir>: the cluster-admin phase, written to <dir> for an admin to
    # run instead of applied (deploy/lib/common.sh, "Render mode"). First, so
    # every kubectl and helm call below goes through it.
    wva_render_init "${RENDER:-}"

    # Before ANY check that exempts kind-emulator -- the preflight and the install
    # both have one. A caller relying on CLUSTER_TYPE=kind auto-detection rather
    # than passing ENVIRONMENT=kind-emulator must resolve to it first, or the
    # exemption silently does not apply. This lived in the install path only, so
    # `CLUSTER_TYPE=kind` passed the install and failed the preflight on the same
    # cluster.
    if [[ "${CLUSTER_TYPE:-}" == "kind" ]]; then
        log_info "Kind cluster detected - setting environment to kind-emulated"
        ENVIRONMENT="kind-emulator"
    fi

    # Which cluster, before anything acts on one. Every command below resolves it
    # from the ambient kubeconfig, so the operator's only evidence that it picked
    # the intended cluster is that they remembered to check -- and an install that
    # lands on the wrong one still reports success.
    wva_report_cluster

    # Before anything reads WVA_NS — the check, the undeploy and the install all
    # need the same answer, or `export NAMESPACE=…` works for one and silently
    # not the others.
    wva_resolve_namespace
    # Says once, up front, when the selected scenario is not the supported one.
    wva_warn_unsupported_scope
    # WVA_WATCH_NS=auto or a list, settled once for every path below. Not on
    # the undeploy: it reads what the live Deployment manages.
    [ "$UNDEPLOY" = "true" ] || wva_resolve_watch_namespaces

    # NOTE on wva_require_namespace, which refuses to guess the managed namespace:
    # it is called in the CHECK and INSTALL paths below, NOT here, and NOT on the
    # undeploy path. Undeploy's input is WVA_NS -- the namespace the controller was
    # installed into -- and asking it for NAMESPACE would make removing an install
    # depend on knowing what that install managed.

    # Preflight-only mode: the same check the install runs, run on its own so it
    # can be answered before committing to an install that creates namespaces and
    # RBAC before it discovers a missing tool.
    if [ "${CHECK_ONLY:-false}" = "true" ]; then
        check_prerequisites
        # Environment-specific "checks" are only run when they actually check.
        # kind-emulator's creates and tears down the cluster and loads images —
        # it is setup wearing a check's name, and a preflight that provisions
        # infrastructure is not one you can safely run to ask a question.
        if [ "$ENVIRONMENT" = "kind-emulator" ]; then
            log_info "Skipping kind-emulator environment checks: they provision the cluster rather than inspect it. Run 'make create-kind-cluster' for that."
        elif [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
            # shellcheck source=/dev/null
            source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
            if declare -f check_specific_prerequisites > /dev/null; then
                check_specific_prerequisites
            fi
        fi
        # The managed namespace is an input, not a guess. See wva_require_namespace.
        wva_require_namespace
        # The two questions a reader actually arrives with — which namespace, and
        # what do I pass for PROMETHEUS_URL — both answerable from here.
        #
        # wva_autoselect_namespace is unreachable in namespace scope now: it runs only
        # while the namespace source is still "default", and requiring NAMESPACE means
        # it never is. Kept for CLUSTER scope, where discovering the namespaces to
        # manage is its actual purpose.
        wva_autoselect_namespace
        wva_report_namespace
        # The preflight must ask about the same phase the install will run, or it
        # reports a problem the install would never have had.
        wva_resolve_install_phase
        check_permissions
        check_single_installation
        # KEDA is as much a prerequisite as the RBAC above: WVA cannot actuate
        # without it, and the install checks it at the same point.
        check_keda_prerequisite
        wva_report_prometheus
        # The monitors the admin phase adds are reported as work it will do,
        # not as failures, when the install would run that phase.
        wva_set_monitors_planned
        # "Prometheus: <url>" answers whether an endpoint resolved, which reads as
        # though it answered whether the metrics WVA needs are in it. They are
        # different questions and only the second one decides whether WVA can work.
        # Gated on SKIP_CHECKS so the escape hatch these messages advertise is real
        # here too: they say "install without the checks: SKIP_CHECKS=true", and a
        # preflight that refused anyway would be telling the reader something false.
        if { [ "$(wva_install_scope)" = "cluster" ] && [ -z "${WVA_WATCH_NS:-}" ]; } || wva_watch_is_multi; then
            # A cluster-scoped controller manages every namespace, so the same
            # checks run once per namespace it would manage, found rather than
            # named. Run against WVA_NS they checked the controller's own
            # namespace, which holds no model servers, and passed on nothing.
            # A controller managing a listed few checks each of those.
            if ! wva_check_managed_namespaces && [ "$SKIP_CHECKS" != "true" ]; then
                log_error "Refusing to continue — fix the namespaces above, or SKIP_CHECKS=true (see docs/reference/operations.md#first-line-troubleshooting)."
            fi
        elif [ "$SKIP_CHECKS" != "true" ]; then
            wva_require_modelserver_metrics
            # The other half of "can WVA see anything": the model servers supply the
            # engine metrics, the EPP supplies the scheduler queue. Missing either is
            # silent, and missing the queue also disables the detector that would have
            # reported the rest.
            wva_require_epp_metrics
            # With a Kueue budget: a LocalQueue here, on a GPU ClusterQueue,
            # and every model server on it.
            wva_require_kueue_queues
        else
            # Still SAY what is missing -- skipping the gate should not mean skipping
            # the finding, or SKIP_CHECKS becomes a way to be uninformed.
            wva_report_modelserver_metrics || true
            wva_report_epp_flowcontrol || true
            wva_report_kueue_queues || true
        fi
        # Informational only. No ScaledObject yet is the EXPECTED state here, because
        # registration is the step AFTER this one -- objects to scale are WVA's
        # premise, ScaledObjects are its own output. This reports what is already
        # registered, so the reader knows whether 'adopt' will come up in the plan.
        # The EPPs of the managed pools must be able to authenticate a /metrics
        # scrape, or they answer 500. Never fatal: setup-prereqs grants it.
        wva_report_epp_metrics_auth
        wva_report_scaledobjects
        log_success "Preflight passed for ENVIRONMENT=$ENVIRONMENT, WVA_SCOPE=${WVA_SCOPE:-<platform default>}, WVA_LIMITER=${WVA_LIMITER:-<unset: keeps the live limiter>}"
        exit 0
    fi

    # Undeploy mode
    if [ "$UNDEPLOY" = "true" ]; then
        log_info "Starting Workload-Variant-Autoscaler Undeployment on $ENVIRONMENT"
        log_info "============================================================="
        echo ""

        if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
            # shellcheck source=/dev/null
            source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
        else
            log_error "Environment-specific script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
        fi

        cleanup
        exit 0
    fi

    log_info "Starting Workload-Variant-Autoscaler Deployment on $ENVIRONMENT"
    log_info "==========================================================="
    echo ""

    # kind-emulator builds the cluster it installs into, and does it inside the
    # environment script's check_specific_prerequisites -- which is sourced fifty
    # lines below. Everything between here and there needs a cluster:
    # check_prerequisites, check_permissions, check_single_installation. On a
    # machine that does not already have one -- a fresh CI runner, which is the
    # entire point of CREATE_CLUSTER=true -- each of them failed in turn on a
    # cluster nothing had yet been given the chance to create.
    #
    # So provision first, and let the ordinary preflight run against a real
    # cluster exactly as it does everywhere else. Fixing the order once beats
    # exempting each check from needing a cluster, which is how this was found:
    # one exemption, then the next check failed the same way.
    #
    # WVA_ENV_SOURCED stops the second call. Re-running
    # check_specific_prerequisites is not idempotent on this environment: with
    # CREATE_CLUSTER=true it DELETES an existing cluster and builds a new one, so
    # a second call would destroy the cluster this one just made, mid-install.
    WVA_ENV_SOURCED=false
    if [ "$ENVIRONMENT" = "kind-emulator" ] && [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
        log_info "Provisioning the $ENVIRONMENT cluster before the preflight, because this run creates it."
        # shellcheck source=/dev/null
        source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
        WVA_ENV_SOURCED=true
        if [ "$SKIP_CHECKS" != "true" ] && declare -f check_specific_prerequisites > /dev/null; then
            check_specific_prerequisites
        fi
    fi

    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi

    # Before ANY object is created: refuse to guess the managed namespace.
    wva_require_namespace

    # Before ANY object is created, and before the permission checks that depend
    # on which namespace this is: point a namespace-scoped install at the
    # namespace actually running llm-d, when the caller named none. Unreachable in
    # namespace scope now; kept for cluster scope.
    wva_autoselect_namespace

    # Which half of the install is left, before anything asks what this caller may
    # create. See wva_resolve_install_phase.
    wva_resolve_install_phase
    wva_report_namespace

    # Before anything is created: a second install repoints the shared
    # ClusterRoleBindings and leaves the existing controller without permissions,
    # which nothing reports.
    if [ "$DEPLOY_WVA" = "true" ]; then
        # Before anything is created: a missing permission discovered halfway
        # leaves a partial install behind.
        if [ "$SKIP_CHECKS" != "true" ]; then
            check_permissions
        fi
        check_single_installation
        # Before anything is created, for the same reason as check_permissions: an
        # install that cannot see llm-d's metrics sizes nothing, and every symptom of
        # that is silent -- the controller reports healthy and the HPA reads a healthy
        # ratio. Refusing here beats refusing after the objects exist.
        #
        # SKIP_CHECKS=true is the way past both, and the only one. kind-emulator is
        # exempt: the e2e path builds its own stack in one sequence.
        if [ "$SKIP_CHECKS" != "true" ] && [ "${ENVIRONMENT:-}" != "kind-emulator" ]; then
            # Missing monitors are added by the prereqs phase below, when this
            # run includes it (wva_apply_llmd_monitors).
            wva_set_monitors_planned
            if { [ "$(wva_install_scope)" = "cluster" ] && [ -z "${WVA_WATCH_NS:-}" ]; } || wva_watch_is_multi; then
                # Every namespace the controller would manage, as the preflight
                # does: the controller's own namespace holds no model servers.
                wva_check_managed_namespaces \
                    || log_error "Refusing to continue — fix the namespaces above, or SKIP_CHECKS=true (see docs/reference/operations.md#first-line-troubleshooting)."
            else
                wva_require_modelserver_metrics
                wva_require_epp_metrics
                wva_require_kueue_queues
            fi
        fi
    fi

    # Before anything is created, like the checks above. This used to be found
    # only by deploy_keda, which ran AFTER the controller was applied, so a cluster
    # without KEDA got namespaces, RBAC and a running controller and then exit 1.
    if [ "$SKIP_CHECKS" != "true" ]; then
        check_keda_prerequisite
    fi

    set_wva_logging_level

    if [ "${WVA_ENV_SOURCED:-false}" = "true" ]; then
        # Already sourced and already run, above. Calling
        # check_specific_prerequisites again would delete and rebuild the cluster
        # this install is halfway into.
        log_info "Environment-specific functions for $ENVIRONMENT are already loaded."
    elif [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
        log_info "Loading environment-specific functions for $ENVIRONMENT..."
        # shellcheck source=/dev/null
        source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"

        if declare -f check_specific_prerequisites > /dev/null; then
            if [ "$SKIP_CHECKS" != "true" ]; then
                check_specific_prerequisites
            fi
        fi
    else
        log_error "Environment script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
    fi

    log_info "Using configuration:"
    echo "    Deployed on:          $ENVIRONMENT"
    echo "    WVA Image:            $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "    WVA Namespace:        $WVA_NS"
    echo "    llm-d Namespace:      $NAMESPACE"
    echo "    Monitoring Namespace: $MONITORING_NAMESPACE"
    echo "    Scaler Backend:       $SCALER_BACKEND"
    echo ""

    # INSTALL_PHASE splits the install where the permissions split.
    #
    #   prereqs     what a CLUSTER ADMIN must do once per namespace: the namespace
    #               itself, the cluster-scoped RBAC, the shared infrastructure
    #               (Prometheus, KEDA) and the ServiceMonitor.
    #   wva         the controller, which a namespace admin can then install and
    #               upgrade on their own for as long as the prereqs stand.
    #   all         both, in order — the single-command install, and the default,
    #               so nothing about the existing behaviour changes.
    #
    # The phases are ordered, not independent: `wva` assumes `prereqs` has run,
    # and says so when it has not rather than failing on the first missing object.
    local phase="${INSTALL_PHASE:-all}"
    local do_prereqs=false do_wva=false
    case "$phase" in
        prereqs) do_prereqs=true ;;
        wva)     do_wva=true ;;
        all)     do_prereqs=true; do_wva=true ;;
        *)       log_error "INSTALL_PHASE must be prereqs, wva or all (got '$phase')" ;;
    esac

    # The controller's live Prometheus TLS settings, read and judged before
    # EITHER phase writes anything. Inside deploy_wva_controller the refusal
    # came after the prereqs phase had already applied namespaces, RBAC and
    # Prometheus, and still said "Nothing was changed".
    if [ "$do_wva" = true ] && [ "$DEPLOY_WVA" = "true" ]; then
        wva_read_live_prometheus_tls "$WVA_NS"
        wva_refuse_unknown_prometheus_tls
    fi

    if [ "$do_prereqs" = true ]; then
        wva_run_prereqs_phase "$phase"
    fi

    if [ "$do_wva" = true ]; then
        if [ "$DEPLOY_WVA" != "true" ]; then
            log_info "Skipping WVA deployment (DEPLOY_WVA=false)"
        elif [ "$phase" = "wva" ]; then
            require_prereqs_present
            WVA_APPLY_SCOPE=controller deploy_wva_controller
        fi

        # ScaledObjects that must follow the scaler to its new TLS mode
        # (WVA_SCALER_TLS_MIGRATE=true), now that the controller has it.
        if [ "$DEPLOY_WVA" = "true" ] && declare -F wva_scaler_tls_finish >/dev/null; then
            wva_scaler_tls_finish
        fi

        # After the scaler backend (wva_run_prereqs_phase installs or confirms it):
        # a ScaledObject is meaningless until KEDA is there to reconcile it.
        install_default_scaledobjects

        # Opt-in scraping for FMA launcher pods, into the WORKLOAD namespace —
        # not the controller's, which is where the overlay would have put it and
        # where it would have selected nothing.
        if [ "${WVA_FMA_LAUNCHER_METRICS:-false}" = "true" ]; then
            for ns in $(wva_watch_words); do
                deploy_fma_launcher_podmonitor "$ns"
            done
        fi
    fi

    if [ "$phase" = "prereqs" ]; then
        if wva_render_active; then
            # Nothing was created, so the "ready" summary would be false.
            wva_render_finish
        else
            print_prereqs_summary
        fi
        exit 0
    fi

    # Honour the verdict. `verify_deployment` computed one and threw it away, so a
    # controller that never started still ended with "complete!" and exit 0 — the
    # two things a wrapper or a CI job actually reads.
    local verified=true
    verify_deployment || verified=false

    print_summary

    if [ "$verified" != true ]; then
        echo ""
        log_warning "Deployment on $ENVIRONMENT FINISHED WITH ERRORS — the WVA controller is not ready, or KEDA has not wired a ScaledObject to it (see the warnings above)."
        log_warning "Everything above was created; nothing is scaling. Diagnose with the commands printed above, then re-run this script (it is idempotent)."
        return 1
    fi

    log_success "Deployment on $ENVIRONMENT complete!"
}
