#!/usr/bin/env bash
#
# Common logging and utility helpers for deploy/install.sh.
# Requires vars: BLUE, GREEN, YELLOW, RED, NC.
# Exposes funcs: log_info/log_warning/log_success/log_error,
# containsElement(), should_skip_helm_repo_update().
#

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1" >&2
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1" >&2
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
    exit 1
}

# Helm repo update behavior:
# - Default: DO NOT skip (`helm repo update` runs)
# - Opt-in: set `SKIP_HELM_REPO_UPDATE=true` to skip (faster, but requires repo indexes to already exist)
should_skip_helm_repo_update() {
    local skip="${SKIP_HELM_REPO_UPDATE:-false}"
    echo "$skip"
}

# Used to check if the environment variable is in a list
containsElement() {
    local e match="$1"
    shift
    for e; do [[ "$e" == "$match" ]] && return 0; done
    return 1
}

# kc_capture runs `kubectl "$@"` and keeps its two streams apart: KC_OUT is
# stdout, KC_ERR is stderr, and the status is kubectl's.
#
# A read that merged them with 2>&1 parsed whatever kubectl printed on stderr as
# the answer. kubectl prints warnings there on SUCCESS -- a deprecated API, an
# admission webhook, a client-side throttling notice -- so a label read came
# back as "Warning: ...", and a count read as a YAML document that yq could not
# parse. Match NotFound and Forbidden against KC_ERR, read data from KC_OUT only.
#
# Sets variables rather than echoing, so the caller stays in its own shell: call
# it as `kc_capture get ... || rc=$?`, never inside `$( )`.
kc_capture() {
    local errf rc=0
    KC_OUT="" KC_ERR=""
    errf="$(mktemp)" || { KC_ERR="mktemp failed"; return 125; }
    KC_OUT="$(kubectl "$@" 2>"$errf")" || rc=$?
    KC_ERR="$(cat "$errf")"
    rm -f "$errf"
    return "$rc"
}

# kc_not_found reports whether stderr text $1 is the API server saying the
# object does not exist -- and nothing else (Forbidden, timeouts, a webhook
# error) is.
kc_not_found() {
    case "$1" in
        *NotFound*|*"not found"*) return 0 ;;
    esac
    return 1
}

# wva_make_cmd echoes the make command that repeats THIS install's shape for
# target $1; any VAR=value arguments after it are put in front.
#
# A remedy printed as `NAMESPACE=$WVA_NS make setup-prereqs` was wrong for two of
# the three shapes: in the out-of-reach one (controller in wva-<ns>, managing
# <ns>) it named the controller's namespace as the one to manage, and in
# cluster scope it dropped SCOPE=cluster, so the admin who ran it set up a
# namespace-scoped install nobody asked for.
#
#   namespace scope, self-managed   NAMESPACE=<ns> make <target>
#   namespace scope, out of reach   NAMESPACE=<managed> make <target> WVA_NS=<ctl> WVA_WATCH_NS=<managed>
#   cluster scope                   make <target> SCOPE=cluster WVA_NS=<ctl>
wva_make_cmd() {
    local target="$1" pre="" kv
    shift
    for kv in "$@"; do pre="$pre$kv "; done
    if [ "$(wva_install_scope)" = cluster ]; then
        printf '%smake %s SCOPE=cluster WVA_NS=%s\n' "$pre" "$target" "$WVA_NS"
    elif [ -n "${WVA_WATCH_NS:-}" ] && [ "$WVA_WATCH_NS" != "$WVA_NS" ]; then
        printf '%sNAMESPACE=%s make %s WVA_NS=%s WVA_WATCH_NS=%s\n' "$pre" "$WVA_WATCH_NS" "$target" "$WVA_NS" "$WVA_WATCH_NS"
    else
        printf '%sNAMESPACE=%s make %s\n' "$pre" "$WVA_NS" "$target"
    fi
}

# wva_install_scope echoes the install scope: "cluster" or "namespace".
#
# The two differ in WHO CAN INSTALL as much as in what the controller reads:
#
#   cluster     manages every namespace. Its manager ClusterRole reads every
#               namespace, and nodes.
#   namespace   manages ONE namespace. Its manager role is a namespaced Role, so
#               it reads nothing outside — but it still carries cluster-scoped
#               RBAC for the things no Role can express: the gpu-inventory
#               limiter (nodes), authenticated metrics (TokenReview) and EPP
#               metrics (nonResourceURLs).
#
# Neither scope is installable by a namespace admin in one command, and pretending
# otherwise was the bug: the PHASE split is what makes a tenant install real. An
# admin runs the prereqs phase once for a namespace; its owner installs and
# upgrades the controller from then on.
#
# WVA_SCOPE selects it. It is the SCENARIO selector: which of WVA's install shapes
# this is, and therefore what the preflight is entitled to expect.
#
# The default is `namespace` EVERYWHERE, because that is the supported scenario --
# llm-d already working in one namespace, WVA added to it. It used to be inferred
# from the platform ("namespace" on OpenShift, "cluster" elsewhere), which was not a
# decision anybody made: the comment here said it "preserves the historical
# inference". The effect was that `deploy/install.sh` run directly on Kubernetes or
# kind selected the CLUSTER scenario -- a different shape, with different
# preconditions -- without the caller asking for it. `make` never did, because
# Makefile's SCOPE already defaults to namespace.
wva_install_scope() {
    local scope="${WVA_SCOPE:-}"
    if [ -z "$scope" ]; then
        scope="namespace"

        # The old platform inference, kept for reference and no longer reached. It
        # is left in place deliberately rather than deleted: it records what the
        # default used to be, for anyone reading a bug report from before this
        # change.
        #
        #   if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        #       scope="namespace"
        #   else
        #       scope="cluster"
        #   fi
    fi
    case "$scope" in
        cluster|namespace) echo "$scope" ;;
        *) log_error "WVA_SCOPE must be 'cluster' or 'namespace', got '$scope'" ;;
    esac
}

# wva_warn_unsupported_scope says so, once, when the caller has selected a scenario
# that is not the one being supported right now.
#
# Cluster scope is NOT disabled -- it is left working, and anyone for whom it works
# should keep using it. What it does not get is the preflight's namespace-centric
# expectations, which are written for scenario 1 and would be wrong here: in a
# cluster-scoped install the namespaces WVA manages need not exist yet, need not hold
# model servers yet, and are discovered rather than named.
wva_warn_unsupported_scope() {
    [ "$(wva_install_scope)" = "cluster" ] || return 0
    log_warning "WVA_SCOPE=cluster — one controller for every namespace. This is WIP and not the scenario currently supported end to end."
    log_warning "  It is not disabled: if it works for you, keep using it. The checks below are written for a single namespace, so they may not fit."
    log_warning "  To install it without them:  SKIP_CHECKS=true"
}

