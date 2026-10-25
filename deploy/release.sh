#!/usr/bin/env bash
# Builds a release from the checked-out commit: what a host runs, and the CLI
# for the platforms people run it on (docs/runbook/release.md).
#
#   release.sh [--allow-dirty] [OUT]
#
# It writes two directories:
#
#   OUT/host  what a host runs: hangar-apiserver, hangar-node, hangar-edge and
#             hangar (static, linux/amd64), the guest kernel, the platform
#             initramfs and the converter initramfs (converter.cpio), the
#             swtpm bundle TPMs run from (swtpm.tar, deploy/swtpm), the
#             console's files as console.tar, deploy/ itself as
#             deploy.tar, VERSION, and SHA256SUMS over all of them. The
#             `release` step of deploy/host installs from a directory like it
#             (docs/runbook/host-bootstrap.md); a testbed takes this one.
#   OUT/dist  what .github/workflows/release.yml publishes for a tag:
#             OUT/host packed as hangar-host_TAG_linux_amd64.tar.gz, the CLI as
#             hangar_TAG_OS_ARCH.tar.gz for each platform in CLI_PLATFORMS
#             (hangar_TAG_windows_ARCH.zip, with hangar.exe, for Windows),
#             install.sh and install.ps1 for the CLI (deploy/cli/install.sh.in
#             and deploy/cli/install.ps1.in), VERSION, and SHA256SUMS over
#             all of them. VERSION is the host's too: a host checks the
#             tar's attestation for that commit before it unpacks it
#             (deploy/host/fetch-release.sh).
#
# HANGAR_RELEASE_TAG is the tag the release is for, vMAJOR.MINOR.PATCH with
# an optional -rc.N. It names the files of OUT/dist, is baked into install.sh
# and install.ps1, and the console shows the install lines for it. Unset,
# the release is a test build named dev-<first 12 of the revision>, whose
# console shows no install line.
#
# OUT defaults to bin/release/hangar-<first 12 of the revision>. The build is
# reproducible: the same commit and tag give the same SHA256SUMS on the same
# toolchain. A tree with uncommitted changes is refused unless --allow-dirty,
# and its VERSION then ends in -dirty, as the binaries' own version does.
#
# HANGAR_KERNEL_CACHE=DIR reuses a prebuilt kernel from DIR/<key>, keyed by
# guest/kernel/cache.sh, and files a kernel it builds there. The cached
# config is checked against guest/kernel/config before it is used.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)

# The platforms the CLI is released for.
CLI_PLATFORMS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)
# The same pattern as release.yml, fetch-release.sh, install.sh.in and
# install.ps1.in.
TAG_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$'

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }
usage() { echo "usage: $0 [--allow-dirty] [OUT]" >&2; exit 2; }

allow_dirty=0
out=
while [ $# -gt 0 ]; do
	case $1 in
	--allow-dirty) allow_dirty=1; shift ;;
	-*) usage ;;
	*) [ -z "$out" ] || usage; out=$1; shift ;;
	esac
done

if [ -n "${HANGAR_KERNEL_CACHE:-}" ]; then
	HANGAR_KERNEL_CACHE=$(realpath -m -- "$HANGAR_KERNEL_CACHE")
fi
cd "$REPO"
version=$(git rev-parse HEAD)
if [ -n "$(git status --porcelain)" ]; then
	[ "$allow_dirty" = 1 ] || die "the tree has uncommitted changes; commit them, or pass --allow-dirty for a test build"
	version=$version-dirty
fi
short=${version:0:12}
case $version in *-dirty) short=$short-dirty ;; esac
tag=${HANGAR_RELEASE_TAG:-}
console_tag=
if [ -n "$tag" ]; then
	[[ $tag =~ $TAG_RE ]] || die "HANGAR_RELEASE_TAG=$tag is not a release tag (vMAJOR.MINOR.PATCH, or that with -rc.N)"
	console_tag=$tag
else
	tag=dev-$short
fi
out=${out:-$REPO/bin/release/hangar-$short}
# OUT is replaced, so it must be a release this script wrote before.
if [ -e "$out" ] && ! { [ -f "$out/host/SHA256SUMS" ] && [ -f "$out/dist/SHA256SUMS" ]; } &&
	! { [ -f "$out/VERSION" ] && [ -f "$out/SHA256SUMS" ]; }; then
	die "$out exists and is not a release (no host/SHA256SUMS and dist/SHA256SUMS); refusing to replace it"
fi

# Everything is staged first, so a failed build never leaves a directory
# that looks like a release.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
host=$stage/host
dist=$stage/dist
mkdir "$host" "$dist"

# pack DIR OUT NAME... writes NAME... of DIR, in that order, as the gzipped
# tar OUT, with no time, owner or umask of this machine in it, so the same
# files pack the same bytes.
pack() {
	local dir=$1 to=$2
	shift 2
	tar --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w \
		-C "$dir" --no-recursion -cf - -- "$@" | gzip -9n >"$to"
}

