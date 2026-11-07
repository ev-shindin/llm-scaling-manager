#!/usr/bin/env bash
# Executes hack/benchmark/learner_recordings.sh, file_capture.sh and the
# two-model arm cleanup against a stub kubectl, and asserts the benchmark keeps
# the learner recordings on every exit path. The Makefile recipes and the
# two-model arm start are not executed: the "harness wiring" cases read them
# and assert each calls the script, in the right place.
#
# The recorder writes to an emptyDir. Before this, nothing in the harness
# copied it, so every teardown deleted the run's recordings and learnreplay had
# nothing to read unless someone ran `kubectl cp` by hand at the right moment.
#
# The stub serves controller Pods built from STUB_PODS
# (name,phase,recording-dir,sidecar;...) plus a relay Pod that carries the
# controller labels and must never be copied from, and answers `exec ... tar`
# with a tar of $STUB_SRC/<pod>/. `bash -n` sees none of this.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH="$ROOT/hack/benchmark"
LR="$BENCH/learner_recordings.sh"
EXPECTED_CASES=68

command -v python3 >/dev/null 2>&1 || {
    echo "FATAL: python3 is required by this check and is missing. Nothing below would be about the code."
    exit 2
}
for tool in timeout flock pgrep; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check (the hang, lock and orphan cases need it) and is missing."
        exit 2
    }
done

