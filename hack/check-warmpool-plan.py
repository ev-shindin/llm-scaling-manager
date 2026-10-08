#!/usr/bin/env python3
"""Check the pod spec deploy/lib/warmpool_plan.py reads a pool's shape from.

The pool's planner sizes a warm pool from the pod template WVA itself reads:
a Deployment's template, a LeaderWorkerSet's leaderTemplate. An LWS may omit
the leaderTemplate -- the LWS controller then builds the leader from the
workerTemplate -- and a planner reading only the leaderTemplate sized such a
pool from an empty spec: no GPUs, no accelerator, silently. The Go accessor
(internal/utils/scaletarget/lws.go) makes the same fallback; this keeps the
script honest with it.

An expected-empty case proves nothing alone, so every case below also runs
against a spec that must be found, and the run must reach the end.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "deploy" / "lib"))
import warmpool_plan  # noqa: E402

CASES = []


def check(name, want, got):
    CASES.append(name)
    if want != got:
        print(f"FAIL  {name}\n        want {want!r}\n        got  {got!r}", file=sys.stderr)
        sys.exit(1)
    print(f"ok    {name}")


worker = {"containers": [{"name": "worker"}]}
leader = {"containers": [{"name": "leader"}]}
template = {"containers": [{"name": "decode"}]}

check("a Deployment reads its pod template",
      template,
      warmpool_plan.pod_spec({"spec": {"template": {"spec": template}}}, "Deployment"))
check("an LWS reads its leaderTemplate",
      leader,
      warmpool_plan.pod_spec({"spec": {"leaderWorkerTemplate": {
          "leaderTemplate": {"spec": leader}, "workerTemplate": {"spec": worker}}}}, "LeaderWorkerSet"))
check("an LWS without a leaderTemplate reads its workerTemplate",
      worker,
      warmpool_plan.pod_spec({"spec": {"leaderWorkerTemplate": {"workerTemplate": {"spec": worker}}}},
                             "leaderworkerset"))
check("an object with neither reads nothing",
      {},
      warmpool_plan.pod_spec({"spec": {}}, "LeaderWorkerSet"))

EXPECT = 4
if len(CASES) != EXPECT:
    print(f"\n{len(CASES)} case(s) ran, {EXPECT} expected -- the run did not finish.", file=sys.stderr)
    sys.exit(1)
print("\nWarm pool plan checks passed.")
