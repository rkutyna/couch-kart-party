#!/bin/bash
# Name-resolution health check for the no-DNS WFC patch build. Read-only.
#   ./netcheck.sh         prints one line per check; exit 0 = healthy, 1 = a check failed
#
# The game reaches this server by the IP literal 127.0.0.1 only (stage 0 Gecko code,
# stage 1 and the payload's resolver hooks all carry the literal), so there is no
# resolver helper process to run. What can go wrong instead is a mismatch between the
# Gecko code in a Dolphin profile and the payload the server serves. This checks:
#   1. patcher/out-local/RMCE01.gecko.txt carries IP-literal URLs and no host name
#   2. the stage 1 the server serves has the MD5 pinned in that Gecko code
#   3. every provisioned instance carries exactly that Gecko code
#   4. the system resolver answers the literal at once (numeric, no DNS query)
#   5. if the NAS is listening: GET /w1 with an IP Host header returns that stage 1
# Called by start.sh (after start) and run/status.sh.

SERVER_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$SERVER_DIR")"

exec python3 - "$ROOT" <<'PY'
import glob, hashlib, os, re, socket, sys, time, urllib.request

root = sys.argv[1]
gecko = os.path.join(root, "patcher", "out-local", "RMCE01.gecko.txt")
payload_dir = os.path.join(root, "server", "wfc-server", "payload")
IP = "127.0.0.1"
TITLE = "$WFC local-ip 8080"
bad = 0


def line(ok, text):
    global bad
    if not ok:
        bad += 1
    print(f"  [{'ok' if ok else 'FAIL'}] {text}")


# 1. Gecko code content
pin = None
hexlines = []
try:
    with open(gecko) as f:
        lines = f.read().split("\n")
    hexlines = [l for l in lines[1:] if l.strip()]
    raw = bytes.fromhex("".join(hexlines).replace(" ", ""))
    url = b"://" + IP.encode() + b":8080/w1\0"
    names = [w for w in (b"localtest", b"nintendowifi", b"gamespy", b"nas.") if w in raw]
    m = re.search(rb"\|([0-9a-f]{32})", raw)
    if m:
        pin = m.group(1).decode()
    line(lines[0] == TITLE and url in raw and (IP.encode() + b"\0") in raw and not names,
         f"Gecko code {os.path.relpath(gecko, root)}: stage 0 URL http{url[:-1].decode()}, "
         f"availability host {IP}, host names in code: {names or 'none'}")
except (OSError, ValueError) as e:
    line(False, f"Gecko code {gecko}: {e}")

# 2. stage 1 on disk matches the pin
try:
    with open(os.path.join(payload_dir, "stage1v1.bin"), "rb") as f:
        s1 = f.read()
    md5 = hashlib.md5(b"\x01\x2c" + s1).hexdigest()
    line(pin is not None and md5 == pin,
         f"server payload/stage1v1.bin matches the Gecko MD5 pin ({md5} vs pin {pin}); "
         f"stage 1 payload URL: {'http://' + IP + ':8080/payload' if (b'http://' + IP.encode() + b':8080/payload') in s1 else 'NOT the IP literal'}")
    if (b"http://" + IP.encode() + b":8080/payload") not in s1:
        bad += 1
except OSError as e:
    line(False, f"server payload: {e}")

# 3. instances
inis = sorted(glob.glob(os.path.join(root, "instances", "i*", "GameSettings", "RMCE01.ini")))
for ini in inis:
    name = ini.split(os.sep)[-3]
    try:
        with open(ini) as f:
            text = f.read().split("\n")
    except OSError as e:
        line(False, f"instance {name}: {e}")
        continue
    body = [l for l in text if l.strip() and l[0] not in "[$"]
    enabled = "[Gecko_Enabled]" in text and TITLE in text[text.index("[Gecko_Enabled]"):]
    line(body == hexlines and enabled, f"instance {name}: carries the no-DNS Gecko code, enabled"
         if body == hexlines and enabled else
         f"instance {name}: Gecko code differs from out-local (run/provision.sh {name[1:]} 1p --force-config)")
if not inis:
    print("  [--] no provisioned instances")

# 4. resolver
t = time.time()
try:
    a = socket.getaddrinfo(IP, 8080, socket.AF_INET, socket.SOCK_STREAM)[0][4][0]
    g = socket.gethostbyname(IP)
except OSError as e:
    a = g = f"error {e}"
ms = (time.time() - t) * 1000
line(a == IP and g == IP and ms < 100,
     f"system resolver: getaddrinfo/gethostbyname('{IP}') -> {a} in {ms:.2f} ms (numeric, no DNS query)")

# 5. live server
s = socket.socket()
s.settimeout(1)
up = s.connect_ex((IP, 8080)) == 0
s.close()
if up:
    try:
        req = urllib.request.Request(f"http://{IP}:8080/w1", headers={"Host": IP})
        t = time.time()
        dat = urllib.request.urlopen(req, timeout=3).read()
        md5 = hashlib.md5(dat).hexdigest()
        line(md5 == pin, f"NAS http://{IP}:8080/w1 (Host: {IP}) -> {len(dat)} bytes, md5 {md5}, "
             f"{(time.time() - t) * 1000:.0f} ms")
    except OSError as e:
        line(False, f"NAS http://{IP}:8080/w1: {e}")
else:
    print(f"  [--] NAS {IP}:8080 not listening (server stopped); live check skipped")

print("name resolution: " + ("healthy (no DNS, no helper process needed)" if not bad else f"{bad} PROBLEM(S)"))
sys.exit(1 if bad else 0)
PY
