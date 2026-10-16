#!/bin/zsh
# setup.sh - one-time installation of mkparty on this Mac. Run it as `./mkparty setup`.
# Idempotent: every step checks what is already there and only does what is missing.
#
#   ./mkparty setup [options]
#     --game PATH         the Mario Kart Wii disc image (.wbfs or .iso, RMCE01 revision 0);
#                         stored as GAME_IMAGE in mkparty.conf
#     --dolphin PATH      the Dolphin app (default /Applications/Dolphin.app); DOLPHIN_APP
#     --build-toolchain   no devkitPPC installed: build one with toolchain/build-devkitppc.sh
#                         (1-2 hours). Installing devkitPro is faster (see README.md).
#     --rebuild-patch     rebuild the game patch (Gecko code + payload) even if present.
#                         The payload signing key is kept; profiles get the new code at
#                         their next start.
#     --only STEP[,STEP]  run only these steps (prereqs, upstreams, toolchain, patch,
#                         server, apps)
#     --skip-prereqs      do not run the prerequisites step (CI / repeat runs)
#
# Steps:
#   1 prereqs    macOS on Apple Silicon, Xcode Command Line Tools (swiftc), Homebrew, go,
#                postgresql@16, ffmpeg with h264_videotoolbox, python >= 3.11 with the
#                packages of requirements.txt (a .venv is made if they are missing), git,
#                Dolphin, the disc image (header RMCE01, revision 0), free ports.
#   2 upstreams  clone WiiLink's wfc-server and wfc-patcher-wii at the pinned commits and
#                apply our patches from patches/.
#   3 toolchain  find devkitPPC (DEVKITPPC, DEVKITPRO, /opt/devkitpro, toolchain/opt);
#                only needed when the game patch must be built.
#   4 patch      generate this install's payload signing key, build the no-DNS patch
#                (domain mkw.invalid, server at 127.0.0.1) into patcher/out-local and copy
#                the payload to server/wfc-server/payload.
#   5 server     config.xml with a random database password and API secret, a private
#                PostgreSQL 16 cluster (server/pgdata, 127.0.0.1:PG_PORT) with the schema,
#                and the wwfc binary.
#   6 apps       partyd, fakephones, the window-capture and OCR helpers (Swift) and the
#                self-signed HTTPS certificate.
# Afterwards (separately, needs the Mac unlocked, starts Dolphin): ./mkparty setup --profiles
# (prepares the Dolphin profiles and generates the screen templates from your own game).
set -uo pipefail

ROOT=${0:A:h}
CONF=$ROOT/mkparty.conf
CONF_EXAMPLE=$ROOT/mkparty.conf.example
LOG_DIR=$ROOT/run/logs

WFC_SERVER_URL=https://github.com/WiiLink24/wfc-server
WFC_SERVER_COMMIT=f0825bf89a28de560de99006802e065e17b9e038
PATCHER_URL=https://github.com/WiiLink24/wfc-patcher-wii
PATCHER_COMMIT=0d9d7a697d7fb299ecc629f2c5da071d116369f2   # tag payload-0.3.1
WFC_DIR=$ROOT/server/wfc-server
PATCHER_DIR=$ROOT/patcher/wfc-patcher-wii-local
OUT_DIR=$ROOT/patcher/out-local
WWFC_DOMAIN=mkw.invalid
GECKO_HEAD='$WFC local-ip 8080'
GAME_ID=RMCE01
PATCH_TITLE=RMCED00        # wfc-patcher-wii's name for RMCE01 (disc, revision 0)

# ------------------------------------------------------------------ output helpers
typeset -i NWARN=0
step() { print -r -- ""; print -r -- "== [$1] $2"; }
ok()   { print -r -- "   ok    $*"; }
info() { print -r -- "   ..    $*"; }
warn() { print -r -- "   WARN  $*"; NWARN+=1; }
fail() { print -u2 -r -- "   FAIL  $*"; print -u2 -r -- ""; print -u2 -r -- "setup stopped. Fix the above and run ./mkparty setup again."; exit 1; }

