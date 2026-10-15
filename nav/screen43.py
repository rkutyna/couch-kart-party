"""Screen classifier for Mario Kart Wii (RMCE01) running in 4:3 (two-player profiles).

The two-player profiles (instances i7 .. i12) boot with SYSCONF.IPL.AR = 4:3, so the
game fills the whole 1280x960 render area of the 1280x1016 window capture (no
letterbox). The menus are laid out differently from 16:9 and the two-player screens
(controller registration, characters / vehicles / drift for two players) do not exist
in single player, so this module has its own regions and templates (nav/refs43,
nav/labels43.json -> nav/templates43.npz). The state names and cursor conventions are
the same as screen.py wherever the screen means the same thing, so driver.py plans
both with the same code; the two-player screens have tuple cursors:

    characters   (p1 name, p2 name, p1 confirmed)      e.g. ("mario", "luigi", False)
    vehicles     (p1 name, p2 name, p1 confirmed)
    drift        (p1 choice, p2 choice, p1 confirmed)   choices "auto" | "manual"

Two-player only states (also used while setting a profile up):
    controller_reg   cursor "wait" (press A on the 2nd controller) | "ok" (OK / Revise)
    choose_mii       cursor "p2" (globe background: the 2nd player picks) | "p1" (license)
    connect2         "Player and Player will both connect to Nintendo WFC." + OK
    create_license   any "Create a License" screen (A continues)
    wfc_notice       first-connect data notices with a Next button (A)
    permit_prompt    "Permit data to be sent to Nintendo?" cursor permit | prohibit
    wont_play        "You will not be able to play ..." cursor yes | no

    classify_frame(frame) -> Result   (screen.Result)
"""
from __future__ import annotations

import json
import os

import numpy as np

import screen as S
from screen import Result

NAV_DIR = os.path.dirname(os.path.abspath(__file__))
REFS_DIR = os.path.join(NAV_DIR, "refs43")         # hand-labelled captures (legacy; not in git)
BANK_FILE = S.bank_path("templates43.npz")         # see screen.bank_path

FRAME_W, FRAME_H = 1280, 1016
PIC_Y0, PIC_H, PIC_W = 56, 960, 1280

TITLE_ROI = (56, 58, 440, 112)
HINT_ROI = (395, 820, 1225, 880)
BACK_ROI = (60, 833, 340, 900)
A_ICON_ROI = (1100, 760, 1192, 850)
DIALOG_WHITE_ROI = (160, 150, 1120, 360)
CHAR_LABELS = ((40, 370, 410, 425), (40, 745, 410, 800))      # p1, p2 name labels
VEH_LABELS = ((270, 380, 640, 445), (270, 725, 640, 790))
P1_ARROW = (60, 175, 210, 375)                                    # yellow arrows (unconfirmed)
TOP_STRIP = (20, 6, 420, 24)

ROIS = {
    "title": TITLE_ROI,
    "hint": HINT_ROI,
    "back": BACK_ROI,
    "aicon": A_ICON_ROI,
    "t_health": (60, 760, 1230, 905),
    "t_title": (60, 100, 1210, 260),
    "t_welcome": (270, 380, 1010, 497),
    "t_disconnected": (300, 379, 980, 505),
    "t_vote": (236, 244, 1044, 312),
    "t_connecting": (90, 480, 1170, 560),
    "t_connect2": (202, 362, 1086, 412),
    "t_results": (140, 45, 260, 115),
    "t_nextrace": (460, 340, 820, 415),
    "t_next_btn": (960, 770, 1200, 850),
    "t_permit": (387, 261, 892, 387),
    "t_wontplay": (210, 177, 1069, 236),
    "t_opponents": (59, 455, 1212, 522),
}
BRIGHT_KINDS = {"hint", "charname", "vehname", "t_connecting", "t_nextrace", "t_results", "t_next_btn"}

WFC_BOXES = {"worldwide": (300, 180, 960, 280), "regional": (300, 335, 960, 435),
             "friends": (300, 495, 960, 595)}
