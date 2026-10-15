# Navigator

Owns up to 6 running Dolphin instances of Mario Kart Wii (RMCE01) for up to 12 phones,
reads each game window, and drives each instance through the online menus with scripted
controller input, using the choices players made in the phone lobby (partyd). The
players end up in an online room at "Course Poll", where each phone gets its video and
its controls.

Instances and profiles (`run/lib.sh`, `inst_mode`, `layout`):

    i1..i6   single-player (1p), 16:9, pad Pipes/p1
    i7..i12  two-player split screen (2p), 4:3; player A = pad p1, top half;
             player B = pad p2, bottom half
    layout(N)  N <= 6: i1..iN;  N >= 7: i7.. (N // 2 of them) + i1 when N is odd

Every instance also has Pipes/hk (hotkeys: screenshot, Volume Up/Down).

    nav.py        command line (modes below)
    screen.py     16:9 capture -> state + cursor (classifier, templates from refs/)
    screen43.py   the same for 4:3 (two-player instances and setup boots; refs43/)
    dolphin.py    one instance: pid / window / capture / pads p1, p2 / hotkeys / relaunch
    driver.py     Worker (one thread per instance, closed loop) and Runner (lobby, pool)
    lobby.py      partyd client (/api/nav/*)
    offline_test.py, labels.json + refs/, labels43.json + refs43/   offline test
    templates.npz, templates43.npz   built by `nav.py build-templates`
                  (refs*, labels*.json and templates*.npz are game-derived and not in
                  git; see "Game assets" in the top-level README.md)
    localconf.py  reads mkparty.conf (DOLPHIN_APP, PARTY_PORT) like run/lib.sh
    fakephones/   configs for party/cmd/fakephones (test phones)
    logs/         nav-<mode>-<time>.log, -obs-i<k>.log, shots/<run>/, unknown/

## Requirements

* python3 with numpy and Pillow; swiftc (builds run/bin/winlist once); ffmpeg; the
  per-instance capture (run/streams.sh start k, started by the navigator) for frames.
* No DNS is needed: the patched game (Gecko code `$WFC local-ip 8080` from
  `patcher/out-local`) reaches the server by the IP literal 127.0.0.1 only.
  `prepare` / `run` first check that every provisioned instance carries that
  code (`server/netcheck.sh` shows the same and more). Only an instance still
  on the old `$WFC localtest.me 8080` code needs `*.localtest.me` to resolve.

## Modes

    nav.py test [-v]
        Offline classifier test over refs/ + labels.json and refs43/ + labels43.json;
        prints a per-state table and the confusions. Must print 100 % for both.
    nav.py build-templates
        Rebuild templates.npz and templates43.npz after adding a reference or changing a region.
    nav.py classify <png> ...
    nav.py setup [k ...] [--force] [--parallel 3] [--timeout 420]
        (./mkparty setup --profiles) Once per profile: provision, boot in 4:3 (--setup), go through
        the first-boot screens with the game's own menus (create the license, the data
        notice and Permit; for 2p the second controller's registration and its Mii),
        connect to Nintendo WFC once, stop, write instances/i<k>/mkparty-ready. Default: all
        profiles that are not ready, 3 at a time.
    nav.py watch [k ...] [--seconds S] [--interval S]
        Print every instance's screen. Sends no input.
    nav.py run [--warm N] [--keepalive S] [--no-infra]
        (./mkparty up) Starts what is missing (game server, partyd with 12 slots and the
        per-instance sources), then follows the lobby for ever:
        lobby     a warm pool of N single-player instances (NAV_WARM, default 3; two-
                  player: NAV_WARM_2P, default 0) holds at the WFC menu. When the number
                  of occupied slots has held for 8 s, instances for layout(occupied)
                  are started in the background (at most 3 booting at once); instances
                  not needed for 90 s are stopped. run/nav-status.json says when all
                  wanted instances are ready (mkparty up waits for it).
        starting  at Start: lockedSlots -> layout -> stop the unneeded instances, start
                  the missing ones ("Starting N more games..."), POST /api/nav/assign
                  (slot -> instance, pad; partyd routes the video source and the
                  controller), unmute the host's instance only. Each instance goes to
                  the WFC menu; then they enter Worldwide > VS one by one (5 s apart, 3 s
                  when all are 1p; 1p first) and go straight through character, vehicle
                  and drift (no hold: the game confirms these screens by itself about
                  25 s after VS was chosen), Check Members, the vote, Course Poll. At
                  Course Poll the instance's slots get showVideo=true, navControl=false.
                  "live" when all are handed over, or 4 s after the first hand-over.
        live      as before: race over -> vote picker on the phone -> Next Race ->
                  Check Members -> the vote dialog -> hand back at Course Poll.
    nav.py pick <k> --character ID --vehicle ID --drift auto|manual [--vote ID] [--go]
        Debugging without a lobby: drive instance k from its current screen to
        Drift with these choices and hold (--go: also confirm and vote).
    nav.py shot <k> <file.png>

Fake phones (built by `./mkparty setup`, or `cd party && go build -o ../run/bin/fakephones ./cmd/fakephones`;
minimap icons from party/cmd/fakephones/icons, not in git):

    run/bin/fakephones -config nav/fakephones/p7.json -wait-msg Ready \
        [-rounds 5 -reset-after 20s] [-after-live wiggle|idle] [-duration 30m] [-dump DIR]

Configs p2, p3, p4, p6, p7, p11, p12 (player counts), p7-luigi (all vote Luigi Circuit),
p7-probe (one phone idle, to see which kart moves). Per phone `afterLive` overrides
-after-live. -dump DIR records 4 s of each phone's received video after its hand-over
(<time>_slot<N>.h264), to check which picture a phone gets.

## How a screen is recognised

Input is the window capture (1280x1016). A capture of another size, or one whose
letterbox bars are not black (window mid-animation), is "unknown". The 16:9 picture
is frame rows 186..886 (screen.py); the 4:3 picture is rows 56..1016 (screen43.py, same
states plus the first-boot ones: create_license, wfc_notice, permit_prompt, wont_play,
controller_reg, choose_mii, connect2; 2p cursors are pairs, one per player). Templates
larger than 3000 px are matched at half resolution (NAV_POOL_MIN), which keeps an
observation at 0.1-0.3 s with six instances. In order:

| state | how |
|---|---|
| emu_alert | Dolphin's own alert sheet ("Invalid read ...") by its text |
| welcome, disconnected_dialog, error_dialog | green (A) prompt icon + white box in the middle; the text decides between welcome / disconnected; any other text or error code is error_dialog |
| vote_prompt | "Vote for the next course?" text; cursor Vote / Random from the label colour |
| health, title, preparing, opponents_chosen | their full-screen text |
| main_menu | title strip + hint line bottom-right (cursor: single / wfc / wfc2 / multi / channel / license) |
| license_select, searching, check_members, course_poll | title strip (check_members also counts the player rows, course_poll reads waiting / selected) |
| connecting | title "Nintendo WFC (1P)" + "Connecting to Nintendo WFC..." |
| wfc_menu | same title + exactly one white button label (worldwide / regional / friends) |
| ww_mode | title "Worldwide" + white label (vs / battle) |
| characters, vehicles | title + the name label under the model, matched against one template per catalog id |
| drift | title + hint line (auto / manual / help), which must agree with the white label |
| courses_cups | title "Courses" + the cup name under the highlighted cup |
| courses_list | title "Courses" + exactly one white course row (+ the cup banner) |
| other_menu | menu frame + Back button with an unknown title (single-player menus, License Settings) |
| race | no menu frame and a colourful picture (weak; never acted on, except to leave the title demo) |
| black | black picture |
| unknown | everything else: fades, slides, a title whose body check fails |

The title strip is compared by normalised correlation and must beat the
runner-up by a margin. Cursors come from text, not from the highlight colour,
because the highlight pulses. Name labels use only the bright part of the
image, so the moving background does not matter. Menu positions (which cell a
character, vehicle, cup or course is in) come from party/web/catalog.json only.

## How it drives

* Frames: the instance's capture (run/stream_inst.sh, one per instance) writes the full
  window frame to run/frames/i<k>.bmp 12 times a second (NAV_FPS) next to the phone
  video (1p: the 16:9 picture to source port 5100+4(k-1); 2p: the top half to that port,
  the bottom half to +2). `screencapture` is only a fallback. Window ids come from
  run/bin/winlist (CGWindowListCopyWindowInfo; `wincap list` can hang).
* One thread per instance. An observation counts when the same (state, cursor) is seen
  on 2 consecutive frames; only then is ONE action issued.
* After a press the worker waits for the observation to change. A button counts as done
  only when the SCREEN changes (a cursor flicker is not enough). A press is repeated only
  when fresh frames taken a full timeout after it still show the same screen (1 s for
  moves, 2.5 s otherwise, 0.33 s for A on Check Members, which ignores input for a
  moment after it appears), at most 3 times (10 for Check Members).
* Grids wrap (characters both ways, vehicles and the course list vertically, cups skip
  locked cups). Moves come from the catalog position of the name on screen and the
  target; A is pressed only when the name on screen is the wanted one.
* Start: see `run`. Entering one by one lets the first instance open the room and the
  others find it. Phase "live" is posted when all are handed over, or 4 s after the first
  hand-over if an instance missed the room (partyd drops all phone input while the phase
  is "starting"); slots still being driven keep navControl and are handed over when they
  get in.
* Two-player instances: both players' choices are made on one screen. The navigator
  moves both cursors in parallel (B first out of A's target cell: two cursors cannot share
  a character), vehicles are a per-player LEFT/RIGHT carousel, and A is pressed for both
  players together; a press is repeated after 6 s if the screen has not moved on. Only
  player A's vote counts in the game (one vote per console): A's choice in odd races, B's
  in even ones (the other's if one picked Random).
* Check Members: the member count is only taken when the same count was read twice in a
  row (a row fading in was read as one more player).
* Race state: a "race" observation is kept for 1.5 s through HUD-less frames, so the
  state no longer flickers to unknown during a race.
* Next race (phase live), per slot:
  1. Race over: as soon as the instance shows the results list, "Next Race / Quit Game",
     "Please wait a moment" or anything later (Check Members, vote dialog), the navigator
     opens the phone's vote picker: POST /api/nav/update {voteOpen: true, voteLocked:
     false, voteDeadlineMs: now + 40 s (NAV_VOTE_INITIAL)}.
  2. "Next Race / Quit Game": it takes navControl and presses A on Next Race.
  3. Check Members: A (as for the first race).
  4. Vote dialog ("Vote for the next course?"): the deadline is tightened to 16 s after
     the dialog appeared (NAV_VOTE_WAIT; POSTed without touching voteLocked). The
     navigator waits there until the phone has locked in (voteLocked), the phone is not
     connected, or the deadline passes; then it enters the player's CURRENT vote (cup,
     course; Random via the dialog) like in the first race.
  5. Course Poll: voteOpen=false, voteDeadlineMs=0, video on, control back to the phone.
  Measured 2026-10-14: without input the game itself moves on to Course Poll 28.5 s and
  29.4 s after the vote dialog appears (one instance once only after 56.7 s); entering a
  vote takes 1-6 s, so 16 s leaves a margin of about 6 s. The picker is typically open
  for about 13-17 s before the dialog appears (results, Next Race, Check Members).
* Out of the room (disconnect / error dialog): back to the WFC menu, waiting 8 s
  (NAV_REJOIN_WAIT) before reconnecting. When everybody who raced is out, the
  navigator POSTs /api/nav/reset.
* Recovery: Dolphin process gone, game window gone for 20 s (60 s after a launch; a window
  list that could not be read counts as unknown, not as gone), Dolphin's alert window
  present with a black picture, or the process idle below 4 % CPU for 20 s with no new
  picture for 10 s (emulation stopped / frozen): relaunch that instance (run/down.sh
  --instance k --force, run/launch.sh), at most 3 times in 10 minutes, status "Game
  restarted. Rejoining...". The Dolphin profiles have
  UsePanicHandlers = False, so an emulated crash ("Invalid read from ...") is written to
  run/logs/i<k>.log instead of pausing behind a dialog; the navigator copies such lines
  into its log. No progress for 45 s on the way to a menu presses B (never inside the room, on the
  results, "Please wait" or during a race); 150 s relaunches.
  A search longer than 150 s is backed out and repeated.
* Logs: every state change and action with millisecond time stamps in
  logs/nav-<run>.log; every observation in logs/nav-<run>-obs-i<k>.log; captures on
  entering the room screens, before each pick, on Course Poll (2, 5, 9, 14, 20 s) and
  every 30 s during a race in logs/shots/<run>/; screens unknown for 3 s in logs/unknown/.
  At start the navigator deletes the oldest runs' logs when logs/ is above 1.5 GB
  (NAV_LOG_CAP_MB), down to 80 % of it, and keeps the newest 20 run/logs/phonestats files.
* Sound: every instance boots with Dolphin's audio on but muted (run/launch.sh sets
  [DSP] Volume = 100, Muted = True). At Start the host's instance (the instance of the
  host slot, else the lowest locked slot) is unmuted and all others are muted, by
  Dolphin's Volume Up / Volume Down hotkeys sent on Pipes/hk 34 times (Volume Up also
  clears Muted; 34 x 3 steps reach 100 or 0 from any value), so the result does not
  depend on the earlier state. Hotkeys work without focus because the profiles have
  [General] HotkeysRequireFocus = False.

## Known limitations

* The Dolphin windows must stay on the Mac's screen: when they are hidden (another
  desktop, a full-screen app) macOS delivers few frames, the navigator gets slower and
  the phone video stutters.
* While the Mac's screen is locked the capture delivers no frames at all: the workers
  log nothing and wait (2026-10-15 01:50-02:00); `mkparty up` warns after 300 s.
* Matchmaking is the game's. The local server ignores the `p` term of the VS search
  (server/wfc-server/filter/eval.go). A console starts searching as soon as VS is chosen
  (server: GPCM status 3) and is joinable as a host once it has confirmed drift. The
  host's game refuses newcomers ("RESV_DENY ... This Domain is already closed (0x11)" in
  the server log) 3-8 s after its room reached 6 or more players: all 7 rooms that got a
  denial within 45 s of the host's VS (2026-10-14/15) had 6-8 players. Rooms with fewer
  players accepted joiners much later (5 players: still open after 40 s); the only
  denials to smaller rooms came 51-114 s after the host's VS. Joins are one at a time (others get
  RESV_WAIT) and take a few seconds each, so with 2p instances a room ends at 6-8
  players: 8-12 players split into two rooms (each races and is handed over); 7 always
  fit (5 + a 2p console). `WWFC_DEBUG_MATCH=1 server/start.sh` logs each search filter
  and room candidate; `WWFC_PREFER_ROOM=1` makes the server offer a searcher only the
  open host with the most members (untested in a live round).
* logs/ grows by roughly 50-100 MB per hour of racing (captures); see the pruning above.
* The game drops a player who makes no progress for roughly 30-60 s (no input, or stuck
  against a wall). When everybody else has been dropped, the last player gets the results
  at once and is then disconnected when the next race is prepared (nobody to race with).
* The test phones (party/cmd/fakephones, -after-live drive) can finish races only on
  Luigi Circuit: they find their own (bigger) icon on the minimap, read the heading from
  the yellow view cone and follow a traced centre line; on other courses they wander
  and are dropped. On 2p instances (one shared, rotated minimap) they are often dropped
  even there.
* `nav.py setup` handled fresh saves for i4-i6 and i8-i12 (license, consent, second
  player). A first-boot screen not in refs43/ would stop setup of that profile (it then
  says FAILED and leaves no mkparty-ready mark).
* Results / "Next Race / Quit Game" / "Please wait a moment" are classified (states
  results, next_race_menu, please_wait); the team/points variants of the results screen
  have not been seen (only VS races were tested).

## Adding a reference

Copy a capture (`nav.py shot`, or from logs/shots, logs/unknown) to refs/, add
it to labels.json (`{"state": ..., "cursor": ...}`), and, if it should be a
template, to TEMPLATES / NAME_REFS in screen.py (4:3 captures: refs43/, labels43.json,
screen43.py). Then `nav.py build-templates`
and `nav.py test`.
