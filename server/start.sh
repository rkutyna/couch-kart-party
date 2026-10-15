#!/bin/bash
# Start the local WiiLink wfc-server stack. Idempotent.
#   ./start.sh          PostgreSQL 16 on 127.0.0.1:$PG_PORT (54329) + wwfc on 127.0.0.1 (NAS on 127.0.0.1:8080)
#   ./start.sh --nas80  also start nas80_proxy.py on 0.0.0.0:80 (loopback peers only). Opt-in.
# wwfc = frontend, which spawns its own "backend" child, both from server/wfc-server.
# PIDs go to server/run/, logs to server/logs/.

set -u

SERVER_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$SERVER_DIR")"
# Local settings of this Mac (mkparty.conf at the project root; PG_PORT, ...).
MKPARTY_CONF="${MKPARTY_CONF:-$ROOT/mkparty.conf}"
[ -f "$MKPARTY_CONF" ] && . "$MKPARTY_CONF"

WANT_NAS80=0
[ "${1:-}" = "--nas80" ] && WANT_NAS80=1

WFC_DIR="$SERVER_DIR/wfc-server"
PGDATA="$SERVER_DIR/pgdata"
LOG_DIR="$SERVER_DIR/logs"
RUN_DIR="$SERVER_DIR/run"
# PostgreSQL 16 from Homebrew (brew install postgresql@16)
PGBIN="${PGBIN:-$( (brew --prefix postgresql@16 || echo /opt/homebrew/opt/postgresql@16) 2>/dev/null)/bin}"
PG_PORT="${PG_PORT:-54329}"

FRONT_PID_FILE="$RUN_DIR/wwfc.pid"
BACK_PID_FILE="$RUN_DIR/wwfc-backend.pid"
PROXY_PID_FILE="$RUN_DIR/nas80-proxy.pid"
PG_OWNED_MARKER="$RUN_DIR/postgres.started-by-start-sh"

mkdir -p "$LOG_DIR" "$RUN_DIR"

# Postgres' postmaster refuses to start unless a valid UTF-8 locale is exported.
export LC_ALL=en_US.UTF-8
export LANG=en_US.UTF-8

pid_is_wwfc() {
    # True if $1 is a live process whose executable is named "wwfc"
    local pid="$1"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    [ "$(basename "$(ps -p "$pid" -o comm= 2>/dev/null | tr -d ' ')")" = "wwfc" ]
}

pid_is_proxy() {
    # True if $1 is a live process running nas80_proxy.py
    local pid="$1"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    ps -p "$pid" -o command= 2>/dev/null | grep -q "nas80_proxy.py"
}

# ---- 1. PostgreSQL ----------------------------------------------------------
if "$PGBIN/pg_ctl" -D "$PGDATA" status >/dev/null 2>&1; then
    echo "[pg] already running (not started by this script unless marker exists)"
else
    echo "[pg] starting PostgreSQL on 127.0.0.1:$PG_PORT"
    if "$PGBIN/pg_ctl" -D "$PGDATA" -l "$LOG_DIR/postgres.log" -w -t 60 start >/dev/null; then
        touch "$PG_OWNED_MARKER"
    else
        echo "[pg] FAILED to start; see $LOG_DIR/postgres.log" >&2
        exit 1
    fi
fi

if ! "$PGBIN/pg_isready" -h 127.0.0.1 -p "$PG_PORT" >/dev/null 2>&1; then
    echo "[pg] not accepting connections on 127.0.0.1:$PG_PORT" >&2
    exit 1
fi

# ---- 2. wwfc ----------------------------------------------------------------
if [ -f "$FRONT_PID_FILE" ] && pid_is_wwfc "$(cat "$FRONT_PID_FILE")"; then
    echo "[wwfc] already running (pid $(cat "$FRONT_PID_FILE"))"
