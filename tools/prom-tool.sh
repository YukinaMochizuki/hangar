#!/usr/bin/env bash
# prom-tool.sh promtool|amtool ARGS... runs the promtool or amtool of the
# Prometheus or Alertmanager a host runs, downloading it into .cache/ on first
# use. Both are pinned in deploy/host/versions.env with the host's binaries,
# so the rules and the configuration are checked by the very version that
# loads them. The pin is linux-amd64's alone, as a host is.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=deploy/host/versions.env
. "$root/deploy/host/versions.env"

case ${1:-} in
promtool)
	version=$HANGAR_PROMETHEUS_VERSION url=$HANGAR_PROMETHEUS_URL sha256=$HANGAR_PROMETHEUS_SHA256
	bin_sha256=$HANGAR_PROMTOOL_BIN_SHA256 dir=prometheus-$version.linux-amd64
	;;
amtool)
	version=$HANGAR_ALERTMANAGER_VERSION url=$HANGAR_ALERTMANAGER_URL sha256=$HANGAR_ALERTMANAGER_SHA256
	bin_sha256=$HANGAR_AMTOOL_BIN_SHA256 dir=alertmanager-$version.linux-amd64
	;;
*) echo "usage: prom-tool.sh promtool|amtool ARGS..." >&2; exit 2 ;;
esac
name=$1
shift
[ "$(uname -s)-$(uname -m)" = Linux-x86_64 ] ||
	{ echo "prom-tool.sh: $name is pinned for linux-amd64 only, not $(uname -s)-$(uname -m)" >&2; exit 1; }

bin=$root/.cache/$name-$version/$name

if ! { [ -x "$bin" ] && echo "$bin_sha256  $bin" | sha256sum -c --quiet - >/dev/null 2>&1; }; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL -o "$tmp/t.tar.gz" "$url"
	echo "$sha256  $tmp/t.tar.gz" | sha256sum -c --quiet -
	tar -xzf "$tmp/t.tar.gz" -C "$tmp" "$dir/$name"
	echo "$bin_sha256  $tmp/$dir/$name" | sha256sum -c --quiet -
	mkdir -p "$(dirname "$bin")"
	install -m 0755 "$tmp/$dir/$name" "$bin"
	# exec runs no EXIT trap.
	rm -rf "$tmp"
	trap - EXIT
fi

exec "$bin" "$@"
