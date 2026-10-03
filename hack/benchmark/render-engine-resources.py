#!/usr/bin/env python3
"""Size the engine's resource REQUESTS to the model a run actually uses.

The scenarios declare Qwen3-32B and size the engines for it. A run that
overrides the model to something far smaller inherits those figures anyway,
because the resource blocks do not follow `MODEL_ID`. The result is a
reservation with no relation to the workload: measured on Qwen3-0.6B, the
engine uses 1.08 CPU and 4.7Gi steady while reserving 8 CPU and 16Gi (prefill)
or 32 CPU and 128Gi (decode). Nineteen pods then reserved 152 CPU to use about
twenty and the fleet could not schedule itself -- runs aborted with seven and
twenty-one pods Pending, and in one of them decode took the headroom first and
prefill spent an entire phase on a single replica with 282 requests queued.

This rewrites the engine resources in the COPY that benchmark-scenarios
installs into the clone, the same way that step already substitutes
__WARM_REPLICAS__ and gpuMemoryUtilization. The source scenarios keep the
figures for the model they declare.

Addressed by exact YAML PATH, not by pattern. An earlier regex version matched
the EPP's own resources block instead of the engine's and quietly changed the
endpoint picker's CPU request -- the paths below cannot do that, and the
assertions below would fail if the structure moved.

ONLY MEASURED MODELS ARE OVERRIDDEN. A model not in the table is left exactly
as the scenario wrote it: the point is to stop inheriting a figure that was
never measured for the model in use, not to replace it with a different guess.

It edits with `yq -i`, which REFORMATS the file it touches -- blank lines are
dropped and inline comments re-indented. That is noise in a generated artifact
and nothing else: this only ever runs against the copy inside the gitignored
clone, never against the scenarios under hack/.

REQUESTS MOVE, LIMITS DO NOT. The request is what the scheduler reserves; the
limit is the burst ceiling. cgroup cpu.stat shows these engines do occasionally
reach the ceiling (nr_throttled 12-13 over hours), so lowering the limit would
trade a scheduling problem for a throughput one.
"""
import argparse
import subprocess
import sys

# model id -> (cpu request, memory request). Limits are deliberately untouched.
#
# Qwen3-0.6B: measured 1.08 CPU and 4.3-4.7Gi steady on a P/D fleet under the
# 1k/6000 -> 20k/250 trace, on both roles. 2 CPU / 8Gi is roughly 2x that.
MEASURED = {
    "Qwen/Qwen3-0.6B": ("2", "8Gi"),
}

# The engine roles, by their path under the scenario. The EPP
# (modelservice.router.epp) and the Envoy sidecar (…router.proxy) have their
# own resources and are NOT engines, so they are absent here by construction.
ROLES = ("prefill", "decode", "standalone")


def yq(args):
    return subprocess.run(["yq"] + args, capture_output=True, text=True, check=True).stdout.strip()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("path")
    ap.add_argument("--model", required=True)
    args = ap.parse_args()

    if args.model not in MEASURED:
        print("  engine resources: %s not measured, keeping the scenario's own figures"
              % args.model)
        return 0
    cpu, mem = MEASURED[args.model]

    changed = []
    for role in ROLES:
        base = ".scenario[0].modelservice.%s.resources" % role
        if yq(["e", base, args.path]) in ("null", ""):
            continue
        before = yq(["e", "%s.requests" % base, args.path])
        subprocess.run(["yq", "-i",
                        '%s.requests.cpu = "%s" | %s.requests.memory = "%s"'
                        % (base, cpu, base, mem), args.path], check=True)
        after = yq(["e", "%s.requests" % base, args.path])
        # The limit must not have moved.
        lim = yq(["e", "%s.limits" % base, args.path])
        assert cpu in after and mem in after, "request not applied for %s: %s" % (role, after)
        assert lim == yq(["e", "%s.limits" % base, args.path]), "limits changed for %s" % role
        if after != before:
            changed.append(role)

    if not changed:
        print("  engine resources: no engine block found in %s" % args.path)
        return 0
    print("  engine resources: %s -> requests %s CPU / %s (%s); limits unchanged"
          % (args.model, cpu, mem, ", ".join(changed)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
