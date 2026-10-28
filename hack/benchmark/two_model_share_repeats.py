#!/usr/bin/env python3
"""The utilization-share arms of every repeat, side by side.

Reads OUT_ROOT/rep<N>/<arm>/ for every repeat the share-arms verb ran (or
OUT_ROOT/<arm>/ as one repeat, for a run made without REPEAT), and prints:

- TTFT per burst, by model and arm: p50 and p95 over the rise window (the
  first RISE_WINDOW seconds of the burst, its own stage) and over the rest of
  the burst, with failed requests;
- the spread: each arm's p95 across repeats, and the today-against-shadow
  difference inside each repeat. Shadow moves nothing, so that difference is
  the run's own noise;
- refused scale-ups: model-seconds held back by reason, and decisions the
  limiters cut, from what the controller published (share.json);
- transfers by outcome, counted from each arm's controller log
  (controller.log). The log names every transfer, donor set and idle fill; the
  counter's increase() missed a series' first sample in the run the well-lit
  path reports, so the metric is printed beside the log count, not instead;
- GPU-seconds and the peak, from the run's own sampler;
- what this shows, in plain sentences the docs can quote.

It reads only files; nothing here needs the cluster. Arms that are missing are
named, not guessed at.
"""

import argparse
import glob
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from two_model_report import (  # noqa: E402  (beside this file, not installed)
    burst_order, clip_series, gpu_series, gpu_seconds_by_model, integrate, load_meta,
    rise_stages, roles_of, window_of)
from two_model_share_report import load_placement, placement_table  # noqa: E402

ARMS = ("today", "shadow", "share")
OUTCOMES = ("done", "aborted", "fill-timeout", "wrong-pod", "cancelled", "redirected")
BAD_OUTCOMES = ("aborted", "fill-timeout", "wrong-pod")


# ---------------------------------------------------------------------------
# Reading
# ---------------------------------------------------------------------------
def repeat_dirs(root):
    """[(label, dir)] of the repeats under root, in order."""
    reps = []
    for d in glob.glob(os.path.join(root, "rep*")):
        m = re.match(r"^rep(\d+)$", os.path.basename(d))
        if m and os.path.isdir(d):
            reps.append((int(m.group(1)), d))
    if reps:
        return [("rep%d" % n, d) for n, d in sorted(reps)]
    if any(os.path.isfile(os.path.join(root, a, "meta.json")) for a in ARMS):
        return [("rep1", root)]
    return []


def load_json(path):
    try:
        with open(path) as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return None


def series(snap, prefix, *labels):
    """{(label values...): number} for the query that starts with prefix, or
    None when the query is absent or failed."""
    for q, res in (snap or {}).items():
        if q.startswith(prefix):
            if isinstance(res, dict):
                return None
            out = {}
            for r in res:
                key = tuple(r["metric"].get(lb, "") for lb in labels)
                out[key] = out.get(key, 0.0) + float(r["value"][1])
            return out
    return None


_KV = re.compile(r'(\w+)=("(?:[^"\\]|\\.)*"|\S+)')


def log_fields(line):
    """The structured fields of one controller log line, or {}.

    The controller logs JSON (production) or a console line ending in a JSON
    object (development); a plain key=value line is read too.
    """
    i = line.find("{")
    if i >= 0:
        try:
            obj = json.loads(line[i:])
            if isinstance(obj, dict):
                return obj
        except ValueError:
            pass
    out = {}
    for k, v in _KV.findall(line):
        out[k] = v[1:-1] if v.startswith('"') else v
    return out


def transfers_from_log(path):
    """What the optimizer logged over one arm, or None when there is no log."""
    if not os.path.isfile(path):
        return None
    ended = {o: 0 for o in OUTCOMES}
    started = fills = node_planned = 0
    sets = set()
    # {model: {"gave": transfers it donated to, "got": transfers it received}},
    # from the started lines' donor and receiver: with three or four models
    # the fleet total says nothing about WHICH model was funded from which.
    by_model = {}
    with open(path, errors="replace") as fh:
        for line in fh:
            if "Utilization share:" not in line:
                continue
            f = log_fields(line)
            if "transfer ended" in line:
                o = str(f.get("outcome", "unknown"))
                ended[o] = ended.get(o, 0) + 1
            elif "transfer started" in line:
                started += 1
                if f.get("set"):
                    sets.add(str(f.get("set")))
                if str(f.get("planned", "")).lower() == "true":
                    node_planned += 1
                for field, side in (("donor", "gave"), ("receiver", "got")):
                    m = f.get(field)
                    if m:
                        e = by_model.setdefault(str(m), {"gave": 0, "got": 0})
                        e[side] += 1
            elif "filling from idle GPUs" in line:
                fills += 1
    return {"started": started, "ended": ended, "donor_sets": len(sets),
            "node_planned": node_planned, "idle_fills": fills, "by_model": by_model}


