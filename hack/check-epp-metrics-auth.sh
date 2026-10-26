#!/usr/bin/env bash
#
# Executes the EPP /metrics authentication grant (wva_grant_epp_metrics_auth in
# deploy/lib/epp_auth.sh, behind make epp-metrics-auth and a step of
# setup-prereqs) against a STUBBED kubectl.
#
# The EPP checks every /metrics scrape with a TokenReview and a
# SubjectAccessReview made as its own ServiceAccount. When that ServiceAccount
# may not create them, the EPP answers 500 to WVA and to Prometheus. The grant
# binds exactly the ServiceAccounts the managed pools run as. What this pins,
# each of which parses cleanly and is wrong on a cluster:
#
#   two pools, two ServiceAccounts   one binding each, to the pool's own one
#   no serviceAccountName            default is NOT bound without
#                                    WVA_EPP_METRICS_AUTH_DEFAULT_SA=true, exit 1
#   EPP Service missing              left ungranted, exit 1; the others still bound
#   pools list Forbidden             exit 2, never "no InferencePool"
#   already allowed                  nothing applied, and a second run applies nothing
#   allowed via another install      this install's own binding is applied too
#   DRY_RUN                          nothing applied, the commands printed, exit 1;
#                                    a value other than true or false is refused
#   both InferencePool API groups    endpointPickerRef and extensionRef both read,
#                                    a group the cluster does not serve skipped
#   cluster scope                    lists with -A, run from a directory holding
#                                    files (a glob of "*" found none and exited 0)
#   a picker that is not a Service   skipped with a warning
#   unreadable                       a Service read Forbidden, or a pool list that
#                                    is not JSON: exit 3, never "no pools"
#   the check                        warns on a denied ServiceAccount, and says
#                                    "could not be checked" when impersonation is refused
#   undeploy                         deletes by this install's label only; keeps the
#                                    ClusterRole another binding uses; says nothing
#                                    alarming when the bindings cannot be read
#
# Every case runs from the repository root, which holds files. Every case
# asserts something that a missing function also fails, so a run against code
# without this feature fails rather than passes empty.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
PASS=0
CASES=0
# EXPECTED_CASES is how many cases this file runs. A case that silently stops
# running (a renamed function, an early exit) must fail the check.
EXPECTED_CASES=18
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
pass() { PASS=$((PASS + 1)); }
ok() { if eval "$1"; then pass; else fail "$2"; fi; }
begin() { CASES=$((CASES + 1)); echo "== $*"; }

if ! command -v jq >/dev/null 2>&1; then
    echo "FAIL: jq is required (not skipping: a skipped check reads as a pass)"
    exit 1
fi

# Sourced through CR-stripped copies: a Windows checkout can write these as
# CRLF. A file that does not exist becomes an empty one, so every case fails on
# its missing function instead of the run aborting.
for f in common epp_auth; do
    if [ -f "$ROOT/deploy/lib/$f.sh" ]; then
        tr -d '\r' <"$ROOT/deploy/lib/$f.sh" >"$WORK/$f.sh"
    else
        : >"$WORK/$f.sh"
    fi
done

NS=wva-check
V1=inferencepools.inference.networking.k8s.io
VX=inferencepools.inference.networking.x-k8s.io
MARK=llm-d.ai/applied-by=wva-epp-metrics-auth

forbidden() { echo "Error from server (Forbidden): $1 is forbidden: User \"tenant\" cannot list resource \"$1\"" >&2; }

# pool <api> <ns> <name> <service> [kind]: one InferencePool. v1 names its
# picker in endpointPickerRef, x-k8s in extensionRef; kind is left out unless given.
pool() {
    local f="$WORK/pools.$1.json" ref=endpointPickerRef
    [ "$1" = x ] && ref=extensionRef
    [ -f "$f" ] || echo '{"items":[]}' >"$f"
    jq --arg ns "$2" --arg n "$3" --arg s "$4" --arg r "$ref" --arg k "${5:-}" \
        '.items += [{metadata: {namespace: $ns, name: $n}, spec: (if $s == "-" then {} else {($r): ({name: $s} + (if $k == "" then {} else {kind: $k} end))} end)}]' "$f" >"$f.tmp" && mv "$f.tmp" "$f"
}