# The limiter set an install declares. Here rather than in limiter_policy.sh
# because the prerequisite checks read it too, and they are sourced, and
# tested, with only this file beside them.
#
# limiter_types_of <list> echoes the limiter set a WVA_LIMITER value names,
# normalised: space-separated, the physical limiter first, each kind once, and
# `none` for an empty set. The list separates entries with commas or spaces:
# `gpu-inventory,quota`.
#
# Every declared limiter is built and a decision must fit all of them, so the
# list is a SET of bounds: the physical one (the GPUs that exist) and the quota
# (an allowance). At most one physical entry, under either spelling, which the
# controller also refuses; `none` beside anything else is a contradiction.
# Refusals go through log_error.
limiter_types_of() {
    local list="${1:-none}" t physical="" quota="" saw_none=0 n=0 reset_glob=1
    case "$-" in *f*) reset_glob=0 ;; esac
    set -f
    for t in $(printf '%s' "$list" | tr ',' ' '); do
        n=$((n + 1))
        case "$t" in
            none) saw_none=1 ;;
            gpu-inventory|inventory)
                if [ -n "$physical" ] && [ "$physical" != "$t" ]; then
                    [ "$reset_glob" = 1 ] && set +f
                    log_error "WVA_LIMITER names the physical limiter twice ('$physical' and '$t'). There is one physical cluster; declare it once."
                fi
                physical="$t" ;;
            quota) quota="quota" ;;
            *)
                [ "$reset_glob" = 1 ] && set +f
                log_error "WVA_LIMITER entries must be 'none', 'gpu-inventory' or 'quota' (a comma-separated list composes them: gpu-inventory,quota), got '$t' in '$list'" ;;
        esac
    done
    [ "$reset_glob" = 1 ] && set +f
    if [ "$saw_none" = 1 ] && { [ -n "$physical" ] || [ -n "$quota" ]; }; then
        log_error "WVA_LIMITER='$list' names 'none' beside a limiter. Name the limiters, or none alone."
    fi
    if [ "$n" -eq 0 ] || { [ -z "$physical" ] && [ -z "$quota" ]; }; then
        echo none
        return 0
    fi
    printf '%s\n' "$(printf '%s %s' "$physical" "$quota" | sed 's/^ *//; s/ *$//')"
}

# limiter_set_has <set> <kind> reports whether a limiter_types_of set holds the
# kind: `physical` (either spelling) or `quota`.
limiter_set_has() {
    case "$2" in
        physical) case " $1 " in *" gpu-inventory "*|*" inventory "*) return 0 ;; esac ;;
        quota) case " $1 " in *" quota "*) return 0 ;; esac ;;
    esac
    return 1
}

# wva_limiter_set echoes the limiter set this install declares: WVA_LIMITER,
# normalised by limiter_types_of. Unset reads as none here; the install itself
# keeps the live policy's limiters when WVA_LIMITER is not given.
wva_limiter_set() {
    limiter_types_of "${WVA_LIMITER:-none}"
}

# wva_scope_is_tenant reports whether this install manages ONE namespace.
#
# It says nothing about what the install creates. That is a question about the
# RENDERED overlay — ask wva_rendered_kinds, never this. Both scopes carry
# cluster-scoped RBAC; what makes a namespace-scoped install a tenant's is that
# an admin created that RBAC in the prereqs phase, not the scope name.
wva_scope_is_tenant() {
    [ "$(wva_install_scope)" = "namespace" ]
}

# wva_scaler_namespaces echoes every namespace running a WVA external-scaler
# Service, one per line -- that is, every install a KEDA trigger could name.
#
# The Service is the right thing to look for rather than the Deployment: the
# address in a ScaledObject resolves to a Service, so a Service is what makes an
# address answerable, and one without a ready controller behind it is a
# different fault with a different fix.
#
# Empty output means "none found OR not allowed to look", and the two are
# different problems. Callers that act on the answer must say which they assumed.
wva_scaler_namespaces() {
    local out
    if out=$(kubectl get svc -A -l app.kubernetes.io/name=workload-variant-autoscaler \
        --field-selector metadata.name=wva-external-scaler \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null); then
        printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | sort -u
        return 0
    fi
    # No cluster-wide list. The one namespace we can name is still worth checking
    # -- and a Forbidden answer there is said, not folded into "none".
    local rc=0
    wva_scaler_svc_state "$WVA_NS" || rc=$?
    case "$rc" in
        0) echo "$WVA_NS" ;;
        1) : ;;
        *) log_warning "Cannot list Services cluster-wide, and cannot verify (Forbidden or unreadable) a wva-external-scaler Service in $WVA_NS either: ${WVA_SCALER_SVC_ERR:-no detail}. Pass WVA_NS=<namespace> to name the install." ;;
    esac
    return 0
}

# wva_scaler_svc_state reports on the wva-external-scaler Service in namespace $1:
# 0 it is there, 1 the API server says it is not, 2 could not look -- Forbidden,
# or any other error. On 2, WVA_SCALER_SVC_ERR holds kubectl's first error line.
#
# Three answers because two of them were being read as one. A tenant whose
# controller runs in a namespace they may not read gets Forbidden for a Service
# that is there and answering, and treating that as "no scaler" either refused a
# correct WVA_NS or rewrote a working address. Call it directly, not in $( ):
# the variable is the caller's.
wva_scaler_svc_state() {
    local rc=0
    WVA_SCALER_SVC_ERR=""
    kc_capture get svc wva-external-scaler -n "$1" -o name || rc=$?
    [ "$rc" -eq 0 ] && return 0
    kc_not_found "$KC_ERR" && return 1
    WVA_SCALER_SVC_ERR="$(printf '%s\n' "$KC_ERR" | grep -v '^[[:space:]]*$' | head -1)"
    return 2
}

# wva_any_pod_ready reports whether any pod matching selector $2 in namespace $1
# is READY. Pass -A for cluster-wide.
#
# `kubectl get pods | grep -q Running` is not this check, and the difference is
# the one that matters when something is wrong: a pod stuck at "0/1 Running" --
# crash-looping, or failing a probe -- matches Running and is not ready. On a
# multi-pod list it is worse, because it answers yes when ANY line says Running,
# including one belonging to a replica that is fine while the one you care about
# is not.
#
# The Ready CONDITION is what "is it up" means, so that is what this reads. A
# failed call returns non-zero rather than "not ready": being unable to look is
# not the same as looking and finding nothing, and a caller that gates on this
# must be able to tell them apart.
wva_any_pod_ready() {
    local scope="$1" selector="$2" out
    local path='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'
    if [ "$scope" = "-A" ]; then
        out=$(kubectl get pods -A -l "$selector" -o jsonpath="$path" 2>/dev/null) || return 2
    else
        out=$(kubectl get pods -n "$scope" -l "$selector" -o jsonpath="$path" 2>/dev/null) || return 2
    fi
    printf '%s\n' "$out" | grep -qx 'True'
}

