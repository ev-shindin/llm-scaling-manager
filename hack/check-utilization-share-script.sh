#!/usr/bin/env bash
#
# Executes deploy/utilization-share.sh: its policy transforms against fixture
# policies, and every verb against a STUBBED kubectl, asserting what it would
# have written and what it refused to write.
#
# The verbs edit the one policy every model's scaling reads, so the cases that
# matter most are refusals: a pod list that fails must not read as "no pod is
# marked" (off would remove the block and strand the marks), a policy that
# cannot be read must not read as "no policy", and a block this script does not
# own is never replaced.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
CASES=0
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# CR-stripped, for the reason hack/check-limiter-declaration.sh gives.
tr -d '\r' < "$ROOT/deploy/utilization-share.sh" > "$WORK/us.sh"

# shellcheck disable=SC1090
. "$WORK/us.sh"
set +e
# AFTER the source: the script defines its own ok and fail, and with those a
# failing case printed [FAIL] and counted nothing, so the check passed.
ok()   { echo "  ok   $*"; }
fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
case_() { CASES=$((CASES + 1)); }

QUOTA_POLICY='# shipped thresholds
scaleUpThreshold: 0.85
limiters:
  - name: install-quota
    type: quota
    scope: namespace
    namespaceQuotas:
      team-a:
        NVIDIA-H200: 8'
INV_POLICY='scaleUpThreshold: 0.85
limiters:
  - name: physical
    type: gpu-inventory'
TUNED_POLICY="$QUOTA_POLICY
optimizer:
  type: utilizationShare
  utilizationShare:
    shadow: true
    maxHeadroomReplicas: 2"

expect() { # expect <name> <want> <got>
    case_
    if [ "$2" = "$3" ]; then ok "$1"; else fail "$1: want [$2], got [$3]"; fi
}

# --- transforms -------------------------------------------------------------
expect "kind: quota" quota "$(printf '%s\n' "$QUOTA_POLICY" | us_limiter_kind)"
expect "kind: gpu-inventory" inventory "$(printf '%s\n' "$INV_POLICY" | us_limiter_kind)"
expect "kind: the inventory alias" inventory "$(printf 'limiters:\n  - name: p\n    type: inventory\n' | us_limiter_kind)"
expect "kind: quota and inventory is both" inventory+quota "$(printf '%s\n  - name: p\n    type: gpu-inventory\n' "$QUOTA_POLICY" | us_limiter_kind)"
expect "kind: inventory first, then quota, is both" inventory+quota "$(printf 'limiters:\n  - type: inventory\n  - name: q\n    type: quota\n    scope: cluster\n    quotas: {H100: 8}\n' | us_limiter_kind)"
expect "share budget: quota alone" quota "$(printf '%s\n' "$QUOTA_POLICY" | us_share_cluster_budget)"
expect "share budget: inventory alone" physical "$(printf '%s\n' "$INV_POLICY" | us_share_cluster_budget)"
expect "share budget: inventory beside a namespace quota is physical" physical "$(printf '%s\n  - name: p\n    type: gpu-inventory\n' "$QUOTA_POLICY" | us_share_cluster_budget)"
expect "share budget: inventory beside a cluster quota is the quota" quota "$(printf 'limiters:\n  - type: gpu-inventory\n  - name: q\n    type: quota\n    scope: cluster\n    quotas: {H100: 8}\n' | us_share_cluster_budget)"
expect "share budget: no limiters" none "$(printf 'scaleUpThreshold: 0.85\n' | us_share_cluster_budget)"
expect "kind: no limiters" none "$(printf 'scaleUpThreshold: 0.85\n' | us_limiter_kind)"
expect "quota keys" "NVIDIA-H200" "$(printf '%s\n' "$QUOTA_POLICY" | us_quota_keys)"
expect "state: no block" "off plain" "$(printf '%s\n' "$QUOTA_POLICY" | us_block_state)"
expect "state: tuned shadow" "shadow plain" "$(printf '%s\n' "$TUNED_POLICY" | us_block_state)"
expect "state: another optimizer" "other:fancy plain" "$(printf 'optimizer:\n  type: fancy\n' | us_block_state)"

