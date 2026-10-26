#!/usr/bin/env bash
#
# Shared verification and deployment summary helpers for deploy/install.sh.
# Covers WVA / Prometheus / scaler backend only — llm-d is installed separately via llm-d project guides.
# Requires funcs: log_info/log_warning/log_success, containsElement().
# Uses constants: DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS and shared selectors.
#

# verify_deployment returns non-zero when the thing this script exists to install
# is not running. The caller MUST honour that: it used to compute `all_good` and
# then discard it, so an install whose controller never started printed
# "Deployment complete!" and exited 0 — which is what CI and every wrapper script
# reads. A silent false success is worse than a loud failure.
#
# Only WVA itself is fatal. Prometheus, Grafana and KEDA may be pre-existing,
# externally managed, or genuinely still pulling images; they stay warnings.
verify_deployment() {
    log_info "Verifying deployment..."

    local all_good=true

    # --- WVA
    log_info "Waiting for the WVA controller to become available..."
    # `kubectl get pods | grep Running` was not a readiness check: a pod stuck at
    # "0/1 Running" — crash-looping on a bad config, or unable to reach Prometheus —
    # matches it.
    #
    # Neither is `--for=condition=Available` on the Deployment, which is what this
    # used to ask. A rolling update keeps the PREVIOUS ReplicaSet's pod running
    # until the new one is ready, and one ready pod is enough to hold Available
    # true — so an upgrade to an image that cannot start reported success while
    # the new pod sat in CrashLoopBackOff. Verified on a real cluster: installing
    # this branch's manifests with the default (released) IMG crash-loops on
    # "unknown flag: --external-scaler-bind-address", and the install still said
    # "WVA controller is running and ready".
    #
    # `rollout status` is the check that means what this claims: it waits for the
    # UPDATED replicas to be available, and fails when they never are. Except at
    # 0 replicas, where it passes at once with nothing running, so
    # wva_controller_rollout_ready also requires a Ready replica.
    local wva_deploy
    wva_deploy=$(kubectl get deployment -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
        -o name 2>/dev/null | head -1)

    # Nothing in $WVA_NS -- before reporting a rollout failure against a
    # namespace that may simply be the wrong one, check whether a WVA controller
    # is running somewhere else entirely. Only reachable when $WVA_NS was never
    # confirmed by the caller (an install always deploys into $WVA_NS first, so
    # its own verify_deployment call finds this branch already covered); this is
    # for a standalone `make verify-deployment` run against an unset or guessed
    # WVA_NS.
    if [ -z "$wva_deploy" ]; then
        if kubectl auth can-i list deployments -A >/dev/null 2>&1; then
            local elsewhere elsewhere_count
            elsewhere=$(kubectl get deployment -A -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
                -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u)
            elsewhere_count=$(printf '%s\n' "$elsewhere" | grep -c . || true)
            if [ "${elsewhere_count:-0}" -eq 1 ]; then
                log_warning "No WVA controller in $WVA_NS -- but one is running in $elsewhere. Re-run with WVA_NS=$elsewhere (or NAMESPACE=$elsewhere for a namespace-scoped install, since that is what resolves WVA_NS by default)."
                return 1
            elif [ "${elsewhere_count:-0}" -gt 1 ]; then
                log_warning "No WVA controller in $WVA_NS, and more than one WVA runs on this cluster: $(printf '%s' "$elsewhere" | paste -sd, -). Pick one with WVA_NS=<ns>."
                return 1
            fi
            # Else: genuinely none anywhere. Fall through -- the diagnostics below
            # report that correctly, and there is nothing else to redirect to.
        else
            log_warning "Cannot confirm whether a WVA controller runs elsewhere -- listing Deployments cluster-wide is not permitted for this identity. If you know its namespace, pass WVA_NS=<ns> (or NAMESPACE=<ns> for a namespace-scoped install)."
        fi
    fi

    WVA_CONTROLLER_NOT_READY=""
    if [ -n "$wva_deploy" ] && wva_controller_rollout_ready "$wva_deploy" "$WVA_NS" \
        "${WVA_VERIFY_TIMEOUT:-180s}" >/dev/null 2>&1; then
        log_success "WVA controller is running and ready in $WVA_NS"
    elif [ "$WVA_CONTROLLER_NOT_READY" = "scaled-to-zero" ] && [ "${WVA_REPLICAS:-}" = "0" ]; then
        # Asked for: a staged install, or one a later step scales up. Said,
        # not failed.
        log_warning "WVA controller is scaled to 0 replicas in $WVA_NS, as WVA_REPLICAS=0 asked: nothing makes scaling decisions until it is scaled up (kubectl -n $WVA_NS scale $wva_deploy --replicas=1)."
    elif [ "$WVA_CONTROLLER_NOT_READY" = "scaled-to-zero" ]; then
        all_good=false
        log_warning "WVA controller is NOT running: $wva_deploy in $WVA_NS is scaled to 0 replicas, so nothing makes scaling decisions."
        log_warning "  Re-run the install (it sets the controller to WVA_REPLICAS, default 1), or: kubectl -n $WVA_NS scale $wva_deploy --replicas=1"
    else
        all_good=false
        log_warning "WVA controller did NOT become ready within ${WVA_VERIFY_TIMEOUT:-180s}."
        kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" >&2 2>/dev/null || true

        # Say WHY, here, rather than printing two commands to run. The container's
        # own last words are almost always the answer and they are one API call
        # away — a released image rejecting a flag this branch's manifests pass
        # ("unknown flag: --external-scaler-bind-address") looks identical to a
        # network problem until you read them.
        local reason
        reason=$(kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
            -o jsonpath='{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{.lastState.terminated.reason}{"\n"}{end}' 2>/dev/null \
            | grep -v '^[[:space:]]*$' | head -3)
        [ -n "$reason" ] && log_warning "  Container state: $(printf '%s' "$reason" | tr '\n' ';')"

        local logs
        logs=$(kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
            --tail=8 --all-containers 2>/dev/null \
            || kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
                --tail=8 --previous 2>/dev/null)
        if [ -n "$logs" ]; then
            log_warning "  Last lines from the container:"
            printf '%s\n' "$logs" | sed 's/^/      /' >&2
        fi
        wva_hint_untrusted_prometheus_cert "$(printf '%s\n%s\n' "$reason" "$logs")"

        log_warning "  Full logs: kubectl -n $WVA_NS logs -l $WVA_CONTROLLER_LABEL_SELECTOR --tail=50 --previous"
        log_warning "  Events:    kubectl -n $WVA_NS get events --sort-by=.lastTimestamp | tail -20"
        log_warning "  If it is rejecting a flag, IMG and these manifests are different versions: build and push this tree (make docker-build docker-push IMG=<ref>) and re-run with that IMG."
    fi

    # --- Did the model-server metrics actually arrive?
    #
    # The sanity check that closes the loop: a PodMonitor existing says nothing
    # about whether it produced a target. One naming a port no container declares
    # generates NO target at all rather than a failing one, so "applied" and
    # "working" are different claims and only the second one matters. This is the
    # half that carries the value — the installer does not create the object (see
    # infra_monitoring.sh), so reporting the outcome is all there is.
    #
    # Asked of WVA rather than of Prometheus, deliberately. The Prometheus URL is
    # in-cluster (on OpenShift, thanos-querier.openshift-monitoring.svc) and is not
    # reachable from wherever this script runs, so querying it would need a route,
    # a token, or a one-shot pod. WVA has already queried it, every optimization
    # interval, and says what it found: wva_saturation_metrics_up, and this log
    # line. One kubectl call, no cluster networking, and it reports the thing the
    # operator actually cares about.
    local wva_logs40 sat_missing
    wva_logs40=$(kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" --tail=40 2>/dev/null)
    sat_missing=$(grep -c "No saturation metrics available" <<< "$wva_logs40" || true)
    if [ "${sat_missing:-0}" -gt 0 ]; then
        log_warning "WVA is running but has NO model-server metrics to size from — it logged \"No saturation metrics available for model\" ${sat_missing}x in its last 40 lines."
        log_warning "  It will hold every workload at minReplicas. On the HPA that looks identical to a correct decision ('1/1 (avg)'), which is why this is checked here."
        local hint_ns
        # One hint per managed namespace: a comma list is not a namespace -n takes.
        for hint_ns in $(wva_watch_words); do
            log_warning "  Usually nothing is scraping the model servers. Check:  kubectl get podmonitor,servicemonitor -n ${hint_ns}"
            log_warning "  That is llm-d's step 3, \"(Optional) Enable monitoring\" — optional for llm-d, required for WVA:"
            log_warning "      kubectl apply -n ${hint_ns} -k \$REPO_ROOT/guides/recipes/modelserver/components/monitoring"
            log_warning "  or, with no llm-d checkout:  kubectl apply -n ${hint_ns} -k config/modelserver-metrics"
        done
        log_warning "  A model server with 0 replicas also produces no series; that case resolves itself when one starts."
    elif grep -q "scaling-decision" <<< "$wva_logs40"; then
        log_success "WVA is reading model-server metrics and emitting scaling decisions"
    fi

    # --- WVA's own metrics: was the ServiceMonitor ACCEPTED, not merely created?
    #
    # The prometheus-operator resolves a ServiceMonitor's bearerTokenSecret the
    # first time it sees the object. If that Secret is absent it REJECTS the
    # ServiceMonitor -- `reason=InvalidConfiguration`, "unable to get secret" --
    # and never reconsiders: a metadata write does not re-trigger it, and
    # re-applying identical content is a no-op, so `kubectl apply` says
    # "unchanged" and the operator does not look again. The rejection lasts as
    # long as the object does.
    #
    # A two-phase install used to create the ServiceMonitor in the admin phase and
    # its Secret in the controller phase, half an hour apart, and so produced
    # exactly that state: no `up` series, no `wva_*` series, and an install that
    # printed "All components verified successfully!" over the top of it. Both
    # kinds are in WVA_PREREQ_KINDS now (see common.sh) so they land together, but
    # an install whose admin phase predates that change still carries the rejected
    # object -- and only the operator's log or this check will ever say so.
    #
    # Checked by cause rather than by event: events expire, the missing Secret does
    # not. Not fatal -- WVA scales from llm-d's metrics, a different path entirely.
    # What is lost is WVA's own telemetry, which fails by rendering empty.
    # `|| true` because the ServiceMonitor CRD is absent on any cluster without
    # the prometheus-operator -- the common case on plain Kubernetes -- and kubectl
    # exits non-zero there. An assignment carries that status and `set -e` turns it
    # into an exit. Today the only caller is `verify_deployment || verified=false`,
    # which suppresses `set -e` for the whole body; this no longer depends on that.
    local sm_json sm_refs
    sm_json=$(kubectl get servicemonitor -n "$WVA_NS" -o json 2>/dev/null) || true
    if [ -n "$sm_json" ]; then
        local sm_missing
        sm_refs=$(printf '%s' "$sm_json" | jq -r '
            .items[]?
            | .metadata.name as $sm
            | .spec.endpoints[]?
            | select(.bearerTokenSecret.name != null)
            | "\($sm) \(.bearerTokenSecret.name)"' 2>/dev/null) || true
        sm_missing=$(printf '%s\n' "$sm_refs" \
          | while read -r sm secret; do
                [ -n "$secret" ] || continue
                kubectl get secret "$secret" -n "$WVA_NS" >/dev/null 2>&1 \
                    || echo "$sm -> $secret"
            done)
        # Two different failures, and the second is the one a naive check misses.
        #
        #   the Secret is absent      the cause. Deterministic, and the state a
        #                             fresh mis-ordered install lands in.
        #   the Secret is present     but the operator already rejected the
        #     and it was rejected     ServiceMonitor when it was not, and does not
        #                             re-evaluate. This is the RECOVERY state: an
        #                             install that creates the missing Secret is
        #                             still not scraped, and checking only for the
        #                             Secret would call that healthy.
        #
        # The rejection is only visible as an event, and events expire (an hour by
        # default), so this catches it within an install's own window and cannot
        # prove its absence later. That asymmetry is the right way round: a false
        # "still rejected" costs one spec touch, a false "healthy" costs all of
        # WVA's telemetry, silently.
        # `|| true`: no matching events is grep exit 1, which pipefail makes the
        # assignment's status and `set -e` makes an exit -- and NO rejection events
        # is the healthy case, so without this the check kills the rest of
        # verify_deployment on exactly the clusters where nothing is wrong.
        local sm_rejected
        # The MESSAGE, not just the name. A missing Secret is only one thing a
        # ServiceMonitor can fail to resolve, and assuming it was misdiagnoses every
        # other cause while prescribing a fix that cannot work. Seen on the shared GPU cluster:
        # the Secret existed and the rejection was a missing CA ConfigMap, for which
        # delete-and-re-apply runs straight back into the same missing object.
        sm_rejected=$(kubectl get events -n "$WVA_NS" \
            --field-selector reason=InvalidConfiguration \
            -o jsonpath='{range .items[*]}{.involvedObject.kind}/{.involvedObject.name}{"  "}{.message}{"\n"}{end}' 2>/dev/null \
            | grep '^ServiceMonitor/' | cut -c1-210 | sort -u) || true

        if [ -n "$sm_missing" ]; then
            log_warning "A ServiceMonitor names a bearerTokenSecret that does not exist, so the prometheus-operator has rejected it and WVA's own metrics are NOT being collected:"
            printf '%s\n' "$sm_missing" | sed 's/^/      /' >&2
            log_warning "  Scaling is unaffected (that reads llm-d's metrics), but no wva_* series will appear and the dashboard will be empty."
            log_warning "  Fix: create the Secret, then force ONE re-evaluation — the operator ignores metadata-only writes, so change the spec, or delete and re-apply the ServiceMonitor."
        elif [ -n "$sm_rejected" ]; then
            log_warning "A ServiceMonitor was rejected by the prometheus-operator (InvalidConfiguration), and it does not re-evaluate on its own. The reason it gave:"
            printf '%s\n' "$sm_rejected" | sed 's/^/      /' >&2
            log_warning "  Until it is re-evaluated there is no scrape target and no wva_* series — on the dashboard the WVA panels stay empty while the vllm:* ones fill. Fix what the message names FIRST, then force one re-evaluation:"
            log_warning "    kubectl -n $WVA_NS delete servicemonitor <name> && $(wva_make_cmd setup-prereqs)"
            log_warning "  Then confirm a target exists — a re-apply of unchanged content will NOT do it."
        elif [ -n "$sm_refs" ]; then
            log_success "WVA metrics ServiceMonitor references resolve (its scrape config is valid)"
        else
            # Nothing here authenticates with a Secret, so there is no reference for
            # the operator to resolve and nothing for this check to confirm. Claiming
            # "valid" would be the same false green the check exists to remove: on
            # plain Kubernetes the ServiceMonitor uses bearerTokenFile -- a projected
            # token the kubelet supplies, which the operator never resolves -- and a
            # namespace with no ServiceMonitor at all reaches this branch too.
            log_info "No ServiceMonitor in $WVA_NS authenticates with a Secret, so there is nothing for the prometheus-operator to resolve or reject (only the OpenShift overlay uses bearerTokenSecret; Kubernetes uses bearerTokenFile)."
        fi
    fi

    # --- The EPP's scheduler queue, WVA's other input.
    #
    # Reported here as well as in the preflight, because a caller who runs
    # `make deploy-wva` without `make check-prereqs` would otherwise never be told,
    # and the gate being off is invisible in every other output: WVA still emits
    # decisions, the HPA still reads healthy, and wva_unmeasured_queue — which
    # exists to catch exactly this class of blindness — is itself sourced from the
    # queue that is missing, so it reads 0.
    # Not at cluster scope: there the controller's namespace holds no EPP, and
    # the preflight checked every managed namespace (wva_check_managed_namespaces).
    if [ "$(wva_install_scope)" != "cluster" ] || [ -n "${WVA_WATCH_NS:-}" ]; then
        local epp_ns
        for epp_ns in $(wva_watch_words); do
            (WVA_WATCH_NS="$epp_ns"; wva_report_epp_flowcontrol) || true
        done
    fi

    # --- Monitoring
    # Keyed on what is actually there, not on DEPLOY_PROMETHEUS. That flag is the
    # INTENT to deploy, and deploy_monitoring_stack deliberately does not deploy
    # when the cluster already has its own Prometheus -- so on exactly that
    # cluster this warned "Prometheus is not ready" about a namespace nothing was
    # installed into, while the controller was happily reading a working
    # Prometheus somewhere else.
    log_info "Checking Prometheus..."
    # The ENDPOINT first, always. "A Prometheus is running in
    # MONITORING_NAMESPACE" is not the same claim as "WVA reads from there", and
    # on OpenShift they are different places: the platform runs a Prometheus in
    # openshift-user-workload-monitoring while WVA reads the Thanos Querier in
    # openshift-monitoring. Leading with the pod check therefore reported a
    # namespace the controller never talks to, and called a platform Prometheus
    # something this install deployed. Observed on the shared GPU cluster.
    #
    # The endpoint is also the more useful fact: it is the one an operator can
    # curl when the controller says it cannot reach Prometheus.
    local prom_endpoint
    prom_endpoint="$(wva_prometheus_endpoint)" || prom_endpoint=""
    if [ -n "$prom_endpoint" ]; then
        log_success "Prometheus: $prom_endpoint"
    elif wva_any_pod_ready "$MONITORING_NAMESPACE" app.kubernetes.io/name=prometheus; then
        log_success "Prometheus is running in $MONITORING_NAMESPACE"
    elif [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        log_warning "Prometheus is not ready yet"
    fi

    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        log_info "Checking Grafana..."
        # Namespace-scoped on purpose, unlike KEDA: Grafana is not a cluster
        # singleton, so finding someone else's on a shared cluster would prove
        # nothing about this install. That does mean it reports only on the
        # namespace it was told about, so it says which one.
        if wva_any_pod_ready "$MONITORING_NAMESPACE" app.kubernetes.io/name=grafana; then
            log_success "Grafana is running"
        elif wva_pods_exist "$MONITORING_NAMESPACE" app.kubernetes.io/name=grafana; then
            log_warning "Grafana is not ready yet"
        else
            log_info "No Grafana in $MONITORING_NAMESPACE. On OpenShift the dashboards are elsewhere (often the grafana namespace), and this install did not deploy one."
        fi
    fi

    # --- Scaler backend
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Checking KEDA..."
        # Cluster-wide, not $KEDA_NAMESPACE. KEDA is a cluster singleton — one
        # operator owns the external-metrics APIService — and where it lives is
        # not ours to assume: on OpenShift it is platform-managed and sits in
        # openshift-keda, while KEDA_NAMESPACE says keda-system. Looking only
        # there reported "KEDA is not ready" on a cluster whose KEDA had been up
        # for ten days with six ScaledObjects READY.
        if wva_any_pod_ready -A "$KEDA_OPERATOR_LABEL_SELECTOR"; then
            log_success "KEDA is running"
        elif wva_pods_exist -A "$KEDA_OPERATOR_LABEL_SELECTOR"; then
            # Found and unhealthy. This one has earned the strong wording.
            log_warning "KEDA is not ready yet — until it is, ScaledObjects stay unready and nothing scales."
        else
            # Not found is not the same as broken, and saying "nothing scales"
            # here would be a false alarm on any cluster whose KEDA does not
            # carry this label -- which is exactly the platform-managed case.
            #
            # A stronger signal sits right in the managed namespace, once one
            # exists: a ScaledObject KEDA has actually reconciled to READY. Only
            # useful in namespace scope (cluster scope would need enumerating
            # every managed namespace, not just one), and only once a workload
            # has been registered -- so this is a no-op the first time
            # verify_deployment runs, as part of an install, before step 3
            # ("Register the workloads") has created anything. It becomes a real
            # confirmation on a later, standalone `make verify-deployment`.
            # Every managed namespace, not the first: a controller managing a
            # list is confirmed by a READY ScaledObject in any of them.
            local keda_ns="" keda_one keda_ready=""
            [ "$(wva_install_scope)" = "namespace" ] && keda_ns="$(wva_watch_words)"
            for keda_one in $keda_ns; do
                if kubectl get scaledobject -n "$keda_one" \
                        -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null \
                        | grep -qx 'True'; then
                    keda_ready="$keda_one"
                    break
                fi
            done
            if [ -n "$keda_ready" ]; then
                log_success "KEDA is running — confirmed by a READY ScaledObject in $keda_ready (its operator pod just carries no label this check recognizes; a platform-managed KEDA often does not)."
            elif [ -n "$keda_ns" ]; then
                log_info "No KEDA operator matched $KEDA_OPERATOR_LABEL_SELECTOR anywhere on the cluster, and no READY ScaledObject in ${keda_ns// /, } confirms it that way either. A platform-managed KEDA may not carry that label — confirm with: kubectl get scaledobject -n ${keda_ns%% *}"
            else
                log_info "No KEDA operator matched $KEDA_OPERATOR_LABEL_SELECTOR anywhere on the cluster. A platform-managed KEDA may not carry that label — confirm with: kubectl get scaledobject -A"
            fi
        fi
        wva_verify_scaler_networkpolicy
        wva_verify_scaler_tls
        wva_verify_scaler_wiring || all_good=false
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Scaler backend skipped (SCALER_BACKEND=none) — assuming external metrics API is pre-installed"
    fi

    if [ "$all_good" = true ]; then
        log_success "All components verified successfully!"
        return 0
    fi
    return 1
}

# wva_verify_scaler_networkpolicy compares the namespace the external-scaler
# NetworkPolicy admits to :9090 with where KEDA runs, and says so LOUDLY when
# they differ: KEDA is then refused, every ScaledObject that names this scaler
# goes not-Ready, and nothing scales -- while the controller itself reports
# healthy. And it says, as a warning, when there is no policy at all: :9090 is
# then open to any Pod. Never fatal, and silent about what it cannot read beyond
# one line saying so.
#
# Where KEDA runs is read the way the install reads it
# (wva_resolve_scaler_keda_namespace): the namespace serving the external-metrics
# APIService, when that is KEDA's. Nothing weaker. A Deployment named
# keda-operator is not evidence -- any tenant can create one -- so it can neither
# confirm a policy nor call one wrong.
wva_verify_scaler_networkpolicy() {
    local np_ns keda rc
    rc=0
    np_ns="$(wva_scaler_np_keda_ns "$WVA_NS")" || rc=$?
    case "$rc" in
        0) ;;
        2)
            log_info "Could not read NetworkPolicy wva-external-scaler in $WVA_NS, so whether it admits KEDA is not checked."
            return 0
            ;;
        4)
            # A policy exists but admits :9090 from no single namespace: it is
            # not the shape the installer writes, so what it lets in is not
            # known here. The installer leaves it as it is and calls it
            # UNKNOWN; saying "open" would send an admin to fix a port that
            # may well be closed.
            log_warning "NetworkPolicy wva-external-scaler in $WVA_NS is not one this install recognises (it admits :9090 from no single namespace): left as it is, state UNKNOWN. Check it: kubectl get networkpolicy wva-external-scaler -n $WVA_NS -o yaml"
            return 0
            ;;
        *)
            log_warning "No NetworkPolicy: scaler port open. NetworkPolicy wva-external-scaler is absent from $WVA_NS, so the external scaler (:9090) is reachable by any Pod."
            if [ "${WVA_SCALER_NETWORKPOLICY:-true}" = "false" ]; then
                log_warning "  WVA_SCALER_NETWORKPOLICY=false left it out on purpose."
            else
                log_warning "  To close it, a cluster admin runs: $(wva_make_cmd setup-prereqs 'KEDA_NAMESPACE=<namespace KEDA runs in>')"
            fi
            return 0
            ;;
    esac
    rc=0
    keda="$(wva_keda_apiservice_ns)" || rc=$?
    if [ "$rc" != 0 ] || [ -z "$keda" ]; then
        log_info "External scaler NetworkPolicy admits :9090 from $np_ns; where KEDA runs is not visible to you (no readable KEDA-served external-metrics APIService), so that is not confirmed. If KEDA runs elsewhere, a cluster admin runs: $(wva_make_cmd setup-prereqs 'KEDA_NAMESPACE=<namespace>')"
        return 0
    fi
    if [ "$keda" = "$np_ns" ]; then
        log_success "External scaler NetworkPolicy admits KEDA from $np_ns, the namespace serving the external-metrics APIService"
        return 0
    fi
    log_warning "=================================================================="
    log_warning "The external scaler NetworkPolicy admits :9090 ONLY from $np_ns,"
    log_warning "  but the external-metrics APIService is served by KEDA in: $keda"
    log_warning "  KEDA's calls are dropped: every ScaledObject naming this scaler goes"
    log_warning "  not-Ready and NOTHING SCALES, while the controller reports healthy."
    log_warning "  Fix: $(wva_make_cmd setup-prereqs 'KEDA_NAMESPACE=<the namespace KEDA runs in>'),"
    log_warning "  or edit NetworkPolicy wva-external-scaler in $WVA_NS."
    log_warning "=================================================================="
    return 0
}

