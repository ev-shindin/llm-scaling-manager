#!/usr/bin/env bash
#
# Shared monitoring orchestration helpers.
# Keeps install.sh main flow concise while delegating to environment/plugin functions.
# Requires funcs: deploy_prometheus_stack(), log_info/log_warning/log_success,
# wait_deployment_available_nonfatal().
# Requires vars: DEPLOY_PROMETHEUS.
#

# fma_launcher_scrape_conflict echoes the names of PodMonitors in $1 that ALREADY
# generate targets for FMA launcher pods, or nothing.
#
# It exists to stop the fma-launcher-metrics component from being applied on top
# of one. Two scrape configs on one pod produce two targets at the same
# (instance, pod) key, and the collector's additive queries -- the `sum by` ones
# behind dispatch rate and generation-token rate -- would DOUBLE-COUNT, while the
# `max by` ones would not. Capacity then looks right while throughput reads 2x,
# which is a hard failure to spot and a worse outcome than not scraping at all.
#
# The distinction between selecting a launcher and being able to scrape one is
# what makes this correct rather than merely plausible; wva_launcher_scrapers
# carries that reasoning, and discovery asks it the same question from the other
# direction. With no launcher pods present it reports nothing, which is right: the
# PodMonitor is inert until FMA appears and should be in place beforehand.
fma_launcher_scrape_conflict() {
    wva_launcher_scrapers "$1" fma-launcher-metrics
}

# deploy_fma_launcher_podmonitor applies the launcher PodMonitor into $1, the
# namespace whose launcher pods should be scraped.
#
# It takes a namespace rather than riding the WVA overlay because the two are not
# the same place. The overlay renders into WVA_NS, where the CONTROLLER runs;
# launcher pods live in the workload namespace, which is WVA_NS only for a plain
# namespace-scoped install, is WVA_WATCH_NS whenever that is set, and is any
# number of namespaces for a cluster-scoped one. A PodMonitor in the wrong
# namespace selects nothing and reports no error, which is the worst way for
# monitoring to fail.
#
# Returns 0 when it applied, or when it deliberately did not. A monitoring add-on
# must not fail an install.
deploy_fma_launcher_podmonitor() {
    local ns="$1" conflict launchers

    if ! kubectl get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1; then
        log_warning "WVA_FMA_LAUNCHER_METRICS is set but the PodMonitor CRD is absent — install the Prometheus Operator first. Skipping."
        return 0
    fi

    conflict=$(fma_launcher_scrape_conflict "$ns" | paste -sd, -)
    if [ -n "$conflict" ]; then
        log_warning "Not applying the FMA launcher PodMonitor: $conflict already scrapes launcher pods in $ns. Two scrape configs on one pod give it two targets under the same (instance, pod) key, and WVA's additive queries would double-count throughput while capacity still looked correct. Remove the other PodMonitor first, or keep using it and leave WVA_FMA_LAUNCHER_METRICS unset."
        return 0
    fi

    if ! kubectl apply -k "$WVA_PROJECT/config/fma-launcher-metrics" -n "$ns" >/dev/null 2>&1; then
        log_warning "Failed to apply the FMA launcher PodMonitor in $ns; continuing without it."
        return 0
    fi

    launchers=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "${launchers:-0}" -gt 0 ]; then
        log_success "FMA launcher PodMonitor applied in $ns ($launchers launcher pod(s) present). Only launchers with a BOUND instance become targets — an unbound one carries no server-port annotation and is skipped, so warm spares never appear as idle replicas."
    else
        log_info "FMA launcher PodMonitor applied in $ns. No launcher pods there yet; it will start scraping if FMA is installed later."
    fi

    log_info "Cluster-scoped installs: this applied to $ns only. Repeat for every namespace that runs FMA launchers — kubectl apply -k config/fma-launcher-metrics -n <ns>"
}

# foreign_prometheus echoes any Prometheus on this cluster that this install did
# NOT put there, or nothing.
#
# The Prometheus Operator's CRDs are cluster-scoped and singular: a second
# kube-prometheus-stack install brings its own operator, which then watches the
# SAME Prometheus/ServiceMonitor resources as the first. Two operators reconciling
# one set of objects fight over them, and the visible symptom is somebody else's
# monitoring going intermittently blind — a bad way to find out you installed WVA.
#
# "Foreign" means outside MONITORING_NAMESPACE. A Prometheus we deployed earlier
# lives there, and re-running the install over it must still upgrade it: this
# script is meant to be idempotent, and the e2e re-deploys onto a live cluster
# every run. Detecting merely "a Prometheus exists" would have turned that upgrade
# into a silent skip.
# --- Model-server metrics -----------------------------------------------------
#
# WVA's capacity model is built entirely on vllm:* series — num_requests_waiting,
# kv_cache_usage_perc, time_per_output_token_seconds, request_prompt_tokens, and
# the rest (see internal/collector). Something has to scrape the model servers for
# any of it to exist.
#
# Providing that is arguably llm-d's job, and a full llm-d standup does provide it:
# a working optimized-baseline namespace carries a `decode` PodMonitor selecting
# llm-d.ai/role=decode on port `modelserver`. But a namespace built from the guide
# need not have one, and the failure is silent in the worst way. Measured on
# the shared GPU cluster: a tenant namespace ran four replicas for hours and produced ZERO vllm
# series, while `check-prereqs` reported "[SUCCESS] Prometheus: <url>" — true, and
# about the endpoint, not its contents — and the controller's own startup
# validation passed for the same reason. WVA then logged "No saturation metrics
# available for model, skipping analysis" every cycle and held at minReplicas,
# which on the HPA is indistinguishable from a correct decision: `1/1 (avg)`.
#
# So: verify it, and confirm afterwards that series actually arrived. The admin
# phase adds the PodMonitor where nothing scrapes the servers yet
# (wva_ensure_llmd_monitors, at the end of this file, says how the reasons it was
# once removed for are met).

