#!/bin/zsh
# usage: up.sh <N>
#   Starts the game server (server/start.sh, idempotent), provisions every
#   instance for the N-player layout, launches Dolphin for each (instances that
#   are already running are left alone), then prints the manual next steps.
#   Does NOT run firstboot, streams or party; those are manual.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: up.sh <N>   (N = 1..12 players)"; exit 2; }

[[ $# -eq 1 ]] || usage
n=$1
valid_n "$n" || die "N must be 1..12"

print -r -- "== game server"
"$SERVER_DIR/start.sh"

local_lines=( "${(@f)$(layout "$n")}" )
fresh=()

print -r -- "== provisioning"
for line in "${local_lines[@]}"; do
  fld=( ${=line} )
  k=$fld[2]; mode=$fld[3]
  if [[ ! -d $(inst_dir "$k")/Wii ]]; then
    fresh+=("$k")                       # no Wii/ yet: this profile has never booted
  fi
  "$SCRIPT_DIR/provision.sh" "$k" "$mode"
done

print -r -- "== launching"
for line in "${local_lines[@]}"; do
  fld=( ${=line} )
  k=$fld[2]; mode=$fld[3]
  pid=$(read_pid "i$k")
  if pid_alive "$pid"; then
    print -r -- "instance $k already running (pid $pid); not relaunching"
  else
    "$SCRIPT_DIR/launch.sh" "$k" "$mode"
  fi
done

print -r -- ""
print -r -- "== next steps (manual)"
if (( ${#fresh[@]} > 0 )); then
  print -r -- "1. Fresh profiles (never booted): first boot each one, one at a time (about 50 s each):"
  for k in "${fresh[@]}"; do
    print -r -- "     $SCRIPT_DIR/firstboot.sh $k"
  done
else
  print -r -- "1. No fresh profiles; skip first boot."
fi
print -r -- "2. Start the phone streams:  $SCRIPT_DIR/streams.sh start $n"
print -r -- "3. Start the phone server:   $SCRIPT_DIR/party.sh start $n"
print -r -- "4. Phones open:              $PHONE_URL"
