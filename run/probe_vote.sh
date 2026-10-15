#!/bin/zsh
# One unattended pass: wait for idle-kick, reconnect, matchmake, confirm Check Members at once, capture vote screens.
D=${0:A:h}; cd $D
yavg() { ./peek.sh $1 >/dev/null 2>&1; ffmpeg -loglevel error -i logs/peek$1.png -vf "crop=1280:500:0:286,signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-" -f null - 2>/dev/null | grep -o "YAVG=[0-9.]*" | cut -d= -f2 | cut -d. -f1; }
all() { for k in 1 2 3; do ./tap.sh $k p1 $1 >/dev/null & done; wait; }
say() { echo "$(date +%T) $*"; }
say "waiting for disconnect notice (bright screen)"
for i in $(seq 1 240); do y=$(yavg 1); (( y > 200 )) && break; sleep 1; done; say "y=$y -> dismiss"
sleep 2; all A; sleep 8; ./snap.sh pv_mainmenu; all A; say "connecting"
for i in $(seq 1 60); do sleep 1; y=$(yavg 1); done   # ~75 s incl. peeks: generous connect wait
./snap.sh pv_wfcmenu; say "y=$y at wfc menu"
for i in 1 2 3 4 5; do all A; sleep 3; done; say "matchmaking"
for i in $(seq 1 120); do y=$(yavg 1); echo -n "$y "; (( y < 55 )) && break; sleep 0.5; done; echo; say "dark screen y=$y -> press A now"
all A; sleep 1.5; ./snap.sh pv_vote1; say "snap vote1"
./tap.sh 2 p1 RIGHT >/dev/null; ./tap.sh 3 p1 DOWN >/dev/null; sleep 1; ./snap.sh pv_vote2_moved
./tap.sh 1 p1 A >/dev/null; sleep 1.5; ./snap.sh pv_vote3_i1_A
./tap.sh 1 p1 DOWN >/dev/null; sleep 1; ./snap.sh pv_vote4_i1_down
./tap.sh 1 p1 A >/dev/null; sleep 1.5; ./snap.sh pv_vote5_i1_voted
./tap.sh 2 p1 A >/dev/null; sleep 1.5; ./tap.sh 2 p1 A >/dev/null; ./tap.sh 3 p1 A >/dev/null; sleep 1.5; ./tap.sh 3 p1 A >/dev/null; sleep 2; ./snap.sh pv_vote6_all
for i in 1 2 3 4 5 6; do sleep 3; ./snap.sh pv_after$i; done; say done
