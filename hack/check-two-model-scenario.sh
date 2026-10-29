#!/usr/bin/env bash
#
# Executes the guards in hack/benchmark/two_model_pool.sh, and runs the
# schedule/report self-test beside it.
#
# The scenario's value is that its two arms differ in ONE thing. Every guard
# here protects that, and each protects it against a mistake that produces a
# complete, plausible, WRONG result rather than an error -- at 34 minutes of GPU
# on a shared cluster per arm:
#
#   * `run pool` with no pool writes a directory labelled `pool` containing the
#     nopool arm, and the report believes the label.
#   * `run nopool` with a pool still holding accelerators does the same in the
#     other direction, charging the pool's GPUs to the arm that exists to show
#     life without one.
#   * `run pool` against a COLD pool measures the first burst paying a model
#     load INTO the pool, and reports it as the pool's cost.
#   * a standup that rendered ONE stack leaves one EPP; the other model 404s for
#     the whole run and the failures read as a result.
#   * two gateway Services, or none, and the driver would pick one and drive
#     every request at a stack that does not serve both models.
#
# kubectl is stubbed: these are argument-and-state guards, and every one must
# fire before anything is created. A guard that fires after the Job exists has
# already spent the accelerators.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/hack/benchmark/two_model_pool.sh"
FAIL=0
CASES=0

case_begin() { CASES=$((CASES + 1)); CASE_FAIL_AT="$FAIL"; }
fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
ok()   { [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

[ -f "$SCRIPT" ] || { echo "FAIL $SCRIPT is missing"; exit 1; }

for tool in python3 jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check and is missing. Nothing below would be about the code."
        exit 2
    }
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB="$WORK/bin"; mkdir -p "$STUB"

# A kubectl that answers from environment variables and RECORDS every call, so a
# case can tell "refused" from "refused after creating the Job".
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env bash
printf 'CALL[%s]\n' "$*" >> "${KCALLS:-/dev/null}"
args="$*"
# WIRE_STUB=1: four model ScaledObjects a-so..d-so naming the WVA scaler, and
# their KEDA HPAs, all wired unless one of these names a ScaledObject:
#   WIRE_CPU     its HPA scales on CPU (KEDA's default when the call failed)
#   WIRE_NOEMN   its status.externalMetricNames is empty (HPA still External)
#   WIRE_ABSENT  it has no HPA (KEDA deletes it while paused)
#   WIRE_PAUSED  it carries Paused=True (its HPA still External); with
#                WIRE_PAUSE_BY=annotation, only the paused-replicas annotation
#                a reset writes, before KEDA sets the condition
#   WIRE_LIVE    its HPA reports ScalingActive False, FailedGetExternalMetric
#   WIRE_HPANAME its status.hpaName is custom-<name>, the HPA named so
# WIRE_WARM=1 adds a warm-pool ScaledObject with no HPA, WIRE_FOREIGN=1 one
# with a cpu trigger, and WIRE_CONN=1 a controller log of refused handshakes
# from a KEDA Pod whose certificate does not chain to the client CA.
if [ -n "${WIRE_STUB:-}" ]; then
  wire_lists() {
    # WIRE_PAUSE_LISTS=N: WIRE_PAUSED holds only for the first N reads of the
    # ScaledObject list, as a reset's pause does until KEDA clears it.
    local paused="${WIRE_PAUSED:-}" n
    if [ -n "${WIRE_PAUSE_LISTS:-}" ]; then
      n="$(cat "${KCALLS}.wire" 2>/dev/null || echo 0)"
      [ "$n" -lt "$WIRE_PAUSE_LISTS" ] || paused=""
    fi
    jq -cn --arg addr "wva-external-scaler.ns-under-test.svc:9090" \
      --arg cpu "${WIRE_CPU:-}" --arg noemn "${WIRE_NOEMN:-}" --arg absent "${WIRE_ABSENT:-}" \
      --arg paused "$paused" --arg live "${WIRE_LIVE:-}" --arg hn "${WIRE_HPANAME:-}" \
      --arg warm "${WIRE_WARM:-0}" --arg foreign "${WIRE_FOREIGN:-0}" --arg pauseby "${WIRE_PAUSE_BY:-condition}" '
      def hname($n): if $n == $hn then "custom-" + $n else "keda-hpa-" + $n end;
      def so($n): {metadata: ({name: $n}
          + (if $n == $paused and $pauseby == "annotation"
             then {annotations: {"autoscaling.keda.sh/paused-replicas": "1"}} else {} end)),
        spec: {triggers: [{type: "external-push", metadata: {scalerAddress: $addr}}]},
        status: ({externalMetricNames: (if $n == $noemn then [] else ["s0-wva-desired-replicas"] end)}
          + (if $n == $hn then {hpaName: hname($n)} else {} end)
          + (if $n == $paused and $pauseby == "condition"
             then {conditions: [{type: "Paused", status: "True"}]} else {} end))};
      def hpa($n): {metadata: {name: hname($n)},
        spec: {metrics: [if $n == $cpu then {type: "Resource", resource: {name: "cpu"}}
                         else {type: "External", external: {metric: {name: "s0-wva-desired-replicas"}}} end]},
        status: {conditions: [if $n == $live
          then {type: "ScalingActive", status: "False", reason: "FailedGetExternalMetric", message: "tls: bad certificate"}
          else {type: "ScalingActive", status: "True", reason: "ValidMetricFound"} end]}};
      ["a-so", "b-so", "c-so", "d-so"] as $m
      | {items: ([$m[] | so(.)]
          + (if $warm == "1" then [{metadata: {name: "warm-so"}, spec: {triggers: [{type: "external-push",
               metadata: {scalerAddress: $addr, warmPoolName: "p"}}]}}] else [] end)
          + (if $foreign == "1" then [{metadata: {name: "foreign-so"}, spec: {triggers: [{type: "cpu",
               metadata: {value: "50"}}]}}] else [] end))},
        {items: ([$m[] | select(. != $absent) | hpa(.)]
          + (if $foreign == "1" then [{metadata: {name: "keda-hpa-foreign-so"}, spec: {metrics: [{type: "Resource"}]}}] else [] end))}'
  }
  case "$args" in
    *"get scaledobject"*"-o json"*|*"get hpa"*"-o json"*)
      lists="$(wire_lists)"
      set -- $args
      kind=""; name=""
      while [ $# -gt 0 ]; do
        case "$1" in
          scaledobject|hpa) kind="$1"; case "${2:-}" in -*|"") name="" ;; *) name="$2" ;; esac; break ;;
        esac
        shift
      done
      if [ "$kind" = hpa ]; then list="$(printf '%s\n' "$lists" | sed -n 2p)"; else list="$(printf '%s\n' "$lists" | sed -n 1p)"; fi
      if [ -z "$name" ] && [ "$kind" = scaledobject ]; then
        echo $(( $(cat "${KCALLS}.wire" 2>/dev/null || echo 0) + 1 )) > "${KCALLS}.wire"
      fi
      if [ -z "$name" ]; then printf '%s\n' "$list"; exit 0; fi
      one="$(printf '%s' "$list" | jq -c --arg n "$name" '.items[] | select(.metadata.name == $n)')"
      if [ -n "$one" ]; then printf '%s\n' "$one"; exit 0; fi
      echo "Error from server (NotFound): $kind \"$name\" not found" >&2
      exit 1 ;;
    *"logs -n "*"-c manager"*)
      [ "${WIRE_CONN:-0}" = 1 ] && echo '2026-10-28T20:21:56Z INFO external-scaler KEDA external scaler refused a TLS handshake {"remote": "10.0.9.9:45434", "reason": "remote error: tls: bad certificate"}'
      exit 0 ;;
    *"get pods -n keda -o json"*)
      echo '{"items":[{"metadata":{"name":"keda-operator-x"},"status":{"podIP":"10.0.9.9"}}]}'; exit 0 ;;
    *"get configmap wva-scaler-keda-auth"*"-o json"*)
      echo '{"data":{"clientCert":"not a certificate"}}'; exit 0 ;;
    *"get secret wva-external-scaler-client-tls"*"-o json"*)
      echo '{"data":{"ca.crt":"bm90IGEgY2VydGlmaWNhdGU="}}'; exit 0 ;;
  esac
fi
case "$args" in
  *"get scaledobject -o json"*)
      if [ "${PD_STUB:-0}" = "1" ]; then
        echo '{"items":[{"metadata":{"name":"pd-decode-wva"},"spec":{"scaleTargetRef":{"name":"pd-decode"},"triggers":[{"metadata":{}}]}},
                        {"metadata":{"name":"pd-prefill-wva"},"spec":{"scaleTargetRef":{"name":"pd-prefill"},"triggers":[{"metadata":{}}]}}]}'
      elif [ "${TWO_PD_STUB:-0}" = "1" ]; then
        # Two P/D stacks (two-model-shapes-pd): a ScaledObject per ROLE per
        # model, on the Deployments the route resolves to.
        so() { printf '{"metadata":{"name":"%s-wva"},"spec":{"scaleTargetRef":{"name":"%s"},"triggers":[{"metadata":{}}]}}' "$1" "$1"; }
        echo "{\"items\":[$(so unsloth--244120d9-instruct-decode),$(so unsloth--244120d9-instruct-prefill),$(so qwen-qwe-6e036fd5-qwen3-8b-decode),$(so qwen-qwe-6e036fd5-qwen3-8b-prefill)]}"
      elif [ "${SIM4_STUB:-0}" = "1" ]; then
        # Four simulator stacks, one ScaledObject each.
        so() { printf '{"metadata":{"name":"%s-so"},"spec":{"scaleTargetRef":{"name":"%s-decode"},"triggers":[{"metadata":{}}]}}' "$1" "$1"; }
        echo "{\"items\":[$(so llama-31-8b),$(so qwen3-8b),$(so qwen3-30b-a3b),$(so qwen3-32b)]}"
      elif [ "${MIXED_SO:-0}" = "1" ]; then
        # A mixed-size run: each ScaledObject's trigger names its model.
        echo '{"items":[{"metadata":{"name":"so-a"},"spec":{"scaleTargetRef":{"name":"qwen3-30b-a3b-decode"},"triggers":[{"metadata":{"modelID":"Qwen/Qwen3-30B-A3B-FP8"}}]}},
                        {"metadata":{"name":"so-b"},"spec":{"scaleTargetRef":{"name":"qwen3-8b-decode"},"triggers":[{"metadata":{"modelID":"Qwen/Qwen3-8B"}}]}}]}'
      else
        echo '{"items":[]}'
      fi
      exit 0 ;;
  *"get scaledobject so-a -o json"*)
      echo '{"metadata":{"name":"so-a"},"spec":{"triggers":[{"metadata":{"modelID":"Qwen/Qwen3-30B-A3B-FP8"}}]}}'; exit 0 ;;
  *"get scaledobject so-b -o json"*)
      echo '{"metadata":{"name":"so-b"},"spec":{"triggers":[{"metadata":{"modelID":"Qwen/Qwen3-8B"}}]}}'; exit 0 ;;
  # Node data, asked as the controller's ServiceAccount. CAN_NODES / CAN_PODS:
  # yes, no, or unknown (this identity may not impersonate it).
  *"auth can-i list nodes"*) can="${CAN_NODES:-unknown}" ;;&
  *"auth can-i list pods"*) can="${CAN_PODS:-unknown}" ;;&
  *"auth can-i"*)
      case "${can:-unknown}" in
        yes) echo yes; exit 0 ;;
        no) echo no; exit 1 ;;
        *) echo 'Error from server (Forbidden): User "me" cannot impersonate resource "serviceaccounts"' >&2; exit 1 ;;
      esac ;;
  # STATUS_JSON: the controller's own status ConfigMap; unset, it is absent.
  *"get configmap wva-controller-status"*) [ -n "${STATUS_JSON:-}" ] || exit 1; echo "$STATUS_JSON"; exit 0 ;;
  *"get deploy wva-controller-manager -o jsonpath={.status.readyReplicas}"*) echo "${CTL_READY:-1}"; exit 0 ;;
  # The controller, which decides whether the policy is the benchmark's own:
  # namespace-scoped (the standup's install) unless CTL_SCOPE says otherwise.
  *"get deploy wva-controller-manager"*"-o json"*)
      # CTL_NODE_SELECTOR: the gpu-inventory node selector the controller runs with.
      if [ -n "${CTL_NODE_SELECTOR:-}" ]; then
        echo '{"spec":{"template":{"spec":{"containers":[{"args":["--watch-namespace=$(WVA_WATCH_NAMESPACE)"],
               "env":[{"name":"POD_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.namespace"}}},
                      {"name":"WVA_WATCH_NAMESPACE","value":"$(POD_NAMESPACE)"},
                      {"name":"WVA_NODE_SELECTOR","value":"'"$CTL_NODE_SELECTOR"'"}]}]}}}}'
        exit 0
      fi
      case "${CTL_SCOPE:-own}" in
        own) echo '{"spec":{"template":{"spec":{"containers":[{"args":["--watch-namespace=$(WVA_WATCH_NAMESPACE)"],
               "env":[{"name":"POD_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.namespace"}}},
                      {"name":"WVA_WATCH_NAMESPACE","value":"$(POD_NAMESPACE)"}]}]}}}}' ;;
        # An older install: the flag names the pod namespace variable directly.
        pod) echo '{"spec":{"template":{"spec":{"containers":[{"args":["--watch-namespace=$(POD_NAMESPACE)"],
               "env":[{"name":"POD_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.namespace"}}}]}]}}}}' ;;
        cluster) echo '{"spec":{"template":{"spec":{"containers":[{"args":["--leader-elect"]}]}}}}' ;;
        # The flag twice: the controller takes the last, here every namespace.
        multiple) echo '{"spec":{"template":{"spec":{"containers":[{"args":["--watch-namespace=ns-under-test","--watch-namespace="]}]}}}}' ;;
        # A namespace from a ConfigMap key: nothing this check can resolve.
        configmap) echo '{"spec":{"template":{"spec":{"containers":[{"args":["--watch-namespace=$(NS_FROM_CM)"],
               "env":[{"name":"NS_FROM_CM","valueFrom":{"configMapKeyRef":{"name":"x","key":"ns"}}}]}]}}}}' ;;
        # The flag and its value as two arguments, and a sidecar listed first.
        twoarg) echo '{"spec":{"template":{"spec":{"containers":[{"name":"proxy","args":["--upstream=x"]},
               {"name":"manager","args":["--watch-namespace","ns-under-test"]}]}}}}' ;;
        # kubectl writes throttling notices to stderr; they are not the JSON.
        noisy) echo 'I1009 throttling request took 1.2s' >&2
               echo '{"spec":{"template":{"spec":{"containers":[{"args":["--watch-namespace=ns-under-test"]}]}}}}' ;;
        fail) echo 'Error from server (Forbidden): deployments.apps is forbidden' >&2; exit 1 ;;
        *) echo '{"spec":{"template":{"spec":{"containers":[{"args":["--watch-namespace='"$CTL_SCOPE"'"]}]}}}}' ;;
      esac
      exit 0 ;;
  *"get scaledobject pd-prefill-wva"*"scaleTargetRef"*) echo "pd-prefill"; exit 0 ;;
  *"get scaledobject pd-decode-wva"*"scaleTargetRef"*) echo "pd-decode"; exit 0 ;;
  # TWO_PD_STUB's ScaledObjects are named <target>-wva.
  *"get scaledobject "*"-wva -o jsonpath={.spec.scaleTargetRef.name}"*)
      n="${args#*get scaledobject }"; echo "${n%%-wva *}"; exit 0 ;;
  # As the modelservice chart renders them: the role is on the pod template,
  # and the Deployment's own labels carry none -- plus an EPP with no role.
  *"get deploy -o json"*)
      if [ "${PD_STUB:-0}" = "1" ]; then
        echo '{"items":[
          {"metadata":{"name":"pd-decode","labels":{"helm.sh/chart":"llm-d-modelservice"}},
           "spec":{"template":{"metadata":{"labels":{"llm-d.ai/role":"decode"}}}}},
          {"metadata":{"name":"pd-prefill","labels":{"helm.sh/chart":"llm-d-modelservice"}},
           "spec":{"template":{"metadata":{"labels":{"llm-d.ai/role":"prefill"}}}}},
          {"metadata":{"name":"pd-router-epp"},"spec":{"template":{"metadata":{"labels":{}}}}}]}'
        exit 0
      fi ;;&
  *"get deploy pd-prefill -o jsonpath={.spec.template.metadata.labels"*) echo "prefill"; exit 0 ;;
  *"get deploy pd-decode -o jsonpath={.spec.template.metadata.labels"*) echo "decode"; exit 0 ;;
  *"get deploy "*"-prefill -o jsonpath={.spec.template.metadata.labels"*) echo "prefill"; exit 0 ;;
  *"get deploy "*"-decode -o jsonpath={.spec.template.metadata.labels"*) echo "decode"; exit 0 ;;
  *"get deploy wva-warm-pool-"*)
      [ "${POOL_EXISTS:-0}" = "1" ] && exit 0
      echo 'Error from server (NotFound): deployments.apps "wva-warm-pool-x" not found' >&2
      exit 1 ;;
  *"get svc -o json"*)
      case "${GATEWAYS:-1}" in
        0) echo '{"items":[]}' ;;
        1) echo '{"items":[{"metadata":{"name":"infra-llmdbench-inference-gateway"},"spec":{"ports":[{"port":80}]}}]}' ;;
        # BOTH on the HTTP port, or it is not an ambiguity: a second gateway
        # that exposes no port 80 is correctly resolvable, and the case would
        # pass while proving nothing.
        *) echo '{"items":[{"metadata":{"name":"infra-llmdbench-inference-gateway"},"spec":{"ports":[{"name":"http","port":80}]}},
                            {"metadata":{"name":"other-inference-gateway"},"spec":{"ports":[{"name":"http","port":80}]}}]}' ;;
      esac
      exit 0 ;;
  *"get svc "*"jsonpath={.spec.clusterIP}"*) echo "${GW_IP-10.16.1.221}"; exit 0 ;;
  *"get deploy -o name"*)
      i=0
      while [ "$i" -lt "${EPP_COUNT:-2}" ]; do echo "deployment.apps/model$i-epp"; i=$((i+1)); done
      exit 0 ;;
  *"get deploy -o json"*)
      echo '{"items":[
        {"metadata":{"name":"llama-31-8b-decode"},"status":{"readyReplicas":1,"replicas":1},
         "spec":{"template":{"spec":{"containers":[{"image":"ghcr.io/example/vllm:v1"}]}}}},
        {"metadata":{"name":"qwen3-8b-decode"},"status":{"readyReplicas":1,"replicas":1},
         "spec":{"template":{"spec":{"containers":[{"image":"ghcr.io/example/vllm:v1"}]}}}}]}'
      exit 0 ;;
  # A single Deployment's status, which is how verify and reset read the fleet.
  # Without this branch the readiness check reads empty and verify refuses for
  # the wrong reason -- which is how the EPP case first passed while proving
  # nothing about EPPs.
  # PREFILL_READY: a prefill Deployment's count, apart from decode's.
  *"get deploy "*"-prefill -o jsonpath={.status."*) echo "${PREFILL_READY:-${READY:-1}}"; exit 0 ;;
  *"get deploy "*"-o jsonpath={.status.readyReplicas}"*) echo "${READY:-1}"; exit 0 ;;
  *"get deploy "*"-o jsonpath={.status.replicas}"*) echo "${READY:-1}"; exit 0 ;;
  # The routing table, which is how a stack name resolves to its backend. Names
  # are the HASH form the harness actually generates, not the scenario's pinned
  # shortName -- which it ignores.
  *"get httproute -o json"*)
      if [ "${ROUTES:-1}" = "1" ]; then
        echo '{"items":[{"metadata":{"name":"multi-model-route"},"spec":{"rules":[
          {"matches":[{"path":{"value":"/llama-31-8b"}}],"backendRefs":[{"name":"unsloth--244120d9-instruct-router"}]},
          {"matches":[{"path":{"value":"/qwen3-8b"}}],"backendRefs":[{"name":"qwen-qwe-6e036fd5-qwen3-8b-router"}]}]}}]}'
      else
        echo '{"items":[]}'
      fi
      exit 0 ;;
  *"get deploy "*"-o name"*) [ "${DEPLOY_EXISTS:-1}" = "1" ] && exit 0; exit 1 ;;
  # The probe Pod's lifecycle. Without these the probe polls for its Pod to
  # finish, against a stub that never says it did -- 180s per attempt, three
  # attempts, two models, and the check takes twenty minutes to say nothing.
  *"get pod probe-"*"jsonpath={.status.phase}"*) echo "Succeeded"; exit 0 ;;
  *"logs probe-"*) echo "${PROBE_RESULT:-HTTP 200}"; exit 0 ;;
  # The controller's own Prometheus URL, and the flow-control probe. FC_MODELS
  # is what Prometheus answers: the model_name labels that actually have a
  # flow-control series.
  *"get cm wva-manager-config"*) echo "PROMETHEUS_BASE_URL: \"${PROM_URL-https://prom.example:9090}\""; exit 0 ;;
  # The preflight TLS probe of that Prometheus (curl with the controller's CA).
  # TLSPROBE_EXIT is curl's exit code; TLSPROBE_OUT what it printed.
  *"get pod tlsprobe-"*"jsonpath={.status.phase}"*)
      [ "${TLSPROBE_EXIT:-0}" = 0 ] && echo "Succeeded" || echo "Failed"; exit 0 ;;
  *"get pod tlsprobe-"*"exitCode"*) echo "${TLSPROBE_EXIT:-0}"; exit 0 ;;
  *"logs tlsprobe-"*) echo "${TLSPROBE_OUT:-HTTP 302}"; exit 0 ;;
  *"get pod fcheck-"*"jsonpath={.status.phase}"*) echo "Succeeded"; exit 0 ;;
  *"logs fcheck-"*)
      # FCHECK_OUT stands in for whatever the probe printed, so a query that
      # never answered can be told apart from one that answered "no series".
      if [ -n "${FCHECK_OUT:-}" ]; then echo "$FCHECK_OUT"; else
        echo "MODELS ${FC_MODELS-unsloth/Meta-Llama-3.1-8B-Instruct Qwen/Qwen3-8B}"; fi
      exit 0 ;;
  *"rollout restart"*|*"rollout status"*) exit 0 ;;
  # The scaling policy the share arms rewrite, and the query Pod that reads
  # the controller's mode back. PQUERY_OUT is what that Pod printed.
  *"get configmap wva-scaling-policy-config"*) printf '%s' "${POLICY_TEXT:-}"; exit 0 ;;
  # STANDUP_SPEC: the scenario the standup recorded; unset, there is no record.
  *"get configmap wva-two-model-standup"*) printf '%s' "${STANDUP_SPEC:-}"; exit 0 ;;
  # Which policy a self-managed controller reads: a label on its namespace,
  # then a wva-policy namespace, then its own.
  *"get namespace wva-policy"*)
      if [ "${WVA_POLICY_NS:-0}" = "1" ]; then echo "namespace/wva-policy"; exit 0; fi
      echo 'Error from server (NotFound): namespaces "wva-policy" not found' >&2
      exit 1 ;;
  *"get namespace "*"-o json"*)
      if [ -n "${NS_POLICY_LABEL:-}" ]; then
        echo '{"metadata":{"name":"ns-under-test","labels":{"wva.llmd.ai/policy-namespace":"'"$NS_POLICY_LABEL"'"}}}'
      else
        echo '{"metadata":{"name":"ns-under-test"}}'
      fi
      exit 0 ;;
  *"get pod pquery-"*"jsonpath={.status.phase}"*) echo "Succeeded"; exit 0 ;;
  *"logs pquery-"*) echo "${PQUERY_OUT:-}"; exit 0 ;;
  # PVC_CLASS: the storage class an existing claim reports.
  *"get pvc"*) [ "${HAS_PVC:-1}" = "1" ] || exit 1
      case "$args" in *storageClassName*) printf '%s' "${PVC_CLASS:-}" ;; esac
      exit 0 ;;
  *"get pods -l llm-d.ai/warm-pool"*)
      [ "${POOL_EXISTS:-0}" = "1" ] && echo "wva-warm-pool-twomodel-abc"
      exit 0 ;;
  # TERMINATING: a decode pod still in its grace period, holding its GPUs.
  *"get pods -o json"*)
      if [ "${TERMINATING:-0}" = "1" ]; then
        echo '{"items":[{"metadata":{"name":"pd-decode-abc-1","deletionTimestamp":"2026-01-01T00:00:00Z",
               "labels":{"pod-template-hash":"abc"},
               "ownerReferences":[{"kind":"ReplicaSet","name":"pd-decode-abc"}]}}]}'
      elif [ "${TERMINATING:-0}" = "sibling" ]; then
        # A sibling Deployment whose name extends pd-decode: not the fleet.
        echo '{"items":[{"metadata":{"name":"pd-decode-b-xyz-1","deletionTimestamp":"2026-01-01T00:00:00Z",
               "labels":{"pod-template-hash":"xyz"},
               "ownerReferences":[{"kind":"ReplicaSet","name":"pd-decode-b-xyz"}]}}]}'
      else
        echo '{"items":[]}'
      fi
      exit 0 ;;
  *exec*)
      # The supervisor answers, and holds NOTHING -- the cold pool.
      echo "${RESIDENT_JSON:-[]}"
      exit 0 ;;
  *"get nodes -o json"*)
      n="${NODE_GPUS:-8}"
      if [ -n "${MULTI_NODES:-}" ]; then
        # MULTI_NODES=<name>=<gpus>,...: several H200 nodes. NODE_TAINTS=
        # <name>=<key>=<value>:<effect>: one taint on one of them.
        items=""
        for e in $(printf '%s' "$MULTI_NODES" | tr ',' ' '); do
          nm="${e%%=*}"; g="${e#*=}"; taints=""
          case "${NODE_TAINTS:-}" in
            "$nm="*) t="${NODE_TAINTS#*=}"; kv="${t%:*}"
                     taints=",\"taints\":[{\"key\":\"${kv%%=*}\",\"value\":\"${kv#*=}\",\"effect\":\"${t##*:}\"}]" ;;
          esac
          items="${items:+$items,}{\"spec\":{\"x\":1${taints}},\"status\":{\"allocatable\":{\"cpu\":\"${NODE_CPU:-127}\",\"memory\":\"2000000000Ki\",\"nvidia.com/gpu\":\"$g\"},\"conditions\":[{\"type\":\"Ready\",\"status\":\"True\"}]},\"metadata\":{\"name\":\"$nm\",\"labels\":{\"gpu.nvidia.com/model\":\"H200\"}}}"
        done
        echo "{\"items\":[${items}]}"
      elif [ "${MIXED_ACCEL:-0}" = "1" ]; then
        echo '{"items":[
          {"spec":{},"status":{"allocatable":{"cpu":"'"${NODE_CPU:-127}"'","memory":"2000000000Ki","nvidia.com/gpu":"'"$n"'"},"conditions":[{"type":"Ready","status":"True"}]},
           "metadata":{"name":"n1","labels":{"gpu.nvidia.com/model":"H200"}}},
          {"spec":{},"status":{"allocatable":{"cpu":"'"${NODE_CPU:-127}"'","memory":"2000000000Ki","nvidia.com/gpu":"'"$n"'"},"conditions":[{"type":"Ready","status":"True"}]},
           "metadata":{"name":"n2","labels":{"gpu.nvidia.com/model":"A100"}}}]}'
      else
        echo '{"items":[{"spec":{},"status":{"allocatable":{"cpu":"'"${NODE_CPU:-127}"'","memory":"2000000000Ki","nvidia.com/gpu":"'"$n"'"},"conditions":[{"type":"Ready","status":"True"}]},
           "metadata":{"name":"n1","labels":{"gpu.nvidia.com/model":"H200"}}}]}'
      fi
      exit 0 ;;
  # FOREIGN_PENDING=1: another team's two whole-node pods, Pending and bound to
  # no node. BOUND_GPUS=<n>: a Running pod on n1 holding n accelerators.
  *"get pods -A -o json"*)
      items=""
      if [ "${FOREIGN_PENDING:-0}" = "1" ]; then
        for i in 1 2; do
          items="${items:+$items,}{\"metadata\":{\"name\":\"other-$i\",\"namespace\":\"other-team\"},\"spec\":{\"containers\":[{\"resources\":{\"requests\":{\"nvidia.com/gpu\":\"8\",\"cpu\":\"8\"}}}]},\"status\":{\"phase\":\"Pending\"}}"
        done
      fi
      if [ -n "${BOUND_GPUS:-}" ]; then
        items="${items:+$items,}{\"metadata\":{\"name\":\"bound\",\"namespace\":\"other-team\"},\"spec\":{\"nodeName\":\"n1\",\"containers\":[{\"resources\":{\"requests\":{\"nvidia.com/gpu\":\"${BOUND_GPUS}\",\"cpu\":\"1\"}}}]},\"status\":{\"phase\":\"Running\"}}"
      fi
      # BOUND_ON=<node>=<gpus>,...: another team's Running pod on each node.
      for e in $(printf '%s' "${BOUND_ON:-}" | tr ',' ' '); do
        items="${items:+$items,}{\"metadata\":{\"name\":\"bound-${e%%=*}\",\"namespace\":\"other-team\"},\"spec\":{\"nodeName\":\"${e%%=*}\",\"containers\":[{\"resources\":{\"requests\":{\"nvidia.com/gpu\":\"${e#*=}\",\"cpu\":\"1\"}}}]},\"status\":{\"phase\":\"Running\"}}"
      done
      echo "{\"items\":[${items}]}"
      exit 0 ;;
  # GWCLASS=1: the cluster already runs Istio (an istio.io GatewayClass).
  *"get gatewayclass -o json"*)
      if [ "${GWCLASS:-0}" = "1" ]; then
        echo '{"items":[{"metadata":{"name":"istio"},"spec":{"controllerName":"istio.io/gateway-controller"}}]}'
      else
        echo '{"items":[]}'
      fi
      exit 0 ;;
  # ISTIO_INJECTOR=login: the reused control plane hands its gateway Pods a
  # proxy image from a registry that needs a login, and a pull secret for it.
  # SECRET_PRESENT=0: the benchmark namespace does not have that secret.
  *"istio-system get configmap -o json"*)
      if [ "${ISTIO_INJECTOR:-}" = "login" ]; then
        echo '{"items":[{"metadata":{"name":"istio-ca-root-cert"},"data":{}},
          {"metadata":{"name":"istio-sidecar-injector"},"data":{"values":"{\"global\":{\"hub\":\"registry.example.com/mesh\",\"tag\":\"1.26\",\"imagePullSecrets\":[\"mesh-pull-secret\"],\"proxy\":{\"image\":\"registry.example.com/mesh/proxyv2@sha256:13da\"}}}"}}]}'
      else
        echo '{"items":[]}'
      fi
      exit 0 ;;
  # SIM4_STUB=1: the four simulator Deployments a four-model kind run left.
  *"get deploy -l app.kubernetes.io/part-of=wva-two-model-sim"*)
      [ "${SIM4_STUB:-0}" = "1" ] && printf '%s\n' llama-31-8b-decode qwen3-8b-decode qwen3-30b-a3b-decode qwen3-32b-decode
      exit 0 ;;
  *"get secret "*"-o name"*)
      if [ "${SECRET_PRESENT:-1}" = "1" ]; then echo "secret/x"; exit 0; fi
      echo 'Error from server (NotFound): secrets "mesh-pull-secret" not found' >&2
      exit 1 ;;
  *) exit 0 ;;