WW_BOXES = {"vs": (380, 260, 800, 360), "battle": (380, 470, 800, 575)}
DRIFT_P1 = {"auto": (560, 170, 1060, 270), "manual": (560, 325, 1060, 425)}
DRIFT_P2 = {"auto": (560, 515, 1060, 615), "manual": (560, 670, 1060, 770)}
VOTE_BOXES = {"vote": (118, 446, 1162, 564), "random": (168, 606, 1111, 724)}
NEXT_RACE_BOXES = {"next": (460, 340, 820, 415), "quit": (470, 550, 810, 630)}
REG_BOXES = {"ok": (760, 340, 1240, 460), "revise": (770, 515, 1230, 625)}
PERMIT_BOXES = {"permit": (480, 550, 800, 640), "prohibit": (480, 685, 800, 790)}
YESNO_BOXES = {"yes": (520, 470, 760, 550), "no": (520, 620, 760, 715)}
LIST_ROWS = [(560, 420 + 101 * i, 1040, 490 + 101 * i) for i in range(4)]
# Character grid cells (row, col) -> box; used to look for P1's "OK" marker.
CHAR_CELL_X = (475, 875)
CHAR_CELL_W = 310
CHAR_CELL_Y = (135, 248, 360, 480, 595, 710)
CHAR_CELL_H = 100
CUP_ORDER = S.CUP_ORDER
MEMBER_SLOTS = [(x, 143 + 84 * r) for r in range(6) for x in (505, 1045)]


_cgrid = None


def _char_grid():
    global _cgrid
    if _cgrid is None:
        with open(S.CATALOG) as f:
            _cgrid = {c["id"]: tuple(c["grid"]) for c in json.load(f)["characters"] if "grid" in c}
    return _cgrid


def cup_label_roi(cup: str):
    i = CUP_ORDER.index(cup)
    r, c = divmod(i, 4)
    cx = 556 + 168 * c
    y0 = 205 + 150 * r            # the name label under the highlighted cup (row 2: estimated)
    return (cx - 70, y0, cx + 70, y0 + 40)


def char_cell(r, c):
    x0 = CHAR_CELL_X[c]
    y0 = CHAR_CELL_Y[r]
    return (x0 + 60, y0 + 15, x0 + CHAR_CELL_W - 60, y0 + CHAR_CELL_H - 15)