# epp <ns> <service> <app label> <serviceaccount|-> [kind]: the Service a pool
# names, and a Deployment (or, with kind=pod, only a Pod) it selects, running
# as <serviceaccount>; - names none.
epp() {
    local ns="$1" svc="$2" app="$3" sa="$4" kind="${5:-deploy}" spec
    jq -n --arg app "$app" '{spec: {selector: {app: $app}}}' >"$WORK/svc.$ns.$svc.json"
    if [ "$sa" = - ]; then spec='{}'; else spec="$(jq -n --arg sa "$sa" '{serviceAccountName: $sa}')"; fi
    local f="$WORK/$kind.$ns.json"
    [ -f "$f" ] || echo '{"items":[]}' >"$f"
    if [ "$kind" = pod ]; then
        jq --arg app "$app" --argjson s "$spec" '.items += [{metadata: {labels: {app: $app}}, spec: $s}]' "$f" >"$f.tmp"
    else
        jq --arg app "$app" --argjson s "$spec" '.items += [{metadata: {name: ($app + "-epp")}, spec: {template: {metadata: {labels: {app: $app, extra: "x"}}, spec: $s}}}]' "$f" >"$f.tmp"
    fi
    mv "$f.tmp" "$f"
}

# allowed <ns>:<sa>: already may create the reviews (granted by its chart).
allowed() { echo "$1" >>"$WORK/allowed"; }

# crb <name> <owner|-> <ns> <sa> [role]: a ClusterRoleBinding already on the
# cluster. With an owner it is one this tool made for that install.
crb() {
    [ -f "$WORK/crbs.json" ] || echo '{"items":[]}' >"$WORK/crbs.json"
    jq --arg n "$1" --arg o "$2" --arg ns "$3" --arg sa "$4" --arg r "${5:-wva-epp-metrics-auth}" '.items += [{
        metadata: {name: $n, labels: (if $o == "-" then {} else {"llm-d.ai/applied-by": "wva-epp-metrics-auth", "llm-d.ai/applied-for": $o} end)},
        roleRef: {name: $r}, subjects: [{kind: "ServiceAccount", name: $sa, namespace: $ns}]}]' \
        "$WORK/crbs.json" >"$WORK/crbs.tmp" && mv "$WORK/crbs.tmp" "$WORK/crbs.json"
    [ "${5:-wva-epp-metrics-auth}" != wva-epp-metrics-auth ] || allowed "$3:$4"
}

