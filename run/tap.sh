#!/bin/zsh
# usage: tap.sh <k> <p1|p2|hk> <BUTTON|LEFT|RIGHT|UP|DOWN> [hold_s=0.15]
#   Same behaviour as spike/tap.sh, for instances/i<k>. Button names are written
#   as PRESS/RELEASE lines; directions as SET MAIN lines, then re-centred.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: tap.sh <k> <p1|p2|hk> <BUTTON|LEFT|RIGHT|UP|DOWN> [hold_s=0.15]"; exit 2; }

[[ $# -ge 3 && $# -le 4 ]] || usage
k=$1; pipe=$2; btn=$3; hold=${4:-0.15}
valid_k "$k" || die "bad instance number '$k'"
case $pipe in p1|p2|hk) ;; *) usage ;; esac
[[ $hold == <-> || $hold == <->.<-> ]] || die "hold must be a number of seconds"

fifo=$(inst_dir "$k")/Pipes/$pipe

case $btn in
  LEFT)  fifo_send "$fifo" "SET MAIN 0 0.5";   sleep "$hold"; fifo_send "$fifo" "SET MAIN 0.5 0.5" ;;
  RIGHT) fifo_send "$fifo" "SET MAIN 1 0.5";   sleep "$hold"; fifo_send "$fifo" "SET MAIN 0.5 0.5" ;;
  UP)    fifo_send "$fifo" "SET MAIN 0.5 1";   sleep "$hold"; fifo_send "$fifo" "SET MAIN 0.5 0.5" ;;
  DOWN)  fifo_send "$fifo" "SET MAIN 0.5 0";   sleep "$hold"; fifo_send "$fifo" "SET MAIN 0.5 0.5" ;;
  *)
    [[ $btn =~ '^[A-Z_][A-Z0-9_]*$' ]] || usage
    fifo_send "$fifo" "PRESS $btn"
    sleep "$hold"
    fifo_send "$fifo" "RELEASE $btn"
    ;;
esac
