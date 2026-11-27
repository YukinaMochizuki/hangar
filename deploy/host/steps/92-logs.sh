# The platform's logs, kept and shown (docs/decisions/monitoring.md): Loki,
# which keeps 90 days on a dataset of its own; Alloy, which ships to it what
# the platform's services write to journald, hangar-apiserver's access lines
# among it; and Grafana, which shows those logs and Prometheus's series.
# Each runs as a systemd service of its own user and listens on loopback
# alone, reached through an SSH tunnel (docs/runbook/host-bootstrap.md), so
# the firewall stays at 22, 80 and 443. Each is pinned in versions.env. A
# unit, a configuration or a binary changed restarts that service, here and
# nowhere else.

# Set before the monitoring step is sourced, which sources this one at its
# end unless it is already being sourced.
LOGS_SOURCED=1
# The monitoring step's helpers and names: its sandbox, its services'
# common lines, the services' markers, the datasets'.
# shellcheck source=deploy/host/steps/90-monitoring.sh
declare -F mon_hardening >/dev/null || . "$(dirname "${BASH_SOURCE[0]}")/90-monitoring.sh"

# The files of deploy/host/monitoring/grafana this step installs.
LOGS_SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/../monitoring/grafana" && pwd)

# Each component runs as the service hangar-KEY, under the user of that name.
LOGS_COMPONENTS="loki alloy grafana"
LOGS_ETC=/etc/hangar/logs
LOGS_LOKI_CONFIG=$LOGS_ETC/loki.yml
LOGS_ALLOY_CONFIG=$LOGS_ETC/config.alloy
LOGS_GRAFANA_CONFIG=$LOGS_ETC/grafana.ini
LOGS_GRAFANA_PROVISIONING=$LOGS_ETC/grafana
LOGS_GRAFANA_DASHBOARDS=$LOGS_ETC/grafana/dashboards/hangar
# The dashboards of deploy/host/monitoring/grafana this step installs, each
# NAME.json.
LOGS_DASHBOARDS="api-activity"
# Loki's data, on a dataset of its own whose quota (lib.sh) keeps it from
# filling the pool. Loki deletes what is older than the retention.
LOGS_LOKI_DATASET=$HANGAR_ZPOOL/loki
LOGS_LOKI_DATA=/var/lib/hangar/loki
LOGS_LOKI_PROPS=setuid=off,exec=off,devices=off,quota=$LOGS_LOKI_QUOTA
LOGS_LOKI_RETENTION=2160h
LOGS_LOKI_LISTEN=127.0.0.1:3100
LOGS_LOKI_GRPC_LISTEN=127.0.0.1:9095
LOGS_ALLOY_LISTEN=127.0.0.1:12345
LOGS_ALLOY_STATE=/var/lib/hangar-alloy
LOGS_GRAFANA_LISTEN=127.0.0.1:3000
LOGS_GRAFANA_STATE=/var/lib/hangar-grafana
# Grafana is a directory, not one binary: each version its own, the unit's
# the one pinned.
LOGS_GRAFANA_ROOT=/opt/hangar
# The password of Grafana's admin, made once by this step, root's alone;
# Grafana reads it as a credential, and only when it makes its database.
LOGS_GRAFANA_PASSWORD=/etc/hangar/grafana-admin-password
# The units whose journal Alloy ships: the platform's own, never a VM's
# (hangar-vm-*), whose name carries the machine's uid.
LOGS_SHIPPED_UNITS="hangar-apiserver hangar-node hangar-edge"

logs_unit_of() { printf 'hangar-%s\n' "$1"; }

# logs_unit_head DESCRIPTION prints the start of a service's unit.
logs_unit_head() {
	printf '%s\n' '# Written by deploy/host/steps/92-logs.sh.' '[Unit]' "Description=Hangar logs: $1"
}

logs_units() {
	local c
	for c in $LOGS_COMPONENTS; do logs_unit_of "$c"; done
}