# wva_pods_exist reports whether any pod matches selector $2 in namespace $1
# (-A for cluster-wide), regardless of readiness.
#
# Pairs with wva_any_pod_ready to separate "it is here and unhealthy" from "it is
# not here". Only the first justifies telling someone their cluster is broken;
# the second usually means we looked in the wrong place.
wva_pods_exist() {
    local scope="$1" selector="$2" out
    if [ "$scope" = "-A" ]; then
        out=$(kubectl get pods -A -l "$selector" -o name 2>/dev/null) || return 2
    else
        out=$(kubectl get pods -n "$scope" -l "$selector" -o name 2>/dev/null) || return 2
    fi
    [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]
}

# wva_scaler_has_endpoints reports whether the external-scaler Service in $1 has a
# ready backend -- a controller actually listening, not merely an address that
# resolves.
#
# A Service with no Endpoints is the shape a half-removed install leaves behind,
# and it is indistinguishable from a live one by name alone. Anything CHOOSING
# between installs must use this; anything deciding whether to leave another
# install's object alone must not, because there the Service existing at all is
# reason enough to keep hands off.
#
# EndpointSlice is read first (Endpoints is deprecated and unserved in newer
# clusters) with Endpoints as the fallback, so this works either side of that
# change rather than silently reporting "no backend" on every Service.
wva_scaler_has_endpoints() {
    local ns="$1" out
    if out=$(kubectl get endpointslice -n "$ns" -l kubernetes.io/service-name=wva-external-scaler \
        -o jsonpath='{range .items[*]}{range .endpoints[*]}{.addresses[0]}{"\n"}{end}{end}' 2>/dev/null); then
        [ -z "$(printf '%s' "$out" | tr -d '[:space:]')" ] || return 0
    fi
    out=$(kubectl get endpoints wva-external-scaler -n "$ns" \
        -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null) || return 1
    [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]
}

# --- Where KEDA runs, for the external-scaler NetworkPolicy.
#
# Two readers, each returning 0 with an answer, 1 for "looked, and there is
# none", and 2 for "could not look". 2 is never folded into 1: a Forbidden read
# reported as "none found" sent the install to keda-system on a cluster whose
# KEDA runs in `keda`, and KEDA was then refused by the policy written for it.
# Shared by the install (wva_resolve_scaler_keda_namespace) and the post-deploy
# verify (wva_verify_scaler_networkpolicy), which must agree on what "KEDA is
# there" means.

# wva_keda_apiservice_ns echoes the namespace of the Service behind the
# v1beta1.external.metrics.k8s.io APIService, when that Service is KEDA's.
#
# The strongest answer there is: the APIService is cluster-scoped and written by
# whoever installed KEDA, so no tenant can plant it, and KEDA's metrics server
# and operator are installed into one namespace by the Helm chart, the operator
# manifests and OpenShift's Custom Metrics Autoscaler alike. Returns 3 when the
# APIService is served by something that is not KEDA (prometheus-adapter); the
# Service it names is then echoed as "<namespace>/<name>" for the message.
wva_keda_apiservice_ns() {
    local rc=0 ns name
    kc_capture get apiservice v1beta1.external.metrics.k8s.io \
        -o jsonpath='{.spec.service.namespace}{" "}{.spec.service.name}' || rc=$?
    if [ "$rc" -ne 0 ]; then
        printf '%s' "$KC_ERR" | grep -qi 'notfound\|not found' && return 1
        return 2
    fi
    read -r ns name <<< "$KC_OUT"
    [ -n "$ns" ] || return 1
    case "$name" in
        *keda*) printf '%s\n' "$ns"; return 0 ;;
        *) printf '%s/%s\n' "$ns" "$name"; return 3 ;;
    esac
}

# wva_scaler_np_keda_ns echoes the namespace an existing wva-external-scaler
# NetworkPolicy in $1 admits to :9090. Returns 1 when there is no such policy,
# 2 when it could not be read, and 4 when it EXISTS but admits :9090 from
# anything other than exactly one namespace -- present, so not "no policy".
wva_scaler_np_keda_ns() {
    local ns="$1" rc=0 found
    kc_capture get networkpolicy wva-external-scaler -n "$ns" \
        -o jsonpath='{range .spec.ingress[*]}{range .ports[*]}{.port}{" "}{end}{"|"}{range .from[*]}{.namespaceSelector.matchLabels.kubernetes\.io/metadata\.name}{" "}{end}{"\n"}{end}' || rc=$?
    if [ "$rc" -ne 0 ]; then
        printf '%s' "$KC_ERR" | grep -qi 'notfound\|not found' && return 1
        return 2
    fi
    found="$(printf '%s\n' "$KC_OUT" | awk -F'|' '{
            n = split($1, p, " "); hit = 0
            for (i = 1; i <= n; i++) if (p[i] == "9090") hit = 1
            if (hit) { m = split($2, f, " "); for (j = 1; j <= m; j++) print f[j] }
        }' | sort -u)"
    [ -n "$found" ] || return 4
    [ "$(printf '%s\n' "$found" | wc -l | tr -d ' ')" = 1 ] || return 4
    printf '%s\n' "$found"
}

# There is no third reader. "Where a Deployment named keda-operator runs" was
# one, and it was plantable: any tenant can create a Deployment of that name, and
# the answer then pointed the policy at their namespace (opening :9090 to them)
# or made it "ambiguous" and stopped the install.

# WVA_SHARED_CLUSTER_ROLE_BINDINGS are the cluster-scoped bindings every overlay
# applies under fixed names. They are renamed per install so two installs cannot
# take them from one another.
WVA_SHARED_CLUSTER_ROLE_BINDINGS=(
    wva-epp-metrics-reader-role-binding
    wva-manager-cluster-monitoring-view
    wva-manager-rolebinding
    wva-metrics-auth-rolebinding
    wva-metrics-reader-rolebinding
    # wva-prometheus-cluster-monitoring-view was here. The binding is gone —
    # see components/openshift/kustomization.yaml for why it never did anything.
    # Dropped from this list too, rather than kept as a no-op: the rename patch
    # only matches objects the overlay renders, so a name listed here for an
    # object that no longer exists implies a cleanup that does not happen.
    # An install made BEFORE its removal keeps its suffixed copy until that
    # install is uninstalled with the tree that still rendered it.
    # Namespace scope only (the cluster-scoped manager role already reads nodes).
    # Listed unconditionally anyway: the rename patch is a no-op when the object
    # is absent, and leaving it out meant two installs shared one binding — the
    # second apply replaced its subject list, so the first controller lost node
    # access and its readiness gate turned that into a NotReady pod.
    wva-node-reader-rolebinding
    # Same story for the Kueue reads a kueue-enabled quota entry needs.
    wva-kueue-reader-rolebinding
    # And for the Namespace get that finds the cluster policy namespace.
    wva-policy-namespace-reader-rolebinding
)