WORK="$(mktemp -d)"
STARTED=""
cleanup() {
    # TERM, then KILL: a mutant whose loop ignores TERM must not outlive the
    # check (one ran on in the background for hours).
    for p in $STARTED; do
        kill -TERM "-$p" 2>/dev/null || kill -TERM "$p" 2>/dev/null || true
    done
    sleep 0.3
    for p in $STARTED; do
        kill -KILL "-$p" 2>/dev/null || kill -KILL "$p" 2>/dev/null || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

fails=0
cases=0
ok()  { cases=$((cases + 1)); echo "  ok: $1"; }
bad() { cases=$((cases + 1)); fails=$((fails + 1)); echo "  FAIL: $1" >&2; }

STUB="$WORK/bin"
mkdir -p "$STUB"
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env python3
import io, json, os, sys, tarfile, time

args = sys.argv[1:]
with open(os.environ.get("KCALLS", os.devnull), "a") as f:
    f.write(" ".join(args) + "\n")
# --context and --request-timeout may come anywhere; drop them.
clean, i = [], 0
while i < len(args):
    a = args[i]
    if a == "--context":
        i += 2; continue
    if a.startswith("--request-timeout"):
        i += 1; continue
    clean.append(a); i += 1

def pods():
    items = [{"metadata": {"name": "relay", "labels": {"control-plane": "controller-manager"}},
              "spec": {"containers": [{"name": "manager", "env": [
                  {"name": "LEARNER_RECORDING_DIR", "value": "/rec"}]},
                  {"name": "recordings"}]},
              "status": {"phase": "Running"}}]
    spec = os.environ.get("STUB_PODS", "")
    for n, entry in enumerate(filter(None, spec.split(";"))):
        name, phase, recdir, sidecar = entry.split(",")
        h = "h%d" % n
        mgr = {"name": "manager"}
        if recdir != "-":
            mgr["env"] = [{"name": "LEARNER_RECORDING_DIR", "value": recdir}]
        ctrs = [mgr] + ([{"name": "recordings"}] if sidecar == "yes" else [])
        items.append({"metadata": {"name": name,
                                   "labels": {"control-plane": "controller-manager", "pod-template-hash": h},
                                   "ownerReferences": [{"kind": "ReplicaSet", "controller": True,
                                                        "name": "wva-controller-manager-" + h}]},
                      "spec": {"containers": ctrs}, "status": {"phase": phase}})
    return {"items": items}

if clean[:2] == ["get", "pods"]:
    if os.environ.get("STUB_BAD_JSON"):
        print("<html>502 Bad Gateway</html>")
        sys.exit(0)
    if os.environ.get("STUB_LIST_FAIL"):
        print('Error from server (Forbidden): pods is forbidden: User "x" cannot list resource "pods"', file=sys.stderr)
        sys.exit(1)
    print(json.dumps(pods()))
    sys.exit(0)

if clean and clean[0] == "exec":
    pod, j, rest = None, 1, clean[1:]
    while j < len(clean):
        a = clean[j]
        if a == "--":
            break
        if a in ("-n", "-c"):
            j += 2; continue
        if pod is None and not a.startswith("-"):
            pod = a
        j += 1
    with open(os.environ.get("KEXEC", os.devnull), "a") as f:
        f.write(pod + "\n")
    if os.environ.get("STUB_EXEC_HANG"):
        # Named, so the check can find a copy that outlived its stop.
        os.execvp("sleep", ["sleep", os.environ["STUB_EXEC_HANG"]])
    if os.environ.get("STUB_EXEC_FAIL"):
        print("error: unable to upgrade connection: container not found", file=sys.stderr)
        sys.exit(1)
    if "tar" not in clean:
        sys.exit(2)
    src = os.path.join(os.environ["STUB_SRC"], pod)
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as t:
        if os.path.isdir(src):
            t.add(src, arcname=".")
    sys.stdout.buffer.write(buf.getvalue())
    sys.exit(0)

print("stub: unexpected kubectl " + " ".join(args), file=sys.stderr)
sys.exit(1)
STUBEOF
chmod +x "$STUB/kubectl"
export KUBECTL_CMD="$STUB/kubectl"
export KCALLS="$WORK/calls.log"
export KEXEC="$WORK/exec.log"
export STUB_SRC="$WORK/src"
mkdir -p "$STUB_SRC"

A="wva-controller-manager-h0-a1"
REC="learnrec-20261101T090000.000000000Z.jsonl"

# grow <pod> <lines> -- the recording in the Pod has that many lines.
grow() {
    mkdir -p "$STUB_SRC/$1"
    : > "$STUB_SRC/$1/$REC"
    local i=0
    while [ "$i" -lt "$2" ]; do
        printf '{"seq":%d,"kind":"Analyze"}\n' "$i" >> "$STUB_SRC/$1/$REC"
        i=$((i + 1))
    done
}
lines() { wc -l < "$1" 2>/dev/null | tr -d ' ' || echo 0; }
reset_calls() { : > "$KCALLS"; : > "$KEXEC"; }

snap() { bash "$LR" snapshot demo-ns "$1" > "$WORK/o" 2> "$WORK/e"; }

echo "learner_recordings.sh snapshot"
# ---- a Running Pod with the recorder on is copied, and only it ------------
export STUB_PODS="$A,Running,/var/lib/wva/learner-recordings,yes"
grow "$A" 5
D="$WORK/rec1"
reset_calls
if snap "$D"; then ok "a snapshot of a recording Pod exits 0"; else bad "snapshot exited $?: $(cat "$WORK/o" "$WORK/e" | head -3)"; fi
cmp -s "$STUB_SRC/$A/$REC" "$D/$A/$REC" \
    && ok "the copy is byte for byte the Pod's recording" \
    || bad "no faithful copy at $D/$A/$REC"
grep -q relay "$KEXEC" \
    && bad "the relay Pod, which only carries the controller labels, was copied from" \
    || ok "only the controller's own Pod is copied from"
grep 'get pods' "$KCALLS" | grep -q -- "--request-timeout=" \
    && ok "the Pod list carries --request-timeout" \
    || bad "the Pod list has no --request-timeout: $(grep 'get pods' "$KCALLS" | head -1)"
grep -q -- "exec .*-c recordings -- tar cf - -C /var/lib/wva/learner-recordings ." "$KCALLS" \
    && ok "it copies through the recordings sidecar, from the directory the manager was given" \
    || bad "the exec was not through the sidecar: $(grep exec "$KCALLS" | head -1)"
row="$(awk -F'\t' -v p="$A" '$2 == p' "$D/collection.tsv" | tail -1)"
case "$row" in
    20[0-9][0-9]-*T*Z"	$A	Running	copied	1	"*) ok "collection.tsv records when, which Pod, its phase and what was copied" ;;
    *) bad "collection.tsv has no row for the copy: ${row:-$(cat "$D/collection.tsv")}" ;;
esac
grep -q "1 file(s), [0-9]* bytes held in $D" "$WORK/o" \
    && ok "the output says how much is held and where" \
    || bad "no summary line: $(cat "$WORK/o")"

# ---- a copy never shrinks the stored one -----------------------------------
before="$(cksum < "$D/$A/$REC")"
grow "$A" 2
snap "$D"
[ "$(cksum < "$D/$A/$REC")" = "$before" ] \
    && ok "a smaller copy (a truncated stream) does not replace the larger stored one" \
    || bad "the stored copy shrank to $(lines "$D/$A/$REC") lines"
grep -q "1 smaller copy refused" "$WORK/o" \
    && ok "the refused smaller copy is reported" \
    || bad "the refusal is silent: $(cat "$WORK/o")"
grow "$A" 9
snap "$D"
[ "$(lines "$D/$A/$REC")" = 9 ] \
    && ok "a grown recording replaces the stored copy" \
    || bad "the grown recording was not taken: $(lines "$D/$A/$REC") lines"
ls -a "$D" | grep -q '^\.incoming' \
    && bad "a temporary directory was left behind: $(ls -a "$D")" \
    || ok "no temporary copy is left behind"

