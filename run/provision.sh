#!/bin/zsh
# usage: provision.sh <k> <1p|2p> [--force-config]
#   Idempotently creates the Dolphin user directory instances/i<k> (like
#   spike/mkinst.sh). Config files are written only if they do not exist yet;
#   --force-config rewrites them (keeping a WirelessMac line Dolphin added).
#   Never touches Wii/ save data. Creates the FIFOs Pipes/p1, Pipes/p2, Pipes/hk.
#   Profiles i1..i6 are single-player (1p), i7..i12 two-player (2p); the mode must match.
#   Every run (also without --force-config) sets the keys the party relies on in the
#   existing Dolphin.ini (audio backend, hotkeys without window focus, no OSD messages,
#   the second pad for 2p) and rewrites Hotkeys.ini (screenshot + volume hotkeys).
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: provision.sh <k> <1p|2p> [--force-config]"; exit 2; }

[[ $# -ge 2 ]] || usage
k=$1; mode=$2; shift 2
force=0
for a in "$@"; do
  case $a in
    --force-config) force=1 ;;
    *) usage ;;
  esac
done
valid_k "$k" || die "bad instance number '$k'"
valid_mode "$mode" || usage
[[ $(inst_mode "$k") == "$mode" ]] || die "instance $k is a $(inst_mode "$k") profile (i1..i6: 1p, i7..i12: 2p)"

# Single-player: 16:9 window, only pad 1 in use. Two-player: 4:3, both pads.
if [[ $mode == 2p ]]; then
  sidev1=6; win_w=640; win_h=480
else
  sidev1=0; win_w=854; win_h=480
fi

[[ -f $GECKO_SRC ]] || die "missing gecko file $GECKO_SRC"
[[ "$(head -n 1 "$GECKO_SRC")" == "$GECKO_HEAD" ]] \
  || die "unexpected first line in $GECKO_SRC (expected: $GECKO_HEAD)"
[[ "$(tail -c 1 "$GECKO_SRC" | od -An -tx1 | tr -d ' ')" == 0a ]] \
  || die "$GECKO_SRC must end with a newline"

dir=$(inst_dir "$k")
ini=$dir/Config/Dolphin.ini

if (( force )); then
  pid=$(read_pid "i$k")
  if pid_alive "$pid"; then
    die "refusing --force-config: instance $k is running (pid $pid)"
  fi
fi

mkdir -p "$dir/Config" "$dir/GameSettings" "$dir/Pipes"

