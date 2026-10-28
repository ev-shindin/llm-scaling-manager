#!/usr/bin/env bash
#
# Shared WVA-specific deployment helpers.
# Requires vars: WVA_NS, NAMESPACE, MONITORING_NAMESPACE, WVA_PROJECT,
# chart/image values, env mode lists.
# Requires funcs: log_info/log_warning/log_success/log_error, containsElement().
#

set_wva_logging_level() {
    log_info "Setting WVA logging level..."

    # Set logging level based on environment
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        WVA_LOG_LEVEL="debug"
        log_info "Development environment - using debug logging"
    else
        WVA_LOG_LEVEL="info"
        log_info "Production environment - using info logging"
    fi

    export WVA_LOG_LEVEL
    log_success "WVA logging level set to: $WVA_LOG_LEVEL"
    echo ""
}

# wva_watch_rbac_names echoes `role binding` — the names this install owns in the
# namespace it manages. Suffixed by the controller's namespace, so two admin-owned
# controllers pointed at one tenant do not overwrite or delete each other's.
wva_watch_rbac_names() {
    local suffix
    suffix="$(wva_ns_suffix "$WVA_NS")"
    printf 'wva-manager-role-%s wva-manager-rolebinding-%s' "$suffix" "$suffix"
}

# wva_apply_watch_namespace_rbac grants the controller its permissions in the
# namespace it MANAGES, when that is not the namespace it RUNS IN.
#
# The overlay puts the Role and RoleBinding in the controller's own namespace,
# which is right when those are the same namespace and useless when they are not:
# a controller in an admin-owned namespace watching a tenant's could not read a
# ConfigMap there, and crash-looped on its first bootstrap read. So the one
# configuration that puts a controller beyond the reach of the tenant it bounds
# was the one configuration that could not run.
#
# It is applied in the PREREQS phase because it is an admin's act by definition:
# writing RBAC into someone else's namespace is precisely what the tenant cannot
# do, and what they must not be able to undo.
#
# The Role is a copy of the rendered one rather than a second definition, so it
# cannot drift from the permissions the controller actually needs.
wva_apply_watch_namespace_rbac() {
    local tmp_overlay="$1" role binding rendered ns prune_after=""
    [ -n "${WVA_WATCH_NS:-}" ] || return 0
    if wva_watch_is_multi; then
        # The same Role and RoleBinding in each managed namespace, and nowhere
        # else: least privilege is the list, not the cluster.
        for ns in $(wva_watch_words); do
            (WVA_WATCH_NS="$ns" WVA_WATCH_GRANT_INNER=1; wva_apply_watch_namespace_rbac "$tmp_overlay") || exit 1
        done
        wva_prune_unlisted_namespaces
        return 0
    fi
    [ "$WVA_WATCH_NS" != "$WVA_NS" ] || return 0
    # A list shrunk to one namespace: the others it set up go too.
    [ -n "${WVA_WATCH_GRANT_INNER:-}" ] || prune_after=1

    read -r role binding <<< "$(wva_watch_rbac_names)"
    rendered=$(kubectl kustomize "$tmp_overlay" 2>/dev/null         | WVA_TARGET_NS="$WVA_WATCH_NS" WVA_ROLE_NAME="$role" \
            WVA_GRANT_KEY="$WVA_WATCH_GRANT_KEY" WVA_GRANT_VAL="$(wva_ns_suffix "$WVA_NS")" yq '
            select(.kind == "Role" and (.metadata.name | test("manager-role$")))
            | .metadata.name = strenv(WVA_ROLE_NAME)
            | .metadata.namespace = strenv(WVA_TARGET_NS)
            | .metadata.labels[strenv(WVA_GRANT_KEY)] = strenv(WVA_GRANT_VAL)') || true
    if [ -z "$rendered" ]; then
        log_error "Could not find the manager Role in the rendered overlay, so the controller would have no permissions in $WVA_WATCH_NS. Refusing to install a controller that cannot read the namespace it manages."
    fi

    log_info "Granting the controller its permissions in $WVA_WATCH_NS (it runs in $WVA_NS)"
    printf '%s
' "$rendered" | kubectl apply -f - > /dev/null
    kubectl apply -f - > /dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${binding}
  namespace: ${WVA_WATCH_NS}
  labels:
    app.kubernetes.io/managed-by: llm-scaling-manager
    ${WVA_WATCH_GRANT_KEY}: "$(wva_ns_suffix "$WVA_NS")"
subjects:
- kind: ServiceAccount
  name: wva-controller-manager
  namespace: ${WVA_NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${role}
EOF
    log_success "  Role/$role and RoleBinding/$binding in $WVA_WATCH_NS"
    [ -z "${prune_after:-}" ] || wva_prune_unlisted_namespaces
}

# WVA_WATCH_GRANT_KEY labels the manager Role and RoleBinding this install
# writes in a namespace it manages from outside; the value is the suffix of the
# controller's namespace (wva_ns_suffix), so each install finds only its own.
WVA_WATCH_GRANT_KEY="llm-d.ai/watch-grant-for"

# wva_granted_namespaces echoes, one per line, every namespace holding a manager
# RoleBinding this install labelled as its own. Fails when RoleBindings cannot
# be listed cluster-wide: an unreadable list is not an empty one.
wva_granted_namespaces() {
    local out
    out="$(kubectl get rolebindings -A -l "${WVA_WATCH_GRANT_KEY}=$(wva_ns_suffix "$WVA_NS")" \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>&1)" || { printf '%s\n' "$out" >&2; return 1; }
    printf '%s\n' "$out" | grep -v '^$' | sort -u || true
}

# wva_revoke_namespace removes from namespace $1 what this install added there:
# its labelled manager Role and RoleBinding, and the monitors setup added for
# it (the monitor mark plus this install's owner label). Nothing else.
wva_revoke_namespace() {
    local ns="$1" sel
    sel="${WVA_WATCH_GRANT_KEY}=$(wva_ns_suffix "$WVA_NS")"
    kubectl delete rolebindings,roles -n "$ns" -l "$sel" --ignore-not-found >/dev/null 2>&1 || true
    if [ -n "${WVA_LLMD_MONITOR_MARK:-}" ]; then
        kubectl delete podmonitors.monitoring.coreos.com,servicemonitors.monitoring.coreos.com,secrets,serviceaccounts \
            -n "$ns" -l "${WVA_LLMD_MONITOR_MARK},${WVA_LLMD_MONITOR_OWNER_KEY}=${WVA_NS}" --ignore-not-found >/dev/null 2>&1 || true
        kubectl delete clusterrolebinding "${WVA_EPP_SCRAPE_ROLE:-wva-epp-metrics-scraper}-${ns}" --ignore-not-found >/dev/null 2>&1 || true
    fi
}

# wva_prune_unlisted_namespaces revokes this install's grants and monitors in
# every namespace it set up before and no longer lists (WVA_WATCH_NS). Shrinking
# the list must shrink the controller's rights: a namespace taken out of it is
# one the controller may no longer touch.
wva_prune_unlisted_namespaces() {
    local granted ns
    if ! granted="$(wva_granted_namespaces 2>/dev/null)"; then
        log_warning "Could not list RoleBindings cluster-wide, so namespaces dropped from WVA_WATCH_NS may keep this controller's grants. Find them: kubectl get rolebindings -A -l ${WVA_WATCH_GRANT_KEY}=$(wva_ns_suffix "$WVA_NS")"
        return 0
    fi
    for ns in $granted; do
        wva_watch_list | grep -qxF -- "$ns" && continue
        wva_revoke_namespace "$ns"
        log_success "  Removed this controller's Role, RoleBinding and monitors in $ns, which WVA_WATCH_NS no longer lists"
    done
}

# wva_pod_list_names echoes "<clusterrole> <clusterrolebinding>": the cluster-wide
# Pod list a namespace-scoped controller in WVA_NS gets beside gpu-inventory.
wva_pod_list_names() {
    local suffix
    suffix="$(wva_ns_suffix "$WVA_NS")"
    printf 'wva-pod-list-role-%s wva-pod-list-rolebinding-%s\n' "$suffix" "$suffix"
}

# wva_apply_pod_list_grant gives a namespace-scoped controller list on Pods in
# every namespace when its limiters hold gpu-inventory (option B: a quota for
# the budget, the cluster's GPUs for placement). The inventory counts the GPUs
# every Pod on a node holds; through its Role alone it sees only its own
# namespaces' Pods, and another tenant's GPUs on the same node read as free.
# Read-only: list, nothing else, and on Pods only. Admin phase only.
wva_apply_pod_list_grant() {
    [ "$(wva_install_scope)" = namespace ] || return 0
    [ -n "${WVA_LIMITER:-}" ] || return 0
    limiter_set_has "$(wva_limiter_set)" physical || return 0
    local role binding
    read -r role binding <<< "$(wva_pod_list_names)"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${role}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/managed-by: llm-scaling-manager
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${binding}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/managed-by: llm-scaling-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${role}
subjects:
- kind: ServiceAccount
  name: wva-controller-manager
  namespace: ${WVA_NS}
EOF
    log_success "  ClusterRole/$role: list Pods in every namespace, so gpu-inventory sees the GPUs other namespaces hold on the same nodes"
}

# wva_revoke_pod_list_grant removes what wva_apply_pod_list_grant wrote.
wva_revoke_pod_list_grant() {
    local role binding
    read -r role binding <<< "$(wva_pod_list_names)"
    kubectl delete clusterrolebinding "$binding" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete clusterrole "$role" --ignore-not-found >/dev/null 2>&1 || true
}

# wva_resolve_prometheus_url settles which Prometheus this install points at.
#
# Idempotent, and separated from the writing of it because the ANSWER is needed
# before the overlay is applied while the WRITE has to happen after — see
# wva_render_manager_config_patch.
wva_resolve_prometheus_url() {
    [ -z "${PROMETHEUS_URL_EXPLICIT:-}" ] || return 0
    [ -z "${WVA_PROMETHEUS_URL_RESOLVED:-}" ] || return 0
    local detected
    detected="$(wva_detect_prometheus_url)"
    if [ -n "$detected" ] && [ "$detected" != "${PROMETHEUS_URL:-}" ]; then
        log_info "Using the Prometheus already on this cluster: $detected"
        log_info "  (the default names the one this installer would deploy; pass PROMETHEUS_URL=<url> to override)"
        PROMETHEUS_URL="$detected"
    elif [ -z "$detected" ] && [ "${WVA_LIVE_PROM_TLS:-}" = "read" ] && [ -n "${WVA_LIVE_PROM_URL:-}" ] \
        && [ "${WVA_LIVE_PROM_URL%/}" != "${PROMETHEUS_URL%/}" ]; then
        # Nothing found is often nothing this identity may list: a namespace
        # admin cannot list Prometheus resources cluster-wide. The address the
        # controller runs with -- settled by an earlier install, usually the
        # admin's -- is a better answer than the shipped default, which names
        # a Prometheus this installer would deploy.
        log_info "Keeping the Prometheus the controller runs with: $WVA_LIVE_PROM_URL"
        log_info "  (no Prometheus found that you may list; pass PROMETHEUS_URL=<url> to change it)"
        PROMETHEUS_URL="$WVA_LIVE_PROM_URL"
    fi
    WVA_PROMETHEUS_URL_RESOLVED=1
}

# wva_render_manager_config_patch puts this install's Prometheus URL into the
# ConfigMap the FIRST apply creates.
#
# Without it the controller starts on the shipped default -- a kube-prometheus-
# stack address in a namespace most clusters do not have -- exits 2 on it
# ("CRITICAL: Failed to connect to Prometheus"), and is only then corrected by
# the patch below, recovering on the restart. The install converges, so this read
# as cosmetic; it is not. The installer prints "You do not need to pass
# PROMETHEUS_URL" and the guide's next step is `kubectl logs -f`, which shows a
# stack trace and RESTARTS=1 on a healthy install. Measured on a multi-node H200 cluster.
#
# It also closes a window on every UPGRADE: `kubectl apply -k` re-applies the
# shipped ConfigMap, so the default was briefly live again each time, and a
# controller that happened to restart in that window died on it.
#
# The content is generated from the shipped file, never a second copy of it: yq
# reads data["config.yaml"] and assigns into it, so comments and every other
# setting survive, and a new key in the shipped ConfigMap needs no change here.
#
# Because it replaces the whole config.yaml, every key an operator set on the
# live one is gone after the apply unless it is written here. The two TLS keys
# are: a re-run that dropped PROMETHEUS_CA_CERT_PATH left a controller that had
# verified against a mounted CA verifying against the public roots only, and
# it could no longer connect. See wva_prometheus_ca_path.
wva_render_manager_config_patch() {
    local tmp_overlay="$1" src rendered
    src="$WVA_PROJECT/config/base/manager/manager-configmap.yaml"
    [ -f "$src" ] || return 0
    [ -n "${PROMETHEUS_URL:-}" ] || return 0

    local skip ca
    skip="$(wva_prometheus_skip_verify)"
    ca="$(wva_prometheus_ca_path)"
    rendered="$(yq '.data."config.yaml"' "$src" \
        | WVA_URL="$PROMETHEUS_URL" yq '.PROMETHEUS_BASE_URL = strenv(WVA_URL)' \
        | if [ -n "$skip" ]; then WVA_SKIP="$skip" yq '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = strenv(WVA_SKIP)'; else cat; fi \
        | if [ -n "$ca" ]; then WVA_CA="$ca" yq '.PROMETHEUS_CA_CERT_PATH = strenv(WVA_CA)'; else cat; fi)" || return 0
    [ -n "$rendered" ] || return 0

    {
        printf 'apiVersion: v1\n'
        printf 'kind: ConfigMap\n'
        printf 'metadata:\n'
        printf '  name: wva-manager-config\n'
        printf 'data:\n'
        printf '  config.yaml: |\n'
        printf '%s\n' "$rendered" | sed 's/^/    /'
    } > "$tmp_overlay/manager-config-patch.yaml"

    cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- path: manager-config-patch.yaml
  target:
    kind: ConfigMap
    name: wva-manager-config
EOF
}

# wva_resolve_scaler_keda_namespace sets WVA_SCALER_KEDA_NS to the namespace the
# external-scaler NetworkPolicy admits to :9090 -- the one KEDA's operator runs in
# -- when that namespace is KNOWN, and to empty when it would only be a guess.
#
# A wrong answer either fails CLOSED (KEDA refused, its ScaledObjects go
# not-Ready and nothing scales) or OPEN (a tenant namespace reaches :9090 and
# can rewrite any workload's trigger metadata). So each source is one a tenant
# cannot plant, and an unreadable source is "unknown", never "not there":
#
#   1. KEDA_NAMESPACE, when the caller set it.
#   2. The namespace of the Service behind the v1beta1.external.metrics.k8s.io
#      APIService, when that Service is KEDA's. Cluster-scoped and admin-written.
#   3. KEDA_NAMESPACE again, when KEDA_HELM_INSTALL=true AND that APIService is
#      NotFound: this install is the one putting KEDA there (deploy_keda runs
#      after the controller, so it cannot be found yet). With the APIService
#      present, KEDA already runs somewhere and deploy_keda will not install a
#      second one, so KEDA_NAMESPACE says nothing about where it is.
#   4. The namespace an EXISTING wva-external-scaler NetworkPolicy in WVA_NS
#      already admits. An admin who corrected the policy by hand must not have
#      the correction undone by the next upgrade.
#
# Nothing else. A Deployment named keda-operator is not a source: any tenant can
# create one, and it then either pointed the policy at their namespace (opening
# :9090 to them) or made the answer "ambiguous" and stopped the install. Nor is
# the platform default: on a cluster whose KEDA runs in `keda`, a policy written
# for keda-system refuses KEDA and nothing scales. When no source answers, no
# policy is written (wva_append_scaler_networkpolicy_patch) and the caller is
# told, loudly, how to close the port.
#
# Sets variables rather than echoing: log_error is an exit, and an exit inside
# `$( )` would end only the subshell. WVA_SCALER_KEDA_UNREAD names the sources
# that could not be read, for the warning.
wva_resolve_scaler_keda_namespace() {
    WVA_SCALER_KEDA_NS=""
    WVA_SCALER_KEDA_UNREAD=""
    # What is known about the policy ALREADY in WVA_NS, once it has been read:
    # absent | unknown (could not be read) | unrecognised (present, but its :9090
    # rule admits no single namespace). Empty when nobody looked.
    WVA_SCALER_NP_STATE=""
    if [ "${KEDA_NAMESPACE_EXPLICIT:-false}" = "true" ]; then
        WVA_SCALER_KEDA_NS="$KEDA_NAMESPACE"
        log_info "External scaler NetworkPolicy admits KEDA from $WVA_SCALER_KEDA_NS (KEDA_NAMESPACE)"
        return 0
    fi
    local found rc

    rc=0
    found="$(wva_keda_apiservice_ns)" || rc=$?
    case "$rc" in
        0)
            WVA_SCALER_KEDA_NS="$found"
            log_info "External scaler NetworkPolicy admits KEDA from $WVA_SCALER_KEDA_NS (the namespace serving the external-metrics APIService)"
            return 0
            ;;
        1)
            if [ "${KEDA_HELM_INSTALL:-false}" = "true" ]; then
                WVA_SCALER_KEDA_NS="$KEDA_NAMESPACE"
                log_info "External scaler NetworkPolicy admits KEDA from $WVA_SCALER_KEDA_NS (KEDA_HELM_INSTALL=true installs KEDA there; no external-metrics APIService exists yet)"
                return 0
            fi
            ;;
        2) WVA_SCALER_KEDA_UNREAD="$WVA_SCALER_KEDA_UNREAD the external-metrics APIService;" ;;
        3) log_info "The external-metrics APIService is served by $found, which is not KEDA; looking further." ;;
    esac

    rc=0
    found="$(wva_scaler_np_keda_ns "$WVA_NS")" || rc=$?
    case "$rc" in
        0)
            WVA_SCALER_KEDA_NS="$found"
            log_info "External scaler NetworkPolicy admits KEDA from $WVA_SCALER_KEDA_NS (kept from the policy already in $WVA_NS; set KEDA_NAMESPACE to change it)"
            return 0
            ;;
        1) WVA_SCALER_NP_STATE=absent ;;
        2)
            WVA_SCALER_NP_STATE=unknown
            WVA_SCALER_KEDA_UNREAD="$WVA_SCALER_KEDA_UNREAD NetworkPolicy wva-external-scaler in $WVA_NS;"
            ;;
        4) WVA_SCALER_NP_STATE=unrecognised ;;
    esac
    return 0
}

