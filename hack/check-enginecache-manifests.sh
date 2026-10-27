#!/usr/bin/env bash
# What deploy/enginecache.sh and hack/benchmark/engine_cache_claim.sh actually
# EMIT, and what they refuse.
#
# A hostPath volume is one character away from handing every pod in a
# namespace root write on the node, a claim without a claimRef is taken by the
# first claim that asks for its class, and a preparer that requests a GPU or
# carries a `$(` in its command parses fine and ships wrong. `bash -n` sees
# none of it; this renders the manifests and looks, drives status, delete and
# apply through a stub kubectl with fake nodes, pods and volumes, runs the
# scenario edit against the same stub, and reads the Makefile's argv. It
# needs no cluster.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
PY=${PYTHON:-python3}
FAILED=0
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# A missing tool is a failure, not a pass: a check that skips leaves every
# assertion unmade and the run green.
for tool in yq jq "$PY"; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required (this check needs yq, jq and python3 with PyYAML)
' "$tool" >&2; exit 1; }
done
"$PY" -c "import yaml" 2>/dev/null || { printf 'FATAL: python3 has no PyYAML (pip install pyyaml)
' >&2; exit 1; }

IMG=docker.io/vllm/vllm-openai:v0.26.0

# ---------------------------------------------------------------------------
# 1. The render.
# ---------------------------------------------------------------------------
bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --capacity 300Gi \
    --node-selector example.com/accelerator=h200 --toleration example.com/dedicated --dry-run > "$T/render.yaml" || fail "render failed"
bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --dry-run > "$T/render-default.yaml" || fail "default render failed"
bash deploy/enginecache.sh apply -n other-ns --path /mnt/local/weights/check-ns --image "$IMG" --dry-run > "$T/render-other.yaml" || fail "other-ns render failed"

"$PY" - "$T/render.yaml" "$T/render-default.yaml" "$T/render-other.yaml" deploy/lib/accelerator_labels.py <<'PYEOF' || FAILED=1
import re, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
default = [d for d in yaml.safe_load_all(open(sys.argv[2], encoding="utf-8")) if d]
other = [d for d in yaml.safe_load_all(open(sys.argv[3], encoding="utf-8")) if d]
ns = {}
exec(open(sys.argv[4], encoding="utf-8").read(), ns)
product_keys = ns["PRODUCT_KEYS"]
bad = []
def check(cond, msg):
    if not cond:
        bad.append(msg)

check([d["kind"] for d in docs] == ["ServiceAccount", "PersistentVolume", "PersistentVolumeClaim", "DaemonSet"], "kinds: %s" % [d["kind"] for d in docs])
sa, pv, pvc, ds = docs
check(sa["metadata"]["name"] == "engine-cache-preparer" and sa["metadata"]["namespace"] == "check-ns" and sa.get("automountServiceAccountToken") is False,
      "a ServiceAccount of its own, with no token: %s" % sa)
check(ds["spec"]["template"]["spec"].get("serviceAccountName") == "engine-cache-preparer", "the preparer runs as its own ServiceAccount")
check(pvc["metadata"]["name"] == "engine-cache" == ds["metadata"]["name"], "the claim is named engine-cache, one per namespace, and the DaemonSet shares the name")
check(pv["spec"].get("claimRef") == {"namespace": "check-ns", "name": "engine-cache"}, "the volume is bound to this claim and no other: %s" % pv["spec"].get("claimRef"))
check(pvc["spec"]["volumeName"] == pv["metadata"]["name"], "the claim names the volume")
check(re.match(r"^engine-cache-[a-z0-9]+$", pv["metadata"]["name"]), "the volume's name carries a hash: %s" % pv["metadata"]["name"])
check(other[1]["metadata"]["name"] != pv["metadata"]["name"], "another namespace on the same directory gets its own volume")
check(pv["spec"]["storageClassName"] == pvc["spec"]["storageClassName"] == "node-local-engine-cache", "one synthetic storage class on both sides")
check(pv["spec"]["capacity"]["storage"] == pvc["spec"]["resources"]["requests"]["storage"] == "300Gi", "--capacity reaches both, equal")
check(pv["spec"]["persistentVolumeReclaimPolicy"] == "Retain", "Retain: the caches are the point")
check(pv["spec"]["hostPath"] == {"path": "/mnt/local/weights/check-ns/engine-cache", "type": "DirectoryOrCreate"}, "hostPath under DIR/engine-cache: %s" % pv["spec"]["hostPath"])
check(pv["spec"]["accessModes"] == ["ReadWriteMany"] == pvc["spec"]["accessModes"], "RWX on both: every node mounts the same claim")
for d in docs:
    check(d["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "wva-enginecache", "managed-by on %s" % d["kind"])
for d in (pv, pvc, ds):
    check(d["metadata"].get("annotations", {}).get("wva.llmd.ai/engine-cache-path") == "/mnt/local/weights/check-ns/engine-cache", "annotation names the directory on %s" % d["kind"])
check(ds["metadata"]["annotations"]["wva.llmd.ai/engine-cache-node-selector"] == "example.com/accelerator=h200", "the DaemonSet records the selector")
tpl = ds["spec"]["template"]; ps = tpl["spec"]
check(all(tpl["metadata"]["labels"].get(k) == v for k, v in ds["spec"]["selector"]["matchLabels"].items()), "selector matches the template labels")
check(ps["nodeSelector"] == {"example.com/accelerator": "h200"} and "affinity" not in ps, "an explicit selector is a nodeSelector, no affinity")
check({t["key"] for t in ps["tolerations"]} == {"nvidia.com/gpu", "example.com/dedicated"}, "tolerations: %s" % ps["tolerations"])
check(ps.get("automountServiceAccountToken") is False, "no ServiceAccount token")
check((ps.get("securityContext") or {}).get("seccompProfile", {}).get("type") == "RuntimeDefault", "seccomp RuntimeDefault")
vols = ps["volumes"]
check(len(vols) == 1 and vols[0].get("persistentVolumeClaim", {}).get("claimName") == "engine-cache", "the preparer mounts the CLAIM, not a hostPath: %s" % vols)
check(all("hostPath" not in v for v in vols), "no hostPath volume in the pod (Pod Security baseline)")
c = ps["containers"][0]
check(c["image"] == "docker.io/vllm/vllm-openai:v0.26.0" and c["imagePullPolicy"] == "IfNotPresent", "prepares with the given image, IfNotPresent")
env = {e["name"]: e for e in c["env"]}
check(env["NVIDIA_VISIBLE_DEVICES"]["value"] == "void", "no GPU injected into the preparer")
mount = c["volumeMounts"][0]
check(mount["name"] == vols[0]["name"] and mount["mountPath"] == "/engine-cache" and "subPath" not in mount, "the claim is mounted at /engine-cache, whole")
script = c["args"][0]
check("$(" not in script and "$$" not in script, "the command carries no $( or $$ (Kubernetes rewrites both)")
loop = re.search(r"^for d in (.*); do$", script, re.M)
check(loop is not None and set(loop.group(1).split()) == {"vllm", "flashinfer", "triton", "pycache"}, "one mkdir loop over the four cache directories, Python bytecode (pycache) included: %s" % (loop.group(1) if loop else None))
check('mkdir -p "/engine-cache/$d"' in script and 'chmod 1777 "/engine-cache/$d"' in script, "each made, then world-writable and sticky")
check('touch "/engine-cache/$MARKER"' in script and env["MARKER"]["value"] == ".prepared", "the marker is written after the directories, its name from the environment")
check("trap 'exit 0' TERM" in script and "sleep 3600" in script, "then the pod stays, so numberReady counts prepared nodes")
check(c["command"] == ["/bin/sh", "-c"], "sh -c with the script as args")
probe = c["readinessProbe"]["exec"]["command"]
check(probe == ["/bin/sh", "-c", "test -f /engine-cache/.prepared && test -w /engine-cache/vllm"], "Ready is the marker AND the directory writable by this UID: %s" % probe)
check("$NODE_NAME" in script, "the command is rendered as written, not evaluated on the operator's machine")
check(env.get("NODE_NAME", {}).get("valueFrom") == {"fieldRef": {"fieldPath": "spec.nodeName"}} and "$HOSTNAME" not in script,
      "the preparer's log names the node (NODE_NAME from spec.nodeName), not the pod (HOSTNAME): %s" % env.get("NODE_NAME"))
res = c.get("resources", {})
for section in ("requests", "limits"):
    for k in res.get(section, {}):
        check("gpu" not in k and "/" not in k, "the preparer requests an accelerator: %s" % k)
check(res.get("limits", {}).get("memory"), "a memory limit")
csc = c.get("securityContext", {})
check(csc.get("allowPrivilegeEscalation") is False and "ALL" in csc.get("capabilities", {}).get("drop", []), "drops all capabilities")

check(len(default) == 4, "defaults render four documents")
if len(default) == 4:
    dps = default[3]["spec"]["template"]["spec"]
    check("nodeSelector" not in dps, "no --node-selector: no nodeSelector")
    terms = dps.get("affinity", {}).get("nodeAffinity", {}).get("requiredDuringSchedulingIgnoredDuringExecution", {}).get("nodeSelectorTerms", [])
    keys = [t["matchExpressions"][0]["key"] for t in terms if len(t.get("matchExpressions", [])) == 2]
    check(keys == product_keys, "the default affinity is the product-key list: %s" % keys)
    check(default[2]["spec"]["resources"]["requests"]["storage"] == "200Gi", "default capacity 200Gi")
    check(dps["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "default toleration")
for b in bad:
    print("  FAIL render: " + b)
sys.exit(1 if bad else 0)
PYEOF
[ "$FAILED" = 0 ] && ok "render: volume, claim and preparer have the shape the header promises"
bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --seed-claim workload-pvc:engine-cache --dry-run > "$T/render-seed.yaml" || fail "seed render failed"
bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --seed-claim shared-caches --dry-run > "$T/render-seed2.yaml" || fail "seed render (no subPath) failed"
"$PY" - "$T/render-seed.yaml" "$T/render-seed2.yaml" "$T/render-default.yaml" <<'PYEOF' || FAILED=1
import sys, yaml
bad = []
def check(cond, msg):
    if not cond:
        bad.append(msg)
seed = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d][3]["spec"]["template"]["spec"]
seed2 = [d for d in yaml.safe_load_all(open(sys.argv[2], encoding="utf-8")) if d][3]["spec"]["template"]["spec"]
plain = [d for d in yaml.safe_load_all(open(sys.argv[3], encoding="utf-8")) if d][3]
vols = {v["name"]: v for v in seed["volumes"]}
check(vols.get("seed", {}).get("persistentVolumeClaim") == {"claimName": "workload-pvc", "readOnly": True}, "the seed claim is a read-only volume: %s" % vols.get("seed"))
mounts = {m["name"]: m for m in seed["containers"][0]["volumeMounts"]}
check(mounts.get("seed") == {"name": "seed", "mountPath": "/seed", "readOnly": True}, "the seed claim is mounted whole, read-only, with NO subPath (the kubelet would have to create it; on a fresh claim the pod never starts): %s" % mounts.get("seed"))
m2 = {m["name"]: m for m in seed2["containers"][0]["volumeMounts"]}
check("subPath" not in m2.get("seed", {}), "no subPath when none was given: %s" % m2.get("seed"))
script = seed["containers"][0]["args"][0]
check('merge() {' in script and 'local e n t' in script and 'merge "$e" "$t"' in script and 'elif [ -e "$t" ] || [ -L "$t" ]' in script and 'cp -a --no-preserve=ownership "$e" "$t"' in script, "the seed is merged file by file, descending directories both sides have: what the node has is kept, what it lacks is copied")
check(script.index("if [ -d /seed ]") < script.index('touch "/engine-cache/$MARKER"'), "the seed is copied before the prepared marker is written")
check('[ "$seeded_from" != "$want" ]' in script and 'echo "$want" > "/engine-cache/$SEED_MARKER"' in script and 'want="$SEED_ID@$stamp"' in script, "the seed marker records the seed and its publish stamp, and a different seed or a new stamp is merged in")
i_ok = script.index('if [ "$complete" = 1 ]; then\n'); i_mark = script.index('echo "$want" > "/engine-cache/$SEED_MARKER"'); i_loop = script.index('cp -a --no-preserve=ownership "$e" "$t"')
check(i_loop < i_ok < i_mark and '|| { complete=0; rm -f "$t"; continue; }' in script and 'else complete=0; rm -rf "$t"; added=' in script, "the marker is written only after every copy succeeded; a failed copy is removed and the seed retried next start")
probe_seed = seed["containers"][0]["readinessProbe"]["exec"]["command"][2]
check(probe_seed.endswith('&& grep -qx -e "workload-pvc:engine-cache" -e "workload-pvc:engine-cache@[0-9]*" -e "none:workload-pvc:engine-cache" /engine-cache/.seeded'), "with a seed, Ready also requires the node to carry that seed (as published, or before any publish), or the record that it had nothing to seed: %s" % probe_seed)
env = {e["name"]: e.get("value") for e in seed["containers"][0]["env"]}
check(env.get("SEED_MARKER") == ".seeded" and env.get("SEED_ID") == "workload-pvc:engine-cache" and env.get("SEED_SUBPATH") == "engine-cache", "the seed marker's name, the seed's identity and its subPath come from the environment: %s" % env)
check("$(" not in script and "$$" not in script, "still no $( or $$ in the command")
plain_ps = plain["spec"]["template"]["spec"]
check(all(v["name"] != "seed" for v in plain_ps["volumes"]) and all(m["name"] != "seed" for m in plain_ps["containers"][0]["volumeMounts"]), "no --seed-claim: no seed volume or mount")
check(plain["metadata"]["annotations"]["wva.llmd.ai/engine-cache-seed"] == "", "no --seed-claim: an empty seed annotation")
seed_ds = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d][3]
check(seed_ds["metadata"]["annotations"]["wva.llmd.ai/engine-cache-seed"] == "workload-pvc:engine-cache", "the DaemonSet records the seed")
for b in bad:
    print("  FAIL seed render: " + b)
sys.exit(1 if bad else 0)
PYEOF
[ "$FAILED" = 0 ] && ok "render: --seed-claim mounts the seed read-only and the preparer copies it once, before the marker"
# the merge, run: a fake seed and cache; a key the node has is kept, a key it
# lacks (under a directory both have) is added, a second run is a no-op, a
# changed seed adds its new key; under dash when the host has it, sh otherwise
M="$T/merge"; mkdir -p "$M/seed/vllm/torch_compile_cache/keyA" "$M/seed/vllm/torch_compile_cache/keyB" "$M/seed/triton/x" "$M/cache/vllm/torch_compile_cache/keyA"
echo old > "$M/cache/vllm/torch_compile_cache/keyA/f"; echo new > "$M/seed/vllm/torch_compile_cache/keyA/f"; echo b > "$M/seed/vllm/torch_compile_cache/keyB/f"; echo t > "$M/seed/triton/x/f"
sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$M/seed|g; s|/engine-cache|$M/cache|g; s|^trap .*||" > "$M/p.sh"
SH=sh; command -v dash >/dev/null 2>&1 && SH=dash
SEED_MARKER=.seeded SEED_ID=shared:x MARKER=.prepared HOSTNAME=n1 "$SH" "$M/p.sh" > "$M/run1.out" 2>&1
[ "$(cat "$M/cache/vllm/torch_compile_cache/keyA/f")" = old ] && [ "$(cat "$M/cache/vllm/torch_compile_cache/keyB/f")" = b ] && [ "$(cat "$M/cache/triton/x/f")" = t ] && [ "$(cat "$M/cache/.seeded")" = shared:x ] \
    && ok "merge ($SH): the node's key kept, the seed's other key added beside it, triton added, the marker records the seed" || fail "merge run 1: $(cat "$M/run1.out"; ls -R "$M/cache" | head -20)"
grep -q "2 file(s) copied" "$M/run1.out" && ok "merge: two files copied on the first run, and the message says files" || fail "merge run 1 message: $(cat "$M/run1.out")"
SEED_MARKER=.seeded SEED_ID=shared:x MARKER=.prepared HOSTNAME=n1 "$SH" "$M/p.sh" > "$M/run2.out" 2>&1
grep -q "seeded on" "$M/run2.out" && fail "merge: the same seed was copied again" || ok "merge: the same seed is not copied again"
SEED_MARKER=.seeded SEED_ID=shared:x MARKER=.prepared NODE_NAME=node-a HOSTNAME=engine-cache-x7k2p "$SH" "$M/p.sh" > "$M/run2n.out" 2>&1
grep -q "prepared on node-a:" "$M/run2n.out" && ! grep -q engine-cache-x7k2p "$M/run2n.out" && ok "preparer: its log names the node, not the pod" || fail "preparer log names: $(cat "$M/run2n.out")"
mkdir -p "$M/seed/vllm/torch_compile_cache/keyC"; echo c > "$M/seed/vllm/torch_compile_cache/keyC/f"
SEED_MARKER=.seeded SEED_ID=shared:y MARKER=.prepared HOSTNAME=n1 "$SH" "$M/p.sh" > "$M/run3.out" 2>&1
[ "$(cat "$M/cache/vllm/torch_compile_cache/keyC/f")" = c ] && [ "$(cat "$M/cache/.seeded")" = shared:y ] && grep -q "1 file(s) copied" "$M/run3.out" && ok "merge: a changed seed adds its new key and rewrites the marker" || fail "merge run 3: $(cat "$M/run3.out")"
# a copy that cannot complete: an unreadable seed entry (the preparer's own
# chmod 1777 would undo a read-only target directory)
mkdir -p "$M/seed/triton/y"; echo y > "$M/seed/triton/y/f"; chmod 000 "$M/seed/triton/y"
SEED_MARKER=.seeded SEED_ID=shared:z MARKER=.prepared HOSTNAME=n1 "$SH" "$M/p.sh" > "$M/run4.out" 2>&1; chmod 755 "$M/seed/triton/y"
if [ "$(id -u)" = 0 ]; then ok "merge: (the failed-copy case needs a non-root check runner)"; else
grep -q "did not complete" "$M/run4.out" && [ ! -e "$M/cache/.seeded" ] && [ ! -e "$M/cache/triton/y" ] && ok "merge: a copy that fails leaves no seed marker (not the previous seed's) and nothing half-copied" || fail "merge run 4: $(cat "$M/run4.out"; cat "$M/cache/.seeded" 2>&1)"; fi
# a seed claim whose subPath does not exist yet -- a fresh stand-up's
# workload claim: nothing to seed, said so, and Ready; the seed is merged
# once it appears
F="$T/fresh"; mkdir -p "$F/seed" "$F/cache"
sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$F/seed|g; s|/engine-cache|$F/cache|g; s|^trap .*||" > "$F/p.sh"
SEED_MARKER=.seeded SEED_ID=workload-pvc:engine-cache SEED_SUBPATH=engine-cache MARKER=.prepared HOSTNAME=n1 "$SH" "$F/p.sh" > "$F/run1.out" 2>&1; rc=$?
[ "$rc" = 0 ] && [ -f "$F/cache/.prepared" ] && [ "$(cat "$F/cache/.seeded")" = "none:workload-pvc:engine-cache" ] && grep -q "nothing to seed" "$F/run1.out" \
    && ok "seed: an absent subPath is nothing to seed -- prepared, marked none:, and said so" || fail "seed absent: rc=$rc $(cat "$F/run1.out"; cat "$F/cache/.seeded" 2>&1)"
mkdir -p "$F/seed/engine-cache/vllm/torch_compile_cache/keyS"; echo s > "$F/seed/engine-cache/vllm/torch_compile_cache/keyS/f"
SEED_MARKER=.seeded SEED_ID=workload-pvc:engine-cache SEED_SUBPATH=engine-cache MARKER=.prepared HOSTNAME=n1 "$SH" "$F/p.sh" > "$F/run2.out" 2>&1
[ "$(cat "$F/cache/vllm/torch_compile_cache/keyS/f" 2>/dev/null)" = s ] && [ "$(cat "$F/cache/.seeded")" = workload-pvc:engine-cache ] \
    && ok "seed: a seed that appears later is merged from its subPath, and the marker records it" || fail "seed appears later: $(cat "$F/run2.out"; cat "$F/cache/.seeded")"
# publish: a node's caches go into the seed (what the seed has is kept), and
# a stamped seed is merged again by a node already seeded from it
P="$T/publish"; mkdir -p "$P/node/vllm/torch_compile_cache/keyN" "$P/node/triton/t" "$P/seed/engine-cache/vllm/torch_compile_cache/keyS"
echo n > "$P/node/vllm/torch_compile_cache/keyN/f"; echo t > "$P/node/triton/t/f"; echo s > "$P/seed/engine-cache/vllm/torch_compile_cache/keyS/f"
sed -n "/^publish_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$P/seed|g; s|/engine-cache|$P/node|g" > "$P/pub.sh"
grep -q 'cp -a' "$P/pub.sh" || fail "publish: no publish_script extracted"
SEED_SUBPATH=engine-cache NODE_NAME=n1 "$SH" "$P/pub.sh" > "$P/pub.out" 2>&1; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$P/seed/engine-cache/vllm/torch_compile_cache/keyN/f")" = n ] && [ "$(cat "$P/seed/engine-cache/triton/t/f")" = t ] \
    && [ "$(cat "$P/seed/engine-cache/vllm/torch_compile_cache/keyS/f")" = s ] && grep -q "published from n1: 2 file(s) copied" "$P/pub.out" \
    && ok "publish: a node's caches are merged into the seed, the seed's own entries kept" || fail "publish run: rc=$rc $(cat "$P/pub.out")"
# Python bytecode: pycache/ is prepared 1777 like the other three (an engine
# under another UID can create its image's key directory), and seed and
# publish carry it -- each node of a LeaderWorkerSet needs its own copy.
Y="$T/pyc"; mkdir -p "$Y/seed/pycache/k1/usr/lib/python3/vllm" "$Y/cache" "$Y/node/pycache/k2/usr/lib/python3/sglang" "$Y/pseed"
echo a > "$Y/seed/pycache/k1/usr/lib/python3/vllm/a.cpython-312.pyc"; echo b > "$Y/node/pycache/k2/usr/lib/python3/sglang/b.cpython-312.pyc"
sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$Y/seed|g; s|/engine-cache|$Y/cache|g; s|^trap .*||" > "$Y/p.sh"
SEED_MARKER=.seeded SEED_ID=shared:pyc MARKER=.prepared NODE_NAME=n1 "$SH" "$Y/p.sh" > "$Y/run1.out" 2>&1; rc=$?
mode="$(stat -c %a "$Y/cache/pycache" 2>/dev/null)"
[ "$rc" = 0 ] && [ "$mode" = 1777 ] && grep -q "pycache}" "$Y/run1.out" \
    && ok "preparer: pycache/ is made 1777, beside vllm/, flashinfer/ and triton/, and the log names it" || fail "preparer pycache: rc=$rc mode=${mode:-none} $(cat "$Y/run1.out")"
[ "$(cat "$Y/cache/pycache/k1/usr/lib/python3/vllm/a.cpython-312.pyc" 2>/dev/null)" = a ] && grep -q "1 file(s) copied" "$Y/run1.out" \
    && ok "seed: an image's bytecode under pycache/<key> is merged onto the node" || fail "seed pycache: $(cat "$Y/run1.out"; ls -R "$Y/cache" | head -20)"
sed -n "/^publish_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$Y/pseed|g; s|/engine-cache|$Y/node|g" > "$Y/pub.sh"
NODE_NAME=n2 "$SH" "$Y/pub.sh" > "$Y/pub.out" 2>&1; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$Y/pseed/pycache/k2/usr/lib/python3/sglang/b.cpython-312.pyc" 2>/dev/null)" = b ] && grep -q "published from n2: 1 file(s) copied" "$Y/pub.out" \
    && ok "publish: a node's bytecode under pycache/<key> is merged into the seed" || fail "publish pycache: rc=$rc $(cat "$Y/pub.out"; ls -R "$Y/pseed" | head -20)"
# the node seeded before the publish ($F, marker workload-pvc:engine-cache) merges again once the seed is stamped
echo 20261013120000 > "$F/seed/engine-cache/.published"
mkdir -p "$F/seed/engine-cache/vllm/torch_compile_cache/keyP"; echo p > "$F/seed/engine-cache/vllm/torch_compile_cache/keyP/f"
SEED_MARKER=.seeded SEED_ID=workload-pvc:engine-cache SEED_SUBPATH=engine-cache MARKER=.prepared HOSTNAME=n1 "$SH" "$F/p.sh" > "$F/run3.out" 2>&1
[ "$(cat "$F/cache/vllm/torch_compile_cache/keyP/f" 2>/dev/null)" = p ] && [ "$(cat "$F/cache/.seeded")" = "workload-pvc:engine-cache@20261013120000" ] \
    && ok "seed: a stamped seed is merged again by a node already seeded, and the marker records the stamp" || fail "stamped seed: $(cat "$F/run3.out"; cat "$F/cache/.seeded")"
SEED_MARKER=.seeded SEED_ID=workload-pvc:engine-cache SEED_SUBPATH=engine-cache MARKER=.prepared HOSTNAME=n1 "$SH" "$F/p.sh" > "$F/run4.out" 2>&1
grep -q "seeded on" "$F/run4.out" && fail "stamped seed: merged again with no new stamp" || ok "seed: the same stamp is not merged twice"
# a newer stamp whose merge fails, on a node that merged the stamp before: the
# node must not stay Ready on the old marker. The rendered probe is run against
# the node directory. A fake cp fails on one entry, so this holds under root too.
mkdir -p "$T/failcp"
REAL_CP="$(command -v cp)"
cat > "$T/failcp/cp" <<EOF
#!/bin/sh
for a in "\$@"; do case "\$a" in *keyFAIL*) echo "cp: fake failure on \$a" >&2; exit 1 ;; esac; done
exec "$REAL_CP" "\$@"
EOF
chmod +x "$T/failcp/cp"
probe_f="$(yq -r 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].readinessProbe.exec.command[2]' "$T/render-seed.yaml" | sed "s|/engine-cache|$F/cache|g")"
if [ -z "$probe_f" ] || ! sh -c "$probe_f"; then fail "probe: the rendered probe refuses a node that merged the current stamp: '$probe_f' marker $(cat "$F/cache/.seeded")"; else
    echo 20261014120000 > "$F/seed/engine-cache/.published"
    mkdir -p "$F/seed/engine-cache/vllm/torch_compile_cache/keyFAIL"; echo x > "$F/seed/engine-cache/vllm/torch_compile_cache/keyFAIL/f"
    PATH="$T/failcp:$PATH" SEED_MARKER=.seeded SEED_ID=workload-pvc:engine-cache SEED_SUBPATH=engine-cache MARKER=.prepared HOSTNAME=n1 "$SH" "$F/p.sh" > "$F/run5.out" 2>&1
    if grep -q "did not complete" "$F/run5.out" && ! sh -c "$probe_f" 2>/dev/null && [ ! -e "$F/cache/vllm/torch_compile_cache/keyFAIL" ]; then
        ok "seed: a newer stamp that fails to merge leaves the node not Ready, not Ready on the stamp before"
    else
        fail "seed: a failed merge of a newer stamp left the node Ready (marker: $(cat "$F/cache/.seeded" 2>&1)): $(cat "$F/run5.out")"
    fi
    SEED_MARKER=.seeded SEED_ID=workload-pvc:engine-cache SEED_SUBPATH=engine-cache MARKER=.prepared HOSTNAME=n1 "$SH" "$F/p.sh" > "$F/run6.out" 2>&1
    [ "$(cat "$F/cache/.seeded" 2>/dev/null)" = "workload-pvc:engine-cache@20261014120000" ] && sh -c "$probe_f" \
        && ok "seed: the failed stamp is merged at the next start, and the node is Ready again" || fail "seed retry after a failed stamp: $(cat "$F/run6.out")"