# ------------------------------------------------------------------ arguments
opt_game="" opt_dolphin="" opt_build_tc=0 opt_rebuild=0 opt_skip_prereqs=0
typeset -a only=()
while (( $# )); do
  case $1 in
    --game) opt_game=${2:-}; shift ;;
    --dolphin) opt_dolphin=${2:-}; shift ;;
    --build-toolchain) opt_build_tc=1 ;;
    --rebuild-patch) opt_rebuild=1 ;;
    --only) only=(${(s:,:)${2:-}}); shift ;;
    --skip-prereqs) opt_skip_prereqs=1 ;;
    -h|--help) sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) print -u2 "unknown option: $1 (see ./mkparty setup --help)"; exit 2 ;;
  esac
  shift
done
want() {
  if (( ${#only} )); then (( ${only[(Ie)$1]} )); return; fi
  [[ $1 == prereqs ]] && (( opt_skip_prereqs )) && return 1
  return 0
}

# ------------------------------------------------------------------ local config
# conf_set KEY VALUE: sets KEY="VALUE" in mkparty.conf (replacing a commented example line).
conf_set() {
  local key=$1 val=$2
  [[ $val != *[\"\$\`\\]* ]] || fail "$key: the value may not contain \" \$ \` or \\: $val"
  python3 -I - "$CONF" "$key" "$val" <<'PY' || fail "could not write $CONF"
import re, sys
path, key, val = sys.argv[1:4]
lines = open(path).read().splitlines(True)
new = f'{key}="{val}"\n'
pat = re.compile(r'^\s*#?\s*(?:export\s+)?' + re.escape(key) + r'=')
for i, l in enumerate(lines):
    if pat.match(l):
        lines[i] = new
        break
else:
    if lines and not lines[-1].endswith("\n"):
        lines[-1] += "\n"
    lines.append(new)
open(path, "w").write("".join(lines))
PY
  typeset -g "$key=$val"
}

mkdir -p "$LOG_DIR"
if [[ ! -f $CONF ]]; then
  cp "$CONF_EXAMPLE" "$CONF" || fail "cannot create $CONF"
  print -r -- "created mkparty.conf (local settings of this Mac, not in git)"
fi
source "$CONF"
if [[ -n $opt_dolphin ]]; then conf_set DOLPHIN_APP "${opt_dolphin:A}"; fi
if [[ -n $opt_game ]]; then conf_set GAME_IMAGE "${opt_game:A}"; fi
DOLPHIN_APP=${${DOLPHIN_APP:-/Applications/Dolphin.app}%/}
GAME_IMAGE=${GAME_IMAGE:-}
PG_PORT=${PG_PORT:-54329}
PARTY_PORT=${PARTY_PORT:-8000}
HTTPS_PORT=${HTTPS_PORT:-8443}
if [[ -z $GAME_IMAGE ]]; then
  # One disc image in the project folder: take it.
  typeset -a found=( $ROOT/*.(wbfs|iso)(N) )
  if (( ${#found} == 1 )); then
    conf_set GAME_IMAGE "$found[1]"
    print -r -- "using the disc image $found[1] (GAME_IMAGE in mkparty.conf)"
  fi
fi

# Homebrew prefix and the tools in it.
if command -v brew >/dev/null 2>&1; then BREW_PREFIX=$(brew --prefix)
elif [[ -x /opt/homebrew/bin/brew ]]; then BREW_PREFIX=/opt/homebrew
elif [[ -x /usr/local/bin/brew ]]; then BREW_PREFIX=/usr/local
else BREW_PREFIX=""; fi
[[ -n $BREW_PREFIX && :$PATH: != *:$BREW_PREFIX/bin:* ]] && PATH=$PATH:$BREW_PREFIX/bin
[[ -x $ROOT/.venv/bin/python3 ]] && PATH=$ROOT/.venv/bin:$PATH
export PATH

# ------------------------------------------------------------------ 1. prerequisites
check_disc() {
  # Prints "<ID> <disc> <revision>" of a .wbfs (disc header at 0x200) or .iso (at 0).
  python3 -I - "$1" <<'PY'
import sys
with open(sys.argv[1], "rb") as f:
    head = f.read(0x240)
off = 0x200 if head[:4] == b"WBFS" else 0
h = head[off:off + 0x20]
if len(h) < 0x20 or h[0x18:0x1C] != bytes.fromhex("5D1C9EA3"):
    print("NOTWII 0 0"); sys.exit(0)
print(h[:6].decode("ascii", "replace"), h[6], h[7])
PY
}

prereqs() {
  step 1 "prerequisites"
  [[ $(uname -s) == Darwin ]] || fail "macOS is required (Dolphin window capture, VideoToolbox, Bonjour)"
  if [[ $(uname -m) == arm64 ]]; then ok "macOS $(sw_vers -productVersion) on Apple Silicon"
  else warn "macOS on $(uname -m): only Apple Silicon has been tested"; fi

  if xcrun --find swiftc >/dev/null 2>&1 || command -v swiftc >/dev/null 2>&1; then ok "swiftc (Xcode Command Line Tools)"
  else fail "swiftc missing: install the Xcode Command Line Tools (xcode-select --install)"; fi
  [[ -n $BREW_PREFIX ]] || fail "Homebrew missing: https://brew.sh"
  ok "Homebrew at $BREW_PREFIX"
  command -v git >/dev/null 2>&1 || fail "git missing (Xcode Command Line Tools)"
  command -v go >/dev/null 2>&1 || fail "go missing: brew install go"
  ok "$(go version)"
  local pgbin; pgbin="$(brew --prefix postgresql@16 2>/dev/null)/bin"
  [[ -x $pgbin/initdb ]] || fail "PostgreSQL 16 missing: brew install postgresql@16"
  ok "PostgreSQL 16 ($pgbin)"
  command -v ffmpeg >/dev/null 2>&1 || fail "ffmpeg missing: brew install ffmpeg"
  ffmpeg -hide_banner -encoders 2>/dev/null | grep -q h264_videotoolbox \
    || fail "ffmpeg at $(command -v ffmpeg) has no h264_videotoolbox encoder (use Homebrew's ffmpeg)"
  ok "ffmpeg with h264_videotoolbox ($(command -v ffmpeg))"
  command -v openssl >/dev/null 2>&1 || fail "openssl missing"

  command -v python3 >/dev/null 2>&1 || fail "python3 missing: brew install python"
  python3 -I -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)' \
    || fail "python >= 3.11 needed, found $(python3 --version 2>&1)"
  if python3 -c 'import numpy, PIL, cryptography' 2>/dev/null; then
    ok "$(python3 --version) with numpy, Pillow, cryptography ($(command -v python3))"
  else
    info "python packages missing: creating $ROOT/.venv with requirements.txt"
    [[ -x $ROOT/.venv/bin/python3 ]] || python3 -m venv "$ROOT/.venv" || fail "python3 -m venv failed"
    "$ROOT/.venv/bin/python3" -m pip install --quiet --upgrade pip >/dev/null 2>&1
    "$ROOT/.venv/bin/python3" -m pip install --quiet -r "$ROOT/requirements.txt" || fail "pip install -r requirements.txt failed"
    PATH=$ROOT/.venv/bin:$PATH; export PATH
    python3 -c 'import numpy, PIL, cryptography' || fail "the packages still do not import"
    ok "python packages installed in .venv (mkparty uses it automatically)"
  fi

  if [[ -x $DOLPHIN_APP/Contents/MacOS/Dolphin ]]; then ok "Dolphin at $DOLPHIN_APP"
  else fail "Dolphin not found at $DOLPHIN_APP: install it from https://dolphin-emu.org or set DOLPHIN_APP in mkparty.conf (or --dolphin PATH)"; fi

  [[ -n $GAME_IMAGE ]] || fail "no disc image set: run ./mkparty setup --game /path/to/MarioKartWii.wbfs (or set GAME_IMAGE in mkparty.conf)"
  [[ -f $GAME_IMAGE ]] || fail "disc image not found: $GAME_IMAGE (GAME_IMAGE in mkparty.conf)"
  case ${GAME_IMAGE:l} in
    *.wbfs|*.iso)
      local id disc rev
      read -r id disc rev <<< "$(check_disc "$GAME_IMAGE")"
      [[ $id == NOTWII ]] && fail "$GAME_IMAGE is not a Wii disc image"
      [[ $id == "$GAME_ID" ]] || fail "$GAME_IMAGE is $id; mkparty needs Mario Kart Wii USA ($GAME_ID)"
      [[ $rev == 0 ]] || fail "$GAME_IMAGE is $GAME_ID revision $rev; the patch is built for revision 0"
      ok "disc image $GAME_ID revision $rev: $GAME_IMAGE" ;;
    *) warn "cannot read the header of $GAME_IMAGE (not .wbfs / .iso); it must be $GAME_ID revision 0" ;;
  esac

  local p who busy=0
  for p in $PARTY_PORT $HTTPS_PORT 8080 28910 29900 29901 29920 29998 29999 $PG_PORT; do
    who=$(lsof -nP -iTCP:$p -sTCP:LISTEN -Fc 2>/dev/null | sed -n 's/^c//p' | head -1)
    if [[ -n $who ]]; then warn "TCP port $p is in use by '$who' (fine if it is this party running; else stop it)"; busy=1; fi
  done
  for p in 27900 27901; do
    who=$(lsof -nP -iUDP:$p -Fc 2>/dev/null | sed -n 's/^c//p' | head -1)
    if [[ -n $who ]]; then warn "UDP port $p is in use by '$who'"; busy=1; fi
  done
  (( busy )) || ok "ports free (TCP $PARTY_PORT $HTTPS_PORT 8080 28910 29900 29901 29920 29998 29999 $PG_PORT, UDP 27900 27901)"
}

# ------------------------------------------------------------------ 2. upstream sources
# fetch_upstream <name> <url> <commit> <dir> <patch>
fetch_upstream() {
  local name=$1 url=$2 commit=$3 dir=$4 patch=$5 head
  if [[ ! -d $dir/.git ]]; then
    [[ -e $dir ]] && fail "$dir exists but is not a git clone; move it away"
    info "cloning $url"
    git clone --quiet "$url" "$dir" || fail "git clone $url failed"
    git -C "$dir" -c advice.detachedHead=false checkout --quiet "$commit" || fail "commit $commit not found in $url"
  fi
  head=$(git -C "$dir" rev-parse HEAD)
  if [[ $head == "$commit" ]]; then ok "$name at the pinned commit ${commit[1,12]}"
  else warn "$name is at ${head[1,12]}, not the pinned ${commit[1,12]} (leaving it as it is)"; fi
  if git -C "$dir" apply --reverse --check "$patch" 2>/dev/null; then
    ok "$name: patches/${patch:t} already applied"
  elif git -C "$dir" apply --check "$patch" 2>/dev/null; then
    git -C "$dir" apply "$patch" || fail "applying ${patch:t} failed"
    ok "$name: applied patches/${patch:t}"
  else
    fail "patches/${patch:t} does not apply to $dir (local changes? see git -C $dir status)"
  fi
}

# mark_altered: WiiLink's licence asks that altered versions of wfc-patcher-wii are plainly
# marked as such; the patched clone gets a notice file at its top.
mark_altered() {
  local f=$PATCHER_DIR/ALTERED-BY-COUCH-KART-PARTY.md
  [[ -f $f ]] && { ok "wfc-patcher-wii: altered-version notice present"; return; }
  cat > "$f" <<'MD' || fail "could not write $f"
# Altered version

This is an altered version of WiiLink's wfc-patcher-wii
(https://github.com/WiiLink24/wfc-patcher-wii), not the original. It was changed by
Couch Kart Party's setup, which applied `patches/wfc-patcher-wii.patch` (a "no-DNS" build:
the online-service host names are replaced by the IP literal 127.0.0.1 and the game's HTTP
port is 8080) to the upstream commit pinned in `setup.sh`.

The original software and its licence (see LICENSE in this folder) are by WiiLink.
Credit: WiiLink WFC - https://wfc.wiilink.ca
MD
  ok "wfc-patcher-wii: wrote ALTERED-BY-COUCH-KART-PARTY.md (altered-version notice)"
}

upstreams() {
  step 2 "upstream sources (WiiLink wfc-server, wfc-patcher-wii) + our patches"
  mkdir -p "$ROOT/patcher"
  fetch_upstream wfc-server "$WFC_SERVER_URL" "$WFC_SERVER_COMMIT" "$WFC_DIR" "$ROOT/patches/wfc-server.patch"
  fetch_upstream wfc-patcher-wii "$PATCHER_URL" "$PATCHER_COMMIT" "$PATCHER_DIR" "$ROOT/patches/wfc-patcher-wii.patch"
  mark_altered
}

# ------------------------------------------------------------------ 3. toolchain
patch_built() {
  [[ -f $OUT_DIR/RMCE01.gecko.txt && -f $OUT_DIR/payload/stage1.bin && -f $OUT_DIR/payload/stage1v1.bin \
     && -f $OUT_DIR/payload/private-key.pem && -f $OUT_DIR/payload/binary/payload.$PATCH_TITLE.bin ]]
}
find_devkitppc() {
  local d
  for d in ${DEVKITPPC:-} ${DEVKITPRO:+$DEVKITPRO/devkitPPC} /opt/devkitpro/devkitPPC $ROOT/toolchain/opt/devkitpro/devkitPPC; do
    [[ -x $d/bin/powerpc-eabi-gcc ]] && { print -r -- "${d%/}"; return 0; }
  done
  return 1
}
toolchain() {
  step 3 "PowerPC toolchain (devkitPPC)"
  if patch_built && (( ! opt_rebuild )); then ok "not needed: the game patch is already built (patcher/out-local)"; return; fi
  local d
  if d=$(find_devkitppc); then
    DEVKITPPC=$d; export DEVKITPPC
    ok "devkitPPC at $d ($("$d/bin/powerpc-eabi-gcc" --version | head -1))"
    return
  fi
  if (( opt_build_tc )); then
    info "building the toolchain (1-2 hours); log: $LOG_DIR/setup-toolchain.log"
    "$ROOT/toolchain/build-devkitppc.sh" > "$LOG_DIR/setup-toolchain.log" 2>&1 \
      || fail "toolchain build failed; see $LOG_DIR/setup-toolchain.log"
    DEVKITPPC=$ROOT/toolchain/opt/devkitpro/devkitPPC; export DEVKITPPC
    ok "devkitPPC built at $DEVKITPPC"
    return
  fi
  fail "no devkitPPC found (looked at \$DEVKITPPC, \$DEVKITPRO/devkitPPC, /opt/devkitpro/devkitPPC, toolchain/opt).
         Either install devkitPro's devkitPPC (faster; https://devkitpro.org/wiki/Getting_Started, then
         set DEVKITPPC=/opt/devkitpro/devkitPPC in mkparty.conf if it is elsewhere), or build one here
         from the GNU sources: ./mkparty setup --build-toolchain (1-2 hours)."
}

# ------------------------------------------------------------------ 4. game patch
sync_payload() {
  if [[ -d $WFC_DIR/payload ]] && diff -rq "$OUT_DIR/payload" "$WFC_DIR/payload" >/dev/null 2>&1; then
    ok "server payload matches patcher/out-local/payload"
  else
    rm -rf "$WFC_DIR/payload.new"
    cp -R "$OUT_DIR/payload" "$WFC_DIR/payload.new" && rm -rf "$WFC_DIR/payload" && mv "$WFC_DIR/payload.new" "$WFC_DIR/payload" \
      || fail "could not copy the payload to $WFC_DIR/payload"
    chmod 600 "$WFC_DIR/payload/private-key.pem"
    ok "copied the payload to server/wfc-server/payload"
  fi
}
build_patch() {
  step 4 "game patch (no-DNS WFC patch for $GAME_ID, signed with this install's key)"
  [[ -d $PATCHER_DIR ]] || fail "$PATCHER_DIR missing (run the upstreams step)"
  if patch_built && (( ! opt_rebuild )); then
    ok "patcher/out-local is built ($(sed -n 1p "$OUT_DIR/RMCE01.gecko.txt"))"
  else
    [[ -n ${DEVKITPPC:-} ]] || fail "DEVKITPPC unknown (run the toolchain step)"
    if [[ -f $PATCHER_DIR/misc/private-key-OUT.pem ]]; then ok "payload signing key exists (kept)"
    else info "a new payload signing key will be generated (misc/private-key-OUT.pem in the patcher clone)"; fi
    # make.sh calls `python`; give it the python3 that has `cryptography`.
    local shim; shim=$(mktemp -d) || fail "mktemp failed"
    ln -s "$(command -v python3)" "$shim/python"
    info "building (a few minutes); log: $LOG_DIR/setup-patch.log"
    ( cd "$PATCHER_DIR" && PATH=$shim:$PATH DEVKITPPC=$DEVKITPPC \
        ./make.sh --clean --all -- -j"$(sysctl -n hw.ncpu)" "-DWWFC_DOMAIN=\"$WWFC_DOMAIN\"" ) \
        > "$LOG_DIR/setup-patch.log" 2>&1
    local rc=$?
    rm -rf "$shim"
    (( rc == 0 )) || fail "the patch build failed; see $LOG_DIR/setup-patch.log"
    local b=$PATCHER_DIR new=$OUT_DIR.new
    for f in patch/build/$PATCH_TITLE.txt stage1/build/stage1.bin dist/binary/payload.$PATCH_TITLE.bin misc/private-key-OUT.pem misc/wwfcPayloadPublicKey-OUT.hpp; do
      [[ -f $b/$f ]] || fail "build output missing: $b/$f (see $LOG_DIR/setup-patch.log)"
    done
    rm -rf "$new"; mkdir -p "$new/payload" "$new/keys"
    { print -r -- "$GECKO_HEAD"; cat "$b/patch/build/$PATCH_TITLE.txt"; } > "$new/RMCE01.gecko.txt"
    cp -R "$b/dist/binary" "$new/payload/binary"
    cp "$b/stage1/build/stage1.bin" "$new/payload/stage1.bin"
    cp "$b/stage1/build/stage1.bin" "$new/payload/stage1v1.bin"
    ( umask 077; cp "$b/misc/private-key-OUT.pem" "$new/payload/private-key.pem"
      cp "$b/misc/private-key-OUT.pem" "$new/keys/wwfc-payload-private-key-local.pem" )
    cp "$b/misc/wwfcPayloadPublicKey-OUT.hpp" "$new/keys/wwfcPayloadPublicKey-local.hpp"
    rm -rf "$OUT_DIR" && mv "$new" "$OUT_DIR" || fail "could not install $OUT_DIR"
    ok "built patcher/out-local ($(grep -c . "$OUT_DIR/RMCE01.gecko.txt") Gecko lines, $(ls "$OUT_DIR/payload/binary" | wc -l | tr -d ' ') payloads)"
  fi
  [[ -d $WFC_DIR ]] && sync_payload
}

# ------------------------------------------------------------------ 5. server
server() {
  step 5 "game server (wfc-server + PostgreSQL 16 on 127.0.0.1:$PG_PORT)"
  [[ -d $WFC_DIR ]] || fail "$WFC_DIR missing (run the upstreams step)"
  local pgbin; pgbin="$(brew --prefix postgresql@16 2>/dev/null)/bin"
  [[ -x $pgbin/initdb ]] || fail "PostgreSQL 16 missing: brew install postgresql@16"
  mkdir -p "$ROOT/server/logs"
  [[ -e $WFC_DIR/logs || -L $WFC_DIR/logs ]] || ln -s ../logs "$WFC_DIR/logs"

  if [[ -f $WFC_DIR/config.xml ]]; then
    ok "config.xml exists (kept)"
  else
    ( umask 077
      python3 -I - "$ROOT/server/config.template.xml" "$WFC_DIR/config.xml" "$PG_PORT" <<'PY'
import secrets, sys
src, dst, port = sys.argv[1:4]
s = open(src).read()
s = s[s.index("<Config>"):]
s = (s.replace("@DB_PASSWORD@", secrets.token_hex(24)).replace("@API_SECRET@", secrets.token_hex(32))
      .replace("@PG_PORT@", port))
open(dst, "w").write(s)
PY
    ) || fail "could not write config.xml"
    ok "wrote config.xml (random database password and API secret)"
  fi
  local dbpass dbport
  dbpass=$(python3 -I -c 'import re,sys; print(re.search(r"<password>(.*?)</password>", open(sys.argv[1]).read()).group(1))' "$WFC_DIR/config.xml") \
    || fail "no <password> in config.xml"
  dbport=$(python3 -I -c 'import re,sys; m=re.search(r"<databaseAddress>[^<:]*:(\d+)</databaseAddress>", open(sys.argv[1]).read()); print(m.group(1) if m else 5432)' "$WFC_DIR/config.xml")
  [[ $dbport == "$PG_PORT" ]] || warn "config.xml uses database port $dbport but PG_PORT is $PG_PORT"

  export LC_ALL=en_US.UTF-8 LANG=en_US.UTF-8
  local pgdata=$ROOT/server/pgdata
  if [[ -f $pgdata/PG_VERSION ]]; then
    ok "database cluster exists (server/pgdata, version $(<"$pgdata/PG_VERSION"))"
  else
    local pw; pw=$(mktemp) || fail "mktemp failed"
    print -r -- "$dbpass" > "$pw"
    "$pgbin/initdb" -D "$pgdata" -U wiilink -A scram-sha-256 --pwfile="$pw" -E UTF8 --locale=en_US.UTF-8 \
      > "$LOG_DIR/setup-initdb.log" 2>&1
    local rc=$?; rm -f "$pw"
    (( rc == 0 )) || fail "initdb failed; see $LOG_DIR/setup-initdb.log"
    print -r -- "
# mkparty: private cluster, loopback only
port = $PG_PORT
listen_addresses = '127.0.0.1'
unix_socket_directories = ''" >> "$pgdata/postgresql.conf"
    ok "created the database cluster server/pgdata (user wiilink, 127.0.0.1:$PG_PORT)"
  fi

  local started=0
  if ! "$pgbin/pg_ctl" -D "$pgdata" status >/dev/null 2>&1; then
    "$pgbin/pg_ctl" -D "$pgdata" -l "$ROOT/server/logs/postgres.log" -w -t 60 start >/dev/null \
      || fail "PostgreSQL did not start; see server/logs/postgres.log"
    started=1
  fi
  local have
  have=$(PGPASSWORD=$dbpass "$pgbin/psql" -h 127.0.0.1 -p "$PG_PORT" -U wiilink -d postgres -tAc \
    "select 1 from pg_database where datname = 'wwfc'" 2>"$LOG_DIR/setup-db.log")
  if [[ $? -ne 0 ]]; then
    (( started )) && "$pgbin/pg_ctl" -D "$pgdata" -m fast -w stop >/dev/null
    fail "cannot connect to PostgreSQL on 127.0.0.1:$PG_PORT as wiilink; see $LOG_DIR/setup-db.log"
  fi
  if [[ $have == 1 ]]; then
    ok "database wwfc exists"
  else
    if PGPASSWORD=$dbpass "$pgbin/createdb" -h 127.0.0.1 -p "$PG_PORT" -U wiilink wwfc >>"$LOG_DIR/setup-db.log" 2>&1 \
      && PGPASSWORD=$dbpass "$pgbin/psql" -h 127.0.0.1 -p "$PG_PORT" -U wiilink -d wwfc -v ON_ERROR_STOP=1 -q \
           -f "$WFC_DIR/schema.sql" >>"$LOG_DIR/setup-db.log" 2>&1; then
      ok "created the database wwfc with schema.sql"
    else
      (( started )) && "$pgbin/pg_ctl" -D "$pgdata" -m fast -w stop >/dev/null
      fail "creating the database failed; see $LOG_DIR/setup-db.log"
    fi
  fi
  (( started )) && "$pgbin/pg_ctl" -D "$pgdata" -m fast -w -t 60 stop >/dev/null

  if [[ -x $WFC_DIR/wwfc && -z $(find "$WFC_DIR" -name '*.go' -newer "$WFC_DIR/wwfc" -print -quit) ]]; then
    ok "wwfc binary is up to date"
  else
    ( cd "$WFC_DIR" && go build -o wwfc . ) > "$LOG_DIR/setup-wwfc-build.log" 2>&1 \
      || fail "go build of wfc-server failed; see $LOG_DIR/setup-wwfc-build.log"
    ok "built server/wfc-server/wwfc"
  fi
  [[ -f $WFC_DIR/payload/stage1.bin ]] || warn "the server has no payload yet (run the patch step)"
}

# ------------------------------------------------------------------ 6. apps
# newer_than <target> <file|dir>...: true if target is missing or older than any source.
newer_than() {
  local t=$1; shift
  [[ -e $t ]] || return 0
  [[ -n $(find "$@" -type f -newer "$t" -print -quit 2>/dev/null) ]]
}
apps() {
  step 6 "phone server, test phones, capture helpers, HTTPS certificate"
  local p=$ROOT/party
  if newer_than "$p/partyd" "$p"/*.go "$p/go.mod" "$p/go.sum" "$p/web"; then
    ( cd "$p" && go build -o partyd . ) > "$LOG_DIR/setup-partyd-build.log" 2>&1 \
      || fail "go build of partyd failed; see $LOG_DIR/setup-partyd-build.log"
    ok "built party/partyd"
  else ok "party/partyd is up to date"; fi
  mkdir -p "$ROOT/run/bin" "$ROOT/capture/bin"
  if newer_than "$ROOT/run/bin/fakephones" "$p/cmd/fakephones" "$p/go.mod"; then
    ( cd "$p" && go build -o ../run/bin/fakephones ./cmd/fakephones ) > "$LOG_DIR/setup-fakephones-build.log" 2>&1 \
      || fail "go build of fakephones failed; see $LOG_DIR/setup-fakephones-build.log"
    ok "built run/bin/fakephones"
  else ok "run/bin/fakephones is up to date"; fi
  if newer_than "$ROOT/capture/bin/wincap" "$ROOT/capture/wincap.swift"; then
    swiftc -O -swift-version 5 -o "$ROOT/capture/bin/wincap" "$ROOT/capture/wincap.swift" > "$LOG_DIR/setup-swift.log" 2>&1 \
      || fail "swiftc wincap failed; see $LOG_DIR/setup-swift.log"
    ok "built capture/bin/wincap"
  else ok "capture/bin/wincap is up to date"; fi
  if newer_than "$ROOT/run/bin/winlist" "$ROOT/run/src/winlist.swift"; then
    swiftc -O -o "$ROOT/run/bin/winlist" "$ROOT/run/src/winlist.swift" >> "$LOG_DIR/setup-swift.log" 2>&1 \
      || fail "swiftc winlist failed; see $LOG_DIR/setup-swift.log"
    ok "built run/bin/winlist"
  else ok "run/bin/winlist is up to date"; fi
  if newer_than "$ROOT/run/bin/ocr" "$ROOT/run/src/ocr.swift"; then
    swiftc -O -o "$ROOT/run/bin/ocr" "$ROOT/run/src/ocr.swift" >> "$LOG_DIR/setup-swift.log" 2>&1 \
      || fail "swiftc ocr failed; see $LOG_DIR/setup-swift.log"
    ok "built run/bin/ocr (Vision text recognition, used to generate the screen templates)"
  else ok "run/bin/ocr is up to date"; fi
  local out; out=$("$p/make-cert.sh" 2>&1) || fail "make-cert.sh failed: $out"
  ok "HTTPS certificate: ${out%%$'\n'*}"
}

# ------------------------------------------------------------------ run
print -r -- "mkparty setup in $ROOT"
want prereqs && prereqs
want upstreams && upstreams
want toolchain && toolchain
if want patch; then
  if ! patch_built || (( opt_rebuild )) && [[ -z ${DEVKITPPC:-} ]]; then
    d=$(find_devkitppc) && { DEVKITPPC=$d; export DEVKITPPC; }
  fi
  build_patch
fi
want server && server
want apps && apps

print -r -- ""
if (( NWARN )); then print -r -- "setup finished with $NWARN warning(s) (see WARN above)."
else print -r -- "setup finished."; fi
typeset -a ready=( "$ROOT"/instances/i*/mkparty-ready(N) )
k_ready=${#ready}
if (( k_ready < 12 )); then
  print -r -- "Last step (once, about 25 minutes: profiles ~8, screen templates ~16; starts Dolphin; keep the Mac unlocked):"
  print -r -- "  ./mkparty setup --profiles"
  print -r -- "Give the terminal Screen Recording permission (System Settings > Privacy & Security) when macOS asks."
else
  assets=${MKPARTY_ASSETS:-$ROOT/assets}; [[ $assets == /* ]] || assets=$ROOT/$assets
  if [[ -f $assets/templates.npz && -f $assets/templates43.npz ]] || [[ -z ${MKPARTY_ASSETS:-} && -f $ROOT/nav/templates.npz ]]; then
    print -r -- "All 12 Dolphin profiles are set up. Start a party with: ./mkparty up"
  else
    print -r -- "The profiles are set up but the screen templates are not: run ./mkparty calibrate (about 16-18 minutes)."
  fi
fi