esac
STUBEOF
chmod +x "$STUB/kubectl"

# A make that records the target and the scenario it was handed, so the
# teardown can be checked without running the harness's.
cat > "$STUB/make" <<'STUBEOF'
#!/usr/bin/env bash
printf 'MAKE[%s] SPEC=%s STACKS=%s\n' "$*" "${BENCHMARK_SPEC:-}" "${BENCHMARK_STACKS:-}" >> "${KCALLS:-/dev/null}"
exit 0
STUBEOF
chmod +x "$STUB/make"

# run_verb <verb...> -- the script with the stub first on PATH, into $OUT/$RC.
run_verb() {
    CALLS="$WORK/calls"; : > "$CALLS"
    OUT="$(PATH="$STUB:$PATH" KCALLS="$CALLS" BENCHMARK_NAMESPACE=ns-under-test \
        POOL_EXISTS="${POOL_EXISTS:-0}" EPP_COUNT="${EPP_COUNT:-2}" \
        GATEWAYS="${GATEWAYS:-1}" HAS_PVC="${HAS_PVC:-1}" \
        NODE_GPUS="${NODE_GPUS:-8}" MIXED_ACCEL="${MIXED_ACCEL:-0}" \
        RESIDENT_JSON="${RESIDENT_JSON:-[]}" \
        POLICY_TEXT="${POLICY_TEXT:-}" PQUERY_OUT="${PQUERY_OUT:-}" WVA_NS="${WVA_NS:-ns-under-test}" \
        WVA_POLICY_NS="${WVA_POLICY_NS:-0}" NS_POLICY_LABEL="${NS_POLICY_LABEL:-}" \
        CTL_SCOPE="${CTL_SCOPE:-own}" TERMINATING="${TERMINATING:-0}" \
        SHARE_ALLOW_SHARED_POLICY="${SHARE_ALLOW_SHARED_POLICY:-0}" \
        PD_STUB="${PD_STUB:-0}" PD_SINGLE_STACK="${PD_SINGLE_STACK:-0}" PD_ENDPOINT="${PD_ENDPOINT:-}" \
        MIN_PREFILL="${MIN_PREFILL:-}" MIN_DECODE="${MIN_DECODE:-}" \
        MAX_PREFILL="${MAX_PREFILL:-}" MAX_DECODE="${MAX_DECODE:-}" RESET_TIMEOUT="${RESET_TIMEOUT:-}" \
        SHARE_MODE_TIMEOUT=1 SHARE_MODE_POLL=1 \
        GPU_SAMPLE_SECONDS="${GPU_SAMPLE_SECONDS:-5}" GPU_SAMPLE_TIMEOUT_SECONDS="${GPU_SAMPLE_TIMEOUT_SECONDS:-20}" \
        WARM_GATE_TIMEOUT=1 OUT_ROOT="$WORK/out" \
        ${VERB_TIMEOUT:+timeout "$VERB_TIMEOUT"} bash "$SCRIPT" "$@" 2>&1)"
    RC=$?
}

# ---------------------------------------------------------------------------
# The arm must match the cluster
# ---------------------------------------------------------------------------
case_begin
POOL_EXISTS=0 run_verb run pool
if [ "$RC" -eq 0 ]; then
    fail "'run pool' was accepted with no pool present; the results would be the nopool arm under a pool label"
elif ! printf '%s' "$OUT" | grep -q 'pool-create'; then
    fail "'run pool' refused without saying what to do about it: $OUT"
elif grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "'run pool' created something before refusing: $(cat "$CALLS")"
else
    ok "'run pool' with no pool is refused, before anything is created"
fi

case_begin
POOL_EXISTS=1 run_verb run nopool
if [ "$RC" -eq 0 ]; then
    fail "'run nopool' was accepted while a pool held accelerators; its GPU-seconds would carry the pool's cost into the arm that exists to show life without one"
elif ! printf '%s' "$OUT" | grep -q 'pool-delete'; then
    fail "'run nopool' refused without naming the fix: $OUT"
else
    ok "'run nopool' with a pool present is refused"
fi

# A controller outside the benchmark namespace serves others: its policy is
# theirs too, and the share arms must not rewrite it.
case_begin
WVA_NS=shared-controller POLICY_TEXT='limiters: []' VERB_TIMEOUT=60 run_verb run share
if [ "$RC" -eq 0 ]; then
    fail "'run share' was accepted against a controller in another namespace"
elif ! printf '%s' "$OUT" | grep -q 'shared with every namespace'; then
    fail "'run share' refused a shared controller without saying why: $OUT"
elif grep -q 'patch configmap' "$CALLS" 2>/dev/null; then
    fail "'run share' rewrote a shared policy before refusing: $(cat "$CALLS")"
else
    ok "'run share' refuses to rewrite a policy a controller in another namespace serves"
fi

# The utilization-share arms compare two optimizers under ONE quota. Without a
# quota the optimizer has no budget, reports `invalid`, and the arm would be
# today's optimizer under the name share.
case_begin
POLICY_TEXT='limiters: []' VERB_TIMEOUT=60 run_verb run share
if [ "$RC" -eq 0 ]; then
    fail "'run share' was accepted under a policy with no quota limiter"
elif ! printf '%s' "$OUT" | grep -q 'WVA_LIMITER=quota'; then
    fail "'run share' refused without naming the fix: $OUT"
elif grep -q 'CALL\[.*patch configmap' "$CALLS" 2>/dev/null || grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "'run share' changed something before refusing: $(cat "$CALLS")"
else
    ok "'run share' with no quota in the policy is refused, before anything is written"
fi

# The policy is intent; the controller's mode is the fact. A controller that
# never re-read the policy is still running the previous arm.
QUOTA_POLICY='limiters:
  - name: q
    type: quota
    scope: namespace
    namespaceQuotas:
      ns-under-test:
        H200: 5'
case_begin
POLICY_TEXT="$QUOTA_POLICY" PQUERY_OUT='RESULT {"max by (mode) (wva_utilization_share_mode{namespace=\"ns-under-test\"})": [{"metric": {"mode": "off"}, "value": [0, "1"]}]}' \
    VERB_TIMEOUT=60 run_verb run share
if [ "$RC" -eq 0 ]; then
    fail "'run share' started while the controller reported mode off; it would measure today's optimizer as share"
elif ! printf '%s' "$OUT" | grep -q 'not running the optimizer mode'; then
    fail "'run share' refused without naming the mode as the reason: $OUT"
elif ! grep 'patch configmap wva-scaling-policy-config' "$CALLS" | grep -q 'shadow: false'; then
    fail "'run share' did not write an acting optimizer block into the policy: $(cat "$CALLS")"
elif ! grep 'patch configmap wva-scaling-policy-config' "$CALLS" | grep -q 'H200: 5'; then
    fail "'run share' dropped the quota when it wrote the optimizer block: $(cat "$CALLS")"
elif grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "'run share' started the load before the mode was confirmed: $(cat "$CALLS")"
else
    ok "'run share' writes the acting block, keeps the quota, and waits for the controller to report it"
fi

# The share arms rewrite the benchmark namespace's policy. A controller that
# reads another one -- a wva-policy namespace, or the namespace its own is
# labelled to read -- would run every arm under a quota nobody wrote down, and
# that policy is shared, so it is not the benchmark's to rewrite.
case_begin
WVA_POLICY_NS=1 POLICY_TEXT="$QUOTA_POLICY" VERB_TIMEOUT=60 run_verb run today
if [ "$RC" -eq 0 ]; then
    fail "'run today' was accepted while a wva-policy namespace exists; the controller reads that policy, not the one the arm rewrites"
elif ! printf '%s' "$OUT" | grep -q 'shared'; then
    fail "'run today' refused without saying the wva-policy policy is shared: $OUT"
elif grep -q 'patch configmap' "$CALLS" 2>/dev/null; then
    fail "'run today' rewrote a policy before refusing: $(cat "$CALLS")"
else
    ok "'run today' refuses when a wva-policy namespace holds the policy in force"
fi

case_begin
NS_POLICY_LABEL=team-policy POLICY_TEXT="$QUOTA_POLICY" VERB_TIMEOUT=60 run_verb run today
if [ "$RC" -eq 0 ]; then
    fail "'run today' was accepted while its namespace is labelled to read team-policy's policy"
elif ! printf '%s' "$OUT" | grep -q 'team-policy'; then
    fail "'run today' refused without naming the labelled policy namespace: $OUT"
elif grep -q 'patch configmap' "$CALLS" 2>/dev/null; then
    fail "'run today' rewrote a policy before refusing: $(cat "$CALLS")"
else
    ok "'run today' refuses when its namespace is labelled to read another namespace's policy"
fi

# Sitting in the benchmark namespace is not being namespace-scoped: a
# cluster-scoped controller installed there manages every namespace, and so does
# its policy.
case_begin
CTL_SCOPE=cluster POLICY_TEXT="$QUOTA_POLICY" VERB_TIMEOUT=60 run_verb run today
if [ "$RC" -eq 0 ]; then
    fail "'run today' was accepted with a controller that watches every namespace"
