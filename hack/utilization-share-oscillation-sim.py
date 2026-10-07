#!/usr/bin/env python3
"""Oscillation simulator for the utilization-share optimizer proposal.

See docs/proposals/utilization-share-optimizer.md, section 6.7. This is design
evidence, not a test of any code in the tree: it models the planner of
sections 5 and 6 closely enough to show whether a rule set oscillates.

Model. One budget group. Every role is a single variant whose replicas are
all the same size. Demand is a function of time. Every CYCLE seconds the
planner computes targets and may start transfers; a transfer's donor
replica is released DELAY seconds later (the HPA scale-down window plus
drain), and the receiver holds its new replica from then on. A role may
give or receive at most two replicas per cycle; at most two transfers are
in flight per group.

Every variant may cancel a transfer while the HPA still holds the
scale-down window's maximum (the first WINDOW seconds): no donor pod has been
removed yet, so a cancel costs nothing.

Variants:
  static          never transfers -- the baseline to beat
  as-written      the planner as the proposal stood before section 6.7: band
                  and planning judged on HELD GPUs, any out-of-band role
                  licenses any transfer, no reversal hold, and a cancel rule
                  that mirrors the admission rule
  proposed        section 6.7: COMMITTED GPUs; transfers only for actionable
                  roles (out of band AND off the integer target); a reversal
                  hold of 2 x DELAY, also after a cancel; cancel only on a
                  clear reversal (hysteresis)
  +slow-giving    proposed, plus a donor gives only what it would still give
                  at its peak need over the last 2 x DELAY. Rejected: the HPA
                  window already does this job, and it costs shortfall

Reported per scenario, averaged over SEEDS:
  landed     transfers that completed
  cancelled  transfers cancelled free inside the window
  wasted     landed transfers reversed within one DELAY after landing
  short   time-averaged GPUs below instantaneous need, as a share of need
          (what makes requests queue)

Run: python3 hack/utilization-share-oscillation-sim.py
"""
import math
import random

K = 0.8              # scale-up threshold
CYCLE = 30           # optimize interval, s
WINDOW = 300         # HPA scale-down stabilization window, s
DELAY = 360          # release time: the window + drain, s
T_END = 6 * 3600     # simulated time, s
HOLD = 2 * DELAY     # reversal hold, s
PEAK_W = 2 * DELAY   # slow-giving window, s
TAU = 0.15           # relative band, fraction of the target
TAU_ABS = 0.05       # idle floor, utilization
CONFIRM = 2          # consecutive out-of-band cycles
SEEDS = range(1, 6)


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


def in_band(G, tgt, N, g):
    if abs(G - tgt) <= max(TAU * tgt, 0.5 * g):
        return True
    d = N * K
    # idle floor, only for a role that is not past its threshold
    return G > 0 and tgt > 0 and d / G <= K and abs(d / G - d / tgt) <= TAU_ABS


