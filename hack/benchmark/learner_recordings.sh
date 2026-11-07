#!/usr/bin/env bash
# Copy the learner recordings out of the WVA controller's Pods, so a benchmark
# run keeps them after teardown.
#
# The recorder (config/components/learner-recorder) writes to an emptyDir. A
# new Pod has a new, empty one, so anything that replaces the Pod -- an
# uninstall, a teardown, a controller restart -- deletes every recording the
# run made. learnreplay can only read what was copied out first.
#
# Usage:
#   learner_recordings.sh [--context <ctx>] snapshot <namespace> <dir>
#   learner_recordings.sh [--context <ctx>] [--force] start <namespace> <dir>
#   learner_recordings.sh [--context <ctx>] stop <namespace> <dir>
#   learner_recordings.sh [--context <ctx>] stop-all <namespace> <dir-prefix>
#
#   snapshot  copy once, now. Exits 0 when <dir> holds at least one recording
#             afterwards, 1 when it holds none (the reason is printed), 2 on a
#             usage error.
#   start     snapshot every RECORDINGS_INTERVAL seconds (default 300) in the
#             background, for at most RECORDINGS_MAX_SECONDS (default 21600),
#             so a Pod that dies mid-run has been copied recently.
#   stop      end that loop, then take the final snapshot. The final snapshot
#             is taken even when no loop was running.
#   stop-all  stop every capture under <dir-prefix>* that names <namespace>;
#             a teardown calls it, so an interrupted run still gets its final
#             snapshot before the controller goes. Exits 1 when it stopped none.
#
# What lands in <dir>:
#   <dir>/<pod>/learnrec-*.jsonl   the copies, one directory per Pod
#   <dir>/collection.tsv           one line per Pod per snapshot: when, which
#                                  Pod, its phase, the outcome, the files and
#                                  bytes held for it afterwards
#
# The rules, each learned by losing a run's evidence:
#   - a copy never shrinks the stored one. Each file is copied to a temporary
#     name and replaces the stored copy only when it is larger; a recording is
#     append-only, so a smaller copy is a truncated stream, not new data.
#     The compare and the rename hold a per-Pod flock, so two copies running
#     at once cannot interleave them.
#   - `kubectl exec` needs a Running Pod. A Pod in any other phase is skipped
#     and its last copy kept, which is why `start` snapshots during the run
#     and not only at the end.
#   - every exec is bounded (RECORDINGS_COPY_TIMEOUT, default 300 s, through
#     `timeout --foreground` when it is installed, so a stop's group signal
#     reaches the copy in flight); every list carries --request-timeout
#     (RECORDINGS_REQUEST_TIMEOUT, default 30s).
#   - no recordings is said, never implied: an empty result is not a pass.
#
# The copy goes through the `recordings` sidecar the recorder patch adds,
# because the manager image has no tar. A Pod with no recording directory has
# the recorder off; one with a directory and no sidecar cannot be copied from,
# and both are reported as such.
#
# Any harness can call it: snapshot before anything replaces the controller's
# Pod, or start/stop around a run. KUBECTL_CMD overrides kubectl.
set -u
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac

HERE="$(cd "$(dirname "$0")" && pwd)"
SELF="$HERE/$(basename "$0")"
# shellcheck source=hack/benchmark/capture_lib.sh
. "$HERE/capture_lib.sh"
CONTROLLER_PODS="$HERE/controller_pods.py"

capture_parse_args "$@" || exit 2
set -- ${CAPTURE_POSITIONAL[@]+"${CAPTURE_POSITIONAL[@]}"}

INTERVAL="${RECORDINGS_INTERVAL:-300}"
MAX_SECONDS="${RECORDINGS_MAX_SECONDS:-21600}"
COPY_TIMEOUT="${RECORDINGS_COPY_TIMEOUT:-300}"
REQUEST_TIMEOUT="${RECORDINGS_REQUEST_TIMEOUT:-30s}"
# The tools the copy leans on, by name, so a check can take one away.
TIMEOUT_CMD="${RECORDINGS_TIMEOUT_CMD:-timeout}"
FLOCK_CMD="${RECORDINGS_FLOCK_CMD:-flock}"
for v in INTERVAL MAX_SECONDS COPY_TIMEOUT; do
    # 10# so a leading zero is not octal; 0, 00 and 000 are all zero.
    case "${!v}" in
        ''|*[!0-9]*) echo "RECORDINGS_$v must be a whole number of seconds above 0 (got '${!v}')" >&2; exit 2 ;;
    esac
    if [ "$((10#${!v}))" -lt 1 ]; then
        echo "RECORDINGS_$v must be a whole number of seconds above 0 (got '${!v}')" >&2; exit 2
    fi
