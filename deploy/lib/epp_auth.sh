#!/usr/bin/env bash
#
# Cluster-admin action: let each EPP (endpoint picker) of the InferencePools
# this install manages authenticate the scrapes of its own /metrics.
#
# Requires funcs: log_info/log_success/log_warning, wva_install_scope,
# wva_watch_list, wva_ns_suffix, wva_make_cmd (common.sh).
# Optional vars: WVA_WATCH_NS, NAMESPACE_EXPLICIT, DRY_RUN.
#
# Why: the EPP serves /metrics behind controller-runtime's authentication
# filter. For every scrape it sends a TokenReview (who is calling?) and a
# SubjectAccessReview (may they GET /metrics?) to the API server, as the EPP's
# OWN ServiceAccount. When that ServiceAccount may not create them, the filter
# cannot answer the question at all and replies 500 -- to WVA's direct scrape
# and to Prometheus alike, every cycle. A caller that lacks the right gets 401
# or 403 instead; 500 is always the EPP's side.
#
# llm-d's router chart grants it when it creates the ServiceAccount, but an EPP
# installed any other way (a hand-written Deployment, an older chart, another
# guide) may run as a ServiceAccount with only its namespaced Role. That is a
# cluster-scoped grant, so a namespace owner cannot make it; this is the admin
# step that does, for exactly the ServiceAccounts the managed pools run as.
#
# What it writes, when the ServiceAccount is not already allowed (or is
# allowed only through another install's binding), and never for the default
# ServiceAccount without WVA_EPP_METRICS_AUTH_DEFAULT_SA=true. The ClusterRole
# grants what the built-in system:auth-delegator grants:
#
#   ClusterRole        wva-epp-metrics-auth: create tokenreviews and
#                      subjectaccessreviews, nothing else. Shared.
#   ClusterRoleBinding one per install and ServiceAccount, named
#                      wva-epp-metrics-auth-<namespace>-<hash>, labelled
#                      llm-d.ai/applied-by=wva-epp-metrics-auth and
#                      llm-d.ai/applied-for=<controller namespace>, which is how
#                      undeploy-wva finds exactly these (wva_remove_epp_metrics_auth).
#
# Not optimized-baseline-epp-tokenreview (deploy/lib/epp-tokenreview-rbac.yaml):
# that role belongs to the test EPP the e2e installs, and undeploy_epp removes
# it with that EPP. Sharing it would tie a user's EPP to the test fixture.

WVA_EPP_AUTH_ROLE="wva-epp-metrics-auth"
WVA_EPP_AUTH_MARK_KEY="llm-d.ai/applied-by"
WVA_EPP_AUTH_MARK_VAL="wva-epp-metrics-auth"
WVA_EPP_AUTH_OWNER_KEY="llm-d.ai/applied-for"
WVA_EPP_AUTH_POOL_KINDS="inferencepools.inference.networking.k8s.io inferencepools.inference.networking.x-k8s.io"

# wva_epp_auth_wanted reports whether the admin phase grants it.
# WVA_EPP_METRICS_AUTH=false turns it off.
wva_epp_auth_wanted() {
    [ "${WVA_EPP_METRICS_AUTH:-true}" != false ]
}

# wva_epp_auth_namespaces echoes the namespaces this install manages, one per
# line, the way so_target_namespaces resolves them: "*" for a cluster-scoped
# install that names none (every namespace), else WVA_WATCH_NS, else a NAMESPACE
# given beside WVA_NS, else WVA_NS.
wva_epp_auth_namespaces() {
    if [ "$(wva_install_scope)" = cluster ] && [ -z "${WVA_WATCH_NS:-}" ]; then
        echo '*'
    elif [ -n "${WVA_WATCH_NS:-}" ]; then
        wva_watch_list
    elif [ -n "${NAMESPACE_EXPLICIT:-}" ] && [ "$(wva_install_scope)" = namespace ]; then
        echo "$NAMESPACE_EXPLICIT"
    else
        echo "$WVA_NS"
    fi
}