# wva_scaler_flag_of echoes the value of flag $2 on the manager container of
# Deployment JSON $1, given as --flag=value or as --flag followed by the value.
wva_scaler_flag_of() {
    printf '%s' "$1" | jq -r --arg f "$2" '
        [.spec.template.spec.containers[]? | select(.name == "manager") | (.args // [])] | first // [] | . as $a
        | ([range(0; $a | length) | select($a[.] | startswith($f + "=")) | $a[.] | sub("^[^=]*="; "")]
           + [range(0; ($a | length) - 1) | select($a[.] == $f) | $a[. + 1]]) | first // ""' 2>/dev/null || true
}

# wva_verify_scaler_tls reports how KEDA's calls to the external scaler are
# protected: plaintext, TLS, or mutual TLS (and the client identity it pins);
# the serving certificate's expiry and names; and, when TLS is on, every
# ScaledObject that would still dial plaintext. Read from the running
# Deployment, so a hand-set flag is reported as faithfully as an installed one.
# Never fatal.
wva_verify_scaler_tls() {
    local dep rc=0 cert_path client_ca san mode secret crt sans end host unauth
    dep="$(kubectl get deployment wva-controller-manager -n "$WVA_NS" -o json 2>/dev/null)" || rc=$?
    if [ "$rc" != 0 ] || [ -z "$dep" ]; then
        log_info "Could not read Deployment wva-controller-manager in $WVA_NS, so the external scaler's TLS mode is not reported."
        return 0
    fi
    cert_path="$(wva_scaler_flag_of "$dep" --external-scaler-cert-path)"
    client_ca="$(wva_scaler_flag_of "$dep" --external-scaler-client-ca)"
    san="$(wva_scaler_flag_of "$dep" --external-scaler-client-san)"
    if [ -z "$cert_path" ]; then
        log_info "External scaler: plaintext gRPC (no TLS). To serve mutual TLS and move the ScaledObjects that name it:  $(wva_make_cmd deploy-wva WVA_SCALER_TLS=true WVA_SCALER_TLS_MIGRATE=true)  (see docs/reference/configuration.md#external-scaler-mutual-tls)."
        # ScaledObjects on the TLS address or reference cannot reach it.
        if declare -F wva_scaler_plaintext_scaledobjects >/dev/null; then
            rc=0
            unauth="$(wva_scaler_plaintext_scaledobjects plaintext)" || rc=$?
            if [ "$rc" = 0 ] && [ -n "$unauth" ]; then
                log_warning "=================================================================="
                log_warning "The external scaler is on plaintext, but these ScaledObjects use its TLS address or authenticationRef $(wva_scaler_keda_auth_name):"
                printf '%s\n' "$unauth" | head -10 | sed 's/^/    /' >&2
                log_warning "  $(wva_scaler_mixed_risk)"
                log_warning "  Fix: turn TLS on, or move them back:  $(wva_make_cmd deploy-wva WVA_SCALER_TLS=false WVA_SCALER_TLS_MIGRATE=true)"
                log_warning "=================================================================="
            fi
        fi
        return 0
    fi
    if [ -z "$client_ca" ]; then
        mode="TLS, server only: any client may call it"
    elif [ -z "$san" ]; then
        mode="mutual TLS: any client certificate the client CA signed"
    else
        mode="mutual TLS, client identity $san"
    fi
    log_success "External scaler: $mode"

    # The serving certificate, from the Secret mounted at the cert path.
    secret="$(printf '%s' "$dep" | jq -r --arg p "$cert_path" '
        .spec.template.spec as $s
        | [($s.containers[]? | select(.name == "manager") | .volumeMounts[]? | select(.mountPath == $p) | .name) as $v
           | $s.volumes[]? | select(.name == $v) | .secret.secretName // empty] | first // ""' 2>/dev/null || true)"
    if [ -z "$secret" ]; then
        log_info "  serving certificate: not mounted from a Secret at $cert_path, so its expiry is not checked."
    elif ! command -v openssl >/dev/null 2>&1; then
        log_info "  serving certificate: Secret $secret (openssl is not on PATH, so its expiry and names are not checked)."
    else
        crt="$(mktemp)"
        if kubectl get secret "$secret" -n "$WVA_NS" -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d > "$crt" 2>/dev/null \
            && [ -s "$crt" ] && end="$(openssl x509 -in "$crt" -noout -enddate 2>/dev/null)"; then
            end="${end#notAfter=}"
            sans="$(openssl x509 -in "$crt" -noout -text 2>/dev/null \
                | awk '/Subject Alternative Name/ { getline; print }' | tr ',' '\n' | sed -n 's/^ *DNS://p' | tr '\n' ' ')"
            log_info "  serving certificate: Secret $secret, expires $end, names: ${sans:-none}"
            if ! openssl x509 -in "$crt" -noout -checkend 0 >/dev/null 2>&1; then
                log_warning "  The external scaler certificate in Secret $secret has EXPIRED: KEDA's handshakes fail and nothing scales. Re-run make deploy-wva to re-issue it (cert-manager renews its own)."
            elif ! openssl x509 -in "$crt" -noout -checkend $((${WVA_SCALER_RENEW_DAYS:-30} * 86400)) >/dev/null 2>&1; then
                log_warning "  The external scaler certificate in Secret $secret expires within ${WVA_SCALER_RENEW_DAYS:-30} days ($end). Re-run make deploy-wva to re-issue it (cert-manager renews its own)."
            fi
            for host in "wva-external-scaler.${WVA_NS}.svc" "wva-external-scaler.${WVA_NS}.svc.cluster.local"; do
                case " $sans " in
                    *" $host "*) ;;
                    *) log_warning "  The external scaler certificate does not name $host, a scalerAddress host the installer writes: KEDA rejects the server and nothing scales." ;;
                esac
            done
        else
            log_info "  serving certificate: Secret $secret is not readable by you (or has no tls.crt), so its expiry is not checked."
        fi
        rm -f "$crt"
    fi

    # KEDA's side, as the admin phase recorded it.
    if declare -F wva_scaler_keda_marker_state >/dev/null && [ -n "$san" ]; then
        wva_scaler_keda_marker_state "$san"
        if [ "$WVA_SCALER_KEDA_READY" != true ]; then
            log_warning "  KEDA's side is not usable: ${WVA_SCALER_KEDA_WHY}. KEDA cannot present a client certificate and nothing scales. A cluster admin runs:  $(wva_make_cmd setup-prereqs)"
        else
            log_info "  KEDA's client certificate: ClusterTriggerAuthentication $(wva_scaler_keda_auth_name), expires ${WVA_SCALER_KEDA_NOT_AFTER}"
            if [ "$WVA_SCALER_KEDA_STALE" = true ]; then
                log_info "  KEDA holds an older client certificate than the one in $WVA_NS; it works until it expires. Refresh:  $(wva_make_cmd setup-prereqs)  and then restart KEDA's operator:  $(wva_scaler_keda_restart_cmd)"
            fi
            wva_scaler_keda_ca_report log_warning
            # Say so when it matches, too: the docs promise this line, and
            # silence read as "not checked".
            case "${WVA_SCALER_KEDA_CA_STALE:-}" in
                false) log_info "  KEDA trusts the current CA: the one that signs the scaler's certificate now." ;;
                unread) log_info "  The scaler's current CA could not be read, so whether KEDA trusts it is not checked." ;;
            esac
            if [ "$WVA_SCALER_KEDA_EXPIRING" = true ]; then
                log_warning "  KEDA's client certificate expires within ${WVA_SCALER_RENEW_DAYS} days (${WVA_SCALER_KEDA_NOT_AFTER}). After that nothing scales. A cluster admin refreshes it with:  $(wva_make_cmd setup-prereqs)  and then restarts KEDA's operator:  $(wva_scaler_keda_restart_cmd)"
            fi
        fi
    fi

    if ! declare -F wva_scaler_plaintext_scaledobjects >/dev/null; then
        return 0
    fi
    # Hand-set TLS flags: KEDA's side is whoever set them, not this installer's.
    if [ -z "$(printf '%s' "$dep" | jq -r --arg a "$WVA_SCALER_TLS_ANNOTATION" '.spec.template.metadata.annotations[$a] // ""')" ]; then
        log_info "  TLS was set up by hand, so ScaledObjects are not checked against this installer's address and ClusterTriggerAuthentication."
        return 0
    fi
    rc=0
    unauth="$(wva_scaler_plaintext_scaledobjects tls)" || rc=$?
    if [ "$rc" != 0 ]; then
        log_info "  Could not list ScaledObjects, so whether every one reaches this scaler over TLS is not checked."
    elif [ -n "$unauth" ]; then
        log_warning "=================================================================="
        log_warning "TLS is on, but these ScaledObjects name this scaler on the plaintext address or without authenticationRef $(wva_scaler_keda_auth_name) (kind ClusterTriggerAuthentication):"
        printf '%s\n' "$unauth" | head -10 | sed 's/^/    /' >&2
        log_warning "  $(wva_scaler_mixed_risk)"
        log_warning "  Fix: add that authenticationRef and the address $(wva_scaler_address "$WVA_NS" tls), or move them:  $(wva_make_cmd deploy-wva WVA_SCALER_TLS=true WVA_SCALER_TLS_MIGRATE=true)"
        log_warning "=================================================================="
    else
        log_info "  Every ScaledObject naming this scaler uses $(wva_scaler_address "$WVA_NS" tls) and authenticationRef $(wva_scaler_keda_auth_name)."
    fi
}