done

usage() {
    echo "usage: $0 [--context <ctx>] [--force] snapshot|start|stop <namespace> <dir> | stop-all <namespace> <dir-prefix>" >&2
    exit 2
}

# The record the ownership helpers key on. It is also the run's log of what
# each snapshot found, filed with the copies.
_index() { printf '%s/collection.tsv\n' "$1"; }

_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

_size() { wc -c < "$1" 2>/dev/null | tr -d ' ' || echo 0; }

# _held <dir> -- "<files> <bytes>" of the copies under <dir>, temporary ones
# excluded.
_held() {
    local files=0 bytes=0 f s
    for f in "$1"/*/*; do
        # `*` skips dot-directories, so the .incoming.* temporaries never count.
        [ -f "$f" ] || continue
        s="$(_size "$f")"
        files=$((files + 1)); bytes=$((bytes + ${s:-0}))
    done
    printf '%s %s\n' "$files" "$bytes"
}

# _bounded <cmd...> -- run a command for at most COPY_TIMEOUT seconds.
#
# --foreground keeps the command in the caller's process group. Without it,
# timeout puts its child in a group of its own, so the group signal a stop
# sends missed the copy in flight: the stop reported success, dropped the
# ownership record, and the exec ran on for up to COPY_TIMEOUT seconds.
#
# A missing tool is said once per process, on stderr (in a background copy
# that is capture.log), rather than silently copying with less protection.
# The tools are probed once, by _check_tools, before any copy: _bounded's
# stderr is the exec's, captured into the copy's error file, so a warning
# printed there was never seen.
TIMEOUT_MODE=""
_check_tools() {
    [ -n "$TIMEOUT_MODE" ] && return 0
    if "$TIMEOUT_CMD" --foreground 1 true >/dev/null 2>&1; then
        TIMEOUT_MODE=foreground
    elif command -v "$TIMEOUT_CMD" >/dev/null 2>&1; then
        TIMEOUT_MODE=plain
        echo "WARNING: learner recordings: $TIMEOUT_CMD has no --foreground; a stop may miss a copy in flight, which then runs for up to ${COPY_TIMEOUT}s" >&2
    else
        TIMEOUT_MODE=none
        echo "WARNING: learner recordings: no $TIMEOUT_CMD command; a copy is not bounded and can hang" >&2
    fi
    if ! command -v "$FLOCK_CMD" >/dev/null 2>&1; then
        HAVE_FLOCK=0
        echo "WARNING: learner recordings: no $FLOCK_CMD command; two copies of one Pod run unlocked and can shrink a copy" >&2
    fi
}
_bounded() {
    case "$TIMEOUT_MODE" in
        foreground) "$TIMEOUT_CMD" --foreground "$COPY_TIMEOUT" "$@" ;;
        plain) "$TIMEOUT_CMD" "$COPY_TIMEOUT" "$@" ;;
        *) "$@" ;;
    esac
}

# _locked <lockfile> <cmd...> -- run a command holding an exclusive lock, so
# two copies of one Pod (the background one and a stop or teardown) cannot
# each compare sizes and then rename in the other order, which shrank a copy.
# flock, because the kernel drops the lock when its holder dies; where it is
# missing the command runs unlocked.
_locked() {
    local lock="$1"; shift
    if [ "$HAVE_FLOCK" -eq 1 ]; then
        ( "$FLOCK_CMD" 9 && "$@" ) 9>"$lock"
    else
        "$@"
    fi
}
HAVE_FLOCK=1

# _install <new-file> <stored-file> -- replace the stored copy only when the
# new one is larger. Prints grown, kept or same. Run under _locked.
_install() {
    local new old
    new="$(_size "$1")"; old=0
    [ -f "$2" ] && old="$(_size "$2")"
    if [ "${new:-0}" -gt "${old:-0}" ]; then
        # Same filesystem, so the rename is atomic: a reader sees the old
        # copy or the new one, never half of either.
        mv -f "$1" "$2" && echo grown
    elif [ "${new:-0}" -lt "${old:-0}" ]; then
        echo kept
    else
        echo same
    fi
}

_kubectl_argv() {
    # Word-split KUBECTL_CMD on purpose, as capture_kube does.
    KARGV=($CAPTURE_KUBECTL)
    [ -n "$CAPTURE_CTX" ] && KARGV+=(--context "$CAPTURE_CTX")
}

_record() {
    # time pod phase outcome files bytes detail
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$(_now)" "$2" "$3" "$4" "$5" "$6" "$7" >> "$(_index "$1")"
}

# _copy_pod <dir> <ns> <pod> <recdir> <container> -- copy one Pod. Prints one
# line; sets COPY_OUTCOME.
_copy_pod() {
    local dir="$1" ns="$2" pod="$3" recdir="$4" ctr="$5"
    local tmp rc err f base res replaced=0 kept=0 same=0 lockfail=0
    tmp="$(mktemp -d "$dir/.incoming.XXXXXX")" || { COPY_OUTCOME="no-temp-dir"; return 1; }
    _kubectl_argv
    _bounded "${KARGV[@]}" exec -n "$ns" "$pod" -c "$ctr" -- tar cf - -C "$recdir" . \
        > "$tmp/stream.tar" 2> "$tmp/stderr"
    rc=$?
    if [ "$rc" -ne 0 ]; then
        err="$(head -1 "$tmp/stderr" 2>/dev/null | tr '\t' ' ')"
        [ "$rc" -eq 124 ] && err="timed out after ${COPY_TIMEOUT}s${err:+: $err}"
        rm -rf "$tmp"
        COPY_OUTCOME="copy-failed"
        COPY_DETAIL="exit $rc${err:+: $err}; the previous copy is kept"
        return 1
    fi
    mkdir -p "$tmp/x" "$dir/$pod"
    if ! tar xf "$tmp/stream.tar" -C "$tmp/x" 2> "$tmp/stderr"; then
        err="$(head -1 "$tmp/stderr" 2>/dev/null | tr '\t' ' ')"
        rm -rf "$tmp"
        COPY_OUTCOME="copy-failed"
        COPY_DETAIL="the stream did not unpack${err:+: $err}; the previous copy is kept"
        return 1
    fi
    # The recorder writes learnrec-*.jsonl, never a dotfile.
    for f in "$tmp/x"/*; do
        [ -f "$f" ] || continue
        base="$(basename "$f")"
        # A lock that cannot be opened or taken installs nothing, and says so:
        # it used to print nothing and be counted as unchanged.
        res="$(_locked "$dir/.$pod.lock" _install "$f" "$dir/$pod/$base")" || res="${res:-lock-failed}"
        case "$res" in
            grown) replaced=$((replaced + 1)) ;;
            kept) kept=$((kept + 1)) ;;
            same) same=$((same + 1)) ;;
            *) lockfail=$((lockfail + 1)) ;;
        esac
    done
    rm -rf "$tmp"
    COPY_DETAIL="$replaced grown, $same unchanged, $kept smaller copy refused"
    if [ "$lockfail" -gt 0 ]; then
        COPY_OUTCOME="lock-failed"
        COPY_DETAIL="$COPY_DETAIL, $lockfail not installed: the lock $dir/.$pod.lock could not be taken, or the rename failed"
        return 1
    fi
    COPY_OUTCOME="copied"
    return 0
}

# snapshot_once <ns> <dir>
snapshot_once() {
    local ns="$1" dir="$2" list rows rc line pod phase recdir ctr held files bytes
    local reasons=""
    _check_tools
    mkdir -p "$dir" || { echo "learner recordings: cannot create $dir" >&2; return 1; }
    [ -f "$(_index "$dir")" ] || printf 'time\tpod\tphase\toutcome\tfiles\tbytes\tdetail\n' > "$(_index "$dir")"
    list="$(capture_kube --request-timeout="$REQUEST_TIMEOUT" get pods -n "$ns" \
        -l control-plane=controller-manager -o json 2>"$dir/.list.stderr")"
    rc=$?
    if [ "$rc" -ne 0 ]; then
        # Forbidden or unreachable is not "no controller": say which.
        reasons="could not list the controller Pods in $ns: $(head -1 "$dir/.list.stderr" 2>/dev/null)"
        _record "$dir" "-" "-" "list-failed" 0 0 "$(head -1 "$dir/.list.stderr" 2>/dev/null | tr '\t' ' ')"
    else
        # The helper exits 0 with no rows for a list with no controller Pod,
        # and non-zero when it could not read the list at all (bad JSON, no
        # python3). The second is not "no controller", so it is named.
        rows="$(printf '%s' "$list" | python3 "$CONTROLLER_PODS" --pods 2>"$dir/.list.stderr")"
        rc=$?
        if [ "$rc" -ne 0 ]; then
            reasons="could not read the controller Pod list (controller_pods.py exit $rc: $(head -1 "$dir/.list.stderr" 2>/dev/null))"
            _record "$dir" "-" "-" "list-unreadable" 0 0 "$(head -1 "$dir/.list.stderr" 2>/dev/null | tr '\t' ' ')"
            rows=""
        elif [ -z "$rows" ]; then
            reasons="no controller Pod in namespace $ns"
            _record "$dir" "-" "-" "no-controller-pod" 0 0 "-"
        fi
    fi
    rm -f "$dir/.list.stderr"
    while IFS="$(printf '\t')" read -r pod phase recdir ctr; do
        [ -n "$pod" ] || continue
        if [ "$recdir" = "-" ]; then
            line="recorder off (no LEARNER_RECORDING_DIR or --learner-recording-dir on the manager)"
            _record "$dir" "$pod" "$phase" "recorder-off" 0 0 "-"
        elif [ "$ctr" = "-" ]; then
            line="records to $recdir but has no recordings sidecar to copy through (the manager image has no tar)"
            _record "$dir" "$pod" "$phase" "no-sidecar" 0 0 "$recdir"
        elif [ "$phase" != "Running" ]; then
            line="is $phase; kubectl exec needs a Running Pod, so the previous copy is kept"
            _record "$dir" "$pod" "$phase" "not-running" 0 0 "-"
        else
            COPY_DETAIL=""
            _copy_pod "$dir" "$ns" "$pod" "$recdir" "$ctr"
            held="$(_held_pod "$dir" "$pod")"
            line="$COPY_OUTCOME ($COPY_DETAIL); holds $held"
            _record "$dir" "$pod" "$phase" "$COPY_OUTCOME" "${held%% *}" "$(printf '%s' "$held" | cut -d' ' -f3)" "$COPY_DETAIL"
        fi
        echo "learner recordings: $pod $line"
        reasons="${reasons:+$reasons; }$pod: $line"
    done <<EOF
${rows:-}
EOF
    held="$(_held "$dir")"
    files="${held%% *}"; bytes="${held##* }"
    if [ "$files" -gt 0 ]; then
        echo "learner recordings: $files file(s), $bytes bytes held in $dir (log: $(_index "$dir"))"
        return 0
    fi
    echo "learner recordings: NONE held in $dir -- ${reasons:-nothing found}"
    return 1
}

# _held_pod <dir> <pod> -- "<files> file(s) <bytes> bytes" for one Pod.
_held_pod() {
    local files=0 bytes=0 f s
    for f in "$1/$2"/*; do
        [ -f "$f" ] || continue
        s="$(_size "$f")"
        files=$((files + 1)); bytes=$((bytes + ${s:-0}))
    done
    printf '%s file(s) %s bytes\n' "$files" "$bytes"
}

CMD="${1:-}"
[ -n "$CMD" ] || usage
case "$CMD" in
  snapshot)
    [ "$#" -eq 3 ] || usage
    snapshot_once "$2" "$3"
    exit $?
    ;;
  start)
    [ "$#" -eq 3 ] || usage
    NS="$2"; DIR="$3"; OUT="$(_index "$DIR")"
    mkdir -p "$DIR" || { echo "cannot create $DIR" >&2; exit 1; }
    capture_claim "$OUT" "$NS" || exit 1
    if [ "$CAPTURE_FORCE" -ne 1 ]; then
        held="$(_held "$DIR")"
        if [ "${held%% *}" -gt 0 ]; then
            echo "refusing to start: $DIR already holds ${held%% *} recording copies of a previous run" >&2
            echo "  pass --force to add to them, or choose another directory" >&2
            capture_release "$OUT"
            exit 1
        fi
    fi
    rm -f "$OUT.stop"
    ctxargs=()
    [ -n "$CAPTURE_CTX" ] && ctxargs=(--context "$CAPTURE_CTX")
    set -m
    (
      # TERM's default action would end the loop too; the trap only makes it
      # end with 0. It must exit: a loop that ignored TERM would outlive the
      # stop that sent it.
      trap 'exit 0' TERM INT HUP
      deadline=$(( $(date +%s) + MAX_SECONDS ))
      while [ ! -f "$OUT.stop" ]; do
        bash "$SELF" ${ctxargs[@]+"${ctxargs[@]}"} snapshot "$NS" "$DIR" || true
        if [ "$(date +%s)" -ge "$deadline" ]; then
          echo "learner recordings: the background copy ran for ${MAX_SECONDS}s and stopped; stop takes the final snapshot"
          exit 0
        fi
        sleep "$INTERVAL" &
        wait $!
      done
    ) >> "$DIR/capture.log" 2>&1 &
    loop_pid=$!
    set +m
    if ! echo "$loop_pid" > "$OUT.pid" || ! capture_adopt "$OUT" "$NS" "$loop_pid"; then
        echo "cannot record the capture at $DIR -- stopping it rather than running unowned" >&2
        kill -TERM "-$loop_pid" 2>/dev/null || kill "$loop_pid" 2>/dev/null || true
        capture_release "$OUT"
        exit 1
    fi
    echo "learner recordings: copying every ${INTERVAL}s (pid $loop_pid, context ${CAPTURE_CTX:-inherited}," \
         "namespace $NS) -> $DIR"
    ;;
  stop)
    [ "$#" -eq 3 ] || usage
    NS="$2"; DIR="$3"; OUT="$(_index "$DIR")"
    rc=0
    if capture_owned "$OUT"; then
        capture_check_owner "$OUT" "$NS" "$CAPTURE_CTX"
        # capture_check_owner treats an unnamed context as "not stated". Here
        # it matters: the final copy below would read whatever KUBECONFIG says,
        # which may be another cluster than the one the run copied from.
        owner_ctx="$(sed -n 's/.* context=\([^ ]*\) .*/\1/p' "$OUT.owner" 2>/dev/null)"
        if [ -z "$CAPTURE_CTX" ] && [ -n "$owner_ctx" ] && [ "$owner_ctx" != "<inherited" ]; then
            echo "warning: the copy into $DIR was started with --context $owner_ctx, and this stop names none;" >&2
            echo "         the final copy reads the cluster KUBECONFIG names" >&2
        fi
        touch "$OUT.stop"
        # A capture that will not die is reported, and the final snapshot is
        # still taken: the per-Pod lock and the larger-only rule make a copy
        # that runs beside it harmless.
        capture_stop "$OUT" "$OUT.pid" || rc=1
        rm -f "$OUT.stop"
        [ "$rc" -eq 0 ] && capture_finish "$OUT"
    else
        echo "learner recordings: no background copy owns $DIR; taking the final snapshot anyway"
    fi
    # A stopped copy can leave its temporary directory behind.
    [ "$rc" -eq 0 ] && rm -rf "$DIR"/.incoming.* 2>/dev/null
    snapshot_once "$NS" "$DIR" || true
    exit "$rc"
    ;;
  stop-all)
    [ "$#" -eq 3 ] || usage
    NS="$2"; PREFIX="$3"; stopped=0
    for d in "$PREFIX"*; do
        [ -d "$d" ] || continue
        capture_owned "$(_index "$d")" || continue
        case "$(cat "$(_index "$d").owner" 2>/dev/null)" in
            *"namespace=$NS "*) : ;;
            *) continue ;;
        esac
        echo "learner recordings: stopping the copy into $d"
        bash "$SELF" ${CAPTURE_CTX:+--context "$CAPTURE_CTX"} stop "$NS" "$d" || true
        stopped=$((stopped + 1))
    done
    if [ "$stopped" -eq 0 ]; then
        echo "learner recordings: no background copy of namespace $NS under $PREFIX*"
        exit 1
    fi
    exit 0
    ;;
  *)
    usage ;;
esac