elif ! printf '%s' "$OUT" | grep -q 'watches every namespace'; then
    fail "'run today' refused a cluster-scoped controller without saying why: $OUT"
elif grep -q 'patch configmap' "$CALLS" 2>/dev/null; then
    fail "'run today' rewrote a policy before refusing: $(cat "$CALLS")"
else
    ok "'run today' refuses a controller that watches every namespace, even in its own"
fi

case_begin
CTL_SCOPE=team-a POLICY_TEXT="$QUOTA_POLICY" VERB_TIMEOUT=60 run_verb run today
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'watches team-a'; then
    fail "'run today' with a controller watching another namespace: rc $RC, $OUT"
else
    ok "'run today' refuses a controller that watches another namespace"
fi

# Both shapes the installs render resolve to the pod's own namespace: the
# chain through WVA_WATCH_NAMESPACE (every case above), and an older install
# naming POD_NAMESPACE in the flag -- which a first version of the guard read
# as a namespace called "$(POD_NAMESPACE)" and refused on a real cluster.
case_begin
CTL_SCOPE=pod POLICY_TEXT="$QUOTA_POLICY" VERB_TIMEOUT=60 run_verb run today
if printf '%s' "$OUT" | grep -q 'watches'; then
    fail "'run today' refused a controller watching \$(POD_NAMESPACE), its own namespace: $OUT"
elif ! grep -q 'get namespace ns-under-test' "$CALLS"; then
    fail "'run today' never reached the check after the scope check: $OUT"
else
    ok "'run today' accepts a controller watching its own namespace through POD_NAMESPACE"
fi

# The scope check must not be fooled, and must not refuse a correct install.
# guard_case <scope> <want: refuse|accept> <phrase> <description>
guard_case() {
    case_begin
    CTL_SCOPE="$1" POLICY_TEXT="$QUOTA_POLICY" VERB_TIMEOUT=60 run_verb run today
    if [ "$2" = refuse ]; then
        if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q "$3"; then
            fail "$4: rc $RC, want a refusal saying '$3': $OUT"
        elif grep -q 'patch configmap' "$CALLS" 2>/dev/null; then
            fail "$4: a policy was rewritten before the refusal"
        else
            ok "$4"
        fi
    elif printf '%s' "$OUT" | grep -qE 'watches|more than once|cannot resolve|could not'; then
        fail "$4: refused: $OUT"
    elif ! grep -q 'get namespace ns-under-test' "$CALLS"; then
        fail "$4: never reached the check after the scope check: $OUT"
    else
        ok "$4"
    fi
}
guard_case multiple refuse 'more than once' "'run today' refuses a controller giving --watch-namespace twice (it takes the last)"
guard_case configmap refuse 'cannot resolve' "'run today' refuses a watch namespace it cannot resolve"
guard_case fail refuse 'could not read the controller' "'run today' refuses when it cannot read the controller"
guard_case twoarg accept '' "'run today' reads --watch-namespace as two arguments, in the manager container"
guard_case noisy accept '' "'run today' keeps kubectl stderr out of the JSON it parses"

case_begin
SHARE_ALLOW_SHARED_POLICY=1 CTL_SCOPE=cluster POLICY_TEXT="$QUOTA_POLICY" VERB_TIMEOUT=60 run_verb run today
if printf '%s' "$OUT" | grep -q 'watches every namespace'; then
    fail "SHARE_ALLOW_SHARED_POLICY=1 did not let a cluster-scoped controller through: $OUT"
else
    ok "SHARE_ALLOW_SHARED_POLICY=1 lets an operator who owns the policy through"
fi

# Reset settles only when no pod of the fleet is still terminating: one in its
# grace period still holds its GPUs, and the next arm would start short.
case_begin
TERMINATING=1 PD_STUB=1 PD_SINGLE_STACK=1 MIN_PREFILL=1 MIN_DECODE=1 RESET_TIMEOUT=1 run_verb reset
RC_TERM="$RC"
TERMINATING=0 PD_STUB=1 PD_SINGLE_STACK=1 MIN_PREFILL=1 MIN_DECODE=1 RESET_TIMEOUT=1 run_verb reset
TERMINATING=sibling PD_STUB=1 PD_SINGLE_STACK=1 MIN_PREFILL=1 MIN_DECODE=1 RESET_TIMEOUT=1 run_verb reset
RC_SIB="$RC"
if [ "$RC_TERM" -eq 0 ]; then
    fail "reset settled while a decode pod was still terminating"
elif [ "$RC" -ne 0 ]; then
    fail "control: reset did not settle with no pod terminating: $OUT"
elif [ "$RC_SIB" -ne 0 ]; then
    fail "reset waited on a terminating pod of pd-decode-b, a sibling Deployment, not the fleet's pd-decode"
else
    ok "reset waits for the fleet's terminating pods, and only the fleet's"
fi

# A single-stack P/D model: one prefill and one decode Deployment. Every arm
# starts from each ROLE's floor; a reset that scaled only decode left prefill
# frozen at whatever the previous arm grew it to.
case_begin
PD_STUB=1 PD_SINGLE_STACK=1 MIN_PREFILL=2 MIN_DECODE=1 RESET_TIMEOUT=1 run_verb reset
if ! grep -q 'scale deploy pd-prefill --replicas=2' "$CALLS"; then
    fail "P/D reset did not scale prefill to MIN_PREFILL: $(grep scale "$CALLS")"
elif ! grep -q 'scale deploy pd-decode --replicas=1' "$CALLS"; then
    fail "P/D reset did not scale decode to MIN_DECODE: $(grep scale "$CALLS")"
elif ! grep 'patch scaledobject pd-prefill-wva' "$CALLS" | grep -q 'minReplicaCount.:2'; then
    fail "P/D reset did not set prefill's floor on its ScaledObject: $(grep 'patch scaledobject' "$CALLS")"
else
    ok "P/D reset puts each role at its own floor, prefill included"
fi

# The ceiling is per role: one number for both let prefill hold GPUs it never
# uses, or capped decode below its need.
case_begin
PD_STUB=1 PD_SINGLE_STACK=1 PD_ENDPOINT=http://pd-router.ns-under-test.svc:80 \
    MAX_PREFILL=4 MAX_DECODE=3 POLICY_TEXT="$QUOTA_POLICY" \
    PQUERY_OUT='RESULT {"max by (mode) (wva_utilization_share_mode{namespace=\"ns-under-test\"})": [{"metric": {"mode": "off"}, "value": [0, "1"]}]}' \
    VERB_TIMEOUT=60 run_verb run today
if ! grep 'patch scaledobject pd-prefill-wva' "$CALLS" | grep -q 'maxReplicaCount.:4'; then
    fail "P/D run did not cap prefill at MAX_PREFILL: $(grep 'maxReplicaCount' "$CALLS")"
elif ! grep 'patch scaledobject pd-decode-wva' "$CALLS" | grep -q 'maxReplicaCount.:3'; then
    fail "P/D run did not cap decode at MAX_DECODE: $(grep 'maxReplicaCount' "$CALLS")"
elif ! grep -q "base_url: 'http://pd-router.ns-under-test.svc:80'" "$WORK/out/today/profile-a.yaml" 2>/dev/null || \
     ! grep -q "base_url: 'http://pd-router.ns-under-test.svc:80'" "$WORK/out/today/profile-b.yaml" 2>/dev/null; then
    fail "P/D run did not send both loaders to PD_ENDPOINT: $(grep -h -i 'url\|endpoint\|base' "$WORK"/out/today/profile-*.yaml 2>/dev/null | head -4)"
else
    ok "P/D run caps each role at its own ceiling and drives PD_ENDPOINT"
fi

case_begin
PD_SINGLE_STACK=1 VERB_TIMEOUT=60 run_verb standup
if [ "$RC" -eq 0 ]; then
    fail "standup ran with PD_SINGLE_STACK=1; the two-stack standup would render the wrong scenario"
elif ! printf '%s' "$OUT" | grep -q 'BENCHMARK_SPEC=guides/pd-disaggregation'; then
    fail "standup refused P/D without naming the standup to use: $OUT"
else
    ok "standup refuses a single-stack P/D run and names the standup to use"
fi

# ---------------------------------------------------------------------------
# TWO P/D stacks (two-model-shapes-pd), driven through the shared route, not
# PD_SINGLE_STACK: each model's decode and its prefill are separate scaled roles.
# ---------------------------------------------------------------------------
# The peak counts both roles of both models. Counted as one role per model it
# was HALF the fleet, and preflight passed on a cluster with room for half.
case_begin
BENCH_SPEC=guides/two-model-shapes-pd NODE_GPUS=16 run_verb preflight
PD_PEAK_OUT="$OUT"
BENCH_SPEC=guides/two-model-shapes NODE_GPUS=16 run_verb preflight
if ! printf '%s' "$PD_PEAK_OUT" | grep -q 'peaks at 12'; then
    fail "preflight for two P/D models (2 x (prefill 3 + decode 3)) did not report a peak of 12: $(printf '%s' "$PD_PEAK_OUT" | grep peaks)"
elif ! printf '%s' "$PD_PEAK_OUT" | grep -q 'P/D: every model is two roles'; then
    fail "preflight for two P/D models did not state the per-role ranges: $PD_PEAK_OUT"
elif ! printf '%s' "$OUT" | grep -q 'peaks at 6'; then
    fail "control: the aggregated two-model-shapes no longer peaks at 6: $(printf '%s' "$OUT" | grep peaks)"
else
    ok "preflight counts prefill and decode of both P/D models, and an aggregated scenario as before"
fi

case_begin
TWO_PD_STUB=1 MIN_PREFILL=2 MIN_DECODE=1 RESET_TIMEOUT=1 run_verb reset
miss=""
for d in unsloth--244120d9-instruct-prefill qwen-qwe-6e036fd5-qwen3-8b-prefill; do
    grep -q "scale deploy $d --replicas=2" "$CALLS" || miss="$miss $d"
    grep "patch scaledobject $d-wva" "$CALLS" | grep -q 'minReplicaCount.:2' || miss="$miss $d-wva"
done
for d in unsloth--244120d9-instruct-decode qwen-qwe-6e036fd5-qwen3-8b-decode; do
    grep -q "scale deploy $d --replicas=1" "$CALLS" || miss="$miss $d"
done
if [ -n "$miss" ]; then
    fail "two-stack P/D reset did not put every role of both models at its floor; missing:$miss
$(grep -E 'scale deploy|minReplicaCount' "$CALLS")"
else
    ok "two-stack P/D reset puts both models' prefill and decode at their own floors"
fi

case_begin
TWO_PD_STUB=1 MAX_PREFILL=4 MAX_DECODE=3 POLICY_TEXT="$QUOTA_POLICY" \
    PQUERY_OUT='RESULT {"max by (mode) (wva_utilization_share_mode{namespace=\"ns-under-test\"})": [{"metric": {"mode": "off"}, "value": [0, "1"]}]}' \
    VERB_TIMEOUT=60 run_verb run today
miss=""
for so in unsloth--244120d9-instruct-prefill qwen-qwe-6e036fd5-qwen3-8b-prefill; do
    grep "patch scaledobject $so-wva" "$CALLS" | grep -q 'maxReplicaCount.:4' || miss="$miss $so"
done
for so in unsloth--244120d9-instruct-decode qwen-qwe-6e036fd5-qwen3-8b-decode; do
    grep "patch scaledobject $so-wva" "$CALLS" | grep -q 'maxReplicaCount.:3' || miss="$miss $so"
done
if [ -n "$miss" ]; then
    fail "two-stack P/D run did not cap every role at its own ceiling; wrong or missing:$miss
$(grep maxReplicaCount "$CALLS")"
else
    ok "two-stack P/D run caps both models' prefill at MAX_PREFILL and decode at MAX_DECODE"
fi

# A P/D model with no ready prefill serves through decode alone; the run would
# measure an aggregated model under a P/D name.
case_begin
TWO_PD_STUB=1 DEPLOY_EXISTS=1 ROUTES=1 PREFILL_READY=0 run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed two P/D stacks whose prefill Deployments have no ready replica"
elif ! printf '%s' "$OUT" | grep -q 'prefill Deployment .*-prefill has no ready replica'; then
    fail "verify refused without naming the unready prefill Deployment: $OUT"
else
    ok "verify refuses a P/D stack whose prefill is not serving"
fi

# The floor arm raises EVERY model ScaledObject, so it must wait for every role.
# Waiting on decode alone started the arm while prefill was still short.
case_begin
TWO_PD_STUB=1 FLOOR_REPLICAS=2 READY=2 PREFILL_READY=1 RESET_TIMEOUT=1 VERB_TIMEOUT=60 run_verb run floor
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'did not reach the floor'; then
    fail "the floor arm started while both prefill Deployments were below the floor (decode at 2, prefill at 1): rc $RC
$(printf '%s' "$OUT" | tail -5)"
else
    ok "the floor arm waits for prefill as well as decode"
fi

# A floor above a role's ceiling cannot be reached: KEDA refuses the patch. The
# arm must say so at once, not wait RESET_TIMEOUT and report a missed floor.
case_begin
TWO_PD_STUB=1 FLOOR_REPLICAS=2 MAX_PREFILL=1 READY=2 RESET_TIMEOUT=1 VERB_TIMEOUT=60 run_verb run floor
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q "above a role's ceiling (1)"; then
    fail "the floor arm did not refuse a floor of 2 over a prefill ceiling of 1: rc $RC
$(printf '%s' "$OUT" | tail -5)"
else
    ok "the floor arm refuses a floor above a role's ceiling, by name"
fi

# A COLD pool is the worst result this scenario can produce: the arm runs to
# completion and reports the cost of a pool nobody would operate that way.
case_begin
POOL_EXISTS=1 RESIDENT_JSON='[]' run_verb run pool
if [ "$RC" -eq 0 ]; then
    fail "'run pool' started against a pool holding neither model; it would measure a cold pool and report it as the pool's cost"
elif ! printf '%s' "$OUT" | grep -qi 'cold pool'; then
    fail "'run pool' refused without naming the cold pool as the reason: $OUT"
elif ! printf '%s' "$OUT" | grep -q 'warm'; then
    fail "'run pool' refused without naming the step that fixes it: $OUT"
elif grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "'run pool' created the load Job before refusing: $(cat "$CALLS")"
else
    ok "'run pool' against a pool that holds neither model is refused"
fi

case_begin
POOL_EXISTS=0 run_verb run
[ "$RC" -eq 0 ] && fail "'run' with no arm was accepted" || ok "'run' requires an arm"

case_begin
POOL_EXISTS=0 run_verb run sideways
[ "$RC" -eq 0 ] && fail "'run sideways' was accepted" || ok "an unknown arm is refused"

# ---------------------------------------------------------------------------
# verify -- the guards that make a 34-minute run worth starting
# ---------------------------------------------------------------------------
case_begin
EPP_COUNT=1 run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with ONE EPP; the standup rendered one stack, and the missing model would 404 for the whole run"
elif ! printf '%s' "$OUT" | grep -qi 'epp'; then
    fail "verify refused without naming the EPP count: $OUT"
else
    ok "one EPP means one stack, and verify says so"
fi

case_begin
GATEWAYS=0 run_verb verify
[ "$RC" -eq 0 ] && fail "verify passed with no inference gateway Service" \
    || ok "no gateway Service is refused"

case_begin
GATEWAYS=2 run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with TWO gateway Services; the driver would pick one and send both models' traffic at whichever stack owns it"
else
    ok "an ambiguous gateway is refused rather than picked"
fi

# READY IS NOT WIRED. A four-model baseline ran both arms with every
# ScaledObject Ready and every KEDA HPA on the CPU default: KEDA's operator
# held a client certificate from an earlier install, the controller refused
# every call, and no model ever left one replica. verify passed.
# Judged by the installer's own wva_scaler_so_wiring (HPA metric AND the
# ScaledObject's status.externalMetricNames), plus two live signals: the HPA
# failing to read the metric, and the controller refusing KEDA's handshakes.
WIRE_FIX="the ScaledObjects above are not wired to WVA"
case_begin
WIRE_STUB=1 run_verb verify
if [ "$RC" -ne 0 ] || printf '%s' "$OUT" | grep -q "$WIRE_FIX"; then
    fail "verify refused four wired ScaledObjects (rc $RC): $(printf '%s' "$OUT" | tail -4)"
else
    ok "four wired ScaledObjects pass verify"
fi

case_begin
WIRE_STUB=1 WIRE_CPU=d-so run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with model D's HPA on the CPU default: WVA's decisions reach nothing for it"
elif ! printf '%s' "$OUT" | grep -q 'd-so: HPA keda-hpa-d-so: trigger(s) 0 have no External metric'; then
    fail "verify refused without naming the unwired ScaledObject and its trigger: $OUT"
elif printf '%s' "$OUT" | grep -q '      a-so:'; then
    fail "verify named a wired ScaledObject as unwired: $OUT"
elif ! printf '%s' "$OUT" | grep -q 'rollout restart deployment/keda-operator'; then
    fail "verify refused without naming the fix (restart KEDA's operator): $OUT"
else
    ok "an HPA on the CPU default is refused, only that model named, with the KEDA restart"
fi

case_begin
WIRE_STUB=1 WIRE_NOEMN=c-so run_verb verify
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'c-so: HPA keda-hpa-c-so: trigger(s) 0'; then
    fail "verify passed an HPA with the External metric whose ScaledObject lists no externalMetricNames (rc $RC): KEDA built the spec once and the call since failed"
else
    ok "an External HPA whose ScaledObject lists no external metric is refused"
fi

case_begin
WIRE_STUB=1 WIRE_LIVE=b-so WIRING_TIMEOUT=2 WIRING_POLL=1 run_verb verify
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'b-so: HPA keda-hpa-b-so: tls: bad certificate (FailedGetExternalMetric'; then
    fail "verify passed an HPA that cannot read WVA's metric now (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "an HPA failing to get the external metric is refused, live"
fi

case_begin
WIRE_STUB=1 WIRE_CONN=1 KEDA_NAMESPACE_EXPLICIT=true KEDA_NAMESPACE=keda run_verb verify
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q "refused TLS handshakes from KEDA's Pod(s) keda-operator-x"; then
    fail "verify passed while the controller refused KEDA's handshakes (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "a controller refusing KEDA's handshakes is refused, though every HPA looks wired"
fi

case_begin
WIRE_STUB=1 WIRE_PAUSED=a-so WIRING_TIMEOUT=2 WIRING_POLL=1 run_verb verify
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'a-so: still paused after 2s'; then
    fail "a ScaledObject still paused when the wait ran out did not refuse as paused (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "a pause that is never lifted refuses once the wait runs out"
fi

# The installer's wva_scaler_so_wiring sees the Paused condition only. The
# annotation a reset writes comes first, and KEDA may not have set the
# condition yet: judged then, the HPA still reads as wired.
case_begin
WIRE_STUB=1 WIRE_PAUSED=b-so WIRE_PAUSE_BY=annotation WIRING_TIMEOUT=2 WIRING_POLL=1 run_verb verify
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'b-so: still paused after 2s'; then
    fail "a ScaledObject paused by annotation alone passed as wired (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "a pause the annotation states before the condition does is still a pause"
fi

case_begin
rm -f "$WORK/calls.wire"
WIRE_STUB=1 WIRE_PAUSED=a-so WIRE_PAUSE_LISTS=2 WIRING_TIMEOUT=30 WIRING_POLL=1 run_verb verify
if [ "$RC" -ne 0 ] || printf '%s' "$OUT" | grep -q "$WIRE_FIX"; then
    fail "a pause lifted on the third look was judged before it lifted (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "the check waits for a reset's pause to clear, then passes"
fi

case_begin
WIRE_STUB=1 WIRE_ABSENT=d-so WIRING_TIMEOUT=2 WIRING_POLL=1 run_verb verify
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'd-so: HPA keda-hpa-d-so still absent after 2s'; then
    fail "a ScaledObject with no HPA was not refused as absent (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "an HPA KEDA never rebuilt refuses as absent once the wait runs out"
fi

