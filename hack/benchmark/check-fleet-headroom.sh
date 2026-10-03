#!/usr/bin/env bash
# Refuse to start a benchmark the cluster cannot actually run.
#
# GPUs are the resource everyone checks and CPU is the one that binds. Each
# engine Pod on this fleet requests 2 CPU, so a 10/10 P/D fleet wants 40 CPU on
# top of whatever else is resident -- and a GPU is useless to a Pod the
# scheduler cannot place. The cost of not checking is on record: one run spent
# 40 minutes with 1 of 10 replicas Running because the headroom a GPU-only
# pre-flight never looked at had been taken by another tenant, and the result
# was unusable.
#
# The ask is computed from what the fleet is ALLOWED to grow to -- each
# ScaledObject's maxReplicaCount times that role's per-Pod requests -- not from
# what it currently holds. A fleet checked at its current size passes and then
# fails to scale, which is the failure this exists to prevent.
#
# Headroom is measured in REQUESTS, not usage, because that is what the
# scheduler packs on: a node with idle CPU but no unreserved CPU will not take
# the Pod. Requests of Running and Pending Pods both count; a Pending Pod has
# already claimed its slot.
#
# READ-ONLY. It inspects and reports; it never scales, deletes or patches
# anything. Exit 0 means the fleet fits, 1 means it does not, 2 means the check
# could not be made -- which is NOT a pass, because a check that cannot see is
# not permission to proceed.
#
# Usage:
#   hack/benchmark/check-fleet-headroom.sh <namespace> [--exclude-node NAME]...
#
# Nodes may be excluded when they are known-bad: a node that cannot create Pod
# sandboxes counts its CPU as free and will never run anything.
set -u

ns=${1:-}
if [ -z "$ns" ] || [ "$ns" = "--help" ] || [ "$ns" = "-h" ]; then
  sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
fi
shift

excluded=""
while [ $# -gt 0 ]; do
  case "$1" in
    --exclude-node)
      shift
      [ $# -gt 0 ] || { echo "check-fleet-headroom: --exclude-node needs a node name" >&2; exit 2; }
      excluded="$excluded $1"
      ;;
    *) echo "check-fleet-headroom: unknown argument '$1'" >&2; exit 2 ;;
  esac
  shift
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "check-fleet-headroom: $1 is required" >&2; exit 2; }; }
need kubectl
need jq

