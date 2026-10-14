#!/usr/bin/env bash
#
# Executes enable_physical_limiter (deploy/lib/physical_limiter.sh) against a
# stubbed kubectl, and asserts WHO it grants cluster-wide node read to.
#
# It exists because the answer used to be "whatever any labelled Deployment
# names". The command found controllers by the label
# app.kubernetes.io/name=workload-variant-autoscaler, which anyone who can create
# a Deployment can set, and bound the ServiceAccount that Deployment named --
# `default`, say, which every pod in that namespace runs as. A cluster-admin
# command then handed a tenant-chosen account node read across the cluster, and
# reported it as "granted".
#
# What is asserted is the bindings the command would APPLY, read back from the
# manifests the stub receives -- not the source, and not the log line.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
CASES=0

for tool in yq jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check, and is missing. Nothing below would be about the code."
        exit 2
    }
done

# CR-stripped copies, for the reason hack/check-limiter-declaration.sh gives.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh limiter_policy.sh physical_limiter.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
unset WVA_QUOTAS WVA_QUOTA_SCOPE WVA_QUOTA_KUEUE WVA_SCOPE WVA_WATCH_NS WVA_LIMITER WVA_LIMITER_TYPE \
    WVA_LIMITER_TARGETS WVA_CONTROLLER_SA WVA_CONTROLLER_IMAGE_REPOS WVA_IMAGE_REPO
WVA_NS="wva-system"
WVA_POLICY_NS="wva-policy"
# shellcheck disable=SC1090
. "$WORK/common.sh"
# shellcheck disable=SC1090
. "$WORK/limiter_policy.sh"
# shellcheck disable=SC1090
. "$WORK/physical_limiter.sh"

case_begin() { CASES=$((CASES + 1)); CASE_FAIL_AT="$FAIL"; }
fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
ok()   { [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

# ---------------------------------------------------------------------------
# The stub. Deployments come from $KDEPLOYS/<namespace>.json; a namespace named
# in $KFAIL_NS cannot be read. Every applied manifest is reduced to one GRANT
# line per binding: `<binding kind> <binding name> <namespace>/<account>`.
# ---------------------------------------------------------------------------
STUB="$WORK/stub"; mkdir -p "$STUB"
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env bash
printf 'CALL[%s]\n' "$*" >> "$KCALLS"
# Parsed, not glob-matched: `-o json` is a prefix of `-o jsonpath=...`, and a
# pattern on the joined string answered every jsonpath read with the whole list.
ns=""; out=""; prev=""; name=""; pos=0
for a in "$@"; do
  case "$prev" in
    -n) ns="$a" ;;
    -o) out="$a" ;;
    -l) : ;;
    *) case "$a" in
         -*) : ;;
         *) pos=$((pos + 1)); [ "$pos" -eq 3 ] && name="$a" ;;   # get deploy NAME
       esac ;;
  esac
  case "$a" in -o*=*) out="${a#-o}" ;; esac
  prev="$a"