# wva_warn_scaler_port_open says, loudly, that the external scaler's :9090 has
# no NetworkPolicy, and how to close it. $1 is why.
wva_warn_scaler_port_open() {
    local why="$1" phase_cmd
    phase_cmd="$(wva_make_cmd setup-prereqs 'KEDA_NAMESPACE=<namespace KEDA runs in>')"
    log_warning "=================================================================="
    log_warning "NO NetworkPolicy for the external scaler: :9090 is reachable by ANY Pod"
    log_warning "  that can reach the controller. Any of them can rewrite another"
    log_warning "  workload's trigger metadata and read every scaling decision."
    log_warning "  Why: $why"
    if [ -n "${WVA_SCALER_KEDA_UNREAD:-}" ]; then
        log_warning "  Not readable with your permissions, so unknown rather than absent:${WVA_SCALER_KEDA_UNREAD%;}."
    fi
    log_warning "  To close it, as a cluster admin:"
    log_warning "    $phase_cmd"
    log_warning "  or serve the scaler over mutual TLS (WVA_SCALER_TLS=true; see"
    log_warning "  docs/reference/configuration.md)."
    log_warning "=================================================================="
}

# wva_warn_scaler_policy_unknown says the external-scaler NetworkPolicy in
# WVA_NS was left as it is and that its state is not known here -- never that
# there is none. $1 is why.
#
# Used when a policy may well exist: it could not be read, it is there in a
# shape this install does not recognise, or this phase does not manage it.
# "NO NetworkPolicy ... :9090 is reachable by ANY Pod" was printed in all three
# cases, which told an admin with a correct policy that the port was open, and
# pointed them at a fix for a problem that may not exist.
wva_warn_scaler_policy_unknown() {
    local why="$1"
    log_warning "NetworkPolicy wva-external-scaler in $WVA_NS: left as it is, state UNKNOWN. $why"
    log_warning "  If it is absent, the external scaler (:9090) is reachable by any Pod that can reach the controller."
    log_warning "  Check:  kubectl get networkpolicy wva-external-scaler -n $WVA_NS -o yaml"
    log_warning "  To (re)write it, a cluster admin runs:  $(wva_make_cmd setup-prereqs 'KEDA_NAMESPACE=<namespace KEDA runs in>')"
}

# wva_append_scaler_networkpolicy_patch points the external-scaler NetworkPolicy
# at KEDA's namespace, or leaves it out of the render. It appends to the
# `patches:` list of the throw-away overlay's kustomization.yaml, which must
# already exist.
#
# The policy is an ADMIN-phase object (WVA_PREREQ_NAMED): only the phase that
# can read where KEDA runs writes it. The controller phase neither applies it
# nor resolves anything for it, because a tenant cannot see KEDA and every
# answer it reached was a guess. Left out of the render, rather than written
# from a guess, when no trusted source says where KEDA is
# (wva_resolve_scaler_keda_namespace); WVA_SCALER_NETWORKPOLICY=false leaves it
# out on purpose. WVA_SCALER_NP_APPLIED records which, for the summary.
#
# The `test` op makes a change to the shipped rule's shape fail the build rather
# than rewrite whatever now sits at that path.
wva_append_scaler_networkpolicy_patch() {
    local kustomization="$1"
    WVA_SCALER_NP_APPLIED=false
    WVA_SCALER_NP_STATE=""
    case "${WVA_SCALER_NETWORKPOLICY:-true}" in
        false)
            # An apply never prunes, so an upgrade that opts out has to remove
            # the policy an earlier install left, or opting out does nothing.
            # Only the phases that own the policy remove it -- and only they
            # may say the port is open: the controller phase leaves the admin's
            # policy alone, so there it is UNKNOWN whether :9090 is restricted.
            case "${WVA_APPLY_SCOPE:-all}" in
                all|prereqs)
                    log_warning "WVA_SCALER_NETWORKPOLICY=false: the external scaler (:9090) is reachable from every Pod that can reach the controller. Any of them can rewrite another workload's trigger metadata and read every scaling decision."
                    kubectl delete networkpolicy wva-external-scaler -n "$WVA_NS" --ignore-not-found >/dev/null 2>&1 || true ;;
                *)
                    wva_warn_scaler_policy_unknown "WVA_SCALER_NETWORKPOLICY=false does not reach it from this controller-only phase: the policy is the cluster admin's, and this phase neither writes nor removes it."
                    log_warning "  To remove it, a cluster admin runs:  $(wva_make_cmd setup-prereqs WVA_SCALER_NETWORKPOLICY=false)" ;;
            esac
            wva_append_scaler_networkpolicy_delete "$kustomization"
            return 0
            ;;
        true) ;;
        *) log_error "WVA_SCALER_NETWORKPOLICY must be true or false (got '${WVA_SCALER_NETWORKPOLICY}')" ;;
    esac
    # The controller phase filters the policy out of what it applies; nothing to
    # decide, and nothing it could decide correctly.
    [ "${WVA_APPLY_SCOPE:-all}" != "controller" ] || return 0
    wva_resolve_scaler_keda_namespace
    if [ -z "$WVA_SCALER_KEDA_NS" ]; then
        # Left out of the render either way, so an existing policy is not
        # touched. Only an ABSENT one means the port is open.
        case "$WVA_SCALER_NP_STATE" in
            unknown)
                wva_warn_scaler_policy_unknown "It could not be read with your permissions, and no other source this install trusts says where KEDA runs, so it is neither written nor removed.${WVA_SCALER_KEDA_UNREAD:+ Not readable:${WVA_SCALER_KEDA_UNREAD%;}.}" ;;
            unrecognised)
                wva_warn_scaler_policy_unknown "It exists, but its :9090 rule admits no single namespace, so this install cannot tell where it means KEDA to be; it is neither rewritten nor removed." ;;
            *)
                wva_warn_scaler_port_open "no source this install trusts says where KEDA runs (KEDA_NAMESPACE unset, no KEDA-served external-metrics APIService, no existing policy to keep), and a policy written from a guess refuses KEDA wherever the guess is wrong." ;;
        esac
        wva_append_scaler_networkpolicy_delete "$kustomization"
        return 0
    fi
    WVA_SCALER_NP_APPLIED=true
    cat >> "$kustomization" <<PATCH
- patch: |-
    - op: test
      path: /spec/ingress/1/ports/0/port
      value: 9090
    - op: replace
      path: /spec/ingress/1/from/0/namespaceSelector/matchLabels/kubernetes.io~1metadata.name
      value: "${WVA_SCALER_KEDA_NS}"
  target:
    group: networking.k8s.io
    kind: NetworkPolicy
    name: wva-external-scaler
PATCH
}

# wva_append_scaler_networkpolicy_delete removes the external-scaler
# NetworkPolicy from the render of kustomization $1.
wva_append_scaler_networkpolicy_delete() {
    cat >> "$1" <<PATCH
- patch: |-
    \$patch: delete
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    metadata:
      name: wva-external-scaler
  target:
    group: networking.k8s.io
    kind: NetworkPolicy
    name: wva-external-scaler
PATCH
}

# wva_image_pull_policy echoes the pull policy for an image reference: $1 is the
# tag, $2 the digest (may be empty).
#
# IfNotPresent only for a reference that cannot move -- a digest, or a semver
# release tag (v1.2.3). Everything else is Always: :main and :latest are rebuilt
# on every push, and a tag this cannot classify (ref-<sha> in the OpenShift e2e,
# re-pushed after a merge) is not known to be immutable. IfNotPresent on a moving
# tag is how a node kept a 6-day-old :main through a restart.
wva_image_pull_policy() {
    local tag="$1" digest="${2:-}"
    if [ -n "$digest" ]; then
        echo IfNotPresent
        return 0
    fi
    if [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
        echo IfNotPresent
        return 0
    fi
    echo Always
}

# wva_warn_floating_image warns when the controller image is a moving tag.
#
# :main is the default because this tree's manifests need a build of this tree
# (see WVA_IMAGE_TAG in install.sh), and it is right for development. It is
# wrong for anything that must be reproduced: the same install command deploys a
# different binary after every push to main, and nothing records which one ran.
# So say so, loudly, and say what to pass instead. A warning rather than a
# refusal: developers and the kind e2e deploy :main on purpose.
wva_warn_floating_image() {
    local repo="$1" tag="$2" digest="${3:-}"
    [ -z "$digest" ] || return 0
    case "$tag" in
        main|latest|dev|nightly|"") ;;
        *) return 0 ;;
    esac
    log_warning "=================================================================="
    log_warning "The controller image is a FLOATING tag: ${repo}:${tag:-<none>}"
    log_warning "  It names whatever was pushed last, so this install is not reproducible:"
    log_warning "  the same command deploys a different binary after the next push."
    log_warning "  For anything beyond development, pin it:"
    log_warning "    IMG=${repo}@sha256:<digest>   (exactly one build)"
    log_warning "    IMG=${repo}:vX.Y.Z            (a release tag)"
    log_warning "=================================================================="
}

# wva_prometheus_skip_verify echoes the PROMETHEUS_TLS_INSECURE_SKIP_VERIFY value
# this install writes into the manager config, or nothing to leave the shipped
# one ("false", verify) in place.
#
# The shipped config verifies. The installer turns verification OFF in one case
# only: the kube-prometheus-stack it deploys itself (DEPLOY_PROMETHEUS=true, and
# a URL in MONITORING_NAMESPACE -- the same rule wva_prometheus_scraper_sa uses).
# That stack's certificate is self-signed on the workstation and nothing mounts
# it into the controller, so there is no CA to verify against;
# wva_warn_prometheus_tls says so at the end of the install.
#
# Any other https:// Prometheus keeps verification on. The controller sends it
# a bearer token, and a Prometheus nobody named is no place to send one
# unverified. If its certificate is from a private CA, the controller cannot
# connect until the CA is mounted, and wva_warn_prometheus_tls says how.
#
# One exception, for upgrades: a controller that runs unverified NOW, with no
# CA, against the SAME Prometheus, stays unverified
# (wva_prometheus_tls_kept_from_live). Earlier releases shipped "true" for
# every Prometheus. Turning verification on with no CA to verify against stops
# the new pod from connecting -- "CRITICAL: Failed to connect to Prometheus" --
# while the install still reports success. wva_warn_prometheus_tls_upgrade says
# it was kept, and how to turn it on. A different Prometheus is a new decision:
# what was true of the old one says nothing about the new one.
#
# An EXPLICIT PROMETHEUS_CA_CERT_PATH means verify: the caller gave something to
# verify against, so neither the own-stack rule nor the upgrade rule turns it
# off. A CA path kept from the live config does not: the controller may have run
# with it and verification off, and turning verification on behind the
# operator's back is the same unannounced change as above. It keeps the live
# value.
#
# An explicit PROMETHEUS_TLS_INSECURE_SKIP_VERIFY wins; OpenShift verifies
# against the service CA; plain http:// takes no TLS settings at all.
wva_prometheus_skip_verify() {
    if [ -n "${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY:-}" ]; then
        printf '%s' "$PROMETHEUS_TLS_INSECURE_SKIP_VERIFY"
        return 0
    fi
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    case "${PROMETHEUS_URL:-}" in
        https://*) ;;
        *) return 0 ;;
    esac
    if [ -n "${PROMETHEUS_CA_CERT_PATH:-}" ]; then
        printf 'false'
        return 0
    fi
    # The CA this install copied and mounts (wva_sync_prometheus_ca) is
    # something to verify against, as an explicit path is, and the install
    # probes the connection with it before reporting success.
    if [ -n "${WVA_PROM_CA_MOUNT:-}" ]; then
        printf 'false'
        return 0
    fi
    if [ -n "$(wva_prometheus_ca_path)" ]; then
        if [ "${WVA_LIVE_SKIP_VERIFY:-}" = "true" ]; then printf 'true'; else printf 'false'; fi
        return 0
    fi
    if wva_prometheus_is_own_stack || wva_prometheus_tls_kept_from_live; then
        printf 'true'
    fi
    return 0
}

# wva_prometheus_ca_path echoes the PROMETHEUS_CA_CERT_PATH this install writes
# into the manager config, or nothing.
#
# An explicit PROMETHEUS_CA_CERT_PATH wins. Otherwise the one the live config
# holds is kept, for the SAME Prometheus: the controller runs with it, and the
# CA volume and mount that go with it were added by the operator, which this
# install does not touch. Dropping it left the controller verifying against the
# public roots only, and a private-CA Prometheus then refused it. A different
# Prometheus is a new decision -- its certificate may come from another CA --
# so the live path is dropped there, and wva_warn_prometheus_tls_upgrade says
# so. Not on OpenShift, whose overlay sets the service CA as Deployment env;
# not for plain http://.
wva_prometheus_ca_path() {
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    case "${PROMETHEUS_URL:-}" in
        https://*) ;;
        *) return 0 ;;
    esac
    if [ -n "${PROMETHEUS_CA_CERT_PATH:-}" ]; then
        printf '%s' "$PROMETHEUS_CA_CERT_PATH"
        return 0
    fi
    # The installer's own copy is kept only while it is mounted: the volume
    # comes from this render (wva_append_prometheus_ca_mount_patch), and a path
    # with no volume behind it is a controller that cannot read its CA.
    if [ "${WVA_LIVE_PROM_TLS:-}" = "read" ] && [ -n "${WVA_LIVE_CA_PATH:-}" ] \
        && [ "$WVA_LIVE_CA_PATH" != "$WVA_PROM_CA_DIR/ca.crt" ] && wva_same_prometheus_as_live; then
        printf '%s' "$WVA_LIVE_CA_PATH"
        return 0
    fi
    [ -z "${WVA_PROM_CA_MOUNT:-}" ] || printf '%s' "$WVA_PROM_CA_DIR/ca.crt"
    return 0
}

# The CA this install copies for the controller: Secret WVA_PROM_CA_SECRET in
# WVA_NS, key ca.crt, mounted read-only at WVA_PROM_CA_DIR.
# WVA_PROM_CA_MOUNT is "1" once wva_sync_prometheus_ca has settled that the
# controller mounts it; WVA_PROM_CA_ERR holds why a source could not be read.
WVA_PROM_CA_SECRET=wva-prometheus-ca
WVA_PROM_CA_DIR=/etc/wva-prometheus-ca
WVA_PROM_CA_MOUNT=""
WVA_PROM_CA_ERR=""

# wva_prometheus_ca_source echoes where the Prometheus CA is copied from, as
# "<secret|configmap> <namespace> <name> <certificate key>" or "file <path>":
# PROMETHEUS_CA_FILE, else PROMETHEUS_SECRET_NAME/PROMETHEUS_SECRET_NS set by
# the caller, else what the Prometheus CR behind PROMETHEUS_URL serves its
# certificate from (spec.web.tlsConfig.cert). Nothing when none is known.
wva_prometheus_ca_source() {
    local ns cr ref
    if [ -n "${PROMETHEUS_CA_FILE:-}" ]; then
        printf 'file %s\n' "$PROMETHEUS_CA_FILE"
        return 0
    fi
    if [ -n "${PROMETHEUS_SECRET_NAME_EXPLICIT:-}" ]; then
        printf 'secret %s %s tls.crt\n' \
            "${PROMETHEUS_SECRET_NS_EXPLICIT:-${PROMETHEUS_SECRET_NS:-${MONITORING_NAMESPACE:-}}}" "$PROMETHEUS_SECRET_NAME_EXPLICIT"
        return 0
    fi
    read -r ns cr <<< "$(wva_prometheus_cr_for_url "${PROMETHEUS_URL:-}")"
    [ -n "${cr:-}" ] || return 0
    ref="$(wva_prometheus_cert_ref "$ns" "$cr")"
    [ -n "$ref" ] || return 0
    set -- $ref
    printf '%s %s %s %s\n' "$1" "$ns" "$2" "$3"
}

# wva_read_prometheus_ca writes to stdout the CA certificates held by source
# $1..$4 (see wva_prometheus_ca_source): key ca.crt when it has one, else the
# certificate key -- a self-signed serving certificate is its own CA. Only
# those keys are read, never the private key, and only CERTIFICATE blocks
# leave: a bundle that also carries a key gives up the certificates alone.
# Fails with WVA_PROM_CA_ERR set; a Forbidden read is an error, not "no CA".
wva_read_prometheus_ca() {
    local kind="$1" ns="$2" obj="${3:-}" key="${4:-tls.crt}" k pem="" rc
    WVA_PROM_CA_ERR=""
    if [ "$kind" = file ]; then
        if [ ! -r "$ns" ]; then
            WVA_PROM_CA_ERR="PROMETHEUS_CA_FILE=$ns is not readable"
            return 1
        fi
        pem="$(cat "$ns")"
    else
        for k in ca.crt "$key"; do
            rc=0
            kc_capture get "$kind" "$obj" -n "$ns" -o "jsonpath={.data.${k//./\\.}}" || rc=$?
            if [ "$rc" -ne 0 ]; then
                WVA_PROM_CA_ERR="$(printf '%s\n' "$KC_ERR" | head -1)"
                [ -n "$WVA_PROM_CA_ERR" ] || WVA_PROM_CA_ERR="kubectl exited $rc"
                return 1
            fi
            pem="$KC_OUT"
            [ -n "$pem" ] || continue
            if [ "$kind" = secret ]; then
                pem="$(printf '%s' "$pem" | base64 -d 2>/dev/null)" || pem=""
            fi
            [ -z "$pem" ] || break
        done
    fi
    pem="$(printf '%s\n' "$pem" | awk '/-----BEGIN CERTIFICATE-----/ { p = 1 } p { print } /-----END CERTIFICATE-----/ { p = 0 }')"
    if [ -z "$pem" ]; then
        WVA_PROM_CA_ERR="$kind ${obj:+$ns/$obj }holds no certificate under ca.crt or $key"
        return 1
    fi
    printf '%s\n' "$pem"
}

