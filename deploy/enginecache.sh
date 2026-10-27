#!/usr/bin/env bash
# Keep the engines' compile caches on every accelerator node's local disk, so
# a replica's start does not depend on a shared volume being writable.
#
# vLLM keeps three caches between runs -- the torch.compile artefacts, the
# FlashInfer autotune table and Triton's JIT cache -- and a replica that finds
# them warm skips the compile; one that does not pays it at every start.
# Pointing them at a shared RWX claim works until the claim does not: measured
# on one cluster, nine starts of the same pod spec split by node into 51-56 s
# and 75-81 s, and the 25 s was the shared claim being published read-only on
# some nodes by the CSI driver, the engine falling back to an empty cache
# under /tmp, and every start there compiling from nothing (14 s of
# torch.compile against 3 s from the cache, and 8 s more before the engine).
# A fourth directory, pycache/, holds Python bytecode for any Python engine
# (PYTHONPYCACHEPREFIX=/engine-cache/pycache/<image key>): engine images ship
# almost no .pyc, so every container compiles its imports again -- measured
# on vLLM v0.26.0, ~11 s of every start. One key per image (`make
# workload-patch` derives it from the image reference): a .pyc is checked
# against its source's mtime and size only, so two images must not share one.
# This puts the caches on the node: one static hostPath PersistentVolume, one
# claim bound to it, and one DaemonSet that prepares the directory on every
# selected node and then reports Ready. The model servers mount the claim
# exactly as they mount any other; a hostPath is a bind mount with no
# storage driver in its path, and each node's cache is warm from the second
# start on that node -- or from the first, with --seed-claim, which copies a
# shared claim's caches onto each node as it is prepared. (The filesystem
# under it can still go read-only -- that is what the preparer's readiness
# and the engines' guard are for.)
#
#   enginecache.sh apply  -n NS --path DIR --image IMG [--seed-claim PVC[:SUBPATH]] [--dry-run]
#                                                                     prepare DIR/engine-cache on every accelerator node
#   enginecache.sh status -n NS [--node-selector KEY=VALUE[,...]]     which nodes are prepared, which are not, and why
#   enginecache.sh delete -n NS [--dry-run]                           drop the claim, volume and preparer (the caches stay)
#   enginecache.sh publish -n NS [--dry-run]                          fill the seed claim from every prepared node, then re-seed every node
#                                                                     (run once each engine configuration has started somewhere)
#   enginecache.sh keys   -n NS [--container C] POD [POD...]         the compile cache key(s) a running engine pod used
#   enginecache.sh verify -n NS --keys KEY[,KEY...] [--node-selector KEY=VALUE[,...]]
#                                                                     is every key on every usable node, and readable
#
#   -n, --namespace NS          the namespace the claim and preparer live in
#
# Options:
#   --path DIR                  directory on the node, e.g. /mnt/local/weights/<ns>
#                               -- the same DIR weights.sh takes; the caches
#                               land under DIR/engine-cache (/var/mnt/weights/<ns>
#                               on RHCOS, under /var). What DIR must be:
#                                 * absolute, at least two components, no `..`,
#                                   no trailing slash; the whole of a top-level
#                                   directory is refused
#                                 * on a disk that is not the node's own, and
#                                   never a network mount: a hostPath is a
#                                   bind mount, and what is written there is
#                                   charged to no quota -- a fill is
#                                   DiskPressure for every pod on the node
#                                 * never a system path: /etc, /usr, /var/lib,
#                                   /var/log, /tmp, /home, /opt/bin and their
#                                   kind are refused, and so are /var/mnt and
#                                   /var/srv themselves (RHCOS's root disk;
#                                   mount the NVMe at a subdirectory first --
#                                   the script cannot tell a mountpoint from a
#                                   directory)
#                                 * one DIR per trust domain (see below: what
#                                   lands there is code every engine on the
#                                   node loads); on OpenShift one per project
#                                   is the only thing that works
#                                 * creatable by the preparer, which runs as
#                                   root on Kubernetes; on OpenShift prepare
#                                   DIR/engine-cache on each node first (see
#                                   the end of this text)
#                               The same DIR must exist on EVERY node the
#                               preparer selects (--node-selector; a node it
#                               cannot prepare shows in `status`, and an
#                               engine there costs a compile, not the
#                               replica, with the guard). Without a node
#                               directory at all, the engines can keep the
#                               caches on a shared ReadWriteMany claim
#                               instead -- `make workload-patch` with
#                               WVA_ENGINE_CACHE_CLAIM=<claim>[:<subPath>]
#                               writes that -- at the cost this script exists
#                               to remove: a CSI driver can publish that claim
#                               read-only on a node, and every start there
#                               then compiles. And what is on that claim is
#                               code every engine loads: whoever can write it
#                               runs code in every engine, on every node at
#                               once -- trust it as you would the engine image.
#   --image IMG                 image to prepare with: any image carrying /bin/sh
#                               -- the engine image itself is the natural
#                               choice, and it is then held on the node as a
#                               side effect
#   --capacity SIZE             the volume's declared capacity (default 200Gi;
#                               it is a declaration, hostPath has no quota).
#                               A re-apply keeps an existing claim's size
#                               when this is not given, and refuses a SIZE
#                               that differs from it: a claim on a static
#                               volume cannot be resized (delete first).
#                               --dry-run reads no cluster, so it renders
#                               SIZE or the default
#   --seed-claim PVC[:SUBPATH]  a claim in the namespace holding caches to
#                               copy onto each node before it is prepared --
#                               the shared claim the caches lived on before,
#                               or one filled once on purpose. Mounted
#                               read-only; copied without overwriting what
#                               the node already has; once per node (a
#                               marker). The caches are portable across
#                               nodes of one accelerator model, driver and
#                               engine version: without a seed the first
#                               start on EVERY node compiles (the scheduler
#                               spreads replicas, so that is one cold start
#                               per node), with one no node starts cold.
#                               What is copied is code the engines load (see
#                               below): the seed claim must be trusted as
#                               the cache itself is, and more -- one write
#                               to it reaches every node at the next
#                               prepare, not the one node a pod landed on.
#                               A changed seed is merged in too (the marker
#                               records the seed). A subPath that does not
#                               exist yet is nothing to seed, not an error.
#   --node-selector KEY=VALUE[,KEY=VALUE...]
#                               which nodes count as accelerator nodes. The
#                               default is any node carrying a known GPU
#                               product label (deploy/lib/accelerator_nodes.sh);
#                               set this to what your model servers select
#                               on when they select on something.
#                               One or more KEY=VALUE pairs, comma
#                               separated, no spaces: a node must carry
#                               every one (k1=v1,k2=v2 is k1=v1 AND
#                               k2=v2, as in a nodeSelector)
#   --toleration KEY            tolerate a taint with KEY (any value, any
#                               effect); nvidia.com/gpu is always tolerated
#   --dry-run                   apply: print the manifests instead of
#                               applying them; delete: print what would go
#   --keys KEY[,KEY...]         verify: the compile cache keys to look for,
#                               as `keys` prints them -- a directory under
#                               VLLM_CACHE_ROOT such as
#                               torch_compile_cache/1a2b3c4d5e@rank_0_0+rank_1_0:
#                               the key, then every rank directory the engine
#                               wrote under it, each of which must be there.
#                               Without @ranks any rank will do; a bare hash
#                               means torch_compile_cache/<hash>
#   --container C               keys: the engine container (default vllm)
#
# Checking the cache without a second GPU start. Start each engine
# configuration once (prefill and decode compile under different keys), run
# `keys` on that pod, then `verify` with what it printed. verify reads each
# key's directory through the preparer pod already Ready on every usable node
# (kubectl exec into the preparer, which holds no GPU) and reads every file
# under it. It exits non-zero when any node lacks a key, when a node has no
# Ready preparer, or when a read could not be made -- a Forbidden exec is
# "could not check", never "absent". It reads as the preparer's user, root
# on Kubernetes, which can read any file; so it also reads the modes, for the
# engine's UID: the owner of the key directory (the engine that wrote it), or
# ENGINE_CACHE_UID=<uid> in the environment when the engines run as another
# UID than the one that owns the key (a seeded node: the preparer copied it
# as root). A file that UID cannot read (or a directory under the key it
# cannot read and search, or one on the way to it from the volume mount it
# cannot search) is
# reported unreadable, naming the UID. vLLM's AOT files are 600, which is
# fine for the engine that owns them. A seeded or published key is owned by
# root, so without ENGINE_CACHE_UID it is judged for root. Without it every
# line names the UID it was judged for, the summary warns and names the UIDs
# it assumed, and the verdict says which ("VERIFIED for uid 1000",
# "VERIFIED for root"); a plain VERIFIED needs ENGINE_CACHE_UID. It checks the
# torch.compile cache only: the flashinfer/, triton/ and pycache/ directories
# beside vllm/ are not read.
#
# The claim is named engine-cache, one per namespace (the caches inside are
# keyed by a hash of the engine config, so every model and flag set in the
# namespace has its own entry and none hits another's). A model server
# mounts it read-write at, say, /engine-cache and points VLLM_CACHE_ROOT,
# FLASHINFER_WORKSPACE_DIR and TRITON_CACHE_DIR under it -- and keeps a guard
# that unsets a variable whose directory is not writable, so a node that is
# not prepared costs a compile, not the replica. The guard covers
# PYTHONPYCACHEPREFIX too: with it set Python reads bytecode only from it, so
# one it cannot write is slower than none. The volume is cluster-scoped:
# apply needs leave to create PersistentVolumes, which a namespace tenant does
# not have -- ask the cluster admin to run apply, or to create the volume.
# Nothing here uses a hostPath volume in a Pod (the preparer mounts the
# claim), so Pod Security "baseline" admits it and "restricted" does not. The
# node directory is cluster-shared, WORLD-writable state (1777 on the four
# cache directories, so any UID the engines run as can write; sticky, so one
# UID's files are not another's to remove), what is written there is charged
# to no quota, and what is written there is CODE: torch.compile artefacts,
# FlashInfer and Triton kernels and Python bytecode that every engine on the
# node loads and runs. Whoever can write the directory runs code in every engine on that
# node -- on Kubernetes that is anyone with pods/create in a namespace that
# can mount the claim, which is no more than pods/create already grants,
# and across namespaces it is everyone pointed at the same DIR. So the
# rules are weights.sh's, harder: one DIR per trust domain, on a disk that
# is not the node's own, and wipe DIR/engine-cache on the nodes when a
# namespace or a directory changes hands -- delete keeps the caches, and a
# new tenant of the same name and DIR would load the old one's. 1777 is a
# trade-off, taken so the engines need no fsGroup; a namespace that runs
# its engines under one UID and wants the separation can chown the four
# directories to it and chmod them 1770 on each node. The preparer runs as
# its own ServiceAccount, engine-cache-preparer.
#
# On OpenShift -- NOT YET RUN THERE -- restricted-v2 runs the preparer and
# the engines as the project's range UID with GID 0, and the kubelet does not
# relabel a hostPath: prepare DIR/engine-cache on each node as weights.sh
# --help says (chgrp 0, chmod 2775, chcon -t container_file_t, or a
# MachineConfig mount with context=...:container_file_t:s0). The preparer
# then creates the four directories inside it as that UID, owns them, and
# its chmod 1777 takes (the setgid bit is not inherited by a mode set
# outright; the files carry the project's SELinux level either way). `status`
# says when the preparer died on a permission error.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"
# shellcheck source=lib/accelerator_nodes.sh
source "$HERE/lib/accelerator_nodes.sh"
# shellcheck source=lib/nodedir.sh
source "$HERE/lib/nodedir.sh"
# NOTE: log_error EXITS. Nothing may follow it that needs to run.

