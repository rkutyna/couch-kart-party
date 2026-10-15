"""Closed-loop driving: one Worker thread per Dolphin instance, one Runner on top.

A Worker loops: capture -> classify -> debounce -> (check the pending action |
plan one action). It acts only on a STABLE observation: the same (state,
cursor) seen on STABLE_N consecutive captures. After every press it waits for
the stable observation to change; if nothing changed within the action's
timeout the same press is retried (bounded), and only while the screen is still
exactly the one it was issued on. Nothing is pressed on a timer, with two
documented exceptions that are still gated on the observed screen (the optional
WFC-menu keep-alive, and B / A nudges after a long stall).

A Worker drives one instance: a single-player one (16:9, screen.py, one player on pad
p1) or a two-player split-screen one (4:3, screen43.py, player A on pad p1 and player
B on pad p2; both pick characters / vehicles / drift on the same screens, the room
screens are driven with pad p1). The Runner owns the instance pool: which instances
run, which slots race on which instance (POST /api/nav/assign), and the host's audio.

What a Worker does with a screen depends on its Goal:
  wfc    reach "Nintendo WFC (1P)" with Worldwide highlighted and stay there
  setup  go on to the Drift screen with the wanted character / vehicle / drift
         and hold there until goal.go, then confirm and play the room screens
         (Check Members, vote) up to Course Poll, where the slot is handed over
  watch  the player has control; only the room screens of the NEXT race are
         driven (after taking navControl), then the slot is handed back
"""
from __future__ import annotations

import json
import os
import queue
import subprocess
import threading
import time
from dataclasses import dataclass, replace

import dolphin
import lobby
import screen
import screen43

NAV_DIR = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(NAV_DIR)
CATALOG = os.path.join(ROOT, "party", "web", "catalog.json")
LOG_DIR = os.path.join(NAV_DIR, "logs")
UNKNOWN_DIR = os.path.join(LOG_DIR, "unknown")

MIN_FRESH = 3              # fresh frames needed after a press before it may be repeated
STABLE_N = 2               # consecutive agreeing observations before acting
MAX_TRIES = 3              # presses of one action before it is reported stuck
T_PRESS = 2.5              # wait for a screen change after A / B
T_MOVE = 1.0               # wait for the cursor to move after a flick
T_CHECK = 0.33             # repeat interval of A on Check Members
T_FAST = 1.0               # A on Check Members / vote dialog
TRANSITION_MAX = 20.0      # black / unknown after a press may last this long
STUCK_COOLDOWN = 1.5
UNKNOWN_SAVE_AFTER = 3.0
MAX_UNKNOWN_SAVES = 40
WFC_SETTLE = 1.5           # the WFC menu must hold this long to count as reached
STALL_NUDGE = 45.0         # no progress this long (not at the goal): press B
STALL_RELAUNCH = 150.0     # ... this long: relaunch the instance
SEARCH_PATIENCE = 150.0    # searching this long: back out and search again
REJOIN_WAIT = float(os.environ.get("NAV_REJOIN_WAIT", "8"))  #         # seconds between a disconnect dialog and the next connect
FROZEN_AFTER = 20.0        # s of a stopped process (low CPU, no new picture) before it counts as frozen
# (6 s was too short: with 6 instances booting or entering new screens at once, Dolphins wait
# several seconds for Metal shader compiles at ~2 % CPU; two healthy instances were relaunched.)
RACE_SHOT_EVERY = 30.0     # seconds between evidence captures while a race is on (captures are ~3 MB PNGs)
DEAD_AFTER = 20.0          # no capture this long while the window is gone: relaunch
BOOT_GRACE = 60.0          # s after a launch before a missing window counts (boot storms are slow)
MAX_RELAUNCH = 3           # relaunches within RELAUNCH_WINDOW before the instance is given up
RELAUNCH_WINDOW = 600.0

READY_MSG = "Ready to start."
GO_STAGGER = float(os.environ.get("NAV_GO_STAGGER", "3"))
LIVE_GRACE = 4.0           # s after the first hand-over before going live without the rest
ENTER_STAGGER = float(os.environ.get("NAV_ENTER_STAGGER", "5"))  # s between instances entering Worldwide VS
ENTER_STAGGER_1P = float(os.environ.get("NAV_ENTER_STAGGER_1P", "3"))  # ... when all are single-player
RACE_OVER_STATES = {"results", "next_race_menu", "please_wait", "preparing", "check_members", "vote_prompt"}
VOTE_INITIAL = float(os.environ.get("NAV_VOTE_INITIAL", "40"))   # s from race end to the latest vote entry
VOTE_WAIT_AT_PROMPT = float(os.environ.get("NAV_VOTE_WAIT", "16"))  # s after the vote dialog appears
# Measured 2026-10-14: with no input the game moved on from the vote dialog / Courses to
# Course Poll by itself 28.5 s and 29.4 s after the dialog appeared (one instance only
# after 56.7 s). Entering a vote takes 1-6 s (5.7 s from the deadline to Course Poll for
# Toad's Factory), so 16 s leaves a margin of about 6 s.
ROOM_STATES = {"check_members", "vote_prompt", "courses_cups", "courses_list", "course_poll"}
TRANSIENT = {"unknown", "black"}
RACE_STICKY = 1.5          # s of other observations before a stable "race" changes (HUD-less frames)
MEMBERS_N = 2              # Check Members rows must be counted the same this many times in a row
BOOT_STATES = {None, "black", "unknown", "health", "title"}
SHOT_ON_ENTER = {"check_members", "vote_prompt", "courses_cups", "courses_list", "course_poll",
                 "disconnected_dialog", "error_dialog", "emu_alert", "opponents_chosen", "results",
                 "welcome", "searching", "preparing"}


def now_t() -> float:
    return time.time()


def now_s() -> str:
    t = time.time()
    return time.strftime("%H:%M:%S", time.localtime(t)) + f".{int((t % 1) * 1000):03d}"


class Log:
    """Line log with millisecond timestamps (file + stdout)."""

    def __init__(self, path: str, echo: bool = True):
        self.path = path
        os.makedirs(os.path.dirname(path), exist_ok=True)
        self._f = open(path, "a", buffering=1)
        self._lock = threading.Lock()
        self.echo = echo

    def __call__(self, msg: str):
        line = f"{now_s()} {msg}"
        with self._lock:
            if self.echo:
                print(line, flush=True)
            self._f.write(line + "\n")


class Catalog:
    """Menu positions from party/web/catalog.json (the source of truth for moves)."""

    def __init__(self, path: str = CATALOG):
        with open(path) as f:
            d = json.load(f)
        self.char_grid = {c["id"]: tuple(c["grid"]) for c in d["characters"] if "grid" in c}
        self.char_weight = {c["id"]: c["weight"] for c in d["characters"]}
        self.veh_grid = {v["id"]: tuple(v["grid"]) for v in d["vehicles"] if "grid" in v}
        self.veh_weight = {v["id"]: v["weight"] for v in d["vehicles"]}
        self.cup_grid = {c["id"]: tuple(c["grid"]) for c in d["cups"]}
        self.cup_unlocked = {c["id"]: bool(c.get("unlocked")) for c in d["cups"]}
        self.course_cup, self.course_row = {}, {}
        for cup in d["cups"]:
            for co in cup["courses"]:
                self.course_cup[co["id"]] = cup["id"]
                self.course_row[co["id"]] = int(co["row"])
        self.names = {c["id"]: c["name"] for c in d["characters"]}
        self.names.update({v["id"]: v["name"] for v in d["vehicles"]})
        self.names.update({co["id"]: co["name"] for cup in d["cups"] for co in cup["courses"]})


def vstep(cur: int, want: int, rows: int) -> str:
    """UP or DOWN, the shorter way round on a column that wraps."""
    down = (want - cur) % rows
    return "DOWN" if down <= rows - down else "UP"


@dataclass(frozen=True)
class Goal:
    mode: str = "wfc"              # wfc | setup | watch
    round: int = 0
    character: str | None = None   # player A (pad p1)
    vehicle: str | None = None
    drift: str = "auto"
    vote: str = "random"
    go: bool = False
    keepalive: float = 0.0         # seconds between keep-alive moves on the WFC menu (0 = off)
    vote_locked: bool = False      # the phone has locked in its vote for the next race
    connected: bool = True         # the phone is connected
    slots: tuple = ()              # lobby slots on this instance: (A,) or (A, B)
    # player B (pad p2) of a two-player instance
    character_b: str | None = None
    vehicle_b: str | None = None
    drift_b: str = "auto"
    vote_b: str = "random"
    vote_locked_b: bool = False
    connected_b: bool = True
    # setup: hold at the WFC menu until every instance of the round is there. The game
    # confirms character / vehicle / drift by itself about 25 s after VS is chosen
    # (measured 2026-10-14: every instance left Drift 25.3-26 s after A on VS), so the
    # instances must enter Worldwide VS together.
    enter: bool = True


@dataclass
class Action:
    name: str
    button: str | None = None
    move: str | None = None
    timeout: float = T_PRESS
    key0: tuple = (None, None)
    issued: float = 0.0
    tries: int = 1
    max_tries: int = MAX_TRIES
    fresh: int = 0                 # fresh frames seen since the press
    pad: int = 1                   # controller: 1 = player A, 2 = player B (two-player instance)
    moves: tuple = ()              # several pads at once: ((pad, direction), ...)
    presses: tuple = ()            # several pads one after the other: ((pad, button), ...)
    cursor_ok: bool = False        # a button whose effect is a cursor change (two-player OK marks)


class Hooks:
    """What a Worker reports to the outside (the lobby), for the slots on its instance.
    Default: nothing."""

    def status(self, slots, text: str): ...
    def take_control(self, slots) -> bool: return True
    def handover(self, slots) -> bool: return True
    def vote_window(self, slots, open_: bool, deadline: float = 0.0, new: bool = True) -> bool: return True


class Saver(threading.Thread):
    """Writes PNG files off the worker threads."""

    def __init__(self):
        super().__init__(daemon=True)
        self.q = queue.Queue()

    def save(self, frame, path: str):
        self.q.put((frame, path))

    def run(self):
        while True:
            frame, path = self.q.get()
            try:
                dolphin.Instance.save_png(frame, path)
            except Exception:
                pass
            self.q.task_done()