done
deploys() {
  case " ${KFAIL_NS:-} " in *" $ns "*) exit 1 ;; esac
  if [ -f "$KDEPLOYS/$ns.json" ]; then cat "$KDEPLOYS/$ns.json"; else printf '{"items":[]}'; fi
}
# Failure injection, each matched against the joined arguments or the applied
# document: KFAIL_APPLY (a substring of the document), KFAIL_PATCH (a substring
# of the arguments),
# KCM_NOTFOUND / KCM_FORBIDDEN (every policy ConfigMap read).
case "$1" in
  auth) exit 0 ;;
  create)
    printf 'apiVersion: v1\nkind: %s\nmetadata:\n  name: %s\n' "$2" "$3"
    exit 0 ;;
  label) cat; exit 0 ;;
  patch)
    [ -n "${KFAIL_PATCH:-}" ] && [ "$*" != "${*#*"$KFAIL_PATCH"}" ] && { echo "Error from server (Forbidden): cannot patch" >&2; exit 1; }
    exit 0 ;;
  apply)
    doc="$(cat)"
    [ -n "${KFAIL_APPLY:-}" ] && [ "$doc" != "${doc#*"$KFAIL_APPLY"}" ] && { echo "Error from server (Forbidden): cannot apply" >&2; exit 1; }
    printf '%s\n' "$doc" | yq -N e 'select(.kind == "ClusterRoleBinding" or .kind == "RoleBinding") | .kind + " " + .metadata.name + " " + .subjects[0].namespace + "/" + .subjects[0].name' - \
      | sed 's/^/GRANT /' >> "$KGRANTS"
    # The Namespace rule of every ClusterRole: its resourceNames, or * for none.
    printf '%s\n' "$doc" | yq -N e 'select(.kind == "ClusterRole") | .metadata.name + " " + ([.rules[] | select(.resources[] == "namespaces") | ((.resourceNames // ["*"]) | join(","))] | join(";"))' - \
      | sed 's/^/NSRULE /' >> "$KGRANTS"
    exit 0 ;;
  get)
    case "$2" in
      deploy)
        if [ "$*" != "${*#*-A}" ]; then printf '%s\n' "${KDEPLOY_NS_LIST:-}"; exit 0; fi
        case "$out" in
          json) deploys ;;
          jsonpath=*)
            # A plain field path is evaluated against the fixture, so the code
            # under test reads what a cluster would answer. Filter expressions
            # (the WVA_WATCH_NAMESPACE env lookup) answer empty: no fixture
            # sets one.
            path="${out#jsonpath=}"; path="${path#\{}"; path="${path%\}}"
            case "$path" in *"[?("*) exit 0 ;; esac
            if [ -n "$name" ]; then
              deploys | jq -r --arg n "$name" ".items[] | select(.metadata.name == \$n) | ${path} // empty"
            else
              deploys | jq -r "${path} // empty"
            fi ;;
        esac ;;
      # A namespace with <ns>.policy has that policy; every other one a plain one.
      configmap)
        [ -z "${KCM_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): configmaps is forbidden' >&2; exit 1; }
        [ -z "${KCM_NOTFOUND:-}" ] || { echo 'Error from server (NotFound): configmaps "wva-scaling-policy-config" not found' >&2; exit 1; }
        if [ -f "$KDEPLOYS/$ns.policy" ]; then cat "$KDEPLOYS/$ns.policy"; else printf 'scaleUpThreshold: 0.85'; fi ;;
      *) : ;;
    esac ;;
  *) : ;;
esac
exit 0
STUBEOF
chmod +x "$STUB/kubectl"

DEPLOYS="$WORK/deploys"; mkdir -p "$DEPLOYS"

# deploy_json <name> <serviceaccount|-> <manager image|-> [<other container name>]
# One Deployment object. `-` omits the field, so an absent ServiceAccount and an
# absent manager container are both expressible.
deploy_json() {
    local name="$1" sa="$2" image="$3" cname="${4:-manager}"
    jq -n --arg name "$name" --arg sa "$sa" --arg image "$image" --arg cname "$cname" '
        {metadata: {name: $name, labels: {"app.kubernetes.io/name": "workload-variant-autoscaler"}},
         spec: {template: {spec: (
            (if $sa == "-" then {} else {serviceAccountName: $sa} end)
            + {containers: [{name: $cname, image: (if $image == "-" then "busybox" else $image end)}]}
         )}}}'
}
# put_ns <namespace> <deployment json>... -- the namespace fixture.
put_ns() {
    local ns="$1"; shift
    printf '%s\n' "$@" | jq -s '{apiVersion: "v1", kind: "List", items: .}' > "$DEPLOYS/$ns.json"
}

IMG="ghcr.io/ev-shindin/llm-scaling-manager:main"
put_ns tenant-a "$(deploy_json wva-controller-manager wva-controller-manager "$IMG")"
# The attack: the label, the real image, the namespace default account.
put_ns tenant-b "$(deploy_json impostor default "$IMG")"
# The controller account, but someone else's image.
put_ns tenant-c "$(deploy_json lookalike wva-controller-manager "quay.io/someone/anything:v1")"
# The controller account and image, but no `manager` container.
put_ns tenant-d "$(deploy_json sidecar wva-controller-manager "$IMG" app)"
# A genuine controller and an impostor side by side, impostor first: the old
# code read `.items[0]` and would have bound the impostor account.
put_ns tenant-e "$(deploy_json aaa-impostor tenant-admin "$IMG")" \
                "$(deploy_json wva-controller-manager wva-controller-manager "$IMG")"