# wva_modelserver_scrape_conflict echoes any monitor in $1, other than WVA's own,
# that already scrapes the model servers.
#
# Same reasoning as fma_launcher_scrape_conflict: two scrape configs on one pod
# give it two targets, and WVA's additive queries would double-count throughput
# while per-replica capacity still looked right. Better to leave an existing,
# working scrape alone -- which makes a MISS here actively harmful rather than
# merely quiet: the report tells the reader to apply config/modelserver-metrics,
# and doing that on top of a scrape that already exists IS the double-count.
#
# Markers come from so_serving_markers, never a private list. Matching the
# serving ROLE alone missed the shape the llm-d modelservice path actually
# renders -- a PodMonitor selecting llm-d.ai/inferenceServing=true plus
# llm-d.ai/model, seen on the shared GPU cluster as vllm-<model>-<hash> -- and llm-d attaches
# llm-d.ai/-prefixed labels to non-serving components too (gateway, EPP,
# requester), so the marker list is the only thing that separates the two. One
# definition for discovery, the preflight count and this check, or they disagree.
wva_modelserver_scrape_conflict() {
    local ns="$1" kind markers services
    markers="$(so_serving_markers_json)"

    # A ServiceMonitor selects SERVICES, not pods, so a pod label in its selector
    # proves nothing and a pod-label test against one is answering the wrong
    # question. Resolve the indirection instead: find the Services this monitor
    # selects, then ask whether THOSE target a model server. Fetched once.
    services="$(kubectl get services -n "$ns" -o json 2>/dev/null \
                | jq -c '[.items[]? | {labels: (.metadata.labels // {}),
                                       selector: (.spec.selector // {})}]' 2>/dev/null || true)"
    [ -n "$services" ] || services='[]'

    for kind in podmonitors servicemonitors; do
        # `|| true`: this whole function runs inside a command substitution whose
        # result is assigned, and under `set -o pipefail` a Forbidden or
        # CRD-absent kubectl makes the loop's status the function's status, which
        # then aborts the caller's assignment under `set -e`. Measured: with
        # ServiceMonitors Forbidden -- an ordinary namespace-scoped install --
        # check-prereqs died silently just before "Preflight passed".
        kubectl get "$kind" -n "$ns" -o json 2>/dev/null | jq -r \
            --arg kind "$kind" --argjson markers "$markers" --argjson services "$services" '
            # A label map is a model server when any of its k=v pairs is a marker.
            def serving_map($m): ($m | to_entries
                | any((.key + "=" + (.value|tostring)) as $kv
                      | ($markers | index($kv)) != null));
            # matchExpressions form: key In [values], any pair of which is a marker.
            def serving_exprs($e): (($e // [])
                | any(. as $x
                      | (($x.values // [])
                         | any(($x.key + "=" + (.|tostring)) as $kv
                               | ($markers | index($kv)) != null))));
            .items[]?
            | select((.metadata.labels["app.kubernetes.io/component"] // "")
                     != "modelserver-metrics")
            | . as $m
            | (.spec.selector.matchLabels // {}) as $sel
            | select(
                if $kind == "podmonitors" then
                    serving_map($sel) or serving_exprs(.spec.selector.matchExpressions)
                else
                    # Service labels satisfy the monitor selector, and that
                    # Service in turn selects serving pods. An empty selector is
                    # not treated as "matches everything" here: it would call
                    # every namespace with a Service already-scraped and suppress
                    # the warning, which is the failure this check exists to catch.
                    ($sel | length) > 0
                    and ($services | any(
                            (.labels) as $svc
                            | ($sel | to_entries | all($svc[.key] == .value))
                              and serving_map(.selector)))
                end)
            | "\($kind|rtrimstr("s"))/\($m.metadata.name)"' 2>/dev/null || true
    done
    return 0
}

# wva_serving_workload_count echoes how many workloads in $1 are model servers,
# read from the pod template -- the same basis discovery uses, so it counts a
# workload scaled to zero.
#
# Markers and pod paths both come from scaledobject.sh instead of being restated.
# Undercounting does not produce a wrong number here, it produces NO CHECK: the
# caller returns early on zero, so a namespace whose servers carry
# llm-d.ai/inferenceServing but no role label would skip the warning entirely --
# reproducing, for that shape, the silent no-metrics failure this exists to catch.
#
# Both Deployment and LeaderWorkerSet, matching how scaledobject.sh scans:
# multi-node / disaggregated serving is a supported shape, and a Deployment-only
# count would silently no-op for a namespace whose decode/prefill pods are owned
# by a LWS.
wva_serving_workload_count() {
    local ns="$1" total=0 kind resource pod n markers
    markers="$(so_serving_markers_json)"
    for kind in Deployment LeaderWorkerSet; do
        resource='deployments'; pod="$SO_POD_PATH_DEPLOYMENT"
        if [ "$kind" = "LeaderWorkerSet" ]; then
            resource='leaderworkersets'; pod="$SO_POD_PATH_LWS"
        fi
        # `|| true` for the same reason as above -- most clusters have no
        # LeaderWorkerSet CRD, and that kubectl exits non-zero.
        n=$(kubectl get "$resource" -n "$ns" -o json 2>/dev/null \
            | jq --argjson p "$pod" --argjson markers "$markers" '
                [ .items[]?
                  | ((getpath($p) // (if $p == ["spec", "leaderWorkerTemplate", "leaderTemplate"] then getpath(["spec", "leaderWorkerTemplate", "workerTemplate"]) else null end) // {}) | .metadata.labels // {})
                  | select(to_entries
                           | any((.key + "=" + (.value|tostring)) as $kv
                                 | ($markers | index($kv)) != null))
                ] | length' 2>/dev/null || true)
        total=$((total + ${n:-0}))
    done
    echo "$total"
}

# wva_report_modelserver_metrics is the read-only half, for the preflight: say
# whether anything scrapes the model servers, without applying anything.
wva_report_modelserver_metrics() {
    local ns="${WVA_WATCH_NS:-$WVA_NS}" conflict servers

    kubectl get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1 || {
        log_info "No PodMonitor CRD on this cluster, so model-server scraping cannot be checked or created here."
        return 0
    }

    servers="$(wva_serving_workload_count "$ns")"
    [ "${servers:-0}" -gt 0 ] || return 0

    # Can this identity actually SEE monitors here? A Forbidden list is not an empty
    # one, and this check refuses installs -- so "I could not look" must not be
    # reported as "nothing scrapes them". Matches how the EPP gate above separates a
    # confirmed-absent signal from an unconfirmed one; the two gates refuse on the
    # same grounds or neither is trustworthy.
    if ! kubectl auth can-i list podmonitors -n "$ns" >/dev/null 2>&1; then
        log_warning "  Model-server metrics: cannot tell whether anything scrapes the $servers model server(s) in $ns — listing PodMonitors there is not permitted for this identity."
        log_warning "    Not treated as missing: refusing an install over a question this check was not allowed to ask would block a correctly monitored namespace."
        return 0
    fi

    conflict="$(wva_modelserver_scrape_conflict "$ns" | paste -sd, -)"
    if [ -n "$conflict" ]; then
        log_success "  Model-server metrics: $conflict already scrapes them."
        return 0
    fi
    if kubectl get podmonitor -n "$ns" -l app.kubernetes.io/component=modelserver-metrics \
         --no-headers 2>/dev/null | grep -q .; then
        log_success "  Model-server metrics: WVA's own PodMonitor is in place."
        return 0
    fi
    # The admin phase adds it (wva_ensure_llmd_monitors), so a missing one is
    # work that step does, not a reason to stop before it.
    if [ "${WVA_MONITORS_PLANNED:-false}" = true ]; then
        log_info "  Model-server metrics: nothing scrapes them yet. Setup adds PodMonitor wva-modelserver-metrics here."
        return 0
    fi
    log_warning "  Monitoring not enabled for your llm-d servers; please enable Model-server metrics."
    log_warning "    The cluster-admin step adds it for you:  make setup-prereqs (with the same SCOPE)"
    log_warning "    This is llm-d's step 3, \"(Optional) Enable monitoring\" — optional for llm-d, required for WVA."
    log_warning "      From an llm-d checkout:"
    log_warning "        kubectl apply -n $ns -k \$REPO_ROOT/guides/recipes/modelserver/components/monitoring"
    log_warning "        (and .../components/monitoring-pd as well, for a prefill/decode split)"
    log_warning "      Or directly:"
    log_warning "        kubectl apply -n $ns -k config/modelserver-metrics"
    return 1
}

# wva_require_modelserver_metrics turns that report into a stop.
#
# Same class as the EPP gate, and for the same reason: llm-d's monitoring step is
# "(Optional) Enable monitoring" -- optional FOR LLM-D, required for WVA -- so a
# correctly-followed llm-d install can leave a namespace WVA cannot size anything in,
# and every symptom of that is silent. Refusing here asks the caller to finish llm-d,
# which is where the fix actually lives. Full rationale lives once in the guide's
# "How it works" section, not repeated here.
#
# SKIP_CHECKS=true is the bypass, and the only one. A per-check WVA_ALLOW_* flag was
# tried and removed: the scenario is already selected by WVA_SCOPE, and a second knob
# for the same decision is a second thing to get wrong.
#
# Worth revisiting later: rather than refusing, WVA could DEGRADE knowingly -- keep
# running with engine metrics only and say which capabilities are off. That is the
# better answer and a bigger change; refusing is the honest interim.
wva_require_modelserver_metrics() {
    wva_report_modelserver_metrics && return 0
    log_error "Refusing to continue — fix monitoring above, or SKIP_CHECKS=true (see docs/reference/operations.md#first-line-troubleshooting)."
}

# The object itself is added by the admin phase, not here: see
# wva_ensure_llmd_monitors at the end of this file. It was once created and then
# removed, over ownership, cleanup and drift; that function says how each is met.

foreign_prometheus() {
    kubectl get crd prometheuses.monitoring.coreos.com >/dev/null 2>&1 || return 0
    kubectl get prometheuses.monitoring.coreos.com -A \
        -o go-template="{{range .items}}{{if ne .metadata.namespace \"${MONITORING_NAMESPACE}\"}}{{.metadata.namespace}}/{{.metadata.name}} {{end}}{{end}}" 2>/dev/null || true
    return 0
}

# install_operational_dashboard publishes the WVA dashboard as a sidecar-labelled
# ConfigMap.
#
# It lives OUTSIDE the kube-prometheus-stack install because the dashboard and the
# Prometheus are independent decisions. It used to be a block inside that install,
# so the dashboard shipped only when this script also deployed Prometheus — meaning
# the case that needs it most, an existing cluster with its own monitoring, was the
# one case that never got it. Whoever operates WVA there was left reading raw
# PromQL for panels we already ship.
#
# The Grafana sidecar discovers dashboards by label, not by namespace ownership, so
# a plain labelled ConfigMap is all an existing Grafana needs. DASHBOARD_NS targets
# a Grafana living somewhere other than MONITORING_NAMESPACE.
# wva_grafana_dashboard_search_ns echoes the namespaces a Grafana dashboard
# sidecar watches, or nothing if that cannot be determined.
#
# This is the difference between a dashboard that appears and a ConfigMap nobody
# reads. The sidecar discovers dashboards by LABEL, but only inside the namespaces
# it is told to watch: kube-prometheus-stack passes searchNamespace through as the
# sidecar container's NAMESPACE env var, and its default is the sidecar's own
# namespace. Publishing a correctly-labelled ConfigMap anywhere else succeeds and
# is then silently ignored -- there is no error to notice, which is why this is
# detected rather than documented.
wva_grafana_dashboard_search_ns() {
    local ns="$1" out
    out=$(kubectl get deploy,statefulset -n "$ns" -l app.kubernetes.io/name=grafana \
            -o jsonpath='{range .items[*]}{range .spec.template.spec.containers[*]}{.name}{"~"}{range .env[*]}{.name}{"="}{.value}{","}{end}{"\n"}{end}{end}' \
            2>/dev/null) || return 0
    printf '%s\n' "$out" | awk -F'~' '/sc-dashboard/ {print $2}' \
        | tr ',' '\n' | sed -n 's/^NAMESPACE=//p' | head -1
}

# wva_dashboard_ns_is_watched reports whether $1 is covered by the watch list $2.
wva_dashboard_ns_is_watched() {
    local ns="$1" watched="$2"
    case "$watched" in
        ALL|all) return 0 ;;
        "")      return 1 ;;
    esac
    case ",${watched}," in
        *",${ns},"*) return 0 ;;
    esac
    return 1
}

# wva_choose_dashboard_ns picks the namespace to publish into.
#
# ONE SHARED OBJECT REMAINS THE DEFAULT, deliberately: the ConfigMap name is fixed
# and the dashboard is generic and variable-driven, so ten namespace-scoped installs
# applying the same object is better than ten identical entries in the picker. See
# the note below on pinning.
#
# This function therefore does NOT switch to a per-tenant copy on its own, even
# when Grafana would see it. Detection is used to make the ADVICE accurate -- to
# suggest DASHBOARD_NS only when it would actually render, and to warn when the
# chosen namespace is not watched -- not to change where the dashboard lands.
wva_choose_dashboard_ns() {
    printf '%s' "${DASHBOARD_NS:-$MONITORING_NAMESPACE}"
}

# wva_dashboard_manual_help prints the two routes that need no cluster rights.
# Printed at the moment of failure, because that is when it is read.
wva_dashboard_manual_help() {
    local ns="$1" watched
    watched="$(wva_grafana_dashboard_search_ns "$MONITORING_NAMESPACE")"
    if wva_dashboard_ns_is_watched "$WVA_NS" "$watched"; then
        log_info "  Grafana watches '${watched}', so this works with no admin at all:"
        log_info "    DASHBOARD_NS=$WVA_NS  (publishes a private copy into your own namespace)"
    fi
    log_info "  Fastest, and needs NO Kubernetes permission:"
    log_info "    In Grafana: Dashboards -> New -> Import -> upload"
    log_info "    deploy/grafana/operational-dashboard.json"
    log_info "  Or send a cluster admin this:"
    log_info "    kubectl create configmap wva-operation-dashboard -n $ns \\"
    log_info "      --from-file=operational-dashboard.json=deploy/grafana/operational-dashboard.json \\"
    log_info "      && kubectl label configmap wva-operation-dashboard -n $ns grafana_dashboard=1"
}

install_operational_dashboard() {
    [ "${DEPLOY_OPERATIONAL_DASHBOARD:-true}" = "true" ] || return 0

    local json="$WVA_PROJECT/deploy/grafana/operational-dashboard.json"
    local ns watched
    ns="$(wva_choose_dashboard_ns)"
    watched="$(wva_grafana_dashboard_search_ns "$MONITORING_NAMESPACE")"
    if [ ! -f "$json" ]; then
        log_warning "Operational dashboard JSON not found at $json — skipping"
        return 0
    fi
    # Forbidden is not absent. Publishing into a SHARED monitoring namespace is a
    # cluster-admin action, and a namespace admin running `make deploy-wva` will
    # be denied here -- telling them the namespace "does not exist" sends them to
    # create one, when what they need is either their admin or a copy in their
    # own namespace.
    local ns_err
    if ! ns_err=$(kubectl get namespace "$ns" 2>&1 >/dev/null); then
        case "$ns_err" in
            *[Ff]orbidden*)
                log_info "No permission to read namespace $ns — the shared dashboard belongs to whoever administers monitoring."
                log_info "  The install is unaffected; only this dashboard step is skipped."
                wva_dashboard_manual_help "$ns"
                return 0
                ;;
            *)
                log_warning "Namespace $ns does not exist — skipping the operational dashboard. Set DASHBOARD_NS to the namespace your Grafana watches."
                return 0
                ;;
        esac
    fi

    # ONE dashboard object serves every install on the cluster: the name is
    # fixed and the namespace is shared, so ten namespace-scoped installs apply
    # the same ConfigMap. That is deliberate -- the dashboard is generic and
    # driven by variables, and ten identical copies in the picker would be
    # worse -- but it means an OLDER install must not quietly revert a newer
    # one. The version it was published with is stamped on the object, and a
    # lower version leaves it alone and says so.
    local ours existing
    # The tag wva_split_image_ref parsed, not "${IMG##*:}": that read the hex of
    # a digest (repo@sha256:<hex>) as the version, and the path of a tagless
    # registry-with-port reference (host:5000/wva) likewise. A digest-only
    # reference has no version to compare and publishes as "unknown".
    # `make dashboard` passes IMG alone, so fall back to its tag by the same rule:
    # a colon in the last path segment, and no digest.
    ours="${WVA_IMAGE_TAG:-}"
    if [ -z "$ours" ] && [ -n "${IMG:-}" ] && [[ "$IMG" != *@* ]] && [[ "${IMG##*/}" == *:* ]]; then
        ours="${IMG##*:}"
    fi
    [ -n "$ours" ] || ours="unknown"
    existing=$(kubectl get configmap wva-operation-dashboard -n "$ns" \
        -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/dashboard-version}' 2>/dev/null || true)

    # Compare only when BOTH sides look like versions. sort -V on a tag like
    # `wva-ext`, `latest` or `main` orders it against v0.7.0 by ASCII accident --
    # `wva-ext` would win and block every release publish thereafter, which is a
    # silent way to freeze a cluster's dashboard on somebody's dev build.
    # Compare the NUMERIC core only. sort -V puts v0.8.0-rc4 after v0.8.0, the
    # opposite of what a pre-release means, so an rc install would block the GA
    # release's dashboard for good.
    local semver='^v?[0-9]+\.[0-9]+' existing_core ours_core
    existing_core=$(printf '%s' "$existing" | sed 's/^v//; s/-.*$//')
    ours_core=$(printf '%s' "$ours" | sed 's/^v//; s/-.*$//')
    if [ -n "$existing" ] && [ "$existing_core" != "$ours_core" ] && \
       printf '%s' "$existing" | grep -qE "$semver" && \
       printf '%s' "$ours" | grep -qE "$semver" && \
       [ "$(printf '%s\n%s\n' "$existing_core" "$ours_core" | sort -V | tail -1)" = "$existing_core" ]; then
        log_info "Dashboard in $ns was published by $existing, newer than this install ($ours) — leaving it alone."
        log_info "  Republish with: kubectl delete configmap wva-operation-dashboard -n $ns"
        return 0
    fi

    # Pinning the namespace variable is only safe when this dashboard is NOT the
    # shared one. The ConfigMap name is fixed, so if it lands in a shared
    # monitoring namespace then ten installs write the same object -- pinning it
    # to one tenant's namespace would mean everyone sees whoever installed last,
    # which is worse than the "All" it replaced. Pin only when the dashboard is
    # published into the install's OWN namespace, where nobody else can be
    # looking at it.
    local scope patched
    scope="$(wva_install_scope)"
    patched="$(mktemp)"
    if [ "$scope" = "namespace" ] && [ "$ns" != "$WVA_NS" ]; then
        cp "$json" "$patched"
        log_info "Dashboard published to the shared $ns — namespace left selectable."
        if wva_dashboard_ns_is_watched "$WVA_NS" "$watched"; then
            log_info "  DASHBOARD_NS=$WVA_NS would publish a copy pinned to this namespace instead."
        fi
    elif [ "$scope" = "namespace" ]; then
        yq -o=json -I2 \
            '(.templating.list[] | select(.name == "namespace") | .current) =
                 {"text": "'"$WVA_NS"'", "value": "'"$WVA_NS"'"} |
             (.templating.list[] | select(.name == "namespace") | .includeAll) = false' \
            "$json" > "$patched" 2>/dev/null || cp "$json" "$patched"
        log_info "Dashboard scoped to $WVA_NS (namespace-scoped install)"
    else
        cp "$json" "$patched"
        log_info "Dashboard left cluster-wide (cluster-scoped install)"
    fi

    if kubectl create configmap wva-operation-dashboard \
        --from-file=operational-dashboard.json="$patched" \
        -n "$ns" --dry-run=client -o yaml \
        | kubectl label --local -f - grafana_dashboard=1 -o yaml \
        | kubectl annotate --local -f - "wva.llmd.ai/dashboard-version=$ours" -o yaml \
        | kubectl apply -f - >/dev/null; then
        log_success "Operational dashboard published to $ns (ConfigMap wva-operation-dashboard, version $ours, label grafana_dashboard=1)"
        # Publishing is not the same as appearing. Say so now rather than leaving
        # someone to conclude the dashboard is broken.
        if [ -n "$watched" ] && ! wva_dashboard_ns_is_watched "$ns" "$watched"; then
            log_warning "Grafana's sidecar watches '${watched}', not $ns — this ConfigMap will NOT appear as a dashboard."
            log_info "  Ask for grafana.sidecar.dashboards.searchNamespace=ALL, or:"
            wva_dashboard_manual_help "$ns"
        fi
        # The tenant's scoping is a LINK, not a default. A variable default lives
        # in the dashboard JSON, and this object is shared by every install on
        # the cluster -- so there is no per-tenant default to set. A URL gives
        # each namespace its own entry point into the one dashboard, with no
        # collisions and nothing for the next install to overwrite.
        local uid slug
        uid=$(yq -r '.uid // "wva-operational"' "$json" 2>/dev/null)
        slug=$(yq -r '.title // "wva"' "$json" 2>/dev/null \
               | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]\+/-/g; s/^-//; s/-$//')
        log_info "  This namespace's view: <your-grafana>/d/$uid/$slug?var-namespace=$WVA_NS"
        log_info "  Cluster-wide view:     <your-grafana>/d/$uid/$slug"
    else
        # Same distinction on the write: a tenant who can READ the shared
        # namespace still cannot usually create ConfigMaps in it.
        local apply_err
        apply_err=$(kubectl create configmap wva-operation-dashboard \
            --from-file=operational-dashboard.json="$patched" \
            -n "$ns" --dry-run=client -o yaml 2>&1 \
            | kubectl apply --dry-run=server -f - 2>&1 >/dev/null || true)
        case "$apply_err" in
            *[Ff]orbidden*)
                log_info "No permission to write the dashboard into $ns — it is the monitoring namespace's to own."
                log_info "  The install is unaffected; only this dashboard step is skipped."
                wva_dashboard_manual_help "$ns"
                ;;
            *)
                log_warning "Could not publish the operational dashboard to $ns: ${apply_err:-unknown error}"
                ;;
        esac
    fi
    rm -f "$patched"
}

# wva_detect_prometheus_url echoes an in-cluster URL for a Prometheus that is
# already here, or nothing.
#
# The platform defaults name the Prometheus THIS INSTALLER WOULD DEPLOY. On a
# cluster that has its own — the common case, and the whole point of the
# existing-llm-d path — that default is silently wrong, and WVA exits on a
# Prometheus it cannot reach. It presents as CrashLoopBackOff, which reads like a
# broken image rather than a setting nobody was asked for.
#
# Detection order is most-specific first. Everything here is a READ; the caller
# decides whether to use the answer.
wva_detect_prometheus_url() {
    local ns name port scheme

    # OpenShift: THANOS, at a well-known address, and no lookup at all.
    #
    # The platform's aggregation point is thanos-querier in openshift-monitoring,
    # and that is a constant — not something to discover. Reading it would need
    # access to a platform namespace that a tenant does not have, so a version of
    # this that "detected" it reported NOTHING for exactly the reader who most
    # needs the answer. (Verified on a real cluster: a namespace admin gets
    # nothing back.) Knowing beats looking.
    if wva_is_openshift; then
        # Refine the port only if we happen to be able to read it. 9091 is the
        # TLS-terminated one the platform has served for years; the read is a
        # nicety, never a requirement.
        port="$(kubectl get svc thanos-querier -n openshift-monitoring \
            -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null || true)"
        echo "https://thanos-querier.openshift-monitoring.svc.cluster.local:${port:-9091}"
        return 0
    fi

    # A Prometheus Operator install: the first Prometheus CR that scrapes the
    # model namespaces and has a Service in front of it. Which Service is
    # wva_pick_prometheus_service's decision.
    #
    # The API lists CRs by namespace name, so "the first CR" alone is whichever
    # namespace sorts first. On a shared cluster that was a new per-user
    # Prometheus selecting monitors only in its own namespace: the controller
    # read no series, every variant looked idle, and nothing reported an error.
    local crs targets cov v why want unserved=""
    crs="$(kubectl get prometheuses.monitoring.coreos.com -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
    targets="$(wva_prometheus_target_namespaces | paste -sd' ' -)"
    if [ -n "$crs" ] && [ -n "$targets" ]; then
        if cov="$(wva_prometheus_coverage "$targets")" && [ -n "$cov" ]; then
            while read -r ns name v why; do
                [ "$v" = yes ] || continue
                if wva_pick_prometheus_service "$ns" "$name"; then
                    return 0
                fi
                unserved="${unserved:+$unserved, }$ns/$name"
            done <<< "$cov"
            # None is known to scrape them. Still a fallback -- a URL that may
            # be right beats the shipped default, which names a Prometheus this
            # installer would deploy -- but said out loud, with the reasons.
            if [ -n "$unserved" ]; then
                log_warning "$unserved scrapes $targets, but no Service routes to it, so another Prometheus is picked and may read no metrics for your models:"
            else
                log_warning "No Prometheus is known to scrape $targets, so the one picked may read no metrics for your models:"
            fi
            while read -r ns name v why; do
                [ -n "$ns" ] || continue
                log_warning "  $ns/$name: $why"
            done <<< "$cov"
            if printf '%s\n' "$cov" | awk '$3 == "no"' | grep -q .; then
                log_warning "  (Only monitor selectors are read: a Prometheus that scrapes through additionalScrapeConfigs, or selects no monitors, is not seen here.)"
            fi
            log_warning "  Pass PROMETHEUS_URL=<url> of the Prometheus that scrapes $targets."
            # One that could not be checked before one known not to scrape them.
            for want in unknown no; do
                while read -r ns name v why; do
                    [ "$v" = "$want" ] || continue
                    if wva_pick_prometheus_service "$ns" "$name"; then
                        return 0
                    fi
                done <<< "$cov"
            done
        else
            log_warning "Could not check which Prometheus scrapes $targets (reading the Prometheus resources failed), so the first one listed is used. Pass PROMETHEUS_URL=<url> to choose."
        fi
    elif [ -n "$crs" ] && [ "$(printf '%s\n' "$crs" | grep -c .)" -gt 1 ]; then
        # Nothing to check against, and guessing a namespace is how a check
        # passes for the wrong one.
        if [ "${WVA_WATCH_NS:-}" = auto ]; then
            log_info "Several Prometheus resources, and WVA_WATCH_NS=auto has not found the model namespaces yet, so the first one is used without checking what it scrapes. Name them in WVA_WATCH_NS to check, or pass PROMETHEUS_URL=<url> to choose."
        else
            log_info "Several Prometheus resources and no model namespace named, so the first one is used without checking what it scrapes. Pass NAMESPACE=<model namespace> to check, or PROMETHEUS_URL=<url> to choose."
        fi
    fi
    while read -r ns name; do
        [ -n "$ns" ] || continue
        if wva_pick_prometheus_service "$ns" "$name"; then
            return 0
        fi
    done <<< "$crs"

    # Last resort: a Service serving a Prometheus web port, in the namespaces this
    # install already knows about. Deliberately NOT `get svc -A`: on a large
    # shared cluster that is slow enough to look hung, and a tenant cannot list
    # services cluster-wide anyway, so it would be a long wait for a denial.
    for ns in "${MONITORING_NAMESPACE:-}" "${WVA_NS:-}" "${NAMESPACE:-}" monitoring prometheus; do
        [ -n "$ns" ] || continue
        while read -r name port; do
            [ -n "$name" ] || continue
            case "$name" in *prometheus*|*thanos-quer*) : ;; *) continue ;; esac
            scheme=http
            # The scheme follows the port: a TLS port answered over http gives
            # "connection reset", which is a far worse thing to debug than
            # "no Prometheus found".
            case "$port" in 9091|8443) scheme=https ;; esac
            echo "${scheme}://${name}.${ns}.svc.cluster.local:${port}"
            return 0
        done < <(kubectl get svc -n "$ns" \
            -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.ports[0].port}{"\n"}{end}' 2>/dev/null || true)
    done

    return 0
}