class Worker(threading.Thread):
    def __init__(self, k: int, log: Log, cat: Catalog, saver: Saver, run_id: str, hooks: Hooks | None = None,
                 setup: bool = False, classifier=None, collector=None):
        super().__init__(daemon=True, name=f"worker-i{k}")
        self.k = k
        self.inst = dolphin.Instance(k, setup=setup)
        self.two = self.inst.mode == "2p"
        # classifier: an object with classify_frame(frame) (default: the template classifier
        # of the instance's aspect; calibration passes ocrscreen.OcrScreen). collector:
        # called with (worker, frame, result, time) after every observation (calibration).
        self.cls = classifier or (screen43 if self.inst.aspect43 else screen)
        self.collector = collector
        self.linger = {}                # state -> s to stay on it before acting (calibration)
        self.explore_wont_play = False  # setup / calibration: visit "Prohibit" -> No once
        self.hold_pick = False          # calibration: on the targets, do not press A (yet)
        self.explored = set()
        self.log = log
        self.cat = cat
        self.saver = saver
        self.hooks = hooks or Hooks()
        self.run_id = run_id
        self.shots_dir = os.path.join(LOG_DIR, "shots", run_id)
        self.obs = open(os.path.join(LOG_DIR, f"nav-{run_id}-obs-i{k}.log"), "a", buffering=1)
        self.goal = Goal()
        self._stop = False
        # observation
        self.frame = None
        self.raw = None                 # last Result
        self.raw_key = (None, None)
        self.raw_n = 0
        self.raw_first = 0.0            # when the current run of identical observations began
        self.stable = (None, None)      # debounced (state, cursor)
        self.stable_since = 0.0
        self.prev_known = None          # last stable state that was not transient
        self.last_obs_t = 0.0
        self.max_gap = 0.0
        self.n_obs = 0
        self.unknown_since = None
        self.unknown_saved = False
        self.n_unknown_saved = 0
        # action
        self.pending: Action | None = None
        self.cooldown_until = 0.0
        self.last_progress = time.time()
        self.last_nudge = 0.0
        self.last_keepalive = time.time()
        # round flags (read by the Runner)
        self.drift_ready = False
        self.handed = False             # handed over at Course Poll in this round
        self.lost = False               # out of the room in this round (disconnect, crash)
        self.failed = False             # could not be recovered
        self.nav_on = False             # we hold navControl (watch mode)
        self.poll_visit_done = False
        self.members = None             # last Check Members player count
        self.t_handover = None
        self.relaunches = 0
        self.status_text = None
        self.shot_plan = []             # [(time, name)] delayed shots
        self.timings = []               # (state, seconds from first sight to the press)
        self.conn_errors = 0            # error dialogs since the WFC menu was last reached
        self.emu_errors = 0             # emulator error lines seen in Dolphin's log
        self.left_room_at = 0.0         # when a disconnect / error dialog was last seen
        self.vote_open = False          # the phone's "vote for the next race" picker is open
        self.vote_opened_at = 0.0
        self.vote_deadline = 0.0        # epoch s; the navigator enters the vote by then at the latest
        self.prompt_at = 0.0            # when the vote dialog of this race appeared
        self.vote_entered_log = None
        self.random_at = 0.0            # when A on Random was pressed (Courses flashes behind it)
        self.last_vote = {}             # player index -> the vote its phone made while connected
        self.members_run = (None, 0)    # (count, consecutive observations) on Check Members
        self.races = 0                  # course votes entered (two-player: whose vote alternates)
        self.vote_by = None             # two-player: which player's vote was entered last

    # ------------------------------------------------------------ control
    def set_goal(self, g: Goal):
        old = self.goal
        # partyd frees a slot 20 s after its phone disconnects and resets its vote to
        # "random"; for a phone that is gone the last vote it made is entered.
        if g.connected and g.vote:
            self.last_vote[0] = g.vote
        if g.connected_b and g.vote_b and len(g.slots) > 1:
            self.last_vote[1] = g.vote_b
        if g.round != old.round or (g.mode == "wfc" and old.mode != "wfc"):
            self.drift_ready = False
            self.handed = False
            self.lost = False
            self.poll_visit_done = False
            self.members = None
            self.t_handover = None
            self.last_progress = time.time()
            # a vote window of an earlier round is over (partyd's reset cleared it)
            self.vote_open = False
            self.prompt_at = 0.0
        if g != old:
            self.goal = g

    def stop(self):
        self._stop = True

    @property
    def state(self):
        return self.stable[0]

    @property
    def slots(self) -> tuple:
        return self.goal.slots

    @property
    def at_wfc(self) -> bool:
        return (self.stable == ("wfc_menu", "worldwide") and self.pending is None
                and time.time() - self.stable_since >= WFC_SETTLE)

    def mode(self) -> str:
        if self.lost:
            return "wfc"
        if self.goal.mode == "setup" and self.handed:
            return "watch"
        if self.goal.mode == "setup" and not self.goal.enter:
            return "wfc"                  # wait at the WFC menu for the others (Goal.enter)
        return self.goal.mode

    # ------------------------------------------------------------ loop
    def run(self):
        while not self._stop:
            try:
                self.tick()
            except Exception as e:  # a worker must never die silently
                self.log(f"i{self.k} ERROR in worker: {type(e).__name__}: {e}")
                time.sleep(1.0)

    def tick(self):
        if self.failed:
            time.sleep(1.0)
            return
        t0 = time.time()
        frame = self.inst.grab() if self.inst.alive() else None
        if frame is None:
            self._no_frame()
            return
        self.no_frame_since = None
        res = self.cls.classify_frame(frame)
        t1 = time.time()
        self.frame = frame
        if self.last_obs_t:
            self.max_gap = max(self.max_gap, t1 - self.last_obs_t)
        self.last_obs_t = t1
        self.n_obs += 1
        self.obs.write(f"{now_s()} {res.state} cur={res.cursor} conf={res.conf:.2f} how={res.how} "
                       f"cap+cls={1000 * (t1 - t0):.0f}ms src={self.inst.source}\n")
        if self.inst.source in ("stream", "screencapture"):
            self.last_fresh_t = t1
            if self.pending is not None:
                self.pending.fresh += 1
        self._observe(res, t1)
        if self.collector is not None:
            self.collector(self, frame, res, t1)
        self._scan_dolphin_log(t1)
        if self._emulation_dead(res, t1):
            return
        if res.state == "race" and self.mode() == "watch" and t1 - self.last_race_shot > RACE_SHOT_EVERY:
            self.last_race_shot = t1
            self.shot("race")
        self._shots(t1)
        self._step(t1)
        time.sleep(0.01)

    no_frame_since = None
    last_relaunch = 0.0
    dead_check_at = 0.0
    last_fresh_t = 0.0
    last_race_shot = 0.0
    dlog_pos = None
    dlog_at = 0.0

    def _scan_dolphin_log(self, now):
        """Dolphin's console log (run/logs/i<k>.log): with UsePanicHandlers = False a game
        crash such as "Invalid read from 0x218" is only an error line there. Note each one."""
        if now < self.dlog_at:
            return
        self.dlog_at = now + 2.0
        path = os.path.join(dolphin.RUN_DIR, "logs", f"i{self.k}.log")
        try:
            size = os.path.getsize(path)
            if self.dlog_pos is None or size < self.dlog_pos:
                self.dlog_pos = size if self.dlog_pos is None else 0
            if size == self.dlog_pos:
                return
            with open(path, "rb") as f:
                f.seek(self.dlog_pos)
                chunk = f.read(200000).decode("utf-8", "replace")
            self.dlog_pos = size
        except OSError:
            return
        for line in chunk.splitlines():
            if any(w in line for w in ("Invalid read", "Invalid write", "Unknown instruction", "IntCPU", "Panic")):
                self.emu_errors += 1
                self.log(f"i{self.k} DOLPHIN LOG (screen {self.stable[0]}): {line.strip()[:220]}")

    low_cpu_since = None
    cpu_check_at = 0.0

    def _frozen(self, now) -> bool:
        """Emulation has stopped (crash behind an alert, hang, SIGSTOP): the Dolphin process
        idles below 4 % CPU (a running game uses 15-30 % even on a static menu). The picture
        alone cannot tell: the window keeps being captured with its last image."""
        if now < self.cpu_check_at:
            return False
        self.cpu_check_at = now + 3.0
        c = self.inst.cpu()
        if c < 0 or c > 4.0 or now - self.last_fresh_t < 10.0:
            self.low_cpu_since = None     # running, or the picture still changes
            return False
        if self.low_cpu_since is None:
            self.low_cpu_since = now
            return False
        if now - self.low_cpu_since < FROZEN_AFTER:
            return False
        self.low_cpu_since = None
        self.shot("frozen")
        self._recover(f"Dolphin idle at {c:.1f} % CPU for {FROZEN_AFTER:.0f}+ s on {self.stable[0]}: emulation stopped")
        return True

    def _emulation_dead(self, res, now) -> bool:
        if self._frozen(now):
            return True
        """A black / frozen picture for a few seconds: look for Dolphin's alert panel (a game
        crash, e.g. "Invalid read from 0x218": emulation is paused behind it and the process
        idles at about 1 % CPU). Relaunch when found."""
        if res.state not in ("black", "unknown") or self.stable[0] not in ("black", "unknown"):
            return False
        if now - self.stable_since < 3.0 or now < self.dead_check_at:
            return False
        self.dead_check_at = now + 2.0
        wid = self.inst.alert_window()
        if wid is None:
            return False
        cpu = self.inst.cpu()
        self.shot("emu_alert_black")
        self._recover(f"Dolphin alert panel (window {wid}) with a {res.state} picture, process at {cpu:.0f} % CPU: the game crashed")
        return True

    def _no_frame(self):
        now = time.time()
        if self.no_frame_since is None:
            self.no_frame_since = now
            self.log(f"i{self.k} no capture: {self.inst.last_err or 'process not running'}")
        gone = now - self.no_frame_since
        if not self.inst.alive():
            if gone > 1.0:
                self._recover("Dolphin process is not running")
            else:
                time.sleep(0.25)
            return
        if gone > DEAD_AFTER and now - self.inst.launched_at > BOOT_GRACE and self.inst.wid is None \
                and self.inst.refresh_window() is False:
            self._recover(f"game window missing for {gone:.0f} s (emulation dead?)")
            return
        time.sleep(0.25)

    def _recover(self, why: str):
        """Stop this instance's Dolphin (recorded pid only) and launch it again."""
        self.no_frame_since = None
        if self.relaunches and time.time() - self.last_relaunch > RELAUNCH_WINDOW:
            self.relaunches = 0           # the earlier trouble is long past
        if self.relaunches >= MAX_RELAUNCH:
            self.failed = True
            self.lost = True
            self._status("Game crashed and could not be restarted.")
            self.log(f"i{self.k} FAILED: {why}; gave up after {self.relaunches} relaunches")
            return
        self.relaunches += 1
        self.last_relaunch = time.time()
        self.log(f"i{self.k} RECOVER ({why}): relaunch {self.relaunches}/{MAX_RELAUNCH}")
        self._status("Game restarted. Rejoining...")
        if self.mode() == "watch" or (self.goal.mode == "setup" and self.goal.go and self.state in ROOM_STATES):
            self.lost = True      # it cannot get back into the running race
        self.pending = None
        self.stable = (None, None)
        self.raw_key = (None, None)
        self.prev_known = None
        self.drift_ready = False
        ok = self.inst.relaunch(self.log)
        self.last_progress = time.time()
        self.log(f"i{self.k} relaunch {'ok: pid %s window %s' % (self.inst.pid, self.inst.wid) if ok else 'FAILED'}")

    # ------------------------------------------------------------ observation
    def _observe(self, res, now: float):
        key = (res.state, tuple(res.cursor) if isinstance(res.cursor, list) else res.cursor)
        if key == self.raw_key:
            self.raw_n += 1
        else:
            self.raw_key = key
            self.raw_n = 1
            self.raw_first = now
        self.raw = res
        if res.state == "check_members" and res.detail.get("members"):
            # Rows slide in and the flag animates: count only what is seen MEMBERS_N times in
            # a row (a single frame once counted 4 rows for 3 players).
            m = res.detail["members"]
            run = self.members_run
            self.members_run = (m, run[1] + 1) if run[0] == m else (m, 1)
            if self.members_run[1] >= MEMBERS_N:
                self.members = max(self.members or 0, m)
        need = STABLE_N
        if self.stable[0] == "race" and key[0] in TRANSIENT:
            # Frames without the HUD (item flashes, dark tunnels) classify as unknown: a race
            # stays a race unless something else holds for RACE_STICKY seconds.
            need = max(STABLE_N, 1) if now - self.raw_first >= RACE_STICKY else 10 ** 9
        if self.raw_n >= need and key != self.stable:
            old = self.stable
            self.stable = key
            self.stable_since = self.raw_first
            self.last_progress = now
            self.log(f"i{self.k} {old[0]}{'' if old[1] is None else '[%s]' % (old[1],)} -> "
                     f"{key[0]}{'' if key[1] is None else '[%s]' % (key[1],)} ({res.conf:.2f} {res.how})")
            if key[0] != old[0]:
                self._entered(key[0], old[0], now)
            if key[0] not in TRANSIENT:
                self.prev_known = key[0]
        # unknown capture collection
        if res.state == "unknown":
            if self.unknown_since is None:
                self.unknown_since = now
                self.unknown_saved = False
            elif (now - self.unknown_since > UNKNOWN_SAVE_AFTER and not self.unknown_saved
                  and self.n_unknown_saved < MAX_UNKNOWN_SAVES):
                self.unknown_saved = True
                self.n_unknown_saved += 1
                path = os.path.join(UNKNOWN_DIR, f"{self.run_id}_{time.strftime('%H%M%S')}_i{self.k}.png")
                self.saver.save(self.frame, path)
                self.log(f"i{self.k} unknown for {now - self.unknown_since:.1f} s ({res.how}); saved {path}")
        else:
            self.unknown_since = None

    def _entered(self, st, old, now):
        if st in ("error_dialog", "disconnected_dialog"):
            self.left_room_at = now
        if st == "error_dialog":
            self.conn_errors += 1
            self.log(f"i{self.k} error dialog #{self.conn_errors} since the last time online")
        elif st in ("wfc_menu", "searching", "check_members"):
            self.conn_errors = 0
        if st == "race":
            self.boot_demo = self.prev_known in ("title", "health")   # not None: a navigator started mid-race
        if st in SHOT_ON_ENTER:
            self.shot(f"enter_{st}")
        if st not in TRANSIENT:
            self.two_done = {}            # a new screen: the two-player A presses start again
        if st == "check_members":
            self.shot_plan.append((now + 0.6, "check_members_b"))
        if st in ("searching", "opponents_chosen", "next_race_menu"):
            self.members = None           # a new room / race: count the members again
            self.members_run = (None, 0)
        if st == "course_poll":
            self.shot_plan += [(now + 2.0, "poll_2s"), (now + 5.0, "poll_5s"), (now + 9.0, "poll_9s"),
                               (now + 14.0, "poll_14s"), (now + 20.0, "poll_20s")]
        elif st not in TRANSIENT:         # a glitch frame on Course Poll is not a new visit
            self.poll_visit_done = False
        if st not in ROOM_STATES and st not in TRANSIENT:
            self.shot_plan = [p for p in self.shot_plan if not p[1].startswith("poll")]

    def shot(self, name: str):
        if self.frame is None:
            return
        path = os.path.join(self.shots_dir, f"{time.strftime('%H%M%S')}_{int((time.time() % 1) * 1000):03d}_i{self.k}_{name}.png")
        self.saver.save(self.frame, path)

    def _shots(self, now: float):
        if self.shot_plan and now >= self.shot_plan[0][0]:
            _, name = self.shot_plan.pop(0)
            self.shot(name)

    # ------------------------------------------------------------ acting
    def _issue(self, a: Action):
        a.key0 = self.stable
        a.issued = time.time()
        seen = a.issued - self.stable_since
        self.log(f"i{self.k} ACT {a.name}{'' if a.tries == 1 else ' (try %d/%d)' % (a.tries, a.max_tries)} "
                 f"[{self.stable[0]} seen {seen:.2f} s ago]")
        if a.tries == 1 and self.stable[0] in ("check_members", "vote_prompt", "courses_cups", "courses_list"):
            self.timings.append((self.stable[0], seen))
        try:
            if a.presses:
                for i, (pad, b) in enumerate(a.presses):
                    if i:
                        time.sleep(0.15)
                    self.inst.press(b, pad)
            elif a.button:
                self.inst.press(a.button, a.pad)
            elif a.moves:
                self.inst.move_pads(a.moves)
            elif a.move:
                self.inst.move(a.move, a.pad)
        except OSError as e:
            self.log(f"i{self.k} controller write failed: {e} (is Dolphin reading the pipe?)")
        self.pending = a

    def _settle(self, now: float) -> bool:
        """True when a new action may be planned."""
        p = self.pending
        if p is None:
            return now >= self.cooldown_until
        changed = self.stable != p.key0
        if changed and p.button and not p.cursor_ok and self.stable[0] == p.key0[0] and now - p.issued < p.timeout:
            changed = False               # a button must change the SCREEN; a cursor flicker is not it
        if changed:
            if self.stable[0] in TRANSIENT:
                if now - p.issued < TRANSITION_MAX:
                    return False          # mid-transition: wait for a known screen
                self.pending = None
                return True
            self.pending = None
            self.last_progress = now
            return True
        if now - p.issued < p.timeout:
            return False
        if self.raw_key != p.key0:
            return False                  # something is changing right now
        # Repeat a press only when FRESH frames (not a repeat of the last one: the capture
        # delivers few frames when the Dolphin windows are off screen) taken a full timeout
        # after the press still show the same screen. Otherwise a slow picture would make
        # us press twice and the second press would land on the next screen.
        if p.fresh < MIN_FRESH or self.last_fresh_t - p.issued < p.timeout:
            return False
        if p.tries < p.max_tries:
            self.pending = None
            self._issue(replace(p, tries=p.tries + 1, fresh=0))
            return False
        self.pending = None
        if p.max_tries == 1:
            return True                   # a single press by design (two-player A presses)
        self.log(f"i{self.k} STUCK: '{p.name}' had no effect on {self.stable[0]} after {p.max_tries} tries")
        self.cooldown_until = now + STUCK_COOLDOWN
        return False

    def _step(self, now: float):
        if not self._settle(now):
            return
        st, cur = self.stable
        if st is None:
            return
        if now - self.stable_since < self.linger.get(st, 0.0):
            return
        act = self._plan(st, cur, now)
        self._status(self._status_for(st))
        if act is not None:
            self._issue(act)

    # ------------------------------------------------------------ planning
    def _plan(self, st, cur, now) -> Action | None:
        g = self.goal
        mode = self.mode()

        if st == "emu_alert":
            self._recover("Dolphin alert sheet (emulator error)")
            return None
        if st == "welcome":
            return Action("A on welcome dialog", button="A")
        if st in ("error_dialog", "disconnected_dialog"):
            if mode == "watch" or (g.mode == "setup" and self.handed):
                if not self.nav_on:
                    self.nav_on = self.hooks.take_control(self.slots)
                self.lost = True
                self.log(f"i{self.k} left the room ({st})")
                if self.vote_open:
                    self.vote_open = False
                    self.prompt_at = 0.0
                    self.hooks.vote_window(self.slots, False, 0.0)
            return Action(f"A on {st}", button="A")

        if mode == "watch":
            if st in RACE_OVER_STATES and not self.vote_open and self.handed:
                self._open_vote(now, st)
            if st == "next_race_menu":
                # The race is over; the phone shows the vote picker over the video, so the
                # navigator takes the controls and chooses "Next Race".
                if not self.nav_on:
                    self.nav_on = self.hooks.take_control(self.slots)
                    self.log(f"i{self.k} race over: navControl taken on next_race_menu")
                if cur != "next":
                    return Action("UP to Next Race", move="UP", timeout=T_MOVE)
                return Action("A on Next Race", button="A")
            if st in ROOM_STATES:
                if st != "course_poll" and not self.nav_on:
                    self.nav_on = self.hooks.take_control(self.slots)
                    self.log(f"i{self.k} next race: navControl taken on {st}")
                return self._plan_room(st, cur, g)
            if st in ("main_menu", "license_select", "title", "health", "wfc_menu", "connecting"):
                self.lost = True
                self.log(f"i{self.k} is at {st} while the race is live: out of the room")
            return None

        # ---- wfc / setup ----
        a = self._plan_firstboot(st, cur)
        if a is not None or st in ("create_license", "choose_mii", "controller_reg", "connect2",
                                   "wfc_notice", "permit_prompt", "wont_play"):
            return a
        if st in ("health", "title", "license_select"):
            return Action(f"A on {st}", button="A")
        if st == "main_menu":
            want = "wfc2" if self.two else "wfc"
            if cur == want:
                wait = min(5.0 * self.conn_errors, 30.0)      # back off after connection errors
                if now - self.left_room_at < REJOIN_WAIT:
                    wait = max(wait, REJOIN_WAIT - (now - self.left_room_at) + (now - self.stable_since))
                if now - self.stable_since < wait:
                    return None
                return Action(f"A on Nintendo WFC {'2 Players' if self.two else '1 Player'}", button="A")
            if cur in ("single", "license", "multi"):
                return Action(f"DOWN from {cur}", move="DOWN", timeout=T_MOVE)
            if want == "wfc2" and cur == "wfc":
                return Action("RIGHT from wfc", move="RIGHT", timeout=T_MOVE)
            return Action(f"LEFT from {cur}", move="LEFT", timeout=T_MOVE)
        if st == "connecting":
            return None
        if st == "wfc_menu":
            if cur != "worldwide":
                return Action(f"UP from {cur}", move="UP", timeout=T_MOVE)
            if mode == "wfc":
                if g.keepalive and now - self.last_keepalive >= g.keepalive and now - self.stable_since > 2:
                    self.last_keepalive = now
                    return Action("keep-alive DOWN (UP follows)", move="DOWN", timeout=T_MOVE)
                return None
            return Action("A on Worldwide", button="A")
        if st == "ww_mode":
            if mode == "wfc":
                return Action("B back from Worldwide", button="B")
            if cur != "vs":
                return Action("UP to VS", move="UP", timeout=T_MOVE)
            return Action("A on VS", button="A")
        if st == "characters":
            if mode == "wfc":
                return Action("B back from Characters", button="B")
            if self.two:
                return self._plan_two("characters", cur, (g.character, g.character_b), self.cat.char_grid, 6)
            return self._plan_grid("characters", cur, g.character, self.cat.char_grid, rows=6)
        if st == "vehicles":
            if mode == "wfc":
                return Action("B back from Vehicles", button="B")
            if self.two:
                return self._plan_two("vehicles", cur, (g.vehicle, g.vehicle_b), self.cat.veh_grid, 3)
            return self._plan_grid("vehicles", cur, g.vehicle, self.cat.veh_grid, rows=3)
        if st == "drift":
            if mode == "wfc":
                return Action("B back from Drift", button="B")
            if self.two:
                return self._plan_drift_two(cur, g)
            if cur == "help":
                return Action("DOWN from Help", move="DOWN", timeout=T_MOVE)
            if cur != g.drift:
                return Action(f"{'DOWN' if g.drift == 'manual' else 'UP'} to {g.drift}",
                              move="DOWN" if g.drift == "manual" else "UP", timeout=T_MOVE)
            if not g.go:
                if not self.drift_ready:
                    self.drift_ready = True
                    self.shot(f"ready_drift_{g.drift}")
                    self.log(f"i{self.k} READY at Drift [{g.drift}] with {g.character} / {g.vehicle}")
                return None
            self.drift_ready = True
            return Action(f"A to confirm drift {g.drift} (start search)", button="A")
        if st == "searching":
            if mode == "wfc":
                return Action("B back from search", button="B")
            if now - self.stable_since > SEARCH_PATIENCE and now - self.last_nudge > 20:
                self.last_nudge = now
                self.log(f"i{self.k} searching for {now - self.stable_since:.0f} s: backing out to search again")
                return Action("B back from search (too long)", button="B")
            return None
        if st in ("opponents_chosen", "preparing"):
            return None
        if st in ROOM_STATES:
            if mode == "setup" and g.go:
                return self._plan_room(st, cur, g)
            if st in ("courses_cups", "courses_list"):
                return Action("B back from Courses", button="B")
            return None                       # in a room we do not want: wait to be dropped
        if st == "next_race_menu":
            # A finished race of an earlier round (e.g. after a lobby reset): leave the room.
            if cur != "quit":
                return Action("DOWN to Quit Game (leaving an old room)", move="DOWN", timeout=T_MOVE)
            return Action("A on Quit Game (leaving an old room)", button="A")
        if st in ("results", "please_wait", "race"):
            if st == "race" and self.boot_demo and now - self.stable_since > 5 and now - self.last_nudge > 5:
                self.last_nudge = now
                return Action("A to leave the title demo", button="A")
            # A race this instance is not wanted in: send nothing, so the game drops it for
            # being idle (about 30-40 s); relaunch only if that does not happen. (No B nudges
            # here: they did nothing in a race and could land on the next screen.)
            idle = now - self.last_progress
            if idle > STALL_RELAUNCH:
                self._recover(f"still at {st} after {idle:.0f} s without input")
            return None
        if st == "other_menu":
            if self.prev_known in ROOM_STATES or (g.mode == "setup" and g.go):
                return None               # a room screen in transition: never B in a room
            return Action("B back from an unrelated menu", button="B")
        return self._stall(now)

    boot_demo = False   # the "race" on screen followed the title screen: it is the attract demo

    def _plan_firstboot(self, st, cur) -> Action | None:
        """Two-player entry to Nintendo WFC (controller registration, the second player's
        Mii) and the first-boot / first-connect screens of a fresh profile (only shown in
        4:3: two-player profiles and `nav.py setup`)."""
        if st == "controller_reg":
            if cur == "wait":
                return Action("A on the second controller (registration)", button="A", pad=2)
            if cur == "revise":
                return Action("UP to OK (registration)", move="UP", timeout=T_MOVE)
            return Action("A on OK (controllers)", button="A")
        if st == "choose_mii":
            if cur == "p2":
                return Action("A on a Mii for player 2", button="A", pad=2)
            return Action("A on a guest Mii (new license)", button="A")
        if st in ("connect2", "create_license", "wfc_notice"):
            return Action(f"A on {st}", button="A")
        if st == "permit_prompt" and self.explore_wont_play and "wont_play" not in self.explored:
            # calibration: show "You will not be able to play ..." once (Prohibit, then No)
            if cur != "prohibit":
                return Action("DOWN to Prohibit (to see the confirmation once)", move="DOWN", timeout=T_MOVE)
            return Action("A on Prohibit (the next screen asks again: No)", button="A")
        if st == "wont_play":
            self.explored.add("wont_play")
        if st == "permit_prompt":
            if cur != "permit":
                return Action("UP to Permit (data goes to the local server)", move="UP", timeout=T_MOVE)
            return Action("A on Permit", button="A")
        if st == "wont_play":
            if cur != "no":
                return Action("DOWN to No", move="DOWN", timeout=T_MOVE)
            return Action("A on No (back to Permit)", button="A")
        return None

    def _stall(self, now):
        """Black / unknown / unexpected screen while heading for a menu goal."""
        idle = now - self.last_progress
        if idle > STALL_RELAUNCH:
            self._recover(f"no progress for {idle:.0f} s at {self.stable[0]}")
            return None
        if idle > STALL_NUDGE and now - self.last_nudge > 10:
            self.last_nudge = now
            return Action(f"B nudge (no progress for {idle:.0f} s)", button="B", timeout=5.0)
        return None

    def _grid_move(self, state, cur, want, grid, rows, pad=1, who=""):
        """One step from cur to want on a character / vehicle grid, or None when there."""
        (r0, c0), (r1, c1) = grid[cur], grid[want]
        if (r0, c0) == (r1, c1):
            return None
        if r0 != r1:
            mv = vstep(r0, r1, rows)
        else:
            mv = "RIGHT" if c1 > c0 else "LEFT"
        return Action(f"{mv} on {state}{who}: {cur} -> {want}", move=mv, timeout=T_MOVE, pad=pad)

    def _plan_grid(self, state, cur, want, grid, rows) -> Action | None:
        if want not in grid or cur not in grid:
            self.log(f"i{self.k} {state}: cannot plan from '{cur}' to '{want}' (not in the catalog grid)")
            self.cooldown_until = time.time() + 2.0
            return None
        if state == "vehicles" and self.cat.veh_weight.get(cur) != self.cat.veh_weight.get(want):
            self.log(f"i{self.k} vehicles: on screen is a {self.cat.veh_weight.get(cur)} vehicle, wanted "
                     f"{want} is {self.cat.veh_weight.get(want)}; going back to Characters")
            return Action("B back to Characters (wrong weight class)", button="B")
        a = self._grid_move(state, cur, want, grid, rows)
        if a is not None:
            return a
        if self.hold_pick:
            return None
        self.shot(f"pick_{state}_{want}")
        return Action(f"A to pick {want}", button="A")

    two_done = None       # pads that pressed A on their target on this screen (two-player)
    two_done_at = 0.0

    def _two_press(self, state, pad, what, now, last=False) -> Action | None:
        """A for one player of a two-player screen. The game shows player A's confirmation
        only as a pulsing mark, which cannot be read reliably, so each player presses A
        once on its target (A again on a confirmed player does nothing, seen live) and the
        pair is pressed again when the screen has not moved on 3 s after player B's A."""
        done = self.two_done.setdefault(state, set())
        done.add(pad)
        self.two_done_at = now
        return Action(f"A to pick {what} (player {'AB'[pad - 1]})", button="A", pad=pad,
                      cursor_ok=not last, timeout=T_MOVE if not last else T_PRESS, max_tries=1)

    def _plan_two(self, state, cur, wants, grid, rows) -> Action | None:
        """Characters / vehicles of a two-player instance: cur = (A's name, B's name, A
        confirmed as far as it can be read). Both cursors move at the same time (the game
        confirms by itself about 25 s after VS); each player presses A on its target; the
        second A leaves the screen. Characters: the two cursors cannot share a cell (a
        move onto the other cursor skips past it), so when B sits on A's target, B moves
        first. On Vehicles each player has a carousel (LEFT / RIGHT through the 6 vehicles
        of its weight class, karts then bikes)."""
        n1, n2, _ok1 = cur
        now = time.time()
        if self.two_done is None:
            self.two_done = {}
        for name, want in ((n1, wants[0]), (n2, wants[1])):
            if want not in grid or name not in grid:
                self.log(f"i{self.k} {state}: cannot plan from '{name}' to '{want}' (not in the catalog grid)")
                self.cooldown_until = time.time() + 2.0
                return None
        if state == "vehicles":
            for i, (name, want) in enumerate(((n1, wants[0]), (n2, wants[1]))):
                if self.cat.veh_weight.get(name) != self.cat.veh_weight.get(want):
                    self.log(f"i{self.k} vehicles: player {'AB'[i]} shows a {self.cat.veh_weight.get(name)} "
                             f"vehicle, wanted {want} is {self.cat.veh_weight.get(want)}; back to Characters")
                    return Action(f"B back to Characters (player {'AB'[i]}: wrong weight class)", button="B",
                                  pad=i + 1)
        done = self.two_done.setdefault(state, set())
        if done >= {1, 2}:
            # A screen change can take a few seconds under load; pressing again too early
            # lands on the next screen (seen: B's A confirmed its vehicle at once).
            if now - self.two_done_at < 6.0 or now - self.last_fresh_t > 1.0:
                return None
            self.log(f"i{self.k} {state}: still here 6 s after both players pressed A; pressing again")
            done.clear()
        move = self._carousel_move if state == "vehicles" else self._grid_move
        m1 = None if 1 in done else move(state, n1, wants[0], grid, rows, pad=1, who=" (A)")
        m2 = None if 2 in done else move(state, n2, wants[1], grid, rows, pad=2, who=" (B)")
        if state == "characters" and 1 not in done and n2 == wants[0] and m1 is not None:
            # B is on A's character: B moves out of the way first
            if m2 is None:
                m2 = Action(f"DOWN on characters (B): out of A's way ({n2})", move="DOWN", timeout=T_MOVE, pad=2)
            return m2
        if m1 is not None and m2 is not None:
            return Action(f"{m1.move} (A) + {m2.move} (B) on {state}: A {n1} -> {wants[0]}, B {n2} -> {wants[1]}",
                          moves=((1, m1.move), (2, m2.move)), timeout=T_MOVE)
        if m1 is None and m2 is None:
            if self.hold_pick:
                return None
            # both on their targets: A for A then B (B's A leaves the screen)
            self.shot(f"pick_{state}_{wants[0]}_{wants[1]}")
            todo = [p for p in (1, 2) if p not in done]
            done |= {1, 2}
            self.two_done_at = now
            return Action(f"A to pick {wants[0]} (A) and {wants[1]} (B)", presses=tuple((p, "A") for p in todo),
                          timeout=T_PRESS, max_tries=1)
        return m1 or m2

    def _carousel_move(self, state, cur, want, grid, rows, pad=1, who=""):
        """Two-player Vehicles: LEFT / RIGHT through karts (rows 0-2 of column 0) then bikes."""
        (r0, c0), (r1, c1) = grid[cur], grid[want]
        i0, i1 = c0 * 3 + r0, c1 * 3 + r1
        if i0 == i1:
            return None
        d = (i1 - i0) % 6
        mv = "RIGHT" if d <= 3 else "LEFT"
        return Action(f"{mv} on {state}{who}: {cur} -> {want}", move=mv, timeout=T_MOVE, pad=pad)

    def _plan_drift_two(self, cur, g) -> Action | None:
        """Drift of a two-player instance: cur = (A's choice, B's choice, A confirmed).
        Both cursors are put on the wanted choice and the instance holds there; on go,
        A confirms (pad p1), then B (pad p2), which starts the search."""
        c1, c2, _ok1 = cur
        now = time.time()
        if self.two_done is None:
            self.two_done = {}
        done = self.two_done.setdefault("drift", set())
        mv = lambda want: "DOWN" if want == "manual" else "UP"
        m = []
        if 1 not in done and c1 != g.drift:
            m.append((1, mv(g.drift)))
        if 2 not in done and c2 != g.drift_b:
            m.append((2, mv(g.drift_b)))
        if m:
            return Action(" + ".join(f"{d} ({'AB'[p - 1]})" for p, d in m) + f" on drift to A {g.drift}, B {g.drift_b}",
                          moves=tuple(m), timeout=T_MOVE)
        if not g.go:
            if not self.drift_ready:
                self.drift_ready = True
                self.shot(f"ready_drift_{g.drift}_{g.drift_b}")
                self.log(f"i{self.k} READY at Drift [A {g.drift}, B {g.drift_b}] with "
                         f"A {g.character} / {g.vehicle}, B {g.character_b} / {g.vehicle_b}")
            return None
        self.drift_ready = True
        if done >= {1, 2}:
            if now - self.two_done_at < 6.0 or now - self.last_fresh_t > 1.0:
                return None
            self.log(f"i{self.k} drift: still here 6 s after both players pressed A; pressing again")
            done.clear()
        todo = [p for p in (1, 2) if p not in done]
        done |= {1, 2}
        self.two_done_at = now
        return Action(f"A to confirm drift A {g.drift}, B {g.drift_b} (start search)",
                      presses=tuple((p, "A") for p in todo), timeout=T_PRESS, max_tries=1)

    def _open_vote(self, now, st):
        self.vote_open = True
        self.vote_opened_at = now
        self.vote_deadline = now + VOTE_INITIAL
        self.prompt_at = 0.0
        self.vote_entered_log = None
        ok = self.hooks.vote_window(self.slots, True, self.vote_deadline)
        self.log(f"i{self.k} race over ({st}): vote picker OPEN on the phone{'s' if self.two else ''}, "
                 f"deadline in {VOTE_INITIAL:.0f} s{'' if ok else ' (lobby update FAILED)'}")

    def _players(self, g):
        """[(vote, locked, connected)] of the players on this instance."""
        out = [(g.vote, g.vote_locked, g.connected)]
        if self.two and len(g.slots) > 1:
            out.append((g.vote_b, g.vote_locked_b, g.connected_b))
        return out

    def _vote_ready(self) -> bool:
        """The vote may be entered now: every player locked in or gone, or deadline passed."""
        if now_t() >= self.vote_deadline:
            return True
        return all(locked or not conn for _v, locked, conn in self._players(self.goal))

    def _vote_of(self, g) -> str:
        """The course to enter. A two-player instance has ONE vote in the game (both of its
        players show the same course on Course Poll): it alternates between player A's and
        player B's choice from race to race, and takes the other player's choice when the
        one whose turn it is chose Random."""
        votes = []
        for i, (v, _locked, conn) in enumerate(self._players(g)):
            if not conn and self.last_vote.get(i):
                v = self.last_vote[i]
            votes.append(v or "random")
        if len(votes) == 1:
            self.vote_by = 0
            return votes[0]
        first = self.races % 2
        order = [first, 1 - first]
        for i in order:
            if votes[i] != "random":
                self.vote_by = i
                return votes[i]
        self.vote_by = first
        return "random"

    def _effective_vote(self, g):
        vote = self._vote_of(g)
        cup = self.cat.course_cup.get(vote)
        if vote != "random" and (cup is None or not self.cat.cup_unlocked.get(cup)):
            return "random"
        return vote

    def _log_vote_entered(self, g):
        if self.vote_entered_log is not None:
            return
        t = now_t()
        locked = [p[1] for p in self._players(g)]
        conn = [p[2] for p in self._players(g)]
        why = ("locked in" if all(l or not c for l, c in zip(locked, conn)) and any(locked)
               else "phone disconnected" if not all(conn)
               else "deadline" if self.vote_open else "no vote window (first race)")
        self.vote_entered_log = why
        vote = self._vote_of(g)
        who = "" if not self.two or len(g.slots) < 2 else f" (player {'AB'[self.vote_by or 0]}'s choice; " \
              f"A {g.vote}, B {g.vote_b})"
        self.log(f"i{self.k} VOTE entering {vote}{who} ({why}; window open {t - self.vote_opened_at:.1f} s, "
                 f"dialog up {t - self.prompt_at:.1f} s)" if self.vote_open else
                 f"i{self.k} VOTE entering {vote}{who} ({why})")

    def _plan_room(self, st, cur, g: Goal) -> Action | None:
        cat = self.cat
        vote = self._vote_of(g)
        cup = cat.course_cup.get(vote)
        if vote != "random" and (cup is None or not cat.cup_unlocked.get(cup)):
            if not getattr(self, "_warned_vote", None) == vote:
                self._warned_vote = vote
                self.log(f"i{self.k} vote '{vote}' is not selectable on this save (locked or unknown): voting Random")
            vote, cup = "random", None
        if st == "check_members":
            # The OK button ignores input for a moment after the screen appears and shows no
            # feedback for about 0.8 s after it accepts, so A is repeated quickly until the
            # screen changes (seen live: A at +0.09 s was ignored, A at +1.09 s was taken).
            return Action(f"A on OK (Check Members{'' if self.members is None else ', %d players' % self.members})",
                          button="A", timeout=T_CHECK, max_tries=10)
        if st == "vote_prompt":
            if self.prompt_at == 0.0 or now_t() - self.prompt_at > 60:
                self.prompt_at = now_t()
                if self.vote_open:
                    # The game's own vote timer starts with this dialog: tighten the deadline.
                    dl = min(self.vote_deadline, self.prompt_at + VOTE_WAIT_AT_PROMPT)
                    if dl != self.vote_deadline:
                        self.vote_deadline = dl
                        self.hooks.vote_window(self.slots, True, dl, new=False)
                        self.log(f"i{self.k} vote dialog up: phone vote deadline {dl - now_t():.1f} s from now")
            if self.vote_open and not self._vote_ready():
                return None                       # waiting for the player(s) to lock in
            self._log_vote_entered(g)
            vote = self._effective_vote(g)
            cup = cat.course_cup.get(vote)
            want = "random" if vote == "random" else "vote"
            if cur != want:
                return Action(f"{'DOWN' if want == 'random' else 'UP'} to {want}",
                              move="DOWN" if want == "random" else "UP", timeout=T_MOVE)
            if want == "random":
                self.random_at = now_t()
            return Action(f"A on {'Random' if want == 'random' else 'Vote'}", button="A", timeout=T_FAST)
        if st in ("courses_cups", "courses_list"):
            if self.vote_open and self.vote_entered_log is None and not self._vote_ready():
                return None                       # (only if the dialog was skipped) wait for the player
            self._log_vote_entered(g)
            vote = self._effective_vote(g)
            cup = cat.course_cup.get(vote)
        if st == "courses_cups":
            if vote == "random":
                if now_t() - self.random_at < 3.0:
                    return None                   # Courses shows briefly after A on Random
                return Action("B back to the vote dialog (want Random)", button="B")
            if cur == cup:
                self.list_cup = cup
                return Action(f"A on {cup} cup", button="A", timeout=T_FAST)
            (r0, c0), (r1, c1) = cat.cup_grid[cur], cat.cup_grid[cup]
            if r0 != r1:
                return Action(f"DOWN on cups: {cur} -> {cup}", move="DOWN", timeout=T_MOVE)
            mv = "RIGHT" if c1 > c0 else "LEFT"
            return Action(f"{mv} on cups: {cur} -> {cup}", move=mv, timeout=T_MOVE)
        if st == "courses_list":
            lcup, row = cur
            if lcup is None:
                lcup = self.list_cup              # 4:3: the list's cup is the one A was pressed on
            if vote == "random" or (lcup is not None and lcup != cup):
                return Action(f"B back to the cups (list shows {lcup})", button="B")
            want = cat.course_row[vote]
            if row != want:
                return Action(f"{vstep(row, want, 4)} in course list: row {row} -> {want}",
                              move=vstep(row, want, 4), timeout=T_MOVE)
            self.shot(f"pick_course_{vote}")
            return Action(f"A to vote {vote}", button="A", timeout=T_PRESS)
        if st == "course_poll":
            if not self.poll_visit_done:
                self.poll_visit_done = True
                if self.vote_entered_log is not None:
                    self.races += 1
                if self.vote_open:
                    self.log(f"i{self.k} vote window closed (opened {now_t() - self.vote_opened_at:.1f} s ago)")
                self.vote_open = False
                self.prompt_at = 0.0
                self.vote_entered_log = None
                ok = self.hooks.handover(self.slots)
                self.nav_on = False
                first = not self.handed
                self.handed = True
                self.t_handover = time.time()
                self.log(f"i{self.k} HANDOVER of slot(s) {list(self.slots)} at Course Poll{'' if first else ' (next race)'}"
                         f"{'' if ok else ' (lobby update FAILED)'}; Check Members showed {self.members}")
            return None
        return None

    list_cup = None

    # ------------------------------------------------------------ status text
    STATUS = {
        "health": "Starting the game...", "title": "Starting the game...",
        "license_select": "Starting the game...", "main_menu": "Going online...",
        "controller_reg": "Going online...", "choose_mii": "Going online...", "connect2": "Going online...",
        "connecting": "Connecting...", "welcome": "Connecting...",
        "wfc_menu": "Online. Waiting...", "ww_mode": "Entering Worldwide...",
        "characters": "Picking {character}...", "vehicles": "Picking {vehicle}...",
        "drift": "Setting drift...", "searching": "Finding the others...",
        "opponents_chosen": "Room found!", "preparing": "Room found!",
        "check_members": "Joining the room...", "vote_prompt": "Voting...",
        "courses_cups": "Voting for {course}...", "courses_list": "Voting for {course}...",
        "course_poll": "Waiting for everyone's vote...",
        "disconnected_dialog": "Disconnected from the room.",
        "error_dialog": "Connection error.",
    }

    def _status_for(self, st):
        g = self.goal
        if self.lost and st in ("main_menu", "connecting", "wfc_menu", "license_select"):
            return "Disconnected from the room. Back at the menu."
        if st == "searching" and self.goal.go and time.time() - self.stable_since > 60:
            return "Still looking for the others' room..."
        if st == "drift" and self.drift_ready and not g.go:
            return "Ready. Waiting for the others..."
        tpl = self.STATUS.get(st)
        if tpl is None:
            return None
        n = self.cat.names
        if self.two and len(g.slots) > 1 and st in ("characters", "vehicles"):
            a = n.get(g.character if st == "characters" else g.vehicle, "?")
            b = n.get(g.character_b if st == "characters" else g.vehicle_b, "?")
            return f"Picking {a} and {b}..."
        return tpl.format(character=n.get(g.character, "character"), vehicle=n.get(g.vehicle, "vehicle"),
                          course=n.get(self._vote_of(g), "a random course"))

    def _status(self, text):
        if text is None or text == self.status_text:
            return
        self.status_text = text
        if (self.goal.mode != "wfc" or self.lost) and self.slots:
            self.hooks.status(self.slots, text)