# wva_epp_auth_err_kind classifies kubectl stderr in file $1: forbidden,
# notfound (the object), noresource (the API is not served: no such CRD), or
# other.
wva_epp_auth_err_kind() {
    if grep -qi 'forbidden' "$1"; then
        echo forbidden
    elif grep -qiE "doesn.t have a resource type|could not find the requested resource|no matches for kind" "$1"; then
        echo noresource
    elif grep -qiE 'NotFound|not found' "$1"; then
        echo notfound
    else
        echo other
    fi
}

# wva_epp_auth_plan echoes, tab-separated, one line per finding for the
# namespaces in $1 (wva_epp_auth_namespaces):
#
#   sa       <ns> <serviceaccount> <pool>  an EPP of <pool> runs as it
#   nopicker <ns> <pool> <why>             the pool names no endpoint picker
#                                          Service (none, or another kind)
#   missing  <ns> <pool> <why>             the EPP cannot be resolved (left ungranted)
#   unknown  <ns> <pool> <why>             it could not be read
#   nolist   <ns> - <why>                  the pools could not be listed
#
# Both InferencePool API groups are read. A group the cluster does not serve
# is skipped; a list that is refused is nolist, never "no pools": forbidden is
# not absent. Neither is an answer that does not parse: that is unknown.
#
# The namespaces are read one per line, never word-split: "*" (every
# namespace) in a `for` loop expanded to the file names of the current
# directory, so a cluster-scoped run listed pools in namespaces named after
# files, found none, and reported success.
wva_epp_auth_plan() {
    local namespaces="$1" ns kind errf json rc where pools found pns pool svc refkind
    errf="$(mktemp)"
    while IFS= read -r ns; do
        [ -n "$ns" ] || continue
        if [ "$ns" = '*' ]; then where=(-A); else where=(-n "$ns"); fi
        pools=""
        for kind in $WVA_EPP_AUTH_POOL_KINDS; do
            rc=0
            json="$(kubectl get "$kind" "${where[@]}" -o json 2>"$errf")" || rc=$?
            if [ "$rc" -ne 0 ]; then
                case "$(wva_epp_auth_err_kind "$errf")" in
                    noresource) continue ;;
                    forbidden) printf 'nolist\t%s\t-\tlisting %s is forbidden\n' "$ns" "$kind" ;;
                    *) printf 'nolist\t%s\t-\t%s\n' "$ns" "$(head -1 "$errf")" ;;
                esac
                continue
            fi
            rc=0
            found="$(printf '%s' "$json" | jq -r '.items[]
                | ((.spec.endpointPickerRef // .spec.extensionRef // {})) as $r
                | [.metadata.namespace, .metadata.name, ($r.name // ""), ($r.kind // "Service")]
                | @tsv' 2>/dev/null)" || rc=$?
            if [ "$rc" -ne 0 ]; then
                printf 'unknown\t%s\t-\tcould not read the %s list (not JSON with .items)\n' "$ns" "$kind"
                continue
            fi
            pools="${pools}${found}"$'\n'
        done
        while IFS=$'\t' read -r pns pool svc refkind; do
            [ -n "$pns" ] || continue
            if [ -z "$svc" ]; then
                printf 'nopicker\t%s\t%s\tthe pool names no endpoint picker\n' "$pns" "$pool"
                continue
            fi
            if [ "$refkind" != Service ]; then
                printf 'nopicker\t%s\t%s\tits endpoint picker is a %s, not a Service, so its EPP is not resolved here\n' "$pns" "$pool" "$refkind"
                continue
            fi
            wva_epp_auth_pool_sas "$pns" "$pool" "$svc" "$errf"
        done <<< "$(printf '%s' "$pools" | sort -u)"
    done <<< "$namespaces"
    rm -f "$errf"
}

# wva_epp_auth_pool_sas echoes the plan lines (wva_epp_auth_plan) for pool $2 in
# namespace $1, whose endpoint picker is Service $3: the Service selector, then
# the Deployments whose pod template it selects, else the Pods it selects, and
# the ServiceAccount each runs as (default when none is named). $4 is a scratch
# file for stderr.
wva_epp_auth_pool_sas() {
    local ns="$1" pool="$2" svc="$3" errf="$4" json rc sel sas
    rc=0
    json="$(kubectl get service "$svc" -n "$ns" -o json 2>"$errf")" || rc=$?
    if [ "$rc" -ne 0 ]; then
        case "$(wva_epp_auth_err_kind "$errf")" in
            notfound) printf 'missing\t%s\t%s\tits endpoint picker Service %s does not exist\n' "$ns" "$pool" "$svc" ;;
            *) printf 'unknown\t%s\t%s\tcould not read Service %s: %s\n' "$ns" "$pool" "$svc" "$(head -1 "$errf")" ;;
        esac
        return 0
    fi
    if ! sel="$(printf '%s' "$json" | jq -c '.spec.selector // {}' 2>/dev/null)"; then
        printf 'unknown\t%s\t%s\tcould not read Service %s (not JSON)\n' "$ns" "$pool" "$svc"
        return 0
    fi
    if [ -z "$sel" ] || [ "$sel" = '{}' ]; then
        printf 'missing\t%s\t%s\tService %s has no selector, so its EPP cannot be found\n' "$ns" "$pool" "$svc"
        return 0
    fi
    rc=0
    json="$(kubectl get deployments -n "$ns" -o json 2>"$errf")" || rc=$?
    if [ "$rc" -ne 0 ]; then
        printf 'unknown\t%s\t%s\tcould not list Deployments: %s\n' "$ns" "$pool" "$(head -1 "$errf")"
        return 0
    fi
    if ! sas="$(printf '%s' "$json" | jq -r --argjson sel "$sel" '.items[]
        | select((.spec.template.metadata.labels // {}) as $l
                 | $sel | to_entries | all(.value == $l[.key]))
        | .spec.template.spec.serviceAccountName // .spec.template.spec.serviceAccount // "default"' 2>/dev/null)"; then
        printf 'unknown\t%s\t%s\tcould not read the Deployment list (not JSON with .items)\n' "$ns" "$pool"
        return 0
    fi
    sas="$(printf '%s\n' "$sas" | grep -v '^$' | sort -u)"
    if [ -z "$sas" ]; then
        # Not a Deployment (a StatefulSet, a bare Pod): the running Pods say.
        rc=0
        json="$(kubectl get pods -n "$ns" -l "$(printf '%s' "$sel" | jq -r 'to_entries | map(.key + "=" + .value) | join(",")')" -o json 2>"$errf")" || rc=$?
        if [ "$rc" -ne 0 ]; then
            printf 'unknown\t%s\t%s\tcould not list Pods: %s\n' "$ns" "$pool" "$(head -1 "$errf")"
            return 0
        fi
        if ! sas="$(printf '%s' "$json" | jq -r '.items[] | .spec.serviceAccountName // .spec.serviceAccount // "default"' 2>/dev/null)"; then
            printf 'unknown\t%s\t%s\tcould not read the Pod list (not JSON with .items)\n' "$ns" "$pool"
            return 0
        fi
        sas="$(printf '%s\n' "$sas" | grep -v '^$' | sort -u)"
    fi
    if [ -z "$sas" ]; then
        printf 'missing\t%s\t%s\tno Deployment or Pod matches the selector of Service %s\n' "$ns" "$pool" "$svc"
        return 0
    fi
    printf '%s\n' "$sas" | while IFS= read -r sa; do
        [ -n "$sa" ] && printf 'sa\t%s\t%s\t%s\n' "$ns" "$sa" "$pool"
    done
}

# wva_epp_auth_can echoes yes, no or unknown: whether ServiceAccount $2 in
# namespace $1 may create both tokenreviews and subjectaccessreviews. Asked with
# kubectl auth can-i --as, which needs the right to impersonate: refused, that
# is unknown, never no.
wva_epp_auth_can() {
    local res out errf
    errf="$(mktemp)"
    for res in tokenreviews.authentication.k8s.io subjectaccessreviews.authorization.k8s.io; do
        out="$(kubectl auth can-i create "$res" --as="system:serviceaccount:$1:$2" 2>"$errf")" || true
        case "$out" in
            yes*) continue ;;
            no*) rm -f "$errf"; echo no; return 0 ;;
            *) rm -f "$errf"; echo unknown; return 0 ;;
        esac
    done
    rm -f "$errf"
    echo yes
}