# A pinned digest and a registry port must still be recognised as the repo.
put_ns tenant-f "$(deploy_json wva-controller-manager wva-controller-manager "ghcr.io/ev-shindin/llm-scaling-manager:v1.2@sha256:abc")"
# A self-managed controller (the base manifest: --watch-namespace from
# WVA_WATCH_NAMESPACE, which is $(POD_NAMESPACE)) with a sidecar listed FIRST.
# Read from containers[0], it has no flag, looks cluster-scoped, and is never
# granted the Namespace read it needs to find wva-policy.
put_ns tenant-g "$(jq -n --arg img "$IMG" '
    {metadata: {name: "wva-controller-manager", labels: {"app.kubernetes.io/name": "workload-variant-autoscaler"}},
     spec: {selector: {matchLabels: {"control-plane": "controller-manager"}},
            template: {spec: {serviceAccountName: "wva-controller-manager", containers: [
              {name: "kube-rbac-proxy", image: "quay.io/brancz/kube-rbac-proxy:v0.18.0", args: ["--secure-listen-address=0.0.0.0:8443"]},
              {name: "manager", image: $img,
               args: ["--leader-elect=true", "--watch-namespace=$(WVA_WATCH_NAMESPACE)"],
               env: [{name: "POD_NAMESPACE", valueFrom: {fieldRef: {fieldPath: "metadata.namespace"}}},
                     {name: "WVA_WATCH_NAMESPACE", value: "$(POD_NAMESPACE)"}]}]}}}}')"

# run_enable [VAR=value ...] -- enable_physical_limiter in a subshell, the stub
# first on PATH. Output in $WORK/out, grants in $GRANTS, status in $RC.
run_enable() {
    GRANTS="$WORK/grants"; : > "$GRANTS"
    CALLS="$WORK/calls"; : > "$CALLS"
    (
        PATH="$STUB:$PATH"; export PATH
        KGRANTS="$GRANTS" KCALLS="$CALLS" KDEPLOYS="$DEPLOYS"
        export KGRANTS KCALLS KDEPLOYS
        KDEPLOY_NS_LIST="tenant-a tenant-b tenant-c tenant-d tenant-e tenant-f tenant-g"
        export KDEPLOY_NS_LIST
        KDEPLOY_NS_LIST="$(printf '%s\n' $KDEPLOY_NS_LIST)"
        for kv in "$@"; do export "${kv?}"; done
        "${PL_FN:-enable_physical_limiter}"
    ) >"$WORK/out" 2>&1
    RC=$?
}
granted_to() { grep -q "^GRANT ClusterRoleBinding wva-node-reader-rolebinding-[^ ]* $1\$" "$GRANTS"; }
any_grant_in() { grep -q "^GRANT [A-Za-z]* [^ ]* $1/" "$GRANTS"; }

# ---------------------------------------------------------------------------
case_begin
run_enable
if [ "$RC" -ne 0 ]; then
    fail "enable_physical_limiter failed on a cluster where everything answers: $(tail -15 "$WORK/out")"
elif ! granted_to tenant-a/wva-controller-manager; then
    fail "the genuine controller in tenant-a was not granted node read: $(cat "$GRANTS")"
else
    ok "a genuine controller is granted node read"
fi

case_begin
if any_grant_in tenant-b; then
    fail "a labelled Deployment running as tenant-b/default was GRANTED -- a tenant-chosen account now reads every node: $(grep tenant-b "$GRANTS")"
elif ! grep -q 'SKIPPED tenant-b/impostor' "$WORK/out"; then
    fail "the impostor in tenant-b was not granted but not named either; an admin cannot tell it was skipped: $(tail -15 "$WORK/out")"
else
    ok "a labelled impostor with another ServiceAccount is skipped, by name, and granted nothing"
fi

case_begin
if any_grant_in tenant-c; then
    fail "a Deployment running a foreign image was granted: $(grep tenant-c "$GRANTS")"
elif ! grep -q 'SKIPPED tenant-c/lookalike' "$WORK/out"; then
    fail "the foreign-image Deployment was not named as skipped: $(tail -15 "$WORK/out")"
else
    ok "the controller account on a foreign image is skipped"
fi

case_begin
if any_grant_in tenant-d; then
    fail "a Deployment with no manager container was granted: $(grep tenant-d "$GRANTS")"
elif ! grep -q 'SKIPPED tenant-d/sidecar' "$WORK/out"; then
    fail "the Deployment with no manager container was not named as skipped: $(tail -15 "$WORK/out")"
else
    ok "a Deployment with no manager container is skipped"
fi

case_begin
if grep -q 'tenant-e/tenant-admin' "$GRANTS"; then
    fail "the impostor listed first in tenant-e had its account granted: $(grep tenant-e "$GRANTS")"
