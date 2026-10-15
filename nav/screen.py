"""Screen classifier and cursor readers for Mario Kart Wii (RMCE01) in Dolphin.

Input: a full window capture, 1280x1016 RGB (56 px macOS title bar, then a 4:3
area in which the 16:9 game picture occupies frame rows 186..886). Everything
below works on the PICTURE (700x1280x3 uint8), in picture coordinates.

    frame_ok(frame)        -> bool   capture has the expected geometry (letterbox is black)
    picture_of(frame)      -> picture
    classify(pic)          -> Result(state, conf, cursor, how, detail)

How a screen is recognised (classify):
  1. Emulator alert (Dolphin's own "Invalid read" sheet) by its text.
  2. Game dialogs: the green (A) prompt icon plus a white box in the middle of
     the picture. The text decides which: welcome / disconnected / (any other
     text, any error code) error_dialog.
  3. "Vote for the next course?" prompt by its text.
  4. Full-screen texts: health warning, title logo, "Preparing the race...".
  5. Menu screens by the title strip top-left (normalised correlation against
     stored crops, with a margin over the runner-up), then a per-screen body
     check that also yields the cursor. A title whose body check fails is a
     transition and comes back as "unknown".
  6. No menu frame at all (top strip is not the white bar): "race" when the
     picture is colourful, otherwise unknown.

Cursors are read from TEXT wherever the game shows one (the highlighted
button pulses, so colour is not a stable signal): the hint line bottom-right,
the character / vehicle name label, the cup name under the highlighted cup.
Button lists (Worldwide/Regional/Friends, VS/Battle, Vote/Random, the course
list) are read from the label colour: the highlighted label is white, the
others grey.

Templates are cut from captures of the user's own game by `./mkparty calibrate`
(nav/calibrate.py; see TEMPLATES for what each one is cut from) into
<assets>/templates.npz (bank_path). Installs from before calibration may instead have
nav/templates.npz built from hand-labelled nav/refs (`nav.py build-templates`).
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

import numpy as np
from PIL import Image

import localconf

NAV_DIR = os.path.dirname(os.path.abspath(__file__))
REFS_DIR = os.path.join(NAV_DIR, "refs")          # hand-labelled captures (legacy; not in git)


def bank_path(name: str) -> str:
    """Where a template bank is read from: the assets folder (MKPARTY_ASSETS, default
    <root>/assets, filled by `./mkparty calibrate` from the user's own game); without one
    there, a bank in nav/ (installs from before calibration existed). An explicitly set
    MKPARTY_ASSETS is always used as it is."""
    p = os.path.join(localconf.assets_dir(), name)
    if os.path.exists(p) or localconf.assets_explicit():
        return p
    legacy = os.path.join(NAV_DIR, name)
    return legacy if os.path.exists(legacy) else p


BANK_FILE = bank_path("templates.npz")


def assets_missing(bank_file: str, refs_dir: str = "") -> str:
    """The error for an install whose screen templates have not been generated yet."""
    return (f"screen templates missing: {bank_file}\n"
            "They are generated on this Mac from your own copy of the game (no game imagery is\n"
            "distributed): run `./mkparty setup --profiles` (first time) or `./mkparty calibrate`\n"
            "(see README.md, 'Game assets').")
CATALOG = os.path.join(os.path.dirname(NAV_DIR), "party", "web", "catalog.json")

FRAME_W, FRAME_H = 1280, 1016
PIC_Y0, PIC_H, PIC_W = 186, 700, 1280

STATES = [
    "health", "title", "license_select", "main_menu", "connecting", "welcome",
    "wfc_menu", "ww_mode", "characters", "vehicles", "drift", "searching",
    "opponents_chosen", "preparing", "check_members", "vote_prompt",
    "courses_cups", "courses_list", "course_poll", "race", "results",
    "error_dialog", "disconnected_dialog", "emu_alert", "other_menu", "black",
    "unknown",
]

# ---------------------------------------------------------------- regions
TITLE_ROI = (50, 38, 330, 74)
HINT_ROI = (760, 604, 1262, 648)
BACK_ROI = (96, 612, 292, 660)
A_ICON_ROI = (1116, 556, 1178, 618)
DIALOG_WHITE_ROI = (420, 250, 860, 400)
CHAR_NAME_ROI = (100, 512, 384, 560)
VEH_NAME_ROI = (70, 508, 520, 572)
CUP_LIST_BANNER_ROI = (700, 205, 1010, 260)
TOP_STRIP = (20, 6, 420, 24)

ROIS = {
    "title": TITLE_ROI,
    "hint": HINT_ROI,
    "back": BACK_ROI,
    "aicon": A_ICON_ROI,
    "charname": CHAR_NAME_ROI,
    "vehname": VEH_NAME_ROI,
    "banner": CUP_LIST_BANNER_ROI,
    "t_health": (210, 532, 1070, 644),
    "t_title": (224, 92, 1056, 166),
    "t_welcome": (350, 276, 930, 360),
    "t_disconnected": (350, 276, 930, 364),
    "t_vote": (340, 180, 940, 222),
    "t_connecting": (250, 330, 930, 372),      # without the animated dots
    "t_preparing": (370, 326, 910, 384),
    "t_opponents": (250, 300, 1040, 400),
    "t_emu": (420, 276, 1010, 436),
    "t_poll_wait": (700, 604, 1262, 648),
    "t_pleasewait": (350, 325, 930, 385),
    "t_nextrace": (500, 245, 780, 305),
    "t_results": (268, 38, 362, 88),          # "1st" of the first row of the results list
}
# "gray" templates compare luminance; "bright" templates compare only the bright
# (text) part, so a moving background (globe, flag, course preview) does not matter.
BRIGHT_KINDS = {"hint", "charname", "vehname", "banner", "cup", "t_connecting", "t_preparing",
                "t_opponents", "t_poll_wait", "t_pleasewait", "t_nextrace", "t_results"}
BRIGHT_FLOOR = 150

CUP_ORDER = ["mushroom", "flower", "star", "special", "shell", "banana", "leaf", "lightning"]


def cup_label_roi(cup: str):
    i = CUP_ORDER.index(cup)
    r, c = divmod(i, 4)
    cx = 600 + 170 * c
    y0 = 143 + 100 * r
    return (cx - 108, y0, cx + 108, y0 + 50)


LIST_ROWS = [(600, 311 + 74 * i, 1100, 355 + 74 * i) for i in range(4)]
WFC_BOXES = {"worldwide": (450, 135, 830, 205), "regional": (450, 250, 830, 318),
             "friends": (450, 365, 830, 432)}
WW_BOXES = {"vs": (420, 185, 760, 268), "battle": (420, 340, 760, 425)}
DRIFT_BOXES = {"auto": (580, 133, 1120, 320), "manual": (607, 357, 1093, 527)}
NEXT_RACE_BOXES = {"next": (500, 245, 780, 305), "quit": (510, 400, 770, 455)}
VOTE_BOXES = {"vote": (247, 320, 1033, 413), "random": (291, 440, 987, 527)}

# ---------------------------------------------------------------- templates
# (kind, key, legacy reference file, source spec). kind selects the ROI and the
# comparison; key is what a match means. Several templates per key are fine (best score
# wins). The legacy file is a hand-labelled capture in nav/refs (installs from before
# calibration); the source spec says which screen a capture must show for `./mkparty
# calibrate` to cut this template from it: (state or states, cursor or None, text or
# None), text = a phrase the screen shows (OCR, nav/ocrscreen.py). "@dialog" = any game
# dialog with the (A) icon. A legacy file of None: generated banks only, when seen.
DIALOG_STATES = ("welcome", "disconnected_dialog", "error_dialog")
TEMPLATES = [
    ("title", "main_menu", "mainmenu_1.png", ("main_menu", None, None)),
    ("title", "license_select", "title_1.png", ("license_select", None, None)),
    ("title", "characters", "char_select_1.png", ("characters", None, None)),
    ("title", "characters", "sp_char_05_mario.png", ("characters", None, None)),
    ("title", "vehicles", "vehicle_select_1.png", ("vehicles", None, None)),
    ("title", "drift", "drift_select_1.png", ("drift", None, None)),
    ("title", "wfc", "wfc_menu_1.png", (("wfc_menu", "connecting"), None, None)),
    ("title", "ww_mode", "ww_mode_1.png", ("ww_mode", None, None)),
    ("title", "searching", "r2_mm_1.png", ("searching", None, None)),
    ("title", "check_members", "mm_a_1.png", ("check_members", None, None)),
    ("title", "course_poll", "check_members2_1.png", ("course_poll", None, None)),
    ("title", "courses", "pv_after1_3.png", (("courses_cups", "courses_list"), None, None)),
    ("title", "courses", "sp_cups_06_mushroom.png", (("courses_cups", "courses_list"), None, None)),
    ("back", "back", "mainmenu_1.png", ("main_menu", None, None)),
    ("back", "back", "sp_char_05_mario.png", ("characters", None, None)),
    ("aicon", "a", "r2_a_1.png", ("@dialog", None, None)),
    ("aicon", "a", "r2_a_3.png", ("@dialog", None, None)),   # the icon pulses in size
    ("t_health", "health", "boot_health_1.png", ("health", None, None)),
    ("t_title", "title", "title_2.png", ("title", None, None)),
    ("t_welcome", "welcome", "wfc_welcome_1.png", ("welcome", None, None)),
    ("t_disconnected", "disconnected", "race_b_1.png", ("disconnected_dialog", None, None)),
    ("t_vote", "vote", "pv_vote3_i1_A_1.png", ("vote_prompt", None, None)),
    ("t_connecting", "connecting", "r2_c_1.png", ("connecting", None, None)),
    ("t_preparing", "preparing", "pv_vote1_1.png", ("preparing", None, None)),
    ("t_pleasewait", "pleasewait", "please_wait_1.png", ("please_wait", None, None)),
    ("t_nextrace", "nextrace", "next_race_menu_1.png", ("next_race_menu", "next", None)),
    ("t_results", "results", "results_1.png", ("results", None, None)),
    ("hint", "main_menu:single", "mainmenu_1.png", ("main_menu", "single", None)),
    ("hint", "main_menu:wfc", "after_dc_1.png", ("main_menu", "wfc", None)),
    ("hint", "main_menu:wfc2", "mm_wfc2.png", ("main_menu", "wfc2", None)),
    ("hint", "main_menu:multi", "mm_multi2.png", ("main_menu", "multi", "two players")),
    ("hint", "main_menu:multi", "mm_multi4.png", ("main_menu", "multi", "four players")),
    ("hint", "main_menu:multi", None, ("main_menu", "multi", "three players")),
    ("hint", "main_menu:channel", "mm_channel.png", ("main_menu", "channel", None)),
    ("hint", "main_menu:license", "mm_license.png", ("main_menu", "license", None)),
    ("hint", "drift:auto", "drift_select_1.png", ("drift", "auto", None)),
    ("hint", "drift:manual", "sp_drift_00_manual.png", ("drift", "manual", None)),
    ("hint", "drift:help", "sp_drift_02_help.png", ("drift", "help", None)),
    ("hint", "wfc_menu:worldwide", "wfc_menu_1.png", ("wfc_menu", "worldwide", None)),
    ("hint", "ww_mode:vs", "ww_mode_1.png", ("ww_mode", "vs", None)),
    ("hint", "course_poll:waiting", "check_members2_1.png", ("course_poll", None, "waiting for everyone")),
    ("hint", "course_poll:selected", "vote_cups_moved_1.png", ("course_poll", None, "has been selected")),
]
# Character / vehicle name labels and cup labels: one reference per catalog id (the
# source spec: characters / vehicles / courses_cups with that cursor; courses_list of
# that cup).
NAME_REFS = {
    "charname": {
        "yoshi": "sp_char_00_yoshi.png", "wario": "sp_char_01_wario.png",
        "donkey-kong": "sp_char_02_donkey-kong.png", "baby-mario": "sp_char_03_baby-mario.png",
        "toad": "sp_char_04_toad.png", "mario": "sp_char_05_mario.png",
        "luigi": "sp_char_06_luigi.png", "peach": "sp_char_07_peach.png",
        "waluigi": "sp_char_08_waluigi.png", "bowser": "sp_char_09_bowser.png",
        "baby-peach": "sp_char_10_baby-peach.png", "koopa-troopa": "sp_char_11_koopa-troopa.png",
    },
    "vehname": {
        "standard-kart-s": "sp_veh_light_00_standard-kart-s.png",
        "booster-seat": "sp_veh_light_01_booster-seat.png",
        "mini-beast": "sp_veh_light_02_mini-beast.png",
        "standard-bike-s": "sp_veh_light_04_standard-bike-s.png",
        "bullet-bike": "sp_veh_light_05_bullet-bike.png",
        "bit-bike": "sp_veh_light_06_bit-bike.png",
        "standard-kart-m": "sp_veh_medium_00_standard-kart-m.png",
        "classic-dragster": "sp_veh_medium_01_classic-dragster.png",
        "wild-wing": "sp_veh_medium_02_wild-wing.png",
        "standard-bike-m": "sp_veh_medium_04_standard-bike-m.png",
        "mach-bike": "sp_veh_medium_05_mach-bike.png",
        "sugarscoot": "sp_veh_medium_06_sugarscoot.png",
        "standard-kart-l": "sp_veh_heavy_00_standard-kart-l.png",
        "offroader": "sp_veh_heavy_01_offroader.png",
        "flame-flyer": "sp_veh_heavy_02_flame-flyer.png",
        "standard-bike-l": "sp_veh_heavy_04_standard-bike-l.png",
        "flame-runner": "sp_veh_heavy_05_flame-runner.png",
        "wario-bike": "sp_veh_heavy_06_wario-bike.png",
    },
    "cup": {
        "mushroom": "sp_cups_06_mushroom.png", "flower": "sp_cups_07_flower.png",
        "shell": "sp_cups_11_shell.png", "banana": "sp_cups_12_banana.png",
    },
    "banner": {
        "mushroom": "sp_list_00_mushroom_0.png", "flower": "sp_list_21_flower_0.png",
        "banana": "sp_list_17_banana_0.png", "shell": "sp_list_shell_1.png",
    },
}

THR_TITLE = 0.80
THR_TITLE_MARGIN = 0.08
THR_TEXT = 0.80
THR_NAME = 0.90
THR_NAME_MARGIN = 0.06
SHIFT = 2
POOL_MIN = int(os.environ.get("NAV_POOL_MIN", "3000"))   # template pixels above which NCC runs at half resolution


@dataclass
class Result:
    state: str
    conf: float
    cursor: object = None
    how: str = ""
    detail: dict = field(default_factory=dict)

    def __str__(self) -> str:
        c = f" cursor={self.cursor}" if self.cursor is not None else ""
        return f"{self.state}{c} ({self.conf:.2f} {self.how})"


# ---------------------------------------------------------------- frames
def load_frame(path: str) -> np.ndarray:
    return np.asarray(Image.open(path).convert("RGB"))


def frame_ok(frame: np.ndarray) -> bool:
    """True when the capture is the full 1280x1016 window with black letterbox bars."""
    if frame.shape[:2] != (FRAME_H, FRAME_W):
        return False
    top = frame[70:176, 40:1240]
    bot = frame[896:1006, 40:1240]
    return float(top.mean()) < 6.0 and float(bot.mean()) < 6.0


def picture_of(frame: np.ndarray) -> np.ndarray:
    return frame[PIC_Y0:PIC_Y0 + PIC_H, :PIC_W]


def load_picture(path: str):
    """(picture, ok) from a capture file."""
    fr = load_frame(path)
    if fr.shape[:2] != (FRAME_H, FRAME_W):
        return None, False
    return picture_of(fr), frame_ok(fr)


# ---------------------------------------------------------------- features
def _gray(a: np.ndarray) -> np.ndarray:
    return a[..., 0] * np.float32(0.299) + a[..., 1] * np.float32(0.587) + a[..., 2] * np.float32(0.114)


def _bright(a: np.ndarray) -> np.ndarray:
    return np.clip(a.min(axis=2).astype(np.float32) - BRIGHT_FLOOR, 0, None)


def _green(a: np.ndarray) -> np.ndarray:
    b = a.astype(np.float32)
    return np.clip(b[..., 1] - np.maximum(b[..., 0], b[..., 2]), 0, None)


def _feat(pic, roi, kind, grow=0):
    x0, y0, x1, y1 = roi
    sub = pic[max(0, y0 - grow):y1 + grow, max(0, x0 - grow):x1 + grow]
    if kind == "aicon":
        return _green(sub)
    return _bright(sub) if kind in BRIGHT_KINDS else _gray(sub)


def _ncc(a, b) -> float:
    a = a - a.mean()
    b = b - b.mean()
    den = float(np.sqrt((a * a).sum() * (b * b).sum()))
    if den < 1e-3:
        return 0.0
    return float((a * b).sum() / den)


def _pool2(a):
    h, w = a.shape[0] // 2 * 2, a.shape[1] // 2 * 2
    a = a[:h, :w]
    return (a[0::2, 0::2] + a[1::2, 0::2] + a[0::2, 1::2] + a[1::2, 1::2]) * 0.25


def _ncc_shift(region, tmpl, shift=SHIFT) -> float:
    """Best normalised correlation of tmpl over the (2 shift + 1)^2 placements in region,
    all placements at once (the same values as _ncc per placement, about 20x faster).
    Large templates (text strips) are compared at half resolution with half the shift."""
    if tmpl.size > POOL_MIN and shift >= 2:
        return _ncc_shift(_pool2(region), _pool2(tmpl), shift // 2)
    th, tw = tmpl.shape
    ny, nx = region.shape[0] - th + 1, region.shape[1] - tw + 1
    ny, nx = min(ny, 2 * shift + 1), min(nx, 2 * shift + 1)
    if ny <= 0 or nx <= 0:
        return -1.0
    region = region[:ny + th - 1, :nx + tw - 1].astype(np.float64)
    t = tmpl.astype(np.float64)
    t = t - t.mean()
    tn = float(np.sqrt((t * t).sum()))
    if tn < 1e-3:
        return 0.0
    win = np.lib.stride_tricks.sliding_window_view(region, (th, tw))      # (ny, nx, th, tw)
    num = np.einsum("ijkl,kl->ij", win, t)
    n = th * tw
    s1 = win.sum(axis=(2, 3))
    s2 = np.einsum("ijkl,ijkl->ij", win, win)
    var = s2 - s1 * s1 / n
    den = np.sqrt(np.clip(var, 0, None)) * tn
    ncc = np.where(den < 1e-3, 0.0, num / np.where(den < 1e-3, 1.0, den))
    return float(ncc.max())


# ---------------------------------------------------------------- bank
_bank = None


def _roi_for(kind, key):
    if kind == "cup":
        return cup_label_roi(key)
    return ROIS[kind]


def use_bank(path: str):
    """Read templates from this bank file from now on (calibration, tests)."""
    global BANK_FILE, _bank
    BANK_FILE = path
    _bank = None


def legacy_entries():
    """[(kind, key, file, roi)] of the bank built from the hand-labelled nav/refs."""
    out = [(k, key, f, _roi_for(k, key)) for k, key, f, _spec in TEMPLATES if f]
    for kind, d in NAME_REFS.items():
        for key, f in d.items():
            out.append((kind, key, f, _roi_for(kind, key)))
    return out


def build_bank(refs_dir: str = REFS_DIR, out: str | None = None, entries=None) -> int:
    """Cut the templates [(kind, key, capture file in refs_dir, roi)] (default: the legacy
    entries from nav/refs) and write them to `out` (default: the bank in use)."""
    out = out or BANK_FILE
    entries = legacy_entries() if entries is None else entries
    arrays, meta = {}, []
    cache = {}
    for i, (kind, key, f, roi) in enumerate(entries):
        if f not in cache:
            cache[f] = picture_of(load_frame(os.path.join(refs_dir, f)))
        arrays[f"t{i}"] = _feat(cache[f], roi, kind).astype(np.float32)
        meta.append({"kind": kind, "key": key, "src": os.path.basename(f), "roi": list(roi)})
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    tmp = out + ".tmp.npz"
    np.savez_compressed(tmp, meta=json.dumps(meta), **arrays)
    os.replace(tmp, out)
    global _bank
    _bank = None
    return len(entries)


def bank() -> dict:
    global _bank
    if _bank is None:
        if not os.path.exists(BANK_FILE):
            raise SystemExit(assets_missing(BANK_FILE))
        z = np.load(BANK_FILE, allow_pickle=False)
        meta = json.loads(str(z["meta"]))
        b = {}
        for i, m in enumerate(meta):
            b.setdefault(m["kind"], []).append((m["key"], z[f"t{i}"], tuple(m["roi"])))
        _bank = b
    return _bank


def scores(pic, kind, prefix: str | None = None) -> dict:
    """Best score per key for all templates of a kind (optionally keys with a prefix)."""
    out = {}
    regions = {}
    for key, tmpl, roi in bank().get(kind, []):
        if prefix is not None and not key.startswith(prefix):
            continue
        if roi not in regions:
            regions[roi] = _feat(pic, roi, kind, grow=SHIFT)
        s = _ncc_shift(regions[roi], tmpl)
        if s > out.get(key, -1.0):
            out[key] = s
    return out


def best(sc: dict):
    """(key, score, runner-up score) of a score dict."""
    if not sc:
        return None, 0.0, 0.0
    r = sorted(sc.items(), key=lambda kv: -kv[1])
    return r[0][0], r[0][1], (r[1][1] if len(r) > 1 else 0.0)


def _pick(sc: dict, thr: float, margin: float):
    k, s, s2 = best(sc)
    if k is None or s < thr or s - s2 < margin:
        return None, s
    return k, s


# ---------------------------------------------------------------- readers
def _white_count(pic, box) -> int:
    x0, y0, x1, y1 = box
    sub = pic[y0:y1, x0:x1]
    mn = sub.min(axis=2)
    mx = sub.max(axis=2)
    return int(((mn > 208) & (mx - mn < 40)).sum())


def pick_white(pic, boxes: dict, min_count: int = 500):
    """Key of the one box whose label is white (the highlighted button), else None."""
    counts = {k: _white_count(pic, b) for k, b in boxes.items()}
    r = sorted(counts.items(), key=lambda kv: -kv[1])
    (k, top), (_, second) = r[0], r[1]
    if top < min_count or top < 3 * second + 200:
        return None
    return k


def hint(pic, state: str):
    """Cursor name from the hint line bottom-right, for states that have hint templates."""
    sc = scores(pic, "hint", state + ":")
    k, s = _pick(sc, THR_TEXT, 0.05)
    return (k.split(":", 1)[1] if k else None), s


def read_character(pic):
    return _pick(scores(pic, "charname"), THR_NAME, THR_NAME_MARGIN)


def read_vehicle(pic):
    return _pick(scores(pic, "vehname"), THR_NAME, THR_NAME_MARGIN)


def read_cup(pic):
    return _pick(scores(pic, "cup"), THR_NAME, THR_NAME_MARGIN)


def read_list(pic):
    """(cup or None, row) in the course list, or None when it is not the list."""
    row = pick_white(pic, {i: b for i, b in enumerate(LIST_ROWS)}, min_count=350)
    if row is None:
        return None
    cup, _ = _pick(scores(pic, "banner"), THR_NAME, THR_NAME_MARGIN)
    if cup is None:
        return None                 # the cup banner is still sliding in (or this is not the list)
    return (cup, row)


MEMBER_SLOTS = [(x, 96 + 61 * r) for r in range(6) for x in (455, 1000)]


def member_count(pic) -> int:
    """Number of player rows on Check Members (yellow VR numbers are counted)."""
    n = 0
    for x, y in MEMBER_SLOTS:
        sub = pic[y:y + 56, x:x + 150].astype(np.int16)
        yel = (sub[..., 0] > 200) & (sub[..., 1] > 170) & (sub[..., 2] < 110)
        if int(yel.sum()) > 120:
            n += 1
    return n


def race_hud(pic) -> bool:
    """The yellow TIME / LAP read-out top-right of a race in progress."""
    sub = pic[70:215, 780:1250].astype(np.int16)
    yel = (sub[..., 0] > 200) & (sub[..., 1] > 170) & (sub[..., 2] < 90)
    cols = yel.any(axis=0).sum()
    return int(yel.sum()) > 1500 and cols > 150


def colour_stats(pic) -> dict:
    sub = pic[::4, ::4].astype(np.int16)
    mx = sub.max(axis=2)
    mn = sub.min(axis=2)
    lum = _gray(sub.astype(np.float32))
    return {"lum": float(lum.mean()), "sat": float(((mx - mn) > 60).mean())}


def has_chrome(pic) -> bool:
    """The white menu bar along the top (present on every menu and dialog screen)."""
    x0, y0, x1, y1 = TOP_STRIP
    sub = pic[y0:y1, x0:x1]
    mn = sub.min(axis=2)
    return float((mn > 200).mean()) > 0.9


# ---------------------------------------------------------------- classify
def emu_sheet(frame: np.ndarray) -> bool:
    """Dolphin's own alert sheet ("Invalid read from ...": macOS interface, not the game's
    picture) over the window: its yellow warning triangle next to a plain panel."""
    tri = frame[370:485, 270:395].astype(np.int16)
    yel = int(((tri[..., 0] > 200) & (tri[..., 1] > 150) & (tri[..., 2] < 80)).sum())
    if yel < 3000:
        return False
    return float(frame[340:360, 240:1040].astype(np.float32).std()) < 4.0


def classify_frame(frame: np.ndarray) -> Result:
    """Classify a full window capture (checks the capture geometry first)."""
    if frame.shape[:2] != (FRAME_H, FRAME_W):
        return Result("unknown", 0.0, how=f"bad-frame-size {frame.shape[1]}x{frame.shape[0]}")
    pic = picture_of(frame)
    # The emulator alert sheet hangs over the letterbox, so it is checked first (by its
    # shape; banks built before 2026-10-16 also carry a text template of it).
    if emu_sheet(frame):
        return Result("emu_alert", 1.0, how="emu-sheet")
    emu = scores(pic, "t_emu").get("emu", 0.0)
    if emu >= THR_TEXT:
        return Result("emu_alert", emu, how="emu-text")
    if not frame_ok(frame):
        return Result("unknown", 0.0, how="bad-frame-letterbox")
    return classify(pic)


def classify(pic: np.ndarray) -> Result:
    d = {}
    st = colour_stats(pic)
    d["stats"] = {k: round(v, 3) for k, v in st.items()}
    if st["lum"] < 4:
        return Result("black", 1.0, how="black", detail=d)

    # 2. Game dialogs: green (A) icon and a white box.
    a = scores(pic, "aicon").get("a", 0.0)
    if a >= 0.75:
        x0, y0, x1, y1 = DIALOG_WHITE_ROI
        white = float((pic[y0:y1, x0:x1].min(axis=2) > 215).mean())
        d["dialog"] = {"a": round(a, 3), "white": round(white, 3)}
        if white > 0.55:
            w = scores(pic, "t_welcome").get("welcome", 0.0)
            dc = scores(pic, "t_disconnected").get("disconnected", 0.0)
            if w >= THR_TEXT and w > dc:
                return Result("welcome", w, how="dialog-text", detail=d)
            if dc >= THR_TEXT:
                return Result("disconnected_dialog", dc, how="dialog-text", detail=d)
            return Result("error_dialog", a, how="dialog-box", detail=d)

    # 3. Vote prompt.
    v = scores(pic, "t_vote").get("vote", 0.0)
    if v >= THR_TEXT:
        cur = pick_white(pic, VOTE_BOXES)
        if cur is None:
            return Result("unknown", v, how="vote-prompt-no-cursor", detail=d)
        return Result("vote_prompt", v, cursor=cur, how="vote-text", detail=d)

    # 4. Full-screen texts.
    # After a race: "Next Race / Quit Game" over the course.
    nr = scores(pic, "t_nextrace").get("nextrace", 0.0)
    if nr >= THR_TEXT:
        cur = pick_white(pic, NEXT_RACE_BOXES, min_count=300)
        if cur is not None:
            return Result("next_race_menu", nr, cursor=cur, how="next-race-text", detail=d)
    rs = scores(pic, "t_results").get("results", 0.0)
    if rs >= THR_TEXT:
        return Result("results", rs, how="results-list", detail=d)
    for kind, key, state in (("t_health", "health", "health"), ("t_title", "title", "title"),
                             ("t_preparing", "preparing", "preparing"),
                             ("t_pleasewait", "pleasewait", "please_wait"),
                             ("t_opponents", "opponents", "opponents_chosen")):
        s = scores(pic, kind).get(key, 0.0)
        if s >= THR_TEXT:
            return Result(state, s, how="text", detail=d)

    # 5. Menu screens by title.
    ts = scores(pic, "title")
    tk, t1, t2 = best(ts)
    d["title"] = [(k, round(s, 3)) for k, s in sorted(ts.items(), key=lambda kv: -kv[1])[:3]]
    if tk is not None and t1 >= THR_TITLE and t1 - t2 >= THR_TITLE_MARGIN:
        return _menu(pic, tk, t1, d)

    chrome = has_chrome(pic)
    d["chrome"] = chrome
    if chrome:
        b = scores(pic, "back").get("back", 0.0)
        if b >= THR_TEXT and t1 < 0.6:
            return Result("other_menu", b, how="back-button", detail=d)
        return Result("unknown", 0.0, how="menu-frame-no-title", detail=d)

    # 6. No menu frame.
    hud = race_hud(pic)
    d["hud"] = hud
    if hud:
        return Result("race", 0.9, how="race-hud", detail=d)
    if st["sat"] > 0.30 and st["lum"] > 40:
        return Result("race", 0.5, how="no-frame-colour", detail=d)
    return Result("unknown", 0.0, how="no-match", detail=d)


def _menu(pic, tk: str, conf: float, d: dict) -> Result:
    def unknown(why):
        return Result("unknown", conf, how=f"{tk}:{why}", detail=d)

    if tk == "main_menu":
        cur, s = hint(pic, "main_menu")
        if cur is None:
            return unknown("no-hint")
        return Result("main_menu", min(conf, s), cursor=cur, how="title+hint", detail=d)
    if tk == "license_select":
        return Result("license_select", conf, how="title", detail=d)
    if tk == "wfc":
        c = scores(pic, "t_connecting").get("connecting", 0.0)
        if c >= 0.45:
            return Result("connecting", c, how="title+text", detail=d)
        cur = pick_white(pic, WFC_BOXES)
        if cur is None:
            return unknown("no-cursor")
        return Result("wfc_menu", conf, cursor=cur, how="title+label", detail=d)
    if tk == "ww_mode":
        cur = pick_white(pic, WW_BOXES)
        if cur is None:
            return unknown("no-cursor")
        return Result("ww_mode", conf, cursor=cur, how="title+label", detail=d)
    if tk == "characters":
        cur, s = read_character(pic)
        if cur is None:
            return unknown("no-name")
        return Result("characters", min(conf, s), cursor=cur, how="title+name", detail=d)
    if tk == "vehicles":
        cur, s = read_vehicle(pic)
        if cur is None:
            return unknown("no-name")
        return Result("vehicles", min(conf, s), cursor=cur, how="title+name", detail=d)
    if tk == "drift":
        cur, s = hint(pic, "drift")
        lab = pick_white(pic, DRIFT_BOXES)
        if cur is None or (cur != "help" and lab != cur) or (cur == "help" and lab is not None):
            return unknown("no-cursor")
        return Result("drift", min(conf, s), cursor=cur, how="title+hint+label", detail=d)
    if tk == "searching":
        return Result("searching", conf, how="title", detail=d)
    if tk == "check_members":
        d["members"] = member_count(pic)
        return Result("check_members", conf, how="title", detail=d)
    if tk == "course_poll":
        d["poll"] = hint(pic, "course_poll")[0]
        return Result("course_poll", conf, how="title", detail=d)
    if tk == "courses":
        cup, s = read_cup(pic)
        if cup is not None:
            return Result("courses_cups", min(conf, s), cursor=cup, how="title+cup-label", detail=d)
        lst = read_list(pic)
        if lst is not None:
            return Result("courses_list", conf, cursor=lst, how="title+row", detail=d)
        return unknown("no-cursor")
    return unknown("unhandled")
