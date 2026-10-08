#!/usr/bin/env python3
"""What the utilization-share optimizer did in each arm of the two-model run.

The TTFT and accelerator-second tables come from two_model_report.py, which
compares shadow and share against the today baseline exactly as it compares the
pool arms against nopool. This adds what only the controller knows: which mode
it ran in, the transfers it made, the model-seconds a model was held back and
why, and the seconds a replica move was planned.

It refuses, rather than reports, three runs that would mislead:

- arms whose limiters differ: the comparison is of two optimizers under ONE
  quota, and a different quota is a different experiment;
- an arm whose controller did not hold the arm's mode for the whole window: a
  policy that was never read measures the previous arm under a new name;
- a today or shadow arm in which a transfer happened: neither may move a GPU,
  so one that did was not the arm it is labelled.

`--rewrite-policy ARM` is the other half: it rewrites a policy's `optimizer:`
block for an arm (stdin to stdout), and is what the driver runs before each arm.
"""

import argparse
import json
import os
import sys

ARMS = ("today", "shadow", "share")
MODE = {"today": "off", "shadow": "shadow", "share": "active"}
OUTCOMES = ("done", "aborted", "fill-timeout", "wrong-pod")


def strip_optimizer(text):
    """The policy with its top-level optimizer: block removed, nothing else."""
    out, skip = [], False
    for line in text.splitlines():
        if line[:1] not in ("", " ", "\t", "#"):
            skip = line.split(":", 1)[0].strip() == "optimizer"
        if not skip:
            out.append(line)
    while out and not out[-1].strip():
        out.pop()
    return out


def rewrite_policy(text, arm):
    """The policy as arm ARM runs it: today has no optimizer block, shadow and
    share the utilization-share optimizer with shadow on and off."""
    if arm not in ARMS:
        raise ValueError("unknown arm %r (want one of %s)" % (arm, ", ".join(ARMS)))
    out = strip_optimizer(text)
    if arm != "today":
        out += ["optimizer:", "  type: utilizationShare", "  utilizationShare:",
                "    shadow: " + ("true" if arm == "shadow" else "false")]
    return "\n".join(out) + "\n"


def series(snap, prefix, label):
    """{label value: number} for the query in snap that starts with prefix."""
    for q, res in (snap or {}).items():
        if q.startswith(prefix):
            if isinstance(res, dict):
                return None
            return {r["metric"].get(label, ""): float(r["value"][1]) for r in res}
    return None


def scalar(snap, prefix):
    for q, res in (snap or {}).items():
        if q.startswith(prefix):
            if isinstance(res, dict):
                return None
            return float(res[0]["value"][1]) if res else 0.0
    return None


def load_arm(root, arm):
    d = os.path.join(root, arm)
    if not os.path.isfile(os.path.join(d, "meta.json")):
        return None
    snap = policy = None
    try:
        with open(os.path.join(d, "share.json")) as fh:
            snap = json.load(fh)
    except (OSError, ValueError):
        pass
    try:
        with open(os.path.join(d, "policy.yaml")) as fh:
            policy = fh.read()
    except OSError:
        pass
    return {
        "name": arm,
        "policy": policy,
        "snap": snap,
        "transfers": series(snap, "sum by (outcome)", "outcome"),
        "blocked": series(snap, "sum by (reason)", "reason"),
        "planned": scalar(snap, "count_over_time((max("),
        "modes": series(snap, "min by (mode)", "mode"),
    }


def problems(arms):
    """Why these arms cannot be compared, or [] when they can."""
    out = []
    base = arms[0]
    for a in arms:
        if a["policy"] is None:
            out.append("the %s arm did not keep the policy it ran under (policy.yaml), so "
                       "nothing establishes that its quota was the same" % a["name"])
        elif base["policy"] is not None and strip_optimizer(a["policy"]) != strip_optimizer(base["policy"]):
            out.append("the %s and %s arms ran under different policies apart from the "
                       "optimizer block: a different quota is a different experiment"
                       % (base["name"], a["name"]))
        if a["snap"] is None:
            out.append("the %s arm has no controller metrics (share.json), so neither its "
                       "mode nor its transfers are known" % a["name"])
            continue
        modes = a["modes"]
        want = MODE[a["name"]]
        if modes is None:
            out.append("the %s arm's mode query failed" % a["name"])
        elif modes.get(want) != 1.0:
            out.append("the controller did not hold mode '%s' for the whole %s arm (min over "
                       "the window: %s): it measured something else for part of it"
                       % (want, a["name"], modes.get(want, "absent")))
        moved = sum((a["transfers"] or {}).values())
        if a["name"] != "share" and moved >= 0.5:
            out.append("the %s arm recorded %.0f transfer(s); it may not move a GPU, so it "
                       "was not the arm it is labelled" % (a["name"], moved))
    return out


def fmt(v, unit=""):
    return "-" if v is None else "%.0f%s" % (v, unit)


def report(arms):
    print("## What the optimizer did")
    print("")
    print("| arm | mode | transfers done | aborted | fill-timeout | wrong-pod | "
          "s a move was planned | model-s held back |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for a in arms:
        t = a["transfers"] or {}
        held = sum((a["blocked"] or {}).values()) if a["blocked"] is not None else None
        print("| %s | %s | %s | %s | %s | %s | %s | %s |" % (
            a["name"], MODE[a["name"]],
            *(fmt(t.get(o, 0.0)) for o in OUTCOMES),
            fmt(a["planned"]), fmt(held)))
    print("")
    print("Model-seconds held back, by reason (a model blocked for 60 s is 60):")
    print("")
    reasons = sorted({r for a in arms for r in (a["blocked"] or {})})
    if not reasons:
        print("- no model was held back in any arm.")
    else:
        print("| reason | " + " | ".join(a["name"] for a in arms) + " |")
        print("| --- |" + " --- |" * len(arms))
        for r in reasons:
            print("| %s | %s |" % (r, " | ".join(fmt((a["blocked"] or {}).get(r, 0.0)) for a in arms)))
    print("")
    print("The shadow arm moves nothing, so its TTFT should match today's; the seconds "
          "a move was planned are what the optimizer would have acted on. A shadow arm "
          "that differs from today by as much as share does says the run's spread is "
          "larger than the effect.")
    print("")


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--out-root", help="the directory holding today/, shadow/ and share/")
    p.add_argument("--rewrite-policy", metavar="ARM",
                   help="rewrite the policy on stdin for ARM and print it")
    args = p.parse_args(argv)
    if args.rewrite_policy:
        try:
            sys.stdout.write(rewrite_policy(sys.stdin.read(), args.rewrite_policy))
        except ValueError as e:
            print(e, file=sys.stderr)
            return 2
        return 0
    if not args.out_root:
        p.error("--out-root is required")
    arms = [a for a in (load_arm(args.out_root, n) for n in ARMS) if a]
    if not arms or arms[0]["name"] != "today":
        print("no today arm in %s: it is the baseline" % args.out_root, file=sys.stderr)
        return 1
    bad = problems(arms)
    if bad:
        print("## The arms are not comparable")
        print("")
        for b in bad:
            print("- " + b)
        return 1
    report(arms)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