# ====================================================================== Runner
class LobbyHooks(Hooks):
    def __init__(self, log: Log):
        self.log = log

    def _post(self, what, **kw) -> bool:
        try:
            lobby.post_update(**kw)
            return True
        except lobby.LobbyError as e:
            self.log(f"lobby update failed ({what}): {e}")
            return False

    def status(self, slots, text):
        self._post("status", slots=[{"slot": s, "status": text} for s in slots])

    def take_control(self, slots) -> bool:
        return self._post("take control", slots=[{"slot": s, "navControl": True} for s in slots])

    def handover(self, slots) -> bool:
        return self._post("handover", slots=[{"slot": s, "showVideo": True, "navControl": False,
                                              "voteOpen": False, "voteDeadlineMs": 0,
                                              "status": "Waiting for everyone's vote..."} for s in slots])

    def vote_window(self, slots, open_, deadline=0.0, new=True) -> bool:
        """Open (new=True: also clears the phone's lock-in), re-time (new=False) or close
        the phones' vote pickers."""
        es = []
        for s in slots:
            e = {"slot": s, "voteOpen": bool(open_), "voteDeadlineMs": int(deadline * 1000) if open_ else 0}
            if open_ and new:
                e["voteLocked"] = False
            es.append(e)
        return self._post("vote window", slots=es)