NAMESPACE=""
NODE_PATH=""
IMAGE=""
CAPACITY="200Gi"
CAPACITY_GIVEN=false
NODE_SELECTOR=""          # empty: every node carrying a known GPU product label
NODE_SELECTOR_GIVEN=false
TOLERATIONS=("nvidia.com/gpu")
DRY_RUN=false
LABEL_COMPONENT="app.kubernetes.io/component=node-local-engine-cache,app.kubernetes.io/managed-by=wva-enginecache"
STORAGE_CLASS="node-local-engine-cache"
CLAIM="engine-cache"
SUBDIR="engine-cache"
MARKER=".prepared"
SEED_MARKER=".seeded"
SEED_CLAIM=""
SEED_SUBPATH=""
SA_NAME="engine-cache-preparer"
KEYS=""
CONTAINER="vllm"
PODS=()

usage() {
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
    exit 0
}

# pv_for is the volume's name. PersistentVolumes are cluster-scoped, so the
# namespace and the directory are in the hash: two namespaces preparing the
# same directory each get their own volume, bound to their own claim.
pv_for() {
    printf '%s-%s' "$CLAIM" "$(wva_ns_suffix "${NAMESPACE}/${NODE_PATH}")"
}

check_path() {
    local why
    why="$(nodedir_ok "$1")" || log_error "--path: ${why}"
}

check_image() {
    case "$1" in
        "") log_error "--image must not be empty" ;;
        *[!A-Za-z0-9._:/@-]*) log_error "not an image reference: '$1' (registry/path[:tag|@sha256:digest], no spaces or quotes)" ;;
    esac
}

# preparer_script is the preparer's command. A quoted heredoc: nothing in it
# is bash's to evaluate at render time -- it runs on the node, as written. A
# `$(...)` here would otherwise run on the operator's machine at apply and
# bake its output into every node's manifest. The markers' names and the
# seed's identity reach it as environment variables. No `$(` and no `$$` in
# it: Kubernetes rewrites both in a container's command. The seed is merged
# file by file, descending directories both sides have: an entry the node
# already has is kept, one it lacks is copied (so a second engine config's
# key lands beside the node's own under torch_compile_cache), a copy that
# fails is removed and the seed marker withheld, so the next start of the
# pod tries again; the marker records the seed it came from, so a changed
# --seed-claim is merged in too. A recursive function, with `local`, which
# dash has.
preparer_script() {
    cat <<'SCRIPT'
for d in vllm flashinfer triton pycache; do
  mkdir -p "/engine-cache/$d" || exit 1
  chmod 1777 "/engine-cache/$d" 2>/dev/null || true
done
# cp -a without ownership is GNU only; busybox cp refuses the option. Try it
# once; without it a plain cp copies the content and mode, not the times.
cpa=0
: > /tmp/.ec-cp-check 2>/dev/null && cp -a --no-preserve=ownership /tmp/.ec-cp-check /tmp/.ec-cp-check.2 2>/dev/null && cpa=1
rm -f /tmp/.ec-cp-check /tmp/.ec-cp-check.2
# merge SRC DST: an entry DST has is kept, one it lacks is copied, a directory
# both have is descended -- so a second engine config's key lands beside the
# node's own. It walks every directory and copies one file at a time:
#  * a symlink is skipped and logged: vLLM writes none, and a link is not
#    the cache (it would point at whatever the target is on each node);
#  * a file not every user can read is copied readable by every user, and
#    counted once its copy succeeds: the engine may run as another UID;
#  * a new directory is removed whole when any copy under it fails, so no
#    half-copied key is left behind, and the merge is marked incomplete;
#    what was counted under it is uncounted;
#    a directory that cannot be listed is a failed copy, not an empty one.
# The copier owns every copy, whatever owned the original. The globs match
# every name: plain, one leading dot, and two leading dots (..x).
merge() {
  local e n t was wa wp wpe pub
  # a directory that cannot be listed would glob to nothing and read as empty
  ls -a "$1" >/dev/null || { complete=0; return; }
  for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    [ -e "$e" ] || [ -L "$e" ] || continue
    n="${e##*/}"; t="$2/$n"
    if [ -L "$e" ]; then
      links="$links."; echo "engine cache: skipped $e: a symlink, not cache content"
    elif [ -e "$t" ] || [ -L "$t" ]; then
      if [ -d "$e" ] && [ -d "$t" ] && [ ! -L "$t" ]; then merge "$e" "$t"; fi
    elif [ -d "$e" ]; then
      was="$complete"; wa="$added"; wp="$private"; wpe="$private_eg"; complete=1
      mkdir "$t" && chmod a+rx "$t" || complete=0
      [ "$complete" = 0 ] || merge "$e" "$t"
      if [ "$complete" = 1 ]; then complete="$was"; else complete=0; rm -rf "$t"; added="$wa"; private="$wp"; private_eg="$wpe"; fi
    elif [ -f "$e" ]; then
      pub=0; ls -ld "$e" | grep -q '^-......r' && pub=1
      if [ "$cpa" = 1 ]; then cp -a --no-preserve=ownership "$e" "$t"; else cp "$e" "$t"; fi && chmod a+r "$t" || { complete=0; rm -f "$t"; continue; }
      added="$added."
      [ "$pub" = 1 ] || { private="$private."; [ -n "$private_eg" ] || private_eg="$e"; }
    else
      echo "engine cache: skipped $e: not a file or a directory"
    fi
  done
}
# merge_report prints what the merge skipped or changed.
merge_report() {
  [ -z "$links" ] || echo "engine cache: ${#links} symlink(s) skipped (named above)"
  [ -z "$private" ] || echo "engine cache: ${#private} file(s) not readable by every user were copied readable by every user, for example $private_eg"
}
seeded_from=""
[ ! -f "/engine-cache/$SEED_MARKER" ] || read -r seeded_from < "/engine-cache/$SEED_MARKER" || seeded_from=""
# The seed claim is mounted whole at /seed and SEED_SUBPATH is looked up here:
# a subPath on the mount would make the kubelet create it, which fails on a
# fresh claim and leaves the pod unable to start. An absent seed is nothing
# to seed -- the marker says so with a "none:" prefix, which the readiness
# probe accepts, and the next start merges the seed if it has appeared.
seed_dir="/seed${SEED_SUBPATH:+/$SEED_SUBPATH}"
# `enginecache.sh publish` stamps the seed each time it adds to it; the marker
# records the stamp, so a node merges a seed published after it was seeded.
want="$SEED_ID"
if [ -f "$seed_dir/.published" ]; then
  read -r stamp < "$seed_dir/.published" || stamp=""
  [ -z "$stamp" ] || want="$SEED_ID@$stamp"
fi
if [ -d /seed ] && [ ! -d "$seed_dir" ]; then
  if [ "$seeded_from" != "none:$SEED_ID" ] && [ "$seeded_from" != "$SEED_ID" ]; then
    echo "none:$SEED_ID" > "/engine-cache/$SEED_MARKER" || exit 1
  fi
  echo "engine cache: no seed on $NODE_NAME: $SEED_ID has no $seed_dir yet; nothing to seed"
elif [ -d /seed ] && [ "$seeded_from" != "$want" ]; then
  # Drop the old marker first. If this merge fails, the node must be not
  # Ready -- not Ready on the stamp it merged before.
  rm -f "/engine-cache/$SEED_MARKER" || exit 1
  complete=1; added=""; links=""; private=""; private_eg=""
  for d in vllm flashinfer triton pycache; do
    [ -d "$seed_dir/$d" ] || continue
    merge "$seed_dir/$d" "/engine-cache/$d"
  done
  merge_report
  if [ "$complete" = 1 ]; then
    echo "$want" > "/engine-cache/$SEED_MARKER" || exit 1
    echo "engine cache: seeded on $NODE_NAME from $want: ${#added} file(s) copied; what the node already had is kept"
  else
    echo "engine cache: seeding on $NODE_NAME from $want did not complete (a copy failed and was removed); it is retried when this pod next starts"
  fi
fi
touch "/engine-cache/$MARKER" || exit 1
echo "engine cache: prepared on $NODE_NAME: /engine-cache/{vllm,flashinfer,triton,pycache}"
trap 'exit 0' TERM; while :; do sleep 3600 & wait; done
SCRIPT
}

# seed_volume_yaml and seed_mount_yaml print the seed claim's volume and its
# read-only mount at /seed when --seed-claim was given, nothing otherwise.
seed_volume_yaml() {
    [ -n "$SEED_CLAIM" ] || return 0
    printf '        - name: seed\n          persistentVolumeClaim:\n            claimName: %s\n            readOnly: true\n' "$SEED_CLAIM"
}
# seed_probe prints the readiness probe's extra clause with a seed: the node
# carries the marker of THIS seed. Any stamp matches, which is safe only
# because the preparer drops the marker before it merges a new stamp: the
# marker a pod finds is the stamp that pod merged, never an older one left
# behind by a merge that failed. The probe is a single-quoted YAML scalar,
# so the double quotes in it need no escaping.
seed_probe() {
    [ -n "$SEED_CLAIM" ] || return 0
    local id="${SEED_CLAIM}${SEED_SUBPATH:+:$SEED_SUBPATH}"
    printf ' && grep -qx -e "%s" -e "%s@[0-9]*" -e "none:%s" /engine-cache/%s' "$id" "$id" "$id" "$SEED_MARKER"
}
# seed_mount_yaml mounts the whole claim, never a subPath: the kubelet creates
# a missing subPath, which fails on a fresh claim (the pod never starts). The
# preparer finds SEED_SUBPATH under /seed itself.
seed_mount_yaml() {
    [ -n "$SEED_CLAIM" ] || return 0
    printf '            - name: seed\n              mountPath: /seed\n              readOnly: true\n'
}