# crbs_matching <selector>: the stored bindings carrying every k=v of it.
crbs_matching() {
    [ -f "$WORK/crbs.json" ] || echo '{"items":[]}' >"$WORK/crbs.json"
    jq --arg sel "$1" '($sel | split(",") | map(split("=") | {(.[0]): .[1]}) | add) as $want
        | .items |= map(select((.metadata.labels // {}) as $l | $want | to_entries | all(.value == $l[.key])))' "$WORK/crbs.json"
}

stub_kubectl() {
    printf '%s\n' "$*" >>"$WORK/kubectl.log"
    local a="$*" api ns name who owner
    case "$a" in
        "get $V1 "*|"get $VX "*)
            api=v1; case "$a" in "get $VX "*) api=x ;; esac
            if [ "${STUB_POOLS:-}" = FORBIDDEN ]; then forbidden inferencepools; return 1; fi
            if [ "${STUB_POOLS:-}" = GARBAGE ]; then echo 'this is not json'; return 0; fi
            if [ "$api" = v1 ] && [ "${STUB_V1:-}" = NORES ]; then
                echo "error: the server doesn't have a resource type \"inferencepools\"" >&2; return 1
            fi
            set -- $a
            if [ "$3" = -A ]; then
                cat "$WORK/pools.$api.json" 2>/dev/null || echo '{"items":[]}'
            else
                jq --arg ns "$4" '.items |= map(select(.metadata.namespace == $ns))' "$WORK/pools.$api.json" 2>/dev/null || echo '{"items":[]}'
            fi
            ;;
        "get service "*)
            [ "${STUB_SVC:-}" != FORBIDDEN ] || { forbidden services; return 1; }
            set -- $a
            name="$3"; ns="$5"
            if [ -f "$WORK/svc.$ns.$name.json" ]; then cat "$WORK/svc.$ns.$name.json"
            else echo "Error from server (NotFound): services \"$name\" not found" >&2; return 1; fi
            ;;
        "get deployments -n "*)
            set -- $a
            cat "$WORK/deploy.$4.json" 2>/dev/null || echo '{"items":[]}'
            ;;
        "get pods -n "*)
            set -- $a
            cat "$WORK/pod.$4.json" 2>/dev/null || echo '{"items":[]}'
            ;;
        "auth can-i create "*)
            if [ "${STUB_IMPERSONATE:-}" = FORBIDDEN ]; then
                echo 'Error from server (Forbidden): users "system:serviceaccount:x:y" is forbidden: User "tenant" cannot impersonate resource "users"' >&2
                return 1
            fi
            who="${a##*--as=system:serviceaccount:}"
            if grep -qxF -- "$who" "$WORK/allowed" 2>/dev/null; then echo yes; else echo no; return 1; fi
            ;;
        "apply -f -")
            local doc
            doc="$(cat)"
            printf '%s\n---\n' "$doc" >>"$WORK/applied"
            case "$doc" in
                *"kind: ClusterRoleBinding"*)
                    ns="$(printf '%s\n' "$doc" | sed -n 's/^  namespace: //p')"
                    who="$(printf '%s\n' "$doc" | sed -n 's/^  name: //p' | tail -1)"
                    name="$(printf '%s\n' "$doc" | sed -n 's/^  name: //p' | head -1)"
                    owner="$(printf '%s\n' "$doc" | sed -n 's/^    llm-d.ai\/applied-for: //p')"
                    crb "$name" "$owner" "$ns" "$who" ;;
            esac
            ;;
        "get clusterrolebindings -l "*)
            [ "${STUB_CRB:-}" != FORBIDDEN ] || { forbidden clusterrolebindings; return 1; }
            set -- $a
            case "$a" in
                *"-o name") crbs_matching "$4" | jq -r '.items[] | "clusterrolebinding.rbac.authorization.k8s.io/" + .metadata.name' ;;
                *) crbs_matching "$4" ;;
            esac
            ;;
        "get clusterrolebindings -o jsonpath="*)
            [ -f "$WORK/crbs.json" ] && jq -r '.items[].roleRef.name' "$WORK/crbs.json"
            echo other-role
            ;;
        "delete clusterrolebinding "*)
            set -- $a
            shift 2
            printf '%s\n' "$@" | grep -v -- '^--' >"$WORK/deleted"
            jq --slurpfile d <(jq -R . "$WORK/deleted" | jq -s .) '.items |= map(select(.metadata.name as $n | $d[0] | index($n) | not))' \
                "$WORK/crbs.json" >"$WORK/crbs.tmp" && mv "$WORK/crbs.tmp" "$WORK/crbs.json"
            ;;
        "delete clusterrolebindings -l "*)
            set -- $a
            crbs_matching "$4" | jq -r '.items[].metadata.name' >"$WORK/deleted"
            jq --slurpfile d <(jq -R . "$WORK/deleted" | jq -s .) '.items |= map(select(.metadata.name as $n | $d[0] | index($n) | not))' \
                "$WORK/crbs.json" >"$WORK/crbs.tmp" && mv "$WORK/crbs.tmp" "$WORK/crbs.json"
            ;;
        "get clusterroles "*) echo "clusterrole.rbac.authorization.k8s.io/wva-epp-metrics-auth" ;;
        "delete "*) : ;;
        *) return 0 ;;
    esac
}