# (kind, key, legacy reference file in refs43, source spec); see screen.TEMPLATES
DIALOG_STATES = S.DIALOG_STATES
WFC_STATES = ("wfc_menu", "connecting", "connect2", "wfc_notice", "permit_prompt")
TEMPLATES = [
    ("title", "main_menu", "mm_single_1.png", ("main_menu", None, None)),
    ("title", "license_select", "license_select_1.png", ("license_select", None, None)),
    ("title", "create_license", "create_license_1.png", ("create_license", None, None)),
    ("title", "choose_mii", "choose_mii_lic_1.png", ("choose_mii", "p1", None)),
    ("title", "choose_mii", "choose_mii_p2_1.png", ("choose_mii", "p2", None)),
    ("title", "controller_reg", "ctrl_reg_ok_1.png", ("controller_reg", None, None)),
    ("title", "characters", "chars_1.png", ("characters", None, None)),
    ("title", "vehicles", "vehicles_1.png", ("vehicles", None, None)),
    ("title", "drift", "drift_1.png", ("drift", None, None)),
    ("title", "wfc", "wfc_menu_1.png", (WFC_STATES, None, "nintendo wfc 2p")),
    ("title", "wfc", None, (WFC_STATES, None, "nintendo wfc 1p")),
    ("title", "ww_mode", "ww_mode_1.png", ("ww_mode", None, None)),
    ("title", "searching", "searching_2.png", ("searching", None, None)),
    ("title", "check_members", "check_members_1.png", ("check_members", None, None)),
    ("title", "course_poll", "course_poll_1.png", ("course_poll", None, None)),
    ("title", "courses", "courses_cups_1.png", (("courses_cups", "courses_list"), None, None)),
    ("back", "back", "mm_single_1.png", ("main_menu", None, None)),
    ("aicon", "a", "welcome_1.png", ("@dialog", None, None)),
    ("aicon", "a", "disconnected_1.png", ("@dialog", None, None)),
    ("aicon", "a", "guest_notice_1.png", ("@dialog", None, None)),
    ("t_health", "health", "health_1.png", ("health", None, None)),
    ("t_title", "title", "title_1.png", ("title", None, None)),
    ("t_welcome", "welcome", "welcome_1.png", ("welcome", None, None)),
    ("t_disconnected", "disconnected", "disconnected_1.png", ("disconnected_dialog", None, None)),
    ("t_vote", "vote", "vote_prompt_1.png", ("vote_prompt", None, None)),
    ("t_connecting", "connecting", "connecting_1.png", ("connecting", None, None)),
    ("t_connect2", "connect2", "connect2_1.png", ("connect2", None, None)),
    ("t_results", "results", "results_1.png", ("results", None, None)),
    ("t_nextrace", "nextrace", "next_race_1.png", ("next_race_menu", "next", None)),
    ("t_next_btn", "next", "wfc_notice_1.png", ("wfc_notice", None, None)),
    ("t_permit", "permit", "permit_prohibit_1.png", ("permit_prompt", None, None)),
    ("t_wontplay", "wontplay", "wont_play_no_1.png", ("wont_play", None, None)),
    ("t_opponents", "opponents", "opponents_1.png", ("opponents_chosen", None, None)),
    ("hint", "main_menu:single", "mm_single_1.png", ("main_menu", "single", None)),
    ("hint", "main_menu:single", "mm_single_2.png", ("main_menu", "single", None)),
    ("hint", "main_menu:wfc", "mm_wfc_1.png", ("main_menu", "wfc", None)),
    ("hint", "main_menu:wfc2", "mm_wfc2_1.png", ("main_menu", "wfc2", None)),
    ("hint", "main_menu:wfc2", "mm_wfc2_2.png", ("main_menu", "wfc2", None)),
    ("hint", "main_menu:multi", "mm_multi2_1.png", ("main_menu", "multi", "two players")),
    ("hint", "main_menu:multi", "mm_multi3_1.png", ("main_menu", "multi", "three players")),
    ("hint", "main_menu:multi", "mm_multi4_1.png", ("main_menu", "multi", "four players")),
    ("hint", "main_menu:channel", "mm_channel_1.png", ("main_menu", "channel", None)),
    ("hint", "main_menu:license", "mm_license_1.png", ("main_menu", "license", None)),
    ("hint", "main_menu:license", "mm_license_2.png", ("main_menu", "license", None)),
    ("hint", "controller_reg:wait", "ctrl_reg_wait_1.png", ("controller_reg", "wait", None)),
    ("hint", "controller_reg:ok", "ctrl_reg_ok_1.png", ("controller_reg", "ok", None)),
    ("hint", "course_poll:waiting", "course_poll_1.png", ("course_poll", None, "waiting for everyone")),
]
# Name labels: (kind, key, legacy file, which label the legacy reference shows). A
# generated bank takes each name from whichever player's label shows it.
NAME_REFS = [("charname", n, f"cn_p1_{n}.png", 0) for n in
             ("wario", "toad", "baby-mario", "mario", "yoshi", "donkey-kong")] + \
            [("charname", n, f"cn_p2_{n}.png", 1) for n in
             ("luigi", "peach", "waluigi", "bowser", "baby-peach", "koopa-troopa")] + \
            [("vehname", n, f"vn_p1_{n}.png", 0) for n in
             ("standard-kart-s", "booster-seat", "mini-beast", "standard-bike-s", "bullet-bike", "bit-bike",
              "standard-kart-l", "offroader", "flame-flyer", "standard-bike-l", "flame-runner", "wario-bike")] + \
            [("vehname", n, f"vn_p2_{n}.png", 1) for n in
             ("standard-kart-m", "classic-dragster", "wild-wing", "standard-bike-m", "mach-bike", "sugarscoot")]
# P1's "OK" marker on the character grid (chars_p1ok_1: P1 confirmed Mario, row 2 col 0).
OK_REF = ("okbtn", "ok", "chars_p1ok_1.png", (2, 0))

THR_TITLE = 0.80
THR_TITLE_MARGIN = 0.08
THR_TEXT = 0.80
THR_NAME = 0.88
THR_NAME_MARGIN = 0.05
SHIFT = 2


def load_frame(path):
    return S.load_frame(path)