# wva_prometheus_target_namespaces echoes the namespaces the models run in, one
# per line: the namespaces wva_monitor_namespaces looks after -- the
# WVA_WATCH_NS list, else WVA_NS in namespace scope -- and NAMESPACE as the
# caller set it in cluster scope. Nothing when WVA_WATCH_NS is still `auto`
# (nothing discovered yet), or in cluster scope with no NAMESPACE: that
# controller manages every namespace, and picking one to check would be a guess.
wva_prometheus_target_namespaces() {
    case "${WVA_WATCH_NS:-}" in
        auto) return 0 ;;
        ?*) wva_watch_list; return 0 ;;
    esac
    if [ "$(wva_install_scope)" = namespace ]; then
        [ -z "${WVA_NS:-}" ] || printf '%s\n' "$WVA_NS"
    elif [ -n "${NAMESPACE_EXPLICIT:-}" ]; then
        printf '%s\n' "$NAMESPACE_EXPLICIT"
    fi
    return 0
}

# The labels WVA's own monitors carry (config/modelserver-metrics and
# wva_epp_scrape_docs), for a namespace that has no monitor of that kind yet:
# setup-prereqs adds them later. When there is exactly one Prometheus it also
# stamps that Prometheus's selector matchLabels onto them
# (wva_prometheus_selector_labels), and wva_prometheus_coverage does the same.
WVA_SHIPPED_PODMONITOR_LABELS='{"app.kubernetes.io/name":"workload-variant-autoscaler","app.kubernetes.io/component":"modelserver-metrics","llm-d.ai/applied-by":"wva-setup-prereqs"}'
WVA_SHIPPED_SERVICEMONITOR_LABELS='{"app.kubernetes.io/name":"workload-variant-autoscaler","app.kubernetes.io/component":"epp-metrics","llm-d.ai/applied-by":"wva-setup-prereqs"}'