# wva_epp_auth_binding_name echoes the ClusterRoleBinding name for
# ServiceAccount $2 in namespace $1, for this install (WVA_NS). The hash keeps
# two installs, and a namespace and ServiceAccount whose names run together,
# apart.
wva_epp_auth_binding_name() {
    printf '%s-%s-%s\n' "$WVA_EPP_AUTH_ROLE" "$1" "$(wva_ns_suffix "$WVA_NS/$1/$2")"
}

# wva_epp_auth_role_doc renders the shared ClusterRole.
wva_epp_auth_role_doc() {
    cat <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${WVA_EPP_AUTH_ROLE}
  labels:
    ${WVA_EPP_AUTH_MARK_KEY}: ${WVA_EPP_AUTH_MARK_VAL}
rules:
- apiGroups: ["authentication.k8s.io"]
  resources: ["tokenreviews"]
  verbs: ["create"]
- apiGroups: ["authorization.k8s.io"]
  resources: ["subjectaccessreviews"]
  verbs: ["create"]
EOF
}

# wva_epp_auth_binding_doc renders the ClusterRoleBinding for ServiceAccount $2
# in namespace $1.
wva_epp_auth_binding_doc() {
    cat <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $(wva_epp_auth_binding_name "$1" "$2")
  labels:
    ${WVA_EPP_AUTH_MARK_KEY}: ${WVA_EPP_AUTH_MARK_VAL}
    ${WVA_EPP_AUTH_OWNER_KEY}: ${WVA_NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${WVA_EPP_AUTH_ROLE}
subjects:
- kind: ServiceAccount
  name: $2
  namespace: $1
EOF
}

# wva_epp_auth_manual_cmds echoes the kubectl commands that grant ServiceAccount
# $2 in namespace $1 by hand: the same objects this writes, labels included.
wva_epp_auth_manual_cmds() {
    local name
    name="$(wva_epp_auth_binding_name "$1" "$2")"
    printf '    kubectl create clusterrole %s --verb=create --resource=tokenreviews.authentication.k8s.io,subjectaccessreviews.authorization.k8s.io\n' "$WVA_EPP_AUTH_ROLE"
    printf '    kubectl create clusterrolebinding %s --clusterrole=%s --serviceaccount=%s:%s\n' "$name" "$WVA_EPP_AUTH_ROLE" "$1" "$2"
    printf '    kubectl label clusterrolebinding %s %s=%s %s=%s\n' "$name" "$WVA_EPP_AUTH_MARK_KEY" "$WVA_EPP_AUTH_MARK_VAL" "$WVA_EPP_AUTH_OWNER_KEY" "$WVA_NS"
}


# wva_epp_auth_wva_owners echoes, one per line, the controller namespace
# (llm-d.ai/applied-for) of every binding this tool made for ServiceAccount $2
# in namespace $1, whichever install made it. Returns 1 when the bindings
# cannot be read.
wva_epp_auth_wva_owners() {
    local json
    json="$(kubectl get clusterrolebindings -l "${WVA_EPP_AUTH_MARK_KEY}=${WVA_EPP_AUTH_MARK_VAL}" -o json 2>/dev/null)" || return 1
    printf '%s' "$json" | jq -r --arg ns "$1" --arg sa "$2" --arg ok "$WVA_EPP_AUTH_OWNER_KEY" '.items[]
        | select(any(.subjects[]?; .kind == "ServiceAccount" and .name == $sa and .namespace == $ns))
        | .metadata.labels[$ok] // ""' 2>/dev/null || return 1
}

# wva_epp_auth_default_ok reports whether the default ServiceAccount may be
# bound: only with WVA_EPP_METRICS_AUTH_DEFAULT_SA=true. Every pod in the
# namespace that names no ServiceAccount runs as default (the model servers,
# typically), so binding it gives all of them the reviews, not just the EPP.
wva_epp_auth_default_ok() {
    [ "${WVA_EPP_METRICS_AUTH_DEFAULT_SA:-false}" = true ]
}

# wva_grant_epp_metrics_auth is make epp-metrics-auth, and a step of the admin
# phase (setup-prereqs). For every InferencePool this install manages it finds
# the ServiceAccount the pool's EPP runs as (wva_epp_auth_plan) and binds it to
# wva-epp-metrics-auth with this install's own binding, unless it may already
# create tokenreviews and subjectaccessreviews through a grant this tool did
# not make, or through this install's binding. One allowed only through ANOTHER
# install's binding gets this install's too: that install's undeploy removes
# its own, and would otherwise put this EPP back on 500 without a word. One
# binding per ServiceAccount, however many pools share it; applying it again
# changes nothing.
#
# The default ServiceAccount is bound only with
# WVA_EPP_METRICS_AUTH_DEFAULT_SA=true (wva_epp_auth_default_ok); otherwise it
# is left ungranted, with a warning naming the EPP.
#
# DRY_RUN=true writes nothing and prints the plan and the commands; any value
# other than true or false is refused before anything is read. Returns 0 when
# every EPP is allowed (or there is none), 1 when one is left ungranted (a dry
# run with work to do, an EPP that cannot be resolved, the default
# ServiceAccount without the opt-in, an apply that failed), 2 when DRY_RUN is
# not true or false or the pools cannot be listed, 3 when an EPP or its
# permission could not be checked.
wva_grant_epp_metrics_auth() {
    local dry="${DRY_RUN:-false}"
    case "$dry" in
        true|false) ;;
        *) log_warning "DRY_RUN must be true or false, got $dry: nothing was read or changed."
           return 2 ;;
    esac
    local namespaces plan kind ns a b seen="" st owners why role_applied=false
    local nolist=0 ungranted=0 unknown=0 todo=0 report="" todo_cmds="" defaults="" others=""
    namespaces="$(wva_epp_auth_namespaces)"
    plan="$(wva_epp_auth_plan "$namespaces")"

    while IFS=$'\t' read -r kind ns a b; do
        case "$kind" in
            nolist)
                nolist=$((nolist + 1))
                report="${report}    could not list  $([ "$ns" = '*' ] && echo 'all namespaces' || echo "$ns"): $b"$'\n' ;;
            nopicker)
                report="${report}    no EPP          $ns/$a: $b"$'\n'
                case "$b" in *"not a Service"*) others="${others}${ns}/${a}: ${b}"$'\n' ;; esac ;;
            missing)
                ungranted=$((ungranted + 1))
                report="${report}    unresolved      $ns/$a: $b"$'\n' ;;
            unknown)
                unknown=$((unknown + 1))
                report="${report}    unknown         $ns/$a: $b"$'\n' ;;
            sa)
                case " $seen " in *" $ns/$a "*) continue ;; esac
                seen="$seen $ns/$a"
                st="$(wva_epp_auth_can "$ns" "$a")"
                why="may create the reviews: $st"
                if [ "$st" = yes ]; then
                    if ! owners="$(wva_epp_auth_wva_owners "$ns" "$a")"; then
                        # Cannot tell whose grant it is: binding our own as
                        # well costs nothing and cannot be undone by another.
                        why="allowed, but whose grant could not be read"
                    elif printf '%s\n' "$owners" | grep -qxF -- "$WVA_NS"; then
                        report="${report}    allowed         $ns/$a (EPP of $b; this install's binding)"$'\n'
                        continue
                    elif [ -z "$owners" ]; then
                        report="${report}    allowed         $ns/$a (EPP of $b)"$'\n'
                        continue
                    else
                        why="allowed only through the binding of the install in $(printf '%s' "$owners" | grep -v '^$' | paste -sd, -), which its undeploy removes"
                    fi
                fi
                if [ "$a" = default ] && ! wva_epp_auth_default_ok; then
                    if [ "$st" = yes ]; then
                        report="${report}    allowed         $ns/$a (EPP of $b; $why; not adding this install's own for the default ServiceAccount)"$'\n'
                    else
                        ungranted=$((ungranted + 1))
                        defaults="${defaults}${ns} (EPP of ${b})"$'\n'
                        report="${report}    not bound       $ns/$a (EPP of $b): the default ServiceAccount, see below"$'\n'
                    fi
                    continue
                fi
                if [ "$dry" = true ]; then
                    todo=$((todo + 1))
                    todo_cmds="${todo_cmds}$(wva_epp_auth_manual_cmds "$ns" "$a")"$'\n'
                    report="${report}    would bind      $ns/$a (EPP of $b; $why)"$'\n'
                    continue
                fi
                if [ "$role_applied" = false ]; then
                    if wva_epp_auth_role_doc | kubectl apply -f - >/dev/null; then
                        role_applied=true
                    else
                        log_warning "Could not apply ClusterRole $WVA_EPP_AUTH_ROLE."
                    fi
                fi
                if [ "$role_applied" = true ] && wva_epp_auth_binding_doc "$ns" "$a" | kubectl apply -f - >/dev/null; then
                    report="${report}    granted         $ns/$a (EPP of $b; $why): ClusterRoleBinding $(wva_epp_auth_binding_name "$ns" "$a")"$'\n'
                else
                    ungranted=$((ungranted + 1))
                    report="${report}    failed          $ns/$a (EPP of $b): the binding could not be applied"$'\n'
                fi ;;
        esac
    done <<< "$plan"

    if [ -z "$report" ]; then
        log_info "No InferencePool in $(printf '%s\n' "$namespaces" | sed 's/^\*$/any namespace/' | paste -sd' ' -): no EPP to grant."
        return 0
    fi
    echo "EPP ServiceAccounts and their /metrics authentication (create tokenreviews, subjectaccessreviews):"
    printf '%s' "$report"
    if [ -n "$others" ]; then
        log_warning "These pools name an endpoint picker that is not a Service, so their EPP was not resolved or granted; check its ServiceAccount by hand (docs/reference/troubleshooting.md#epp-metrics-returns-500):"
        printf '%s' "$others" | sed 's/^/    /' >&2
    fi
    if [ -n "$defaults" ]; then
        log_warning "Not bound: the EPP in these namespaces runs as the default ServiceAccount, and every pod there that names none (the model servers, typically) runs as it too. Binding it would give all of them the reviews:"
        printf '%s' "$defaults" | sed 's/^/    /' >&2
        log_warning "  Give the EPP its own ServiceAccount (serviceAccountName in its Deployment) and re-run, or accept the wider grant: $(wva_make_cmd epp-metrics-auth WVA_EPP_METRICS_AUTH_DEFAULT_SA=true)"
    fi
    if [ "$nolist" -gt 0 ]; then
        log_warning "Could not list the InferencePools above, so their EPPs were not checked or granted. Run this as a cluster admin."
        return 2
    fi
    if [ "$dry" = true ] && [ "$todo" -gt 0 ]; then
        log_info "DRY_RUN=true: nothing was changed. To grant, run  $(wva_make_cmd epp-metrics-auth)  or:"
        printf '%s' "$todo_cmds" | awk 'NF && !seen[$0]++'
        return 1
    fi
    if [ "$ungranted" -gt 0 ]; then
        log_warning "$ungranted EPP(s) left ungranted: their /metrics answers 500 to every scrape until their ServiceAccount may create tokenreviews and subjectaccessreviews."
        return 1
    fi
    [ "$unknown" -eq 0 ] || return 3
    return 0
}