# wva_verify_scaler_wiring names every ScaledObject naming this scaler whose
# HPA KEDA built without the scaler's external metric, or whose
# status.externalMetricNames lacks it (wva_scaler_so_wiring). Such a
# ScaledObject reads Ready and Active, while its HPA scales on the CPU default
# and WVA's decisions reach nothing. KEDA wires a new or changed ScaledObject
# within about half a minute, so this waits up to WVA_VERIFY_WIRE_WAIT seconds
# (default 60) before it calls one unwired. Returns 1 when one is, and when
# KEDA presents a client certificate the controller refuses
# (wva_scaler_keda_conn_state): then it names the KEDA restart, not a re-wire.
wva_verify_scaler_wiring() {
    declare -F wva_scaler_so_wiring >/dev/null || return 0
    local rows rc=0
    rows="$(wva_scaler_plaintext_scaledobjects any)" || rc=$?
    [ "$rc" = 0 ] && [ -n "$rows" ] || return 0
    # KEDA presenting an old client certificate fails every call, wired or
    # not, and only a restart of its operator fixes it: said first, and no
    # re-wire offered.
    wva_scaler_keda_conn_state
    # A stale connection after a reinstall: KEDA is shared, so this names the
    # restart and does not fail the install (wva_scaler_keda_stale_act), unless
    # KEDA_RESTART=true lets it restart KEDA, after which wiring is checked.
    if [ "$WVA_SCALER_KEDA_CONN" = stale ]; then
        wva_scaler_keda_stale_act "$rows" || return 0
        wva_scaler_wait_wired "$rows" "${WVA_VERIFY_WIRE_WAIT:-60}"
        return
    fi
    if [ "$WVA_SCALER_KEDA_CONN" = copy ]; then
        log_warning "=================================================================="
        log_warning "KEDA's calls to the external scaler are refused, so these ScaledObjects get no metric from it and the HPA scales on the CPU default:"
        printf '%s\n' "$rows" | sed 's/^/    /' >&2
        wva_scaler_keda_conn_report || true
        log_warning "=================================================================="
        return 1
    fi
    if wva_scaler_wait_wired "$rows" "${WVA_VERIFY_WIRE_WAIT:-60}"; then
        [ -z "$WVA_SCALER_WIRE_UNKNOWN" ] \
            || log_info "  Could not read these ScaledObjects or their HPAs, so whether KEDA wired the scaler's metric is not checked: $(printf '%s' "$WVA_SCALER_WIRE_UNKNOWN" | paste -sd' ' -)"
        return 0
    fi
    # Read again: refusals may have started while it waited, which is the usual
    # case for ScaledObjects created just now.
    wva_scaler_keda_conn_state
    if [ "$WVA_SCALER_KEDA_CONN" = stale ]; then
        wva_scaler_keda_stale_act "$WVA_SCALER_UNWIRED" || return 0
        wva_scaler_wait_wired "$rows" "${WVA_VERIFY_WIRE_WAIT:-60}"
        return
    fi
    log_warning "=================================================================="
    log_warning "KEDA did not wire the scaler's metric onto the HPA of these ScaledObjects. They read Ready, but the HPA scales on the CPU default and WVA's decisions reach nothing:"
    printf '%s' "$WVA_SCALER_UNWIRED" | sed 's/ .*//; s/^/    /' >&2
    if wva_scaler_keda_conn_report; then
        log_warning "=================================================================="
        return 1
    fi
    log_warning "  KEDA asks for the metric spec only when the spec changes, and a call that failed then (the scaler moving or restarting) is not retried. Change it again:"
    printf '%s' "$WVA_SCALER_UNWIRED" | while IFS= read -r row; do
        [ -n "$row" ] && printf '    %s\n' "$(wva_scaler_rewire_cmd "$row")" >&2
    done
    # The common cause, said statically: verify-deployment does not read
    # KEDA's version (scaler_runtime.sh does, at install).
    log_warning "  Or re-wire them all:  $(wva_make_cmd scaledobjects-rewire). On KEDA 2.19 and older this follows a WVA controller restart or outage, a KEDA bug fixed in 2.20: upgrade KEDA to 2.21 (docs/reference/keda.md#unwired-hpa-before-keda-220)."
    log_warning "=================================================================="
    return 1
}

