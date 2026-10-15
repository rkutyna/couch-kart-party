#!/usr/bin/env python3
"""Navigator: drives Mario Kart Wii (RMCE01) Dolphin instances through the online menus.

usage (paths are derived from this file):
  nav.py test [-v]                 offline classifier test over nav/refs + nav/labels.json
  nav.py build-templates           rebuild nav/templates.npz from nav/refs
  nav.py classify <png> [...]      state / cursor of capture files
  nav.py watch --players N [--seconds S]
                                   print every instance's screen (no input is sent)
  nav.py prepare --players N [--hold MIN] [--keepalive S] [--no-infra]
                                   start server / instances / streams / partyd as needed,
                                   drive every instance to "Nintendo WFC (1P)"; with --hold
                                   stay and report disconnects for MIN minutes
  nav.py run [--warm N] [--keepalive S]
                                   start the game server, partyd and a warm pool of N (default
                                   NAV_WARM or 3) single-player games at the WFC menu, then follow
                                   the phone lobby: at Start bring up exactly the games the locked
                                   players need (<= 6: one each; 7-12: two-player split screen)
                                   and drive them (see README.md)
  nav.py setup [k ...]             prepare Dolphin profiles once: license, online consent, the
                                   second player of two-player profiles (default: every profile
                                   i1..i12 without instances/i<k>/mkparty-ready)
  nav.py pick <k> --character ID --vehicle ID --drift auto|manual [--vote ID] [--go]
                                   debugging: drive instance k from its current screen to the
                                   Drift screen with these choices and hold there (--go: confirm
                                   and play the room screens too). No lobby is used.
  nav.py shot <k> <file.png>       save one window capture of instance k

Dolphin is started only through run/launch.sh and stopped only through run/down.sh.
Logs: nav/logs/nav-<mode>-<time>.log (+ -obs-i<k>.log with every observation), captures
in nav/logs/shots/<mode>-<time>/, unknown screens in nav/logs/unknown/. nav/logs is
pruned to NAV_LOG_CAP_MB (default 1500) at start and every 10 minutes (oldest runs first).
"""
from __future__ import annotations

import argparse
import json
import os
import socket
import subprocess
import sys
import time

NAV_DIR = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(NAV_DIR)
RUN = os.path.join(ROOT, "run")
sys.path.insert(0, NAV_DIR)

import dolphin  # noqa: E402
import driver  # noqa: E402
import lobby  # noqa: E402
import screen  # noqa: E402
import screen43  # noqa: E402

WFC_HOST = "naswii.localtest.me"


LOG_CAP_MB = float(os.environ.get("NAV_LOG_CAP_MB", "1500"))


def _du(path: str) -> int:
    total = 0
    for dp, _dn, fn in os.walk(path):
        for f in fn:
            try:
                total += os.path.getsize(os.path.join(dp, f))
            except OSError:
                pass
    return total


def prune_logs(keep_run: str | None = None, cap_mb: float = LOG_CAP_MB, log=print):
    """Keep nav/logs under cap_mb: delete whole old runs (their log, observation logs and
    capture folder, oldest first; never keep_run), then the oldest unknown-screen captures
    and loose files. Also caps run/logs/phonestats-*.jsonl and stream logs."""
    import glob
    import shutil
    d = driver.LOG_DIR
    cap = cap_mb * 1e6
    size = _du(d)
    if size <= cap:
        return
    runs = {}
    for f in glob.glob(os.path.join(d, "nav-*.log")):
        rid = os.path.basename(f)[4:-4].split("-obs-")[0]
        runs.setdefault(rid, []).append(f)
    for sd in glob.glob(os.path.join(d, "shots", "*")):
        runs.setdefault(os.path.basename(sd), []).append(sd)

    def age(rid):
        return min(os.path.getmtime(p) for p in runs[rid])
    freed = 0
    for rid in sorted(runs, key=age):
        if size - freed <= cap * 0.8:
            break
        if rid == keep_run:
            continue
        for p in runs[rid]:
            n = _du(p) if os.path.isdir(p) else os.path.getsize(p)
            (shutil.rmtree if os.path.isdir(p) else os.remove)(p)
            freed += n
    loose = sorted(glob.glob(os.path.join(d, "unknown", "*")) + glob.glob(os.path.join(d, "*.out")),
                   key=os.path.getmtime)
    for p in loose:
        if size - freed <= cap * 0.8:
            break
        freed += os.path.getsize(p)
        os.remove(p)
    log(f"logs: pruned {freed / 1e6:.0f} MB from {d} ({size / 1e6:.0f} MB -> {(size - freed) / 1e6:.0f} MB, "
        f"cap {cap_mb:.0f} MB)")
    stats = sorted(glob.glob(os.path.join(RUN, "logs", "phonestats-*.jsonl")), key=os.path.getmtime)
    for p in stats[:-20]:
        os.remove(p)