# ---- two copies of one Pod do not interleave compare and rename ------------
# Another copy holds the Pod's lock for 3 s; this one must wait for it rather
# than compare sizes while the other is between its compare and its rename.
DL="$WORK/lock"; mkdir -p "$DL"
( flock 9 && sleep 3 ) 9>"$DL/.$A.lock" &
holder=$!
sleep 0.5
t0="$(date +%s)"
snap "$DL"
t1="$(date +%s)"
wait "$holder" 2>/dev/null
[ $((t1 - t0)) -ge 2 ] && [ "$(lines "$DL/$A/$REC")" = 9 ] \
    && ok "a copy waits for the Pod's lock held by another copy ($((t1 - t0))s for a 3s hold), then installs" \
    || bad "a copy did not wait for the Pod's lock: $((t1 - t0))s, $(lines "$DL/$A/$REC") lines"

# ---- a Pod that is not Running keeps its last copy, and is not exec-ed ----
export STUB_PODS="$A,Failed,/var/lib/wva/learner-recordings,yes"
grow "$A" 1
reset_calls
snap "$D"; rc=$?
[ -s "$KEXEC" ] && bad "kubectl exec was tried on a Failed Pod" || ok "no exec against a Pod that is not Running"
[ "$(lines "$D/$A/$REC")" = 9 ] && ok "the previous copy of a Failed Pod is kept" || bad "the copy changed to $(lines "$D/$A/$REC") lines"
grep -q "is Failed; kubectl exec needs a Running Pod" "$WORK/o" \
    && ok "the output says why the Pod was not copied" \
    || bad "no reason given: $(cat "$WORK/o")"
[ "$rc" -eq 0 ] && ok "a snapshot that holds an earlier copy still exits 0" || bad "exit $rc with a copy held"

# ---- a failed and a hung exec keep the previous copy ----------------------
export STUB_PODS="$A,Running,/var/lib/wva/learner-recordings,yes"
STUB_EXEC_FAIL=1 snap "$D"
[ "$(lines "$D/$A/$REC")" = 9 ] && ok "a failed exec keeps the previous copy" || bad "a failed exec changed the copy"
grep -q "copy-failed (exit 1: error: unable to upgrade connection" "$WORK/o" \
    && ok "a failed exec is reported with its error" \
    || bad "the failed exec is not reported: $(cat "$WORK/o")"
t0="$(date +%s)"
STUB_EXEC_HANG=30 RECORDINGS_COPY_TIMEOUT=2 snap "$D"
t1="$(date +%s)"
[ $((t1 - t0)) -lt 15 ] && ok "a hung exec is cut off by RECORDINGS_COPY_TIMEOUT ($((t1 - t0))s for a 2s bound)" || bad "a hung exec ran $((t1 - t0))s"
grep -q "timed out after 2s" "$WORK/o" && ok "the timeout is named" || bad "the timeout is not named: $(cat "$WORK/o")"

# ---- no recordings is said, never implied ----------------------------------
E="$WORK/empty1"
export STUB_PODS="$A,Running,-,no"
snap "$E"; rc=$?
[ "$rc" -eq 1 ] && ok "a recorder that is off exits 1" || bad "a recorder that is off exited $rc"
grep -q "NONE held in $E .*recorder off" "$WORK/o" \
    && ok "a recorder that is off is named in the NONE line" \
    || bad "no NONE line naming the recorder: $(cat "$WORK/o")"
export STUB_PODS="$A,Running,/var/lib/wva/learner-recordings,no"
snap "$WORK/empty2"
grep -q "no recordings sidecar" "$WORK/o" \
    && ok "a recording Pod without the sidecar is named, not skipped" \
    || bad "the missing sidecar is not named: $(cat "$WORK/o")"
export STUB_PODS=""
snap "$WORK/empty3"; rc=$?
[ "$rc" -eq 1 ] && grep -q "NONE held .*no controller Pod in namespace demo-ns" "$WORK/o" \
    && ok "no controller Pod is a NONE line and exit 1" \
    || bad "no controller Pod: exit $rc, $(cat "$WORK/o")"
export STUB_PODS="$A,Running,/var/lib/wva/learner-recordings,yes"
mkdir -p "$STUB_SRC/emptypod"
rm -rf "${STUB_SRC:?}/$A"; mkdir -p "$STUB_SRC/$A"
snap "$WORK/empty4"; rc=$?
[ "$rc" -eq 1 ] && grep -q "NONE held" "$WORK/o" \
    && ok "an empty recording directory is a NONE line and exit 1" \
    || bad "an empty recording directory: exit $rc, $(cat "$WORK/o")"
