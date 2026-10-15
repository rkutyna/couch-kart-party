"""Offline classifier test over the labelled references: nav/refs + labels.json (16:9,
screen.py) and nav/refs43 + labels43.json (4:3 two-player profiles, screen43.py)."""
from __future__ import annotations

import json
import os
import time

import screen
import screen43

NAV_DIR = os.path.dirname(os.path.abspath(__file__))
LABELS = os.path.join(NAV_DIR, "labels.json")
LABELS43 = os.path.join(NAV_DIR, "labels43.json")


def norm(c):
    return tuple(c) if isinstance(c, list) else c


def run(verbose: bool = False, refs: str | None = None, labels: str | None = None,
        refs43: str | None = None, labels43: str | None = None) -> int:
    """Classify every labelled capture with the bank in use (screen.BANK_FILE,
    screen43.BANK_FILE). Default captures: the hand-labelled nav/refs + labels.json and
    nav/refs43 + labels43.json; calibration passes its own (<assets>/captures + labels)."""
    rc = 0
    for mod, lab, refs_dir in ((screen, labels or LABELS, refs or screen.REFS_DIR),
                               (screen43, labels43 or LABELS43, refs43 or screen43.REFS_DIR)):
        print(f"== {lab} ({mod.__name__}, bank {mod.BANK_FILE})")
        rc |= run_one(mod, lab, verbose, refs_dir)
    return rc


def run_one(screen, LABELS, verbose: bool = False, refs_dir: str | None = None) -> int:
    refs_dir = refs_dir or screen.REFS_DIR
    with open(LABELS) as f:
        labels = json.load(f)["labels"]
    conf, wrong, cur_wrong, times = {}, [], [], []
    per = {}
    ncur = 0
    for name in sorted(labels):
        want = labels[name]
        path = os.path.join(refs_dir, name)
        if not os.path.exists(path):
            wrong.append((name, want["state"], "MISSING FILE", ""))
            continue
        fr = screen.load_frame(path)
        t0 = time.perf_counter()
        res = screen.classify_frame(fr)
        times.append((time.perf_counter() - t0) * 1000)
        ok = res.state == want["state"]
        conf[(want["state"], res.state)] = conf.get((want["state"], res.state), 0) + 1
        p = per.setdefault(want["state"], [0, 0])
        p[1] += 1
        p[0] += int(ok)
        if not ok:
            wrong.append((name, want["state"], str(res), res.detail))
        elif "cursor" in want:
            ncur += 1
            if norm(want["cursor"]) != norm(res.cursor):
                cur_wrong.append((name, want["cursor"], str(res)))
        if verbose:
            print(f"{name:44s} {res}")
    total = sum(v[1] for v in per.values())
    good = sum(v[0] for v in per.values())
    print(f"labelled captures: {total}; state correct: {good} ({100.0 * good / max(total, 1):.1f}%)")
    print(f"cursor labels: {ncur}; cursor correct: {ncur - len(cur_wrong)}")
    print("per state (correct/labelled):")
    for s in sorted(per):
        print(f"  {s:20s} {per[s][0]:3d}/{per[s][1]:3d}")
    off = sorted((t, g, n) for (t, g), n in conf.items() if t != g)
    print("confusions (truth -> got: count):", ", ".join(f"{t}->{g}:{n}" for t, g, n in off) or "none")
    for name, truth, got, det in wrong:
        print(f"  WRONG {name}: truth={truth} got={got} {det}")
    for name, want, got in cur_wrong:
        print(f"  CURSOR {name}: want={want} got={got}")
    if times:
        print(f"classify time: mean {sum(times) / len(times):.1f} ms, max {max(times):.1f} ms")
    return 0 if not wrong and not cur_wrong else 1


if __name__ == "__main__":
    import sys
    sys.exit(run("-v" in sys.argv))