# run <function> [VAR=value...]: runs the function in a subshell with the
# libraries sourced and kubectl stubbed, from the repository root (a directory
# that holds files). stdout lands in $WORK/stdout, warnings in $WORK/warnings,
# info in $WORK/info. Sets RC.
run() {
    local fn="$1"
    shift
    : >"$WORK/kubectl.log"
    : >"$WORK/warnings"
    : >"$WORK/info"
    : >"$WORK/applied"
    : >"$WORK/stdout"
    (
        cd "$ROOT" || exit 9
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        # shellcheck disable=SC1091
        . "$WORK/epp_auth.sh"
        log_info() { echo "$1" >>"$WORK/info"; }
        log_success() { echo "$1" >>"$WORK/info"; }
        log_warning() { echo "$1" >>"$WORK/warnings"; }
        log_error() { echo "$1" >>"$WORK/warnings"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        WVA_NS=$NS
        WVA_SCOPE=namespace
        WVA_WATCH_NS=apps
        unset DRY_RUN NAMESPACE_EXPLICIT STUB_POOLS STUB_V1 STUB_IMPERSONATE STUB_SVC STUB_CRB WVA_EPP_METRICS_AUTH_DEFAULT_SA
        local kv
        for kv in "$@"; do eval "$kv"; done
        declare -F "$fn" >/dev/null || { echo "$fn is not defined" >>"$WORK/warnings"; exit 3; }
        "$fn" >"$WORK/stdout" 2>>"$WORK/warnings"
    )
    RC=$?
}

reset() { rm -f "$WORK"/pools.*.json "$WORK"/svc.*.json "$WORK"/deploy.*.json "$WORK"/pod.*.json "$WORK/allowed" "$WORK/crbs.json" "$WORK/deleted"; }
# bound <ns> <sa>: a ClusterRoleBinding applied in this run for it, to
# wva-epp-metrics-auth, labelled for this install.
bound() {
    awk -v RS='---' -v ns="$1" -v sa="$2" -v own="$NS" '/kind: ClusterRoleBinding/ && /name: wva-epp-metrics-auth\n/ && index($0, "llm-d.ai/applied-for: " own "\n") && index($0, "  name: " sa "\n  namespace: " ns) { found = 1 } END { exit !found }' "$WORK/applied"
}
bindings() { grep -c '^kind: ClusterRoleBinding' "$WORK/applied"; }
roles() { grep -c '^kind: ClusterRole$' "$WORK/applied"; }

begin "two pools, two ServiceAccounts: each bound, the ClusterRole applied once"
reset
pool v1 apps chat chat-epp
pool v1 apps code code-epp
epp apps chat-epp chat chat-sa
epp apps code-epp code code-sa
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 0 ] && bound apps chat-sa && bound apps code-sa && [ "$(bindings)" = 2 ] && [ "$(roles)" = 1 ]' "two pools: exit $RC (want 0), $(bindings) bindings, $(roles) roles, applied $(cat "$WORK/applied")"
ok 'grep -q "granted .*apps/chat-sa" "$WORK/stdout" && grep -q "granted .*apps/code-sa" "$WORK/stdout"' "two pools: report missing: $(cat "$WORK/stdout")"

begin "the default ServiceAccount: not bound without the opt-in (exit 1, named), bound with it"
reset
pool v1 apps chat chat-epp
epp apps chat-epp chat -
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 1 ] && [ "$(bindings)" = 0 ] && grep -q "not bound .*apps/default" "$WORK/stdout" && grep -q "WVA_EPP_METRICS_AUTH_DEFAULT_SA=true" "$WORK/warnings" && grep -q "apps (EPP of chat)" "$WORK/warnings"' "default, no opt-in: exit $RC (want 1), applied $(cat "$WORK/applied"), warnings $(cat "$WORK/warnings")"
run wva_grant_epp_metrics_auth WVA_EPP_METRICS_AUTH_DEFAULT_SA=true
ok '[ "$RC" = 0 ] && bound apps default && [ "$(bindings)" = 1 ]' "default, opt-in: exit $RC (want 0), applied $(cat "$WORK/applied")"

begin "a pool whose EPP Service is missing: left ungranted, exit 1; the other pool still bound"
reset
pool v1 apps chat chat-epp
pool v1 apps gone gone-epp
epp apps chat-epp chat chat-sa
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 1 ] && bound apps chat-sa && [ "$(bindings)" = 1 ] && grep -q "unresolved .*apps/gone: its endpoint picker Service gone-epp does not exist" "$WORK/stdout"' "missing Service: exit $RC (want 1), stdout $(cat "$WORK/stdout"), applied $(cat "$WORK/applied")"

begin "the pools cannot be listed: exit 2, nothing applied, never no InferencePool"
reset
pool v1 apps chat chat-epp
epp apps chat-epp chat chat-sa
run wva_grant_epp_metrics_auth STUB_POOLS=FORBIDDEN
ok '[ "$RC" = 2 ] && [ ! -s "$WORK/applied" ] && grep -q "could not list" "$WORK/stdout" && ! grep -q "No InferencePool" "$WORK/info"' "forbidden list: exit $RC (want 2), stdout $(cat "$WORK/stdout"), info $(cat "$WORK/info"), applied $(cat "$WORK/applied")"