# WVA_OWNED_CLUSTER_ROLES are the ClusterRoles this project defines, mapped to the
# binding that references each. They are renamed per install for the same reason
# the bindings are, and it took a shared cluster to make the reason concrete:
# pokprod001 had ten WVA installs sharing four ClusterRoles under fixed names.
# Identical rules make an apply a no-op, so nothing had gone wrong yet — but an
# install carrying different rules would have rewritten the permissions of ten
# other controllers, cluster-wide, with no error and no restart to notice.
#
# Only OUR roles. `cluster-monitoring-view` is OpenShift's own: its bindings are
# renamed, its roleRef must not be.
#
# Kustomize's name-reference transformer would repoint roleRefs automatically for
# a nameSuffix, but nameSuffix renames every resource — including the
# external-scaler Service that every ScaledObject trigger addresses by name. So
# the roleRef is repointed explicitly, alongside the rename.
WVA_OWNED_CLUSTER_ROLES=(
    "wva-manager-role:wva-manager-rolebinding"
    "wva-metrics-auth-role:wva-metrics-auth-rolebinding"
    "wva-metrics-reader:wva-metrics-reader-rolebinding"
    "wva-epp-metrics-reader-role:wva-epp-metrics-reader-role-binding"
    # Namespace scope only; the patches are no-ops when the object is absent.
    # The role is node-reader-ROLE — the binding drops the suffix, the role does
    # not, and getting that wrong leaves the role unrenamed and the binding
    # pointing at a name nothing creates.
    "wva-node-reader-role:wva-node-reader-rolebinding"
    "wva-kueue-reader-role:wva-kueue-reader-rolebinding"
    "wva-policy-namespace-reader-role:wva-policy-namespace-reader-rolebinding"
)

# wva_ns_suffix echoes the per-namespace suffix appended to those names.
# sha256sum is GNU; macOS ships shasum. The suffix must be IDENTICAL on whatever
# host installs and whatever host uninstalls — a different suffix means the
# uninstall leaks every ClusterRoleBinding it was meant to remove — so this picks
# one implementation deterministically rather than depending on the box.
wva_ns_suffix() {
    if command -v sha256sum >/dev/null 2>&1; then
        printf '%s' "$1" | sha256sum | cut -c1-8
    else
        printf '%s' "$1" | shasum -a 256 | cut -c1-8
    fi
}

# wva_kueue_reader_names sets WVA_KUEUE_READER_NAMES to the Namespaces, space
# separated, that the Kueue reader of the controller installed in $1 may `get`:
# $2 first (the namespace it manages), then every name the live ClusterRole
# wva-kueue-reader-role-<suffix> already carries on its Namespace rule.
#
# ONE answer for the two commands that write that ClusterRole: the installer's
# prereqs phase (wva_append_kueue_reader_scope_patch) and
# `make enable-physical-limiter` (pl_grant_kueue_read). Each used to compute its
# own set -- the installer the managed namespace, enable the controller's own --
# so for a controller in wva-foo managing foo every enable took `get
# namespaces/foo` away, its Kueue source bounded foo at nothing, and the next
# setup-prereqs gave it back. Both now start from the managed namespace and keep
# what is there: a name an admin added by hand survives either command. A rule
# with no resourceNames adds nothing -- narrowing an unrestricted read to the
# managed namespace is what the installer always did.
#
# Only DNS-label names are kept, since they are written into YAML. Sets
# variables rather than echoing (call it as `wva_kueue_reader_names ... ||
# rc=$?`). A role that exists and cannot be read RETURNS non-zero with the
# reason in KC_ERR: Forbidden is not absent, and writing the role without
# knowing what it holds may remove a name.
wva_kueue_reader_names() {
    local ns="$1" managed="$2" rc=0 n existing="" seen=" "
    WVA_KUEUE_READER_NAMES=""
    KC_ERR=""
    kc_capture get clusterrole "wva-kueue-reader-role-$(wva_ns_suffix "$ns")" -o yaml || rc=$?
    if [ "$rc" -ne 0 ]; then
        kc_not_found "$KC_ERR" || { KC_ERR="${KC_ERR:-kubectl exited $rc}"; return 1; }
    elif [ -n "$KC_OUT" ]; then
        if ! existing="$(printf '%s\n' "$KC_OUT" \
            | yq '.rules[] | select((.resources // [])[] == "namespaces") | (.resourceNames // [])[]' 2>&1)"; then
            KC_ERR="could not parse it: $existing"
            return 1
        fi
    fi
    local -a all
    # read -a, not an unquoted expansion: a `*` in a hand-edited role must not
    # glob into the names of files in the working directory.
    read -r -a all <<< "$managed $(printf '%s' "$existing" | tr '\n' ' ')"
    for n in "${all[@]}"; do
        case "$n" in ''|*[!a-z0-9-]*) continue ;; esac
        case "$seen" in *" $n "*) continue ;; esac
        seen="$seen$n "
        WVA_KUEUE_READER_NAMES="${WVA_KUEUE_READER_NAMES:+$WVA_KUEUE_READER_NAMES }$n"
    done
    return 0
}

# WVA_LEGACY_CLUSTER_ROLE_BINDINGS are cluster-scoped bindings this project used
# to render and no longer does.
#
# They need deleting BY NAME, because the uninstall works by rendering the overlay
# and deleting what it renders -- so the moment a resource file leaves the overlay,
# `delete -k` can no longer see it and every copy already on the cluster is
# stranded. Measured on pokprod when the Prometheus binding was dropped: 12 of them
# were sitting there, aged 2 to 78 days, one per install, with no tooling left that
# could remove any of them.
WVA_LEGACY_CLUSTER_ROLE_BINDINGS=(
    # Granted the user-workload Prometheus SA cluster-monitoring-view "so it can
    # scrape the controller's secure /metrics endpoint". It never did: that
    # ClusterRole carries `namespaces get` and `prometheuses/api` and no
    # nonResourceURLs rule, so it cannot authorize a /metrics scrape at all --
    # confirmed by reading it off a live cluster. The scrape is authorized by
    # WVA's OWN SA via the ServiceMonitor's bearerTokenSecret plus the
    # metrics-reader nonResourceURLs rule. So this only ever handed a
    # cluster-wide platform-API read to a service account that did not need it.
    wva-prometheus-cluster-monitoring-view
)