STUB_LIST_FAIL=1 snap "$WORK/empty5"; rc=$?
[ "$rc" -eq 1 ] && grep -q "could not list the controller Pods in demo-ns: Error from server (Forbidden)" "$WORK/o" \
    && ok "a refused Pod list is reported as refused, not as no controller" \
    || bad "a refused Pod list: exit $rc, $(cat "$WORK/o")"
STUB_BAD_JSON=1 snap "$WORK/empty6"; rc=$?
[ "$rc" -eq 1 ] && grep -q "could not read the controller Pod list (controller_pods.py exit 1" "$WORK/o" \
    && ! grep -q "no controller Pod in namespace" "$WORK/o" \
    && ok "a Pod list the helper cannot read is named as such, not as no controller" \
    || bad "an unreadable Pod list: exit $rc, $(cat "$WORK/o")"

echo "learner_recordings.sh start/stop"
# ---- the background copy runs during the run, stop takes the final one ----
grow "$A" 3
D2="$WORK/run/wva_learner_recordings-demo-ns-1"
mkdir -p "$WORK/run"
if RECORDINGS_INTERVAL=1 bash "$LR" start demo-ns "$D2" > "$WORK/o" 2>&1; then
    ok "start exits 0"
else
    bad "start failed: $(cat "$WORK/o")"
fi
PID="$(cat "$D2/collection.tsv.pid" 2>/dev/null || true)"
[ -n "$PID" ] && STARTED="$STARTED $PID"
waited=0
while [ ! -f "$D2/$A/$REC" ] && [ "$waited" -lt 50 ]; do sleep 0.2; waited=$((waited + 1)); done
[ -f "$D2/$A/$REC" ] \
    && ok "a copy is on disk while the run is still going" \
    || bad "no copy during the run: $(cat "$D2/capture.log" 2>/dev/null | head -3)"
if RECORDINGS_INTERVAL=1 bash "$LR" start demo-ns "$D2" > "$WORK/o" 2>&1; then
    bad "a second start took a live capture"
else
    ok "a second start on a live capture is refused"
fi
grow "$A" 7
bash "$LR" stop demo-ns "$D2" > "$WORK/o" 2>&1; rc=$?
[ "$rc" -eq 0 ] && ok "stop exits 0" || bad "stop exited $rc: $(cat "$WORK/o")"
[ "$(lines "$D2/$A/$REC")" = 7 ] \
    && ok "stop takes the final copy, after the last periodic one" \
    || bad "the final copy is missing: $(lines "$D2/$A/$REC") lines"
sleep 0.5
if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    bad "the background copy (pid $PID) outlived its stop"
else
    ok "the background copy is gone after stop"
fi
[ ! -e "$D2/collection.tsv.owner" ] && ok "stop drops the ownership record" || bad "the record is left: $(cat "$D2/collection.tsv.owner")"

# ---- a stop with no loop still takes the final copy -----------------------
D3="$WORK/run/norun"
bash "$LR" stop demo-ns "$D3" > "$WORK/o" 2>&1
[ "$(lines "$D3/$A/$REC")" = 7 ] && grep -q "taking the final snapshot anyway" "$WORK/o" \
    && ok "a stop whose start failed still takes the final copy" \
    || bad "no final copy without a loop: $(cat "$WORK/o")"

# ---- a final copy that finds no Pod keeps the earlier copy -----------------
STUB_PODS="" bash "$LR" stop demo-ns "$D3" > "$WORK/o" 2>&1
[ "$(lines "$D3/$A/$REC")" = 7 ] && grep -q "1 file(s), [0-9]* bytes held in $D3" "$WORK/o" \
    && ok "a stop after the controller is gone keeps the copy taken before" \
    || bad "a stop with no controller Pod lost the earlier copy: $(lines "$D3/$A/$REC") lines, $(cat "$WORK/o")"

# ---- start will not mix runs in one directory unless told to ---------------
if bash "$LR" start demo-ns "$D3" > "$WORK/o" 2>&1; then
    bad "start took a directory holding a previous run's copies"
    bash "$LR" stop demo-ns "$D3" > /dev/null 2>&1
else
    grep -q "already holds 1 recording copies of a previous run" "$WORK/o" \
        && ok "start refuses a directory that holds a previous run's copies" \
        || bad "start failed for another reason: $(cat "$WORK/o")"
fi
[ ! -e "$D3/collection.tsv.owner" ] && ok "the refused start leaves no ownership record" || bad "the refused start left $(cat "$D3/collection.tsv.owner")"
if RECORDINGS_INTERVAL=60 bash "$LR" --force start demo-ns "$D3" > "$WORK/o" 2>&1; then
    ok "start --force adds to a directory that holds copies"
    STARTED="$STARTED $(cat "$D3/collection.tsv.pid" 2>/dev/null)"
    bash "$LR" stop demo-ns "$D3" > /dev/null 2>&1