case_begin
WIRE_STUB=1 WIRE_HPANAME=c-so WIRING_TIMEOUT=2 WIRING_POLL=1 run_verb verify
if [ "$RC" -ne 0 ] || printf '%s' "$OUT" | grep -q "$WIRE_FIX"; then
    fail "an HPA found through status.hpaName was not found (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "the HPA is the one status.hpaName names, not keda-hpa-<name>"
fi

case_begin
WIRE_STUB=1 WIRE_WARM=1 WIRING_TIMEOUT=2 WIRING_POLL=1 run_verb verify
if [ "$RC" -ne 0 ] || printf '%s' "$OUT" | grep -q 'warm-so'; then
    fail "a warm-pool ScaledObject was judged as a model (rc $RC): $(printf '%s' "$OUT" | tail -3)"
else
    ok "a warm-pool ScaledObject is left out of the wiring check"
fi

case_begin
WIRE_STUB=1 WIRE_FOREIGN=1 run_verb verify
if [ "$RC" -ne 0 ]; then
    fail "a ScaledObject naming no WVA scaler was refused rather than warned about: $(printf '%s' "$OUT" | tail -3)"
elif ! printf '%s' "$OUT" | grep -q 'ScaledObject foreign-so names no WVA scaler (its HPA scales on Resource)'; then
    fail "a ScaledObject naming no WVA scaler passed without a warning: $OUT"
else
    ok "a ScaledObject that names no WVA scaler is warned about, not refused"
fi

case_begin
WIRE_STUB=1 WIRE_CPU=a-so POOL_EXISTS=0 VERB_TIMEOUT=30 run_verb run nopool
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q "arm nopool refused: $WIRE_FIX"; then
    fail "an arm started with an unwired ScaledObject: rc $RC
$(printf '%s' "$OUT" | tail -5)"
elif grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "the arm created the load Job before refusing the unwired ScaledObject"
else
    ok "an arm with an unwired ScaledObject is refused before any load"
fi

# ---------------------------------------------------------------------------
# preflight -- the accelerator arithmetic that makes the arms comparable
# ---------------------------------------------------------------------------
case_begin
run_verb preflight
if ! printf '%s' "$OUT" | grep -q 'peaks at 6'; then
    fail "preflight did not report the peak this run needs (2 models x 3 replicas = 6): $OUT"
elif ! printf '%s' "$OUT" | grep -q 'pool arm:.*1\.\.3 replicas.*on top'; then
    # The SAME ceiling in every arm, with the pool on top. The old check wanted
    # the pool arm's ceiling LOWERED by the pool's share; that stopped the pool
    # arm from ever holding three real replicas and a bridge.
    fail "preflight did not state that the pool arm keeps the same per-model ceiling with the pool on top: $OUT"
elif ! printf '%s' "$OUT" | grep -q 'floor arm:.*2\.\.3 replicas'; then
    fail "preflight did not state the floor arm's range: $OUT"
else
    ok "preflight states every arm's range, and that the ceiling is the same in all of them"
fi

# The controller's Prometheus, probed with the controller's TLS settings
# before anything holds an accelerator: a certificate it would refuse fails
# preflight, and the probe never skips verification.
case_begin
run_verb preflight
if ! printf '%s' "$OUT" | grep -q "Prometheus TLS verified with the controller's settings"; then
    fail "preflight did not probe Prometheus with the controller's TLS settings: $(printf '%s' "$OUT" | grep -i tls)"
else
    ok "preflight probes Prometheus with the controller's TLS settings"
fi
case_begin
TLSPROBE_EXIT=60 TLSPROBE_OUT='curl: (60) SSL certificate problem: self-signed certificate' run_verb preflight
if [ "$RC" -eq 0 ]; then
    fail "preflight passed although curl, with the controller's CA, refused the Prometheus certificate"
elif ! printf '%s' "$OUT" | grep -q 'do not verify'; then
    fail "preflight failed on a refused certificate without saying so: $(printf '%s' "$OUT" | grep -i tls)"
else
    ok "preflight fails when the controller's TLS settings would not verify Prometheus"
fi

case_begin
NODE_GPUS=4 run_verb preflight
if [ "$RC" -eq 0 ]; then
    fail "preflight passed with 4 free accelerators and a peak of 6; the run would spend its bursts Pending and both arms would measure the scheduler"
else
    ok "a peak larger than the free accelerators is refused"
fi

case_begin
# Two accelerator names are a POOL question: preflight warns and passes (the
# models are placed by their accelerator request, and the share arms refuse a
# pool), and pool-create is where it refuses. Refusing in preflight failed runs
# that never create a pool, on nodes that carry one GPU under two label keys.
MIXED_ACCEL=1 run_verb preflight
if [ "$RC" -ne 0 ]; then
    fail "preflight refused a cluster advertising two accelerator names, though only a pool arm needs one: $(printf '%s' "$OUT" | grep -A2 'accelerator products')"
elif ! printf '%s' "$OUT" | grep -q 'set ACCELERATOR=<product> before'; then
    fail "preflight did not say a pool arm needs ACCELERATOR, and why the others do not: $OUT"
else
    ok "two accelerator names warn in preflight, naming ACCELERATOR for a pool arm, and do not fail it"
fi

case_begin
MIXED_ACCEL=1 run_verb pool-create
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'set ACCELERATOR=<product'; then
    fail "pool-create did not refuse an ambiguous accelerator; a pool on the wrong product is never eligible to lend: $OUT"
elif grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "pool-create created something before refusing: $(cat "$CALLS")"
else
    ok "pool-create refuses an ambiguous accelerator, before anything is created"
fi

# A missing model claim is normal before the first standup, which creates it,
# and node-local weights require a fresh namespace. What refuses is a claim
# that exists on a class the node-local volume could never replace.
case_begin
HAS_PVC=0 run_verb preflight
if [ "$RC" -ne 0 ] || ! printf '%s' "$OUT" | grep -q 'the standup creates it'; then
    fail "a fresh namespace with no model claim did not pass preflight: $(printf '%s' "$OUT" | grep -iE 'pvc|claim')"
else
    ok "a fresh namespace passes: the model claim is the standup's to create"
fi
case_begin
HAS_PVC=1 PVC_CLASS=shared-vast BENCHMARK_MODEL_HOSTPATH=/mnt/local/w run_verb preflight
c1="$RC"; o1="$OUT"
HAS_PVC=1 PVC_CLASS=node-local-weights BENCHMARK_MODEL_HOSTPATH=/mnt/local/w run_verb preflight
if [ "$c1" -eq 0 ] || ! printf '%s' "$o1" | grep -q "already exists on storage class 'shared-vast'"; then
    fail "node-local weights over an existing shared-class claim passed preflight; the node-local volume would never bind: $(printf '%s' "$o1" | grep -iE 'claim|pvc')"
elif [ "$RC" -ne 0 ]; then
    fail "an existing claim already on the node-local class was refused: $(printf '%s' "$OUT" | grep -iE 'claim|pvc')"
else
    ok "node-local weights refuse an existing claim on another class, and accept one on their own"
fi

case_begin
OUT="$(PATH="$STUB:$PATH" KCALLS=/dev/null BENCHMARK_NAMESPACE=ns PHASE_SECONDS=300 \
    NODE_GPUS=8 bash "$SCRIPT" preflight 2>&1)"; RC=$?
if [ "$RC" -eq 0 ]; then
    fail "preflight passed with PHASE_SECONDS=300, which is shorter than scale-down stabilization: each model holds the replicas it grew through most of the OTHER model's rise, so the anti-phase premise is never exercised"
else
    ok "a phase shorter than scale-down stabilization is refused"
fi

# ---------------------------------------------------------------------------
# A namespace is never optional -- this drives load and creates Jobs.
# ---------------------------------------------------------------------------
case_begin
OUT="$(PATH="$STUB:$PATH" BENCHMARK_NAMESPACE= bash "$SCRIPT" status 2>&1)"; RC=$?
[ "$RC" -eq 0 ] && fail "a verb ran with no BENCHMARK_NAMESPACE" \
    || ok "every verb requires a namespace"

# ---------------------------------------------------------------------------
# Both halves of the spec must exist, or the CLI cannot see it at all.
#
# `--spec` resolves config/specification/<name>.yaml.j2 and NEVER a scenario, so
# a scenario shipped on its own is invisible: the CLI reports
# "Specification '<name>' not found" and prints the list it does know, which
# reads like a missing file while the scenario is plainly sitting in the clone.
# That cost one standup.
# ---------------------------------------------------------------------------
case_begin
SPEC_DIR="$ROOT/hack/benchmark/scenarios/guides"
if [ ! -f "$SPEC_DIR/two-model-warm-pool.yaml" ]; then
    fail "the scenario two-model-warm-pool.yaml is missing"
elif [ ! -f "$SPEC_DIR/two-model-warm-pool.yaml.j2" ]; then
    fail "two-model-warm-pool.yaml exists but its SPECIFICATION (.yaml.j2) does not, so --spec cannot resolve it and the standup dies after installing the scenario"
elif ! grep -q 'scenario_file' "$SPEC_DIR/two-model-warm-pool.yaml.j2"; then
    fail "the specification does not name a scenario_file; the CLI would render the default one"
elif ! grep -q 'scenarios/guides/two-model-warm-pool.yaml' "$SPEC_DIR/two-model-warm-pool.yaml.j2"; then
    fail "the specification points at a scenario other than its own: $(grep -A1 scenario_file "$SPEC_DIR/two-model-warm-pool.yaml.j2")"
else
    ok "the scenario and its specification both exist, and the specification names it"
fi

# two-model-shapes is two-model-warm-pool with room for 20k-token prompts and
# nothing else. A copy drifts: every non-comment line but those three must
# match, and its specification must name it, not the scenario it was copied
# from.
case_begin
if [ ! -f "$SPEC_DIR/two-model-shapes.yaml" ] || [ ! -f "$SPEC_DIR/two-model-shapes.yaml.j2" ]; then
    fail "two-model-shapes.yaml or its specification is missing"
elif ! grep -q 'scenarios/guides/two-model-shapes.yaml' "$SPEC_DIR/two-model-shapes.yaml.j2"; then
    fail "the shapes specification points at a scenario other than its own: $(grep -A1 scenario_file "$SPEC_DIR/two-model-shapes.yaml.j2")"
else
    drift="$(diff <(grep -vE '^[[:space:]]*#' "$SPEC_DIR/two-model-warm-pool.yaml" | tr -d '\r') \
                  <(grep -vE '^[[:space:]]*#' "$SPEC_DIR/two-model-shapes.yaml" | tr -d '\r') | grep -E '^[<>]')"
    expected="< workDir: \"~/data/two-model-warm-pool\"
<       maxModelLen: 8192
<       maxModelLen: 8192
>   workDir: \"~/data/two-model-shapes\"
>       maxModelLen: 32768
>       maxModelLen: 32768"
    if [ "$(printf '%s\n' "$drift" | sed 's/^\([<>]\) */\1 /' | sort)" != "$(printf '%s\n' "$expected" | sed 's/^\([<>]\) */\1 /' | sort)" ]; then
        fail "two-model-shapes.yaml differs from two-model-warm-pool.yaml in more than workDir and maxModelLen: $drift"
    else
        ok "two-model-shapes differs from two-model-warm-pool only in workDir and maxModelLen, and its specification names it"
    fi
fi

# two-model-shapes-pd is two-model-shapes with both models disaggregated. Its
# STACKS must differ from two-model-shapes' only by the prefill block each one
# adds -- the models, the decode sizing and the WVA bounds are the comparison's
# fixed points, and a copy drifts.
case_begin
if [ ! -f "$SPEC_DIR/two-model-shapes-pd.yaml" ] || [ ! -f "$SPEC_DIR/two-model-shapes-pd.yaml.j2" ]; then
    fail "two-model-shapes-pd.yaml or its specification is missing"
elif ! grep -q 'scenarios/guides/two-model-shapes-pd.yaml' "$SPEC_DIR/two-model-shapes-pd.yaml.j2"; then
    fail "the P/D shapes specification points at a scenario other than its own: $(grep -A1 scenario_file "$SPEC_DIR/two-model-shapes-pd.yaml.j2")"
else
    # The scenario: list, comments and blank lines out; for the P/D file, each
    # stack's `      prefill:` block (up to the next key at its indent) too.
    stacks_of() {
        tr -d '\r' < "$1" | sed -n '/^scenario:/,$p' | grep -vE '^[[:space:]]*(#|$)' | awk '
            /^      prefill:$/ { skip = 1; next }
            skip && /^ {0,6}[^ ]/ { skip = 0 }
            !skip { print }'
    }
    drift="$(diff <(stacks_of "$SPEC_DIR/two-model-shapes.yaml") <(stacks_of "$SPEC_DIR/two-model-shapes-pd.yaml"))"
    n_prefill="$(tr -d '\r' < "$SPEC_DIR/two-model-shapes-pd.yaml" | sed -n '/^scenario:/,$p' | grep -c '^      prefill:$')"
    if [ "$n_prefill" -ne 2 ]; then
        fail "two-model-shapes-pd has $n_prefill per-stack prefill blocks, not 2 -- one per model"
    elif [ -n "$drift" ]; then
        fail "two-model-shapes-pd's stacks differ from two-model-shapes' in more than their prefill blocks: $drift"
    else
        ok "two-model-shapes-pd's stacks are two-model-shapes' plus one prefill block each, and its specification names it"
    fi
fi

# What makes it P/D, in the shared block every stack inherits. Any one missing
# does not crash: requests fall back to decode doing its own prefill, or route
# to one role, and the run measures an aggregated model under a P/D name.
case_begin
pd_body="$(tr -d '\r' < "$SPEC_DIR/two-model-shapes-pd.yaml" | grep -vE '^[[:space:]]*#')"
shared_body="$(printf '%s\n' "$pd_body" | sed -n '/^shared:/,/^scenario:/p')"
missing=""
printf '%s\n' "$shared_body" | grep -qE '^    prefill:$' || missing="$missing shared.modelservice.prefill"
printf '%s\n' "$shared_body" | sed -n '/^    prefill:$/,/^    [a-z]/p' | grep -qE '^      enabled: true$' || missing="$missing prefill.enabled=true"
printf '%s\n' "$shared_body" | grep -qE '^      connector: nixlv2$' || missing="$missing routing.connector=nixlv2"
printf '%s\n' "$shared_body" | grep -qE 'pluginsConfigFile: "pd-config.yaml"' || missing="$missing pluginsConfigFile=pd-config.yaml"
for plugin in always-disagg-pd-decider disagg-profile-handler prefill-filter decode-filter metrics-data-source; do
    printf '%s\n' "$shared_body" | grep -qE -- "- type: $plugin\$" || missing="$missing plugin:$plugin"