fi
# the preparer and the publisher merge with the same code
m_prep="$(sed -n '/^preparer_script() {/,/^SCRIPT$/p' deploy/enginecache.sh | sed -n '/^cpa=0$/,/^merge_report() {/p')"
m_pub="$(sed -n '/^publish_script() {/,/^SCRIPT$/p' deploy/enginecache.sh | sed -n '/^cpa=0$/,/^merge_report() {/p')"
[ -n "$m_prep" ] && [ "$m_prep" = "$m_pub" ] && ok "merge: the preparer and the publisher run the same merge" || fail "merge: the preparer's and the publisher's merge differ, or neither was found"
# a busybox cp, which refuses --no-preserve: seeding and publish still
# complete, with a plain cp. The fake logs every call, so the run shows it
# was the cp in use.
mkdir -p "$T/bbcp"
cat > "$T/bbcp/cp" <<EOF
#!/bin/sh
echo "\$*" >> "$T/bbcp.log"
for a in "\$@"; do case "\$a" in --no-preserve*) echo "cp: unrecognized option '\$a'" >&2; exit 1 ;; esac; done
exec "$REAL_CP" "\$@"
EOF
chmod +x "$T/bbcp/cp"; : > "$T/bbcp.log"
B="$T/bb"; mkdir -p "$B/seed/vllm/torch_compile_cache/keyB/rank_0_0" "$B/cache"
echo b > "$B/seed/vllm/torch_compile_cache/keyB/rank_0_0/f"
sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$B/seed|g; s|/engine-cache|$B/cache|g; s|^trap .*||" > "$B/p.sh"
PATH="$T/bbcp:$PATH" SEED_MARKER=.seeded SEED_ID=shared:bb MARKER=.prepared HOSTNAME=n1 "$SH" "$B/p.sh" > "$B/run.out" 2>&1
grep -q -- '--no-preserve' "$T/bbcp.log" && [ "$(cat "$B/cache/vllm/torch_compile_cache/keyB/rank_0_0/f" 2>/dev/null)" = b ] && [ "$(cat "$B/cache/.seeded" 2>/dev/null)" = shared:bb ] \
    && ok "merge: with a busybox cp (no --no-preserve) the seed is merged and the node marked seeded" || fail "merge with busybox cp: $(cat "$B/run.out"; cat "$T/bbcp.log")"
BP="$T/bbpub"; mkdir -p "$BP/node/vllm/torch_compile_cache/keyN/rank_0_0" "$BP/seed"
echo n > "$BP/node/vllm/torch_compile_cache/keyN/rank_0_0/f"
sed -n "/^publish_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$BP/seed|g; s|/engine-cache|$BP/node|g" > "$BP/pub.sh"
PATH="$T/bbcp:$PATH" SEED_SUBPATH=engine-cache NODE_NAME=n1 "$SH" "$BP/pub.sh" > "$BP/pub.out" 2>&1; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$BP/seed/engine-cache/vllm/torch_compile_cache/keyN/rank_0_0/f" 2>/dev/null)" = n ] \
    && ok "publish: with a busybox cp the node's caches reach the seed" || fail "publish with busybox cp: rc=$rc $(cat "$BP/pub.out")"
# symlinks are never copied, at any depth, by either merge; a file only its
# owner can read is copied readable by every user, and both are reported
L="$T/links"; LK="$L/seed/vllm/torch_compile_cache/keyL"
mkdir -p "$LK/rank_0_0" "$L/cache" "$L/elsewhere"
echo l > "$LK/rank_0_0/f"; echo s > "$L/elsewhere/s"; echo p > "$LK/rank_0_0/private"; chmod 600 "$LK/rank_0_0/private"
ln -s "$L/elsewhere/s" "$LK/rank_0_0/link"; ln -s "$L/elsewhere" "$LK/linkdir"
sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$L/seed|g; s|/engine-cache|$L/cache|g; s|^trap .*||" > "$L/p.sh"
SEED_MARKER=.seeded SEED_ID=shared:l MARKER=.prepared HOSTNAME=n1 "$SH" "$L/p.sh" > "$L/run.out" 2>&1
LC="$L/cache/vllm/torch_compile_cache/keyL"
if [ "$(cat "$LC/rank_0_0/f" 2>/dev/null)" = l ] && [ ! -e "$LC/rank_0_0/link" ] && [ ! -L "$LC/rank_0_0/link" ] && [ ! -e "$LC/linkdir" ] && [ ! -L "$LC/linkdir" ] \
    && grep -q '2 symlink(s) skipped' "$L/run.out" && grep -q "skipped $LK/linkdir: a symlink" "$L/run.out" && [ "$(cat "$L/cache/.seeded" 2>/dev/null)" = shared:l ]; then
    ok "merge: symlinks in the seed, to a file or a directory, are skipped and named; the rest is merged and the node seeded"
else
    fail "merge copied a symlink, or did not say so: $(ls -lR "$LC" 2>&1 | head -12; cat "$L/run.out")"
fi
ls -ld "$LC/rank_0_0/private" 2>/dev/null | grep -q '^-......r' && grep -q '1 file(s) not readable by every user were copied readable by every user' "$L/run.out" \
    && ok "merge: a file only its owner could read is copied readable by every user, and counted" || fail "merge: private file: $(ls -ld "$LC/rank_0_0/private" 2>&1); $(cat "$L/run.out")"
# a name with two leading dots is copied like any other; a 640 file counts as
# not readable by every user, a 644 file does not
DD="$T/dotdot"; DK="$DD/seed/vllm/torch_compile_cache"
mkdir -p "$DK/keyD" "$DK/keyG" "$DD/cache"
echo o > "$DK/keyD/..odd"; echo f > "$DK/keyD/f"; echo g > "$DK/keyG/g640"; chmod 640 "$DK/keyG/g640"; echo h > "$DK/keyG/g644"; chmod 644 "$DK/keyG/g644"
sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$DD/seed|g; s|/engine-cache|$DD/cache|g; s|^trap .*||" > "$DD/p.sh"
SEED_MARKER=.seeded SEED_ID=shared:dd MARKER=.prepared HOSTNAME=n1 "$SH" "$DD/p.sh" > "$DD/run.out" 2>&1
[ "$(cat "$DD/cache/vllm/torch_compile_cache/keyD/..odd" 2>/dev/null)" = o ] && grep -q "4 file(s) copied" "$DD/run.out" \
    && ok "merge: a file named ..odd is copied and counted" || fail "merge: ..odd: $(ls -la "$DD/cache/vllm/torch_compile_cache/keyD" 2>&1 | tr '\n' ';') $(cat "$DD/run.out")"
grep -q "1 file(s) not readable by every user were copied readable by every user, for example $DK/keyG/g640" "$DD/run.out" \
    && ok "merge: a 640 file is counted as not readable by every user, a 644 file is not" || fail "merge: 640 count: $(cat "$DD/run.out")"
# a file whose copy fails is not counted as copied readable: not in a
# directory the node already had, nor under a new directory that is then
# removed. A fake cp fails on zFAIL, so this holds under root too.
mkdir -p "$T/failz"
cat > "$T/failz/cp" <<EOF
#!/bin/sh
for a in "\$@"; do case "\$a" in *zFAIL*) echo "cp: fake failure on \$a" >&2; exit 1 ;; esac; done
exec "$REAL_CP" "\$@"
EOF
chmod +x "$T/failz/cp"
FZ="$T/failcount"; FK="$FZ/seed/vllm/torch_compile_cache"
mkdir -p "$FK/keyE" "$FK/keyR" "$FZ/cache/vllm/torch_compile_cache/keyE"
echo e > "$FK/keyE/zFAIL"; chmod 600 "$FK/keyE/zFAIL"; echo a > "$FK/keyR/a"; chmod 600 "$FK/keyR/a"; echo z > "$FK/keyR/zFAIL"
sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$FZ/seed|g; s|/engine-cache|$FZ/cache|g; s|^trap .*||" > "$FZ/p.sh"
PATH="$T/failz:$PATH" SEED_MARKER=.seeded SEED_ID=shared:fz MARKER=.prepared HOSTNAME=n1 "$SH" "$FZ/p.sh" > "$FZ/run.out" 2>&1
grep -q "did not complete" "$FZ/run.out" && [ ! -e "$FZ/cache/vllm/torch_compile_cache/keyR" ] && ! grep -q "copied readable by every user" "$FZ/run.out" \
    && ok "merge: a file whose copy failed, or was removed with its new directory, is not counted as copied readable" || fail "merge: failed copies counted: $(cat "$FZ/run.out")"
LP="$T/linkpub"; mkdir -p "$LP/node/vllm/torch_compile_cache/keyM" "$LP/seed"
echo m > "$LP/node/vllm/torch_compile_cache/keyM/f"; echo q > "$LP/node/vllm/torch_compile_cache/keyM/private"; chmod 600 "$LP/node/vllm/torch_compile_cache/keyM/private"
ln -s /etc/hostname "$LP/node/vllm/torch_compile_cache/keyM/link"
sed -n "/^publish_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$LP/seed|g; s|/engine-cache|$LP/node|g" > "$LP/pub.sh"
SEED_SUBPATH=engine-cache NODE_NAME=n1 "$SH" "$LP/pub.sh" > "$LP/pub.out" 2>&1; rc=$?
LS="$LP/seed/engine-cache/vllm/torch_compile_cache/keyM"
[ "$rc" = 0 ] && [ "$(cat "$LS/f" 2>/dev/null)" = m ] && [ ! -L "$LS/link" ] && [ ! -e "$LS/link" ] && ls -ld "$LS/private" 2>/dev/null | grep -q '^-......r' && grep -q '1 symlink(s) skipped' "$LP/pub.out" \
    && ok "publish: a node's symlink never reaches the seed, and a private file reaches it readable" || fail "publish with a symlink and a private file: rc=$rc $(ls -l "$LS" 2>&1); $(cat "$LP/pub.out")"
# a key only its owner can read -- 700 directories and a 600 file, as vLLM
# writes its AOT files -- is copied readable and searchable by every user, by
# both merges and with either cp: the copier is root and a non-root engine
# must read the copy. Run under umask 077, so a mode the copy merely inherits
# from mkdir or cp is private and only the merge's own chmod makes it public.
modes_case() { # $1: merge or publish; $2: gnu or busybox
    local W="$T/modes-$1-$2" k="vllm/torch_compile_cache/torch_aot_compile/keyR" src dst pth="$PATH" rc d bad=""
    if [ "$1" = merge ]; then src="$W/seed"; dst="$W/cache"; else src="$W/node"; dst="$W/seed/engine-cache"; fi
    [ "$2" = gnu ] || pth="$T/bbcp:$PATH"
    mkdir -p "$src/$k/rank_0_0" "$W/cache" "$W/seed"
    echo m > "$src/$k/rank_0_0/model"; chmod 600 "$src/$k/rank_0_0/model"
    chmod 700 "$src/vllm/torch_compile_cache" "$src/vllm/torch_compile_cache/torch_aot_compile" "$src/$k" "$src/$k/rank_0_0"
    if [ "$1" = merge ]; then
        sed -n "/^preparer_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$W/seed|g; s|/engine-cache|$W/cache|g; s|^trap .*||" > "$W/run.sh"
        (umask 077; PATH="$pth" SEED_MARKER=.seeded SEED_ID=shared:m MARKER=.prepared NODE_NAME=n1 "$SH" "$W/run.sh") > "$W/run.out" 2>&1; rc=$?
    else
        sed -n "/^publish_script() {/,/^SCRIPT$/p" deploy/enginecache.sh | sed '1,2d;$d' | sed "s|/seed|$W/seed|g; s|/engine-cache|$W/node|g" > "$W/run.sh"
        (umask 077; PATH="$pth" SEED_SUBPATH=engine-cache NODE_NAME=n1 "$SH" "$W/run.sh") > "$W/run.out" 2>&1; rc=$?
    fi
    [ "$(cat "$dst/$k/rank_0_0/model" 2>/dev/null)" = m ] || bad="$bad no copy;"
    ls -ld "$dst/$k/rank_0_0/model" 2>/dev/null | grep -q '^-......r' || bad="$bad model not o+r;"
    for d in vllm/torch_compile_cache vllm/torch_compile_cache/torch_aot_compile "$k" "$k/rank_0_0"; do
        ls -ld "$dst/$d" 2>/dev/null | grep -q '^d......r.x' || bad="$bad $d not o+rx;"
    done
    # publish also creates the seed subPath and its engine directory
    if [ "$1" = publish ]; then
        for d in . vllm; do ls -ld "$dst/$d" 2>/dev/null | grep -q '^d......r.x' || bad="$bad seed $d not o+rx;"; done
    fi
    [ "$rc" = 0 ] && [ -z "$bad" ] && ok "$1 ($2 cp): a 600 file under 700 directories is copied o+r, its directories o+rx" \
        || fail "$1 ($2 cp) modes: rc=$rc$bad $(ls -lR "$dst/vllm" 2>&1 | head -12 | tr '\n' ';') $(cat "$W/run.out")"
}
modes_case merge gnu; modes_case merge busybox; modes_case publish gnu; modes_case publish busybox
# the readiness probe accepts every marker form the preparer writes
for m in "workload-pvc:engine-cache" "workload-pvc:engine-cache@20261013120000" "none:workload-pvc:engine-cache"; do
    echo "$m" > "$T/marker"
    grep -qx -e "workload-pvc:engine-cache" -e "workload-pvc:engine-cache@[0-9]*" -e "none:workload-pvc:engine-cache" "$T/marker" || fail "probe refuses marker $m"