# wva_delete_legacy_crbs removes THIS INSTALL's copies of the bindings above.
#
# Scoped by name to this install, never a label sweep. These are cluster-scoped
# objects on a shared cluster with one copy per install, and this repo has already
# paid for getting that wrong once -- an uninstall that resolved fixed names
# stripped a different, healthy install of its permissions (see
# wva_append_crb_name_patches). Deleting only the names this namespace would have
# produced keeps other tenants' copies untouched.
#
# TWO SUFFIX GENERATIONS, both tried: the current suffix is a hash of the
# namespace, an older one used the namespace name verbatim. A cluster upgraded
# through both carries both, which is exactly what pokprod shows --
# `...-016f647d` beside `...-benchmark-epp-keda`. Computing only the hash would
# have left half of them behind.
#
# Failure is reported, not fatal: a namespace-scoped install has no rights to
# delete a ClusterRoleBinding, and the object is inert anyway.
wva_delete_legacy_crbs() {
    local ns="$1" crb name
    [ -n "$ns" ] || return 0
    for crb in "${WVA_LEGACY_CLUSTER_ROLE_BINDINGS[@]}"; do
        for name in "${crb}-$(wva_ns_suffix "$ns")" "${crb}-${ns}"; do
            kubectl get clusterrolebinding "$name" >/dev/null 2>&1 || continue
            if kubectl delete clusterrolebinding "$name" >/dev/null 2>&1; then
                log_info "Removed obsolete ClusterRoleBinding $name (granted the user-workload Prometheus SA cluster-monitoring-view; it never authorized a scrape)."
            else
                log_warning "Obsolete ClusterRoleBinding $name is still present and needs a cluster admin to remove:"
                log_warning "    kubectl delete clusterrolebinding $name"
                log_info   "  It grants the user-workload Prometheus SA cluster-wide monitoring read and is not used by anything."
            fi
        done
    done
}

# wva_append_crb_name_patches appends the rename patches to a kustomization file.
#
# Deploy AND undeploy must both use this. They diverged once, with real cost: the
# install renamed the bindings and the uninstall did not, so `kubectl delete -k`
# resolved the FIXED names and removed whichever install currently owned them —
# an uninstall of one WVA stripping a different, healthy one of its permissions —
# while leaking its own suffixed bindings. One definition, used by both.
wva_append_crb_name_patches() {
    local kustomization="$1" ns="$2" suffix crb pair role binding
    suffix="$(wva_ns_suffix "$ns")"
    printf 'patches:\n' >> "$kustomization"
    for crb in "${WVA_SHARED_CLUSTER_ROLE_BINDINGS[@]}"; do
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /metadata/name
      value: ${crb}-${suffix}
  target:
    kind: ClusterRoleBinding
    name: ${crb}
EOF
    done
    # The roles, and the roleRef of the binding that names each. Both, or the
    # binding points at a ClusterRole that no longer exists under that name and
    # the controller silently has no permissions at all.
    for pair in "${WVA_OWNED_CLUSTER_ROLES[@]}"; do
        role="${pair%%:*}"; binding="${pair#*:}"
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /metadata/name
      value: ${role}-${suffix}
  target:
    kind: ClusterRole
    name: ${role}
- patch: |-
    - op: replace
      path: /roleRef/name
      value: ${role}-${suffix}
  target:
    kind: ClusterRoleBinding
    name: ${binding}
EOF
    done
    # The OpenShift ServiceMonitor pins the serving cert's name with
    # tlsConfig.serverName, and that is a plain STRING: `namespace:` rewrites
    # namespace fields, not a namespace spelled inside a value. Left alone it
    # keeps naming the overlay's default namespace, the service-ca SAN never
    # matches, and Prometheus rejects every scrape with
    #
    #   http: TLS handshake error ...: remote error: tls: bad certificate
    #
    # once per scrape interval, forever. Nothing else fails: the controller is
    # healthy and idle, so the only symptom is that no wva_* series ever appear
    # — which also silently empties any benchmark report built from them. Found
    # by installing into a namespace that is not the overlay default and reading
    # the controller log.
    #
    # It belongs here rather than in the component because only this layer knows
    # the namespace: a component's replacement runs before the outer
    # `namespace:` transform, when the Service still has no namespace at all.
    if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /spec/endpoints/0/tlsConfig/serverName
      value: wva-controller-manager-metrics-service.${ns}.svc
  target:
    kind: ServiceMonitor
EOF
    fi
}

# wva_overlay_dir echoes the absolute Kustomize overlay directory for the selected
# scope and platform. Deploy and cleanup MUST resolve it the same way, or an
# uninstall leaves behind exactly the resources the other overlay owns.
wva_overlay_dir() {
    local scope platform
    scope="$(wva_install_scope)"
    if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        platform="openshift"
    else
        platform="kubernetes"
    fi
    local dir="$WVA_PROJECT/config/overlays/${scope}-scoped/${platform}"
    [ -d "$dir" ] || log_error "No overlay for scope '${scope}' on '${platform}' (looked in $dir)"
    (cd "$dir" && pwd)
}

# The kinds a CLUSTER ADMIN owns, and the phase split built on them.
#
# Everything here either is cluster-scoped or needs a permission a namespace admin
# does not have with the stock `admin` ClusterRole. Splitting the install on this
# line is what makes a tenant install real rather than aspirational: an admin runs
# the prereqs phase once for a namespace, and whoever owns that namespace can then
# install and upgrade the controller without holding any cluster-scoped right.
#
#   Namespace           cluster-scoped to create.
#   ClusterRole/Binding cluster-scoped, obviously.
#   ServiceMonitor      namespaced, but the stock `admin` ClusterRole does NOT
#                       grant monitoring.coreos.com — verified on a real cluster,
#                       where it was one of two denials standing between a
#                       namespace admin and a working "self-service" install.
#   Role/RoleBinding    namespaced, and still not the tenant's to create: RBAC
#                       forbids granting permissions you do not hold yourself
#                       (privilege-escalation prevention), and the controller's
#                       Role names resources the stock `admin` ClusterRole does
#                       not carry. A tenant applying it gets "attempting to grant
#                       RBAC permissions not currently held", the Role is skipped,
#                       its RoleBinding then fails with "not found", and the
#                       controller comes up unable to read a ConfigMap in its own
#                       namespace — a CrashLoopBackOff two removes from its cause.
#                       Verified with a token-scoped kubeconfig; it cannot be
#                       fixed by granting more to the tenant without granting them
#                       everything the controller has.
# ServiceAccount and Secret are here for the ServiceMonitor's sake, and the
# ordering is the whole point.
#
# On OPENSHIFT the ServiceMonitor authenticates with `bearerTokenSecret:
# wva-controller-manager-token`. (On Kubernetes it uses `bearerTokenFile` -- a
# projected service-account token the kubelet supplies -- so there is no Secret
# for the operator to resolve and this failure cannot occur there. The kinds move
# on both platforms anyway: the objects reference each other on both, and one
# phase boundary is easier to reason about than a per-platform one.) The prometheus-operator resolves that Secret when
# it first sees the ServiceMonitor, and if the Secret is absent it REJECTS the
# object -- `reason=InvalidConfiguration`, "unable to get secret" -- and does not
# retry when the Secret later appears. Nothing re-evaluates it: a metadata write
# is not enough, and re-running this phase applies identical content, so
# `kubectl apply` reports "unchanged" and the operator never looks again.
#
# With the Secret in the CONTROLLER phase, the two-phase install therefore created
# the ServiceMonitor 33 minutes before the Secret it needs, and WVA's own metrics
# were permanently uncollected -- no `up` series, no `wva_*` series -- while every
# install step reported success. Measured on pokprod001: the install said
# "All components verified successfully!" and Thanos had nothing.
#
# The single-command install (INSTALL_PHASE=all) was unaffected, because it applies
# everything at once and the render orders Secret before ServiceMonitor. That is
# why this survived: it is the two-person split, the shape the guides recommend,
# that breaks. The two namespaces on that cluster running this code with working
# metrics both had their Secret created 3 seconds BEFORE their ServiceMonitor.
#
# Both kinds belong to an admin anyway -- they are namespaced, and creating them
# needs no rights a namespace owner lacks, but they are part of the same
# monitoring wiring the ServiceMonitor is. Moving them here keeps the three
# objects that reference each other in one phase, applied in render order, which
# puts ServiceAccount and Secret ahead of ServiceMonitor.
#
# This list also drives the phase-2 gate (wva_rendered_prereq_objects), so the
# controller phase now checks for them and names them if an admin's phase 1
# predates this change.
#
# Note the filter matches on KIND alone -- it cannot name objects. Every
# ServiceAccount and Secret in the render is therefore admin-owned, today the
# two controller/epp-metrics ServiceAccounts and their service-account-token
# Secrets, which is what is wanted. A Secret added to the base later becomes
# admin-owned silently, and a tenant would meet it as a missing prerequisite at
# phase 2 rather than as anything more obvious. Adding one means deciding which
# phase owns it.
WVA_PREREQ_KINDS=(Namespace ClusterRole ClusterRoleBinding ServiceMonitor Role RoleBinding ServiceAccount Secret)