# wva_sync_prometheus_ca gives the controller the CA of the HTTPS Prometheus
# it verifies, with nothing done by hand: it copies the CA certificate from the
# Prometheus TLS Secret into Secret WVA_PROM_CA_SECRET in WVA_NS, and sets
# WVA_PROM_CA_MOUNT so the overlay mounts it and the config points
# PROMETHEUS_CA_CERT_PATH at it.
#
# Before this, a kube-prometheus-stack with web TLS needed three manual steps
# (a Secret, a volume patched onto the Deployment, a re-run with the path), and
# without them the controller verified against the public roots and could not
# connect.
#
# The source Secret lives in the monitoring namespace, which the admin phase
# (setup-prereqs, or INSTALL_PHASE=all) can read and a namespace admin usually
# cannot. So the admin phase makes the copy, and the controller phase uses
# that copy when it cannot read the source itself. Only when neither can it
# stops, naming what an admin runs. It never turns verification off instead.
#
# Not when the caller decided: PROMETHEUS_CA_CERT_PATH (mounted by them),
# PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true; nor the CA path a live controller
# verifies this Prometheus with; nor OpenShift, whose overlay mounts the
# service CA; nor plain http://.
wva_sync_prometheus_ca() {
    WVA_PROM_CA_MOUNT=""
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    case "${PROMETHEUS_URL:-}" in https://*) ;; *) return 0 ;; esac
    [ "${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY:-}" != "true" ] || return 0
    [ -z "${PROMETHEUS_CA_CERT_PATH:-}" ] || return 0
    if [ "${WVA_LIVE_PROM_TLS:-}" = "read" ] && [ -n "${WVA_LIVE_CA_PATH:-}" ] \
        && [ "$WVA_LIVE_CA_PATH" != "$WVA_PROM_CA_DIR/ca.crt" ] && wva_same_prometheus_as_live; then
        return 0
    fi

    local src kind ns obj key tmp rc=0 owner
    src="$(wva_prometheus_ca_source)"
    if [ -n "$src" ]; then
        read -r kind ns obj key <<< "$src"
        tmp="$(mktemp)"
        if wva_read_prometheus_ca "$kind" "$ns" "$obj" "$key" > "$tmp"; then
            kc_capture get secret "$WVA_PROM_CA_SECRET" -n "$WVA_NS" \
                -o 'jsonpath={.metadata.labels.app\.kubernetes\.io/managed-by}' || rc=$?
            if [ "$rc" -eq 0 ] && [ "$KC_OUT" != "wva-installer" ]; then
                rm -f "$tmp"
                log_info "Prometheus CA: Secret $WVA_PROM_CA_SECRET in $WVA_NS was not made by this installer; mounting it as it is."
                WVA_PROM_CA_MOUNT=1
                return 0
            fi
            local b64
            b64="$(base64 < "$tmp" | tr -d '\n')"
            if kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${WVA_PROM_CA_SECRET}
  namespace: ${WVA_NS}
  labels:
    app.kubernetes.io/managed-by: wva-installer
type: Opaque
data:
  ca.crt: ${b64}
EOF
            then
                rm -f "$tmp"
                log_success "Prometheus CA: copied from $kind $ns${obj:+/$obj} into Secret $WVA_PROM_CA_SECRET in $WVA_NS (the certificate only); the controller verifies with it."
                WVA_PROM_CA_MOUNT=1
                return 0
            fi
            WVA_PROM_CA_ERR="could not write Secret $WVA_PROM_CA_SECRET in $WVA_NS (see the error above)"
        fi
        rm -f "$tmp"
    fi

    # A copy made earlier -- by the admin phase -- serves when the source is
    # not readable now.
    if kubectl get secret "$WVA_PROM_CA_SECRET" -n "$WVA_NS" >/dev/null 2>&1; then
        log_info "Prometheus CA: using Secret $WVA_PROM_CA_SECRET in $WVA_NS, copied earlier${WVA_PROM_CA_ERR:+ (the source cannot be read now: $WVA_PROM_CA_ERR)}."
        WVA_PROM_CA_MOUNT=1
        return 0
    fi
    # No TLS Secret known at all: the public roots, and the probe says
    # whether they are enough.
    [ -n "$src" ] || return 0

    local fetch
    case "$kind" in
        file) fetch="cp $ns ca.crt" ;;
        secret) fetch="kubectl get secret $obj -n $ns -o jsonpath='{.data.${key//./\\.}}' | base64 -d > ca.crt" ;;
        *) fetch="kubectl get configmap $obj -n $ns -o jsonpath='{.data.${key//./\\.}}' > ca.crt" ;;
    esac
    log_error "Cannot give the controller the Prometheus CA: ${WVA_PROM_CA_ERR:-the source could not be read}.
  The controller verifies $PROMETHEUS_URL, so it needs the CA, and no copy exists in $WVA_NS yet.
  A cluster admin's prerequisites phase makes the copy:
    $(wva_make_cmd setup-prereqs)
  or the admin copies the certificate alone (never the key):
    $fetch
    kubectl create secret generic $WVA_PROM_CA_SECRET -n $WVA_NS --from-file=ca.crt=ca.crt
  Then re-run this install."
}

# wva_append_prometheus_ca_mount_patch mounts Secret WVA_PROM_CA_SECRET into
# the manager container at WVA_PROM_CA_DIR, read-only, in kustomization $1,
# when wva_sync_prometheus_ca settled that it should.
wva_append_prometheus_ca_mount_patch() {
    local kustomization="$1"
    [ -n "${WVA_PROM_CA_MOUNT:-}" ] || return 0
    grep -q '^patches:' "$kustomization" || printf 'patches:\n' >> "$kustomization"
    cat >> "$kustomization" <<PATCH
- patch: |-
    - op: test
      path: /spec/template/spec/containers/0/name
      value: manager
    - op: add
      path: /spec/template/spec/containers/0/volumeMounts/-
      value: {name: wva-prometheus-ca, mountPath: ${WVA_PROM_CA_DIR}, readOnly: true}
    - op: add
      path: /spec/template/spec/volumes/-
      value: {name: wva-prometheus-ca, secret: {secretName: ${WVA_PROM_CA_SECRET}, items: [{key: ca.crt, path: ca.crt}]}}
  target:
    kind: Deployment
    name: wva-controller-manager
PATCH
}

# wva_same_prometheus_as_live reports whether PROMETHEUS_URL is the Prometheus
# the live config names. A trailing slash does not make a different Prometheus.
#
# Nor does the Service in front of it. Detection used to answer
# prometheus-operated.<ns>, and now answers the Service the certificate names
# (wva_pick_prometheus_service) -- the same Prometheus. Read as a new one, a
# re-run dropped the CA the controller verified it with, and verification on
# with no CA is a controller that cannot connect. Only for a detected URL: one
# the caller set is the caller's decision.
wva_same_prometheus_as_live() {
    [ -n "${WVA_LIVE_PROM_URL:-}" ] || return 1
    [ "${WVA_LIVE_PROM_URL%/}" != "${PROMETHEUS_URL%/}" ] || return 0
    [ -z "${PROMETHEUS_URL_EXPLICIT:-}" ] || return 1
    local live_host="${WVA_LIVE_PROM_URL#*://}" ns
    live_host="${live_host%%/*}"
    live_host="${live_host%%:*}"
    ns="$(wva_prometheus_url_namespace "$WVA_LIVE_PROM_URL")"
    [ -n "$ns" ] || return 1
    case "$live_host" in
        "prometheus-operated.$ns.svc"|"prometheus-operated.$ns.svc.cluster.local") ;;
        *) return 1 ;;
    esac
    [ "${WVA_LIVE_PROM_URL%%://*}" = "${PROMETHEUS_URL%%://*}" ] \
        && [ "$(wva_prometheus_url_namespace "${PROMETHEUS_URL:-}")" = "$ns" ]
}

# wva_prometheus_is_own_stack reports whether PROMETHEUS_URL is the
# kube-prometheus-stack this install deploys: DEPLOY_PROMETHEUS=true, and the
# URL names that stack's Service in MONITORING_NAMESPACE --
# kube-prometheus-stack-prometheus, the default -- or, when nobody set
# PROMETHEUS_URL, the prometheus-operated Service a re-run detects in its place.
#
# Not "any URL in MONITORING_NAMESPACE": a Thanos, or a second Prometheus, in
# that namespace is somebody else's, and was being sent the controller's token
# with verification off.
#
# prometheus-operated is trusted on the name alone, unlike the default: it is
# the Service the Prometheus operator makes for ANY Prometheus CR in that
# namespace, not only the one this install's chart creates. Not checked against
# the Helm release because this is asked in many subshells and before the stack
# exists (INSTALL_PHASE=all); the URL reaches here only when nobody set one and
# the installer detected it, and MONITORING_NAMESPACE is this install's own.
#
# A URL holding userinfo, a query, a fragment or a backslash is never the own
# stack. Read naively, https://kube-prometheus-stack-prometheus.<mon>.svc:443@evil.example.com/
# names the stack before the "@" and connects to evil.example.com after it --
# with verification off and the controller's token. The host is also read
# after the last "@", so a URL that slips past the first rule still names the
# host it connects to.
wva_prometheus_is_own_stack() {
    [ "${DEPLOY_PROMETHEUS:-true}" = "true" ] || return 1
    local mon="${MONITORING_NAMESPACE:-workload-variant-autoscaler-monitoring}" host
    host="${PROMETHEUS_URL:-}"
    case "$host" in
        *@*|*\?*|*\#*|*\\*) return 1 ;;
    esac
    host="${host#*://}"
    host="${host%%/*}"
    host="${host##*@}"
    host="${host%%:*}"
    case "$host" in
        "kube-prometheus-stack-prometheus.$mon.svc"|"kube-prometheus-stack-prometheus.$mon.svc.cluster.local")
            return 0 ;;
        "prometheus-operated.$mon.svc"|"prometheus-operated.$mon.svc.cluster.local")
            [ -z "${PROMETHEUS_URL_EXPLICIT:-}" ] ;;
        *) return 1 ;;
    esac
}

# WVA_LIVE_PROM_TLS records what the manager config held BEFORE this install
# re-rendered it. One of:
#   ""        not read (the prereqs phase, or a caller that never asked);
#   absent    no wva-manager-config: a new install;
#   read      read; WVA_LIVE_SKIP_VERIFY, WVA_LIVE_CA_PATH and WVA_LIVE_PROM_URL
#             hold its values, and WVA_LIVE_PROM_KEYS every PROMETHEUS_* key
#             (wva_prometheus_keys) to tell afterwards whether any changed;
#   unknown   it exists or may exist, but could not be read (WVA_LIVE_PROM_TLS_ERR).
# Globals, set by wva_read_live_prometheus_tls in the caller's own shell.
WVA_LIVE_PROM_TLS=""
WVA_LIVE_SKIP_VERIFY=""
WVA_LIVE_CA_PATH=""
WVA_LIVE_PROM_URL=""
WVA_LIVE_PROM_KEYS=""
WVA_LIVE_PROM_TLS_ERR=""

# wva_prometheus_keys echoes every PROMETHEUS_* key of config.yaml $1, one
# "KEY = value" per line, sorted. Properties output prints "true" and true the
# same way, so a quoted and an unquoted value are not a change.
wva_prometheus_keys() {
    printf '%s\n' "$1" | yq -o=props 'with_entries(select(.key | test("^PROMETHEUS_")))' 2>/dev/null | sort
}

# wva_read_live_prometheus_tls <namespace> reads the Prometheus TLS settings of
# the live manager config in <namespace> into the WVA_LIVE_* globals above.
#
# Must run before the overlay apply: the apply replaces config.yaml with the
# rendered one, and after it the old values are gone. A read that fails for any
# reason other than NotFound is "unknown", never "absent" -- a Forbidden read
# taken as a new install would turn verification on in silence.
wva_read_live_prometheus_tls() {
    local ns="$1" rc=0 cfg
    WVA_LIVE_PROM_TLS="" WVA_LIVE_SKIP_VERIFY="" WVA_LIVE_CA_PATH="" WVA_LIVE_PROM_URL="" WVA_LIVE_PROM_KEYS="" WVA_LIVE_PROM_TLS_ERR=""
    kc_capture get configmap wva-manager-config -n "$ns" -o json || rc=$?
    if [ "$rc" -ne 0 ]; then
        if kc_not_found "$KC_ERR"; then
            WVA_LIVE_PROM_TLS=absent
            return 0
        fi
        WVA_LIVE_PROM_TLS=unknown
        WVA_LIVE_PROM_TLS_ERR="$(printf '%s\n' "$KC_ERR" | head -1)"
        [ -n "$WVA_LIVE_PROM_TLS_ERR" ] || WVA_LIVE_PROM_TLS_ERR="kubectl exited $rc"
        return 0
    fi
    cfg="$(printf '%s' "$KC_OUT" | jq -r '.data["config.yaml"] // ""' 2>/dev/null)" || cfg=""
    if [ -z "$cfg" ]; then
        WVA_LIVE_PROM_TLS=unknown
        WVA_LIVE_PROM_TLS_ERR="wva-manager-config in $ns has no config.yaml"
        return 0
    fi
    if ! WVA_LIVE_SKIP_VERIFY="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY // ""' 2>/dev/null)" \
        || ! WVA_LIVE_CA_PATH="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_CA_CERT_PATH // ""' 2>/dev/null)" \
        || ! WVA_LIVE_PROM_URL="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_BASE_URL // ""' 2>/dev/null)"; then
        WVA_LIVE_PROM_TLS=unknown WVA_LIVE_SKIP_VERIFY="" WVA_LIVE_CA_PATH="" WVA_LIVE_PROM_URL=""
        WVA_LIVE_PROM_TLS_ERR="the config.yaml of wva-manager-config in $ns does not parse as YAML"
        return 0
    fi
    WVA_LIVE_PROM_KEYS="$(wva_prometheus_keys "$cfg")"
    WVA_LIVE_PROM_TLS=read
}

# wva_prometheus_tls_kept_from_live reports whether the live controller skips
# verification with no CA path for the Prometheus this install points at -- the
# state an upgrade keeps rather than break.
#
# The no-CA term never decides today: both callers have already returned for a
# live CA on the same Prometheus (wva_prometheus_ca_path), and a different one
# fails the URL term. It is kept so the function means what its name says on
# its own, for a caller that does not check the CA first.
wva_prometheus_tls_kept_from_live() {
    [ "${WVA_LIVE_PROM_TLS:-}" = "read" ] \
        && [ "${WVA_LIVE_SKIP_VERIFY:-}" = "true" ] \
        && [ -z "${WVA_LIVE_CA_PATH:-}" ] \
        && wva_same_prometheus_as_live
}

# wva_rerun_install_vars echoes, one per line, the VAR=value arguments a re-run
# needs to install the SAME thing: every input that changes the install and
# that the caller set rather than left to a default.
#
# Without them a printed re-run changed the install it was meant to repeat:
# with no PROMETHEUS_URL it detected a Prometheus again, possibly another one;
# with no DEPLOY_PROMETHEUS=false it installed kube-prometheus-stack; with no IMG
# it deployed the default image over the one running. The image is carried
# whenever it is not the default, however it was chosen (IMG, -i,
# WVA_IMAGE_REPO/WVA_IMAGE_TAG).
wva_rerun_install_vars() {
    local img
    [ -z "${ENVIRONMENT_EXPLICIT:-}" ] || printf 'ENVIRONMENT=%q\n' "$ENVIRONMENT_EXPLICIT"
    if [ -n "${WVA_IMAGE_REPO:-}" ]; then
        if [ -n "${WVA_IMAGE_DIGEST:-}" ]; then img="$WVA_IMAGE_REPO@$WVA_IMAGE_DIGEST"; else img="$WVA_IMAGE_REPO:${WVA_IMAGE_TAG:-}"; fi
        [ "$img" = "${WVA_DEFAULT_IMAGE_REF:-}" ] || printf 'IMG=%q\n' "$img"
    fi
    [ -z "${PROMETHEUS_URL_EXPLICIT:-}" ] || printf 'PROMETHEUS_URL=%q\n' "$PROMETHEUS_URL_EXPLICIT"
    [ "${DEPLOY_PROMETHEUS:-true}" != "false" ] || printf 'DEPLOY_PROMETHEUS=false\n'
    return 0
}

# wva_rerun_cmd echoes the make command that re-runs this install, with any
# VAR=value arguments put in front. The variables that shaped this install
# (wva_rerun_install_vars) come first, so pasting it repeats the install and
# changes only what the arguments name.
wva_rerun_cmd() {
    local phase="" kv
    local -a vars=()
    local arg skip
    while IFS= read -r kv; do
        [ -n "$kv" ] || continue
        # A variable the arguments set again is theirs to set: printed twice,
        # the command reads as though both values counted.
        skip=""
        for arg in "$@"; do
            [ "${arg%%=*}" != "${kv%%=*}" ] || skip=1
        done
        [ -n "$skip" ] || vars+=("$kv")
    done <<< "$(wva_rerun_install_vars)"
    [ "${INSTALL_PHASE_EXPLICIT:-}" != "wva" ] || phase="INSTALL_PHASE=wva"
    wva_make_cmd deploy-wva ${vars[@]+"${vars[@]}"} ${phase:+"$phase"} "$@"
}