done
printf '%s\n' "$shared_body" | grep -qE -- '- flowControl$' || missing="$missing featureGates:flowControl"
printf '%s\n' "$shared_body" | grep -A1 -E '^    routerEndpointPicker:$' | grep -qE 'tag: v0\.11\.0$' || missing="$missing images.routerEndpointPicker=v0.11.0"
[ "$(printf '%s\n' "$pd_body" | grep -c 'kv_connector":"NixlConnector"')" -eq 2 ] || missing="$missing NixlConnector-on-both-roles"
if printf '%s\n' "$pd_body" | grep -q 'matchLabels'; then
    missing="$missing no-matchLabels(each-InferencePool-would-select-both-models)"
fi
if [ -n "$missing" ]; then
    fail "two-model-shapes-pd is not P/D on every stack; missing or wrong:$missing"
elif ! grep -qE '^  - name: "llama-31-8b"' "$SPEC_DIR/two-model-shapes-pd.yaml" || \
     ! grep -qE '^  - name: "qwen3-8b"' "$SPEC_DIR/two-model-shapes-pd.yaml"; then
    fail "two-model-shapes-pd's stack names are not the driver's STACK_A/STACK_B defaults"
else
    ok "two-model-shapes-pd enables prefill with the nixlv2 connector and the P/D EPP config for every stack"
fi

case_begin
# The stack names are the HTTPRoute path prefixes AND the driver's defaults --
# one fact in two files. Drift makes every request 404 for a whole run.
if ! grep -q 'STACK_A="${STACK_A:-llama-31-8b}"' "$SCRIPT"; then
    fail "the driver's STACK_A default changed; it must match a stack name in the scenario"
elif ! grep -qE '^  - name: "llama-31-8b"' "$SPEC_DIR/two-model-warm-pool.yaml"; then
    fail "the scenario has no stack named llama-31-8b, which the driver builds model A's endpoint from"
elif ! grep -qE '^  - name: "qwen3-8b"' "$SPEC_DIR/two-model-warm-pool.yaml"; then
    fail "the scenario has no stack named qwen3-8b, which the driver builds model B's endpoint from"
elif ! grep -q 'STACK_B="${STACK_B:-qwen3-8b}"' "$SCRIPT"; then
    fail "the driver's STACK_B default changed; it must match a stack name in the scenario"
else
    ok "both stack names are the driver's defaults and the scenario's, which is what the HTTPRoute keys on"
fi

# The stack -> Deployment mapping must come from the ROUTE, never from a name.
# The harness IGNORES model.shortName and generates a namespace-salted hash --
# measured on a multi-node H200 cluster: unsloth/Meta-Llama-3.1-8B-Instruct became
# `unsloth--244120d9-instruct`, which shares no substring with the stack name
# `llama-31-8b`. Matching names finds nothing, silently, and verify then reports
# a stack that is plainly serving as absent.
case_begin
DEPLOY_EXISTS=1 ROUTES=1 run_verb verify
if printf '%s' "$OUT" | grep -q 'unsloth--244120d9-instruct-decode'; then
    ok "a stack resolves to its Deployment through the HTTPRoute, not through its name"
elif printf '%s' "$OUT" | grep -q 'no decode Deployment'; then
    fail "the stack did not resolve to a Deployment. It has to be looked up through the route's backendRef, because the harness's generated name shares no substring with the stack name."
else
    fail "verify did not name the Deployment it resolved, so this case cannot tell how it was found: $OUT"
fi

case_begin
ROUTES=0 run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with NO HTTPRoute: there is no path for either model, and every request in a 34-minute run would 404"
else
    ok "a namespace with no HTTPRoute is refused"
fi

# THE ONE THAT COST AN ENTIRE A/B. `featureGates: [flowControl]` was present and
# correct in both EPP ConfigMaps, and the metric it gates was never emitted: the
# EPP reads that config ONCE at startup, the Helm update did not change the pod
# template, and process_start_time_seconds showed both EPPs still running the
# config they loaded 35 minutes before the gate existed. WVA's scheduler-queue
# query returned no series for either model through two 34-minute arms, and
# nothing said so. A config is a statement of intent; the series is the fact.
case_begin
FC_MODELS="" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed while the EPP flow-control queue had NO series: WVA's scheduler-queue signal would be absent for the whole run and the autoscaler would scale on a metric that does not exist"
elif ! printf '%s' "$OUT" | grep -q 'rollout restart'; then
    fail "verify refused without naming the fix (restart the EPPs so they re-read the config): $OUT"
else
    ok "a flow-control queue with no series is refused, naming the EPP restart"
fi

case_begin
FC_MODELS="Qwen/Qwen3-8B" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with a flow-control series for only ONE of the two models"
else
    ok "flow control live for one model but not the other is refused"
fi

# NOT KNOWING IS NOT PASSING. Both of these used to `return 0`, which is the
# same outcome the 34-minute arms got: the check ran, looked at nothing, and
# said nothing was wrong.
case_begin
PROM_URL="" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed without being able to read PROMETHEUS_BASE_URL; it checked nothing and reported success, which is how two full arms ran on a metric that was never emitted"
elif ! printf '%s' "$OUT" | grep -q 'WVA_NS'; then
    fail "verify refused without naming the knob that fixes it (WVA_NS, when the controller is elsewhere): $OUT"
else
    ok "an unreadable Prometheus URL is refused, not skipped"
fi

case_begin
FCHECK_OUT="QUERYFAIL timeout" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed when the Prometheus query never answered; an unanswered query says nothing about whether the signal exists"
elif ! printf '%s' "$OUT" | grep -q 'SKIP_FLOW_CONTROL_CHECK'; then
    fail "verify refused without naming the deliberate way past: $OUT"
else
    ok "a Prometheus query that did not answer is refused, not skipped"
fi

# ---------------------------------------------------------------------------
# The load Job, rendered. Load comes from inference-perf -- the llm-d harness's
# own generator -- so what the Pod is handed decides whether a run happens at
# all, and every one of these mistakes produces a plausible-looking directory
# rather than an error.
# ---------------------------------------------------------------------------
# Sourced rather than invoked: these are about what the render FUNCTIONS
# produce, and reaching them through a verb would need a cluster.
( . "$SCRIPT" ) >/dev/null 2>&1 || true
# shellcheck disable=SC1090
. "$SCRIPT" >/dev/null 2>&1
NS=ns-under-test
# The stub has to be on PATH here too: these functions call kubectl, and against
# the REAL one they fail and take a fallback -- which is a pass for the wrong
# reason on any case about what the normal path produces.
PATH="$STUB:$PATH"

case_begin
url="$(base_url_for_stack gw-svc:80 llama-31-8b)"
if printf '%s' "$url" | grep -q '/v1'; then
    fail "base_url_for_stack returned '$url'. inference-perf appends the route itself, so every request would go to /v1/completions/v1/completions and the gateway would 404 the whole run"
elif ! printf '%s' "$url" | grep -q '/llama-31-8b$'; then
    fail "base_url_for_stack returned '$url', which does not end at the stack's path prefix; llm-d routes a multi-model stack by path and the requests would reach the wrong stack"
else
    ok "the harness base_url is the gateway plus the stack prefix, with no route"
fi

# The DATA PATH must not resolve anything. Measured twice on a multi-node H200 cluster at only
# ~10 rps: 5.7% of requests died of ClientConnectorDNSError, spread across the
# whole run rather than bunched at startup -- aiohttp resolves per connection,
# and ndots:5 costs four lookups for each. That is the driver's own loss, three
# times the report's threshold, and it voids the arm.
case_begin
if ! printf '%s' "$url" | grep -qE '^http://[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:'; then
    fail "base_url is '$url', a name the load Pod would resolve on every connection. The gateway's ClusterIP is knowable before the run, and resolving nothing is the point"
else
    ok "the load addresses the gateway by ClusterIP, so the data path resolves nothing"
fi

case_begin
fallback="$(GW_IP="" base_url_for_stack gw-svc:80 llama-31-8b)"
if printf '%s' "$fallback" | grep -qE '^http://[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:'; then
    fail "with no ClusterIP readable the fallback still produced an address: $fallback"
elif ! printf '%s' "$fallback" | grep -q 'svc\.cluster\.local\.:'; then
    fail "the DNS fallback '$fallback' is not an ABSOLUTE name. Without the trailing dot, ndots:5 makes the resolver try every search domain first -- four lookups per connection, which is the failure this is falling back from"
else
    ok "an unreadable ClusterIP falls back to an absolute name, not a searched one"
fi

case_begin
JOB_YAML="$(MODEL_A=model-a MODEL_B=model-b LOAD_IMAGE=ghcr.io/llm-d/llm-d-benchmark:vX \
    render_load_job wva-two-model-load-pool pool 1770000000 2>&1)"
n_containers="$(printf '%s\n' "$JOB_YAML" | grep -c '^        - name: load-')"
# The START_AT each container is handed; the Pod's run label carries the same
# instant and is not a container's barrier.
n_starts="$(printf '%s\n' "$JOB_YAML" | grep -c 'value: "1770000000"')"
if [ "$n_containers" -ne 2 ]; then
    fail "the load Job rendered $n_containers load container(s), not 2. Only one model would be driven, and the other's rows would be an empty half of an anti-phase run"
elif [ "$n_starts" -ne 2 ]; then
    fail "the two containers do not share one start barrier ($n_starts of 2 carry it); their ladders would be offset by however far apart the containers happened to start, which is the anti-phase this scenario measures"
elif ! printf '%s\n' "$JOB_YAML" | grep -q '/profiles/run.sh'; then
    fail "the load containers do not run the harness wrapper, so nothing waits on the barrier or holds the Pod open for collection"
else
    ok "one Pod, two containers, one start barrier"
fi

# Each arm otherwise refetches both tokenizers across the public internet, and a
# blip there costs the arm: measured, a CAS Client Error from the Hugging Face
# CDN took model A down while model B fetched fine, and the run was refused
# after five minutes of preload grace with both models' accelerators held.
case_begin
if ! printf '%s\n' "$JOB_YAML" | grep -A2 '^        - name: hf$' | grep -q 'persistentVolumeClaim'; then
    fail "the tokenizer cache is not on the shared claim, so every arm refetches both tokenizers from the internet and a CDN blip costs a whole arm: $(printf '%s\n' "$JOB_YAML" | grep -A2 '^        - name: hf$')"
elif ! printf '%s\n' "$JOB_YAML" | grep -q 'subPath: benchmark-tokenizer-cache'; then
    fail "the cache mount has no subPath, so it would write into the same tree the models read their weights from"
else
    ok "tokenizers cache on the shared claim, under their own subPath"
fi

# FREE ACCELERATORS ARE NOT PLACEABLE ACCELERATORS. Measured: preflight said
# "11 free, this run peaks at 6" and passed, and the run then sat with 4
# replicas Pending for thirty minutes -- a decode Pod asks for 16 CPU as well as
# its accelerator, and the nodes that HAD free accelerators had 7 and 15 free
# cores. The nopool arm ran its whole schedule on one replica per model while
# its Deployments asked for three, and 90 minutes of a shared cluster produced
# a comparison between two different fleets.
case_begin
NODE_CPU=8 NODE_GPUS=8 run_verb preflight
if printf '%s' "$OUT" | grep -q 'placeable: 0'; then
    : # nodes too small to place anything at all is the same finding
fi
if [ "$RC" -eq 0 ]; then
    fail "preflight passed on nodes with 8 cores each, where a 16-CPU decode Pod cannot be placed at all; the run would spend its bursts Pending and measure the scheduler"
elif ! printf '%s' "$OUT" | grep -q 'can actually take a Pod'; then
    fail "preflight refused without naming placeability as the reason: $OUT"
else
    ok "accelerators that no node can place a Pod on are refused, not counted as free"
fi

case_begin
NODE_CPU=127 NODE_GPUS=8 run_verb preflight
if printf '%s' "$OUT" | grep -q 'can actually take a Pod'; then
    fail "preflight complained about placeability on nodes with 127 cores and 8 accelerators, where everything fits: $OUT"
else
    ok "roomy nodes raise no placeability complaint"
fi

# THE DECODE REQUEST IS THE SCENARIO'S. Preflight read it with a regex anchored
# on the first `decode:` -- the shared block, which has no requests -- found
# nothing, and placed 16-CPU Pods for a scenario that asks for 8. On a busy
# cluster that turned six placeable slots into "only 5 can take a Pod" and a
# refusal. Measured on a shared H200 cluster, before any standup.
case_begin
NODE_CPU=127 NODE_GPUS=8 run_verb preflight
if ! printf '%s' "$OUT" | grep -q 'Pod(s) of 8 CPU / 64Gi'; then
    fail "preflight did not place the scenario's own 8-CPU / 64Gi decode Pod: $(printf '%s' "$OUT" | grep placeable)"
else
    ok "preflight places the decode Pod the scenario declares (8 CPU / 64Gi), not a 16-CPU default"
fi

# DECODE_CPU is the knob the refusal names, and preflight counts with it.
case_begin
DECODE_CPU=4 NODE_CPU=127 NODE_GPUS=8 run_verb preflight
if ! printf '%s' "$OUT" | grep -q 'Pod(s) of 4 CPU'; then
    fail "DECODE_CPU=4 did not reach preflight's placement count: $(printf '%s' "$OUT" | grep placeable)"
else
    ok "DECODE_CPU sets the decode request preflight places"
fi

case_begin
NODE_CPU=10 NODE_GPUS=8 run_verb preflight
if [ "$RC" -eq 0 ]; then
    fail "preflight passed with one 8-CPU decode Pod placeable on a 10-core node and a peak of 6"
elif ! printf '%s' "$OUT" | grep -q 'DECODE_CPU=<cores>'; then
    fail "the placement refusal does not name the knob that lowers the request: $(printf '%s' "$OUT" | grep -A3 'can actually take')"
else
    ok "the placement refusal names DECODE_CPU, the knob that fixes it"
fi

# A PENDING POD HOLDS NO ACCELERATOR. Preflight subtracted another team's two
# unschedulable 8-GPU pods from the free count and refused with "only 2 free",
# beside its own count of 18 placeable. Bound pods count; unbound ones are a
# warning.
case_begin
FOREIGN_PENDING=1 NODE_GPUS=8 run_verb preflight
if [ "$RC" -ne 0 ] || ! printf '%s' "$OUT" | grep -q '8 free'; then
    fail "two unbound Pending pods were counted as holding accelerators: $(printf '%s' "$OUT" | grep -E 'free|accelerators')"
elif ! printf '%s' "$OUT" | grep -q '2 Pending pod(s) wait for 16 accelerator(s)'; then
    fail "unbound Pending pods were not named as a risk: $OUT"
else
    ok "unbound Pending pods hold no accelerators: counted out, named as a risk, preflight passes"
fi
case_begin
BOUND_GPUS=4 NODE_GPUS=8 run_verb preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q '4 held by pods on those nodes, 4 free'; then
    fail "a Running pod bound to the node was not counted: $(printf '%s' "$OUT" | grep -E 'free|accelerators')"
else
    ok "a pod bound to a node holds its accelerators, and a peak of 6 against 4 free is refused"
fi

# THE TEARDOWN UNDOES THE STANDUP'S SCENARIO. It used to hand the harness its
# default spec (guides/workload-autoscaling) for a two-model-shapes-pd
# namespace, which missed that run's node-local downloader and hung for 20
# minutes on the volume behind it.
case_begin
STANDUP_SPEC=guides/two-model-shapes-pd VERB_TIMEOUT=60 run_verb teardown
if ! grep -q 'MAKE\[--no-print-directory benchmark-teardown\] SPEC=guides/two-model-shapes-pd' "$CALLS"; then
    fail "teardown did not hand the harness the scenario the standup recorded: $(grep 'MAKE' "$CALLS" || echo 'no make call')"
else
    ok "teardown uses the scenario the standup recorded"
fi
case_begin
STANDUP_SPEC=guides/two-model-shapes-pd BENCH_SPEC=guides/two-model-shapes VERB_TIMEOUT=60 run_verb teardown
c1="$RC"; m1="$(grep -c 'MAKE\[' "$CALLS")"
VERB_TIMEOUT=60 run_verb teardown
if [ "$c1" -eq 0 ] || [ "$m1" -ne 0 ]; then
    fail "teardown with a BENCH_SPEC that disagrees with the recorded scenario was not refused before the harness ran"
elif [ "$RC" -eq 0 ] || grep -q 'MAKE\[' "$CALLS" || ! printf '%s' "$OUT" | grep -q 'no record of the scenario'; then
    fail "teardown with no record and no BENCH_SPEC guessed a scenario instead of refusing: $OUT"
else
    ok "teardown refuses a BENCH_SPEC that disagrees with the record, and refuses to guess with neither"
fi
case_begin
VERB_TIMEOUT=60 run_verb standup
if ! grep -q 'create configmap wva-two-model-standup --from-literal=spec=guides/two-model-warm-pool' "$CALLS"; then
    fail "the standup did not record its scenario for the teardown: $(grep -E 'configmap|MAKE' "$CALLS" | head -3)"
elif [ "$(grep -n 'wva-two-model-standup' "$CALLS" | head -1 | cut -d: -f1)" -gt "$(grep -n 'MAKE\[' "$CALLS" | head -1 | cut -d: -f1)" ]; then
    fail "the standup recorded its scenario only after the harness ran; a standup that fails half-way leaves no record"
else
    ok "the standup records its scenario before the harness runs"
fi

# A SIMULATOR TEARDOWN RUN WITHOUT THE STANDUP'S MODEL_COUNT still removes
# every stack present: it used to walk only its own two models, and left models
# C and D's ScaledObjects scaling Deployments being deleted.
case_begin
SIM4_STUB=1 STANDUP_SPEC=sim:guides/n-model-share VERB_TIMEOUT=60 run_verb teardown
sim_dels="$(grep -o 'delete scaledobject [a-z0-9.-]*-so' "$CALLS" | sed 's/delete scaledobject //' | sort | paste -sd, -)"
if [ "$sim_dels" != "llama-31-8b-so,qwen3-30b-a3b-so,qwen3-32b-so,qwen3-8b-so" ]; then
    fail "a simulator teardown with MODEL_COUNT unset deleted ScaledObjects '$sim_dels', not all four stacks'"
else
    ok "a simulator teardown removes every stack present, whatever MODEL_COUNT it is run with"
fi

# THE STACKS THE STANDUP KEEPS: the run's own whenever the scenario has more.
# Two models pointed at the four-stack n-model-share stood up all four.
case_begin
VERB_TIMEOUT=60 run_verb standup
std_two="$(grep -o 'benchmark-standup\] SPEC=[^ ]* STACKS=.*' "$CALLS" | head -1)"
std_two_out="$(grep -E 'MAKE|configmap' "$CALLS" | head -5)"
BENCH_SPEC=guides/n-model-share EPP_COUNT=0 VERB_TIMEOUT=60 run_verb standup
std_n2="$(grep -o 'benchmark-standup\] SPEC=[^ ]* STACKS=.*' "$CALLS" | head -1)"
MODEL_COUNT=4 EPP_COUNT=0 VERB_TIMEOUT=60 run_verb standup
std_n4="$(grep -o 'benchmark-standup\] SPEC=[^ ]* STACKS=.*' "$CALLS" | head -1)"
MODEL_COUNT=3 EPP_COUNT=0 VERB_TIMEOUT=60 run_verb standup
std_n3="$(grep -o 'benchmark-standup\] SPEC=[^ ]* STACKS=.*' "$CALLS" | head -1)"
if [ "$std_two" != "benchmark-standup] SPEC=guides/two-model-warm-pool STACKS=" ]; then
    fail "two models on their own two-stack scenario were handed a stack list: '$std_two' ($(printf '%s' "$std_two_out" | tail -4))"
elif [ "$std_n2" != "benchmark-standup] SPEC=guides/n-model-share STACKS=llama-31-8b,qwen3-8b" ]; then
    fail "two models on the four-stack n-model-share would stand up more than their two: '$std_n2'"
elif [ "$std_n3" != "benchmark-standup] SPEC=guides/n-model-share STACKS=llama-31-8b,qwen3-8b,qwen3-30b-a3b" ]; then
    fail "three models on n-model-share are not handed their three stacks: '$std_n3' ($(printf '%s' "$OUT" | tail -4))"
elif [ "$std_n4" != "benchmark-standup] SPEC=guides/n-model-share STACKS=" ]; then
    fail "four models on the four-stack scenario were handed a list to drop from: '$std_n4'"
else
    ok "the standup keeps only the run's stacks when the scenario has more: 2 and 3 of 4; all of a scenario that is the run's"
fi

# A QUOTA BELOW THE CEILINGS IS THE PEAK. The share runbook stands up under a
# 4-GPU quota with two models of up to 3; preflight asked for 6 placeable and
# refused a cluster with 5, though the run can never hold more than 4.
case_begin
NODE_CPU=40 NODE_GPUS=8 run_verb preflight
if [ "$RC" -eq 0 ]; then
    fail "the control does not refuse: 40 cores place 5 decode Pods of 8 CPU against a peak of 6"
else
    ok "without a quota, 5 placeable against a peak of 6 is refused (the control)"
fi
case_begin
WVA_QUOTAS='H200=4' NODE_CPU=40 NODE_GPUS=8 run_verb preflight
if [ "$RC" -ne 0 ] || ! printf '%s' "$OUT" | grep -q 'holds the models to 4 GPU(s)'; then
    fail "WVA_QUOTAS='H200=4' did not cap the peak at the quota: $(printf '%s' "$OUT" | grep -E 'peaks at|quota|can actually')"
else
    ok "a quota below the ceilings is the peak preflight places for"
fi
case_begin
WVA_QUOTAS='H200=4 A100=8' NODE_CPU=40 NODE_GPUS=8 run_verb preflight
c1="$RC"
ACCELERATOR=H200 WVA_QUOTAS='H200=4,A100=8' NODE_CPU=40 NODE_GPUS=8 run_verb preflight
if [ "$c1" -eq 0 ] || [ "$RC" -ne 0 ]; then
    fail "with two quota entries, preflight must not guess (rc $c1) and must take ACCELERATOR's entry (rc $RC)"
else
    ok "with two quota entries the cap is ACCELERATOR's, and without ACCELERATOR there is none"
fi

# WHO OWNS ISTIO, without editing a tracked file. Preflight used to tell the
# reader to add skipInstall to the scenario under hack/; GATEWAY_REUSE=auto
# now does that on the copy the standup renders.
case_begin
GWCLASS=1 NODE_CPU=127 run_verb preflight
if printf '%s' "$OUT" | grep -q 'already provides Istio'; then
    fail "preflight still refuses a cluster that runs Istio under GATEWAY_REUSE=auto: $(printf '%s' "$OUT" | grep -A4 'already provides')"
elif ! printf '%s' "$OUT" | grep -q 'the standup reuses it'; then
    fail "preflight did not say the standup will reuse the cluster's Istio: $OUT"
else
    ok "a cluster that runs Istio passes preflight: the standup reuses it, no tracked file edited"
fi

case_begin
GWCLASS=1 GATEWAY_REUSE=false NODE_CPU=127 run_verb preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'GATEWAY_REUSE=auto (the default) or true'; then
    fail "GATEWAY_REUSE=false on a cluster that runs Istio was not refused with the knob named: $OUT"
else
    ok "GATEWAY_REUSE=false on a cluster that runs Istio is refused, naming GATEWAY_REUSE"
fi

case_begin
GWCLASS=0 GATEWAY_REUSE=true NODE_CPU=127 run_verb preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'no istio.io/gateway-controller GatewayClass exists'; then
    fail "GATEWAY_REUSE=true on a cluster with no Istio passed preflight; the Gateway would never be Programmed: $OUT"
else
    ok "GATEWAY_REUSE=true with no Istio on the cluster is refused"
fi

# A REUSED ISTIO WHOSE GATEWAY PROXY THIS NAMESPACE CANNOT PULL. Measured: the
# standup downloaded four models and then aborted on a gateway Pod in
# ErrImagePull, because the control plane named a pull secret the namespace
# did not have. Preflight reads the injector and refuses that up front.
case_begin
GWCLASS=1 NODE_CPU=127 run_verb preflight
GW_BASE_RC="$RC"
GWCLASS=1 ISTIO_INJECTOR=login SECRET_PRESENT=0 NODE_CPU=127 run_verb preflight
if [ "$GW_BASE_RC" -ne 0 ]; then
    fail "the control fails: a cluster that runs Istio, injector unread, did not pass preflight (rc=$GW_BASE_RC), so a refusal below would prove nothing"
elif [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q "pull secret(s) 'mesh-pull-secret', which ns-under-test does not have" \
    || ! printf '%s' "$OUT" | grep -q 'registry.example.com/mesh/proxyv2@sha256:13da'; then
    fail "a reused Istio whose proxy needs a pull secret the namespace lacks passed preflight (rc=$RC): $(printf '%s' "$OUT" | grep -iE 'gateway|pull' | head -5)"
else
    ok "a reused Istio whose gateway proxy needs a missing pull secret is refused, naming the image and the secret"
fi

case_begin
GWCLASS=1 ISTIO_INJECTOR=login SECRET_PRESENT=1 NODE_CPU=127 run_verb preflight
c1="$RC"; o1="$OUT"
GWCLASS=1 ISTIO_INJECTOR=login SECRET_PRESENT=0 GATEWAY_PULL_CHECK=0 NODE_CPU=127 run_verb preflight
if printf '%s' "$o1" | grep -q 'does not have' || ! printf '%s' "$o1" | grep -q 'pull secrets: mesh-pull-secret'; then
    fail "the pull secret present still read as missing (rc=$c1): $(printf '%s' "$o1" | grep -iE 'pull' | head -3)"
elif printf '%s' "$OUT" | grep -q 'does not have' || ! printf '%s' "$OUT" | grep -q 'GATEWAY_PULL_CHECK=0'; then
    fail "GATEWAY_PULL_CHECK=0 did not skip the check: $(printf '%s' "$OUT" | grep -iE 'pull' | head -3)"
elif [ "$c1" -ne 0 ] || [ "$RC" -ne 0 ]; then
    fail "with the secret present (rc=$c1) or the check off (rc=$RC) preflight still refused: $(printf '%s' "$OUT" | grep WARN | head -3)"
else
    ok "the pull secret present passes, and GATEWAY_PULL_CHECK=0 skips the check"
fi

case_begin
GWCLASS=1 NODE_CPU=127 run_verb preflight
if [ "$RC" -ne 0 ] || ! printf '%s' "$OUT" | grep -q 'its pull secrets are unchecked'; then
    fail "an unreadable injector was not reported as unchecked, or refused (rc=$RC): $(printf '%s' "$OUT" | grep -iE 'pull|injector' | head -3)"
else
    ok "an injector it cannot read is reported as unchecked, not refused"
fi

case_begin
missing=""
for v in BENCHMARK_GATEWAY_SKIP_INSTALL BENCHMARK_GATEWAY_PROVIDER_NAMESPACE BENCHMARK_DECODE_CPU BENCHMARK_DECODE_MEMORY; do
    sed -n '/^verb_standup() {/,/^}/p' "$SCRIPT" | grep -q "$v=" || missing="$missing $v"
done
if [ -n "$missing" ]; then
    fail "the standup does not hand$missing to make benchmark-standup, so the setting stops at preflight"
else
    ok "the standup hands the gateway and decode settings to make benchmark-standup"
fi

# THE BURSTS MUST NOT OVERLAP. The two models cross a stage boundary at
# different moments -- a stage ends when its requests drain and the bursting
# model drains slower -- so the band is what keeps the drift from becoming time
# when both are bursting. A pool asked for two models at once can serve one,
# and that would be recorded as the pool failing at the thing being measured.
case_begin
if [ "${OVERLAP_SECONDS:-0}" -le 0 ]; then
    fail "OVERLAP_SECONDS defaults to '${OVERLAP_SECONDS:-}', so there is no band between bursts and the two models' peaks run into each other by however far they drift"
elif ! grep -q -- '--overlap "\$OVERLAP_SECONDS"' "$SCRIPT"; then
    fail "OVERLAP_SECONDS is set but never reaches the profile generator, so the rendered schedule has no band whatever the knob says"
elif ! grep -q -- '--overlap "\$OVERLAP_SECONDS"' "$SCRIPT" || ! grep -q 'overlap_seconds' "$ROOT/hack/benchmark/harness_results.py"; then
    fail "the band is never recorded in the meta, so the report cannot check that the measured drift stayed inside it"
else
    ok "a ${OVERLAP_SECONDS}s band separates the bursts, and it reaches both the profiles and the meta"
fi

case_begin
img="$(LOAD_IMAGE="" ROOT=/nonexistent load_image)"
if ! printf '%s' "$img" | grep -q 'llm-d-benchmark'; then
    fail "load_image resolved to '$img', which is not the harness image. inference-perf lives only in llm-d-benchmark; any other image starts, finds no such command, and the arm ends with an empty results directory"
else
    ok "the load image is the llm-d-benchmark harness ($img)"
fi

# ---------------------------------------------------------------------------
# The GPU sampler keeps writing. A share arm lost 2185s of samples with no line
# saying why: the machine running the sampler was suspended. A hung or failed
# API call silences it the same way.
# ---------------------------------------------------------------------------
# run_bounded <seconds> <cmd...> -- in the background, in its own process
# group, killed whole at the limit: a sampler that hangs (or spins on a clock
# that never arrives) fails this check instead of stalling it, and the stub
# processes it started go with it.
run_bounded() {
    local limit="$1"; shift
    set -m
    ( "$@" ) &
    local pid=$! waited=0
    set +m
    while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$limit" ]; do
        command sleep 1; waited=$((waited + 1))
    done
    kill -- "-$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
    return 0
}

# A kubectl per case, each in its own directory: KSTUB_MODE picks it.
KSTUB="$WORK/kstub"; mkdir -p "$KSTUB"
cat > "$KSTUB/kubectl" <<'KSTUBEOF'
#!/usr/bin/env bash
case "${KSTUB_MODE:-items}" in
  # Hangs unless the call carries a request timeout, then fails as a
  # timed-out call does.
  honours) case "$*" in *--request-timeout=*) exit 1 ;; esac; exec sleep 60 ;;
  # Hangs whatever the call says: an exec credential plugin that never returns.
  ignores) exec sleep 60 ;;
  # Succeeds with nothing on stdout.
  empty) exit 0 ;;
  *) echo '{"items":[]}' ;;
