#!/bin/zsh
# usage: snap.sh <label> -> peek all 3 instances, save refs nav/refs/<label>_<k>.png, composite to logs/peek123.png
D=${0:A:h}; for k in 1 2 3; do $D/peek.sh $k >/dev/null; cp $D/logs/peek$k.png $D/../nav/refs/$1_$k.png; done
ffmpeg -y -loglevel error -i $D/logs/peek1.png -i $D/logs/peek2.png -i $D/logs/peek3.png -filter_complex "[0]crop=1280:700:0:186,scale=560:-1[a];[1]crop=1280:700:0:186,scale=560:-1[b];[2]crop=1280:700:0:186,scale=560:-1[c];[a][b][c]hstack=3" $D/logs/peek123.png
