#!/bin/zsh
# usage: streams.sh start <k...> | stop [k...] | status
#   One capture/stream supervisor per Dolphin INSTANCE (run/stream_inst.sh): it writes
#   the navigator's frames (run/frames/i<k>.bmp) and sends the phone video of the
#   instance (one 16:9 stream, or two halves for a two-player instance) to partyd's
#   per-instance source ports. Which phone sees which source is decided in partyd
#   (POST /api/nav/assign), so streams are never restarted when the players change.
#   start:  starts the supervisors for the given instances that are not running yet
#           (idempotent). A supervisor waits while its Dolphin is not running and
#           re-attaches after a relaunch.
#   stop:   stops the given (default: all) supervisors recorded in run/pids/stream-i*.pid
#           and their wincap/ffmpeg children. Only recorded pids are signalled.
#   status: lists the recorded stream pids.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: streams.sh start <k...> | stop [k...] | status"; exit 2; }

stream_alive() {
  local pid
  pid=$(read_pid "stream-i$1")
  pid_alive "$pid" && proc_is "$pid" "stream_inst.sh"
}

stop_one() {
  local f=$1 name=${1:t:r} pid kids d
  pid=$(<"$f")
  if pid_alive "$pid" && proc_is "$pid" "stream_"; then
    kids=$(descendants "$pid") || true
    kill -TERM "$pid" 2>/dev/null || true        # the supervisor first, so it cannot restart the pipeline
    for d in ${(f)kids}; do kill -TERM "$d" 2>/dev/null || true; done
    if wait_exit "$pid" 5; then
      print -r -- "stopped $name (pid $pid)"
    else
      kill -KILL "$pid" 2>/dev/null || true
      warn "$name (pid $pid) needed SIGKILL"
    fi
    for d in ${(f)kids}; do pid_alive "$d" && kill -KILL "$d" 2>/dev/null || true; done
  else
    print -r -- "$name: not running (stale pid file)"
  fi
  rm -f "$f"
}

cmd_start() {
  local k pid
  mkdir -p "$LOG_DIR" "$PID_DIR"
  for k in "$@"; do
    [[ $k == <1-12> ]] || die "bad instance '$k'"
    if stream_alive "$k"; then
      print -r -- "i$k: stream already running (pid $(read_pid "stream-i$k"))"
      continue
    fi
    nohup "$SCRIPT_DIR/stream_inst.sh" "$k" >> "$LOG_DIR/stream-i$k.log" 2>&1 < /dev/null &
    pid=$!
    print -r -- "$pid" > "$(pid_file "stream-i$k")"
    print -r -- "i$k: stream supervisor pid $pid ($(inst_mode "$k"), log $LOG_DIR/stream-i$k.log)"
  done
}

cmd_stop() {
  local f k
  if (( $# )); then
    for k in "$@"; do
      f=$(pid_file "stream-i$k")
      [[ -e $f ]] && stop_one "$f"
    done
    return 0
  fi
  for f in "$PID_DIR"/stream-i*.pid(N) "$PID_DIR"/stream-s*.pid(N); do
    stop_one "$f"                        # stream-s*: per-slot streams of the older scheme
  done
}

cmd_status() {
  local f name pid any=0
  for f in "$PID_DIR"/stream-i*.pid(N); do
    any=1; name=${f:t:r}; pid=$(<"$f")
    if pid_alive "$pid"; then print -r -- "$name pid $pid alive"; else print -r -- "$name pid $pid not running"; fi
  done
  (( any )) || print -r -- "no stream pid files"
}

[[ $# -ge 1 ]] || usage
cmd=$1; shift
case $cmd in
  start)  (( $# >= 1 )) || usage; cmd_start "$@" ;;
  stop)   cmd_stop "$@" ;;
  status) cmd_status ;;
  *) usage ;;
esac
