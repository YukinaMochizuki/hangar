#!/usr/bin/env bash
# Checks the host's monitoring as the monitoring step installs it
# (deploy/host/steps/90-monitoring.sh), with the pinned promtool and amtool
# (tools/prom-tool.sh): the alerting rules and their tests, and the
# Prometheus and Alertmanager configurations the step writes, rendered from
# deploy/host/host.env.example with the front door on and off, and with the
# platform's services serving /metrics and not.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tool=$root/tools/prom-tool.sh
mon=$root/deploy/host/monitoring

"$tool" promtool check rules "$mon"/rules/*.yml
"$tool" promtool test rules "$mon"/tests/*.yml

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# render DIR FRONT_DOOR METRICS writes the step's configurations into DIR,
# from the example host env file and a local env file with the front door
# on (1) or off (0), and the services' binaries taking --metrics-listen (1)
# or from before it (0): stand-ins that answer -h as each would. A
# subshell: the step's names stay out of this script.
render() (
	dir=$1
	mkdir -p "$dir/rules" "$dir/bin"
	flag=name
	[ "$3" = 0 ] || flag='metrics-listen'
	for b in hangar-apiserver hangar-node hangar-edge; do
		printf '#!/bin/sh\necho "Usage of %s:" >&2\necho "  -%s string" >&2\n' "$dir/bin/$b" "$flag" >"$dir/bin/$b"
		chmod 755 "$dir/bin/$b"
	done
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
	SERVICES_INSTALLED_BIN=$dir/bin HANGAR_RELEASE_DIR=
	mon_rules_into "$dir/rules"
	mon_prometheus_config | sed "s|$MON_RULES_DIR/\\*.yml|$dir/rules/*.yml|" >"$dir/prometheus.yml"
	mon_alertmanager_config >"$dir/alertmanager.yml"
	mon_blackbox_config >"$dir/blackbox.yml"
	# The step's own check before it writes them, with the pinned tools.
	for b in promtool amtool; do
		printf '#!/bin/sh\nexec %q %s "$@"\n' "$tool" "$b" >"$dir/bin/$b"
		chmod 755 "$dir/bin/$b"
	done
	MON_BIN_DIR=$dir/bin
	problem=$(mon_config_problem)
	[ -z "$problem" ] || { echo "check-monitoring.sh: the monitoring step refuses its own configuration: $problem" >&2; exit 1; }
)

fail() {
	echo "check-monitoring.sh: $*" >&2
	exit 1
}

for front_door in 1 0; do
	for metrics in 1 0; do
		dir=$work/front-door-$front_door-metrics-$metrics
		render "$dir" "$front_door" "$metrics"
		echo "front door $([ "$front_door" = 1 ] && echo on || echo off), services' metrics $([ "$metrics" = 1 ] && echo on || echo off):"
		"$tool" promtool check config "$dir/prometheus.yml"
		"$tool" amtool check-config "$dir/alertmanager.yml"
	done
done
# With the front door on, and only then, the probes are scraped.
grep -q 'https://api.example.com/healthz' "$work/front-door-1-metrics-1/prometheus.yml" ||
	fail "the front door's probes are not scraped"
! grep -q '/probe' "$work/front-door-0-metrics-1/prometheus.yml" ||
	fail "probes are scraped with the front door off"
# Each service whose binary takes --metrics-listen is scraped, the edge
# only while it runs (the front door on); none whose binary does not.
for job in hangar-apiserver:127.0.0.1:9101 hangar-node:127.0.0.1:9102 hangar-edge:127.0.0.1:9103; do
	grep -A2 -x "  - job_name: ${job%%:*}" "$work/front-door-1-metrics-1/prometheus.yml" | grep -qF "[\"${job#*:}\"]" ||
		fail "${job%%:*} is not scraped at ${job#*:}"
done
grep -qx '  - job_name: hangar-node' "$work/front-door-0-metrics-1/prometheus.yml" ||
	fail "hangar-node is not scraped with the front door off"
! grep -qx '  - job_name: hangar-edge' "$work/front-door-0-metrics-1/prometheus.yml" ||
	fail "hangar-edge is scraped with the front door off"
! grep -qE '^  - job_name: hangar-(apiserver|node|edge)$' "$work/front-door-1-metrics-0/prometheus.yml" ||
	fail "a service whose binary has no --metrics-listen is scraped"
# Nothing is relabeled onto the services' metrics: no tenant in a label.
sed -n '/^  - job_name: hangar-/,/^  - job_name: [^h]/p' "$work/front-door-1-metrics-1/prometheus.yml" | grep -q relabel &&
	fail "a service's job relabels"
true