# wva_refuse_unknown_prometheus_tls stops the install, before anything is
# written, when the live controller config could not be read and this install
# would otherwise choose the TLS setting blind.
#
# An unreadable config is not a new install. Taken as one, the install wrote
# "verify" for a controller that may have run unverified with no CA, and it then
# could not connect -- the same failure wva_live_limiters refuses for the
# limiter. The operator knows which it was; the install does not.
#
# Not refused when the caller decided -- an explicit skip, or an explicit CA
# path, which means verify -- nor for the Prometheus this install deploys,
# whose setting does not depend on the live config. main calls this before
# any phase runs, so "Nothing was changed" is true; it resolves the URL only
# once the cheaper answers have not settled it.
wva_refuse_unknown_prometheus_tls() {
    [ "${WVA_LIVE_PROM_TLS:-}" = "unknown" ] || return 0
    [ -z "${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY:-}" ] || return 0
    [ -z "${PROMETHEUS_CA_CERT_PATH:-}" ] || return 0
    [ -z "${WVA_PROM_CA_MOUNT:-}" ] || return 0
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    wva_resolve_prometheus_url
    case "${PROMETHEUS_URL:-}" in
        https://*) ;;
        *) return 0 ;;
    esac
    ! wva_prometheus_is_own_stack || return 0
    log_error "Cannot read the controller config this install replaces (${WVA_LIVE_PROM_TLS_ERR:-wva-manager-config in $WVA_NS is unreadable}), so it cannot tell whether TLS verification for $PROMETHEUS_URL is on now. Nothing was changed. Re-run naming it:
    $(wva_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=false)   to verify (a public CA, or set PROMETHEUS_CA_CERT_PATH too)
    $(wva_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true)    to connect without verifying"
}

# wva_hint_untrusted_prometheus_cert prints the two fixes when $1 -- the
# controller's last container lines -- shows it refusing the Prometheus
# certificate. That is the failure this install's TLS default can cause, and
# the raw x509 line does not say what to do about it. Not on OpenShift: its
# overlay mounts the service CA and takes no TLS settings from the installer,
# so neither re-run would change anything there.
wva_hint_untrusted_prometheus_cert() {
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    printf '%s\n' "$1" | grep -qE 'x509|certificate signed by unknown authority' || return 0
    # A certificate for another host is not fixed by a CA: the controller
    # trusts it and still refuses it. Name the hosts it is valid for instead.
    if printf '%s\n' "$1" | grep -qE 'certificate is valid for|not valid for any names'; then
        log_warning "  The Prometheus certificate does not name the host the controller connects to. Point it at a name the"
        log_warning "  certificate holds (listed in the x509 line above):  $(wva_rerun_cmd 'PROMETHEUS_URL=https://<name>:<port>')"
        log_warning "  See docs/reference/troubleshooting.md#prometheus-certificate-name-mismatch."
        return 0
    fi
    log_warning "  The controller does not trust the Prometheus certificate. Fix it one of two ways:"
    log_warning "    1. Mount the CA into the controller, then re-run with its path (docs/reference/prometheus.md#tls-configuration):"
    log_warning "         $(wva_rerun_cmd 'PROMETHEUS_CA_CERT_PATH=<file>')"
    log_warning "    2. For a development cluster only, re-run without verification:"
    log_warning "         $(wva_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true)"
    log_warning "  The re-run restarts the controller so it reads the new setting."
}

# wva_warn_prometheus_tls_upgrade says when wva_prometheus_skip_verify kept
# verification off from the live config, when it did not because this install
# points at a different Prometheus, and when it dropped the live CA path for
# the same reason.
wva_warn_prometheus_tls_upgrade() {
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    case "${PROMETHEUS_URL:-}" in
        https://*) ;;
        *) return 0 ;;
    esac
    if [ -z "${PROMETHEUS_CA_CERT_PATH:-}" ] && [ -z "${WVA_PROM_CA_MOUNT:-}" ] && [ "${WVA_LIVE_PROM_TLS:-}" = "read" ] \
        && [ -n "${WVA_LIVE_CA_PATH:-}" ] && ! wva_same_prometheus_as_live; then
        log_warning "=================================================================="
        log_warning "The controller verified ${WVA_LIVE_PROM_URL:-its old Prometheus} with the CA at $WVA_LIVE_CA_PATH."
        log_warning "  This install points at $PROMETHEUS_URL instead, so that CA path was DROPPED."
        log_warning "  If the new Prometheus has a certificate from the same CA, re-run with it:"
        log_warning "    $(wva_rerun_cmd "PROMETHEUS_CA_CERT_PATH=$WVA_LIVE_CA_PATH")"
        log_warning "=================================================================="
    fi
    [ -z "${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY:-}" ] || return 0
    [ -z "$(wva_prometheus_ca_path)" ] || return 0
    ! wva_prometheus_is_own_stack || return 0
    [ "${WVA_LIVE_PROM_TLS:-}" = "read" ] || return 0
    [ "${WVA_LIVE_SKIP_VERIFY:-}" = "true" ] || return 0
    if wva_prometheus_tls_kept_from_live; then
        log_warning "=================================================================="
        log_warning "TLS verification for $PROMETHEUS_URL stays OFF, as it was before this upgrade."
        log_warning "  New installs verify by default. This controller has no CA to verify with, so turning it"
        log_warning "  on now would stop it connecting to Prometheus. To turn verification on, do one of these:"
        log_warning "    1. Mount the CA into the controller and re-run with PROMETHEUS_CA_CERT_PATH=<file> (docs/reference/prometheus.md#tls-configuration)."
        log_warning "    2. If a public CA signed its certificate, re-run with verification on:"
        log_warning "         $(wva_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=false)"
        log_warning "=================================================================="
        return 0
    fi
    log_warning "=================================================================="
    log_warning "TLS verification was OFF for ${WVA_LIVE_PROM_URL:-the Prometheus this controller used before}."
    log_warning "  This install points at $PROMETHEUS_URL instead, so it takes the new default and VERIFIES it."
    log_warning "  If that Prometheus has a private CA, the controller cannot connect. To keep verification off:"
    log_warning "    $(wva_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true)"
    log_warning "=================================================================="
    return 0
}

# wva_warn_prometheus_tls warns when the controller will connect to an HTTPS
# Prometheus without verifying its certificate, and when it will verify one
# with no CA configured -- which fails for a private CA. $1 is the manager
# config.yaml. OpenShift verifies against the service CA, mounted by its
# overlay, and never reaches either warning. Sets WVA_TLS_SUMMARY to one line
# print_summary repeats, since this warning scrolls past long before the end.
wva_warn_prometheus_tls() {
    local cfg="$1" url skip ca
    WVA_TLS_SUMMARY=""
    [ -n "$cfg" ] || return 0
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    url="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_BASE_URL // ""' 2>/dev/null || true)"
    skip="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY // ""' 2>/dev/null || true)"
    ca="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_CA_CERT_PATH // ""' 2>/dev/null || true)"
    case "$url" in https://*) ;; *) return 0 ;; esac
    if [ -n "$ca" ]; then
        [ "$skip" = "true" ] || return 0
        # A CA kept from the live config keeps the live skip value
        # (wva_prometheus_skip_verify), so a controller can hold a CA and still
        # not verify. Say so: the CA path reads as though it verifies.
        WVA_TLS_SUMMARY="does NOT verify the certificate of $url, although a CA path ($ca) is set (see the TLS warning above)"
        log_warning "WVA will NOT verify the TLS certificate of $url: PROMETHEUS_TLS_INSECURE_SKIP_VERIFY is true, as it was before."
        log_warning "  PROMETHEUS_CA_CERT_PATH is set ($ca) but unused while verification is off. To verify against it:"
        log_warning "    $(wva_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=false)"
        return 0
    fi
    if [ "$skip" != "true" ]; then
        WVA_TLS_SUMMARY="verifies $url against the public roots only; if it has a private CA, the controller cannot connect (see the TLS warning above)"
        log_warning "=================================================================="
        log_warning "WVA will VERIFY the TLS certificate of $url against the public roots only (no PROMETHEUS_CA_CERT_PATH)."
        log_warning "  The installer turns verification off only for the Prometheus it deploys itself."
        log_warning "  If its certificate comes from a private CA, the controller cannot connect. To check, once it has started:"
        log_warning "    kubectl logs -n $WVA_NS deploy/wva-controller-manager | grep -m1 x509"
        log_warning "  No output: nothing to do. Otherwise fix it one of two ways:"
        log_warning "    1. Mount the CA into the controller, then re-run with its path (docs/reference/prometheus.md#tls-configuration):"
        log_warning "         $(wva_rerun_cmd 'PROMETHEUS_CA_CERT_PATH=<file>')"
        log_warning "    2. For a development cluster only, re-run without verification:"
        log_warning "         $(wva_rerun_cmd PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true)"
        log_warning "  A re-run that changes either setting restarts the controller, which reads them only at startup."
        log_warning "=================================================================="
        return 0
    fi
    WVA_TLS_SUMMARY="does NOT verify the certificate of $url (development only; see the TLS warning above)"
    log_warning "WVA will NOT verify the TLS certificate of $url (PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true, no PROMETHEUS_CA_CERT_PATH)."
    log_warning "  Fine for a development cluster. In production, mount the CA that signed the Prometheus certificate into the"
    log_warning "  controller, then re-run with its path:"
    log_warning "    $(wva_rerun_cmd 'PROMETHEUS_CA_CERT_PATH=<file>')"
    log_warning "  See docs/reference/prometheus.md#tls-configuration."
}

# wva_prometheus_url_namespace echoes the namespace a cluster-local Prometheus
# URL ($1) addresses -- <service>.<namespace>.svc[...] -- or nothing.
wva_prometheus_url_namespace() {
    local host="${1#*://}" svc ns rest
    host="${host%%/*}"
    host="${host%%:*}"
    IFS=. read -r svc ns rest <<< "$host"
    [ -n "$svc" ] && [ -n "$ns" ] || return 0
    case "$rest" in
        svc|svc.*) printf '%s\n' "$ns" ;;
    esac
}

# wva_prometheus_scraper_sa echoes "<namespace> <serviceaccount>" for the
# Prometheus that scrapes the controller's /metrics, or nothing when it cannot
# tell. Nothing means NO binding: a grant to an account nobody chose is worse
# than none, because it waits for whoever creates that name.
#
# config/base names kube-prometheus-stack-prometheus in
# workload-variant-autoscaler-monitoring: the Prometheus this installer deploys,
# in its default namespace. On a cluster with its own Prometheus that subject is
# an account that does not exist. Only three answers are trusted, each one the
# caller CHOSE:
#
#   PROMETHEUS_SA=<ns>/<name>  an explicit answer
#   PROMETHEUS_URL, set by the caller (PROMETHEUS_URL_EXPLICIT), addressing a
#                              namespace whose Prometheus CRs agree on ONE
#                              non-default spec.serviceAccountName
#   the kube-prometheus-stack THIS install deploys: DEPLOY_PROMETHEUS=true and
#                              PROMETHEUS_URL addressing MONITORING_NAMESPACE --
#                              its account is known, not read
#
# Never from an AUTO-DETECTED URL. wva_detect_prometheus_url returns the first
# Prometheus CR, in the order the API lists them, that selects the model
# namespace, or when none is known to, the first one; on a shared cluster that
# is usually someone else's, and the grant then went to an account in a
# namespace nobody named. Never "the first Prometheus CR on the cluster" for
# the same reason, and never `default`, which is what a CR with no
# serviceAccountName runs as: every Pod in that namespace without its own
# account would be granted. PROMETHEUS_URL must be resolved before this is
# asked (wva_resolve_prometheus_url), or the namespace is the default's.
wva_prometheus_scraper_sa() {
    if [ -n "${PROMETHEUS_SA:-}" ]; then
        case "$PROMETHEUS_SA" in
            ?*/?*) printf '%s %s\n' "${PROMETHEUS_SA%%/*}" "${PROMETHEUS_SA#*/}" ;;
            *) log_warning "PROMETHEUS_SA must be <namespace>/<serviceaccount>; ignoring '$PROMETHEUS_SA', so no Prometheus is granted" ;;
        esac
        return 0
    fi
    local url_ns ns sa sas=""
    url_ns="$(wva_prometheus_url_namespace "${PROMETHEUS_URL:-}")"
    [ -n "$url_ns" ] || return 0
    if [ -n "${PROMETHEUS_URL_EXPLICIT:-}" ]; then
        while read -r ns sa; do
            [ "$ns" = "$url_ns" ] || continue
            sas="$sas ${sa:-default}"
        done < <(kubectl get prometheuses.monitoring.coreos.com -n "$url_ns" \
            -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.spec.serviceAccountName}{"\n"}{end}' 2>/dev/null || true)
        sas="$(printf '%s\n' $sas | sort -u)"
        if [ -n "$sas" ]; then
            # One account, and not `default`; anything else is not an answer.
            [ "$(printf '%s\n' "$sas" | wc -l | tr -d ' ')" = 1 ] || return 0
            [ "$sas" != default ] || return 0
            printf '%s %s\n' "$url_ns" "$sas"
            return 0
        fi
    fi
    if [ "${DEPLOY_PROMETHEUS:-true}" = "true" ] \
        && [ "$url_ns" = "${MONITORING_NAMESPACE:-workload-variant-autoscaler-monitoring}" ]; then
        printf '%s kube-prometheus-stack-prometheus\n' "$url_ns"
    fi
}

# wva_append_metrics_reader_subject_patch appends to kustomization $1 a patch
# that points the metrics-reader binding at $3 ("<namespace> <serviceaccount>"),
# or leaves the binding out of the render when $3 is empty: this install never
# creates a grant to an account nobody chose. Left out, an existing binding is
# not touched by the apply (wva_metrics_reader_binding keeps it, and says so).
# $2 is the install namespace. Must come AFTER
# wva_append_crb_name_patches, and targets the name that leaves the binding with
# -- a target naming the shipped name matches nothing once it is renamed, and
# kustomize says nothing about a patch that matched nothing.
wva_append_metrics_reader_subject_patch() {
    local kustomization="$1" ns="$2" scraper="$3" name
    name="wva-metrics-reader-rolebinding-$(wva_ns_suffix "$ns")"
    grep -q '^patches:' "$kustomization" || printf 'patches:\n' >> "$kustomization"
    if [ -n "$scraper" ]; then
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /subjects
      value:
      - kind: ServiceAccount
        name: ${scraper#* }
        namespace: ${scraper%% *}
  target:
    kind: ClusterRoleBinding
    name: ${name}
EOF
    else
        cat >> "$kustomization" <<EOF
- patch: |-
    \$patch: delete
    apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: ${name}
  target:
    kind: ClusterRoleBinding
    name: ${name}
EOF
    fi
}

# wva_append_metrics_reader_decision decides who the metrics-reader binding
# names and appends that patch to kustomization $1 for install namespace $2. It
# sets WVA_METRICS_READER_SCRAPER to "<namespace> <serviceaccount>", or to empty
# when the binding is dropped.
#
# ONE decision for the install and for wva_render_manifests, which renders the
# set of admin-owned objects the controller-only phase expects to exist. They
# diverged: the install dropped the binding, the expected set still listed it,
# so --check reported it MISSING forever and INSTALL_PHASE=wva refused.
#
# The URL is resolved FIRST: the subject is read from the namespace
# PROMETHEUS_URL addresses, and asking before the detection ran read the
# shipped default's namespace instead of the Prometheus the controller will use.
# Sets a variable rather than echoing so the resolution stays in this shell.
wva_append_metrics_reader_decision() {
    local kustomization="$1" ns="$2"
    wva_resolve_prometheus_url
    WVA_METRICS_READER_SCRAPER="$(wva_prometheus_scraper_sa)"
    wva_append_metrics_reader_subject_patch "$kustomization" "$ns" "$WVA_METRICS_READER_SCRAPER"
}

# wva_metrics_reader_binding is the install's half of the metrics-reader
# decision, for kustomization $1 and install namespace $2: it writes or
# repoints the binding when a trusted source names the Prometheus account, and
# otherwise KEEPS whatever binding is already there and says so.
#
# Never a delete. "No trusted source answers" is what an upgrade sees whenever
# its caller did not repeat PROMETHEUS_SA or PROMETHEUS_URL -- not evidence the
# binding is wrong -- and deleting it on every such re-run stopped Prometheus
# scraping the controller, silently: no error, just no wva_* series. Left out of
# the render, the binding is not touched by the apply; the warning names it, who
# it grants, and the two ways to change that.
#
# Only the phases that own the binding decide. The controller-only phase
# (WVA_APPLY_SCOPE=controller) filters every ClusterRoleBinding out of what it
# applies, so a decision and its messages there described something it does not
# do -- the same reason wva_append_scaler_networkpolicy_patch returns early.
wva_metrics_reader_binding() {
    local kustomization="$1" ns="$2" scraper name rc=0 subjects
    [ "${WVA_APPLY_SCOPE:-all}" != "controller" ] || return 0
    wva_append_metrics_reader_decision "$kustomization" "$ns"
    scraper="$WVA_METRICS_READER_SCRAPER"
    if [ -n "$scraper" ]; then
        log_info "Controller /metrics readable by ServiceAccount ${scraper#* } in ${scraper%% *}"
        return 0
    fi
    name="wva-metrics-reader-rolebinding-$(wva_ns_suffix "$ns")"
    kc_capture get clusterrolebinding "$name" \
        -o jsonpath='{range .subjects[*]}{.namespace}{"/"}{.name}{" "}{end}' || rc=$?
    if [ "$rc" -eq 0 ]; then
        subjects="$(printf '%s' "$KC_OUT" | sed 's/ *$//')"
        log_warning "Kept ClusterRoleBinding $name as it is (grants the controller's /metrics to: ${subjects:-no subject}): nothing you set says which Prometheus account scrapes it, so this install neither repoints nor removes it."
        log_warning "  Trusted sources: PROMETHEUS_SA, a PROMETHEUS_URL you passed (its namespace's Prometheus CR, one non-default serviceAccountName), or the kube-prometheus-stack this install deploys -- never a Prometheus found by detection (${PROMETHEUS_URL:-none})."
        log_warning "  To repoint it:  $(wva_make_cmd setup-prereqs 'PROMETHEUS_SA=<namespace>/<serviceaccount>')"
        log_warning "  To remove it:   kubectl delete clusterrolebinding $name"
        return 0
    fi
    if ! kc_not_found "$KC_ERR"; then
        log_warning "Could not read ClusterRoleBinding $name (${KC_ERR:-kubectl exited $rc}), so whether a Prometheus may read the controller's /metrics is unknown; this install leaves it as it is."
        log_warning "  Check:          kubectl get clusterrolebinding $name -o yaml"
        log_warning "  To grant one:   $(wva_make_cmd setup-prereqs 'PROMETHEUS_SA=<namespace>/<serviceaccount>')"
        return 0
    fi
    log_warning "No Prometheus is granted the controller's /metrics: nothing you set says which account scrapes it. Only PROMETHEUS_SA, a PROMETHEUS_URL you passed (its namespace's Prometheus CR, one non-default serviceAccountName), or the kube-prometheus-stack this install deploys are trusted -- never a Prometheus found by detection (${PROMETHEUS_URL:-none})."
    log_warning "  To grant it:  $(wva_make_cmd setup-prereqs 'PROMETHEUS_SA=<namespace>/<serviceaccount>'), or bind ClusterRole wva-metrics-reader-$(wva_ns_suffix "$ns") yourself."
}

# wva_append_kueue_reader_scope_patch narrows the namespace-scoped Kueue
# reader's `get namespaces` to the one namespace this controller manages ($3),
# in kustomization $1 for install namespace $2. Must come after
# wva_append_crb_name_patches: it targets the renamed ClusterRole.
#
# The reader GETs a Namespace by name, only for a LocalQueue's namespace, and a
# namespace-scoped controller lists LocalQueues in its managed namespace alone
# (internal/kueue, Options.Namespace = the watch namespace). So no other name is
# ever read, and a grant to read every Namespace's labels is more than the
# reader uses. The `test` op makes a reordered rule fail the build rather than
# narrow the wrong one.
#
# Names after $3 are kept too: the install passes what wva_kueue_reader_names
# found on the live role, so a name an admin or `make enable-physical-limiter`
# put there is not taken away by the next install (common.sh says why the two
# must agree).
wva_append_kueue_reader_scope_patch() {
    local kustomization="$1" ns="$2" managed="${3:-}" n names="" seen=" "
    [ -n "$managed" ] || return 0
    shift 3
    for n in "$managed" "$@"; do
        [ -n "$n" ] || continue
        case "$seen" in *" $n "*) continue ;; esac
        seen="$seen$n "
        names="$names      - \"$n\""$'\n'
    done
    grep -q '^patches:' "$kustomization" || printf 'patches:\n' >> "$kustomization"
    cat >> "$kustomization" <<EOF
- patch: |-
    - op: test
      path: /rules/1/resources/0
      value: namespaces
    - op: add
      path: /rules/1/resourceNames
      value:
${names}  target:
    kind: ClusterRole
    name: wva-kueue-reader-role-$(wva_ns_suffix "$ns")
EOF
}

