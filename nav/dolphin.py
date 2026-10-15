"""Instance I/O for the navigator: process, window, captures, controller lines.

  Instance(k).alive()          recorded Dolphin pid is live and is ours (-u instances/i<k>)
  Instance(k).grab()           full window capture (1016x1280x3 uint8) or None
  Instance(k).press(B, pad)    PRESS B, hold, RELEASE B on Pipes/p<pad> (pad 2: player B of a
                               two-player instance)
  Instance(k).move(D, pad)     one stick flick: SET MAIN to the direction, then centre
  Instance(k).set_audio(on)    deterministic volume 100 / 0 with the Volume Up / Down hotkeys
  Instance(k).launch(log)      run/launch.sh k <mode> [--setup]
  Instance(k).relaunch(log)    stop exactly this instance (run/down.sh --instance k --force)
                               and start it again

Profiles i1..i6 are single-player (16:9), i7..i12 two-player split screen (4:3).

Window ids come from run/bin/winlist (CGWindowListCopyWindowInfo; `wincap list` can hang).
Every external call has a timeout.
Frames come from `screencapture -x -o -t bmp -l <windowid>` (about 0.1 s; works
when the window is covered). Dolphin is started only by run/launch.sh and stopped
only by run/down.sh.
"""
from __future__ import annotations

import errno
import os
import subprocess
import tempfile
import time

import numpy as np
from PIL import Image

import localconf

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUN_DIR = os.path.join(ROOT, "run")
PID_DIR = os.path.join(RUN_DIR, "pids")
INST_ROOT = os.path.join(ROOT, "instances")
WINLIST = os.path.join(RUN_DIR, "bin", "winlist")
WINLIST_SRC = os.path.join(RUN_DIR, "src", "winlist.swift")
DOLPHIN = localconf.dolphin_binary()   # DOLPHIN_APP in mkparty.conf (run/lib.sh: same)

HOTKEY_STEPS = 34            # Volume Up / Down change the volume by 3 (0..100): 34 steps = full range
VOLUME_UP, VOLUME_DOWN = "Y", "Z"   # Pipes/hk buttons (run/provision.sh Hotkeys.ini)
DIRS = {"LEFT": (0.0, 0.5), "RIGHT": (1.0, 0.5), "UP": (0.5, 1.0), "DOWN": (0.5, 0.0)}
STREAM_STALE = 4.0           # seconds without a new stream frame before falling back
PRESS_HOLD = 0.10
MOVE_HOLD = 0.12


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except OSError as e:
        return e.errno == errno.EPERM


def cmdline(pid: int) -> str:
    try:
        return subprocess.run(["ps", "-p", str(pid), "-o", "command="], capture_output=True,
                              text=True, timeout=5).stdout.strip()
    except Exception:
        return ""


def _winlist_bin():
    """run/bin/winlist (built from run/src/winlist.swift when missing)."""
    if not os.path.exists(WINLIST):
        os.makedirs(os.path.dirname(WINLIST), exist_ok=True)
        subprocess.run(["swiftc", "-O", WINLIST_SRC, "-o", WINLIST], timeout=180, check=True)
    return WINLIST


def list_windows():
    """[(window id, pid, title, w, h, on_screen)] of Dolphin windows (run/bin/winlist).
    `capture/bin/wincap list` is not used: it has been seen to hang."""
    try:
        r = subprocess.run([_winlist_bin()], capture_output=True, text=True, timeout=5)
    except Exception:
        return None                   # unknown (timed out under load): not "no windows"
    if r.returncode != 0:
        return None
    out = r.stdout
    rows = []
    for line in out.splitlines():
        f = line.split("\t")
        if len(f) < 6:
            continue
        try:
            rows.append((int(f[0]), int(f[1]), f[2], int(f[3]), int(f[4]), f[5] == "onScreen"))
        except ValueError:
            continue
    return rows


def window_for_pid(pid: int):
    """The RMCE01 game window of pid (an on-screen one is preferred), or None."""
    best, rank = None, -1.0
    rows = list_windows()
    if rows is None:
        return None
    for wid, wpid, title, w, h, on in rows:
        if wpid != pid or "RMCE01" not in title:
            continue
        r = (1e12 if on else 0.0) + w * h
        if r > rank:
            best, rank = wid, r
    return best


def inst_mode(k: int) -> str:
    """i1..i6: single-player profiles; i7..i12: two-player profiles (run/lib.sh inst_mode)."""
    return "2p" if k >= 7 else "1p"