# wva_apply_epp_metrics_auth runs wva_grant_epp_metrics_auth in the admin phase,
# when wanted. Never fatal: the checks name what is still missing.
wva_apply_epp_metrics_auth() {
    wva_epp_auth_wanted || return 0
    log_info "Letting the managed EPPs authenticate /metrics scrapes (WVA_EPP_METRICS_AUTH=false skips this):"
    local rc=0
    DRY_RUN=false wva_grant_epp_metrics_auth || rc=$?
    case "$rc" in
        0) ;;
        2) log_warning "  EPP /metrics authentication: the InferencePools could not be listed; re-run $(wva_make_cmd epp-metrics-auth) as a cluster admin." ;;
        *) log_warning "  EPP /metrics authentication: not every EPP is granted (see above); $(wva_make_cmd epp-metrics-auth) re-runs it." ;;
    esac
    return 0
}

# wva_report_epp_metrics_auth warns, never fails, for each EPP of a managed pool
# whose ServiceAccount may not create tokenreviews or subjectaccessreviews,
# naming the command that grants it. One whose permission cannot be asked
# (impersonation refused, the pools unreadable) is said to be unchecked, not
# fine. Used by check-prereqs and verify-deployment.
wva_report_epp_metrics_auth() {
    local plan kind ns a b seen="" st missing="" unchecked=0
    plan="$(wva_epp_auth_plan "$(wva_epp_auth_namespaces)")"
    while IFS=$'\t' read -r kind ns a b; do
        case "$kind" in
            nolist|unknown) unchecked=$((unchecked + 1)) ;;
            sa)
                case " $seen " in *" $ns/$a "*) continue ;; esac
                seen="$seen $ns/$a"
                st="$(wva_epp_auth_can "$ns" "$a")"
                case "$st" in
                    yes) ;;
                    no) missing="${missing}${ns}/${a}"$'\n' ;;
                    *) unchecked=$((unchecked + 1)) ;;
                esac ;;
        esac
    done <<< "$plan"
    if [ -n "$missing" ]; then
        log_warning "EPP /metrics authentication: these EPP ServiceAccounts may not create tokenreviews or subjectaccessreviews, so each EPP answers 500 to every /metrics scrape, from WVA and from Prometheus:"
        printf '%s' "$missing" | sed 's/^/    /' >&2
        log_warning "  A cluster admin grants it with  $(wva_make_cmd epp-metrics-auth)  (setup-prereqs does it too). See docs/reference/troubleshooting.md#epp-metrics-returns-500."
        if printf '%s' "$missing" | grep -q '/default$'; then
            log_warning "  An EPP running as the default ServiceAccount is not bound without WVA_EPP_METRICS_AUTH_DEFAULT_SA=true: give it its own ServiceAccount instead."
        fi
    fi
    if [ "$unchecked" -gt 0 ]; then
        log_info "EPP /metrics authentication: $unchecked EPP(s) could not be checked (listing them, or asking as their ServiceAccount, is not permitted for you). Not a pass; $(wva_make_cmd epp-metrics-auth "DRY_RUN=true") shows them as a cluster admin."
    fi
    return 0
}

