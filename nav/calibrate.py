"""Calibration: generate the navigator's game-derived assets on this Mac, from the user's
own copy of the game. Nothing game-derived is shipped with the project.

    ./mkparty calibrate                (nav.py calibrate)

What it does (about 10-15 minutes, Dolphin windows appear, the games stay muted):

  1. capture   every screen the navigator must recognise is brought up with the game's own
               menus and captured. Which screen is on display is read WITHOUT templates:
               Apple's Vision OCR reads the title strip, hint line, name labels and dialog
               texts (nav/ocrscreen.py) and the known menu flow says what to press.
               a. first-boot screens (license, data notice, Permit, ...): seen while
                  `nav.py setup` prepares fresh profiles, else on a scratch profile i13
                  (instances/i13, deleted afterwards);
               b. offline menus, no time limit: i1 (16:9) and i7 (4:3, two players) tour
                  the main menu, all 12 characters, all 18 vehicles, drift, the 4 unlocked
                  cups and their course lists;
               c. online: i1 and i7 go online together (local game server) and play two
                  short rounds: WFC menus, Worldwide VS, Check Members, the vote, Course
                  Poll, the race, the results / Next Race / Please wait, and the
                  disconnect dialog of the console that idles.
  2. build     for every template in screen.TEMPLATES / screen43.TEMPLATES (+ name labels)
               the captures matching its source spec are compared and the ones that
               cover the others best are cut into <assets>/templates.npz, templates43.npz.
  3. check     every capture is classified with the new banks and must come out as OCR
               labelled it (self-test; also `nav.py test --refs <assets>`).

Output (<assets> = MKPARTY_ASSETS, default <root>/assets; never in git):
    templates.npz, templates43.npz    the banks the navigator uses
    captures/16x9/*.png, captures/4x3/*.png, captures/index.jsonl   the captures + OCR labels
    labels.json, labels43.json        the labels in the offline-test format (refs = captures/*)
    sources.json                      which capture each template was cut from
    icons/<character>.png             test-phone minimap icons (when a race was seen)
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import threading
import time

import numpy as np

import dolphin
import driver
import localconf
import ocrscreen
import screen as S
import screen43 as S4

NAV_DIR = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(NAV_DIR)
RUN = os.path.join(ROOT, "run")
TRANSIENT = {"unknown", "black"}
SCRATCH_K = 13


def aspect_of(a43: bool) -> str:
    return "4x3" if a43 else "16x9"


# ====================================================================== collector
class Collector:
    """Keeps captures of every (aspect, state, cursor, texts) seen on a settled screen: a
    frame is kept when the OCR classification has been the same for `settle` consecutive
    observations; at most `per_key` frames per key, `gap` s apart (dialogs: more frames,
    for the pulsing (A) icon)."""

    # state -> (frames kept, s between them, consecutive same observations needed)
    LIMITS = {"welcome": (10, 0.12, 3), "disconnected_dialog": (10, 0.12, 3), "error_dialog": (10, 0.12, 3),
              "race": (6, 1.0, 3), "title": (3, 0.8, 6), "health": (3, 0.8, 6),
              "course_poll": (4, 0.3, 2), "next_race_menu": (3, 0.3, 2), "please_wait": (4, 0.3, 2)}

    def __init__(self, assets: str, log, per_key: int = 3, gap: float = 0.35, settle: int = 3):
        self.dir = os.path.join(assets, "captures")
        self.index = os.path.join(self.dir, "index.jsonl")
        os.makedirs(os.path.join(self.dir, "16x9"), exist_ok=True)
        os.makedirs(os.path.join(self.dir, "4x3"), exist_ok=True)
        self.log = log
        self.per_key, self.gap, self.settle = per_key, gap, settle
        self.count, self.last = {}, {}
        self.n = 0
        self.lock = threading.Lock()
        self.saver = driver.Saver()
        self.saver.start()
        self.keys_seen = set()
        self.lum = {}              # instance -> mean brightness of its last observation
        if os.path.exists(self.index):
            for line in open(self.index):
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                key = self._key(e["aspect"], e["state"], e.get("cursor"), e.get("title", ""), e.get("hint", ""))
                self.count[key] = self.count.get(key, 0) + 1
                self.n += 1

    @staticmethod
    def _key(aspect, state, cursor, title, hint):
        c = tuple(cursor) if isinstance(cursor, list) else cursor
        return (aspect, state, c, title, hint)

    def offer(self, k: int, a43: bool, frame, res, n_same: int, now: float, tag: str = ""):
        limit, gap, settle = self.LIMITS.get(res.state, (self.per_key, self.gap, self.settle))
        lum = res.detail.get("stats", {}).get("lum", 0.0)
        prev = self.lum.get(k)
        self.lum[k] = lum
        if res.state in TRANSIENT or n_same < settle:
            return None
        if prev is not None and abs(lum - prev) > max(4.0, 0.06 * lum):
            return None                 # fading in or out
        aspect = aspect_of(a43)
        d = res.detail
        title, hint = d.get("title_text", ""), d.get("hint_text", "")
        if res.state in S.DIALOG_STATES or res.state in ("race", "results", "please_wait", "preparing",
                                                         "opponents_chosen", "next_race_menu"):
            title = hint = ""          # their OCR text changes from frame to frame
        key = self._key(aspect, res.state, res.cursor, title, hint)
        with self.lock:
            self.keys_seen.add(key)
            if self.count.get(key, 0) >= limit or now - self.last.get(key, 0.0) < gap:
                return None
            self.count[key] = self.count.get(key, 0) + 1
            self.last[key] = now
            self.n += 1
            cur = res.cursor
            cs = "" if cur is None else "_" + "-".join(str(x) for x in (cur if isinstance(cur, tuple) else (cur,)))
            name = f"{aspect}/{res.state}{cs}_i{k}_{self.n:04d}.png"
            ocr = " | ".join(t for t, _b in d.get("ocr", []))
            e = {"file": name, "aspect": aspect, "state": res.state,
                 "cursor": list(cur) if isinstance(cur, tuple) else cur, "title": title, "hint": hint,
                 "text": ocrscreen.norm(ocr), "k": k, "t": round(now, 3), "how": res.how, "tag": tag}
            if "members" in d:
                e["members"] = d["members"]
            if "poll" in d:
                e["poll"] = d["poll"]
            with open(self.index, "a") as f:
                f.write(json.dumps(e) + "\n")
        self.saver.save(frame, os.path.join(self.dir, name))
        return name

    def worker_hook(self, w, frame, res, now):
        """driver.Worker collector: (worker, frame, result, time)."""
        self.offer(w.k, w.inst.aspect43, frame, res, w.raw_n, now, tag="worker")

    def flush(self):
        self.saver.q.join()

    def entries(self):
        self.flush()
        out = []
        if os.path.exists(self.index):
            for line in open(self.index):
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                if os.path.exists(os.path.join(self.dir, e["file"])):
                    out.append(e)
        return out

    def have(self, aspect, state, cursor=None, text=None) -> bool:
        for e in self.entries():
            if e["aspect"] == aspect and e["state"] == state and (cursor is None or _cur_eq(e["cursor"], cursor)) \
                    and (text is None or text in e["text"]):
                return True
        return False


def _cur_eq(have, want) -> bool:
    """want may hold None as a wildcard (in a tuple)."""
    if want is None:
        return True
    h = tuple(have) if isinstance(have, list) else have
    if isinstance(want, tuple):
        if not isinstance(h, tuple) or len(h) != len(want):
            return False
        return all(w is None or w == x for w, x in zip(want, h))
    return h == want


# ====================================================================== pilot
class Stop(Exception):
    pass


class Pilot:
    """A sequential, closed-loop driver for one instance, reading screens with OCR. Used for
    the menu tours (the online rounds use driver.Worker with the same classifier)."""

    def __init__(self, k: int, log, collector: Collector, setup: bool = False):
        self.k = k
        self.inst = dolphin.Instance(k, setup=setup)
        self.a43 = self.inst.aspect43
        self.cls = ocrscreen.OcrScreen(self.a43)
        self.log = log
        self.col = collector
        self.key = None
        self.n = 0
        self.res = None
        self.deadline = None

    def say(self, msg):
        self.log(f"cal i{self.k}: {msg}")

    def observe(self):
        if self.deadline and time.time() > self.deadline:
            raise Stop(f"i{self.k}: time is up")
        fr = self.inst.grab() if self.inst.alive() else None
        if fr is None:
            if not self.inst.alive():
                raise Stop(f"i{self.k}: Dolphin is not running")
            time.sleep(0.3)
            return None
        r = self.cls.classify_frame(fr)
        key = (r.state, r.cursor)
        if key == self.key:
            self.n += 1
        else:
            self.key, self.n = key, 1
        self.res = r
        self.col.offer(self.k, self.a43, fr, r, self.n, time.time(), tag="pilot")
        return r

    def settled(self, timeout: float = 15.0, n: int = 2, ok=None):
        """The next screen seen on n consecutive observations (not black / unknown) that
        satisfies ok (if given), or None after timeout."""
        t0 = time.time()
        while time.time() - t0 < timeout:
            r = self.observe()
            if r is None or r.state in TRANSIENT or self.n < n:
                continue
            if ok is None or ok(r):
                return r
        return None

    def dwell(self, seconds: float):
        """Keep observing (and collecting) for a while."""
        t0 = time.time()
        while time.time() - t0 < seconds:
            self.observe()

    def press(self, b: str, pad: int = 1):
        self.inst.press(b, pad)

    def move(self, d: str, pad: int = 1):
        self.inst.move(d, pad)

    def act(self, what, timeout: float = 4.0, pad: int = 1, n: int = 2):
        """Press a button ("A", "B") or flick ("UP", ...) and return the first settled screen
        that differs from the one before (None if nothing changed within timeout)."""
        before = self.key
        if what in dolphin.DIRS:
            self.move(what, pad)
        else:
            self.press(what, pad)
        t0 = time.time()
        while time.time() - t0 < timeout:
            r = self.observe()
            if r is None or r.state in TRANSIENT or self.n < n:
                continue
            if (r.state, r.cursor) != before:
                return r
        return None

    def until(self, want, step, timeout: float = 60.0, desc: str = ""):
        """Repeat: settle; if want(res) return it; else step(res) (press something)."""
        t0 = time.time()
        while time.time() - t0 < timeout:
            r = self.settled(timeout=8.0)
            if r is None:
                continue
            if want(r):
                return r
            step(r)
        raise Stop(f"i{self.k}: did not reach {desc or 'the wanted screen'} in {timeout:.0f} s "
                   f"(last {self.res and self.res.state} {self.res and self.res.cursor})")


# ====================================================================== menu tours
def vstep(r0: int, r1: int, rows: int) -> str:
    return driver.vstep(r0, r1, rows)


def grid_move(a, b, rows):
    """One flick from grid cell a to b (rows wrap; columns move left / right)."""
    (r0, c0), (r1, c1) = a, b
    if r0 != r1:
        return vstep(r0, r1, rows)
    return "RIGHT" if c1 > c0 else "LEFT"


def boot_to(p: Pilot, state: str = "main_menu", timeout: float = 150.0):
    """From wherever the instance is (booting, a menu) to `state` (main menu by default)."""
    def step(r):
        if r.state in ("health", "title"):
            p.dwell(4.5)                     # (the title logo flies in: keep the settled picture)
            p.act("A", timeout=3.0)
        elif r.state == "license_select" or r.state in S.DIALOG_STATES:
            p.dwell(0.8)
            p.act("A", timeout=3.0)
        elif r.state == "race":
            p.act("A", timeout=3.0)          # the title demo
        else:
            p.act("B", timeout=3.0)
    return p.until(lambda r: r.state == state, step, timeout=timeout, desc=state)


def explore(p: Pilot, state: str, node_of, moves=("RIGHT", "DOWN", "LEFT", "UP"), max_steps: int = 48,
            dwell: float = 0.5, pad: int = 1):
    """Visit every cursor position of a menu by trying every move from every position seen
    (the menu's layout is learnt, not assumed). Returns {node: {move: node}}."""
    graph = {}
    r = p.settled(ok=lambda r: r.state == state)
    if r is None:
        raise Stop(f"i{p.k}: not on {state}")
    cur = node_of(r)
    for _ in range(max_steps):
        p.dwell(dwell)
        tried = graph.setdefault(cur, {})
        todo = [m for m in moves if m not in tried]
        if todo:
            m = todo[0]
        else:
            path = _bfs(graph, cur, lambda n: any(mv not in graph.get(n, {}) for mv in moves))
            if not path:
                break
            m = path[0]
        r = p.act(m, timeout=1.6, pad=pad)
        if r is None:
            new = cur
        elif r.state != state:
            p.say(f"explore {state}: {m} left the menu ({r.state}); back")
            p.act("B", timeout=4.0)
            graph[cur][m] = cur
            r = p.settled(ok=lambda r: r.state == state, timeout=8.0)
            if r is None:
                raise Stop(f"i{p.k}: lost {state} while exploring")
            cur = node_of(r)
            continue
        else:
            new = node_of(r)
        graph[cur][m] = new
        cur = new
    p.say(f"explore {state}: {len(graph)} positions: {sorted(map(str, graph))}")
    return graph


def _bfs(graph, start, goal):
    """Moves from start to the nearest node satisfying goal (excluding start), via known edges."""
    from collections import deque
    q, seen = deque([(start, [])]), {start}
    while q:
        n, path = q.popleft()
        if path and goal(n):
            return path
        for m, nb in graph.get(n, {}).items():
            if nb not in seen:
                seen.add(nb)
                q.append((nb, path + [m]))
    return None


def go_to(p: Pilot, graph, want, node_of, pad: int = 1, tries: int = 3):
    for _ in range(tries):
        r = p.settled()
        cur = node_of(r)
        if want(cur):
            return r
        path = _bfs(graph, cur, want)
        if not path:
            raise Stop(f"i{p.k}: no known way from {cur}")
        for m in path:
            p.act(m, timeout=1.6, pad=pad)
    r = p.settled()
    if not want(node_of(r)):
        raise Stop(f"i{p.k}: could not reach the wanted cursor (at {node_of(r)})")
    return r


def menu_pick(p: Pilot, phrase: str, move: str = "DOWN", max_moves: int = 8):
    """On a list menu (other_menu): move until the hint line says `phrase`, then A."""
    for _ in range(max_moves + 1):
        r = p.settled()
        if phrase in (r.detail.get("hint_text") or ""):
            return p.act("A", timeout=5.0)
        p.act(move, timeout=1.6)
    raise Stop(f"i{p.k}: no menu entry '{phrase}'")


def main_menu_tour(p: Pilot):
    node = lambda r: (r.cursor, r.detail.get("hint_text", ""))
    g = explore(p, "main_menu", node)
    seen = {c for c, _h in g}
    p.say(f"main menu cursors seen: {sorted(map(str, seen))}")
    return g, node


def single_grid_tour(p: Pilot, state: str, targets, grid, rows: int, dwell: float = 0.9):
    for t in targets:
        for _ in range(14):
            r = p.settled(ok=lambda r: r.state == state, timeout=8.0)
            if r is None:
                raise Stop(f"i{p.k}: left {state}")
            if r.cursor == t:
                break
            if r.cursor not in grid:
                raise Stop(f"i{p.k}: {state} cursor {r.cursor} not in the grid")
            p.act(grid_move(grid[r.cursor], grid[t], rows), timeout=2.0)
        else:
            raise Stop(f"i{p.k}: could not reach {t} on {state}")
        p.dwell(dwell)


def cups_tour(p: Pilot, cat, two: bool = False, dwell: float = 0.9):
    """The unlocked cups on Courses, and the course list of each (all four rows)."""
    cups = [c for c in S.CUP_ORDER if cat.cup_unlocked.get(c)]
    for cup in cups:
        for _ in range(12):
            r = p.settled(ok=lambda r: r.state in ("courses_cups", "courses_list", "drift"), timeout=6.0)
            if r is None:
                p.say(f"cups: no readable cup ({p.res and p.res.how}); looking again")
                continue
            if r.state == "drift":                # one B too many: back to Courses
                p.act("A", timeout=4.0)
                continue
            if r.state == "courses_list":
                p.act("B", timeout=3.0)
                continue
            if r.cursor == cup:
                break
            (r0, c0), (r1, c1) = cat.cup_grid[r.cursor], cat.cup_grid[cup]
            p.act("DOWN" if r1 > r0 else "UP" if r1 < r0 else ("RIGHT" if c1 > c0 else "LEFT"), timeout=2.0)
        else:
            raise Stop(f"i{p.k}: could not reach the {cup} cup")
        p.dwell(dwell)
        p.press("A")
        r = p.settled(ok=lambda r: r.state == "courses_list", timeout=6.0)
        if r is None and p.res is not None and p.res.state == "courses_cups":
            p.press("A")
            r = p.settled(ok=lambda r: r.state == "courses_list", timeout=6.0)
        if r is None:
            raise Stop(f"i{p.k}: A on the {cup} cup did not open its list ({p.res and p.res.state})")
        for _row in range(4):
            p.dwell(dwell)
            p.act("DOWN", timeout=2.0)
        p.act("B", timeout=4.0)


CHAR_ORDER = ["mario", "yoshi", "wario", "donkey-kong", "baby-mario", "toad", "koopa-troopa", "baby-peach",
              "bowser", "waluigi", "peach", "luigi"]
WEIGHT_CHAR = {"light": "baby-mario", "medium": "mario", "heavy": "bowser"}


def vehicle_order(cat, weight):
    vs = [v for v, w in cat.veh_weight.items() if w == weight and v in cat.veh_grid]
    return sorted(vs, key=lambda v: (cat.veh_grid[v][1], cat.veh_grid[v][0] * (1 if cat.veh_grid[v][1] == 0 else -1)))


def tour_1p(p: Pilot, cat):
    """16:9 offline tour: main menu, Single Player > VS > Solo Race: all characters, the
    vehicles of each weight class, drift, the cups and their course lists."""
    boot_to(p)
    g, node = main_menu_tour(p)
    go_to(p, g, lambda n: n[0] == "single", node)
    p.until(lambda r: r.state == "other_menu" and "single player" in (r.detail.get("title_text") or ""),
            lambda r: p.act("A", timeout=6.0), timeout=30, desc="Single Player")
    menu_pick(p, "custom rules")
    p.until(lambda r: r.state == "characters", lambda r: p.act("A", timeout=6.0), timeout=30, desc="Characters")
    single_grid_tour(p, "characters", CHAR_ORDER, cat.char_grid, 6)
    for i, weight in enumerate(("light", "medium", "heavy")):
        single_grid_tour(p, "characters", [WEIGHT_CHAR[weight]], cat.char_grid, 6, dwell=0.3)
        r = p.act("A", timeout=6.0)
        if r is None or r.state != "vehicles":
            r = p.settled(ok=lambda r: r.state == "vehicles", timeout=8.0)
        if r is None:
            raise Stop(f"i{p.k}: no Vehicles after picking {WEIGHT_CHAR[weight]}")
        single_grid_tour(p, "vehicles", vehicle_order(cat, weight), cat.veh_grid, 3)
        if i < 2:
            p.until(lambda r: r.state == "characters", lambda r: p.act("B", timeout=4.0), timeout=20,
                    desc="Characters again")
    p.until(lambda r: r.state == "drift", lambda r: p.act("A", timeout=6.0), timeout=20, desc="Drift")
    explore(p, "drift", lambda r: r.cursor, moves=("UP", "DOWN", "RIGHT", "LEFT"), max_steps=12, dwell=0.8)
    p.until(lambda r: r.state == "drift" and r.cursor == "auto",
            lambda r: p.act("DOWN" if r.cursor == "help" else "UP", timeout=2.0), timeout=20, desc="drift auto")
    p.until(lambda r: r.state in ("courses_cups", "courses_list"), lambda r: p.act("A", timeout=6.0),
            timeout=20, desc="Courses")
    cups_tour(p, cat)
    boot_to(p)
    p.say("16:9 menu tour done")


def two_grid_tour(p: Pilot, targets, grid, pad: int, dwell: float = 0.9):
    """Two-player Characters: move one player's cursor (pad) over targets; a target the
    other player's cursor sits on is skipped (two cursors cannot share a cell) and
    returned."""
    idx, other = pad - 1, 2 - pad
    left = []
    for t in targets:
        for _ in range(12):
            r = p.settled(ok=lambda r: r.state == "characters", timeout=8.0)
            if r is None:
                raise Stop(f"i{p.k}: left Characters")
            cur = r.cursor[idx]
            if cur == t:
                p.dwell(dwell)
                break
            if r.cursor[other] == t:
                left.append(t)
                break
            p.act(grid_move(grid[cur], grid[t], 6), timeout=2.0, pad=pad)
        else:
            left.append(t)
    return left


def tour_2p(p: Pilot, cat):
    """4:3 offline tour of a two-player profile: main menu, Multiplayer (two players) >
    controller registration > VS > Solo Race: the vehicles of each weight class, both
    players' drift, cups and course lists."""
    boot_to(p)
    g, node = main_menu_tour(p)
    go_to(p, g, lambda n: n[0] == "multi" and "two players" in n[1], node)

    def step(r):
        if r.state == "controller_reg":
            p.act("A", timeout=4.0, pad=2 if r.cursor == "wait" else 1)
        elif r.state == "main_menu":
            p.act("A", timeout=6.0)
        elif r.state == "other_menu" and "multiplayer" in (r.detail.get("title_text") or ""):
            menu_pick(p, "custom rules")
        else:
            p.act("A", timeout=6.0)
    p.until(lambda r: r.state == "characters", step, timeout=60, desc="two-player Characters")
    # (the names and player A's OK mark are captured online: offline the labels sit higher)
    grid = cat.char_grid
    two_grid_tour(p, ["baby-mario"], grid, 1, dwell=0.2)
    if two_grid_tour(p, ["luigi"], grid, 2, dwell=0.2):
        raise Stop(f"i{p.k}: player B could not reach Luigi")
    p.until(lambda r: r.state == "vehicles",
            lambda r: (p.press("A", 1), time.sleep(0.4), p.act("A", timeout=5.0, pad=2)),
            timeout=30, desc="two-player Vehicles")

    def carousel(n=6):
        for _ in range(n):
            p.dwell(0.9)
            p.inst.move_pads(((1, "RIGHT"), (2, "RIGHT")))
            p.settled(ok=lambda r: r.state == "vehicles", timeout=4.0)
    carousel()
    p.until(lambda r: r.state == "characters", lambda r: p.act("B", timeout=4.0), timeout=20,
            desc="Characters again")
    two_grid_tour(p, ["bowser"], grid, 1, dwell=0.2)
    p.until(lambda r: r.state == "vehicles",
            lambda r: (p.press("A", 1), time.sleep(0.4), p.act("A", timeout=5.0, pad=2)),
            timeout=30, desc="two-player Vehicles (heavy)")
    carousel()
    p.act("A", timeout=3.0)
    p.dwell(1.0)
    p.until(lambda r: r.state == "drift", lambda r: p.act("A", timeout=5.0, pad=2), timeout=20, desc="Drift")
    p.dwell(0.9)
    for mv, pad in (("DOWN", 1), ("DOWN", 2), ("UP", 1)):
        p.act(mv, timeout=2.0, pad=pad)
        p.dwell(0.9)
    p.act("A", timeout=3.0)
    p.dwell(1.2)
    p.until(lambda r: r.state in ("courses_cups", "courses_list"), lambda r: p.act("A", timeout=5.0, pad=2),
            timeout=20, desc="Courses")
    cups_tour(p, cat)
    boot_to(p)
    p.say("4:3 two-player menu tour done")


# ====================================================================== online rounds
LINGER = {"welcome": 1.6, "disconnected_dialog": 1.6, "error_dialog": 1.6, "main_menu": 0.8,
          "wfc_menu": 0.8, "ww_mode": 0.8, "vote_prompt": 1.2, "courses_cups": 0.8, "courses_list": 0.8,
          "next_race_menu": 2.5, "check_members": 0.6, "characters": 0.6, "vehicles": 0.6, "drift": 0.6,
          "controller_reg": 1.5, "choose_mii": 1.2, "connect2": 1.2, "health": 4.5, "title": 4.5,
          "license_select": 1.0, "create_license": 1.2, "wfc_notice": 1.2, "permit_prompt": 1.2,
          "wont_play": 1.2, "connecting": 0.0}
# Two-player Characters online: the name labels sit lower than on the offline Multiplayer
# screen, so both players' cursors tour the grid there (_tour2p_step; one row per step,
# ~1 s each; the game confirms by itself about 25 s after VS).
ICON_CHARS = ["mario", "luigi", "peach"]


class Racer(threading.Thread):
    """During a race: the active instance accelerates and weaves (so the game keeps it in
    the race), the idle one sends nothing (the game drops it after 30-60 s). Race frames of
    the single-player instance are kept for the test phones' minimap icons."""

    def __init__(self, runner, active, col: Collector, icon_k=None, icon_char=None):
        super().__init__(daemon=True)
        self.r, self.active, self.col = runner, set(active), col
        self.icon_k, self.icon_char = icon_k, icon_char
        self.stop_ = False
        self.icon_frames = []

    def run(self):
        held = {}
        last_icon = 0.0
        t = 0
        while not self.stop_:
            t += 1
            for k, w in list(self.r.workers.items()):
                racing = w.state == "race"
                pads = (1, 2) if w.two else (1,)
                if k == self.icon_k and racing and time.time() - last_icon > 0.7 and w.frame is not None \
                        and len(self.icon_frames) < 40:
                    last_icon = time.time()
                    self.icon_frames.append(w.frame)
                if k not in self.active:
                    continue
                try:
                    if racing:
                        for pad in pads:
                            if not held.get((k, pad)):
                                w.inst.send("PRESS A", pad)
                                held[(k, pad)] = True
                            x = 0.5 + 0.35 * np.sin(t / 6.0 + pad)
                            w.inst.send(f"SET MAIN {x:.2f} 0.5", pad)
                    elif any(held.get((k, pd)) for pd in pads):
                        for pad in pads:
                            w.inst.send("RELEASE A", pad)
                            w.inst.send("SET MAIN 0.5 0.5", pad)
                            held[(k, pad)] = False
                except OSError:
                    pass
            time.sleep(0.25)
        for (k, pad), h in held.items():
            if h and k in self.r.workers:
                try:
                    self.r.workers[k].inst.send("RELEASE A", pad)
                    self.r.workers[k].inst.send("SET MAIN 0.5 0.5", pad)
                except OSError:
                    pass


def _tour2p_step(w, tr, log):
    """Online two-player Characters: player A's cursor walks down the left column while
    player B's walks down the right one (two cursors never meet that way), ~1 s on each
    pair; then player A's OK mark is shown on Mario, then both confirm."""
    if not w.hold_pick:
        return
    st, cur = w.stable
    now = time.time()
    if st in ("vehicles", "drift", "searching") or (st not in ("characters", None, "unknown", "black")
                                                     and tr.get("plan")):
        w.hold_pick = False
        return
    if st != "characters" or not isinstance(cur, tuple):
        return
    grid = w.cat.char_grid
    if "plan" not in tr:
        # left column for A, right column for B, starting from the rows they are on
        col = {c: sorted((rc[0], n) for n, rc in grid.items() if rc[1] == c) for c in (0, 1)}
        ra = grid.get(cur[0], (2, 0))[0]
        rb = grid.get(cur[1], (2, 1))[0]
        if ra == rb and grid.get(cur[0], (0, 0))[1] == 1:
            rb = (rb + 1) % 6
        tr["plan"] = [(col[0][(ra + i) % 6][1], col[1][(rb + i) % 6][1]) for i in range(6)] + [("mario", "luigi")]
        tr["i"], tr["at"], tr["ok_at"] = 0, None, None
        log(f"cal i{w.k}: two-player character tour: {tr['plan']}")
        w.set_goal(driver.replace(w.goal, character=tr["plan"][0][0], character_b=tr["plan"][0][1]))
        return
    plan = tr["plan"]
    want = plan[tr["i"]]
    if (cur[0], cur[1]) != want:
        tr["at"] = None
        return
    if tr["at"] is None:
        tr["at"] = now
        return
    if now - tr["at"] < 0.9:
        return
    if tr["i"] < len(plan) - 1:
        tr["i"] += 1
        tr["at"] = None
        a, b = plan[tr["i"]]
        w.set_goal(driver.replace(w.goal, character=a, character_b=b))
        return
    if tr["ok_at"] is None:
        w.inst.press("A", 1)                  # player A confirms: its OK mark shows
        tr["ok_at"] = now
    elif now - tr["ok_at"] > 1.6:
        log(f"cal i{w.k}: two-player character tour done")
        w.inst.press("A", 2)                  # player B confirms: on to Vehicles
        w.two_done = {"characters": {1, 2}}
        w.two_done_at = now
        w.hold_pick = False


def online_rounds(log, col: Collector, run_id: str, ks=(1, 7), rounds: int = 3, timeout: float = 420.0):
    """Bring ks online together and play `rounds` short rounds (Luigi Circuit). Round i:
    instance ks[i % 2] drives, the other idles. Returns {character: [race frames]} of the
    single-player instance."""
    r = driver.Runner(list(ks), log, run_id, classifier_factory=ocr_factory(), collector=col.worker_hook)
    for w in r.workers.values():
        w.linger = dict(LINGER)
    r.start()
    icons = {}
    try:
        if not r.prepare(300):
            raise Stop("online: the instances did not reach the Nintendo WFC menu")
        one = [k for k in ks if dolphin.inst_mode(k) == "1p"]
        for rnd in range(1, rounds + 1):
            active = [ks[(rnd - 1) % len(ks)]]
            ch = ICON_CHARS[(rnd - 1) % len(ICON_CHARS)]
            log(f"cal online round {rnd}/{rounds}: active (drives) {active}, single-player character {ch}")
            racer = Racer(r, active, col, icon_k=one[0] if one else None, icon_char=ch)
            racer.start()
            t0 = time.time()
            for i, (k, w) in enumerate(sorted(r.workers.items(), key=lambda kw: (kw[1].two, kw[0]))):
                slots = (2 * k, 2 * k + 1) if w.two else (2 * k,)
                a_b = ("mario", "luigi") if w.two else (ch, "luigi")
                g = driver.Goal("setup", round=rnd, character=a_b[0], vehicle="standard-kart-m", drift="auto",
                                vote="luigi-circuit", go=True, slots=slots, character_b=a_b[1],
                                vehicle_b="standard-kart-m", vote_b="luigi-circuit", enter=False)
                w.set_goal(g)
                w.hold_pick = w.two
            tours = {k: {} for k, w in r.workers.items() if w.two}
            order = sorted(r.workers.values(), key=lambda w: (w.two, w.k))
            entered = 0
            while time.time() - t0 < timeout:
                for k, tr in tours.items():
                    _tour2p_step(r.workers[k], tr, log)
                n_in = min(len(order), 1 + int((time.time() - t0) // driver.ENTER_STAGGER))
                for w in order[entered:n_in]:
                    w.set_goal(driver.replace(w.goal, enter=True))
                entered = max(entered, n_in)
                ws = list(r.workers.values())
                if all(w.handed for w in ws) and all(w.lost for w in ws) and all(w.at_wfc for w in ws):
                    break
                if all(w.at_wfc for w in ws) and time.time() - t0 > 120 and not any(w.handed for w in ws):
                    log("cal online: nobody got into a room; next round")
                    break
                time.sleep(0.15)
            racer.stop_ = True
            racer.join(timeout=3)
            if racer.icon_frames:
                icons[ch] = racer.icon_frames
            log(f"cal online round {rnd} over after {time.time() - t0:.0f} s: {r.summary()}")
    finally:
        r.stop()
        col.flush()
    return icons


# ====================================================================== build
COVER = {"title": 0.90, "hint": 0.90, "back": 0.90, "aicon": 0.85, "charname": 0.95, "vehname": 0.95,
         "cup": 0.95, "banner": 0.95, "okbtn": 0.85}
EXTRA = {"title": 2, "hint": 2, "back": 1, "aicon": 3, "charname": 2, "vehname": 2}     # templates allowed beyond the table's count
STATE_OK = set(S.STATES) | {"next_race_menu", "please_wait"}


class Cand:
    """A capture as the source of one template: its picture, the ROI to cut and match."""

    def __init__(self, e, pic, roi, mod, kind):
        self.e, self.pic, self.roi, self.mod, self.kind = e, pic, roi, mod, kind
        self.tmpl = mod._feat(pic, roi, kind).astype(np.float32)
        self.region = mod._feat(pic, roi, kind, grow=S.SHIFT)

    def score(self, other: "Cand") -> float:
        """How well this capture's template matches the other capture (runtime matcher)."""
        if other.region.shape[0] < self.tmpl.shape[0] or other.region.shape[1] < self.tmpl.shape[1]:
            return -1.0
        return S._ncc_shift(other.region, self.tmpl)


def _spec_match(e, spec) -> bool:
    states, cursor, text = spec
    if states == "@dialog":
        states = S.DIALOG_STATES
    if isinstance(states, str):
        states = (states,)
    if e["state"] not in states:
        return False
    if cursor is not None and not _cur_eq(e.get("cursor"), cursor):
        return False
    return text is None or text in e.get("text", "")


def template_rows(a43: bool, cat):
    """[(kind, key, [(spec, roi_fn)], optional)] for one aspect: every row of TEMPLATES and
    the name labels. roi_fn(capture entry) -> ROI (or None when the capture cannot serve)."""
    mod = S4 if a43 else S
    rows = []
    for kind, key, legacy, spec in mod.TEMPLATES:
        roi = mod.ROIS[kind] if a43 else S._roi_for(kind, key)
        rows.append((kind, key, spec, (lambda e, roi=roi: roi), legacy is None))
    if not a43:
        for kind, d in S.NAME_REFS.items():
            for key in d:
                st = {"charname": "characters", "vehname": "vehicles", "cup": "courses_cups",
                      "banner": "courses_list"}[kind]
                cur = (key, None) if kind == "banner" else key
                roi = S._roi_for(kind, key)
                rows.append((kind, key, (st, cur, None), (lambda e, roi=roi: roi), False))
        return rows
    for kind, st, labels in (("charname", "characters", S4.CHAR_LABELS), ("vehname", "vehicles", S4.VEH_LABELS)):
        ids = cat.char_grid if kind == "charname" else cat.veh_grid
        for key in ids:
            def roi_fn(e, key=key, labels=labels):
                if e.get("tag") == "pilot" and kind == "charname":
                    return None           # offline (Multiplayer) the labels sit higher: online only
                c = e.get("cursor") or []
                return labels[0] if c and c[0] == key else labels[1] if len(c) > 1 and c[1] == key else None
            rows.append((kind, key, (st, None, None), roi_fn, False))

    def ok_roi(e):
        if e.get("tag") == "pilot":
            return None                   # online only (see above)
        c = e.get("cursor") or []
        if len(c) < 3 or not c[2] or c[0] not in cat.char_grid:
            return None
        return S4.char_cell(*cat.char_grid[c[0]])
    rows.append(("okbtn", "ok", ("characters", None, None), ok_roi, False))
    return rows


def select_sources(entries, a43: bool, cat, capdir: str, log):
    """Choose the capture each template is cut from. Per (kind, key): each table row takes,
    among the captures matching its spec, the one whose template matches (>= COVER) most
    of the group's captures not matched yet; then up to EXTRA more templates while some
    captures of the group are still not matched. Returns ([(kind, key, file, roi)], missing)."""
    mod = S4 if a43 else S
    aspect = aspect_of(a43)
    es = [e for e in entries if e["aspect"] == aspect]
    pics = {}

    def pic(e):
        if e["file"] not in pics:
            pics[e["file"]] = mod.picture_of(S.load_frame(os.path.join(capdir, e["file"])))
        return pics[e["file"]]
    groups = {}
    for row in template_rows(a43, cat):
        groups.setdefault((row[0], row[1]), []).append(row)
    out, missing, report = [], [], []
    fading = set()
    for (kind, key), rows in groups.items():
        cands_by_row = []
        allc = {}
        for kind_, key_, spec, roi_fn, optional in rows:
            cs = []
            for e in es:
                if not _spec_match(e, spec):
                    continue
                roi = roi_fn(e)
                if roi is None:
                    continue
                ck = (e["file"], tuple(roi))
                if ck not in allc:
                    allc[ck] = Cand(e, pic(e), roi, mod, kind)
                cs.append(allc[ck])
            cands_by_row.append((cs, optional, spec))
        allc = list(allc.values())
        if kind.startswith("t_") or kind in ("hint", "banner", "cup", "charname", "vehname"):
            # text still fading in (or in its dim colour phase): much weaker than the same text
            # on the other captures
            en = [float(c.tmpl.std()) for c in allc]
            top = max(en) if en else 0.0
            weak = {id(c) for c, v in zip(allc, en) if v < 0.35 * top}
            for c in allc:
                if id(c) in weak:
                    fading.add(c.e["file"])
            allc = [c for c in allc if id(c) not in weak]
            cands_by_row = [([c for c in cs if id(c) not in weak], o, sp) for cs, o, sp in cands_by_row]
        if not allc:
            if not all(opt for _c, opt, _s in cands_by_row):
                missing.append(f"{aspect} {kind}:{key} (needs {[s for _c, o, s in cands_by_row if not o]})")
            continue
        thr = COVER.get(kind, 0.90)
        sc = {}

        def covers(t):
            if id(t) not in sc:
                sc[id(t)] = {id(c) for c in allc if t.score(c) >= thr}
            return sc[id(t)]
        covered, chosen = set(), []
        for cs, optional, spec in cands_by_row:
            if not cs:
                if not optional:
                    missing.append(f"{aspect} {kind}:{key} (no capture shows {spec})")
                continue
            best = max(cs, key=lambda c: (len(covers(c) - covered), len(covers(c))))
            chosen.append(best)
            covered |= covers(best)
        for _ in range(EXTRA.get(kind, 1)):
            if len(covered) >= len(allc):
                break
            best = max(allc, key=lambda c: len(covers(c) - covered))
            if not covers(best) - covered:
                break
            chosen.append(best)
            covered |= covers(best)
        for c in chosen:
            out.append((kind, key, c.e["file"], tuple(int(v) for v in c.roi)))
        report.append(f"{kind}:{key} {len(chosen)} template(s) cover {len(covered)}/{len(allc)}")
    log(f"cal build {aspect}: {len(out)} templates from {len({f for _k, _y, f, _r in out})} captures; "
        f"{len(fading)} captures with text still fading in are left out")
    for r in report:
        a, b = r.split("cover ")[1].split("/")
        if a != b:
            log(f"cal build {aspect}: {r}")
    return out, missing, fading


def labels_from(entries, a43: bool, skip=()):
    """Offline-test labels (state, cursor) of the captures, for the template classifier."""
    aspect = aspect_of(a43)
    out = {}
    for e in entries:
        if e["aspect"] != aspect or e["state"] in TRANSIENT or e["file"] in skip:
            continue
        if a43 and e["state"] == "characters" and e.get("tag") == "pilot":
            continue    # the offline two-player Characters screen (labels elsewhere) is not driven
        lab = {"state": e["state"]}
        if e.get("cursor") is not None:
            lab["cursor"] = e["cursor"]
        out[e["file"]] = lab
    return out


def build(assets: str, log, cat=None) -> dict:
    """Select sources, build both banks into <assets>, write labels and sources, run the
    self-test. Returns a summary dict (missing templates, self-test misses)."""
    import offline_test
    cat = cat or driver.Catalog()
    capdir = os.path.join(assets, "captures")
    col_entries = []
    for line in open(os.path.join(capdir, "index.jsonl")):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if os.path.exists(os.path.join(capdir, e["file"])):
            col_entries.append(e)
    summary = {"missing": [], "captures": len(col_entries)}
    sources = {}
    for a43, mod, bank, lab in ((False, S, "templates.npz", "labels.json"), (True, S4, "templates43.npz", "labels43.json")):
        ents, missing, fading = select_sources(col_entries, a43, cat, capdir, log)
        summary["missing"] += missing
        sources[bank] = [{"kind": k, "key": key, "file": f, "roi": list(roi)} for k, key, f, roi in ents]
        out = os.path.join(assets, bank)
        n = mod.build_bank(refs_dir=capdir, out=out, entries=ents)
        log(f"cal build: wrote {n} templates to {out}")
        with open(os.path.join(assets, lab), "w") as f:
            json.dump({"_comment": "generated by nav/calibrate.py: OCR labels of captures/ (refs dir = captures)",
                       "labels": labels_from(col_entries, a43, fading)}, f, indent=0, sort_keys=True)
    with open(os.path.join(assets, "sources.json"), "w") as f:
        json.dump(sources, f, indent=1)
    S.use_bank(os.path.join(assets, "templates.npz"))
    S4.use_bank(os.path.join(assets, "templates43.npz"))
    rc = offline_test.run(refs=capdir, labels=os.path.join(assets, "labels.json"),
                          refs43=capdir, labels43=os.path.join(assets, "labels43.json"))
    summary["selftest_ok"] = rc == 0
    for m in summary["missing"]:
        log(f"cal build: MISSING {m}")
    return summary


# ====================================================================== minimap icons
ICON_R = 17                       # party/cmd/fakephones/minimap.go iconR: icons are 35x35
MM_BOX = (860, 220, 1260, 660)    # minimap in 16:9 picture coordinates (minimap.go)


def _conv(a, k):
    H, W = a.shape
    kh, kw = k.shape
    F = np.fft.rfft2(a, (H + kh, W + kw))
    G = np.fft.rfft2(k, (H + kh, W + kw))
    c = np.fft.irfft2(F * G, (H + kh, W + kw))
    return c[kh // 2:kh // 2 + H, kw // 2:kw // 2 + W]


def _ring(r0, r1):
    y, x = np.mgrid[-r1:r1 + 1, -r1:r1 + 1]
    d = np.hypot(x, y)
    k = ((d >= r0) & (d <= r1)).astype(float)
    return k / k.sum()


def own_icon(mm):
    """Centre of the player's own minimap icon: the large face with a dark outline of
    radius ~22 px and the yellow view cone next to it. Returns (x, y, score) or None."""
    m = mm.astype(np.int16)
    dark = (m.max(axis=2) < 70).astype(float)
    yel = ((m[..., 0] > 180) & (m[..., 1] > 150) & (m[..., 2] < 120) & (m[..., 0] - m[..., 2] > 90)).astype(float)
    if yel.sum() < 150:
        return None
    s = _conv(dark, _ring(20, 24)) - _conv(dark, _ring(0, 16)) + 0.5 * _conv(yel, _ring(25, 55))
    b = 26
    s[:b] = s[-b:] = -9
    s[:, :b] = s[:, -b:] = -9
    y, x = np.unravel_index(int(np.argmax(s)), s.shape)
    return (int(x), int(y), float(s[y, x])) if s[y, x] > 0.4 else None


def _icon_ncc(a, b):
    yy, xx = np.mgrid[-ICON_R:ICON_R + 1, -ICON_R:ICON_R + 1]
    msk = np.hypot(xx, yy) <= ICON_R
    a = a[msk].astype(float)
    b = b[msk].astype(float)
    a -= a.mean()
    b -= b.mean()
    return float((a * b).sum() / np.sqrt((a * a).sum() * (b * b).sum() + 1e-6))


def make_icons(frames_by_char, out_dir: str, log) -> list:
    """icons/<character>.png (35x35) for the test phones, from race frames of a 16:9
    instance driving that character: the own icon is located in each frame and the crop
    that agrees best with all the others is kept."""
    from PIL import Image
    os.makedirs(out_dir, exist_ok=True)
    made = []
    x0, y0, x1, y1 = MM_BOX
    for ch, frames in frames_by_char.items():
        crops = []
        for f in frames:
            mm = S.picture_of(f)[y0:y1, x0:x1]
            hit = own_icon(mm)
            if hit is None:
                continue
            x, y, _s = hit
            crops.append(mm[y - ICON_R:y + ICON_R + 1, x - ICON_R:x + ICON_R + 1])
        if len(crops) < 5:
            log(f"cal icons: {ch}: only {len(crops)} usable race frames; no icon")
            continue
        med = [float(np.median([_icon_ncc(c, o) for o in crops])) for c in crops]
        i = int(np.argmax(med))
        if med[i] < 0.85:
            log(f"cal icons: {ch}: the crops disagree (best median {med[i]:.2f}); no icon")
            continue
        Image.fromarray(np.ascontiguousarray(crops[i])).save(os.path.join(out_dir, f"{ch}.png"))
        made.append(ch)
        log(f"cal icons: {ch}.png from {len(crops)} race frames (agreement {med[i]:.2f})")
    return made


# ====================================================================== the whole run
FIRSTBOOT = [("create_license", None), ("wfc_notice", None), ("permit_prompt", None), ("wont_play", None),
             ("choose_mii", "p1"), ("controller_reg", "wait"), ("error_dialog", None)]


def firstboot_missing(col: Collector):
    return [f"{st}{'' if c is None else '[' + c + ']'}" for st, c in FIRSTBOOT if not col.have("4x3", st, c)]


def ocr_factory(setup: bool = False):
    return lambda k: ocrscreen.OcrScreen(dolphin.Instance(k, setup=setup).aspect43)


def sh(log, *args) -> int:
    r = subprocess.run(list(args), capture_output=True, text=True, timeout=120)
    for line in (r.stdout + r.stderr).strip().splitlines():
        log(f"{os.path.basename(args[0])}: {line}")
    return r.returncode


def boot(k: int, log, setup: bool = False) -> dolphin.Instance:
    """A freshly booted instance k (stopped first if it runs), through the project's scripts."""
    inst = dolphin.Instance(k, setup=setup)
    if inst.alive():
        inst.stop(log)
    if sh(log, os.path.join(RUN, "provision.sh"), str(k), inst.mode) != 0:
        raise Stop(f"provision.sh {k} failed")
    if not inst.launch(log):
        raise Stop(f"instance {k}: launch failed (see run/logs/i{k}.log)")
    return inst


def scratch_firstboot(col: Collector, log, run_id: str, timeout: float = 480.0) -> bool:
    """The first-boot screens (new license, data notice, Permit / Prohibit, the second
    player) on a scratch profile instances/i13 with a fresh save, deleted afterwards."""
    k = SCRATCH_K
    d = os.path.join(dolphin.INST_ROOT, f"i{k}")
    mark = os.path.join(d, "mkparty-calibration-scratch")
    if os.path.exists(d) and not os.path.exists(mark):
        raise Stop(f"{d} exists and is not a calibration scratch profile; move it away")
    if os.path.exists(d):
        dolphin.Instance(k).stop(log)
        shutil.rmtree(d)
    os.makedirs(d)
    open(mark, "w").write("scratch profile of nav/calibrate.py (first-boot screens); deleted afterwards\n")
    log(f"cal first-boot: scratch profile i{k} (fresh save)")
    try:
        boot(k, log, setup=True)
        r = driver.Runner([k], log, run_id, setup=True, classifier_factory=ocr_factory(True),
                          collector=col.worker_hook)
        w = r.workers[k]
        w.linger = dict(LINGER)
        w.explore_wont_play = True
        r.start()
        ok = r.prepare(timeout)
        r.stop()
        return ok
    finally:
        dolphin.Instance(k).stop(log)
        if os.path.exists(mark):
            shutil.rmtree(d, ignore_errors=True)


def run(assets: str, log, rounds: int = 3, keep: bool = False, firstboot: bool = True) -> int:
    """`nav.py calibrate`: capture (first-boot screens if needed, the menu tours, the online
    rounds), then build and check the banks. 0 = assets complete and self-test passed."""
    t0 = time.time()
    run_id = f"calibrate-{time.strftime('%Y%m%d-%H%M%S')}"
    capdir = os.path.join(assets, "captures")
    if not keep and os.path.isdir(capdir):
        shutil.rmtree(capdir)             # captures of an earlier calibration (generated here)
    col = Collector(assets, log)
    ocrscreen.warm_up(log)
    times = {}
    if firstboot:
        miss = firstboot_missing(col)
        if miss:
            t = time.time()
            log(f"cal: first-boot screens not captured yet ({', '.join(miss)})")
            if not scratch_firstboot(col, log, run_id):
                log("cal: the scratch profile did not reach the Nintendo WFC menu")
            times["first-boot"] = time.time() - t
    t = time.time()
    cat = driver.Catalog()
    errors = []

    def tour(k, fn):
        try:
            boot(k, log)
            p = Pilot(k, log, col)
            p.deadline = time.time() + 900
            fn(p, cat)
            p.cls.ocr.close()
        except Exception as e:  # reported; the build says what is missing
            errors.append(f"i{k}: {type(e).__name__}: {e}")
            log(f"cal: tour on i{k} stopped: {type(e).__name__}: {e}")
    th = [threading.Thread(target=tour, args=(1, tour_1p)), threading.Thread(target=tour, args=(7, tour_2p))]
    for x in th:
        x.start()
        time.sleep(3)
    for x in th:
        x.join()
    times["menu tours"] = time.time() - t
    t = time.time()
    icons = {}
    try:
        icons = online_rounds(log, col, run_id, ks=(1, 7), rounds=rounds)
    except Exception as e:
        errors.append(f"online: {type(e).__name__}: {e}")
        log(f"cal: online rounds stopped: {type(e).__name__}: {e}")
    times["online rounds"] = time.time() - t
    for k in (1, 7):
        dolphin.Instance(k).stop(log)
    col.flush()
    made = make_icons(icons, os.path.join(assets, "icons"), log)
    t = time.time()
    summary = build(assets, log, cat)
    times["build + check"] = time.time() - t
    summary.update({"errors": errors, "icons": made, "seconds": {k: round(v) for k, v in times.items()},
                    "total_seconds": round(time.time() - t0), "time": time.strftime("%Y-%m-%d %H:%M:%S")})
    with open(os.path.join(assets, "calibration.json"), "w") as f:
        json.dump(summary, f, indent=1)
    ok = not summary["missing"] and summary["selftest_ok"]
    log(f"cal: {'DONE' if ok else 'INCOMPLETE'} after {time.time() - t0:.0f} s ({summary['captures']} captures, "
        f"missing {len(summary['missing'])}, self-test {'passed' if summary['selftest_ok'] else 'FAILED'}, "
        f"icons {made}); {assets}")
    return 0 if ok else 1


def _cli():
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("what")
    ap.add_argument("k", type=int)
    ap.add_argument("--assets", default=localconf.assets_dir())
    ap.add_argument("--rounds", type=int, default=2)
    a = ap.parse_args()
    log = driver.Log(os.path.join(driver.LOG_DIR, f"nav-caltest-{time.strftime('%Y%m%d-%H%M%S')}.log"))
    col = Collector(a.assets, log)
    cat = driver.Catalog()
    t0 = time.time()
    if a.what == "online":
        subprocess.run([os.path.join(ROOT, "server", "start.sh")], check=True)
        icons = online_rounds(log, col, "caltest-online", ks=(1, a.k), rounds=a.rounds)
        log(f"icons frames: { {c: len(v) for c, v in icons.items()} }")
        import pickle
        with open(os.path.join(a.assets, "icon_frames.pkl"), "wb") as f:
            pickle.dump(icons, f)
        return
    p = Pilot(a.k, log, col)
    try:
        {"tour1p": tour_1p, "tour2p": tour_2p}[a.what](p, cat)
    finally:
        col.flush()
        log(f"{a.what} finished after {time.time() - t0:.0f} s; {col.n} captures")


if __name__ == "__main__":
    _cli()
