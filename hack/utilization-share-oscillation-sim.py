#!/usr/bin/env python3
"""Oscillation simulator for the utilization-share optimizer proposal.

See docs/proposals/utilization-share-optimizer.md, section 6.7. This is design
evidence, not a test of any code in the tree: it models the planner of
sections 5 and 6 closely enough to show whether a rule set oscillates, and
what each anti-oscillation rule contributes.

Model
-----
One budget group; every role is a single variant whose replicas are all the
same size. Every CYCLE seconds the planner reads demand -- LAG seconds old,
averaged over LAG seconds, as the analyzers' windows deliver it -- computes
targets, and may start or cancel transfers. A transfer's donor replica is
released RELEASE seconds after it starts (the HPA scale-down window plus
drain). The receiver holds its new replica from then on, and SERVES from it
FILL seconds later (pod start and model load). A role gives or receives at
most two replicas per cycle; at most two transfers are in flight per group.

A transfer may be cancelled while the HPA still holds the window's maximum
(its first WINDOW seconds): no donor pod has been removed yet, so a cancel
costs nothing.

Rules (each a flag, so every rule can be removed on its own):
  committed    judge band and plan on committed GPUs (held, plus in-flight
               receipts, minus in-flight gifts), not held GPUs
  actionable   only roles out of band AND off their integer target trigger,
               and every transfer must involve one
  hysteresis   cancel only on a clear reversal: the donor would land more than
               2 x its tolerance below target, or the receiver is more than
               its tolerance above target without the transfer. Without it,
               cancel mirrors admission
  hold         a role that gave cannot receive, and one that received cannot
               give, for HOLD seconds from the transfer's start or cancel
  swing        a role whose transfers change direction twice within SWING_W
               (8 x the decide-to-serve LATENCY) is following a load it cannot
               catch; for SWING_W it is planned on its MEAN need over that window
  slow_giving  a donor gives only what it would still give at its peak need
               over the last HOLD seconds (rejected in section 6.7)

Reported per scenario, averaged over SEEDS:
  landed     transfers that completed before the end of the run
  cancelled  transfers cancelled free inside the window
  wasted     landed transfers undone within one RELEASE after landing: the
             donor receives, or the receiver gives, any replica in that time
             (catches A->B->C->A cycles, not only exact pair reversals)
  short      time-averaged GPUs below instantaneous need that are actually
             SERVING, as a share of need (what makes requests queue). The
             sweep also reports it weighted by each model's weight, since the
             optimizer deliberately shorts a light model before a heavy one

Run: python3 hack/utilization-share-oscillation-sim.py
"""
import math
import random

K = 0.8              # scale-up threshold
CYCLE = 30           # optimize interval, s
WINDOW = 300         # HPA scale-down stabilization window, s
RELEASE = 360        # release time: the window + drain, s
FILL = 180           # receiver pod start + model load, s
LAG = 60             # analyzer delay: demand seen is LAG s old, averaged over LAG s
T_END = 6 * 3600     # simulated time, s
HOLD = 2 * RELEASE   # reversal hold, s
TAU = 0.15           # relative band, fraction of the target
TAU_ABS = 0.05       # idle floor, utilization
CONFIRM = 2          # consecutive actionable cycles
SEEDS = range(1, 6)
LATENCY = 2 * LAG + CONFIRM * CYCLE + RELEASE + FILL   # decide-to-serve, s
SWING_W = 8 * LATENCY  # swing damping window, s

ALL = dict(committed=True, actionable=True, hysteresis=True, hold=True, slow_giving=False, headroom_band=None,
           swing="mean")
NONE = dict(committed=False, actionable=False, hysteresis=False, hold=False, slow_giving=False, headroom_band=None,
            swing=False)


def z(G, N, w):
    if N <= 0:
        return math.inf
    x = G / N - 1
    return x / w if x >= 0 else x * w