done
echo "other-claim:x" > "$T/marker"
grep -qx -e "workload-pvc:engine-cache" -e "workload-pvc:engine-cache@[0-9]*" -e "none:workload-pvc:engine-cache" "$T/marker" && fail "probe accepts another seed's marker" || ok "probe: accepts the seed's three marker forms, refuses another seed's"
# publish waits on the usable nodes (status), never `rollout status`, which
# also waits for a cordoned node's pod that never comes up
pub_body="$(sed -n '/^cmd_publish() {/,/^}/p; /^reseed_wait() {/,/^}/p' deploy/enginecache.sh | grep -v '^[[:space:]]*#')"
if [ -z "$pub_body" ]; then fail "publish: no cmd_publish found"
elif printf '%s' "$pub_body" | grep -q 'rollout status'; then fail "publish: waits with rollout status, which counts cordoned nodes"
elif printf '%s' "$pub_body" | grep -q 'enginecache.sh" status'; then ok "publish: waits for the usable nodes to re-seed, not for every DaemonSet pod"
else fail "publish: no wait for the preparers to re-seed"; fi
# a publish pod that Failed is reported at once, not after the whole timeout
if printf '%s' "$pub_body" | grep -q 'for=jsonpath=.{.status.phase}.=Succeeded'; then
    fail "publish: waits on Succeeded alone, so a Failed pod sits out the timeout"
fi
# the re-seed wait counts only preparers of the restarted template: status alone
# passes on the pods from before the restart
printf '%s' "$pub_body" | grep -q 'stale_preparers "$gen"' && ok "publish: the re-seed wait needs every live preparer of the restarted template" \
    || fail "publish: the re-seed wait passes on preparers from before the restart"
sp_body="$(sed -n '/^stale_preparers() {/,/^}/p' deploy/enginecache.sh)"
if [ -z "$sp_body" ]; then fail "publish: no stale_preparers"; else
    # n1 Ready, n2 not. The last pod is being deleted on n2, where no kubelet can confirm it.
    pods='{"items":[
      {"metadata":{"labels":{"pod-template-generation":"13"}},"spec":{"nodeName":"n1"},"status":{"phase":"Running"}},
      {"metadata":{"labels":{"pod-template-generation":"12"}},"spec":{"nodeName":"n1"},"status":{"phase":"Running"}},
      {"metadata":{"labels":{"pod-template-generation":"12"}},"spec":{"nodeName":"n1"},"status":{"phase":"Failed"}},
      {"metadata":{"labels":{"pod-template-generation":"12"},"deletionTimestamp":"2026-10-13T00:00:00Z"},"spec":{"nodeName":"n2"},"status":{"phase":"Running"}}]}'
    nodes='{"items":[
      {"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
      {"metadata":{"name":"n2"},"status":{"conditions":[{"type":"Ready","status":"Unknown"}]}}]}'
    # MODE:GEN:WANT -- ok reads both; nodesfail cannot list nodes; podsfail cannot read the pods
    for c in "ok:13:1" "ok:14:2" "nodesfail:13:2" "podsfail:13:?"; do
        out="$(MODE="${c%%:*}" PODS="$pods" NODES="$nodes" bash -c 'kubectl() {
              case "$MODE:$2" in podsfail:pods|nodesfail:nodes) return 1 ;; *:pods) printf %s "$PODS" ;; *:nodes) printf %s "$NODES" ;; esac; }
            NAMESPACE=ns; '"$sp_body"'
stale_preparers '"$(echo "$c" | cut -d: -f2)")"
        want="${c##*:}"
        [ "$out" = "$want" ] && ok "stale_preparers ($c): $out" || fail "stale_preparers ($c): got '$out', want $want"
    done
    printf '%s' "$pub_body" | grep -q 'deprecated\\.daemonset\\.template\\.generation' \
        && ok "publish: compares against the DaemonSet's template generation, which the pod label follows" \
        || fail "publish: compares against .metadata.generation, which also moves on edits outside the template"
fi
# a node that never merges the new stamp: the wait times out and publish fails,
# never reporting success. status is a stand-in that exits as told.
rw_body="$(sed -n '/^reseed_wait() {/,/^}/p' deploy/enginecache.sh)"
if [ -z "$rw_body" ]; then fail "publish: no reseed_wait, so nothing stops a success report after the re-seed wait times out"; else
    mkdir -p "$T/rw"; printf '#!/bin/sh\nexit "$STATUS_RC"\n' > "$T/rw/enginecache.sh"
    for c in 1:1 0:0; do
        out="$(STATUS_RC="${c%%:*}" bash -c 'stale_preparers() { echo 0; }; sleep() { SECONDS=$((SECONDS + $1)); }; HERE="'"$T/rw"'"; NAMESPACE=ns; '"$rw_body"'
SECONDS=0; reseed_wait 13 600; echo "rc=$?"')"
        [ "$out" = "rc=${c#*:}" ] && ok "reseed_wait: status exiting ${c%%:*} gives $out" || fail "reseed_wait: status exiting ${c%%:*} gave '$out', want rc=${c#*:}"
    done
    pub_only="$(sed -n '/^cmd_publish() {/,/^}/p' deploy/enginecache.sh | grep -v '^[[:space:]]*#')"
    if printf '%s\n' "$pub_only" | grep -A1 'reseed_wait "$gen"' | grep -q '|| log_error' \
        && [ "$(printf '%s\n' "$pub_only" | grep -n 'reseed_wait "$gen"' | cut -d: -f1)" -lt "$(printf '%s\n' "$pub_only" | grep -n 'log_success' | cut -d: -f1)" ]; then
        ok "publish: a re-seed wait that times out is an error, and success is reported only after it passes"
    else
        fail "publish: reports success whether or not every usable node merged the new stamp"
    fi
fi
ps_body="$(sed -n '/^pod_succeeded() {/,/^}/p' deploy/enginecache.sh)"
if [ -z "$ps_body" ]; then fail "publish: no pod_succeeded"; else
    # "At once" is counted in sleeps, not read off SECONDS: SECONDS also follows
    # the wall clock, so a fork that straddles a second boundary read as a wait.
    for c in Succeeded:0 Failed:1 Pending:1; do
        out="$(PHASE="${c%%:*}" bash -c 'kubectl() { printf %s "$PHASE"; }; sleep() { slept=$((slept + 1)); SECONDS=$((SECONDS + $1)); }; NAMESPACE=ns; '"$ps_body"'
slept=0; SECONDS=0; pod_succeeded p 900; echo "rc=$? slept=$slept"')"
        want="rc=${c#*:}"
        case "$c:$out" in
            Pending*) [ "${out%% *}" = "$want" ] && ok "pod_succeeded: a pod still $(echo "${c%%:*}") at the deadline is a failure" || fail "pod_succeeded on ${c%%:*}: $out" ;;
            *) [ "$out" = "$want slept=0" ] && ok "pod_succeeded: ${c%%:*} answers at once ($want)" || fail "pod_succeeded on ${c%%:*}: $out (want $want at once)" ;;
        esac
    done
fi
n="$(grep -c "cat <<'SCRIPT'" deploy/enginecache.sh)"
[ "$n" = 2 ] && grep -q '^\$(preparer_script | sed' deploy/enginecache.sh && grep -q '^\$(publish_script | sed' deploy/enginecache.sh && ok "render: the preparer's and the publisher's commands come from quoted heredocs, so nothing in them is evaluated at apply time" || fail "render: a preparer or publisher command is not in a quoted heredoc ($n of 2)"

# ---------------------------------------------------------------------------
# 2. What apply refuses, before any kubectl.
# ---------------------------------------------------------------------------
refuse() {  # refuse "<why>" args...
    local why="$1"; shift
    if bash deploy/enginecache.sh "$@" >/dev/null 2>&1; then fail "accepted: $why"; else ok "refused: $why"; fi
}
refuse "apply without a namespace" apply --path /mnt/local/w --image "$IMG" --dry-run
refuse "apply without a path" apply -n ns --image "$IMG" --dry-run
refuse "apply without an image" apply -n ns --path /mnt/local/w --dry-run
refuse "a path under /var/lib" apply -n ns --path /var/lib/kubelet --image "$IMG" --dry-run
refuse "a relative path" apply -n ns --path mnt/local --image "$IMG" --dry-run
refuse "a top-level directory on its own" apply -n ns --path /mnt --image "$IMG" --dry-run
refuse "an image with a space" apply -n ns --path /mnt/local/w --image "img one" --dry-run
refuse "a capacity that is not a quantity" apply -n ns --path /mnt/local/w --image "$IMG" --capacity "1 Ti" --dry-run
refuse "a selector with a quote" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a=b"' --dry-run
refuse "a selector with a trailing comma" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a=b,' --dry-run
refuse "a selector with an empty pair" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a=b,,c=d' --dry-run
refuse "a selector naming a key twice" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a=b,a=c' --dry-run
refuse "a selector value with a space" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a=b c' --dry-run
refuse "a selector with a newline between pairs" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector $'a=b\nc=d' --dry-run
refuse "a selector key with two slashes" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a/b/c=d' --dry-run
refuse "a selector value ending in a dash" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a=b-' --dry-run
# several pairs, comma separated: a node must carry every one
SEL2=example.com/accelerator=h200,example.com/state=production
if bash deploy/enginecache.sh apply -n ns --path /mnt/local/w --image "$IMG" --node-selector "$SEL2" --dry-run > "$T/render-sel2.yaml" 2>&1; then
    [ "$(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-sel2.yaml")" = '{"example.com/accelerator":"h200","example.com/state":"production"}' ] \
        && [ "$(yq -r 'select(.kind == "DaemonSet") | .metadata.annotations["wva.llmd.ai/engine-cache-node-selector"]' "$T/render-sel2.yaml")" = "$SEL2" ] \
        && ok "two pairs: the nodeSelector carries both keys, the annotation the selector as given" || fail "two pairs rendered: $(yq -o json -I0 'select(.kind == "DaemonSet") | [.metadata.annotations, .spec.template.spec.nodeSelector]' "$T/render-sel2.yaml")"
else
    fail "a two-pair --node-selector was refused: $(tail -1 "$T/render-sel2.yaml")"
fi
"$PY" -c 'import re, sys; sys.exit(0 if re.search(r"\n      nodeSelector:\n        \"example\.com/accelerator\": \"h200\"\n(?!        )", open(sys.argv[1]).read()) else 1)' "$T/render.yaml" \
    && ok "one pair: the nodeSelector block is the one it always was, the key now quoted as the value is" || fail "one pair: the nodeSelector block changed"
refuse "--dry-run on status" status -n ns --dry-run
refuse "an unknown command" prepare -n ns
refuse "an unknown option" apply -n ns --path /mnt/local/w --image "$IMG" --bogus --dry-run
refuse "a seed that is the engine-cache claim itself" apply -n ns --path /mnt/local/w --image "$IMG" --seed-claim engine-cache --dry-run
refuse "a seed subPath with '..'" apply -n ns --path /mnt/local/w --image "$IMG" --seed-claim "shared:../x" --dry-run
refuse "a seed subPath that is absolute" apply -n ns --path /mnt/local/w --image "$IMG" --seed-claim "shared:/x" --dry-run
refuse "a seed claim name that is not one" apply -n ns --path /mnt/local/w --image "$IMG" --seed-claim "Shared Caches" --dry-run
refuse "a seed with an empty subPath after the colon" apply -n ns --path /mnt/local/w --image "$IMG" --seed-claim "shared:" --dry-run

# ---------------------------------------------------------------------------
# 3. engine_cache_claim.sh against a stub kubectl.
# ---------------------------------------------------------------------------
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
ARGV=" \$* "
case "\$ARGV" in
  *" get pvc "*)
    case "\$ARGV" in *" -n "*) ;; *) echo "stub kubectl: pvc read without -n:\$ARGV" >&2; exit 2 ;; esac
    case "\$ARGV" in
      *"jsonpath={.status.phase}"*) printf '%s' "\${STUB_PVC_PHASE-Bound}" ;;
      *"jsonpath={.spec.storageClassName}"*) printf '%s' "\${STUB_PVC_CLASS-node-local-engine-cache}" ;;
      *) echo "stub kubectl: unexpected pvc read:\$ARGV" >&2; exit 2 ;;
    esac ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
STUB_PATH="$T/bin:$PATH"
cat > "$T/scenario.yaml" <<'EOF'
scenario:
  - name: pd
    prefill:
      additionalVolumeMounts:
        - name: engine-cache
          mountPath: /engine-cache
          subPath: engine-cache
      additionalVolumes:
        - name: engine-cache
          type: persistentVolumeClaim
          persistentVolumeClaim:
            claimName: workload-pvc
    decode:
      additionalVolumeMounts:
        - name: engine-cache
          mountPath: /engine-cache
          subPath: engine-cache
        - name: other
          mountPath: /other
          subPath: keep
      additionalVolumes:
        - name: engine-cache
          type: persistentVolumeClaim
          persistentVolumeClaim:
            claimName: workload-pvc
        - name: other
          type: persistentVolumeClaim
          persistentVolumeClaim:
            claimName: other-pvc
EOF
cp "$T/scenario.yaml" "$T/s1.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s1.yaml" check-ns > "$T/s1.out" 2>&1; then
    a="$(yq -r '.scenario[0].prefill.additionalVolumes[0].persistentVolumeClaim.claimName' "$T/s1.yaml")"
    b="$(yq -r '.scenario[0].decode.additionalVolumes[0].persistentVolumeClaim.claimName' "$T/s1.yaml")"
    o="$(yq -r '.scenario[0].decode.additionalVolumes[1].persistentVolumeClaim.claimName' "$T/s1.yaml")"
    [ "$a" = engine-cache ] && [ "$b" = engine-cache ] && [ "$o" = other-pvc ] && ok "engine_cache_claim: both roles' engine-cache volumes name the claim, another volume is untouched" || fail "engine_cache_claim volumes: prefill=$a decode=$b other=$o"
    sp="$(yq -r '.scenario[0].decode.additionalVolumeMounts[0] | has("subPath")' "$T/s1.yaml")"
    sk="$(yq -r '.scenario[0].decode.additionalVolumeMounts[1].subPath' "$T/s1.yaml")"
    [ "$sp" = false ] && [ "$sk" = keep ] && ok "engine_cache_claim: the engine-cache mount loses its subPath, another mount keeps its own" || fail "engine_cache_claim mounts: subPath=$sp other=$sk"
    grep -q '2 engine-cache volume(s)' "$T/s1.out" && ok "engine_cache_claim: the message counts both" || fail "engine_cache_claim message: $(cat "$T/s1.out")"
else
    fail "engine_cache_claim failed on the fixture: $(cat "$T/s1.out")"
fi
cp "$T/scenario.yaml" "$T/s2.yaml"
if STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s2.yaml" check-ns > "$T/s2.out" 2>&1; then fail "engine_cache_claim must refuse a claim that is not Bound"; else
    grep -q "no Bound claim named engine-cache after 30 s" "$T/s2.out" && cmp -s "$T/scenario.yaml" "$T/s2.yaml" && ok "engine_cache_claim: a claim that stays Pending is refused after the wait, scenario untouched" || fail "engine_cache_claim Pending: $(tail -1 "$T/s2.out")"; fi
# the claim binds a moment after the apply: the first reads see Pending, then Bound
cat > "$T/bin/kubectl-late" <<EOF
#!/usr/bin/env bash
n=\$(cat "$T/late.n" 2>/dev/null || echo 0); n=\$((n + 1)); echo "\$n" > "$T/late.n"
case "\$*" in *"jsonpath={.status.phase}"*) [ "\$n" -ge 3 ] && printf Bound || printf Pending ;; *"jsonpath={.spec.storageClassName}"*) printf node-local-engine-cache ;; *) exit 2 ;; esac
EOF
chmod +x "$T/bin/kubectl-late"; mkdir -p "$T/late"; cp "$T/bin/kubectl-late" "$T/late/kubectl"; rm -f "$T/late.n"
cp "$T/scenario.yaml" "$T/s2b.yaml"
if PATH="$T/late:$PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s2b.yaml" check-ns > "$T/s2b.out" 2>&1; then
    [ "$(cat "$T/late.n")" -ge 3 ] && grep -q '2 engine-cache volume(s)' "$T/s2b.out" && ok "engine_cache_claim: a claim Bound on the third read is waited for, then the edit is made" || fail "engine_cache_claim late bind: reads=$(cat "$T/late.n"); $(tail -1 "$T/s2b.out")"
else fail "engine_cache_claim must wait for a claim that binds a moment later: $(tail -1 "$T/s2b.out")"; fi
cp "$T/scenario.yaml" "$T/s3.yaml"
if STUB_PVC_PHASE= PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s3.yaml" check-ns >/dev/null 2>&1; then fail "engine_cache_claim must refuse a missing claim"; else ok "engine_cache_claim: a missing claim is refused"; fi
cp "$T/scenario.yaml" "$T/s4.yaml"
if STUB_PVC_CLASS=shared-vast PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s4.yaml" check-ns > "$T/s4.out" 2>&1; then fail "engine_cache_claim must refuse a claim on another class"; else
    grep -q "not node-local-engine-cache" "$T/s4.out" && cmp -s "$T/scenario.yaml" "$T/s4.yaml" && ok "engine_cache_claim: a same-named claim on another class is refused, scenario untouched" || fail "engine_cache_claim class: $(tail -1 "$T/s4.out")"; fi