esac
KSTUBEOF
chmod +x "$KSTUB/kubectl"

case_begin
SAMPLES="$WORK/hang-gpus.jsonl"
hang_sample() { KSTUB_MODE=honours PATH="$KSTUB:$PATH" GPU_SAMPLE_SECONDS=1 sample_gpus "$SAMPLES" "$(( $(date +%s) + 3 ))" 2>/dev/null; }
run_bounded 8 hang_sample
if ! grep -q '"failed": true' "$SAMPLES" 2>/dev/null; then
    fail "against an API call that fails once its request timeout fires the sampler wrote $(wc -l < "$SAMPLES" 2>/dev/null || echo 0) line(s) and no failure mark"
else
    ok "a call that times out is marked as a failed poll"
fi

case_begin
SAMPLES="$WORK/outer-gpus.jsonl"
outer_sample() { KSTUB_MODE=ignores PATH="$KSTUB:$PATH" GPU_SAMPLE_SECONDS=1 GPU_SAMPLE_TIMEOUT_SECONDS=1 sample_gpus "$SAMPLES" "$(( $(date +%s) + 3 ))" 2>/dev/null; }
if ! command -v timeout >/dev/null 2>&1; then
    fail "this machine has no timeout(1), so the sampler's outer bound cannot be checked here"
else
    run_bounded 20 outer_sample
    if ! grep -q '"failed": true' "$SAMPLES" 2>/dev/null; then
        fail "against a call that ignores its request timeout the sampler wrote $(wc -l < "$SAMPLES" 2>/dev/null || echo 0) line(s) and no failure mark: the outer timeout did not bound it"
    else
        ok "a call that ignores its request timeout is cut off by the outer one and marked"
    fi
fi

case_begin
SAMPLES="$WORK/empty-gpus.jsonl"
empty_sample() { KSTUB_MODE=empty PATH="$KSTUB:$PATH" GPU_SAMPLE_SECONDS=1 sample_gpus "$SAMPLES" "$(( $(date +%s) + 2 ))" 2>/dev/null; }
run_bounded 8 empty_sample
if ! grep -q '"failed": true' "$SAMPLES" 2>/dev/null || grep -q '"pods"' "$SAMPLES" 2>/dev/null; then
    fail "a call that succeeded with an empty answer was not marked as a failed poll: $(head -c 300 "$SAMPLES" 2>/dev/null)"
else
    ok "an empty answer is a failed poll, not a sample"
fi

# stall_run <jump> <until offset>: the sampler on a clock moved by hand. Every
# sleep is 5s except the second, which is <jump> seconds; the window ends
# <until offset> seconds after the start. GPU_SAMPLE_SECONDS=5 and
# GPU_SAMPLE_TIMEOUT_SECONDS=20 put the stall line at 5 + 2 x 25 + 10 = 65s.
CLOCK="$WORK/clock"
stall_run() {
    local jump="$1" until_off="$2"
    SAMPLES="$WORK/stall-$jump-gpus.jsonl"
    stall_body() {
        echo 1000 > "$CLOCK"; echo 0 > "$CLOCK.n"
        sampler_now() { cat "$CLOCK"; }
        sleep() {
            local n; n=$(( $(cat "$CLOCK.n") + 1 )); echo "$n" > "$CLOCK.n"
            if [ "$n" -eq 2 ]; then echo $(( $(cat "$CLOCK") + jump )) > "$CLOCK"
            else echo $(( $(cat "$CLOCK") + 5 )) > "$CLOCK"; fi
        }
        KSTUB_MODE=items PATH="$KSTUB:$PATH" GPU_SAMPLE_SECONDS=5 GPU_SAMPLE_TIMEOUT_SECONDS=20 \
            sample_gpus "$SAMPLES" "$(( 1000 + until_off ))" 2>"$WORK/stall.err"
    }
    run_bounded 15 stall_body
}

case_begin
stall_run 3000 3020
marks="$(grep -c stalled_since "$SAMPLES" 2>/dev/null)"
if ! grep -q '"stalled_since": 1005' "$SAMPLES" 2>/dev/null; then
    fail "a 3000s stall of the sampler left no stall mark: $(head -c 300 "$SAMPLES" 2>/dev/null)"
elif [ "$marks" -ne 1 ]; then
    fail "the 5s sleeps were also marked as stalls: $marks marks"
elif ! grep -q 'did not run for 3000s' "$WORK/stall.err"; then
    fail "the stall was marked but not said: $(cat "$WORK/stall.err")"
elif ! sed -n '/stalled_since/,$p' "$SAMPLES" | grep -q '"pods"'; then
    fail "the sampler wrote no sample after the stall: it did not resume"
else
    ok "a stall is written down and said, sampling resumes after it, and ordinary sleeps are not stalls"
fi

case_begin
stall_run 65 200
legal="$(grep -c stalled_since "$SAMPLES" 2>/dev/null)"
stall_run 66 200
if [ "$legal" -ne 0 ]; then
    fail "a pass 65s after the last -- a sleep and two calls at their outer timeout, the most a live loop takes -- was marked a stall"
elif ! grep -q '"stalled_since": 1005' "$SAMPLES" 2>/dev/null; then
    fail "a pass 66s after the last, one past the stall line, was not marked"
else
    ok "the stall line sits just past the slowest legal pass"
fi

case_begin
stall_run 3000 1500
if ! grep -q '"stalled_since": 1005' "$SAMPLES" 2>/dev/null; then
    fail "a stall that ran past the end of the window left no mark: $(cat "$SAMPLES" 2>/dev/null)"
else
    ok "a stall that runs past the end of the window is marked too"
fi

case_begin
STALLED="$WORK/stalled.jsonl"
printf '%s\n' '{"ts": 1000, "pods": []}' '{"ts": 1400, "stalled_since": 1000}' '{"ts": 1405, "pods": []}' > "$STALLED"
got="$(sampler_stalled "$STALLED")"; rc=$?
if [ "$rc" -ne 0 ] || [ "$got" != "400" ]; then
    fail "sampler_stalled read a 400s stall as rc $rc, '$got'"
elif sampler_stalled "$WORK/hang-gpus.jsonl" >/dev/null; then
    fail "sampler_stalled reported a stall in a file with none"
elif ! grep -q 'stalled="$(sampler_stalled "$out_dir/gpus.jsonl")"' "$SCRIPT" ||
     ! grep -q 'die "arm $arm stopped: the GPU sampler did not run' "$SCRIPT"; then
    fail "the run verb no longer stops an arm whose sampler stalled; its cost cannot be priced and the GPUs it holds on a shared cluster are held for nothing"
else
    ok "a stall is found in the samples, and the run verb stops the arm on it"
fi

case_begin
GPU_SAMPLE_TIMEOUT_SECONDS=1m run_verb run today
bad_unit="$RC:$OUT"
GPU_SAMPLE_SECONDS=0.5 run_verb run today
if [ "${bad_unit%%:*}" -eq 0 ] || ! printf '%s' "$bad_unit" | grep -q 'GPU_SAMPLE_TIMEOUT_SECONDS must be a whole number of seconds'; then
    fail "GPU_SAMPLE_TIMEOUT_SECONDS=1m was not refused: it would reach kubectl and switch the stall check off. ${bad_unit:0:300}"
elif [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'GPU_SAMPLE_SECONDS must be a whole number'; then
    fail "GPU_SAMPLE_SECONDS=0.5 was not refused: ${OUT:0:300}"
elif grep -q 'CALL\[.*apply' "$CALLS" 2>/dev/null; then
    fail "a sampler setting was refused only after something was created: $(cat "$CALLS")"
else
    ok "sampler settings in anything but whole seconds are refused before an arm starts"
fi

case_begin
INH="$WORK/inhibit"; mkdir -p "$INH"
cat > "$INH/systemd-inhibit" <<'INHEOF'
#!/usr/bin/env bash
echo "$*" > "${INH_LOG:?}"
exec sleep 30
INHEOF
chmod +x "$INH/systemd-inhibit"
INH_LOG="$WORK/inhibit.log"; export INH_LOG
RUN_INHIBIT=""
PATH="$INH:$PATH" hold_awake 2>/dev/null
held="$RUN_INHIBIT"
alive=no; [ -n "$held" ] && kill -0 "$held" 2>/dev/null && alive=yes
run_cleanup
# Killed is gone: a zombie this shell has not reaped yet holds no lock.
command sleep 0.3
gone=yes
if [ -n "$held" ] && kill -0 "$held" 2>/dev/null; then
    case "$(ps -o stat= -p "$held" 2>/dev/null)" in Z*) ;; *) gone=no ;; esac
fi
if [ "$alive" != yes ] || ! grep -q -- '--what=sleep' "$INH_LOG" 2>/dev/null; then
    fail "hold_awake took no sleep lock through systemd-inhibit (pid '$held', args '$(cat "$INH_LOG" 2>/dev/null)')"
elif [ "$gone" != yes ]; then
    kill "$held" 2>/dev/null
    fail "run_cleanup left the sleep lock held"
else
    ok "an arm holds a sleep lock where the machine offers one, and gives it back on cleanup"
fi

case_begin
NOINH="$WORK/noinhibit"; mkdir -p "$NOINH"
RUN_INHIBIT=""
said="$(PATH="$NOINH" hold_awake 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] || [ -n "$RUN_INHIBIT" ] || ! printf '%s' "$said" | grep -q 'keep it awake'; then
    fail "with no inhibitor hold_awake returned $rc, held '$RUN_INHIBIT', said '$said'"
else
    ok "with no inhibitor the arm still runs, and says to keep the machine awake"
fi

# ---------------------------------------------------------------------------
# Mixed replica sizes: a 2-GPU model beside a 1-GPU model under one quota.
# ---------------------------------------------------------------------------
# One GPUS_PER_REPLICA for both models priced a 3 x 2-GPU MoE as 3 GPUs: the
# peak, the placement and every GPU-second would describe a run that did not
# happen. And a share arm without node data measures the small model never
# funding the large one -- the documented limit, not the experiment.
MIXED="BENCH_SPEC=guides/two-model-mixed-sizes STACK_A=qwen3-30b-a3b MODEL_A=Qwen/Qwen3-30B-A3B-FP8 MAX_REPLICAS_A=3 MAX_REPLICAS_B=4"
# mixed_verb [VAR=value...] <verb...>: run_verb with the mixed-size settings
# and the given ones exported, in a subshell, so none of them outlives the case.
mixed_verb() {
    (
        for kv in $MIXED; do export "$kv"; done
        while [ $# -gt 0 ] && [[ "$1" =~ ^[A-Z_][A-Z0-9_]*= ]]; do export "$1"; shift; done
        run_verb "$@"
        printf '%s' "$OUT" > "$WORK/mixed.out"
        echo "$RC" > "$WORK/mixed.rc"
    )
    OUT="$(cat "$WORK/mixed.out")"; RC="$(cat "$WORK/mixed.rc")"; CALLS="$WORK/calls"
}

case_begin
if [ ! -f "$SPEC_DIR/two-model-mixed-sizes.yaml" ] || [ ! -f "$SPEC_DIR/two-model-mixed-sizes.yaml.j2" ]; then
    fail "two-model-mixed-sizes.yaml or its specification is missing"
elif ! grep -q 'scenarios/guides/two-model-mixed-sizes.yaml' "$SPEC_DIR/two-model-mixed-sizes.yaml.j2"; then
    fail "the mixed-sizes specification points at a scenario other than its own"
else
    drift="$(diff <(grep -vE '^[[:space:]]*#' "$SPEC_DIR/two-model-shapes.yaml" | tr -d '\r') \
                  <(grep -vE '^[[:space:]]*#' "$SPEC_DIR/two-model-mixed-sizes.yaml" | tr -d '\r') | grep -E '^[<>]' \
             | sed 's/^\([<>]\) */\1 /' | sort | tr '\n' '|')"
    expected="$(printf '%s\n' \
        '< workDir: "~/data/two-model-shapes"' '< - name: "llama-31-8b"' \
        '< name: unsloth/Meta-Llama-3.1-8B-Instruct' '< shortName: llama-31-8b' \
        '< path: models/unsloth/Meta-Llama-3.1-8B-Instruct' '< huggingfaceId: unsloth/Meta-Llama-3.1-8B-Instruct' \
        '< size: 32Gi' '< memory: 64Gi' '< memory: 64Gi' '< cpu: "8"' '< cpu: "8"' \
        '< nvidia.com/gpu: "1"' '< nvidia.com/gpu: "1"' '< maxReplicas: 3' \
        '> workDir: "~/data/two-model-mixed-sizes"' '> - name: "qwen3-30b-a3b"' \
        '> name: Qwen/Qwen3-30B-A3B-FP8' '> shortName: qwen3-30b-a3b' '> path: models/Qwen/Qwen3-30B-A3B-FP8' \
        '> huggingfaceId: Qwen/Qwen3-30B-A3B-FP8' '> size: 40Gi' '> parallelism:' '> tensor: 2' '> data: 1' \
        '> dataLocal: 1' '> workers: 1' '> memory: 128Gi' '> memory: 128Gi' '> cpu: "16"' '> cpu: "16"' \
        '> nvidia.com/gpu: "2"' '> nvidia.com/gpu: "2"' '> maxReplicas: 4' | sort | tr '\n' '|')"
    if [ "$drift" != "$expected" ]; then
        fail "two-model-mixed-sizes.yaml differs from two-model-shapes.yaml in more than stack A's model and size, B's ceiling and workDir: $drift"
    else
        ok "two-model-mixed-sizes is two-model-shapes with stack A a 2-GPU TP-2 MoE, and its specification names it"
    fi
fi

case_begin
mixed_verb NODE_GPUS=16 preflight
if ! printf '%s' "$OUT" | grep -q 'this run peaks at 10'; then
    fail "preflight did not price the mixed run at 3 x 2 + 4 x 1 = 10 GPUs: $(printf '%s' "$OUT" | grep -E 'peaks|model A')"
elif ! printf '%s' "$OUT" | grep -q 'model A (qwen3-30b-a3b, Qwen/Qwen3-30B-A3B-FP8): 2 GPU(s) per replica (tensor 2), 1..3'; then
    fail "preflight did not read model A's size and tensor parallelism from the scenario: $(printf '%s' "$OUT" | grep 'model A')"
else
    ok "preflight prices each model at its own size and ceiling, read from the scenario"
fi

case_begin
mixed_verb GPUS_PER_REPLICA_A=1 NODE_GPUS=16 preflight
if [ "$RC" -eq 0 ]; then
    fail "preflight passed GPUS_PER_REPLICA_A=1 for a stack that deploys 2 GPUs per replica"
elif ! printf '%s' "$OUT" | grep -q 'priced on a size it does not deploy'; then
    fail "preflight refused without naming the size mismatch: $(printf '%s' "$OUT" | grep -iE 'warn|error' | head -3)"
else
    ok "a per-model size that disagrees with the scenario is refused"
fi

case_begin
mixed_verb STACK_A=llama-31-8b NODE_GPUS=16 preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'is not a stack of guides/two-model-mixed-sizes'; then
    fail "preflight accepted a STACK_A the scenario does not have (rc=$RC): $(printf '%s' "$OUT" | grep -i stack | head -2)"
else
    ok "a stack the scenario does not have is refused, with the stacks it does have"
fi

case_begin
mixed_verb WVA_LIMITER=quota WVA_QUOTAS='H200=7' standup
if [ "$RC" -eq 0 ]; then
    fail "standup of a mixed-size run went ahead with WVA_LIMITER=quota alone; its share arms could never fund the 2-GPU model"
elif ! printf '%s' "$OUT" | grep -q 'declares no gpu-inventory limiter'; then
    fail "standup refused without naming the missing gpu-inventory limiter: $(printf '%s' "$OUT" | tail -2)"
elif grep -q '^MAKE\[' "$CALLS"; then
    fail "standup refused only after starting the harness: $(grep '^MAKE\[' "$CALLS")"
else
    ok "a mixed-size standup without gpu-inventory is refused before anything is stood up"
fi

QUOTA_ONLY='limiters:
- type: quota
  quotas:
    H200: 7'
BOTH_LIMITERS='limiters:
- type: gpu-inventory
- type: quota
  quotas:
    H200: 7'

case_begin
mixed_verb POLICY_TEXT="$QUOTA_ONLY" CAN_NODES=yes CAN_PODS=yes run today
if [ "$RC" -eq 0 ]; then
    fail "a mixed-size today arm ran under a policy with no gpu-inventory limiter"
elif ! printf '%s' "$OUT" | grep -q 'declares no gpu-inventory limiter' || ! printf '%s' "$OUT" | grep -q 'kubectl create clusterrolebinding wva-pod-lister-ns-under-test'; then
    fail "the refusal did not name the missing limiter and print the admin commands: $(printf '%s' "$OUT" | tail -5)"
elif grep -q 'patch configmap wva-scaling-policy-config\|apply -f' "$CALLS"; then
    fail "the arm wrote the policy or created the load before refusing"
else
    ok "a mixed-size share arm without gpu-inventory is refused before anything changes, with the admin commands"
fi

case_begin
mixed_verb POLICY_TEXT="$BOTH_LIMITERS" CAN_NODES=yes CAN_PODS=no run share
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'may not list pods in every namespace'; then
    fail "a share arm whose controller cannot list pods cluster-wide was not refused for it (rc=$RC): $(printf '%s' "$OUT" | grep -i 'pods' | head -2)"
elif grep -q 'apply -f' "$CALLS"; then
    fail "the arm created the load before refusing"
else
    ok "a controller that cannot list pods in every namespace stops the share arm"
fi

case_begin
mixed_verb POLICY_TEXT="$BOTH_LIMITERS" CAN_NODES=unknown CAN_PODS=unknown CTL_READY=1 \
    STATUS_JSON='{"data":{"podListDenied":"pods is forbidden"}}' run shadow
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'may not list pods in every namespace'; then
    fail "without impersonation, the controller's own podListDenied did not stop the arm (rc=$RC)"
else
    ok "without impersonation, the controller's own podListDenied stops the arm"
fi

case_begin
mixed_verb POLICY_TEXT="$BOTH_LIMITERS" CAN_NODES=unknown CAN_PODS=unknown CTL_READY=1 \
    STATUS_JSON='{"data":{"lastCycle":"ok"}}' run today
if ! printf '%s' "$OUT" | grep -q 'node data: gpu-inventory in the policy'; then
    fail "node data that is all there was not accepted: $(printf '%s' "$OUT" | grep -iE 'node data|may not|could not tell' | head -3)"
elif ! grep -q 'patch configmap wva-scaling-policy-config' "$CALLS"; then
    fail "with node data present the arm did not go on to write its policy"
else
    ok "with gpu-inventory, a Ready controller and no podListDenied, the arm goes on"
fi

case_begin
POLICY_TEXT="$QUOTA_ONLY" run_verb run today
if printf '%s' "$OUT" | grep -q 'needs node data'; then
    fail "a same-size run asked for node data it does not need"
elif ! grep -q 'patch configmap wva-scaling-policy-config' "$CALLS"; then
    fail "a same-size today arm under a quota alone did not reach its policy write: $(printf '%s' "$OUT" | tail -3)"
else
    ok "a run whose models are one size needs no node data"
fi

case_begin
mixed_verb NODE_GPUS=8 BOUND_GPUS=4 BENCH_NODE=n1 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'BENCH_NODE=n1 has 4 free accelerator(s) and the run peaks at 7'; then
    fail "preflight did not refuse a pinned node with 4 free for a 7-GPU run (rc=$RC): $(printf '%s' "$OUT" | grep BENCH_NODE)"
else
    ok "a pinned node with fewer free accelerators than the run peaks at is refused"
fi

# The single-node path, unchanged: a node with the peak free passes on its own
# count, and nothing of the multi-node path runs.
case_begin
mixed_verb NODE_GPUS=8 BENCH_NODE=n1 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -ne 0 ] || ! printf '%s' "$OUT" | grep -q 'BENCH_NODE=n1 has 8 free accelerator(s); the run peaks at 7'; then
    fail "a pinned node with the peak free was not accepted on its own count (rc=$RC): $(printf '%s' "$OUT" | grep BENCH_NODE)"