out="$(printf '%s\n' "$TUNED_POLICY" | us_policy_with_mode active)"
expect "active: shadow false" false "$(printf '%s\n' "$out" | yq '.optimizer.utilizationShare.shadow')"
expect "active: tuning kept" 2 "$(printf '%s\n' "$out" | yq '.optimizer.utilizationShare.maxHeadroomReplicas')"
expect "active: limiters kept" 8 "$(printf '%s\n' "$out" | yq '.limiters[0].namespaceQuotas.team-a.NVIDIA-H200')"
case_
if grep -q '^# shipped thresholds' <<<"$out"; then ok "active: comments kept"; else fail "active: the policy's comment was dropped"; fi
out="$(printf '%s\n' "$INV_POLICY" | us_policy_with_mode shadow physical)"
expect "shadow physical: physicalGroups" true "$(printf '%s\n' "$out" | yq '.optimizer.utilizationShare.physicalGroups')"
expect "shadow physical: shadow" true "$(printf '%s\n' "$out" | yq '.optimizer.utilizationShare.shadow')"
out="$(printf '%s\n' "$TUNED_POLICY" | us_policy_with_mode off)"
expect "off: block removed" null "$(printf '%s\n' "$out" | yq '.optimizer')"
expect "off: everything else as before" \
    "$(printf '%s\n' "$QUOTA_POLICY" | yq -o=json -I=0 '.')" "$(printf '%s\n' "$out" | yq -o=json -I=0 '.')"

# The keys written must be the ones the controller decodes.
case_
src="$(tr -d '\r' < "$ROOT/internal/config/utilization_share.go")"
missing=""
for tag in 'yaml:"shadow' 'yaml:"physicalGroups' 'yaml:"utilizationShare'; do
    # A here-string, not a pipe: the sourced script set pipefail, and grep -q
    # exiting at its first match killed printf with SIGPIPE, which pipefail then
    # reported as a missing tag -- intermittently, as the file grew.
    grep -qF -- "$tag" <<<"$src" || missing="$missing $tag"
done
grep -qE 'OptimizerTypeUtilizationShare[[:space:]]*=[[:space:]]*"utilizationShare"' <<<"$src" || missing="$missing type-constant"
if [ -z "$missing" ]; then ok "the keys written are the ones internal/config decodes"; else fail "internal/config no longer decodes:$missing"; fi

