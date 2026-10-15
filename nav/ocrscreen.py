"""Screen recognition WITHOUT templates: reads the game's text with Apple's Vision OCR.

Used only to bootstrap (nav/calibrate.py, `nav.py setup` on an install without screen
templates): it tells the screens apart by the text they show (title strip, hint line,
name labels, dialog texts) plus the structural readers of screen.py / screen43.py that
need no templates (white button labels, the member rows, the race HUD, ...). It returns
the same states and cursors as screen.classify_frame (16:9) / screen43.classify_frame
(4:3), so driver.Worker drives with it unchanged; it is slower (0.1-0.3 s per frame)
and is not used at run time once the templates exist.

    OcrScreen(aspect43).classify_frame(frame) -> screen.Result   (detail["ocr"]: the texts)

The OCR is run/bin/ocr (run/src/ocr.swift, VNRecognizeTextRequest, offline), one server
process per OcrScreen, fed with BMP files.
"""
from __future__ import annotations

import difflib
import json
import os
import re
import subprocess
import tempfile
import threading
import unicodedata

import numpy as np
from PIL import Image

import screen as S
import screen43 as S4

NAV_DIR = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(NAV_DIR)
OCR_BIN = os.path.join(ROOT, "run", "bin", "ocr")
OCR_SRC = os.path.join(ROOT, "run", "src", "ocr.swift")

# Cyrillic / Greek look-alikes the recogniser sometimes returns for Latin letters.
_HOMO = str.maketrans("аеорсухкмтвнАЕОРСУХКМТВНіІјЈ", "aeopcyxkmtbhAEOPCYXKMTBHiIjJ")


def norm(s: str) -> str:
    """Lower-case letters, digits and single spaces only (accents and look-alikes folded)."""
    s = unicodedata.normalize("NFKD", s.translate(_HOMO))
    s = "".join(c for c in s if not unicodedata.combining(c))
    s = re.sub(r"[^a-z0-9]+", " ", s.lower())
    return s.strip()


def ocr_bin() -> str:
    if not os.path.exists(OCR_BIN) or os.path.getmtime(OCR_BIN) < os.path.getmtime(OCR_SRC):
        os.makedirs(os.path.dirname(OCR_BIN), exist_ok=True)
        subprocess.run(["swiftc", "-O", OCR_SRC, "-o", OCR_BIN], check=True, timeout=600)
    return OCR_BIN


class Ocr:
    """One run/bin/ocr server process (thread-safe)."""

    def __init__(self):
        self._p = None
        self.errors = 0
        self._lock = threading.Lock()
        fd, self._tmp = tempfile.mkstemp(prefix="mkwocr-", suffix=".bmp")
        os.close(fd)

    def _proc(self):
        if self._p is None or self._p.poll() is not None:
            self._p = subprocess.Popen([ocr_bin()], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=subprocess.DEVNULL, text=True, bufsize=1)
        return self._p

    def read(self, frame: np.ndarray, crop=None) -> list:
        """[(text, (x0, y0, x1, y1))] in frame pixels."""
        with self._lock:
            Image.fromarray(frame).save(self._tmp, format="BMP")
            p = self._proc()
            req = self._tmp + ("\t" + " ".join(str(int(v)) for v in crop) if crop else "")
            try:
                p.stdin.write(req + "\n")
                p.stdin.flush()
                line = p.stdout.readline()
            except (OSError, ValueError):
                self._p = None
                return []
        try:
            d = json.loads(line)
        except ValueError:
            self._p = None              # the helper died: started again with the next frame
            self.errors += 1
            return []
        if isinstance(d, dict):
            self.errors += 1            # Vision failed on this frame (e.g. while it reloads its model)
            return []
        return [(x["t"], tuple(x["b"])) for x in d]

    def close(self):
        if self._p is not None:
            try:
                self._p.stdin.close()
                self._p.wait(timeout=3)
            except Exception:
                self._p.kill()
            self._p = None
        try:
            os.remove(self._tmp)
        except OSError:
            pass


def warm_up(log=print):
    """Load Vision's text model once (the first request after a while can take ~40 s)."""
    t0 = __import__("time").time()
    o = Ocr()
    try:
        o.read(np.zeros((64, 64, 3), np.uint8))
    finally:
        o.close()
    log(f"ocr: text recognition ready ({__import__('time').time() - t0:.1f} s)")


