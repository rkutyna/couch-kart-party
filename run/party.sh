#!/bin/zsh
# usage: party.sh start [N]
#        party.sh stop
#   start: writes run/slots.json with 12 player slots (N is accepted and ignored: the
#          navigator assigns the slots that race to instances at Start) and the stream
#          sources of every instance (instance k pad p -> udp SRC_BASE + 4(k-1) + 2(p-1)),
#          then starts party/partyd with plain HTTP on :8000 and HTTPS on :8443 (needed
#          for tilt steering). Slots start unassigned (no FIFO, no video source); the
#          navigator gives each raced slot its pipe and source (POST /api/nav/assign).
#          The HTTPS certificate is party/certs/fullchain.pem + privkey.pem when present,
#          else the self-signed pair, which make-cert.sh creates on first start. Phone
#          stats go to run/logs. Pid in run/pids/partyd.pid, log in run/logs/partyd.log.
#   stop:  stops only the partyd recorded in run/pids/partyd.pid.
set -euo pipefail
SCRIPT_DIR=${0:A:h}
source "$SCRIPT_DIR/lib.sh"

usage() { print -u2 -- "usage: party.sh start | party.sh stop"; exit 2; }

# write_slots: prints slots.json: 12 unassigned slots and the 24 instance sources.
write_slots() {
  local slot k p comma
  print -r -- '{'
  print -r -- '  "slots": ['
  for (( slot = 1; slot <= 12; slot++ )); do
    comma=","; (( slot == 12 )) && comma=""
    print -r -- "    {\"slot\": $slot, \"rtpPort\": $(( RTP_BASE + 2 * (slot - 1) )), \"pipe\": \"\", \"label\": \"P$slot\"}$comma"
  done
  print -r -- '  ],'
  print -r -- '  "sources": ['
  for (( k = 1; k <= MAX_INST; k++ )); do
    for p in 1 2; do
      comma=","; (( k == MAX_INST && p == 2 )) && comma=""
      print -r -- "    {\"instance\": $k, \"pad\": $p, \"port\": $(src_port $k $p)}$comma"
    done
  done
  print -r -- '  ]'
  print -r -- '}'
}

cmd_start() {
  local pid tmp
  [[ -x $PARTYD ]] || die "partyd not built at $PARTYD"
  mkdir -p "$LOG_DIR" "$PID_DIR"
  pid=$(read_pid partyd)
  if pid_alive "$pid"; then
    die "partyd already running (pid $pid); run party.sh stop first"
  fi
  if lsof -nP -iTCP:"$PARTY_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    die "port $PARTY_PORT is already in use; not starting partyd"
  fi

  tmp=$SLOTS_JSON.tmp
  write_slots > "$tmp"
  python3 -I -c 'import json,sys; json.load(open(sys.argv[1]))' "$tmp" \
    || die "generated slots JSON is invalid"
  mv -f "$tmp" "$SLOTS_JSON"
  print -r -- "wrote $SLOTS_JSON (12 slots, unassigned)"

  # HTTPS for tilt steering: a real certificate wins over the self-signed one.
  local -a https_args=()
  local cert key
  if [[ -f $CERT_DIR/fullchain.pem && -f $CERT_DIR/privkey.pem ]]; then
    cert=$CERT_DIR/fullchain.pem; key=$CERT_DIR/privkey.pem
  else
    if [[ ! -f $CERT_DIR/selfsigned-cert.pem || ! -f $CERT_DIR/selfsigned-key.pem ]]; then
      print -r -- "creating the self-signed HTTPS certificate (first start)"
      "$MAKE_CERT" || warn "could not create the certificate; starting without HTTPS"
    fi
    cert=$CERT_DIR/selfsigned-cert.pem; key=$CERT_DIR/selfsigned-key.pem
  fi
  if [[ -f $cert && -f $key ]]; then
    if lsof -nP -iTCP:"$HTTPS_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
      warn "port $HTTPS_PORT is already in use; starting without HTTPS"
    else
      https_args=(-https ":$HTTPS_PORT" -tls-cert "$cert" -tls-key "$key")
    fi
  fi

  nohup "$PARTYD" -config "$SLOTS_JSON" -http ":$PARTY_PORT" "${https_args[@]}" \
    -statsdir "$STATS_DIR" \
    > "$LOG_DIR/partyd.log" 2>&1 < /dev/null &
  pid=$!
  print -r -- "$pid" > "$(pid_file partyd)"
  sleep 1.5
  if ! pid_alive "$pid"; then
    rm -f "$(pid_file partyd)"
    die "partyd exited; see $LOG_DIR/partyd.log"
  fi
  print -r -- "partyd pid $pid on :$PARTY_PORT (log $LOG_DIR/partyd.log)"
  print -r -- "phone URL: $PHONE_URL"
  if (( ${#https_args[@]} )); then
    print -r -- "secure URL (tilt steering): $SECURE_URL  (self-signed certificate: accept the warning once)"
  fi
  print -r -- "phone stats: $STATS_DIR/phonestats-*.jsonl"
}

cmd_stop() {
  local pid f=$(pid_file partyd)
  pid=$(read_pid partyd)
  if pid_alive "$pid" && proc_is "$pid" "partyd"; then
    kill_tree "$pid" TERM
    if wait_exit "$pid" 5; then
      print -r -- "stopped partyd (pid $pid)"
    else
      warn "partyd (pid $pid) still running after 5 s"
      exit 1
    fi
  else
    print -r -- "partyd: not running"
  fi
  rm -f "$f"
}

[[ $# -ge 1 ]] || usage
case $1 in
  start) [[ $# -le 2 ]] || usage; cmd_start ;;
  stop)  [[ $# -le 2 ]] || usage; cmd_stop ;;
  *) usage ;;
esac
