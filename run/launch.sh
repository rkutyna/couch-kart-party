#!/bin/zsh
# usage: launch.sh <k> <1p|2p> [--setup]
#   Starts Dolphin for instance k in the background (nohup) and records its pid.
#   Refuses if the recorded pid is still alive or the profile is not provisioned.
#   Every launch starts with the audio on Cubeb at volume 100 but MUTED (Dolphin.ini
#   [DSP] Muted = True); the navigator unmutes the host's instance (nav/dolphin.py).
#   --setup: boot in 4:3 whatever the profile (nav.py setup drives the first-boot
#   screens with the 4:3 classifier, nav/screen43.py).
#   DOES NOT RUN ANYTHING UNTIL CALLED: this is the only script that starts Dolphin.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: launch.sh <k> <1p|2p> [--setup]"; exit 2; }

[[ $# -eq 2 || ( $# -eq 3 && $3 == --setup ) ]] || usage
k=$1; mode=$2; setup=${3:-}
valid_k "$k" || die "bad instance number '$k'"
valid_mode "$mode" || usage
[[ $(inst_mode "$k") == "$mode" ]] || die "instance $k is a $(inst_mode "$k") profile"

dir=$(inst_dir "$k")
[[ -f $dir/Config/Dolphin.ini && -p $dir/Pipes/p1 ]] \
  || die "instance $k is not provisioned; run provision.sh $k $mode first"
[[ -x $DOLPHIN ]] || die "Dolphin not found at $DOLPHIN"
[[ -f $GAME ]] || die "game file not found: $GAME"

mkdir -p "$LOG_DIR" "$PID_DIR"
pidf=$(pid_file "i$k")
old=$(read_pid "i$k")
if pid_alive "$old"; then
  die "instance $k already has a live Dolphin (pid $old, $pidf); run down.sh or kill it yourself first"
fi

# -C SYSCONF.IPL.AR: True = 16:9 (single player), False = 4:3 (two player split).
# (Verified 2026-10-14: i7 boots in 4:3 and i1 in 16:9 with these overrides.)
if [[ $mode == 1p && -z $setup ]]; then ar=True; else ar=False; fi
# Boot muted (deterministic audio state; see the header).
python3 -I "$RUN_DIR/src/ini_set.py" "$dir/Config/Dolphin.ini" DSP.Volume=100 DSP.Muted=True \
  || die "could not set the audio keys in $dir/Config/Dolphin.ini"

nohup "$DOLPHIN" -u "$dir" -b -v Metal -e "$GAME" -C "SYSCONF.IPL.AR=$ar" \
  > "$LOG_DIR/i$k.log" 2>&1 < /dev/null &
pid=$!
print -r -- "$pid" > "$pidf"
sleep 2
if pid_alive "$pid"; then
  print -r -- "started instance $k ($mode, AR=$ar) pid $pid, log $LOG_DIR/i$k.log"
else
  rm -f "$pidf"
  die "Dolphin for instance $k exited at once; see $LOG_DIR/i$k.log"
fi
