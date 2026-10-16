# Patches

Our changes to two upstream projects by WiiLink. This folder holds only the patches, not
the upstream code: `setup.sh` fetches each upstream at a pinned commit and applies the
matching patch to it.

| Patch | Upstream | Pinned commit | Licence of the patch |
|---|---|---|---|
| `wfc-server.patch` | [wfc-server](https://github.com/WiiLink24/wfc-server) | `f0825bf89a28de560de99006802e065e17b9e038` | GNU AGPL-3.0 ([LICENSE-wfc-server.md](LICENSE-wfc-server.md)) |
| `wfc-patcher-wii.patch` | [wfc-patcher-wii](https://github.com/WiiLink24/wfc-patcher-wii) | `0d9d7a697d7fb299ecc629f2c5da071d116369f2` (tag `payload-0.3.1`) | WiiLink's licence ([LICENSE-wfc-patcher-wii](LICENSE-wfc-patcher-wii)) |

The rest of this repository is MIT-licensed (see [../LICENSE](../LICENSE)). These two
patch files are not: they carry the licences of the projects they modify.

## How they are used

`./mkparty setup` (step 2, "upstreams") clones wfc-server into `server/wfc-server` and
wfc-patcher-wii into `patcher/wfc-patcher-wii-local`, checks out the pinned commits, and
applies the patches with `git apply`. Running setup again is safe: a patch that is already
applied is detected and skipped.

## What the patches change

**`wfc-server.patch`** (wfc-server; `filter/`, `gpcm/`, `nas/`, `qr2/`, `serverbrowser/`):
makes the server work for a private, offline game on one Mac. The NAS answers the patched
game's requests by IP address instead of the `nas.` host name, accepts the device ID that
newer stage 1 payloads send, advertises loopback addresses to peers on this machine, ignores
the Mario Kart Wii `p` matchmaking flag and a `wl:report` sent before login, and logs game
downloads. It also adds two switches for matchmaking: `WWFC_PREFER_ROOM=1` (join the
fullest open room) and `WWFC_DEBUG_MATCH=1` (log each candidate room). A unit test covers the
`p` flag change. In the patched server itself both switches are off unless the environment
sets them; Couch Kart Party's `server/start.sh` turns `WWFC_PREFER_ROOM` on by default
(`WWFC_PREFER_ROOM=0` in the environment or `mkparty.conf` turns it off), so in a party it
is on unless you say otherwise.

**`wfc-patcher-wii.patch`** (wfc-patcher-wii; `include/`, `patch/`, `payload/`, `stage1/`):
builds a "no-DNS" game patch. The online-service host names the game uses (`nintendowifi.net`,
`gamespy.com`, `wiimmfi.de` and the WiiLink domain) are replaced by the IP literal `127.0.0.1`
(`WWFC_LOCAL_IP`), and the game's HTTP port is 8080 instead of 80. The game then makes no DNS
queries and works offline.

## Licences

**`wfc-server.patch`** is distributed under the GNU Affero General Public License, version 3,
the licence of wfc-server. See [LICENSE-wfc-server.md](LICENSE-wfc-server.md).

**`wfc-patcher-wii.patch`** is distributed under WiiLink's licence, the licence of
wfc-patcher-wii. The full text is in [LICENSE-wfc-patcher-wii](LICENSE-wfc-patcher-wii)
(copied verbatim from the upstream `LICENSE`).

This is an altered version of WiiLink's wfc-patcher-wii. Credit: WiiLink WFC -
https://wfc.wiilink.ca. `setup.sh` also writes a notice saying so,
`ALTERED-BY-COUCH-KART-PARTY.md`, into the patched clone (`patcher/wfc-patcher-wii-local`).
