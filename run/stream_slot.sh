#!/bin/zsh
# usage: stream_slot.sh <instance k> <slot> <rtp_port>
#   Keeps ONE phone stream alive: the game window of instance k as H.264 RTP on
#   127.0.0.1:<rtp_port> (partyd listens there). Runs until it is killed
#   (streams.sh stop). Whenever the pipeline ends (Dolphin quit, crashed or was
#   relaunched, the window id changed) it waits and starts again with the pid that
#   is in run/pids/i<k>.pid at that moment, so the stream follows a relaunch.
#
#   The window id comes from run/bin/winlist (owner pid + title contains RMCE01);
#   `wincap list` is not used because it can hang.
#   `wincap --pid` and `--print-size-only` are never used (the first hangs, the
#   second has reported a 1x size for a 2x window): the frame size is forced with
#   --width/--height to 2x the window's size in points (1280x1016 normally).
#   Each slot uses its own copy of the wincap binary (run/bin/wincap<slot>),
#   because two processes of one binary cannot capture at the same time.
#
#   Environment (defaults in brackets):
#     STREAM_CROP    crop of the 1280x1016 capture, ffmpeg order w:h:x:y [1280:700:0:186]
#                    (= the 16:9 game picture; the same as crop=x:y:w:h 0:186:1280:700)
#     STREAM_SCALE   output size w:h                                    [768:420]
#     STREAM_BITRATE H.264 bitrate                                      [1500k]
#     STREAM_FPS     capture / encode frame rate                        [60]
#     NAV_FPS        navigator frames per second (run/frames/i<k>.bmp)     [12]
#     STREAM_MATRIX  YCbCr matrix of the capture (bt709 | bt601)         [bt709]
#     FFMPEG         ffmpeg binary                                      [ffmpeg on PATH, else <brew --prefix>/bin/ffmpeg]
set -uo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"
CROP=${STREAM_CROP:-1280:700:0:186}
SCALE=${STREAM_SCALE:-768:420}
BITRATE=${STREAM_BITRATE:-1500k}
FPS=${STREAM_FPS:-60}
NAV_FPS=${NAV_FPS:-12}
MATRIX=${STREAM_MATRIX:-bt709}
FRAME_DIR=$SCRIPT_DIR/frames
mkdir -p "$FRAME_DIR"

[[ $# -eq 3 ]] || { print -u2 -- "usage: stream_slot.sh <instance k> <slot> <rtp_port>"; exit 2; }
k=$1; slot=$2; port=$3
[[ $k == <-> && $slot == <-> && $port == <-> ]] || { print -u2 -- "bad arguments"; exit 2; }
[[ -x $FFMPEG ]] || { print -u2 -- "ffmpeg not found at $FFMPEG"; exit 1; }
IFS=: read -r CW CH CX CY <<< "$CROP"
[[ $CW == <-> && $CH == <-> && $CX == <-> && $CY == <-> ]] || { print -u2 -- "STREAM_CROP must be w:h:x:y, got $CROP"; exit 2; }
[[ $SCALE == <->:<-> ]] || { print -u2 -- "STREAM_SCALE must be w:h, got $SCALE"; exit 2; }

MYCAP=$SCRIPT_DIR/bin/wincap$slot
mkdir -p "$SCRIPT_DIR/bin"
if [[ ! -x $MYCAP || $WINCAP -nt $MYCAP ]]; then
  cp "$WINCAP" "$MYCAP" && chmod +x "$MYCAP"
fi

note() { print -u2 -- "$(date '+%H:%M:%S') stream_slot[s$slot i$k]: $*"; }

last=""
while true; do
  pid=$(read_pid "i$k")
  if ! pid_alive "$pid" || ! proc_is "$pid" "$DOLPHIN"; then
    [[ $last == nopid ]] || note "instance $k is not running; waiting"
    last=nopid; sleep 1; continue
  fi
  # winlist: windowID  pid  title  w  h  onScreen|offScreen   (w, h in points)
  line=$(winlist_run | awk -F'\t' -v p="$pid" '
    $2 == p && $3 ~ /RMCE01/ { a = $4 * $5 + ($6 == "onScreen" ? 1e12 : 0); if (a > best) { best = a; l = $1 " " $4 " " $5 } }
    END { if (l != "") print l }')
  if [[ -z $line ]]; then
    [[ $last == nowin ]] || note "no RMCE01 window for pid $pid yet; waiting"
    last=nowin; sleep 1; continue
  fi
  f=( ${=line} ); wid=$f[1]
  W=$(( f[2] * 2 )); H=$(( f[3] * 2 ))
  cw=$CW; ch=$CH; cx=$CX; cy=$CY
  if (( W != 1280 || H != 1016 )); then
    cw=$(( (CW * W / 1280) & ~1 )); ch=$(( (CH * H / 1016) & ~1 ))
    cx=$(( (CX * W / 1280) & ~1 )); cy=$(( (CY * H / 1016) & ~1 ))
  fi
  note "pid $pid window $wid frame ${W}x${H} crop ${cw}:${ch}:${cx}:${cy} scale $SCALE bitrate $BITRATE -> rtp://127.0.0.1:$port"
  last=run
  # Two outputs from the one capture: the phone stream (cropped, scaled, H.264 RTP) and,
  # for the navigator, the full frame as a BMP rewritten NAV_FPS times a second
  # (run/frames/i<k>.bmp, written to a temp file and renamed, so a reader never sees a
  # partial image). A second capture of the window (screencapture) costs 0.5-1.5 s per
  # frame when three instances are streaming; this costs next to nothing.
  "$MYCAP" capture --window "$wid" --fps "$FPS" --width "$W" --height "$H" \
  | "$FFMPEG" -hide_banner -loglevel warning \
      -fflags nobuffer -flags low_delay \
      -f rawvideo -pix_fmt nv12 -video_size "${W}x${H}" -framerate "$FPS" \
      -color_range tv -colorspace "$MATRIX" -i - \
      -filter_complex "[0:v]split=2[a][b];[a]crop=${cw}:${ch}:${cx}:${cy},scale=${SCALE%:*}:${SCALE#*:}[v];[b]fps=${NAV_FPS},scale=in_color_matrix=${MATRIX}:in_range=tv:out_range=pc,format=bgr24[n]" \
      -map "[v]" -color_range tv -c:v h264_videotoolbox -realtime 1 -profile:v baseline -bf 0 -g 30 -b:v "$BITRATE" \
      -f rtp -payload_type 96 "rtp://127.0.0.1:${port}?pkt_size=1200" \
      -map "[n]" -c:v bmp -f image2 -update 1 -atomic_writing 1 -y "$FRAME_DIR/i$k.bmp"
  note "pipeline ended (status ${pipestatus[*]}); restarting in 1 s"
  sleep 1
done
