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
    local tmp_overlay="$1" role binding rendered
    [ -n "${WVA_WATCH_NS:-}" ] || return 0
    [ "$WVA_WATCH_NS" != "$WVA_NS" ] || return 0

    read -r role binding <<< "$(wva_watch_rbac_names)"
    rendered=$(kubectl kustomize "$tmp_overlay" 2>/dev/null         | WVA_TARGET_NS="$WVA_WATCH_NS" WVA_ROLE_NAME="$role" yq '
            select(.kind == "Role" and (.metadata.name | test("manager-role$")))
            | .metadata.name = strenv(WVA_ROLE_NAME)
            | .metadata.namespace = strenv(WVA_TARGET_NS)') || true
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
# stack trace and RESTARTS=1 on a healthy install. Measured on CoreWeave waldorf.
#
# It also closes a window on every UPGRADE: `kubectl apply -k` re-applies the
# shipped ConfigMap, so the default was briefly live again each time, and a
# controller that happened to restart in that window died on it.
#
# The content is generated from the shipped file, never a second copy of it: yq
# reads data["config.yaml"] and assigns into it, so comments and every other
# setting survive, and a new key in the shipped ConfigMap needs no change here.
wva_render_manager_config_patch() {
    local tmp_overlay="$1" src rendered
    src="$WVA_PROJECT/config/base/manager/manager-configmap.yaml"
    [ -f "$src" ] || return 0
    [ -n "${PROMETHEUS_URL:-}" ] || return 0

    rendered="$(yq '.data."config.yaml"' "$src" \
        | yq ".PROMETHEUS_BASE_URL = \"$PROMETHEUS_URL\"")" || return 0
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
    log_warning "  or serve the scaler over mutual TLS (--external-scaler-cert-path and"
    log_warning "  --external-scaler-client-ca; see docs/reference/configuration.md)."
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

# wva_warn_prometheus_tls warns when the controller will connect to an HTTPS
# Prometheus without verifying its certificate. $1 is the manager config.yaml.
#
# The shipped config skips verification, and on Kubernetes it has to: the
# installer has no CA to give the controller -- the certificate of the
# kube-prometheus-stack it deploys is self-signed and generated on the
# workstation, and nothing mounts it into the controller. Flipping the default
# would make every such install exit on "x509: certificate signed by unknown
# authority". So the default stays, and an install that ends up unverified says
# so, with the production setting. OpenShift ships verification on (service CA)
# and never reaches the warning.
wva_warn_prometheus_tls() {
    local cfg="$1" url skip ca
    [ -n "$cfg" ] || return 0
    url="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_BASE_URL // ""' 2>/dev/null || true)"
    skip="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_TLS_INSECURE_SKIP_VERIFY // ""' 2>/dev/null || true)"
    ca="$(printf '%s\n' "$cfg" | yq -r '.PROMETHEUS_CA_CERT_PATH // ""' 2>/dev/null || true)"
    case "$url" in https://*) ;; *) return 0 ;; esac
    [ "$skip" = "true" ] || return 0
    [ -z "$ca" ] || return 0
    log_warning "WVA will NOT verify the TLS certificate of $url (PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true, no PROMETHEUS_CA_CERT_PATH)."
    log_warning "  Fine for a development cluster. In production, mount the CA that signed the Prometheus certificate into the"
    log_warning "  controller and set PROMETHEUS_CA_CERT_PATH=<file> and PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=false."
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
# prometheus-operated Service it finds in the order the API lists Prometheus
# CRs; on a shared cluster that is whichever namespace sorts first, usually
# someone else's, and the grant then went to an account in a namespace nobody
# named. Never "the first Prometheus CR on the cluster" for the same reason, and
# never `default`, which is what a CR with no serviceAccountName runs as: every
# Pod in that namespace without its own account would be granted. PROMETHEUS_URL
# must be resolved before this is asked (wva_resolve_prometheus_url), or the
# namespace is the default's.
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

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    if [ -n "${WVA_IMAGE_DIGEST:-}" ]; then
        log_info "Using image: $WVA_IMAGE_REPO@$WVA_IMAGE_DIGEST"
    else
        log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    fi

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
    # that cached :main once keeps it forever: measured on pokprod, a controller
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
        wva_append_install_kueue_reader_scope_patch "$tmp_overlay/kustomization.yaml" "$WVA_NS" "${WVA_WATCH_NS:-$WVA_NS}"
        wva_append_policy_namespace_reader_scope_patch "$tmp_overlay/kustomization.yaml" "$WVA_NS" \
            "${WVA_WATCH_NS:-$WVA_NS}" "${WVA_POLICY_NS:-}" "$(wva_managed_policy_label "${WVA_WATCH_NS:-$WVA_NS}")"
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
    wva_resolve_prometheus_url
    wva_render_manager_config_patch "$tmp_overlay"

    # Who may call the external scaler: KEDA's namespace only, unless opted out.
    wva_append_scaler_networkpolicy_patch "$tmp_overlay/kustomization.yaml"

    # Prune on INSTALL as well as uninstall. Waiting for an uninstall would leave
    # the grant in place on every cluster that already has it, since upgrading is
    # what people actually do -- and the whole reason the binding was dropped is
    # that it handed out access nobody needed.
    wva_delete_legacy_crbs "$WVA_NS"

    # WVA_REPLICAS runs the controller with a warm standby.
    #
    # The manifest already sets --leader-elect=true, and cmd/main.go exposes the
    # lease/renew/retry durations, so the capability was there — the Deployment just
    # had no replicas field (defaulting to 1) and the installer no way to set one.
    #
    # This is FAILOVER, not throughput: only the leader runs the optimization loops,
    # and the standbys sit on the lease doing nothing until it lapses. Two replicas
    # turn a node drain or a crash from "no scaling decisions until the pod is
    # rescheduled" into a lease timeout.
    if [ -n "${WVA_REPLICAS:-}" ] && [ "${WVA_REPLICAS}" != "1" ]; then
        log_info "Controller replicas: $WVA_REPLICAS (leader-elected; standbys take over on failure, they do not share the work)"
        cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- patch: |-
    - op: add
      path: /spec/replicas
      value: ${WVA_REPLICAS}
  target:
    kind: Deployment
    name: wva-controller-manager
EOF
    fi

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
        if ! kubectl get namespace "$WVA_WATCH_NS" >/dev/null 2>&1; then
            log_error "WVA_WATCH_NS=$WVA_WATCH_NS does not exist. Create the tenant namespace before installing."
        fi
        if [ "$WVA_WATCH_NS" = "$WVA_NS" ]; then
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
        all|prereqs) wva_apply_watch_namespace_rbac "$tmp_overlay" ;;
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
    wva_resolve_prometheus_url

    if [ -n "${PROMETHEUS_URL:-}" ]; then
        log_info "Pointing WVA at Prometheus: $PROMETHEUS_URL"
        patch_manager_config ".PROMETHEUS_BASE_URL = \"$PROMETHEUS_URL\""
        wva_reconcile_prometheus_scheme "$PROMETHEUS_URL"
    fi
    if [ -n "${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY:-}" ]; then
        patch_manager_config ".PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = \"${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY}\""
    fi
    # Read back what the controller will actually run with, after every patch
    # above, rather than reasoning about which of them applied.
    wva_warn_prometheus_tls "$(kubectl get configmap wva-manager-config -n "$WVA_NS" \
        -o jsonpath='{.data.config\.yaml}' 2>/dev/null || true)"

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
            policy_declared_limiters "$limiter_set" "$current_default"
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
    if [ -z "$wva_deploy" ]; then
        log_warning "No WVA Deployment found in $WVA_NS to wait for"
    elif kubectl rollout status "$wva_deploy" -n "$WVA_NS" --timeout=60s; then
        :
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