# check_seed refuses a seed that is not a claim name with an optional subPath
# under it: the claim is mounted as given, and a subPath with '..' would
# reach outside it.
check_seed() {
    local claim="${1%%:*}" sub=""
    [ "$claim" = "$1" ] || sub="${1#*:}"
    case "$claim" in
        ""|*[!a-z0-9.-]*) log_error "--seed-claim: not a claim name: '${claim}' (a DNS-1123 name, with an optional :subPath)" ;;
    esac
    [ "$claim" = "$1" ] || [ -n "$sub" ] || log_error "--seed-claim: '${1}' names an empty subPath; give PVC or PVC:SUBPATH"
    [ "$claim" != "$CLAIM" ] || log_error "--seed-claim: the seed cannot be the engine-cache claim itself"
    case "$sub" in
        *..*|/*|*[!A-Za-z0-9._/-]*) log_error "--seed-claim: not a subPath: '${sub}' (relative, path characters, no '..')" ;;
    esac
    SEED_CLAIM="$claim"
    SEED_SUBPATH="$sub"
}

# render prints the ServiceAccount, the volume, the claim and the preparer.
render() {
    local pv
    pv="$(pv_for)"
    cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SA_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
# The preparer's own identity, so what an admin grants for it (an SCC on
# OpenShift) does not land on the namespace's default ServiceAccount. It
# makes no API call, so no token.
automountServiceAccountToken: false
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${pv}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
  annotations:
    wva.llmd.ai/engine-cache-path: "${NODE_PATH}/${SUBDIR}"
spec:
  capacity:
    storage: ${CAPACITY}
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ${STORAGE_CLASS}
  # Bound to this claim and no other: a static volume without a claimRef is
  # taken by the first claim that asks for its class.
  claimRef:
    namespace: ${NAMESPACE}
    name: ${CLAIM}
  hostPath:
    path: ${NODE_PATH}/${SUBDIR}
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${CLAIM}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
  annotations:
    wva.llmd.ai/engine-cache-path: "${NODE_PATH}/${SUBDIR}"
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${STORAGE_CLASS}
  volumeName: ${pv}
  resources:
    requests:
      storage: ${CAPACITY}
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${CLAIM}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
  annotations:
    wva.llmd.ai/engine-cache-path: "${NODE_PATH}/${SUBDIR}"
    wva.llmd.ai/engine-cache-node-selector: "${NODE_SELECTOR}"
    wva.llmd.ai/engine-cache-seed: "${SEED_CLAIM}${SEED_SUBPATH:+:$SEED_SUBPATH}"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: node-local-engine-cache
      wva.llmd.ai/engine-cache: ${CLAIM}
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100%
  template:
    metadata:
      labels:
        app.kubernetes.io/name: node-local-engine-cache
        app.kubernetes.io/component: node-local-engine-cache
        app.kubernetes.io/managed-by: wva-enginecache
        wva.llmd.ai/engine-cache: ${CLAIM}
    spec:
      serviceAccountName: ${SA_NAME}
      automountServiceAccountToken: false
      securityContext:
        seccompProfile:
          type: RuntimeDefault
$(accelerator_placement_yaml "$NODE_SELECTOR")
      tolerations:
$(for t in "${TOLERATIONS[@]}"; do printf '        - key: "%s"\n          operator: Exists\n' "$t"; done)
      terminationGracePeriodSeconds: 1
      volumes:
        - name: engine-cache
          persistentVolumeClaim:
            claimName: ${CLAIM}
$(seed_volume_yaml)
      containers:
        - name: prepare
          image: "${IMAGE}"
          imagePullPolicy: IfNotPresent
          # One directory per cache, writable by any UID the engines run as
          # (1777: sticky, so one UID's files are not another's to remove --
          # and world-writable, so whoever can write here writes what every
          # engine on the node will load; see the header), then a marker,
          # then the pod stays, Ready, so the DaemonSet's numberReady is the
          # count of prepared nodes. The preparer creates the four
          # directories itself, so it owns them and the chmod takes, on
          # Kubernetes as root and on OpenShift as the project UID inside
          # the directory the admin prepared.
          command: ["/bin/sh", "-c"]
          args:
            - |
$(preparer_script | sed 's/^/              /')
          env:
            - name: MARKER
              value: "${MARKER}"
            - name: SEED_MARKER
              value: "${SEED_MARKER}"
            - name: SEED_ID
              value: "${SEED_CLAIM}${SEED_SUBPATH:+:$SEED_SUBPATH}"
            - name: SEED_SUBPATH
              value: "${SEED_SUBPATH}"
            # The node, for the log: HOSTNAME is the pod's name.
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            # Engine images bake in NVIDIA_VISIBLE_DEVICES=all, and the NVIDIA
            # runtime honours it from a container that requested no GPU.
            - name: NVIDIA_VISIBLE_DEVICES
              value: "void"
          volumeMounts:
            - name: engine-cache
              mountPath: /engine-cache
$(seed_mount_yaml)
          # Ready is "prepared, and writable by this UID": what the engines
          # will find, as the UID they run as -- and, with a seed, seeded from
          # it, so numberReady and status count a node whose seed did not
          # complete as not ready. The engines do not wait on the preparer.
          readinessProbe:
            exec:
              command: ["/bin/sh", "-c", 'test -f /engine-cache/${MARKER} && test -w /engine-cache/vllm$(seed_probe)']
            periodSeconds: 10
          resources:
            requests:
              cpu: 5m
              memory: 16Mi
            limits:
              memory: 64Mi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF
}

cmd_apply() {
    [ -n "$NAMESPACE" ] || log_error "apply needs -n NAMESPACE"
    [ -n "$NODE_PATH" ] || log_error "apply needs --path DIR (a directory on the node)"
    [ -n "$IMAGE" ] || log_error "apply needs --image IMG (an image with /bin/sh; the engine image)"
    check_path "$NODE_PATH"
    check_image "$IMAGE"
    if [ "$DRY_RUN" = true ]; then
        render
        return
    fi
    # Four documents in one apply: without leave for the cluster-scoped
    # volume, kubectl would still create the rest -- a Pending claim and
    # pods that never schedule -- and report only the volume's Forbidden.
    if ! kubectl auth can-i create persistentvolumes -A -q 2>/dev/null; then
        log_error "apply needs leave to create PersistentVolumes (cluster-scoped), which a namespace tenant does not have -- ask the cluster admin to run apply (storage-admin on OpenShift)"
    fi
    # A claim of this name on another class (or on none) is somebody else's:
    # the volume here would never bind to it, and the engines would go on
    # using it. And a claim's volume is immutable: a second apply with a new
    # --path would create a second volume, land the DaemonSet with the new
    # directory in its annotation, and leave the claim bound to the old one
    # -- status would then report a directory the engines do not mount.
    local existing
    existing="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.metadata.name}/{.spec.storageClassName}' 2>/dev/null || true)"
    if [ -n "$existing" ] && [ "${existing#*/}" != "$STORAGE_CLASS" ]; then
        log_error "namespace ${NAMESPACE} already has a claim named ${CLAIM} on storage class '${existing#*/}', which this script did not make; delete it or use a namespace without one"
    fi
    # The directory the claim is really on is its volume's, not the
    # DaemonSet's annotation (the DaemonSet may be gone; the claim stays bound).
    local volume applied
    volume="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)"
    applied=""
    [ -z "$volume" ] || applied="$(kubectl get pv "$volume" -o jsonpath='{.spec.hostPath.path}' 2>/dev/null || true)"
    if [ -n "$applied" ] && [ "$applied" != "${NODE_PATH}/${SUBDIR}" ]; then
        log_error "namespace ${NAMESPACE} already has its engine cache at ${applied} (volume ${volume}); the claim's volume cannot change. enginecache.sh delete -n ${NAMESPACE} first (the caches on the nodes stay), then apply with the new --path"
    fi
    if [ -n "$SEED_CLAIM" ]; then
        local seed_phase
        seed_phase="$(kubectl get pvc -n "$NAMESPACE" "$SEED_CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        [ "$seed_phase" = Bound ] || log_error "--seed-claim ${SEED_CLAIM}: no Bound claim of that name in ${NAMESPACE} (found: '${seed_phase:-none}'); the preparer would mount it and never start"
    fi
    # an existing claim keeps its size (deploy/lib/nodedir.sh says why)
    nodedir_claim_capacity "$NAMESPACE" "$CLAIM" "$CAPACITY_GIVEN" "enginecache.sh delete -n ${NAMESPACE}"
    render | kubectl apply -f - >/dev/null
    log_info "preparing ${NODE_PATH}/${SUBDIR} on $(accelerator_selector_text "$NODE_SELECTOR"); claim ${CLAIM}${SEED_CLAIM:+, seeded from ${SEED_CLAIM}${SEED_SUBPATH:+:$SEED_SUBPATH}} -- mount it read-write and point VLLM_CACHE_ROOT, FLASHINFER_WORKSPACE_DIR, TRITON_CACHE_DIR and PYTHONPYCACHEPREFIX (pycache/<image key>) under it"
    if kubectl api-resources --api-group=security.openshift.io 2>/dev/null | grep -q securitycontextconstraints; then
        log_warning "OpenShift: the preparer and the engines run as the project UID (GID 0) under restricted-v2 and the kubelet does not relabel a hostPath; on each node, before this takes: mkdir -p ${NODE_PATH}/${SUBDIR} && chgrp 0 ${NODE_PATH}/${SUBDIR} && chmod 2775 ${NODE_PATH}/${SUBDIR} && chcon -t container_file_t ${NODE_PATH}/${SUBDIR} (oc debug node/<n> -- chroot /host ...), or a MachineConfig mount with context=system_u:object_r:container_file_t:s0. Not yet run on OpenShift"
    fi
    if ! command -v jq >/dev/null 2>&1; then
        log_warning "jq is not installed; skipping the per-node report (enginecache.sh status needs it)"
        return 0
    fi
    # The claim binds to its static volume a few seconds after the apply;
    # give it 20 s, so the report below does not read that delay as a fault.
    local tries=0
    while [ "$tries" -lt 10 ] && [ "$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null || true)" = Pending ]; do
        tries=$((tries + 1))
        sleep 2
    done
    cmd_status report || true
}