class Instance:
    def __init__(self, k: int, setup: bool = False):
        self.k = k
        self.mode = inst_mode(k)
        self.setup = setup            # booted in 4:3 for profile setup (launch.sh --setup)
        self.dir = os.path.join(INST_ROOT, f"i{k}")
        self.fifo = os.path.join(self.dir, "Pipes", "p1")
        self.fifos = {1: self.fifo, 2: os.path.join(self.dir, "Pipes", "p2")}
        self.hk = os.path.join(self.dir, "Pipes", "hk")
        self.audio = None             # last volume set by set_audio (None: as booted = muted)
        self.launched_at = 0.0        # when launch() last started Dolphin
        self.pidfile = os.path.join(PID_DIR, f"i{k}.pid")
        self.wid = None
        self.pid = None
        self.fail_streak = 0          # consecutive failed captures
        self.last_err = ""
        self.frame_file = os.path.join(RUN_DIR, "frames", f"i{k}.bmp")
        self._last_mtime = 0
        self._last_frame = None
        self.source = ""
        self._tmp = os.path.join(tempfile.gettempdir(), f"mkwnav-{os.getpid()}-i{k}.bmp")

    # ----- process -----
    def recorded_pid(self):
        try:
            return int(open(self.pidfile).read().strip())
        except (OSError, ValueError):
            return None

    def alive(self) -> bool:
        """The recorded pid is a live Dolphin started for this instance directory."""
        pid = self.recorded_pid()
        if pid is None or not pid_alive(pid):
            return False
        if pid == self.pid:
            return True               # verified before; the pid file has not changed
        c = cmdline(pid)
        if DOLPHIN in c and f"-u {self.dir}" in c:
            self.pid = pid
            return True
        return False

    def refresh_window(self):
        """True: window found; False: the window list has no game window of this pid;
        None: the window list could not be read (winlist timed out under load)."""
        pid = self.recorded_pid()
        if not pid:
            self.wid = None
            return False
        rows = list_windows()
        if rows is None:
            return None
        self.wid = None
        best = -1.0
        for wid, wpid, title, w, h, on in rows:
            if wpid == pid and "RMCE01" in title and (1e12 if on else 0.0) + w * h > best:
                self.wid, best = wid, (1e12 if on else 0.0) + w * h
        return self.wid is not None

    def alert_window(self):
        """Window id of a Dolphin alert panel of this instance ("Invalid read from ...":
        a small untitled 420x203 window; emulation is paused behind it), or None."""
        pid = self.recorded_pid()
        for wid, wpid, title, w, h, _on in list_windows() or []:
            if wpid == pid and title == "" and 380 <= w <= 480 and 150 <= h <= 260:
                return wid
        return None

    def cpu(self) -> float:
        """%CPU of the Dolphin process (a running game is above 15, a dead one near 1)."""
        try:
            out = subprocess.run(["ps", "-p", str(self.recorded_pid()), "-o", "%cpu="],
                                 capture_output=True, text=True, timeout=3).stdout.strip()
            return float(out)
        except Exception:
            return -1.0

    # ----- frames -----
    def grab(self):
        """The newest full-window frame as an array, or None (see last_err).

        Preferred source: run/frames/i<k>.bmp, which the slot's stream pipeline rewrites
        about 12 times a second (no extra capture cost). Each call returns a frame that
        is newer than the one returned before. When that file is stale (no stream
        running) the window is captured with screencapture instead, which is slow when
        several instances are streaming (0.5-1.5 s)."""
        t0 = time.time()
        while True:
            try:
                st = os.stat(self.frame_file)
            except OSError:
                break
            age = time.time() - st.st_mtime
            if age > STREAM_STALE:
                if self._last_frame is not None and self.alive():
                    self.source = "stream-same"     # static or stopped picture; see Worker._frozen
                    time.sleep(0.2)
                    return self._last_frame
                break
            if st.st_mtime_ns != self._last_mtime:
                try:
                    with Image.open(self.frame_file) as im:
                        frame = np.asarray(im.convert("RGB"))
                except Exception:
                    time.sleep(0.01)
                    continue
                self._last_mtime = st.st_mtime_ns
                self._last_frame = frame
                self.fail_streak = 0
                self.source = "stream"
                return frame
            if time.time() - t0 > 0.3:
                # No new frame: the capture only delivers frames when the picture changes
                # (and fewer when the window is off screen), so the screen is static.
                if self._last_frame is not None:
                    self.source = "stream-same"
                    return self._last_frame
                break
            time.sleep(0.01)
        self.source = "screencapture"
        return self.grab_window()

    def grab_window(self):
        """One screencapture of the game window, or None (see last_err)."""
        if self.wid is None and not self.refresh_window():
            self.fail_streak += 1
            self.last_err = "no game window"
            return None
        try:
            if os.path.exists(self._tmp):
                os.remove(self._tmp)
            r = subprocess.run(["screencapture", "-x", "-o", "-t", "bmp", "-l", str(self.wid), self._tmp],
                               capture_output=True, text=True, timeout=5)
            if r.returncode != 0 or not os.path.exists(self._tmp) or os.path.getsize(self._tmp) < 1000:
                raise RuntimeError((r.stderr or "empty capture").strip()[:120])
            with Image.open(self._tmp) as im:
                frame = np.asarray(im.convert("RGB"))
        except Exception as e:  # screencapture failed or gave a broken file
            self.fail_streak += 1
            self.last_err = f"screencapture window {self.wid}: {e}"
            self.wid = None       # re-resolve next time (the window may have been replaced)
            return None
        self.fail_streak = 0
        return frame

    @staticmethod
    def save_png(frame, path: str):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        Image.fromarray(frame).save(path, compress_level=1)

    @property
    def aspect43(self) -> bool:
        """The game runs in 4:3 (two-player profile, or any profile booted for setup)."""
        return self.mode == "2p" or self.setup

    # ----- controller -----
    def send(self, line: str, pad: int = 1, fifo: str | None = None):
        """Write one line to the pad's FIFO without blocking. Raises OSError if nobody reads it."""
        fd = os.open(fifo or self.fifos[pad], os.O_WRONLY | os.O_NONBLOCK)
        try:
            os.write(fd, (line + "\n").encode())
        finally:
            os.close(fd)

    def press(self, button: str, pad: int = 1, hold: float = PRESS_HOLD):
        self.send(f"PRESS {button}", pad)
        time.sleep(hold)
        self.send(f"RELEASE {button}", pad)

    def move(self, direction: str, pad: int = 1, hold: float = MOVE_HOLD):
        x, y = DIRS[direction]
        self.send(f"SET MAIN {x:g} {y:g}", pad)
        time.sleep(hold)
        self.send("SET MAIN 0.5 0.5", pad)

    def move_pads(self, moves, hold: float = MOVE_HOLD):
        """Flick several pads at once: moves = [(pad, direction), ...]."""
        for pad, d in moves:
            x, y = DIRS[d]
            self.send(f"SET MAIN {x:g} {y:g}", pad)
        time.sleep(hold)
        for pad, _d in moves:
            self.send("SET MAIN 0.5 0.5", pad)

    def neutral(self):
        """Release everything the navigator may have held."""
        for pad in ((1, 2) if self.mode == "2p" else (1,)):
            try:
                self.send("RELEASE A", pad)
                self.send("RELEASE B", pad)
                self.send("SET MAIN 0.5 0.5", pad)
            except OSError:
                pass

    def set_audio(self, on: bool) -> bool:
        """Volume 100 (on) or 0 (off), whatever the state before: 34 presses of Volume Up
        (which also clears Dolphin's "Muted") or Volume Down on the hotkey FIFO. Dolphin
        boots muted (run/launch.sh). Returns False when the FIFO has no reader."""
        b = VOLUME_UP if on else VOLUME_DOWN
        try:
            for _ in range(HOTKEY_STEPS):
                self.send(f"PRESS {b}", fifo=self.hk)
                time.sleep(0.04)
                self.send(f"RELEASE {b}", fifo=self.hk)
                time.sleep(0.04)
        except OSError:
            return False
        self.audio = on
        return True

    # ----- recovery -----
    def stop(self, log=print) -> bool:
        r = subprocess.run([os.path.join(RUN_DIR, "down.sh"), "--instance", str(self.k), "--force"],
                           capture_output=True, text=True, timeout=60)
        for line in (r.stdout + r.stderr).strip().splitlines():
            log(f"i{self.k} down.sh: {line}")
        self.wid = None
        self.pid = None
        return r.returncode == 0

    def launch(self, log=print, wait_window: float = 60.0) -> bool:
        args = [os.path.join(RUN_DIR, "launch.sh"), str(self.k), self.mode] + (["--setup"] if self.setup else [])
        r = subprocess.run(args, capture_output=True, text=True, timeout=60)
        self.audio = None
        self.launched_at = time.time()
        for line in (r.stdout + r.stderr).strip().splitlines():
            log(f"i{self.k} launch.sh: {line}")
        if r.returncode != 0:
            return False
        t0 = time.time()
        while time.time() - t0 < wait_window:
            if self.alive() and self.refresh_window() is True:
                self.fail_streak = 0
                return True
            time.sleep(0.5)
        return False

    def relaunch(self, log=print) -> bool:
        self.stop(log)
        return self.launch(log)
