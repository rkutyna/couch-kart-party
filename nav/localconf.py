"""Local settings of this Mac, shared with the shell scripts (run/lib.sh).

They live in mkparty.conf at the project root (not in git; ./mkparty setup creates it from
mkparty.conf.example). The file is plain shell assignments, one KEY="value" per line; run/lib.sh
sources it, and this module reads the same lines (no shell expansion besides ~ and $VARS).
An environment variable of the same name wins over the file.
"""
from __future__ import annotations

import os
import re

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONF_FILE = os.environ.get("MKPARTY_CONF") or os.path.join(ROOT, "mkparty.conf")
_LINE = re.compile(r"""^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(?:"([^"]*)"|'([^']*)'|([^\s#]*))""")
_values: dict | None = None


def _load() -> dict:
    global _values
    if _values is None:
        _values = {}
        try:
            with open(CONF_FILE) as f:
                for line in f:
                    m = _LINE.match(line)
                    if m:
                        v = next(g for g in m.groups()[1:] if g is not None)
                        _values[m.group(1)] = os.path.expanduser(os.path.expandvars(v))
        except OSError:
            pass
    return _values


def get(key: str, default: str = "") -> str:
    v = os.environ.get(key)
    if v:
        return v
    return _load().get(key) or default


def dolphin_binary() -> str:
    app = get("DOLPHIN_APP", "/Applications/Dolphin.app").rstrip("/")
    return os.path.join(app, "Contents", "MacOS", "Dolphin")
