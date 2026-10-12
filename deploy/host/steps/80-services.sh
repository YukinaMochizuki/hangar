# The apiserver and the node agent as systemd services. The apiserver runs as
# its own unprivileged user with its database on hangar/state, the public API
# on loopback in dev auth (until the Cloudflare tunnel lands), and the node
# socket in a runtime directory it owns. The agent runs as root on this host's
# drivers. A unit changed, or a binary the release step replaced, restarts
# that service, here and nowhere else.

SERVICES_USER=hangar
SERVICES_STATE=/var/lib/hangar/state
SERVICES_LISTEN=127.0.0.1:8080
SERVICES_SOCKET=/run/hangar/node.sock
SERVICES_UNITS="hangar-apiserver hangar-node"

services_unit_path() { printf '/etc/systemd/system/%s.service\n' "$1"; }
services_marker() { printf '%s/restart-%s\n' "$STATE_DIR" "$1"; }

# services_reserved_disk is what root disks may not take from the pool: what
# the image cache and the VMs' directories may hold, their quotas.
services_reserved_disk() {
	printf '%dGi\n' $(($(gib_of "$HANGAR_IMAGES_QUOTA") + $(gib_of "$HANGAR_VMS_QUOTA")))
}

services_unit_hangar_apiserver() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh.
[Unit]
Description=Hangar API server
# zfs-mount.service mounts the datasets without mount units, so
# RequiresMountsFor= cannot see them; ExecStartPre checks the mount itself.
Requires=zfs-mount.service
After=network.target zfs-mount.service

[Service]
Type=exec
# Outside the sandbox (+): ReadWritePaths= bind-mounts the path onto itself.
ExecStartPre=+/usr/bin/mountpoint -q $SERVICES_STATE
User=$SERVICES_USER
Group=$SERVICES_USER
# HANGAR_BOOTSTRAP_ADMIN, kept on the host only.
EnvironmentFile=$LOCAL_ENV
# Dev auth trusts a header, so the public API is on loopback only until the
# Cloudflare tunnel and Access land. The node socket accepts root's agent.
ExecStart=/usr/local/bin/hangar-apiserver --db $SERVICES_STATE/hangar.db --listen $SERVICES_LISTEN --auth-mode dev --socket $SERVICES_SOCKET --node-uid 0 --node-name $HANGAR_NODE_NAME --bootstrap-admin \${HANGAR_BOOTSTRAP_ADMIN}
RuntimeDirectory=hangar
RuntimeDirectoryMode=0755
Restart=on-failure
RestartSec=2
UMask=0077

# It needs its database, two sockets and nothing else.
ReadWritePaths=$SERVICES_STATE
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
PrivateIPC=yes
ProtectProc=invisible
ProcSubset=pid
ProtectHostname=yes
ProtectClock=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
RemoveIPC=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
}

services_unit_hangar_node() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh.
[Unit]
Description=Hangar node agent
Requires=zfs-mount.service
After=network-online.target zfs-mount.service hangar-apiserver.service
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=exec
# Never on the root filesystem: the datasets carry the quotas and the
# no-exec properties.
ExecStartPre=/usr/bin/mountpoint -q /var/lib/hangar/images
ExecStartPre=/usr/bin/mountpoint -q /var/lib/hangar/vms
# The default paths: Cloud Hypervisor, the guest kernel and initramfs under
# /usr/local, the pool's hangar dataset, /var/lib/hangar/images and vms.
ExecStart=/usr/local/bin/hangar-node --drivers=host --name $HANGAR_NODE_NAME --uplink $HANGAR_UPLINK --public-ipv4 $HANGAR_PUBLIC_IPV4 --reserved-disk $(services_reserved_disk) --reserved-memory $HANGAR_RESERVED_MEMORY
Restart=always
RestartSec=2
# Each VM is a transient unit of its own (hangar-vm-*.service), outside this
# cgroup, so a stop or restart of the agent kills the agent and what it runs
# itself (zfs, mkfs), and never a VM.
KillMode=control-group

