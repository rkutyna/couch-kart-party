#!/usr/bin/env bash
# Builds a devkitPPC-equivalent powerpc-eabi toolchain from upstream GNU sources,
# following the staging of devkitPro's buildscripts (build-binutils.sh,
# build-gcc-stage1.sh, build-newlib.sh, build-gcc-stage2.sh) with the same
# devkitPro patches and configure options (--with-cpu=750, --disable-multilib).
# Each stage writes a marker file so re-running resumes where it stopped.
#
# Versions taken from devkitPro/buildscripts (select_toolchain.sh, devkitPPC branch) at
# commit 5740da9beb433ff9f5b724fafc1127f3a5ddc944 (2026-10-10):
#   binutils 2.47 (patch binutils-2.47-2), gcc 16.2.0 (patch gcc-16.2.0-1),
#   newlib 4.6.0.20260123 (patch newlib-4.6.0.20260123-4).
#
# Only needed when no devkitPro devkitPPC is installed (the faster, preferred option:
# https://devkitpro.org/wiki/Getting_Started, then DEVKITPPC=/opt/devkitpro/devkitPPC).
# Downloads the GNU release tarballs from ftp.gnu.org / sourceware.org and the devkitPro
# patches from github.com/devkitPro/buildscripts at that commit, checks their SHA-256,
# and installs into toolchain/opt/devkitpro/devkitPPC (about 1-2 hours on an M-series Mac).
# Needs: Xcode Command Line Tools (clang), Homebrew make (gmake), gmp, mpfr, libmpc.
#   brew install make gmp mpfr libmpc
set -euo pipefail

TC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC=$TC/src
DL=$SRC/downloads
BLD=$TC/build
PREFIX=$TC/opt/devkitpro/devkitPPC
TARGET=powerpc-eabi
BINUTILS_VER=2.47
GCC_VER=16.2.0
NEWLIB_VER=4.6.0.20260123
JOBS=${JOBS:-$(sysctl -n hw.ncpu 2>/dev/null || echo 8)}
DKP_COMMIT=5740da9beb433ff9f5b724fafc1127f3a5ddc944

BREW_PREFIX=${BREW_PREFIX:-$(brew --prefix 2>/dev/null || echo /opt/homebrew)}
for f in gmp mpfr libmpc; do
    [ -d "$BREW_PREFIX/opt/$f" ] || { echo "missing Homebrew $f: brew install make gmp mpfr libmpc" >&2; exit 1; }
done
[ -x "$BREW_PREFIX/bin/gmake" ] || { echo "missing Homebrew make (gmake): brew install make" >&2; exit 1; }

export PATH=$BREW_PREFIX/bin:/usr/bin:/bin:/usr/sbin:/sbin
export MAKE=gmake
export CC=/usr/bin/clang
export CXX=/usr/bin/clang++
export MAKEINFO=true
# Host flags as in devkitPro build-devkit.sh (Darwin branch), with a deployment target that
# works on Apple Silicon. -DSTDINT_LONG32=0 is the devkitPPC-specific cppflags.
CPPFLAGS_HOST="-DSTDINT_LONG32=0 -mmacosx-version-min=11.0 -fbracket-depth=512 -I$BREW_PREFIX/include"
LDFLAGS_HOST="-mmacosx-version-min=11.0 -L$BREW_PREFIX/lib"
export CPPFLAGS="$CPPFLAGS_HOST"
export LDFLAGS="$LDFLAGS_HOST"

mark() { touch "$1"; }
log() { echo "==> $*"; }

mkdir -p "$SRC" "$DL" "$BLD" "$PREFIX"

# ---------------------------------------------------------------- download + verify
# file  sha256  url
DOWNLOADS="
binutils-$BINUTILS_VER.tar.xz 154ab23b60070e8f27013c22977f1129425d67d1e8acd6e13010e617811e4cff https://ftp.gnu.org/gnu/binutils/binutils-$BINUTILS_VER.tar.xz
gcc-$GCC_VER.tar.xz e6738e29597f733270731aa90600f37ffdc045079dfc27ec7e8192cc81085c3e https://ftp.gnu.org/gnu/gcc/gcc-$GCC_VER/gcc-$GCC_VER.tar.xz
newlib-$NEWLIB_VER.tar.gz 6ff27e3bf022666f43f7802255be680eeff722ac181b1725d21e2e8318604ee3 https://sourceware.org/pub/newlib/newlib-$NEWLIB_VER.tar.gz
binutils-$BINUTILS_VER-2.patch f591eb5bc1eeb534741822b6c1c0213f38892696ba5f1e8c402182a7ab3801ff https://raw.githubusercontent.com/devkitPro/buildscripts/$DKP_COMMIT/patches/binutils-$BINUTILS_VER-2.patch
gcc-$GCC_VER-1.patch 34cdb5dbdcfccec0852b21228bed42839c438ddfafc5cb3f01a3fbdb2ab995e8 https://raw.githubusercontent.com/devkitPro/buildscripts/$DKP_COMMIT/patches/gcc-$GCC_VER-1.patch
newlib-$NEWLIB_VER-4.patch 6d5aba4356553c727765c98e7553d3784f31d361c856e220ef27cc26353fb88f https://raw.githubusercontent.com/devkitPro/buildscripts/$DKP_COMMIT/patches/newlib-$NEWLIB_VER-4.patch
"
echo "$DOWNLOADS" | while read -r file sum url; do
    [ -n "$file" ] || continue
    if [ ! -f "$DL/$file" ] || [ "$(shasum -a 256 "$DL/$file" | cut -d' ' -f1)" != "$sum" ]; then
        log "downloading $url"
        curl -fL --retry 3 --progress-bar -o "$DL/$file.part" "$url"
        mv "$DL/$file.part" "$DL/$file"
    fi
    got=$(shasum -a 256 "$DL/$file" | cut -d' ' -f1)
    if [ "$got" != "$sum" ]; then
        echo "SHA-256 mismatch for $file: got $got, want $sum" >&2
        exit 1
    fi
    log "verified $file"
