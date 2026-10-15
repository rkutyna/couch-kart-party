#!/bin/zsh
# usage: all.sh <wait> <BUTTON...>  -> tap each button on instances 1..3 (p1), then composite peek
D=${0:A:h}; w=$1; shift
for b in "$@"; do for k in 1 2 3; do $D/tap.sh $k p1 $b >/dev/null & done; wait; sleep 1.5; done
sleep $w; for k in 1 2 3; do $D/peek.sh $k >/dev/null; done
ffmpeg -y -loglevel error -i $D/logs/peek1.png -i $D/logs/peek2.png -i $D/logs/peek3.png -filter_complex "[0]scale=480:-1[a];[1]scale=480:-1[b];[2]scale=480:-1[c];[a][b][c]hstack=3" $D/logs/peek123.png