# wva_summary_scaledobjects echoes <namespace>/<name> of every ScaledObject,
# in the namespaces this install watches, with a trigger naming this install's
# scaler on either address. $1, when given, is the list
# wva_scaler_scaledobjects_json already read. Exits 2 when they cannot be
# listed, or the lister is not loaded: unknown, not none.
wva_summary_scaledobjects() {
    declare -F wva_scaler_plaintext_scaledobjects >/dev/null || return 2
    wva_scaler_plaintext_scaledobjects any "${1:-}"
}

# wva_rewire_classify reads how KEDA wired each <namespace>/<name> row in $1
# (wva_scaler_so_wiring) and sorts them into WVA_REWIRE_WIRED,
# WVA_REWIRE_PAUSED, WVA_REWIRE_UNKNOWN (one row per line) and
# WVA_REWIRE_UNWIRED ("<namespace>/<name> <index>..." per line, the trigger
# indices KEDA left without the metric).
wva_rewire_classify() {
    local row st
    WVA_REWIRE_WIRED=""
    WVA_REWIRE_PAUSED=""
    WVA_REWIRE_UNWIRED=""
    WVA_REWIRE_UNKNOWN=""
    while IFS= read -r row; do
        [ -n "$row" ] || continue
        st="$(wva_scaler_so_wiring "${row%%/*}" "${row#*/}")"
        case "$st" in
            wired) WVA_REWIRE_WIRED="${WVA_REWIRE_WIRED}${row}"$'\n' ;;
            paused) WVA_REWIRE_PAUSED="${WVA_REWIRE_PAUSED}${row}"$'\n' ;;
            unwired*) WVA_REWIRE_UNWIRED="${WVA_REWIRE_UNWIRED}${row} ${st#unwired }"$'\n' ;;
            *) WVA_REWIRE_UNKNOWN="${WVA_REWIRE_UNKNOWN}${row}"$'\n' ;;
        esac
    done <<< "$1"
}