# logs_pin KEY VAR prints KEY's VAR (VERSION, URL, SHA256, BIN_SHA256).
logs_pin() {
	local v
	v=HANGAR_${1^^}_$2
	printf '%s\n' "${!v}"
}

logs_grafana_home() { printf '%s/grafana-%s\n' "$LOGS_GRAFANA_ROOT" "$HANGAR_GRAFANA_VERSION"; }

# logs_bin KEY prints the binary KEY's service runs.
logs_bin() {
	case $1 in
	grafana) printf '%s/bin/grafana\n' "$(logs_grafana_home)" ;;
	*) printf '%s/%s\n' "$MON_BIN_DIR" "$1" ;;
	esac
}

logs_component_pinned() {
	local b
	b=$(logs_bin "$1")
	[ -x "$b" ] && echo "$(logs_pin "$1" BIN_SHA256)  $b" | sha256sum -c --quiet - >/dev/null 2>&1
}

# logs_install KEY downloads KEY's archive, checks it and the binary in it
# against versions.env, and installs it: Loki's and Alloy's binary replaced
# whole, Grafana's directory beside the others' until the unit runs it.
logs_install() {
	local key=$1 tmp
	tmp=$(mktemp -d)
	curl -fsSL -o "$tmp/a" "$(logs_pin "$key" URL)"
	if ! echo "$(logs_pin "$key" SHA256)  $tmp/a" | sha256sum -c --quiet -; then
		rm -rf "$tmp"
		die "checksum mismatch for $(logs_pin "$key" URL)"
	fi
	case $key in
	grafana)
		mkdir -p "$LOGS_GRAFANA_ROOT"
		rm -rf "$(logs_grafana_home).hangar-new"
		mkdir "$(logs_grafana_home).hangar-new"
		tar -xzf "$tmp/a" -C "$(logs_grafana_home).hangar-new" --strip-components=1 --no-same-owner
		if ! echo "$(logs_pin "$key" BIN_SHA256)  $(logs_grafana_home).hangar-new/bin/grafana" | sha256sum -c --quiet -; then
			rm -rf "$tmp" "$(logs_grafana_home).hangar-new"
			die "the grafana binary in $(logs_pin "$key" URL) does not match HANGAR_GRAFANA_BIN_SHA256"
		fi
		rm -rf "$(logs_grafana_home)"
		mv "$(logs_grafana_home).hangar-new" "$(logs_grafana_home)"
		;;
	*)
		# The host has python3, not unzip.
		python3 -m zipfile -e "$tmp/a" "$tmp/x"
		if ! echo "$(logs_pin "$key" BIN_SHA256)  $tmp/x/$key-linux-amd64" | sha256sum -c --quiet -; then
			rm -rf "$tmp"
			die "the $key binary in $(logs_pin "$key" URL) does not match HANGAR_${key^^}_BIN_SHA256"
		fi
		install -m 0755 "$tmp/x/$key-linux-amd64" "$MON_BIN_DIR/$key.hangar-new"
		mv -f "$MON_BIN_DIR/$key.hangar-new" "$MON_BIN_DIR/$key"
		;;
	esac
	rm -rf "$tmp"
}

# logs_old_grafanas prints each Grafana directory but the pinned one.
logs_old_grafanas() {
	local d
	for d in "$LOGS_GRAFANA_ROOT"/grafana-*; do
		[ -d "$d" ] && [ "$d" != "$(logs_grafana_home)" ] && printf '%s\n' "$d"
	done
	return 0
}