elif ! granted_to tenant-e/wva-controller-manager; then
    fail "the genuine controller beside an impostor was not granted: $(cat "$GRANTS")"
else
    ok "beside an impostor, only the genuine controller is granted"
fi

case_begin
if ! granted_to tenant-f/wva-controller-manager; then
    fail "a controller image pinned by tag and digest was not recognised: $(tail -15 "$WORK/out")"
else
    ok "an image pinned by tag and digest is recognised by its repository"
fi

case_begin
listed_at="$(grep -n 'Controllers to be granted' "$WORK/out" | head -1 | cut -d: -f1)"
first_grant="$(grep -n 'CALL\[apply' "$CALLS" | head -1 | cut -d: -f1)"
if [ -z "$listed_at" ]; then
    fail "the command did not print the controllers it is about to grant: $(tail -15 "$WORK/out")"
elif ! sed -n "${listed_at},\$p" "$WORK/out" | grep -q 'tenant-a/wva-controller-manager as ServiceAccount wva-controller-manager'; then
    fail "the printed list does not name the genuine controller: $(tail -15 "$WORK/out")"
elif sed -n "${listed_at},/Publishing/p" "$WORK/out" | grep -q 'impostor as'; then
    fail "the printed list names the impostor as one to be granted: $(tail -15 "$WORK/out")"
elif [ -z "$first_grant" ]; then
    fail "no apply reached the stub at all; this case can no longer tell before from after"
else
    ok "the final list is printed, and holds only verified controllers"
fi

# Honest about what verification is: signals any tenant can satisfy.
case_begin
if sed -n "${listed_at:-1},/Publishing/p" "$WORK/out" | grep -q 'signals, not proof'; then
    ok "the list says verification is signals, not proof"
else
    fail "the list of controllers to be granted presents verification as proof: $(tail -15 "$WORK/out")"
fi

# The banner names who is bound and who is NOT. "In force for every WVA on this
# cluster" was printed with three labelled Deployments in the skipped list.
case_begin
banner="$(sed -n '/limiter is now in force/,$p' "$WORK/out")"
if grep -q 'every WVA' "$WORK/out"; then
    fail "the banner still claims the limiter is in force for every WVA: $(grep 'every WVA' "$WORK/out")"
elif ! grep -q 'in force for .*tenant-a/wva-controller-manager' <<<"$banner"; then
    fail "the banner does not name the controllers it bound: $banner"
elif ! grep -q 'NOT bound' <<<"$banner"; then
    fail "the banner does not say which controllers are NOT bound: $banner"
else
    missing=""
    for d in tenant-b/impostor tenant-c/lookalike tenant-d/sidecar; do
        sed -n '/NOT bound/,$p' <<<"$banner" | grep -q "$d" || missing="$missing $d"
    done
    if [ -n "$missing" ]; then
        fail "the NOT-bound list after the banner leaves out:$missing: $banner"
    else
        ok "the banner names the bound controllers, and every skipped one as NOT bound"
    fi
fi

# containers[0] is not the manager. The sidecar-first controller in tenant-g
# manages its own namespace, so it needs the Namespace read to find wva-policy.
case_begin
if grep -q '^GRANT ClusterRoleBinding wva-policy-namespace-reader-rolebinding-[^ ]* tenant-g/wva-controller-manager$' "$GRANTS"; then
    ok "the manager container, not containers[0], decides the managed namespace"
else
    fail "the self-managed controller behind a sidecar was not granted the Namespace read (read as cluster-scoped from containers[0]?): $(grep tenant-g "$GRANTS")"
fi

# ---------------------------------------------------------------------------
# WVA_LIMITER_TARGETS is an allowlist of namespaces, not of Deployments: naming
# the impostor namespace does not make its Deployment genuine.
case_begin
run_enable WVA_LIMITER_TARGETS=tenant-b
if [ "$RC" -ne 0 ]; then
    fail "enable with WVA_LIMITER_TARGETS=tenant-b failed: $(tail -15 "$WORK/out")"
elif any_grant_in tenant-b; then
    fail "naming tenant-b in WVA_LIMITER_TARGETS granted its impostor: $(cat "$GRANTS")"
elif any_grant_in tenant-a; then
    fail "WVA_LIMITER_TARGETS=tenant-b still granted tenant-a; it is no longer an allowlist: $(cat "$GRANTS")"