def simulate(roles, demand, B, variant, seed):
    rnd = random.Random(seed)
    held = {r: roles[r]["init"] for r in roles}
    inflight = []                       # (release_time, donor, receiver, g)
    out_cnt = {r: 0 for r in roles}
    last_gave = {r: -1e9 for r in roles}
    last_got = {r: -1e9 for r in roles}
    needhist = {r: [] for r in roles}
    history = []
    cancels = 0
    short, total = 0.0, 0.0
    proposed = variant in ("proposed", "+slow-giving")
    slow_giving = variant == "+slow-giving"
    t = 0
    while t < T_END:
        for item in [i for i in inflight if i[0] <= t]:
            _, dn, rc, g = item
            held[dn] -= g
            held[rc] += g
            inflight.remove(item)
        need = {r: demand[r](t, rnd) / K for r in roles}
        short += sum(max(0.0, need[r] - held[r]) for r in roles)
        total += sum(need.values())
        for r in roles:
            needhist[r] = [(tt, v) for tt, v in needhist[r] if t - tt <= PEAK_W] + [(t, need[r])]
        if variant == "static":
            t += CYCLE
            continue
        cont = continuous_targets(roles, need, B)
        # Cancellation (section 6.3). While the HPA still holds the window's
        # maximum -- the first WINDOW seconds of a transfer -- no donor pod has
        # been removed, so cancelling costs nothing. A transfer is cancelled
        # when its donor would now be pushed below its band, or its receiver
        # is back at or above its target without it.
        for item in list(inflight):
            rel, dn, rc, g = item
            if rel - DELAY + WINDOW <= t:
                continue
            committed_now = dict(held)
            for _, d2, r2, g2 in inflight:
                if (_, d2, r2, g2) != item:
                    committed_now[d2] -= g2
                    committed_now[r2] += g2
            after = committed_now[dn] - g
            tol_d = max(TAU * cont[dn], 0.5 * roles[dn]["g"])
            if proposed:
                # hysteresis: cancel only on a CLEAR reversal -- the donor would
                # land more than twice its tolerance below target, or the
                # receiver is now at or above its target without this transfer
                donor_short = after < cont[dn] - 2 * tol_d
                receiver_fine = committed_now[rc] >= cont[rc]
            else:
                # the mirror image of the admission rule
                donor_short = after < cont[dn] and not in_band(after, cont[dn], need[dn], roles[dn]["g"])
                receiver_fine = committed_now[rc] >= cont[rc] or in_band(committed_now[rc], cont[rc], need[rc], roles[rc]["g"])
            if donor_short or receiver_fine:
                inflight.remove(item)
                cancels += 1
                history.remove((rel - DELAY, dn, rc))
                if proposed:
                    # a cancelled transfer holds the pair like a completed one
                    last_gave[dn] = t
                    last_got[rc] = t
        committed = dict(held)
        for _, dn, rc, g in inflight:
            committed[dn] -= g
            committed[rc] += g
        base = committed if proposed else held
        integ = integer_targets(roles, need, B)
        for r in roles:
            oob = not in_band(base[r], cont[r], need[r], roles[r]["g"])
            if proposed:
                oob = oob and base[r] != integ[r]
            out_cnt[r] = out_cnt[r] + 1 if oob else 0
        if any(out_cnt[r] >= CONFIRM for r in roles) and len(inflight) < 2:
            if slow_giving:
                peak = {r: max(v for _, v in needhist[r]) for r in roles}
                integ_peak = integer_targets(roles, peak, B)
            else:
                integ_peak = integ
            recv = sorted((r for r in roles if base[r] < integ[r]),
                          key=lambda r: z(base[r], need[r], roles[r]["w"]))
            donors = sorted((r for r in roles if base[r] > integ[r] and base[r] > integ_peak[r]
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
                    if proposed:
                        if t - last_gave[rc] < HOLD or t - last_got[dn] < HOLD:
                            continue
                        if out_cnt[rc] < CONFIRM and out_cnt[dn] < CONFIRM:
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
                    inflight.append((t + DELAY, dn, rc, g))
                    last_gave[dn] = t
                    last_got[rc] = t
                    history.append((t, dn, rc))
        t += CYCLE
    wasted = sum(1 for i, (t1, a, b) in enumerate(history)
                 if any(t2 - t1 <= 2 * DELAY and c == b and d == a for t2, c, d in history[i + 1:]))
    return len(history), wasted, short / total, cancels


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
    ("1-GPU replicas, 16 GPUs, steady load, +-10 % noise", one_gpu((9, 5, 2)),
     {"A": noisy(4), "B": noisy(3), "C": noisy(1)}, 16),
    ("1-GPU replicas, 16 GPUs, two models tied", one_gpu((8, 6, 2)),
     {"A": noisy(3.2), "B": noisy(3.2), "C": noisy(1)}, 16),
    ("1-GPU replicas, 16 GPUs, B doubles at 1 h", one_gpu((9, 5, 2)),
     {"A": noisy(4), "B": step(3, 6, 3600), "C": noisy(1)}, 16),
    ("1-GPU replicas, 12 GPUs, B doubles at 1 h", one_gpu((6, 5, 1)),
     {"A": noisy(4), "B": step(3, 6, 3600), "C": noisy(1)}, 12),
    ("1-GPU replicas, 12 GPUs, A/B load swaps every 15 min", one_gpu((6, 5, 1)),
     {"A": periodic(3.5, 0.4, 900), "B": periodic(3.5, 0.4, 900, math.pi), "C": noisy(1)}, 12),
    ("1-GPU replicas, 12 GPUs, A/B load swaps every 60 min", one_gpu((6, 5, 1)),
     {"A": periodic(3.5, 0.4, 3600), "B": periodic(3.5, 0.4, 3600, math.pi), "C": noisy(1)}, 12),
    ("8-GPU P/D, 64 GPUs, steady load, +-10 % noise", pd_8gpu((16, 24, 8, 16)),
     {"A/prefill": noisy(8), "A/decode": noisy(14.4), "B/prefill": noisy(4.8), "B/decode": noisy(9.6)}, 64),
    ("8-GPU P/D, 64 GPUs, A prompts<->outputs every 60 min", pd_8gpu((16, 24, 8, 16)),
     {"A/prefill": periodic(6.4, 0.4, 3600), "A/decode": periodic(17.6, 0.25, 3600, math.pi),
      "B/prefill": noisy(4.8), "B/decode": noisy(9.6)}, 64),
    ("8-GPU P/D, 48 GPUs, A prompts<->outputs every 60 min", pd_8gpu((8, 24, 8, 8)),
     {"A/prefill": periodic(6.4, 0.4, 3600), "A/decode": periodic(17.6, 0.25, 3600, math.pi),
      "B/prefill": noisy(4.8), "B/decode": noisy(9.6)}, 48),
]

VARIANTS = ("static", "as-written", "proposed", "+slow-giving")


def main():
    print("6 h simulated, %d s cycles, %d s release, mean of %d seeds" % (CYCLE, DELAY, len(SEEDS)))
    print("| scenario | " + " | ".join(VARIANTS) + " |")
    print("| --- |" + " --- |" * len(VARIANTS))
    for name, roles, demand, B in SCENARIOS:
        cells = []
        for v in VARIANTS:
            runs = [simulate(roles, demand, B, v, s) for s in SEEDS]
            n = sum(r[0] for r in runs) / len(runs)
            w = sum(r[1] for r in runs) / len(runs)
            sh = sum(r[2] for r in runs) / len(runs)
            c = sum(r[3] for r in runs) / len(runs)
            cells.append("%.0f / %.0f / %.0f / %.1f %%" % (n, c, w, 100 * sh))
        print("| %s | %s |" % (name, " | ".join(cells)))
    print("cells: transfers landed / cancelled free inside the window / wasted / shortfall")


if __name__ == "__main__":
    main()