# wva_rewire_keda_conn_check checks KEDA's connection to the scaler when every
# ScaledObject in $1 is wired (wva_scaler_keda_conn_state). A stale one is
# handled as wva_verify_scaler_wiring does (wva_scaler_keda_stale_act, which
# restarts KEDA's operator with KEDA_RESTART=true), except with $2 = true (a
# dry run), which only names the restart. Returns 1 while KEDA's calls are
# refused, 0 otherwise.
wva_rewire_keda_conn_check() {
    wva_scaler_keda_conn_state
    case "$WVA_SCALER_KEDA_CONN" in
        stale)
            if [ "$2" = true ]; then
                log_warning "KEDA's calls to the external scaler are refused, so these ScaledObjects get no metric from it, wired or not:"
                printf '%s\n' "$1" | sed '/^$/d; s/^/    /' >&2
                wva_scaler_keda_conn_report || true
                log_info "DRY_RUN=true: KEDA's operator was not restarted."
                return 1
            fi
            wva_scaler_keda_stale_act "$1" ;;
        copy)
            log_warning "KEDA's calls to the external scaler are refused, so these ScaledObjects get no metric from it, wired or not:"
            printf '%s\n' "$1" | sed '/^$/d; s/^/    /' >&2
            wva_scaler_keda_conn_report || true
            return 1 ;;
        *) return 0 ;;
    esac
}

