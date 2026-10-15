# lib.sh - shared settings and helpers for the Mario Kart Wii party launcher.
# Sourced by the other scripts in run/. Not meant to be executed directly.
#   source "$SCRIPT_DIR/lib.sh"
# Every path is derived from this file's location (it lives in $ROOT/run/).

RUN_DIR=${${(%):-%x}:A:h}
ROOT=${RUN_DIR:h}
LOG_DIR=$RUN_DIR/logs
PID_DIR=$RUN_DIR/pids
INST_ROOT=$ROOT/instances
SLOTS_JSON=$RUN_DIR/slots.json

# Local settings of this Mac: mkparty.conf in $ROOT (not in git; created by
# ./mkparty setup from mkparty.conf.example). Plain KEY="value" lines.
MKPARTY_CONF=${MKPARTY_CONF:-$ROOT/mkparty.conf}
[[ -f $MKPARTY_CONF ]] && source "$MKPARTY_CONF"
DOLPHIN_APP=${${DOLPHIN_APP:-/Applications/Dolphin.app}%/}
GAME_IMAGE=${GAME_IMAGE:-}
DOLPHIN=$DOLPHIN_APP/Contents/MacOS/Dolphin
GAME=$GAME_IMAGE

# Homebrew prefix (/opt/homebrew on Apple Silicon); its bin goes on PATH for ffmpeg etc.
if [[ -z ${BREW_PREFIX:-} ]]; then
  if [[ -n ${HOMEBREW_PREFIX:-} ]]; then BREW_PREFIX=$HOMEBREW_PREFIX
  elif command -v brew >/dev/null 2>&1; then BREW_PREFIX=$(brew --prefix 2>/dev/null)
  elif [[ -x /opt/homebrew/bin/brew ]]; then BREW_PREFIX=/opt/homebrew
  else BREW_PREFIX=/usr/local; fi
fi
[[ :$PATH: == *:$BREW_PREFIX/bin:* ]] || PATH=$PATH:$BREW_PREFIX/bin
# Python packages (numpy, Pillow): the virtualenv that setup creates when the system
# python3 lacks them.
[[ -x $ROOT/.venv/bin/python3 && :$PATH: != *:$ROOT/.venv/bin:* ]] && PATH=$ROOT/.venv/bin:$PATH
export PATH
FFMPEG=${FFMPEG:-$(command -v ffmpeg 2>/dev/null || print -r -- "$BREW_PREFIX/bin/ffmpeg")}

# Gecko code (stage 0 of the WFC patch), built by setup from the patched wfc-patcher-wii.
# out-local = no-DNS build: every server URL / host name is the IP literal 127.0.0.1
# (works offline, on any network).
GECKO_SRC=${GECKO_SRC:-$ROOT/patcher/out-local/RMCE01.gecko.txt}
GECKO_HEAD=${GECKO_HEAD:-'$WFC local-ip 8080'}
SERVER_DIR=$ROOT/server
WINCAP=$ROOT/capture/bin/wincap
STREAM_SH=$ROOT/capture/stream_window.sh
PARTYD=$ROOT/party/partyd

PARTY_PORT=${PARTY_PORT:-8000}
HTTPS_PORT=${HTTPS_PORT:-8443}
export PARTY_PORT     # nav/lobby.py talks to partyd on this port
PHONE_URL=http://$(scutil --get LocalHostName 2>/dev/null || hostname -s).local:$PARTY_PORT/
SECURE_URL=https://$(scutil --get LocalHostName 2>/dev/null || hostname -s).local:$HTTPS_PORT/
RTP_BASE=5004          # per-slot ingest ports of partyd (5004, 5006, ...; test patterns only)
SRC_BASE=5100          # per-instance stream ports: instance k pad p -> SRC_BASE + 4(k-1) + 2(p-1)
MAX_INST=12

# HTTPS for tilt steering. party.sh uses party/certs/fullchain.pem + privkey.pem
# when present, else selfsigned-*.pem (created by make-cert.sh on first start).
CERT_DIR=$ROOT/party/certs
MAKE_CERT=$ROOT/party/make-cert.sh
# Phone stats: partyd writes phonestats-<start>.jsonl here.
STATS_DIR=$LOG_DIR

# Height of the window title bar in captured FRAME PIXELS. 56 is a guess for a
# 2x Retina display (28 pt title bar). UNVERIFIED: tune it by looking at a
# capture. Override per run with: TITLEBAR_PX=60 ./streams.sh start 7
TITLEBAR_PX=${TITLEBAR_PX:-56}

die() { print -u2 -- "error: $*"; exit 1; }
warn() { print -u2 -- "warning: $*"; }

valid_k()  { [[ "${1:-}" == <1-999> ]]; }
valid_n()  { [[ "${1:-}" == <1-12> ]]; }
valid_mode() { [[ "${1:-}" == (1p|2p) ]]; }

inst_dir() { print -r -- "$INST_ROOT/i$1"; }
pid_file() { print -r -- "$PID_DIR/$1.pid"; }

# read_pid <name>: prints the recorded pid (empty if no file). Always returns 0.
read_pid() {
  local f
  f=$(pid_file "$1")
  if [[ -s $f ]]; then
    print -r -- "$(<"$f")"
  fi
  return 0
}

# pid_alive <pid>: true if the pid is a live process we can signal.
pid_alive() {
  [[ "${1:-}" == <-> ]] && kill -0 "$1" 2>/dev/null
}

# proc_is <pid> <text>: true if the live process command line contains <text>.
proc_is() {
  local c
  c=$(ps -p "$1" -o command= 2>/dev/null) || return 1
  [[ "$c" == *"$2"* ]]
}

