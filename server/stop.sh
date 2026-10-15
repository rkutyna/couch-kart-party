#!/bin/bash
# Stop only what start.sh started: the wwfc frontend + backend child (via PID files),
# and PostgreSQL if start.sh was the one that started it.
# Idempotent: safe to run when nothing is running.
#   ./stop.sh              stop wwfc, and PostgreSQL if start.sh started it
#   ./stop.sh --keep-db    stop wwfc only

set -u

SERVER_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$SERVER_DIR")"
# Local settings of this Mac (mkparty.conf at the project root; PG_PORT, ...).
MKPARTY_CONF="${MKPARTY_CONF:-$ROOT/mkparty.conf}"
[ -f "$MKPARTY_CONF" ] && . "$MKPARTY_CONF"
LOG_DIR="$SERVER_DIR/logs"
RUN_DIR="$SERVER_DIR/run"
PGDATA="$SERVER_DIR/pgdata"
# PostgreSQL 16 from Homebrew (brew install postgresql@16)
PGBIN="${PGBIN:-$( (brew --prefix postgresql@16 || echo /opt/homebrew/opt/postgresql@16) 2>/dev/null)/bin}"
PG_PORT="${PG_PORT:-54329}"

FRONT_PID_FILE="$RUN_DIR/wwfc.pid"
BACK_PID_FILE="$RUN_DIR/wwfc-backend.pid"
PROXY_PID_FILE="$RUN_DIR/nas80-proxy.pid"
PG_OWNED_MARKER="$RUN_DIR/postgres.started-by-start-sh"

KEEP_DB=0
[ "${1:-}" = "--keep-db" ] && KEEP_DB=1

mkdir -p "$LOG_DIR" "$RUN_DIR"

pid_is_wwfc() {
    local pid="$1"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    [ "$(basename "$(ps -p "$pid" -o comm= 2>/dev/null | tr -d ' ')")" = "wwfc" ]
}

pid_is_proxy() {
    local pid="$1"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    ps -p "$pid" -o command= 2>/dev/null | grep -q "nas80_proxy.py"
}

wait_gone() {
    # wait_gone PID SECONDS -> 0 if the process exited in time
    local pid="$1" secs="$2"
    for _ in $(seq 1 $((secs * 5))); do
        kill -0 "$pid" 2>/dev/null || return 0
        sleep 0.2
    done
    ! kill -0 "$pid" 2>/dev/null
}

# ---- 0. port 80 forwarder (independent of wwfc) ------------------------------
if [ -f "$PROXY_PID_FILE" ]; then
    PROXY_PID="$(cat "$PROXY_PID_FILE")"
    if pid_is_proxy "$PROXY_PID"; then
        echo "[nas80] SIGTERM forwarder $PROXY_PID"
        kill -TERM "$PROXY_PID"
        wait_gone "$PROXY_PID" 5 || kill -KILL "$PROXY_PID" 2>/dev/null
    else
        echo "[nas80] forwarder pid file stale"
    fi
    rm -f "$PROXY_PID_FILE"
else
    echo "[nas80] no forwarder pid file"
fi

# ---- 1. wwfc frontend (SIGTERM makes it tell the backend to exit over RPC) ----
if [ -f "$FRONT_PID_FILE" ]; then
    FRONT_PID="$(cat "$FRONT_PID_FILE")"
    if pid_is_wwfc "$FRONT_PID"; then
        echo "[wwfc] SIGTERM frontend $FRONT_PID"
        kill -TERM "$FRONT_PID"
        if ! wait_gone "$FRONT_PID" 20; then
            echo "[wwfc] frontend still alive after 20s; SIGKILL" >&2
            kill -KILL "$FRONT_PID" 2>/dev/null
        fi
    else
        echo "[wwfc] frontend pid file stale (pid $FRONT_PID not a live wwfc)"
    fi
    rm -f "$FRONT_PID_FILE"
else
    echo "[wwfc] no frontend pid file"
fi

# ---- 2. wwfc backend child (only if it outlived the frontend) ----------------
if [ -f "$BACK_PID_FILE" ]; then
    BACK_PID="$(cat "$BACK_PID_FILE")"
    if pid_is_wwfc "$BACK_PID"; then
        echo "[wwfc] backend $BACK_PID still alive; SIGTERM"
        kill -TERM "$BACK_PID"
        if ! wait_gone "$BACK_PID" 10; then
            echo "[wwfc] backend still alive; SIGKILL" >&2
            kill -KILL "$BACK_PID" 2>/dev/null
        fi
    fi
    rm -f "$BACK_PID_FILE"
fi

# ---- 3. PostgreSQL (only if start.sh started it) -----------------------------
if [ "$KEEP_DB" = 1 ]; then
    echo "[pg] --keep-db: leaving PostgreSQL running"
elif [ -f "$PG_OWNED_MARKER" ]; then
    if "$PGBIN/pg_ctl" -D "$PGDATA" status >/dev/null 2>&1; then
        echo "[pg] stopping PostgreSQL (started by start.sh)"
        "$PGBIN/pg_ctl" -D "$PGDATA" -m fast -w -t 60 stop >/dev/null || echo "[pg] stop failed" >&2
    else
        echo "[pg] not running"
    fi
    rm -f "$PG_OWNED_MARKER"
else
    echo "[pg] not started by start.sh; leaving it alone"
fi

echo "[names] no resolver helper to stop (clients use the IP literal 127.0.0.1)"
echo "[done]"