printf 'scenario:\n  - name: x\n    decode: {}\n' > "$T/s5.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s5.yaml" check-ns >/dev/null 2>&1; then fail "engine_cache_claim must fail on a scenario with no engine-cache volume"; else ok "engine_cache_claim: a scenario with no engine-cache volume is refused, not silently left shared"; fi
cp "$T/scenario.yaml" "$T/s6.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s6.yaml" check-ns 'Bad_Name' >/dev/null 2>&1; then fail "engine_cache_claim accepted a claim name that is not a label"; else ok "engine_cache_claim: a claim name that is not a DNS label is refused"; fi
# every shipped scenario that carries the volume is repointed (two roles in
# the P/D one, one in the others); a vLLM scenario without one gets one per
# engine role, with the cache directories under it; one with no vLLM role to
# add it to is refused
EC_ROLES='.. | select(type == "!!map" and (has("decode") or has("prefill"))) | (.decode, .prefill) | select(type == "!!map" and has("vllm") and .enabled != false)'
for sc in hack/benchmark/scenarios/guides/*.yaml; do
    cp "$sc" "$T/real.yaml"
    have="$(yq -r '[.. | select(type == "!!map" and has("additionalVolumes")) | .additionalVolumes[] | select(.name == "engine-cache")] | length' "$sc")"
    roles="$(yq -r "[${EC_ROLES}] | length" "$sc")"
    want="$have"; [ "$have" -gt 0 ] || want="$roles"
    if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/real.yaml" check-ns > "$T/real.out" 2>&1; then
        n="$(yq -r '[.. | select(type == "!!map" and has("additionalVolumes")) | .additionalVolumes[] | select(.name == "engine-cache") | .persistentVolumeClaim.claimName] | map(select(. == "engine-cache")) | length' "$T/real.yaml")"
        sub="$(yq -r '[.. | select(type == "!!map" and has("additionalVolumeMounts")) | .additionalVolumeMounts[] | select(.name == "engine-cache") | has("subPath")] | map(select(.)) | length' "$T/real.yaml")"
        [ "$want" -gt 0 ] && [ "$n" = "$want" ] && [ "$sub" = 0 ] && ok "engine_cache_claim: $(basename "$sc") -- $n engine-cache volume(s) on the claim, no subPath left" || fail "engine_cache_claim on $(basename "$sc"): had $have, wanted $want, on the claim $n, subPaths left $sub"
        if [ "$have" = 0 ]; then
            wired="$(yq -r "[${EC_ROLES} | select(.vllm.cacheRoot == \"/engine-cache/vllm\" and ([.extraEnvVars[] | select(.name == \"FLASHINFER_WORKSPACE_DIR\" or .name == \"TRITON_CACHE_DIR\") | .value] | join(\",\")) == \"/engine-cache/flashinfer,/engine-cache/triton\" and ([.additionalVolumeMounts[] | select(.name == \"engine-cache\" and .mountPath == \"/engine-cache\")] | length) == 1)] | length" "$T/real.yaml")"
            [ "$wired" = "$roles" ] && grep -q 'added an engine cache' "$T/real.out" && ok "engine_cache_claim: $(basename "$sc") -- had no engine cache; all $roles vLLM role(s) now keep VLLM_CACHE_ROOT, FlashInfer and Triton under /engine-cache" || fail "engine_cache_claim on $(basename "$sc"): added wiring on $wired of $roles role(s): $(cat "$T/real.out")"
        fi
    else
        [ "$have" = 0 ] && [ "$roles" = 0 ] && grep -q 'declares no engine cache to move' "$T/real.out" && ok "engine_cache_claim: $(basename "$sc") -- carries no engine-cache volume and no vLLM role, and is refused" || fail "engine_cache_claim on $(basename "$sc"): $(cat "$T/real.out")"
    fi
done


# ---------------------------------------------------------------------------
# 3b. status and delete, offline, against a stub kubectl with fake objects.
# ---------------------------------------------------------------------------
LBL='app.kubernetes.io/component=node-local-engine-cache,app.kubernetes.io/managed-by=wva-enginecache'
PVNAME="$(yq -r 'select(.kind == "PersistentVolume") | .metadata.name' "$T/render-default.yaml")"
[ -n "$PVNAME" ] || fail "no volume name in the rendered manifests"
cat > "$T/nodes.json" <<EOF
{"items":[
 {"metadata":{"name":"node-ready",   "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-notready","labels":{"gpu.nvidia.com/model":"H200"}}},
 {"metadata":{"name":"node-failing", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-nopod",   "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-cpu",     "labels":{"gpu.nvidia.com/model":""}}},
 {"metadata":{"name":"node-evicted", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-cordoned","labels":{"nvidia.com/gpu.product":"H200"}},"spec":{"unschedulable":true,"taints":[{"key":"node.kubernetes.io/unschedulable","effect":"NoSchedule"}]}},
 {"metadata":{"name":"node-foreign", "labels":{"nvidia.com/gpu.product":"H200"}},"spec":{"taints":[{"key":"example.com/team-x","effect":"NoExecute"}]}}
]}
EOF
cat > "$T/pods.json" <<EOF
{"items":[
 {"metadata":{"name":"ec-ready","labels":{"wva.llmd.ai/engine-cache":"engine-cache"}},"spec":{"nodeName":"node-ready"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"name":"ec-notready","labels":{"wva.llmd.ai/engine-cache":"engine-cache"}},"spec":{"nodeName":"node-notready"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"running":{}}}]}},
 {"metadata":{"name":"ec-failing","labels":{"wva.llmd.ai/engine-cache":"engine-cache"}},"spec":{"nodeName":"node-failing"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1,"reason":"Error"}}}]}},
 {"metadata":{"name":"other","labels":{"wva.llmd.ai/engine-cache":"other"}},"spec":{"nodeName":"node-nopod"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"name":"ec-evicted","labels":{"wva.llmd.ai/engine-cache":"engine-cache"}},"spec":{"nodeName":"node-evicted"},"status":{"phase":"Failed","reason":"Evicted","message":"The node had condition: [DiskPressure]."}}
]}
EOF
# the DaemonSet as apply wrote it: it tolerates nvidia.com/gpu, nothing else
printf '%s' '{"spec":{"template":{"spec":{"tolerations":[{"key":"nvidia.com/gpu","operator":"Exists"}]}}}}' > "$T/ds.json"
# the volumes carrying our labels: ours, the same claim in another namespace,
# and one whose claimRef is a weights claim that somehow carries our labels
cat > "$T/pvs.json" <<EOF
{"items":[
 {"metadata":{"name":"${PVNAME}"},                   "spec":{"claimRef":{"namespace":"check-ns","name":"engine-cache"}}},
 {"metadata":{"name":"engine-cache-othernamespace"}, "spec":{"claimRef":{"namespace":"other-ns","name":"engine-cache"}}},
 {"metadata":{"name":"engine-cache-not-this-claim"}, "spec":{"claimRef":{"namespace":"check-ns","name":"weights-x"}}}
]}
EOF
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$T/calls"
ARGV=" \$* "
want() { case "\$ARGV" in *" \$1 \$2 "*) ;; *) echo "stub kubectl: \$1 \$2 missing from:\$ARGV" >&2; exit 2 ;; esac; }
refuse() { case "\$ARGV" in *" \$1 "*) echo "stub kubectl: unexpected \$1 in:\$ARGV" >&2; exit 2 ;; esac; }
case "\$1 \$2" in
  "get nodes")   if [ -n "\${STUB_SELECTOR:-}" ]; then want -l "\$STUB_SELECTOR"; else refuse -l; fi
                 [ -n "\${STUB_NODES_ERROR:-}" ] && { echo "\$STUB_NODES_ERROR" >&2; exit 1; }
                 if [ -n "\${STUB_NODES_FILE:-}" ]; then jq --arg s "\${STUB_SELECTOR:-}" '.items |= map(select((.metadata.labels // {}) as \$l | all(\$s | split(",")[]; index("=") as \$i | \$l[.[:\$i]] == .[\$i+1:])))' "\$STUB_NODES_FILE"; else cat "$T/nodes.json"; fi ;;
  "get pods")    want -n check-ns; want -l "$LBL"
                 [ -z "\${STUB_PODS_ERROR:-}" ] || { echo "\$STUB_PODS_ERROR" >&2; exit 1; }
                 cat "\${STUB_PODS_FILE:-$T/pods.json}" ;;
  "get pod")     want -n check-ns
                 case "\$ARGV" in *"VLLM_CACHE_ROOT"*) cat "$T/engine-root/\$5" 2>/dev/null; exit 0 ;; *) echo "stub kubectl: unexpected pod read:\$ARGV" >&2; exit 2 ;; esac ;;
  "exec -n")     want -n check-ns; exec bash "$T/exec-stub.sh" "\$@" ;;
  "get daemonset")
      want -n check-ns
      case "\$ARGV" in *" engine-cache "*) ;; *) echo "stub kubectl: get daemonset for a name that is not the DaemonSet:\$ARGV" >&2; exit 2 ;; esac
      case "\$ARGV" in
        *"engine-cache-path}"*) printf '%s' "\${STUB_DS_PATH-/mnt/local/weights/check-ns/engine-cache}" ;;
        *" -o json "*) cat "$T/ds.json" ;;
        *"jsonpath={.spec.template.spec.nodeSelector}"*)
           if [ -n "\${STUB_DS_SELECTOR:-}" ]; then printf '%s' "\$STUB_DS_SELECTOR" | jq -Rc 'split(",") | map(index("=") as \$i | {key: .[:\$i], value: .[\$i+1:]}) | from_entries'; else printf '{}'; fi ;;
        *) echo "stub kubectl: unexpected daemonset read:\$ARGV" >&2; exit 2 ;;
      esac ;;
  "get pvc")
      want -n check-ns
      case "\$ARGV" in
        *" workload-pvc "*) case "\$ARGV" in *"jsonpath={.status.phase}"*) printf '%s' "\${STUB_SEED_PHASE-Bound}"; exit 0 ;; *) echo "stub kubectl: unexpected seed read:\$ARGV" >&2; exit 2 ;; esac ;;
        *" engine-cache "*) ;;
        *) echo "stub kubectl: the claim read names no claim:\$ARGV" >&2; exit 2 ;;
      esac
      case "\$ARGV" in
        *"jsonpath={.status.phase}"*)
            if [ -n "\${STUB_BIND_AFTER:-}" ]; then
                n=\$(( \$(cat "$T/bind.n" 2>/dev/null || echo 0) + 1 )); echo "\$n" > "$T/bind.n"
                if [ "\$n" -ge "\$STUB_BIND_AFTER" ]; then printf Bound; else printf Pending; fi
            else printf '%s' "\${STUB_PVC_PHASE-Bound}"; fi ;;
        *"jsonpath={.metadata.name}/{.spec.storageClassName}"*) [ -z "\${STUB_PVC_PHASE-Bound}" ] || printf 'engine-cache/%s' "\${STUB_PVC_CLASS-}" ;;
        *"jsonpath={.spec.volumeName}"*) [ -z "\${STUB_PVC_PHASE-Bound}" ] || printf '%s' "\${STUB_PVC_VOLUME-$PVNAME}" ;;
        *"jsonpath={.spec.storageClassName}"*) printf '%s' "\${STUB_PVC_CLASS-}" ;;
        *"jsonpath={.spec.resources.requests.storage}"*)
            case "\$ARGV" in *" --ignore-not-found "*) ;; *) echo "stub kubectl: the size read lacks --ignore-not-found:\$ARGV" >&2; exit 2 ;; esac
            [ -z "\${STUB_PVC_READ_ERROR:-}" ] || { echo "\$STUB_PVC_READ_ERROR" >&2; exit 1; }
            [ -z "\${STUB_PVC_PHASE-Bound}" ] || printf '%s' "\${STUB_PVC_SIZE-200Gi}" ;;
        *) echo "stub kubectl: unexpected pvc read:\$ARGV" >&2; exit 2 ;;
      esac ;;
  "get events")  want -n check-ns; printf '%s' "\${STUB_EVENT:-}" ;;
  "logs -n")     want -n check-ns
                 case "\$ARGV" in
                   *" -c vllm "*) [ -z "\${STUB_ENGINE_LOG_ERROR:-}" ] || { echo "\$STUB_ENGINE_LOG_ERROR" >&2; exit 1; }; cat "$T/engine-logs/\$4" ;;
                   *) printf '%s\n' "\${STUB_LOG:-}" ;;
                 esac ;;
  "auth can-i")  case "\$ARGV" in *" create persistentvolumes -A -q "*) [ -z "\${STUB_NO_PV:-}" ] || exit 1; exit 0 ;; *) echo "stub kubectl: unexpected can-i:\$ARGV" >&2; exit 2 ;; esac ;;
  "api-resources --api-group=security.openshift.io") [ -n "\${STUB_OPENSHIFT:-}" ] && echo "securitycontextconstraints scc security.openshift.io/v1 false SecurityContextConstraints"; : ;;
  "apply -f")    cat > "\${STUB_APPLIED:-/dev/null}"; echo applied ;;
  "delete daemonset"|"delete pvc"|"delete serviceaccount") want -n check-ns; echo deleted ;;
  "get pv")      refuse -n
                 case "\$ARGV" in
                   *"jsonpath={.spec.hostPath.path}"*) case "\$ARGV" in *" $PVNAME "*) printf '%s' "\${STUB_PV_PATH-/mnt/local/weights/check-ns/engine-cache}" ;; *) echo "stub kubectl: pv read for a volume that is not the claim's:\$ARGV" >&2; exit 2 ;; esac ;;
                   *" -o name "*)
                       case "\$ARGV" in *" $PVNAME "*) ;; *) echo "stub kubectl: pv read for a volume that is not the claim's:\$ARGV" >&2; exit 2 ;; esac
                       case "\${STUB_PV_STATE:-exists}" in
                         exists) printf 'persistentvolume/%s\n' "$PVNAME" ;;
                         absent) case "\$ARGV" in *" --ignore-not-found "*) ;; *) echo "Error from server (NotFound): persistentvolumes $PVNAME not found" >&2; exit 1 ;; esac ;;
                         forbidden) echo "Error from server (Forbidden): persistentvolumes $PVNAME is forbidden: stub" >&2; exit 1 ;;
                       esac ;;
                   *) want -l "$LBL"; want -o json; cat "$T/pvs.json" ;;
                 esac ;;
  "delete pv")   refuse -n; echo deleted ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/status.out" 2>&1; rc=$?
expect_line() { if grep -qE "^  $1 +$2 " "$T/status.out"; then ok "status: $1 -> $2"; else fail "status: $1 expected $2; got: $(grep -E "^  $1 " "$T/status.out" || echo none)"; fi; }
expect_line node-ready    prepared
grep -qE '^  node-ready +prepared +Ready *$' "$T/status.out" && ok "status: a healthy row ends at its phase (an empty reason is not swallowed)" || fail "status: healthy row: $(grep '^  node-ready' "$T/status.out")"
expect_line node-notready absent
grep -qE '^  node-notready +absent +Running \(not ready\)' "$T/status.out" && ok "status: a preparer that is up but not Ready (the directory not writable by its UID) is not prepared" || fail "status: not-ready row: $(grep '^  node-notready' "$T/status.out")"
expect_line node-failing  absent
grep -q 'node-failing .*CrashLoopBackOff (last exit 1 Error)' "$T/status.out" && ok "status: a crash-looping preparer carries its reason and last exit" || fail "status: failing row: $(grep '^  node-failing' "$T/status.out")"
expect_line node-nopod    absent
grep -qE '^  node-nopod +absent +no pod' "$T/status.out" && ok "status: a node the DaemonSet did not reach is 'no pod' (another DaemonSet's pod there does not count)" || fail "status: nopod row: $(grep '^  node-nopod' "$T/status.out")"
expect_line node-evicted  absent
grep -q 'node-evicted .*Evicted' "$T/status.out" && ok "status: an evicted preparer shows its reason" || fail "status: Evicted reason missing"
grep -q '^  node-cpu ' "$T/status.out" && fail "status: a node with an empty product label was counted" || ok "status: the default placement leaves the empty-label node out"
grep -q 'Permission denied\|Read-only' "$T/status.out" && fail "status: a permission diagnosis with no such error in the log" || ok "status: no permission diagnosis when the log shows none"
grep -q '1/5 usable nodes prepared; 4 not' "$T/status.out" && ok "status: the tally counts only Ready as prepared, on usable nodes" || fail "status: tally: $(grep 'nodes prepared' "$T/status.out")"
grep -qE '^  node-cordoned +excluded +cordoned' "$T/status.out" && grep -qE '^  node-foreign +excluded +tainted example.com/team-x:NoExecute' "$T/status.out" && ok "status: a cordoned node and a node tainted against the preparer are listed as excluded, with why" || fail "status: excluded rows: $(grep -E 'node-cordoned|node-foreign' "$T/status.out" | tr '\n' ';')"
cp "$T/nodes.json" "$T/nodes-full.json"
jq '.items |= map(select(.metadata.name == "node-ready" or .metadata.name == "node-cordoned" or .metadata.name == "node-foreign"))' "$T/nodes-full.json" > "$T/nodes.json"
if PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/done.out" 2>&1; then grep -q '1/1 usable nodes prepared' "$T/done.out" && ok "status: every usable node prepared is a pass, unusable nodes aside" || fail "status complete: $(grep 'nodes prepared' "$T/done.out")"; else fail "status: a complete preparation failed on nodes no engine can use: $(grep -E 'nodes prepared|excluded' "$T/done.out" | tr '\n' ';')"; fi
cp "$T/nodes-full.json" "$T/nodes.json"
grep -q "claim engine-cache: Bound" "$T/status.out" && grep -q '^/mnt/local/weights/check-ns/engine-cache  (claim' "$T/status.out" && ok "status: reports the directory (off the DaemonSet's annotation) and the claim Bound" || fail "status: header: $(head -1 "$T/status.out")"
[ "$rc" -ne 0 ] && ok "status: exits non-zero while a node is not prepared" || fail "status: exit 0 with nodes not prepared"
grep -q 'stub kubectl:' "$T/status.out" && fail "status: a kubectl call lacked its scope: $(grep 'stub kubectl:' "$T/status.out" | head -1)" || ok "status: every kubectl call carried -n / -l"
STUB_LOG='mkdir: cannot create directory /engine-cache/vllm: Read-only file system' PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/eacces.out" 2>&1 || true
grep -q 'cannot write /mnt/local/weights/check-ns/engine-cache: Read-only file system in its log' "$T/eacces.out" && grep -q 'chgrp 0 DIR && chmod 2775 DIR && chcon -t container_file_t DIR' "$T/eacces.out" && ok "status: a preparer dying on a read-only or denied directory is named, with the node-directory steps" || fail "status EACCES diagnosis: $(grep -c 'cannot write' "$T/eacces.out") line(s)"
grep -q '^logs -n check-ns ec-failing --tail=20$' "$T/calls" && ok "status: the log read names the failing pod, no other" || fail "status: log read: $(grep '^logs' "$T/calls" | head -2 | tr '\n' ';')"
if STUB_DS_PATH="" PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/none.out" 2>&1; then fail "status with no DaemonSet must fail"; else grep -q 'no engine-cache DaemonSet' "$T/none.out" && ok "status: no DaemonSet is refused with a reason" || fail "status: $(tail -1 "$T/none.out")"; fi
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns >/dev/null 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: lists the nodes the DaemonSet was applied for (its live nodeSelector)" || fail "status: live selector not used: $(grep 'get nodes' "$T/calls")"
: > "$T/calls"
STUB_DS_SELECTOR="$SEL2" STUB_SELECTOR="$SEL2" PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/sel2-live.out" 2>&1 || true
grep -q "get nodes -l ${SEL2} " "$T/calls" && ok "status: every pair of the live nodeSelector reaches the node listing" || fail "status: two-key live selector read back as: $(grep 'get nodes' "$T/calls"); $(tail -1 "$T/sel2-live.out")"
cat > "$T/nodes-sel2.json" <<'EOF'
{"items":[
 {"metadata":{"name":"node-both",        "labels":{"example.com/accelerator":"h200","example.com/state":"production"}}},
 {"metadata":{"name":"node-accel-only",  "labels":{"example.com/accelerator":"h200"}}},
 {"metadata":{"name":"node-state-only",  "labels":{"example.com/state":"production"}}},
 {"metadata":{"name":"node-other-state", "labels":{"example.com/accelerator":"h200","example.com/state":"broken"}}}
]}
EOF
STUB_NODES_FILE="$T/nodes-sel2.json" STUB_SELECTOR="$SEL2" PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns --node-selector "$SEL2" > "$T/sel2-status.out" 2>&1 || true
grep -q '^  node-both ' "$T/sel2-status.out" && ! grep -Eq 'node-(accel-only|state-only|other-state)' "$T/sel2-status.out" \
    && ok "status: a node carrying only one of the pairs is not counted" || fail "status two-pair node rows: $(grep -o 'node-[a-z-]*' "$T/sel2-status.out" | sort -u | tr '\n' ' ')"
if STUB_DS_SELECTOR='a=b"c' PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/edited.out" 2>&1; then fail "status must refuse a DaemonSet nodeSelector that is not label characters"; else
    grep -q 'this script did not write' "$T/edited.out" && ok "status: a DaemonSet nodeSelector this script did not write is refused before kubectl" || fail "status on an edited DaemonSet: $(tail -1 "$T/edited.out")"; fi
if STUB_NODES_ERROR='Error from server (Forbidden): nodes is forbidden: User cannot list' PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/forb.out" 2>&1; then fail "status must fail when nodes cannot be listed"; else
    grep -q 'cluster-reader' "$T/forb.out" && ok "status: a Forbidden node list names the permission (cluster-reader)" || fail "status Forbidden: $(tail -1 "$T/forb.out")"; fi
if STUB_PV_STATE=absent STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/pending.out" 2>&1; then fail "status must fail while the claim is not Bound"; else grep -q "the claim is not bound: its volume ${PVNAME} is cluster-scoped and was not created" "$T/pending.out" && ok "status: an unbound claim whose volume is not there is blamed on leave to create it, and fails" || fail "status: unbound claim: $(head -2 "$T/pending.out" | tr '\n' ';')"; fi
# a claim whose volume exists is a binding delay, not missing rights; a
# volume that cannot be read is unknown, never absent
if STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/pendpv.out" 2>&1; then fail "status must fail while the claim is not Bound, even with its volume there"; else
    grep -q "the claim is not bound yet: its volume ${PVNAME} exists" "$T/pendpv.out" && ! grep -q 'leave to create' "$T/pendpv.out" \
        && ok "status: an unbound claim whose volume exists is a binding delay, with no blame on permissions" || fail "status: unbound claim, volume there: $(sed -n 2p "$T/pendpv.out")"; fi
if STUB_PV_STATE=forbidden STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash deploy/enginecache.sh status -n check-ns > "$T/pendforb.out" 2>&1; then fail "status must fail while the claim is not Bound, volume unreadable"; else
    grep -q "its volume ${PVNAME} could not be read (Error from server (Forbidden)" "$T/pendforb.out" && ! grep -q 'was not created' "$T/pendforb.out" \
        && ok "status: an unbound claim whose volume read is Forbidden says so, never that the volume is absent" || fail "status: unbound claim, volume Forbidden: $(sed -n 2p "$T/pendforb.out")"; fi
# apply waits briefly for the claim to bind before its report (sleep is a
# stand-in that returns at once, so the wait costs nothing here)
mkdir -p "$T/fastsleep"; printf '#!/bin/sh\nexit 0\n' > "$T/fastsleep/sleep"; chmod +x "$T/fastsleep/sleep"
rm -f "$T/bind.n"
STUB_BIND_AFTER=3 STUB_PVC_CLASS=node-local-engine-cache PATH="$T/fastsleep:$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" > "$T/latebind.out" 2>&1 || true
grep -q 'claim engine-cache: Bound' "$T/latebind.out" && ! grep -q 'not bound' "$T/latebind.out" \
    && ok "apply: a claim that binds a few seconds after the apply is reported Bound, not as a fault" || fail "apply, claim binding late: reads=$(cat "$T/bind.n" 2>/dev/null); $(grep -E 'claim engine-cache|not bound' "$T/latebind.out" | tr '\n' ';')"
rm -f "$T/bind.n"
STUB_BIND_AFTER=99 STUB_PVC_CLASS=node-local-engine-cache PATH="$T/fastsleep:$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" > "$T/slowbind.out" 2>&1 || true
[ "$(cat "$T/bind.n" 2>/dev/null || echo 0)" -ge 10 ] && grep -q "the claim is not bound yet: its volume ${PVNAME} exists" "$T/slowbind.out" && ! grep -q 'leave to create' "$T/slowbind.out" \
    && ok "apply: a claim still Pending after the wait, its volume there, is reported as not bound yet" || fail "apply, claim still Pending: reads=$(cat "$T/bind.n" 2>/dev/null); $(grep -E 'not bound' "$T/slowbind.out" | tr '\n' ';')"
rm -f "$T/bind.n"
# apply: refuses before anything lands without PV rights; refuses a foreign claim; the OpenShift notice
: > "$T/calls"
if STUB_NO_PV=1 STUB_PVC_PHASE="" PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" > "$T/nopv.out" 2>&1; then fail "apply must refuse without leave to create PersistentVolumes"; else
    grep -q 'leave to create PersistentVolumes' "$T/nopv.out" && ! grep -q '^apply -f' "$T/calls" && ok "apply: without leave to create PersistentVolumes nothing is applied" || fail "apply without PV rights: $(tail -1 "$T/nopv.out"); calls: $(grep '^apply' "$T/calls")"; fi
: > "$T/calls"
if STUB_PVC_CLASS=shared-vast PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" > "$T/foreign.out" 2>&1; then fail "apply must refuse a same-named claim on another class"; else
    grep -q "already has a claim named engine-cache on storage class 'shared-vast'" "$T/foreign.out" && ! grep -q '^apply -f' "$T/calls" && ok "apply: a claim named engine-cache on another class is refused, nothing applied" || fail "apply foreign claim: $(tail -1 "$T/foreign.out")"; fi
: > "$T/calls"
STUB_PVC_CLASS=node-local-engine-cache PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" > "$T/apply.out" 2>&1 || true
grep -q '^apply -f' "$T/calls" && grep -q 'preparing /mnt/local/weights/check-ns/engine-cache on' "$T/apply.out" && grep -q '^  node-ready ' "$T/apply.out" && ok "apply: applies, says what it prepared where, and reports per node" || fail "apply: $(head -3 "$T/apply.out" | tr '\n' ';')"
grep -q 'OpenShift:' "$T/apply.out" && fail "apply: the OpenShift notice printed on a cluster without SCCs" || ok "apply: no OpenShift notice on a cluster without SecurityContextConstraints"
# A re-apply keeps an existing claim's size: the claim is bound to a static
# volume, and only a dynamically provisioned claim can be resized.
claim_size() { yq -r 'select(.kind == "PersistentVolumeClaim") | .spec.resources.requests.storage' "$1" 2>/dev/null; }
EAPPLY=(apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG")
: > "$T/applied.yaml"
STUB_PVC_SIZE=150Gi STUB_PVC_CLASS=node-local-engine-cache STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/enginecache.sh "${EAPPLY[@]}" > "$T/keep.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 150Gi ] && [ "$(yq -r 'select(.kind == "PersistentVolume") | .spec.capacity.storage' "$T/applied.yaml")" = 150Gi ] && grep -q 'exists at 150Gi; keeping that size' "$T/keep.out" \
    && ok "re-apply: an existing 150Gi claim and no --capacity renders 150Gi on the claim and the volume" || fail "re-apply keep: '$(claim_size "$T/applied.yaml")'; $(head -2 "$T/keep.out" | tr '\n' ';')"
: > "$T/applied.yaml"
STUB_PVC_SIZE=150Gi STUB_PVC_CLASS=node-local-engine-cache STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/enginecache.sh "${EAPPLY[@]}" --capacity 150Gi > "$T/equal.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 150Gi ] && ok "re-apply: --capacity equal to the existing claim applies" || fail "re-apply equal: '$(claim_size "$T/applied.yaml")'; $(head -2 "$T/equal.out" | tr '\n' ';')"
: > "$T/calls"
if STUB_PVC_SIZE=150Gi STUB_PVC_CLASS=node-local-engine-cache PATH="$STUB_PATH" bash deploy/enginecache.sh "${EAPPLY[@]}" --capacity 300Gi > "$T/resize.out" 2>&1; then fail "re-apply must refuse a --capacity that differs from the existing claim"; else
    grep -q -- '--capacity 300Gi differs from the existing claim engine-cache in check-ns, which is 150Gi: a claim bound to a static (node-local) volume cannot be resized' "$T/resize.out" && grep -q 'enginecache.sh delete -n check-ns first' "$T/resize.out" && ! grep -q '^apply -f' "$T/calls" \
        && ok "re-apply: a different --capacity is refused before anything lands, saying why and what frees the claim" || fail "re-apply resize: $(tail -1 "$T/resize.out"); calls: $(grep '^apply' "$T/calls")"; fi
: > "$T/applied.yaml"
STUB_DS_PATH="" STUB_PVC_PHASE="" STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/enginecache.sh "${EAPPLY[@]}" > "$T/fresh.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 200Gi ] && ok "apply: no claim yet renders the default 200Gi" || fail "apply fresh: '$(claim_size "$T/applied.yaml")'; $(head -2 "$T/fresh.out" | tr '\n' ';')"
: > "$T/calls"
if STUB_PVC_READ_ERROR='Error from server (Forbidden): persistentvolumeclaims is forbidden: stub' STUB_PVC_CLASS=node-local-engine-cache PATH="$STUB_PATH" bash deploy/enginecache.sh "${EAPPLY[@]}" > "$T/pvcforb.out" 2>&1; then fail "apply must fail when the claim size cannot be read"; else
    grep -q 'cannot read the claim engine-cache in check-ns (kubectl: Error from server (Forbidden)' "$T/pvcforb.out" && ! grep -q '^apply -f' "$T/calls" \
        && ok "apply: a claim whose size cannot be read (Forbidden) is an error, not an absent claim; nothing applied" || fail "apply pvc Forbidden: $(tail -1 "$T/pvcforb.out"); calls: $(grep '^apply' "$T/calls")"; fi
# a re-apply with another directory is refused: the claim's volume cannot
# change -- read off the volume the claim is bound to, so a DaemonSet deleted
# by hand does not make the claim look free
: > "$T/calls"
if STUB_DS_PATH="" STUB_PVC_CLASS=node-local-engine-cache PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/other --image "$IMG" > "$T/moved.out" 2>&1; then fail "apply must refuse a second directory for a namespace that has one"; else
    grep -q "already has its engine cache at /mnt/local/weights/check-ns/engine-cache (volume ${PVNAME})" "$T/moved.out" && ! grep -q '^apply -f' "$T/calls" && ok "apply: a re-apply with another --path is refused before anything lands, off the claim's bound volume, DaemonSet or no DaemonSet" || fail "apply moved: $(tail -1 "$T/moved.out")"; fi
# a claim of the name with no class at all is not ours either
if STUB_DS_PATH="" STUB_PVC_CLASS="" PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" > "$T/noclass.out" 2>&1; then fail "apply must refuse an existing claim with no storage class"; else
    grep -q "already has a claim named engine-cache on storage class ''" "$T/noclass.out" && ok "apply: an existing claim on no class is refused too" || fail "apply no-class claim: $(tail -1 "$T/noclass.out")"; fi
STUB_DS_PATH="" STUB_PVC_PHASE="" STUB_OPENSHIFT=1 PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /var/mnt/weights/p --image "$IMG" > "$T/ocp.out" 2>&1 || true
grep -q 'OpenShift: the preparer and the engines run as the project UID' "$T/ocp.out" && grep -q 'chcon -t container_file_t /var/mnt/weights/p/engine-cache' "$T/ocp.out" && ok "apply: on a cluster with SecurityContextConstraints the node-directory steps are printed, with the cache directory" || fail "apply on OpenShift: $(grep -c OpenShift "$T/ocp.out") notice(s)"
# the seed claim must be Bound: the preparer would otherwise mount it and never start
: > "$T/calls"
if STUB_DS_PATH="" STUB_PVC_PHASE="" STUB_SEED_PHASE=Pending PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --seed-claim workload-pvc:engine-cache > "$T/seedp.out" 2>&1; then fail "apply must refuse a seed claim that is not Bound"; else
    grep -q "no Bound claim of that name" "$T/seedp.out" && ! grep -q '^apply -f' "$T/calls" && ok "apply: a seed claim that is not Bound is refused, nothing applied" || fail "apply seed Pending: $(tail -1 "$T/seedp.out")"; fi
: > "$T/calls"
STUB_DS_PATH="" STUB_PVC_PHASE="" PATH="$STUB_PATH" bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --seed-claim workload-pvc:engine-cache > "$T/seedok.out" 2>&1 || true
grep -q '^apply -f' "$T/calls" && grep -q 'seeded from workload-pvc:engine-cache' "$T/seedok.out" && ok "apply: with a Bound seed claim it applies and says what seeds it" || fail "apply with a seed: $(head -2 "$T/seedok.out" | tr '\n' ';')"
grep -q 'seed: none' "$T/status.out" && ok "status: reports no seed when none was applied" || fail "status: seed line: $(head -1 "$T/status.out")"
# delete: DaemonSet, claim and ServiceAccount in the namespace; the volume found
# by OUR labels and its claimRef into this namespace and claim, no other
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/enginecache.sh delete -n check-ns >/dev/null 2>&1
grep -q "^delete daemonset -n check-ns engine-cache --ignore-not-found" "$T/calls" && grep -q "^delete pvc -n check-ns engine-cache --ignore-not-found" "$T/calls" \
    && grep -q "^delete pv ${PVNAME} --ignore-not-found" "$T/calls" && grep -q "^delete serviceaccount -n check-ns engine-cache-preparer" "$T/calls" && ok "delete: DaemonSet, claim, ServiceAccount, and the volume whose claimRef is this namespace's engine-cache" || fail "delete issued: $(grep delete "$T/calls" | tr '\n' ';')"
grep -q '^get pvc' "$T/calls" && fail "delete: a claim was read for its volume name (a tenant writes that field)" || ok "delete: no claim is read for a volume name"
grep -q "^delete pv .*othernamespace" "$T/calls" && fail "delete: another namespace's volume was deleted" || ok "delete: another namespace's volume is left alone"
grep -q "^delete pv .*not-this-claim" "$T/calls" && fail "delete: a volume bound to another claim in this namespace was deleted" || ok "delete: a volume with our labels but another claimRef is left alone"
grep -q "^get pv -l ${LBL} -o json" "$T/calls" && ok "delete: the volumes are listed by our labels, cluster-scoped" || fail "delete: pv listing: $(grep '^get pv' "$T/calls")"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/enginecache.sh delete -n check-ns --dry-run > "$T/dry.out" 2>&1
grep -q '^delete ' "$T/calls" && fail "delete --dry-run deleted something" || ok "delete --dry-run: prints what would go, deletes nothing ($(grep -c 'would run' "$T/dry.out") lines)"

# ---------------------------------------------------------------------------
# 3c. verify and keys: the GPU-free cache check, against the same stub. The
# exec into a preparer runs the real verify script locally, on a fake node
# directory per pod ($T/ecv/<pod>/vllm), so what is asserted is what runs.
# ---------------------------------------------------------------------------
cat > "$T/exec-stub.sh" <<'EOF'
#!/usr/bin/env bash
# kubectl exec -n NS POD -c prepare -- /bin/sh -c WRAP sh SCRIPT ROOT KEY...
pod="$4"
case " ${STUB_EXEC_FORBIDDEN:-} " in *" $pod "*)
    echo "Error from server (Forbidden): pods \"$pod\" is forbidden: User \"tenant\" cannot create resource \"pods/exec\" in API group \"\" in the namespace \"check-ns\"" >&2; exit 1 ;; esac
case " ${STUB_EXEC_FAIL:-} " in *" $pod "*)
    echo "error: unable to upgrade connection: container not found (\"prepare\")" >&2; exit 1 ;; esac
# STUB_EXEC_EXIT="POD:CODE ...": the command in POD ended with CODE, as a
# timeout in the preparer ends it (kubectl exec passes the code through)
for x in ${STUB_EXEC_EXIT:-}; do case "$x" in "$pod:"*)
    echo "command terminated with exit code ${x#*:}" >&2; exit "${x#*:}" ;; esac; done
[ "$5 $6 $7 $8 $9" = "-c prepare -- /bin/sh -c" ] || { echo "stub kubectl: exec is not sh -c in the prepare container: $*" >&2; exit 2; }
[ "${11}" = sh ] && [ "${13}" = /engine-cache/vllm ] || { echo "stub kubectl: exec root is not /engine-cache/vllm: ${13}" >&2; exit 2; }
wrap="${10}"; script="${12}"
shift 13
# STUB_EXEC_PATH: the PATH inside the preparer (an image without find)
sh_bin="$(command -v "$STUB_SH")"
PATH="${STUB_EXEC_PATH:-$PATH}" exec "$sh_bin" -c "$wrap" sh "$script" "$STUB_EC_ROOT/$pod/vllm" "$@"
EOF
ECV_SH=sh; command -v dash >/dev/null 2>&1 && ECV_SH=dash
mkdir -p "$T/engine-logs" "$T/engine-root"
K1=torch_compile_cache/3f2a9c1d0e
K2=torch_compile_cache/77b0e1aa42
ecv_reset() {  # every node holds both keys, two ranks each
    rm -rf "$T/ecv"
    local p k r
    for p in ec-a ec-b ec-c ec-d; do for k in "$K1" "$K2"; do for r in rank_0_0 rank_1_0; do
        mkdir -p "$T/ecv/$p/vllm/$k/$r/backbone"; echo artefact > "$T/ecv/$p/vllm/$k/$r/backbone/model.py"
    done; done; done
    chmod -R u=rwX,go=rX "$T/ecv"  # the modes a preparer leaves, whatever the umask here
}
ecv_pod() {  # ecv_pod NAME NODE READY
    printf '{"metadata":{"name":"%s","labels":{"wva.llmd.ai/engine-cache":"engine-cache"}},"spec":{"nodeName":"%s"},"status":{"phase":"Running","containerStatuses":[{"ready":%s,"state":{"running":{}}}]}}' "$1" "$2" "$3"
}
ecv_node() { printf '{"metadata":{"name":"%s","labels":{"nvidia.com/gpu.product":"H200"}}}' "$1"; }
printf '{"items":[%s,%s,%s]}' "$(ecv_node n-a)" "$(ecv_node n-b)" "$(ecv_node n-c)" > "$T/ecv-nodes.json"
printf '{"items":[%s,%s,%s,%s]}' "$(ecv_pod ec-a n-a true)" "$(ecv_pod ec-b n-b true)" "$(ecv_pod ec-c n-c true)" \
    "$(ecv_pod other-ds n-a true | sed 's/"engine-cache":"engine-cache"/"engine-cache":"other"/')" > "$T/ecv-pods.json"
ecv() {  # ecv OUTFILE ARGS... -- verify against the fixture; prints the exit code
    local out="$1"; shift
    STUB_SH="$ECV_SH" STUB_EC_ROOT="$T/ecv" STUB_NODES_FILE="${ECV_NODES:-$T/ecv-nodes.json}" STUB_PODS_FILE="${ECV_PODS:-$T/ecv-pods.json}" \
        PATH="${ECV_PATH:-$STUB_PATH}" bash deploy/enginecache.sh verify -n check-ns "$@" > "$out" 2>&1
    echo $?
}
ecv_row() { grep -qE "^  $2 +$3 +${4//+/\\+}( |\$)" "$1"; }  # ecv_row FILE NODE RESULT KEY (a + in KEY is literal)
# The fixture's keys are owned by the runner, and without ENGINE_CACHE_UID
# they are judged for that owner, which the verdict names (VERIFIED for
# root, VERIFIED for uid 1000)
ME="$(id -u)"; OTHER=$((ME + 4242))
if [ "$ME" = 0 ]; then VOK='VERIFIED for root: every key is on every usable node'; else VOK="VERIFIED for uid ${ME}: every key is on every usable node"; fi
HINT='run again with ENGINE_CACHE_UID=$(kubectl exec -n check-ns <engine pod> -- id -u)'
# every node has every key: a pass, a bare hash taken as torch_compile_cache/<hash>
ecv_reset
rc="$(ecv "$T/ecv-all.out" --keys "3f2a9c1d0e,$K2")"
if [ "$rc" = 0 ] && grep -q '3 usable node(s) x 2 key(s): 6 present, 0 absent, 0 unreadable, 0 could not check' "$T/ecv-all.out" \
    && grep -q "$VOK" "$T/ecv-all.out" && ecv_row "$T/ecv-all.out" n-b present "$K1"; then
    ok "verify: every key on every node passes (6 of 6 present; a bare hash is torch_compile_cache/<hash>)"
else fail "verify all present: rc=$rc $(tail -3 "$T/ecv-all.out" | tr '\n' ';')"; fi
grep -q "^exec -n check-ns ec-a -c prepare -- /bin/sh -c " "$T/calls" && ! grep -q '^exec -n check-ns other-ds' "$T/calls" \
    && ok "verify: reads through our preparer pods only, in the prepare container" || fail "verify exec calls: $(grep '^exec' "$T/calls" | cut -c1-60 | tr '\n' ';')"
# one node lacks a key: a fail, naming the node and the key
ecv_reset; rm -rf "$T/ecv/ec-b/vllm/$K2"
rc="$(ecv "$T/ecv-miss.out" --keys "$K1,$K2")"
if [ "$rc" != 0 ] && ecv_row "$T/ecv-miss.out" n-b ABSENT "$K2" && ecv_row "$T/ecv-miss.out" n-b present "$K1" && ecv_row "$T/ecv-miss.out" n-a present "$K2" \
    && grep -q '5 present, 1 absent, 0 unreadable, 0 could not check' "$T/ecv-miss.out" && grep -q 'NOT VERIFIED' "$T/ecv-miss.out"; then
    ok "verify: a node missing a key fails, and names the node and the key (n-b $K2)"
else fail "verify one missing: rc=$rc $(grep -E 'n-b|present,' "$T/ecv-miss.out" | tr '\n' ';')"; fi
# one rank directory missing: the key with its ranks is absent, naming the
# rank; the plain key (any rank) still passes
ecv_reset; rm -rf "$T/ecv/ec-a/vllm/$K1/rank_1_0"
rc="$(ecv "$T/ecv-rank.out" --keys "$K1@rank_0_0+rank_1_0")"
if [ "$rc" != 0 ] && grep -qE "^  n-a +ABSENT +$K1@rank_0_0\+rank_1_0  \(rank directory missing or empty: rank_1_0\)" "$T/ecv-rank.out" \
    && ecv_row "$T/ecv-rank.out" n-b present "$K1@rank_0_0+rank_1_0" && grep -q '2 present, 1 absent, 0 unreadable, 0 could not check' "$T/ecv-rank.out"; then
    ok "verify: a key whose rank directory is missing on a node is absent there, naming the rank (n-a rank_1_0)"
else fail "verify missing rank: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-rank.out" | tr '\n' ';')"; fi
rc="$(ecv "$T/ecv-anyrank.out" --keys "$K1")"
[ "$rc" = 0 ] && ecv_row "$T/ecv-anyrank.out" n-a present "$K1" && ok "verify: a plain key (no ranks) is present with any rank directory" || fail "verify plain key, one rank: rc=$rc $(grep n-a "$T/ecv-anyrank.out")"
# a rank directory that holds no file is not that rank
ecv_reset; rm -rf "$T/ecv/ec-c/vllm/$K2/rank_1_0"; mkdir -p "$T/ecv/ec-c/vllm/$K2/rank_1_0/backbone"
rc="$(ecv "$T/ecv-rankempty.out" --keys "$K2@rank_0_0+rank_1_0")"
[ "$rc" != 0 ] && grep -qE "^  n-c +ABSENT +$K2@rank_0_0\+rank_1_0  \(rank directory missing or empty: rank_1_0\(empty\)\)" "$T/ecv-rankempty.out" \
    && ok "verify: a rank directory with no file in it is absent, naming the rank" || fail "verify empty rank: rc=$rc $(grep n-c "$T/ecv-rankempty.out")"
# an empty key directory is not a cache
ecv_reset; rm -rf "$T/ecv/ec-c/vllm/$K1"; mkdir -p "$T/ecv/ec-c/vllm/$K1/rank_0_0"
rc="$(ecv "$T/ecv-empty.out" --keys "$K1")"
[ "$rc" != 0 ] && grep -qE "^  n-c +ABSENT +$K1  \(an empty directory\)" "$T/ecv-empty.out" && ok "verify: a key directory with no file in it is absent" || fail "verify empty dir: rc=$rc $(grep n-c "$T/ecv-empty.out")"
# Forbidden exec: could not check, never absent
ecv_reset
rc="$(STUB_EXEC_FORBIDDEN=ec-c ecv "$T/ecv-forb.out" --keys "$K1,$K2")"
if [ "$rc" != 0 ] && ecv_row "$T/ecv-forb.out" n-c "COULD NOT CHECK" "$K1" && ecv_row "$T/ecv-forb.out" n-c "COULD NOT CHECK" "$K2" \
    && ! grep -qE '^  n-c +ABSENT' "$T/ecv-forb.out" && grep -q 'exec Forbidden: reading the cache needs pods/exec' "$T/ecv-forb.out" \
    && grep -q '4 present, 0 absent, 0 unreadable, 2 could not check' "$T/ecv-forb.out"; then
    ok "verify: a Forbidden exec is could-not-check on that node, never absent, and names the permission"
else fail "verify Forbidden: rc=$rc $(grep -E 'n-c|Forbidden|present,' "$T/ecv-forb.out" | tr '\n' ';')"; fi
# an exec that fails for another reason: could not check, with the reason
ecv_reset
rc="$(STUB_EXEC_FAIL=ec-a ecv "$T/ecv-xfail.out" --keys "$K1")"
[ "$rc" != 0 ] && ecv_row "$T/ecv-xfail.out" n-a "COULD NOT CHECK" "$K1" && grep -q 'exec into ec-a failed (exit 1): error: unable to upgrade connection' "$T/ecv-xfail.out" \
    && ok "verify: a failed exec is could-not-check, with the error" || fail "verify exec failure: rc=$rc $(grep -E 'n-a|exec' "$T/ecv-xfail.out" | tr '\n' ';')"
# a node whose preparer is not Ready, and one with no preparer at all: could not check
ecv_reset
printf '{"items":[%s,%s,%s]}' "$(ecv_node n-a)" "$(ecv_node n-d)" "$(ecv_node n-e)" > "$T/ecv-nodes2.json"
printf '{"items":[%s,%s]}' "$(ecv_pod ec-a n-a true)" "$(ecv_pod ec-d n-d false)" > "$T/ecv-pods2.json"
: > "$T/calls"
rc="$(ECV_NODES="$T/ecv-nodes2.json" ECV_PODS="$T/ecv-pods2.json" ecv "$T/ecv-nopod.out" --keys "$K1")"
if [ "$rc" != 0 ] && ecv_row "$T/ecv-nopod.out" n-d "COULD NOT CHECK" "$K1" && ecv_row "$T/ecv-nopod.out" n-e "COULD NOT CHECK" "$K1" \
    && grep -q 'no Ready preparer on n-d (Running (not ready))' "$T/ecv-nopod.out" && grep -q 'no Ready preparer on n-e (no pod)' "$T/ecv-nopod.out" \
    && ecv_row "$T/ecv-nopod.out" n-a present "$K1" && ! grep -q '^exec -n check-ns ec-d' "$T/calls"; then
    ok "verify: a node with no Ready preparer (not ready, or no pod) fails as could-not-check, and is not exec-ed into"
else fail "verify no Ready preparer: rc=$rc $(grep -E 'n-d|n-e' "$T/ecv-nopod.out" | tr '\n' ';')"; fi
# The modes are read for the engine's UID: the owner of the key directory
# (the engine that wrote it), or ENGINE_CACHE_UID. vLLM v0.31.0 writes its
# AOT files 600 under 755 key directories, all owned by the engine (measured
# on a cluster, as root): readable by that engine, so present -- this was
# UNREADABLE on every node when only the bits for other users were read.
AOTK=torch_compile_cache/torch_aot_compile/5d1e0c9b8a
ecv_aot() {  # every node holds AOTK, as vLLM writes it
    local p
    for p in ec-a ec-b ec-c; do
        mkdir -p "$T/ecv/$p/vllm/$AOTK/rank_0_0" "$T/ecv/$p/vllm/$AOTK/modelinfos"
        echo aot > "$T/ecv/$p/vllm/$AOTK/rank_0_0/model"; echo meta > "$T/ecv/$p/vllm/$AOTK/modelinfos/m.json"
        chmod 755 "$T/ecv/$p/vllm/$AOTK" "$T/ecv/$p/vllm/$AOTK/rank_0_0" "$T/ecv/$p/vllm/$AOTK/modelinfos"
        chmod 600 "$T/ecv/$p/vllm/$AOTK/rank_0_0/model" "$T/ecv/$p/vllm/$AOTK/modelinfos/m.json"
    done
}
ecv_reset; ecv_aot
rc="$(ecv "$T/ecv-aot600.out" --keys "$AOTK@rank_0_0")"
[ "$rc" = 0 ] && ecv_row "$T/ecv-aot600.out" n-a present "$AOTK@rank_0_0" && grep -q '3 usable node(s) x 1 key(s): 3 present, 0 absent, 0 unreadable, 0 could not check' "$T/ecv-aot600.out" \
    && grep -q "$VOK" "$T/ecv-aot600.out" && grep -q 'modes read for the owner of each key directory' "$T/ecv-aot600.out" \
    && grep -qE "^  n-a +present +$AOTK@rank_0_0  \(2 files; for uid ${ME}, the key directory's owner\)" "$T/ecv-aot600.out" \
    && ok "verify: vLLM's 600 AOT files under a 755 key directory, owned by the key's owner (uid ${ME}), are present, for that uid" || fail "verify AOT 600: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-aot600.out" | tr '\n' ';')"
# Without ENGINE_CACHE_UID each key is judged for its owner, which is the
# engines' UID only when they wrote it: a key owned by root (a seeded or
# published copy: the preparer copies as root, modes kept), or by another
# deployment's UID, passes for that owner. So the summary always warns, names
# the UIDs it assumed, and the verdict names them; a plain VERIFIED needs
# ENGINE_CACHE_UID.
grep -qF "WARNING: 3 of 3 node/key checks were judged for the owner of the key directory (uid ${ME}), taken as the engines' UID." "$T/ecv-aot600.out" \
    && grep -qF "$HINT" "$T/ecv-aot600.out" && ! grep -q 'different owners' "$T/ecv-aot600.out" \
    && ok "verify: keys owned by uid ${ME}, no ENGINE_CACHE_UID: a warning naming uid ${ME} (3 of 3 node/key checks), the id -u hint, and ${VOK%%:*}" \
    || fail "verify owner warning: $(grep -E 'WARNING|run again|VERIFIED' "$T/ecv-aot600.out" | tr '\n' ';')"
if [ "$ME" = 0 ]; then
    grep -q "A key owned by root (uid 0) is a seeded or published copy, or a root engine's" "$T/ecv-aot600.out" \
        && ok "verify: root-owned keys (seeded, 755 key, 600 files), no ENGINE_CACHE_UID: the warning says what a root key is" \
        || fail "verify root-owned warning: $(grep -E 'WARNING|root|VERIFIED' "$T/ecv-aot600.out" | tr '\n' ';')"
    rc="$(ENGINE_CACHE_UID=1000 ecv "$T/ecv-aotseed.out" --keys "$AOTK@rank_0_0")"
    [ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$AOTK@rank_0_0  \($AOTK/(rank_0_0/model|modelinfos/m\.json): not readable by uid 1000 \(ENGINE_CACHE_UID\)\)" "$T/ecv-aotseed.out" \
        && ! grep -q 'WARNING:' "$T/ecv-aotseed.out" \
        && ok "verify: the same root-owned keys with ENGINE_CACHE_UID=1000 are unreadable, naming uid 1000" || fail "verify seeded, uid 1000: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-aotseed.out" | tr '\n' ';')"
    rc="$(ENGINE_CACHE_UID=0 ecv "$T/ecv-aotroot.out" --keys "$AOTK@rank_0_0")"
    [ "$rc" = 0 ] && grep -q '^  VERIFIED: every key is on every usable node' "$T/ecv-aotroot.out" && ! grep -q 'WARNING:' "$T/ecv-aotroot.out" \
        && grep -qE "^  n-a +present +$AOTK@rank_0_0  \(2 files; for uid 0, ENGINE_CACHE_UID\)" "$T/ecv-aotroot.out" \
        && ok "verify: ENGINE_CACHE_UID=0 names root: VERIFIED, no warning" || fail "verify ENGINE_CACHE_UID=0: rc=$rc $(grep -E 'n-a|WARNING|VERIFIED' "$T/ecv-aotroot.out" | tr '\n' ';')"
    # a key owned by a non-root UID that may not be the engines' (another
    # deployment, or a changed image user): VERIFIED for that uid, warned
    ecv_reset; ecv_aot; for p in ec-a ec-b ec-c; do chown -R 1000:1000 "$T/ecv/$p/vllm/$AOTK"; done
    rc="$(ecv "$T/ecv-own1000.out" --keys "$AOTK@rank_0_0")"
    [ "$rc" = 0 ] && grep -q '^  VERIFIED for uid 1000: every key is on every usable node, readable by uid 1000' "$T/ecv-own1000.out" \
        && grep -qF "WARNING: 3 of 3 node/key checks were judged for the owner of the key directory (uid 1000)" "$T/ecv-own1000.out" \
        && grep -qF "$HINT" "$T/ecv-own1000.out" && ! grep -q 'A key owned by root' "$T/ecv-own1000.out" && ! grep -q 'different owners' "$T/ecv-own1000.out" \
        && ok "verify: keys owned by uid 1000, no ENGINE_CACHE_UID: VERIFIED for uid 1000, a warning and the id -u hint" \
        || fail "verify owner 1000: rc=$rc $(grep -E 'WARNING|run again|VERIFIED' "$T/ecv-own1000.out" | tr '\n' ';')"
    rc="$(ENGINE_CACHE_UID=1000 ecv "$T/ecv-own1000u.out" --keys "$AOTK@rank_0_0")"
    [ "$rc" = 0 ] && grep -q '^  VERIFIED: every key is on every usable node$' "$T/ecv-own1000u.out" && ! grep -q 'WARNING:' "$T/ecv-own1000u.out" \
        && ok "verify: the same keys with ENGINE_CACHE_UID=1000: a plain VERIFIED, no warning" \
        || fail "verify owner 1000, ENGINE_CACHE_UID=1000: rc=$rc $(grep -E 'WARNING|VERIFIED' "$T/ecv-own1000u.out" | tr '\n' ';')"
    # the same key owned by 1000 on one node and 2000 on the others: both
    # named, and said plainly that one of them is the wrong user
    chown -R 2000:2000 "$T/ecv/ec-b/vllm/$AOTK" "$T/ecv/ec-c/vllm/$AOTK"
    rc="$(ecv "$T/ecv-ownmix.out" --keys "$AOTK@rank_0_0")"
    [ "$rc" = 0 ] && grep -q '^  VERIFIED for uids 1000, 2000: every key is on every usable node, each readable by the owner of its key directory' "$T/ecv-ownmix.out" \
        && grep -qF "WARNING: 3 of 3 node/key checks were judged for the owner of the key directory (uids 1000, 2000)" "$T/ecv-ownmix.out" \
        && grep -qF "The keys have 2 different owners. The engines of one configuration all run as one UID, so at least one line was judged for the wrong user" "$T/ecv-ownmix.out" \
        && grep -qF "$HINT" "$T/ecv-ownmix.out" \
        && ok "verify: one key owned by 1000 on one node and 2000 on two: VERIFIED for uids 1000, 2000, and at least one line judged for the wrong user" \
        || fail "verify mixed owners across nodes: rc=$rc $(grep -E 'WARNING|owners|VERIFIED' "$T/ecv-ownmix.out" | tr '\n' ';')"
    ecv_reset; ecv_aot  # back to keys the runner owns
fi
rc="$(ENGINE_CACHE_UID="$ME" ecv "$T/ecv-aotme.out" --keys "$AOTK@rank_0_0")"
[ "$rc" = 0 ] && grep -q '3 present, 0 absent, 0 unreadable' "$T/ecv-aotme.out" && grep -q "modes read for uid ${ME} (ENGINE_CACHE_UID)" "$T/ecv-aotme.out" \
    && grep -q '^  VERIFIED: every key is on every usable node$' "$T/ecv-aotme.out" && ! grep -q 'WARNING:' "$T/ecv-aotme.out" \
    && ok "verify: ENGINE_CACHE_UID equal to the owner of the 600 files (${ME}) is present: a plain VERIFIED, no warning" || fail "verify ENGINE_CACHE_UID=owner: rc=$rc $(grep -E 'n-a|present,|modes|VERIFIED|WARNING' "$T/ecv-aotme.out" | tr '\n' ';')"
rc="$(ENGINE_CACHE_UID="$OTHER" ecv "$T/ecv-aotother.out" --keys "$AOTK@rank_0_0")"
[ "$rc" != 0 ] && grep -qE "^  n-b +UNREADABLE +$AOTK@rank_0_0  \($AOTK/(rank_0_0/model|modelinfos/m\.json): not readable by uid $OTHER \(ENGINE_CACHE_UID\)\)" "$T/ecv-aotother.out" \
    && grep -q '0 present, 0 absent, 3 unreadable, 0 could not check' "$T/ecv-aotother.out" && grep -q 'NOT VERIFIED' "$T/ecv-aotother.out" \
    && ok "verify: ENGINE_CACHE_UID=${OTHER}, not the owner of the 600 files, is unreadable on every node, naming the uid" || fail "verify ENGINE_CACHE_UID=other: rc=$rc $(grep -E 'n-b|present,' "$T/ecv-aotother.out" | tr '\n' ';')"
rc="$(ENGINE_CACHE_UID="00${ME}" ecv "$T/ecv-aotzeros.out" --keys "$AOTK@rank_0_0")"
[ "$rc" = 0 ] && grep -q "modes read for uid ${ME} (ENGINE_CACHE_UID)" "$T/ecv-aotzeros.out" \
    && ok "verify: ENGINE_CACHE_UID with leading zeros is that uid, not octal" || fail "verify ENGINE_CACHE_UID=00${ME}: rc=$rc $(grep -E 'modes|present,' "$T/ecv-aotzeros.out" | tr '\n' ';')"
for bad in abc 12a -1 1.5 99999999999 4294967295; do
    : > "$T/calls"
    rc="$(ENGINE_CACHE_UID="$bad" ecv "$T/ecv-baduid.out" --keys "$K1")"
    [ "$rc" != 0 ] && grep -q "ENGINE_CACHE_UID must be a" "$T/ecv-baduid.out" && ! grep -q '^exec' "$T/calls" \
        && ok "verify: ENGINE_CACHE_UID='${bad}' is refused before any exec" || fail "verify ENGINE_CACHE_UID='${bad}': rc=$rc $(tail -1 "$T/ecv-baduid.out")"
done
# the default UID is the key directory's owner: a 600 file the runner owns
# is present; under ENGINE_CACHE_UID=another, unreadable
ecv_reset; chmod 600 "$T/ecv/ec-a/vllm/$K1/rank_1_0/backbone/model.py"
rc="$(ecv "$T/ecv-mode.out" --keys "$K1")"
[ "$rc" = 0 ] && grep -q '3 present, 0 absent, 0 unreadable, 0 could not check' "$T/ecv-mode.out" \
    && ok "verify: a 600 file owned by the key directory's owner is present" || fail "verify mode 600, owner: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-mode.out" | tr '\n' ';')"
rc="$(ENGINE_CACHE_UID="$OTHER" ecv "$T/ecv-mode2.out" --keys "$K1")"
[ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$K1  \($K1/rank_1_0/backbone/model.py: not readable by uid $OTHER \(ENGINE_CACHE_UID\)\)" "$T/ecv-mode2.out" \
    && grep -q '2 present, 0 absent, 1 unreadable, 0 could not check' "$T/ecv-mode2.out" \
    && ok "verify: a 600 file another UID (ENGINE_CACHE_UID) would run as is unreadable, naming the file and the uid" || fail "verify mode 600, other uid: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-mode2.out" | tr '\n' ';')"
ecv_reset; chmod 700 "$T/ecv/ec-b/vllm/$K2/rank_0_0"
rc="$(ecv "$T/ecv-dmode.out" --keys "$K2")"
[ "$rc" = 0 ] && ecv_row "$T/ecv-dmode.out" n-b present "$K2" \
    && ok "verify: a 700 directory owned by the key directory's owner is present" || fail "verify dir mode 700, owner: rc=$rc $(grep n-b "$T/ecv-dmode.out")"
rc="$(ENGINE_CACHE_UID="$OTHER" ecv "$T/ecv-dmode2.out" --keys "$K2")"
[ "$rc" != 0 ] && grep -qE "^  n-b +UNREADABLE +$K2  \($K2/rank_0_0: not readable by uid $OTHER \(ENGINE_CACHE_UID\)\)" "$T/ecv-dmode2.out" \
    && ok "verify: a 700 directory another UID cannot search is unreadable, naming it and the uid" || fail "verify dir mode 700, other uid: rc=$rc $(grep n-b "$T/ecv-dmode2.out")"
# mixed owners under one key, with the default UID: needs files owned by
# other UIDs, so a root runner. The key directory is uid/gid 4243; a file
# owned by 4242 is readable through the group when it is in group 4243 and
# g+r, through other when it is not -- and never through the bit of the
# class it is not in.
if [ "$ME" != 0 ]; then ok "verify: (the mixed-owner cases need a root check runner)"; else
    mixed() {  # mixed OWNER:GROUP MODE WANT -- the file under ec-a K1, the rest 4243:4243
        ecv_reset; chown -R 4243:4243 "$T/ecv/ec-a/vllm/$K1"
        local f="$T/ecv/ec-a/vllm/$K1/rank_1_0/backbone/model.py"
        chown "$1" "$f"; chmod "$2" "$f"
        rc="$(ecv "$T/ecv-mixed.out" --keys "$K1")"
        if [ "$3" = present ]; then
            [ "$rc" = 0 ] && ecv_row "$T/ecv-mixed.out" n-a present "$K1" && ok "verify: a $2 file owned by $1 under a 4243:4243 key is present" || fail "verify mixed $1 $2: rc=$rc $(grep n-a "$T/ecv-mixed.out")"
        else
            [ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$K1  \($K1/rank_1_0/backbone/model.py: not readable by uid 4243, the owner of the key directory" "$T/ecv-mixed.out" \
                && ok "verify: a $2 file owned by $1 under a 4243:4243 key is unreadable for uid 4243" || fail "verify mixed $1 $2: rc=$rc $(grep n-a "$T/ecv-mixed.out")"
        fi
    }
    mixed 4242:4243 640 present
    mixed 4242:4243 604 unreadable
    mixed 4242:4244 640 unreadable
    mixed 4242:4244 604 present
    mixed 4242:4244 600 unreadable
    mixed 4243:4244 600 present
    mixed 4243:4244 044 unreadable
    ecv_reset; chown -R 4243:4243 "$T/ecv/ec-a/vllm/$K1"; chown 4242:4244 "$T/ecv/ec-a/vllm/$K1/rank_1_0"; chmod 750 "$T/ecv/ec-a/vllm/$K1/rank_1_0"
    rc="$(ecv "$T/ecv-mixeddir.out" --keys "$K1")"
    [ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$K1  \($K1/rank_1_0: not readable by uid 4243" "$T/ecv-mixeddir.out" \
        && ok "verify: a 750 directory owned by another uid and group under the key is unreadable for the key's owner" || fail "verify mixed dir: rc=$rc $(grep n-a "$T/ecv-mixeddir.out")"
fi
# a preparer whose find takes no -user cannot read the owners: could not check
mkdir -p "$T/nofinduser"
printf '#!/bin/sh\ncase " $* " in *" -user "*) echo "find: unrecognized: -user" >&2; exit 1 ;; esac\nexec %s "$@"\n' "$(command -v find)" > "$T/nofinduser/find"; chmod +x "$T/nofinduser/find"
ecv_reset
rc="$(STUB_EXEC_PATH="$T/nofinduser:$PATH" ecv "$T/ecv-nofinduser.out" --keys "$K1")"
[ "$rc" != 0 ] && grep -qE "^  n-a +COULD NOT CHECK +$K1  \(the preparer image's find cannot run the mode check \(it takes no -user\)\)" "$T/ecv-nofinduser.out" && grep -q '0 present, 0 absent, 0 unreadable, 3 could not check' "$T/ecv-nofinduser.out" \
    && ok "verify: a find without -user is could-not-check, never present" || fail "verify find without -user: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-nofinduser.out" | tr '\n' ';')"
# a find that takes -user but rejects -group or -perm: the whole expression
# is probed first, so this is could-not-check too -- an expression find
# refuses prints nothing, which read as every file readable (present)
for prim in group perm; do
    mkdir -p "$T/nofind$prim"
    printf '#!/bin/sh\ncase " $* " in *" -%s "*) echo "find: unrecognized: -%s" >&2; exit 1 ;; esac\nexec %s "$@"\n' "$prim" "$prim" "$(command -v find)" > "$T/nofind$prim/find"; chmod +x "$T/nofind$prim/find"
    ecv_reset
    rc="$(STUB_EXEC_PATH="$T/nofind$prim:$PATH" ecv "$T/ecv-nofind$prim.out" --keys "$K1")"
    [ "$rc" != 0 ] && grep -qE "^  n-a +COULD NOT CHECK +$K1  \(the preparer image's find cannot run the mode check \(it takes no -$prim\)\)" "$T/ecv-nofind$prim.out" \
        && grep -q '0 present, 0 absent, 0 unreadable, 3 could not check' "$T/ecv-nofind$prim.out" \
        && ok "verify: a find without -$prim is could-not-check, never present" || fail "verify find without -$prim: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-nofind$prim.out" | tr '\n' ';')"
done
rc="$(ENGINE_CACHE_UID="$OTHER" STUB_EXEC_PATH="$T/nofindperm:$PATH" ecv "$T/ecv-nofindperm2.out" --keys "$K1")"
[ "$rc" != 0 ] && grep -qE "^  n-a +COULD NOT CHECK +$K1  \(the preparer image's find cannot run the mode check \(it takes no -perm\)\)" "$T/ecv-nofindperm2.out" \
    && ok "verify: a find without -perm is could-not-check under ENGINE_CACHE_UID too" || fail "verify find without -perm, ENGINE_CACHE_UID: rc=$rc $(grep n-a "$T/ecv-nofindperm2.out")"
# the directories on the way to the key are read too: one the engine's UID
# cannot search hides every file under it
ecv_reset; ecv_aot; chmod 700 "$T/ecv/ec-b/vllm/torch_compile_cache/torch_aot_compile"
rc="$(ecv "$T/ecv-anc.out" --keys "$AOTK@rank_0_0")"
[ "$rc" = 0 ] && ecv_row "$T/ecv-anc.out" n-b present "$AOTK@rank_0_0" \
    && ok "verify: a 700 parent directory owned by the key's owner is present" || fail "verify parent 700, owner: rc=$rc $(grep n-b "$T/ecv-anc.out")"
rc="$(ENGINE_CACHE_UID="$OTHER" ecv "$T/ecv-anc2.out" --keys "$AOTK@rank_0_0")"
[ "$rc" != 0 ] && grep -qE "^  n-b +UNREADABLE +$AOTK@rank_0_0  \(torch_compile_cache/torch_aot_compile: not readable by uid $OTHER \(ENGINE_CACHE_UID\)\)" "$T/ecv-anc2.out" \
    && ok "verify: a 700 parent directory another UID cannot search is unreadable, naming the directory" || fail "verify parent 700, other uid: rc=$rc $(grep n-b "$T/ecv-anc2.out")"
ecv_reset; ecv_aot; chmod 700 "$T/ecv/ec-c/vllm"
rc="$(ENGINE_CACHE_UID="$OTHER" ecv "$T/ecv-anc3.out" --keys "$AOTK@rank_0_0")"
chmod 755 "$T/ecv/ec-c/vllm"
[ "$rc" != 0 ] && grep -qE "^  n-c +UNREADABLE +$AOTK@rank_0_0  \($T/ecv/ec-c/vllm: not readable by uid $OTHER \(ENGINE_CACHE_UID\)\)" "$T/ecv-anc3.out" \
    && ok "verify: a cache root another UID cannot search is unreadable, naming it" || fail "verify cache root 700: rc=$rc $(grep n-c "$T/ecv-anc3.out")"
# the volume mount above the cache root (/engine-cache; here the pod
# directory) is on the way too
ecv_reset; ecv_aot; chmod 700 "$T/ecv/ec-c"
rc="$(ENGINE_CACHE_UID="$OTHER" ecv "$T/ecv-anc5.out" --keys "$AOTK@rank_0_0")"
chmod 755 "$T/ecv/ec-c"
[ "$rc" != 0 ] && grep -qE "^  n-c +UNREADABLE +$AOTK@rank_0_0  \($T/ecv/ec-c: not readable by uid $OTHER \(ENGINE_CACHE_UID\)\)" "$T/ecv-anc5.out" \
    && ecv_row "$T/ecv-anc5.out" n-c UNREADABLE "$AOTK@rank_0_0" \
    && ok "verify: a 700 volume mount above the cache root another UID cannot search is unreadable, naming it" || fail "verify mount root 700: rc=$rc $(grep n-c "$T/ecv-anc5.out")"
# the directories above a key are only passed through: search is enough, so
# a 711 volume mount another UID cannot list is no reason to call it unreadable
ecv_reset; chmod 711 "$T/ecv/ec-c" "$T/ecv/ec-c/vllm"
rc="$(ENGINE_CACHE_UID="$OTHER" ecv "$T/ecv-anc711.out" --keys "$K1")"
chmod 755 "$T/ecv/ec-c" "$T/ecv/ec-c/vllm"
[ "$rc" = 0 ] && ecv_row "$T/ecv-anc711.out" n-c present "$K1" && grep -q '^  VERIFIED: every key is on every usable node$' "$T/ecv-anc711.out" \
    && ok "verify: a 711 volume mount and cache root above a readable key are present for another UID" || fail "verify mount root 711, other uid: rc=$rc $(grep -E 'n-c|VERIFIED' "$T/ecv-anc711.out" | tr '\n' ';')"
if [ "$ME" = 0 ]; then
    # a root 711 mount root above keys owned by 1000, judged for their owner
    ecv_reset; ecv_aot; for p in ec-a ec-b ec-c; do chown -R 1000:1000 "$T/ecv/$p/vllm/$AOTK"; done; chmod 711 "$T/ecv/ec-a"
    rc="$(ecv "$T/ecv-anc711r.out" --keys "$AOTK@rank_0_0")"
    chmod 755 "$T/ecv/ec-a"
    [ "$rc" = 0 ] && ecv_row "$T/ecv-anc711r.out" n-a present "$AOTK@rank_0_0" \
        && grep -q '^  VERIFIED for uid 1000: every key is on every usable node, readable by uid 1000' "$T/ecv-anc711r.out" \
        && ok "verify: a root 711 volume mount above a 1000:1000 key is present, VERIFIED for uid 1000" \
        || fail "verify root 711 mount root, owner 1000: rc=$rc $(grep -E 'n-a|VERIFIED' "$T/ecv-anc711r.out" | tr '\n' ';')"
fi
if [ "$ME" = 0 ]; then
    # a root 700 mount root above keys owned by 1000, judged for their owner
    ecv_reset; ecv_aot; chown -R 1000:1000 "$T/ecv/ec-a/vllm/$AOTK"; chmod 700 "$T/ecv/ec-a"
    rc="$(ecv "$T/ecv-anc6.out" --keys "$AOTK@rank_0_0")"
    chmod 755 "$T/ecv/ec-a"
    [ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$AOTK@rank_0_0  \($T/ecv/ec-a: not readable by uid 1000, the owner of the key directory" "$T/ecv-anc6.out" \
        && ok "verify: a root 700 volume mount above a 1000:1000 key is unreadable for uid 1000, naming the mount" \
        || fail "verify root 700 mount root, owner 1000: rc=$rc $(grep n-a "$T/ecv-anc6.out")"
fi
if [ "$ME" = 0 ]; then
    # the reviewer's case: torch_aot_compile root 700, the key 1000:1000
    ecv_reset; ecv_aot; chown -R 1000:1000 "$T/ecv/ec-a/vllm/$AOTK"; chmod 700 "$T/ecv/ec-a/vllm/torch_compile_cache/torch_aot_compile"
    for uidset in "" 1000; do
        rc="$(ENGINE_CACHE_UID="$uidset" ecv "$T/ecv-anc4.out" --keys "$AOTK@rank_0_0")"
        [ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$AOTK@rank_0_0  \(torch_compile_cache/torch_aot_compile: not readable by uid 1000" "$T/ecv-anc4.out" \
            && ok "verify: a root 700 torch_aot_compile above a 1000:1000 key is unreadable for uid 1000 (ENGINE_CACHE_UID='${uidset}')" \
            || fail "verify root 700 parent, ENGINE_CACHE_UID='${uidset}': rc=$rc $(grep n-a "$T/ecv-anc4.out")"
    done
fi
# the make target passes ENGINE_CACHE_UID through only when it is set
if env -u ENGINE_CACHE_UID make -n engine-cache-verify NAMESPACE=ns ENGINE_CACHE_KEYS=k ENGINE_CACHE_UID=1001 2>/dev/null | grep -q 'ENGINE_CACHE_UID="1001" bash deploy/enginecache.sh verify' \
    && ! env -u ENGINE_CACHE_UID make -n engine-cache-verify NAMESPACE=ns ENGINE_CACHE_KEYS=k 2>/dev/null | grep -q 'ENGINE_CACHE_UID='; then
    ok "engine-cache-verify: ENGINE_CACHE_UID reaches the script when set, and only then"
else fail "engine-cache-verify: ENGINE_CACHE_UID pass-through: $(env -u ENGINE_CACHE_UID make -n engine-cache-verify NAMESPACE=ns ENGINE_CACHE_KEYS=k ENGINE_CACHE_UID=1001 2>&1 | tail -1)"; fi
# a file no one but root can read: as root the read passes and the mode
# check names it (owner read is missing); as any other runner the read
# itself fails -- unreadable either way, never present
ecv_reset; chmod 000 "$T/ecv/ec-a/vllm/$K1/rank_1_0/backbone/model.py"
rc="$(ecv "$T/ecv-unread.out" --keys "$K1")"
chmod 644 "$T/ecv/ec-a/vllm/$K1/rank_1_0/backbone/model.py"
if [ "$ME" = 0 ]; then why000='not readable by uid 0, the owner of the key directory \(taken as the engine.s UID; ENGINE_CACHE_UID names another\)'; else why000='cannot be read'; fi
[ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$K1  \($K1/rank_1_0/backbone/model.py: $why000\)" "$T/ecv-unread.out" \
    && ok "verify: a file with mode 000 is unreadable, naming the file (runner uid $(id -u): ${why000//\\/})" || fail "verify unreadable: rc=$rc $(grep n-a "$T/ecv-unread.out")"
# vLLM writes no symbolic link: one under the key is unreadable, never
# followed or skipped -- a link to a file, a dangling link, and a directory
# on the way to the key
ecv_reset
f="$T/ecv/ec-b/vllm/$K1/rank_1_0/backbone/model.py"; mv "$f" "$T/elsewhere.py"; ln -s "$T/elsewhere.py" "$f"
rc="$(ecv "$T/ecv-links.out" --keys "$K1")"
[ "$rc" != 0 ] && grep -qE "^  n-b +UNREADABLE +$K1  \($K1/rank_1_0/backbone/model.py: a symlink under the cache; vLLM writes none\)" "$T/ecv-links.out" \
    && grep -q '2 present, 0 absent, 1 unreadable, 0 could not check' "$T/ecv-links.out" \
    && ok "verify: a symbolic link to a file under the key is unreadable, naming it" || fail "verify symlink: rc=$rc $(grep -E 'n-b|present,' "$T/ecv-links.out" | tr '\n' ';')"
ecv_reset; ln -s "$T/no-such-file" "$T/ecv/ec-a/vllm/$K2/rank_0_0/backbone/extra.py"
rc="$(ecv "$T/ecv-dangling.out" --keys "$K2")"
[ "$rc" != 0 ] && grep -qE "^  n-a +UNREADABLE +$K2  \($K2/rank_0_0/backbone/extra.py: a symlink under the cache" "$T/ecv-dangling.out" \
    && ok "verify: a dangling symbolic link under the key is unreadable, not skipped" || fail "verify dangling symlink: rc=$rc $(grep n-a "$T/ecv-dangling.out")"
ecv_reset; mv "$T/ecv/ec-c/vllm/torch_compile_cache" "$T/elsewhere-tcc"; ln -s "$T/elsewhere-tcc" "$T/ecv/ec-c/vllm/torch_compile_cache"
rc="$(ecv "$T/ecv-dirlink.out" --keys "$K1@rank_0_0+rank_1_0")"
rm -rf "$T/elsewhere-tcc"
[ "$rc" != 0 ] && grep -qE "^  n-c +UNREADABLE +$K1@rank_0_0\+rank_1_0  \(torch_compile_cache: a symlink under the cache; vLLM writes none\)" "$T/ecv-dirlink.out" \
    && ok "verify: a symlinked directory on the way to the key is unreadable, not followed" || fail "verify symlinked component: rc=$rc $(grep n-c "$T/ecv-dirlink.out")"
# the read runs under a time limit on both sides of the exec: timeout 300
# here, and the preparer's own timeout around the script inside the pod
ecv_reset; mkdir -p "$T/tbin"
printf '#!/bin/sh\necho "$*" | cut -c1-24 >> "%s"\nshift\nexec "$@"\n' "$T/timeout.calls" > "$T/tbin/timeout"; chmod +x "$T/tbin/timeout"
: > "$T/timeout.calls"
rc="$(ECV_PATH="$T/tbin:$STUB_PATH" ecv "$T/ecv-timeout.out" --keys "$K1")"
[ "$rc" = 0 ] && grep -q '^300 kubectl exec' "$T/timeout.calls" && grep -q '^280 /bin/sh -c' "$T/timeout.calls" \
    && ok "verify: timeout 300 around the exec here, timeout 280 around the script in the preparer" || fail "verify time limits: rc=$rc calls=$(tr '\n' ';' < "$T/timeout.calls")"
# no timeout here (stock macOS): a warning, and the check still runs
mkdir -p "$T/notimeout"
IFS=: read -r -a pdirs <<< "$STUB_PATH"
for d in "${pdirs[@]}"; do for f in "$d"/*; do
    # an empty PATH directory leaves "$d/*" unexpanded: no command, skip it
    # (linked, it was a dangling "*" that every later empty directory tried
    # to link again: "ln: ... File exists")
    [ -e "$f" ] || continue
    b="${f##*/}"; [ "$b" = timeout ] || [ -e "$T/notimeout/$b" ] || ln -s "$f" "$T/notimeout/$b"