class Items:
    """OCR results in PICTURE coordinates with normalised text."""

    def __init__(self, raw, pic_y0: int):
        self.items = []
        for t, (x0, y0, x1, y1) in raw:
            if y1 <= pic_y0:            # the window title bar
                continue
            self.items.append((norm(t), t, (x0, y0 - pic_y0, x1, y1 - pic_y0)))
        self.text = " | ".join(n for n, _t, _b in self.items)

    def has(self, *phrases) -> bool:
        return any(norm(p) in self.text for p in phrases)

    def within(self, roi, grow: int = 0):
        """Items whose centre lies in roi (picture coordinates) grown by `grow`."""
        x0, y0, x1, y1 = roi
        out = []
        for n, t, (a, b, c, d) in self.items:
            cx, cy = (a + c) / 2, (b + d) / 2
            if x0 - grow <= cx <= x1 + grow and y0 - grow <= cy <= y1 + grow:
                out.append((n, t, (a, b, c, d)))
        return out

    def text_in(self, roi, grow: int = 0) -> str:
        return " ".join(n for n, _t, _b in self.within(roi, grow))

    def compact(self):
        return [(t, b) for _n, t, b in self.items]


def _best_name(text: str, names: dict, cutoff: float = 0.75):
    """The catalog id whose display name matches `text` best (or None)."""
    if not text:
        return None
    best, score = None, 0.0
    for cid, name in names.items():
        n = norm(name)
        r = 1.0 if n == text else difflib.SequenceMatcher(None, n, text).ratio()
        if n in text.split(" | ") or (len(n) > 3 and n == text.strip()):
            r = 1.0
        if r > score:
            best, score = cid, r
    return best if score >= cutoff else None


_cat = None


def catalog():
    global _cat
    if _cat is None:
        with open(S.CATALOG) as f:
            d = json.load(f)
        _cat = {
            "characters": {c["id"]: c["name"] for c in d["characters"] if c.get("unlocked")},
            "vehicles": {v["id"]: v["name"] for v in d["vehicles"] if v.get("unlocked")},
            "cups": {c["id"]: c["name"] for c in d["cups"]},
            "char_grid": {c["id"]: tuple(c["grid"]) for c in d["characters"] if "grid" in c},
        }
    return _cat


# Hint line texts (bottom right) -> cursor.
MAIN_HINTS = [("play a grand prix", "single"), ("play online with a friend", "wfc2"), ("play online", "wfc"),
              ("race or battle with", "multi"), ("view rankings", "channel"), ("check records", "license")]
DRIFT_HINTS = [("drift automatically", "auto"), ("drift using buttons", "manual"), ("explanation for drifting", "help")]

# The title texts (top left) -> screen.py title keys.
TITLES = [("main menu", "main_menu"), ("select license", "license_select"), ("create a license", "create_license"),
          ("choose a mii", "choose_mii"), ("controller registration", "controller_reg"),
          ("characters", "characters"), ("vehicles", "vehicles"), ("drift", "drift"),
          ("nintendo wfc", "wfc"), ("worldwide vs", "searching"), ("worldwide", "ww_mode"),
          ("check members", "check_members"), ("course poll", "course_poll"), ("courses", "courses")]


def dialog_icon(pic, aspect43: bool) -> int:
    """Green pixels of the (A) prompt icon at the bottom right of a game dialog."""
    x0, y0, x1, y1 = (S4 if aspect43 else S).A_ICON_ROI
    sub = pic[y0:y1, x0:x1].astype(np.int16)
    return int(((sub[..., 1] > 150) & (sub[..., 1] - np.maximum(sub[..., 0], sub[..., 2]) > 60)).sum())