def bursts(meta, model_key):
    """[(rise stage index, rest stage index or None)] for one model."""
    sched = (meta or {}).get("schedule") or []
    key = "rate_" + model_key
    out = []
    for i in rise_stages(sched, model_key):
        rest = i + 1 if i + 1 < len(sched) and sched[i + 1][key] == sched[i][key] else None
        out.append((i, rest))
    return out


def window_stats(arm, model_key, idx):
    if idx is None:
        return None
    w = window_of(arm, model_key, idx)
    if w is None:
        return None
    return {k: w.get(k) for k in ("n", "failed", "p50", "p95")}


def load_arm(d, name):
    meta = load_meta(d)
    if meta is None:
        return None
    sched = meta.get("schedule") or []
    t_end = sched[-1]["end"] if sched else None
    gs = clip_series(gpu_series(os.path.join(d, "gpus.jsonl"), meta.get("t0", 0)), t_end)
    gsec, _ = integrate(gs, 1)
    arm = {"name": name, "dir": d, "meta": meta}
    snap = load_json(os.path.join(d, "share.json"))
    roles = roles_of(meta)
    prefixes = (load_json(os.path.join(d, "budget.json")) or {}).get("model_deploys")
    out = {
        "roles": list(roles),
        "gpu_seconds": gsec,
        # Per model, by the Deployments the arm recorded; None for an arm that
        # recorded none (every arm before this split existed).
        "gpu_seconds_by_model": (gpu_seconds_by_model(os.path.join(d, "gpus.jsonl"),
                                                      meta.get("t0", 0), t_end, prefixes, roles)
                                 if prefixes else None),
        "peak_gpus": max((s[1] for s in gs), default=None),
        "bursts": {},
        "blocked": series(snap, "sum by (model_name, reason)", "model_name", "reason"),
        "blocked_by_reason": series(snap, "sum by (reason)", "reason"),
        "limited": series(snap, "sum by (variant_name, limiter_name)", "variant_name", "limiter_name"),
        "metric_transfers": series(snap, "sum by (outcome)", "outcome"),
        "log": transfers_from_log(os.path.join(d, "controller.log")),
        "has_share_json": snap is not None,
        "placement": load_placement(d),
    }
    for key in roles:
        out["bursts"][key] = [
            {"rise_stage": r, "rise": window_stats(arm, key, r), "rest": window_stats(arm, key, rest)}
            for r, rest in bursts(meta, key)]
    return out


# ---------------------------------------------------------------------------
# Printing
# ---------------------------------------------------------------------------
def s(v):
    return "-" if v is None else "%.2f" % v


def n0(v):
    return "-" if v is None else "%.0f" % v


def p95_of(data, rep, arm, key, b, part="rise"):
    a = data.get(rep, {}).get(arm)
    if not a:
        return None
    bl = a["bursts"].get(key) or []
    if b >= len(bl) or not bl[b].get(part):
        return None
    return bl[b][part].get("p95")


def burst_name(key, b, models):
    return "model %s (%s) burst %d" % (key.upper(), models[key], b + 1)


def spread(values):
    vals = [v for v in values if v is not None]
    return (max(vals) - min(vals)) if len(vals) >= 2 else None


def run_roles(data, reps):
    """Every model any arm drove, in model order: a and b, then c and d."""
    seen = set()
    for r in reps:
        for a in data[r].values():
            seen.update(a.get("roles") or ())
    return tuple(k for k in ("a", "b", "c", "d") if k in seen) or ("a", "b")


def bursting_order(data, reps):
    """The models in the order they burst: b, a for two; b, c, d, a for four."""
    return burst_order(run_roles(data, reps))