# wva_prometheus_coverage echoes "<namespace> <name> <yes|no|unknown> <why>"
# for every Prometheus CR, in the order the API lists them: whether it scrapes
# every namespace in $1 (space-separated). Fails when the CRs cannot be read.
#
# Prometheus Operator semantics, per monitor kind:
#   *MonitorNamespaceSelector  {} every namespace; unset only the CR's own;
#                              otherwise matchLabels / matchExpressions against
#                              the namespace's labels
#   *MonitorSelector           {} every monitor; unset none; otherwise the
#                              labels of the monitors in that namespace. With
#                              none there yet, WVA's own as setup-prereqs would
#                              label them: a match is yes, anything else
#                              unknown, since llm-d may still add its own
#
# BOTH kinds must select it. vLLM is scraped by a PodMonitor and the EPP by a
# ServiceMonitor; a Prometheus that has only one of them gives the controller
# half its signals (saturation without the flow-control queue, or the reverse),
# which looks like a working install with a scaler that never moves.
#
# A read that is refused is "unknown", never yes or no. A namespace whose
# labels cannot be read is still decided when the selector names only
# kubernetes.io/metadata.name, which is the namespace's name.
wva_prometheus_coverage() {
    local targets="$1" crs ns raw labels missing pm sm info='[]' errf
    crs="$(kubectl get prometheuses.monitoring.coreos.com -A -o json 2>/dev/null)" || return 1
    printf '%s' "$crs" | jq -e '.items | type == "array"' >/dev/null 2>&1 || return 1
    errf="$(mktemp)" || return 1
    for ns in $targets; do
        # stderr apart: a kubectl warning mixed into the JSON would read as
        # unreadable labels, and NotFound has to be told from Forbidden.
        raw="$(kubectl get namespace "$ns" -o json 2>"$errf")" || raw=""
        labels="$(printf '%s' "$raw" | jq -c '.metadata.labels // {}' 2>/dev/null)" || labels=""
        missing=false
        [ -n "$labels" ] || ! grep -q NotFound "$errf" || missing=true
        pm="$(kubectl get podmonitors.monitoring.coreos.com -n "$ns" -o json 2>/dev/null \
            | jq -c '[.items[]? | .metadata.labels // {}]' 2>/dev/null)" || pm=""
        sm="$(kubectl get servicemonitors.monitoring.coreos.com -n "$ns" -o json 2>/dev/null \
            | jq -c '[.items[]? | .metadata.labels // {}]' 2>/dev/null)" || sm=""
        info="$(jq -c --arg ns "$ns" --argjson l "${labels:-null}" --argjson missing "$missing" \
            --argjson pm "${pm:-null}" --argjson sm "${sm:-null}" \
            '. + [{ns: $ns, labels: $l, missing: $missing, pm: $pm, sm: $sm}]' <<< "$info")" || { rm -f "$errf"; return 1; }
    done
    rm -f "$errf"
    printf '%s' "$crs" | jq -r --argjson info "$info" \
        --argjson shippm "$WVA_SHIPPED_PODMONITOR_LABELS" --argjson shipsm "$WVA_SHIPPED_SERVICEMONITOR_LABELS" '
        def matches($sel; $l):
            (($sel.matchLabels // {}) | to_entries | all(. as $e | $l[$e.key] == $e.value))
            and (($sel.matchExpressions // []) | all(. as $x
                | ($l | has($x.key)) as $has
                | if $x.operator == "In" then $has and (($x.values // []) | any(. == $l[$x.key]))
                  elif $x.operator == "NotIn" then ($has | not) or (($x.values // []) | all(. != $l[$x.key]))
                  elif $x.operator == "Exists" then $has
                  elif $x.operator == "DoesNotExist" then ($has | not)
                  else false end));
        def selkeys($sel): [(($sel.matchLabels // {}) | keys[]), (($sel.matchExpressions // [])[] | .key)];
        def nsv($sel; $crns; $n):
            if $sel == null then (if $crns == $n.ns then "yes" else "no" end)
            elif $n.labels != null then (if matches($sel; $n.labels) then "yes" else "no" end)
            elif (selkeys($sel) | all(. == "kubernetes.io/metadata.name")) then
                (if matches($sel; {"kubernetes.io/metadata.name": $n.ns}) then "yes" else "no" end)
            else "unknown" end;
        # $mons: the labels of the monitors there, or null when listing them
        # failed. A refused list is unknown unless the selector takes every
        # monitor; an empty one is judged by what setup-prereqs would add.
        def monv($sel; $mons; $ship):
            if $sel == null then "no"
            elif (selkeys($sel) | length) == 0 then "yes"
            elif $mons == null then "unknown"
            elif ($mons | length) > 0 then (if ($mons | any(. as $m | matches($sel; $m))) then "yes" else "no" end)
            elif matches($sel; $ship) then "yes"
            else "unknown" end;
        def why($c):
            if $c.v == "unknown" then
                (if ($c.what | endswith("NamespaceSelector")) then
                    (if $c.missing then "could not check (namespace \($c.ns) does not exist yet)"
                     else "could not check (reading the labels of namespace \($c.ns) failed)" end)
                 elif $c.listed then "could not check (no \($c.kind) in \($c.ns) yet, and its \($c.what) does not match the labels setup-prereqs gives one)"
                 else "could not check (listing \($c.kind)s in \($c.ns) failed)" end)
            elif ($c.what | endswith("NamespaceSelector")) then "its \($c.what) does not select \($c.ns)"
            else "its \($c.what) selects no \($c.kind) in \($c.ns)" end;
        # setup-prereqs stamps the selector matchLabels of a lone Prometheus
        # onto the monitors it adds (wva_prometheus_selector_labels).
        (if (.items | length) == 1 then .items[0].spec // {} else {} end) as $lone
        | ($shippm + ($lone.podMonitorSelector.matchLabels // {})) as $shippm
        | ($shipsm + ($lone.serviceMonitorSelector.matchLabels // {})) as $shipsm
        | .items[]
        | .metadata.namespace as $crns | .metadata.name as $name | (.spec // {}) as $s
        | [ $info[] as $n
            | {ns: $n.ns, missing: $n.missing, kind: "PodMonitor", what: "podMonitorNamespaceSelector",
               v: nsv($s.podMonitorNamespaceSelector; $crns; $n)},
              {ns: $n.ns, missing: $n.missing, kind: "ServiceMonitor", what: "serviceMonitorNamespaceSelector",
               v: nsv($s.serviceMonitorNamespaceSelector; $crns; $n)},
              {ns: $n.ns, listed: ($n.pm != null), kind: "PodMonitor", what: "podMonitorSelector",
               v: monv($s.podMonitorSelector; $n.pm; $shippm)},
              {ns: $n.ns, listed: ($n.sm != null), kind: "ServiceMonitor", what: "serviceMonitorSelector",
               v: monv($s.serviceMonitorSelector; $n.sm; $shipsm)} ] as $c
        | (if any($c[]; .v == "no") then "no" elif all($c[]; .v == "yes") then "yes" else "unknown" end) as $v
        | (if $v == "yes" then "scrapes them" else why([$c[] | select(.v == $v)][0]) end) as $why
        | "\($crns) \($name) \($v) \($why)"'
}

# wva_pick_prometheus_service echoes the URL of the Service to reach Prometheus
# CR $2 in namespace $1 by, or fails when no Service routes to it.
#
# The controller verifies the Prometheus certificate, so the host must be a name
# that certificate carries. Every Prometheus CR gets a `prometheus-operated`
# Service, but a certificate rarely names it: a kube-prometheus-stack serves
# <release>-kube-prometheus-stack-prometheus, and that is the name its
# certificate is issued for. Picking prometheus-operated there gave a controller
# that crash-looped on "x509: certificate is valid for
# llmd-kube-prometheus-stack-prometheus.llm-d-monitoring.svc, ..., not
# prometheus-operated.llm-d-monitoring.svc.cluster.local", while the install
# reported that Prometheus as found.
#
# So: when the certificate can be read, the first Service (the CR's own before
# prometheus-operated) with a host form it names -- .svc.cluster.local, then
# .svc. When it cannot (no tlsConfig, a Forbidden read, no openssl), the CR's
# own Service, then prometheus-operated.
#
# The scheme comes from the Prometheus CR, not from the port number. A
# Prometheus with spec.web.tlsConfig serves HTTPS on its ordinary 9090 --
# which is what this repo's own kube-prometheus-stack install produces -- so
# calling it http:// pointed WVA at a TLS port in plaintext.
wva_pick_prometheus_service() {
    local ns="$1" name="$2" scheme=http services names svc port host
    services="$(wva_prometheus_services "$ns" "$name")"
    [ -n "$services" ] || return 1
    if [ -n "$(kubectl get prometheuses.monitoring.coreos.com "$name" -n "$ns" \
        -o jsonpath='{.spec.web.tlsConfig}' 2>/dev/null || true)" ]; then
        scheme=https
    fi
    if [ "$scheme" = https ] && names="$(wva_prometheus_cert_names "$ns" "$name")"; then
        while read -r svc port; do
            for host in "$svc.$ns.svc.cluster.local" "$svc.$ns.svc"; do
                if wva_cert_name_covers "$host" "$names"; then
                    echo "https://$host:$port"
                    return 0
                fi
            done
        done <<< "$services"
    fi
    read -r svc port <<< "$services"
    echo "$scheme://$svc.$ns.svc.cluster.local:$port"
}

# wva_prometheus_services echoes "<service> <port>", one per line, for each
# Service in namespace $1 that routes to Prometheus CR $2: first the CR's own
# (a selector naming it, operator.prometheus.io/name or the older prometheus
# label), sorted by name, then prometheus-operated, which the operator makes
# for every CR. The port is the one named http-web or web, else the first.
wva_prometheus_services() {
    local ns="$1" name="$2" port
    kubectl get svc -n "$ns" -o json 2>/dev/null | jq -r --arg n "$name" '
        [.items[]
         | select(.metadata.name != "prometheus-operated")
         | select(((.spec.selector // {})["operator.prometheus.io/name"] // (.spec.selector // {}).prometheus // "") == $n)]
        | sort_by(.metadata.name)[]
        | (.spec.ports // []) as $p
        | .metadata.name + " " + ((($p | map(select(.name == "http-web" or .name == "web")) | .[0]) // $p[0] // {}).port // 9090 | tostring)' 2>/dev/null || true
    if kubectl get svc prometheus-operated -n "$ns" >/dev/null 2>&1; then
        port="$(kubectl get svc prometheus-operated -n "$ns" \
            -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null || true)"
        echo "prometheus-operated ${port:-9090}"
    fi
}

# wva_prometheus_cert_names echoes the DNS names on the certificate Prometheus
# CR $2 in namespace $1 serves, one per line, from the Secret or ConfigMap its
# spec.web.tlsConfig.cert names. It reads that one key -- the public
# certificate -- and never the private key the CR names beside it. Fails when
# the CR has no such certificate, it cannot be read (Forbidden included: a read
# that was refused is not a certificate without names), or it names no host.
wva_prometheus_cert_names() {
    local ns="$1" name="$2" ref kind obj key pem names
    ref="$(wva_prometheus_cert_ref "$ns" "$name")"
    [ -n "$ref" ] || return 1
    read -r kind obj key <<< "$ref"
    [ -n "$key" ] || return 1
    pem="$(kubectl get "$kind" "$obj" -n "$ns" -o "jsonpath={.data.${key//./\\.}}" 2>/dev/null)" || return 1
    [ -n "$pem" ] || return 1
    if [ "$kind" = secret ]; then
        pem="$(printf '%s' "$pem" | base64 -d 2>/dev/null)" || return 1
    fi
    names="$(wva_cert_dns_names "$pem")"
    [ -n "$names" ] || return 1
    printf '%s\n' "$names"
}

# wva_prometheus_cert_ref echoes "<secret|configmap> <name> <key>": where the
# certificate Prometheus CR $2 in namespace $1 serves is stored, from its
# spec.web.tlsConfig.cert. Nothing when it has none, or the CR cannot be read.
wva_prometheus_cert_ref() {
    kubectl get prometheuses.monitoring.coreos.com "$2" -n "$1" -o json 2>/dev/null \
        | jq -r '.spec.web.tlsConfig.cert // {}
            | if .secret.name then "secret " + .secret.name + " " + (.secret.key // "tls.crt")
              elif .configMap.name then "configmap " + .configMap.name + " " + (.configMap.key // "tls.crt")
              else empty end' 2>/dev/null || true
}

# wva_cert_dns_names echoes the DNS subject alternative names of PEM
# certificate $1, one per line. Go verifies the host against these alone; the
# common name has not counted since Go 1.15.
wva_cert_dns_names() {
    printf '%s\n' "$1" | openssl x509 -noout -text 2>/dev/null \
        | awk '/X509v3 Subject Alternative Name/ { getline; print; exit }' \
        | tr ',' '\n' | sed -n 's/^[[:space:]]*DNS://p'
}

# wva_cert_name_covers reports whether one of the certificate names in $2 (one
# per line) covers host $1: equal ignoring case, or a "*." wildcard standing for
# exactly one leftmost label, as Go matches them.
wva_cert_name_covers() {
    local host name label
    host="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
    label="${host%%.*}"
    while IFS= read -r name; do
        name="$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')"
        [ -n "$name" ] || continue
        [ "$name" != "$host" ] || return 0
        case "$name" in
            \*.*) [ -n "$label" ] && [ "$host" != "$label" ] && [ "$host" = "$label.${name#\*.}" ] && return 0 ;;
        esac
    done <<< "$2"
    return 1
}

# wva_prometheus_cert_check reports whether the certificate of the Prometheus
# that URL $1 reaches names the host the controller verifies: $2 (the
# PROMETHEUS_SERVER_NAME it runs with) when set, else the URL's host.
#   0  a Prometheus certificate read in that namespace names it
#   1  the Service the URL names routes to a Prometheus CR whose certificate
#      was read and does not name it: the controller cannot connect
#   2  cannot tell -- not https, not a cluster Service, a Service that is not
#      a Prometheus CR's (a Thanos has a certificate of its own), or a
#      certificate that cannot be read
# Sets WVA_PROM_CERT_NAMES to the names read, comma-separated, and
# WVA_PROM_CERT_SUGGEST to a URL for a Service there whose host they name.
WVA_PROM_CERT_NAMES=""
WVA_PROM_CERT_SUGGEST=""
wva_prometheus_cert_check() {
    local url="$1" host ns svc cr names services s p h rc=2
    WVA_PROM_CERT_NAMES="" WVA_PROM_CERT_SUGGEST=""
    case "$url" in https://*) ;; *) return 2 ;; esac
    host="${url#*://}"
    host="${host%%/*}"
    host="${host##*@}"
    host="${host%%:*}"
    svc="${host%%.*}"
    ns="$(wva_prometheus_url_namespace "$url")"
    [ -n "$ns" ] || return 2
    case "$host" in
        "$svc.$ns.svc"|"$svc.$ns.svc.cluster.local") ;;
        *) return 2 ;;
    esac
    [ -z "${2:-}" ] || host="$2"
    while read -r cr; do
        [ -n "$cr" ] || continue
        services="$(wva_prometheus_services "$ns" "$cr")"
        printf '%s\n' "$services" | awk -v s="$svc" '$1 == s { found = 1 } END { exit !found }' || continue
        names="$(wva_prometheus_cert_names "$ns" "$cr")" || continue
        WVA_PROM_CERT_NAMES="$(printf '%s\n' "$names" | paste -sd, - | sed 's/,/, /g')"
        if wva_cert_name_covers "$host" "$names"; then
            return 0
        fi
        rc=1
        while read -r s p; do
            for h in "$s.$ns.svc.cluster.local" "$s.$ns.svc"; do
                if wva_cert_name_covers "$h" "$names"; then
                    WVA_PROM_CERT_SUGGEST="https://$h:$p"
                    break 2
                fi
            done
        done <<< "$services"
    done < <(kubectl get prometheuses.monitoring.coreos.com -n "$ns" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
    return "$rc"
}

# wva_prometheus_cr_for_url echoes "<namespace> <prometheus-cr>" for the
# Prometheus CR whose Service URL $1 names -- its own Service or
# prometheus-operated -- or nothing.
wva_prometheus_cr_for_url() {
    local url="$1" host ns svc cr
    host="${url#*://}"
    host="${host%%/*}"
    host="${host##*@}"
    host="${host%%:*}"
    svc="${host%%.*}"
    ns="$(wva_prometheus_url_namespace "$url")"
    [ -n "$ns" ] || return 0
    case "$host" in
        "$svc.$ns.svc"|"$svc.$ns.svc.cluster.local") ;;
        *) return 0 ;;
    esac
    while read -r cr; do
        [ -n "$cr" ] || continue
        if wva_prometheus_services "$ns" "$cr" | awk -v s="$svc" '$1 == s { found = 1 } END { exit !found }'; then
            printf '%s %s\n' "$ns" "$cr"
            return 0
        fi
    done < <(kubectl get prometheuses.monitoring.coreos.com -n "$ns" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
}

# wva_is_openshift reports whether this is an OpenShift cluster.
#
# ENVIRONMENT first, because the make targets set it and it costs nothing. The
# fallback asks API DISCOVERY, which every authenticated user may read — unlike
# the platform namespaces, which a tenant may not.
wva_is_openshift() {
    [ "${ENVIRONMENT:-}" = "openshift" ] && return 0
    kubectl api-resources --api-group=route.openshift.io -o name >/dev/null 2>&1 \
        && [ -n "$(kubectl api-resources --api-group=route.openshift.io -o name 2>/dev/null)" ]
}

# wva_prometheus_endpoint returns the endpoint WVA will actually read, by the
# same precedence infra_wva.sh applies when it patches the controller config:
# an explicit PROMETHEUS_URL, else whatever is already on the cluster. Empty
# when neither -- meaning this install intends to deploy one.
#
# It exists because three places claimed to answer this question and gave three
# different answers on one run: the preflight named the cluster's own Prometheus,
# setup-prereqs named the shipped default, and the final summary said Prometheus
# was deployed in a namespace that does not exist. Only the first was true.
wva_prometheus_endpoint() {
    if [ -n "${PROMETHEUS_URL_EXPLICIT:-}" ]; then
        printf '%s' "$PROMETHEUS_URL_EXPLICIT"
        return 0
    fi
    # || true: a detector that finds nothing must not abort the caller under
    # `set -e` -- see the note in install_core.sh about assignments and $?.
    local detected
    detected="$(wva_detect_prometheus_url 2>/dev/null)" || detected=""
    printf '%s' "$detected"
}

# wva_report_prometheus tells the reader which Prometheus this install will use,
# and how that was decided. Called by the preflight, where "what do I pass for
# PROMETHEUS_URL?" is the question actually being asked.
wva_report_prometheus() {
    local detected
    if [ -n "${PROMETHEUS_URL_EXPLICIT:-}" ]; then
        log_info "Prometheus: $PROMETHEUS_URL_EXPLICIT (you set PROMETHEUS_URL)"
        wva_report_prometheus_cert_name "$PROMETHEUS_URL_EXPLICIT"
        return 0
    fi
    detected="$(wva_detect_prometheus_url)"
    if [ -n "$detected" ]; then
        # Found is not usable: a certificate that does not name the host is a
        # controller that cannot connect, so it is not reported as a success.
        if ! wva_report_prometheus_cert_name "$detected"; then
            log_warning "Prometheus: $detected (found, but see the certificate warning above)"
            return 0
        fi
        log_success "Prometheus: $detected"
        if wva_is_openshift; then
            log_info "  OpenShift's Thanos Querier — a fixed address, so this needs no permission to know."
        fi
        log_info "  You do not need to pass PROMETHEUS_URL. Set it only to override this."
        return 0
    fi
    # Never fold "I may not look" into "there is none": one is a missing
    # permission and the other is a missing Prometheus, and they are fixed by
    # different people.
    if ! kubectl auth can-i list prometheuses.monitoring.coreos.com -A >/dev/null 2>&1; then
        log_warning "Could not look for a Prometheus (listing them cluster-wide is not permitted for you), so this cannot tell you the URL."
        log_warning "  Ask whoever runs your monitoring for it, and pass PROMETHEUS_URL=<url>."
        return 0
    fi
    log_warning "No Prometheus found on this cluster. The install will deploy one (DEPLOY_PROMETHEUS=true), or pass PROMETHEUS_URL=<url> to point at one outside the cluster."
}

# wva_report_prometheus_cert_name warns, and fails, when the certificate of
# the Prometheus URL $1 reaches is known not to name its host. Anything it
# cannot tell passes: the install probes the connection itself later
# (wva_verify_prometheus_tls).
wva_report_prometheus_cert_name() {
    local rc=0
    wva_prometheus_cert_check "$1" || rc=$?
    [ "$rc" -eq 1 ] || return 0
    log_warning "The Prometheus certificate does not name the host in $1, so a controller that verifies it cannot connect."
    wva_prometheus_cert_name_advice "$1"
    return 1
}

# wva_prometheus_cert_name_advice prints the names the Prometheus certificate
# holds (WVA_PROM_CERT_NAMES, from wva_prometheus_cert_check) and how to point
# the controller at one of them.
wva_prometheus_cert_name_advice() {
    log_warning "  The certificate is valid for: ${WVA_PROM_CERT_NAMES:-names that could not be read}"
    if [ -n "$WVA_PROM_CERT_SUGGEST" ]; then
        log_warning "  Re-run with the Service it names:  $(wva_rerun_cmd "PROMETHEUS_URL=$WVA_PROM_CERT_SUGGEST")"
    else
        log_warning "  Re-run with PROMETHEUS_URL=https://<one of those names>:<port>, a Service in front of that Prometheus."
    fi
    log_warning "  See docs/reference/troubleshooting.md#prometheus-certificate-name-mismatch."
}

deploy_monitoring_stack() {
    if [ "$DEPLOY_PROMETHEUS" != "true" ]; then
        log_info "Skipping Prometheus deployment (DEPLOY_PROMETHEUS=false) — WVA will read ${PROMETHEUS_URL:-the configured URL}"
        install_operational_dashboard
        return 0
    fi

    # A namespace-scoped install does not create the monitoring namespace -- see
    # create_namespaces in infra_wva.sh, which skips it deliberately because a
    # namespace admin is not allowed to create one -- so it cannot deploy a
    # Prometheus into it either. Those two steps disagreed: the namespace was
    # skipped and the deployment went ahead anyway, applying the TLS secret into a
    # namespace that did not exist. `&> /dev/null` on that apply swallowed the
    # NotFound, `set -e` ended the install there, and the operator got
    # "Creating Kubernetes secret for Prometheus TLS" followed by nothing.
    #
    # Reproduced on kind: namespace scope, no Prometheus on the cluster, exit 2
    # with no error text.
    # Tenant phase only. `make setup-prereqs` is the cluster-admin phase and may
    # create the namespace -- create_namespaces does, just above -- so refusing
    # there would block the very command that fixes this.
    if [ "${INSTALL_PHASE:-all}" = "wva" ] \
        && ! kubectl get namespace "$MONITORING_NAMESPACE" >/dev/null 2>&1; then
        log_error "  This install has no Prometheus to use, and cannot deploy one.

WVA reads its metrics from Prometheus and exits without one. This is the tenant
half of the install, which only creates objects inside $WVA_NS, and deploying a
Prometheus means creating the namespace $MONITORING_NAMESPACE -- a cluster-admin
act.

  point at a Prometheus that already exists:  PROMETHEUS_URL=<url> make <target>
  or have a cluster admin run the first step: make setup-prereqs"
    fi

    # DEPLOY_PROMETHEUS defaults to true because the new-cluster path needs a
    # Prometheus and most people running this have none. That default is wrong for
    # the OTHER common case — an existing llm-d cluster, which always has one — so
    # detect rather than obey. KEDA is handled the same way a few files over; this
    # was the one piece of shared infrastructure that still installed blind.
    local existing
    existing="$(foreign_prometheus)"
    if [ -n "$existing" ] && [ "${PROMETHEUS_FORCE_INSTALL:-false}" != "true" ]; then
        log_info "Prometheus is already on this cluster (${existing% }) — not installing a second one."
        # The endpoint the install will USE, not the shipped default. The default
        # names the Prometheus this installer would have deployed, and on a
        # cluster that already has its own that namespace does not exist -- so
        # this line used to contradict both the preflight before it and
        # infra_wva.sh after it, which detects the real one and overrides.
        local will_scrape
        will_scrape="$(wva_prometheus_endpoint)" || will_scrape=""
        if [ -n "$will_scrape" ]; then
            log_info "  WVA will scrape $will_scrape. Re-run with PROMETHEUS_URL=<url> to override."
        else
            log_info "  WVA will scrape ${PROMETHEUS_URL:-the shipped default}. If that is not the right endpoint, re-run with PROMETHEUS_URL=<url>."
        fi
        log_info "  Pass PROMETHEUS_FORCE_INSTALL=true to install anyway (two Prometheus Operators reconcile the same CRs and will fight)."
        # The dashboard is still ours to ship, and this is precisely the cluster
        # whose operator has a Grafana to put it in.
        install_operational_dashboard
        return 0
    fi
    if [ -n "$existing" ]; then
        log_warning "PROMETHEUS_FORCE_INSTALL=true with Prometheus already present (${existing% }). The two operators will contend over the same Prometheus and ServiceMonitor resources."
    fi

    deploy_prometheus_stack
}

# --- EPP flow control ---------------------------------------------------------
#
# WVA needs the EPP's flow-control queue, and the guide understates it as a
# scale-from-zero concern. Three things depend on it:
#
#   scale-from-zero   at 0 replicas there are no engine metrics, so the scheduler
#                     queue is the ONLY evidence anyone is asking for the model.
#                     Without it a parked workload never wakes.
#   arrival rate      domain/analyzer.go: "Zero when the metric is unavailable (EPP
#                     absent or no traffic yet)". Absence is indistinguishable from
#                     idleness, so requests queued but not yet dispatched are
#                     invisible to the demand model.
#   wva_unmeasured_queue
#                     the detector for "serving traffic through pods WVA cannot
#                     attribute" — an unscraped FMA launcher, a PodMonitor naming a
#                     port the pods do not declare, ownerReferences reaching no
#                     scale target. It is sourced from this queue PRECISELY because
#                     it does not depend on any engine pod being scraped. With the
#                     gate off it reads 0 forever, so the safety net is disabled by
#                     the same absence it exists to catch.
#
# The gate is not a container flag. It is declared in the EPP's own plugins config:
#
#     featureGates:
#       - flowControl
#
# Measured on the shared GPU cluster: the namespaces whose EPP config carries that list have
# inference_extension_flow_control_queue_size series; the one built from llm-d's
# optimized-baseline guide has no featureGates section and no series.
#
# WVA reads the queue by scraping the EPP directly with a token mounted at
# /var/run/secrets/epp-metrics/token, and falls back to the same metric in
# Prometheus. Either path is fine — but neither has anything to read while the gate
# is off, which is why this checks the gate rather than the plumbing.

# wva_epp_services echoes the EPP Service each InferencePool in $1 points at.
# Read from the pool's endpointPickerRef rather than guessed from a name, because
# that reference is what the gateway itself dials.
wva_epp_services() {
    local ns="$1" kind
    for kind in inferencepools.inference.networking.k8s.io inferencepools.inference.networking.x-k8s.io; do
        kubectl get "$kind" -n "$ns" -o json 2>/dev/null | jq -r '
            .items[]?
            | (.spec.endpointPickerRef // .spec.extensionRef // {})
            | select((.name // "") != "")
            | .name' 2>/dev/null
    done | sort -u
}

# wva_epp_flowcontrol_state echoes on|off|unknown for the EPP behind Service $2.
#
# Resolves the config the way the container does: the --config-file argument names
# a path, the volumeMount covering that path names a volume, and the volume names
# the ConfigMap and therefore the key. Guessing "<service>-plugins.yaml" would be
# right here and wrong for any guide that names its file differently.
wva_epp_flowcontrol_state() {
    local ns="$1" svc="$2" sel pod cfg key sub cm data
    sel=$(kubectl get svc "$svc" -n "$ns" -o json 2>/dev/null \
        | jq -r '(.spec.selector // {}) | to_entries | map(.key + "=" + .value) | join(",")')
    [ -n "$sel" ] || { echo unreadable; return 0; }
    # One fetch, reused for every field below.
    pod=$(kubectl get pods -n "$ns" -l "$sel" -o json 2>/dev/null | jq -c '.items[0] // empty')
    # No EPP pod RIGHT NOW is not a gate that is off: an EPP restarting, or one
    # scaled to zero, would otherwise refuse the install.
    [ -n "$pod" ] || { echo nopod; return 0; }

    # `--config-file <path>` and `--config-file=<path>` are both legal.
    cfg=$(printf '%s' "$pod" | jq -r '
        (.spec.containers[] | select((.args // []) | any(. == "--config-file"))
         | (.args | index("--config-file")) as $i | .args[$i + 1]),
        (.spec.containers[].args[]? | select(startswith("--config-file=")) | sub("^--config-file="; ""))
        | select(. != null and . != "")' 2>/dev/null | head -1)
    [ -n "$cfg" ] || { echo unreadable; return 0; }
    key="${cfg##*/}"
    # A subPath mount places ONE ConfigMap key at the exact file path, and that key
    # is the subPath -- which need not equal the file's basename. Read it when the
    # mount is a file mount rather than assuming the two agree.
    sub=$(printf '%s' "$pod" | jq -r --arg cfg "$cfg" '
        .spec.containers[].volumeMounts[]? | select(.mountPath == $cfg) | .subPath // empty' 2>/dev/null | head -1)
    [ -n "$sub" ] && key="$sub"

    # Resolved the way the container resolves it: the volumeMount covering the
    # config's directory names a volume, and that volume names the ConfigMap and
    # therefore the key. Guessing "<service>-plugins.yaml" is right for one guide and
    # wrong for the next.
    # Either shape of mount: the DIRECTORY, or the file itself via subPath, which
    # is a standard way to place a single config file and previously resolved to no
    # ConfigMap at all -- reported as unreadable, which now refuses the install.
    cm=$(printf '%s' "$pod" | jq -r --arg dir "${cfg%/*}" --arg cfg "$cfg" '
        (.spec.containers[].volumeMounts[]? | select(.mountPath == $dir or .mountPath == $cfg) | .name) as $v
        | .spec.volumes[]? | select(.name == $v) | .configMap.name // empty' 2>/dev/null | head -1)
    [ -n "$cm" ] || { echo unreadable; return 0; }

    data=$(kubectl get configmap "$cm" -n "$ns" -o jsonpath="{.data.${key//./\\.}}" 2>/dev/null)
    [ -n "$data" ] || { echo unreadable; return 0; }
    # The gate is an ENTRY in a featureGates list. Matched on the entry rather than on
    # the word appearing anywhere in the file, so a plugin merely named after flow
    # control does not read as the gate being on.
    #
    # Both YAML spellings of a list, because this now REFUSES an install: the block
    # form is what llm-d's guides/flow-control/tuning.md and this repo's own
    # epp-flow-control.values.yaml write, but `featureGates: [flowControl]` is the
    # same thing and is how this gate is described in prose. Quotes and a trailing
    # comment are stripped for the same reason. Reading a correctly enabled gate as
    # off would refuse a working cluster, and the only way past it -- SKIP_CHECKS=true
    # -- switches off every preflight check rather than correcting this one.
    # \042 and \047 are " and ', which would otherwise fight the shell quoting here.
    if printf '%s' "$data" | awk '
        # inline: featureGates: [flowControl, other]
        /^[[:space:]]*featureGates:[[:space:]]*\[/ {
            line = $0
            sub(/^[^[]*\[/, "", line); sub(/\].*$/, "", line)
            n = split(line, a, ",")
            for (i = 1; i <= n; i++) {
                gsub(/[[:space:]\042\047]/, "", a[i])
                if (a[i] == "flowControl") { found = 1; exit }
            }
            next
        }
        /^[[:space:]]*featureGates:/ { inlist = 1; next }
        inlist && /^[[:space:]]*#/ { next }
        inlist && /^[[:space:]]*-/ {
            item = $0
            sub(/^[[:space:]]*-[[:space:]]*/, "", item)
            sub(/[[:space:]]+#.*$/, "", item)
            gsub(/^[\042\047]|[\042\047]$/, "", item)
            sub(/[[:space:]]+$/, "", item)
            if (item == "flowControl") { found = 1; exit }
            next
        }
        inlist && /^[[:space:]]*[^-[:space:]]/ { inlist = 0 }
        END { exit !found }'; then
        echo on
    else
        echo off
    fi
}

# wva_epp_scrapers echoes any PodMonitor/ServiceMonitor in $1 whose matchLabels
# select the EPP pods behind Service $2 — i.e. something scrapes the EPP.
#
# Checked structurally rather than by querying Prometheus, because the Prometheus
# URL is in-cluster and unreachable from wherever the installer runs. matchLabels
# only: a matchExpressions selector is reported as "cannot tell" rather than
# guessed at, since a wrong "yes" here would suppress the warning that matters.
wva_epp_scrapers() {
    local ns="$1" svc="$2" svc_json sel pod svc_labels pod_labels

    # Each object is fetched ONCE. An earlier version re-read the Service twice and
    # the pod three times across this function and the gate check, which cost ~13s
    # per EPP on a real cluster and pushed the preflight past two minutes.
    svc_json=$(kubectl get svc "$svc" -n "$ns" -o json 2>/dev/null)
    [ -n "$svc_json" ] || return 0
    svc_labels=$(printf '%s' "$svc_json" | jq -c '.metadata.labels // {}')
    sel=$(printf '%s' "$svc_json" | jq -r '(.spec.selector // {}) | to_entries | map(.key + "=" + .value) | join(",")')

    pod_labels='{}'
    if [ -n "$sel" ]; then
        pod=$(kubectl get pods -n "$ns" -l "$sel" -o json 2>/dev/null \
            | jq -c '.items[0] // empty')
        [ -n "$pod" ] && pod_labels=$(printf '%s' "$pod" | jq -c '.metadata.labels // {}')
    fi

    # A ServiceMonitor selects SERVICES and a PodMonitor selects PODS, so each is
    # matched against the labels of the object it actually selects. Comparing both
    # against pod labels reported "nothing scrapes the EPP" for a namespace whose EPP
    # ServiceMonitor was working -- its selector names the SERVICE's
    # app.kubernetes.io/name and version, which the pods do not both carry.
    #
    # matchLabels is a subset test: every key/value the monitor names must be present
    # on the target. matchExpressions is not evaluated, so a monitor using only those
    # reads as "not scraping" -- the safe direction, since a wrong "yes" would
    # suppress the warning that matters.
    local subset='
        .items[]?
        | . as $m
        | (.spec.selector.matchLabels // {}) as $ml
        | select(($ml | length) > 0)
        | select([ $ml | to_entries[] | select($l[.key] == .value) ] | length == ($ml | length))
        | $kind + "/" + $m.metadata.name'

    # matchExpressions is still not evaluated, but a monitor using ONLY those is now
    # reported as unevaluated rather than counted as absent. Ignoring them was called
    # "the safe direction, since a wrong yes would suppress the warning that matters"
    # -- true while this was a warning. It now refuses the install, so a wrong NO
    # blocks a correctly monitored cluster, and the safe direction is to say "cannot
    # tell" out loud.
    # Each filter is PARENTHESISED where they are combined below. `|` binds looser
    # than `,` in jq, so "$subset, $unevaluated" parsed as
    #     .items[]? | ... | (name_string, .items[]? | ...)
    # -- the second filter applied to the STRING the first produced, which fails
    # with `Cannot index string with string ("spec")`. stderr is sent to /dev/null
    # here, so the function simply returned nothing, and the gate refused the
    # install of a namespace whose EPP ServiceMonitor was working. Measured on
    # the shared GPU cluster: the selector matched the Service 2/2 by hand, `$subset` alone
    # returned the monitor, and the combined program returned an error.
    local unevaluated='
        .items[]?
        | . as $m
        | select((.spec.selector.matchLabels // {} | length) == 0)
        | select((.spec.selector.matchExpressions // [] | length) > 0)
        | "unevaluated:" + $kind + "/" + $m.metadata.name'

    kubectl get servicemonitors -n "$ns" -o json 2>/dev/null         | jq -r --argjson l "$svc_labels" --arg kind servicemonitor "($subset), ($unevaluated)" 2>/dev/null
    # WVA's own model-server PodMonitor selects the serving roles by
    # matchExpressions and never the EPP. Counted, it read as "cannot tell
    # whether the EPP is scraped" in every namespace setup had just given one.
    kubectl get podmonitors -n "$ns" -o json 2>/dev/null \
        | jq '.items |= map(select((.metadata.labels["app.kubernetes.io/component"] // "") != "modelserver-metrics"))' 2>/dev/null \
        | jq -r --argjson l "$pod_labels" --arg kind podmonitor "($subset), ($unevaluated)" 2>/dev/null
}

# wva_report_epp_flowcontrol reports both EPP requirements and returns non-zero
# when either is missing, so the caller decides whether that is fatal.
#
# They are SEPARATE requirements with different fixes, measured on the shared GPU cluster:
#
#   inference_extension_scheduler_attempts_total   19 namespaces — published
#     (arrival rate -> throughput analyzer)        whenever the EPP is SCRAPED.
#                                                  No gate needed.
#   inference_extension_flow_control_queue_size     4 namespaces — only where the
#     (scale-from-zero, wva_unmeasured_queue)       flowControl GATE is on.
#
# The arrival-rate query is PromQL (registration/throughput_analyzer.go), so it
# needs Prometheus to scrape the EPP; WVA's direct EPP scrape covers only the queue
# fallback. So "the gate is on" and "the EPP is scraped" are both required, and
# neither implies the other.
wva_report_epp_flowcontrol() {
    local ns="${WVA_WATCH_NS:-$WVA_NS}" svc state scrapers unevaluated any=false missing=0 unconfirmed=0 degraded=0

    for svc in $(wva_epp_services "$ns"); do
        any=true

        # Called ONCE and split, not twice: this function issues four kubectl reads
        # per EPP, and calling it again to pick out the unevaluated monitors would
        # undo the fetch-once work above that took a real cluster from ~13s to ~9s.
        local monitors
        monitors="$(wva_epp_scrapers "$ns" "$svc")"
        scrapers="$(printf '%s
' "$monitors" | grep -v '^unevaluated:' | grep -v '^$' | paste -sd, -)"
        unevaluated="$(printf '%s
' "$monitors" | sed -n 's/^unevaluated://p' | paste -sd, -)"
        if [ -n "$scrapers" ]; then
            log_success "  EPP metrics: $svc is scraped by $scrapers (arrival rate, and so the throughput analyzer, depends on this)."
        elif [ -n "$unevaluated" ]; then
            unconfirmed=$((unconfirmed + 1))
            log_warning "  EPP metrics: cannot tell whether $svc is scraped — $unevaluated select by matchExpressions, which this check does not evaluate."
            log_warning "    Not treated as missing: refusing an install over a selector form this script declines to read would block a correctly monitored cluster."
            log_warning "    Confirm by hand:  kubectl get --raw /api/v1/namespaces/$ns/services/$svc:metrics/proxy/metrics | head"
        elif [ "${WVA_MONITORS_PLANNED:-false}" = true ]; then
            # Added by the admin phase (wva_ensure_llmd_monitors), like the
            # model-server PodMonitor above.
            log_info "  EPP metrics: nothing scrapes $svc yet. Setup adds ServiceMonitor ${WVA_EPP_SCRAPE_NAME} here."
        else
            missing=$((missing + 1))
            log_warning "  Router monitoring not enabled; please enable EPP metrics."
            log_warning "    The cluster-admin step adds it for you:  make setup-prereqs (with the same SCOPE)"
            log_warning "    Or llm-d's own: re-run the router install with guides/recipes/router/features/monitoring.values.yaml"
        fi

        state=$(wva_epp_flowcontrol_state "$ns" "$svc")
        case "$state" in
            on)
                log_success "  EPP flow control: enabled on $svc (its config declares featureGates: [flowControl])." ;;
            off)
                # A warning, not a refusal. Nothing but scale-from-zero needs
                # the gate: the arrival rate falls back to the EPP's placements
                # (throughput_analyzer.go, arrivalPlacementRate) and the
                # scheduler queue is optional to the saturation analyzer. llm-d
                # ships the gate off (flow control is an explicit opt-in), so
                # refusing here blocked every fresh llm-d install for a feature
                # most of them do not use. The installer does not turn it on:
                # the gate is a line inside the router's Helm values, and an
                # edit made beside Helm is reverted by its next upgrade.
                degraded=$((degraded + 1))
                log_warning "  EPP flow control: off on $svc. Not refused: scaling works without it. Two things do not:"
                log_warning "    a model at zero replicas never wakes, and the arrival rate reads low while a model is saturated."
                log_warning "    To turn it on, add to the EPP's EndpointPickerConfig, re-run your llm-d router install, then restart the EPP:"
                log_warning "        featureGates:"
                log_warning "        - flowControl"
                log_warning "    llm-d ships a ready file: guides/workload-autoscaling/keda-epp/<guide>/router.values.yaml" ;;
            nopod)
                # No EPP pod at this instant. A restarting or scaled-to-zero EPP is
                # not a gate that is off, and refusing the install for it would block
                # on a condition that resolves itself.
                unconfirmed=$((unconfirmed + 1))
                log_warning "  EPP flow control: no running pod behind $svc, so the gate cannot be read right now. Not treated as missing; re-run once the EPP is up." ;;
            *)
                # Fails CLOSED, deliberately: "cannot READ the config" is the case
                # where this kubectl identity can see the Service but not the Pods or
                # ConfigMaps -- check_permissions verifies only create rights on what
                # the install creates, never read rights on these -- and an unconfirmed
                # signal is exactly what this check exists to stop. Distinct from
                # `nopod` above, which is transient rather than a permissions gap.
                missing=$((missing + 1))
                log_warning "  EPP flow control: could not read $svc's plugins config, so this cannot confirm the gate is on. Check that its EndpointPickerConfig declares the flowControl feature gate, and that this kubectl identity can read Pods and ConfigMaps in $ns." ;;
        esac
    done

    if [ "$any" = false ]; then
        log_info "  EPP: no InferencePool in $ns names an endpoint picker, so there is no EPP to check yet."
        return 0
    fi
    [ "$unconfirmed" -eq 0 ] || log_info "  $unconfirmed EPP signal(s) could not be confirmed either way; these do not block the install."
    # Only CONFIRMED-absent signals are fatal. "I could not look" and "I looked and
    # it is not there" are different answers, and only the second is evidence about
    # the cluster.
    [ "$missing" -eq 0 ]
}

# wva_require_epp_metrics turns the report into a gate.
#
# Fatal by default, because every symptom of these being absent is silent: WVA
# still emits decisions, the HPA still reads a healthy ratio, and the one metric
# that would have flagged it is sourced from the missing signal. An install that
# cannot see demand is not a working install, and saying so at install time is the
# only place anybody is looking.
wva_require_epp_metrics() {
    wva_report_epp_flowcontrol && return 0
    log_error "Refusing to continue — fix the EPP above, or SKIP_CHECKS=true (see docs/reference/operations.md#first-line-troubleshooting)."
}

# --- Kueue as the budget ------------------------------------------------------
#
# With WVA_QUOTA_KUEUE=true the quota's figure comes from Kueue's ClusterQueues,
# and Kueue charges a model server's GPUs only when its pods go through a
# LocalQueue. Three things make that true in a namespace, and each one missing
# is silent: the install succeeds, the controller logs a budget, and the
# namespace's GPUs are charged to no queue.
#
#   1. a LocalQueue in the namespace, pointing at a ClusterQueue that exists,
#      declares a GPU resource, and whose namespaceSelector admits the
#      namespace (internal/kueue/reader.go counts nothing else);
#   2. every model server naming that LocalQueue in kueue.x-k8s.io/queue-name,
#      on the object or its pod template -- or the namespace selected by a
#      LocalQueue named `default`, which Kueue fills in on objects created
#      after it;
#   3. the running model-server pods carrying the label, which is what Kueue
#      actually charges. A Deployment created before its `default` LocalQueue
#      keeps unlabelled pods, and Kueue refuses to add the label to it later
#      (the label is immutable on the Deployment), so the fix is the pod
#      template.

WVA_KUEUE_GPU_RESOURCES='["nvidia.com/gpu","amd.com/gpu","habana.ai/gaudi","gpu.intel.com/i915","gpu.intel.com/xe"]'
WVA_KUEUE_QUEUE_LABEL="kueue.x-k8s.io/queue-name"

# wva_kueue_budget_wanted reports whether this run declares a quota bounded by
# Kueue: WVA_LIMITER naming quota, with WVA_QUOTA_KUEUE=true.
wva_kueue_budget_wanted() {
    local set
    [ "${WVA_QUOTA_KUEUE:-false}" = true ] || return 1
    # A WVA_LIMITER limiter_types_of refuses is the install's refusal to make,
    # not this check's: read as no Kueue budget here.
    set="$(limiter_types_of "${WVA_LIMITER:-none}" 2>/dev/null)" || return 1
    limiter_set_has "$set" quota
}

# wva_kueue_gpu_queues echoes, one per line, "<ClusterQueue> <GPUs>" for every
# ClusterQueue in the JSON list $1 that declares a GPU resource, GPUs being its
# nominal GPU quota summed over its flavors.
wva_kueue_gpu_queues() {
    printf '%s' "$1" | jq -r --argjson gpu "$WVA_KUEUE_GPU_RESOURCES" '
        .items[]?
        | [ .spec.resourceGroups[]?.flavors[]?.resources[]?
            | select(.name as $n | $gpu | index($n))
            | (.nominalQuota | tostring | tonumber? // 0) ] as $q
        | select(($q | length) > 0)
        | "\(.metadata.name) \($q | add)"'
}

# wva_report_kueue_queues is the per-namespace Kueue check: the three
# conditions above, for the namespace WVA_WATCH_NS (else WVA_NS). Returns 1
# when one is missing, naming the fix; a read it may not make is "cannot tell
# whether", never a pass. Silent unless the run wants a Kueue budget.
wva_report_kueue_queues() {
    wva_kueue_budget_wanted || return 0
    local ns="${WVA_WATCH_NS:-$WVA_NS}" version cqs lqs labels rows gpu_cqs one_cq
    local usable="" usable_cqs="" bad=0 row lq cq verdict n workloads pods

    if ! kc_capture get crd localqueues.kueue.x-k8s.io -o jsonpath='{.spec.versions[?(@.storage==true)].name}'; then
        if kc_not_found "$KC_ERR"; then
            log_warning "  Kueue budget: this cluster has no Kueue (no LocalQueue CRD), and WVA_QUOTA_KUEUE=true reads the budget from it. Install Kueue, or pick a budget without it (drop WVA_QUOTA_KUEUE)."
            return 1
        fi
        log_warning "  Kueue budget: cannot tell whether Kueue is installed: ${KC_ERR}"
        return 0
    fi
    version="${KC_OUT:-v1beta2}"
    if ! kc_capture get clusterqueues.kueue.x-k8s.io -o json; then
        log_warning "  Kueue budget: cannot tell whether a ClusterQueue holds a GPU quota: ${KC_ERR}"
        return 0
    fi
    cqs="$KC_OUT"
    gpu_cqs="$(wva_kueue_gpu_queues "$cqs")"
    one_cq="<a ClusterQueue with a GPU quota>"
    [ "$(printf '%s\n' "$gpu_cqs" | grep -c .)" = 1 ] && one_cq="${gpu_cqs%% *}"
    if ! kc_capture get localqueues.kueue.x-k8s.io -n "$ns" -o json; then
        log_warning "  Kueue budget: cannot tell whether $ns has a LocalQueue: ${KC_ERR}"
        return 0
    fi
    lqs="$KC_OUT"
    if [ "$(printf '%s' "$lqs" | jq '.items | length')" = 0 ]; then
        log_warning "  Kueue budget: no LocalQueue in $ns, so Kueue grants it nothing and its model servers run outside Kueue."
        if [ -z "$gpu_cqs" ]; then
            log_warning "    No ClusterQueue declares a GPU quota either. Create one with nominalQuota on nvidia.com/gpu, then a LocalQueue here that points at it."
        else
            log_warning "    ClusterQueues with a GPU quota: $(printf '%s\n' "$gpu_cqs" | awk '{printf "%s%s (%s GPUs)", (NR>1?", ":""), $1, $2}')."
        fi
        log_warning "    Create one:  kubectl create -n $ns -f - <<< '{\"apiVersion\":\"kueue.x-k8s.io/${version}\",\"kind\":\"LocalQueue\",\"metadata\":{\"name\":\"serving\"},\"spec\":{\"clusterQueue\":\"${one_cq}\"}}'"
        return 1
    fi
    # The namespace labels, for a ClusterQueue namespaceSelector; null when
    # they cannot be read, which leaves the selector unevaluated.
    labels=null
    if kc_capture get namespace "$ns" -o json; then
        labels="$(printf '%s' "$KC_OUT" | jq -c '.metadata.labels // {}')" || labels=null
    fi
    # One row per LocalQueue: name, ClusterQueue, verdict, GPUs.
    rows="$(printf '%s' "$lqs" | jq -r --argjson cqs "$cqs" --argjson labels "$labels" --argjson gpu "$WVA_KUEUE_GPU_RESOURCES" '
        def gpus: [ .spec.resourceGroups[]?.flavors[]?.resources[]?
                    | select(.name as $n | $gpu | index($n))
                    | (.nominalQuota | tostring | tonumber? // 0) ];
        def expr($l): . as $x | ($l[$x.key]) as $v
            | if $x.operator == "In" then $v != null and (($x.values // []) | index($v)) != null
              elif $x.operator == "NotIn" then $v == null or (($x.values // []) | index($v)) == null
              elif $x.operator == "Exists" then $l | has($x.key)
              elif $x.operator == "DoesNotExist" then ($l | has($x.key)) | not
              else false end;
        def admits($l): .spec.namespaceSelector as $s
            | if $s == null then false
              else ((($s.matchLabels // {}) | to_entries | all(.key as $k | .value == $l[$k]))
                    and (($s.matchExpressions // []) | all(expr($l)))) end;
        ($cqs.items // [] | map({key: .metadata.name, value: .}) | from_entries) as $by
        | .items[]
        | (.spec.clusterQueue // "") as $cq | $by[$cq] as $q
        | [ .metadata.name, $cq,
            ( if $q == null then "missing"
              elif ($q | gpus | length) == 0 then "nogpu"
              elif $labels == null and ($q.spec.namespaceSelector // {}) != {} then "unknown"
              elif ($q | admits($labels // {})) | not then "notadmitted"
              else "ok" end ),
            ( if $q == null then 0 else ($q | gpus | add // 0) end ) ]
        | @tsv')" || { log_warning "  Kueue budget: cannot tell whether $ns has a usable LocalQueue: the LocalQueues did not parse."; return 0; }
    while IFS=$'\t' read -r lq cq verdict n; do
        [ -n "$lq" ] || continue
        case "$verdict" in
            ok)
                usable="${usable:+$usable }$lq"
                case " $usable_cqs " in *" $cq "*) ;; *) usable_cqs="${usable_cqs:+$usable_cqs }$cq" ;; esac
                log_success "  Kueue budget: LocalQueue $lq -> ClusterQueue $cq ($n GPUs)." ;;
            missing) log_warning "  Kueue budget: LocalQueue $lq points at ClusterQueue '${cq}', which does not exist. Point it at ${one_cq}." ;;
            nogpu) log_warning "  Kueue budget: LocalQueue $lq points at ClusterQueue $cq, which declares no GPU resource, so it says nothing about GPUs. Point it at ${one_cq}." ;;
            notadmitted) log_warning "  Kueue budget: LocalQueue $lq points at ClusterQueue $cq, whose namespaceSelector does not admit $ns, so Kueue admits nothing through it. Label $ns to match the selector, or widen it." ;;
            unknown) log_warning "  Kueue budget: cannot tell whether ClusterQueue $cq admits $ns: its namespaceSelector needs the namespace labels, which may not be read here." ;;
        esac
    done <<< "$rows"
    if [ -z "$usable" ]; then
        printf '%s\n' "$rows" | grep -q "	unknown	" && return 0
        log_warning "  Kueue budget: no usable LocalQueue in $ns (one on a ClusterQueue with a GPU quota that admits $ns). Fix one of those above."
        return 1
    fi
    case "$usable_cqs" in
        *" "*) log_warning "  Kueue budget: $ns has LocalQueues on several GPU ClusterQueues ($usable_cqs). A namespace gets one queue's figure, not their sum (multi-queue-namespace). Keep one LocalQueue for the model servers." ;;
    esac

    # Each model server names a usable LocalQueue, or the namespace is
    # selected by a usable LocalQueue named default.
    workloads="$(for kind in deployments leaderworkersets; do
        kubectl get "$kind" -n "$ns" -o json 2>/dev/null || true
    done | jq -rs --argjson markers "$(so_serving_markers_json)" --arg q "$WVA_KUEUE_QUEUE_LABEL" '
        [ .[].items[]? ] | .[]
        | ((.spec.template.metadata.labels // .spec.leaderWorkerTemplate.leaderTemplate.metadata.labels
            // .spec.leaderWorkerTemplate.workerTemplate.metadata.labels) // {}) as $t
        | select($t | to_entries | any((.key + "=" + (.value | tostring)) as $kv | ($markers | index($kv)) != null))
        | [ ((.kind // "Deployment") | ascii_downcase) + "/" + .metadata.name,
            ((.metadata.labels // {})[$q] // $t[$q] // "") ]
        | @tsv' 2>/dev/null || true)"
    local w queue fixq="${usable%% *}"
    while IFS=$'\t' read -r w queue; do
        [ -n "$w" ] || continue
        if [ -z "$queue" ]; then
            case " $usable " in
                *" default "*) continue ;;
            esac
            log_warning "  Kueue budget: $w carries no ${WVA_KUEUE_QUEUE_LABEL} label, so Kueue charges its GPUs to no queue."
            log_warning "    Add it to the pod template (this restarts its pods; or set it in your llm-d values):  kubectl patch -n $ns $w --type=merge -p '{\"spec\":{\"template\":{\"metadata\":{\"labels\":{\"${WVA_KUEUE_QUEUE_LABEL}\":\"${fixq}\"}}}}}'"
            bad=1
        elif ! printf '%s\n' $usable | grep -qxF -- "$queue"; then
            log_warning "  Kueue budget: $w names LocalQueue '$queue', which is not a usable LocalQueue in $ns (usable: ${usable// /, }). Set ${WVA_KUEUE_QUEUE_LABEL}=${fixq} on its pod template."
            bad=1
        fi
    done <<< "$workloads"

    # The running model-server pods: what Kueue actually charges. A pod Kueue
    # manages carries kueue.x-k8s.io/managed=true; one without it holds GPUs no
    # queue is charged for.
    pods="$(kubectl get pods -n "$ns" -o json 2>/dev/null | jq -r --argjson markers "$(so_serving_markers_json)" --arg q "$WVA_KUEUE_QUEUE_LABEL" '
        .items[]?
        | select(.status.phase == "Running" or .status.phase == "Pending")
        | (.metadata.labels // {}) as $l
        | select($l | to_entries | any((.key + "=" + (.value | tostring)) as $kv | ($markers | index($kv)) != null))
        | select(($l["kueue.x-k8s.io/managed"] // "") != "true")
        | [ .metadata.name, (if ($l[$q] // "") == "" then "unlabelled" else "uncovered" end) ]
        | @tsv' 2>/dev/null || true)"
    if [ -n "$pods" ]; then
        log_warning "  Kueue budget: $(printf '%s\n' "$pods" | grep -c .) model-server pod(s) in $ns run outside Kueue (no kueue.x-k8s.io/managed label): $(printf '%s\n' "$pods" | cut -f1 | head -3 | paste -sd, -)."
        if printf '%s\n' "$pods" | grep -q "	unlabelled$"; then
            log_warning "    They started before their model server was on a queue. Label the pod template as above; the rollout replaces them."
        fi
        if printf '%s\n' "$pods" | grep -q "	uncovered$"; then
            log_warning "    They carry ${WVA_KUEUE_QUEUE_LABEL}, so Kueue does not manage pods in $ns: its webhooks or managedJobsNamespaceSelector leave $ns out, or the pod and deployment integrations are off (integrations.frameworks in kueue-manager-config). Cover $ns, then restart the model servers."
        fi
        bad=1
    fi
    [ "$bad" = 0 ] || return 1
    log_success "  Kueue budget: the model servers in $ns are on LocalQueue ${usable// /, }."
    return 0
}

# wva_require_kueue_queues turns that report into a stop, like the two above.
wva_require_kueue_queues() {
    wva_report_kueue_queues && return 0
    log_error "Refusing to continue — fix the Kueue queues above, or SKIP_CHECKS=true (see docs/reference/operations.md#first-line-troubleshooting)."
}

# wva_report_kueue_cluster_budget says, once per run, what a scope: cluster
# Kueue budget adds up to, for the namespaces given as arguments. The cluster
# figure is the GPU quota of EVERY ClusterQueue (internal/kueue/reader.go), not
# only the ones these namespaces use, so a queue for other work (training,
# batch) widens the budget past what the models can be admitted to. Warns,
# naming the finite cap that keeps the budget to the models' queues.
wva_report_kueue_cluster_budget() {
    wva_kueue_budget_wanted || return 0
    [ "${WVA_QUOTA_SCOPE:-namespace}" = cluster ] || return 0
    local gpu_cqs used="" ns cq name n total=0 mine=0 others=""
    kc_capture get clusterqueues.kueue.x-k8s.io -o json || return 0
    gpu_cqs="$(wva_kueue_gpu_queues "$KC_OUT")"
    [ -n "$gpu_cqs" ] || return 0
    for ns in "$@"; do
        for cq in $(kubectl get localqueues.kueue.x-k8s.io -n "$ns" -o jsonpath='{.items[*].spec.clusterQueue}' 2>/dev/null || true); do
            case " $used " in *" $cq "*) ;; *) used="${used:+$used }$cq" ;; esac
        done
    done
    while read -r name n; do
        [ -n "$name" ] || continue
        total=$((total + n))
        case " $used " in
            *" $name "*) mine=$((mine + n)) ;;
            *) others="${others:+$others, }$name ($n)" ;;
        esac
    done <<< "$gpu_cqs"
    if [ -z "$others" ]; then
        log_success "Kueue budget: $total GPUs, the GPU quota of ClusterQueue(s) ${used// /, }, shared by every managed namespace; WVA_QUOTAS can only lower it."
        return 0
    fi
    # A finite WVA_QUOTAS cap keeps the budget to it whatever Kueue sums.
    case " $(printf '%s' "${WVA_QUOTAS:-}" | tr ',' ' ') " in
        *"=-1 "*) ;;
        *)
            log_info "Kueue budget: the GPU quota of every ClusterQueue, $total GPUs, capped by WVA_QUOTAS (${WVA_QUOTAS:-}). The managed namespaces use ClusterQueue(s) ${used// /, } ($mine GPUs)."
            return 0 ;;
    esac
    log_warning "Kueue budget: the shared budget is the GPU quota of EVERY ClusterQueue, $total GPUs, and ClusterQueue(s) $others serve none of the managed namespaces."
    log_warning "  To keep the budget to the models' queues, cap it at their $mine GPUs: WVA_QUOTAS='<accelerator>=${mine}' instead of -1."
    return 0
}

# wva_inferencepool_namespaces echoes every namespace that has an InferencePool,
# in either API group (the same two wva_epp_services reads).
wva_inferencepool_namespaces() {
    local kind
    for kind in inferencepools.inference.networking.k8s.io inferencepools.inference.networking.x-k8s.io; do
        kubectl get "$kind" -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null || true
    done | grep -v '^$' | sort -u || true
}

# wva_ns_check_reasons echoes, comma-separated, what the per-namespace checks in
# $1 (their output) found missing. Read from the lines the checks print, so the
# summary cannot name a failure the details above it do not show.
wva_ns_check_reasons() {
    local out="$1" reasons=()
    case "$out" in *"Monitoring not enabled for your llm-d servers"*) reasons+=("model servers not scraped") ;; esac
    case "$out" in *"Router monitoring not enabled"*) reasons+=("EPP not scraped") ;; esac
    case "$out" in *"could not read"*"plugins config"*) reasons+=("EPP config unreadable") ;; esac
    case "$out" in *"Kueue budget: no LocalQueue in"*|*"Kueue budget: no usable LocalQueue"*) reasons+=("no usable Kueue LocalQueue") ;; esac
    case "$out" in *"Kueue charges its GPUs to no queue"*|*"which is not a usable LocalQueue"*|*"run outside Kueue"*) reasons+=("model servers not on a Kueue queue") ;; esac
    case "$out" in *"this cluster has no Kueue"*) reasons+=("no Kueue") ;; esac
    local IFS=,
    printf '%s' "${reasons[*]}" | sed 's/,/, /g'
}

# wva_ns_check_notes echoes, comma-separated, what the checks in $1 found that
# does not fail a namespace: monitors the admin phase will add, and an EPP with
# flow control off. Read from the checks' own lines, like wva_ns_check_reasons.
wva_ns_check_notes() {
    local out="$1" notes=()
    case "$out" in *"Setup adds PodMonitor"*) notes+=("setup adds the model-server PodMonitor") ;; esac
    case "$out" in *"Setup adds ServiceMonitor"*) notes+=("setup adds the EPP ServiceMonitor") ;; esac
    case "$out" in *"EPP flow control: off"*) notes+=("EPP flow control off: no scale from zero") ;; esac
    case "$out" in *"Kueue budget: the model servers in"*) notes+=("on Kueue LocalQueue $(printf '%s\n' "$out" | sed -n 's/.*Kueue budget: the model servers in .* are on LocalQueue \(.*\)\.$/\1/p' | head -1)") ;; esac
    local IFS=,
    printf '%s' "${notes[*]}" | sed 's/,/, /g'
}

# wva_llmd_namespaces echoes every namespace with llm-d model servers or an
# InferencePool: the namespaces a cluster-scoped controller manages.
wva_llmd_namespaces() {
    { wva_namespaces_with_model_servers; wva_inferencepool_namespaces; } | grep -v '^$' | sort -u || true
}

# wva_check_managed_namespaces runs the per-namespace checks of a cluster-scoped
# install against every namespace it would manage, found rather than named: every
# namespace with llm-d model servers or an InferencePool.
#
# The checks are the namespace-scoped install's own (wva_report_modelserver_metrics
# and wva_report_epp_flowcontrol), run once per namespace with WVA_WATCH_NS set to
# it. A cluster-scoped preflight used to run them once, against the controller's
# namespace, which holds no model servers -- so it passed having checked nothing.
#
# Output is grouped per namespace, then one summary line per namespace and an
# overall line. A namespace this identity may not read is "could not check",
# never passed: FORBIDDEN IS NOT ABSENT. Returns 1 when any namespace FAILED;
# "could not check" does not fail, the same rule the checks themselves apply to a
# signal they could not confirm.
wva_check_managed_namespaces() {
    local namespaces ns out rc reasons notes count listed=false
    local passed=0 failed=0 unchecked=0 summary=()

    # A namespace-scoped controller that manages a listed few: those, as named
    # (WVA_WATCH_NS, already settled from `auto` by wva_resolve_watch_namespaces).
    # A listed namespace with no llm-d in it fails: the controller would manage
    # nothing there, which the single-namespace check refuses too.
    if [ "$(wva_install_scope)" = namespace ] && declare -F wva_watch_is_multi >/dev/null && wva_watch_is_multi; then
        listed=true
        namespaces="$(wva_watch_list)"
        count="$(printf '%s\n' "$namespaces" | grep -c .)"
        log_info "Checking the $count namespace(s) this controller manages (WVA_WATCH_NS):"
    else
        if ! kubectl auth can-i list deployments -A >/dev/null 2>&1; then
            log_warning "Namespaces: could not check any. Listing workloads cluster-wide is not permitted for you, so the namespaces with llm-d cannot be found."
            log_warning "  This is not a pass. Check each one yourself:  make check-prereqs NAMESPACE=<namespace>"
            return 0
        fi

        namespaces="$(wva_llmd_namespaces)"
        if [ -z "$namespaces" ]; then
            log_warning "Namespaces: no llm-d model servers and no InferencePool on this cluster, so there is nothing to scale yet."
            return 0
        fi
        count="$(printf '%s\n' "$namespaces" | grep -c .)"
        log_info "Checking the $count namespace(s) with llm-d model servers or an InferencePool:"
    fi

    for ns in $namespaces; do
        echo "" >&2
        log_info "Namespace $ns"
        if ! kubectl auth can-i list pods -n "$ns" >/dev/null 2>&1 \
            || ! kubectl auth can-i get configmaps -n "$ns" >/dev/null 2>&1; then
            log_warning "  Could not check $ns: reading its Pods or ConfigMaps is not permitted for you."
            unchecked=$((unchecked + 1))
            summary+=("could not check|$ns|reading it is not permitted for you")
            continue
        fi
        if [ "$listed" = true ] && [ "$(wva_serving_workload_count "$ns")" = 0 ] \
            && [ -z "$(wva_epp_services "$ns" 2>/dev/null)" ]; then
            log_warning "  No llm-d found in $ns: no model servers and no InferencePool. Install llm-d there, or leave it out of WVA_WATCH_NS."
            failed=$((failed + 1))
            summary+=("failed|$ns|no llm-d model servers")
            continue
        fi
        rc=0
        out="$( (WVA_WATCH_NS="$ns"; wva_report_modelserver_metrics) 2>&1 )" || rc=1
        out="${out}
$( (WVA_WATCH_NS="$ns"; wva_report_epp_flowcontrol) 2>&1 )" || rc=1
        out="${out}
$( (WVA_WATCH_NS="$ns"; wva_report_kueue_queues) 2>&1 )" || rc=1
        printf '%s\n' "$out" | grep -v '^$' >&2 || true
        if [ "$rc" -ne 0 ]; then
            reasons="$(wva_ns_check_reasons "$out")"
            log_warning "  $ns: failed${reasons:+ ($reasons)}. Fix what is listed above."
            failed=$((failed + 1))
            summary+=("failed|$ns|${reasons:-see above}")
        elif printf '%s' "$out" | grep -qE 'cannot tell whether|could not be confirmed|no running pod behind'; then
            log_warning "  $ns: could not check everything. See above."
            unchecked=$((unchecked + 1))
            summary+=("could not check|$ns|some signals could not be confirmed")
        else
            notes="$(wva_ns_check_notes "$out")"
            log_success "  $ns: passed${notes:+ ($notes)}."
            passed=$((passed + 1))
            summary+=("passed|$ns|$notes")
        fi
    done

    echo "" >&2
    log_info "Namespace summary:"
    local row status name why
    for row in "${summary[@]}"; do
        status="${row%%|*}"; row="${row#*|}"
        name="${row%%|*}"; why="${row#*|}"
        case "$status" in
            passed) log_success "  $name: passed${why:+ ($why)}" ;;
            *)      log_warning "  $name: ${status}${why:+ ($why)}" ;;
        esac
    done
    # shellcheck disable=SC2086 # one word per namespace
    wva_report_kueue_cluster_budget $namespaces
    if [ "$failed" -gt 0 ]; then
        log_warning "Namespaces: $passed passed, $failed failed, $unchecked could not be checked."
        return 1
    fi
    if [ "$unchecked" -gt 0 ]; then
        log_warning "Namespaces: $passed passed, $unchecked could not be checked. Those are not passes."
        return 0
    fi
    log_success "Namespaces: all $passed passed."
    return 0
}

# --- Monitors in the llm-d namespaces, added by the admin phase ---------------
#
# A fresh llm-d install scrapes nothing: llm-d's monitoring step is optional,
# and its router chart ships prometheus.enabled=false. WVA cannot size anything
# without the model-server series, nor read the arrival rate without the EPP's.
# The checks used to stop there and print commands for the reader to run in
# every namespace; the admin phase now adds what is missing itself.
#
# What used to argue against this, and how each is met:
#   ownership  only where NOTHING scrapes the target yet (the same conflict
#              tests the checks use), so an existing scrape is never doubled.
#   cleanup    every object carries WVA_LLMD_MONITOR_MARK, and undeploy-wva
#              removes exactly those (wva_remove_llmd_monitors). An object
#              applied by hand from config/modelserver-metrics has no mark and
#              is left alone.
#   drift      the PodMonitor is config/modelserver-metrics, which mirrors
#              llm-d's; the EPP ServiceMonitor scrapes the port and path the
#              router chart's own ServiceMonitor names (http-metrics, /metrics).
#
# DEPLOY_LLMD_MONITORS=false turns it off. The EPP flow-control gate is not
# applied here: it is a line inside the router's Helm values, and an edit made
# beside Helm is reverted by the next upgrade (see wva_report_epp_flowcontrol).

WVA_LLMD_MONITOR_MARK_KEY="llm-d.ai/applied-by"
WVA_LLMD_MONITOR_MARK_VAL="wva-setup-prereqs"
WVA_LLMD_MONITOR_MARK="${WVA_LLMD_MONITOR_MARK_KEY}=${WVA_LLMD_MONITOR_MARK_VAL}"
# Which install added a monitor: the controller's namespace. Set beside the
# mark, so a controller that stops managing a namespace removes only its own
# (wva_prune_unlisted_namespaces) and never another install's.
WVA_LLMD_MONITOR_OWNER_KEY="llm-d.ai/applied-for"
# The EPP ServiceMonitor, and the identity Prometheus scrapes it as. The EPP
# authenticates /metrics (a TokenReview, then a SubjectAccessReview for GET
# /metrics), so the scrape carries a ServiceAccount token, as the router chart's
# own ServiceMonitor does with auth on.
WVA_EPP_SCRAPE_NAME="wva-epp-metrics"
WVA_EPP_SCRAPE_SA="wva-epp-metrics-scraper"
WVA_EPP_SCRAPE_ROLE="wva-epp-metrics-scraper"

# wva_llmd_monitors_wanted reports whether this run adds missing monitors.
# Not on kind-emulator: the e2e path builds its own stack.
wva_llmd_monitors_wanted() {
    [ "${DEPLOY_LLMD_MONITORS:-true}" = true ] && [ "${ENVIRONMENT:-}" != kind-emulator ]
}

# wva_monitor_namespaces echoes the namespaces whose monitors this install
# looks after: every llm-d namespace at cluster scope, else the watched one.
wva_monitor_namespaces() {
    if [ "$(wva_install_scope)" = "cluster" ] && [ -z "${WVA_WATCH_NS:-}" ]; then
        wva_llmd_namespaces
    elif declare -F wva_watch_is_multi >/dev/null && wva_watch_is_multi; then
        wva_watch_list
    else
        echo "${WVA_WATCH_NS:-$WVA_NS}"
    fi
}

# wva_prometheus_selector_labels echoes, as JSON, the matchLabels of the
# selector $1 (podMonitorSelector or serviceMonitorSelector) of the cluster's
# Prometheus, when there is exactly one. A kube-prometheus-stack installed with
# its defaults selects only monitors labelled release=<its release>, and a
# monitor without that label is never scraped, silently. With none, or several,
# '{}': there is no single answer to copy.
wva_prometheus_selector_labels() {
    kubectl get prometheuses.monitoring.coreos.com -A -o json 2>/dev/null \
        | jq -c --arg f "$1" '[.items[]? | (.spec[$f].matchLabels // {})]
                              | if length == 1 then .[0] else {} end' 2>/dev/null \
        || echo '{}'
}

# wva_apply_modelserver_podmonitor applies config/modelserver-metrics in $1,
# marked, and labelled for the cluster's Prometheus.
wva_apply_modelserver_podmonitor() {
    local ns="$1" labels
    labels="$(wva_prometheus_selector_labels podMonitorSelector)"
    [ -n "$labels" ] || labels='{}'
    NS="$ns" LABELS="$labels" MK="$WVA_LLMD_MONITOR_MARK_KEY" MV="$WVA_LLMD_MONITOR_MARK_VAL" \
        OK="$WVA_LLMD_MONITOR_OWNER_KEY" OV="$WVA_NS" \
        yq '.metadata.namespace = strenv(NS)
            | .metadata.labels += (strenv(LABELS) | from_yaml)
            | .metadata.labels[strenv(MK)] = strenv(MV)
            | .metadata.labels[strenv(OK)] = strenv(OV)' \
            "$WVA_PROJECT/config/modelserver-metrics/modelserver-podmonitor.yaml" \
        | kubectl apply -f - >/dev/null
}

# wva_epp_scrape_docs renders, for the EPP behind Service $2 in $1, the
# ServiceMonitor and the identity it scrapes as. Echoes nothing (and returns
# 1) when the Service names no metrics port.
wva_epp_scrape_docs() {
    local ns="$1" svc="$2" svc_json port sel labels
    svc_json="$(kubectl get svc "$svc" -n "$ns" -o json 2>/dev/null)" || return 1
    [ -n "$svc_json" ] || return 1
    # The port the router chart names for metrics; `metrics` in older charts.
    port="$(printf '%s' "$svc_json" | jq -r '[.spec.ports[]? | .name // empty
        | select(. == "http-metrics" or . == "metrics")] | first // empty')"
    [ -n "$port" ] || return 1
    # The Service's name label, as the chart's own ServiceMonitor selects it;
    # every label when it has none. Never the chart version label, which an
    # upgrade changes and would leave the monitor selecting nothing.
    sel="$(printf '%s' "$svc_json" | jq -c '(.metadata.labels // {}) as $l
        | if $l["app.kubernetes.io/name"] then {"app.kubernetes.io/name": $l["app.kubernetes.io/name"]}
          else ($l | with_entries(select(.key != "helm.sh/chart" and .key != "app.kubernetes.io/version"))) end')"
    [ "$sel" != "{}" ] || return 1
    labels="$(wva_prometheus_selector_labels serviceMonitorSelector)"
    [ -n "$labels" ] || labels='{}'
    labels="$(jq -c --arg k "$WVA_LLMD_MONITOR_MARK_KEY" --arg v "$WVA_LLMD_MONITOR_MARK_VAL" \
        --arg ok "$WVA_LLMD_MONITOR_OWNER_KEY" --arg ov "$WVA_NS" \
        '. + {"app.kubernetes.io/name": "workload-variant-autoscaler", "app.kubernetes.io/component": "epp-metrics"} + {($k): $v, ($ok): $ov}' <<< "$labels")"
    cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${WVA_EPP_SCRAPE_SA}
  namespace: ${ns}
  labels: ${labels}
---
apiVersion: v1
kind: Secret
type: kubernetes.io/service-account-token
metadata:
  name: ${WVA_EPP_SCRAPE_SA}-token
  namespace: ${ns}
  labels: ${labels}
  annotations:
    kubernetes.io/service-account.name: ${WVA_EPP_SCRAPE_SA}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${WVA_EPP_SCRAPE_ROLE}
  labels: ${labels}
rules:
- nonResourceURLs: ["/metrics"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${WVA_EPP_SCRAPE_ROLE}-${ns}
  labels: ${labels}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${WVA_EPP_SCRAPE_ROLE}
subjects:
- kind: ServiceAccount
  name: ${WVA_EPP_SCRAPE_SA}
  namespace: ${ns}
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: ${WVA_EPP_SCRAPE_NAME}
  namespace: ${ns}
  labels: ${labels}
spec:
  selector:
    matchLabels: ${sel}
  endpoints:
  - port: ${port}
    path: /metrics
    interval: 10s
    authorization:
      type: Bearer
      credentials:
        name: ${WVA_EPP_SCRAPE_SA}-token
        key: token
EOF
}

# wva_ensure_llmd_monitors adds, in namespace $1, the monitors the checks would
# report missing: the model-server PodMonitor, and a ServiceMonitor for each
# EPP. Only where nothing scrapes the target yet. Appends what it added to
# WVA_LLMD_MONITORS_ADDED. Never fatal: the checks that follow say what is
# still missing.
wva_ensure_llmd_monitors() {
    local ns="$1" servers conflict svc monitors added=()

    if ! kubectl get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1 \
        || ! kubectl get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1; then
        log_warning "  $ns: no Prometheus Operator CRDs, so no monitors can be added. Install a Prometheus Operator, or let this install deploy one."
        return 0
    fi

    servers="$(wva_serving_workload_count "$ns")"
    if [ "${servers:-0}" -gt 0 ] \
        && [ -z "$(wva_modelserver_scrape_conflict "$ns")" ] \
        && ! kubectl get podmonitor -n "$ns" -l app.kubernetes.io/component=modelserver-metrics \
             --no-headers 2>/dev/null | grep -q .; then
        if wva_apply_modelserver_podmonitor "$ns"; then
            added+=("PodMonitor wva-modelserver-metrics")
        else
            log_warning "  $ns: could not add PodMonitor wva-modelserver-metrics."
        fi
    fi

    for svc in $(wva_epp_services "$ns"); do
        monitors="$(wva_epp_scrapers "$ns" "$svc")"
        if [ -n "$monitors" ]; then
            # A monitor this cannot evaluate may already scrape it, and a second
            # scrape of one target doubles every rate read from it.
            printf '%s' "$monitors" | grep -q -v '^unevaluated:' \
                || log_info "  $ns: $(printf '%s' "$monitors" | sed 's/^unevaluated://' | paste -sd, -) may already scrape $svc (it selects by matchExpressions), so no second monitor is added."
            continue
        fi
        if ! wva_epp_scrape_docs "$ns" "$svc" > "${TMPDIR:-/tmp}/wva-epp-scrape.$$.yaml"; then
            log_warning "  $ns: $svc names no metrics port (http-metrics or metrics), so no ServiceMonitor was added for it."
        elif kubectl apply -f "${TMPDIR:-/tmp}/wva-epp-scrape.$$.yaml" >/dev/null; then
            added+=("ServiceMonitor ${WVA_EPP_SCRAPE_NAME} for $svc")
        else
            log_warning "  $ns: could not add ServiceMonitor ${WVA_EPP_SCRAPE_NAME} for $svc."
        fi
        rm -f "${TMPDIR:-/tmp}/wva-epp-scrape.$$.yaml"
    done

    if [ "${#added[@]}" -gt 0 ]; then
        local list
        list="$(printf '%s\n' "${added[@]}" | paste -sd, - | sed 's/,/, /g')"
        log_success "  $ns: added $list"
        WVA_LLMD_MONITORS_ADDED+=("$ns: $list")
    fi
    return 0
}

# wva_apply_llmd_monitors runs wva_ensure_llmd_monitors in every namespace this
# install looks after, when wanted. Called by the admin phase, after the
# monitoring stack (whose CRDs the monitors need).
wva_apply_llmd_monitors() {
    wva_llmd_monitors_wanted || return 0
    local ns
    WVA_LLMD_MONITORS_ADDED=()
    log_info "Adding the monitors llm-d did not install (DEPLOY_LLMD_MONITORS=false skips this):"
    for ns in $(wva_monitor_namespaces); do
        [ -n "$ns" ] || continue
        wva_ensure_llmd_monitors "$ns"
    done
    [ "${#WVA_LLMD_MONITORS_ADDED[@]}" -gt 0 ] || log_info "  None needed: every model server and EPP is already scraped."
}

# wva_remove_llmd_monitors deletes what wva_apply_llmd_monitors added, by its
# mark: in every namespace at cluster scope, else in the watched one. The
# shared ClusterRole goes only with a cluster-scoped install, the last user.
wva_remove_llmd_monitors() {
    local where=(-A) ns
    if declare -F wva_watch_is_multi >/dev/null && wva_watch_is_multi; then
        # One controller, several managed namespaces: the same removal in each.
        for ns in $(wva_watch_words); do
            (WVA_WATCH_NS="$ns"; wva_remove_llmd_monitors) || true
        done
        return 0
    fi
    if [ "$(wva_install_scope)" != "cluster" ] || [ -n "${WVA_WATCH_NS:-}" ]; then
        ns="${WVA_WATCH_NS:-$WVA_NS}"
        where=(-n "$ns")
        kubectl delete clusterrolebinding "${WVA_EPP_SCRAPE_ROLE}-${ns}" --ignore-not-found >/dev/null 2>&1 || true
        # The shared ClusterRole goes with its last binding: one still bound
        # (another namespace's EPP monitor) keeps it. A list that cannot be read
        # keeps it too.
        local bound
        if bound="$(kubectl get clusterrolebindings -o jsonpath='{range .items[*]}{.roleRef.name}{"\n"}{end}' 2>/dev/null)" \
            && ! printf '%s\n' "$bound" | grep -qxF -- "$WVA_EPP_SCRAPE_ROLE"; then
            # Only our own: the ClusterRole setup labelled, never one of that name it did not write.
            if kubectl get clusterroles -l "$WVA_LLMD_MONITOR_MARK" -o name 2>/dev/null \
                | grep -qxF -- "clusterrole.rbac.authorization.k8s.io/$WVA_EPP_SCRAPE_ROLE"; then
                kubectl delete clusterrole "$WVA_EPP_SCRAPE_ROLE" --ignore-not-found >/dev/null 2>&1 || true
            fi
        fi
    else
        kubectl delete clusterrolebindings,clusterroles -l "$WVA_LLMD_MONITOR_MARK" --ignore-not-found >/dev/null 2>&1 || true
    fi
    kubectl delete podmonitors.monitoring.coreos.com,servicemonitors.monitoring.coreos.com,secrets,serviceaccounts \
        "${where[@]}" -l "$WVA_LLMD_MONITOR_MARK" --ignore-not-found >/dev/null 2>&1 || true
    if [ -n "${ns:-}" ]; then
        log_info "Removed the monitors setup added in $ns (label $WVA_LLMD_MONITOR_MARK)."
    else
        log_info "Removed the monitors setup added in the llm-d namespaces (label $WVA_LLMD_MONITOR_MARK)."
    fi
}
