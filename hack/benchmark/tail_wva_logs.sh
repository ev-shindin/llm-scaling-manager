#!/usr/bin/env bash
# Continuously capture the WVA controller's own logs for the duration of a
# benchmark run, into a plain file this process controls end to end.
#
# The controller pod's log buffer is bounded by kubelet's fixed per-container
# rotation size (a small, fixed cap unrelated to how much disk the node
# actually has), not by how long the run takes. Any run longer than a few
# minutes -- worse now that k1/k2 decision logging adds real per-cycle
# volume on top of KEDA's own polling chatter -- rotates the very lines
# dump_k2_decisions.py needs, out from under it, often before the run even
# finishes. Tailing continuously from the start and writing to a plain file
# on local disk sidesteps that: once a line has left the pod, the
# container's own rotation no longer matters.
#
# Usage:
#   tail_wva_logs.sh [--context <ctx>] [--force] start <namespace> <outfile>
#   tail_wva_logs.sh [--context <ctx>] stop <namespace> <outfile>
#
# `kubectl logs -f` does not survive the run: the apiserver's load balancer
# resets long-lived streaming connections on an idle-ish timeout regardless of
# how much log volume is flowing (measured on pokprod001: "connection reset by
# peer" at ~15 minutes into an 83-minute run, mid-stream, pod itself never
# restarted). A bare `-f` call that drops silently at that point undoes the
# whole reason this script exists -- 69 minutes of the run's k1/k2 decisions
# were never captured anywhere, kubelet's own rotation having already reclaimed
# the pod's in-container copy. So `start` runs the tail in a reconnect loop:
# on any exit, resume from the last line's own timestamp via --since-time
# rather than from "now", so a drop loses zero lines (just re-fetches the
# handful spanning the reconnect, deduplicated by `stop`).
set -u


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

CMD="${1:?usage: $0 [--context <ctx>] [--force] start <namespace> <outfile> | stop <namespace> <outfile>}"
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

case "$CMD" in
  start)
    NS="${2:?namespace required}"; OUT="${3:?outfile required}"
    mkdir -p "$(dirname "$OUT")"
    _guard_output "$OUT" || exit 1
    : > "$OUT"
    rm -f "$OUT.stop"
    # No --prefix: dump_k2_decisions.py's LOG_LINE regex expects the
    # timestamp at column 0. A single-replica controller means this is
    # unambiguous without one; the rare exception (a brief moment where two
    # pods match during a rollout) isn't worth breaking the parser for.
    (
      while [ ! -f "$OUT.stop" ]; do
        since_ts="$(tail -n1 "$OUT" 2>/dev/null | cut -f1)"
        if [ -n "$since_ts" ]; then
          _kube logs -n "$NS" -l app.kubernetes.io/name=workload-variant-autoscaler \
            -f --since-time="$since_ts" --tail=-1 --max-log-requests=10 \
            >> "$OUT" 2>> "$OUT.stderr"
        else
          _kube logs -n "$NS" -l app.kubernetes.io/name=workload-variant-autoscaler \
            -f --since=1s --tail=-1 --max-log-requests=10 \
            >> "$OUT" 2>> "$OUT.stderr"
        fi
        sleep 1
      done
    ) &
    echo $! > "$OUT.pid"
    _write_owner "$OUT" "$NS" "$(cat "$OUT.pid")"
    echo "WVA log tail started (pid $(cat "$OUT.pid"), context ${CTX:-inherited}," \
         "namespace $NS) -> $OUT"
    ;;
  stop)
    NS="${2:?namespace required}"; OUT="${3:?outfile required}"
    _check_owner "$OUT" "$NS"
    rm -f "$OUT.owner"
    touch "$OUT.stop"
    if [ -f "$OUT.pid" ]; then
      pid="$(cat "$OUT.pid")"
      kill "$pid" 2>/dev/null || true
      # `kubectl logs -f` occasionally re-execs an internal watch on
      # reconnect; the pidfile only ever has the original PID, so sweep for
      # anything matching this exact namespace+label as a backstop.
      sleep 1
      pkill -f "logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler" 2>/dev/null || true
      rm -f "$OUT.pid"
    fi
    rm -f "$OUT.stop"
    # --since-time reconnects overlap by design (see start); collapse the
    # handful of re-fetched duplicate lines per reconnect back to one each.
    if [ -f "$OUT" ]; then
      awk '!seen[$0]++' "$OUT" > "$OUT.dedup" && mv "$OUT.dedup" "$OUT"
    fi
    n=$(wc -l < "$OUT" 2>/dev/null || echo 0)
    echo "WVA log tail stopped: $n line(s) -> $OUT"
    ;;
  *)
    echo "unknown command: $CMD" >&2; exit 2 ;;
esac
