#!/usr/bin/env bash
# Builds the converter initramfs that converter VMs boot: hangar-init as
# /init, which a converter's boot disk makes the converter, and qemu-img
# with the shared libraries it needs, taken from the Alpine packages
# converter.env pins, each checked against its sha256, as an uncompressed,
# reproducible newc cpio in bin/guest/converter.cpio. The platform's
# initramfs carries none of it (docs/decisions/custom-images.md).
#
# It needs Go, curl, tar and sha256sum, and no C toolchain: the packages
# are Alpine's builds. SOURCE_DATE_EPOCH sets every entry's mtime; it
# defaults to the time of the last commit. docs/runbook/converter.md says
# how to bump the packages.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
# shellcheck source=guest/converter/converter.env
. "$HERE/converter.env"
# shellcheck source=deploy/host/versions.env
. "$REPO/deploy/host/versions.env"

WORK=${HANGAR_CONVERTER_WORKDIR:-$REPO/.cache/converter}
OUT=${HANGAR_CONVERTER_OUT:-$REPO/bin/guest}

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

for cmd in curl sha256sum tar go; do
	command -v "$cmd" >/dev/null || die "$cmd not found"
done

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git -C "$REPO" log -1 --format=%ct)
fi
[[ $SOURCE_DATE_EPOCH =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be seconds since the epoch"

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$WORK" "$STAGE/pkgs"
ROOT=$STAGE/root
mkdir -m 0755 "$ROOT"

# Download what the cache lacks; check every package on every run, so a
# corrupt or swapped cache cannot slip through.
while read -r pkg sum; do
	[ -n "$pkg" ] || continue
	[[ $pkg =~ ^(main|community)/[A-Za-z0-9._+-]+$ ]] || die "converter.env: $pkg is not REPO/NAME-VERSION"
	[[ $sum =~ ^[0-9a-f]{64}$ ]] || die "converter.env: $pkg has no sha256"
	file=$WORK/${pkg#*/}.apk
	if [ ! -f "$file" ]; then
		url="$HANGAR_CONVERTER_MIRROR/$HANGAR_CONVERTER_ALPINE/${pkg%%/*}/x86_64/${pkg#*/}.apk"
		log "download: $url"
		curl -fsSL -o "$file.part" "$url" || die "$url: gone from the mirror? bump converter.env (docs/runbook/converter.md)"
		mv "$file.part" "$file"
	fi
	if ! echo "$sum  $file" | sha256sum --check --status; then
		rm -f "$file"
		die "sha256 mismatch for $file (deleted); converter.env and the mirror disagree"
	fi
	# An .apk is gzip streams concatenated: its signature, its metadata
	# and its files, one tar.
	tar -xzf "$file" -C "$STAGE/pkgs" --warning=no-unknown-keyword --exclude='.PKGINFO' --exclude='.SIGN.*' \
		--exclude='.pre-*' --exclude='.post-*' --exclude='.trigger'
done <<<"$HANGAR_CONVERTER_PACKAGES"

grep -qE "^community/qemu-img-$HANGAR_CONVERTER_QEMU_IMG " <<<"$HANGAR_CONVERTER_PACKAGES" ||
	die "converter.env pins no qemu-img-$HANGAR_CONVERTER_QEMU_IMG, the one deploy/host/versions.env names; bump both"

# qemu-img, musl's loader, and the shared libraries; none of the packages'
# other programs, data or configuration.
P=$STAGE/pkgs
[ -x "$P/usr/bin/qemu-img" ] || die "no usr/bin/qemu-img in the packages"
install -d -m 0755 "$ROOT/usr" "$ROOT/usr/bin" "$ROOT/usr/lib" "$ROOT/lib"
install -m 0755 "$P/usr/bin/qemu-img" "$ROOT/usr/bin/qemu-img"
cp -a "$P/lib/ld-musl-x86_64.so.1" "$P/lib/libc.musl-x86_64.so.1" "$ROOT/lib/"
find "$P/usr/lib" -maxdepth 1 \( -type f -o -type l \) -name 'lib*.so*' -exec cp -a {} "$ROOT/usr/lib/" \;
chmod -R u+rwX,go+rX,go-w "$ROOT"
# It runs, its libraries all there, under its own loader on this host.
"$ROOT/lib/ld-musl-x86_64.so.1" --library-path "$ROOT/lib:$ROOT/usr/lib" "$ROOT/usr/bin/qemu-img" --version | head -n 1 >&2 ||
	die "qemu-img does not run with the libraries the packages hold"

# Static, as the platform initramfs's: the same hangar-init, whose boot
# disk makes it the converter.
(cd "$REPO/cmd/hangar-init" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
	-ldflags=-buildid= -o "$STAGE/init" .)
install -m 0755 "$STAGE/init" "$ROOT/init"
# Mount points only: hangar-init mounts devtmpfs on /dev.
mkdir -m 0755 "$ROOT/dev" "$ROOT/proc" "$ROOT/sys" "$ROOT/newroot"
# What it carries, a line a host's verify.sh finds in the archive.
mkdir -m 0755 "$ROOT/etc"
printf 'hangar-converter qemu-img-%s\n' "$HANGAR_CONVERTER_QEMU_IMG" >"$ROOT/etc/hangar-converter"
chmod 0644 "$ROOT/etc/hangar-converter"

mkdir -p "$OUT"
(cd "$REPO" && go run ./tools/newc -root "$ROOT" -mtime "$SOURCE_DATE_EPOCH" -o "$OUT/converter.cpio.part")
mv "$OUT/converter.cpio.part" "$OUT/converter.cpio"
(cd "$OUT" && sha256sum converter.cpio && ls -l converter.cpio) >&2