# --- verbs, against a stubbed kubectl ---------------------------------------
STATE="$WORK/state"
mkdir -p "$WORK/bin"
cat > "$WORK/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# A cluster in files. policy + rv is the ConfigMap; logs is every controller
# pod's log; standby makes `logs deploy/X` read a standby pod that logs
# nothing; warn makes every call print a client warning on stderr.
S="$STUB_STATE"
args=" $* "
[ -e "$S/warn" ] && echo 'WARNING: the gcp auth plugin is deprecated; use gcloud instead.' >&2
ns=""; prev=""
for a in "$@"; do [ "$prev" = "-n" ] && ns="$a"; prev="$a"; done
case "$args" in
  *" get configmap wva-controller-status "*)
    [ -e "$S/status.json" ] || { echo 'Error from server (NotFound): configmaps "wva-controller-status" not found' >&2; exit 1; }
    jq '{data: .}' "$S/status.json" ;;
  *" get configmap "*)
    [ -e "$S/cm.forbidden" ] && { echo 'Error from server (Forbidden): configmaps is forbidden' >&2; exit 1; }
    [ -e "$S/policy" ] || { echo 'Error from server (NotFound): configmaps "wva-scaling-policy-config" not found' >&2; exit 1; }
    case "$args" in
      *jsonpath*) cat "$S/policy" ;;
      *) jq -n --rawfile d "$S/policy" --arg rv "$(cat "$S/rv" 2>/dev/null || echo 1)" \
           '{metadata:{resourceVersion:$rv},data:{default:$d}}' ;;
    esac ;;
  *" patch configmap "*)
    p=""; prev=""
    for a in "$@"; do [ "$prev" = "-p" ] && p="$a"; prev="$a"; done
    rv="$(cat "$S/rv" 2>/dev/null || echo 1)"
    prv="$(printf '%s' "$p" | jq -r '.metadata.resourceVersion // ""')"
    [ -z "$prv" ] || [ "$prv" = "$rv" ] || {
      echo 'Error from server (Conflict): the object has been modified; please apply your changes to the latest version' >&2; exit 1; }
    printf '%s' "$p" | jq -r '.data.default' > "$S/policy"
    newrv=$((rv + 1))
    # Another writer edits the policy right after this one, and the controller
    # applies THAT version: an unrelated key, or (concurrent.block) the block.
    if [ -e "$S/concurrent" ]; then
      if [ -e "$S/concurrent.block" ]; then
        yq -i 'del(.optimizer)' "$S/policy"
      else
        printf 'scaleDownBoundary: 0.5\n' >> "$S/policy"
      fi
      newrv=$((rv + 2))
    fi
    echo "$newrv" > "$S/rv"
    echo x >> "$S/patches"
    # A status-publishing controller records what it applied, unless stalled.
    if [ -e "$S/status.json" ] && [ ! -e "$S/status.stalled" ]; then
      mode=off
      if grep -q 'bogus' "$S/policy"; then mode=invalid
      elif grep -q 'type: utilizationShare' "$S/policy"; then
        if grep -q 'shadow: true' "$S/policy"; then mode=shadow; else mode=active; fi
      fi
      [ -e "$S/status.mode" ] && mode="$(cat "$S/status.mode")"
      acc=true; [ -e "$S/status.reject" ] && acc=false
      jq --arg rv "$newrv" --arg ns "$ns" --arg m "$mode" --arg a "$acc" \
        '.policyResourceVersion=$rv | .policyNamespace=$ns | .utilizationShare=$m | .policyAccepted=$a
         | .policyError=(if $a=="false" then "limiters: entry[0]: name must not be empty" else "" end)
         | .utilizationShareError=(if $m=="invalid" then "unknown field bogus" else "" end)' \
        "$S/status.json" > "$S/status.tmp" && mv "$S/status.tmp" "$S/status.json"
    fi
    case "$args" in *jsonpath*) printf '%s' "$((rv + 1))" ;; esac
    if [ ! -e "$S/noreload" ]; then
      if [ "$ns" = wva-policy ]; then
        echo 'INFO Loaded cluster policy from the policy namespace {"policyNamespace": "wva-policy"}' >> "$S/logs"
      else
        echo 'INFO Updated global scaling policy from ConfigMap' >> "$S/logs"
      fi
    fi
    grep -q 'bogus' "$S/policy" && echo 'ERROR Utilization share: invalid optimizer block; keeping the optimizer' >> "$S/logs"
    # shadow removes the active optimizer's marks
    grep -q 'shadow: true' "$S/policy" && [ ! -e "$S/marks.stick" ] && echo '{"items":[]}' > "$S/pods.json"
    exit 0 ;;
  *" get namespace "*)
    case "$args" in
      *" wva-policy "*) [ -e "$S/ns.wva-policy" ] && { echo namespace/wva-policy; exit 0; }
                        echo 'Error from server (NotFound): namespaces "wva-policy" not found' >&2; exit 1 ;;
      *) jq -n --arg l "$(cat "$S/ns.label" 2>/dev/null)" \
           '{metadata:{labels:(if $l == "" then {} else {"wva.llmd.ai/policy-namespace":$l} end)}}' ;;
    esac ;;
  *" get deploy "*)
    echo '{"spec":{"selector":{"matchLabels":{"control-plane":"controller-manager"}}}}' ;;
  *" logs "*)
    [ -e "$S/logs.forbidden" ] && { echo 'Error from server (Forbidden): pods/log is forbidden' >&2; exit 1; }
    case "$args" in *" -l "*) ;; *) [ -e "$S/standby" ] && exit 0 ;; esac
    cat "$S/logs" 2>/dev/null; exit 0 ;;
  *" get events "*)
    [ -e "$S/events.forbidden" ] && { echo 'Error from server (Forbidden): events is forbidden' >&2; exit 1; }
    case "$args" in
      *AcceleratorNotResolved*) cat "$S/events.unresolved" 2>/dev/null ;;
      *type=Warning*) cat "$S/events.warning" 2>/dev/null ;;
    esac
    exit 0 ;;
  *" get pods "*)
    [ -e "$S/pods.forbidden" ] && { echo 'Error from server (Forbidden): pods is forbidden' >&2; exit 1; }
    # An admin edits the limiter while `off` waits for the marks.
    if [ -e "$S/admin.edit" ]; then
      n=$(( $(cat "$S/pods.calls" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$S/pods.calls"
      if [ "$n" -eq 1 ]; then
        sed -i 's/NVIDIA-H200: 8/NVIDIA-H200: 6/' "$S/policy"; echo $(( $(cat "$S/rv" 2>/dev/null || echo 1) + 1 )) > "$S/rv"
      fi
    fi
    cat "$S/pods.json" 2>/dev/null || echo '{"items":[]}' ;;
  *" rollout "*)
    [ -e "$S/restart.noreload" ] || echo 'INFO Updated global scaling policy from ConfigMap' >> "$S/logs"
    exit 0 ;;
  *) echo "stub kubectl: unhandled: $*" >&2; exit 3 ;;
esac
STUB
chmod +x "$WORK/bin/kubectl"
MARKED_PODS='{"items":[{"metadata":{"name":"donor-0","annotations":{"llm-d.ai/utilization-share-transfer":"t1"}}}]}'

# run <verb> [args] -- a fresh process, as make runs it. STATE must be set up.
run() {
    PATH="$WORK/bin:$PATH" STUB_STATE="$STATE" RELOAD_WAIT=2 CYCLE_WAIT=0 MARK_WAIT="${MARK_WAIT_T:-2}" \
        bash "$WORK/us.sh" "$1" -n team-a "${@:2}" > "$WORK/out" 2>&1
    RC=$?
}
fresh() { rm -rf "$STATE"; mkdir -p "$STATE"; [ -n "${1:-}" ] && printf '%s\n' "$1" > "$STATE/policy"; :; }
patches() { [ -e "$STATE/patches" ] && grep -c . "$STATE/patches" || echo 0; }
pol() { yq "$1" "$STATE/policy"; }
says() { grep -qF -- "$1" "$WORK/out"; }

vcase() { # vcase <name> <condition...>
    local name="$1"; shift
    case_
    if "$@"; then ok "$name"; else fail "$name (rc=$RC): $(tr '\n' ' ' < "$WORK/out" | cut -c1-300)"; fi
}

fresh "$QUOTA_POLICY"; run shadow
vcase "shadow: writes shadow true and confirms the reload" \
    eval '[ $RC -eq 0 ] && [ "$(pol .optimizer.utilizationShare.shadow)" = true ] && says "did not refuse it"'

fresh "$INV_POLICY"; run shadow --physical-groups
vcase "shadow --physical-groups: plans the inventory" \
    eval '[ $RC -eq 0 ] && [ "$(pol .optimizer.utilizationShare.physicalGroups)" = true ]'

fresh "$INV_POLICY"; run shadow
vcase "shadow on inventory without the flag: warns it plans nothing" \
    eval '[ $RC -eq 0 ] && says "--physical-groups"'

fresh "$QUOTA_POLICY
optimizer:
  type: fancy"; run shadow
vcase "shadow: refuses another optimizer, writes nothing" eval '[ $RC -ne 0 ] && [ "$(patches)" = 0 ]'

fresh 'scaleUpThreshold: 0.85'; run shadow
vcase "shadow: refuses a policy with no limiters" eval '[ $RC -ne 0 ] && [ "$(patches)" = 0 ]'

fresh; run shadow
vcase "shadow: refuses when there is no policy" eval '[ $RC -ne 0 ] && [ "$(patches)" = 0 ]'

fresh "$QUOTA_POLICY"; touch "$STATE/cm.forbidden"; run shadow
vcase "shadow: an unreadable policy is not an absent one" \
    eval '[ $RC -ne 0 ] && says "Forbidden" && [ "$(patches)" = 0 ]'

fresh "$QUOTA_POLICY"; run activate
vcase "activate: refuses with no shadow block first" eval '[ $RC -ne 0 ] && [ "$(patches)" = 0 ]'

fresh "$TUNED_POLICY"; run activate
vcase "activate: from shadow sets shadow false, keeps tuning" \
    eval '[ $RC -eq 0 ] && [ "$(pol .optimizer.utilizationShare.shadow)" = false ] && [ "$(pol .optimizer.utilizationShare.maxHeadroomReplicas)" = 2 ]'

fresh "$TUNED_POLICY
    bogus: 1"; run activate
vcase "activate: reports the controller's refusal" eval '[ $RC -ne 0 ] && says "refused the policy"'

fresh "$TUNED_POLICY"; touch "$STATE/noreload"; run activate
vcase "activate: no reload is a failure that names --restart" eval '[ $RC -ne 0 ] && says "--restart"'

fresh "$TUNED_POLICY"; touch "$STATE/noreload"; run activate --restart
vcase "activate --restart: restarts and confirms" eval '[ $RC -eq 0 ] && says "restarting"'

fresh "$(printf '%s\n' "$TUNED_POLICY" | us_policy_with_mode active)"; printf '%s' "$MARKED_PODS" > "$STATE/pods.json"; run off
vcase "off: through shadow, marks cleared, block removed" \
    eval '[ $RC -eq 0 ] && [ "$(pol .optimizer)" = null ] && [ "$(pol .limiters[0].type)" = quota ] && [ "$(patches)" = 2 ]'

fresh "$(printf '%s\n' "$TUNED_POLICY" | us_policy_with_mode active)"; printf '%s' "$MARKED_PODS" > "$STATE/pods.json"; touch "$STATE/pods.forbidden"; run off
vcase "off: pods that cannot be listed keep the block" \
    eval '[ $RC -ne 0 ] && [ "$(pol .optimizer.type)" = utilizationShare ] && says "marks were not checked"'

fresh "$(printf '%s\n' "$TUNED_POLICY" | us_policy_with_mode active)"; printf '%s' "$MARKED_PODS" > "$STATE/pods.json"; touch "$STATE/marks.stick"; MARK_WAIT_T=1 run off
vcase "off: marks that do not clear keep the block in shadow" \
    eval '[ $RC -ne 0 ] && [ "$(pol .optimizer.utilizationShare.shadow)" = true ] && says "still marked"'

fresh "$QUOTA_POLICY"; run off
vcase "off: nothing to remove is not an error" eval '[ $RC -eq 0 ] && [ "$(patches)" = 0 ]'

fresh "$QUOTA_POLICY"; echo 'INFO GPU limiter (re)built from config {"limiterTypes": "quota", "limiters": "quota/install-quota"} {"accelerator": "NVIDIA-H200"}' > "$STATE/logs"; run check
vcase "check: a resolving quota passes" eval '[ $RC -eq 0 ] && says "no AcceleratorNotResolved"'

# H200 is a substring of the quota's NVIDIA-H200 key; a substring match passed it.
fresh "$QUOTA_POLICY"; echo '{"accelerator": "H200"}' > "$STATE/logs"; run check
vcase "check: a quota key that is not the logged name warns" eval 'says "has no '"'"'H200'"'"' key"'

fresh "$QUOTA_POLICY"; echo 'event/x' > "$STATE/events.unresolved"; run check
vcase "check: an unresolved accelerator fails" eval '[ $RC -ne 0 ] && says "AcceleratorNotResolved event"'

fresh "$QUOTA_POLICY"; touch "$STATE/events.forbidden"; run check
vcase "check: unreadable events fail, not pass" eval '[ $RC -ne 0 ] && says "could not read events"'

fresh "$QUOTA_POLICY"; touch "$STATE/logs.forbidden"; run check
vcase "check: an unreadable log fails, not reads as empty" eval '[ $RC -ne 0 ] && says "could not read the log"'

fresh 'scaleUpThreshold: 0.85'; run check
vcase "check: no limiters fails" eval '[ $RC -ne 0 ] && says "no limiters: list"'

fresh "$INV_POLICY"; run check
vcase "check: inventory without physical groups says how to plan it" eval '[ $RC -eq 0 ] && says "--physical-groups"'

# A composed set: both kinds bound, and check says which is the share's cluster
# budget. It used to read the list as a quota alone, as the controller did.
COMPOSED_POLICY="$QUOTA_POLICY
  - name: gpu
    type: gpu-inventory"
fresh "$COMPOSED_POLICY"; echo 'INFO GPU limiter constructed {"limiterTypes": "inventory+quota", "limiters": "inventory+quota/install-quota,gpu"} {"accelerator": "NVIDIA-H200"}' > "$STATE/logs"; run check
vcase "check: a composed set names both limiters and the physical cluster budget" \
    eval '[ $RC -eq 0 ] && says "gpu-inventory and quota, both in force" && says "budget is the GPUs that exist" && ! says "--physical-groups"'

fresh "$COMPOSED_POLICY"; printf '{"policyNamespace":"team-a","policyResourceVersion":"1","utilizationShare":"off","policyAccepted":"true","limiter":"inventory+quota/install-quota,gpu"}' > "$STATE/status.json"; run status
vcase "status: a composed set's limiter reads types/names from the controller status" \
    eval 'says "limiter inventory+quota/install-quota,gpu"'

fresh "$TUNED_POLICY"; printf '%s' "$MARKED_PODS" > "$STATE/pods.json"; echo 'LAST SEEN TYPE REASON
1m Warning UtilizationShareCancelled deploy/a' > "$STATE/events.warning"; run status
vcase "status: reports the mode, warnings and marks" \
    eval '[ $RC -eq 0 ] && says "optimizer block: shadow" && says "UtilizationShareCancelled" && says "donor-0"'

# --- the second review's findings ------------------------------------------
expect "state: an optimizer that is not a map" "other:not-a-map plain" "$(printf 'optimizer: foo\n' | us_block_state)"
( cd "$WORK" && touch x && expect "state: a glob-like type is not expanded" "other:? plain" "$(printf 'optimizer:\n  type: "?"\n' | us_block_state)"; rm -f x )
CASES=$((CASES + 1))
expect "documents: two" 2 "$(printf 'a: 1\n---\nb: 2\n' | us_doc_count)"

POLICYNS_LOG='INFO Cluster policy comes from the well-known namespace {"policyNamespace": "wva-policy"}'
fresh "$QUOTA_POLICY"; echo "$POLICYNS_LOG" > "$STATE/logs"; run shadow
vcase "policy namespace: found in the log, written there, confirmed by its own reload line" \
    eval '[ $RC -eq 0 ] && says "policy namespace: wva-policy" && [ "$(pol .optimizer.utilizationShare.shadow)" = true ]'

fresh "$QUOTA_POLICY"; echo "$POLICYNS_LOG" > "$STATE/logs"; run shadow --policy-namespace team-a
vcase "policy namespace: one the controller does not read is refused" \
    eval '[ $RC -ne 0 ] && says "reads its policy from wva-policy" && [ "$(patches)" = 0 ]'

fresh "$TUNED_POLICY"; touch "$STATE/noreload" "$STATE/restart.noreload"; run activate --restart
vcase "restart: no reload after the restart either is a failure, not OK" \
    eval '[ $RC -ne 0 ] && says "logged no read"'

fresh "$QUOTA_POLICY"; touch "$STATE/warn"; run shadow
vcase "a kubectl warning on stderr is not written into the policy" \
    eval '[ $RC -eq 0 ] && ! grep -q "gcp auth plugin" "$STATE/policy"'

fresh "$(printf '%s\n' "$TUNED_POLICY" | us_policy_with_mode active)"; printf '%s' "$MARKED_PODS" > "$STATE/pods.json"; touch "$STATE/admin.edit"; run off
vcase "off: a limiter edit made while it waited is kept" \
    eval '[ $RC -eq 0 ] && [ "$(pol .optimizer)" = null ] && [ "$(pol .limiters[0].namespaceQuotas.team-a.NVIDIA-H200)" = 6 ]'

fresh "$QUOTA_POLICY
---
other: 1"; run shadow
vcase "a policy of two YAML documents is refused" eval '[ $RC -ne 0 ] && says "more than one YAML document" && [ "$(patches)" = 0 ]'

fresh "$QUOTA_POLICY
optimizer: foo"; run shadow
vcase "an optimizer that is not a map is not overwritten" eval '[ $RC -ne 0 ] && [ "$(patches)" = 0 ]'

# The kubelet rotated the startup line away: the controller's rule decides.
fresh "$QUOTA_POLICY"; touch "$STATE/ns.wva-policy"; run shadow
vcase "policy namespace: no log line, wva-policy exists: written there" \
    eval '[ $RC -eq 0 ] && says "policy namespace: wva-policy" && says "into wva-policy/"'

fresh "$QUOTA_POLICY"; echo team-policy > "$STATE/ns.label"; touch "$STATE/ns.wva-policy"; RELOAD_T=1 run shadow
vcase "policy namespace: no log line, the Namespace label wins over wva-policy" \
    eval 'says "policy namespace: team-policy"'

fresh "$QUOTA_POLICY"; touch "$STATE/ns.wva-policy"; run shadow --controller-namespace wva-system
vcase "policy namespace: a controller managing another namespace reads its own" \
    eval '[ $RC -eq 0 ] && says "into wva-system/"'

fresh "$QUOTA_POLICY"; run shadow --policy-namespace other-ns
vcase "policy namespace: a flag naming one the controller does not read is refused" \
    eval '[ $RC -ne 0 ] && says "reads its own policy" && [ "$(patches)" = 0 ]'

# A controller that publishes wva-controller-status: confirmed from it, no log.
status_on() { printf '{"policyNamespace":"%s","policyResourceVersion":"1","utilizationShare":"off","policyAccepted":"true","limiter":"quota/install-quota"}' "${1:-team-a}" > "$STATE/status.json"; }
fresh "$QUOTA_POLICY"; status_on; run shadow
vcase "status: a shadow write is confirmed from the status, with no log at all" \
    eval '[ $RC -eq 0 ] && says "applied version 2: utilization share is shadow"'

fresh "$QUOTA_POLICY"; status_on; touch "$STATE/status.reject"; run shadow
vcase "status: a rejected policy fails with the controller's reason" \
    eval '[ $RC -ne 0 ] && says "rejected the policy: limiters"'

fresh "$TUNED_POLICY
    bogus: 1"; status_on; run activate
vcase "status: an invalid optimizer block fails with its error" \
    eval '[ $RC -ne 0 ] && says "optimizer block invalid: unknown field bogus"'

fresh "$QUOTA_POLICY"; status_on; touch "$STATE/status.stalled"; run shadow
vcase "status: a version never applied fails and names --restart" \
    eval '[ $RC -ne 0 ] && says "has not applied version 2" && says "--restart"'

# A concurrent write moved the version on before the controller reported it.
# Requiring the written version exactly failed a write the controller applied.
fresh "$QUOTA_POLICY"; status_on; touch "$STATE/concurrent"; run shadow
vcase "status: a later version that still carries the block written confirms it" \
    eval '[ $RC -eq 0 ] && says "still carries the block written" && says "utilization share is shadow" && [ "$(pol .scaleDownBoundary)" = 0.5 ]'

# ...and only then: a later write that took the block away is not ours applied.
fresh "$QUOTA_POLICY"; status_on; touch "$STATE/concurrent" "$STATE/concurrent.block"; run shadow
vcase "status: a later version that replaced the block is not a confirmation" \
    eval '[ $RC -ne 0 ] && says "has not applied version 2" && ! says "still carries"'

fresh "$QUOTA_POLICY"; status_on wva-policy; run shadow --policy-namespace team-a
vcase "status: the policy namespace it reports wins over a wrong flag" \
    eval '[ $RC -ne 0 ] && says "reads its policy from wva-policy" && [ "$(patches)" = 0 ]'

fresh "$QUOTA_POLICY"; status_on; echo active > "$STATE/status.mode"; run shadow
vcase "status: a mode other than the one written fails" \
    eval '[ $RC -ne 0 ] && says "mode '"'"'active'"'"', not '"'"'shadow'"'"'"'

# A large log with the reload line FIRST: printf | grep -q under pipefail lost it
# to SIGPIPE once the log outgrew a pipe buffer.
fresh "$TUNED_POLICY"; touch "$STATE/noreload"
{ echo 'INFO Updated global scaling policy from ConfigMap'; head -c 1048576 /dev/zero | tr '\0' 'x' | fold -w 200; } > "$STATE/logs"
run activate
vcase "a reload line at the top of a large log is found" eval '[ $RC -eq 0 ] && says "did not refuse it"'

fresh "$TUNED_POLICY"; touch "$STATE/standby"; run activate
vcase "logs: every controller pod is read, not one that may be a standby" eval '[ $RC -eq 0 ] && says "did not refuse it"'

fresh "$QUOTA_POLICY"; echo '{"level":"info","msg":"x","accelerator":"NVIDIA-H200"}' > "$STATE/logs"; run check
vcase "check: reads accelerators from a json-encoded log" eval '[ $RC -eq 0 ] && says "resolves: NVIDIA-H200"'

CASES_EXPECTED=75
if [ "$CASES" -ne "$CASES_EXPECTED" ]; then
    fail "$CASES cases ran, not $CASES_EXPECTED. A case was added or removed; update CASES_EXPECTED deliberately."
fi
if [ "$FAIL" -ne 0 ]; then
    echo "utilization-share script check: $FAIL failure(s)"
    exit 1
fi
echo "utilization-share script check OK ($CASES cases)"