# wva_prereq_kind_filter echoes a yq expression selecting (or with $1=exclude,
# rejecting) the admin-owned kinds. One definition, so the prereqs phase and the
# controller phase cannot disagree about where the line is and leave a kind that
# neither applies.
# WVA_PREREQ_NAMED are objects the prereqs phase owns BY NAME, for kinds it does
# not own wholesale.
#
# Only wva-metrics-server-ca, and it has to be here: the ServiceMonitor above is
# admin-owned and references this ConfigMap as its `ca`. The prometheus-operator
# resolves that the moment the ServiceMonitor appears and REJECTS it if the
# ConfigMap is absent, then never reconsiders -- so a two-phase install created
# the monitor in phase 1, the ConfigMap in phase 2, and left a standing rejection:
#
#   failed to get ca "<configmap=wva-metrics-server-ca,key=service-ca.crt>":
#   configmaps "wva-metrics-server-ca" not found
#
# with no scrape target, no wva_* series, and a dashboard whose WVA panels are
# empty while its vllm:* panels fill. Exactly the failure #1516 fixed for the
# ServiceMonitor's Secret; the CA was missed because it is a different kind.
#
# By NAME rather than by kind, because the overlay renders three ConfigMaps and
# the other two are the tenant's: wva-manager-config and, especially,
# wva-scaling-policy-config, which a namespace owner is meant to edit. Making
# every ConfigMap admin-owned to fix one of them would take the scaling policy
# with it.
#
# NetworkPolicy/wva-external-scaler is here for a different reason: it must name
# the namespace KEDA runs in, and only the admin's phase can read that (the
# external.metrics APIService is cluster-scoped). Applied from the tenant's
# phase it was always a guess -- keda-system -- and with KEDA in `keda` it
# refused KEDA and nothing scaled. The tenant phase now neither writes nor
# requires it: wva_missing_prereqs reports it OPTIONAL, because an admin phase
# that could not find KEDA leaves it out on purpose
# (wva_append_scaler_networkpolicy_patch) rather than write a guess.
WVA_PREREQ_NAMED=(
    "ConfigMap/wva-metrics-server-ca"
    "NetworkPolicy/wva-external-scaler"
)

wva_prereq_kind_filter() {
    local mode="${1:-select}" k n expr=""
    for k in "${WVA_PREREQ_KINDS[@]}"; do
        [ -n "$expr" ] && expr="$expr or "
        expr="${expr}.kind == \"$k\""
    done
    for n in "${WVA_PREREQ_NAMED[@]}"; do
        expr="$expr or (.kind == \"${n%%/*}\" and .metadata.name == \"${n##*/}\")"
    done
    if [ "$mode" = "exclude" ]; then
        echo "select(($expr) | not)"
    else
        echo "select($expr)"
    fi
}

# wva_resolve_namespace points a namespace-scoped install at NAMESPACE when the
# caller named no WVA_NS.
#
# Here rather than in the Makefile because every entry point needs it — install,
# undeploy and check — and `wva_install_scope` is the only thing that knows the
# scope once WVA_SCOPE is empty and the platform default applies. The make-side
# version reached only the 12 phase targets, so `NAMESPACE=x make
# undeploy-wva-on-k8s` resolved to the DEFAULT namespace while the install it was
# meant to undo had gone to x.
#
# Cluster scope is untouched: that controller manages every namespace and belongs
# in its own, not inside whichever one runs llm-d.
# wva_bootstrap_env gives a caller that sources these libraries directly — rather
# than going through install.sh — the same namespace install.sh would have used.
#
# The targets that plan and apply ScaledObjects do exactly that, so none of
# install.sh's preamble runs for them: they saw WVA_NS's default and ignored
# `export NAMESPACE=…` entirely. The plan then scanned the wrong namespace and
# found nothing, or — worse, because it looks like it worked — wrote every trigger
# with a scaler address in a namespace where no scaler runs. KEDA reports that as
# a failing trigger on a healthy-looking install, which reads as "WVA is broken"
# rather than "WVA was handed the wrong address".
#
# ${VAR-…} without the colon, so an explicitly empty value set by install.sh is
# left alone: a nested call must not re-capture a WVA_NS that a default filled in.
wva_bootstrap_env() {
    WVA_NS_EXPLICIT=${WVA_NS_EXPLICIT-${WVA_NS:-}}
    NAMESPACE_EXPLICIT=${NAMESPACE_EXPLICIT-${NAMESPACE:-}}
    export WVA_NS_EXPLICIT NAMESPACE_EXPLICIT
    WVA_NS=${WVA_NS:-workload-variant-autoscaler-system}
    export WVA_NS
    wva_resolve_namespace
    # NOT wva_require_namespace. wva_bootstrap_env is shared with the read-only
    # tooling -- so-list, so-park, so-freeze, so-resume, scaledobjects-plan,
    # dashboard -- and requiring the managed namespace here refused all of them:
    #
    #   $ make scaledobjects-plan
    #   [ERROR] NAMESPACE is not set, and it is required.
    #   WVA is added to a namespace where llm-d is ALREADY RUNNING, so the
    #   namespace is an input to this install, ...
    #
    # for a command that plans and installs nothing. The requirement belongs to the
    # install, so it is called from the check and install paths in install_core.sh,
    # which is also where its own comment says it should be.
}