# wva_append_install_kueue_reader_scope_patch is the install's call of
# wva_append_kueue_reader_scope_patch (same arguments): the managed namespace
# $3, plus the names the live role already carries (wva_kueue_reader_names) --
# the set `make enable-physical-limiter` writes, so neither undoes the other.
#
# Only a phase that applies the role reads it: the controller-only phase
# (WVA_APPLY_SCOPE=controller) writes no cluster-scoped RBAC, and a tenant may
# not read it. A role that exists and cannot be read stops the install rather
# than lose a name.
wva_append_install_kueue_reader_scope_patch() {
    local kustomization="$1" ns="$2" managed="$3"
    WVA_KUEUE_READER_NAMES="$managed"
    case "${WVA_APPLY_SCOPE:-all}" in
        all|prereqs)
            # The namespaces this install managed before (its labelled grants):
            # one no longer listed loses its name here, so a shrunk list
            # shrinks the read instead of keeping the old names forever.
            WVA_KUEUE_READER_PREVIOUS="$(wva_granted_namespaces 2>/dev/null | tr '\n' ' ' || true)"
            wva_kueue_reader_names "$ns" "$managed" \
                || log_error "Could not read ClusterRole wva-kueue-reader-role-$(wva_ns_suffix "$ns") ($KC_ERR). Refusing to rewrite it without knowing which Namespaces it already names: the rewrite could take one away."
            ;;
    esac
    # The managed name as given when the helper kept none, as before: an empty
    # list would leave the shipped rule unnamed, which is every Namespace.
    [ -n "$WVA_KUEUE_READER_NAMES" ] || WVA_KUEUE_READER_NAMES="$managed"
    # shellcheck disable=SC2086 # one word per Namespace name
    wva_append_kueue_reader_scope_patch "$kustomization" "$ns" $WVA_KUEUE_READER_NAMES
}

# wva_kueue_reader_unneeded reports whether this install's controller needs no
# Kueue reader: it declares no limiter (WVA_LIMITER=none), and no policy it
# reads configures a Kueue quota source (wva_kueue_source_configured: a
# cluster policy it resolves to may). WVA_LIMITER unset keeps the live
# limiter, which may read Kueue, and a policy that cannot be read may too:
# either keeps the reader.
wva_kueue_reader_unneeded() {
    [ -n "${WVA_LIMITER:-}" ] || return 1
    [ "$(limiter_types_of "$WVA_LIMITER" 2>/dev/null)" = none ] || return 1
    declare -F wva_kueue_source_configured >/dev/null || return 1
    [ -z "$(wva_kueue_source_configured 2>/dev/null)" ]
}

# wva_append_kueue_reader_delete drops the namespace-scoped Kueue reader --
# ClusterRole wva-kueue-reader-role-<suffix> and its binding -- from the render
# of kustomization $1 for install namespace $2. Must come after
# wva_append_crb_name_patches: it targets the renamed objects.
#
# Kustomize cannot make a component conditional, so the overlay always lists
# components/kueue-reader and an install with no limiter dropped it here. It
# only stops CREATING the read: one an earlier install made stays, and
# `make enable-physical-limiter` grants it again when a Kueue quota is
# published (pl_grant_kueue_read).
wva_append_kueue_reader_delete() {
    local kustomization="$1" suffix kind name
    suffix="$(wva_ns_suffix "$2")"
    grep -q '^patches:' "$kustomization" || printf 'patches:\n' >> "$kustomization"
    for kind in ClusterRole ClusterRoleBinding; do
        name="wva-kueue-reader-role-${suffix}"
        [ "$kind" = ClusterRole ] || name="wva-kueue-reader-rolebinding-${suffix}"
        cat >> "$kustomization" <<PATCH
- patch: |-
    \$patch: delete
    apiVersion: rbac.authorization.k8s.io/v1
    kind: ${kind}
    metadata:
      name: ${name}
  target:
    kind: ${kind}
    name: ${name}
PATCH
    done
}

# wva_managed_policy_label echoes the wva.llmd.ai/policy-namespace label on
# namespace $1, or nothing when it is absent or cannot be read. Only ever ADDS
# a name to the grant below, so an unreadable label costs the controller the
# read of a namespace it does not GET anyway (it uses the label's value as is).
wva_managed_policy_label() {
    [ -n "$1" ] || return 0
    kubectl get namespace "$1" \
        -o jsonpath='{.metadata.labels.wva\.llmd\.ai/policy-namespace}' 2>/dev/null || true
}

# wva_append_policy_namespace_reader_scope_patch narrows the namespace-scoped
# policy-namespace reader's `get namespaces`, in kustomization $1 for install
# namespace $2, to the Namespaces the controller reads (managed namespace $3,
# plus the names after it). Must come after wva_append_crb_name_patches: it
# targets the renamed ClusterRole.
#
# ResolvePolicyNamespace GETs two Namespaces by name, never a list: the
# well-known wva-policy, and the managed one for its policy-namespace label.
# The names are the set pl_grant_namespace_read (physical_limiter.sh) writes
# under the SAME ClusterRole name -- its own namespace, wva-policy, and the
# policy namespace -- so neither apply widens what the other narrowed. The
# shipped component carries no resourceNames, and an install over a limiter's
# narrowed grant used to re-widen it to every Namespace in the cluster.
#
# The `test` op makes a reordered rule fail the build rather than narrow the
# wrong one.
wva_append_policy_namespace_reader_scope_patch() {
    local kustomization="$1" ns="$2" managed="$3" n names="" seen=" "
    shift 3
    [ -n "$ns" ] || return 0
    for n in "$ns" "$managed" wva-policy "$@"; do
        [ -n "$n" ] || continue
        case "$seen" in *" $n "*) continue ;; esac
        seen="$seen$n "
        names="$names      - \"$n\""$'\n'
    done
    grep -q '^patches:' "$kustomization" || printf 'patches:\n' >> "$kustomization"
    cat >> "$kustomization" <<EOF
- patch: |-
    - op: test
      path: /rules/0/resources/0
      value: namespaces
    - op: add
      path: /rules/0/resourceNames
      value:
${names}  target:
    kind: ClusterRole
    name: wva-policy-namespace-reader-role-$(wva_ns_suffix "$ns")
EOF
}

# wva_append_replicas_patch sets the controller Deployment's replicas to
# WVA_REPLICAS in the throw-away overlay's kustomization.yaml $1. Nothing is
# written when WVA_REPLICAS is unset (install.sh defaults it to 1).
#
# More than one replica is a warm standby: the manifest sets
# --leader-elect=true, so only the leader runs the optimization loops and the
# standbys wait on the lease. Two replicas turn a node drain or a crash from "no
# scaling decisions until the pod is rescheduled" into a lease timeout. It is
# failover, not throughput.
#
# Written at 1 as well. The manifest carries no replicas field, and
# `kubectl apply` leaves a field the manifest does not set alone, so a re-install
# over a controller someone had scaled to 0 kept it at 0.
#
# WVA_REPLICAS must be a non-negative integer: anything else is refused here,
# not left for the API server to reject the whole apply. With namespace $2 the
# live Deployment's count is read first, and a re-install that changes it (a
# controller scaled to 2 by hand, put back to 1) says so.
wva_append_replicas_patch() {
    local kustomization="$1" ns="${2:-}" live
    [ -n "${WVA_REPLICAS:-}" ] || return 0
    case "$WVA_REPLICAS" in
        *[!0-9]*|0?*)
            log_error "WVA_REPLICAS=$WVA_REPLICAS is not a non-negative integer (0, 1, 2, ...)" ;;
    esac
    if [ "$WVA_REPLICAS" = "0" ]; then
        log_warning "WVA_REPLICAS=0: the controller is installed with no replica, so nothing makes scaling decisions until it is scaled up"
    elif [ "$WVA_REPLICAS" != "1" ]; then
        log_info "Controller replicas: $WVA_REPLICAS (leader-elected; standbys take over on failure, they do not share the work)"
    fi
    if [ -n "$ns" ]; then
        live="$(kubectl get deployment wva-controller-manager -n "$ns" \
            -o jsonpath='{.spec.replicas}' 2>/dev/null)" || live=""
        if [ -n "$live" ] && [ "$live" != "$WVA_REPLICAS" ]; then
            log_warning "Controller replicas: the live Deployment runs $live; this install sets it to WVA_REPLICAS=$WVA_REPLICAS. Pass WVA_REPLICAS=$live to keep it."
        fi
    fi
    grep -q '^patches:' "$kustomization" || printf 'patches:\n' >> "$kustomization"
    cat >> "$kustomization" <<EOF
- patch: |-
    - op: add
      path: /spec/replicas
      value: ${WVA_REPLICAS}
  target:
    kind: Deployment
    name: wva-controller-manager
EOF
}

# wva_write_prometheus_settings writes the Prometheus URL and TLS settings into
# the live manager config, after the overlay apply, and warns about what the
# controller will run with. Needs the WVA_LIVE_* globals that
# wva_read_live_prometheus_tls set before the apply.
wva_write_prometheus_settings() {
    wva_resolve_prometheus_url

    if [ -n "${PROMETHEUS_URL:-}" ]; then
        log_info "Pointing WVA at Prometheus: $PROMETHEUS_URL"
        # strenv, never spliced into the program: a quote in the URL would
        # otherwise be yq syntax.
        WVA_URL="$PROMETHEUS_URL" patch_manager_config '.PROMETHEUS_BASE_URL = strenv(WVA_URL)'
        wva_reconcile_prometheus_scheme "$PROMETHEUS_URL"
    fi
    local skip_verify ca_path
    skip_verify="$(wva_prometheus_skip_verify)"
    if [ -n "$skip_verify" ]; then
        WVA_SKIP="$skip_verify" patch_manager_config '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = strenv(WVA_SKIP)'
    fi
    ca_path="$(wva_prometheus_ca_path)"
    if [ -n "$ca_path" ]; then
        WVA_CA="$ca_path" patch_manager_config '.PROMETHEUS_CA_CERT_PATH = strenv(WVA_CA)'
    fi
    wva_warn_prometheus_tls_upgrade
    # Read back what the controller will actually run with, after every patch
    # above, rather than reasoning about which of them applied.
    local cfg
    cfg="$(kubectl get configmap wva-manager-config -n "$WVA_NS" \
        -o jsonpath='{.data.config\.yaml}' 2>/dev/null || true)"
    wva_warn_prometheus_tls "$cfg"
    wva_verify_prometheus_tls "$cfg"
}

# wva_verify_prometheus_tls stops the install when the controller, with the
# manager config $1 it will run with, cannot verify the Prometheus certificate.
#
# The install used to print "Prometheus: <url>" as a success and leave the
# controller to find out: it exits in ValidatePrometheusAPI, and the x509 line
# that says why is in a log nobody was told to read. Two checks, both only for
# an HTTPS Prometheus the controller verifies, and not on OpenShift, whose
# overlay mounts the service CA and takes no TLS settings from here:
#
#   1. the certificate the Prometheus CR serves, read from its Secret or
#      ConfigMap, must name the host (wva_prometheus_cert_check). Known not to,
#      the install stops; not knowable, it goes on to the probe.
#   2. a one-shot Pod in the controller's namespace connects with curl, with
#      the CA the controller is given (the same volume, at the same path) or
#      the public roots, and no option that skips verification
#      (wva_probe_prometheus_tls). A refused certificate stops the install;
#      a probe that could not run is a warning that says why.
#
# PROMETHEUS_TLS_PROBE=false skips the probe -- for a cluster that cannot pull
# its image (PROMETHEUS_TLS_PROBE_IMAGE) -- but never the certificate check.
wva_verify_prometheus_tls() {
    local cfg="$1" url skip ca sn rc=0
    [ -n "$cfg" ] || return 0
    [ "${ENVIRONMENT:-kubernetes}" != "openshift" ] || return 0
    url="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_BASE_URL // ""' 2>/dev/null || true)"
    skip="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY // ""' 2>/dev/null || true)"
    ca="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_CA_CERT_PATH // ""' 2>/dev/null || true)"
    sn="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_SERVER_NAME // ""' 2>/dev/null || true)"
    case "$url" in https://*) ;; *) return 0 ;; esac
    [ "$skip" != "true" ] || return 0

    wva_prometheus_cert_check "$url" "$sn" || rc=$?
    if [ "$rc" -eq 1 ]; then
        local host="${url#*://}"
        host="${host%%/*}"
        host="${host##*@}"
        host="${sn:-${host%%:*}}"
        log_warning "=================================================================="
        log_warning "The Prometheus certificate does not name $host, and the controller verifies it, so it cannot connect:"
        log_warning "  it would exit with \"x509: certificate is valid for ${WVA_PROM_CERT_NAMES}, not $host\"."
        wva_prometheus_cert_name_advice "$url"
        log_warning "=================================================================="
        log_error "Prometheus at $url: its certificate does not name the host the controller verifies (see above)."
    fi
    if [ "${PROMETHEUS_TLS_PROBE:-true}" = "false" ]; then
        log_info "Not probing the Prometheus TLS connection (PROMETHEUS_TLS_PROBE=false)."
        return 0
    fi
    wva_probe_prometheus_tls "$url" "$ca" "$sn"
}

