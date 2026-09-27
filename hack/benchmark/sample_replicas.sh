#!/usr/bin/env bash
# Sample serving replica counts for the duration of a benchmark run.
#
# The harness already writes metrics/processed/replica_status_timeseries.json,
# and on every FMA run measured it came back with snapshots but no controllers:
# collect_metrics.sh filters them with
#
#     model_filter="${LLMDBENCH_HARNESS_STACK_NAME:-}"
#     if model_filter and model != model_filter: continue
#
# comparing a STACK name against the llm-d.ai/model LABEL. Ours are
# "inference-scheduling-wva" and "qwen-qwe-...", which never match, so every
# controller is dropped. The variable cannot be overridden from here either:
# run_only.sh writes it into the harness pod spec from endpoint_stack_name,
# which is also used as --stack, so it cannot simply be set to the model.
#
# Rather than depend on that being fixed upstream, sample it ourselves. The
# result is the same shape the harness produces, so postprocess reads it with
# the same code.
#
# Usage:
#   sample_replicas.sh [--context <ctx>] [--force] start <namespace> <outfile>
#   sample_replicas.sh [--context <ctx>] stop <outfile>
set -u
# --help prints this file's header comment -- the documentation the script
# already carries, so it cannot drift from what the script does. Placed before
# any argument handling because several of these take a namespace as $1, and
# without it `--help` was consumed as one.
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac



# Optional leading flags, before the command:
#   --context <ctx>   which kube context to watch. Without it the script
#                     inherits whatever KUBECONFIG the calling shell had, and
#                     then nothing in `ps` says which cluster a running capture
#                     is watching -- which is how a live capture gets mistaken
#                     for a stale one and killed.
#   --force           permit start to replace a populated output file.
CTX=""
FORCE=0
while :; do
    case "${1:-}" in
        --context) CTX="${2:?--context needs a value}"; shift 2 ;;
        --context=*) CTX="${1#--context=}"; shift ;;
        --force) FORCE=1; shift ;;
        *) break ;;
    esac
done

CMD="${1:?usage: $0 [--context <ctx>] [--force] start <namespace> <outfile> | stop <outfile>}"
KUBECTL="${KUBECTL_CMD:-kubectl}"
# Every kubectl call goes through this, so the context cannot be applied to some
# calls and not others.
_kube() {
    if [ -n "$CTX" ]; then
        $KUBECTL --context "$CTX" "$@"
    else
        $KUBECTL "$@"
    fi
}

# Refuse to replace a populated output unless the caller says it owns the path.
# A capture that silently truncates is worse than one that fails to start: the
# run that is about to begin can be restarted, the data already on disk cannot.
_guard_output() {
    local out="$1"
    if [ "$FORCE" -eq 1 ] || [ ! -s "$out" ]; then
        return 0
    fi
    echo "refusing to start: $out already holds $(wc -c < "$out") bytes" >&2
    if [ -f "$out.owner" ]; then
        echo "  it belongs to: $(cat "$out.owner")" >&2
    fi
    echo "  pass --force to replace it, or choose another output path" >&2
    return 1
}

# Who owns this capture, in a form a human reading /tmp can act on.
_write_owner() {
    printf 'namespace=%s context=%s started=%s pid=%s cmd=%s\n' \
        "$2" "${CTX:-<inherited KUBECONFIG>}" \
        "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$3" "$(basename "$0")" > "$1.owner"
}

# Warn rather than fail: a mismatch usually means someone is stopping a capture
# they did not start, and the honest thing is to say so and still stop it.
_check_owner() {
    local out="$1" ns="$2"
    [ -f "$out.owner" ] || return 0
    case "$(cat "$out.owner")" in
        *"namespace=$ns "*) : ;;
        *) echo "warning: $out was started as [$(cat "$out.owner")]," >&2
           echo "         but stop was called with namespace=$ns" >&2 ;;
    esac
}
INTERVAL="${REPLICA_SAMPLE_INTERVAL:-10}"

_snapshot() {
    local ns="$1"
    _kube --namespace "$ns" get deployments,statefulsets -o json 2>/dev/null \
      | python3 -c '
import json, sys
from datetime import datetime, timezone
try:
    data = json.load(sys.stdin)
except Exception:
    data = {"items": []}
snap = {"timestamp": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "controllers": []}
for item in data.get("items", []):
    tmpl = item.get("spec", {}).get("template", {}).get("metadata", {}).get("labels", {})
    # Same predicate as the harness, WITHOUT the model filter that empties it:
    # a serving pod template, or the FMA requester.
    if tmpl.get("llm-d.ai/inferenceServing") != "true" and tmpl.get("llm-d.ai/role") != "requester":
        continue
    st = item.get("status", {})
    snap["controllers"].append({
        "name": item.get("metadata", {}).get("name", ""),
        "kind": item.get("kind", "Deployment"),
        "desired_replicas": item.get("spec", {}).get("replicas", 0),
        "ready_replicas": st.get("readyReplicas", 0) or 0,
        "available_replicas": st.get("availableReplicas", 0) or 0,
    })
print(json.dumps(snap))
'
}

