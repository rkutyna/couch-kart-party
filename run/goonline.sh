#!/bin/zsh
# usage: goonline.sh <k>   (run after firstboot.sh on a fresh profile; UNTUNED timings)
# firstboot's last tap lands in "Single Player", so back out first.
D=${0:A:h}; k=$1
t() { $D/tap.sh $k p1 $1 >/dev/null; sleep $2; }
t B 2        # back to main menu
t DOWN 1     # highlight Nintendo WFC (1 Player)
t A 8        # enter; data notice
t A 3        # next
t A 3        # next -> Permit / Prohibit (Prohibit highlighted)
t UP 1       # Permit (online play requires it; data goes to the local private server)
t A 20       # connect
echo "instance $k: online sequence sent"