else
    bad "start --force refused: $(cat "$WORK/o")"
fi

# ---- a stop reaches the copy in flight -------------------------------------
# The exec hangs; timeout used to put it in a process group of its own, so the
# stop's group signal missed it and it ran on after the stop said it was done.
HANGV="47.$$"
D6="$WORK/run/hang"
reset_calls
STUB_EXEC_HANG="$HANGV" RECORDINGS_COPY_TIMEOUT=120 RECORDINGS_INTERVAL=1 \
    bash "$LR" start demo-ns "$D6" > "$WORK/o" 2>&1
STARTED="$STARTED $(cat "$D6/collection.tsv.pid" 2>/dev/null)"
waited=0
while ! pgrep -f "sleep $HANGV" >/dev/null 2>&1 && [ "$waited" -lt 50 ]; do sleep 0.2; waited=$((waited + 1)); done
pgrep -f "sleep $HANGV" >/dev/null 2>&1 \
    && ok "the background copy has an exec in flight" \
    || bad "no exec in flight to stop: $(cat "$D6/capture.log" 2>/dev/null | head -3)"
bash "$LR" stop demo-ns "$D6" > "$WORK/o" 2>&1; rc=$?
sleep 0.5
left="$(pgrep -f "sleep $HANGV" | tr '\n' ' ')"
if [ -z "$left" ] && [ "$rc" -eq 0 ]; then
    ok "no exec outlives the stop"
else
    bad "the stop (exit $rc) left the exec running: pid(s) $left"
    for p in $left; do kill "$p" 2>/dev/null; done
fi

# ---- a stop that names no context, of a copy that named one, warns --------
D7="$WORK/run/ctx"
RECORDINGS_INTERVAL=60 bash "$LR" --context c1 start demo-ns "$D7" > /dev/null 2>&1
STARTED="$STARTED $(cat "$D7/collection.tsv.pid" 2>/dev/null)"
bash "$LR" stop demo-ns "$D7" > "$WORK/o" 2> "$WORK/e"
grep -q "was started with --context c1, and this stop names none" "$WORK/e" \
    && ok "a stop that names no context warns when the copy was started with one" \
    || bad "no warning for a stop without the copy's context: $(cat "$WORK/e")"
# A new directory: D7 now holds copies, and start would refuse it.
D7="$WORK/run/ctx2"
reset_calls
RECORDINGS_INTERVAL=60 bash "$LR" --context c1 start demo-ns "$D7" > /dev/null 2>&1
STARTED="$STARTED $(cat "$D7/collection.tsv.pid" 2>/dev/null)"
waited=0
while ! grep -q . "$KEXEC" && [ "$waited" -lt 50 ]; do sleep 0.2; waited=$((waited + 1)); done
bash "$LR" --context c1 stop demo-ns "$D7" > "$WORK/o" 2> "$WORK/e"
grep -q "warning" "$WORK/e" \
    && bad "a stop that names the same context warned: $(cat "$WORK/e")" \
    || ok "a stop that names the copy's context does not warn"
# Every call, the background copy's and the final one's, names the cluster.
n_exec="$(grep -c '^--context c1 .*exec ' "$KCALLS")"
n_list="$(grep -c '^--context c1 .*get pods' "$KCALLS")"
n_bare="$(grep -E 'exec |get pods' "$KCALLS" | grep -vc -- '--context c1')"
[ "$n_exec" -ge 2 ] && [ "$n_list" -ge 2 ] && [ "$n_bare" -eq 0 ] \
    && ok "the background copy and the final one both pass --context to kubectl ($n_list lists, $n_exec execs)" \
    || bad "a call went without --context c1: $n_list lists and $n_exec execs with it, $n_bare without: $(cat "$KCALLS")"

# ---- the background copy ends itself at RECORDINGS_MAX_SECONDS ------------
D8="$WORK/run/deadline"
RECORDINGS_MAX_SECONDS=1 RECORDINGS_INTERVAL=1 bash "$LR" start demo-ns "$D8" > /dev/null 2>&1
P8="$(cat "$D8/collection.tsv.pid" 2>/dev/null || true)"; STARTED="$STARTED $P8"
waited=0
while [ -n "$P8" ] && kill -0 "$P8" 2>/dev/null && [ "$waited" -lt 50 ]; do sleep 0.2; waited=$((waited + 1)); done
if [ -n "$P8" ] && ! kill -0 "$P8" 2>/dev/null && grep -q "ran for 1s and stopped" "$D8/capture.log"; then
    ok "the background copy stops itself after RECORDINGS_MAX_SECONDS and says so"