[Install]
WantedBy=multi-user.target
EOF
}

services_unit() { "services_unit_${1//-/_}"; }

services_local_env_ok() {
	[ -f "$LOCAL_ENV" ] && [[ $(stat -c '%U %a' "$LOCAL_ENV") =~ ^root\ 6[04]0$ ]] &&
		grep -qE '^HANGAR_BOOTSTRAP_ADMIN=[^[:space:]]+@[^[:space:]]+$' "$LOCAL_ENV"
}

# services_admin prints the bootstrap admin's email from the local env file.
services_admin() {
	sed -n 's/^HANGAR_BOOTSTRAP_ADMIN=//p' "$LOCAL_ENV" | tail -n 1 | tr -d "\"'"
}

services_user_ok() {
	local uid shell
	uid=$(id -u "$SERVICES_USER" 2>/dev/null) || return 1
	shell=$(getent passwd "$SERVICES_USER" | cut -d: -f7)
	[ "$uid" -lt 1000 ] && [ "$uid" != 0 ] && [ "$shell" = /usr/sbin/nologin ]
}

services_state_ok() {
	[ "$(stat -c '%U %G %a' "$SERVICES_STATE")" = "$SERVICES_USER $SERVICES_USER 700" ]
}

# services_stale UNIT holds when the running service is not what is
# installed: its unit changed since it started, or its binary was replaced.
services_stale() {
	local pid exe
	[ ! -e "$(services_marker "$1")" ] || return 0
	[[ " ${RELEASE_REPLACED:-} " == *" $1 "* ]] && return 0
	pid=$(systemctl show -p MainPID --value "$1.service")
	[ "$pid" != 0 ] || return 1
	exe=$(readlink "/proc/$pid/exe") || return 1
	[ "$exe" != "/usr/local/bin/$1" ]
}

services_units_ok() {
	local u
	for u in $SERVICES_UNITS; do
		services_unit "$u" | cmp -s - "$(services_unit_path "$u")" || return 1
		[ "$(systemctl show -p NeedDaemonReload --value "$u.service")" = no ] || return 1
	done
}

# services_dropins prints every drop-in for the hangar units, on disk or
# loaded: one would change what runs without changing the unit verify compares.
services_dropins() {
	local u d
	for u in $SERVICES_UNITS; do
		for d in /etc /run /usr/local/lib /usr/lib /lib; do
			ls -d "$d/systemd/system/$u.service.d" 2>/dev/null || true
		done
		systemctl show -p DropInPaths --value "$u.service" | tr ' ' '\n'
	done | { grep . || true; } | sort -u
}

services_no_dropins() { [ -z "$(services_dropins)" ]; }

services_running_ok() {
	local u
	for u in $SERVICES_UNITS; do
		systemctl is-enabled --quiet "$u.service" && systemctl is-active --quiet "$u.service" || return 1
		! services_stale "$u" || return 1
	done
}

services_socket_ok() {
	[ -S "$SERVICES_SOCKET" ] && [ "$(stat -c '%U %a' "$SERVICES_SOCKET")" = "$SERVICES_USER 660" ]
}

# services_loopback_only holds when the apiserver's TCP listeners are all on
# loopback, and it has one.
services_loopback_only() {
	local pid addrs
	pid=$(systemctl show -p MainPID --value hangar-apiserver.service)
	addrs=$(ss -Hltnp | grep "pid=$pid," | awk '{print $4}')
	[ -n "$addrs" ] && ! grep -qvE '^(127\.[0-9.]+|\[::1\]):[0-9]+$' <<<"$addrs"
}