done; done
[ ! -e "$T/notimeout/timeout" ] || fail "verify no timeout: the stripped PATH still has timeout"
rc="$(ECV_PATH="$T/notimeout" ecv "$T/ecv-notimeout.out" --keys "$K1")"
[ "$rc" = 0 ] && grep -q 'no timeout command here (stock macOS has none' "$T/ecv-notimeout.out" \
    && ok "verify: without a timeout command it warns there is no limit on this side, and still checks" || fail "verify no timeout: rc=$rc $(grep -iE 'timeout|VERIFIED' "$T/ecv-notimeout.out" | tr '\n' ';')"
# the preparer's timeout ends the read: 124 from GNU timeout is timed out;
# 143 or 137 (the TERM or KILL status of the script) is busybox timeout OR a
# killed or rolled preparer, so it says both -- each is could not check, never absent
for code in 124 143 137; do
    rc="$(STUB_EXEC_EXIT="ec-b:$code" ecv "$T/ecv-rto-$code.out" --keys "$K1")"
    if [ "$code" = 124 ]; then want='the read on ec-b timed out (280 s in the preparer, 300 s here)'
    else want="the read on ec-b timed out or was killed (exit ${code})"; fi
    if [ "$rc" != 0 ] && ecv_row "$T/ecv-rto-$code.out" n-b "COULD NOT CHECK" "$K1" && grep -qF "$want" "$T/ecv-rto-$code.out" \
        && ! grep -q ABSENT "$T/ecv-rto-$code.out" && grep -q '2 present, 0 absent, 0 unreadable, 1 could not check' "$T/ecv-rto-$code.out"; then
        ok "verify: a read the preparer ends with exit ${code} is timed out, could not check"
    else fail "verify remote exit ${code}: rc=$rc $(grep -E 'n-b|ec-b|present,' "$T/ecv-rto-$code.out" | tr '\n' ';')"; fi