# put_file <dest>: reads the new content on stdin. Keeps an existing file unless
# --force-config. Always drains stdin so the producer never gets SIGPIPE.
put_file() {
  local dest=$1 rel=${1#$dir/}
  if [[ -e $dest ]] && (( ! force )); then
    cat > /dev/null
    print -r -- "  keep   $rel (exists)"
    return 0
  fi
  cat > "$dest.tmp"
  mv -f "$dest.tmp" "$dest"
  print -r -- "  write  $rel"
}

render_dolphin_ini() {
  local wm=""
  if [[ -f $ini ]]; then
    wm=$(grep -E '^WirelessMac[[:space:]]*=' "$ini" | head -n 1) || true
  fi
  print -r -- "[General]"
  [[ -n $wm ]] && print -r -- "$wm"
  cat <<INI
HotkeysRequireFocus = False
[Core]
SIDevice0 = 6
SIDevice1 = $sidev1
SIDevice2 = 0
SIDevice3 = 0
EnableCheats = True
[Input]
BackgroundInput = True
[Interface]
ConfirmStop = False
UsePanicHandlers = False
OnScreenDisplayMessages = False
[DSP]
Backend = Cubeb
Volume = 100
Muted = True
[Analytics]
PermissionAsked = True
Enabled = False
[Display]
RenderWindowWidth = $win_w
RenderWindowHeight = $win_h
RenderWindowAutoSize = False
INI
}

# Logger.ini: Dolphin's panic alerts ("Invalid read from ...") are not shown as a pausing
# dialog (UsePanicHandlers = False above); they are written as errors to the console
# (run/logs/i<k>.log) and to Logs/dolphin.log in the profile instead.
render_logger_ini() {
  cat <<'INI'
[Options]
WriteToConsole = True
WriteToFile = True
WriteToWindow = False
Verbosity = 2
[Logs]
MASTER = True
MI = True
CORE = True
PowerPC = True
COMMON = True
BOOT = True
INI
}

render_gfx_ini() {
  cat <<'INI'
[Settings]
InternalResolution = 1
ShowFPS = False
ShowSpeed = False
INI
}

render_gcpad_ini() {
  local i
  for i in 1 2; do
    print -r -- "[GCPad$i]"
    cat <<INI
Device = Pipe/0/p$i
Buttons/A = \`Button A\`
Buttons/B = \`Button B\`
Buttons/X = \`Button X\`
Buttons/Y = \`Button Y\`
Buttons/Z = \`Button Z\`
Buttons/Start = \`Button START\`
Main Stick/Up = \`Axis MAIN Y +\`
Main Stick/Down = \`Axis MAIN Y -\`
Main Stick/Left = \`Axis MAIN X -\`
Main Stick/Right = \`Axis MAIN X +\`
Triggers/L = \`Button L\`
Triggers/R = \`Button R\`
D-Pad/Up = \`Button D_UP\`
D-Pad/Down = \`Button D_DOWN\`
D-Pad/Left = \`Button D_LEFT\`
D-Pad/Right = \`Button D_RIGHT\`
INI
  done
}

render_wiimote_ini() {
  cat <<'INI'
[Wiimote1]
Source = 0
[Wiimote2]
Source = 0
[Wiimote3]
Source = 0
[Wiimote4]
Source = 0
INI
}

# Hotkeys on the FIFO Pipes/hk (HotkeysRequireFocus = False in Dolphin.ini, so they work
# while the window is in the background). The volume keys change the volume in steps of 3
# (0..100); Volume Up / Down also clear "Muted". The navigator sets the volume
# deterministically with 34 presses (nav/dolphin.py set_audio), never with the toggle.
render_hotkeys_ini() {
  cat <<'INI'
[Hotkeys]
Device = Pipe/0/hk
General/Take Screenshot = `Button A`
Volume/Volume Up = `Button Y`
Volume/Volume Down = `Button Z`
INI
}

# Per-game settings: the Gecko code block, and the enabled-code list naming it.
render_gecko_ini() {
  print -r -- "[Gecko]"
  cat "$GECKO_SRC"
  print -r -- "[Gecko_Enabled]"
  head -n 1 "$GECKO_SRC"
}

print -r -- "profile $dir ($mode, ${win_w}x${win_h}, forced=$force)"
render_dolphin_ini | put_file "$ini"
render_gfx_ini | put_file "$dir/Config/GFX.ini"
render_logger_ini | put_file "$dir/Config/Logger.ini"
render_gcpad_ini | put_file "$dir/Config/GCPadNew.ini"
render_wiimote_ini | put_file "$dir/Config/WiimoteNew.ini"
render_hotkeys_ini | force=1 put_file "$dir/Config/Hotkeys.ini"
render_gecko_ini | put_file "$dir/GameSettings/RMCE01.ini"
# Keys the party needs, also in an older existing Dolphin.ini (other lines are kept).
python3 -I "$RUN_DIR/src/ini_set.py" "$ini" General.HotkeysRequireFocus=False \
  Interface.OnScreenDisplayMessages=False Interface.ConfirmStop=False Interface.UsePanicHandlers=False \
  DSP.Backend=Cubeb Core.SIDevice1=$sidev1 Input.BackgroundInput=True
print -r -- "  set    Config/Dolphin.ini party keys (audio Cubeb, hotkeys without focus, no OSD)"

for p in p1 p2 hk; do
  f=$dir/Pipes/$p
  if [[ -p $f ]]; then
    print -r -- "  ok     Pipes/$p (fifo)"
  elif [[ -e $f ]]; then
    die "$f exists and is not a FIFO"
  else
    mkfifo "$f"
    print -r -- "  mkfifo Pipes/$p"
  fi
done
print -r -- "done: instance $k is provisioned"