else
    ok "WVA_LIMITER_TARGETS does not turn an impostor into a controller, and limits the grant to the named namespaces"
fi

# A named namespace with no controller is SKIPPED: nothing granted, nothing
# written there, and it is listed as NOT bound, with the reason.
case_begin
run_enable WVA_LIMITER_TARGETS=tenant-new
if [ "$RC" -ne 0 ]; then
    fail "enable with a target holding no controller failed: $(tail -15 "$WORK/out")"
elif any_grant_in tenant-new; then
    fail "a named namespace with no controller was granted: $(grep tenant-new "$GRANTS")"
elif grep -q 'CALL\[\(patch\|apply\|create\) .*-n tenant-new' "$CALLS"; then
    fail "a policy was written into a named namespace with no controller: $(grep tenant-new "$CALLS")"
elif ! sed -n '/NOT bound/,$p' "$WORK/out" | grep -q 'tenant-new: no controller found'; then
    fail "the namespace with no controller is not listed as NOT bound, 'no controller found': $(tail -15 "$WORK/out")"
else
    ok "a named namespace with no controller is skipped, written nothing, and listed as NOT bound"
fi

# A namespace the admin NAMED relaxes the image check -- a controller built
# from a mirror or a custom IMG -- but nothing else (tenant-b above: still not).
case_begin
run_enable WVA_LIMITER_TARGETS=tenant-c
if [ "$RC" -ne 0 ]; then
    fail "enable with WVA_LIMITER_TARGETS=tenant-c failed: $(tail -15 "$WORK/out")"
elif ! granted_to tenant-c/wva-controller-manager; then
    fail "a controller on a foreign image in a namespace the admin NAMED was skipped; a mirror-built controller can then never be granted: $(tail -15 "$WORK/out")"
else
    ok "a named namespace relaxes the image check, and only that"
fi

# A mirror is accepted when named, and the account can be renamed.
case_begin
run_enable WVA_CONTROLLER_IMAGE_REPOS=quay.io/someone/anything
if ! granted_to tenant-c/wva-controller-manager; then
    fail "WVA_CONTROLLER_IMAGE_REPOS did not admit the mirror it names: $(tail -15 "$WORK/out")"
elif any_grant_in tenant-a; then
    fail "WVA_CONTROLLER_IMAGE_REPOS did not REPLACE the default repository list: $(cat "$GRANTS")"
else
    ok "WVA_CONTROLLER_IMAGE_REPOS names the repositories a controller may run from"
fi

# ---------------------------------------------------------------------------
# A namespace that cannot be read is a refusal, not a skip: the command cannot
# know what it is leaving ungranted.
case_begin
run_enable KFAIL_NS=tenant-c
if [ "$RC" -eq 0 ]; then
    fail "enable exited 0 having failed to read a controller namespace: $(tail -15 "$WORK/out")"
elif grep -q 'in force' "$WORK/out"; then
    fail "enable announced the limiter after failing to read a controller namespace: $(tail -15 "$WORK/out")"
elif [ -s "$GRANTS" ]; then
    fail "enable granted before refusing: $(cat "$GRANTS")"
else
    ok "an unreadable controller namespace stops enable before it grants anything"
fi

# pl_image_repo, directly.
case_begin
for pair in \
    "ghcr.io/ev-shindin/llm-scaling-manager:main=ghcr.io/ev-shindin/llm-scaling-manager" \
    "ghcr.io/ev-shindin/llm-scaling-manager@sha256:abc=ghcr.io/ev-shindin/llm-scaling-manager" \
    "registry.local:5000/wva=registry.local:5000/wva" \
    "registry.local:5000/wva:dev=registry.local:5000/wva"; do
    in="${pair%=*}"; want="${pair##*=}"
    got="$(pl_image_repo "$in")"
    [ "$got" = "$want" ] || fail "pl_image_repo $in gave $got, want $want"
done
ok "pl_image_repo strips tags and digests, and keeps a registry port"

# The installer image (a repo or a full reference) joins the published repo when
# no explicit list is given, so a controller built from a custom IMG is known.
case_begin
got="$(WVA_IMAGE_REPO=registry.local:5000/me/wva:dev pl_image_repos)"
if [ "$got" = "ghcr.io/ev-shindin/llm-scaling-manager registry.local:5000/me/wva" ]; then
    ok "pl_image_repos adds the installer image repository to the published one"
