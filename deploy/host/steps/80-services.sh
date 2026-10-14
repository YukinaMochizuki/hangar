# The apiserver and the node agent as systemd services. The apiserver runs as
# its own unprivileged user with its database on hangar/state and the public
# API on loopback, in the auth mode local.env asks for (dev when unset). Its
# two unix sockets, the node agent's and root's admin socket, are made by
# systemd (socket units) in /run/hangar, a directory root owns, so the
# apiserver can neither take root's socket nor replace either. The agent runs
# as root on this host's drivers. A unit changed, or a binary the release step
# replaced, restarts that service, here and nowhere else.

SERVICES_USER=hangar
SERVICES_STATE=/var/lib/hangar/state
SERVICES_LISTEN=127.0.0.1:8080
SERVICES_RUN=/run/hangar
SERVICES_SOCKET=$SERVICES_RUN/node.sock
SERVICES_ADMIN_SOCKET=$SERVICES_RUN/admin.sock
SERVICES_TMPFILES=/etc/tmpfiles.d/hangar.conf
# The ids each VM's Cloud Hypervisor runs as, one a VM, its uid and gid
# (DefaultVMUsers in internal/node/driver/chvm, which refuses to start on a
# host where anything else has one). Kept under this name in /etc/subuid
# and /etc/subgid, so useradd gives no user a subordinate range of them.
SERVICES_VM_IDS_NAME=hangar-vms
SERVICES_VM_IDS_FIRST=200000
SERVICES_VM_IDS_COUNT=65536
SERVICES_UNITS="hangar-apiserver hangar-node"
# The apiserver's sockets: socket units of their own, part of its service.
SERVICES_SOCKETS="hangar-apiserver-node hangar-apiserver-admin"

services_unit_path() { printf '/etc/systemd/system/%s.service\n' "$1"; }
services_socket_path() { printf '/etc/systemd/system/%s.socket\n' "$1"; }
services_marker() { printf '%s/restart-%s\n' "$STATE_DIR" "$1"; }

# services_mark UNIT records, once a run, that a running UNIT is to restart.
SERVICES_MARKED=
services_mark() {
	systemctl is-active --quiet "$1.service" || return 0
	[[ " $SERVICES_MARKED " != *" $1 "* ]] || return 0
	SERVICES_MARKED="$SERVICES_MARKED $1"
	run "mark $1 for restart" sh -c "mkdir -p $STATE_DIR && touch $(services_marker "$1")"
}

# services_reserved_disk is what root disks may not take from the pool: what
# the image cache and the VMs' directories may hold, their quotas.
services_reserved_disk() {
	printf '%dGi\n' $(($(gib_of "$HANGAR_IMAGES_QUOTA") + $(gib_of "$HANGAR_VMS_QUOTA")))
}

# services_auth_flags prints the apiserver's flags for the auth mode
# local.env asks for. The values land in the unit, so a changed mode is a
# changed unit and restarts the apiserver.
services_auth_flags() {
	case $(auth_mode) in
	cloudflare)
		printf -- '--auth-mode cloudflare --cf-team-domain %s --cf-audience %s\n' \
			"$(local_env HANGAR_CF_TEAM_DOMAIN)" "$(local_env HANGAR_CF_AUDIENCE)"
		;;
	*) printf -- '--auth-mode dev\n' ;;
	esac
}

services_vm_ids_line() {
	printf '%s:%d:%d\n' "$SERVICES_VM_IDS_NAME" "$SERVICES_VM_IDS_FIRST" "$SERVICES_VM_IDS_COUNT"
}

# services_vm_ids_file FILE prints FILE with the VMs' range kept, last, and
# no other line of that name.
services_vm_ids_file() {
	[ ! -f "$1" ] || grep -v "^$SERVICES_VM_IDS_NAME:" "$1" || true
	services_vm_ids_line
}

services_vm_ids_kept() {
	local f
	for f in /etc/subuid /etc/subgid; do
		services_vm_ids_file "$f" | cmp -s - "$f" || return 1
	done
}