else
    bad "the background copy outlived RECORDINGS_MAX_SECONDS=1: $(cat "$D8/capture.log" 2>/dev/null | tail -2)"
fi
bash "$LR" stop demo-ns "$D8" > /dev/null 2>&1

# ---- a missing tool is a WARNING, once, not a silent downgrade ------------
printf '#!/usr/bin/env bash\n[ "$1" = --foreground ] && exit 125\nexec timeout "$@"\n' > "$STUB/oldtimeout"
chmod +x "$STUB/oldtimeout"
RECORDINGS_TIMEOUT_CMD="$STUB/oldtimeout" snap "$WORK/tools1"
[ "$(grep -c "WARNING: .*has no --foreground" "$WORK/e")" -eq 1 ] \
    && ok "a timeout without --foreground is one WARNING" \
    || bad "no single --foreground WARNING: $(cat "$WORK/e")"
RECORDINGS_TIMEOUT_CMD=no-such-timeout snap "$WORK/tools2"
[ "$(grep -c "WARNING: .*no no-such-timeout command; a copy is not bounded" "$WORK/e")" -eq 1 ] \
    && ok "no timeout at all is one WARNING" \
    || bad "no single missing-timeout WARNING: $(cat "$WORK/e")"
# Two files in the Pod, so a per-file warning would print twice.
cp "$STUB_SRC/$A/$REC" "$STUB_SRC/$A/learnrec-20261101T100000.000000000Z.jsonl"
RECORDINGS_FLOCK_CMD=no-such-flock snap "$WORK/tools3"
rm -f "$STUB_SRC/$A/learnrec-20261101T100000.000000000Z.jsonl"
[ "$(grep -c "WARNING: .*no no-such-flock command; two copies of one Pod run unlocked" "$WORK/e")" -eq 1 ] \
    && [ -s "$WORK/tools3/$A/$REC" ] \
    && ok "no flock is one WARNING, and the copy is still taken" \
    || bad "no single missing-flock WARNING: $(cat "$WORK/e")"
snap "$WORK/tools4"
grep -q WARNING "$WORK/e" && bad "a WARNING with every tool present: $(cat "$WORK/e")" || ok "no WARNING with every tool present"

# ---- a lock that cannot be taken installs nothing, and says so -------------
DLF="$WORK/lockfail"; mkdir -p "$DLF/.$A.lock"
snap "$DLF"; rc=$?
[ ! -e "$DLF/$A/$REC" ] && [ "$rc" -eq 1 ] && grep -q "lock-failed (.*1 not installed" "$WORK/o" \
    && awk -F'\t' -v p="$A" '$2 == p && $4 == "lock-failed"' "$DLF/collection.tsv" | grep -q . \
    && ok "a lock that cannot be opened is reported as lock-failed, not as unchanged" \
    || bad "a failed lock: exit $rc, $(cat "$WORK/o")"

# ---- usage errors exit 2 ---------------------------------------------------
u=0
bash "$LR" snapshot demo-ns > /dev/null 2>&1; [ $? -eq 2 ] && u=$((u + 1))
bash "$LR" frobnicate demo-ns "$WORK/x" > /dev/null 2>&1; [ $? -eq 2 ] && u=$((u + 1))
bash "$LR" --bogus snapshot demo-ns "$WORK/x" > /dev/null 2>&1; [ $? -eq 2 ] && u=$((u + 1))
RECORDINGS_INTERVAL=00 bash "$LR" snapshot demo-ns "$WORK/x" > /dev/null 2>&1; [ $? -eq 2 ] && u=$((u + 1))
RECORDINGS_COPY_TIMEOUT=1x bash "$LR" snapshot demo-ns "$WORK/x" > /dev/null 2>&1; [ $? -eq 2 ] && u=$((u + 1))
[ "$u" -eq 5 ] \
    && ok "a missing argument, an unknown command, an unknown flag, 00 and 1x each exit 2" \
    || bad "only $u of 5 usage errors exited 2"

# ---- an interrupted run: teardown finds its copy and finishes it ----------
# The second capture's namespace, demo-ns-2, shares the demo-ns- prefix, so
# only the namespace in its ownership record keeps stop-all off it.
D4="$WORK/run/wva_learner_recordings-demo-ns-2"
D5="$WORK/run/wva_learner_recordings-demo-ns-2-3"
RECORDINGS_INTERVAL=60 bash "$LR" start demo-ns "$D4" > /dev/null 2>&1
P4="$(cat "$D4/collection.tsv.pid" 2>/dev/null || true)"; STARTED="$STARTED $P4"
RECORDINGS_INTERVAL=60 bash "$LR" start demo-ns-2 "$D5" > /dev/null 2>&1
P5="$(cat "$D5/collection.tsv.pid" 2>/dev/null || true)"; STARTED="$STARTED $P5"
# Both background copies have taken their first copy before the calls are
# reset, so what follows is stop-all's own.
waited=0
while { ! grep -q "held in" "$D4/capture.log" 2>/dev/null || ! grep -q "held in" "$D5/capture.log" 2>/dev/null; } \
    && [ "$waited" -lt 50 ]; do sleep 0.2; waited=$((waited + 1)); done