# wva_rewire_scaledobjects re-wires this install's ScaledObjects that KEDA left
# on the CPU default. It is make scaledobjects-rewire, and the last step of
# make scaledobjects-apply.
#
# KEDA asks the scaler for a ScaledObject's metric spec only when its spec
# changes. When that one call fails, the HPA stays on the CPU default for good.
# This lists the ScaledObjects naming this scaler (wva_summary_scaledobjects),
# re-wires the unwired ones (wva_scaler_ensure_wired: it changes trigger
# metadata $WVA_SCALER_REWIRE_KEY, never deletes or recreates one), and reports
# each as wired, paused, still unwired, or unreadable. A paused one is skipped.
# It re-wires every ScaledObject naming this scaler in the namespaces this
# install watches, not only the ones scaledobjects-apply wrote.
#
# With every one wired, it still checks KEDA's connection, as
# wva_verify_scaler_wiring does: a KEDA presenting an old client certificate
# is refused on every call, wired or not, and only its operator restart helps
# (wva_scaler_keda_stale_act, which restarts it with KEDA_RESTART=true).
#
# DRY_RUN=true changes nothing and prints the commands instead; any value
# other than true or false is refused before anything is read. The first wait
# is WVA_SCALER_REWIRE_WAIT seconds, else WVA_VERIFY_WIRE_WAIT, else 30.
# Returns 1 while one is unwired or KEDA's connection is refused, 2 when they
# cannot be listed or DRY_RUN is not true or false, 3 when one could not be
# read or when none names this scaler but some name another WVA namespace
# and no scaler Service runs in $WVA_NS (WVA_NS points at the wrong install).
wva_rewire_scaledobjects() {
    declare -F wva_scaler_ensure_wired >/dev/null || return 2
    local rows row rc=0 dry="${DRY_RUN:-false}"
    case "$dry" in
        true|false) ;;
        *) log_warning "DRY_RUN must be true or false, got $dry: nothing was read or changed."
           return 2 ;;
    esac
    local WVA_SCALER_REWIRE_WAIT="${WVA_SCALER_REWIRE_WAIT:-${WVA_VERIFY_WIRE_WAIT:-30}}"
    # Read once: both the rows naming this scaler and, when there are none,
    # the namespaces the others name come from the same list.
    local listed
    listed="$(wva_scaler_scaledobjects_json)" || rc=$?
    [ "$rc" != 0 ] || rows="$(wva_summary_scaledobjects "$listed")" || rc=$?
    if [ "$rc" != 0 ]; then
        log_warning "Could not list the ScaledObjects naming this scaler (wva-external-scaler.$WVA_NS), so none was checked or re-wired."
        return 2
    fi
    if [ -z "$rows" ]; then
        # None naming this scaler, while some name another WVA's: WVA_NS is
        # wrong unless a scaler runs in it, in which case those belong to a
        # second install.
        local others
        others="$(wva_scaler_other_namespaces "$listed")" || rc=$?
        if [ "$rc" != 0 ]; then
            log_warning "No ScaledObject names this scaler (wva-external-scaler.$WVA_NS), and whether they name another could not be read, so none was checked."
            return 3
        fi
        local svc=""
        if [ -n "$others" ]; then
            svc=0
            wva_scaler_svc_state "$WVA_NS" || svc=$?
        fi
        if [ -n "$others" ] && [ "$svc" = 0 ]; then
            log_info "No ScaledObject names this scaler (wva-external-scaler.$WVA_NS): nothing to re-wire. Some name the scaler of another install in $(printf '%s' "$others" | paste -sd' ' -); run this there to re-wire those."
            return 0
        fi
        if [ -n "$others" ] && [ "$svc" = 2 ]; then
            log_warning "No ScaledObject names this scaler (wva-external-scaler.$WVA_NS), but some name wva-external-scaler.<namespace> with WVA_NS $(printf '%s' "$others" | paste -sd' ' -), and whether a scaler runs in $WVA_NS could not be read (${WVA_SCALER_SVC_ERR:-no error text}). None was checked."
            return 3
        fi
        if [ -n "$others" ]; then
            log_warning "No ScaledObject names this scaler (wva-external-scaler.$WVA_NS), but some name wva-external-scaler.<namespace> with WVA_NS $(printf '%s' "$others" | paste -sd' ' -), and no scaler Service runs in $WVA_NS. None was checked. Run it again with WVA_NS set to the namespace WVA runs in, e.g.  $(WVA_WATCH_NS="${WVA_WATCH_NS:-$WVA_NS}"; WVA_NS="$(printf '%s\n' "$others" | head -n1)"; wva_make_cmd scaledobjects-rewire)"
            return 3
        fi
        log_info "No ScaledObject names this scaler (wva-external-scaler.$WVA_NS): nothing to re-wire."
        return 0
    fi
    wva_rewire_classify "$rows"
    if [ -n "$WVA_REWIRE_UNWIRED" ] && [ "$dry" != true ]; then
        wva_scaler_ensure_wired "$(printf '%s' "$WVA_REWIRE_UNWIRED" | sed 's/ .*//')" || true
        wva_rewire_classify "$rows"
    fi
    echo "ScaledObjects naming this scaler:"
    printf '%s' "$WVA_REWIRE_WIRED" | sed 's/^/    wired      /'
    printf '%s' "$WVA_REWIRE_PAUSED" | sed 's/^/    paused     /; s/$/  (skipped)/'
    printf '%s' "$WVA_REWIRE_UNWIRED" | sed 's/ .*//; s/^/    unwired    /'
    printf '%s' "$WVA_REWIRE_UNKNOWN" | sed 's/^/    unknown    /; s/$/  (it or its HPA could not be read)/'
    if [ -z "$WVA_REWIRE_UNWIRED" ]; then
        wva_rewire_keda_conn_check "$rows" "$dry" || return 1
        [ -z "$WVA_REWIRE_UNKNOWN" ] || return 3
        return 0
    fi
    if [ "$dry" = true ]; then
        log_info "DRY_RUN=true: nothing was changed."
        # A KEDA presenting an old client certificate: a re-wire does not
        # help, its operator restart does (wva_scaler_keda_conn_report).
        wva_scaler_keda_conn_state
        if ! wva_scaler_keda_conn_report; then
            log_info "To re-wire, run  $(wva_make_cmd scaledobjects-rewire)  or:"
            printf '%s' "$WVA_REWIRE_UNWIRED" | while IFS= read -r row; do
                [ -n "$row" ] && printf '    %s\n' "$(wva_scaler_rewire_cmd "$row")"
            done
        fi
        return 1
    fi
    log_warning "$(printf '%s' "$WVA_REWIRE_UNWIRED" | grep -c .) ScaledObject(s) still unwired: their HPAs scale on the CPU default and WVA's decisions do not reach them. The warning above says why."
    return 1
}