def summarize(data, reps, models):
    """Plain sentences, each one checkable against the tables above it."""
    lines = []
    n_bursts = {k: max((len(a["bursts"].get(k) or []) for r in reps for a in data[r].values()), default=0)
                for k in run_roles(data, reps)}
    for key in bursting_order(data, reps):
        for b in range(n_bursts[key]):
            name = burst_name(key, b, models)
            today = [p95_of(data, r, "today", key, b) for r in reps]
            share = [p95_of(data, r, "share", key, b) for r in reps]
            shadow = [p95_of(data, r, "shadow", key, b) for r in reps]
            noise_candidates = [spread(today), spread(shadow)]
            for r in reps:
                noise_candidates.append(spread([p95_of(data, r, "today", key, b), p95_of(data, r, "shadow", key, b)]))
            noise = max([v for v in noise_candidates if v is not None], default=None)
            if any(v is None for v in today + share):
                lines.append("- %s: not every repeat has both today and share, so no claim is made." % name)
                continue
            gains = [t - sh for t, sh in zip(today, share)]
            txt = ("- %s, rise window p95: today %s s, share %s s (by repeat). "
                   % (name, " / ".join(s(v) for v in today), " / ".join(s(v) for v in share)))
            if noise is None:
                txt += "There is no second measurement to set a noise floor, so this is not yet a result."
            elif all(g > noise for g in gains):
                txt += ("Share was faster in every repeat, by at least %.2f s, more than the "
                        "run's own spread of %.2f s (today against shadow and repeat against repeat)."
                        % (min(gains), noise))
            elif all(-g > noise for g in gains):
                txt += ("Share was slower in every repeat, by at least %.2f s, more than the "
                        "run's spread of %.2f s." % (min(-g for g in gains), noise))
            else:
                txt += ("The difference is within the run's own spread of %.2f s in at least one "
                        "repeat: this run does not show a difference for this burst." % noise)
            lines.append(txt)
    # Transfers.
    sets = done = 0
    bad = {o: 0 for o in BAD_OUTCOMES}
    have_log = False
    for r in reps:
        lg = (data[r].get("share") or {}).get("log")
        if lg is None:
            continue
        have_log = True
        sets += lg["donor_sets"]
        done += lg["ended"].get("done", 0)
        for o in BAD_OUTCOMES:
            bad[o] += lg["ended"].get(o, 0)
    if not have_log:
        lines.append("- No share arm kept its controller log, so transfers are not counted from the log.")
    else:
        lines.append("- Share arms, all repeats: %d transfer(s) ended done; %d donor set(s) started "
                     "(several small donors funding one larger replica). %s"
                     % (done, sets, "Small-to-large lending was attempted." if sets
                        else "Small-to-large lending never started."))
        if any(bad.values()):
            lines.append("- Transfers that did not land: %s." % ", ".join("%d %s" % (v, o) for o, v in bad.items() if v))
        else:
            lines.append("- No transfer ended aborted, fill-timeout or wrong-pod.")
    # Cost.
    costs = []
    for r in reps:
        t = (data[r].get("today") or {}).get("gpu_seconds")
        sh = (data[r].get("share") or {}).get("gpu_seconds")
        if t and sh:
            costs.append(100.0 * (sh - t) / t)
    if costs:
        lines.append("- GPU-seconds, share against today: %s by repeat."
                     % " / ".join("%+.0f%%" % c for c in costs))
    return lines