def continuous_targets(roles, need, B):
    claims = {r: max(need[r], roles[r]["floor"]) for r in roles}
    pinned = {r for r in roles if roles[r]["floor"] >= need[r]}
    free = [r for r in roles if r not in pinned and need[r] > 0]
    S = B - sum(claims.values())
    tgt = dict(claims)
    if not free:
        return tgt
    if S >= 0:
        s = S / sum(roles[r]["w"] * need[r] for r in free)
        for r in free:
            tgt[r] = need[r] * (1 + s * roles[r]["w"])
    else:
        t = -S / sum(need[r] / roles[r]["w"] for r in free)
        for r in free:
            tgt[r] = max(roles[r]["floor"], need[r] * (1 - t / roles[r]["w"]))
    return tgt


def integer_targets(roles, need, B):
    G = {r: roles[r]["floor"] for r in roles}
    left = B - sum(G.values())
    while True:
        cands = [r for r in roles if need[r] > 0 and roles[r]["g"] <= left]
        if not cands:
            return G
        r = min(cands, key=lambda r: (z(G[r], need[r], roles[r]["w"]), -roles[r]["w"], r))
        G[r] += roles[r]["g"]
        left -= roles[r]["g"]


def tol(tgt, g):
    return max(TAU * tgt, 0.5 * g)


def in_band(G, tgt, N, g):
    if abs(G - tgt) <= tol(tgt, g):
        return True
    d = N * K
    # idle floor, only for a role that is not past its threshold
    return G > 0 and tgt > 0 and d / G <= K and abs(d / G - d / tgt) <= TAU_ABS


