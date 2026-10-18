#!/usr/bin/env bash
# Builds the guest kernel: x86_64 allnoconfig + hangar.fragment, no modules,
# producing a vmlinux for Cloud Hypervisor direct kernel boot (PVH).
#
#   build.sh                  resolve the config, check it, build vmlinux
#   build.sh --config-only    resolve and check the config, do not compile
#   build.sh --update-config  rewrite the committed config instead of failing
#
# The resolved .config is checked against guest/kernel/config, so a change to
# the fragment or the kernel version shows up as a diff of that file.
# docs/runbook/guest-kernel.md covers prerequisites, bumps and LTS moves.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
# shellcheck source=guest/kernel/kernel.env
. "$HERE/kernel.env"

FRAGMENT=$HERE/hangar.fragment
COMMITTED=$HERE/config
WORK=${HANGAR_KERNEL_WORKDIR:-$REPO/.cache/guest-kernel}
OUT=${HANGAR_KERNEL_OUT:-$REPO/bin/guest}
JOBS=${JOBS:-$(nproc)}

CONFIG_ONLY=0
UPDATE=0
for arg in "$@"; do
	case $arg in
	--config-only) CONFIG_ONLY=1 ;;
	--update-config) UPDATE=1 ;;
	*) echo "usage: $0 [--config-only] [--update-config]" >&2; exit 2 ;;
	esac
done

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

for cmd in curl sha256sum tar xz make gcc flex bison bc perl; do
	command -v "$cmd" >/dev/null || die "$cmd not found (prerequisites: docs/runbook/guest-kernel.md)"
done

TARBALL=$WORK/linux-$HANGAR_KERNEL_VERSION.tar.xz
SRC=$WORK/linux-$HANGAR_KERNEL_VERSION
BUILD=$WORK/build
mkdir -p "$WORK"

if [ ! -f "$TARBALL" ]; then
	log "download: $HANGAR_KERNEL_URL"
	curl -fsSL -o "$TARBALL.part" "$HANGAR_KERNEL_URL"
	mv "$TARBALL.part" "$TARBALL"
fi
# Verified on every run, so a corrupt or swapped cache cannot slip through.
if ! echo "$HANGAR_KERNEL_SHA256  $TARBALL" | sha256sum --check --status; then
	rm -f "$TARBALL"
	die "sha256 mismatch for $TARBALL (deleted); kernel.env and kernel.org disagree"
fi

# The source tree is only ever read (the build uses O=), so one pristine
# extraction is reused; extracting to a temporary name keeps a partial one out.
if [ ! -d "$SRC" ]; then
	log "extract: $TARBALL"
	rm -rf "$SRC.part"
	mkdir "$SRC.part"
	tar -xJf "$TARBALL" -C "$SRC.part" --strip-components=1
	mv "$SRC.part" "$SRC"
fi

rm -rf "$BUILD"
mkdir -p "$BUILD"
# ARCH=x86_64: allnoconfig starts from a 64-bit kernel and sets every bool it
# may to n, 64-bit hardening included, so the fragment names everything the
# guest relies on. guest/kernel/config records the outcome.
KARCH=x86_64
# Kconfig asks the Rust compiler what it supports (CONFIG_RUSTC_HAS_*) when
# one is on PATH, so a host with rustc would resolve another config. The
# guest has no Rust, so every make here is given commands that fail
# instead: Kconfig then sees no Rust toolchain, whatever the host has. The
# vmlinux make gets them too, or it would run Kconfig again with rustc.
KMAKE_NO_RUST=(RUSTC=false BINDGEN=false)
kmake() { make -s -C "$SRC" O="$BUILD" ARCH=$KARCH "${KMAKE_NO_RUST[@]}" "$@"; }

kmake allnoconfig
"$SRC/scripts/kconfig/merge_config.sh" -m -O "$BUILD" "$BUILD/.config" "$FRAGMENT" >/dev/null
kmake olddefconfig

# Kconfig silently drops a symbol whose dependencies are unmet or that no
# longer exists, so every fragment line is checked against the result.
awk '
	NR == FNR {
		if ($0 ~ /^CONFIG_/) { k = $0; sub(/=.*/, "", k); got[k] = substr($0, length(k) + 2) }
		next
	}
	/^[[:space:]]*(#|$)/ { next }
	!/^CONFIG_[A-Za-z0-9_]+=/ { printf "hangar.fragment:%d: cannot parse: %s\n", FNR, $0; bad = 1; next }
	{
		k = $0; sub(/=.*/, "", k); want = substr($0, length(k) + 2)
		if (want == "n") {
			if (k in got) { printf "hangar.fragment:%d: %s=n but resolved to %s\n", FNR, k, got[k]; bad = 1 }
		} else if (!(k in got)) {
			printf "hangar.fragment:%d: %s=%s was dropped by Kconfig\n", FNR, k, want; bad = 1
		} else if (got[k] != want) {
			printf "hangar.fragment:%d: %s=%s resolved to %s\n", FNR, k, want, got[k]; bad = 1
		}
	}
	END { exit bad }
' "$BUILD/.config" "$FRAGMENT" >&2 || die "the resolved config does not honour hangar.fragment"

# Lines that only identify the toolchain are left out of the comparison; what
# the toolchain can do (CONFIG_CC_HAS_* and friends) stays in, since it changes
# what gets built.
normalize() {
	grep -vE '^CONFIG_(CC_VERSION_TEXT|(GCC|CLANG|AS|LD|LLD|RUSTC|RUSTC_LLVM|PAHOLE)_VERSION)=' "$1"
}
if [ "$UPDATE" = 1 ]; then
	normalize "$BUILD/.config" >"$COMMITTED"
	log "updated: ${COMMITTED#"$REPO"/}"
elif ! normalize "$BUILD/.config" | diff -u --label committed --label resolved "$COMMITTED" - >&2; then
	die "resolved config differs from ${COMMITTED#"$REPO"/}; review the diff, then re-run with --update-config and commit it"
fi

[ "$CONFIG_ONLY" = 1 ] && exit 0

# Fixed identity strings keep the kernel version banner the same on every
# build host. The timestamp is the release's own (tar keeps file mtimes).
export KBUILD_BUILD_USER=hangar KBUILD_BUILD_HOST=hangar KBUILD_BUILD_VERSION=1
KBUILD_BUILD_TIMESTAMP=$(date -u -d "@$(stat -c %Y "$SRC/Makefile")" '+%Y-%m-%d %H:%M:%S UTC')
export KBUILD_BUILD_TIMESTAMP

log "build: linux $HANGAR_KERNEL_VERSION, -j$JOBS"
start=$(date +%s)
make -C "$SRC" O="$BUILD" ARCH=$KARCH "${KMAKE_NO_RUST[@]}" -j"$JOBS" vmlinux >"$BUILD/build.log" 2>&1 ||
	{ tail -n 40 "$BUILD/build.log" >&2; die "kernel build failed (full log: $BUILD/build.log)"; }

mkdir -p "$OUT"
cp "$BUILD/vmlinux" "$OUT/vmlinux"
cp "$BUILD/.config" "$OUT/config"
log "built in $(($(date +%s) - start))s"
(cd "$OUT" && sha256sum vmlinux && ls -l vmlinux) >&2