elif printf '%s' "$OUT" | grep -q 'BENCH_NODES'; then
    fail "the single-node path ran multi-node checks: $(printf '%s' "$OUT" | grep BENCH_NODES)"
else
    ok "BENCH_NODE alone is checked as before, and only it"
fi

# SEVERAL nodes. Free 3 + 4 + 2 = 9 for a 7-GPU quota, each with room for a
# 2-GPU replica: no node has the peak, and the run still fits.
MULTI3='MULTI_NODES=n1=8,n2=8,n3=8'
case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=5,n2=4,n3=6 BENCH_NODES=n1,n2,n3 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -ne 0 ] || ! printf '%s' "$OUT" | grep -q 'BENCH_NODES have 9 free GPU(s) together; the run peaks at 7'; then
    fail "three nodes with 3 + 4 + 2 free were not accepted for a 7-GPU run (rc=$RC): $(printf '%s' "$OUT" | grep -E 'BENCH_NODES|n[123]:')"
elif printf '%s' "$OUT" | grep -q 'WARN.*BENCH_NODES'; then
    fail "a fitting node list still drew a BENCH_NODES warning: $(printf '%s' "$OUT" | grep 'WARN.*BENCH_NODES')"
elif ! printf '%s' "$OUT" | grep -q 'n2: 4 free GPU(s).*up to 2 2-GPU replica(s)'; then
    fail "preflight did not say what each node holds: $(printf '%s' "$OUT" | grep 'n2:')"
else
    ok "several nodes whose free GPUs add up to the peak pass, each with its own count"
fi

case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=5,n2=6,n3=7 BENCH_NODES=n1,n2 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'BENCH_NODES have 5 free GPU(s) together and the run peaks at 7'; then
    fail "two nodes with 3 + 2 free were not refused for a 7-GPU run (rc=$RC): $(printf '%s' "$OUT" | grep BENCH_NODES)"
else
    ok "a node list whose free GPUs add up to less than the peak is refused"
fi

case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=2,n2=7 BENCH_NODES=n1,n2 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'n2 has 1 free GPU(s).*cannot hold one 2-GPU replica'; then
    fail "a node with 1 free GPU was accepted for a run whose larger replica takes 2 (rc=$RC): $(printf '%s' "$OUT" | grep -E 'n2|BENCH_NODES')"
else
    ok "a listed node that cannot hold one 2-GPU replica is refused, even when the total is enough"
fi

case_begin
mixed_verb "$MULTI3" NODE_CPU=20 BOUND_ON=n1=5,n2=4,n3=6 BENCH_NODES=n1,n2,n3 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'CPU and memory for only 3 Pod(s), and the run can hold 5'; then
    fail "nodes with GPUs but CPU for one decode Pod each were not refused (rc=$RC): $(printf '%s' "$OUT" | grep -E 'BENCH_NODES|CPU')"
else
    ok "the listed nodes must have the CPU and memory for the Pods the peak can be"
fi

case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=4,n2=4 BENCH_NODES=n1,n9 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'BENCH_NODES: no node named n9'; then
    fail "a listed node that does not exist was not refused (rc=$RC): $(printf '%s' "$OUT" | grep BENCH_NODES)"
else
    ok "a listed node that does not exist is refused by name"
fi

case_begin
mixed_verb NODE_GPUS=8 BENCH_NODE=n1 BENCH_NODES=n1,n2 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'are both set: one node, or a list'; then
    fail "BENCH_NODE and BENCH_NODES together were not refused (rc=$RC)"
else
    ok "BENCH_NODE and BENCH_NODES together are refused"
fi

# TAINTS: never tolerated by default, only when named.
case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=4,n2=4 NODE_TAINTS='n2=dedicated=gpu:NoSchedule' BENCH_NODES=n1,n2 WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'n2 carries dedicated=gpu:NoSchedule, which BENCH_TOLERATE does not name'; then
    fail "a tainted listed node was not refused by default (rc=$RC): $(printf '%s' "$OUT" | grep -E 'n2|taint')"
else
    ok "a listed node with a taint nobody named is refused, not tolerated"
fi

case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=4,n2=4 NODE_TAINTS='n2=dedicated=gpu:NoSchedule' BENCH_NODES=n1,n2 \
    BENCH_TOLERATE='dedicated=gpu:NoSchedule' WVA_QUOTAS='H200=7' preflight
if [ "$RC" -ne 0 ] || printf '%s' "$OUT" | grep -q 'which BENCH_TOLERATE does not name' || ! printf '%s' "$OUT" | grep -q 'BENCH_NODES have 8 free GPU(s) together'; then
    fail "a taint named in BENCH_TOLERATE still refused the node (rc=$RC): $(printf '%s' "$OUT" | grep -E 'n2|BENCH_')"
else
    ok "a taint named in BENCH_TOLERATE is tolerated, on the listed nodes"
fi

case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=4,n2=4 NODE_TAINTS='n2=dedicated=gpu:NoSchedule' BENCH_NODES=n1,n2 \
    BENCH_TOLERATE='dedicated=other:NoSchedule' WVA_QUOTAS='H200=7' preflight
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'n2 carries dedicated=gpu:NoSchedule'; then
    fail "a toleration for another value of the key let the node through (rc=$RC)"
else
    ok "a toleration for another value of the taint does not cover it"
fi

case_begin
mixed_verb "$MULTI3" BOUND_ON=n1=4,n2=4 BENCH_NODES=n1,n2 BENCH_TOLERATE='*' WVA_QUOTAS='H200=7' preflight
out1="$OUT" rc1="$RC"
mixed_verb NODE_GPUS=8 BENCH_TOLERATE='dedicated' WVA_QUOTAS='H200=7' preflight
if [ "$rc1" -eq 0 ] || ! printf '%s' "$out1" | grep -q 'names no taint key'; then
    fail "BENCH_TOLERATE='*' (every taint) was not refused (rc=$rc1)"
