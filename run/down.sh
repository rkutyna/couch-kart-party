#!/bin/zsh
# usage: down.sh [--server] [--force]
#        down.sh --instance <k> [--force]
#   Without --instance: stops the phone streams, partyd, and ONLY the Dolphin
#   processes whose pids are recorded in run/pids/i<k>.pid. Leaves the game server
#   running unless --server is given.
#   With --instance k: stops only that instance's Dolphin (streams and partyd keep
#   running; a stream follows the instance when it is launched again).
#   Each pid is checked to be Dolphin started with `-u instances/i<k>` before it is
#   signalled. Dolphin gets SIGTERM and time to save. With --force, a Dolphin that
#   is still alive after the wait gets SIGKILL (that recorded pid only; a crashed
#   Dolphin ignores SIGTERM). Never uses pkill.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: down.sh [--server] [--force] | down.sh --instance <k> [--force]"; exit 2; }

stop_server=0; force=0; only=""
while (( $# )); do
  case $1 in
    --server) stop_server=1 ;;
    --force) force=1 ;;
    --instance) shift; [[ $# -ge 1 ]] || usage; only=$1; valid_k "$only" || die "bad instance number '$only'" ;;
    *) usage ;;
  esac
  shift
done
[[ -z $only || $stop_server -eq 0 ]] || usage

# stop_instance <pidfile>: returns 1 when the process is still running afterwards.
stop_instance() {
  local f=$1 name=${1:t:r} pid wait_s=15
  local k=${name#i}
  pid=$(<"$f")
  if [[ -z $pid ]] || ! pid_alive "$pid"; then
    print -r -- "$name: not running (stale pid file removed)"
    rm -f "$f"
    return 0
  fi
  if ! proc_is "$pid" "$DOLPHIN" || ! proc_is "$pid" "-u $(inst_dir "$k")"; then
    warn "$name: pid $pid is alive but is not this instance's Dolphin; leaving it and its pid file alone"
    return 1
  fi
  (( force )) && wait_s=8
  kill -TERM "$pid" 2>/dev/null || true
  if wait_exit "$pid" $wait_s; then
    print -r -- "$name: Dolphin pid $pid stopped"
    rm -f "$f"
    return 0
  fi
  if (( force )); then
    kill -KILL "$pid" 2>/dev/null || true
    if wait_exit "$pid" 5; then
      print -r -- "$name: Dolphin pid $pid ignored SIGTERM; killed with SIGKILL (--force)"
      rm -f "$f"
      return 0
    fi
  fi
  warn "$name: Dolphin pid $pid still running after $wait_s s (use --force, or quit it in the app)"
  return 1
}

if [[ -n $only ]]; then
  f=$(pid_file "i$only")
  if [[ ! -e $f ]]; then
    print -r -- "i$only: no pid file (not running)"
    exit 0
  fi
  stop_instance "$f"
  exit $?
fi

print -r -- "== streams"
"$SCRIPT_DIR/streams.sh" stop || warn "streams.sh stop reported a problem"

print -r -- "== partyd"
"$SCRIPT_DIR/party.sh" stop || warn "party.sh stop reported a problem"

print -r -- "== Dolphin (recorded pids only)"
rc=0
for f in "$PID_DIR"/i*.pid(N); do
  stop_instance "$f" || rc=1
done

if (( stop_server )); then
  print -r -- "== game server"
  "$SERVER_DIR/stop.sh"
else
  print -r -- "game server left running (use --server to stop it)"
fi
exit $rc
