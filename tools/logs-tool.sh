#!/usr/bin/env bash
# logs-tool.sh loki|alloy ARGS... runs the Loki or Alloy a host runs,
# downloading it into .cache/ on first use, from the pins in
# deploy/host/versions.env, so the configuration the logs step writes is
# checked by the very version that loads it. The pin is linux-amd64's alone,
# as a host is.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=deploy/host/versions.env
. "$root/deploy/host/versions.env"

case ${1:-} in
loki) version=$HANGAR_LOKI_VERSION url=$HANGAR_LOKI_URL sha256=$HANGAR_LOKI_SHA256 bin_sha256=$HANGAR_LOKI_BIN_SHA256 ;;
alloy) version=$HANGAR_ALLOY_VERSION url=$HANGAR_ALLOY_URL sha256=$HANGAR_ALLOY_SHA256 bin_sha256=$HANGAR_ALLOY_BIN_SHA256 ;;
*) echo "usage: logs-tool.sh loki|alloy ARGS..." >&2; exit 2 ;;
esac
name=$1
shift
[ "$(uname -s)-$(uname -m)" = Linux-x86_64 ] ||
	{ echo "logs-tool.sh: $name is pinned for linux-amd64 only, not $(uname -s)-$(uname -m)" >&2; exit 1; }

bin=$root/.cache/$name-$version/$name

if ! { [ -x "$bin" ] && echo "$bin_sha256  $bin" | sha256sum -c --quiet - >/dev/null 2>&1; }; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL -o "$tmp/a.zip" "$url"
	echo "$sha256  $tmp/a.zip" | sha256sum -c --quiet -
	python3 -m zipfile -e "$tmp/a.zip" "$tmp/x"
	echo "$bin_sha256  $tmp/x/$name-linux-amd64" | sha256sum -c --quiet -
	mkdir -p "$(dirname "$bin")"
	install -m 0755 "$tmp/x/$name-linux-amd64" "$bin"
	# exec runs no EXIT trap.
	rm -rf "$tmp"
	trap - EXIT
fi

exec "$bin" "$@"