begin "already allowed: nothing applied; a second run after a grant applies nothing"
reset
pool v1 apps chat chat-epp
pool v1 apps code code-epp
epp apps chat-epp chat chat-sa
epp apps code-epp code code-sa
allowed apps:chat-sa
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 0 ] && ! bound apps chat-sa && bound apps code-sa && [ "$(bindings)" = 1 ] && grep -q "allowed .*apps/chat-sa" "$WORK/stdout"' "already allowed: exit $RC (want 0), applied $(cat "$WORK/applied"), stdout $(cat "$WORK/stdout")"
# The stub remembers the binding it applied, as a cluster would.
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/applied" ] && grep -q "allowed .*apps/code-sa (EPP of code; this install" "$WORK/stdout"' "second run: exit $RC (want 0), applied $(cat "$WORK/applied"), stdout $(cat "$WORK/stdout")"

begin "allowed only through another install's binding: this install's own is applied too"
reset
pool v1 apps chat chat-epp
epp apps chat-epp chat chat-sa
crb wva-epp-metrics-auth-apps-0000aaaa other-wva apps chat-sa
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 0 ] && bound apps chat-sa && [ "$(bindings)" = 1 ] && grep -q "granted .*apps/chat-sa .*install in other-wva" "$WORK/stdout"' "other install: exit $RC (want 0), applied $(cat "$WORK/applied"), stdout $(cat "$WORK/stdout")"

begin "one ServiceAccount behind two pools: one binding"
reset
pool v1 apps chat chat-epp
pool x apps chat-x chat-epp
epp apps chat-epp chat shared-sa
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 0 ] && bound apps shared-sa && [ "$(bindings)" = 1 ]' "shared: exit $RC (want 0), $(bindings) bindings, applied $(cat "$WORK/applied")"

begin "DRY_RUN=true: nothing applied, the plan and the commands printed, exit 1; DRY_RUN=1 refused, exit 2"
reset
pool v1 apps chat chat-epp
epp apps chat-epp chat chat-sa
run wva_grant_epp_metrics_auth DRY_RUN=true
ok '[ "$RC" = 1 ] && [ ! -s "$WORK/applied" ] && grep -q "would bind .*apps/chat-sa" "$WORK/stdout" && grep -q "kubectl create clusterrolebinding wva-epp-metrics-auth-apps-[0-9a-f]\{8\} --clusterrole=wva-epp-metrics-auth --serviceaccount=apps:chat-sa" "$WORK/stdout" && grep -q "kubectl create clusterrole wva-epp-metrics-auth --verb=create" "$WORK/stdout"' "dry run: exit $RC (want 1), applied $(cat "$WORK/applied"), stdout $(cat "$WORK/stdout")"
run wva_grant_epp_metrics_auth DRY_RUN=1
ok '[ "$RC" = 2 ] && [ ! -s "$WORK/kubectl.log" ] && grep -q "DRY_RUN must be true or false" "$WORK/warnings"' "DRY_RUN=1: exit $RC (want 2), kubectl $(tr '\n' ';' <"$WORK/kubectl.log")"

begin "x-k8s.io pools (extensionRef) only, the v1 API not served, a Pod-only EPP: bound"
reset
pool x apps old old-epp
epp apps old-epp old old-sa pod
run wva_grant_epp_metrics_auth STUB_V1=NORES
ok '[ "$RC" = 0 ] && bound apps old-sa && [ "$(bindings)" = 1 ]' "x-k8s: exit $RC (want 0), stdout $(cat "$WORK/stdout"), warnings $(cat "$WORK/warnings"), applied $(cat "$WORK/applied")"

begin "cluster scope, run from a directory holding files: lists with -A, binds, and the check sees it too"
reset
pool v1 apps chat chat-epp
pool x team-b old old-epp
epp apps chat-epp chat chat-sa
epp team-b old-epp old old-sa
run wva_grant_epp_metrics_auth WVA_SCOPE=cluster "unset WVA_WATCH_NS"
ok '[ "$RC" = 0 ] && grep -q "^get $V1 -A -o json$" "$WORK/kubectl.log" && ! grep -q "^get $V1 -n " "$WORK/kubectl.log" && bound apps chat-sa && bound team-b old-sa' "cluster scope: exit $RC (want 0), pool calls $(grep inferencepools "$WORK/kubectl.log" | head -3 | tr '\n' ';'), stdout $(cat "$WORK/stdout"), info $(cat "$WORK/info")"
reset
pool v1 apps chat chat-epp
epp apps chat-epp chat chat-sa
run wva_report_epp_metrics_auth WVA_SCOPE=cluster "unset WVA_WATCH_NS"
ok '[ "$RC" = 0 ] && grep -q "apps/chat-sa" "$WORK/warnings"' "cluster scope check: exit $RC, warnings $(cat "$WORK/warnings")"