# wva_remove_epp_metrics_auth deletes the bindings this install (WVA_NS) made,
# by their labels, and the shared ClusterRole once no binding uses it. Called
# by undeploy-wva. When the bindings cannot be read (a namespace owner's
# uninstall) it says what a cluster admin runs, without a warning: this install
# may never have made any.
wva_remove_epp_metrics_auth() {
    local sel="${WVA_EPP_AUTH_MARK_KEY}=${WVA_EPP_AUTH_MARK_VAL},${WVA_EPP_AUTH_OWNER_KEY}=${WVA_NS}" mine out
    if ! mine="$(kubectl get clusterrolebindings -l "$sel" -o name 2>&1)"; then
        log_info "EPP /metrics authentication bindings are cluster-scoped and could not be read here. If setup-prereqs made any for this install, a cluster admin removes them with: kubectl delete clusterrolebindings -l $sel"
        return 0
    fi
    [ -n "$mine" ] || return 0
    if ! out="$(kubectl delete clusterrolebindings -l "$sel" --ignore-not-found 2>&1)"; then
        log_warning "Could not remove the EPP /metrics authentication bindings this install made: $(printf '%s' "$out" | head -1)"
        log_warning "  A cluster admin removes them with: kubectl delete clusterrolebindings -l $sel"
        return 0
    fi
    local bound
    # A list that cannot be read keeps the ClusterRole: another install may
    # still bind it.
    if bound="$(kubectl get clusterrolebindings -o jsonpath='{range .items[*]}{.roleRef.name}{"\n"}{end}' 2>/dev/null)" \
        && ! printf '%s\n' "$bound" | grep -qxF -- "$WVA_EPP_AUTH_ROLE"; then
        # Only the one this wrote, by its label.
        if kubectl get clusterroles -l "${WVA_EPP_AUTH_MARK_KEY}=${WVA_EPP_AUTH_MARK_VAL}" -o name 2>/dev/null \
            | grep -qxF -- "clusterrole.rbac.authorization.k8s.io/$WVA_EPP_AUTH_ROLE"; then
            kubectl delete clusterrole "$WVA_EPP_AUTH_ROLE" --ignore-not-found >/dev/null 2>&1 || true
        fi
    fi
    log_info "Removed the EPP /metrics authentication grants this install made (label ${WVA_EPP_AUTH_OWNER_KEY}=${WVA_NS})."
}
