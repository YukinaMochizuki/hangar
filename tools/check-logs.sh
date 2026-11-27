#!/usr/bin/env bash
# Checks the platform's logs as the logs step installs them
# (deploy/host/steps/92-logs.sh): the Loki and Alloy configurations it
# writes, rendered from deploy/host/host.env.example, with the pinned Loki
# and Alloy (tools/logs-tool.sh); that Alloy labels lines with nothing but
# the unit and the node; and that the dashboards are JSON whose every query
# reads Loki or Prometheus by the uids the step provisions. Written as the
# step writes them, the files must pass the step's own verify check of them.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tool=$repo/tools/logs-tool.sh

fail() {
	echo "check-logs.sh: $*" >&2
	exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# A subshell: the step's names stay out of this script.
(
	# shellcheck source=deploy/host/lib.sh
	. "$repo/deploy/host/lib.sh"
	# shellcheck source=deploy/host/versions.env
	. "$repo/deploy/host/versions.env"
	# shellcheck source=deploy/host/host.env.example
	. "$repo/deploy/host/host.env.example"
	LOCAL_ENV=$work/local.env
	printf 'HANGAR_AUTH_MODE=oidc\n' >"$LOCAL_ENV"
	# shellcheck source=deploy/host/steps/92-logs.sh
	. "$repo/deploy/host/steps/92-logs.sh"
	logs_loki_config >"$work/loki.yml"
	logs_alloy_config >"$work/config.alloy"
	# The step's own check before it writes them, with the pinned tools.
	mkdir "$work/bin"
	for b in loki alloy; do
		printf '#!/bin/sh\nexec %q %s "$@"\n' "$tool" "$b" >"$work/bin/$b"
		chmod 755 "$work/bin/$b"
	done
	MON_BIN_DIR=$work/bin
	problem=$(logs_config_problem)
	[ -z "$problem" ] || fail "the logs step refuses its own configuration: $problem"
	# Written as the step writes them, with put_file, the files must pass
	# verify's check of them. The files are the test's, not root's, as
	# verify expects, so stat reports root as their owner.
	LOGS_ETC=$work/as-written LOGS_LOKI_CONFIG=$work/as-written/loki.yml
	LOGS_ALLOY_CONFIG=$work/as-written/config.alloy LOGS_GRAFANA_CONFIG=$work/as-written/grafana.ini
	LOGS_GRAFANA_PROVISIONING=$work/as-written/grafana LOGS_GRAFANA_DASHBOARDS=$work/as-written/grafana/dashboards/hangar
	DRY_RUN=0 CHANGES=0
	# shellcheck disable=SC2086 # arg is one word, or none
	while read -r path mode gen arg; do put_file "$path" "$mode" "$("$gen" $arg)" 2>/dev/null; done < <(logs_files)
	stat() { printf '%s root root\n' "$(command stat -c %a "${@: -1}")"; }
	logs_files_ok || fail "verify fails the files the step wrote: $(find "$work/as-written" -type f | paste -sd' ' -)"
	[ -z "$(logs_stray_dashboards)" ] || fail "verify finds stray dashboards among the step's own"
)

# No tenant in a label: Alloy sets the unit and the node, nothing else.
labels=$(grep -oE 'target_label *= *"[^"]+"|labels *= *\{[^}]*\}' "$work/config.alloy")
[ "$labels" = "$(printf '%s\n' 'target_label  = "unit"' 'labels        = {node = "node-01"}')" ] ||
	fail "Alloy sets labels other than unit and node: $labels"
! grep -q 'hangar-vm' "$work/config.alloy" || fail "Alloy ships a VM unit's journal"

for d in "$repo"/deploy/host/monitoring/grafana/*.json; do
	python3 - "$d" <<'EOF' || fail "dashboard $d"
import json, sys
d = json.load(open(sys.argv[1]))
name = sys.argv[1].rsplit("/", 1)[-1][: -len(".json")]
assert d["uid"] == name, f"uid {d['uid']} is not the file's name {name}"
for p in d["panels"]:
    ds = p["datasource"]
    assert ds["uid"] in ("loki", "prometheus"), f"panel {p['title']} reads {ds}"
    assert p["targets"], f"panel {p['title']} has no query"
EOF
done
echo "logs configuration and dashboards: ok"