done
# a preparer image with no find cannot read the modes: could not check, never present
mkdir -p "$T/nofind"
for f in "$T/notimeout"/*; do b="${f##*/}"; [ "$b" = find ] || [ -e "$T/nofind/$b" ] || ln -s "$(readlink "$f")" "$T/nofind/$b"; done
[ ! -e "$T/nofind/find" ] || fail "verify no find: the stripped PATH still has find"
rc="$(STUB_EXEC_PATH="$T/nofind" ecv "$T/ecv-nofind.out" --keys "$K1")"
[ "$rc" != 0 ] && grep -qE "^  n-a +COULD NOT CHECK +$K1  \(the preparer image has no find to read file modes with\)" "$T/ecv-nofind.out" \
    && grep -q '0 present, 0 absent, 0 unreadable, 3 could not check' "$T/ecv-nofind.out" && grep -q 'NOT VERIFIED' "$T/ecv-nofind.out" \
    && ok "verify: a preparer image with no find is could-not-check on every node, never present" || fail "verify no find: rc=$rc $(grep -E 'n-a|present,' "$T/ecv-nofind.out" | tr '\n' ';')"
# the preparer pods cannot be listed: an error, nothing called absent
rc="$(STUB_PODS_ERROR='Error from server (Forbidden): pods is forbidden: stub' ecv "$T/ecv-podsforb.out" --keys "$K1")"
[ "$rc" != 0 ] && grep -q 'cannot list the engine-cache preparer pods in check-ns: Error from server (Forbidden)' "$T/ecv-podsforb.out" && grep -q 'nothing was checked' "$T/ecv-podsforb.out" && ! grep -q ABSENT "$T/ecv-podsforb.out" \
    && ok "verify: preparer pods that cannot be listed are an error, nothing called absent" || fail "verify pods Forbidden: rc=$rc $(tail -1 "$T/ecv-podsforb.out")"