grow "$A" 11
reset_calls
bash "$LR" --context c9 stop-all demo-ns "$WORK/run/wva_learner_recordings-demo-ns-" > "$WORK/o" 2>&1; rc=$?
n_list="$(grep -c '^--context c9 .*get pods' "$KCALLS")"
n_bare="$(grep -E 'exec |get pods' "$KCALLS" | grep -vc -- '--context c9')"
[ "$n_list" -ge 1 ] && [ "$n_bare" -eq 0 ] \
    && ok "stop-all passes its --context to the stop it runs" \
    || bad "stop-all dropped --context c9: $n_list lists with it, $n_bare calls without: $(cat "$KCALLS")"
[ "$rc" -eq 0 ] && [ "$(lines "$D4/$A/$REC")" = 11 ] \
    && ok "stop-all takes the final copy of an interrupted run's capture" \
    || bad "stop-all: exit $rc, $(lines "$D4/$A/$REC") lines, $(head -3 "$WORK/o")"
kill -0 "$P4" 2>/dev/null && bad "stop-all left the interrupted run's copy running" || ok "stop-all stops the interrupted run's copy"
kill -0 "$P5" 2>/dev/null && ok "stop-all leaves another namespace's copy alone, under the same prefix" || bad "stop-all stopped namespace demo-ns-2's copy"
bash "$LR" stop demo-ns-2 "$D5" > /dev/null 2>&1
bash "$LR" stop-all demo-ns "$WORK/run/wva_learner_recordings-demo-ns-" > "$WORK/o" 2>&1; rc=$?
[ "$rc" -eq 1 ] && grep -q "no background copy of namespace demo-ns" "$WORK/o" \
    && ok "stop-all with nothing running says so and exits 1, so the teardown copies itself" \
    || bad "stop-all with nothing running: exit $rc, $(cat "$WORK/o")"

echo "file_capture.sh"
# ---- the recordings are filed with the results -----------------------------
R="$WORK/results/guidellm-1_1"; mkdir -p "$R"
bash "$BENCH/file_capture.sh" "$R" "$WORK/none.json" "$WORK/none.log" "$D2" > "$WORK/o" 2>&1
cmp -s "$D2/$A/$REC" "$R/learner-recordings/$A/$REC" && [ -s "$R/learner-recordings/collection.tsv" ] \
    && ok "the recordings and collection.tsv are filed under results/learner-recordings" \
    || bad "the recordings were not filed: $(cat "$WORK/o")"
R2="$WORK/results/guidellm-2_2"; mkdir -p "$R2"
bash "$BENCH/file_capture.sh" "$R2" "$WORK/none.json" "$WORK/none.log" "$WORK/empty1" > "$WORK/o" 2>&1
grep -q "NO learner recordings to file" "$WORK/o" \
    && ok "a run with no recordings says so when filing" \
    || bad "filing no recordings is silent: $(cat "$WORK/o")"
R3="$WORK/results/guidellm-3_3"; mkdir -p "$R3"
touch "$D2.startfailed"
bash "$BENCH/file_capture.sh" "$R3" "$WORK/none.json" "$WORK/none.log" "$D2" > "$WORK/o" 2>&1
rm -f "$D2.startfailed"
grep -q "not filing the learner recordings: their capture never started" "$WORK/o" && [ ! -e "$R3/learner-recordings/$A" ] \
    && ok "recordings whose capture never started are withheld, and the output says where they are" \
    || bad "recordings were filed although their capture never started: $(cat "$WORK/o")"

echo "harness wiring"
# ---- every exit path of the harness calls it -------------------------------
MK="$(tr -d '\r' < "$ROOT/Makefile")"
# body <target> -- the recipe of one target.
body() { printf '%s\n' "$MK" | awk -v t="$1:" 'index($0, t) == 1 { on = 1; next } on && /^[^\t]/ { on = 0 } on { print }'; }
run="$(body benchmark-run)"
order="$(printf '%s\n' "$run" | grep -n -e 'learner_recordings.sh.* start ' -e 'LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) run' -e 'learner_recordings.sh.* stop ' -e 'file_capture.sh' | cut -d: -f2- | sed -e 's/.*learner_recordings.sh.* start .*/start/' -e 's/.*LLMDBENCHMARK.*/run/' -e 's/.*learner_recordings.sh.* stop .*/stop/' -e 's/.*file_capture.sh.*/file/' | tr '\n' ' ')"
[ "$order" = "start run stop file " ] \
    && ok "benchmark-run starts the copy before the run, stops it after, then files it" \
    || bad "benchmark-run order is [$order]"