else
    if [ ! -x "$WFC_DIR/wwfc" ]; then
        echo "[wwfc] binary missing: $WFC_DIR/wwfc (run ./mkparty setup)" >&2
        exit 1
    fi
    if [ ! -f "$WFC_DIR/config.xml" ]; then
        echo "[wwfc] config missing: $WFC_DIR/config.xml (run ./mkparty setup)" >&2
        exit 1
    fi

    # wwfc resolves config.xml, payload/, dlc/, game_list.tsv and logs/ relative to its cwd
    cd "$WFC_DIR" || exit 1
    nohup ./wwfc >> "$LOG_DIR/wwfc-stdout.log" 2>&1 < /dev/null &
    FRONT_PID=$!
    echo "$FRONT_PID" > "$FRONT_PID_FILE"
    echo "[wwfc] started frontend pid $FRONT_PID"

    # The frontend spawns "wwfc backend --noreload --nosignal" as a child process
    BACK_PID=""
    for _ in $(seq 1 50); do
        BACK_PID="$(pgrep -P "$FRONT_PID" 2>/dev/null | head -n1)"
        [ -n "$BACK_PID" ] && break
        sleep 0.2
    done
    if [ -n "$BACK_PID" ]; then
        echo "$BACK_PID" > "$BACK_PID_FILE"
        echo "[wwfc] backend child pid $BACK_PID"
    else
        echo "[wwfc] warning: backend child not found yet (check $LOG_DIR/wwfc-stdout.log)"
    fi

    # Wait for the NAS HTTP listener (8080) and the GPCM listener to come up
    ready=0
    for _ in $(seq 1 100); do
        if lsof -nP -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1 && lsof -nP -iTCP:29900 -sTCP:LISTEN >/dev/null 2>&1; then
            ready=1
            break
        fi
        if ! pid_is_wwfc "$FRONT_PID"; then
            break
        fi
        sleep 0.3
    done
    if [ "$ready" = 1 ]; then
        echo "[wwfc] listening (NAS 127.0.0.1:8080, GPCM 127.0.0.1:29900)"
    else
        echo "[wwfc] not fully up; check $LOG_DIR/wwfc-stdout.log" >&2
        exit 1
    fi
fi

# ---- 3. port 80 front for the NAS (OPT-IN: ./start.sh --nas80) ---------------
# Binds the wildcard 0.0.0.0:80 (the only :80 bind a non-root user gets here), so it is
# off by default to keep everything on 127.0.0.1.
if [ "$WANT_NAS80" != 1 ]; then
    echo "[nas80] not started (opt-in with --nas80; NAS stays on 127.0.0.1:8080)"
elif [ -f "$PROXY_PID_FILE" ] && pid_is_proxy "$(cat "$PROXY_PID_FILE")"; then
    echo "[nas80] already running (pid $(cat "$PROXY_PID_FILE"))"
else
    nohup python3 "$SERVER_DIR/nas80_proxy.py" >> "$LOG_DIR/nas80-proxy.log" 2>&1 < /dev/null &
    PROXY_PID=$!
    echo "$PROXY_PID" > "$PROXY_PID_FILE"
    ok=0
    for _ in $(seq 1 50); do
        if lsof -nP -iTCP:80 -sTCP:LISTEN >/dev/null 2>&1; then ok=1; break; fi
        kill -0 "$PROXY_PID" 2>/dev/null || break
        sleep 0.2
    done
    if [ "$ok" = 1 ]; then
        echo "[nas80] forwarder pid $PROXY_PID listening on 0.0.0.0:80 -> 127.0.0.1:8080 (loopback peers only)"
    else
        echo "[nas80] forwarder failed to bind :80; see $LOG_DIR/nas80-proxy.log" >&2
        exit 1
    fi
fi

# Warn (do not fail) if the client payload has not been copied in yet
for f in stage1.bin stage1v1.bin private-key.pem; do
    [ -f "$WFC_DIR/payload/$f" ] || echo "[wwfc] note: $WFC_DIR/payload/$f not present yet (needed for patched clients)"
done

# ---- 4. name resolution ---------------------------------------------------------
# The no-DNS patch build (patcher/out-local) reaches us by the IP literal 127.0.0.1
# only, so there is no resolver helper to start; just verify the pieces agree.
echo "[names] no resolver helper needed (clients use the IP literal 127.0.0.1); checking:"
"$SERVER_DIR/netcheck.sh" || echo "[names] WARNING: name-resolution check failed (see above)" >&2

echo "[done] PIDs: frontend=$(cat "$FRONT_PID_FILE" 2>/dev/null) backend=$(cat "$BACK_PID_FILE" 2>/dev/null) nas80-proxy=$(cat "$PROXY_PID_FILE" 2>/dev/null)"