def simulate(roles, demand, B, rules, seed):
    """rules: None for stand-still, else a dict of the flags above."""
    rnd = random.Random(seed)
    held = {r: roles[r]["init"] for r in roles}
    serving = dict(held)
    inflight = []                    # dicts: start, dn, rc, g
    filling = []                     # (serve_time, rc, g)
    act_cnt = {r: 0 for r in roles}
    last_gave = {r: -1e9 for r in roles}
    last_got = {r: -1e9 for r in roles}
    dirs = {r: [] for r in roles}    # (t, +1 received / -1 gave), for swing detection
    swing_w = rules.get("swing_w", SWING_W) if rules else SWING_W
    damped_until = {r: -1e9 for r in roles}
    seen = {r: [] for r in roles}    # (t, need) samples, for lag and peak
    landed, gifts, receipts = [], [], []   # landed: (land_t, dn, rc)
    cancels = 0
    short, total = 0.0, 0.0
    wshort, wtotal = 0.0, 0.0
    t = 0
    while t < T_END:
        for tr in [x for x in inflight if x["start"] + RELEASE <= t]:
            held[tr["dn"]] -= tr["g"]
            serving[tr["dn"]] -= tr["g"]
            held[tr["rc"]] += tr["g"]
            filling.append((t + FILL, tr["rc"], tr["g"]))
            landed.append((t, tr["dn"], tr["rc"]))
            inflight.remove(tr)
        for f in [x for x in filling if x[0] <= t]:
            serving[f[1]] += f[2]
            filling.remove(f)

        inst = {r: demand[r](t, rnd) / K for r in roles}
        short += sum(max(0.0, inst[r] - serving[r]) for r in roles)
        total += sum(inst.values())
        wshort += sum(roles[r]["w"] * max(0.0, inst[r] - serving[r]) for r in roles)
        wtotal += sum(roles[r]["w"] * inst[r] for r in roles)
        for r in roles:
            seen[r] = [(tt, v) for tt, v in seen[r] if t - tt <= max(HOLD, 2 * LAG, swing_w)] + [(t, inst[r])]
        if rules is None:
            t += CYCLE
            continue

        # what the analyzers deliver: LAG seconds old, averaged over LAG seconds
        need = {}
        for r in roles:
            win = [v for tt, v in seen[r] if LAG <= t - tt <= 2 * LAG]
            need[r] = sum(win) / len(win) if win else inst[r]
        if rules.get("swing") == "mean":
            for r in roles:
                if t < damped_until[r]:
                    win = [v for tt, v in seen[r] if t - tt <= swing_w]
                    need[r] = sum(win) / len(win)
        cont = continuous_targets(roles, need, B)
        integ = integer_targets(roles, need, B)

        # cancellation, inside the window only
        for tr in list(inflight):
            if t - tr["start"] > WINDOW:
                continue
            others = dict(held)
            for o in inflight:
                if o is not tr:
                    others[o["dn"]] -= o["g"]
                    others[o["rc"]] += o["g"]
            dn, rc, g = tr["dn"], tr["rc"], tr["g"]
            after = others[dn] - g
            if rules["hysteresis"]:
                donor_short = after < cont[dn] - 2 * tol(cont[dn], roles[dn]["g"])
                receiver_fine = others[rc] > cont[rc] + tol(cont[rc], roles[rc]["g"])
            else:
                donor_short = after < cont[dn] and not in_band(after, cont[dn], need[dn], roles[dn]["g"])
                receiver_fine = others[rc] >= cont[rc] or in_band(others[rc], cont[rc], need[rc], roles[rc]["g"])
            if donor_short or receiver_fine:
                inflight.remove(tr)
                cancels += 1
                if rules["hold"]:
                    last_gave[dn] = t
                    last_got[rc] = t

        committed = dict(held)
        for tr in inflight:
            committed[tr["dn"]] -= tr["g"]
            committed[tr["rc"]] += tr["g"]
        base = committed if rules["committed"] else held

        for r in roles:
            trig = not in_band(base[r], cont[r], need[r], roles[r]["g"])
            hb = rules.get("headroom_band")
            if trig and hb is not None and base[r] >= need[r]:
                # headroom-only deviation: the role is not short. Act only on a
                # much larger miss (hb = fraction of target; inf = never)
                trig = abs(base[r] - cont[r]) > max(hb * cont[r], 0.5 * roles[r]["g"])
            if rules["actionable"]:
                trig = trig and base[r] != integ[r]
            act_cnt[r] = act_cnt[r] + 1 if trig else 0

        if any(act_cnt[r] >= CONFIRM for r in roles) and len(inflight) < 2:
            if rules["slow_giving"]:
                peak = {r: max(v for tt, v in seen[r] if t - tt <= HOLD) for r in roles}
                integ_give = integer_targets(roles, peak, B)
            else:
                integ_give = integ
            recv = sorted((r for r in roles if base[r] < integ[r]),
                          key=lambda r: z(base[r], need[r], roles[r]["w"]))
            donors = sorted((r for r in roles if base[r] > integ[r] and base[r] > integ_give[r]
                             and base[r] > roles[r]["floor"]),
                            key=lambda r: -z(base[r], need[r], roles[r]["w"]))
            work = dict(base)
            moved = {r: 0 for r in roles}
            for rc in recv:
                for dn in donors:
                    if len(inflight) >= 2 or moved[rc] >= 2 or moved[dn] >= 2:
                        continue
                    if roles[dn]["g"] < roles[rc]["g"]:
                        continue
                    if rules["hold"] and (t - last_gave[rc] < HOLD or t - last_got[dn] < HOLD):
                        continue
                    if rules.get("swing") is True and (t < damped_until[rc] or t < damped_until[dn]):
                        continue
                    if rules["actionable"] and act_cnt[rc] < CONFIRM and act_cnt[dn] < CONFIRM:
                        continue
                    g = roles[rc]["g"]
                    before = min(z(work[dn], need[dn], roles[dn]["w"]), z(work[rc], need[rc], roles[rc]["w"]))
                    after = min(z(work[dn] - g, need[dn], roles[dn]["w"]), z(work[rc] + g, need[rc], roles[rc]["w"]))
                    donor_ok = work[dn] - g >= cont[dn] or in_band(work[dn] - g, cont[dn], need[dn], roles[dn]["g"])
                    if not (after > before and donor_ok):
                        continue
                    work[dn] -= g
                    work[rc] += g
                    moved[dn] += 1
                    moved[rc] += 1
                    inflight.append(dict(start=t, dn=dn, rc=rc, g=g))
                    last_gave[dn] = t
                    last_got[rc] = t
                    if rules.get("swing"):
                        # a role whose transfers change direction twice within
                        # SWING_W is following a load it cannot catch: damp it
                        for r, sgn in ((dn, -1), (rc, +1)):
                            dirs[r] = [(tt, d) for tt, d in dirs[r] if t - tt <= swing_w] + [(t, sgn)]
                            flips = sum(1 for a, b in zip(dirs[r], dirs[r][1:]) if a[1] != b[1])
                            if flips >= 2:
                                damped_until[r] = t + swing_w
        t += CYCLE

    wasted = 0
    for t1, dn, rc in landed:
        if any(0 < t2 - t1 <= RELEASE and (r2 == dn or d2 == rc) for t2, d2, r2 in landed):
            wasted += 1
    return len(landed), cancels, wasted, short / total, wshort / wtotal