# wva_require_namespace stops unless the caller named the namespace WVA is to manage.
#
# NAMESPACE and WVA_NS are DIFFERENT questions, and the fallback below made them look
# like one:
#
#   NAMESPACE   the namespace WVA MANAGES -- where llm-d is running. Mandatory in
#               namespace scope: it is the whole input to the scenario.
#   WVA_NS      the namespace the CONTROLLER IS INSTALLED INTO. It matters when those
#               two differ, which is the cluster-wide and multi-tenant shape, not
#               this one.
#
# So WVA_NS is deliberately NOT accepted as a way to name the target here. Someone
# who sets only WVA_NS has said where the controller goes, not what it manages, and
# guessing the second from the first is how an install ends up in a namespace with no
# llm-d in it.
#
# What this replaces: WVA_NS fell back to `workload-variant-autoscaler-system` and
# wva_autoselect_namespace then searched the cluster and silently ADOPTED the single
# namespace that ran model servers -- or, finding none, kept the fallback. Both paths
# could install a healthy-looking controller into a namespace with nothing to scale,
# which is the silent failure this preflight exists to prevent. The fallback and the
# discovery are both still in the file: discovery is how a CLUSTER-scoped install
# finds the namespaces it manages, which is its proper use.
wva_require_namespace() {
    [ "$(wva_install_scope)" = "namespace" ] || return 0
    [ -z "${NAMESPACE_EXPLICIT:-}" ] || return 0
    # kind-emulator provisions llm-d, the EPP and WVA in one sequence and owns its own
    # namespaces: `make deploy-e2e-infra` calls install.sh with no NAMESPACE on
    # purpose, using the controller-namespace defaults. Requiring it there would break
    # the e2e suite to enforce a rule about a scenario the suite is not running.
    [ "${ENVIRONMENT:-}" != "kind-emulator" ] || return 0

    local found="" count=0 hint=""
    # A hint, not a choice. Listing workloads cluster-wide is exactly what a namespace
    # admin may not do, so this is best-effort and its absence is not an error.
    if kubectl auth can-i list deployments -A >/dev/null 2>&1; then
        found="$(wva_namespaces_with_model_servers 2>/dev/null || true)"
        count=$(printf '%s' "$found" | grep -c . || true)
    fi
    # Capped. On a shared cluster this list is long -- 100+ namespaces on pokprod001 --
    # and an error message that scrolls the instruction off the screen has buried the
    # one line the reader needed.
    if [ "${count:-0}" -gt 0 ]; then
        # Built with explicit newlines: $( ) strips trailing ones, so appending the
        # "and N more" line after a command substitution put it on the end of the
        # last namespace instead of its own line.
        hint="
llm-d model servers are running in:
$(printf '%s' "$found" | head -10 | sed 's/^/  - /')"
        [ "$count" -gt 10 ] && hint="${hint}
  … and $((count - 10)) more"
        hint="${hint}
"
    fi

    log_error "NAMESPACE is not set, and it is required.

WVA is added to a namespace where llm-d is ALREADY RUNNING, so the namespace is an
input to this install, not something to guess. Name the one llm-d serves from:

    NAMESPACE=<namespace> make <target>
${hint}
NAMESPACE is llm-d's own variable, so if you followed one of its guides it is already
exported in that shell.

(WVA_NS is a different setting: it names where the CONTROLLER is installed, which
differs from the managed namespace in the cluster-wide shape, and when the controller
is installed out of reach in a namespace of its own: then pass both, as
NAMESPACE=<managed> WVA_NS=<controller namespace> WVA_WATCH_NS=<managed>.)"
}

wva_resolve_namespace() {
    WVA_NS_SOURCE=default
    export WVA_NS_SOURCE
    [ -z "${WVA_NS_EXPLICIT:-}" ] || { WVA_NS_SOURCE=explicit; return 0; }
    [ -n "${NAMESPACE_EXPLICIT:-}" ] || return 0   # stays "default": discovery may still fill it
    [ "$(wva_install_scope)" = "namespace" ] || return 0
    WVA_NS="$NAMESPACE_EXPLICIT"
    WVA_NS_SOURCE=namespace-env
    export WVA_NS WVA_NS_SOURCE
}

# wva_prepare_overlay_base populates $1/base with the overlay this install will
# apply.
#
# The preflight, the deploy and the undeploy all go through here. They must: a
# preflight that infers the shape instead of reading it is how a namespace-scoped
# OpenShift install passed `--check` and then failed partway through
# `kubectl apply -k`, and an undeploy that built the base differently left
# cluster-scoped objects behind after reporting a clean removal.
#
# It used to re-render the overlay under WVA_ADMIN_GRANTS, dropping the
# unauthenticated-metrics component and adding node-reader, so that one command
# could serve both a cluster admin and a tenant. The phase split removed the
# question: everything cluster-scoped belongs to the prereqs phase, which is the
# admin's by definition, so the overlay is now the same for both and this is a
# symlink.
wva_prepare_overlay_base() {
    local tmp_overlay="$1" kustomize_overlay
    kustomize_overlay="$(wva_overlay_dir)"
    # Symlinked so kustomization.yaml can reference it with a relative path —
    # Kustomize rejects absolute paths in resources.
    ln -s "$kustomize_overlay" "$tmp_overlay/base"
}

# wva_repair_immutable_rolerefs deletes this install's ClusterRoleBindings whose
# roleRef no longer matches what will be applied, so the apply can recreate them.
#
# `roleRef` is IMMUTABLE. An upgrade that changes which ClusterRole a binding
# points at therefore fails with
#
#   ClusterRoleBinding ... is invalid: roleRef: ... cannot change roleRef
#
# and the install stops halfway — which is exactly what happened the first time
# the ClusterRoles were given per-namespace names, on a cluster with an install
# already on it. Found by running the upgrade, not by reading it.
#
# It only ever deletes a binding whose name matches what THIS install renders,
# i.e. one already carrying this namespace's suffix. Another install's binding
# has a different suffix and cannot be selected here. The controller is without
# that binding for the moment between the delete and the apply below.
wva_repair_immutable_rolerefs() {
    local name want have
    while read -r name want; do
        [ -n "$name" ] && [ -n "$want" ] || continue
        have="$(kubectl get clusterrolebinding "$name" -o jsonpath='{.roleRef.name}' 2>/dev/null || true)"
        [ -n "$have" ] || continue          # absent: the apply just creates it
        [ "$have" = "$want" ] && continue   # already right
        log_info "Recreating ClusterRoleBinding $name — roleRef is immutable and must change ($have -> $want)"
        kubectl delete clusterrolebinding "$name" --ignore-not-found >/dev/null 2>&1 || true
    done < <(wva_render_manifests \
        | yq 'select(.kind == "ClusterRoleBinding") | .metadata.name + " " + .roleRef.name' 2>/dev/null \
        | grep -v '^null' || true)
}