class LobbyWatch(threading.Thread):
    """Long-polls /api/nav/lobby and keeps the latest snapshot."""

    def __init__(self, log: Log):
        super().__init__(daemon=True)
        self.log = log
        self._snap = None
        self._lock = threading.Lock()
        self._stop = False

    def snapshot(self):
        with self._lock:
            return self._snap

    def stop(self):
        self._stop = True

    def run(self):
        since = None
        while not self._stop:
            try:
                snap = lobby.get_lobby(since=since)
                with self._lock:
                    self._snap = snap
                since = snap.get("version")
            except lobby.LobbyError as e:
                self.log(f"lobby poll: {e}")
                since = None
                time.sleep(1.0)


# ---------------------------------------------------------------- instance pool
WARM_1P = int(os.environ.get("NAV_WARM", "3"))        # single-player instances kept at the WFC menu
WARM_2P = int(os.environ.get("NAV_WARM_2P", "0"))     # two-player instances kept at the WFC menu
# NAV_TV_AUDIO=0: never unmute anything (no host audio; every game is kept at volume 0).
TV_AUDIO = os.environ.get("NAV_TV_AUDIO", "1") != "0"
LOBBY_STOP_GRACE = 90.0     # s an instance must be unneeded in the lobby before it is stopped
BOOT_PARALLEL = 3           # Dolphins booting at the same time (a boot storm starves everything)
OCC_SETTLE = 8.0            # s the number of phones must hold before the lobby pre-warms for it
BOOT_SPACING = 6.0          # s a boot slot stays taken after the game window appeared
LAUNCH_PATIENCE = 240.0     # s Start waits for a starting instance to reach Drift before going without it
STATUS_FILE = os.path.join(ROOT, "run", "nav-status.json")