# services_vm_ids_clash prints each user, group or subordinate range but
# the VMs' that has one of the VMs' ids.
services_vm_ids_clash() {
	local lo=$SERVICES_VM_IDS_FIRST hi=$((SERVICES_VM_IDS_FIRST + SERVICES_VM_IDS_COUNT))
	getent passwd | awk -F: -v lo="$lo" -v hi="$hi" \
		'($3 >= lo && $3 < hi) || ($4 >= lo && $4 < hi) { print "user " $1 }'
	getent group | awk -F: -v lo="$lo" -v hi="$hi" '$3 >= lo && $3 < hi { print "group " $1 }'
	local f
	for f in /etc/subuid /etc/subgid; do
		[ -f "$f" ] || continue
		awk -F: -v lo="$lo" -v hi="$hi" -v me="$SERVICES_VM_IDS_NAME" -v f="$f" \
			'NF == 3 && $1 != me && $2 < hi && lo < $2 + $3 { print f " range of " $1 }' "$f"
	done
}

services_vm_ids_free() { [ -z "$(services_vm_ids_clash)" ]; }

services_vm_ids_problem() {
	printf "the VMs' ids %d to %d are taken: %s; move each to other ids (usermod, groupmod, or its line in /etc/subuid or /etc/subgid), then re-run\n" \
		"$SERVICES_VM_IDS_FIRST" $((SERVICES_VM_IDS_FIRST + SERVICES_VM_IDS_COUNT - 1)) "$(services_vm_ids_clash | paste -sd, - | sed 's/,/, /g')"
}

services_tmpfiles() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh. Root's, so the apiserver, which
# runs as $SERVICES_USER, cannot remove or replace the sockets systemd makes here.
d $SERVICES_RUN 0755 root root -
EOF
}

services_socket_hangar_apiserver_node() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh.
[Unit]
Description=Hangar API server: the node agent's socket
# Stopped and restarted with the apiserver, so a stopped apiserver is not
# started again by the agent dialing it.
PartOf=hangar-apiserver.service

[Socket]
ListenStream=$SERVICES_SOCKET
FileDescriptorName=node
Service=hangar-apiserver.service
# The agent runs as root; the group is for nothing else.
SocketUser=$SERVICES_USER
SocketGroup=$SERVICES_USER
SocketMode=0660
EOF
}

services_socket_hangar_apiserver_admin() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh.
[Unit]
Description=Hangar API server: root's admin socket
PartOf=hangar-apiserver.service

[Socket]
ListenStream=$SERVICES_ADMIN_SOCKET
FileDescriptorName=admin
Service=hangar-apiserver.service
# The apiserver also checks the peer is uid 0; this keeps everyone else from
# even connecting.
SocketUser=root
SocketGroup=root
SocketMode=0600
EOF
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
# The public API is on loopback in either auth mode: cloudflared reaches it
# there. The node socket accepts root's agent; the admin socket, root.
ExecStart=/usr/local/bin/hangar-apiserver --db $SERVICES_STATE/hangar.db --listen $SERVICES_LISTEN $(services_auth_flags) --socket $SERVICES_SOCKET --admin-socket $SERVICES_ADMIN_SOCKET --node-uid 0 --node-name $HANGAR_NODE_NAME --bootstrap-admin \${HANGAR_BOOTSTRAP_ADMIN}
Sockets=$(for s in $SERVICES_SOCKETS; do printf '%s.socket ' "$s"; done | sed 's/ $//')
Restart=on-failure
RestartSec=2
UMask=0077

# It needs its database, the two sockets systemd passes it, and nothing else.
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
ExecStart=/usr/local/bin/hangar-node --drivers=host --name $HANGAR_NODE_NAME --uplink $HANGAR_UPLINK --public-ipv4 $HANGAR_PUBLIC_IPV4 --public-ipv6-prefix $(ipv6_prefix "$HANGAR_IPV6_ADDR") --reserved-disk $(services_reserved_disk) --reserved-memory $HANGAR_RESERVED_MEMORY
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
services_socket() { "services_socket_${1//-/_}"; }