# wva_probe_prometheus_tls connects to Prometheus URL $1 from a one-shot Pod in
# $WVA_NS the way the controller will: verifying the certificate against CA
# file $2 -- mounted from the controller Deployment's own volume, at its own
# path -- or the public roots when $2 is empty, for host $3 when set
# (PROMETHEUS_SERVER_NAME) else the URL's host. Never with an option that
# skips verification.
#
# Only a refused certificate (curl exit 51 or 60) stops the install. Anything
# else -- the Pod is Forbidden, its image cannot be pulled, a NetworkPolicy
# admits the controller but not this Pod, Prometheus wants a client
# certificate -- says the probe could not tell, which is not the same as the
# connection failing, and is a warning that names the reason.
wva_probe_prometheus_tls() {
    local url="$1" ca="$2" sn="$3" image pod deploy mount manifest rc=0 phase="" code="" out="" i hostport trust="the public roots"
    [ -z "$ca" ] || trust="the CA at $ca"
    image="${PROMETHEUS_TLS_PROBE_IMAGE:-quay.io/curl/curl:8.11.1}"
    pod="wva-prometheus-tls-probe"
    kc_capture get deployment wva-controller-manager -n "$WVA_NS" -o json || rc=$?
    if [ "$rc" -ne 0 ]; then
        log_warning "Could not probe the Prometheus TLS connection: cannot read deployment/wva-controller-manager in $WVA_NS (${KC_ERR:-kubectl exited $rc}), so the CA it mounts is unknown."
        return 0
    fi
    deploy="$KC_OUT"

    # The volume mount that holds the CA file: the longest mountPath it is
    # under, or the subPath mount at exactly its path.
    if [ -n "$ca" ]; then
        mount="$(printf '%s' "$deploy" | jq -c --arg ca "$ca" '
            .spec.template.spec.containers as $cs
            | (($cs | map(select(.name == "manager")) | .[0]) // $cs[0]).volumeMounts // []
            | map(select(. as $m | ($m.subPath // "") as $sp
                | if $sp != "" then $ca == $m.mountPath
                  else ($ca | startswith(($m.mountPath | rtrimstr("/")) + "/")) end))
            | sort_by(.mountPath | length) | last // empty' 2>/dev/null)" || mount=""
        if [ -z "$mount" ]; then
            log_warning "=================================================================="
            log_warning "PROMETHEUS_CA_CERT_PATH is $ca, but the controller mounts no volume that holds it."
            log_warning "  It will fail to read the CA and exit. Mount the CA first, then re-run (docs/reference/prometheus.md#tls-configuration)."
            log_warning "=================================================================="
            return 0
        fi
    fi

    hostport="${url#*://}"
    hostport="${hostport%%/*}"
    hostport="${hostport##*@}"
    case "$hostport" in *:*) ;; *) hostport="$hostport:443" ;; esac
    if ! manifest="$(printf '%s' "$deploy" | jq -c --arg pod "$pod" --arg img "$image" \
        --arg url "$url" --arg ca "$ca" --arg sn "$sn" --arg hp "$hostport" --argjson m "${mount:-null}" '
        .spec.template.spec as $t
        | ($hp | split(":") | last) as $port
        | (if $sn != "" then ($url | sub("^https://[^/]+"; "https://" + $sn + ":" + $port)) else $url end) as $target
        | {apiVersion: "v1", kind: "Pod",
           metadata: {name: $pod, labels: {"app.kubernetes.io/name": "wva-prometheus-tls-probe",
                                           "app.kubernetes.io/part-of": "wva"}},
           spec: {restartPolicy: "Never", activeDeadlineSeconds: 120,
                  automountServiceAccountToken: false, enableServiceLinks: false,
                  nodeSelector: ($t.nodeSelector // {}), tolerations: ($t.tolerations // []),
                  securityContext: (($t.securityContext // {}) + {runAsNonRoot: true, seccompProfile: {type: "RuntimeDefault"}}
                                    | if .runAsUser then . else .runAsUser = 65532 end),
                  volumes: (if $m then [($t.volumes // [])[] | select(.name == $m.name)] else [] end),
                  containers: [{name: "probe", image: $img,
                      command: (["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "15"]
                                + (if $ca != "" then ["--cacert", $ca] else [] end)
                                + (if $sn != "" then ["--connect-to", ($sn + ":" + $port + ":" + $hp)] else [] end)
                                + [$target]),
                      resources: {requests: {cpu: "10m", memory: "16Mi"}, limits: {cpu: "100m", memory: "64Mi"}},
                      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                                        capabilities: {drop: ["ALL"]}},
                      volumeMounts: (if $m then [$m + {readOnly: true}] else [] end)}]}}' 2>/dev/null)" \
        || [ -z "$manifest" ]; then
        log_warning "Could not probe the Prometheus TLS connection: building the probe Pod from deployment/wva-controller-manager failed."
        return 0
    fi

    kubectl delete pod "$pod" -n "$WVA_NS" --ignore-not-found --wait=true >/dev/null 2>&1 || true
    rc=0
    kc_capture create -n "$WVA_NS" -f - <<< "$manifest" || rc=$?
    if [ "$rc" -ne 0 ]; then
        log_warning "Could not probe the Prometheus TLS connection from inside the cluster: creating a Pod in $WVA_NS was refused (${KC_ERR:-kubectl exited $rc})."
        log_warning "  The controller's own connection is not checked until it starts. If it does not become ready, read its log for x509."
        return 0
    fi
    log_info "Probing the Prometheus TLS connection from inside the cluster (Pod $pod, $trust, no verification skipped)..."
    for i in $(seq 1 "${PROMETHEUS_TLS_PROBE_TIMEOUT:-90}"); do
        phase="$(kubectl get pod "$pod" -n "$WVA_NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        case "$phase" in Succeeded|Failed) break ;; esac
        if kubectl get pod "$pod" -n "$WVA_NS" \
            -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null \
            | grep -qE 'ErrImagePull|ImagePullBackOff|InvalidImageName'; then
            phase=ImagePull
            break
        fi
        sleep 1
    done
    code="$(kubectl get pod "$pod" -n "$WVA_NS" \
        -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null || true)"
    # curl's own error line when there is one (a refusal runs to several lines
    # of advice), else what -w printed: the HTTP status.
    out="$(kubectl logs "$pod" -n "$WVA_NS" 2>/dev/null || true)"
    if printf '%s\n' "$out" | grep -q '^curl: ('; then
        out="$(printf '%s\n' "$out" | grep -m1 '^curl: (')"
    else
        out="$(printf '%s\n' "$out" | tail -1)"
    fi
    kubectl delete pod "$pod" -n "$WVA_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true

    case "$phase:$code" in
        Succeeded:0)
            log_success "Prometheus TLS verified from inside the cluster: $url ($trust; HTTP ${out:-?})"
            return 0 ;;
        ImagePull:*)
            log_warning "Could not probe the Prometheus TLS connection: the probe image $image cannot be pulled here."
            log_warning "  Set PROMETHEUS_TLS_PROBE_IMAGE to a curl image this cluster can pull, or PROMETHEUS_TLS_PROBE=false to skip the probe."
            return 0 ;;
        Failed:51|Failed:60)
            WVA_PROM_CERT_NAMES="" WVA_PROM_CERT_SUGGEST=""
            wva_prometheus_cert_check "$url" "$sn" >/dev/null 2>&1 || true
            log_warning "=================================================================="
            log_warning "The controller will not be able to verify the Prometheus certificate at $url:"
            log_warning "  curl, from a Pod in $WVA_NS with $trust: $out"
            if printf '%s' "$out" | grep -qiE 'subject name|does not match|not match target host'; then
                wva_prometheus_cert_name_advice "$url"
            else
                log_warning "  The certificate is signed by a CA the controller is not given. The installer copies it from the"
                log_warning "  Prometheus TLS Secret when it may read it; a cluster admin's prerequisites phase makes that copy:"
                log_warning "    $(wva_make_cmd setup-prereqs)"
                log_warning "  or name it: PROMETHEUS_SECRET_NAME/PROMETHEUS_SECRET_NS, or PROMETHEUS_CA_FILE=<file>"
                log_warning "  (docs/reference/prometheus.md#prometheus-with-tls-eg-kube-prometheus-stack-on-a-managed-gpu-cloud)."
            fi
            log_warning "=================================================================="
            log_error "Prometheus at $url: the controller cannot verify its certificate (see above)."
            ;;
        Failed:6)
            log_warning "Prometheus at $url does not resolve from $WVA_NS: $out"
            log_error "Prometheus at $url: no such host in the cluster, so the controller cannot reach it. Pass PROMETHEUS_URL=<url> naming a Service that exists."
            ;;
        *)
            log_warning "Could not complete the Prometheus TLS probe (phase ${phase:-unknown}, curl exit ${code:-none}${out:+: $out})."
            log_warning "  Not a verdict on the certificate: a NetworkPolicy that admits only the controller, or a Prometheus that wants a client certificate, ends the probe the same way."
            return 0 ;;
    esac
}

# wva_restart_on_prometheus_change restarts the controller when a PROMETHEUS_*
# key of the manager config ($1, as written) differs from what it held before
# this install (WVA_LIVE_PROM_KEYS).
#
# The controller reads its config file once, at startup: the ConfigMap is
# mounted through subPath, which the kubelet never refreshes. Patching it changed
# nothing until the pod happened to restart, so a re-run that turned
# verification off, or set a CA path, left the running controller failing
# exactly as before while the install reported the fix. A config that could
# not be read before ("unknown"), or cannot be read now, is restarted too: the
# install cannot tell whether it changed. A new install ("absent") is not: its
# pod starts on the rendered config.
wva_restart_on_prometheus_change() {
    local cfg="$1"
    case "${WVA_LIVE_PROM_TLS:-}" in
        read)
            [ -z "$cfg" ] || [ "$(wva_prometheus_keys "$cfg")" != "$WVA_LIVE_PROM_KEYS" ] || return 0 ;;
        unknown) ;;
        *) return 0 ;;
    esac
    log_info "The Prometheus settings in wva-manager-config changed; restarting the controller, which reads them only at startup."
    if ! kubectl rollout restart deployment/wva-controller-manager -n "$WVA_NS"; then
        log_warning "Could not restart the controller. It keeps the old Prometheus settings until its next restart. Restart it with:"
        log_warning "  kubectl rollout restart deployment/wva-controller-manager -n $WVA_NS"
    fi
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    if [ -n "${WVA_IMAGE_DIGEST:-}" ]; then
        log_info "Using image: $WVA_IMAGE_REPO@$WVA_IMAGE_DIGEST"
    else
        log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    fi

    # First, before this function changes anything on the cluster: a policy the
    # new controller would reject stops the install here, with the reason
    # (wva_preflight_reserved_limiter_names). hack/check-limiter-declaration.sh
    # pins that this call stays ahead of every step that writes.
    wva_preflight_reserved_limiter_names

    # Select the Kustomize overlay by install SCOPE (WVA_SCOPE=cluster|namespace)
    # and platform. Both scopes work on both platforms; the default preserves the
    # historical inference. See wva_overlay_dir.
    local kustomize_overlay
    kustomize_overlay="$(wva_overlay_dir)"

    # Build a throw-away overlay that pins the image without modifying tracked files.
    local tmp_overlay
    tmp_overlay=$(mktemp -d)
    trap 'rm -rf "$tmp_overlay"' EXIT

    # Shared with the preflight and the undeploy, which render the SAME shape.
    # See wva_prepare_overlay_base.
    wva_prepare_overlay_base "$tmp_overlay"

    # Symlink prometheus-alerts component if needed
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "true" ]; then
        ln -s "$WVA_PROJECT/config/components/prometheus-alerts" "$tmp_overlay/prometheus-alerts"
    fi

    # NOTE: the FMA launcher PodMonitor is deliberately NOT wired in here. This
    # overlay renders into the CONTROLLER's namespace, and launcher pods live in
    # the workload namespace — which differs whenever WVA_WATCH_NS is set, and is
    # plural for a cluster-scoped install. It is applied per workload namespace
    # instead; see deploy_fma_launcher_podmonitor.

    # config/base/manager/kustomization.yaml transforms the base image name "controller"
    # to the published release image. The overlay must match the POST-transform name.
    local base_image
    base_image=$(grep 'newName:' "$WVA_PROJECT/config/base/manager/kustomization.yaml" | awk '{print $2}' | head -1)
    # A MOVING tag has to be re-pulled, or a redeploy silently runs old code.
    #
    # config/base/manager/deployment.yaml pins imagePullPolicy: IfNotPresent,
    # which is correct for a digest or a release tag and wrong for :main. A node
    # that cached :main once keeps it forever: measured on the shared GPU cluster, a controller
    # restarted on 2026-08-19 was still running an image built 2026-08-14, six
    # days and many commits stale. Two log lines that had been added in between
    # were simply absent from the binary, which reads exactly like a metrics
    # signal that never arrives -- and cost a long investigation to tell apart.
    #
    # Not forced to Always unconditionally: the kind e2e builds the image and
    # loads it into the cluster under this same :main tag, with no registry
    # behind it, so Always there would pull the REMOTE image over the one under
    # test. That path sets WVA_IMAGE_PULL_POLICY=IfNotPresent explicitly.
    local pull_policy="${WVA_IMAGE_PULL_POLICY:-}"
    [ -n "$pull_policy" ] || pull_policy="$(wva_image_pull_policy "$WVA_IMAGE_TAG" "${WVA_IMAGE_DIGEST:-}")"
    wva_warn_floating_image "$WVA_IMAGE_REPO" "$WVA_IMAGE_TAG" "${WVA_IMAGE_DIGEST:-}"

    cat > "$tmp_overlay/pull-policy-patch.yaml" <<EOF
- op: replace
  path: /spec/template/spec/containers/0/imagePullPolicy
  value: $pull_policy
EOF

    # A digest replaces the tag rather than joining it: kustomize renders
    # `digest:` as repo@sha256:..., which is what the node resolves.
    local image_pin="  newTag: \"$WVA_IMAGE_TAG\""
    [ -z "${WVA_IMAGE_DIGEST:-}" ] || image_pin="  digest: \"$WVA_IMAGE_DIGEST\""
    cat > "$tmp_overlay/kustomization.yaml" <<EOF
namespace: $WVA_NS
resources:
- ./base
images:
- name: $base_image
  newName: $WVA_IMAGE_REPO
$image_pin
EOF

    # Include Prometheus alerting rules component if requested
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "true" ]; then
        log_info "Including Prometheus alerting rules component..."
        printf 'components:\n' >> "$tmp_overlay/kustomization.yaml"
        printf -- '- ./prometheus-alerts\n' >> "$tmp_overlay/kustomization.yaml"
    fi

    # Give this install its own ClusterRoleBinding names.
    #
    # Every overlay applies these under FIXED names, and an apply replaces a
    # ClusterRoleBinding's subject list — so a second install anywhere on the
    # cluster repoints them at its own namespace and leaves the first controller's
    # ServiceAccount with no permissions. Silently: no error, no restart, no event,
    # just every API call failing afterwards.
    #
    # This was already done here, but only for OpenShift, on the theory that shared
    # clusters were an OpenShift problem. They are not: the same apply does the same
    # thing on any cluster, and it is reproducible on kind in one command. Suffixing
    # unconditionally makes concurrent installs merely unwise rather than
    # destructive — check_single_installation still stops them by default.
    #
    # Upgrades of an install that predates this keep working: the old un-suffixed
    # binding still names the same ServiceAccount, so nothing is lost while it
    # lingers, and `undeploy` removes both.
    wva_append_crb_name_patches "$tmp_overlay/kustomization.yaml" "$WVA_NS"
    if [ "$(wva_install_scope)" = "namespace" ]; then
        # Every managed namespace by name: one, or each of a list. The Kueue and
        # Namespace reads stay narrowed to exactly those (and the policy names).
        local managed_words managed_labels="" m
        managed_words="$(wva_watch_words)"
        for m in $managed_words; do managed_labels="$managed_labels $(wva_managed_policy_label "$m")"; done
        # shellcheck disable=SC2086 # one word per Namespace name
        wva_append_install_kueue_reader_scope_patch "$tmp_overlay/kustomization.yaml" "$WVA_NS" "$managed_words"
        # shellcheck disable=SC2086 # one word per Namespace name
        wva_append_policy_namespace_reader_scope_patch "$tmp_overlay/kustomization.yaml" "$WVA_NS" \
            $managed_words "${WVA_POLICY_NS:-}" $managed_labels
        if wva_kueue_reader_unneeded; then
            log_info "WVA_LIMITER=none and no policy this controller reads uses Kueue: the Kueue reader (ClusterRole wva-kueue-reader-role-$(wva_ns_suffix "$WVA_NS")) is not created"
            wva_append_kueue_reader_delete "$tmp_overlay/kustomization.yaml" "$WVA_NS"
        fi
    fi

    # Grant the Prometheus that will actually scrape the controller, not the one
    # the base names. See wva_append_metrics_reader_decision.
    if [ "${ENVIRONMENT:-}" != "openshift" ]; then
        wva_metrics_reader_binding "$tmp_overlay/kustomization.yaml" "$WVA_NS"
    fi

    # Appended here, not in the heredoc above: wva_append_crb_name_patches writes
    # its own `patches:` key, and a second one is a duplicate mapping key that
    # kustomize resolves by keeping the last -- which silently dropped this patch
    # and left a stale image running while the deploy reported success.
    grep -q '^patches:' "$tmp_overlay/kustomization.yaml" || printf 'patches:
' >> "$tmp_overlay/kustomization.yaml"
    cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- path: pull-policy-patch.yaml
  target:
    kind: Deployment
    labelSelector: control-plane=controller-manager
EOF

    # And the Prometheus URL, so the Deployment this apply creates never sees the
    # shipped default. Appended after the `patches:` key exists, for the same
    # reason the pull-policy patch is.
    #
    # The TLS settings the controller runs with NOW are read first: the render
    # replaces the whole config.yaml, and an upgrade must not turn verification
    # on for a Prometheus it was connecting to unverified. See
    # wva_read_live_prometheus_tls. main reads it before any phase runs; this
    # read is for a caller that did not.
    if [ "${WVA_APPLY_SCOPE:-all}" != "prereqs" ] && [ -z "${WVA_LIVE_PROM_TLS:-}" ]; then
        wva_read_live_prometheus_tls "$WVA_NS"
    fi
    wva_resolve_prometheus_url
    # The Prometheus CA, copied and mounted before the render, so the config and
    # the Deployment the first apply creates already verify with it.
    wva_sync_prometheus_ca
    wva_refuse_unknown_prometheus_tls
    wva_render_manager_config_patch "$tmp_overlay"
    wva_append_prometheus_ca_mount_patch "$tmp_overlay/kustomization.yaml"

    # Who may call the external scaler: KEDA's namespace only, unless opted out.
    wva_append_scaler_networkpolicy_patch "$tmp_overlay/kustomization.yaml"

    # How KEDA's calls are authenticated: WVA_SCALER_TLS provisions the
    # certificates and mounts them (deploy/lib/scaler_tls.sh). Before the apply,
    # so the first pod of a TLS install finds its Secrets.
    wva_scaler_tls_prepare "$tmp_overlay/kustomization.yaml"

    # Prune on INSTALL as well as uninstall. Waiting for an uninstall would leave
    # the grant in place on every cluster that already has it, since upgrading is
    # what people actually do -- and the whole reason the binding was dropped is
    # that it handed out access nobody needed.
    wva_delete_legacy_crbs "$WVA_NS"

    wva_append_replicas_patch "$tmp_overlay/kustomization.yaml" "$WVA_NS"

    # WVA_VERBOSITY sets the controller's --v; unset keeps the shipped default.
    wva_append_verbosity_patch "$tmp_overlay/kustomization.yaml"

    # WVA_WATCH_NS separates the namespace the controller MANAGES from the one it
    # RUNS IN.
    #
    # Without it a namespace-scoped controller manages its own namespace, so
    # bounding a tenant means running the controller inside the tenant's namespace —
    # where the tenant owns this Deployment, and can therefore edit its args, its
    # env, or its image. No setting carried by a Deployment its subject controls can
    # bind that subject. Put the controller in a namespace the admin owns and point
    # it at the tenant's, and the bound holds because the tenant cannot reach it.
    if [ -n "${WVA_WATCH_NS:-}" ]; then
        if [ "$(wva_install_scope)" != "namespace" ]; then
            log_error "WVA_WATCH_NS only applies to a namespace-scoped install (WVA_SCOPE=namespace). A cluster-scoped controller manages every namespace."
        fi
        local watched ns_rc
        for watched in $(wva_watch_words); do
            # Forbidden is not absent: a namespace owner may not read the
            # Namespace object of a namespace it does not own, and the admin
            # phase that granted the controller its Role there already found
            # it. Only NotFound refuses.
            ns_rc=0
            kc_capture get namespace "$watched" -o name || ns_rc=$?
            if [ "$ns_rc" -ne 0 ]; then
                if kc_not_found "$KC_ERR"; then
                    log_error "WVA_WATCH_NS: namespace $watched does not exist. Create the tenant namespace before installing."
                fi
                log_info "WVA_WATCH_NS: cannot read namespace $watched (${KC_ERR:-kubectl exited $ns_rc}); the controller's Role there is the admin phase's, checked with the other prerequisites."
            fi
        done
        if wva_watch_is_multi; then
            log_info "Controller runs in $WVA_NS and manages $(wva_watch_words | sed 's/ /, /g')"
        elif [ "$WVA_WATCH_NS" = "$WVA_NS" ]; then
            log_warning "WVA_WATCH_NS equals WVA_NS: the controller runs in the namespace it manages. If that namespace belongs to a tenant, they own this Deployment and can lift any limit set on them."
        else
            log_info "Controller runs in $WVA_NS and manages $WVA_WATCH_NS"
        fi
        # Strategic merge, which matches env entries by NAME. A JSON-patch index
        # into the env array would silently rewrite whichever variable happened to
        # sit at that position the day someone added one above it.
        #
        # The explicit `target:` is required, not decoration. Without one, kustomize
        # derives the target from the patch's own metadata — including a namespace
        # the patch does not carry — while the overlay's namespace transformer has
        # already stamped one on the Deployment. The two never match, and kustomize
        # fails the whole build with "no matches for Id
        # Deployment.v1.apps/wva-controller-manager.[noNs]", so the install of the
        # one configuration that puts the controller beyond a tenant's reach was the
        # one configuration that could not be installed.
        cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- patch: |-
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: wva-controller-manager
    spec:
      template:
        spec:
          containers:
          - name: manager
            env:
            - name: WVA_WATCH_NAMESPACE
              value: "${WVA_WATCH_NS}"
  target:
    kind: Deployment
    name: wva-controller-manager
