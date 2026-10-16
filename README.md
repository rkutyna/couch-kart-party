# Couch Kart Party

Phone-controlled local Mario Kart Wii parties.

Up to 12 players race each other online in Mario Kart Wii, each controlling the game from
their own phone. Everything runs on one Mac: a private Nintendo-WFC-compatible server
(WiiLink's wfc-server, patched to run locally), up to six Dolphin emulators running your
own copy of the game, a phone server that streams each game's picture to the phones over
WebRTC and takes their controls, and a "navigator" that drives the games through the online
menus so that the players land in the race lobby together, then hands each game over to
its phone(s).

* 1–6 players: one emulated Wii per player (16:9 picture).
* 7–12 players: two players share one emulated Wii in split screen (4:3; the phone of
  player A gets the top half, player B the bottom half), plus one single-player game
  when the number is odd. 12 players = 6 split-screen games.

The player count is not set by hand: it follows the phones that are in the lobby
(connected and Ready) when the host taps Start.

Licence: MIT for this project's code, except the two patch files in `patches/`, which have
their own licences (see [Licence](#licence)).

## Requirements

* A Mac with Apple Silicon (tested on macOS 27 with an M3 Pro) and the Xcode
  Command Line Tools (`xcode-select --install`, for `swiftc`).
* [Homebrew](https://brew.sh) with: `brew install go postgresql@16 ffmpeg python`
  (Python 3.11 or newer; ffmpeg must have the `h264_videotoolbox` encoder, which
  Homebrew's has).
* [Dolphin](https://dolphin-emu.org), installed as an app (default
  `/Applications/Dolphin.app`; tested with build 2609a).
* Your own Mario Kart Wii disc image: the USA release, game ID **RMCE01, revision 0**,
  as `.wbfs` or `.iso`. It is not part of this project and never will be.
* A PowerPC toolchain to build the game patch, either
  1. **devkitPro's devkitPPC (preferred, fast)**: install it with devkitPro's installer
     (<https://devkitpro.org/wiki/Getting_Started>); setup finds it through
     `DEVKITPPC` / `DEVKITPRO` or at `/opt/devkitpro/devkitPPC`; or
  2. a devkitPPC-equivalent built from the GNU sources by
     `toolchain/build-devkitppc.sh` (`./mkparty setup --build-toolchain`; downloads
     binutils, GCC and newlib from ftp.gnu.org / sourceware.org and devkitPro's patches
     from github.com/devkitPro/buildscripts at a pinned commit, checks their SHA-256,
     installs into `toolchain/opt/`; about 1–2 hours; also needs
     `brew install make gmp mpfr libmpc`).
* The phones and the Mac on the same Wi-Fi (or the Mac's Internet Sharing hotspot).
* The terminal that runs mkparty needs Screen Recording permission (the games' windows
  are captured).
* Nothing else: the screen templates the navigator uses to read the game's menus are
  generated on your Mac from your own game during setup (see [Game assets](#game-assets)).

## Install

    git clone https://github.com/rkutyna/couch-kart-party.git
    cd couch-kart-party
    ./mkparty setup --game "/path/to/Mario Kart Wii (USA).wbfs"   # add --dolphin /path/to/Dolphin.app if elsewhere

`./mkparty setup` is safe to run again at any time: every step checks what is already
there. Its steps:

1. **prerequisites**: macOS/Apple Silicon, swiftc, Homebrew, go, postgresql@16, ffmpeg
   with h264_videotoolbox, Python ≥ 3.11 with the packages in `requirements.txt`
   (if they are missing, a `.venv` is created and used automatically), Dolphin, the disc
   image (its header must say RMCE01 revision 0) and the ports the party uses.
2. **upstreams**: clones WiiLink's [wfc-server](https://github.com/WiiLink24/wfc-server)
   into `server/wfc-server` and [wfc-patcher-wii](https://github.com/WiiLink24/wfc-patcher-wii)
   into `patcher/wfc-patcher-wii-local`, both at pinned commits, and applies our changes
   from `patches/`.
3. **toolchain**: finds devkitPPC (or builds one with `--build-toolchain`).
4. **patch**: generates this install's payload signing key, builds the no-DNS game patch
   (every server address is the IP literal 127.0.0.1, so the game needs no DNS and works
   offline) into `patcher/out-local`, and copies the payload into the server.
5. **server**: writes `server/wfc-server/config.xml` from `server/config.template.xml`
   with a random database password and API secret, creates a private PostgreSQL 16
   cluster in `server/pgdata` (127.0.0.1 only, port `PG_PORT`, default 54329) with
   wfc-server's schema, and builds the server.
6. **apps**: builds the phone server `party/partyd`, the test phones `run/bin/fakephones`,
   the window-capture helpers (`capture/bin/wincap`, `run/bin/winlist`), the text
   recognition helper `run/bin/ocr` (Apple's Vision framework, offline; used only to
   generate the screen templates) and the self-signed HTTPS certificate (for this Mac's
   Bonjour name and addresses).

Local settings live in `mkparty.conf` (created by setup from `mkparty.conf.example`, not
in git): `DOLPHIN_APP`, `GAME_IMAGE`, and optionally `DEVKITPPC`, `PG_PORT`, `PARTY_PORT`,
`HTTPS_PORT`, `MKPARTY_ASSETS`.

Then, once, with the Mac unlocked (it starts Dolphin; about 25 minutes, the games stay
silent; Dolphin windows come and go, leave them on screen):

    ./mkparty setup --profiles

1. It prepares the 12 Dolphin profiles (`instances/i1..i6` single-player, `i7..i12`
   split-screen): creates the game's license, accepts the online data notice ("Permit"),
   registers the second controller and goes online once, all through the game's own menus
   (about 6 minutes). Profiles already set up (marked `instances/i<k>/mkparty-ready`) are
   skipped; `nav/nav.py setup <k> --force` redoes one.
2. It then generates the screen templates from your game (`./mkparty calibrate`, about 15
   minutes here, as the new profiles already showed the first-boot screens; see
   [Game assets](#game-assets)).

## Run a party

    ./mkparty up          # start everything; ready in about 1–2 minutes
    ./mkparty status      # what is running, lobby state, phone URLs
    ./mkparty down        # stop everything

`up` starts everything in the background and returns to the prompt once the games
are online; Ctrl-C while it waits only stops the output. The only way to stop the
party is `./mkparty down`.

`up` prints the addresses for the phones (`https://<this-mac>.local:8443/` and the
`http` one). Open `http://localhost:8000/tv.html` on the Mac or a TV to show a join QR
code.

In the lobby 3 single-player games are kept online and ready (`./mkparty up --warm N`
or `NAV_WARM=N` changes that). When more phones join, more games are brought up in
the background; when the host taps Start, any games still missing are started ("Starting
N more games…" on the phones) and games that are not needed are stopped.

Keep the Mac unlocked while the party runs: while the screen is locked the games'
windows cannot be captured, so the navigator sees nothing and waits (`up` warns about
it). The Mac is kept awake while a party or calibration runs (`caffeinate`, released by
`./mkparty down` or when calibration ends), so the screen does not lock on its own; caffeinate
cannot unlock a screen that is already locked. Set `MKPARTY_KEEP_AWAKE=0` to disable it.
Keep the Dolphin windows on the Mac's screen (not minimised, not on another desktop
or behind a full-screen app): macOS delivers their pictures slowly when they are
hidden, which makes the phone video stutter and the navigator slower.

### Sound

Only the host's game (the game of the player who taps Start) plays sound, through the
Mac's speakers, from Start on. All other games stay muted. The sound comes straight
from Dolphin, so it is not in sync with the phones' video (the phones are a fraction
of a second behind).

    NAV_TV_AUDIO=0 ./mkparty up     # keep all games silent

### Matchmaking

The local game server prefers rooms (`WWFC_PREFER_ROOM`, on by default): when a console
searching for a race could join more than one open room, the server offers it only the
room with the most members, so that the party does not end up in several rooms by
chance; consoles that find no open room still find each other as usual. To turn it off:
`./mkparty down`, then `WWFC_PREFER_ROOM=0 ./mkparty up` (or set `WWFC_PREFER_ROOM=0`
in `mkparty.conf`); it is read when the game server starts.

## What the players do

1. Open the address on the phone. With the `https` address the browser warns about
   the certificate once: tap "Show details" / "Advanced" and "visit this website"
   (https is needed for tilt steering; the `http` address works without tilt).
2. Pick a name, character, vehicle, drift and a course to vote for; tap Ready.
3. The host (the first player) taps Start. The phones show what their game is doing
   ("Picking Mario…", "Finding the others…"). After about a minute (1–7 players: 55–75 s
   measured) the phone switches to live video at the Course Poll screen and the
   player drives.
4. When the race is over the phone shows a course picker ("vote for the next race")
   with a countdown. Pick a course and tap Lock in. The navigator takes the game
   through to the next Course Poll and gives the controls back.

Split screen: the two players of one game pick their character, vehicle and drift on
the same screen; the game does not let them take the same character (the navigator then
takes the nearest free one). A split-screen game has only ONE course vote: the
navigator enters player A's choice in the first race, player B's in the next, and so on.

## Troubleshooting

* `./mkparty setup` stops at a FAIL line: it says what is missing and how to get it; fix
  it and run setup again (finished steps are skipped). Build logs: `run/logs/setup-*.log`.
* "no devkitPPC found": install devkitPro's devkitPPC, or set `DEVKITPPC` in
  `mkparty.conf`, or run `./mkparty setup --build-toolchain`.
* "screen templates missing" / "The screen templates are not generated yet": run
  `./mkparty calibrate` (see [Game assets](#game-assets)). If it ends with "did not finish
  cleanly", run it again; its log is `nav/logs/nav-calibrate-*.log`.
* "Not installed yet …" when running `up`: run `./mkparty setup`.
  "Not set up yet: i<k> …": run `./mkparty setup --profiles`.
* Certificate warning on the phone: expected (self-signed); accept it once, or use
  the `http://` address. `party/make-cert.sh --force` makes a new certificate (for
  example after renaming the Mac); a real one at `party/certs/fullchain.pem` +
  `privkey.pem` is used instead when present.
* More than 7 players end up in TWO races (for example 6 + 6). The game's matchmaking
  closes a room to newcomers a few seconds (3–8 s seen) after it reaches 6 or more
  players, and joining takes a few seconds per game; everybody still races, but not
  all against each other. With 8 players (four split-screen games) the fourth game
  usually finds nobody to race with: it joins the room only once the first race has
  started, so its two players miss that race.
* A phone shows "Game restarted. Rejoining…": the navigator noticed a crashed or
  frozen game and restarted it; that player joins the next race.
* A player was "Disconnected from the room": the game drops a player who does not
  move for about 30–40 s. The navigator takes that game back online; when every game
  of the race is out, the lobby returns to the start screen by itself.
* To start over: the host taps Reset in the lobby, or `./mkparty down` and
  `./mkparty up` again.
* `server/netcheck.sh` checks that the game patch, the server's payload and the Dolphin
  profiles agree.
* Logs: `run/logs/` (`mkparty.log`, `nav.log`, `partyd.log`, `i<k>.log`,
  `stream-i<k>.log`), `server/logs/` and `nav/logs/` (navigator decisions and captures;
  old runs are deleted when the folder passes 1.5 GB, `NAV_LOG_CAP_MB`).

## Limits

* With 12 players the Mac is busy: 6 emulators (about 1.6–1.9 cores together), 6
  encoders (about 1 core), the navigator (about 1 core); memory about 2.5 GB for the
  emulators. Each extra game booting makes the running ones slower for a moment.
* Players of different races (8–12 players) do not see each other.
* The game itself confirms the character/vehicle/drift screen about 25 s after VS is
  chosen; the navigator always gets there first, but a game that boots very slowly can
  miss the others' room and race separately.

## Project layout

    mkparty                 the command: setup, calibrate, up, down, status
    setup.sh                installation steps (./mkparty setup)
    mkparty.conf.example    local settings template (mkparty.conf is not in git)
    requirements.txt        Python packages
    patches/                our changes to WiiLink's wfc-server and wfc-patcher-wii
    server/                 start.sh / stop.sh / netcheck.sh for the local game server,
                            config.template.xml; wfc-server/ and pgdata/ are created by setup
    patcher/                created by setup: wfc-patcher-wii-local/ (clone), out-local/ (build)
    toolchain/              build-devkitppc.sh (only without devkitPro)
    nav/                    the navigator (Python): screen recognition, menu driving,
                            lobby client; see nav/README.md
    party/                  partyd (Go): phone web app (web/), WebRTC video, controls,
                            lobby; cmd/fakephones (test phones), cmd/testclient, cmd/statsreport
    run/                    launcher scripts: Dolphin profiles, instances, capture streams
    capture/                window capture (wincap.swift) and a stand-alone stream script
    instances/              created: the Dolphin profiles with their saves (never in git)
    assets/                 created by calibrate: screen templates etc. from your game (never in git)

## Game assets

The navigator recognises the game's screens by comparing parts of each picture (title
strip, hint line, name labels, dialog texts) with small templates. Those templates are cut
from pictures of the game, so **this project distributes none of them**: they are generated
on your Mac from your own copy of the game, by

    ./mkparty calibrate        # run automatically by ./mkparty setup --profiles

It needs profiles i1 and i7 set up and nothing else running (`./mkparty down`), takes
about 18 minutes (measured: first-boot screens on a scratch profile ~4 min, menu tours ~6
min, three short online rounds ~9-10 min, building and checking ~1 min), keeps every game
silent and starts Dolphin windows; keep the Mac unlocked. What it does:

1. It reads which screen is on display **without templates**: Apple's Vision text
   recognition (`run/bin/ocr`, offline) reads the title, hint line, name labels and dialog
   texts, and the known menu flow says what to press (`nav/ocrscreen.py`,
   `nav/calibrate.py`). The project ships only this metadata: which region of which
   screen each template is cut from and the texts the screens show.
2. It brings up every screen the navigator must recognise and captures it: the first-boot
   screens (new license, data notice, Permit / Prohibit, the second player) on a scratch
   profile `instances/i13` with a fresh save (deleted afterwards; during
   `setup --profiles` they are captured from the new profiles instead); offline tours on
   i1 (16:9) and i7 (4:3, two players) of the main menu, all 12 characters, all 18
   vehicles, drift, the 4 unlocked cups and their course lists; and three online rounds
   of i1 and i7 against each other on the local game server (WFC menus, Worldwide VS,
   Check Members, the vote, Course Poll, the race, results / Next Race / Please wait, and
   the disconnect dialog of the console that idles).
3. It cuts the templates from the captures that match the others best, writes the two
   template banks and checks them: every capture must be classified as the text
   recognition labelled it (`nav/nav.py test --refs assets`).

Everything it makes is in `assets/` (`MKPARTY_ASSETS` in `mkparty.conf` or the environment
moves it), which is never in git:

* `templates.npz`, `templates43.npz`: the screen templates (16:9 and 4:3). **Required.**
* `captures/` (about 500 pictures, ~350 MB), `labels.json`, `labels43.json`,
  `sources.json`, `calibration.json`: the captures with their labels, which capture each
  template came from, and the run's summary (for `nav.py test` and rebuilding with
  `nav.py build-templates`). You may delete `captures/` once the banks exist.
* `icons/mario.png`, `luigi.png`, `peach.png`: the minimap icons the test phones
  (`run/bin/fakephones`) steer by, from the races of the online rounds (optional; a test
  phone without its icon drives without steering).

Installs from before calibration existed may have `nav/templates.npz` etc.; they are used
only when `assets/` has no banks.

## Licence

* This project's code, everything in this repository except the two patch files below,
  is licensed under the MIT License; see [LICENSE](LICENSE). Copyright (c) 2026 Roger Kutyna.
* The two patch files in `patches/` modify WiiLink's wfc-server and wfc-patcher-wii, so
  they carry those projects' licences: `wfc-server.patch` is AGPL-3.0, and
  `wfc-patcher-wii.patch` is under WiiLink's licence. See [patches/README.md](patches/README.md).
* Game assets: none are distributed; they are generated on your Mac from your own disc
  (see [Game assets](#game-assets)).

## Credits

This project builds on, and fetches at setup time (it does not include their code):

* **WiiLink WFC** - <https://wfc.wiilink.ca>
  * [wfc-server](https://github.com/WiiLink24/wfc-server) (AGPL-3.0): the game server.
    Our changes are in `patches/wfc-server.patch`.
  * [wfc-patcher-wii](https://github.com/WiiLink24/wfc-patcher-wii) (WiiLink's licence): the
    game patch and payload. Our changes (an altered version: server by IP literal, no DNS)
    are in `patches/wfc-patcher-wii.patch`.
* [Dolphin](https://dolphin-emu.org) (GPL-2.0-or-later): the emulator, installed by you.
* [devkitPro](https://devkitpro.org): devkitPPC and the
  [buildscripts](https://github.com/devkitPro/buildscripts) whose patches
  `toolchain/build-devkitppc.sh` applies to [GNU binutils](https://www.gnu.org/software/binutils/),
  [GCC](https://gcc.gnu.org) and [newlib](https://sourceware.org/newlib/).
* [Pion WebRTC](https://github.com/pion/webrtc) (MIT): WebRTC in partyd;
  [go-qrcode](https://github.com/skip2/go-qrcode) (MIT): the join QR code.
* [FFmpeg](https://ffmpeg.org), [PostgreSQL](https://www.postgresql.org),
  [NumPy](https://numpy.org), [Pillow](https://python-pillow.org),
  [cryptography](https://cryptography.io).

Mario Kart Wii and Wii are trademarks of Nintendo. This project is not affiliated with or
endorsed by Nintendo, WiiLink or the Dolphin project, and contains no Nintendo software:
you need your own legally obtained copy of the game.