begin "an endpoint picker that is not a Service: skipped with a warning, nothing bound"
reset
pool v1 apps chat chat-epp Backend
epp apps chat-epp chat chat-sa
run wva_grant_epp_metrics_auth
ok '[ "$RC" = 0 ] && [ "$(bindings)" = 0 ] && grep -q "no EPP .*apps/chat: its endpoint picker is a Backend, not a Service" "$WORK/stdout" && grep -q "not a Service" "$WORK/warnings" && ! grep -q "^get service " "$WORK/kubectl.log"' "non-Service: exit $RC (want 0), stdout $(cat "$WORK/stdout"), warnings $(cat "$WORK/warnings")"

begin "unreadable: a Service read Forbidden, or a pool list that is not JSON, exits 3, never no pools"
reset
pool v1 apps chat chat-epp
epp apps chat-epp chat chat-sa
run wva_grant_epp_metrics_auth STUB_SVC=FORBIDDEN
ok '[ "$RC" = 3 ] && [ ! -s "$WORK/applied" ] && grep -q "unknown .*apps/chat: could not read Service chat-epp" "$WORK/stdout"' "service forbidden: exit $RC (want 3), stdout $(cat "$WORK/stdout")"
run wva_grant_epp_metrics_auth STUB_POOLS=GARBAGE
ok '[ "$RC" = 3 ] && ! grep -q "No InferencePool" "$WORK/info" && grep -q "could not read the" "$WORK/stdout"' "garbage list: exit $RC (want 3), stdout $(cat "$WORK/stdout"), info $(cat "$WORK/info")"

begin "the check warns on a denied ServiceAccount and names the fix, applies nothing, never fails"
reset
pool v1 apps chat chat-epp
pool v1 apps code code-epp
epp apps chat-epp chat chat-sa
epp apps code-epp code code-sa
allowed apps:code-sa
run wva_report_epp_metrics_auth
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/applied" ] && grep -q "apps/chat-sa" "$WORK/warnings" && ! grep -q "apps/code-sa" "$WORK/warnings" && grep -q "make epp-metrics-auth" "$WORK/warnings"' "check: exit $RC (want 0), warnings $(cat "$WORK/warnings")"

begin "the check, impersonation refused: could not be checked, not denied and not fine"
reset
pool v1 apps chat chat-epp
epp apps chat-epp chat chat-sa
run wva_report_epp_metrics_auth STUB_IMPERSONATE=FORBIDDEN
ok '[ "$RC" = 0 ] && ! grep -q "apps/chat-sa" "$WORK/warnings" && grep -q "could not be checked" "$WORK/info"' "impersonation: exit $RC (want 0), warnings $(cat "$WORK/warnings"), info $(cat "$WORK/info")"

begin "undeploy deletes by this install's label; the make target, setup-prereqs and undeploy are wired"
reset
crb wva-epp-metrics-auth-apps-1111bbbb "$NS" apps chat-sa
run wva_remove_epp_metrics_auth
ok '[ "$RC" = 0 ] && grep -q "^delete clusterrolebinding wva-epp-metrics-auth-apps-1111bbbb " "$WORK/kubectl.log" && grep -q "^delete clusterrole wva-epp-metrics-auth " "$WORK/kubectl.log"' "undeploy: exit $RC, kubectl $(tr '\n' ';' <"$WORK/kubectl.log")"
MK="$(tr -d '\r' <"$ROOT/Makefile")"
TARGET="$(printf '%s\n' "$MK" | awk '/^epp-metrics-auth:/ { on = 1; next } on && /^[^\t]/ { exit } on')"
ok 'printf "%s\n" "$MK" | grep -q "^epp-metrics-auth: ## " && printf "%s" "$TARGET" | grep -q "wva_grant_epp_metrics_auth" && printf "%s" "$TARGET" | grep -q "DRY_RUN=\$(DRY_RUN)"' "epp-metrics-auth missing, without help, or not passing DRY_RUN: $TARGET"
ok 'tr -d "\r" <"$ROOT/deploy/lib/install_core.sh" | grep -q "^        wva_apply_epp_metrics_auth$" && tr -d "\r" <"$ROOT/deploy/lib/cleanup.sh" | grep -q "^            wva_remove_epp_metrics_auth$" && tr -d "\r" <"$ROOT/deploy/install.sh" | grep -q "lib/epp_auth.sh"' "setup-prereqs, undeploy or install.sh not wired"

