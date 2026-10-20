#!/usr/bin/env bash
# Checks the host's monitoring as the monitoring step installs it
# (deploy/host/steps/90-monitoring.sh), with the pinned promtool and amtool
# (tools/prom-tool.sh): the alerting rules and their tests, and the
# Prometheus and Alertmanager configurations the step writes, rendered from
# deploy/host/host.env.example with the front door on and off.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tool=$root/tools/prom-tool.sh
mon=$root/deploy/host/monitoring

"$tool" promtool check rules "$mon"/rules/*.yml
"$tool" promtool test rules "$mon"/tests/*.yml

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# render DIR FRONT_DOOR writes the step's configurations into DIR, from the
# example host env file and a local env file with the front door on (1) or
# off (0). A subshell: the step's names stay out of this script.
render() (
	dir=$1
	mkdir -p "$dir"
	# shellcheck source=deploy/host/lib.sh
	. "$root/deploy/host/lib.sh"
	# shellcheck source=deploy/host/versions.env
	. "$root/deploy/host/versions.env"
	# shellcheck source=deploy/host/host.env.example
	. "$root/deploy/host/host.env.example"
	LOCAL_ENV=$dir/local.env
	printf 'HANGAR_AUTH_MODE=oidc\n' >"$LOCAL_ENV"
	[ "$2" = 0 ] || printf 'HANGAR_EDGE_DOMAIN=example.net\nHANGAR_CP_DOMAIN=example.com\n' >>"$LOCAL_ENV"
	# shellcheck source=deploy/host/steps/90-monitoring.sh
	. "$root/deploy/host/steps/90-monitoring.sh"
	mon_rules >"$dir/host.yml"
	mon_prometheus_config | sed "s|$MON_RULES_DIR/\\*.yml|$dir/host.yml|" >"$dir/prometheus.yml"
	mon_alertmanager_config >"$dir/alertmanager.yml"
	mon_blackbox_config >"$dir/blackbox.yml"
)

for front_door in 1 0; do
	dir=$work/front-door-$front_door
	render "$dir" "$front_door"
	echo "front door $([ "$front_door" = 1 ] && echo on || echo off):"
	"$tool" promtool check config "$dir/prometheus.yml"
	"$tool" amtool check-config "$dir/alertmanager.yml"
done
# With the front door on, and only then, the probes are scraped.
grep -q 'https://api.example.com/healthz' "$work/front-door-1/prometheus.yml" ||
	{ echo "check-monitoring.sh: the front door's probes are not scraped" >&2; exit 1; }
if grep -q '/probe' "$work/front-door-0/prometheus.yml"; then
	echo "check-monitoring.sh: probes are scraped with the front door off" >&2
	exit 1
fi
