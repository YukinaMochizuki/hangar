# The host's monitoring (docs/decisions/monitoring.md): Prometheus, which keeps
# 30 days on a dataset of its own and evaluates the rules in
# deploy/host/monitoring/rules; Alertmanager, which sends critical alerts to
# one Telegram chat at once and holds warnings to one daily window, as one
# message; node_exporter, with the zfs, mdadm, systemd (the platform's units
# and the monitoring stack's alone) and hwmon collectors, and what
# hangar-zpool-metrics adds of each pool; smartctl_exporter on each NVMe disk;
# and blackbox_exporter, probing the API's /healthz and the console over
# HTTPS with the front door on. Each runs as a systemd service of its own
# user and listens on loopback alone, so the firewall stays at 22, 80 and 443.
# Each binary is pinned in versions.env, as Cloud Hypervisor is. A unit, a
# configuration or a binary changed restarts that service, here and nowhere
# else.

# The other steps' names and helpers: the services' and the edge's units,
# the firewall's, the datasets'.
# shellcheck source=deploy/host/steps/85-edge.sh
declare -F edge_sockets >/dev/null || . "$(dirname "${BASH_SOURCE[0]}")/85-edge.sh"
# shellcheck source=deploy/host/steps/45-firewall.sh
declare -F firewall_rules >/dev/null || . "$(dirname "${BASH_SOURCE[0]}")/45-firewall.sh"
# shellcheck source=deploy/host/steps/30-zfs.sh
declare -F zfs_dataset_apply >/dev/null || . "$(dirname "${BASH_SOURCE[0]}")/30-zfs.sh"

# The files of deploy/host/monitoring this step installs.
MON_SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/../monitoring" && pwd)

# What each component is pinned as: its key, the prefix of its variables in
# versions.env, and the binaries the step installs from its tarball. Each
# runs as the service hangar-<key with - for _>, under the user of that name.
MON_COMPONENTS="prometheus alertmanager node_exporter smartctl_exporter blackbox_exporter"
MON_BIN_DIR=/usr/local/bin
MON_ETC=/etc/hangar/monitoring
MON_PROM_CONFIG=$MON_ETC/prometheus.yml
MON_RULES_DIR=$MON_ETC/rules
MON_AM_CONFIG=$MON_ETC/alertmanager.yml
MON_BLACKBOX_CONFIG=$MON_ETC/blackbox.yml
# The Telegram bot's token, which Alertmanager reads as it sends: the
# operator installs it from the vault (docs/runbook/host-bootstrap.md); this
# step never makes one.
MON_TELEGRAM_TOKEN=/etc/hangar/alertmanager-telegram-token
MON_TELEGRAM_API_URL=${HANGAR_ALERTS_TELEGRAM_API_URL:-https://api.telegram.org}
MON_ALERTS_TZ=${HANGAR_ALERTS_TZ:-${HANGAR_UPGRADES_TZ:-UTC}}
# Warnings are sent in this window each day, in MON_ALERTS_TZ.
MON_WARNING_WINDOW_START=09:00
MON_WARNING_WINDOW_END=10:00
# Prometheus's data, on a dataset of its own whose quota keeps it from
# filling the pool; Prometheus drops its oldest blocks past
# MON_TSDB_RETENTION_SIZE, before the quota is reached.
MON_TSDB_DATASET=$HANGAR_ZPOOL/prometheus
MON_TSDB=/var/lib/hangar/prometheus
MON_TSDB_PROPS=setuid=off,exec=off,devices=off,quota=$MON_TSDB_QUOTA
MON_TSDB_RETENTION_TIME=30d
MON_TSDB_RETENTION_SIZE=16GB
MON_AM_STATE=/var/lib/hangar-alertmanager
MON_TEXTFILE_DIR=/var/lib/hangar-node-exporter/textfile
MON_ZPOOL_METRICS=/usr/local/libexec/hangar-zpool-metrics
MON_ZPOOL_UNIT=hangar-zpool-metrics
MON_PROM_LISTEN=127.0.0.1:9090
MON_AM_LISTEN=127.0.0.1:9093
MON_NODE_LISTEN=127.0.0.1:9100
MON_SMARTCTL_LISTEN=127.0.0.1:9633
MON_BLACKBOX_LISTEN=127.0.0.1:9115

# mon_unit_of KEY prints the service KEY runs as, which is also its user.
mon_unit_of() { printf 'hangar-%s\n' "${1//_/-}"; }

mon_units() {
	local c
	for c in $MON_COMPONENTS; do mon_unit_of "$c"; done
}

# mon_pin KEY VAR prints KEY's VAR (VERSION, URL, SHA256) from versions.env.
mon_pin() {
	local v
	v=HANGAR_${1^^}_$2
	printf '%s\n' "${!v}"
}

# mon_bins KEY prints each binary installed from KEY's tarball and the
# versions.env variable holding its sha256: the one its service runs first.
mon_bins() {
	case $1 in
	prometheus) printf '%s\n' prometheus:HANGAR_PROMETHEUS_BIN_SHA256 promtool:HANGAR_PROMTOOL_BIN_SHA256 ;;
	alertmanager) printf '%s\n' alertmanager:HANGAR_ALERTMANAGER_BIN_SHA256 amtool:HANGAR_AMTOOL_BIN_SHA256 ;;
	*) printf '%s:HANGAR_%s_BIN_SHA256\n' "$1" "${1^^}" ;;
	esac
}

