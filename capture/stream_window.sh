#!/bin/bash
# Stream ONE window as low-latency H.264 RTP to 127.0.0.1:<rtp_port>.
#
#   stream_window.sh <windowid|pid:PID> <rtp_port> [crop=x:y:w:h] [bitrate=2500k]
#
#   windowid   numeric CGWindowID from `wincap list`
#   pid:PID    owner PID; the largest on-screen window of that process is used
#              (the resolved window id is printed and then used for the stream)
#   crop=      rectangle in captured-frame PIXELS (x:y:w:h, all even), applied by ffmpeg
#   bitrate=   H.264 target bitrate, default 2500k
#
# Writes the SDP description to capture/out/<rtp_port>.sdp (written by ffmpeg
# when the RTP stream starts). Stop with Ctrl-C: wincap and ffmpeg both exit.
# If ffmpeg exits first, wincap sees EPIPE and exits; if wincap exits (window
# gone), ffmpeg sees EOF and exits.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WINCAP="${WINCAP:-$SCRIPT_DIR/bin/wincap}"
FFMPEG="${FFMPEG:-$(command -v ffmpeg || echo "$(brew --prefix 2>/dev/null || echo /opt/homebrew)/bin/ffmpeg")}"
FPS="${FPS:-60}"
OUT_DIR="$SCRIPT_DIR/out"

usage() {
  echo "usage: $0 <windowid|pid:PID> <rtp_port> [crop=x:y:w:h] [bitrate=2500k]" >&2
  exit 2
}

[[ $# -ge 2 ]] || usage
TARGET="$1"
PORT="$2"
shift 2

CROP=""
BITRATE="2500k"
for arg in "$@"; do
  case "$arg" in
    crop=*)    CROP="${arg#crop=}" ;;
    bitrate=*) BITRATE="${arg#bitrate=}" ;;
    *) echo "unknown option: $arg" >&2; usage ;;
  esac
done

if ! [[ "$PORT" =~ ^[0-9]+$ ]] || (( PORT < 1024 || PORT > 65535 )); then
  echo "rtp_port must be an integer 1024..65535" >&2
  exit 2
fi
if ! [[ "$BITRATE" =~ ^[0-9]+[kKmM]?$ ]]; then
  echo "bitrate must look like 2500k or 4M" >&2
  exit 2
fi

case "$TARGET" in
  pid:*)
    PID="${TARGET#pid:}"
    [[ "$PID" =~ ^[0-9]+$ ]] || usage
    SEL=(--pid "$PID")
    ;;
  *)
    [[ "$TARGET" =~ ^[0-9]+$ ]] || usage
    SEL=(--window "$TARGET")
    ;;
esac

if [[ ! -x "$WINCAP" ]]; then
  echo "wincap not built at $WINCAP (see build command in the capture report)" >&2
  exit 1
fi
if [[ ! -x "$FFMPEG" ]]; then
  echo "ffmpeg not found at $FFMPEG" >&2
  exit 1
fi

# 1. Ask wincap for the frame size and the resolved window id. No frames are
#    produced by this call; it exits right away.
QUERY="$("$WINCAP" capture "${SEL[@]}" --fps "$FPS" --print-size-only)"
# Format: SIZE <w>x<h> FPS <fps> WINDOW <id>
read -r _ SIZE _ _ _ WID <<< "$QUERY"
if [[ -z "${WID:-}" || -z "${SIZE:-}" ]]; then
  echo "could not parse wincap output: $QUERY" >&2
  exit 1
fi
W="${SIZE%x*}"
H="${SIZE#*x}"

# 2. Optional crop, applied by ffmpeg on the raw frames (pixel units).
#    Our crop syntax is x:y:w:h; ffmpeg's crop filter wants w:h:x:y.
VF=()
if [[ -n "$CROP" ]]; then
  IFS=: read -r CX CY CW CH <<< "$CROP"
  for v in "$CX" "$CY" "$CW" "$CH"; do
    [[ "$v" =~ ^[0-9]+$ ]] || { echo "crop must be x:y:w:h integers" >&2; exit 2; }
    (( v % 2 == 0 )) || { echo "crop values must be even (NV12 chroma is 2x2): $CROP" >&2; exit 2; }
  done
  (( CW > 0 && CH > 0 && CX + CW <= W && CY + CH <= H )) || {
    echo "crop $CROP is outside the ${W}x${H} frame" >&2; exit 2; }
  VF=(-vf "crop=${CW}:${CH}:${CX}:${CY}")
  OUT_W="$CW"; OUT_H="$CH"
else
  OUT_W="$W"; OUT_H="$H"
fi

mkdir -p "$OUT_DIR"
SDP="$OUT_DIR/${PORT}.sdp"

echo "stream_window: window=$WID source=${W}x${H} output=${OUT_W}x${OUT_H} fps=$FPS bitrate=$BITRATE -> rtp://127.0.0.1:${PORT} (sdp: $SDP)" >&2

# 3. Pipeline. wincap writes raw NV12 on stdout; ffmpeg reads it from stdin.
#    ${VF[@]+...} keeps the empty-array case safe under `set -u` on bash 3.2.
"$WINCAP" capture --window "$WID" --fps "$FPS" \
| "$FFMPEG" -hide_banner -loglevel warning \
    -fflags nobuffer -flags low_delay \
    -f rawvideo -pix_fmt nv12 -video_size "${W}x${H}" -framerate "$FPS" -i - \
    ${VF[@]+"${VF[@]}"} \
    -color_range tv -c:v h264_videotoolbox -realtime 1 -profile:v baseline -bf 0 -g 30 -b:v "$BITRATE" \
    -f rtp -payload_type 96 -sdp_file "$SDP" \
    "rtp://127.0.0.1:${PORT}?pkt_size=1200"