def noisy(level, amp=0.10):
    return lambda t, rnd: max(0.0, level * (1 + rnd.uniform(-amp, amp)))


def periodic(mean, swing, period, phase=0.0):
    return lambda t, rnd: max(0.0, mean * (1 + swing * math.sin(2 * math.pi * t / period + phase))
                              * (1 + rnd.uniform(-0.05, 0.05)))


def step(before, after, at):
    return lambda t, rnd: (before if t < at else after) * (1 + rnd.uniform(-0.05, 0.05))


def one_gpu(init):
    return {n: {"w": w, "g": 1, "floor": 1, "init": i}
            for (n, w), i in zip((("A", 2), ("B", 1), ("C", 1)), init)}


def pd_8gpu(init):
    names = (("A/prefill", 2), ("A/decode", 2), ("B/prefill", 1), ("B/decode", 1))
    return {n: {"w": w, "g": 8, "floor": 8, "init": i} for (n, w), i in zip(names, init)}


SCENARIOS = [
    ("1-GPU, 16 GPUs, steady load +-10 %", one_gpu((9, 5, 2)),
     {"A": noisy(4), "B": noisy(3), "C": noisy(1)}, 16),
    ("1-GPU, 16 GPUs, two models tied", one_gpu((8, 6, 2)),
     {"A": noisy(3.2), "B": noisy(3.2), "C": noisy(1)}, 16),
    ("1-GPU, 16 GPUs, B's demand doubles", one_gpu((9, 5, 2)),
     {"A": noisy(4), "B": step(3, 6, 3600), "C": noisy(1)}, 16),
    ("1-GPU, 12 GPUs, B's demand doubles", one_gpu((6, 5, 1)),
     {"A": noisy(4), "B": step(3, 6, 3600), "C": noisy(1)}, 12),
    ("1-GPU, 12 GPUs, A/B swap every 60 min", one_gpu((6, 5, 1)),
     {"A": periodic(3.5, 0.4, 3600), "B": periodic(3.5, 0.4, 3600, math.pi), "C": noisy(1)}, 12),
    ("1-GPU, 12 GPUs, A/B swap every 15 min", one_gpu((6, 5, 1)),
     {"A": periodic(3.5, 0.4, 900), "B": periodic(3.5, 0.4, 900, math.pi), "C": noisy(1)}, 12),
    ("1-GPU, 12 GPUs, A/B swap every 30 min", one_gpu((6, 5, 1)),
     {"A": periodic(3.5, 0.4, 1800), "B": periodic(3.5, 0.4, 1800, math.pi), "C": noisy(1)}, 12),
    ("1-GPU, 12 GPUs, A/B swap every 3 h", one_gpu((6, 5, 1)),
     {"A": periodic(3.5, 0.4, 10800), "B": periodic(3.5, 0.4, 10800, math.pi), "C": noisy(1)}, 12),
    ("8-GPU P/D, 64 GPUs, steady load +-10 %", pd_8gpu((16, 24, 8, 16)),
     {"A/prefill": noisy(8), "A/decode": noisy(14.4), "B/prefill": noisy(4.8), "B/decode": noisy(9.6)}, 64),
    ("8-GPU P/D, 64 GPUs, A prompts<->outputs every 60 min", pd_8gpu((16, 24, 8, 16)),
     {"A/prefill": periodic(6.4, 0.4, 3600), "A/decode": periodic(17.6, 0.25, 3600, math.pi),
      "B/prefill": noisy(4.8), "B/decode": noisy(9.6)}, 64),
    ("8-GPU P/D, 48 GPUs, A prompts<->outputs every 60 min", pd_8gpu((8, 24, 8, 8)),
     {"A/prefill": periodic(6.4, 0.4, 3600), "A/decode": periodic(17.6, 0.25, 3600, math.pi),
      "B/prefill": noisy(4.8), "B/decode": noisy(9.6)}, 48),
]