done

# ---------------------------------------------------------------- extract + patch
extract_and_patch() {
    local name=$1 ver=$2 patchfile=$3 tarball=$4 tarflag=$5
    if [ ! -d "$SRC/$name-$ver" ]; then
        log "extracting $name-$ver"
        tar -xf "$DL/$tarball" -C "$SRC"
    fi
    if [ ! -f "$SRC/$name-$ver/.dkp-patched" ]; then
        log "patching $name-$ver with $patchfile"
        patch -p1 -d "$SRC/$name-$ver" -i "$DL/$patchfile"
        mark "$SRC/$name-$ver/.dkp-patched"
    fi
}
extract_and_patch binutils "$BINUTILS_VER" "binutils-$BINUTILS_VER-2.patch" "binutils-$BINUTILS_VER.tar.xz" J
extract_and_patch gcc "$GCC_VER" "gcc-$GCC_VER-1.patch" "gcc-$GCC_VER.tar.xz" J
extract_and_patch newlib "$NEWLIB_VER" "newlib-$NEWLIB_VER-4.patch" "newlib-$NEWLIB_VER.tar.gz" z
touch "$SRC/newlib-$NEWLIB_VER/libgloss/doc/porting.info"

# ---------------------------------------------------------------- binutils
mkdir -p "$BLD/binutils" && cd "$BLD/binutils"
if [ ! -f configured ]; then
    log "configure binutils"
    "$SRC/binutils-$BINUTILS_VER/configure" --prefix="$PREFIX" --target=$TARGET \
        --disable-nls --disable-werror --disable-shared --disable-debug \
        --enable-poison-system-directories
    mark configured
fi
if [ ! -f built ]; then log "build binutils"; $MAKE -j"$JOBS"; mark built; fi
if [ ! -f installed ]; then log "install binutils"; $MAKE install; mark installed; fi

# ---------------------------------------------------------------- gcc stage 1 (compiler only)
GCC_CONFIG_ARGS=(
    --target=$TARGET --prefix="$PREFIX"
    --enable-languages=c,c++
    --with-gnu-as --with-gnu-ld --with-gcc
    --enable-cxx-flags=-ffunction-sections
    --disable-libstdcxx-verbose
    --enable-poison-system-directories
    --enable-threads=posix --disable-win32-registry --disable-nls --disable-debug
    --disable-libmudflap --disable-libssp --disable-libgomp
    --disable-libstdcxx-pch
    --enable-libstdcxx-time=yes
    --enable-libstdcxx-filesystem-ts
    --with-newlib=yes
    --with-native-system-header-dir=/include
    --with-sysroot="$PREFIX/$TARGET"
    --disable-tm-clone-registry
    --disable-__cxa_atexit
    --with-cpu=750 --disable-multilib
    --with-pkgversion="devkitPPC-equivalent build"
    --with-gmp="$BREW_PREFIX" --with-mpfr="$BREW_PREFIX" --with-mpc="$BREW_PREFIX"
)
mkdir -p "$BLD/gcc" && cd "$BLD/gcc"
if [ ! -f configured-stage1 ]; then
    log "configure gcc stage1"
    CFLAGS_FOR_TARGET="-O2 -ffunction-sections -fdata-sections" \
    CXXFLAGS_FOR_TARGET="-O2 -ffunction-sections -fdata-sections" \
    LDFLAGS_FOR_TARGET="" \
    "$SRC/gcc-$GCC_VER/configure" "${GCC_CONFIG_ARGS[@]}"
    mark configured-stage1
fi
if [ ! -f built-stage1 ]; then log "build gcc stage1 (all-gcc)"; $MAKE -j"$JOBS" all-gcc; mark built-stage1; fi
if [ ! -f installed-stage1 ]; then log "install gcc stage1"; $MAKE install-gcc; mark installed-stage1; fi

# ---------------------------------------------------------------- newlib
mkdir -p "$BLD/newlib" && cd "$BLD/newlib"
export PATH="$PREFIX/bin:$PATH"
if [ ! -f configured ]; then
    log "configure newlib"
    CFLAGS_FOR_TARGET="-O2 -ffunction-sections -fdata-sections -DCUSTOM_MALLOC_LOCK" \
    "$SRC/newlib-$NEWLIB_VER/configure" --target=$TARGET --prefix="$PREFIX" \
        --disable-newlib-supplied-syscalls --enable-newlib-mb \
        --disable-newlib-wide-orient --enable-newlib-register-fini
    mark configured
fi
if [ ! -f built ]; then log "build newlib"; $MAKE -j"$JOBS"; mark built; fi
if [ ! -f installed ]; then log "install newlib"; $MAKE install -j1; mark installed; fi

# ---------------------------------------------------------------- gcc stage 2 (libgcc, libstdc++, full)
cd "$BLD/gcc"
if [ ! -f built-stage2 ]; then log "build gcc stage2 (full)"; $MAKE -j"$JOBS"; mark built-stage2; fi
if [ ! -f installed-stage2 ]; then log "install gcc stage2"; $MAKE install; mark installed-stage2; fi

log "toolchain installed at $PREFIX"
"$PREFIX/bin/$TARGET-gcc" --version | head -1