else
    fail "pl_image_repos with WVA_IMAGE_REPO=registry.local:5000/me/wva:dev gave '$got'"
fi

# disable: a skipped controller whose own policy still declares a limiter is
# reported, and not written to.
case_begin
printf 'limiters:\n- type: gpu-inventory\n  name: physical\n' > "$DEPLOYS/tenant-c.policy"
PL_FN=disable_physical_limiter run_enable
rm -f "$DEPLOYS/tenant-c.policy"
if [ "$RC" -ne 0 ]; then
    fail "disable failed on a cluster where everything answers: $(tail -15 "$WORK/out")"
elif grep -q 'CALL\[patch configmap wva-scaling-policy-config -n tenant-c' "$CALLS"; then
    fail "disable wrote the policy of a Deployment that failed verification"
elif ! grep -q 'tenant-c/lookalike.*STILL declares 1 limiter' "$WORK/out"; then
    fail "disable left a limiter in a skipped controller's policy without saying so: $(tail -15 "$WORK/out")"
else
    ok "disable names the limiter it left in a skipped controller's policy, and does not write it"
fi

# ---------------------------------------------------------------------------
# The Kueue reader: enable must narrow `get namespaces` exactly as the installer
# does (wva_append_kueue_reader_scope_patch), or every enable re-widens it.
case_begin
run_enable WVA_LIMITER_TYPE=quota WVA_QUOTA_KUEUE=true WVA_QUOTAS=H200=8
g_rule="$(grep "^NSRULE wva-kueue-reader-role-$(wva_ns_suffix tenant-g) " "$GRANTS" | tail -1)"
a_rule="$(grep "^NSRULE wva-kueue-reader-role-$(wva_ns_suffix tenant-a) " "$GRANTS" | tail -1)"
if [ "$RC" -ne 0 ]; then
    fail "enable with a Kueue quota failed: $(tail -15 "$WORK/out")"
elif [ "${g_rule##* }" != "tenant-g" ]; then
    fail "the namespace-scoped controller in tenant-g got Kueue get namespaces on [${g_rule##* }], want only tenant-g"
elif [ "${a_rule##* }" != "*" ]; then
    fail "the cluster-scoped controller in tenant-a got a narrowed Kueue namespace read [${a_rule##* }]; it reads LocalQueues everywhere"
else
    ok "the Kueue reader's get namespaces names the managed namespace (namespace scope) and nothing else"
fi

# A namespace-scoped controller does not watch a policy namespace outside the
# one it manages, so it sees the new policy only after a restart -- which
# enable must do, after the write. A cluster-scoped one reloads live.
case_begin
run_enable
restart_g="$(grep -n 'CALL\[rollout restart deploy wva-controller-manager -n tenant-g\]' "$CALLS" | head -1 | cut -d: -f1)"
write_wp="$(grep -n 'CALL\[patch configmap wva-scaling-policy-config -n wva-policy' "$CALLS" | head -1 | cut -d: -f1)"
if [ -z "$restart_g" ]; then
    fail "the self-managed controller in tenant-g, which reads wva-policy only at startup, was not restarted: $(grep -i restart "$WORK/out")"
elif [ -z "$write_wp" ] || [ "$restart_g" -lt "$write_wp" ]; then
    fail "tenant-g was restarted before the policy it must read was written (restart at $restart_g, write at ${write_wp:-never})"
elif grep -q 'CALL\[rollout restart deploy wva-controller-manager -n tenant-a\]' "$CALLS"; then
    fail "the cluster-scoped controller in tenant-a was restarted; it reloads policy live"
elif grep -q 'applies live' "$WORK/out"; then
    fail "enable still says the policy applies live to every controller: $(grep 'applies live' "$WORK/out")"
else
    ok "namespace-scoped controllers are restarted after the write; a cluster-scoped one is not"
fi

# Enabling the physical limiter COMPOSES with a quota the cluster policy
# already declares. Every declared limiter is built and both bound, so the
# quota must survive the write; the command used to replace the whole list.
case_begin
printf 'limiters:\n- name: tenant-quota\n  type: quota\n  scope: cluster\n  quotas:\n    H200: 8\n' > "$DEPLOYS/wva-policy.policy"
run_enable
rm -f "$DEPLOYS/wva-policy.policy"
# The payload spans lines: from the patch call up to the next recorded call.
written="$(awk '/^CALL\[/ { on = 0 } /^CALL\[patch configmap wva-scaling-policy-config -n wva-policy/ { on = 1 } on' "$CALLS")"
if [ "$RC" -ne 0 ]; then
    fail "enable failed over a policy that declares a quota: $(tail -15 "$WORK/out")"