def frame_ok(frame) -> bool:
    return frame.shape[:2] == (FRAME_H, FRAME_W)


def picture_of(frame):
    return frame[PIC_Y0:PIC_Y0 + PIC_H, :PIC_W]


def _feat(pic, roi, kind, grow=0):
    x0, y0, x1, y1 = roi
    sub = pic[max(0, y0 - grow):y1 + grow, max(0, x0 - grow):x1 + grow]
    if kind == "aicon":
        return S._green(sub)
    return S._bright(sub) if kind in BRIGHT_KINDS else S._gray(sub)


_bank = None


def use_bank(path: str):
    global BANK_FILE, _bank
    BANK_FILE = path
    _bank = None


def legacy_entries():
    entries = [(k, key, f, ROIS[k]) for k, key, f, _spec in TEMPLATES if f]
    for kind, key, f, which in NAME_REFS:
        entries.append((kind, key, f, (CHAR_LABELS if kind == "charname" else VEH_LABELS)[which]))
    kind, key, f, (r, c) = OK_REF
    entries.append((kind, key, f, char_cell(r, c)))
    return entries


def build_bank(refs_dir: str = REFS_DIR, out: str | None = None, entries=None) -> int:
    """See screen.build_bank (entries: [(kind, key, file, roi)], default the legacy ones)."""
    out = out or BANK_FILE
    entries = legacy_entries() if entries is None else entries
    arrays, meta, cache = {}, [], {}
    for i, (kind, key, f, roi) in enumerate(entries):
        if f not in cache:
            cache[f] = picture_of(S.load_frame(os.path.join(refs_dir, f)))
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
            raise SystemExit(S.assets_missing(BANK_FILE))
        z = np.load(BANK_FILE, allow_pickle=False)
        meta = json.loads(str(z["meta"]))
        b = {}
        for i, m in enumerate(meta):
            b.setdefault(m["kind"], []).append((m["key"], z[f"t{i}"], tuple(m["roi"])))
        _bank = b
    return _bank


def scores(pic, kind, prefix=None, roi=None) -> dict:
    """Best score per key over the templates of a kind, each matched at its own region
    (or at `roi` for position-free templates such as name labels)."""
    out, regions = {}, {}
    for key, tmpl, troi in bank().get(kind, []):
        if prefix is not None and not key.startswith(prefix):
            continue
        r = roi or troi
        if r not in regions:
            regions[r] = _feat(pic, r, kind, grow=SHIFT)
        s = S._ncc_shift(regions[r], tmpl, SHIFT)
        if s > out.get(key, -1.0):
            out[key] = s
    return out


def _pick(sc, thr, margin):
    return S._pick(sc, thr, margin)


def hint(pic, state):
    sc = scores(pic, "hint", state + ":")
    k, s = _pick(sc, THR_TEXT, 0.05)
    return (k.split(":", 1)[1] if k else None), s


def read_name(pic, kind, which):
    labels = CHAR_LABELS if kind == "charname" else VEH_LABELS
    return _pick(scores(pic, kind, roi=labels[which]), THR_NAME, THR_NAME_MARGIN)


def _grey_text(pic, box) -> int:
    """Grey (unselected) label text: light, unsaturated pixels that are not white."""
    x0, y0, x1, y1 = box
    sub = pic[y0:y1, x0:x1].astype(np.int16)
    mn, mx = sub.min(axis=2), sub.max(axis=2)
    return int(((mn > 120) & (mn < 205) & (mx - mn < 45)).sum())


def drift_side(pic, boxes):
    """(selected choice or None, confirmed). Once a player confirms, the other choice's
    box disappears (no grey text left in it)."""
    sel = S.pick_white(pic, boxes, min_count=400)
    if sel is None:
        return None, False
    other = "manual" if sel == "auto" else "auto"
    return sel, _grey_text(pic, boxes[other]) < 300


def p1_arrows(pic) -> bool:
    x0, y0, x1, y1 = P1_ARROW
    sub = pic[y0:y1, x0:x1].astype(np.int16)
    yel = (sub[..., 0] > 170) & (sub[..., 1] > 140) & (sub[..., 2] < 90)
    return int(yel.sum()) > 1500


