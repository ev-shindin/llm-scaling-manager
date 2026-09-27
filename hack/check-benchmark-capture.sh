#!/usr/bin/env bash
# Execute the benchmark capture helpers against a stub kubectl and assert the
# three properties that cost real data when they were missing.
#
# `bash -n` sees none of this. A capture that truncates the file it was pointed
# at, or that watches a cluster nobody named, parses perfectly.
#
#   1. start refuses a populated output unless --force. A capture that replaces
#      a run already on disk destroys the only copy; the run about to begin can
#      be restarted, the bytes already written cannot.
#   2. --context reaches every kubectl call, and is recorded beside the pidfile.
#      Without it the cluster a running capture watches is invisible in ps, so a
#      live capture cannot be told from an abandoned one.
#   3. stop says so when it is handed a namespace the capture was not started
#      with, rather than silently finalising someone elses file.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SR="$ROOT/hack/benchmark/sample_replicas.sh"
TL="$ROOT/hack/benchmark/tail_wva_logs.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fails=0
checks=0

ok() { checks=$((checks + 1)); echo "  ok: $1"; }
bad() { checks=$((checks + 1)); fails=$((fails + 1)); echo "  FAIL: $1" >&2; }

# A kubectl that records how it was called and returns an empty item list, so
# the samplers python filter has something valid to read.
cat > "$WORK/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "$@" >> "${STUB_LOG:?}"
echo '{"items":[]}'
STUB
chmod +x "$WORK/kubectl"
export KUBECTL_CMD="$WORK/kubectl"
export STUB_LOG="$WORK/calls.log"
: > "$STUB_LOG"
export REPLICA_SAMPLE_INTERVAL=3600   # one sample, then sleep past the test

# ---- 1. a populated output is not replaced -------------------------------
OUT="$WORK/samples.json"
printf 'PRECIOUS DATA' > "$OUT"
if bash "$SR" start demo-ns "$OUT" >"$WORK/out1" 2>"$WORK/err1"; then
    bad "start overwrote a populated output (exit 0)"
else
    if grep -q "refusing to start" "$WORK/err1"; then
        ok "start refuses a populated output"
    else
        bad "start failed but not with a refusal: $(head -1 "$WORK/err1")"
    fi
fi
if [ "$(cat "$OUT")" = "PRECIOUS DATA" ]; then
    ok "the populated output is byte-for-byte untouched"
else
    bad "the populated output was modified: $(head -c 40 "$OUT")"
fi

# ---- 2. --force replaces it, --context reaches kubectl and the owner file --
: > "$STUB_LOG"
if bash "$SR" --context my-cluster --force start demo-ns "$OUT" >"$WORK/out2" 2>&1; then
    ok "start with --force succeeds on a populated output"
else
    bad "start --force failed: $(head -2 "$WORK/out2")"
fi
sleep 1
bash "$SR" stop "$OUT" demo-ns >/dev/null 2>&1 || true

if grep -q -- "--context my-cluster" "$STUB_LOG"; then
    ok "--context reaches the kubectl calls"
else
    bad "--context never reached kubectl; calls were: $(head -2 "$STUB_LOG")"
fi

# the owner file is written while the capture runs, so re-run and inspect it
: > "$STUB_LOG"
bash "$SR" --context my-cluster --force start other-ns "$OUT" >/dev/null 2>&1
if [ -f "$OUT.owner" ]; then
    owner="$(cat "$OUT.owner")"
    case "$owner" in
        *"namespace=other-ns"*) ok "the owner file records the namespace" ;;
        *) bad "owner file has no namespace: $owner" ;;
    esac
    case "$owner" in
        *"context=my-cluster"*) ok "the owner file records the context" ;;
        *) bad "owner file has no context: $owner" ;;
    esac
else
    bad "no owner file was written"
fi

# ---- 3. stop warns when the namespace does not match --------------------
warn="$(bash "$SR" stop "$OUT" a-different-ns 2>&1 >/dev/null || true)"
case "$warn" in
    *"but stop was called with namespace=a-different-ns"*)
        ok "stop warns on a namespace mismatch" ;;
    *)
        bad "stop did not warn on a mismatch; said: $(echo "$warn" | head -1)" ;;
esac

# ---- the log tail carries the same two guards ----------------------------
LOGOUT="$WORK/controller.log"
printf 'EARLIER RUN\n' > "$LOGOUT"
if bash "$TL" start demo-ns "$LOGOUT" >/dev/null 2>"$WORK/err3"; then
    bad "tail start overwrote a populated log"
else
    grep -q "refusing to start" "$WORK/err3" \
        && ok "tail start refuses a populated log" \
        || bad "tail start failed without a refusal"
fi
if [ "$(cat "$LOGOUT")" = "EARLIER RUN" ]; then
    ok "the populated log is untouched"
else
    bad "the populated log was modified"
fi

: > "$STUB_LOG"
bash "$TL" --context other-cluster --force start demo-ns "$LOGOUT" >/dev/null 2>&1
sleep 1
bash "$TL" stop demo-ns "$LOGOUT" >/dev/null 2>&1 || true
if grep -q -- "--context other-cluster" "$STUB_LOG"; then
    ok "tail passes --context to kubectl"
else
    bad "tail did not pass --context; calls: $(head -2 "$STUB_LOG")"
fi

echo "benchmark capture checks: $checks run, $fails failed"
[ "$fails" -eq 0 ] || exit 1
