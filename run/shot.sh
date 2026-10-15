#!/bin/zsh
# usage: shot.sh <k>
#   Presses the hotkey (PRESS A on Pipes/hk = Take Screenshot), waits 1.2 s, and
#   prints the path of the newest PNG in instances/i<k>/ScreenShots/RMCE01/.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: shot.sh <k>"; exit 2; }

[[ $# -eq 1 ]] || usage
k=$1
valid_k "$k" || die "bad instance number '$k'"

dir=$(inst_dir "$k")
"$SCRIPT_DIR/tap.sh" "$k" hk A
sleep 1.2

shots_dir=$dir/ScreenShots/RMCE01
# (N) null glob, (.) regular files, (om) newest first
newest=( "$shots_dir"/*.png(N.om) )
(( ${#newest[@]} > 0 )) || die "no screenshot in $shots_dir yet"
print -r -- "${newest[1]}"