EOF
    fi


    # WVA_APPLY_SCOPE splits one overlay across the two install phases:
    #
    #   prereqs     only the admin-owned kinds (see WVA_PREREQ_KINDS)
    #   controller  everything else — what a namespace admin may create once the
    #               prereqs exist
    #   all         both, which is the single-command install and the default
    #
    # Rendering once and filtering, rather than maintaining two overlays, is what
    # keeps the phases from drifting apart: a resource added to the base lands in
    # exactly one phase, decided by its kind, with nothing to keep in sync.
    # Before the apply, and only for the phases that carry the bindings.
    case "${WVA_APPLY_SCOPE:-all}" in
        all|prereqs) wva_repair_immutable_rolerefs ;;
    esac

    case "${WVA_APPLY_SCOPE:-all}" in
        all|prereqs)
            wva_apply_watch_namespace_rbac "$tmp_overlay"
            wva_apply_pod_list_grant
            ;;
    esac

    # Before the apply that takes `patch` on pods out of the ClusterRole: an
    # earlier install's warm pools and utilization-share marks still need it,
    # and nothing else would grant it again (lib/pod_writer.sh). A
    # namespace-scoped controller keeps `patch` in its own Role.
    case "${WVA_APPLY_SCOPE:-all}" in
        all|prereqs)
            if [ "$(wva_install_scope)" = "cluster" ]; then
                wva_regrant_pod_writers "$WVA_NS"
            fi
            ;;
    esac

    # A private registry needs the pull secret on the ServiceAccount, not on the
    # Deployment: patching the SA covers the controller and anything else that
    # runs as it, and survives a re-apply of the Deployment from the overlay.
    # Applied before the Deployment so the first pod schedules with it rather
    # than starting in ImagePullBackOff and recovering later.
    # A private registry needs the pull secret on the ServiceAccount rather than
    # the Deployment: the SA covers the controller and anything else running as
    # it, and survives a re-apply of the Deployment from the overlay. Applied
    # after the objects exist, below — this only validates it now, so a missing
    # secret is an error before anything is created rather than an
    # ImagePullBackOff afterwards.
    if [ -n "${WVA_IMAGE_PULL_SECRET:-}" ] && [ "${WVA_APPLY_SCOPE:-all}" != "prereqs" ]; then
        kubectl get secret "$WVA_IMAGE_PULL_SECRET" -n "$WVA_NS" >/dev/null 2>&1 || \
            log_error "WVA_IMAGE_PULL_SECRET=$WVA_IMAGE_PULL_SECRET does not exist in $WVA_NS. Create it first:
    kubectl create secret docker-registry $WVA_IMAGE_PULL_SECRET -n $WVA_NS \\
        --docker-server=<registry> --docker-username=<user> --docker-password=<token>"
    fi

    # The per-model policy entries this apply is about to DELETE, saved first.
    #
    # The shipped scaling-policy ConfigMap carries one key, `default`. An
    # operator adds more -- a per-model or per-namespace entry, which
    # config/base/manager/scaling-policy-configmap.yaml documents in its own
    # header -- and `kubectl apply -k` then deletes every one of them, because
    # apply's three-way merge removes keys that are in last-applied-configuration
    # and absent from the new manifest.
    #
    # Measured on kind: a ConfigMap holding `batch`, `default` and `interactive`
    # came back holding `default` alone, from a plain `make deploy-wva` with no
    # limiter involved at all -- so every install and every UPGRADE silently
    # discarded them. Restored below, after the apply.
    local preserved_policy=""
    if [ "${WVA_APPLY_SCOPE:-all}" != "prereqs" ]; then
        preserved_policy="$(kubectl get configmap wva-scaling-policy-config -n "$WVA_NS" -o json 2>/dev/null \
            | jq -c 'if .data then {data: (.data | del(.default))} else empty end' 2>/dev/null || true)"
        # `{"data":{}}` is "nothing but default", which needs no restoring.
        [ "$preserved_policy" = '{"data":{}}' ] && preserved_policy=""

        # And the limiter the live `default` declares, which the same apply resets
        # to the shipped none. Kept below when WVA_LIMITER is not given -- see
        # wva_live_limiters. Unreadable with WVA_LIMITER unset is a refusal: the
        # install cannot tell whether it is about to unbound a bounded controller.
        local live_rc=0
        wva_live_limiters "$WVA_NS" || live_rc=$?
        if [ "$live_rc" -ne 0 ] && [ -z "${WVA_LIMITER:-}" ]; then
            log_error "Cannot read the limiter this controller runs with now (${WVA_LIVE_LIMITERS_ERR:-wva-scaling-policy-config in $WVA_NS is unreadable}), and this apply resets it. Re-run naming it: WVA_LIMITER=gpu-inventory|quota to declare one, or WVA_LIMITER=none to run without."
        fi
    fi

    log_info "Applying Kustomize overlay: $kustomize_overlay (scope: ${WVA_APPLY_SCOPE:-all})"
    case "${WVA_APPLY_SCOPE:-all}" in
        all)
            kubectl apply -k "$tmp_overlay"
            ;;
        prereqs)
            kubectl kustomize "$tmp_overlay" \
                | yq "$(wva_prereq_kind_filter select)" \
                | kubectl apply -f -
            ;;
        controller)
            kubectl kustomize "$tmp_overlay" \
                | yq "$(wva_prereq_kind_filter exclude)" \
                | kubectl apply -f -
            ;;
        *)
            log_error "WVA_APPLY_SCOPE must be all, prereqs or controller (got '${WVA_APPLY_SCOPE}')"
            ;;
    esac

    # The ServiceMonitor references wva-metrics-scraper-token now; the token the
    # controller's own ServiceAccount used to be scraped with goes.
    case "${WVA_APPLY_SCOPE:-all}" in
        all|prereqs) wva_delete_legacy_controller_token "$WVA_NS" ;;
    esac

    # Put the operator's own entries back. A merge patch names only the keys it
    # carries, so `default` -- which the apply has just refreshed from the
    # shipped manifest -- is left exactly as applied.
    if [ -n "$preserved_policy" ]; then
        local restored_keys
        restored_keys="$(printf '%s' "$preserved_policy" | jq -r '.data | keys | join(", ")' 2>/dev/null || true)"
        if kubectl patch configmap wva-scaling-policy-config -n "$WVA_NS" --type=merge \
                -p "$preserved_policy" >/dev/null 2>&1; then
            log_info "Kept the policy entries this apply would have pruned: ${restored_keys}"
        else
            log_warning "Could not restore the policy entries the apply removed: ${restored_keys}. Re-add them before relying on any per-model policy."
        fi
    fi

    # The ServiceAccount exists now, so the pull secret can go on it. The pod may
    # already be starting; a Deployment restart makes the next pod pick it up
    # rather than waiting out an ImagePullBackOff.
    if [ -n "${WVA_IMAGE_PULL_SECRET:-}" ] && [ "${WVA_APPLY_SCOPE:-all}" != "prereqs" ]; then
        log_info "Image pull secret: $WVA_IMAGE_PULL_SECRET (on ServiceAccount wva-controller-manager)"
        kubectl patch serviceaccount wva-controller-manager -n "$WVA_NS" \
            -p "{\"imagePullSecrets\":[{\"name\":\"${WVA_IMAGE_PULL_SECRET}\"}]}" >/dev/null || \
            log_warning "  could not patch the ServiceAccount — the controller will not be able to pull from a private registry"
        kubectl rollout restart deployment/wva-controller-manager -n "$WVA_NS" >/dev/null 2>&1 || true
    fi

    # Everything below this point patches namespaced ConfigMaps that the
    # controller phase owns and the prereqs phase has not created yet.
    if [ "${WVA_APPLY_SCOPE:-all}" = "prereqs" ]; then
        log_success "Admin-owned resources applied for $WVA_NS: $(printf '%s ' "${WVA_PREREQ_KINDS[@]}")"
        return 0
    fi

    # Clean up a previously-deployed PrometheusRule when alerting rules are disabled.
    # The `get` guard keeps the common path quiet (only logs/acts when a rule actually
    # exists) and tolerates the PrometheusRule CRD being absent — `--ignore-not-found`
    # only covers a missing object, not a missing resource type.
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "false" ] && \
        kubectl get prometheusrule controller-manager-alerts -n "$WVA_NS" &> /dev/null; then
        log_info "DEPLOY_ALERTING_RULES=false - removing existing PrometheusRule controller-manager-alerts"
        kubectl delete prometheusrule controller-manager-alerts -n "$WVA_NS" --ignore-not-found=true
    fi

# wva_reconcile_prometheus_scheme makes the rest of the Prometheus settings agree
# with the URL's scheme.
#
# WVA refuses a plain-HTTP Prometheus unless told to allow it, refuses to send a
# bearer token over one, and refuses TLS settings alongside one — three separate
# checks, all of them right. The installer wrote the URL and left the other three
# as they were, so pointing at a plain-HTTP Prometheus produced a ConfigMap the
# controller would not start on.
#
# It did not fail the install, which is the part that made it dangerous: the pod
# had already started on the previous value and kept running on it. The install
# reported success, the controller ran for days, and the CrashLoopBackOff arrived
# with the next unrelated restart — a node drain, an upgrade, an eviction — long
# after anyone would connect it to an install-time setting.
#
# So the settings move together with the scheme, or not at all.
wva_reconcile_prometheus_scheme() {
    local url="$1"
    case "$url" in
        http://*) : ;;
        *) return 0 ;;
    esac

    log_warning "This Prometheus speaks plain HTTP, so WVA will read metrics over an unencrypted connection."
    log_warning "  Enabling PROMETHEUS_ALLOW_HTTP and dropping the TLS settings, which WVA rejects alongside an http:// URL."
    log_warning "  Pass PROMETHEUS_URL=https://<host>:<port> to use a TLS endpoint instead."
    patch_manager_config '.PROMETHEUS_ALLOW_HTTP = "true"
        | .PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "false"
        | del(.PROMETHEUS_CA_CERT_PATH)'

    # The token path is Deployment env, and env beats the ConfigMap: leaving it set
    # trips "refusing to send bearer token authentication over plain HTTP" even
    # once the ConfigMap agrees. Emptied by NAME through a strategic merge, so it
    # does not depend on where in the list it happens to sit.
    kubectl patch deployment wva-controller-manager -n "$WVA_NS" --type=strategic -p '{
        "spec":{"template":{"spec":{"containers":[{"name":"manager",
        "env":[{"name":"PROMETHEUS_TOKEN_PATH","value":""}]}]}}}}' > /dev/null 2>&1 || true
}

    # Point the controller at THIS cluster's Prometheus.
    #
    # PROMETHEUS_URL used to be accepted, logged, and then dropped: the shipped
    # ConfigMap hardcodes a kube-prometheus-stack address in the monitoring
    # namespace and nothing ever overwrote it. Installing against a Prometheus this
    # script did not deploy therefore produced a controller that could not reach
    # one — and WVA exits on that ("CRITICAL: Failed to connect to Prometheus"), so
    # it presented as CrashLoopBackOff, reading like a cluster fault rather than a
    # setting that was silently ignored.
    # Nobody set one: the value in PROMETHEUS_URL is the platform default, which
    # names the Prometheus this installer would deploy. On a cluster that already
    # has its own, that is wrong, and WVA exits on a Prometheus it cannot reach —
    # arriving as CrashLoopBackOff, which reads like a broken image rather than a
    # setting nobody was asked for. So look before falling back to it.
    #
    # Resolved BEFORE the overlay was applied, not here -- see
    # wva_resolve_prometheus_url, called from the overlay build above. This is
    # kept as the write path: the value is already rendered into the ConfigMap the
    # apply created, and re-applying it here is what fixes an install whose
    # ConfigMap predates that rendering.
    wva_write_prometheus_settings

    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        log_info "Enabling scale-to-zero in WVA ConfigMap (ENABLE_SCALE_TO_ZERO=true)..."
        patch_manager_config '.WVA_SCALE_TO_ZERO = "true"'
    fi

    # Declare a GPU limiter, if asked.
    #
    # The shipped config declares NONE, deliberately: a GPU-aware optimizer can
    # only allocate to a variant whose accelerator it resolves, so turning it on
    # by default would silently freeze every workload that sets no GPU
    # nodeSelector. It is an install-time CHOICE for a fleet that is ready for it —
    # see docs/concepts/gpu-capacity-accounting.md.
    #
    # Patches the "default" entry rather than shipping a second copy of it: the
    # entry carries every other default too, and a duplicate would drift.
    #
    # WVA_LIMITER is a SET (gpu-inventory,quota): every limiter in it is built
    # and a decision must fit all of them.
    local limiter_set=""
    if [ -n "${WVA_LIMITER:-}" ]; then
        limiter_set="$(wva_limiter_set)" || exit 1
    fi
    case "$limiter_set" in
        '')
            # Not given: keep what the live policy declared before the apply reset
            # it. An upgrade is not a decision to unbound a controller -- least of
            # all one an admin bounded with enable-physical-limiter, whose owner
            # may not be the person upgrading it.
            if [ -n "${WVA_LIVE_LIMITERS:-}" ]; then
                wva_keep_live_limiters "$WVA_NS" "$WVA_LIVE_LIMITERS"
            fi
            ;;
        none)
            if [ -n "${WVA_LIVE_LIMITERS:-}" ]; then
                log_warning "WVA_LIMITER=none: the limiter this controller had is REMOVED, and scaling is now UNBOUNDED. It was:"
                printf '%s\n' "$WVA_LIVE_LIMITERS" | yq -P '.' 2>/dev/null | sed 's/^/    /' >&2 || true
            fi
            # Nothing to do, and that is MEASURED rather than assumed. A branch
            # here removed a previously declared limiter, on the theory that
            # re-installing with the default left one in force. It does not: the
            # overlay apply 130 lines above re-applies wva-scaling-policy-config
            # from the shipped manifest, which resets data.default -- limiters
            # and all -- before this case is ever reached. Confirmed on kind:
            # declare quota, re-install with none, `limiters:` count goes 1 -> 0
            # with this branch doing nothing.
            #
            # The report that motivated it came from a harness that lifted this
            # block out of the installer and never ran the apply. Code on the
            # DEFAULT path, which every install and upgrade takes, is not the
            # place to carry a fix for a failure the path does not have -- and it
            # brought its own: it fired on `limiters: []` (the documented way to
            # declare NO limiter), wrote `default: "{}"`, and aborted the install
            # on a policy it could not parse.
            ;;
        *)
            log_info "Declaring the limiters '${limiter_set}' in the scaling-policy ConfigMap ..."
            local policy_cm current_default updated_default
            # The budget is validated in the PREREQS phase, before any of this
            # ran -- see check_permissions. This is the second line of defence,
            # for a caller that reached here another way.
            # `|| true` because a no-match is grep exit 1, which pipefail turns into
            # a failed assignment and set -e turns into an exit — before the
            # log_error below can say which ConfigMap is missing.
            policy_cm="$(kubectl get configmap -n "$WVA_NS" -o name 2>/dev/null \
                | grep -E "configmap/(wva-)?scaling-policy-config$" | head -1 | cut -d/ -f2 || true)"
            if [ -z "$policy_cm" ]; then
                log_error "No scaling-policy ConfigMap found in $WVA_NS"
            fi
            current_default=$(kubectl get configmap "$policy_cm" -n "$WVA_NS" -o jsonpath='{.data.default}')
            if [ -z "$current_default" ]; then
                log_error "ConfigMap $policy_cm has no data['default'] entry"
            fi
            # Idempotent, and it REPLACES the whole list: WVA_LIMITER is this
            # install's full limiter set, so a re-run with a different set must
            # not leave an entry from the old one behind.
            # One call, no status for a caller to forget: policy_declared_limiters
            # composes in THIS shell, so a refusal ends the install rather than
            # handing back an empty string to patch over the policy.
            # The apply above reset the policy, so the Kueue pilot settings of
            # the quota entry this replaces (borrowNamespaces, countBorrowable)
            # are kept from the limiters read before it (policy_with_limiters).
            LIMITERS_KEEP_FROM="${WVA_LIVE_LIMITERS:-}" policy_declared_limiters "$limiter_set" "$current_default"
            updated_default="$POLICY_DECLARED"
            kubectl patch configmap "$policy_cm" -n "$WVA_NS" --type=merge \
                -p "$(jq -n --arg d "$updated_default" '{data:{"default":$d}}')"
            log_warning "Scaling is now bounded by every limiter in '${limiter_set}' (declared in ${WVA_NS}/${policy_cm}):"
            printf '%s\n' "$LIMITER_ENTRY_DECLARED" | sed 's/^/    /' >&2
            # The controller is the only thing that can confirm it. It validates
            # the entry on read and, when it rejects one, discards the ENTIRE
            # `default` policy and falls back to no limiter -- at ERROR in its log
            # and nowhere else. Say where to look, because "the install said
            # bounded" is exactly the belief that shipped unbounded before.
            log_info "Confirm the controller accepted it (a rejected entry drops the whole policy AND the limiter):"
            log_info "    kubectl logs -n ${WVA_NS} deploy/wva-controller-manager | grep -E 'GPU limiter constructed|Invalid saturation scaling'"
            # A GPU-aware optimizer allocates out of per-accelerator pools, so a
            # workload whose accelerator cannot be resolved gets no budget and stops
            # scaling up — silently. Say so at install, and say how to check, because
            # the symptom (a workload that simply never grows) looks like anything.
            # THE QUOTA PATH'S OWN HAZARD, and the installer knew the answer
            # three lines below without ever comparing. A budget is keyed by
            # accelerator NAME, and a name matching no node is not an error
            # anywhere: the entry validates, the controller accepts it, the
            # "GPU limiter constructed" line prints -- and every accelerator the
            # cluster actually has is absent from the map, which
            # QuotaForNamespace reads as zero. Measured: `WVA_QUOTAS='H20=8'` on
            # a cluster of H200s and A100s installs clean and stops all scaling.
            # Shared with the cluster-policy path -- see
            # warn_unadvertised_accelerators. That path publishes to every
            # controller on the cluster and had no such check at all.
            if limiter_set_has "$limiter_set" quota; then
                warn_unadvertised_accelerators
            fi
            if limiter_set_has "$limiter_set" physical; then
                local gpu_products
                # Space-separated on one line, then split — a {"\n"} inside the
                # jsonpath is fragile to quote through the layers this runs under.
                gpu_products=$(kubectl get nodes \
                    -o jsonpath='{.items[*].metadata.labels.nvidia\.com/gpu\.product}' 2>/dev/null \
                    | tr ' ' '\n' | grep -v '^$' | sort -u | wc -l || true)
                if [ "${gpu_products:-0}" -gt 1 ]; then
                    log_warning "This cluster advertises ${gpu_products} different GPU products. Every managed workload MUST pin one with a nodeSelector/nodeAffinity GPU product key — an unpinned workload is charged to no accelerator pool, receives no budget, and will not scale up. Check with:"
                    log_warning "    kubectl get nodes -L nvidia.com/gpu.product"
                    log_warning "    kubectl get events -A --field-selector reason=AcceleratorNotResolved"
                else
                    log_info "Single GPU product detected: an unpinned workload is deduced onto it, so no nodeSelector is required."
                fi
            fi
            ;;
    esac

    # After every patch to the manager config, so the new pod reads all of them.
    # See wva_restart_on_prometheus_change.
    wva_restart_on_prometheus_change "$(kubectl get configmap wva-manager-config -n "$WVA_NS" \
        -o jsonpath='{.data.config\.yaml}' 2>/dev/null || true)"

    # Wait for WVA to be ready.
    #
    # `kubectl wait --for=condition=Ready pod -l ...` asks about ANY pod carrying
    # the label — and during a rolling update the PREVIOUS pod is still there and
    # still ready. An upgrade whose new pod cannot start therefore satisfied this
    # instantly. Ask the rollout instead: it is the only check that requires the
    # UPDATED replicas to be the ready ones.
    log_info "Waiting for WVA controller to be ready..."
    local wva_deploy
    wva_deploy=$(kubectl get deployment -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
        -o name 2>/dev/null | head -1)
    # WVA_CONTROLLER_ROLLED_OUT records the result: wva_scaler_tls_finish moves
    # ScaledObjects to the scaler's new mode only once the new pod serves it.
    WVA_CONTROLLER_ROLLED_OUT=false
    if [ -z "$wva_deploy" ]; then
        log_warning "No WVA Deployment found in $WVA_NS to wait for"
    elif wva_controller_rollout_ready "$wva_deploy" "$WVA_NS" 60s; then
        WVA_CONTROLLER_ROLLED_OUT=true
    elif [ "$WVA_CONTROLLER_NOT_READY" = "scaled-to-zero" ]; then
        log_warning "WVA controller $wva_deploy in $WVA_NS is at 0 replicas: no controller pod runs"
    else
        log_warning "WVA controller is not ready yet - check 'kubectl get pods -n $WVA_NS'"
    fi

    log_success "WVA deployment complete"
}

