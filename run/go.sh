#!/bin/zsh
# usage: go.sh <label> <wait> <BUTTON...> : tap buttons on all 3 (p1), wait, snap with label
D=${0:A:h}; l=$1; w=$2; shift 2
for b in "$@"; do for k in 1 2 3; do $D/tap.sh $k p1 $b >/dev/null & done; wait; sleep 1.2; done
sleep $w; $D/snap.sh $l
