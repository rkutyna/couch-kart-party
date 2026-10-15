#!/usr/bin/env python3
"""ini_set.py <file> <Section>.<Key>=<value> ...

Sets keys in a Dolphin .ini file in place, keeping every other line as it is
(Dolphin's own keys, comments, order). A missing section is appended; a missing key
is added at the end of its section. Used by run/provision.sh and run/launch.sh for
the few keys the party needs (audio, hotkeys without focus, no OSD messages)."""
import os
import sys


def main(argv):
    if len(argv) < 3:
        print(__doc__, file=sys.stderr)
        return 2
    path = argv[1]
    want = []
    for a in argv[2:]:
        k, _, v = a.partition("=")
        sec, _, key = k.partition(".")
        if not sec or not key or not _:
            print(f"bad assignment {a!r}", file=sys.stderr)
            return 2
        want.append((sec, key, v))
    try:
        lines = open(path).read().splitlines()
    except FileNotFoundError:
        lines = []
    for sec, key, val in want:
        start = None
        for i, l in enumerate(lines):
            if l.strip() == f"[{sec}]":
                start = i
                break
        if start is None:
            lines += [f"[{sec}]", f"{key} = {val}"]
            continue
        end = len(lines)
        for j in range(start + 1, len(lines)):
            if lines[j].strip().startswith("["):
                end = j
                break
        done = False
        for j in range(start + 1, end):
            if lines[j].split("=", 1)[0].strip() == key:
                lines[j] = f"{key} = {val}"
                done = True
                break
        if not done:
            lines.insert(end, f"{key} = {val}")
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write("\n".join(lines) + "\n")
    os.replace(tmp, path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