def report(data, reps, models, missing):
    print("# Utilization share, %d repeat(s)" % len(reps))
    print("")
    if missing:
        print("Missing arms (not run, or refused before results): %s." % ", ".join(missing))
        print("")
    print("## TTFT per burst (seconds)")
    print("")
    print("The bursting model only. Rise is the burst's opening window (its own stage); "
          "rest is the remainder of the burst.")
    print("")
    print("| burst | repeat | arm | rise n | rise failed | rise p50 | rise p95 | rest p50 | rest p95 |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for key in bursting_order(data, reps):
        nb = max((len(a["bursts"].get(key) or []) for r in reps for a in data[r].values()), default=0)
        for b in range(nb):
            for r in reps:
                for arm in ARMS:
                    a = data[r].get(arm)
                    if not a or b >= len(a["bursts"].get(key) or []):
                        continue
                    w = a["bursts"][key][b]
                    rise, rest = w.get("rise") or {}, w.get("rest") or {}
                    print("| %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (
                        burst_name(key, b, models), r, arm, n0(rise.get("n")), n0(rise.get("failed")),
                        s(rise.get("p50")), s(rise.get("p95")), s(rest.get("p50")), s(rest.get("p95"))))
    print("")
    print("## Spread (rise-window p95, seconds)")
    print("")
    print("| burst | arm | p95 by repeat | spread across repeats |")
    print("| --- | --- | --- | --- |")
    for key in bursting_order(data, reps):
        nb = max((len(a["bursts"].get(key) or []) for r in reps for a in data[r].values()), default=0)
        for b in range(nb):
            for arm in ARMS:
                vals = [p95_of(data, r, arm, key, b) for r in reps]
                if all(v is None for v in vals):
                    continue
                print("| %s | %s | %s | %s |" % (burst_name(key, b, models), arm,
                                                 " / ".join(s(v) for v in vals), s(spread(vals))))
            for r in reps:
                d = spread([p95_of(data, r, "today", key, b), p95_of(data, r, "shadow", key, b)])
                if d is not None:
                    print("| %s | today vs shadow, %s | | %s |" % (burst_name(key, b, models), r, s(d)))
    print("")
    print("## Refused scale-ups")
    print("")
    print("Model-seconds a model was held back, by reason (wva_model_scaling_blocked), and "
          "scaling decisions a limiter cut (wva_decisions_limited_total).")
    print("")
    print("| repeat | arm | held back (model: reason = s) | limited decisions (variant: limiter = n) |")
    print("| --- | --- | --- | --- |")
    for r in reps:
        for arm in ARMS:
            a = data[r].get(arm)
            if not a:
                continue
            if not a["has_share_json"]:
                print("| %s | %s | no share.json | no share.json |" % (r, arm))
                continue
            bl = a["blocked"]
            lim = a["limited"]
            bl_txt = "query failed" if bl is None else (
                ", ".join("%s: %s = %.0f" % (m or "?", why or "?", v) for (m, why), v in sorted(bl.items()) if v)
                or "none")
            lim_txt = "query failed" if lim is None else (
                ", ".join("%s: %s = %.0f" % (vn or "?", ln or "?", v) for (vn, ln), v in sorted(lim.items()) if v >= 0.5)
                or "none")
            print("| %s | %s | %s | %s |" % (r, arm, bl_txt, lim_txt))
    print("")
    # The same series, totalled per model across reasons: with three or four
    # models sharing a quota, which one was held back is the question.
    held = {}
    for r in reps:
        for arm in ARMS:
            a = data[r].get(arm)
            if not a or not a["has_share_json"] or a["blocked"] is None:
                continue
            for (m, _why), v in a["blocked"].items():
                held.setdefault(m or "?", {}).setdefault((r, arm), 0.0)
                held[m or "?"][(r, arm)] += v
    cols = [(r, arm) for r in reps for arm in ARMS if data[r].get(arm)]
    print("Model-seconds held back, by model (every reason):")
    print("")
    if not held:
        print("- no model was held back in any arm, or no arm kept share.json.")
    else:
        print("| model | " + " | ".join("%s %s" % c for c in cols) + " |")
        print("| --- |" + " --- |" * len(cols))
        for m in sorted(held):
            print("| %s | %s |" % (m, " | ".join(n0(held[m].get(c, 0.0)) for c in cols)))
    print("")
    print("## Transfers")
    print("")
    print("Counted from each arm's controller log. The metric column is increase() over the arm, "
          "for comparison.")
    print("")
    print("| repeat | arm | started | " + " | ".join(OUTCOMES) +
          " | donor sets | node-planned | idle fills | metric done |")
    print("| --- | --- | --- |" + " --- |" * len(OUTCOMES) + " --- | --- | --- | --- |")
    for r in reps:
        for arm in ARMS:
            a = data[r].get(arm)
            if not a:
                continue
            lg = a["log"]
            met = a["metric_transfers"]
            mdone = "-" if met is None else n0(met.get(("done",), 0.0))
            if lg is None:
                print("| %s | %s | no log |" % (r, arm) + " - |" * len(OUTCOMES) + " - | - | - | %s |" % mdone)
                continue
            print("| %s | %s | %d | %s | %d | %d | %d | %s |" % (
                r, arm, lg["started"], " | ".join(str(lg["ended"].get(o, 0)) for o in OUTCOMES),
                lg["donor_sets"], lg["node_planned"], lg["idle_fills"], mdone))
    print("")
    # Per model, from the started lines: which model gave and which received.
    givers = sorted({m for r in reps for a in data[r].values()
                     for m in ((a.get("log") or {}).get("by_model") or {})})
    print("Transfers started, by model (gave = donor, got = receiver):")
    print("")
    if not givers:
        print("- no transfer was started in any arm that kept its log.")
    else:
        print("| repeat | arm | " + " | ".join(givers) + " |")
        print("| --- | --- |" + " --- |" * len(givers))
        for r in reps:
            for arm in ARMS:
                a = data[r].get(arm)
                if not a or a["log"] is None:
                    continue
                bm = a["log"].get("by_model") or {}
                print("| %s | %s | %s |" % (r, arm, " | ".join(
                    "gave %d, got %d" % (bm.get(m, {}).get("gave", 0), bm.get(m, {}).get("got", 0))
                    for m in givers)))
    print("")
    print("## GPU-seconds")
    print("")
    print("| repeat | arm | GPU-seconds | peak GPUs | against today |")
    print("| --- | --- | --- | --- | --- |")
    for r in reps:
        base = (data[r].get("today") or {}).get("gpu_seconds")
        for arm in ARMS:
            a = data[r].get(arm)
            if not a:
                continue
            g = a["gpu_seconds"]
            rel = "-" if not (g and base) else "%+.0f%%" % (100.0 * (g - base) / base)
            print("| %s | %s | %s | %s | %s |" % (r, arm, n0(g), n0(a["peak_gpus"]), rel))
    print("")
    # Per model, for the arms that recorded which Deployments are whose.
    roles = run_roles(data, reps)
    split = [(r, arm, data[r][arm]["gpu_seconds_by_model"]) for r in reps for arm in ARMS
             if data[r].get(arm) and data[r][arm].get("gpu_seconds_by_model")]
    if split:
        print("GPU-seconds by model (pool Pods counted apart; unattributed is any "
              "accelerator no recorded Deployment owns):")
        print("")
        print("| repeat | arm | " + " | ".join(models.get(k, k.upper()) for k in roles)
              + " | pool | unattributed | all |")
        print("| --- | --- |" + " --- |" * (len(roles) + 3))
        for r, arm, g in split:
            print("| %s | %s | %s | %s | %s | %s |" % (
                r, arm, " | ".join(n0(g.get(k)) for k in roles),
                n0(g.get("pool")), n0(g.get("unattributed")), n0(g.get("all"))))
        print("")
    print("## Placement")
    print("")
    print("Where each arm's pods landed. A replica of N GPUs needs N free GPUs on one node, "
          "so on several nodes the placement decides what share could fund.")
    print("")
    rows = [("%s %s" % (r, arm), data[r][arm].get("placement")) for r in reps for arm in ARMS if data[r].get(arm)]
    for line in placement_table(rows):
        print(line)
    print("")
    print("## What this shows")
    print("")
    for line in summarize(data, reps, models):
        print(line)
    print("")


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--out-root", required=True, help="the run's results directory (holds rep1/, rep2/, ...)")
    p.add_argument("--model-a", default="A")
    p.add_argument("--model-b", default="B")
    p.add_argument("--model-c", default="C", help="model C, in a run of three or four models")
    p.add_argument("--model-d", default="D", help="model D, in a run of four models")
    p.add_argument("--json", help="also write every number to this file")
    args = p.parse_args(argv)
    reps = repeat_dirs(args.out_root)
    if not reps:
        print("no share arms under %s (expected rep<N>/<arm>/meta.json)" % args.out_root, file=sys.stderr)
        return 1
    data, missing = {}, []
    for label, d in reps:
        data[label] = {}
        for arm in ARMS:
            a = load_arm(os.path.join(d, arm), arm)
            if a is None:
                missing.append("%s/%s" % (label, arm))
            else:
                data[label][arm] = a
    labels = [label for label, _ in reps]
    if not any(data[r] for r in labels):
        print("no arm under %s has results" % args.out_root, file=sys.stderr)
        return 1
    models = dict((k, getattr(args, "model_" + k)) for k in run_roles(data, labels))
    report(data, labels, models, missing)
    if args.json:
        def keyed(v):
            if isinstance(v, dict):
                return {("/".join(k) if isinstance(k, tuple) else k): keyed(x) for k, x in v.items()}
            if isinstance(v, list):
                return [keyed(x) for x in v]
            return v
        out = {r: {arm: {k: keyed(v) for k, v in a.items()} for arm, a in data[r].items()} for r in labels}
        with open(args.json, "w") as fh:
            json.dump({"repeats": out, "missing": missing,
                       "summary": summarize(data, labels, models)}, fh, indent=1, sort_keys=True)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