# services_node_ready asks the API, as the bootstrap admin, whether the node
# is Ready, for up to 30 s: an agent that just started takes a moment.
services_node_ready() {
	local cfg i ok=1
	cfg=$(mktemp)
	cat >"$cfg" <<EOF
currentContext: local
contexts:
  local: {endpoint: "http://$SERVICES_LISTEN", auth: dev, user: "$(services_admin)"}
EOF
	for i in $(seq 1 30); do
		if HANGAR_CONFIG=$cfg /usr/local/bin/hangar get node "$HANGAR_NODE_NAME" 2>/dev/null |
			grep -qE "^$HANGAR_NODE_NAME +True "; then
			ok=0
			break
		fi
		[ "$i" = 30 ] || sleep 1
	done
	rm -f "$cfg"
	return "$ok"
}

services_apply() {
	local u before reload=0
	if ! services_local_env_ok; then
		local how="write HANGAR_BOOTSTRAP_ADMIN=<email> to $LOCAL_ENV, root's, mode 0600 (docs/runbook/host-bootstrap.md)"
		[ "$DRY_RUN" = 1 ] || die "$LOCAL_ENV is missing or wrong: $how"
		log "warning: $LOCAL_ENV is missing or wrong; a real run refuses until you $how"
	fi

	if ! services_no_dropins; then
		local what
		what="drop-ins change the hangar units: $(services_dropins | tr '\n' ' ')"
		[ "$DRY_RUN" = 1 ] || die "$what; remove them (the units are generated in full by this step), run systemctl daemon-reload, then re-run"
		log "warning: $what; a real run refuses until they are removed"
	fi

	id -u "$SERVICES_USER" >/dev/null 2>&1 ||
		run "create system user $SERVICES_USER" useradd --system --user-group \
			--home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$SERVICES_USER"
	# Under --dry-run the zfs step may not have made the dataset yet.
	if [ ! -d "$SERVICES_STATE" ] || ! services_state_ok; then
		run "give $SERVICES_STATE to $SERVICES_USER, mode 700" \
			sh -c "chown $SERVICES_USER:$SERVICES_USER '$SERVICES_STATE' && chmod 700 '$SERVICES_STATE'"
	fi

	for u in $SERVICES_UNITS; do
		if [ ! -x "/usr/local/bin/$u" ]; then
			[ "$DRY_RUN" = 1 ] || die "/usr/local/bin/$u is not installed; the release step installs it"
		fi
		before=$CHANGES
		put_file "$(services_unit_path "$u")" 644 "$(services_unit "$u")"
		if [ "$CHANGES" != "$before" ]; then
			reload=1
			systemctl is-active --quiet "$u.service" &&
				run "mark $u for restart" sh -c "mkdir -p $STATE_DIR && touch $(services_marker "$u")"
		fi
		[ "$(systemctl show -p NeedDaemonReload --value "$u.service")" = no ] || reload=1
	done
	[ "$reload" = 0 ] || run "reload systemd for the hangar units" systemctl daemon-reload

	# The apiserver first: the agent dials its socket (and retries until it
	# is there).
	for u in $SERVICES_UNITS; do
		systemctl is-enabled --quiet "$u.service" || run "enable $u" systemctl enable "$u.service"
		if ! systemctl is-active --quiet "$u.service"; then
			run "start $u" sh -c "systemctl start $u.service && rm -f $(services_marker "$u")"
		elif services_stale "$u"; then
			run "restart $u" sh -c "systemctl restart $u.service && rm -f $(services_marker "$u")"
		fi
	done
}

services_verify() {
	check "$LOCAL_ENV root's, 0600 or 0640, with HANGAR_BOOTSTRAP_ADMIN" services_local_env_ok
	check "system user $SERVICES_USER" services_user_ok
	check "$SERVICES_STATE is $SERVICES_USER's, mode 700" services_state_ok
	check "units $SERVICES_UNITS as generated and loaded" services_units_ok
	check "no drop-ins for $SERVICES_UNITS" services_no_dropins
	check "units $SERVICES_UNITS enabled, active, running what is installed" services_running_ok
	check "node socket $SERVICES_SOCKET, $SERVICES_USER's, 660" services_socket_ok
	check "apiserver listens on loopback only" services_loopback_only
	check "node $HANGAR_NODE_NAME Ready (hangar get node)" services_node_ready
}