elif [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'BENCH_TOLERATE is set without BENCH_NODES'; then
    fail "BENCH_TOLERATE without BENCH_NODES was not refused (rc=$RC)"
else
    ok "a toleration of every taint, and a toleration for no named nodes, are refused"
fi

case_begin
mixed_verb BENCH_NODES=n1,n2 BENCH_TOLERATE='*' WVA_LIMITER=gpu-inventory,quota WVA_QUOTAS='H200=7' standup
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'names no taint key'; then
    fail "standup took BENCH_TOLERATE='*' (rc=$RC): $(printf '%s' "$OUT" | tail -2)"
elif grep -q '^MAKE\[' "$CALLS"; then
    fail "standup refused the toleration only after starting the harness"
else
    ok "standup refuses a toleration of every taint before anything is stood up"
fi

# Sourced: what the functions render and patch, per model.
case_begin
got="$( (BENCH_SPEC=guides/two-model-mixed-sizes STACK_A=qwen3-30b-a3b MODEL_A=Qwen/Qwen3-30B-A3B-FP8 BENCH_NODE=n1 \
         SIM_GPU_PRODUCT=NVIDIA-H200; sim_manifest a) 2>&1)"
if [ "$(printf '%s\n' "$got" | grep -c 'nvidia.com/gpu: "2"')" -ne 2 ]; then
    fail "the simulator stack for the 2-GPU model does not request and limit 2 GPUs: $(printf '%s\n' "$got" | grep 'nvidia.com/gpu')"
elif ! printf '%s\n' "$got" | grep -q 'kubernetes.io/hostname: "n1"' || ! printf '%s\n' "$got" | grep -q 'nvidia.com/gpu.product: "NVIDIA-H200"'; then
    fail "the simulator stack is not pinned to BENCH_NODE and the product: $(printf '%s\n' "$got" | grep -A3 nodeSelector)"
elif ! printf '%s\n' "$got" | grep -q '"Qwen/Qwen3-30B-A3B-FP8"'; then
    fail "the simulator does not serve model A's name, so the loader would ask it for a model it does not have"
else
    ok "the simulator stack takes the model's own GPUs per replica and the pins"
fi

case_begin
got="$( (BENCH_SPEC=guides/two-model-mixed-sizes STACK_A=qwen3-30b-a3b MODEL_A=Qwen/Qwen3-30B-A3B-FP8 BENCH_NODE= BENCH_NODES=n1,n2 \
         BENCH_TOLERATE= SIM_GPU_PRODUCT=NVIDIA-H200; sim_manifest a) 2>&1)"
got_tol="$( (BENCH_SPEC=guides/two-model-mixed-sizes STACK_A=qwen3-30b-a3b MODEL_A=Qwen/Qwen3-30B-A3B-FP8 BENCH_NODE= BENCH_NODES=n1,n2 \
         BENCH_TOLERATE='dedicated=gpu:NoSchedule' SIM_GPU_PRODUCT=; sim_manifest a) 2>&1)"
if printf '%s\n' "$got" | grep -q 'kubernetes.io/hostname: '; then
    fail "BENCH_NODES pinned the simulator to one node: $(printf '%s\n' "$got" | grep hostname)"
elif ! printf '%s\n' "$got" | grep -q 'values: \["n1","n2"\]' || ! printf '%s\n' "$got" | grep -q 'operator: In'; then
    fail "the simulator stack may not land on any of BENCH_NODES: $(printf '%s\n' "$got" | grep -A8 affinity)"
elif printf '%s\n' "$got" | grep -q 'tolerations'; then
    fail "the simulator stack tolerates a taint nobody named: $(printf '%s\n' "$got" | grep tolerations)"
elif ! printf '%s\n' "$got_tol" | grep -q 'tolerations: \[{"key":"dedicated","operator":"Equal","value":"gpu","effect":"NoSchedule"}\]'; then
    fail "the simulator stack does not take the toleration BENCH_TOLERATE names: $(printf '%s\n' "$got_tol" | grep -A1 tolerations)"
elif command -v yq >/dev/null 2>&1 && \
     [ "$(printf '%s\n' "$got_tol" | yq -r 'select(.kind == "Deployment") | .spec.template.spec | [.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[1], .tolerations[0].key] | join(",")' 2>&1)" != "n2,dedicated" ]; then
    fail "the simulator Deployment does not parse to the affinity and toleration it prints: $(printf '%s\n' "$got_tol" | yq -r 'select(.kind == "Deployment") | .spec.template.spec' 2>&1 | head -20)"
else
    ok "BENCH_NODES places the simulator by affinity on any listed node, with a toleration only when named"
fi

# The share arms on several nodes: the controller's gpu-inventory must count
# exactly the listed nodes, or it funds replicas from GPUs the run never uses.
case_begin
mixed_verb POLICY_TEXT="$BOTH_LIMITERS" CAN_NODES=yes CAN_PODS=yes BENCH_NODES=n1,n2 run today
out1="$OUT" rc1="$RC" calls1="$(cat "$CALLS")"
mixed_verb POLICY_TEXT="$BOTH_LIMITERS" CAN_NODES=yes CAN_PODS=yes BENCH_NODES=n1,n2 \
    CTL_NODE_SELECTOR='kubernetes.io/hostname in (n1,n2)' run today
if [ "$rc1" -eq 0 ] || ! printf '%s' "$out1" | grep -q "reads WVA_NODE_SELECTOR='', not 'kubernetes.io/hostname in (n1,n2)'"; then
    fail "a share arm on BENCH_NODES ran with a controller counting every node (rc=$rc1): $(printf '%s' "$out1" | tail -2)"
elif printf '%s' "$calls1" | grep -q 'patch configmap wva-scaling-policy-config'; then
    fail "the arm wrote its policy before refusing the node selector"
elif ! grep -q 'patch configmap wva-scaling-policy-config' "$CALLS"; then
    fail "with the controller counting the listed nodes the arm did not go on: $(printf '%s' "$OUT" | tail -3)"
else
    ok "a share arm on several nodes needs the controller's gpu-inventory on exactly those nodes"
fi

# Placement, from a sampler file: two nodes, a 2-GPU model on one of them.
case_begin
PL="$WORK/placement"; mkdir -p "$PL"
cat > "$PL/gpus.jsonl" <<'EOF'
{"ts": 1, "pods": [{"name": "a-1", "node": "n1", "gpus": 2, "model": "qwen3-30b-a3b"}, {"name": "b-1", "node": "n2", "gpus": 1, "model": "qwen3-8b"}]}
{"ts": 2, "failed": true}
{"ts": 3, "pods": [{"name": "a-1", "node": "n1", "gpus": 2, "model": "qwen3-30b-a3b"}, {"name": "a-2", "node": "n1", "gpus": 2, "model": "qwen3-30b-a3b"}, {"name": "b-1", "node": "n2", "gpus": 1, "model": "qwen3-8b"}, {"name": "b-2", "node": "n2", "gpus": 1, "model": "qwen3-8b"}]}
EOF
if ! python3 "$ROOT/hack/benchmark/two_model_share_report.py" --placement "$PL/gpus.jsonl" > "$PL/placement.json"; then
    fail "the placement of a two-node sampler file could not be derived"
elif ! sum="$(python3 "$ROOT/hack/benchmark/two_model_share_report.py" --placement-summary "$PL/placement.json")" || \
     [ "$sum" != "n1: up to 4 GPU(s) (qwen3-30b-a3b x2); n2: up to 2 GPU(s) (qwen3-8b x2)" ]; then
    fail "the placement summary is wrong: '$sum'"
elif ! python3 - "$ROOT/hack/benchmark" "$PL/placement.json" <<'PY'
import json, sys
sys.path.insert(0, sys.argv[1])
import two_model_share_report as s
import two_model_report as r
t = "\n".join(s.placement_table([("share", json.load(open(sys.argv[2])))]))
assert "| share | n1 | 4 | qwen3-30b-a3b: 2 | 2 |" in t, t
r.BASELINE = "today"
base = {"max_replicas_per_model": 3, "gpus_per_replica": 1, "bench_nodes": "n1,n2"}
a = {"name": "today", "budget": base}
same = {"name": "share", "budget": dict(base)}
other = {"name": "share", "budget": dict(base, bench_nodes="n1,n3")}
assert r.budget_problem(a, same) is None
assert "ran on nodes" in r.budget_problem(a, other)
PY
then
    fail "the placement table or the node-set guard of the report is wrong (above)"
else
    ok "each arm's placement is derived per node and printed, and arms on other nodes are refused"
fi

case_begin
SC_CALLS="$WORK/ceiling.calls"; : > "$SC_CALLS"
( export KCALLS="$SC_CALLS" MIXED_SO=1
  MAX_REPLICAS=3 MAX_REPLICAS_A=3 MAX_REPLICAS_B=4 MODEL_A=Qwen/Qwen3-30B-A3B-FP8 MODEL_B=Qwen/Qwen3-8B
  set_arm_ceiling today ) >/dev/null 2>&1
if ! grep -q 'patch scaledobject so-a .*"maxReplicaCount":3' "$SC_CALLS" || ! grep -q 'patch scaledobject so-b .*"maxReplicaCount":4' "$SC_CALLS"; then
    fail "each model did not get its own ceiling on its ScaledObject: $(grep 'patch scaledobject' "$SC_CALLS")"
else
    ok "each model's ScaledObject gets that model's ceiling"
fi

case_begin
if python3 - "$ROOT/hack/benchmark" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
import two_model_report as r
r.BASELINE = "today"
base = {"max_replicas_per_model": 3, "gpus_per_replica": 1, "max_replicas_a": 3, "max_replicas_b": 4,
        "gpus_per_replica_a": 2, "gpus_per_replica_b": 1}
other = dict(base, gpus_per_replica_a=1)
a = {"name": "today", "budget": base}
b = {"name": "share", "budget": other}
same = {"name": "shadow", "budget": dict(base)}
sys.exit(0 if r.budget_problem(a, b) and not r.budget_problem(a, same) else 1)
PY
then
    ok "the report refuses arms whose models were priced at different sizes"
else
    fail "the report compared arms whose model A was priced at 2 and 1 GPU(s) per replica"
fi

# The repeats report, on two repeats written here: TTFT per burst, the
# spread, transfers counted from the log (the metric missed one), GPU-seconds.
case_begin
REP="$WORK/reps"
python3 - "$REP" <<'PY'
import json, os, sys
root = sys.argv[1]
sched = [{"start": 0, "end": 60, "rate_a": 1, "rate_b": 1},
         {"start": 60, "end": 90, "rate_a": 1, "rate_b": 8},
         {"start": 90, "end": 160, "rate_a": 1, "rate_b": 8},
         {"start": 160, "end": 180, "rate_a": 1, "rate_b": 1},
         {"start": 180, "end": 210, "rate_a": 6, "rate_b": 1},
         {"start": 210, "end": 280, "rate_a": 6, "rate_b": 1}]
# p95 of the rise windows: B rises at stage 1, A at stage 4.
rise = {"today": (9.0, 20.0), "shadow": (9.5, 19.0), "share": (0.5, 21.0)}
for rep, jitter in (("rep1", 0.0), ("rep2", 0.4)):
    for arm, (b95, a95) in rise.items():
        d = os.path.join(root, rep, arm)
        os.makedirs(d)
        def w(p95):
            return {"n": 100, "failed": 0, "p50": p95 / 2, "p95": p95}
        stages_a = [w(0.2)] * 6
        stages_b = [w(0.2)] * 6
        stages_b[1] = w(b95 + jitter)
        stages_a[4] = w(a95 + jitter)
        meta = {"t0": 1000, "schedule": sched, "windows": {"a": {"overall": w(1), "stages": stages_a},
                                                            "b": {"overall": w(1), "stages": stages_b}}}
        json.dump(meta, open(os.path.join(d, "meta.json"), "w"))
        gpus = 7 if arm == "share" else 5
        with open(os.path.join(d, "gpus.jsonl"), "w") as fh:
            for t in range(990, 1300, 10):
                fh.write(json.dumps({"ts": t, "pods": [{"name": "p", "gpus": gpus, "model": "m"}]}) + "\n")
        json.dump({"sum by (outcome) (increase(x))": [{"metric": {"outcome": "done"}, "value": [0, "1"]}],
                   "sum by (model_name, reason) (y)": [{"metric": {"model_name": "Qwen/Qwen3-30B-A3B-FP8", "reason": "quota-exhausted"}, "value": [0, "120"]}],
                   "sum by (variant_name, limiter_name) (z)": [{"metric": {"variant_name": "a", "limiter_name": "H200"}, "value": [0, "3"]}],
                   "min by (mode) (m)": []},
                  open(os.path.join(d, "share.json"), "w"))
        if arm == "share":
            with open(os.path.join(d, "controller.log"), "w") as fh:
                fh.write('{"level":"info","msg":"Utilization share: filling from idle GPUs","id":"f1","receiver":"b"}\n')
                fh.write('{"level":"info","msg":"Utilization share: transfer started","id":"t1","set":"s1","donor":"b","receiver":"a","planned":true}\n')
                fh.write('{"level":"info","msg":"Utilization share: transfer started","id":"t2","set":"s1","donor":"b","receiver":"a","planned":true}\n')
                fh.write('2026-10-16T10:00:00Z\tINFO\tUtilization share: transfer ended\t{"id": "t1", "outcome": "done", "donor": "b", "receiver": "a"}\n')
                fh.write('{"level":"info","msg":"Utilization share: transfer ended","id":"t2","outcome":"done","donor":"b","receiver":"a"}\n')
PY
REP_OUT="$(python3 "$ROOT/hack/benchmark/two_model_share_repeats.py" --out-root "$REP" \
    --model-a Qwen/Qwen3-30B-A3B-FP8 --model-b Qwen/Qwen3-8B --json "$WORK/reps.json" 2>&1)"
REP_RC=$?
if [ "$REP_RC" -ne 0 ]; then
    fail "the repeats report failed on two complete repeats: $REP_OUT"
elif ! printf '%s\n' "$REP_OUT" | grep -q '| rep2 | share | 2 | 2 | 0 | 0 | 0 | 0 | 0 | 1 | 2 | 1 | 1 |'; then
    fail "transfers were not counted from the controller log (2 done, 1 donor set, 2 node-planned, 1 fill; the metric says 1): $(printf '%s\n' "$REP_OUT" | grep '| share |' | tail -2)"
elif ! printf '%s\n' "$REP_OUT" | grep -q 'model B (Qwen/Qwen3-8B) burst 1, rise window p95: today 9.00 / 9.40 s, share 0.50 / 0.90 s (by repeat). Share was faster in every repeat'; then
    fail "the summary did not report the small model's burst as faster beyond the spread: $(printf '%s\n' "$REP_OUT" | grep 'model B' | tail -1)"
elif ! printf '%s\n' "$REP_OUT" | grep -q 'model A (Qwen/Qwen3-30B-A3B-FP8) burst 1, .*within the run.s own spread'; then
    fail "the summary claimed a difference for the large model's burst inside the spread: $(printf '%s\n' "$REP_OUT" | grep 'model A' | tail -1)"
elif ! printf '%s\n' "$REP_OUT" | grep -q '| rep1 | share | 1960 | 7 | +40% |'; then
    fail "GPU-seconds were not priced against today: $(printf '%s\n' "$REP_OUT" | grep '| rep1 | share |' | tail -1)"
elif ! printf '%s\n' "$REP_OUT" | grep -q 'Qwen/Qwen3-30B-A3B-FP8: quota-exhausted = 120'; then
    fail "refused scale-ups were not reported per model and reason"
else
    ok "the repeats report: TTFT per burst, spread, log-counted transfers, refused scale-ups, GPU-seconds and the summary"
fi

# The standup deploys the copy it makes last, so the engine resources must be
# rendered into THAT copy. Rendered only into benchmark-scenarios' copy, the git
# reset between them restored the upstream file and a P/D run deployed 32 CPU /
# 128Gi after logging 2 CPU / 8Gi.
case_begin
block="$(awk '/Copying local scenario:/ {on=1} on {print} on && /^\tfi$/ {exit}' "$ROOT/Makefile")"
if ! printf '%s\n' "$block" | grep -q 'render-engine-resources.py'; then
    fail "benchmark-standup copies the scenario it deploys without rendering the engine resources into it"
else
    ok "the scenario copy the standup deploys has its engine resources rendered"
fi

# ---------------------------------------------------------------------------
# THREE AND FOUR MODELS (MODEL_COUNT=3|4). Each mistake here drives fewer models
# than the run reports, or reports fewer than it drove: a load Job with two
# containers for four models, a ceiling or a GPU peak that counts two, a
# results wait that stops at two markers. Sourced again in a subshell with
# MODEL_COUNT set, because the model list is fixed when the script is read.
# ---------------------------------------------------------------------------
# nm_source <MODEL_COUNT> [NAME=value...]: the driver re-read with that many
# models, every default it derives from them (the scenario, the stacks)
# recomputed, and the given settings in force when it is read.
nm_source() {
    unset BENCH_SPEC STACK_C STACK_D MODEL_C MODEL_D MAX_REPLICAS_C MAX_REPLICAS_D GPUS_PER_REPLICA_C GPUS_PER_REPLICA_D HIGH_RPS HIGH_RPS_B HIGH_RPS_C HIGH_RPS_D LOW_RPS_C LOW_RPS_D
    MODEL_COUNT="$1"
    shift
    local kv
    for kv in "$@"; do printf -v "${kv%%=*}" '%s' "${kv#*=}"; done
    # Sourced with no arguments: the script's verb dispatch reads $1, which
    # inside this function would be one of the settings above.
    set --
    # shellcheck disable=SC1090
    . "$SCRIPT" >/dev/null 2>&1
    NS=ns-under-test
}

# Every burst of a three- or four-model run needs THREE replicas, so the quota
# of 6 binds (3 + 3 + 1 + 1 = 8). Two models keep their own 9 and 8, and a
# rate the caller sets always wins.
case_begin
r4="$( (nm_source 4; printf '%s ' "$HIGH_RPS" "$HIGH_RPS_B" "$HIGH_RPS_C" "$HIGH_RPS_D" "$LOW_RPS" "$LOW_RPS_C" "$LOW_RPS_D") 2>&1)"
r2="$( (nm_source 2; printf '%s ' "$HIGH_RPS" "$HIGH_RPS_B") 2>&1)"
rset="$( (nm_source 4 HIGH_RPS=12 HIGH_RPS_B=11; printf '%s ' "$HIGH_RPS" "$HIGH_RPS_B") 2>&1)"
if [ "$r4" != "19 16.5 8 5.4 3 1.5 0.5 " ]; then
    fail "four-model rates are '$r4', not bursts 19 16.5 8 5.4 (2.2 x each one-replica ceiling) over lows 3 1.5 0.5"
elif [ "$r2" != "9 8 " ]; then
    fail "a two-model run lost its own burst rates: '$r2', not 9 8"
elif [ "$rset" != "12 11 " ]; then
    fail "a burst rate set by the caller was overridden by the four-model default: '$rset'"
else
    ok "four models burst at 19, 16.5, 8 and 5.4; two keep 9 and 8; a set rate wins"
fi

case_begin
got="$( (nm_source 4; URL_C=http://10.0.0.1:80/qwen3-30b-a3b URL_D=http://10.0.0.1:80/qwen3-32b \
         LOAD_IMAGE=ghcr.io/llm-d/llm-d-benchmark:vX; render_load_job wva-two-model-load-share share 1770000000) 2>&1)"
names="$(printf '%s\n' "$got" | grep -o '^        - name: load-[a-z]' | sed 's/.*load-//' | paste -sd, -)"
if [ "$names" != "a,b,c,d" ]; then
    fail "a four-model load Job renders containers '$names', not a,b,c,d: the models without one are never driven"
elif [ "$(printf '%s\n' "$got" | grep -c 'value: "1770000000"')" -ne 4 ]; then
    fail "the four containers do not share one start barrier, so the rotation is offset by however far apart they started"
elif ! printf '%s\n' "$got" | grep -A1 'name: BASE_URL' | grep -q 'http://10.0.0.1:80/qwen3-32b'; then
    fail "model D's container is not pointed at its own stack: $(printf '%s\n' "$got" | grep -A1 'name: BASE_URL')"
elif command -v yq >/dev/null 2>&1 && \
     [ "$(printf '%s\n' "$got" | yq -r '[.spec.template.spec.containers[].env[] | select(.name == "ROLE") | .value] | join(",")' 2>&1)" != "a,b,c,d" ]; then
    fail "the four-model Job does not parse to four containers with roles a..d"
else
    ok "four models: one Pod, four containers a..d, one barrier, each on its own stack"
fi

case_begin
got2="$( (nm_source 2; printf '%s|%s|%s' "$ROLES" "$(extra_role_flags model endpoint high-rps low-rps)" "${#MODEL_FLAGS[@]}") 2>&1)"
got4="$( (nm_source 4 MAX_REPLICAS_D=5; printf '%s|%s|%s|%s|%s|%s' "$ROLES" "$(peak_gpus)" "$(max_ceiling)" "$(peak_pods 8)" \
          "$(extra_role_flags model | paste -sd' ' -)" "${MODEL_FLAGS[*]}") 2>&1)"
if [ "$got2" != "a b||0" ]; then
    fail "two models are no longer exactly two: roles|extra flags|report flags = '$got2'"
elif [ "$got4" != "a b c d|14|5|8|--model-c Qwen/Qwen3-30B-A3B-FP8 --model-d Qwen/Qwen3-32B|--model-c Qwen/Qwen3-30B-A3B-FP8 --model-d Qwen/Qwen3-32B" ]; then
    fail "four models: roles|peak GPUs|largest ceiling|pods in 8 GPUs|profile flags|report flags = '$got4', want 'a b c d|14|5|8|...' (3+3+3+5 GPUs at 1 each, ceiling 5, 8 pods)"
else
    ok "two models add no flag anywhere; four peak at the sum of their ceilings and pass every model to the reports"
fi

case_begin
export MODEL_COUNT=5
run_verb preflight
unset MODEL_COUNT
if [ "$RC" -eq 0 ] || ! printf '%s' "$OUT" | grep -q 'MODEL_COUNT=5: 2, 3 or 4 models'; then
    fail "MODEL_COUNT=5 was not refused (rc=$RC): $(printf '%s' "$OUT" | tail -3)"
elif [ -s "$CALLS" ]; then
    fail "MODEL_COUNT=5 was refused only after calling the cluster: $(head -3 "$CALLS")"
else
    ok "a model count other than 2, 3 or 4 is refused before anything is asked of the cluster"
fi

# THE QUOTA'S LOWER BOUND, one peak plus every other model's floor: 3 + 3 x 1 = 6
# for four 1-GPU models of up to 3, and 3 + 1 = 4 for two. One below it is refused.
case_begin
MODEL_COUNT=4 WVA_QUOTAS='H200=5' NODE_CPU=127 run_verb preflight
q5_rc="$RC"; q5="$OUT"
MODEL_COUNT=4 WVA_QUOTAS='H200=6' NODE_CPU=127 run_verb preflight
q6="$OUT"
WVA_QUOTAS='H200=3' NODE_CPU=127 run_verb preflight
q3_rc="$RC"; q3="$OUT"
WVA_QUOTAS='H200=4' NODE_CPU=127 run_verb preflight
if [ "$q5_rc" -eq 0 ] || ! printf '%s' "$q5" | grep -q "beside 3 model(s) at MIN_REPLICAS=1 needs 6"; then
    fail "four models under a quota of 5 were not refused for the lower bound of 6 (rc=$q5_rc): $(printf '%s' "$q5" | grep -iE 'quota' | head -3)"
elif ! printf '%s' "$q6" | grep -q "the quota (6) covers one model's peak plus the others' floors (6)"; then
    fail "four models under a quota of 6 did not pass the lower bound: $(printf '%s' "$q6" | grep -iE 'quota' | head -3)"
elif [ "$q3_rc" -eq 0 ] || ! printf '%s' "$q3" | grep -q "beside 1 model(s) at MIN_REPLICAS=1 needs 4"; then
    fail "two models under a quota of 3 were not refused for the lower bound of 4 (rc=$q3_rc): $(printf '%s' "$q3" | grep -iE 'quota' | head -3)"
elif ! printf '%s' "$OUT" | grep -q "the quota (4) covers one model's peak plus the others' floors (4)"; then
    fail "two models under a quota of 4 did not pass the lower bound: $(printf '%s' "$OUT" | grep -iE 'quota' | head -3)"
else
    ok "a quota below one peak plus the other floors is refused: 5 for four models (needs 6), 3 for two (needs 4)"
fi

case_begin
if command -v yq >/dev/null 2>&1; then
    p4="$( (nm_source 4; scenario_size_problems) 2>&1)"
    pdup="$( (nm_source 3; MODEL_C="$MODEL_A"; scenario_size_problems) 2>&1)"
    pwrong="$( (BENCH_SPEC=guides/two-model-warm-pool; MODEL_COUNT=3; . "$SCRIPT" >/dev/null 2>&1; scenario_size_problems) 2>&1)"
    if [ -n "$p4" ]; then
        fail "the four-model defaults disagree with n-model-share: $p4"
    elif ! printf '%s' "$pdup" | grep -q 'share model id'; then
        fail "model C copied model A's id and nothing said so: '$pdup'"
    elif ! printf '%s' "$pwrong" | grep -q 'STACK_C=qwen3-30b-a3b is not a stack of guides/two-model-warm-pool'; then
        fail "three models against the two-stack scenario were not refused: '$pwrong'"
    else
        ok "four models match n-model-share; a copied model id and a two-stack scenario are refused"
    fi
else
    fail "yq is required to read the scenario sizes"
fi

case_begin
# The stack names are the route's path prefixes AND the driver's defaults --
# for every model of n-model-share, as for the two-model scenario.
if ! grep -q 'STACK_C="${STACK_C:-qwen3-30b-a3b}"' "$SCRIPT" || ! grep -q 'STACK_D="${STACK_D:-qwen3-32b}"' "$SCRIPT"; then
    fail "the driver's STACK_C/STACK_D defaults changed; they must match n-model-share's stack names"
elif [ "$(grep -cE '^  - name: "(llama-31-8b|qwen3-8b|qwen3-30b-a3b|qwen3-32b)"' "$SPEC_DIR/n-model-share.yaml")" -ne 4 ]; then
    fail "n-model-share does not carry the four stacks the driver drives"
elif ! grep -q 'scenarios/guides/n-model-share.yaml' "$SPEC_DIR/n-model-share.yaml.j2"; then
    fail "n-model-share's specification points at another scenario: $(grep -A1 scenario_file "$SPEC_DIR/n-model-share.yaml.j2")"
elif ! grep -q 'BENCHMARK_STACKS="$keep_stacks"' "$SCRIPT"; then
    fail "the standup no longer hands the stacks it runs to the scenario copy, so a three-model run stands up four"
else
    ok "n-model-share's four stack names are the driver's defaults, and its specification is its own"
fi

case_begin
printf '{"arm":"share","max_replicas_a":3,"max_replicas_b":3,"gpus_per_replica_a":1,"gpus_per_replica_b":1}\n' > "$WORK/b4.json"
cp "$WORK/b4.json" "$WORK/b2.json"
( nm_source 4; decode_deploy_for() { printf 'short-%s-decode' "$1"; }; add_budget_fields "$WORK/b4.json" ) >/dev/null 2>&1
( nm_source 2; decode_deploy_for() { printf 'short-%s-decode' "$1"; }; add_budget_fields "$WORK/b2.json" ) >/dev/null 2>&1
b4="$(jq -r '[.max_replicas_c, .gpus_per_replica_d, (.model_deploys | keys | join("")), .model_deploys.d, .arm] | map(tostring) | join("|")' "$WORK/b4.json" 2>&1)"
b2="$(jq -r '[has("max_replicas_c"), (.model_deploys | keys | join("")), .model_deploys.b, .max_replicas_b] | map(tostring) | join("|")' "$WORK/b2.json" 2>&1)"
if [ "$b4" != "3|1|abcd|short-qwen3-32b|share" ]; then
    fail "a four-model budget does not record models C and D and every Deployment: '$b4'"
elif [ "$b2" != "false|ab|short-qwen3-8b|3" ]; then
    fail "a two-model budget changed beyond the Deployment map: '$b2'"
else
    ok "budget.json records models C and D and which Deployments are whose; two models keep every field"
fi

case_begin
w3="$( (nm_source 4; printf 'RESULTS-WRITTEN-a\nRESULTS-WRITTEN-b\nRESULTS-WRITTEN-c\n' | all_results_written && echo done || echo waiting) 2>&1)"
w4="$( (nm_source 4; printf 'RESULTS-WRITTEN-a\nRESULTS-WRITTEN-b\nRESULTS-WRITTEN-c\nRESULTS-WRITTEN-d\n' | all_results_written && echo done || echo waiting) 2>&1)"
w2="$( (nm_source 2; printf 'RESULTS-WRITTEN-b\nRESULTS-WRITTEN-a\n' | all_results_written && echo done || echo waiting) 2>&1)"
if [ "$w3" != waiting ]; then
    fail "four loaders counted as done with model D's results unwritten -- the arm would be collected without them"
elif [ "$w4" != done ] || [ "$w2" != done ]; then
    fail "every model's marker present still reads as waiting (four: $w4, two: $w2)"
else
    ok "the results wait needs every model's marker: three of four is still waiting"
fi

# ---------------------------------------------------------------------------
# The schedule and the report, executed.
# ---------------------------------------------------------------------------
case_begin
if python3 "$ROOT/hack/benchmark/two_model_selftest.py"; then
    ok "schedule and report self-test"
else
    fail "the schedule/report self-test failed (above)"
fi

case_begin
CASES_EXPECTED=147
if [ "$CASES" -ne "$CASES_EXPECTED" ]; then
    fail "$CASES cases ran, not $CASES_EXPECTED. Update CASES_EXPECTED deliberately rather than letting coverage drift out."
else
    ok "all $CASES cases ran"
fi

if [ "$FAIL" -ne 0 ]; then
    echo "two-model scenario check FAILED"
    exit 1
fi
echo "two-model scenario check OK"