elif [ -z "$written" ]; then
    fail "enable did not patch the wva-policy ConfigMap: $(grep -c CALL "$CALLS") calls"
elif [ "$written" = "${written#*name: tenant-quota}" ]; then
    fail "enabling the physical limiter dropped the quota already in the cluster policy: $written"
elif [ "$written" = "${written#*type: gpu-inventory}" ]; then
    fail "enable did not declare the physical limiter: $written"
else
    ok "enabling the physical limiter keeps the quota the cluster policy already declares"
fi

# Messages may not claim more than the code did. Each failure below must end
# the command non-zero, name the step, and never print "in force".
# expect_failed <label> <substring the error must carry>
expect_failed() {
    if [ "$RC" -eq 0 ]; then
        fail "$1: exited 0: $(tail -6 "$WORK/out")"
    elif grep -q 'in force\|Removed the limiter' "$WORK/out"; then
        fail "$1: still announced success: $(grep 'in force\|Removed the limiter' "$WORK/out")"
    elif ! grep -q "$2" "$WORK/out"; then
        fail "$1: the error does not name the step ('$2'): $(tail -4 "$WORK/out")"
    else
        ok "$1"
    fi
}
case_begin
run_enable KFAIL_APPLY=wva-node-reader-role
expect_failed "a node-read grant that fails stops enable" "node read"
case_begin
run_enable 'KFAIL_APPLY=kind: namespace'
expect_failed "a policy Namespace that cannot be created stops enable" "could not create Namespace wva-policy"
case_begin
run_enable KCM_NOTFOUND=1 KFAIL_APPLY=wva-scaling-policy-config
expect_failed "a policy ConfigMap create that fails mid-pipeline stops enable" "could not create the policy ConfigMap"
case_begin
PL_FN=disable_physical_limiter run_enable KCM_FORBIDDEN=1
expect_failed "disable: a Forbidden policy read is an error, not nothing to disable" "not a NotFound"
case_begin
printf 'limiters:\n- type: gpu-inventory\n  name: physical\n' > "$DEPLOYS/tenant-a.policy"
PL_FN=disable_physical_limiter run_enable WVA_LIMITER_TARGETS=tenant-a KFAIL_PATCH=null
rm -f "$DEPLOYS/tenant-a.policy"
expect_failed "disable: a failed removal of the default entry is an error, not Removed" "could not remove the limiter from tenant-a"

# The well-known namespace is fixed in the controller: WVA_POLICY_NS cannot move
# it, so naming another is refused, and `wellknown` always means wva-policy.
case_begin
run_enable WVA_POLICY_NS=platform-policy
if [ "$RC" -eq 0 ]; then
    fail "WVA_POLICY_NS=platform-policy was accepted: a policy no controller reads: $(grep -i 'policy in' "$WORK/out")"
elif [ -s "$GRANTS" ]; then
    fail "WVA_POLICY_NS=platform-policy granted before refusing: $(cat "$GRANTS")"
elif ! grep -q 'wva.llmd.ai/policy-namespace' "$WORK/out"; then
    fail "the WVA_POLICY_NS refusal does not say to label the namespace instead: $(tail -4 "$WORK/out")"
else
    ok "WVA_POLICY_NS naming another namespace is refused before anything changes"
fi
case_begin
got="$(PATH="$STUB:$PATH" KCALLS="$WORK/calls" KDEPLOYS="$DEPLOYS" WVA_POLICY_NS=platform-policy pl_policy_resolution tenant-g tenant-g)"
if [ "$got" = "wva-policy wellknown" ]; then
    ok "an unlabelled self-managed controller resolves to wva-policy whatever WVA_POLICY_NS says"
else
    fail "pl_policy_resolution for an unlabelled self-managed controller gave '$got', want 'wva-policy wellknown'"
fi

case_begin
CASES_EXPECTED=29
if [ "$CASES" -ne "$CASES_EXPECTED" ]; then
    fail "$CASES cases ran, not $CASES_EXPECTED. Update CASES_EXPECTED deliberately."
else
    ok "all $CASES cases ran"
fi

if [ "$FAIL" -ne 0 ]; then
    echo "physical limiter targets check FAILED"
    exit 1
fi
echo "physical limiter targets OK"