def member_count(pic) -> int:
    n = 0
    for x, y in MEMBER_SLOTS:
        sub = pic[y:y + 80, x:x + 150].astype(np.int16)
        yel = (sub[..., 0] > 200) & (sub[..., 1] > 170) & (sub[..., 2] < 110)
        if int(yel.sum()) > 150:
            n += 1
    return n


def read_cup(pic):
    """The highlighted cup: the one whose name label (under the cup) is showing."""
    counts = {}
    for cup in CUP_ORDER:
        x0, y0, x1, y1 = cup_label_roi(cup)
        sub = pic[y0:y1, x0:x1]
        counts[cup] = int((sub.min(axis=2) > 190).sum())
    r = sorted(counts.items(), key=lambda kv: -kv[1])
    if r[0][1] < 400 or r[0][1] < 3 * r[1][1] + 100:
        return None, 0.0
    return r[0][0], 0.9


def read_list(pic):
    row = S.pick_white(pic, {i: b for i, b in enumerate(LIST_ROWS)}, min_count=350)
    if row is None:
        return None
    return (None, row)          # the cup of the list is not read in 4:3 (see driver)


def classify_frame(frame) -> Result:
    if not frame_ok(frame):
        return Result("unknown", 0.0, how=f"bad-frame-size {frame.shape[1]}x{frame.shape[0]}")
    return classify(picture_of(frame))


def classify(pic) -> Result:
    d = {}
    st = S.colour_stats(pic)
    d["stats"] = {k: round(v, 3) for k, v in st.items()}
    if st["lum"] < 4:
        return Result("black", 1.0, how="black", detail=d)

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

    v = scores(pic, "t_vote").get("vote", 0.0)
    if v >= THR_TEXT:
        cur = S.pick_white(pic, VOTE_BOXES)
        if cur is None:
            return Result("unknown", v, how="vote-prompt-no-cursor", detail=d)
        return Result("vote_prompt", v, cursor=cur, how="vote-text", detail=d)

    nr = scores(pic, "t_nextrace").get("nextrace", 0.0)
    if nr >= THR_TEXT:
        cur = S.pick_white(pic, NEXT_RACE_BOXES, min_count=300)
        if cur is not None:
            return Result("next_race_menu", nr, cursor=cur, how="next-race-text", detail=d)
    rs = scores(pic, "t_results").get("results", 0.0)
    if rs >= THR_TEXT:
        return Result("results", rs, how="results-list", detail=d)
    for kind, key, state in (("t_health", "health", "health"), ("t_title", "title", "title"),
                             ("t_opponents", "opponents", "opponents_chosen")):
        s = scores(pic, kind).get(key, 0.0)
        if s >= THR_TEXT:
            return Result(state, s, how="text", detail=d)
    s = scores(pic, "t_wontplay").get("wontplay", 0.0)
    if s >= THR_TEXT:
        cur = S.pick_white(pic, YESNO_BOXES, min_count=300)
        if cur is not None:
            return Result("wont_play", s, cursor=cur, how="text+label", detail=d)

    ts = scores(pic, "title")
    tk, t1, t2 = S.best(ts)
    d["title"] = [(k, round(s, 3)) for k, s in sorted(ts.items(), key=lambda kv: -kv[1])[:3]]
    if tk is not None and t1 >= THR_TITLE and t1 - t2 >= THR_TITLE_MARGIN:
        return _menu(pic, tk, t1, d)

    chrome = S.has_chrome(pic)
    d["chrome"] = chrome
    if chrome:
        b = scores(pic, "back").get("back", 0.0)
        if b >= THR_TEXT and t1 < 0.6:
            return Result("other_menu", b, how="back-button", detail=d)
        return Result("unknown", 0.0, how="menu-frame-no-title", detail=d)
    if st["sat"] > 0.30 and st["lum"] > 40:
        return Result("race", 0.5, how="no-frame-colour", detail=d)
    return Result("unknown", 0.0, how="no-match", detail=d)