# Per-pod timings, so a run can say whether FMA WOKE a sleeping instance or
# rebuilt one. That distinction is invisible in replica counts -- both look like
# "a replica arrived" -- and it is the difference between 3s and ~50-80s. We
# only found it by reading controller logs by hand; measuring it makes it a
# number in the results table instead.
#
# Emitted as JSON lines and deduped at stop, because pods vanish on scale-down:
# a single collection at the end would miss exactly the replicas we care about.
_pods_snapshot() {
    local ns="$1"
    _kube --namespace "$ns" get pods -o json 2>/dev/null \
      | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)

# Launcher creation times, so we can tell a pre-existing launcher (which COULD
# have been woken) from one built for this bind (which certainly was not).
launchers = {}
for p in data.get("items", []):
    lb = (p["metadata"].get("labels") or {})
    if lb.get("app.kubernetes.io/component") == "launcher":
        launchers[p["metadata"]["name"]] = p["metadata"].get("creationTimestamp")

for p in data.get("items", []):
    m = p["metadata"]
    lb = m.get("labels") or {}
    # Launchers also carry an inference-serving label, but they are not scale
    # -target replicas -- they are the pool a replica binds INTO. Counting them
    # here would mix the thing being measured with the thing it waits for.
    if lb.get("app.kubernetes.io/component") == "launcher":
        continue
    serving = lb.get("llm-d.ai/inference-serving") == "true" or lb.get("llm-d.ai/inferenceServing") == "true"
    requester = lb.get("llm-d.ai/role") == "requester" or lb.get("app") == "dp-app"
    if not (serving or requester):
        continue
    ready = None
    for c in p.get("status", {}).get("conditions", []):
        if c.get("type") == "Ready" and c.get("status") == "True":
            ready = c.get("lastTransitionTime")
    dual = lb.get("dual-pods.llm-d.ai/dual")
    print(json.dumps({
        "name": m.get("name"),
        "node": p.get("spec", {}).get("nodeName"),
        "created": m.get("creationTimestamp"),
        "ready_at": ready,
        "bound_launcher": dual,
        "launcher_created": launchers.get(dual) if dual else None,
        "is_requester": bool(requester),
    }))
'
}

case "$CMD" in
  start)
    NS="${2:?namespace required}"; OUT="${3:?outfile required}"
    mkdir -p "$(dirname "$OUT")"
    _guard_output "$OUT" || exit 1
    printf '{"snapshots":[' > "$OUT"
    : > "$OUT.pods.jsonl"
    (
      first=1
      while :; do
        snap=$(_snapshot "$NS")
        if [ -n "$snap" ]; then
          [ $first -eq 1 ] || printf ',' >> "$OUT"
          printf '%s' "$snap" >> "$OUT"
          first=0
        fi
        _pods_snapshot "$NS" >> "$OUT.pods.jsonl" 2>/dev/null || true
        sleep "$INTERVAL"
      done
    ) &
    echo $! > "$OUT.pid"
    _write_owner "$OUT" "$NS" "$(cat "$OUT.pid")"
    echo "replica sampler started (pid $(cat "$OUT.pid"), every ${INTERVAL}s," \
         "context ${CTX:-inherited}, namespace $NS) -> $OUT"
    ;;
  stop)
    OUT="${2:?outfile required}"
    _check_owner "$OUT" "${3:-}"
    rm -f "$OUT.owner"
    if [ -f "$OUT.pid" ]; then
      kill "$(cat "$OUT.pid")" 2>/dev/null || true
      rm -f "$OUT.pid"
    fi
    # Close the array even if no snapshot was written, so the file is always
    # valid JSON. An empty snapshots list reads as "not measured" downstream,
    # which is the honest answer -- unlike a zero replica count.
    printf ']}' >> "$OUT"
    # Dedupe the pod observations into one record per pod. Keep the observation
    # that has a Ready time: a pod is seen several times, and only later samples
    # carry the transition we want.
    TIMINGS="$(dirname "$OUT")/wva_pod_timings.json"
    python3 - "$OUT.pods.jsonl" "$TIMINGS" <<'PY' 2>/dev/null || true
import json, sys
src, dst = sys.argv[1], sys.argv[2]
best = {}
try:
    for line in open(src, encoding="utf-8"):
        line = line.strip()
        if not line:
            continue
        try:
            r = json.loads(line)
        except Exception:
            continue
        n = r.get("name")
        if not n:
            continue
        prev = best.get(n)
        # Prefer a record that knows when the pod became Ready, and one that
        # knows which launcher it bound to -- both appear only after the fact.
        if (prev is None
                or (r.get("ready_at") and not prev.get("ready_at"))
                or (r.get("bound_launcher") and not prev.get("bound_launcher"))):
            best[n] = r
except FileNotFoundError:
    pass
json.dump({"pods": list(best.values())}, open(dst, "w", encoding="utf-8"))
print("  pod timings: %d pod(s) -> %s" % (len(best), dst))
PY
    rm -f "$OUT.pods.jsonl"
    n=$(python3 -c "
import json,sys
try:
    d=json.load(open('$OUT'))
    s=d.get('snapshots',[])
    c=sum(len(x.get('controllers',[])) for x in s)
    print(f'{len(s)} snapshot(s), {c} controller sample(s)')
except Exception as e:
    print('unreadable:', e)
" 2>/dev/null)
    echo "replica sampler stopped: $n -> $OUT"
    ;;
  *)
    echo "unknown command: $CMD" >&2; exit 2 ;;
esac