printf '%s\n' "$run" | grep -q -- '^	-$(LLMDBENCHMARK)' \
    && ok "the run keeps its '-', so a failed or timed-out run still reaches the stop" \
    || bad "the run lost its '-': a failure would skip the final copy"
printf '%s\n' "$run" | grep -q 'file_capture.sh .*"$(BENCHMARK_RECORDINGS)"' \
    && ok "benchmark-run hands the recordings to file_capture.sh" \
    || bad "file_capture.sh is not given the recordings"
td="$(body benchmark-teardown)"
order="$(printf '%s\n' "$td" | grep -n -e 'learner_recordings.sh.* stop-all ' -e 'BENCHMARK_WVA_UNDEPLOY_TARGET' -e 'LLMDBENCHMARK).*teardown' | cut -d: -f1 | tr '\n' ' ')"
set -- $order
[ "$#" -eq 3 ] && [ "$1" -lt "$2" ] && [ "$2" -lt "$3" ] \
    && ok "benchmark-teardown copies (stop-all, else a snapshot) before anything is removed" \
    || bad "benchmark-teardown: no copy ahead of the undeploy and teardown (lines: $order)"
rs="$(body benchmark-restart-controller)"
order="$(printf '%s\n' "$rs" | grep -n -e 'learner_recordings.sh.* snapshot ' -e 'scale $(WVA_CONTROLLER_DEPLOY) --replicas=0' | cut -d: -f1 | tr '\n' ' ')"
set -- $order
[ "$#" -eq 2 ] && [ "$1" -lt "$2" ] \
    && ok "benchmark-restart-controller copies before it scales the controller to 0" \
    || bad "benchmark-restart-controller: no copy before the scale to 0 (lines: $order)"
ra="$(body benchmark-run-all)"
printf '%s\n' "$ra" | grep -q "trap 'rec .*exit 130' INT TERM" \
    && [ "$(printf '%s\n' "$ra" | grep -c 'rec "$$scenario_name"')" -eq 3 ] \
    && ok "benchmark-run-all copies after each run, on a failed standup or run, and on an interrupt (which exits)" \
    || bad "benchmark-run-all is missing a copy: $(printf '%s\n' "$ra" | grep -c 'rec "$$scenario_name"') per-scenario copies"

# Continued lines joined, so a call split over two lines reads as one.
tm="$(sed -n '/^verb_run()/,/^}/p' "$BENCH/two_model_pool.sh" | awk '{ if (sub(/\\$/, "")) printf "%s", $0; else print }')"
printf '%s\n' "$tm" | grep -q 'learner_recordings.sh.* start "$WVA_NS" "$RUN_RECDIR"' \
    && ok "a two-model arm starts a periodic copy of the recordings" \
    || bad "a two-model arm takes no periodic copy"

# ---- the two-model arm copies on its cleanup, once ------------------------
reset_calls
# The script reads its verb from $1 when sourced, as when run: clear what the
# wiring checks above left there.
set --
out="$(
    # shellcheck disable=SC1091
    . "$BENCH/two_model_pool.sh" >/dev/null 2>&1
    NS=demo-ns; WVA_NS=demo-ns; KUBE_CONTEXT=""
    PATH="$STUB:$PATH"; unset KUBECTL_CMD
    RUN_RECDIR="$WORK/arm/learner-recordings"
    run_cleanup 2>&1
    run_cleanup 2>&1
    echo "RECDIR=[$RUN_RECDIR]"
)"
[ "$(lines "$WORK/arm/learner-recordings/$A/$REC")" = 11 ] \
    && ok "the two-model arm cleanup (every exit: done, die, interrupt) copies the recordings" \
    || bad "the two-model arm cleanup copied nothing: $out"
[ "$(grep -c . "$KEXEC")" -eq 1 ] \
    && ok "a second cleanup (the EXIT trap after a clean arm) does not copy again" \
    || bad "the arm cleanup copied $(grep -c . "$KEXEC") times"

# ---- the count itself ------------------------------------------------------
if [ "$cases" -eq "$EXPECTED_CASES" ]; then
    echo "  ok: all $EXPECTED_CASES cases ran"
else
    echo "  FAIL: expected $EXPECTED_CASES cases, ran $cases -- a block stopped running" >&2
    fails=$((fails + 1))
fi

echo "learner recordings checks: $cases run, $fails failed"
[ "$fails" -eq 0 ] || exit 1