# wva_render_manifests prints the manifests this install would apply, with the
# namespace transform and the per-namespace ClusterRoleBinding renames applied —
# so the NAMES it prints are the names that will exist on the cluster. The image
# pin is not applied: no caller of this asks about the image.
#
# Empty output means the overlay could not be rendered. Callers must treat that as
# "unknown", never as "nothing".
wva_render_manifests() {
    local tmp ns="${WVA_NS:-wva-system}"
    tmp="$(mktemp -d)" || return 0
    if wva_prepare_overlay_base "$tmp" >/dev/null 2>&1; then
        printf 'namespace: %s\nresources:\n- ./base\n' "$ns" > "$tmp/kustomization.yaml"
        wva_append_crb_name_patches "$tmp/kustomization.yaml" "$ns"
        # The same per-install decisions deploy_wva_controller applies, so the
        # objects this lists are the objects that install creates. Without the
        # metrics-reader one, an install that DROPPED that binding (no
        # Prometheus account it trusts) left it in the expected set, and the
        # controller-only phase reported it missing and refused forever.
        #
        # Guarded because Makefile targets source this file without
        # infra_wva.sh; none of them renders, so the guard never decides an
        # install.
        if [ "$(wva_install_scope)" = "namespace" ] && declare -F wva_append_kueue_reader_scope_patch >/dev/null; then
            wva_append_kueue_reader_scope_patch "$tmp/kustomization.yaml" "$ns" "${WVA_WATCH_NS:-$ns}"
            wva_append_policy_namespace_reader_scope_patch "$tmp/kustomization.yaml" "$ns" \
                "${WVA_WATCH_NS:-$ns}" "${WVA_POLICY_NS:-}" "$(wva_managed_policy_label "${WVA_WATCH_NS:-$ns}")"
        fi
        if [ "${ENVIRONMENT:-}" != "openshift" ] && declare -F wva_append_metrics_reader_decision >/dev/null; then
            wva_append_metrics_reader_decision "$tmp/kustomization.yaml" "$ns" >/dev/null 2>&1
        fi
        # `|| true` because the callers run under `set -e` with pipefail, and a
        # render that fails is the case they exist to handle: it must reach them
        # as empty output, not as the whole install script exiting.
        kubectl kustomize "$tmp" 2>/dev/null || true
    fi
    rm -rf "$tmp"
}

# wva_rendered_kinds echoes, one per line, each distinct Kind this install would
# create. With $1=controller, only the kinds of the objects the controller-only
# phase (INSTALL_PHASE=wva) applies: everything wva_prereq_kind_filter does not
# give to the admin.
#
# Filtered by OBJECT, not by kind name. NetworkPolicy/wva-external-scaler is
# the only NetworkPolicy in the render and is admin-owned by name
# (WVA_PREREQ_NAMED); dropping only WVA_PREREQ_KINDS left NetworkPolicy in the
# tenant's list, so INSTALL_PHASE=wva and --check demanded `create
# networkpolicies` for an object that phase never applies.
wva_rendered_kinds() {
    if [ "${1:-}" = controller ]; then
        wva_render_manifests \
            | yq "$(wva_prereq_kind_filter exclude) | .kind" 2>/dev/null \
            | grep -Ev '^(---|null|)$' | sort -u || true
        return 0
    fi
    # Only column 0 — `kind:` also appears indented under roleRef, and with a
    # leading dash under subjects.
    wva_render_manifests | awk '/^kind: /{print $2}' | sort -u || true
}

# wva_launcher_scrapers echoes the name of every PodMonitor in $1 that would
# actually generate a scrape target for an FMA launcher pod. $2, if given, is a
# PodMonitor name to ignore.
#
# "Would generate a target" is the whole point, and it is NOT the same as
# "selects". llm-d's own vllm-<model> PodMonitor selects a launcher the moment it
# binds -- the serving labels are stamped on it then -- but names its endpoint by
# port NAME, and a launcher declares no container ports, so the operator resolves
# nothing and produces no target. A caller asking "is anything scraping these?"
# would get the wrong answer from a selector test, in both directions: it would
# report scraping where there is none, and refuse to add scraping where it is
# needed.
#
# So a monitor counts only if it could reach the pod:
#   - it sets targetPort (a number bypasses the named-port lookup), or
#   - it rewrites __address__ in a relabeling, or
#   - it names a port that a launcher pod actually declares.
#
# Selectors are resolved by asking the API server rather than by comparing label
# maps, so a monitor that reaches launchers by any combination is caught.
# matchExpressions are not evaluated -- kubectl takes no expression selector -- so
# an expression-only monitor can slip through. This is a guard for the realistic
# case, not a proof, and both callers treat it as advisory.
#
# Echoes nothing when there are no launcher pods: with nothing to resolve against,
# "no monitor reaches them" is the only answer that can be defended.
wva_launcher_scrapers() {
    local ns="$1" exclude="${2:-}" launchers launcher_ports pm sel rewrites ports matched p
    launchers=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        -o name 2>/dev/null)
    [ -n "$launchers" ] || return 0

    launcher_ports=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        -o jsonpath='{range .items[*].spec.containers[*].ports[*]}{.name}{"\n"}{end}' 2>/dev/null \
        | grep -v '^$' | sort -u)

    kubectl get podmonitor -n "$ns" -o json 2>/dev/null \
      | jq -r --arg ex "$exclude" '
          .items[]
          | select($ex == "" or .metadata.name != $ex)
          | [ .metadata.name,
              ((.spec.selector.matchLabels // {}) | to_entries
                | map("\(.key)=\(.value)") | join(",")),
              ( [ (.spec.podMetricsEndpoints // [])[]
                  | (.targetPort != null)
                    or ((.relabelings // []) | any(.targetLabel == "__address__")) ]
                | any ),
              ( [ (.spec.podMetricsEndpoints // [])[] | .port // empty ] | join(" ") ) ]
          | @tsv' 2>/dev/null \
      | while IFS=$'\t' read -r pm sel rewrites ports; do
            [ -n "$sel" ] || continue
            matched=$(kubectl get pods -n "$ns" -l "$sel" -o name 2>/dev/null)
            [ -n "$matched" ] || continue
            printf '%s\n' "$matched" | grep -Fxq -f <(printf '%s\n' "$launchers") || continue
            if [ "$rewrites" = "true" ]; then
                printf '%s\n' "$pm"
                continue
            fi
            for p in $ports; do
                if printf '%s\n' "$launcher_ports" | grep -Fxq -- "$p"; then
                    printf '%s\n' "$pm"
                    break
                fi
            done
        done
}

# wva_rendered_prereq_objects echoes "<Kind> <name>" for every admin-owned object
# this install needs, at the names it will really have.
wva_rendered_prereq_objects() {
    # yq writes a `---` between documents even when each renders to one scalar.
    wva_render_manifests \
        | yq "$(wva_prereq_kind_filter select) | .kind + \" \" + .metadata.name" 2>/dev/null \
        | grep -Ev '^(---|null)' || true
}