# mon_bin_pinned NAME SHAVAR holds when the installed NAME is the pinned one.
mon_bin_pinned() {
	[ -x "$MON_BIN_DIR/$1" ] && echo "${!2}  $MON_BIN_DIR/$1" | sha256sum -c --quiet - >/dev/null 2>&1
}

mon_component_pinned() {
	local b
	for b in $(mon_bins "$1"); do
		mon_bin_pinned "${b%%:*}" "${b#*:}" || return 1
	done
}

# mon_install KEY downloads KEY's tarball, checks it and each binary taken
# from it against versions.env, and installs them, each replaced whole.
mon_install() {
	local key=$1 tmp dir b name shavar
	tmp=$(mktemp -d)
	dir=$key-$(mon_pin "$key" VERSION).linux-amd64
	curl -fsSL -o "$tmp/t.tar.gz" "$(mon_pin "$key" URL)"
	if ! echo "$(mon_pin "$key" SHA256)  $tmp/t.tar.gz" | sha256sum -c --quiet -; then
		rm -rf "$tmp"
		die "checksum mismatch for $(mon_pin "$key" URL)"
	fi
	for b in $(mon_bins "$key"); do
		name=${b%%:*} shavar=${b#*:}
		tar -xzf "$tmp/t.tar.gz" -C "$tmp" "$dir/$name"
		if ! echo "${!shavar}  $tmp/$dir/$name" | sha256sum -c --quiet -; then
			rm -rf "$tmp"
			die "the $name binary in $(mon_pin "$key" URL) does not match $shavar"
		fi
	done
	for b in $(mon_bins "$key"); do
		name=${b%%:*}
		install -m 0755 "$tmp/$dir/$name" "$MON_BIN_DIR/$name.hangar-new"
		mv -f "$MON_BIN_DIR/$name.hangar-new" "$MON_BIN_DIR/$name"
	done
	rm -rf "$tmp"
}

# mon_nvme_devices prints each NVMe controller, /dev/nvmeN, which
# smartctl_exporter reads.
mon_nvme_devices() {
	local d
	for d in /dev/nvme[0-9]*; do
		[[ $d =~ ^/dev/nvme[0-9]+$ ]] && [ -c "$d" ] && printf '%s\n' "$d"
	done
	return 0
}

# mon_watched_units prints the units node_exporter reports, each of which
# must be active: the platform's, as the other steps install them on this
# host, and the monitoring stack's. Never a VM's (hangar-vm-*), whose name
# carries the machine's uid.
mon_watched_units() {
	local u
	for u in $(services_units) "$EDGE_UNIT" "$FIREWALL_UNIT" $(mon_units); do
		[ "$u" != "$EDGE_UNIT" ] || services_edge_on || continue
		printf '%s.service\n' "$u"
	done
	for u in $(services_sockets) $(edge_sockets); do printf '%s.socket\n' "$u"; done
	printf '%s.timer\n' "$MON_ZPOOL_UNIT"
}

# mon_unit_include prints node_exporter's --collector.systemd.unit-include,
# which it anchors: the watched units' names, dots matched literally.
mon_unit_include() {
	mon_watched_units | sed 's/\./[.]/g' | paste -sd'|' -
}

# mon_probes prints each URL blackbox_exporter probes and its module: the
# API's /healthz and the console, with the front door on.
mon_probes() {
	services_front_door_on || return 0
	local cp
	cp=$(local_env HANGAR_CP_DOMAIN)
	printf '%s\n' "https://api.$cp/healthz https_healthz" "https://console.$cp/ https_2xx"
}

mon_prometheus_config() {
	cat <<EOF
# Written by deploy/host/steps/90-monitoring.sh.
global:
  scrape_interval: 30s
  evaluation_interval: 30s

rule_files:
  - $MON_RULES_DIR/*.yml

alerting:
  alertmanagers:
    - static_configs:
        - targets: ["$MON_AM_LISTEN"]

scrape_configs:
  - job_name: prometheus
    static_configs:
      - targets: ["$MON_PROM_LISTEN"]
  - job_name: alertmanager
    static_configs:
      - targets: ["$MON_AM_LISTEN"]
  - job_name: node
    static_configs:
      - targets: ["$MON_NODE_LISTEN"]
    metric_relabel_configs:
      # Each dataset's counters, a machine's volumes among them, named in a
      # label: no tenant in a label.
      - source_labels: [__name__]
        regex: node_zfs_zpool_dataset_.*
        action: drop
  - job_name: smartctl
    scrape_interval: 60s
    static_configs:
      - targets: ["$MON_SMARTCTL_LISTEN"]
  - job_name: blackbox-exporter
    static_configs:
      - targets: ["$MON_BLACKBOX_LISTEN"]
EOF
	local url module
	[ -n "$(mon_probes)" ] || return 0
	cat <<EOF
  - job_name: blackbox
    scrape_interval: 60s
    scrape_timeout: 15s
    metrics_path: /probe
    static_configs:
EOF
	while read -r url module; do
		cat <<EOF
      - targets: ["$url"]
        labels:
          __param_module: $module
EOF
	done < <(mon_probes)
	cat <<EOF
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: $MON_BLACKBOX_LISTEN
EOF
}

# mon_telegram SEND_RESOLVED prints a receiver's telegram_configs, nothing
# while the host env file names no chat: then alerts reach no one.
mon_telegram() {
	[ -n "${HANGAR_ALERTS_TELEGRAM_CHAT_ID:-}" ] || return 0
	cat <<EOF
    telegram_configs:
      - api_url: $MON_TELEGRAM_API_URL
        bot_token_file: $MON_TELEGRAM_TOKEN
        chat_id: $HANGAR_ALERTS_TELEGRAM_CHAT_ID
        send_resolved: $1
EOF
}

mon_alertmanager_config() {
	cat <<EOF
# Written by deploy/host/steps/90-monitoring.sh. Critical alerts go to
# Telegram at once, one message an alert name. Warnings go once a day, in
# the warning window, all in one message. Anything else is sent as critical.
route:
  receiver: critical
  group_by: [alertname]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
    # Always firing; for a dead man's switch outside the host, which there
    # is not yet (docs/decisions/monitoring.md).
    - matchers: ['alertname="Watchdog"']
      receiver: watchdog
    # One group, one tick of group_interval inside the window: one message a
    # day, while anything is firing.
    - matchers: ['severity="warning"']
      receiver: warning
      group_by: [severity]
      group_wait: 1m
      group_interval: 1h
      repeat_interval: 23h
      active_time_intervals: [warning-window]

time_intervals:
  - name: warning-window
    time_intervals:
      - times:
          - start_time: "$MON_WARNING_WINDOW_START"
            end_time: "$MON_WARNING_WINDOW_END"
        location: $MON_ALERTS_TZ

receivers:
  - name: watchdog
  - name: critical
$(mon_telegram true)
  - name: warning
$(mon_telegram false)
EOF
}

mon_blackbox_config() {
	cat <<'EOF'
# Written by deploy/host/steps/90-monitoring.sh. What a client sees from
# outside, by the names it resolves: IPv4 first, then IPv6.
modules:
  https_healthz:
    prober: http
    timeout: 10s
    http:
      valid_status_codes: [200]
      fail_if_not_ssl: true
      fail_if_body_not_matches_regexp: ["^ok"]
      follow_redirects: false
      preferred_ip_protocol: ip4
      ip_protocol_fallback: true
  https_2xx:
    prober: http
    timeout: 10s
    http:
      fail_if_not_ssl: true
      preferred_ip_protocol: ip4
      ip_protocol_fallback: true
EOF
}

# mon_hardening prints the sandbox every monitoring service shares.
mon_hardening() {
	cat <<'EOF'
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateIPC=yes
ProtectHostname=yes
ProtectClock=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
NoNewPrivileges=yes
RemoveIPC=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
SystemCallErrorNumber=EPERM
EOF
}

# mon_unit_head DESCRIPTION prints the start of a service's unit.
mon_unit_head() {
	cat <<EOF
# Written by deploy/host/steps/90-monitoring.sh.
[Unit]
Description=Hangar monitoring: $1
EOF
}

# mon_service_common KEY prints what each service's [Service] starts with.
mon_service_common() {
	local u
	u=$(mon_unit_of "$1")
	cat <<EOF
Type=exec
User=$u
Group=$u
Restart=always
RestartSec=5
UMask=0077
EOF
}

mon_unit_prometheus() {
	mon_unit_head "Prometheus"
	cat <<EOF
# zfs-mount.service mounts the dataset without a mount unit; ExecStartPre
# checks the mount itself.
Requires=zfs-mount.service
After=network.target zfs-mount.service

[Service]
$(mon_service_common prometheus)
ExecStartPre=+/usr/bin/mountpoint -q $MON_TSDB
# An alert's link opens where an SSH tunnel brings Prometheus
# (docs/runbook/host-bootstrap.md).
ExecStart=$MON_BIN_DIR/prometheus --config.file=$MON_PROM_CONFIG --storage.tsdb.path=$MON_TSDB --storage.tsdb.retention.time=$MON_TSDB_RETENTION_TIME --storage.tsdb.retention.size=$MON_TSDB_RETENTION_SIZE --web.listen-address=$MON_PROM_LISTEN --web.external-url=http://localhost:${MON_PROM_LISTEN##*:}
ReadWritePaths=$MON_TSDB
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

mon_unit_alertmanager() {
	mon_unit_head "Alertmanager, to Telegram"
	cat <<EOF
After=network-online.target
Wants=network-online.target

[Service]
$(mon_service_common alertmanager)
# Its silences and what it has sent, across restarts: a warning sent today
# is not sent again after one. No cluster: it would listen on every address.
StateDirectory=${MON_AM_STATE#/var/lib/}
StateDirectoryMode=0700
ExecStart=$MON_BIN_DIR/alertmanager --config.file=$MON_AM_CONFIG --storage.path=$MON_AM_STATE --web.listen-address=$MON_AM_LISTEN --cluster.listen-address=
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

# node_exporter's flags: the collectors it runs, no others. diskstats leaves
# out zvols, one a machine's volume; netdev reports the uplink alone, not
# the machines' taps and bridges. Each $ is systemd's $$.
mon_node_exporter_flags() {
	local c
	printf -- '--web.listen-address=%s --collector.disable-defaults' "$MON_NODE_LISTEN"
	for c in cpu loadavg meminfo vmstat pressure stat filefd uname time timex filesystem diskstats netdev nvme edac hwmon zfs mdadm systemd textfile; do
		printf -- ' --collector.%s' "$c"
	done
	printf -- ' --collector.systemd.unit-include=%s --collector.systemd.enable-restarts-metrics' "$(mon_unit_include)"
	printf -- ' --collector.textfile.directory=%s' "$MON_TEXTFILE_DIR"
	printf -- ' --collector.diskstats.device-exclude=^(z?ram|loop|fd|zd|(h|s|v|xv)d[a-z]|nvme[0-9]+n[0-9]+p)[0-9]+$$'
	printf -- ' --collector.netdev.device-include=^%s$$\n' "$HANGAR_UPLINK"
}

mon_unit_node_exporter() {
	mon_unit_head "node_exporter"
	cat <<EOF
After=network.target

[Service]
$(mon_service_common node_exporter)
ExecStart=$MON_BIN_DIR/node_exporter $(mon_node_exporter_flags)
# It asks systemd about units over D-Bus and reads the uplink over netlink;
# it reads /proc and /sys whole, so no ProcSubset=.
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
PrivateDevices=yes
CapabilityBoundingSet=
AmbientCapabilities=
$(mon_hardening | grep -v '^ProtectClock=')
# The timex collector reads the clock's state with adjtimex, which
# ProtectClock= and @privileged refuse; without CAP_SYS_TIME it can change
# nothing.
SystemCallFilter=adjtimex

[Install]
WantedBy=multi-user.target
EOF
}

# smartctl_exporter runs smartctl on each NVMe controller, which takes
# CAP_SYS_ADMIN (the NVMe admin commands), CAP_SYS_RAWIO, and reading the
# device, root's alone (CAP_DAC_READ_SEARCH). The devices are named, never
# scanned: a host with none names none.
mon_smartctl_flags() {
	local d
	printf -- '--web.listen-address=%s --smartctl.path=/usr/sbin/smartctl --no-smartctl.scan' "$MON_SMARTCTL_LISTEN"
	for d in $(mon_nvme_devices); do printf -- ' --smartctl.device=%s' "$d"; done
	printf '\n'
}

mon_unit_smartctl_exporter() {
	mon_unit_head "smartctl_exporter, the NVMe disks' SMART"
	cat <<EOF
After=network.target

[Service]
$(mon_service_common smartctl_exporter)
ExecStart=$MON_BIN_DIR/smartctl_exporter $(mon_smartctl_flags)
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
DevicePolicy=closed
DeviceAllow=char-nvme r
CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SYS_RAWIO CAP_DAC_READ_SEARCH
AmbientCapabilities=CAP_SYS_ADMIN CAP_SYS_RAWIO CAP_DAC_READ_SEARCH
$(mon_hardening)

[Install]
WantedBy=multi-user.target
EOF
}

mon_unit_blackbox_exporter() {
	mon_unit_head "blackbox_exporter, the HTTPS probes"
	cat <<EOF
After=network-online.target
Wants=network-online.target

[Service]
$(mon_service_common blackbox_exporter)
ExecStart=$MON_BIN_DIR/blackbox_exporter --config.file=$MON_BLACKBOX_CONFIG --web.listen-address=$MON_BLACKBOX_LISTEN
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

mon_zpool_service() {
	cat <<EOF
# Written by deploy/host/steps/90-monitoring.sh.
[Unit]
Description=Hangar monitoring: the pools' size and errors for node_exporter
After=zfs-mount.service

[Service]
Type=oneshot
ExecStart=$MON_ZPOOL_METRICS $MON_TEXTFILE_DIR/zpool.prom
# zpool reads /dev/zfs; it writes the one file.
ReadWritePaths=$MON_TEXTFILE_DIR
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateNetwork=yes
NoNewPrivileges=yes
EOF
}

mon_zpool_timer() {
	cat <<EOF
# Written by deploy/host/steps/90-monitoring.sh.
[Unit]
Description=Hangar monitoring: the pools' size and errors, each minute

[Timer]
OnBootSec=30s
OnUnitActiveSec=1min
AccuracySec=5s

[Install]
WantedBy=timers.target
EOF
}

# mon_files prints each file this step writes but the units: PATH MODE
# GENERATOR.
mon_files() {
	printf '%s\n' \
		"$MON_PROM_CONFIG 644 mon_prometheus_config" \
		"$MON_RULES_DIR/host.yml 644 mon_rules" \
		"$MON_AM_CONFIG 644 mon_alertmanager_config" \
		"$MON_BLACKBOX_CONFIG 644 mon_blackbox_config" \
		"$MON_ZPOOL_METRICS 755 mon_zpool_script"
}

mon_rules() { cat "$MON_SRC/rules/host.yml"; }
mon_zpool_script() { cat "$MON_SRC/zpool-metrics.sh"; }

# mon_unit_files prints each unit file: PATH GENERATOR.
mon_unit_files() {
	local c
	for c in $MON_COMPONENTS; do
		printf '%s mon_unit_%s\n' "$(services_unit_path "$(mon_unit_of "$c")")" "$c"
	done
	printf '%s mon_zpool_service\n' "$(services_unit_path "$MON_ZPOOL_UNIT")"
	printf '/etc/systemd/system/%s.timer mon_zpool_timer\n' "$MON_ZPOOL_UNIT"
}

# mon_restarts_on PATH prints the service a change to PATH restarts.
mon_restarts_on() {
	case $1 in
	"$MON_PROM_CONFIG" | "$MON_RULES_DIR"/*) mon_unit_of prometheus ;;
	"$MON_AM_CONFIG") mon_unit_of alertmanager ;;
	"$MON_BLACKBOX_CONFIG") mon_unit_of blackbox_exporter ;;
	esac
}

# mon_stale UNIT holds when the running UNIT is not what is installed: a
# marker says its unit, configuration or binary changed since it started,
# or its binary was replaced under it.
mon_stale() {
	local pid exe
	[ ! -e "$(services_marker "$1")" ] || return 0
	pid=$(systemctl show -p MainPID --value "$1.service")
	[ "$pid" != 0 ] || return 1
	exe=$(readlink "/proc/$pid/exe") || return 1
	[ "$exe" != "$MON_BIN_DIR/$(mon_key_of "$1")" ]
}

# mon_key_of UNIT prints the component UNIT runs.
mon_key_of() {
	local k=${1#hangar-}
	printf '%s\n' "${k//-/_}"
}

mon_files_ok() {
	local path mode gen
	while read -r path mode gen; do
		"$gen" | cmp -s - "$path" && [ "$(stat -c '%a %U %G' "$path")" = "$mode root root" ] || return 1
	done < <(mon_files)
}

mon_units_ok() {
	local path gen
	while read -r path gen; do
		"$gen" | cmp -s - "$path" || return 1
		[ "$(systemctl show -p NeedDaemonReload --value "$(basename "$path")")" = no ] || return 1
	done < <(mon_unit_files)
}

mon_running_ok() {
	local u
	for u in $(mon_units); do
		systemctl is-enabled --quiet "$u.service" && systemctl is-active --quiet "$u.service" || return 1
		! mon_stale "$u" || return 1
	done
	systemctl is-enabled --quiet "$MON_ZPOOL_UNIT.timer" && systemctl is-active --quiet "$MON_ZPOOL_UNIT.timer"
}

mon_tsdb_ok() {
	local u
	u=$(mon_unit_of prometheus)
	zfs_dataset_ok "$MON_TSDB_DATASET" "$MON_TSDB" "$MON_TSDB_PROPS" "" &&
		[ "$(stat -c '%U %G %a' "$MON_TSDB")" = "$u $u 700" ]
}

mon_textfile_dir_ok() {
	[ "$(stat -c '%U %G %a' "$MON_TEXTFILE_DIR")" = "root root 755" ]
}

# mon_textfile_fresh holds when hangar-zpool-metrics wrote within 3 minutes.
mon_textfile_fresh() {
	local f=$MON_TEXTFILE_DIR/zpool.prom
	[ -f "$f" ] && [ $(($(date +%s) - $(stat -c %Y "$f"))) -lt 180 ] &&
		grep -q "^hangar_zpool_size_bytes{zpool=\"$HANGAR_ZPOOL\"}" "$f"
}

# mon_loopback_only holds when every TCP listener of the monitoring services
# is on loopback, and each has one.
mon_loopback_only() {
	local u pid addrs
	for u in $(mon_units); do
		pid=$(systemctl show -p MainPID --value "$u.service")
		[ "$pid" != 0 ] || return 1
		addrs=$(ss -Hltnp | grep "pid=$pid," | awk '{print $4}')
		[ -n "$addrs" ] && ! grep -qvE '^(127\.[0-9.]+|\[::1\]):[0-9]+$' <<<"$addrs" || return 1
	done
}

# mon_get URL prints what GET URL answers.
mon_get() { curl -fsS --max-time 5 "$1"; }

# mon_until TRIES CMD... runs CMD every 2 s until it succeeds, TRIES times:
# a service that just started takes a moment.
mon_until() {
	local n=$1 i
	shift
	for i in $(seq 1 "$n"); do
		"$@" && return 0
		[ "$i" = "$n" ] || sleep 2
	done
	return 1
}

# mon_targets_up holds when Prometheus scrapes every target it has, each up.
mon_targets_up() {
	mon_get "http://$MON_PROM_LISTEN/api/v1/targets?state=active" | python3 -c '
import json, sys
t = json.load(sys.stdin)["data"]["activeTargets"]
sys.exit(0 if t and all(x["health"] == "up" for x in t) else 1)'
}

# mon_rules_loaded holds when Prometheus has loaded every rule of the rules
# file, and evaluated them without error.
mon_rules_loaded() {
	local want
	want=$(grep -cE '^ +- alert: ' "$MON_SRC/rules/host.yml")
	mon_get "http://$MON_PROM_LISTEN/api/v1/rules" | python3 -c '
import json, sys
g = json.load(sys.stdin)["data"]["groups"]
r = [x for grp in g for x in grp["rules"]]
sys.exit(0 if len(r) == int(sys.argv[1]) and all(x["health"] == "ok" for x in r) else 1)' "$want"
}

# mon_watchdog_through holds when Alertmanager has Watchdog from Prometheus:
# the path from a rule to Alertmanager works.
mon_watchdog_through() {
	mon_get "http://$MON_AM_LISTEN/api/v2/alerts?filter=alertname%3D%22Watchdog%22" | grep -q '"alertname":"Watchdog"'
}

# mon_nvme_seen holds when smartctl_exporter reports each NVMe controller.
mon_nvme_seen() {
	local d metrics
	metrics=$(mon_until 15 mon_get "http://$MON_SMARTCTL_LISTEN/metrics") || return 1
	for d in $(mon_nvme_devices); do
		grep -q "^smartctl_device{.*device=\"${d#/dev/}\"" <<<"$metrics" || return 1
	done
}

mon_token_ok() {
	[ -s "$MON_TELEGRAM_TOKEN" ] &&
		[ "$(stat -c '%U %G %a' "$MON_TELEGRAM_TOKEN")" = "root $(mon_unit_of alertmanager) 640" ]
}

mon_token_how() {
	printf '%s\n' "install the Telegram bot's token from the vault at $MON_TELEGRAM_TOKEN, root's and $(mon_unit_of alertmanager)'s group's, mode 0640 (docs/runbook/host-bootstrap.md)"
}

# mon_config_problem prints why Prometheus or Alertmanager would refuse the
# configuration this step writes, by the pinned promtool and amtool
# installed: nothing when they take it, or are not installed yet (a dry run
# on a fresh host).
mon_config_problem() {
	local tmp out
	[ -x "$MON_BIN_DIR/promtool" ] && [ -x "$MON_BIN_DIR/amtool" ] || return 0
	tmp=$(mktemp -d)
	mon_rules >"$tmp/host.yml"
	mon_prometheus_config | sed "s|$MON_RULES_DIR/\\*.yml|$tmp/host.yml|" >"$tmp/prometheus.yml"
	mon_alertmanager_config >"$tmp/alertmanager.yml"
	out=$("$MON_BIN_DIR/promtool" check config "$tmp/prometheus.yml" 2>&1) ||
		printf 'promtool refuses the Prometheus configuration: %s\n' "$(tr '\n' ' ' <<<"$out")"
	out=$("$MON_BIN_DIR/amtool" check-config "$tmp/alertmanager.yml" 2>&1) ||
		printf 'amtool refuses the Alertmanager configuration: %s\n' "$(tr '\n' ' ' <<<"$out")"
	rm -rf "$tmp"
}

monitoring_apply() {
	local c u path mode gen before reload=0 problem

	for c in $MON_COMPONENTS; do
		u=$(mon_unit_of "$c")
		id -u "$u" >/dev/null 2>&1 ||
			run "create system user $u" useradd --system --user-group \
				--home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$u"
	done

	for c in $MON_COMPONENTS; do
		mon_component_pinned "$c" && continue
		run "install $c $(mon_pin "$c" VERSION)" mon_install "$c"
		services_mark "$(mon_unit_of "$c")"
	done

	# Prometheus's data, on its own dataset, its user's alone.
	zpool list -H -o name "$HANGAR_ZPOOL" >/dev/null 2>&1 || [ "$DRY_RUN" = 1 ] ||
		die "pool $HANGAR_ZPOOL does not exist; the zfs step expects the installer to create it"
	zfs_dataset_apply "$MON_TSDB_DATASET" "$MON_TSDB" "$MON_TSDB_PROPS" ""
	u=$(mon_unit_of prometheus)
	if [ ! -d "$MON_TSDB" ] || [ "$(stat -c '%U %G %a' "$MON_TSDB")" != "$u $u 700" ]; then
		run "give $MON_TSDB to $u, mode 700" sh -c "chown $u:$u '$MON_TSDB' && chmod 700 '$MON_TSDB'"
	fi
	if [ ! -d "$MON_TEXTFILE_DIR" ] || ! mon_textfile_dir_ok; then
		run "make $MON_TEXTFILE_DIR, root's, mode 755" install -d -m 0755 -o root -g root "$MON_TEXTFILE_DIR"
	fi

	problem=$(mon_config_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	while read -r path mode gen; do
		before=$CHANGES
		put_file "$path" "$mode" "$("$gen")"
		[ "$CHANGES" = "$before" ] || [ -z "$(mon_restarts_on "$path")" ] || services_mark "$(mon_restarts_on "$path")"
	done < <(mon_files)
	# Other rules files are not this step's: Prometheus would load them.
	for path in "$MON_RULES_DIR"/*; do
		[ -e "$path" ] && [ "$path" != "$MON_RULES_DIR/host.yml" ] || continue
		run "remove $path: the rules are deploy/host/monitoring/rules/host.yml" rm -f "$path"
		services_mark "$(mon_unit_of prometheus)"
	done

	# The bot's token is the operator's to install, never this step's to make;
	# without it, or a chat, alerts reach no one.
	if [ -z "${HANGAR_ALERTS_TELEGRAM_CHAT_ID:-}" ]; then
		log "warning: the host env file sets no HANGAR_ALERTS_TELEGRAM_CHAT_ID: alerts reach no one until it does and you $(mon_token_how)"
	elif [ ! -e "$MON_TELEGRAM_TOKEN" ]; then
		log "warning: no Telegram bot token at $MON_TELEGRAM_TOKEN: alerts reach no one until you $(mon_token_how)"
	elif ! mon_token_ok; then
		run "give $MON_TELEGRAM_TOKEN to root and $(mon_unit_of alertmanager)'s group, mode 640" \
			sh -c "chown root:$(mon_unit_of alertmanager) '$MON_TELEGRAM_TOKEN' && chmod 640 '$MON_TELEGRAM_TOKEN'"
	fi

	while read -r path gen; do
		before=$CHANGES
		put_file "$path" 644 "$("$gen")"
		if [ "$CHANGES" != "$before" ]; then
			reload=1
			u=$(basename "$path" .service)
			[[ $u == *.timer ]] || [ "$u" = "$MON_ZPOOL_UNIT" ] || services_mark "$u"
		fi
		[ "$(systemctl show -p NeedDaemonReload --value "$(basename "$path")")" = no ] || reload=1
	done < <(mon_unit_files)
	[ "$reload" = 0 ] || run "reload systemd for the monitoring units" systemctl daemon-reload

	for u in $(mon_units); do
		systemctl is-enabled --quiet "$u.service" || run "enable $u" systemctl enable "$u.service"
		if ! systemctl is-active --quiet "$u.service"; then
			run "start $u" sh -c "systemctl start $u.service && rm -f $(services_marker "$u")"
		elif mon_stale "$u"; then
			run "restart $u" sh -c "systemctl restart $u.service && rm -f $(services_marker "$u")"
		fi
	done
	systemctl is-enabled --quiet "$MON_ZPOOL_UNIT.timer" || run "enable $MON_ZPOOL_UNIT.timer" systemctl enable "$MON_ZPOOL_UNIT.timer"
	if ! systemctl is-active --quiet "$MON_ZPOOL_UNIT.timer"; then
		run "start $MON_ZPOOL_UNIT.timer" sh -c "systemctl start $MON_ZPOOL_UNIT.service $MON_ZPOOL_UNIT.timer"
	fi
}

monitoring_verify() {
	local c b units
	for c in $MON_COMPONENTS; do
		for b in $(mon_bins "$c"); do
			check "$MON_BIN_DIR/${b%%:*} $(mon_pin "$c" VERSION) as pinned" mon_bin_pinned "${b%%:*}" "${b#*:}"
		done
		check "system user $(mon_unit_of "$c")" services_user_ok "$(mon_unit_of "$c")"
	done
	check "dataset $MON_TSDB_DATASET at $MON_TSDB, $MON_TSDB_PROPS, $(mon_unit_of prometheus)'s, mode 700" mon_tsdb_ok
	check "$MON_TEXTFILE_DIR root's, mode 755" mon_textfile_dir_ok
	check "monitoring configuration, rules and $MON_ZPOOL_METRICS as generated" mon_files_ok
	units=$(mon_units | paste -sd' ' -)
	check "units $units $MON_ZPOOL_UNIT as generated and loaded" mon_units_ok
	check "units $units enabled, active, running what is installed; $MON_ZPOOL_UNIT.timer active" mon_running_ok
	check "the monitoring services listen on loopback only" mon_loopback_only
	check "$MON_ZPOOL_UNIT wrote $MON_TEXTFILE_DIR/zpool.prom within 3 minutes" mon_textfile_fresh
	check "Prometheus scrapes every target, each up" mon_until 30 mon_targets_up
	check "Prometheus has loaded every rule of host.yml, each healthy" mon_until 15 mon_rules_loaded
	check "Alertmanager has Watchdog from Prometheus" mon_until 45 mon_watchdog_through
	if [ -n "$(mon_nvme_devices)" ]; then
		check "smartctl_exporter reports every NVMe disk: $(mon_nvme_devices | paste -sd' ' -)" mon_nvme_seen
	else
		skip "smartctl_exporter's disks" "no NVMe disk on this host"
	fi
	if [ -n "${HANGAR_ALERTS_TELEGRAM_CHAT_ID:-}" ]; then
		check "Telegram bot token $MON_TELEGRAM_TOKEN, root's and $(mon_unit_of alertmanager)'s group's, mode 640" mon_token_ok
	else
		skip "alerts to Telegram" "the host env file sets no HANGAR_ALERTS_TELEGRAM_CHAT_ID: alerts reach no one"
	fi
}
