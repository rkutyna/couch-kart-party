#!/bin/zsh
# usage: stream_inst.sh <instance k>
#   Keeps the capture of ONE Dolphin instance alive (one window capture per instance):
#     - the full window frame for the navigator: run/frames/i<k>.bmp, NAV_FPS times a second
#     - the phone video as H.264 RTP to partyd's per-instance source ports:
#         single-player (i1..i6): the 16:9 picture          -> udp/src_port(k,1)
#         two-player (i7..i12):  top half (player A, pad p1) -> udp/src_port(k,1)
#                                bottom half (player B, p2)  -> udp/src_port(k,2)
#       partyd forwards a source to whichever phone slot the navigator assigned to that
#       (instance, pad) (POST /api/nav/assign), so the stream never has to be restarted
#       when the players change.
#   Runs until it is killed (streams.sh stop). Whenever the pipeline ends (Dolphin quit,
#   crashed or was relaunched, the window id changed) it waits and starts again with the
#   pid that is in run/pids/i<k>.pid at that moment, so it follows a relaunch.
#
#   The window id comes from run/bin/winlist (owner pid + title contains RMCE01).
#   `wincap --pid` / `--print-size-only` are never used (they hang / report 1x sizes): the
#   frame size is forced to 2x the window's size in points (1280x1016 normally). Each
#   instance uses its own copy of the wincap binary (run/bin/wincap-i<k>), because two
#   processes of one binary cannot capture at the same time.
#
#   Environment (defaults in brackets):
#     STREAM_BITRATE  H.264 bitrate of a 16:9 stream       [1500k]
#     STREAM_BITRATE2 H.264 bitrate of a half (2p) stream  [1100k]
#     STREAM_FPS      capture / encode frame rate          [60]
#     NAV_FPS         navigator frames per second           [12]
#     STREAM_MATRIX   YCbCr matrix of the capture           [bt709]
#     FFMPEG          ffmpeg binary                         [ffmpeg on PATH, else <brew --prefix>/bin/ffmpeg]
set -uo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"
BITRATE=${STREAM_BITRATE:-1500k}
BITRATE2=${STREAM_BITRATE2:-1100k}
FPS=${STREAM_FPS:-60}
NAV_FPS=${NAV_FPS:-12}
MATRIX=${STREAM_MATRIX:-bt709}
FRAME_DIR=$SCRIPT_DIR/frames
mkdir -p "$FRAME_DIR"

[[ $# -eq 1 && $1 == <1-12> ]] || { print -u2 -- "usage: stream_inst.sh <instance 1..12>"; exit 2; }
k=$1
mode=$(inst_mode "$k")
p1=$(src_port "$k" 1); p2=$(src_port "$k" 2)
[[ -x $FFMPEG ]] || { print -u2 -- "ffmpeg not found at $FFMPEG"; exit 1; }

MYCAP=$SCRIPT_DIR/bin/wincap-i$k
mkdir -p "$SCRIPT_DIR/bin"
if [[ ! -x $MYCAP || $WINCAP -nt $MYCAP ]]; then
  cp "$WINCAP" "$MYCAP" && chmod +x "$MYCAP"
fi

note() { print -u2 -- "$(date '+%H:%M:%S') stream_inst[i$k $mode]: $*"; }

# enc <bitrate> <port>: the H.264 RTP output options for one stream.
enc() {
  print -r -- "-c:v h264_videotoolbox -realtime 1 -profile:v baseline -bf 0 -g 30 -b:v $1 -color_range tv -f rtp -payload_type 96 rtp://127.0.0.1:$2?pkt_size=1200"
}

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
  # Geometry in the 1280x1016 window frame (56 px title bar, then the 1280x960 render
  # area); scaled when the window has another size.
  sx() { print -r -- $(( ($1 * W / 1280) & ~1 )); }
  sy() { print -r -- $(( ($1 * H / 1016) & ~1 )); }
  nav="[n]"
  if [[ $mode == 1p ]]; then
    # the 16:9 picture: frame rows 186..886
    graph="[0:v]split=2[a][b];[a]crop=$(sx 1280):$(sy 700):0:$(sy 186),scale=768:420[v1];[b]fps=${NAV_FPS},scale=in_color_matrix=${MATRIX}:in_range=tv:out_range=pc,format=bgr24[n]"
    outs=( -map "[v1]" ${=$(enc "$BITRATE" "$p1")} )
    dests="udp/$p1"
  else
    # 4:3 split screen: player A = frame rows 56..536, player B = rows 536..1016
    graph="[0:v]split=3[a][c][b];[a]crop=$(sx 1280):$(sy 480):0:$(sy 56),scale=768:288[v1];[c]crop=$(sx 1280):$(sy 480):0:$(sy 536),scale=768:288[v2];[b]fps=${NAV_FPS},scale=in_color_matrix=${MATRIX}:in_range=tv:out_range=pc,format=bgr24[n]"
    outs=( -map "[v1]" ${=$(enc "$BITRATE2" "$p1")} -map "[v2]" ${=$(enc "$BITRATE2" "$p2")} )
    dests="udp/$p1 (A) udp/$p2 (B)"
  fi
  note "pid $pid window $wid frame ${W}x${H} -> $dests, frames $FRAME_DIR/i$k.bmp"
  last=run
  "$MYCAP" capture --window "$wid" --fps "$FPS" --width "$W" --height "$H" \
  | "$FFMPEG" -hide_banner -loglevel warning \
      -fflags nobuffer -flags low_delay \
      -f rawvideo -pix_fmt nv12 -video_size "${W}x${H}" -framerate "$FPS" \
      -color_range tv -colorspace "$MATRIX" -i - \
      -filter_complex "$graph" \
      "${outs[@]}" \
      -map "$nav" -c:v bmp -f image2 -update 1 -atomic_writing 1 -y "$FRAME_DIR/i$k.bmp"
  note "pipeline ended (status ${pipestatus[*]}); restarting in 1 s"
  sleep 1
done
