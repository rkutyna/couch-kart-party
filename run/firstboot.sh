#!/bin/zsh
# usage: firstboot.sh <k>
#   Scripted first boot for a FRESH profile, replaying the hand-proven taps on
#   player 1 (p1) only. Waits 12 s after launch, then for each step: tap A, wait,
#   take a screenshot and print its path. Sends input only; does not launch Dolphin.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: firstboot.sh <k>"; exit 2; }

[[ $# -eq 1 ]] || usage
k=$1
valid_k "$k" || die "bad instance number '$k'"

pid=$(read_pid "i$k")
pid_alive "$pid" || die "instance $k is not running (run launch.sh first)"

# Wait per step (seconds) and what the step is expected to reach.
waits=(4 4 4 4 3 4 5 5)
labels=(
  "health screen -> title"
  "title -> license select"
  "select NEW license"
  "confirm create"
  "choose first guest Mii"
  "confirm"
  "license is ready"
  "guest Mii notice -> main menu"
)

print -r -- "waiting 12 s after launch..."
sleep 12

for i in {1..8}; do
  "$SCRIPT_DIR/tap.sh" "$k" p1 A
  sleep "${waits[$i]}"
  shot=$("$SCRIPT_DIR/shot.sh" "$k")
  print -r -- "step $i (${labels[$i]}): $shot"
done
print -r -- "first boot sequence sent to instance $k; check the last screenshot"