# print_summary is the most-read text this install produces: it is what someone
# looks at to find out whether it worked and what to do next. So it says the one
# thing that is true and easy to miss — an installed WVA scales nothing until a
# ScaledObject exists — and it reports the settings that will otherwise surprise
# someone later (no GPU budget, scale-to-zero, a named instance).
print_summary() {
    echo ""
    echo "=========================================="
    echo " Workload-Variant-Autoscaler installed"
    echo "=========================================="
    echo ""
    echo "  Controller:   $WVA_NS  ($(wva_image_ref))"
    echo "  Scope:        $(wva_install_scope)-scoped"
    if [ "$(wva_install_scope)" = "cluster" ]; then
        echo "  Manages:      every namespace"
    elif wva_watch_is_multi; then
        echo "  Manages:      $(wva_watch_words | sed 's/ /, /g') (it runs in $WVA_NS; its cache holds those and its own)"
    elif [ -n "${WVA_WATCH_NS:-}" ] && [ "$WVA_WATCH_NS" != "$WVA_NS" ]; then
        # The admin-owned install: the controller RUNS in one namespace and
        # WATCHES another, which is the whole point of that configuration and the
        # one line an admin checks to confirm it. Printing $WVA_NS here said
        # "Manages: wva-team-a only" about a controller managing team-a --
        # naming the namespace it can see nothing in.
        echo "  Manages:      $WVA_WATCH_NS only (it runs in $WVA_NS; its cache is restricted to $WVA_WATCH_NS)"
    else
        echo "  Manages:      $WVA_NS only (its cache is restricted to it)"
    fi
    # The tag is usually floating -- the shipped default is :main -- so the tag
    # alone does not identify what is running. Checking out the same source commit
    # later reproduces the source and not the controller unless the digest was
    # written down, and nothing wrote it down. Print it: it costs one field and it
    # is the only thing here that answers "what exactly did I deploy?".
    local wva_image wva_digest
    wva_image="$(kubectl get deploy -n "$WVA_NS" -l app.kubernetes.io/name=workload-variant-autoscaler,control-plane=controller-manager -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null)" || wva_image=""
    wva_digest="$(kubectl get pods -n "$WVA_NS" -l app.kubernetes.io/name=workload-variant-autoscaler,control-plane=controller-manager -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null)" || wva_digest=""
    [ -n "$wva_image" ] && echo "  Image:        $wva_image"
    case "$wva_digest" in
        *@sha256:*) echo "  Digest:       ${wva_digest#*@}" ;;
    esac

    # Same rule as the check above: name the endpoint WVA reads, not a namespace
    # that happens to contain a Prometheus.
    local summary_endpoint
    summary_endpoint="$(wva_prometheus_endpoint)" || summary_endpoint=""
    if [ -n "$summary_endpoint" ]; then
        echo "  Prometheus:   $summary_endpoint"
    elif wva_any_pod_ready "$MONITORING_NAMESPACE" app.kubernetes.io/name=prometheus; then
        echo "  Prometheus:   deployed in $MONITORING_NAMESPACE by this install"
    else
        echo "  Prometheus:   ${PROMETHEUS_URL:-<the shipped default>} (not deployed by this install)"
    fi
    # The TLS warning scrolled past long before this summary; repeat its point.
    [ -z "${WVA_TLS_SUMMARY:-}" ] || echo "  TLS:          the controller $WVA_TLS_SUMMARY"
    if [ "$SCALER_BACKEND" = "keda" ]; then
        # The measured answer from check_keda_prerequisite / deploy_keda. This
        # line used to print "installed or already present" whatever was true.
        case "${WVA_KEDA_STATE:-unknown}" in
            present)           echo "  KEDA:         present on the cluster" ;;
            absent-installing) echo "  KEDA:         installed by this run" ;;
            *)                 echo "  KEDA:         not verified by this run (check: kubectl get crd scaledobjects.keda.sh)" ;;
        esac
    else
        echo "  KEDA:         skipped (SCALER_BACKEND=$SCALER_BACKEND) — WVA needs KEDA to actuate"
    fi
    # Report where the dashboard ACTUALLY is, not what the flag asked for. This
    # line used to assert "deployed in $MONITORING_NAMESPACE" even when the publish
    # had been skipped by a Forbidden -- confident and wrong.
    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        local dash_ns=""
        for candidate in "${DASHBOARD_NS:-}" "$WVA_NS" "$MONITORING_NAMESPACE"; do
            [ -n "$candidate" ] || continue
            if kubectl get configmap wva-operation-dashboard -n "$candidate" >/dev/null 2>&1; then
                dash_ns="$candidate"
                break
            fi
        done
        if [ -n "$dash_ns" ]; then
            echo "  Dashboard:    published in $dash_ns"
        else
            echo "  Dashboard:    not published — import deploy/grafana/operational-dashboard.json into Grafana"
        fi
    fi
    # CONTROLLER_INSTANCE is deliberately NOT reported here. No install path puts
    # it on the Deployment, so this line read the installer's shell environment and
    # then asserted a scoping rule the running controller was not applying — the
    # most misleading kind of summary, because it is specific and confident.
    echo ""

    # The step everyone misses. WVA has no watch and no listing: it is asked about
    # a workload by KEDA, or it never hears of it. Said only when no ScaledObject
    # names this scaler: on a re-install with ScaledObjects in place, "nothing
    # scales yet" is false. A list that cannot be read is not "none".
    local named rc=0
    named="$(wva_summary_scaledobjects)" || rc=$?
    if [ "$rc" = 0 ] && [ -n "$named" ]; then
        echo "  ScaledObjects: $(printf '%s\n' "$named" | wc -l | tr -d ' ') name this scaler. Check them:  make verify-scaledobjects"
        echo ""
    else
        if [ "$rc" = 0 ]; then
            echo "  NEXT: nothing scales yet."
        else
            echo "  NEXT: the ScaledObjects could not be listed, so whether any names this"
            echo "  scaler is not known. If none does, nothing scales yet."
        fi
        echo ""
        echo "  A ScaledObject is how a workload registers with WVA. Until one exists,"
        echo "  the controller is running and idle. To see what it would create:"
        echo ""
        echo "      make scaledobjects-plan"
        echo ""
        echo "  then apply it, or an edited copy of it:"
        echo ""
        echo "      make scaledobjects-apply"
        echo ""
    fi

    echo "  Worth knowing:"
    case "$(effective_limiter_summary)" in
        none) echo "    - No GPU limiter: scaling is UNBOUNDED. Set WVA_LIMITER=gpu-inventory, quota, or both (gpu-inventory,quota)" ;;
        *)    echo "    - GPU limiters: $(effective_limiter_summary), all in force together. A workload whose accelerator does not resolve gets no budget and will not scale up" ;;
    esac
    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        echo "    - Scale-to-zero is ON: an idle model can be parked at 0 and woken on demand"
    else
        echo "    - Scale-to-zero is OFF (ENABLE_SCALE_TO_ZERO=false)"
    fi
    echo ""

    echo "  Checking on it:"
    echo "    kubectl -n $WVA_NS logs -l app.kubernetes.io/name=workload-variant-autoscaler -f"
    echo "    kubectl get scaledobject -A          # what WVA has been told about"
    echo "    kubectl get hpa -A                   # KEDA acts through these"
    echo ""
    echo "  Guide: deploy/README.md"
    echo "=========================================="
}

# effective_limiter_summary echoes the limiter set in force after this
# install: the one WVA_LIMITER declared, or -- WVA_LIMITER unset -- the live
# policy's, which the install kept. Reading WVA_LIMITER alone reported a kept
# gpu-inventory,quota policy as "No GPU limiter: scaling is UNBOUNDED".
effective_limiter_summary() {
    if [ -z "${WVA_LIMITER:-}" ] && declare -F wva_live_limiter_set >/dev/null; then
        wva_live_limiter_set "$WVA_NS" 2>/dev/null || echo none
        return 0
    fi
    wva_limiter_set 2>/dev/null || echo "${WVA_LIMITER:-none}"
}
