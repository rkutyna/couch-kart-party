"""Client for partyd's navigator endpoints (loopback only).

  GET  /api/nav/lobby?since=N   version, phase, message, players (long poll ~25 s)
  POST /api/nav/update          {phase?, message?, slots:[{slot, status?, showVideo?, navControl?}]}
  POST /api/nav/reset           back to the lobby
"""
from __future__ import annotations

import json
import urllib.error
import urllib.request

import localconf

BASE = f"http://127.0.0.1:{localconf.get('PARTY_PORT', '8000')}"


class LobbyError(RuntimeError):
    pass


def _req(method: str, path: str, body=None, timeout: float = 40.0):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try:
            msg = json.loads(e.read().decode()).get("error", "")
        except Exception:
            msg = ""
        raise LobbyError(f"{method} {path}: HTTP {e.code} {msg}") from None
    except (urllib.error.URLError, OSError) as e:
        raise LobbyError(f"{method} {path}: {e}") from None


def get_lobby(since: int | None = None, timeout: float = 40.0) -> dict:
    q = f"?since={since}" if since is not None else ""
    return _req("GET", "/api/nav/lobby" + q, timeout=timeout)


def post_update(phase=None, message=None, slots=None) -> dict:
    body = {}
    if phase is not None:
        body["phase"] = phase
    if message is not None:
        body["message"] = message
    if slots:
        body["slots"] = slots
    return _req("POST", "/api/nav/update", body)


def post_reset(message=None) -> dict:
    body = {} if message is None else {"message": message}
    return _req("POST", "/api/nav/reset", body)


def healthy() -> bool:
    try:
        _req("GET", "/api/state", timeout=3)
        return True
    except LobbyError:
        return False


def post_assign(slots) -> dict:
    """POST /api/nav/assign: [{slot, pipe, instance, pad, mode}] (pipe "" = unassigned)."""
    return _req("POST", "/api/nav/assign", {"slots": slots})