class OcrScreen:
    """classify_frame() with the semantics of screen.py (16:9) or screen43.py (4:3)."""

    def __init__(self, aspect43: bool, ocr: Ocr | None = None):
        self.aspect43 = aspect43
        self.mod = S4 if aspect43 else S
        self.ocr = ocr or Ocr()
        self.last_items = None

    # ------------------------------------------------------------------ helpers
    def _title(self, it: Items):
        x0, y0, x1, y1 = self.mod.TITLE_ROI
        cands = [n for n, _t, (a, b, c, d) in it.items if a < x0 + 80 and y0 - 20 <= (b + d) / 2 <= y1 + 10]
        txt = " ".join(cands)
        for key, k in TITLES:
            if txt.startswith(key) or (key in txt and k not in ("drift", "courses", "ww_mode")):
                return k, txt
        return (None, txt) if txt else (None, "")

    def _hint(self, it: Items):
        x0, y0, x1, y1 = self.mod.HINT_ROI
        return " ".join(n for n, _t, (a, b, c, d) in it.items
                        if a > x0 - 200 and y0 - 25 <= (b + d) / 2 <= y1 + 20)

    def _back(self, it: Items) -> bool:
        x0, y0, x1, y1 = self.mod.BACK_ROI
        return any("back" in n.split() or n.endswith("back") for n, _t, _b in it.within((x0 - 60, y0 - 25, x1 + 40, y1 + 25)))

    # ------------------------------------------------------------------ classify
    def classify_frame(self, frame) -> S.Result:
        if frame.shape[:2] != (S.FRAME_H, S.FRAME_W):
            return S.Result("unknown", 0.0, how=f"bad-frame-size {frame.shape[1]}x{frame.shape[0]}")
        pic = self.mod.picture_of(frame)
        st = S.colour_stats(pic)
        d = {"stats": {k: round(v, 3) for k, v in st.items()}}
        if st["lum"] < 4:
            return S.Result("black", 1.0, how="black", detail=d)
        if not self.aspect43 and S.emu_sheet(frame):
            return S.Result("emu_alert", 1.0, how="emu-sheet", detail=d)
        if not self.aspect43 and not S.frame_ok(frame):
            return S.Result("unknown", 0.0, how="bad-frame-letterbox", detail=d)
        it = Items(self.ocr.read(frame), self.mod.PIC_Y0)
        self.last_items = it
        d["ocr"] = it.compact()
        r = self._classify(pic, it, d, st)
        return r

    def _classify(self, pic, it: Items, d, st) -> S.Result:
        m = self.mod
        R = S.Result
        if it.has("invalid read from"):
            return R("emu_alert" if not self.aspect43 else "unknown", 1.0, how="emu-text", detail=d)
        # Game dialogs: the green (A) icon and a white box.
        g = dialog_icon(pic, self.aspect43)
        if g > (1500 if self.aspect43 else 700):
            x0, y0, x1, y1 = m.DIALOG_WHITE_ROI
            white = float((pic[y0:y1, x0:x1].min(axis=2) > 215).mean())
            d["dialog"] = {"green": g, "white": round(white, 3)}
            if white > 0.55:
                if it.has("welcome to"):
                    return R("welcome", 1.0, how="ocr-dialog", detail=d)
                if it.has("you were disconnected"):
                    return R("disconnected_dialog", 1.0, how="ocr-dialog", detail=d)
                return R("error_dialog", 1.0, how="ocr-dialog", detail=d)
        if it.has("vote for the next course"):
            cur = S.pick_white(pic, m.VOTE_BOXES)
            if cur is None:
                return R("unknown", 0.9, how="vote-prompt-no-cursor", detail=d)
            return R("vote_prompt", 1.0, cursor=cur, how="ocr-vote", detail=d)
        if it.has("next race") and it.has("quit game"):
            cur = S.pick_white(pic, m.NEXT_RACE_BOXES, min_count=300)
            if cur is not None:
                return R("next_race_menu", 1.0, cursor=cur, how="ocr-next-race", detail=d)
        x0, y0, x1, y1 = m.ROIS["t_results"]
        if "1st" in it.text_in((x0, y0, x1, y1), 25) and it.has("2nd"):
            return R("results", 1.0, how="ocr-results", detail=d)
        if it.has("wrist strap", "adequate room", "wii remote jacket"):
            return R("health", 1.0, how="ocr-text", detail=d)
        if it.has("press the a button") and not it.has("second player"):
            return R("title", 1.0, how="ocr-text", detail=d)
        if it.has("preparing the race", "preparing therace", "paring the race"):
            return R("unknown" if self.aspect43 else "preparing", 1.0, how="ocr-preparing", detail=d)
        if it.has("please wait a moment"):
            return R("unknown" if self.aspect43 else "please_wait", 1.0, how="ocr-pleasewait", detail=d)
        if it.has("opponents have been chosen") and self.aspect43:
            return R("opponents_chosen", 1.0, how="ocr-text", detail=d)
        if self.aspect43 and it.has("you will not be able to play"):
            cur = S.pick_white(pic, S4.YESNO_BOXES, min_count=300)
            if cur is not None:
                return R("wont_play", 1.0, cursor=cur, how="ocr-text+label", detail=d)

        tk, ttxt = self._title(it)
        d["title_text"] = ttxt
        d["hint_text"] = self._hint(it)
        if tk is not None:
            return self._menu(pic, it, tk, d)
        chrome = S.has_chrome(pic)
        d["chrome"] = chrome
        if chrome:
            if ttxt and self._back(it):
                return R("other_menu", 0.9, how="ocr-back", detail=d)
            return R("unknown", 0.0, how="menu-frame-no-title", detail=d)
        if not self.aspect43:
            if S.race_hud(pic):
                return R("race", 0.9, how="race-hud", detail=d)
        if st["sat"] > 0.30 and st["lum"] > 40:
            return R("race", 0.5, how="no-frame-colour", detail=d)
        return R("unknown", 0.0, how="no-match", detail=d)

    def _menu(self, pic, it: Items, tk, d) -> S.Result:
        m = self.mod
        R = S.Result
        two = self.aspect43

        def unknown(why):
            return R("unknown", 0.9, how=f"ocr-{tk}:{why}", detail=d)

        hint = d.get("hint_text", "")
        if tk == "main_menu":
            cur = next((c for p, c in MAIN_HINTS if p in hint), None)
            if cur is None:
                return unknown("no-hint")
            return R("main_menu", 1.0, cursor=cur, how="ocr-title+hint", detail=d)
        if tk == "license_select":
            return R("license_select", 1.0, how="ocr-title", detail=d)
        if tk in ("create_license", "choose_mii", "controller_reg") and not two:
            return unknown("4:3-only")
        if tk == "create_license":
            return R("create_license", 1.0, how="ocr-title", detail=d)
        if tk == "choose_mii":
            x0, y0, x1, y1 = (600, 700, 1200, 800)
            sub = pic[y0:y1, x0:x1].astype(np.int16)
            blue = float(((sub[..., 2] > 150) & (sub[..., 0] < 120)).mean())
            return R("choose_mii", 1.0, cursor="p2" if blue > 0.3 else "p1", how="ocr-title+bg", detail=d)
        if tk == "controller_reg":
            cur = S.pick_white(pic, S4.REG_BOXES, min_count=300)
            if cur is None:
                if "second player" not in hint:
                    return unknown("no-cursor")
                cur = "wait"
            return R("controller_reg", 1.0, cursor=cur, how="ocr-title+label", detail=d)
        if tk == "wfc":
            if two and it.has("both connect to nintendo wfc"):
                return R("connect2", 1.0, how="ocr-title+text", detail=d)
            if two and "next" in it.text_in(S4.ROIS["t_next_btn"], 20).split():
                return R("wfc_notice", 1.0, how="ocr-title+next", detail=d)
            if two and it.has("permit data to be"):
                cur = S.pick_white(pic, S4.PERMIT_BOXES, min_count=300)
                if cur is None:
                    return unknown("permit-no-cursor")
                return R("permit_prompt", 1.0, cursor=cur, how="ocr-title+text", detail=d)
            if it.has("connecting to nintendo wfc"):
                return R("connecting", 1.0, how="ocr-title+text", detail=d)
            if it.has("disconnect from nintendo wfc"):
                return unknown("disconnect-prompt")
            cur = S.pick_white(pic, m.WFC_BOXES)
            if cur is None or not it.has("worldwide"):
                return unknown("no-cursor")
            return R("wfc_menu", 1.0, cursor=cur, how="ocr-title+label", detail=d)
        if tk == "ww_mode":
            cur = S.pick_white(pic, m.WW_BOXES)
            if cur is None or not it.has("battle"):
                return unknown("no-cursor")
            return R("ww_mode", 1.0, cursor=cur, how="ocr-title+label", detail=d)
        if tk == "searching":
            if it.has("opponents have been chosen") and two:
                return R("opponents_chosen", 1.0, how="ocr-text", detail=d)
            return R("searching", 1.0, how="ocr-title", detail=d)
        if tk == "check_members":
            d["members"] = m.member_count(pic)
            return R("check_members", 1.0, how="ocr-title", detail=d)
        if tk == "course_poll":
            d["poll"] = ("waiting" if "waiting for everyone" in hint else
                         "selected" if "has been selected" in hint else None)
            return R("course_poll", 1.0, how="ocr-title", detail=d)
        cat = catalog()
        if tk == "characters":
            if not two:
                n = _best_name(it.text_in(S.CHAR_NAME_ROI, 25), cat["characters"])
                if n is None:
                    return unknown("no-name")
                return R("characters", 1.0, cursor=n, how="ocr-title+name", detail=d)
            n1 = _best_name(it.text_in(S4.CHAR_LABELS[0], 25), cat["characters"])
            n2 = _best_name(it.text_in(S4.CHAR_LABELS[1], 25), cat["characters"])
            if n1 is None or n2 is None:
                return unknown("no-name")
            ok = False
            rc = cat["char_grid"].get(n1)
            if rc is not None:
                r, c = rc
                cell = (S4.CHAR_CELL_X[c], S4.CHAR_CELL_Y[r], S4.CHAR_CELL_X[c] + S4.CHAR_CELL_W,
                        S4.CHAR_CELL_Y[r] + S4.CHAR_CELL_H)
                ok = "ok" in it.text_in(cell, 10).split()
            return R("characters", 1.0, cursor=(n1, n2, ok), how="ocr-title+names", detail=d)
        if tk == "vehicles":
            if not two:
                n = _best_name(it.text_in(S.VEH_NAME_ROI, 25), cat["vehicles"])
                if n is None:
                    return unknown("no-name")
                return R("vehicles", 1.0, cursor=n, how="ocr-title+name", detail=d)
            n1 = _best_name(it.text_in(S4.VEH_LABELS[0], 25), cat["vehicles"])
            n2 = _best_name(it.text_in(S4.VEH_LABELS[1], 25), cat["vehicles"])
            if n1 is None or n2 is None:
                return unknown("no-name")
            return R("vehicles", 1.0, cursor=(n1, n2, not S4.p1_arrows(pic)), how="ocr-title+names", detail=d)
        if tk == "drift":
            if two:
                c1, ok1 = S4.drift_side(pic, S4.DRIFT_P1)
                c2, _ = S4.drift_side(pic, S4.DRIFT_P2)
                if c1 is None or c2 is None:
                    return unknown("no-cursor")
                return R("drift", 1.0, cursor=(c1, c2, ok1), how="ocr-title+labels", detail=d)
            cur = next((c for p, c in DRIFT_HINTS if p in hint), None)
            lab = S.pick_white(pic, S.DRIFT_BOXES)
            if cur is None or (cur != "help" and lab != cur) or (cur == "help" and lab is not None):
                return unknown("no-cursor")
            return R("drift", 1.0, cursor=cur, how="ocr-title+hint+label", detail=d)
        if tk == "courses":
            if two:
                lst = S4.read_list(pic)
                if lst is not None:
                    return R("courses_list", 1.0, cursor=lst, how="ocr-title+row", detail=d)
                cup, _s = S4.read_cup(pic)
                if cup is not None and norm(cat["cups"][cup]) in it.text:
                    return R("courses_cups", 1.0, cursor=cup, how="ocr-title+cup-label", detail=d)
                return unknown("no-cursor")
            # The cup name is either the banner over the course list or the label under
            # the highlighted cup: whichever place its text is nearest to.
            def centre(r):
                return (r[0] + r[2]) / 2, (r[1] + r[3]) / 2
            bx, by = centre(S.CUP_LIST_BANNER_ROI)
            for cup in S.CUP_ORDER:
                name = norm(cat["cups"][cup])
                lx, ly = centre(S.cup_label_roi(cup))
                for n, _t, b in it.items:
                    if difflib.SequenceMatcher(None, n, name).ratio() < 0.8:
                        continue
                    cx, cy = centre(b)
                    d_banner, d_label = abs(cx - bx) + abs(cy - by), abs(cx - lx) + abs(cy - ly)
                    if min(d_banner, d_label) > 70:
                        continue
                    row = S.pick_white(pic, {i: bx_ for i, bx_ in enumerate(S.LIST_ROWS)}, min_count=350)
                    if d_banner < d_label and row is not None:
                        return R("courses_list", 1.0, cursor=(cup, row), how="ocr-title+banner+row", detail=d)
                    if d_label <= 70 and (row is None or d_label < d_banner):
                        # (the label of a cup in the second row is close to the banner's place)
                        return R("courses_cups", 1.0, cursor=cup, how="ocr-title+cup-label", detail=d)
                    return unknown("list-no-row")
            return unknown("no-cursor")
        return unknown("unhandled")