def layout(n: int):
    """[(instance, mode, number of slots)] for n players: n <= 6 -> n single-player
    instances i1..in; n > 6 -> n//2 two-player instances i7.. plus i1 if n is odd."""
    if n <= 0:
        return []
    if n <= 6:
        return [(k, "1p", 1) for k in range(1, n + 1)]
    out = [(6 + i, "2p", 2) for i in range(1, n // 2 + 1)]
    if n % 2:
        out.append((1, "1p", 1))
    return out


def assign_slots(slots):
    """{instance: (slot A[, slot B])} for the locked slots (lowest first)."""
    slots = sorted(slots)
    out, i = {}, 0
    for k, _mode, ns in layout(len(slots)):
        out[k] = tuple(slots[i:i + ns])
        i += ns
    return out


def profile_ready(k: int) -> bool:
    return os.path.exists(os.path.join(dolphin.INST_ROOT, f"i{k}", "mkparty-ready"))


def run_script(log, *args, timeout=90) -> int:
    try:
        r = subprocess.run(list(args), capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        log(f"{os.path.basename(args[0])} {' '.join(args[1:])}: timed out")
        return -1
    for line in (r.stdout + r.stderr).strip().splitlines():
        log(f"{os.path.basename(args[0])}: {line}")
    return r.returncode


class Runner:
    """Owns the workers. With a lobby (nav.py run) it also owns the instance pool: a warm
    pool at the WFC menu in the lobby (NAV_WARM single-player instances, more when more
    phones join), exactly the instances of layout(n) for the n locked players at Start,
    the slot -> instance wiring in partyd (POST /api/nav/assign), and the TV audio (only
    the host's instance is unmuted)."""

    def __init__(self, ks, log: Log, run_id: str, with_lobby: bool = False, keepalive: float = 0.0,
                 pool: bool = False, setup: bool = False, classifier_factory=None, collector=None):
        self.log = log
        self.run_id = run_id
        self.cat = Catalog()
        self.saver = Saver()
        self.saver.start()
        self.keepalive = keepalive
        self.hooks = LobbyHooks(log) if with_lobby else Hooks()
        self.pool = pool
        self.setup = setup
        self.classifier_factory = classifier_factory   # k -> classifier (None: templates)
        self.collector = collector
        os.makedirs(UNKNOWN_DIR, exist_ok=True)
        self.workers = {}
        self._wlock = threading.Lock()
        self._boot_sem = threading.Semaphore(BOOT_PARALLEL)
        for k in ks:
            self.workers[k] = self.new_worker(k)
        self.round = 0
        self.active = set()                # instances racing this round
        self.assign = {}                   # instance -> slots of this round
        self.phase = None
        self.t_start = None
        self.went = False
        self.live_posted = False
        self.reset_posted = 0.0
        self.msg_posted = 0.0
        self.go_t = 0.0
        self.entered = False
        self.enter_t = 0.0
        self.go_set = set()                # instances that may confirm Drift (search) this round
        self.host_k = None
        self.queue = []
        self.occ_n, self.occ_since = -1, 0.0
        self.starting = {}                 # instance -> time its launch began (pool)
        self.unneeded_since = {}           # instance -> time it became unneeded in the lobby
        self.audio_k = None                # instance whose audio plays (the host's)
        self.audio_busy = set()
        self.last_prune = time.time()
        self.prune = None                  # callable that prunes the logs (nav.py)
        self.started = False

    def new_worker(self, k: int) -> Worker:
        cls = self.classifier_factory(k) if self.classifier_factory else None
        return Worker(k, self.log, self.cat, self.saver, self.run_id, self.hooks, setup=self.setup,
                      classifier=cls, collector=self.collector)

    def start(self):
        self.started = True
        for w in list(self.workers.values()):
            w.set_goal(Goal("wfc", keepalive=self.keepalive))
            w.start()

    def stop(self):
        for w in list(self.workers.values()):
            w.stop()
        for w in list(self.workers.values()):
            w.join(timeout=3)
            w.inst.neutral()
        self.saver.q.join()

    def summary(self) -> str:
        return ", ".join(f"i{k}={w.stable[0]}{'' if w.stable[1] is None else '[%s]' % (w.stable[1],)}"
                         for k, w in sorted(self.workers.items()))

    # ----- pool -----
    def running(self):
        return set(self.workers) | set(self.starting)

    def _spawn(self, k: int, why: str):
        """Launch instance k (stream first, then Dolphin) in the background and add its
        worker when the game window is there."""
        if k in self.running():
            return
        if not profile_ready(k):
            self.log(f"POOL: instance {k} is not set up (no instances/i{k}/mkparty-ready); "
                     "run ./mkparty setup --profiles. Trying anyway.")
        self.starting[k] = time.time()
        self.log(f"POOL: starting instance {k} ({dolphin.inst_mode(k)}): {why}")

        def go():
            with self._boot_sem:          # at most BOOT_PARALLEL Dolphins boot at once
                run_script(self.log, os.path.join(dolphin.RUN_DIR, "streams.sh"), "start", str(k))
                inst = dolphin.Instance(k)
                ok = inst.alive() or inst.launch(self.log)
                if ok:
                    inst.launched_at = inst.launched_at or time.time()
                    time.sleep(BOOT_SPACING)
            w = self.new_worker(k)
            w.inst.launched_at = inst.launched_at or time.time()
            with self._wlock:
                self.workers[k] = w
                self.starting.pop(k, None)
            w.set_goal(Goal("wfc", round=self.round, keepalive=self.keepalive))
            w.start()
            self.log(f"POOL: instance {k} {'up' if ok else 'launch FAILED (the worker keeps trying)'} "
                     f"after {time.time() - t0:.1f} s")
        t0 = time.time()
        threading.Thread(target=go, daemon=True, name=f"spawn-i{k}").start()

    def _retire(self, k: int, why: str):
        """Stop instance k (worker, Dolphin via run/down.sh, its stream) in the background."""
        with self._wlock:
            w = self.workers.pop(k, None)
        if w is None:
            return
        self.log(f"POOL: stopping instance {k}: {why}")
        self.unneeded_since.pop(k, None)

        def go():
            w.stop()
            w.join(timeout=5)
            w.inst.neutral()
            w.inst.stop(self.log)
            run_script(self.log, os.path.join(dolphin.RUN_DIR, "streams.sh"), "stop", str(k))
        threading.Thread(target=go, daemon=True, name=f"retire-i{k}").start()

    def lobby_wanted(self, n_occ: int):
        """Instances to keep ready in the lobby for n_occ connected players."""
        want = {k for k, _m, _n in layout(n_occ)}
        if n_occ <= 6:
            want |= set(range(1, max(WARM_1P, 0) + 1))
            want |= set(range(7, 7 + max(WARM_2P, 0)))
        return {k for k in want if 1 <= k <= 12}

    def _apply_audio(self):
        """Exactly the audio instance plays (volume 100); every other instance is at 0.
        A relaunched instance boots muted (audio None) and is set again. With NAV_TV_AUDIO=0
        nothing is unmuted and every instance is also set to volume 0 once."""
        for k, w in list(self.workers.items()):
            want = TV_AUDIO and (k == self.audio_k)
            have = w.inst.audio
            if (have is None and not want and TV_AUDIO) or have == want or k in self.audio_busy:
                continue
            if not w.inst.alive() or w.stable[0] is None:
                continue

            def go(w=w, want=want, k=k):
                ok = w.inst.set_audio(want)
                self.log(f"AUDIO i{k}: {'ON (volume 100)' if want else 'off (volume 0)'}"
                         f"{'' if ok else ' FAILED (hotkey FIFO not read yet)'}")
                self.audio_busy.discard(k)
            self.audio_busy.add(k)
            threading.Thread(target=go, daemon=True, name=f"audio-i{k}").start()

    # ----- prepare / hold (nav.py prepare, setup) -----
    def prepare(self, timeout: float = 600.0) -> bool:
        t0 = time.time()
        for w in self.workers.values():
            w.set_goal(Goal("wfc", round=w.goal.round, keepalive=self.keepalive))
        last = 0.0
        while time.time() - t0 < timeout:
            if all(w.at_wfc for w in self.workers.values()):
                self.log(f"prepare: every instance is at Nintendo WFC after {time.time() - t0:.1f} s")
                return True
            if any(w.failed for w in self.workers.values()):
                break
            bad = [w.k for w in self.workers.values() if w.conn_errors >= 4]
            if bad:
                self.log(f"prepare: instance(s) {bad} got {4}+ connection error dialogs in a row; "
                         "is the game server up (server/netcheck.sh)?")
                break
            if time.time() - last > 15:
                last = time.time()
                self.log("prepare: " + self.summary())
            time.sleep(0.1)
        self.log("prepare FAILED: " + self.summary())
        return False

    def hold(self, minutes: float):
        t0 = time.time()
        last = 0.0
        left = set()
        self.log(f"hold: {minutes:.1f} min at the WFC menu, keep-alive "
                 f"{'every %.0f s' % self.keepalive if self.keepalive else 'off'}")
        while time.time() - t0 < minutes * 60:
            for k, w in self.workers.items():
                if w.state not in ("wfc_menu", None) and k not in left and w.pending is None:
                    left.add(k)
                    self.log(f"hold: i{k} LEFT the WFC menu after {time.time() - t0:.0f} s -> {w.state}")
            if time.time() - last > 60:
                last = time.time()
                self.log(f"hold {time.time() - t0:.0f} s: " + self.summary())
            time.sleep(0.5)
        self.log(f"hold finished after {minutes:.1f} min: left={sorted(left)}; " + self.summary())
        return left

    # ----- lobby-driven run -----
    def run(self, prep_timeout: float = 600.0, once: bool = False) -> int:
        if not self.pool and not self.prepare(prep_timeout):
            return 1
        lw = LobbyWatch(self.log)
        lw.start()
        self.log("run: following the lobby")
        try:
            while True:
                snap = lw.snapshot()
                if snap is None:
                    time.sleep(0.2)
                    continue
                self._on_snapshot(snap)
                if self.pool:
                    self._apply_audio()
                if self.prune and time.time() - self.last_prune > 600:
                    self.last_prune = time.time()
                    threading.Thread(target=self.prune, daemon=True).start()
                time.sleep(0.05)
        finally:
            lw.stop()

    def _goal_for(self, k, snap, mode, go) -> Goal:
        slots = self.assign.get(k, ())
        by = {p.get("slot"): p for p in snap.get("players", [])}
        pa = by.get(slots[0], {}) if slots else {}
        pb = by.get(slots[1], {}) if len(slots) > 1 else {}
        return Goal(mode, round=self.round, character=pa.get("character"), vehicle=pa.get("vehicle"),
                    drift=pa.get("drift") or "auto", vote=pa.get("vote") or "random", go=go,
                    keepalive=self.keepalive, vote_locked=bool(pa.get("voteLocked")),
                    connected=bool(pa.get("connected", True)), slots=tuple(slots),
                    character_b=pb.get("character"), vehicle_b=pb.get("vehicle"),
                    drift_b=pb.get("drift") or "auto", vote_b=pb.get("vote") or "random",
                    vote_locked_b=bool(pb.get("voteLocked")), connected_b=bool(pb.get("connected", True)))

    def _wire(self, snap):
        """POST /api/nav/assign: every locked slot to its instance FIFO and video source;
        every other slot unassigned (no input, no video)."""
        e = []
        on = {}
        for k, slots in self.assign.items():
            for pad, s in enumerate(slots, start=1):
                on[s] = (k, pad)
        for p in snap.get("players", []):
            s = p.get("slot")
            if s in on:
                k, pad = on[s]
                e.append({"slot": s, "pipe": os.path.join(dolphin.INST_ROOT, f"i{k}", "Pipes", f"p{pad}"),
                          "instance": k, "pad": pad, "mode": dolphin.inst_mode(k)})
            elif p.get("pipe") or p.get("instance"):
                e.append({"slot": s, "pipe": "", "instance": 0, "pad": 0, "mode": ""})
        if not e:
            return
        try:
            lobby.post_assign(e)
            self.log("ASSIGN " + ", ".join(f"slot {x['slot']}->" + (f"i{x['instance']}/p{x['pad']}" if x["instance"]
                                                                    else "none") for x in e))
        except lobby.LobbyError as err:
            self.log(f"assign failed: {err}")

    def _write_status(self, ready, n_ok, want):
        try:
            tmp = STATUS_FILE + ".tmp"
            with open(tmp, "w") as f:
                json.dump({"pid": os.getpid(), "time": time.time(), "phase": self.phase, "ready": ready,
                           "online": n_ok, "wanted": sorted(want), "running": sorted(self.running())}, f)
            os.replace(tmp, STATUS_FILE)
        except OSError:
            pass

    def _on_snapshot(self, snap):
        phase = snap.get("phase")
        now = time.time()
        locked = sorted(snap.get("lockedSlots") or [])
        if phase != self.phase:
            self.log(f"lobby phase {self.phase} -> {phase} (version {snap.get('version')})")
            if phase == "starting":
                self.round += 1
                self.assign = assign_slots(locked)
                self.active = set(self.assign)
                self.t_start = now
                self.went = False
                self.entered = False
                self.go_set = set()
                self.go_t = 0.0
                self.live_posted = False
                self._start_round(snap)
            elif phase == "live" and not self.active:
                # navigator started while a race was live: take the wiring from partyd
                self.round += 1
                self.assign = {}
                for p in sorted(snap.get("players", []), key=lambda p: (p.get("pad") or 0)):
                    if p.get("slot") in locked and p.get("instance"):
                        self.assign.setdefault(p["instance"], ())
                        self.assign[p["instance"]] += (p["slot"],)
                self.active = set(self.assign)
                for k in self.active:
                    if k not in self.running():
                        self._spawn(k, "in the live race (navigator restarted)")
                self.went = True
                self.entered = True
                self.live_posted = True
            elif phase == "lobby":
                self.active = set()
            self.phase = phase

        if phase == "lobby":
            self._lobby(snap, now)
            return

        for k, w in list(self.workers.items()):
            if k not in self.active:
                w.set_goal(Goal("wfc", round=self.round, keepalive=self.keepalive))
                continue
            # "setup" for the whole round: a worker that has handed over watches (Worker.mode),
            # one that has not (e.g. it missed the room) keeps trying to get in.
            go = k in self.go_set or phase == "live" or not self.pool
            g = self._goal_for(k, snap, "setup", go)
            w.set_goal(replace(g, enter=go))
        act = [self.workers[k] for k in self.active if k in self.workers and not self.workers[k].failed]
        waiting_launch = [k for k in self.active if k in self.starting]
        if not act and not waiting_launch:
            if now - self.reset_posted > 10:
                self._reset("no instance is left in the round")
            return

        if phase == "starting":
            late = [k for k in self.active if k not in self.workers and now - self.t_start < LAUNCH_PATIENCE]
            # Entry in a queue: one instance (a single-player one if there is one: it picks
            # fastest) goes to Worldwide VS first, picks and searches at once, and so opens the
            # room; every ENTER_STAGGER s the next instance follows (it too picks and searches
            # at once). The game confirms character / vehicle / drift by itself about 25 s after
            # VS, so instances do not hold at Drift. Seen in the server log: a host takes one
            # joining console at a time; reservations that arrive while it is busy with another
            # are refused ("This Domain is already closed") and that console opens a room of
            # its own - instances that searched together ended in 2-3 rooms.
            if not self.entered and act and not late:
                at = [w for w in act if w.at_wfc]
                if len(at) == len(act) or (now - self.t_start > LAUNCH_PATIENCE and at):
                    self.entered = True
                    self.enter_t = now
                    self.queue = [w.k for w in sorted(at, key=lambda w: (w.two, w.k))]
                    self.host_k = self.queue[0]
                    self.log(f"ENTER: {len(at)}/{len(act)} instance(s) at the WFC menu {now - self.t_start:.1f} s after "
                             f"Start; they go to Worldwide VS one by one: "
                             f"{' '.join('i%d' % k for k in self.queue)}")
            if self.entered and not self.went:
                # single-player games pick in ~9 s, two-player ones in ~13 s: tighter queue
                gap = ENTER_STAGGER if any(self.workers[k].two for k in self.queue if k in self.workers) \
                    else ENTER_STAGGER_1P
                n_in = min(len(self.queue), 1 + int((now - self.enter_t) // gap))
                new = [k for k in self.queue[:n_in] if k not in self.go_set]
                if new:
                    self.go_set |= set(new)
                    self.log(f"GO: i{' i'.join(map(str, new))} to Worldwide VS ({now - self.enter_t:.1f} s after ENTER)")
                if n_in == len(self.queue):
                    self.went = True
                    self.go_t = now
                    self.go_set = set(self.active)       # stragglers (launched late) too
            handed_t = [w.t_handover for w in act if w.handed and w.t_handover]
            all_in = self.went and all(w.handed or w.lost for w in act)
            # partyd drops ALL phone input while the phase is "starting", so the players who
            # are already in the room must get "live" quickly even if another instance missed
            # the room: wait at most LIVE_GRACE s after the first hand-over.
            if not self.live_posted and handed_t and (all_in or now - min(handed_t) > LIVE_GRACE):
                self.live_posted = True
                handed = sorted(s for w in act if w.handed for s in w.slots)
                waiting = sorted(s for w in act if not w.handed and not w.lost for s in w.slots)
                members = {w.k: w.members for w in act}
                self.log(f"{'ALL HANDED OVER' if all_in else 'LIVE WITHOUT slots ' + str(waiting)} "
                         f"{now - self.t_start:.1f} s after Start: slots {handed}; Check Members counts {members}")
                try:
                    # Slots still being driven keep navControl (phase live clears it otherwise).
                    lobby.post_update(phase="live", message="",
                                      slots=[{"slot": s, "navControl": True} for s in waiting])
                except lobby.LobbyError as e:
                    self.log(f"phase live failed: {e}")
                    self.live_posted = False
        handed = [w for w in act if w.handed]
        if self.live_posted and handed and all(w.lost for w in handed) \
                and not any(w.state in ROOM_STATES or w.state == "race" for w in act):
            # Everyone who raced is out of the room (an instance that never got in may still
            # be searching): the round is over.
            if now - self.reset_posted > 10:
                self._reset("every instance that raced has left the room")
            return
        if act and all(w.lost for w in act) and all(w.state in ("main_menu", "wfc_menu", "connecting") for w in act):
            if now - self.reset_posted > 10:
                self._reset("every instance of the round is back at the menu")

    def _start_round(self, snap):
        """Start pressed: wire the slots, bring up exactly the instances of the layout
        (stopping the others that are not part of the warm pool), and pick the TV audio."""
        n = sum(len(v) for v in self.assign.values())
        for k in sorted(self.assign):
            p = {x.get("slot"): x for x in snap.get("players", [])}
            desc = " + ".join(f"slot {s} {p.get(s, {}).get('name')!r}: {p.get(s, {}).get('character')} / "
                              f"{p.get(s, {}).get('vehicle')} / {p.get(s, {}).get('drift')} / vote "
                              f"{p.get(s, {}).get('vote')}" for s in self.assign[k])
            self.log(f"round {self.round}: i{k} ({dolphin.inst_mode(k)}) <- {desc}")
        if not self.pool:
            self._wire(snap)
            return
        keep = set(self.assign)
        if n <= 6:
            keep |= set(range(1, WARM_1P + 1))
        for k in sorted(self.running()):
            if k not in keep and k in self.workers:
                self._retire(k, f"not needed for {n} players")
        missing = [k for k in sorted(self.assign) if k not in self.running()]
        for k in missing:
            self._spawn(k, f"needed for {n} players")
        self._wire(snap)
        host = snap.get("hostSlot") or 0
        locked = sorted(s for v in self.assign.values() for s in v)
        audio_slot = host if host in locked else (locked[0] if locked else 0)
        if TV_AUDIO:
            self.audio_k = next((k for k, v in self.assign.items() if audio_slot in v), None)
            self.log(f"AUDIO: the TV plays instance {self.audio_k} (slot {audio_slot}, host slot {host})")
        else:
            self.audio_k = None
            self.log("AUDIO: off for every instance (NAV_TV_AUDIO=0)")
        if missing:
            msg = f"Starting {len(missing)} more game{'s' if len(missing) > 1 else ''}..."
            try:
                lobby.post_update(message=msg, slots=[{"slot": s, "status": msg} for k in missing
                                                      for s in self.assign[k]])
            except lobby.LobbyError as e:
                self.log(f"lobby message failed: {e}")

    def _lobby(self, snap, now):
        for w in self.workers.values():
            w.set_goal(Goal("wfc", round=self.round, keepalive=self.keepalive))
        want = set(self.workers)
        if self.pool:
            n_occ = sum(1 for p in snap.get("players", []) if p.get("occupied") and p.get("connected"))
            if n_occ != self.occ_n:
                self.occ_n, self.occ_since = n_occ, now
            want = self.lobby_wanted(n_occ)
            # Pre-warm for the phones in the lobby once their number has held for a moment
            # (phones joining one after the other would otherwise boot every layout on the way).
            if now - self.occ_since >= OCC_SETTLE or n_occ <= WARM_1P:
                for k in sorted(want - self.running()):
                    self._spawn(k, f"{n_occ} phone(s) in the lobby")
            for k in sorted(self.running() - want):
                if k in self.starting:
                    continue
                t = self.unneeded_since.setdefault(k, now)
                if now - t > LOBBY_STOP_GRACE:
                    self._retire(k, f"not needed in the lobby for {LOBBY_STOP_GRACE:.0f} s ({n_occ} phones)")
            for k in want:
                self.unneeded_since.pop(k, None)
        # Tell the phones whether Start will be quick: every wanted instance at the WFC menu.
        n_ok = sum(1 for k in want if k in self.workers and self.workers[k].at_wfc)
        ready = n_ok == len(want) and len(want) > 0
        msg = READY_MSG if ready else f"Getting the game ready ({n_ok}/{len(want)} online)..."
        self._write_status(ready, n_ok, want)
        if msg != snap.get("message") and now - self.msg_posted > 1.0:
            self.msg_posted = now
            try:
                lobby.post_update(message=msg)
            except lobby.LobbyError as e:
                self.log(f"lobby message failed: {e}")

    def _reset(self, why: str):
        self.reset_posted = time.time()
        self.log(f"RESET: {why}; POST /api/nav/reset, preparing again")
        try:
            lobby.post_reset(message="Back in the lobby.")
        except lobby.LobbyError as e:
            self.log(f"reset failed: {e}")