def new_run(tag: str):
    run_id = f"{tag}-{time.strftime('%Y%m%d-%H%M%S')}"
    return run_id, driver.Log(os.path.join(driver.LOG_DIR, f"nav-{run_id}.log"))


def sh(*args, check=True) -> int:
    print("+ " + " ".join(args), flush=True)
    r = subprocess.run(list(args), cwd=ROOT)
    if check and r.returncode != 0:
        raise SystemExit(f"command failed ({r.returncode}): {' '.join(args)}")
    return r.returncode


def listening(port: int) -> bool:
    s = socket.socket()
    s.settimeout(1.0)
    try:
        s.connect(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def pidfile_alive(name: str) -> bool:
    try:
        pid = int(open(os.path.join(RUN, "pids", f"{name}.pid")).read().strip())
    except (OSError, ValueError):
        return False
    return dolphin.pid_alive(pid)


def check_dns():
    """Name resolution pre-check. The no-DNS patch build (patcher/out-local, Gecko title
    '$WFC local-ip 8080') reaches the server by the IP literal 127.0.0.1 only, so there is
    nothing to resolve: check that every provisioned instance carries that code. An
    instance still carrying the old '$WFC localtest.me 8080' code needs *.localtest.me to
    resolve to 127.0.0.1 with the system resolver (public DNS; some routers filter it)."""
    import glob
    old = []
    for ini in sorted(glob.glob(os.path.join(ROOT, "instances", "i*", "GameSettings", "RMCE01.ini"))):
        try:
            with open(ini) as f:
                text = f.read()
        except OSError:
            continue
        if "$WFC local-ip 8080" not in text:
            old.append(ini)
    if not old:
        return
    try:
        ip = socket.gethostbyname(WFC_HOST)
    except OSError as e:
        ip = f"no answer ({e})"
    if ip != "127.0.0.1":
        raise SystemExit(
            "Name resolution check failed: these profiles do not carry the no-DNS WFC code:\n  "
            + "\n  ".join(old) + "\n"
            f"and {WFC_HOST} resolves to {ip}, expected 127.0.0.1.\n"
            "The game would show 'Unable to connect to the Internet. Error code: 20100'.\n"
            "Fix: run/provision.sh <k> 1p --force-config (installs the 127.0.0.1 build).\n"
            "See nav/README.md.")


def slots_count() -> int:
    try:
        with open(os.path.join(RUN, "slots.json")) as f:
            return len(json.load(f)["slots"])
    except (OSError, ValueError, KeyError):
        return -1


def ensure_server():
    check_dns()
    if not listening(8080):
        sh(os.path.join(ROOT, "server", "start.sh"))
    else:
        print("game server: NAS 127.0.0.1:8080 is listening")


def ensure_party():
    """partyd with the 12-slot config (run/party.sh). A partyd with an older config (fewer
    slots, no per-instance sources) is restarted."""
    if pidfile_alive("partyd") and lobby.healthy() and slots_count() == 12 and has_sources():
        print("partyd: already running (12 slots)")
        return
    if pidfile_alive("partyd"):
        sh(os.path.join(RUN, "party.sh"), "stop", check=False)
    sh(os.path.join(RUN, "party.sh"), "start")


def has_sources() -> bool:
    try:
        with open(os.path.join(RUN, "slots.json")) as f:
            return len(json.load(f).get("sources", [])) >= 24
    except (OSError, ValueError):
        return False


def ensure_instances(ks, setup: bool = False):
    """Provisioned, running instances with streams (for prepare / pick / setup)."""
    for k in ks:
        mode = dolphin.inst_mode(k)
        sh(os.path.join(RUN, "provision.sh"), str(k), mode)
        inst = dolphin.Instance(k, setup=setup)
        if not inst.alive():
            if not inst.launch(print):
                raise SystemExit(f"instance {k}: launch failed (see run/logs/i{k}.log)")
        sh(os.path.join(RUN, "streams.sh"), "start", str(k))
    for k in ks:
        inst = dolphin.Instance(k)
        t0 = time.time()
        while not (inst.alive() and inst.refresh_window()):
            if time.time() - t0 > 90:
                raise SystemExit(f"instance {k}: no Dolphin game window after 90 s (see run/logs/i{k}.log)")
            time.sleep(1)
        print(f"instance {k}: pid {inst.recorded_pid()}, window {inst.wid}")


def ensure_infrastructure(n: int, streams: bool = True, party: bool = True):
    """Game server, instances 1..n, streams and partyd. Idempotent."""
    ensure_server()
    ensure_instances(range(1, n + 1))
    if party:
        ensure_party()


# ------------------------------------------------------------------ commands
def cmd_test(a):
    import offline_test
    for d in (screen.REFS_DIR, screen43.REFS_DIR, offline_test.LABELS, offline_test.LABELS43):
        if not os.path.exists(d):
            raise SystemExit(f"{d} is missing: the offline test needs the labelled screenshots, which "
                             "are not in the git repository (see README.md, 'Game assets').")
    return offline_test.run(verbose=a.verbose)


def cmd_build(_a):
    for d in (screen.REFS_DIR, screen43.REFS_DIR):
        if not os.path.isdir(d):
            raise SystemExit(f"{d} is missing (see README.md, 'Game assets').")
    print(f"wrote {screen.build_bank()} templates to {screen.BANK_FILE}")
    return 0


def cmd_classify(a):
    for p in a.paths:
        res = screen.classify_frame(screen.load_frame(p))
        extra = {k: v for k, v in res.detail.items() if k in ("members", "poll", "dialog")}
        print(f"{os.path.basename(p)}: {res} {extra or ''}")
    return 0


def cmd_watch(a):
    insts = [dolphin.Instance(k) for k in (a.instances or range(1, a.players + 1))]
    t0 = time.time()
    while not a.seconds or time.time() - t0 < a.seconds:
        parts = []
        for i in insts:
            fr = i.grab() if i.alive() else None
            if fr is None:
                parts.append(f"i{i.k}=NO-CAPTURE({i.last_err or 'not running'})")
            else:
                r = (screen43 if i.aspect43 else screen).classify_frame(fr)
                parts.append(f"i{i.k}={r.state}{'' if r.cursor is None else '[%s]' % (r.cursor,)}({r.conf:.2f})")
        print(driver.now_s(), " ".join(parts), flush=True)
        time.sleep(a.interval)
    return 0


def cmd_prepare(a):
    if not a.no_infra:
        ensure_infrastructure(a.players)
    run_id, log = new_run("prepare")
    r = driver.Runner(range(1, a.players + 1), log, run_id, keepalive=a.keepalive)
    r.start()
    try:
        ok = r.prepare(a.timeout)
        if ok and a.hold:
            left = r.hold(a.hold)
            ok = not left
        gaps = {k: round(w.max_gap, 2) for k, w in r.workers.items()}
        log(f"prepare result: {'ok' if ok else 'FAILED'}; observations {dict((k, w.n_obs) for k, w in r.workers.items())}; "
            f"longest gap between observations (s) {gaps}")
    finally:
        r.stop()
    return 0 if ok else 1


def cmd_run(a):
    if a.warm is not None:
        driver.WARM_1P = a.warm
    if not a.no_infra:
        ensure_server()
        ensure_party()
        for k in range(1, 13):
            if driver.profile_ready(k):
                sh(os.path.join(RUN, "provision.sh"), str(k), dolphin.inst_mode(k))
    run_id, log = new_run("run")
    prune_logs(run_id, log=log)
    log(f"run: warm pool {driver.WARM_1P} single-player + {driver.WARM_2P} two-player instance(s); "
        f"profiles set up: {[k for k in range(1, 13) if driver.profile_ready(k)]}")
    try:
        lobby.post_update(message="Getting the game ready...")
    except lobby.LobbyError as e:
        log(f"lobby message failed: {e}")
    # Instances that are already running (an earlier navigator) are adopted.
    ks = [k for k in range(1, 13) if dolphin.Instance(k).alive()]
    for k in ks:
        sh(os.path.join(RUN, "streams.sh"), "start", str(k))
    r = driver.Runner(ks, log, run_id, with_lobby=True, keepalive=a.keepalive, pool=True)
    r.prune = lambda: prune_logs(run_id, log=log)
    r.start()
    try:
        return r.run(a.timeout)
    except KeyboardInterrupt:
        log("interrupted")
        return 130
    finally:
        r.stop()


def cmd_setup(a):
    """Prepare profiles once: provision, boot in 4:3, create the license (fresh save),
    give online consent (Permit), register the second player (two-player profiles) and
    reach the Nintendo WFC menu; then stop the instance and mark the profile ready."""
    ks = a.instances or [k for k in range(1, 13) if not driver.profile_ready(k) or a.force]
    if not ks:
        print("every profile i1..i12 is set up (instances/i<k>/mkparty-ready); --force to redo")
        return 0
    ensure_server()
    run_id, log = new_run("setup")
    log(f"setup: profiles {ks}, {a.parallel} at a time")
    bad = []
    for i in range(0, len(ks), a.parallel):
        batch = ks[i:i + a.parallel]
        for k in batch:
            if dolphin.Instance(k).alive():
                raise SystemExit(f"instance {k} is running; stop it first (./mkparty down)")
        ensure_instances(batch, setup=True)
        r = driver.Runner(batch, log, run_id, setup=True)
        r.start()
        ok = r.prepare(a.timeout)
        r.stop()
        for k, w in r.workers.items():
            if w.at_wfc:
                with open(os.path.join(dolphin.INST_ROOT, f"i{k}", "mkparty-ready"), "w") as f:
                    f.write(f"set up {time.strftime('%Y-%m-%d %H:%M:%S')} ({dolphin.inst_mode(k)})\n")
                log(f"setup: i{k} ({dolphin.inst_mode(k)}) is ready")
                w.shot("setup_done")
            else:
                bad.append(k)
                log(f"setup: i{k} FAILED at {w.stable[0]}")
        r.saver.q.join()
        for k in batch:
            dolphin.Instance(k).stop(log)
            sh(os.path.join(RUN, "streams.sh"), "stop", str(k), check=False)
        if not ok:
            log(f"setup: batch {batch} did not finish: {r.summary()}")
    log(f"setup finished: ready {[k for k in range(1, 13) if driver.profile_ready(k)]}, failed {bad}")
    return 1 if bad else 0


def cmd_pick(a):
    run_id, log = new_run(f"pick{a.k}")
    r = driver.Runner([a.k], log, run_id)
    w = r.workers[a.k]
    if not w.inst.alive():
        raise SystemExit(f"instance {a.k} is not running")
    w.set_goal(driver.Goal("setup", round=1, character=a.character, vehicle=a.vehicle, drift=a.drift,
                           vote=a.vote, go=a.go))
    w.start()
    t0 = time.time()
    ok = False
    try:
        while time.time() - t0 < a.timeout:
            if not a.go and w.drift_ready and w.pending is None and w.state == "drift":
                ok = True
                break
            if a.go and w.handed:
                ok = True
                break
            time.sleep(0.1)
        time.sleep(0.5)
        log(f"pick result: {'ok' if ok else 'TIMEOUT'} at {r.summary()} after {time.time() - t0:.1f} s; "
            f"captures in {w.shots_dir}")
    finally:
        r.stop()
    return 0 if ok else 1


def cmd_shot(a):
    i = dolphin.Instance(a.k, setup=a.setup)
    fr = i.grab() if i.alive() else None
    if fr is None:
        raise SystemExit(f"no capture: {i.last_err or 'instance not running'}")
    i.save_png(fr, os.path.abspath(a.file))
    print(a.file, (screen43 if i.aspect43 else screen).classify_frame(fr))
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("test"); p.add_argument("-v", "--verbose", action="store_true"); p.set_defaults(fn=cmd_test)
    sub.add_parser("build-templates").set_defaults(fn=cmd_build)
    p = sub.add_parser("classify"); p.add_argument("paths", nargs="+"); p.set_defaults(fn=cmd_classify)
    p = sub.add_parser("watch"); p.add_argument("--players", type=int, default=3)
    p.add_argument("instances", type=int, nargs="*")
    p.add_argument("--seconds", type=float, default=0); p.add_argument("--interval", type=float, default=1.0)
    p.set_defaults(fn=cmd_watch)
    for name, fn in (("prepare", cmd_prepare), ("run", cmd_run)):
        p = sub.add_parser(name)
        if name == "prepare":
            p.add_argument("--players", type=int, required=True)
        else:
            p.add_argument("--players", type=int, help="ignored (the player count follows the lobby)")
            p.add_argument("--warm", type=int, default=None, help="single-player games kept ready in the lobby")
        p.add_argument("--timeout", type=float, default=600.0, help="seconds allowed for prepare")
        p.add_argument("--keepalive", type=float, default=0.0,
                       help="seconds between keep-alive cursor moves on the WFC menu (0 = off)")
        p.add_argument("--no-infra", action="store_true", help="do not start server/instances/streams/partyd")
        if name == "prepare":
            p.add_argument("--hold", type=float, default=0.0, help="minutes to stay and watch for disconnects")
        p.set_defaults(fn=fn)
    p = sub.add_parser("pick")
    p.add_argument("k", type=int)
    p.add_argument("--character", required=True); p.add_argument("--vehicle", required=True)
    p.add_argument("--drift", default="auto", choices=["auto", "manual"])
    p.add_argument("--vote", default="random"); p.add_argument("--go", action="store_true")
    p.add_argument("--timeout", type=float, default=180.0)
    p.set_defaults(fn=cmd_pick)
    p = sub.add_parser("shot"); p.add_argument("k", type=int); p.add_argument("file")
    p.add_argument("--setup", action="store_true", help="the instance was booted for setup (4:3)")
    p.set_defaults(fn=cmd_shot)
    p = sub.add_parser("setup"); p.add_argument("instances", type=int, nargs="*")
    p.add_argument("--force", action="store_true"); p.add_argument("--parallel", type=int, default=3)
    p.add_argument("--timeout", type=float, default=420.0)
    p.set_defaults(fn=cmd_setup)
    a = ap.parse_args(argv)
    if a.cmd not in ("build-templates", "test"):
        # Load both template banks now: the workers load them lazily in their threads,
        # where a missing file would only stop the thread.
        screen.bank()
        screen43.bank()
    return a.fn(a)


if __name__ == "__main__":
    sys.exit(main())