# the multi-pair selector reaches the node listing, as for status
: > "$T/calls"
STUB_DS_SELECTOR="$SEL2" STUB_SELECTOR="$SEL2" STUB_SH="$ECV_SH" STUB_EC_ROOT="$T/ecv" STUB_PODS_FILE="$T/ecv-pods.json" PATH="$STUB_PATH" \
    bash deploy/enginecache.sh verify -n check-ns --keys "$K1" > "$T/ecv-sel2.out" 2>&1
grep -q "get nodes -l ${SEL2} " "$T/calls" && ok "verify: the live two-pair nodeSelector picks the nodes" || fail "verify selector: $(grep 'get nodes' "$T/calls")"
refuse "verify without --keys" verify -n check-ns
refuse "a key with '..'" verify -n check-ns --keys "torch_compile_cache/../x"
refuse "an absolute key" verify -n check-ns --keys /engine-cache/vllm/x
refuse "a key with a space" verify -n check-ns --keys "a b"
refuse "an empty key in the list" verify -n check-ns --keys "a,,b"
refuse_key() {  # refuse_key "<why>" KEY -- refused as a key, before any cluster read
    if PATH="$STUB_PATH" STUB_SH="$ECV_SH" STUB_EC_ROOT="$T/ecv" STUB_NODES_FILE="$T/ecv-nodes.json" STUB_PODS_FILE="$T/ecv-pods.json" \
        bash deploy/enginecache.sh verify -n check-ns --keys "$2" > "$T/refkey.out" 2>&1; then fail "accepted: $1"
    elif grep -q 'not a compile cache key' "$T/refkey.out"; then ok "refused as a key: $1"
    else fail "refused, but not as a key: $1: $(tail -1 "$T/refkey.out")"; fi
}
refuse_key "a key whose last component is ." "torch_compile_cache/."
refuse_key "a key with a . component" "torch_compile_cache/./${K1#*/}"
refuse_key "a key that is ." "."
refuse "a rank that is not rank_<r>_<dp>" verify -n check-ns --keys "$K1@rank_x"
refuse "an empty rank list" verify -n check-ns --keys "$K1@"
refuse "an empty rank in the list" verify -n check-ns --keys "$K1@rank_0_0++rank_1_0"
refuse "a rank that climbs out" verify -n check-ns --keys "$K1@../rank_0_0"
refuse "--keys on status" status -n check-ns --keys a
refuse "keys without a pod" keys -n check-ns
refuse "a pod name that is not one" keys -n check-ns 'Pod;x'
# keys: read off the engine log, one per configuration, every rank folded
cat > "$T/engine-logs/pd-decode-0" <<'EOF'
INFO 10-14 [backends.py:550] Using cache directory: /engine-cache/vllm/torch_compile_cache/3f2a9c1d0e/rank_0_0/backbone for vLLM's torch.compile
INFO 10-14 [backends.py:550] Using cache directory: /engine-cache/vllm/torch_compile_cache/3f2a9c1d0e/rank_1_0/backbone for vLLM's torch.compile
INFO 10-14 [monitor.py:34] torch.compile takes 0.31 s in total
EOF
cat > "$T/engine-logs/pd-prefill-0" <<'EOF'
INFO 10-14 [backends.py:550] Using cache directory: /engine-cache/vllm/torch_compile_cache/77b0e1aa42/rank_0_0/backbone for vLLM's torch.compile
EOF
# the guard the scenarios put before the engine prints this when the cache
# root is not writable, then unsets VLLM_CACHE_ROOT (pd-disaggregation.yaml)
cat > "$T/engine-logs/fell-back" <<'EOF'
engine cache VLLM_CACHE_ROOT=/engine-cache/vllm is not writable; using the engine default instead
INFO 10-14 [backends.py:550] Using cache directory: /root/.cache/vllm/torch_compile_cache/3f2a9c1d0e/rank_0_0/backbone for vLLM's torch.compile
EOF
# VLLM_USE_AOT_COMPILE (vLLM v0.31.0 decorators.py): the artefact sits at
# VLLM_CACHE_ROOT/torch_compile_cache/torch_aot_compile/<sha256>/rank_<r>_<dp>/model
AOT=torch_compile_cache/torch_aot_compile/9c4e1f0a7b3d2c5e8f6a1b4d7c0e3f2a5b8d1c4e7f0a3b6d9c2e5f8a1b4d7c0e
cat > "$T/engine-logs/aot" <<EOF
INFO 10-14 [decorators.py:313] Directly load AOT compilation from path /engine-cache/vllm/$AOT/rank_0_0/model
INFO 10-14 [decorators.py:313] Directly load AOT compilation from path /engine-cache/vllm/$AOT/rank_1_0/model
EOF
printf 'INFO 10-14 Starting vLLM API server\n' > "$T/engine-logs/no-compile"
for p in pd-decode-0 pd-prefill-0 fell-back no-compile aot; do printf /engine-cache/vllm > "$T/engine-root/$p"; done
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns pd-decode-0 > "$T/keys1.out" 2> "$T/keys1.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys1.out")" = "$K1@rank_0_0+rank_1_0" ] && grep -q "make engine-cache-verify NAMESPACE=check-ns ENGINE_CACHE_KEYS=$K1@rank_0_0+rank_1_0\$" "$T/keys1.err" && ! grep -q WARNING "$T/keys1.err" \
    && ok "keys: one key per configuration, naming every rank directory its log shows, and the verify line to run" || fail "keys one pod: rc=$rc out=$(tr '\n' ' ' < "$T/keys1.out") err=$(tr '\n' ' ' < "$T/keys1.err")"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns pd-decode-0 pd-prefill-0 > "$T/keys2.out" 2> "$T/keys2.err"; rc=$?
