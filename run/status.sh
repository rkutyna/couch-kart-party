#!/bin/zsh
# usage: status.sh
#   Read-only. Shows which recorded pids are alive, whether the game server is
#   listening, and partyd's /api/state when partyd is up.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: status.sh"; exit 2; }
[[ $# -eq 0 ]] || usage

print -r -- "== game server"
if lsof -nP -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1; then
  print -r -- "  NAS 127.0.0.1:8080 listening"
else
  print -r -- "  NAS 127.0.0.1:8080 not listening"
fi

print -r -- "== name resolution"
"$SERVER_DIR/netcheck.sh" || true

print -r -- "== Dolphin"
any=0
for f in "$PID_DIR"/i*.pid(N); do
  any=1; name=${f:t:r}; pid=$(<"$f")
  if pid_alive "$pid"; then print -r -- "  $name pid $pid alive"; else print -r -- "  $name pid ${pid:-?} not running"; fi
done
(( any )) || print -r -- "  no Dolphin pid files"

print -r -- "== streams"
any=0
for f in "$PID_DIR"/stream-i*.pid(N); do
  any=1; name=${f:t:r}; pid=$(<"$f")
  if pid_alive "$pid"; then print -r -- "  $name pid $pid alive"; else print -r -- "  $name pid ${pid:-?} not running"; fi
done
(( any )) || print -r -- "  no stream pid files"

print -r -- "== partyd"
pid=$(read_pid partyd)
if pid_alive "$pid"; then
  print -r -- "  partyd pid $pid alive on :$PARTY_PORT"
  curl -s "http://127.0.0.1:$PARTY_PORT/api/state" || print -r -- "  (no answer from /api/state)"
  print -r -- ""
else
  print -r -- "  partyd not running"
fi