services_local_env_ok() {
	[ -f "$LOCAL_ENV" ] && [[ $(stat -c '%U %a' "$LOCAL_ENV") =~ ^root\ 6[04]0$ ]] &&
		grep -qE '^HANGAR_BOOTSTRAP_ADMIN=[^[:space:]]+@[^[:space:]]+$' "$LOCAL_ENV"
}

services_local_env_how() {
	printf '%s\n' "write HANGAR_BOOTSTRAP_ADMIN=<email> to $LOCAL_ENV, root's, mode 0600 (docs/runbook/host-bootstrap.md)"
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
# installed: its unit changed since it started, its binary was replaced, or,
# for the apiserver, one of its sockets is down.
services_stale() {
	local pid exe s
	[ ! -e "$(services_marker "$1")" ] || return 0
	# systemd refuses to start a socket whose service runs, so a socket
	# restarted alone is down until the apiserver restarts with it.
	if [ "$1" = hangar-apiserver ]; then
		for s in $SERVICES_SOCKETS; do
			systemctl is-active --quiet "$s.socket" || return 0
		done
	fi
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
	for u in $SERVICES_SOCKETS; do
		services_socket "$u" | cmp -s - "$(services_socket_path "$u")" || return 1
		[ "$(systemctl show -p NeedDaemonReload --value "$u.socket")" = no ] || return 1
	done
	services_tmpfiles | cmp -s - "$SERVICES_TMPFILES"
}

services_run_ok() {
	[ "$(stat -c '%U %G %a' "$SERVICES_RUN")" = "root root 755" ]
}

# services_dropins prints every drop-in for the hangar units, on disk or
# loaded: one would change what runs without changing the unit verify compares.
services_dropins() {
	local u d units=()
	for u in $SERVICES_UNITS; do units+=("$u.service"); done
	for u in $SERVICES_SOCKETS; do units+=("$u.socket"); done
	for u in "${units[@]}"; do
		for d in /etc /run /usr/local/lib /usr/lib /lib; do
			ls -d "$d/systemd/system/$u.d" 2>/dev/null || true
		done
		systemctl show -p DropInPaths --value "$u" | tr ' ' '\n'
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

services_admin_socket_ok() {
	[ -S "$SERVICES_ADMIN_SOCKET" ] && [ "$(stat -c '%U %G %a' "$SERVICES_ADMIN_SOCKET")" = "root root 600" ]
}

# services_auth_running_ok holds when the apiserver runs the auth flags
# local.env asks for.
services_auth_running_ok() {
	local pid want
	pid=$(systemctl show -p MainPID --value hangar-apiserver.service)
	[ "$pid" != 0 ] || return 1
	want=$(services_auth_flags)
	tr '\0' ' ' <"/proc/$pid/cmdline" | grep -qF -- " $want "
}

# services_loopback_only holds when the apiserver's TCP listeners are all on
# loopback, and it has one.
services_loopback_only() {
	local pid addrs
	pid=$(systemctl show -p MainPID --value hangar-apiserver.service)
	addrs=$(ss -Hltnp | grep "pid=$pid," | awk '{print $4}')
	[ -n "$addrs" ] && ! grep -qvE '^(127\.[0-9.]+|\[::1\]):[0-9]+$' <<<"$addrs"
}

# services_node_ready asks the API, as root on the host through the admin
# socket (whatever the public listener's auth), whether the node is Ready, for
# up to 30 s: an agent that just started takes a moment.
services_node_ready() {
	local cfg i ok=1
	cfg=$(mktemp)
	cat >"$cfg" <<EOF
currentContext: local
contexts:
  local: {endpoint: "unix://$SERVICES_ADMIN_SOCKET", auth: local}
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
		local how
		how=$(services_local_env_how)
		[ "$DRY_RUN" = 1 ] || die "$LOCAL_ENV is missing or wrong: $how"
		log "warning: $LOCAL_ENV is missing or wrong; a real run refuses until you $how"
	fi
	local problem
	problem=$(auth_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	# cloudflared never forwards to an apiserver in dev auth, which trusts a
	# header: it goes before the apiserver could restart in dev.
	[ "$(auth_mode)" = cloudflare ] || cloudflared_down "the auth mode is dev"

	if ! services_no_dropins; then
		local what
		what="drop-ins change the hangar units: $(services_dropins | tr '\n' ' ')"
		[ "$DRY_RUN" = 1 ] || die "$what; remove them (the units are generated in full by this step), run systemctl daemon-reload, then re-run"
		log "warning: $what; a real run refuses until they are removed"
	fi

	services_vm_ids_free || refuse "$(services_vm_ids_problem)"
	put_file /etc/subuid 644 "$(services_vm_ids_file /etc/subuid)"
	put_file /etc/subgid 644 "$(services_vm_ids_file /etc/subgid)"

	id -u "$SERVICES_USER" >/dev/null 2>&1 ||
		run "create system user $SERVICES_USER" useradd --system --user-group \
			--home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$SERVICES_USER"
	# Under --dry-run the zfs step may not have made the dataset yet.
	if [ ! -d "$SERVICES_STATE" ] || ! services_state_ok; then
		run "give $SERVICES_STATE to $SERVICES_USER, mode 700" \
			sh -c "chown $SERVICES_USER:$SERVICES_USER '$SERVICES_STATE' && chmod 700 '$SERVICES_STATE'"
	fi

	put_file "$SERVICES_TMPFILES" 644 "$(services_tmpfiles)"
	# A runtime directory the apiserver once had is its; tmpfiles gives it
	# to root.
	if [ ! -d "$SERVICES_RUN" ] || ! services_run_ok; then
		run "give $SERVICES_RUN to root, mode 755" systemd-tmpfiles --create "$SERVICES_TMPFILES"
	fi

	# A socket is part of the apiserver: a changed one restarts it, and the
	# restart restarts the socket.
	for u in $SERVICES_SOCKETS; do
		before=$CHANGES
		put_file "$(services_socket_path "$u")" 644 "$(services_socket "$u")"
		if [ "$CHANGES" != "$before" ]; then
			reload=1
			services_mark hangar-apiserver
		fi
		[ "$(systemctl show -p NeedDaemonReload --value "$u.socket")" = no ] || reload=1
	done
	for u in $SERVICES_UNITS; do
		if [ ! -x "/usr/local/bin/$u" ]; then
			[ "$DRY_RUN" = 1 ] || die "/usr/local/bin/$u is not installed; the release step installs it"
		fi
		before=$CHANGES
		put_file "$(services_unit_path "$u")" 644 "$(services_unit "$u")"
		if [ "$CHANGES" != "$before" ]; then
			reload=1
			services_mark "$u"
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
	check "the VMs' ids $SERVICES_VM_IDS_FIRST+$SERVICES_VM_IDS_COUNT kept in /etc/subuid and /etc/subgid" services_vm_ids_kept
	check "no user, group or other subordinate range has one of the VMs' ids" services_vm_ids_free
	check "$SERVICES_STATE is $SERVICES_USER's, mode 700" services_state_ok
	check "units $SERVICES_UNITS, sockets $SERVICES_SOCKETS and $SERVICES_TMPFILES as generated and loaded" services_units_ok
	check "no drop-ins for $SERVICES_UNITS $SERVICES_SOCKETS" services_no_dropins
	check "units $SERVICES_UNITS enabled, active, running what is installed" services_running_ok
	check "$SERVICES_RUN root's, 755" services_run_ok
	check "node socket $SERVICES_SOCKET, $SERVICES_USER's, 660" services_socket_ok
	check "admin socket $SERVICES_ADMIN_SOCKET, root's, 600" services_admin_socket_ok
	check "apiserver runs $(auth_mode) auth as $LOCAL_ENV asks: $(services_auth_flags)" services_auth_running_ok
	check "apiserver listens on loopback only" services_loopback_only
	check "node $HANGAR_NODE_NAME Ready (hangar get node)" services_node_ready
}