begin "undeploy keeps the ClusterRole another binding uses, and does not alarm when it cannot read the bindings"
reset
crb wva-epp-metrics-auth-apps-1111bbbb "$NS" apps chat-sa
crb wva-epp-metrics-auth-apps-2222cccc other-wva apps chat-sa
run wva_remove_epp_metrics_auth
ok '[ "$RC" = 0 ] && grep -q "^delete clusterrolebinding wva-epp-metrics-auth-apps-1111bbbb " "$WORK/kubectl.log" && ! grep -q "2222cccc" "$WORK/deleted" && ! grep -q "^delete clusterrole wva-epp-metrics-auth" "$WORK/kubectl.log"' "role in use: kubectl $(tr '\n' ';' <"$WORK/kubectl.log")"
reset
run wva_remove_epp_metrics_auth STUB_CRB=FORBIDDEN
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/warnings" ] && ! grep -q "^delete " "$WORK/kubectl.log" && grep -q "cluster admin removes them" "$WORK/info"' "unreadable: warnings $(cat "$WORK/warnings"), kubectl $(tr '\n' ';' <"$WORK/kubectl.log"), info $(cat "$WORK/info")"

begin "undeploy removes only bindings named wva-epp-metrics-auth-* that point at its ClusterRole, label or not"
reset
crb wva-epp-metrics-auth-apps-1111bbbb "$NS" apps chat-sa
crb wva-epp-metrics-auth-apps-3333dddd "$NS" apps chat-sa cluster-admin
crb someone-elses-binding "$NS" apps chat-sa
run wva_remove_epp_metrics_auth
ok '[ "$RC" = 0 ] && grep -qx "wva-epp-metrics-auth-apps-1111bbbb" "$WORK/deleted" && ! grep -q "3333dddd" "$WORK/deleted" && ! grep -q "someone-elses-binding" "$WORK/deleted" && ! grep -q "^delete clusterrolebindings -l" "$WORK/kubectl.log"' "label only: deleted $(cat "$WORK/deleted" 2>/dev/null), kubectl $(tr '\n' ';' <"$WORK/kubectl.log")"

begin "a pool that names no endpoint picker: reported as such, not as a non-Service picker; a dry run still warns about what is left ungranted"
reset
pool v1 apps nopick -
pool v1 apps chat chat-epp
pool v1 apps gone gone-epp
epp apps chat-epp chat chat-sa
run wva_grant_epp_metrics_auth DRY_RUN=true
ok '[ "$RC" = 1 ] && grep -q "no EPP .*apps/nopick: the pool names no endpoint picker" "$WORK/stdout" && ! grep -q "not a Service" "$WORK/stdout" "$WORK/warnings"' "no picker: exit $RC (want 1), stdout $(cat "$WORK/stdout"), warnings $(cat "$WORK/warnings")"
ok 'grep -q "would bind .*apps/chat-sa" "$WORK/stdout" && grep -q "1 EPP(s) left ungranted" "$WORK/warnings" && [ ! -s "$WORK/applied" ]' "dry run, unresolved: stdout $(cat "$WORK/stdout"), warnings $(cat "$WORK/warnings")"

echo ""
echo "EPP metrics auth: $CASES cases, $PASS assertions passed, $FAIL failed"
if [ "$CASES" != "$EXPECTED_CASES" ]; then
    echo "FAIL: ran $CASES cases, expected $EXPECTED_CASES"
    exit 1
fi
if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
if [ "$PASS" = 0 ]; then
    echo "FAIL: no assertion ran"
    exit 1
fi
echo "EPP metrics auth OK ($CASES cases)"