log "release $tag ($version)"
for cmd in hangar-apiserver hangar-node hangar-edge hangar; do
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags=-buildid= \
		-o "$host/$cmd" "./cmd/$cmd"
done
# The console, built from the lockfile, packed with no time, owner or umask
# of this machine in it, so the same commit packs the same bytes.
pnpm install --frozen-lockfile
VITE_HANGAR_RELEASE=$console_tag pnpm --dir web build
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w \
	-C web/dist -cf "$host/console.tar" .
# deploy/ the same way, its files as git knows them (an untracked one too in a
# dirty test build), so nothing ignored ships, sorted here because tar keeps
# the order of a file list.
git -C deploy ls-files -z --cached --others --exclude-standard |
	while IFS= read -r -d '' f; do [ ! -e "deploy/$f" ] || printf '%s\0' "$f"; done |
	LC_ALL=C sort -z >"$stage/deploy.list"
tar --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w \
	-C deploy --null --no-recursion -T "$stage/deploy.list" -cf "$host/deploy.tar"
rm "$stage/deploy.list"
HANGAR_INITRAMFS_OUT=$stage/guest guest/initramfs/build.sh
HANGAR_CONVERTER_OUT=$stage/guest guest/converter/build.sh
HANGAR_SWTPM_OUT=$host deploy/swtpm/build.sh
if [ -z "${HANGAR_KERNEL_CACHE:-}" ]; then
	HANGAR_KERNEL_OUT=$stage/guest guest/kernel/build.sh
else
	key=$(guest/kernel/cache.sh key)
	entry=$HANGAR_KERNEL_CACHE/$key
	if [ -e "$entry" ]; then
		guest/kernel/cache.sh check "$entry"
		cp "$entry/vmlinux" "$entry/config" "$stage/guest/"
		log "kernel: cache hit $key"
	else
		HANGAR_KERNEL_OUT=$stage/guest guest/kernel/build.sh
		# Filed under a temporary name first, so a reader never sees half an entry.
		mkdir -p "$HANGAR_KERNEL_CACHE"
		part=$(mktemp -d "$HANGAR_KERNEL_CACHE/.$key.XXXXXX")
		cp "$stage/guest/vmlinux" "$stage/guest/config" "$part/"
		chmod 0755 "$part"
		mv -T "$part" "$entry" 2>/dev/null || rm -rf "$part"
		log "kernel: built, cached under $key"
	fi
fi
mv "$stage/guest/vmlinux" "$stage/guest/initramfs.cpio" "$stage/guest/converter.cpio" "$host/"
rm -rf "$stage/guest"
printf '%s\n' "$version" >"$host/VERSION"
(cd "$host" && sha256sum -- * >SHA256SUMS.part && mv SHA256SUMS.part SHA256SUMS)

# The host's files, flat, SHA256SUMS last, so unpacking the tar gives the
# directory OUT/host is (deploy/host/fetch-release.sh).
host_files=()
while IFS= read -r f; do
	[ "$f" = SHA256SUMS ] || host_files+=("$f")
done < <(cd "$host" && LC_ALL=C ls)
pack "$host" "$dist/hangar-host_${tag}_linux_amd64.tar.gz" "${host_files[@]}" SHA256SUMS

# The CLI for each platform, the binary alone at the top of its archive: a
# tar, or for Windows, whose own tools unpack zip, a zip as reproducible.
for p in "${CLI_PLATFORMS[@]}"; do
	os=${p%/*} arch=${p#*/}
	bin=hangar
	[ "$os" != windows ] || bin=hangar.exe
	mkdir "$stage/cli-$os-$arch"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags=-buildid= \
		-o "$stage/cli-$os-$arch/$bin" ./cmd/hangar
	if [ "$os" = windows ]; then
		go run ./tools/reprozip -C "$stage/cli-$os-$arch" -o "$dist/hangar_${tag}_${os}_${arch}.zip" "$bin"
	else
		pack "$stage/cli-$os-$arch" "$dist/hangar_${tag}_${os}_${arch}.tar.gz" "$bin"
	fi
	rm -rf "$stage/cli-$os-$arch"
done
cp "$host/VERSION" "$dist/VERSION"
sed "s/@TAG@/$tag/g" deploy/cli/install.sh.in >"$dist/install.sh"
chmod 0755 "$dist/install.sh"
sed "s/@TAG@/$tag/g" deploy/cli/install.ps1.in >"$dist/install.ps1"
(cd "$dist" && sha256sum -- * >SHA256SUMS.part && mv SHA256SUMS.part SHA256SUMS)

mkdir -p "$(dirname "$out")"
rm -rf "$out.part"
cp -r "$stage" "$out.part"
chmod 0755 "$out.part"
rm -rf "$out"
mv "$out.part" "$out"
log "wrote $out"
cat "$out/dist/SHA256SUMS" >&2
# The digest a host's hangar-fetch-release is given (docs/runbook/release.md).
log "sha256sum SHA256SUMS: $(cd "$out/dist" && sha256sum SHA256SUMS)"