# ec_nodes MODE resolves what status and verify both read off the cluster:
# EC_PATH, the node directory (the DaemonSet's annotation); EC_SELECTOR, the
# nodes it covers (--node-selector, or the live DaemonSet's nodeSelector);
# EC_EXCLUDED, the selected nodes no engine can use; EC_NODES_JSON, the
# usable rest, never empty. In report mode a failure is a warning and a
# return 1; otherwise it exits.
ec_nodes() {
    local mode="$1" why
    EC_SELECTOR="$NODE_SELECTOR"
    EC_PATH="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/engine-cache-path}' 2>/dev/null || true)"
    if [ -z "$EC_PATH" ]; then
        if [ "$mode" = report ]; then
            log_warning "no engine-cache DaemonSet in ${NAMESPACE}"
            return 1
        fi
        log_error "no engine-cache DaemonSet in ${NAMESPACE} (enginecache.sh apply makes one)"
    fi
    if [ "$NODE_SELECTOR_GIVEN" = false ]; then
        # Off the live DaemonSet's pod template, held to the same rule as
        # the flag before it reaches kubectl (a tenant can edit the object).
        EC_SELECTOR="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" \
            -o jsonpath='{.spec.template.spec.nodeSelector}' 2>/dev/null \
            | accelerator_selector_from_json || true)"
        if ! why="$(accelerator_selector_ok "$EC_SELECTOR")"; then
            [ "$mode" = report ] || log_error "DaemonSet ${CLAIM} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
            log_warning "DaemonSet ${CLAIM} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
            return 1
        fi
    fi
    local err_file
    err_file="$(mktemp)"
    if ! EC_NODES_JSON="$(accelerator_nodes_json "$EC_SELECTOR" 2>"$err_file")"; then
        if grep -q Forbidden "$err_file"; then
            why="a namespace tenant may not list nodes; this needs cluster-scoped node read (cluster-reader) -- ask for it or check with the cluster admin"
        else
            why="$(tr '\n' ' ' < "$err_file")"
        fi
        rm -f "$err_file"
        [ "$mode" = report ] || log_error "cannot list nodes: ${why}"
        log_warning "cannot list nodes: ${why}"
        return 1
    fi
    rm -f "$err_file"
    # Usable nodes only: a cordoned or foreign-tainted node can never hold a
    # copy an engine starts on, and counting it failed finished preparations.
    EC_EXCLUDED="$(printf '%s' "$EC_NODES_JSON" | accelerator_excluded_json "$(accelerator_ds_toleration_keys "$NAMESPACE" "$CLAIM" "${TOLERATIONS[@]}")")"
    EC_NODES_JSON="$(printf '%s' "$EC_NODES_JSON" | accelerator_usable_nodes_json "$EC_EXCLUDED")"
    if [ "$(printf '%s' "$EC_NODES_JSON" | jq '.items | length')" -eq 0 ]; then
        accelerator_print_excluded "$EC_EXCLUDED" >&2
        [ "$mode" = report ] || log_error "no node matches $(accelerator_selector_text "$EC_SELECTOR") (--node-selector picks the nodes)"
        log_warning "no node matches $(accelerator_selector_text "$EC_SELECTOR"); the cache is prepared nowhere (--node-selector picks the nodes)"
        return 1
    fi
}

# claim_unbound_reason says why the claim is not Bound. A claim on a static
# volume binds a few seconds after the volume exists, so a volume that exists
# is a delay, not a fault. Only a volume that is not there is blamed on leave
# to create it, and a volume that cannot be read is said to be unknown.
claim_unbound_reason() {
    local vol found err_file
    vol="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)"
    if [ -z "$vol" ]; then
        echo "  the claim is not bound and names no volume -- re-run apply"
        return
    fi
    err_file="$(mktemp)"
    if ! found="$(kubectl get pv "$vol" -o name --ignore-not-found 2>"$err_file")"; then
        echo "  the claim is not bound, and its volume ${vol} could not be read ($(tr '\n' ' ' < "$err_file")) -- whether it exists is not known; reading PersistentVolumes needs cluster-scoped read"
    elif [ -n "$found" ]; then
        echo "  the claim is not bound yet: its volume ${vol} exists, and a claim on it binds within seconds -- run status again"
    else
        echo "  the claim is not bound: its volume ${vol} is cluster-scoped and was not created -- apply needs leave to create PersistentVolumes (ask the cluster admin to run apply)"
    fi
    rm -f "$err_file"
}

