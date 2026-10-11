#!/usr/bin/env bash
# Runs a pinned shellcheck, downloading it into .cache/ on first use. shellcheck
# is not a Go or npm tool, so the release binary is pinned by checksum here.
set -euo pipefail

version=v0.11.0
case $(uname -m) in
x86_64) arch=x86_64 sha256=8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198 ;;
aarch64) arch=aarch64 sha256=12b331c1d2db6b9eb13cfca64306b1b157a86eb69db83023e261eaa7e7c14588 ;;
*) echo "shellcheck.sh: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
dir=$root/.cache/shellcheck-$version
bin=$dir/shellcheck

if [ ! -x "$bin" ]; then
	tarball=shellcheck-$version.linux.$arch.tar.xz
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL -o "$tmp/$tarball" \
		"https://github.com/koalaman/shellcheck/releases/download/$version/$tarball"
	echo "$sha256  $tmp/$tarball" | sha256sum -c --quiet -
	tar -xJf "$tmp/$tarball" -C "$tmp"
	mkdir -p "$dir"
	install -m 0755 "$tmp/shellcheck-$version/shellcheck" "$bin"
fi

exec "$bin" "$@"