# descendants <pid>: prints every descendant pid (children before parents).
descendants() {
  local p out
  out=$(pgrep -P "$1" 2>/dev/null) || true
  for p in ${(f)out}; do
    descendants "$p"
    print -r -- "$p"
  done
  return 0
}

# kill_tree <pid> [SIG]: signals the pid and its descendants. Only ever called
# with pids that these scripts recorded in run/pids.
kill_tree() {
  local pid=$1 sig=${2:-TERM} d out
  out=$(descendants "$pid") || true
  for d in ${(f)out}; do
    kill -"$sig" "$d" 2>/dev/null || true
  done
  kill -"$sig" "$pid" 2>/dev/null || true
  return 0
}

# wait_exit <pid> <seconds>: 0 once the pid is gone, 1 if still alive at timeout.
wait_exit() {
  local pid=$1 secs=$2 i
  for (( i = 0; i < secs * 5; i++ )); do
    pid_alive "$pid" || return 0
    sleep 0.2
  done
  pid_alive "$pid" && return 1
  return 0
}

# Profiles: instances/i1 .. i6 are single-player profiles (16:9, one pad), i7 .. i12 are
# two-player split-screen profiles (4:3, pads p1 + p2). Each has its own save and online
# identity (never copy saves between them).
# inst_mode <k>: prints 1p or 2p.
inst_mode() {
  if (( $1 >= 7 )); then print -r -- 2p; else print -r -- 1p; fi
}

# src_port <k> <pad>: the UDP port the instance's stream for that pad goes to (partyd).
src_port() {
  print -r -- $(( SRC_BASE + 4 * ($1 - 1) + 2 * ($2 - 1) ))
}

# layout <N>: prints one line per instance:  instance <k> <1p|2p> <slotA> [slotB]
#   N <= 6 : N single-player instances i1..iN, slot k on instance k.
#   N  > 6 : floor(N/2) two-player instances i7.. (A = top half = pad p1,
#            B = bottom half = pad p2), plus single-player i1 if N is odd.
#   (The navigator computes the same layout for the locked race roster; slots there are
#   the locked slot numbers, not 1..N.)
layout() {
  local n=$1 k pairs slot=1
  valid_n "$n" || die "N must be 1..12 (got '$n')"
  if (( n <= 6 )); then
    for (( k = 1; k <= n; k++ )); do
      print -r -- "instance $k 1p $k"
    done
  else
    pairs=$(( n / 2 ))
    for (( k = 1; k <= pairs; k++ )); do
      print -r -- "instance $(( k + 6 )) 2p $slot $(( slot + 1 ))"
      slot=$(( slot + 2 ))
    done
    if (( n % 2 == 1 )); then
      print -r -- "instance 1 1p $slot"
    fi
  fi
  return 0
}

# frame_size <dolphin-pid>: prints "W H" in capture frame pixels (2x the window's size in
# points, from winlist). The old `wincap capture --pid ... --print-size-only` query hangs.
frame_size() {
  local line
  line=$(winlist_run | awk -F'\t' -v p="$1" '$2 == p && $3 ~ /RMCE01/ { print $4 * 2, $5 * 2; exit }')
  [[ -n $line ]] || return 1
  print -r -- "$line"
  return 0
}

# crop_args <1p|2p> <W> <H> <titlebar_px>: prints one crop=x:y:w:h per slot
# (slot A first). Crops are in frame pixels, all values even, as stream_window.sh
# requires. The title bar is always removed from the top.
crop_args() {
  local mode=$1 w=$2 h=$3 tb avail half
  tb=$(( $4 & ~1 ))
  (( w > 0 && h > 0 && tb >= 0 && tb < h )) || return 1
  avail=$(( h - tb ))
  if [[ $mode == 1p ]]; then
    (( avail >= 8 )) || return 1
    print -r -- "crop=0:$tb:$w:$(( avail & ~1 ))"
  else
    half=$(( (avail / 2) & ~1 ))
    (( half >= 8 )) || return 1
    print -r -- "crop=0:$tb:$w:$half"
    print -r -- "crop=0:$(( tb + half )):$w:$half"
  fi
  return 0
}

# fifo_send <fifo> <line> [timeout_s]: writes one line to a FIFO. Opening a FIFO
# for write blocks until Dolphin has it open for read, so the write runs in a
# child that we abandon after the timeout instead of hanging the script.
fifo_send() {
  local fifo=$1 line=$2 secs=${3:-5} wp i
  [[ -p $fifo ]] || die "no FIFO at $fifo (provision the instance first)"
  ( print -r -- "$line" > "$fifo" ) &
  wp=$!
  for (( i = 0; i < secs * 10; i++ )); do
    if ! kill -0 "$wp" 2>/dev/null; then
      wait "$wp" && return 0
      return 1
    fi
    sleep 0.1
  done
  kill "$wp" 2>/dev/null || true
  print -u2 -- "timeout: nobody is reading $fifo after ${secs}s (is Dolphin running?)"
  return 1
}

# winlist_run: prints Dolphin's windows (windowID pid title w h onScreen|offScreen, tab
# separated) with run/bin/winlist, built from run/src/winlist.swift when missing. This
# replaces `wincap list`, which has been seen to hang. Killed after 3 s if it stalls.
WINLIST=$RUN_DIR/bin/winlist
winlist_run() {
  if [[ ! -x $WINLIST ]]; then
    mkdir -p "$RUN_DIR/bin"
    swiftc -O "$RUN_DIR/src/winlist.swift" -o "$WINLIST" >&2 || return 1
  fi
  perl -e 'alarm 3; exec @ARGV' "$WINLIST"
}