# cmd_status prints, per node, whether the node is prepared. Ready is the
# marker present and the directory writable by the preparer's UID; a pod
# that keeps failing shows its reason, and no pod at all is a node the
# DaemonSet did not reach. Exits non-zero while any selected node is not
# prepared or the claim is not Bound.
cmd_status() {
    local mode="${1:-verdict}"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for status (the node and pod lists are read with it)"
    [ -n "$NAMESPACE" ] || log_error "status needs -n NAMESPACE"
    local rc=0 ready=0 holders=0
    ec_nodes "$mode" || return 1
    local path="$EC_PATH" selector="$EC_SELECTOR" nodes_json="$EC_NODES_JSON" excluded="$EC_EXCLUDED" node_count
    node_count="$(printf '%s' "$nodes_json" | jq '.items | length')"
    local pods_file bound
    pods_file="$(mktemp)"
    kubectl get pods -n "$NAMESPACE" -l "$LABEL_COMPONENT" -o json > "$pods_file"
    bound="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    local seed
    seed="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/engine-cache-seed}' 2>/dev/null || true)"
    echo "${path}  (claim ${CLAIM}: ${bound:-missing}; seed: ${seed:-none}; nodes: $(accelerator_selector_text "$selector"))"
    if [ "$bound" != Bound ]; then
        rc=1
        [ -z "$bound" ] || claim_unbound_reason
    fi
    printf '  %-28s %-12s %s\n' NODE CACHE PREPARER
    accelerator_print_excluded "$excluded"
    local eacces=0
    # The unit separator keeps empty fields; a tab in IFS would swallow them.
    while IFS=$'\x1f' read -r node phase reason podname message; do
        local has=absent
        [ "$phase" = "no pod" ] || holders=$((holders + 1))
        if [ "$phase" = Ready ]; then
            has=prepared
            ready=$((ready + 1))
        else
            rc=1
        fi
        printf '  %-28s %-12s %s %s\n' "$node" "$has" "$phase" "$reason"
        # the reason carries the last exit after it ("CrashLoopBackOff (last exit 1 Error)")
        case "$reason" in
            CrashLoopBackOff*|Error*)
                if kubectl logs -n "$NAMESPACE" "$podname" --tail=20 2>/dev/null | grep -q 'Permission denied\|Read-only file system'; then
                    echo "    the preparer cannot write ${path}: $(kubectl logs -n "$NAMESPACE" "$podname" --tail=20 2>/dev/null | grep -o 'Permission denied\|Read-only file system' | head -1) in its log"
                    eacces=$((eacces + 1))
                fi ;;
            CreateContainerConfigError) [ -z "$message" ] || echo "    ${message}" ;;
        esac
    done < <(printf '%s' "$nodes_json" | jq -r --arg ds "$CLAIM" --slurpfile pods "$pods_file" \
        '.items[] as $node
         | ($pods[0].items | map(select(.metadata.labels["wva.llmd.ai/engine-cache"] == $ds
              and (.spec.nodeName == $node.metadata.name
                   or ((.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // [])[0].matchFields // [])[0].values[0]? == $node.metadata.name))) | first) as $pod
         | [ $node.metadata.name,
             (if $pod == null then "no pod"
              elif $pod.status.phase == "Running" and (($pod.status.containerStatuses // []) | map(.ready) | all) then "Ready"
              elif $pod.status.phase == "Running" then "Running (not ready)"
              else $pod.status.phase + " (not ready)" end),
             (if $pod == null then ""
              else ($pod.status.reason // ((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.reason // "")))
                   + ((($pod.status.containerStatuses // [])[0].lastState.terminated) | if . then " (last exit " + (.exitCode | tostring) + (if .reason then " " + .reason else "" end) + ")" else "" end) end),
             ($pod.metadata.name // ""),
             (if $pod == null then "" else (((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.message // "")) | gsub("[\\t\\n]"; " ")) end)
           ] | join("")')
    rm -f "$pods_file"
    if [ "$eacces" -gt 0 ]; then
        echo "  ${eacces} node(s): the node directory is not writable by the preparer. On Kubernetes the preparer is root and the kubelet creates the directory root-owned, so this is a mount that is read-only or not there, or a directory that already existed with another owner (ls -ld on the node); on OpenShift it runs as the project UID (GID 0) under restricted-v2 and the directory is not relabelled -- on each node: mkdir -p DIR && chgrp 0 DIR && chmod 2775 DIR && chcon -t container_file_t DIR, or a MachineConfig mount with context=...:container_file_t:s0 (enginecache.sh --help)"
    fi
    echo "  ${ready}/${node_count} usable nodes prepared; $((node_count - ready)) not"
    [ -n "$selector" ] || printf '%s' "$nodes_json" | accelerator_vendor_warning
    if [ "$holders" -eq 0 ]; then
        local why
        why="$(kubectl get events -n "$NAMESPACE" \
            --field-selector "involvedObject.kind=DaemonSet,involvedObject.name=${CLAIM},reason=FailedCreate" \
            -o jsonpath='{.items[-1:].message}' 2>/dev/null || true)"
        [ -n "$why" ] && echo "  the DaemonSet cannot create its pods: ${why}"
    fi
    return $rc
}

run_delete() {
    if [ "$DRY_RUN" = true ]; then
        echo "would run: kubectl $*"
    else
        kubectl "$@"
    fi
}

# cmd_delete removes the preparer, the claim, the volume and the
# ServiceAccount. The caches on the nodes stay: the next apply finds them
# warm. The volume is found by this script's labels and its claimRef into
# this namespace, never by a name read off the claim (a tenant's object).
cmd_delete() {
    [ -n "$NAMESPACE" ] || log_error "delete needs -n NAMESPACE"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for delete (the volume is matched on its claimRef with it)"
    local volumes=() line
    while IFS= read -r line; do
        [ -n "$line" ] && volumes+=("$line")
    done < <(kubectl get pv -l "$LABEL_COMPONENT" -o json 2>/dev/null \
        | jq -r --arg ns "$NAMESPACE" --arg claim "$CLAIM" \
            '.items[] | select(.spec.claimRef.namespace == $ns and .spec.claimRef.name == $claim) | .metadata.name' \
        2>/dev/null || true)
    run_delete delete daemonset -n "$NAMESPACE" "$CLAIM" --ignore-not-found
    run_delete delete pvc -n "$NAMESPACE" "$CLAIM" --ignore-not-found
    [ "${#volumes[@]}" -eq 0 ] || run_delete delete pv "${volumes[@]}" --ignore-not-found
    run_delete delete serviceaccount -n "$NAMESPACE" "$SA_NAME" --ignore-not-found
    return 0
}

# publish_script runs on one node: it merges that node's caches into the seed
# claim -- an entry the seed already has is kept -- and stamps the seed, so
# every preparer merges it again on its next start. A quoted heredoc, for the
# same reasons as preparer_script.
publish_script() {
    cat <<'SCRIPT'
dst="/seed${SEED_SUBPATH:+/$SEED_SUBPATH}"
# umask 022: every directory mkdir -p creates is readable and searchable by
# every user, as the merge makes its own; one that exists is left as it is
(umask 022; mkdir -p "$dst") || exit 1
# cp -a without ownership is GNU only; busybox cp refuses the option. Try it
# once; without it a plain cp copies the content and mode, not the times.
cpa=0
: > /tmp/.ec-cp-check 2>/dev/null && cp -a --no-preserve=ownership /tmp/.ec-cp-check /tmp/.ec-cp-check.2 2>/dev/null && cpa=1
rm -f /tmp/.ec-cp-check /tmp/.ec-cp-check.2
# merge SRC DST: an entry DST has is kept, one it lacks is copied, a directory
# both have is descended -- so a second engine config's key lands beside the
# node's own. It walks every directory and copies one file at a time:
#  * a symlink is skipped and logged: vLLM writes none, and a link is not
#    the cache (it would point at whatever the target is on each node);
#  * a file not every user can read is copied readable by every user, and
#    counted once its copy succeeds: the engine may run as another UID;
#  * a new directory is removed whole when any copy under it fails, so no
#    half-copied key is left behind, and the merge is marked incomplete;
#    what was counted under it is uncounted;
#    a directory that cannot be listed is a failed copy, not an empty one.
# The copier owns every copy, whatever owned the original. The globs match
# every name: plain, one leading dot, and two leading dots (..x).
merge() {
  local e n t was wa wp wpe pub
  # a directory that cannot be listed would glob to nothing and read as empty
  ls -a "$1" >/dev/null || { complete=0; return; }
  for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    [ -e "$e" ] || [ -L "$e" ] || continue
    n="${e##*/}"; t="$2/$n"
    if [ -L "$e" ]; then
      links="$links."; echo "engine cache: skipped $e: a symlink, not cache content"
    elif [ -e "$t" ] || [ -L "$t" ]; then
      if [ -d "$e" ] && [ -d "$t" ] && [ ! -L "$t" ]; then merge "$e" "$t"; fi
    elif [ -d "$e" ]; then
      was="$complete"; wa="$added"; wp="$private"; wpe="$private_eg"; complete=1
      mkdir "$t" && chmod a+rx "$t" || complete=0
      [ "$complete" = 0 ] || merge "$e" "$t"
      if [ "$complete" = 1 ]; then complete="$was"; else complete=0; rm -rf "$t"; added="$wa"; private="$wp"; private_eg="$wpe"; fi
    elif [ -f "$e" ]; then
      pub=0; ls -ld "$e" | grep -q '^-......r' && pub=1
      if [ "$cpa" = 1 ]; then cp -a --no-preserve=ownership "$e" "$t"; else cp "$e" "$t"; fi && chmod a+r "$t" || { complete=0; rm -f "$t"; continue; }
      added="$added."
      [ "$pub" = 1 ] || { private="$private."; [ -n "$private_eg" ] || private_eg="$e"; }
    else
      echo "engine cache: skipped $e: not a file or a directory"
    fi
  done
}
# merge_report prints what the merge skipped or changed.
merge_report() {
  [ -z "$links" ] || echo "engine cache: ${#links} symlink(s) skipped (named above)"
  [ -z "$private" ] || echo "engine cache: ${#private} file(s) not readable by every user were copied readable by every user, for example $private_eg"
}
complete=1; added=""; links=""; private=""; private_eg=""
for d in vllm flashinfer triton pycache; do
  [ -d "/engine-cache/$d" ] || continue
  (umask 022; mkdir -p "$dst/$d") || exit 1
  merge "/engine-cache/$d" "$dst/$d"
done
merge_report
[ "$complete" = 1 ] || { echo "engine cache: publishing from $NODE_NAME did not complete (a copy failed and was removed)"; exit 1; }
echo "engine cache: published from $NODE_NAME: ${#added} file(s) copied into the seed; what the seed already had is kept"
SCRIPT
}

# pod_succeeded POD TIMEOUT_S waits for a run-once pod to finish: 0 when it
# Succeeded, 1 as soon as it Failed or when TIMEOUT_S passes. `kubectl wait`
# on one phase would sit out the whole timeout on a pod that already failed.
pod_succeeded() {
    local pod="$1" deadline=$((SECONDS + $2)) phase
    while [ "$SECONDS" -lt "$deadline" ]; do
        phase="$(kubectl get pod -n "$NAMESPACE" "$pod" -o jsonpath='{.status.phase}' --request-timeout=30s 2>/dev/null)"
        case "$phase" in
            Succeeded) return 0 ;;
            Failed) return 1 ;;
        esac
        sleep 5
    done
    return 1
}

# stale_preparers GEN prints how many live (Pending or Running) preparer pods
# carry a pod-template-generation other than GEN, the DaemonSet's TEMPLATE
# generation (its deprecated.daemonset.template.generation annotation, which
# that label follows; .metadata.generation also moves on non-template edits).
# A pod being deleted on a node that is not Ready is not counted: its kubelet
# cannot confirm the deletion, so it would hold the wait to its timeout.
# Prints "?" when the pods cannot be read.
stale_preparers() {
    local pods ready
    pods="$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=node-local-engine-cache -o json --request-timeout=30s 2>/dev/null)" || pods=""
    [ -n "$pods" ] || { echo "?"; return; }
    # Unreadable nodes (a namespace-scoped identity): count every live pod.
    ready="$(kubectl get nodes -o json --request-timeout=30s 2>/dev/null \
        | jq -c '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True")) | .metadata.name]' 2>/dev/null)" || ready=""
    [ -n "$ready" ] || ready=null
    printf '%s' "$pods" | jq -r --arg g "$1" --argjson ready "$ready" '[.items[]
        | select(.status.phase == "Pending" or .status.phase == "Running")
        | .spec.nodeName as $n
        | select(.metadata.deletionTimestamp == null or $ready == null or ($ready | any(.[]; . == $n)))
        | select(.metadata.labels["pod-template-generation"] != $g)] | length' 2>/dev/null || echo "?"
}

# cmd_publish fills the seed from the nodes: every node whose preparer is Ready
# merges its caches into the seed claim, one node at a time, then the seed is
# stamped and the preparers restart, so each node merges in what any node
# compiled. Run it once every engine configuration has started somewhere; after
# it no node starts any of them cold.
cmd_publish() {
    command -v jq >/dev/null 2>&1 || log_error "jq is required for publish"
    [ -n "$NAMESPACE" ] || log_error "publish needs -n NAMESPACE"
    local seed image claim sub
    seed="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/engine-cache-seed}' 2>/dev/null || true)"
    [ -n "$seed" ] || log_error "publish needs an engine cache applied with --seed-claim in ${NAMESPACE} (there is nothing to publish into)"
    claim="${seed%%:*}"; sub=""
    [ "$claim" = "$seed" ] || sub="${seed#*:}"
    image="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.spec.template.spec.containers[0].image}')"
    local nodes
    nodes="$(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=node-local-engine-cache,wva.llmd.ai/engine-cache=${CLAIM}" -o json \
        | jq -r '.items[] | select(any(.status.conditions[]?; .type=="Ready" and .status=="True")) | .spec.nodeName' | sort -u)"
    [ -n "$nodes" ] || log_error "no Ready engine-cache preparer in ${NAMESPACE}: nothing to publish from (enginecache.sh status says why)"
    local stamp i=0 node pod
    stamp="$(date -u +%Y%m%d%H%M%S)"
    for node in $nodes; do
        i=$((i + 1)); pod="${CLAIM}-publish-${i}"
        local manifest
        manifest="$(cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-engine-cache-publish
    app.kubernetes.io/managed-by: wva-enginecache
spec:
  nodeName: ${node}
  restartPolicy: Never
  serviceAccountName: ${SA_NAME}
  automountServiceAccountToken: false
  securityContext:
    seccompProfile:
      type: RuntimeDefault
  tolerations:
    - operator: Exists
  volumes:
    - name: engine-cache
      persistentVolumeClaim:
        claimName: ${CLAIM}
        readOnly: true
    - name: seed
      persistentVolumeClaim:
        claimName: ${claim}
  containers:
    - name: publish
      image: "${image}"
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh", "-c"]
      args:
        - |
$(publish_script | sed 's/^/          /')
      env:
        - name: SEED_SUBPATH
          value: "${sub}"
        - name: NODE_NAME
          value: "${node}"
        - name: NVIDIA_VISIBLE_DEVICES
          value: "void"
      volumeMounts:
        - name: engine-cache
          mountPath: /engine-cache
          readOnly: true
        - name: seed
          mountPath: /seed
      resources:
        requests:
          cpu: 50m
          memory: 32Mi
        limits:
          memory: 256Mi
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
EOF
)"
        if [ "$DRY_RUN" = true ]; then printf '%s\n---\n' "$manifest"; continue; fi
        kubectl delete pod -n "$NAMESPACE" "$pod" --ignore-not-found --wait=true --timeout=60s >/dev/null
        printf '%s\n' "$manifest" | kubectl apply -f - >/dev/null || log_error "publish: could not create ${pod} on ${node}"
        if ! pod_succeeded "$pod" 900; then
            kubectl logs -n "$NAMESPACE" "$pod" 2>&1 | tail -5 >&2
            kubectl delete pod -n "$NAMESPACE" "$pod" --ignore-not-found --wait=false >/dev/null 2>&1
            log_error "publish: ${pod} on ${node} did not succeed (its log is above); the seed is not stamped, so no node re-seeds from a partial copy"
        fi
        kubectl logs -n "$NAMESPACE" "$pod" 2>/dev/null | tail -1
        kubectl delete pod -n "$NAMESPACE" "$pod" --ignore-not-found --wait=false >/dev/null
    done
    [ "$DRY_RUN" = false ] || return 0
    # Stamp the seed only after every node published, from the last node's view.
    local stamp_pod="${CLAIM}-publish-stamp"
    kubectl delete pod -n "$NAMESPACE" "$stamp_pod" --ignore-not-found --wait=true --timeout=60s >/dev/null
    kubectl run -n "$NAMESPACE" "$stamp_pod" --image="$image" --restart=Never --quiet \
        --overrides="{\"spec\":{\"nodeName\":\"${node}\",\"tolerations\":[{\"operator\":\"Exists\"}],\"serviceAccountName\":\"${SA_NAME}\",\"automountServiceAccountToken\":false,\"volumes\":[{\"name\":\"seed\",\"persistentVolumeClaim\":{\"claimName\":\"${claim}\"}}],\"containers\":[{\"name\":\"stamp\",\"image\":\"${image}\",\"command\":[\"/bin/sh\",\"-c\",\"echo ${stamp} > /seed${sub:+/$sub}/.published\"],\"env\":[{\"name\":\"NVIDIA_VISIBLE_DEVICES\",\"value\":\"void\"}],\"volumeMounts\":[{\"name\":\"seed\",\"mountPath\":\"/seed\"}]}]}}" >/dev/null \
        || log_error "publish: could not create ${stamp_pod}"
    pod_succeeded "$stamp_pod" 300 \
        || { kubectl delete pod -n "$NAMESPACE" "$stamp_pod" --ignore-not-found --wait=false >/dev/null 2>&1; log_error "publish: the seed was filled but not stamped; rerun publish"; }
    kubectl delete pod -n "$NAMESPACE" "$stamp_pod" --ignore-not-found --wait=false >/dev/null
    kubectl rollout restart -n "$NAMESPACE" "daemonset/${CLAIM}" >/dev/null || log_error "publish: could not restart the preparers"
    local gen
    gen="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.metadata.annotations.deprecated\.daemonset\.template\.generation}')"
    sleep 5
    reseed_wait "$gen" 600 \
        || log_error "publish: the seed is stamped ${stamp}, but not every usable node merged it within 600 s. A node whose merge failed is not prepared: enginecache.sh status -n ${NAMESPACE} says which, and that node's preparer log says why"
    log_success "published from ${i} node(s) into ${seed} (stamp ${stamp}); every usable node merged it"
}

# reseed_wait GEN TIMEOUT_S waits for the preparers to merge a new stamp: 0
# once every live preparer is of template GEN and status passes, 1 when
# TIMEOUT_S passes first. It waits on the usable nodes, as status counts them:
# `rollout status` also waits for a cordoned node's pod, which never comes up.
# status alone passes on the preparers from before the restart (still Ready),
# so the wait also needs every live preparer to be of the restarted template.
reseed_wait() {
    local gen="$1" deadline=$((SECONDS + $2))
    until [ -n "$gen" ] && [ "$(stale_preparers "$gen")" = 0 ] && bash "$HERE/enginecache.sh" status -n "$NAMESPACE" >/dev/null 2>&1; do
        [ "$SECONDS" -lt "$deadline" ] || return 1
        sleep 10
    done
}

# KEY_RE matches a compile cache path in an engine's log, up to its rank
# directory. vLLM (v0.31.0) builds two layouts under VLLM_CACHE_ROOT:
#   torch_compile_cache/<hash>/rank_<r>_<dp>/<prefix>         backends.py, logged as
#     "Using cache directory: %s for vLLM's torch.compile"
#   torch_compile_cache/torch_aot_compile/<hash>/rank_<r>_<dp>/model
#                                                             decorators.py (VLLM_USE_AOT_COMPILE), logged as
#     "Directly load AOT compilation from path %s" or "saved AOT compiled function to %s"
# The key is the path between VLLM_CACHE_ROOT and the rank directory; the
# root is whatever precedes the first /torch_compile_cache/ (/ when nothing
# does).
KEY_RE='/([A-Za-z0-9._/-]*/)?torch_compile_cache/(torch_aot_compile/)?[A-Za-z0-9_-]+/rank_[0-9]+_[0-9]+'
# KEY_FAILED_RE matches the decorators.py lines that name an AOT path the
# engine did not leave a usable cache at: a save that failed, and a load that
# failed and compiled again. No key is taken from them.
KEY_FAILED_RE='unable to save AOT compiled function to |Compiling model again due to a load failure from '

# normalize_key prints KEY[@RANK[+RANK...]] with KEY as a directory under
# VLLM_CACHE_ROOT: a bare hash is torch_compile_cache/<hash>. Each RANK is a
# rank directory (rank_<r>_<dp>) verify requires under the key. Refuses
# anything that could leave the cache.
normalize_key() {
    local spec="$1" key ranks="" r
    key="${spec%%@*}"
    if [ "$key" != "$spec" ]; then
        ranks="${spec#*@}"
        local IFS=+
        for r in $ranks; do
            [[ "$r" =~ ^rank_[0-9]+_[0-9]+$ ]] || log_error "--keys: not a rank directory: '${r}' in '${spec}' (KEY@rank_<r>_<dp>[+rank_<r>_<dp>...], as enginecache.sh keys prints it)"
        done
        [[ "$ranks" =~ ^rank_[0-9]+_[0-9]+(\+rank_[0-9]+_[0-9]+)*$ ]] || log_error "--keys: not a rank list: '${ranks}' in '${spec}' (rank_<r>_<dp>[+rank_<r>_<dp>...])"
    fi
    case "$key" in
        ""|/*|*/|*..*|*//*|.|./*|*/.|*/./*|*[!A-Za-z0-9._/-]*) log_error "--keys: not a compile cache key: '${spec}' (torch_compile_cache/<hash>[@rank_0_0+...], as enginecache.sh keys prints it, or the bare hash)" ;;
        */*) printf '%s' "$key" ;;
        *) printf 'torch_compile_cache/%s' "$key" ;;
    esac
    [ -z "$ranks" ] || printf '@%s' "$ranks"
}

# cmd_keys prints the compile cache key(s) each named engine pod used, one per
# line, read off its log, as KEY@RANK[+RANK...]: every rank directory the log
# names under that key, so verify can require each of them. A key the engine
# wrote outside VLLM_CACHE_ROOT (the guard fell back to the engine default) is
# still printed -- it is the same hash -- with a warning that this start did
# not land on the node cache.
cmd_keys() {
    [ -n "$NAMESPACE" ] || log_error "keys needs -n NAMESPACE"
    [ "${#PODS[@]}" -gt 0 ] || log_error "keys needs the engine pod(s): enginecache.sh keys -n NAMESPACE POD [POD...]"
    local pod root log found path base rest pairs="" warned all rc=0 err_file line
    err_file="$(mktemp)"
    for pod in "${PODS[@]}"; do
        if ! root="$(kubectl get pod -n "$NAMESPACE" "$pod" -o jsonpath="{.spec.containers[?(@.name==\"${CONTAINER}\")].env[?(@.name==\"VLLM_CACHE_ROOT\")].value}" 2>"$err_file")"; then
            log_warning "cannot read pod ${pod}: $(tr '\n' ' ' < "$err_file")"
            rc=1; continue
        fi
        if ! log="$(kubectl logs -n "$NAMESPACE" "$pod" -c "$CONTAINER" 2>"$err_file")"; then
            log_warning "cannot read the log of ${pod} (container ${CONTAINER}): $(tr '\n' ' ' < "$err_file")"
            rc=1; continue
        fi
        while IFS= read -r line; do
            path="$(printf '%s\n' "$line" | grep -oE "$KEY_RE" | head -n 1 || true)"
            [ -n "$path" ] || continue
            case "$line" in
                *"unable to save"*) log_warning "${pod}: the engine could not save its AOT compile to ${path}; that line gives no key" ;;
                *) log_warning "${pod}: the AOT compile at ${path} failed to load and was compiled again; that line gives no key" ;;
            esac
        done < <(printf '%s\n' "$log" | grep -E "$KEY_FAILED_RE" || true)
        found="$(printf '%s\n' "$log" | grep -vE "$KEY_FAILED_RE" | grep -oE "$KEY_RE" || true)"
        if [ -z "$found" ]; then
            log_warning "${pod}: no compile cache directory in its log (no \"Using cache directory\" or AOT load/save line under a torch_compile_cache directory: the log rotated, the engine does not compile, or its cache_dir is set by hand)"
            rc=1; continue
        fi
        while [ "$root" != / ] && [ "${root%/}" != "$root" ]; do root="${root%/}"; done
        warned=" "
        for path in $found; do
            base="${path%%/torch_compile_cache/*}"
            rest="${path#"$base"/}"
            [ -n "$base" ] || base=/
            # one "KEY RANK" line per rank directory the log names
            pairs="${pairs}${rest%/*} ${rest##*/}"$'\n'
            case "$warned" in *" ${base} "*) continue ;; esac
            warned="${warned}${base} "
            if [ -z "$root" ]; then
                log_warning "${pod}: container ${CONTAINER} sets no VLLM_CACHE_ROOT; it compiled under ${base}, which may not be the node cache"
            elif [ "${root#/}" = "$root" ]; then
                # a relative root is resolved against the engine working directory, which the log does not show
                log_warning "${pod}: VLLM_CACHE_ROOT=${root} is a relative path, so whether this start filled the node cache cannot be told; set it to the absolute mount path"
            elif [ "$base" != "$root" ]; then
                log_warning "${pod}: compiled under ${base}, not VLLM_CACHE_ROOT=${root} -- the cache root was not writable and the engine fell back; this start did not fill the node cache"
            fi
        done
    done
    rm -f "$err_file"
    # KEY@RANK+RANK, keys and ranks in the order the logs first name them
    all="$(printf '%s' "$pairs" | awk 'NF == 2 {
            if (!($1 in seen)) { seen[$1] = 1; order[++n] = $1; ranks[$1] = "" }
            if (!(($1, $2) in got)) { got[$1, $2] = 1; ranks[$1] = ranks[$1] (ranks[$1] == "" ? "" : "+") $2 }
        } END { for (i = 1; i <= n; i++) print order[i] "@" ranks[order[i]] }')"
    if [ -n "$all" ]; then
        printf '%s\n' "$all"
        echo "check every node with: make engine-cache-verify NAMESPACE=${NAMESPACE} ENGINE_CACHE_KEYS=$(printf '%s' "$all" | tr '\n' ',')" >&2
    fi
    return $rc
}

# verify_script runs inside a preparer pod (kubectl exec, no GPU) as
# `sh -c SCRIPT sh ROOT SPEC...`, each SPEC a KEY[@RANK[+RANK...]]: per
# spec, one line "ecv <result> <spec> [detail]" -- present (with the file
# count and the UID the modes were read for), absent (with the rank
# directories missing, when ranks were named), empty, unreadable ("read
# PATH" for a file the read failed on, "mode UID PATH" for one the engine's
# UID could not read, the key's parent directories included, "link PATH" for
# a symbolic link), or unchecked (no find, a find that rejects the mode
# check, or no owner to read the modes with). Every file is read in full,
# so a directory listed but unreadable is not called present. The read runs
# as the preparer's user, root on Kubernetes, which reads any file; so the
# modes are read too, for the engine's UID: ENGINE_CACHE_UID when set (the
# script's first line, euid=), else the owner of the key directory -- the
# engine that wrote the key, or root for a seeded or published copy. vLLM
# writes its AOT files 600 (measured, v0.31.0): readable by the engine that
# owns them, and by no other UID. vLLM writes no symbolic link, so one anywhere under the
# key, or as a directory on the way to it, is unreadable and never followed:
# a link to a file outside the node cache is not the cache. A quoted heredoc,
# like the others; dash has `local`.
verify_script() {
    printf 'euid=%s\n' "$ENGINE_UID"
    cat <<'VERIFY'
root="$1"; shift
walk() {
  local e
  if [ ! -r "$1" ] || [ ! -x "$1" ]; then bad="$1"; return; fi
  for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    if [ -L "$e" ]; then link="$e"; return 0; fi
    [ -e "$e" ] || continue
    if [ -d "$e" ]; then
      walk "$e"
    elif [ -f "$e" ]; then
      if cat "$e" > /dev/null 2>&1; then files="$files."; else bad="$e"; fi
    fi
    [ -z "$bad$link" ] || return 0
  done
}
# linked prints the first component of KEY under ROOT that is a symbolic link
linked() {
  local p="$1" rest="$2" c
  while [ -n "$rest" ]; do
    c="${rest%%/*}"
    if [ "$c" = "$rest" ]; then rest=""; else rest="${rest#*/}"; fi
    p="$p/$c"
    if [ -L "$p" ]; then echo "$p"; return; fi
  done
}
# above DIR UID GID prints DIR when it is a directory UID cannot search: the
# directories above a key are only passed through, so x is all they need (a
# 711 volume mount is fine). GID empty: only u and o count.
above() {
  if [ -n "$3" ]; then
    find "$1" -prune -type d \( \( -user "$2" ! -perm -100 \) -o \( ! -user "$2" -group "$3" ! -perm -010 \) -o \( ! -user "$2" ! -group "$3" ! -perm -001 \) \) 2> /dev/null
  else
    find "$1" -prune -type d \( \( -user "$2" ! -perm -100 \) -o \( ! -user "$2" ! -perm -001 \) \) 2> /dev/null
  fi
}
# private ROOT KEY prints "ok UID" when UID can search every directory from
# the parent of ROOT (the volume mount) down to the key directory ROOT/KEY,
# and read every file and read and search every directory from the key
# directory down, else "no UID PATH" for the first one it cannot -- UID
# being $euid when set, else the owner of the key directory, the engine
# that wrote the key (root, for a seeded or published copy). A path owned by
# UID needs u+r; one in the key directory's group, g+r (the engine is taken
# to be in that group); any other, o+r (directories: r and x likewise; above
# the key directory, x alone). With
# $euid the UID's groups are not known, so only u and o count. "? WHY" when
# the modes or the owner cannot be read -- including a find that rejects any
# part of the expression, probed whole before it is trusted: an expression
# find refuses prints nothing, which would read as "every file readable".
private() {
  local root="$1" d="$1/$2" rest="$2" a c o u g f miss
  command -v find > /dev/null 2>&1 || { echo "? no-find"; return; }
  o="$(ls -nd "$d" 2> /dev/null | awk '{ print $3, $4 }')"
  if [ -n "$euid" ]; then u="$euid"; g=""; else u="${o%% *}"; g="${o#* }"; fi
  case "$u" in ""|*[!0-9]*) echo "? no-owner"; return ;; esac
  case "$g" in *[!0-9]*) echo "? no-owner"; return ;; esac
  if [ -n "$g" ]; then
    set -- \( -type f \( \( -user "$u" ! -perm -400 \) -o \( ! -user "$u" -group "$g" ! -perm -040 \) -o \( ! -user "$u" ! -group "$g" ! -perm -004 \) \) \) \
      -o \( -type d \( \( -user "$u" ! -perm -500 \) -o \( ! -user "$u" -group "$g" ! -perm -050 \) -o \( ! -user "$u" ! -group "$g" ! -perm -005 \) \) \)
  else
    set -- \( -type f \( \( -user "$u" ! -perm -400 \) -o \( ! -user "$u" ! -perm -004 \) \) \) \
      -o \( -type d \( \( -user "$u" ! -perm -500 \) -o \( ! -user "$u" ! -perm -005 \) \) \)
  fi
  if ! find "$d" -prune \( "$@" \) > /dev/null 2>&1; then
    miss=""
    find "$d" -prune -user "$u" > /dev/null 2>&1 || miss="$miss -user"
    [ -z "$g" ] || find "$d" -prune -group "$g" > /dev/null 2>&1 || miss="$miss -group"
    find "$d" -prune -perm -400 > /dev/null 2>&1 || miss="$miss -perm"
    echo "? no-find-modes$miss"; return
  fi
  # the directories on the way: the volume mount (the cache root's parent),
  # the cache root, then each component of KEY above the key directory:
  # search only
  a="${root%/*}"
  if [ -n "$a" ]; then
    f="$(above "$a" "$u" "$g")"
    [ -z "$f" ] || { echo "no $u $f"; return; }
  fi
  a="$root"
  while :; do
    f="$(above "$a" "$u" "$g")"
    [ -z "$f" ] || { echo "no $u $f"; return; }
    [ -n "$rest" ] || break
    c="${rest%%/*}"
    if [ "$c" = "$rest" ]; then rest=""; else rest="${rest#*/}"; fi
    a="$a/$c"
    [ -n "$rest" ] || break
  done
  f="$(find "$d" \( "$@" \) 2> /dev/null | head -n 1)"
  if [ -n "$f" ]; then echo "no $u $f"; else echo "ok $u"; fi
}
hasfile() {
  local e
  for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    [ -L "$e" ] && continue
    [ -f "$e" ] && return 0
    [ -d "$e" ] && hasfile "$e" && return 0
  done
  return 1
}
for s in "$@"; do
  k="${s%%@*}"; ranks=""; [ "$k" = "$s" ] || ranks="${s#*@}"
  d="$root/$k"; files=""; bad=""; link=""; missing=""
  l="$(linked "$root" "$k")"
  if [ -n "$l" ]; then echo "ecv unreadable $s link ${l#"$root"/}"; continue; fi
  if [ -e "$d" ] && [ ! -d "$d" ]; then echo "ecv unreadable $s not-a-directory"; continue; fi
  if [ ! -e "$d" ]; then echo "ecv absent $s"; continue; fi
  walk "$d"
  old="$IFS"; IFS=+
  for r in $ranks; do
    IFS="$old"
    if [ -L "$d/$r" ] || [ ! -d "$d/$r" ]; then missing="$missing${missing:+,}$r"
    elif ! hasfile "$d/$r"; then missing="$missing${missing:+,}$r(empty)"; fi
  done
  IFS="$old"
  if [ -n "$link" ]; then echo "ecv unreadable $s link ${link#"$root"/}"
  elif [ -n "$bad" ]; then echo "ecv unreadable $s read ${bad#"$root"/}"
  elif [ -n "$missing" ]; then echo "ecv absent $s $missing"
  elif [ -z "$files" ]; then echo "ecv empty $s"
  else
    p="$(private "$root" "$k")"
    case "$p" in
      "? "*) echo "ecv unchecked $s ${p#"? "}" ;;
      "ok "*) echo "ecv present $s ${#files} ${p#ok }" ;;
      "no "*) p="${p#no }"; pu="${p%% *}"; pf="${p#* }"; echo "ecv unreadable $s mode $pu ${pf#"$root"/}" ;;
      *) echo "ecv unchecked $s no-answer" ;;
    esac
  fi
done
VERIFY
}

# VERIFY_WRAP is what kubectl exec runs: `sh -c WRAP sh SCRIPT ROOT SPEC...`.
# It runs the verify script under the preparer image's own timeout when the
# image has one that takes `timeout SECS CMD` (GNU coreutils, newer
# busybox), so a walk that hangs on a stuck mount ends inside the pod and
# not only on this side of the exec.
VERIFY_REMOTE_SECONDS=280
VERIFY_WRAP='s="$1"; shift; if timeout 5 true 2> /dev/null; then exec timeout '"${VERIFY_REMOTE_SECONDS}"' /bin/sh -c "$s" sh "$@"; fi; exec /bin/sh -c "$s" sh "$@"'

# cmd_verify reads every key on every usable node the DaemonSet covers,
# through the Ready preparer on that node, and exits non-zero unless every
# key is present and readable everywhere. A node with no Ready preparer, an
# exec that fails or is Forbidden, is "could not check" -- never "absent".
cmd_verify() {
    command -v jq >/dev/null 2>&1 || log_error "jq is required for verify (the node and pod lists are read with it)"
    [ -n "$NAMESPACE" ] || log_error "verify needs -n NAMESPACE"
    [ -n "$KEYS" ] || log_error "verify needs --keys KEY[,KEY...] (enginecache.sh keys prints the keys an engine pod used)"
    # ENGINE_CACHE_UID: the UID the engines run as, when it is not the owner
    # of each key directory (a seeded node: the preparer copied the key, as
    # root). Digits only; it reaches the remote script as euid=.
    ENGINE_UID="${ENGINE_CACHE_UID:-}"
    case "$ENGINE_UID" in
        "") ;;
        *[!0-9]*|???????????*) log_error "ENGINE_CACHE_UID must be a numeric UID, got '${ENGINE_UID}'" ;;
        *) ENGINE_UID="$((10#$ENGINE_UID))"
           [ "$ENGINE_UID" -le 4294967294 ] || log_error "ENGINE_CACHE_UID must be a UID below 4294967295, got '${ENGINE_CACHE_UID}'" ;;
    esac
    local keys=() raw=() k
    IFS=, read -r -a raw <<< "$KEYS"
    for k in "${raw[@]}"; do
        k="$(normalize_key "$k")" || exit 1
        keys+=("$k")
    done
    ec_nodes verdict
    local pods_file err_file
    pods_file="$(mktemp)"; err_file="$(mktemp)"
    if ! kubectl get pods -n "$NAMESPACE" -l "$LABEL_COMPONENT" -o json > "$pods_file" 2>"$err_file"; then
        local why; why="$(tr '\n' ' ' < "$err_file")"
        rm -f "$pods_file" "$err_file"
        log_error "cannot list the engine-cache preparer pods in ${NAMESPACE}: ${why}; nothing was checked"
    fi
    local timeout=()
    if command -v timeout >/dev/null 2>&1; then
        timeout=(timeout 300)
    else
        log_warning "no timeout command here (stock macOS has none; coreutils provides it): an exec that hangs is cut off only by the preparer's own limit (${VERIFY_REMOTE_SECONDS} s, when its image has timeout)"
    fi
    echo "${EC_PATH}/vllm  (keys: ${#keys[@]}; nodes: $(accelerator_selector_text "$EC_SELECTOR"))"
    if [ -n "$ENGINE_UID" ]; then
        echo "  modes read for uid ${ENGINE_UID} (ENGINE_CACHE_UID); its groups are not known, so group permissions are not counted"
    else
        echo "  modes read for the owner of each key directory, taken as the engine's UID (ENGINE_CACHE_UID names another)"
    fi
    printf '  %-28s %-20s %s\n' NODE RESULT KEY
    accelerator_print_excluded "$EC_EXCLUDED"
    # assumed: node/key checks judged for the key directory's owner (no
    # ENGINE_CACHE_UID); uids: the distinct owners they were judged for
    local present=0 absent=0 unreadable=0 unchecked=0 nodes=0 assumed=0 uids=""
    local node pod ready phase out xrc why line result detail vuid
    while IFS=$'\x1f' read -r node pod ready phase; do
        nodes=$((nodes + 1))
        if [ "$ready" != yes ]; then
            for k in "${keys[@]}"; do
                printf '  %-28s %-20s %s\n' "$node" "COULD NOT CHECK" "$k"
                unchecked=$((unchecked + 1))
            done
            echo "    no Ready preparer on ${node} (${phase}); enginecache.sh status says why"
            continue
        fi
        xrc=0
        out="$(${timeout[@]+"${timeout[@]}"} kubectl exec -n "$NAMESPACE" "$pod" -c prepare -- /bin/sh -c "$VERIFY_WRAP" sh "$(verify_script)" /engine-cache/vllm "${keys[@]}" 2>"$err_file" < /dev/null)" || xrc=$?
        why=""
        if grep -q Forbidden "$err_file"; then
            why="exec Forbidden: reading the cache needs pods/exec on the preparer pods in ${NAMESPACE}"
        elif [ "$xrc" -eq 124 ]; then
            # 124: GNU timeout, here or in the preparer
            why="the read on ${pod} timed out (${VERIFY_REMOTE_SECONDS} s in the preparer, 300 s here): a stuck mount, or a cache too large to read in that time"
        elif [ "$xrc" -eq 143 ] || [ "$xrc" -eq 137 ]; then
            # busybox timeout exits with the signal status; so does a killed or rolled preparer
            why="the read on ${pod} timed out or was killed (exit ${xrc}): a stuck mount, a cache too large to read in ${VERIFY_REMOTE_SECONDS} s, or the preparer was restarted (engine-cache-publish rolls them) -- run verify again once they are Ready"
        elif [ "$xrc" -ne 0 ]; then
            why="exec into ${pod} failed (exit ${xrc}): $(head -1 "$err_file")"
        fi
        for k in "${keys[@]}"; do
            line="$(printf '%s\n' "$out" | awk -v k="$k" '$1 == "ecv" && $3 == k { s = $2; for (i = 4; i <= NF; i++) s = s " " $i; print s; exit }')"
            result="${line%% *}"; detail=""
            case "$line" in *" "*) detail="${line#* }" ;; esac
            case "$result" in
                present)
                    # detail: FILES UID -- the UID the modes were read for
                    vuid="${detail#* }"; detail="${detail%% *}"
                    if [ -n "$ENGINE_UID" ]; then detail="${detail} files; for uid ${vuid}, ENGINE_CACHE_UID"
                    else detail="${detail} files; for uid ${vuid}, the key directory's owner"
                        assumed=$((assumed + 1)); uids="${uids} ${vuid}"; fi
                    printf '  %-28s %-20s %s\n' "$node" "present" "$k  (${detail})"; present=$((present + 1)) ;;
                absent) printf '  %-28s %-20s %s\n' "$node" "ABSENT" "$k${detail:+  (rank directory missing or empty: ${detail})}"; absent=$((absent + 1)) ;;
                empty) printf '  %-28s %-20s %s\n' "$node" "ABSENT" "$k  (an empty directory)"; absent=$((absent + 1)) ;;
                unreadable)
                    case "$detail" in
                        "read "*) detail="${detail#read }: cannot be read" ;;
                        "mode "*) detail="${detail#mode }"
                            if [ -n "$ENGINE_UID" ]; then detail="${detail#* }: not readable by uid ${detail%% *} (ENGINE_CACHE_UID)"
                            else assumed=$((assumed + 1)); uids="${uids} ${detail%% *}"
                                detail="${detail#* }: not readable by uid ${detail%% *}, the owner of the key directory (taken as the engine's UID; ENGINE_CACHE_UID names another)"; fi ;;
                        "link "*) detail="${detail#link }: a symlink under the cache; vLLM writes none" ;;
                        not-a-directory) detail="not a directory" ;;
                    esac
                    printf '  %-28s %-20s %s\n' "$node" "UNREADABLE" "$k  (${detail})"; unreadable=$((unreadable + 1)) ;;
                unchecked)
                    case "$detail" in
                        no-find-modes*) detail="${detail#no-find-modes}"
                            detail="the preparer image's find cannot run the mode check${detail:+ (it takes no${detail})}" ;;
                        no-owner) detail="the owner of the key directory could not be read (the preparer image needs ls -n and awk)" ;;
                        no-find) detail="the preparer image has no find to read file modes with" ;;
                        *) detail="no mode check answer from the preparer" ;;
                    esac
                    printf '  %-28s %-20s %s\n' "$node" "COULD NOT CHECK" "$k  (${detail})"; unchecked=$((unchecked + 1)) ;;
                *) printf '  %-28s %-20s %s\n' "$node" "COULD NOT CHECK" "$k"; unchecked=$((unchecked + 1))
                   [ -n "$why" ] || why="no answer from ${pod} for this key (exit ${xrc})" ;;
            esac
        done
        [ -z "$why" ] || echo "    ${why}"
    done < <(jq -r --arg ds "$CLAIM" --slurpfile pods "$pods_file" \
        '.items[] as $node
         | [$pods[0].items[] | select(.metadata.labels["wva.llmd.ai/engine-cache"] == $ds
              and .spec.nodeName == $node.metadata.name and .metadata.deletionTimestamp == null)]
         | map(. + {ok: (.status.phase == "Running" and ((.status.containerStatuses // []) | length > 0 and (map(.ready) | all)))})
         | ((map(select(.ok)) | first) // first) as $pod
         | [ $node.metadata.name,
             ($pod.metadata.name // ""),
             (if $pod != null and $pod.ok then "yes" else "no" end),
             (if $pod == null then "no pod" else ($pod.status.phase // "unknown") + " (not ready)" end)
           ] | join("\u001f")' <<< "$EC_NODES_JSON")
    rm -f "$pods_file" "$err_file"
    local total=$((nodes * ${#keys[@]}))
    echo "  ${nodes} usable node(s) x ${#keys[@]} key(s): ${present} present, ${absent} absent, ${unreadable} unreadable, ${unchecked} could not check"
    # Without ENGINE_CACHE_UID each key was read for its owner, which is the
    # engines' UID only when they wrote it: a key owned by root was copied by
    # the preparer (a seed merge or a publish) or written by a root engine, and
    # one owned by another UID may be another deployment's, or the image's
    # user may have changed. Name the UIDs assumed, every time.
    local ulist="" nuid=0 uwho=""
    if [ -z "$ENGINE_UID" ] && [ "$assumed" -gt 0 ]; then
        # grep finding nothing must not abort under pipefail: an empty list
        # is reported as such, not as a dead script
        ulist="$(tr ' ' '\n' <<< "$uids" | { grep . || true; } | sort -n -u | paste -sd, -)"
        nuid="$(tr ',' '\n' <<< "$ulist" | { grep -c . || true; })"
        ulist="${ulist//,/, }"
        if [ "$nuid" -gt 1 ]; then uwho="uids ${ulist}"; else uwho="uid ${ulist}"; fi
        echo "  WARNING: ${assumed} of ${total} node/key checks were judged for the owner of the key directory (${uwho}), taken as the engines' UID."
        case ", ${ulist}, " in *", 0, "*)
            echo "  A key owned by root (uid 0) is a seeded or published copy, or a root engine's: it passes for root whatever user the engines run as." ;;
        esac
        if [ "$nuid" -gt 1 ]; then
            echo "  The keys have ${nuid} different owners. The engines of one configuration all run as one UID, so at least one line was judged for the wrong user (unless the keys belong to engines that run as different users: check each with its own ENGINE_CACHE_UID)."
        fi
        echo "  To check for the user your engines run as, run again with ENGINE_CACHE_UID=\$(kubectl exec -n ${NAMESPACE} <engine pod> -- id -u)"
    fi
    if [ "$present" -ne "$total" ] || [ "$total" -eq 0 ]; then
        echo "  NOT VERIFIED: a start on a node above that lacks a key compiles, or a node could not be checked"
        return 1
    fi
    if [ -z "$uwho" ]; then
        echo "  VERIFIED: every key is on every usable node"
    elif [ "$uwho" = "uid 0" ]; then
        echo "  VERIFIED for root: every key is on every usable node, readable by root -- not checked for any other user (see the warning above)"
    elif [ "$nuid" -eq 1 ]; then
        echo "  VERIFIED for ${uwho}: every key is on every usable node, readable by ${uwho} -- not checked for any other user (see the warning above)"
    else
        echo "  VERIFIED for ${uwho}: every key is on every usable node, each readable by the owner of its key directory -- not checked for any one user (see the warning above)"
    fi
}

CMD="${1:-}"
[ -n "$CMD" ] || usage
shift
case "$CMD" in
    -h|--help|help) usage ;;
    apply|status|delete|publish|keys|verify) ;;
    *) log_error "unknown command: ${CMD} (apply | status | delete | publish | keys | verify)" ;;
esac

while [ $# -gt 0 ]; do
    case "$1" in
        -n|--namespace|--path|--image|--capacity|--seed-claim|--node-selector|--toleration|--keys|--container)
            [ $# -ge 2 ] || log_error "$1 needs a value" ;;
    esac
    case "$1" in
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        --path) NODE_PATH="$2"; shift 2 ;;
        --image) IMAGE="$2"; shift 2 ;;
        --capacity) CAPACITY="$2"; CAPACITY_GIVEN=true; shift 2 ;;
        --seed-claim) check_seed "$2"; shift 2 ;;
        --node-selector) NODE_SELECTOR="$2"; NODE_SELECTOR_GIVEN=true; shift 2 ;;
        --toleration) accelerator_check_toleration "$2"; TOLERATIONS+=("$2"); shift 2 ;;
        --dry-run) case "$CMD" in status|keys|verify) log_error "--dry-run applies to apply and delete, not ${CMD}" ;; esac; DRY_RUN=true; shift ;;
        --keys) [ "$CMD" = verify ] || log_error "--keys applies to verify"; KEYS="$2"; shift 2 ;;
        --container) [ "$CMD" = keys ] || log_error "--container applies to keys"
            case "$2" in ""|*[!a-z0-9-]*) log_error "not a container name: '$2'" ;; esac
            CONTAINER="$2"; shift 2 ;;
        -h|--help) usage ;;
        -*) log_error "unknown option: $1" ;;
        *) [ "$CMD" = keys ] || log_error "unknown argument: $1"
           case "$1" in *[!a-z0-9.-]*) log_error "not a pod name: '$1'" ;; esac
           PODS+=("$1"); shift ;;
    esac
done
accelerator_check_selector "$NODE_SELECTOR"
case "$CAPACITY" in
    ""|*[!0-9A-Za-z.]*) log_error "--capacity must be a Kubernetes quantity such as 200Gi, got '${CAPACITY}'" ;;
esac

"cmd_${CMD}"