def _menu(pic, tk, conf, d) -> Result:
    def unknown(why):
        return Result("unknown", conf, how=f"{tk}:{why}", detail=d)

    if tk == "main_menu":
        cur, s = hint(pic, "main_menu")
        if cur is None:
            return unknown("no-hint")
        return Result("main_menu", min(conf, s), cursor=cur, how="title+hint", detail=d)
    if tk in ("license_select", "create_license", "searching", "course_poll"):
        if tk == "course_poll":
            d["poll"] = hint(pic, "course_poll")[0]
        return Result(tk, conf, how="title", detail=d)
    if tk == "choose_mii":
        # The second player's Mii is chosen over the blue globe; a license's over white.
        x0, y0, x1, y1 = (600, 700, 1200, 800)
        sub = pic[y0:y1, x0:x1].astype(np.int16)
        blue = float(((sub[..., 2] > 150) & (sub[..., 0] < 120)).mean())
        return Result("choose_mii", conf, cursor="p2" if blue > 0.3 else "p1", how="title+bg", detail=d)
    if tk == "controller_reg":
        cur = S.pick_white(pic, REG_BOXES, min_count=300)
        if cur is None:
            h, _ = hint(pic, "controller_reg")
            if h != "wait":
                return unknown("no-cursor")
            cur = "wait"
        return Result("controller_reg", conf, cursor=cur, how="title+label", detail=d)
    if tk == "wfc":
        if scores(pic, "t_connect2").get("connect2", 0.0) >= THR_TEXT:
            return Result("connect2", conf, how="title+text", detail=d)
        if scores(pic, "t_next_btn").get("next", 0.0) >= THR_TEXT:
            return Result("wfc_notice", conf, how="title+next", detail=d)
        if scores(pic, "t_permit").get("permit", 0.0) >= THR_TEXT:
            cur = S.pick_white(pic, PERMIT_BOXES, min_count=300)
            if cur is None:
                return unknown("permit-no-cursor")
            return Result("permit_prompt", conf, cursor=cur, how="title+text", detail=d)
        c = scores(pic, "t_connecting").get("connecting", 0.0)
        if c >= 0.45:
            return Result("connecting", c, how="title+text", detail=d)
        cur = S.pick_white(pic, WFC_BOXES)
        if cur is None:
            return unknown("no-cursor")
        return Result("wfc_menu", conf, cursor=cur, how="title+label", detail=d)
    if tk == "ww_mode":
        cur = S.pick_white(pic, WW_BOXES)
        if cur is None:
            return unknown("no-cursor")
        return Result("ww_mode", conf, cursor=cur, how="title+label", detail=d)
    if tk == "characters":
        n1, s1 = read_name(pic, "charname", 0)
        n2, s2 = read_name(pic, "charname", 1)
        if n1 is None or n2 is None:
            return unknown("no-name")
        ok = False
        rc = _char_grid().get(n1)
        if rc is not None:
            ok = scores(pic, "okbtn", roi=char_cell(*rc)).get("ok", 0.0) >= 0.7
        return Result("characters", min(conf, s1, s2), cursor=(n1, n2, ok), how="title+names", detail=d)
    if tk == "vehicles":
        n1, s1 = read_name(pic, "vehname", 0)
        n2, s2 = read_name(pic, "vehname", 1)
        if n1 is None or n2 is None:
            return unknown("no-name")
        return Result("vehicles", min(conf, s1, s2), cursor=(n1, n2, not p1_arrows(pic)), how="title+names",
                      detail=d)
    if tk == "drift":
        c1, ok1 = drift_side(pic, DRIFT_P1)
        c2, _ = drift_side(pic, DRIFT_P2)
        if c1 is None or c2 is None:
            return unknown("no-cursor")
        return Result("drift", conf, cursor=(c1, c2, ok1), how="title+labels", detail=d)
    if tk == "check_members":
        d["members"] = member_count(pic)
        return Result("check_members", conf, how="title", detail=d)
    if tk == "courses":
        # The list (one bright course bar) first: while it slides in, the big cup banner
        # above it can look like a highlighted cup label.
        lst = read_list(pic)
        if lst is not None:
            return Result("courses_list", conf, cursor=lst, how="title+row", detail=d)
        cup, s = read_cup(pic)
        if cup is not None:
            return Result("courses_cups", conf, cursor=cup, how="title+cup-label", detail=d)
        return unknown("no-cursor")
    return unknown("unhandled")
