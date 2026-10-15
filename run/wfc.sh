#!/bin/zsh
# usage: wfc.sh <k>  from the Main Menu (Single Player highlighted): enter Nintendo WFC 1P, first time (with Permit prompt)
D=${0:A:h}; k=$1; t() { $D/tap.sh $k p1 $1 >/dev/null; sleep $2; }
t DOWN 1; t A 8; t A 3; t A 3; t UP 1; t A 22