[ "$rc" = 0 ] && [ "$(tr '\n' ' ' < "$T/keys2.out")" = "$K1@rank_0_0+rank_1_0 $K2@rank_0_0 " ] && grep -q "ENGINE_CACHE_KEYS=$K1@rank_0_0+rank_1_0,$K2@rank_0_0\$" "$T/keys2.err" \
    && ok "keys: prefill and decode give two keys, joined for verify" || fail "keys two pods: rc=$rc out=$(tr '\n' ' ' < "$T/keys2.out") err=$(tail -1 "$T/keys2.err")"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns fell-back > "$T/keys3.out" 2> "$T/keys3.err"; rc=$?
[ "$(cat "$T/keys3.out")" = "$K1@rank_0_0" ] && grep -q 'compiled under /root/.cache/vllm, not VLLM_CACHE_ROOT=/engine-cache/vllm' "$T/keys3.err" \
    && ok "keys: an engine that fell back off the cache root still names its key, and says the start did not fill the node cache" || fail "keys fell back: out=$(cat "$T/keys3.out") err=$(tr '\n' ' ' < "$T/keys3.err")"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns aot > "$T/keys-aot.out" 2> "$T/keys-aot.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys-aot.out")" = "$AOT@rank_0_0+rank_1_0" ] && ! grep -q WARNING "$T/keys-aot.err" \
    && ok "keys: an AOT compile names its key under the cache root (torch_compile_cache/torch_aot_compile/<hash>), with its ranks" || fail "keys AOT: rc=$rc out=$(cat "$T/keys-aot.out") err=$(tr '\n' ' ' < "$T/keys-aot.err")"
printf /engine-cache/vllm/ > "$T/engine-root/pd-prefill-0"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns pd-prefill-0 > "$T/keys-slash.out" 2> "$T/keys-slash.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys-slash.out")" = "$K2@rank_0_0" ] && ! grep -q WARNING "$T/keys-slash.err" \
    && ok "keys: a VLLM_CACHE_ROOT with a trailing / is the same root, not a fall-back" || fail "keys trailing slash: rc=$rc err=$(tr '\n' ' ' < "$T/keys-slash.err")"
printf /engine-cache/vllm > "$T/engine-root/pd-prefill-0"
# the AOT save line names the key; a failed save, or a failed load that
# compiled again, does not (vLLM v0.31.0 decorators.py)
AOT2=torch_compile_cache/torch_aot_compile/0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e
printf 'INFO 10-14 [decorators.py:330] saved AOT compiled function to /engine-cache/vllm/%s/rank_0_0/model\n' "$AOT" > "$T/engine-logs/aot-saved"
{ printf 'WARNING 10-14 [decorators.py:335] unable to save AOT compiled function to /engine-cache/vllm/%s/rank_0_0/model: OSError\n' "$AOT2"
  printf "INFO 10-14 [backends.py:550] Using cache directory: /engine-cache/vllm/%s/rank_0_0/backbone for vLLM's torch.compile\n" "$K1"; } > "$T/engine-logs/aot-unsaved"
{ printf 'WARNING 10-14 [decorators.py:320] Compiling model again due to a load failure from /engine-cache/vllm/%s/rank_0_0/model, reason: EOFError\n' "$AOT"
  printf 'INFO 10-14 [decorators.py:330] saved AOT compiled function to /engine-cache/vllm/%s/rank_0_0/model\n' "$AOT"; } > "$T/engine-logs/aot-reload"
for p in aot-saved aot-unsaved aot-reload; do printf /engine-cache/vllm > "$T/engine-root/$p"; done
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns aot-saved > "$T/keys-aots.out" 2> "$T/keys-aots.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys-aots.out")" = "$AOT@rank_0_0" ] && ! grep -q WARNING "$T/keys-aots.err" \
    && ok "keys: an AOT save line (saved AOT compiled function to ...) names its key" || fail "keys AOT saved: rc=$rc out=$(cat "$T/keys-aots.out") err=$(tr '\n' ' ' < "$T/keys-aots.err")"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns aot-unsaved > "$T/keys-aotu.out" 2> "$T/keys-aotu.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys-aotu.out")" = "$K1@rank_0_0" ] && grep -q "aot-unsaved: the engine could not save its AOT compile to /engine-cache/vllm/$AOT2/rank_0_0; that line gives no key" "$T/keys-aotu.err" \
    && ok "keys: a failed AOT save gives no key, and says the save failed" || fail "keys AOT unsaved: rc=$rc out=$(tr '\n' ' ' < "$T/keys-aotu.out") err=$(tr '\n' ' ' < "$T/keys-aotu.err")"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns aot-reload > "$T/keys-aotr.out" 2> "$T/keys-aotr.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys-aotr.out")" = "$AOT@rank_0_0" ] && grep -q "aot-reload: the AOT compile at /engine-cache/vllm/$AOT/rank_0_0 failed to load and was compiled again; that line gives no key" "$T/keys-aotr.err" \
    && ok "keys: a failed AOT load warns; the save that follows still names the key" || fail "keys AOT reload: rc=$rc out=$(tr '\n' ' ' < "$T/keys-aotr.out") err=$(tr '\n' ' ' < "$T/keys-aotr.err")"
# VLLM_CACHE_ROOT=/ is the root /, not unset
printf "INFO 10-14 [backends.py:550] Using cache directory: /%s/rank_0_0/backbone for vLLM's torch.compile\n" "$K1" > "$T/engine-logs/root-slash"
printf / > "$T/engine-root/root-slash"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns root-slash > "$T/keys-rs.out" 2> "$T/keys-rs.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys-rs.out")" = "$K1@rank_0_0" ] && ! grep -q WARNING "$T/keys-rs.err" \
    && ok "keys: VLLM_CACHE_ROOT=/ is the root /, and its key is found" || fail "keys root /: rc=$rc out=$(cat "$T/keys-rs.out") err=$(tr '\n' ' ' < "$T/keys-rs.err")"
printf / > "$T/engine-root/fell-back"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns fell-back > "$T/keys-rsf.out" 2> "$T/keys-rsf.err"
grep -q 'compiled under /root/.cache/vllm, not VLLM_CACHE_ROOT=/ ' "$T/keys-rsf.err" && ! grep -q 'sets no VLLM_CACHE_ROOT' "$T/keys-rsf.err" \
    && ok "keys: an engine that fell back off VLLM_CACHE_ROOT=/ is told apart from one with no VLLM_CACHE_ROOT" || fail "keys root / fell back: err=$(tr '\n' ' ' < "$T/keys-rsf.err")"
printf /engine-cache/vllm > "$T/engine-root/fell-back"
# a relative VLLM_CACHE_ROOT: the key is found, and the warning says the root is relative, not that it fell back
printf "INFO 10-14 [backends.py:550] Using cache directory: cache/%s/rank_0_0/backbone for vLLM's torch.compile\n" "$K1" > "$T/engine-logs/rel-root"
printf cache > "$T/engine-root/rel-root"
PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns rel-root > "$T/keys-rel.out" 2> "$T/keys-rel.err"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$T/keys-rel.out")" = "$K1@rank_0_0" ] && grep -q 'VLLM_CACHE_ROOT=cache is a relative path' "$T/keys-rel.err" && ! grep -q 'fell back' "$T/keys-rel.err" \
    && ok "keys: a relative VLLM_CACHE_ROOT finds the key and says the root is relative, not that it fell back" || fail "keys relative root: rc=$rc out=$(cat "$T/keys-rel.out") err=$(tr '\n' ' ' < "$T/keys-rel.err")"
if PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns no-compile > "$T/keys4.out" 2>&1; then fail "keys must fail on a log with no cache directory"; else
    grep -q 'no compile cache directory in its log' "$T/keys4.out" && ok "keys: a log with no cache directory is a failure, not an empty answer" || fail "keys no line: $(tail -1 "$T/keys4.out")"; fi
if STUB_ENGINE_LOG_ERROR='Error from server (Forbidden): pods "pd-decode-0" is forbidden: cannot get resource "pods/log"' PATH="$STUB_PATH" bash deploy/enginecache.sh keys -n check-ns pd-decode-0 > "$T/keys5.out" 2>&1; then fail "keys must fail when the log cannot be read"; else
    grep -q 'cannot read the log of pd-decode-0 (container vllm): Error from server (Forbidden)' "$T/keys5.out" && ok "keys: a log that cannot be read (Forbidden) is a failure that says so" || fail "keys log Forbidden: $(tail -1 "$T/keys5.out")"; fi

# ---------------------------------------------------------------------------
# 4. The Makefile: the targets' argv and the standup's step.
# ---------------------------------------------------------------------------
line="$(make -n engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=i:1 ENGINE_CACHE_CAPACITY=50Gi NAMESPACE=ns PREPULL_TOLERATIONS=t1 2>/dev/null | tr -d '\\\n' | grep -o 'bash deploy/enginecache.sh apply.*' || true)"
case "$line" in
    *'-n "ns"'*'--path "/mnt/local/w"'*'--image "i:1"'*'--capacity "50Gi"'*'--toleration t1'*) ok "make engine-cache: namespace, path, image, capacity and toleration reach the script" ;;
    *) fail "make engine-cache argv: $line" ;;
esac
line="$(make -n engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=i:1 ENGINE_CACHE_SEED_CLAIM=shared:caches NAMESPACE=ns 2>/dev/null | tr -d '\\\n' | grep -o 'bash deploy/enginecache.sh apply.*' || true)"
case "$line" in *'--seed-claim "shared:caches"'*) ok "make engine-cache: ENGINE_CACHE_SEED_CLAIM reaches the script" ;; *) fail "make engine-cache seed argv: $line" ;; esac
line="$(make -n engine-cache WEIGHTS_PATH=/mnt/local/w WEIGHTS_IMAGE=i:2 NAMESPACE=ns 2>/dev/null | tr -d '\\\n' | grep -o 'bash deploy/enginecache.sh apply.*' || true)"
case "$line" in *'--seed-claim'*) fail "make engine-cache: a --seed-claim with no ENGINE_CACHE_SEED_CLAIM" ;; *) ok "make engine-cache: no seed unless asked" ;; esac
case "$line" in *'--path "/mnt/local/w"'*'--image "i:2"'*) ok "make engine-cache: defaults to the weights' path and image" ;; *) fail "make engine-cache defaults: $line" ;; esac
# the guard tests $(origin NAMESPACE): given on the command line it expands to a word, otherwise to nothing
if make -n engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=i NAMESPACE=ns 2>/dev/null | grep -q 'test -n "command line"'; then ok "make engine-cache: NAMESPACE must be given on the command line (the Makefile default is not taken)"; else fail "make engine-cache: the NAMESPACE guard is missing"; fi
if make -n engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=i 2>/dev/null | grep -q 'test -n ""'; then ok "make engine-cache: without NAMESPACE the guard is empty and fails at run time"; else fail "make engine-cache: the guard passes without NAMESPACE"; fi
line="$(make -n engine-cache-status NAMESPACE=ns WEIGHTS_NODE_SELECTOR=k=v 2>/dev/null | grep -o 'bash deploy/enginecache.sh status.*' || true)"
case "$line" in *'-n "ns"'*'--node-selector "k=v"'*) ok "make engine-cache-status: namespace and selector reach the script" ;; *) fail "make engine-cache-status argv: $line" ;; esac
line="$(make -n engine-cache-delete NAMESPACE=ns 2>/dev/null | grep -o 'bash deploy/enginecache.sh delete.*' || true)"
case "$line" in *'-n "ns"'*) ok "make engine-cache-delete: the namespace reaches the script" ;; *) fail "make engine-cache-delete argv: $line" ;; esac
line="$(make -n engine-cache-verify NAMESPACE=ns ENGINE_CACHE_KEYS=torch_compile_cache/a,b WEIGHTS_NODE_SELECTOR=k=v,k2=v2 2>/dev/null | grep -o 'bash deploy/enginecache.sh verify.*' || true)"
case "$line" in *'-n "ns" --keys "torch_compile_cache/a,b" --node-selector "k=v,k2=v2"'*) ok "make engine-cache-verify: namespace, keys and selector reach the script" ;; *) fail "make engine-cache-verify argv: $line" ;; esac
if make -n engine-cache-verify NAMESPACE=ns 2>/dev/null | grep -q 'test -n ""'; then ok "make engine-cache-verify: without ENGINE_CACHE_KEYS the guard is empty and fails at run time"; else fail "make engine-cache-verify: no guard on ENGINE_CACHE_KEYS"; fi
line="$(make -n engine-cache-keys NAMESPACE=ns ENGINE_CACHE_PODS='p-decode-1 p-prefill-2' 2>/dev/null | grep -o 'bash deploy/enginecache.sh keys.*' || true)"
case "$line" in *'keys -n "ns" "p-decode-1" "p-prefill-2"'*) ok "make engine-cache-keys: each pod reaches the script as its own argument" ;; *) fail "make engine-cache-keys argv: $line" ;; esac
n="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_MODEL_HOSTPATH=/mnt/local/w BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -n 'model_hostpath.sh\|enginecache.sh apply\|engine_cache_claim.sh\|standup \\$' | cut -d: -f1 | tr '\n' ' ')"
set -- $n
if [ $# -ge 4 ] && [ "$1" -lt "$2" ] && [ "$2" -lt "$3" ] && [ "$3" -lt "$4" ]; then ok "benchmark-standup: BENCHMARK_MODEL_HOSTPATH alone turns the cache on too, after the weights edit and before the harness standup (apply, then the scenario edit)"; else fail "benchmark-standup ordering: $n"; fi
# make -n prints the recipe's shell text; the step is guarded by an if on the
# variable, so with neither set the condition it prints is an empty test
if make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -B1 'img="";' | grep -q 'if \[ -n "" \]'; then ok "benchmark-standup: neither variable, the cache step's condition is empty (the shared volume, as before)"; else fail "benchmark-standup: the cache step is not behind an empty test with neither variable set"; fi
# make -n prints the recipe with the variables substituted, not evaluated: the
# assertion is on the guard LINE (the if on the cache directory, with the
# preparer's image assignment right after it), not on a token anywhere in it
if make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_ENGINE_CACHE_HOSTPATH=/mnt/local/c BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -A1 'if \[ -n "/mnt/local/c" \]; then' | grep -q 'img='; then ok "benchmark-standup: the cache step is guarded by BENCHMARK_ENGINE_CACHE_HOSTPATH itself" ; else fail "benchmark-standup: the cache step's guard is not on BENCHMARK_ENGINE_CACHE_HOSTPATH"; fi
if make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_ENGINE_CACHE_HOSTPATH=/mnt/local/c BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -q -- 'enginecache.sh apply -n "ns" --path "/mnt/local/c"'; then ok "benchmark-standup: BENCHMARK_ENGINE_CACHE_HOSTPATH on its own takes its own directory" ; else fail "benchmark-standup: the cache directory did not reach the apply"; fi
rec="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_ENGINE_CACHE_HOSTPATH=/mnt/local/c BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null)"
printf '%s\n' "$rec" | grep -q 'seed="auto";' && printf '%s\n' "$rec" | grep -qE 'kubectl (--request-timeout=[^ ]+ )?get pvc -n "ns" workload-pvc' && printf '%s\n' "$rec" | grep -q 'seed=workload-pvc:engine-cache' && ok "benchmark-standup: the default seed is the harness workload PVC's engine-cache, when that claim exists" || fail "benchmark-standup: the auto seed is not the workload PVC"
# make -n prints the shell, not its evaluation: the assertion is that the
# seed rides on the guarded apply line itself, as a shell conditional
printf '%s\n' "$rec" | grep -A12 'if \[ -n "/mnt/local/c" \]; then' | grep -q 'enginecache.sh apply -n "ns" --path "/mnt/local/c" .*${seed:+--seed-claim "$seed"}' && ok "benchmark-standup: the seed rides on the guarded apply, as a shell conditional on its value" || fail "benchmark-standup: --seed-claim is not on the guarded apply line"
rec="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_ENGINE_CACHE_HOSTPATH=/mnt/local/c BENCHMARK_ENGINE_CACHE_SEED=none BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null)"
printf '%s\n' "$rec" | grep -q 'seed="none";' && ok "benchmark-standup: BENCHMARK_ENGINE_CACHE_SEED=none reaches the step" || fail "benchmark-standup: seed=none did not reach the recipe"
# the preparer compiles for the engine the scenario pins, not the harness default
printf '%s\n' "$rec" | grep -A2 'if \[ -n "/mnt/local/c" \]; then' | grep -q 'engine_image.sh "[^"]*" "[^"]*/config/scenarios/guides/pd-disaggregation.yaml"' && ok "benchmark-standup: the preparer's image is read with the run's scenario" || fail "benchmark-standup: the preparer's image ignores the scenario's engine pin"

exit $FAILED
