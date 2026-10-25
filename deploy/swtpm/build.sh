#!/usr/bin/env bash
# Builds the swtpm bundle a VM's vTPM runs: swtpm, musl's loader and every
# shared library swtpm needs, taken from the Alpine packages swtpm.env
# pins, each checked against its sha256, as a reproducible tar in
# bin/host/swtpm.tar. A host unpacks it into a directory of its own, which
# a VM's unit binds read-only; its root is otherwise empty and holds no
# loader, so swtpm runs under the bundle's, which is static:
#
#   DIR/lib/ld-musl-x86_64.so.1 --library-path DIR/lib DIR/bin/swtpm ARGS...
#
# The bundle holds nothing else of the packages: no configuration, no
# swtpm_setup, no data. Its file VERSION says which swtpm it is.
#
# It needs Go, tar and sha256sum, and no C toolchain: the packages are
# Alpine's builds, downloaded as the converter's are (tools/converterdeps,
# docs/runbook/swtpm.md). SOURCE_DATE_EPOCH sets every entry's mtime;
# it defaults to the time of the last commit.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
# shellcheck source=deploy/swtpm/swtpm.env
. "$HERE/swtpm.env"
# shellcheck source=deploy/host/versions.env
. "$REPO/deploy/host/versions.env"

WORK=${HANGAR_SWTPM_WORKDIR:-$REPO/.cache/swtpm}
OUT=${HANGAR_SWTPM_OUT:-$REPO/bin/host}

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

for cmd in sha256sum tar go; do
	command -v "$cmd" >/dev/null || die "$cmd not found"
done

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git -C "$REPO" log -1 --format=%ct)
fi
[[ $SOURCE_DATE_EPOCH =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be seconds since the epoch"

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$WORK" "$STAGE/pkgs"
ROOT=$STAGE/swtpm
mkdir -m 0755 "$ROOT"

# Download what the cache lacks, each checked; then check every package on
# every run, so a corrupt or swapped cache cannot slip through.
(cd "$REPO" && go run ./tools/converterdeps fetch -prefix HANGAR_SWTPM -env "$HERE/swtpm.env" -dir "$WORK") ||
	die "the packages could not be downloaded (docs/runbook/swtpm.md)"
while read -r pkg sum; do
	[ -n "$pkg" ] || continue
	[[ $pkg =~ ^(main|community)/[A-Za-z0-9._+-]+$ ]] || die "swtpm.env: $pkg is not REPO/NAME-VERSION"
	[[ $sum =~ ^[0-9a-f]{64}$ ]] || die "swtpm.env: $pkg has no sha256"
	file=$WORK/${pkg#*/}.apk
	if ! echo "$sum  $file" | sha256sum --check --status; then
		rm -f "$file"
		die "sha256 mismatch for $file (deleted); swtpm.env and the download disagree"
	fi
	tar -xzf "$file" -C "$STAGE/pkgs" --warning=no-unknown-keyword --exclude='.PKGINFO' --exclude='.SIGN.*' \
		--exclude='.pre-*' --exclude='.post-*' --exclude='.trigger'
done <<<"$HANGAR_SWTPM_PACKAGES"

grep -qE "^community/swtpm-$HANGAR_SWTPM_VERSION " <<<"$HANGAR_SWTPM_PACKAGES" ||
	die "swtpm.env pins no swtpm-$HANGAR_SWTPM_VERSION, the one deploy/host/versions.env names; bump both"

# swtpm, musl's loader, and the shared libraries, flat in lib/: swtpm's
# own (usr/lib/swtpm, its RUNPATH, which the loader is told instead) too.
P=$STAGE/pkgs
[ -x "$P/usr/bin/swtpm" ] || die "no usr/bin/swtpm in the packages"
install -d -m 0755 "$ROOT/bin" "$ROOT/lib"
install -m 0755 "$P/usr/bin/swtpm" "$ROOT/bin/swtpm"
cp -a "$P/lib/ld-musl-x86_64.so.1" "$P/lib/libc.musl-x86_64.so.1" "$ROOT/lib/"
find "$P/usr/lib" "$P/usr/lib/swtpm" -maxdepth 1 \( -type f -o -type l \) -name 'lib*.so.*' -exec cp -a {} "$ROOT/lib/" \;
printf 'hangar-swtpm swtpm-%s\n' "$HANGAR_SWTPM_VERSION" >"$ROOT/VERSION"
chmod -R u+rwX,go+rX,go-w "$ROOT"
chmod 0644 "$ROOT/VERSION"
# It runs, its libraries all there, under its own loader and no other
# library path on this host.
"$ROOT/lib/ld-musl-x86_64.so.1" --library-path "$ROOT/lib" "$ROOT/bin/swtpm" --version | head -n 1 >&2 ||
	die "swtpm does not run with the libraries the packages hold"

mkdir -p "$OUT"
tar --sort=name --mtime=@"$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu \
	-C "$STAGE" -cf "$OUT/swtpm.tar.part" swtpm
mv "$OUT/swtpm.tar.part" "$OUT/swtpm.tar"
(cd "$OUT" && sha256sum swtpm.tar && ls -l swtpm.tar) >&2