logs_loki_config() {
	cat <<EOF
# Written by deploy/host/steps/92-logs.sh.
auth_enabled: false
analytics:
  reporting_enabled: false
server:
  http_listen_address: ${LOGS_LOKI_LISTEN%:*}
  http_listen_port: ${LOGS_LOKI_LISTEN##*:}
  grpc_listen_address: ${LOGS_LOKI_GRPC_LISTEN%:*}
  grpc_listen_port: ${LOGS_LOKI_GRPC_LISTEN##*:}
  log_level: warn
# One process, its ring in memory: nothing gossips, so nothing listens
# beyond loopback.
common:
  instance_addr: 127.0.0.1
  path_prefix: $LOGS_LOKI_DATA
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory
  storage:
    filesystem:
      chunks_directory: $LOGS_LOKI_DATA/chunks
      rules_directory: $LOGS_LOKI_DATA/rules
memberlist:
  bind_addr: [127.0.0.1]
schema_config:
  configs:
    - from: 2026-01-01
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: index_
        period: 24h
limits_config:
  retention_period: $LOGS_LOKI_RETENTION
compactor:
  working_directory: $LOGS_LOKI_DATA/compactor
  retention_enabled: true
  delete_request_store: filesystem
EOF
}

# logs_shipped_regex is the units Alloy keeps, as a regex of their journal
# unit names, dots matched literally.
logs_shipped_regex() {
	local u
	for u in $LOGS_SHIPPED_UNITS; do printf '%s[.]service\n' "$u"; done | paste -sd'|' -
}

# Alloy's configuration. The labels it sets are the unit and the node, values
# the platform defines: what a line says of a tenant stays in the line
# (docs/decisions/monitoring.md).
logs_alloy_config() {
	cat <<EOF
// Written by deploy/host/steps/92-logs.sh.
loki.relabel "platform" {
  forward_to = []

  rule {
    source_labels = ["__journal__systemd_unit"]
    regex         = "$(logs_shipped_regex)"
    action        = "keep"
  }

  rule {
    source_labels = ["__journal__systemd_unit"]
    regex         = "(.+)[.]service"
    target_label  = "unit"
  }
}

loki.source.journal "platform" {
  forward_to    = [loki.write.local.receiver]
  relabel_rules = loki.relabel.platform.rules
  max_age       = "12h"
  labels        = {node = "$HANGAR_NODE_NAME"}
}

loki.write "local" {
  endpoint {
    url = "http://$LOGS_LOKI_LISTEN/loki/api/v1/push"
  }
}
EOF
}

logs_grafana_config() {
	cat <<EOF
; Written by deploy/host/steps/92-logs.sh.
[server]
http_addr = ${LOGS_GRAFANA_LISTEN%:*}
http_port = ${LOGS_GRAFANA_LISTEN##*:}
; Where an SSH tunnel brings it (docs/runbook/host-bootstrap.md).
root_url = http://localhost:${LOGS_GRAFANA_LISTEN##*:}/

[paths]
data = $LOGS_GRAFANA_STATE
logs = $LOGS_GRAFANA_STATE/log
plugins = $LOGS_GRAFANA_STATE/plugins
provisioning = $LOGS_GRAFANA_PROVISIONING

[log]
mode = console
level = warn

[security]
admin_user = admin
admin_password = \$__file{/run/credentials/$(logs_unit_of grafana).service/admin-password}
disable_gravatar = true

[users]
allow_sign_up = false

[auth.anonymous]
enabled = false

; Nothing reaches out: no usage reports, update checks, news, or plugins
; fetched from the internet.
[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false
feedback_links_enabled = false

[news]
news_feed_enabled = false

[plugins]
preinstall_disabled = true

[snapshots]
enabled = false
EOF
}

logs_grafana_datasources() {
	cat <<EOF
# Written by deploy/host/steps/92-logs.sh.
apiVersion: 1
prune: true
datasources:
  - name: Prometheus
    uid: prometheus
    type: prometheus
    access: proxy
    url: http://$MON_PROM_LISTEN
    isDefault: true
    editable: false
  - name: Loki
    uid: loki
    type: loki
    access: proxy
    url: http://$LOGS_LOKI_LISTEN
    editable: false
EOF
}

logs_grafana_dashboard_provider() {
	cat <<EOF
# Written by deploy/host/steps/92-logs.sh.
apiVersion: 1
providers:
  - name: hangar
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: $LOGS_GRAFANA_DASHBOARDS
EOF
}

logs_dashboard() { cat "$LOGS_SRC/$1.json"; }

# logs_files prints each file this step writes but the units: PATH MODE
# GENERATOR [ARG].
logs_files() {
	local d
	printf '%s\n' \
		"$LOGS_LOKI_CONFIG 644 logs_loki_config" \
		"$LOGS_ALLOY_CONFIG 644 logs_alloy_config" \
		"$LOGS_GRAFANA_CONFIG 644 logs_grafana_config" \
		"$LOGS_GRAFANA_PROVISIONING/datasources/hangar.yml 644 logs_grafana_datasources" \
		"$LOGS_GRAFANA_PROVISIONING/dashboards/hangar.yml 644 logs_grafana_dashboard_provider"
	for d in $LOGS_DASHBOARDS; do printf '%s\n' "$LOGS_GRAFANA_DASHBOARDS/$d.json 644 logs_dashboard $d"; done
}

# logs_restarts_on PATH prints the service a change to PATH restarts.
logs_restarts_on() {
	case $1 in
	"$LOGS_LOKI_CONFIG") logs_unit_of loki ;;
	"$LOGS_ALLOY_CONFIG") logs_unit_of alloy ;;
	"$LOGS_GRAFANA_CONFIG" | "$LOGS_GRAFANA_PROVISIONING"/*) logs_unit_of grafana ;;
	esac
}

logs_unit_loki() {
	logs_unit_head "Loki, the platform's logs"
	cat <<EOF
# zfs-mount.service mounts the dataset without a mount unit; ExecStartPre
# checks the mount itself.
Requires=zfs-mount.service
After=network.target zfs-mount.service

[Service]
$(mon_service_common loki)
ExecStartPre=+/usr/bin/mountpoint -q $LOGS_LOKI_DATA
ExecStart=$(logs_bin loki) -config.file=$LOGS_LOKI_CONFIG -target=all
ReadWritePaths=$LOGS_LOKI_DATA
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
PrivateDevices=yes
ProtectProc=invisible
ProcSubset=pid
CapabilityBoundingSet=
AmbientCapabilities=
$(mon_hardening)

[Install]
WantedBy=multi-user.target
EOF
}

logs_unit_alloy() {
	logs_unit_head "Alloy, shipping the platform's journal to Loki"
	cat <<EOF
After=network.target $(logs_unit_of loki).service
Wants=$(logs_unit_of loki).service

[Service]
$(mon_service_common alloy)
# The journal is readable by its group alone.
SupplementaryGroups=systemd-journal
# Where it has read the journal to, across restarts.
StateDirectory=${LOGS_ALLOY_STATE#/var/lib/}
StateDirectoryMode=0700
ExecStart=$(logs_bin alloy) run --disable-reporting --server.http.listen-addr=$LOGS_ALLOY_LISTEN --storage.path=$LOGS_ALLOY_STATE $LOGS_ALLOY_CONFIG
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
PrivateDevices=yes
ProtectProc=invisible
ProcSubset=pid
CapabilityBoundingSet=
AmbientCapabilities=
$(mon_hardening)

[Install]
WantedBy=multi-user.target
EOF
}

logs_unit_grafana() {
	logs_unit_head "Grafana, the operator's view of logs and metrics"
	cat <<EOF
After=network.target $(logs_unit_of loki).service $(mon_unit_of prometheus).service

[Service]
$(mon_service_common grafana)
StateDirectory=${LOGS_GRAFANA_STATE#/var/lib/}
StateDirectoryMode=0700
LoadCredential=admin-password:$LOGS_GRAFANA_PASSWORD
WorkingDirectory=$(logs_grafana_home)
ExecStart=$(logs_bin grafana) server --homepath=$(logs_grafana_home) --config=$LOGS_GRAFANA_CONFIG
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
PrivateDevices=yes
ProtectProc=invisible
ProcSubset=pid
CapabilityBoundingSet=
AmbientCapabilities=
$(mon_hardening)

[Install]
WantedBy=multi-user.target
EOF
}

# logs_unit_files prints each unit file: PATH GENERATOR.
logs_unit_files() {
	local c
	for c in $LOGS_COMPONENTS; do
		printf '%s logs_unit_%s\n' "$(services_unit_path "$(logs_unit_of "$c")")" "$c"
	done
}

# logs_stale UNIT holds when the running UNIT is not what is installed: a
# marker says its unit, configuration or binary changed since it started,
# or its binary was replaced under it.
logs_stale() {
	local pid exe
	[ ! -e "$(services_marker "$1")" ] || return 0
	pid=$(systemctl show -p MainPID --value "$1.service")
	[ "$pid" != 0 ] || return 1
	exe=$(readlink "/proc/$pid/exe") || return 1
	[ "$exe" != "$(logs_bin "${1#hangar-}")" ]
}

logs_files_ok() {
	local path mode gen arg
	while read -r path mode gen arg; do
		# shellcheck disable=SC2086 # arg is one word, or none
		generated_is "$path" "$gen" $arg && [ "$(stat -c '%a %U %G' "$path")" = "$mode root root" ] || return 1
	done < <(logs_files)
}

# logs_stray_dashboards prints each dashboard in Grafana's directory this
# step did not write.
logs_stray_dashboards() {
	local path d
	for path in "$LOGS_GRAFANA_DASHBOARDS"/*; do
		[ -e "$path" ] || continue
		d=$(basename "$path")
		[[ $d == *.json && " $LOGS_DASHBOARDS " == *" ${d%.json} "* ]] || printf '%s\n' "$path"
	done
}

logs_units_ok() {
	local path gen
	while read -r path gen; do
		generated_is "$path" "$gen" || return 1
		[ "$(systemctl show -p NeedDaemonReload --value "$(basename "$path")")" = no ] || return 1
	done < <(logs_unit_files)
}

logs_running_ok() {
	local u
	for u in $(logs_units); do
		systemctl is-enabled --quiet "$u.service" && systemctl is-active --quiet "$u.service" || return 1
		! logs_stale "$u" || return 1
	done
}

logs_loki_data_ok() {
	local u
	u=$(logs_unit_of loki)
	zfs_dataset_ok "$LOGS_LOKI_DATASET" "$LOGS_LOKI_DATA" "$LOGS_LOKI_PROPS" "" &&
		[ "$(stat -c '%U %G %a' "$LOGS_LOKI_DATA")" = "$u $u 700" ]
}

logs_password_ok() {
	[ -s "$LOGS_GRAFANA_PASSWORD" ] && [ "$(stat -c '%U %G %a' "$LOGS_GRAFANA_PASSWORD")" = "root root 600" ]
}

# logs_loopback_only holds when every TCP listener of these services is on
# loopback, and each has one.
logs_loopback_only() {
	local u pid addrs
	for u in $(logs_units); do
		pid=$(systemctl show -p MainPID --value "$u.service")
		[ "$pid" != 0 ] || return 1
		addrs=$(ss -Hltnp | grep "pid=$pid," | awk '{print $4}')
		[ -n "$addrs" ] && ! grep -qvE '^(127\.[0-9.]+|\[::1\]):[0-9]+$' <<<"$addrs" || return 1
	done
}

logs_loki_ready() { mon_get "http://$LOGS_LOKI_LISTEN/ready" | grep -qx ready; }
logs_alloy_ready() { mon_get "http://$LOGS_ALLOY_LISTEN/-/ready" >/dev/null; }

# logs_loki_has UNIT holds when Loki has lines of UNIT from the last 15
# minutes: Alloy reads its journal, and Loki takes what it sends.
logs_loki_has() {
	curl -fsS --max-time 5 -G "http://$LOGS_LOKI_LISTEN/loki/api/v1/query" \
		--data-urlencode "query=sum(count_over_time({unit=\"$1\"}[15m]))" | python3 -c '
import json, sys
r = json.load(sys.stdin)["data"]["result"]
sys.exit(0 if r and float(r[0]["value"][1]) > 0 else 1)'
}

# logs_grafana_provisioned holds when Grafana answers as healthy and has
# both datasources and every dashboard, asked as its admin.
logs_grafana_provisioned() {
	mon_get "http://$LOGS_GRAFANA_LISTEN/api/health" | grep -q '"database": *"ok"' || return 1
	logs_grafana_get /api/datasources | python3 -c '
import json, sys
sys.exit(0 if {d["uid"] for d in json.load(sys.stdin)} >= {"prometheus", "loki"} else 1)' || return 1
	# shellcheck disable=SC2086 # one argument each
	logs_grafana_get '/api/search?type=dash-db' | python3 -c '
import json, sys
sys.exit(0 if {d["uid"] for d in json.load(sys.stdin)} >= set(sys.argv[1:]) else 1)' $LOGS_DASHBOARDS
}

# logs_grafana_get PATH prints what Grafana answers GET PATH, asked as its
# admin. The password goes to curl on a pipe, never on a command line.
logs_grafana_get() {
	printf 'user = "admin:%s"\n' "$(cat "$LOGS_GRAFANA_PASSWORD")" |
		curl -fsS --max-time 5 -K - "http://$LOGS_GRAFANA_LISTEN$1"
}

# logs_config_problem prints why Loki or Alloy would refuse the
# configuration this step writes, by the pinned binaries installed: nothing
# when they take it, or are not installed yet (a dry run on a fresh host).
logs_config_problem() {
	local tmp out
	[ -x "$(logs_bin loki)" ] && [ -x "$(logs_bin alloy)" ] || return 0
	tmp=$(mktemp -d)
	logs_loki_config >"$tmp/loki.yml"
	logs_alloy_config >"$tmp/config.alloy"
	out=$("$(logs_bin loki)" -config.file="$tmp/loki.yml" -verify-config 2>&1) ||
		printf 'loki refuses its configuration: %s\n' "$(tr '\n' ' ' <<<"$out")"
	out=$("$(logs_bin alloy)" validate "$tmp/config.alloy" 2>&1) ||
		printf 'alloy refuses its configuration: %s\n' "$(tr '\n' ' ' <<<"$out")"
	rm -rf "$tmp"
}

logs_apply() {
	local c u path mode gen arg before reload=0 problem

	for c in $LOGS_COMPONENTS; do
		u=$(logs_unit_of "$c")
		id -u "$u" >/dev/null 2>&1 ||
			run "create system user $u" useradd --system --user-group \
				--home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$u"
	done

	for c in $LOGS_COMPONENTS; do
		logs_component_pinned "$c" && continue
		run "install $c $(logs_pin "$c" VERSION)" logs_install "$c"
		services_mark "$(logs_unit_of "$c")"
	done

	# Loki's data, on its own dataset, its user's alone.
	zpool list -H -o name "$HANGAR_ZPOOL" >/dev/null 2>&1 || [ "$DRY_RUN" = 1 ] ||
		die "pool $HANGAR_ZPOOL does not exist; the zfs step expects the installer to create it"
	zfs_dataset_apply "$LOGS_LOKI_DATASET" "$LOGS_LOKI_DATA" "$LOGS_LOKI_PROPS" ""
	u=$(logs_unit_of loki)
	if [ ! -d "$LOGS_LOKI_DATA" ] || [ "$(stat -c '%U %G %a' "$LOGS_LOKI_DATA")" != "$u $u 700" ]; then
		run "give $LOGS_LOKI_DATA to $u, mode 700" sh -c "chown $u:$u '$LOGS_LOKI_DATA' && chmod 700 '$LOGS_LOKI_DATA'"
	fi

	# Made here, once: no one else needs it before the operator reads it.
	if [ ! -s "$LOGS_GRAFANA_PASSWORD" ]; then
		run "make Grafana's admin password at $LOGS_GRAFANA_PASSWORD, root's, mode 600" \
			sh -c "umask 077 && openssl rand -base64 24 >'$LOGS_GRAFANA_PASSWORD'"
	elif ! logs_password_ok; then
		run "give $LOGS_GRAFANA_PASSWORD to root, mode 600" \
			sh -c "chown root:root '$LOGS_GRAFANA_PASSWORD' && chmod 600 '$LOGS_GRAFANA_PASSWORD'"
	fi

	problem=$(logs_config_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	while read -r path mode gen arg; do
		before=$CHANGES
		# shellcheck disable=SC2086 # arg is one word, or none
		put_file "$path" "$mode" "$("$gen" $arg)"
		[ "$CHANGES" = "$before" ] || [ -z "$(logs_restarts_on "$path")" ] || services_mark "$(logs_restarts_on "$path")"
	done < <(logs_files)
	# Grafana would show any other dashboard there as the platform's.
	while read -r path; do
		run "remove $path: the dashboards are deploy/host/monitoring/grafana/{$(printf '%s' "$LOGS_DASHBOARDS" | tr ' ' ',')}.json" rm -f "$path"
	done < <(logs_stray_dashboards)

	while read -r path gen; do
		before=$CHANGES
		put_file "$path" 644 "$("$gen")"
		if [ "$CHANGES" != "$before" ]; then
			reload=1
			services_mark "$(basename "$path" .service)"
		fi
		[ "$(systemctl show -p NeedDaemonReload --value "$(basename "$path")")" = no ] || reload=1
	done < <(logs_unit_files)
	[ "$reload" = 0 ] || run "reload systemd for the logs units" systemctl daemon-reload

	for u in $(logs_units); do
		systemctl is-enabled --quiet "$u.service" || run "enable $u" systemctl enable "$u.service"
		if ! systemctl is-active --quiet "$u.service"; then
			run "start $u" sh -c "systemctl start $u.service && rm -f $(services_marker "$u")"
		elif logs_stale "$u"; then
			run "restart $u" sh -c "systemctl restart $u.service && rm -f $(services_marker "$u")"
		fi
	done

	# Only once the unit runs the pinned Grafana may the others go.
	if ! logs_stale "$(logs_unit_of grafana)"; then
		while read -r path; do
			run "remove $path, a Grafana not pinned" rm -rf "$path"
		done < <(logs_old_grafanas)
	fi
}

logs_verify() {
	local c units
	for c in $LOGS_COMPONENTS; do
		check "$(logs_bin "$c") $(logs_pin "$c" VERSION) as pinned" logs_component_pinned "$c"
		check "system user $(logs_unit_of "$c")" services_user_ok "$(logs_unit_of "$c")"
	done
	check "no Grafana but $(logs_grafana_home) in $LOGS_GRAFANA_ROOT" test -z "$(logs_old_grafanas)"
	check "dataset $LOGS_LOKI_DATASET at $LOGS_LOKI_DATA, $LOGS_LOKI_PROPS, $(logs_unit_of loki)'s, mode 700" logs_loki_data_ok
	check "Grafana's admin password $LOGS_GRAFANA_PASSWORD, root's, mode 600" logs_password_ok
	check "logs configuration and dashboards as generated" logs_files_ok
	check "no dashboard in $LOGS_GRAFANA_DASHBOARDS but this step's" test -z "$(logs_stray_dashboards)"
	units=$(logs_units | paste -sd' ' -)
	check "units $units as generated and loaded" logs_units_ok
	check "units $units enabled, active, running what is installed" logs_running_ok
	check "the logs services listen on loopback only" logs_loopback_only
	check "Loki ready" mon_until 30 logs_loki_ready
	check "Alloy ready" mon_until 15 logs_alloy_ready
	check "Loki has hangar-apiserver's lines of the last 15 minutes" mon_until 30 logs_loki_has hangar-apiserver
	check "Grafana healthy, with the Prometheus and Loki datasources and the dashboards $LOGS_DASHBOARDS" mon_until 30 logs_grafana_provisioned
}