# ---------------------------------------------------------------- the ask
# Per role: maxReplicaCount from its ScaledObject, and the per-Pod CPU and GPU
# requests from its Deployment. A role with no ScaledObject is taken at its
# current replica count, because nothing will grow it.
asks=$(kubectl -n "$ns" get scaledobject -o json 2>/dev/null | jq -r '
  .items[]? | [(.spec.scaleTargetRef.name // ""), (.spec.maxReplicaCount // 1)] | @tsv')

if [ -z "$asks" ]; then
  echo "check-fleet-headroom: no ScaledObjects in $ns -- nothing to size for" >&2
  exit 2
fi

total_cpu_m=0
total_gpu=0
echo "fleet's maximum ask, per role:"
while IFS=$'\t' read -r target maxr; do
  [ -n "$target" ] || continue
  spec=$(kubectl -n "$ns" get deploy "$target" -o json 2>/dev/null)
  if [ -z "$spec" ]; then
    echo "  WARNING  $target: no such Deployment; skipped" >&2
    continue
  fi
  # The engine container, by GPU request rather than by name: the container is
  # called `vllm` on one chart and something else on the next, but the one
  # holding the accelerator is the engine on all of them.
  per=$(printf '%s' "$spec" | jq -r '
    [.spec.template.spec.containers[]
     | select((.resources.requests["nvidia.com/gpu"] // "0") != "0")
     | {cpu: (.resources.requests.cpu // "0"),
        gpu: ((.resources.requests["nvidia.com/gpu"] // "0") | tonumber)}] | first // empty')
  if [ -z "$per" ]; then
    echo "  WARNING  $target: no container requests a GPU; skipped" >&2
    continue
  fi
  cpu=$(printf '%s' "$per" | jq -r '.cpu')
  gpu=$(printf '%s' "$per" | jq -r '.gpu')
  case "$cpu" in
    *m) cpu_m=${cpu%m} ;;
    *) cpu_m=$(awk -v c="$cpu" 'BEGIN{printf "%d", c*1000}') ;;
  esac
  total_cpu_m=$(( total_cpu_m + cpu_m * maxr ))
  total_gpu=$(( total_gpu + gpu * maxr ))
  printf '  %-46s max=%-3s  %s CPU + %s GPU each\n' "$target" "$maxr" "$cpu" "$gpu"
done <<EOF
$asks
EOF

if [ "$total_gpu" -eq 0 ] && [ "$total_cpu_m" -eq 0 ]; then
  echo "check-fleet-headroom: could not size any role -- refusing to call that a pass" >&2
  exit 2
fi

# ------------------------------------------------------------- the headroom
nodes=$(kubectl get nodes -o json 2>/dev/null \
  | jq -r '.items[] | select(.status.allocatable."nvidia.com/gpu") | .metadata.name')
if [ -z "$nodes" ]; then
  echo "check-fleet-headroom: no GPU nodes visible -- cannot judge headroom" >&2
  exit 2
fi

pods=$(kubectl get pods -A -o json 2>/dev/null)
if [ -z "$pods" ]; then
  echo "check-fleet-headroom: could not list Pods -- cannot judge headroom" >&2
  exit 2
fi

free_cpu_m=0
free_gpu=0
echo
echo "usable nodes:"
for n in $nodes; do
  skip=0
  for x in $excluded; do [ "$n" = "$x" ] && skip=1; done
  if [ "$skip" -eq 1 ]; then
    printf '  %-12s EXCLUDED by request\n' "$n"
    continue
  fi
  alloc=$(kubectl get node "$n" -o json 2>/dev/null | jq -r '.status.allocatable')
  acpu=$(printf '%s' "$alloc" | jq -r '.cpu')
  agpu=$(printf '%s' "$alloc" | jq -r '."nvidia.com/gpu" // "0"')
  case "$acpu" in
    *m) acpu_m=${acpu%m} ;;
    *) acpu_m=$(awk -v c="$acpu" 'BEGIN{printf "%d", c*1000}') ;;
  esac
  used=$(printf '%s' "$pods" | jq --arg n "$n" '
    [.items[]
     | select(.spec.nodeName == $n)
     | select(.status.phase == "Running" or .status.phase == "Pending")
     | .spec.containers[].resources.requests
     | {c: (.cpu // "0"), g: (."nvidia.com/gpu" // "0")}]
    | {cpu: ([.[].c | if test("m$") then (.[:-1]|tonumber) else (tonumber*1000) end] | add // 0),
       gpu: ([.[].g | tonumber] | add // 0)}')
  ucpu_m=$(printf '%s' "$used" | jq -r '.cpu')
  ugpu=$(printf '%s' "$used" | jq -r '.gpu')
  ncpu=$(( acpu_m - ucpu_m ))
  ngpu=$(( agpu - ugpu ))
  [ "$ncpu" -lt 0 ] && ncpu=0
  [ "$ngpu" -lt 0 ] && ngpu=0
  free_cpu_m=$(( free_cpu_m + ncpu ))
  free_gpu=$(( free_gpu + ngpu ))
  printf '  %-12s free: %6s m CPU  %2s GPU   (of %s m, %s)\n' "$n" "$ncpu" "$ngpu" "$acpu_m" "$agpu"
done

# What the fleet already holds comes back when it scales, so it counts as
# available to itself.
own=$(printf '%s' "$pods" | jq --arg ns "$ns" '
  [.items[] | select(.metadata.namespace == $ns)
   | select(.status.phase == "Running")
   | .spec.containers[].resources.requests
   | {c: (.cpu // "0"), g: (."nvidia.com/gpu" // "0")}]
  | {cpu: ([.[].c | if test("m$") then (.[:-1]|tonumber) else (tonumber*1000) end] | add // 0),
     gpu: ([.[].g | tonumber] | add // 0)}')
own_cpu_m=$(printf '%s' "$own" | jq -r '.cpu')
own_gpu=$(printf '%s' "$own" | jq -r '.gpu')

avail_cpu_m=$(( free_cpu_m + own_cpu_m ))
avail_gpu=$(( free_gpu + own_gpu ))

echo
printf 'needs  %6s m CPU   %3s GPU   (the fleet at its ceilings)\n' "$total_cpu_m" "$total_gpu"
printf 'has    %6s m CPU   %3s GPU   (free on usable nodes, plus the %s m / %s this namespace already holds)\n' \
  "$avail_cpu_m" "$avail_gpu" "$own_cpu_m" "$own_gpu"

rc=0
if [ "$avail_cpu_m" -lt "$total_cpu_m" ]; then
  printf 'BLOCKED: short %s m CPU. The fleet would scale into Pending Pods.\n' \
    "$(( total_cpu_m - avail_cpu_m ))"
  rc=1
fi
if [ "$avail_gpu" -lt "$total_gpu" ]; then
  printf 'BLOCKED: short %s GPU.\n' "$(( total_gpu - avail_gpu ))"
  rc=1
fi
[ "$rc" -eq 0 ] && echo "OK: the fleet fits at its ceilings."
exit "$rc"