MAIN = [("stand still", None), ("as first written", NONE), ("section 6.7", ALL)]
ABLATIONS = [
    ("section 6.7", ALL),
    ("- committed", dict(ALL, committed=False)),
    ("- actionable", dict(ALL, actionable=False)),
    ("- hysteresis", dict(ALL, hysteresis=False)),
    ("- hold", dict(ALL, hold=False)),
    ("- swing damping", dict(ALL, swing=False)),
    ("+ slow giving", dict(ALL, slow_giving=True)),
]
REJECTED = [
    ("section 6.7", ALL),
    ("freeze swinging roles", dict(ALL, swing=True)),
    ("short receivers only", dict(ALL, headroom_band=math.inf)),
]


def table(variants):
    print("| scenario | " + " | ".join(n for n, _ in variants) + " |")
    print("| --- |" + " --- |" * len(variants))
    for name, roles, demand, B in SCENARIOS:
        cells = []
        for _, rules in variants:
            runs = [simulate(roles, demand, B, rules, s) for s in SEEDS]
            avg = [sum(r[i] for r in runs) / len(runs) for i in range(5)]
            cells.append("%.0f / %.0f / %.0f / %.1f %%" % (avg[0], avg[1], avg[2], 100 * avg[3]))
        print("| %s | %s |" % (name, " | ".join(cells)))


def sweep():
    print("A/B swap period sweep, 1-GPU, 12 GPUs -- weighted shortfall (unweighted), mean of %d seeds"
          % len(SEEDS))
    print("| swing period | stand still | section 6.7 without swing damping | section 6.7 |")
    print("| --- | --- | --- | --- |")
    for period in (900, 1800, 3600, 5400, 7200, 10800, 14400):
        roles = one_gpu((6, 5, 1))
        dem = {"A": periodic(3.5, 0.4, period), "B": periodic(3.5, 0.4, period, math.pi), "C": noisy(1)}
        cells = []
        for rules in (None, dict(ALL, swing=False), ALL):
            runs = [simulate(roles, dem, 12, rules, s) for s in SEEDS]
            cells.append("%.1f %% (%.1f %%)" % (100 * sum(r[4] for r in runs) / len(runs),
                                               100 * sum(r[3] for r in runs) / len(runs)))
        print("| %d min | %s |" % (period // 60, " | ".join(cells)))


def main():
    print("6 h, %d s cycles, %d s window, %d s release, %d s fill, %d s analyzer lag, mean of %d seeds"
          % (CYCLE, WINDOW, RELEASE, FILL, LAG, len(SEEDS)))
    print("cells: landed / cancelled free / wasted / shortfall\n")
    table(MAIN)
    print()
    table(ABLATIONS)
    print()
    table(REJECTED)
    print()
    sweep()


if __name__ == "__main__":
    main()
