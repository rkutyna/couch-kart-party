#!/bin/zsh
# usage: peek.sh <k> -> window capture of instance k in run/logs/peek<k>.png
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"
k=${1:?usage: peek.sh <k>}; pid=$(read_pid "i$k")
[[ -n $pid ]] || die "instance $k is not running"
w=$(winlist_run | awk -F'\t' -v p="$pid" '$2 == p && $3 ~ /RMCE01/ { print $1; exit }')
[[ -n $w ]] || die "no game window for instance $k"
screencapture -x -o -l "$w" "$LOG_DIR/peek$k.png" && print -r -- "$LOG_DIR/peek$k.png"