# Shared namespace creation loop for deploy/*/install.sh environment plugins.
# Platform adapter provides materialize_namespace(ns), then calls this helper.
# patch_manager_config applies one yq expression to data["config.yaml"] of the
# manager ConfigMap — the file the controller actually reads, mounted at
# /etc/wva/config.yaml.
#
# Patches that one key rather than replacing the object, so nothing echoes back
# server-managed metadata (resourceVersion, managedFields). yq rather than sed
# because the edit must be idempotent and survive quoting and whitespace variance:
# sed would silently fail to match an unquoted or single-quoted value and leave the
# setting untouched, which is the failure mode hardest to notice.
patch_manager_config() {
    local expr="$1" current updated
    current=$(kubectl get configmap wva-manager-config -n "$WVA_NS" \
        -o jsonpath='{.data.config\.yaml}')
    if [ -z "$current" ]; then
        log_error "ConfigMap wva-manager-config has no data['config.yaml'] key"
    fi
    updated=$(echo "$current" | yq "$expr")
    kubectl patch configmap wva-manager-config -n "$WVA_NS" --type=merge \
        -p "$(jq -n --arg cfg "$updated" '{data:{"config.yaml":$cfg}}')"
}

create_namespaces_shared_loop() {
    log_info "Creating namespaces..."

    # Only namespaces this run actually puts something in. Creating the others
    # leaves empty namespaces behind on a cluster that already had its own —
    # llm-d's workloads are somewhere else, and its Prometheus is not ours to
    # place — and an empty llm-d namespace is worse than absent: it looks like the
    # place to deploy models, and WVA is not watching it.
    local wanted="$WVA_NS"
    # Ask the same question the monitoring step will ask, rather than reading the
    # flag: DEPLOY_PROMETHEUS=true only MEANS "deploy one" when the cluster has no
    # Prometheus of its own. Reading the flag alone created an empty monitoring
    # namespace on every existing-llm-d install — the exact litter the comment
    # above is about — and made DEPLOY_PROMETHEUS=false something users had to
    # pass by hand to avoid it.
    # The monitoring namespace is cluster-scoped to create, so gate it on WHO is
    # running, not on what WVA will manage. Those are different questions: scope
    # says which workloads the controller watches, phase says whether this command
    # is the cluster-admin half or the tenant half.
    #
    # The tenant half (INSTALL_PHASE=wva) must not try. A namespace admin is
    # Forbidden from reading the Prometheus CRD, so foreign_prometheus() reports
    # "none found", and the install died creating a namespace it was never allowed
    # to create — after `--check` had said permissions were sufficient.
    #
    # But gating on scope alone was too strict the other way: `make setup-prereqs`
    # IS the cluster-admin phase, and in namespace scope it skipped the namespace
    # and then deployed Prometheus into it anyway. Reproduced on kind: exit 2 after
    # "Creating Kubernetes secret for Prometheus TLS", with no error printed.
    if [ "${DEPLOY_PROMETHEUS:-true}" = "true" ] \
        && [ "${INSTALL_PHASE:-all}" != "wva" ] \
        && [ -z "$(foreign_prometheus)" ]; then
        wanted="$wanted $MONITORING_NAMESPACE"
    fi
    [ "${DEPLOY_LLMD_NS:-false}" = "true" ] && wanted="$wanted $NAMESPACE"

    for ns in $wanted; do
        local ns_exists=false
        local ns_terminating=false

        if kubectl get namespace $ns &> /dev/null; then
            ns_exists=true
            local ns_status
            ns_status=$(kubectl get namespace $ns -o jsonpath='{.status.phase}' 2>/dev/null)
            if [ "$ns_status" = "Terminating" ]; then
                ns_terminating=true
            fi
        fi

        if [ "$ns_exists" = true ] && [ "$ns_terminating" = false ]; then
            log_info "Namespace $ns already exists"
            continue
        elif [ "$ns_terminating" = true ]; then
            log_info "Namespace $ns is terminating, forcing deletion..."
            kubectl get namespace $ns -o json | \
                jq '.spec.finalizers = []' | \
                kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null || true
            kubectl wait --for=delete namespace/$ns --timeout=120s 2>/dev/null || true
        fi

        materialize_namespace "$ns"
        log_success "Namespace $ns created"
    done
}

delete_namespaces_kube_like() {
    log_info "Deleting namespaces..."

    # Only namespaces THIS install created, and only ones nothing else lives in.
    #
    # NAMESPACE needs its own opt-in rather than mirroring DEPLOY_LLMD_NS, because
    # that flag describes the INSTALL and an uninstall never restates it —
    # `make undeploy-wva` forwards only WVA_NS and WVA_SCOPE, so the flag would
    # read as its default (true) and delete anyway. It holds the model servers;
    # deleting it is never something to infer.
    #
    # The same protection has to cover WVA_NS, because in the documented tenant
    # install they are the SAME namespace: a namespace-scoped controller is
    # deliberately placed in the namespace holding the model servers, so
    # `WVA_NS = NAMESPACE`. Reached through the WVA_NS slot that namespace was
    # deleted outright — every Deployment, Secret and PVC in it — while the
    # NAMESPACE slot right beside it refused to. One uninstall flag, two paths to
    # the same namespace, and only one of them guarded.
    #
    # MONITORING_NAMESPACE is shared by every WVA on the cluster and carries a
    # fixed, unsuffixed name, so it takes UNDEPLOY_SHARED — the same gate the rest
    # of the uninstall uses for shared things. DEPLOY_PROMETHEUS says what a fresh
    # install WOULD deploy, never whether this one created what is there.
    local ns seen=""
    for ns in $NAMESPACE $WVA_NS $MONITORING_NAMESPACE; do
        [ -n "$ns" ] || continue
        case " $seen " in *" $ns "*) continue ;; esac
        seen="$seen $ns"
        kubectl get namespace "$ns" &> /dev/null || continue

        local skip=""
        if [ "$ns" = "$WVA_NS" ] && [ "$DEPLOY_WVA" = "false" ]; then
            skip="this install did not create it"
        elif [ "$ns" = "$NAMESPACE" ] || [ "$ns" = "${WVA_WATCH_NS:-}" ]; then
            [ "${DELETE_LLMD_NS:-false}" = "true" ] ||                 skip="it holds the model servers (DELETE_LLMD_NS=true to remove it anyway)"
        elif [ "$ns" = "$MONITORING_NAMESPACE" ]; then
            if [ "$DEPLOY_PROMETHEUS" = "false" ]; then
                skip="this install did not create it"
            elif [ "${UNDEPLOY_SHARED:-false}" != "true" ]; then
                skip="it is shared with every other WVA on this cluster (UNDEPLOY_SHARED=true to remove it anyway)"
            fi
        fi

        if [ -n "$skip" ]; then
            log_info "Keeping namespace $ns — $skip"
            continue
        fi
        log_info "Deleting namespace $ns..."
        kubectl delete namespace "$ns" 2>/dev/null ||             log_warning "Failed to delete namespace $ns"
    done

    log_success "Namespaces deleted"
}

# Shared WVA prerequisites for Kubernetes-like environments.
# Optional:
deploy_wva_prerequisites_kube_like() {
    log_info "Deploying Workload-Variant-Autoscaler prerequisites for Kubernetes..."

    # InferencePool CRDs must exist before the controller starts or its initial
    # watch fails. install-epp.sh installs them again later (idempotent).
    install_inference_crds

    # Extract the Prometheus CA, if this cluster's Prometheus is one we recognise.
    #
    # Guarded, because this script runs under `set -e -o pipefail`: on a cluster
    # whose Prometheus predates the install — different namespace, different secret
    # name, or plain HTTP — the unguarded pipe aborted the whole installation over
    # a certificate that is optional to begin with.
    if kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" &> /dev/null; then
        log_info "Extracting Prometheus TLS certificate"
        kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" \
            -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"
    else
        log_warning "No secret $PROMETHEUS_SECRET_NAME in $MONITORING_NAMESPACE — skipping CA extraction. Expected when using a Prometheus this install did not deploy. Point PROMETHEUS_SECRET_NAME/PROMETHEUS_SECRET_NS at yours, or set PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true to connect without verifying it."
    fi

    # LeaderWorkerSet (WVA dependency; see upstream chart / #910).
    if [ "${DEPLOY_LWS:-true}" = "true" ]; then
        if kubectl get crd leaderworkersets.leaderworkerset.x-k8s.io &> /dev/null; then
            log_info "LeaderWorkerSet CRD already installed, skipping LWS deployment"
        else
            log_info "Installing LeaderWorkerSet version ${LWS_CHART_VERSION} into ${LWS_NAMESPACE} namespace"
            helm upgrade -i lws oci://registry.k8s.io/lws/charts/lws \
                --version="${LWS_CHART_VERSION}" \
                --namespace "${LWS_NAMESPACE}" \
                --create-namespace \
                --wait --timeout 300s
        fi
    else
        log_info "Skipping LeaderWorkerSet installation (DEPLOY_LWS=false)"
    fi

    log_success "WVA prerequisites complete"
}

# OpenShift-specific CA extraction used by deploy/openshift/install.sh.
extract_openshift_prometheus_ca() {
    # Extract OpenShift Service CA certificate for Thanos verification
    # Note: For OpenShift service certificates, we need the Service CA that signed the server cert,
    # not the server certificate itself. The server cert is in thanos-querier-tls, but we need the CA.
    log_info "Extracting OpenShift Service CA certificate for Thanos verification"

    # Method 1: Extract Service CA from openshift-service-ca.crt ConfigMap (preferred)
    # This is the actual CA certificate that signs OpenShift service certificates
    if kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
        log_info "Extracting Service CA from openshift-service-ca.crt ConfigMap"
        kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-service-ca.crt ConfigMap"
        fi
    fi

    # Method 2: Extract Service CA from openshift-config namespace
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_info "Trying to extract Service CA from openshift-config namespace"
        kubectl get configmap openshift-service-ca -n openshift-config -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-config namespace"
        fi
    fi

    # Method 3: Fallback to thanos-querier-tls secret
    # Note: This extracts the server certificate, which may work if the cert chain includes the CA
    # but it's not ideal - we should use the Service CA instead.
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_warning "Service CA not found, falling back to server certificate from thanos-querier-tls"
        log_warning "This may cause TLS verification issues - Service CA is preferred"
        if kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
            log_info "Extracting certificate from thanos-querier-tls secret"
            kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"
            if [ -s "$PROM_CA_CERT_PATH" ]; then
                log_success "Extracted certificate from thanos-querier-tls secret"
            fi
        fi
    fi

    # Verify we have a valid certificate
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_error "Failed to extract OpenShift Service CA certificate"
        log_error "Tried: openshift-service-ca.crt ConfigMap, openshift-config ConfigMap, and thanos-querier-tls secret"
        exit 1
    fi

    # Verify the certificate is valid PEM format
    if ! openssl x509 -in "$PROM_CA_CERT_PATH" -text -noout &> /dev/null; then
        log_warning "Certificate file may not be in valid PEM format, but continuing..."
        log_warning "If TLS errors occur, verify the certificate format is correct"
    else
        # Log certificate details for debugging
        local cert_subject
        cert_subject=$(openssl x509 -in "$PROM_CA_CERT_PATH" -noout -subject 2>/dev/null | sed 's/subject=//' || echo "unknown")
        log_info "Certificate subject: $cert_subject"
    fi
}


# WVA_VERBOSITY_MAX is the most verbose level the controller logs at:
# logging.TRACE in internal/logging/logger.go. Above it --v changes nothing.
WVA_VERBOSITY_MAX=5

# wva_append_verbosity_patch adds --v=WVA_VERBOSITY to the manager container's
# args in the throw-away overlay's kustomization.yaml $1. Unset writes nothing,
# and the controller runs at its shipped --v=2. The per-replica capacity lines
# (k2-decision, replica-capacity-decision, derived-mu) need 3, which the
# benchmark install passes. WVA_VERBOSITY must be an integer from 0 to
# WVA_VERBOSITY_MAX; anything else is refused.
wva_append_verbosity_patch() {
    local kustomization="$1"
    [ -n "${WVA_VERBOSITY:-}" ] || return 0
    case "$WVA_VERBOSITY" in
        *[!0-9]*|0?*)
            log_error "WVA_VERBOSITY=$WVA_VERBOSITY is not an integer from 0 to $WVA_VERBOSITY_MAX" ;;
    esac
    [ "$WVA_VERBOSITY" -le "$WVA_VERBOSITY_MAX" ] \
        || log_error "WVA_VERBOSITY=$WVA_VERBOSITY is above $WVA_VERBOSITY_MAX, the most verbose level the controller logs at"
    log_info "Controller verbosity: --v=$WVA_VERBOSITY"
    grep -q '^patches:' "$kustomization" || printf 'patches:\n' >> "$kustomization"
    cat >> "$kustomization" <<PATCH
- patch: |-
    - op: test
      path: /spec/template/spec/containers/0/name
      value: manager
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --v=${WVA_VERBOSITY}
  target:
    kind: Deployment
    name: wva-controller-manager
PATCH
}
