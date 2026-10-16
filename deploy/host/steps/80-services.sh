# The apiserver, the node agent and the edge as systemd services. The
# apiserver runs as its own unprivileged user with its database on
# hangar/state and the public API on loopback, in the auth mode local.env
# asks for (dev when unset). Its three unix sockets, the node agent's, the
# edge's and root's admin socket, are made by systemd (socket units) in
# /run/hangar, a directory root owns, so the apiserver can neither take
# root's socket nor replace any. The agent runs as root on this host's
# drivers, and makes the edge's dial socket there itself. The edge
# (hangar-edge) runs as a user of its own, on loopback, and only when
# local.env names the public endpoints' domain (docs/runbook/cloudflare.md).
# A unit changed, or a binary the release step replaced, restarts that
# service, here and nowhere else.

SERVICES_USER=hangar
SERVICES_STATE=/var/lib/hangar/state
SERVICES_LISTEN=127.0.0.1:8080
SERVICES_RUN=/run/hangar
SERVICES_SOCKET=$SERVICES_RUN/node.sock
SERVICES_ADMIN_SOCKET=$SERVICES_RUN/admin.sock
# The edge: its user, the apiserver's socket it reads the API on, the agent's
# socket it reaches machines through, and where cloudflared sends it
# requests (converge.EdgeOrigin in internal/cloudflare/converge).
SERVICES_EDGE_USER=hangar-edge
SERVICES_EDGE_SOCKET=$SERVICES_RUN/edge.sock
SERVICES_EDGE_DIAL=$SERVICES_RUN/edge-dial.sock
SERVICES_EDGE_LISTEN=127.0.0.1:8090
# The Cloudflare API token the apiserver opens PUBLIC endpoints with: the
# operator installs it (docs/runbook/cloudflare.md); this step never makes
# one.
SERVICES_CF_TOKEN=$SERVICES_STATE/cloudflare-api-token
# The OAuth client's secret the apiserver signs people in with, in oidc
# auth: the operator installs it (docs/runbook/host-bootstrap.md); this step
# never makes one.
SERVICES_OIDC_SECRET=$SERVICES_STATE/oidc-client-secret
# The smoke tests' service token, smoke/smoke: the operator makes it with
# hangar token create (docs/runbook/host-bootstrap.md); this step never
# makes one, and only checks it when it is there.
SERVICES_SMOKE_TOKEN=/etc/hangar/smoke-token
SERVICES_TMPFILES=/etc/tmpfiles.d/hangar.conf
# The platform's SSH CA, its private key: the operator installs it from the
# vault (docs/runbook/host-bootstrap.md); this step never makes one.
SERVICES_SSH_CA=$SERVICES_STATE/ssh-ca
# The key-encryption key Secrets are sealed under (docs/decisions/secrets.md).
# Unlike the SSH CA, this step makes it if it is missing, so Secrets work on a
# new host; the operator then copies it to the vault, since a backup of the
# database is of no use without it.
SERVICES_SECRETS_KEK=$SERVICES_STATE/secrets-kek
# The key a rotation replaces, there only while one is under way; the
# apiserver opens what it sealed and seals it again under the new one.
SERVICES_SECRETS_KEK_PREVIOUS=$SERVICES_SECRETS_KEK.previous
# The ids each VM's Cloud Hypervisor runs as, one a VM, its uid and gid
# (DefaultVMUsers in internal/node/driver/chvm, which refuses to start on a
# host where anything else has one). Kept under this name in /etc/subuid
# and /etc/subgid, so useradd gives no user a subordinate range of them.
SERVICES_VM_IDS_NAME=hangar-vms
SERVICES_VM_IDS_FIRST=200000
SERVICES_VM_IDS_COUNT=65536
SERVICES_UNITS="hangar-apiserver hangar-node hangar-edge"
# The apiserver's sockets: socket units of their own, part of its service.
SERVICES_SOCKETS="hangar-apiserver-node hangar-apiserver-admin hangar-apiserver-edge"

# services_edge_on holds when local.env names the public endpoints' domain:
# then the edge runs.
services_edge_on() { [ -n "$(local_env HANGAR_EDGE_DOMAIN)" ]; }

# services_units prints the units this host runs: the edge only when it is
# on.
services_units() {
	local u
	for u in $SERVICES_UNITS; do
		[ "$u" != hangar-edge ] || services_edge_on || continue
		printf '%s\n' "$u"
	done
}

# services_edge_uid prints the edge user's uid, or nothing before the user
# exists (a dry run on a fresh host).
services_edge_uid() { id -u "$SERVICES_EDGE_USER" 2>/dev/null || true; }

# services_endpoint_flags prints the apiserver's flags for public
# endpoints: their domain, and with the Cloudflare API token, the account
# and the bypass policy hangar-cloudflare made.
services_endpoint_flags() {
	local domain account bypass
	domain=$(local_env HANGAR_EDGE_DOMAIN)
	account=$(local_env HANGAR_CF_ACCOUNT_ID)
	bypass=$(local_env HANGAR_CF_BYPASS_POLICY_ID)
	printf -- '--edge-socket %s --edge-uid %s --cloudflare-api-token-file %s' \
		"$SERVICES_EDGE_SOCKET" "$(services_edge_uid)" "$SERVICES_CF_TOKEN"
	[ -z "$domain" ] || printf -- ' --endpoint-domain %s' "$domain"
	[ -z "$account" ] || printf -- ' --cloudflare-account-id %s' "$account"
	[ -z "$bypass" ] || printf -- ' --cloudflare-bypass-policy-id %s' "$bypass"
	printf '\n'
}

# services_endpoints_problem prints what is wrong with local.env's public
# endpoint settings, nothing when they are whole or absent.
services_endpoints_problem() {
	services_edge_on || return 0
	[ "$(auth_mode)" = oidc ] ||
		printf '%s\n' "public endpoints (HANGAR_EDGE_DOMAIN) need oidc auth: the tunnel that brings their traffic never runs in dev auth"
	[[ $(local_env HANGAR_CF_TEAM_DOMAIN) =~ ^https://[A-Za-z0-9.-]+$ ]] ||
		printf '%s\n' "public endpoints need HANGAR_CF_TEAM_DOMAIN=https://<team>.cloudflareaccess.com in $LOCAL_ENV: the edge checks Access tokens"
	[[ $(local_env HANGAR_EDGE_DOMAIN) =~ ^[a-z0-9.-]+$ ]] ||
		printf '%s\n' "HANGAR_EDGE_DOMAIN in $LOCAL_ENV is not a domain"
	[[ $(local_env HANGAR_EDGE_CF_AUDIENCE) =~ ^[A-Za-z0-9]+$ ]] ||
		printf '%s\n' "public endpoints need HANGAR_EDGE_CF_AUDIENCE=<the wildcard Access application's AUD tag, from edge.env> in $LOCAL_ENV"
	[[ $(local_env HANGAR_CF_ACCOUNT_ID) =~ ^[0-9a-f]{32}$ ]] ||
		printf '%s\n' "public endpoints need HANGAR_CF_ACCOUNT_ID=<from edge.env> in $LOCAL_ENV"
	[[ $(local_env HANGAR_CF_BYPASS_POLICY_ID) =~ ^[0-9a-f-]+$ ]] ||
		printf '%s\n' "public endpoints need HANGAR_CF_BYPASS_POLICY_ID=<from edge.env> in $LOCAL_ENV"
}

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
	oidc)
		printf -- '--auth-mode oidc --oidc-client-id %s --oidc-client-secret-file %s --public-url %s' \
			"$(local_env HANGAR_OIDC_CLIENT_ID)" "$SERVICES_OIDC_SECRET" "$(local_env HANGAR_PUBLIC_URL)"
		[ -z "$(local_env HANGAR_OIDC_ISSUER)" ] || printf -- ' --oidc-issuer %s' "$(local_env HANGAR_OIDC_ISSUER)"
		printf '\n'
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

services_socket_hangar_apiserver_edge() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh.
[Unit]
Description=Hangar API server: the edge's socket
PartOf=hangar-apiserver.service

[Socket]
ListenStream=$SERVICES_EDGE_SOCKET
FileDescriptorName=edge
Service=hangar-apiserver.service
# The apiserver also checks the peer is the edge's uid; this keeps everyone
# else from even connecting.
SocketUser=root
SocketGroup=$SERVICES_EDGE_USER
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
ExecStart=/usr/local/bin/hangar-apiserver --db $SERVICES_STATE/hangar.db --listen $SERVICES_LISTEN $(services_auth_flags) --socket $SERVICES_SOCKET --admin-socket $SERVICES_ADMIN_SOCKET --node-uid 0 --node-name $HANGAR_NODE_NAME --bootstrap-admin \${HANGAR_BOOTSTRAP_ADMIN} --ssh-ca-key $SERVICES_SSH_CA $(services_endpoint_flags) --secrets-kek $SERVICES_SECRETS_KEK --secrets-kek-previous $SERVICES_SECRETS_KEK_PREVIOUS --default-max-owned-tenants $HANGAR_DEFAULT_MAX_OWNED_TENANTS
Sockets=$(for s in $SERVICES_SOCKETS; do printf '%s.socket ' "$s"; done | sed 's/ $//')
Restart=on-failure
RestartSec=2
UMask=0077

# It needs its database, the sockets systemd passes it, and nothing else.
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
ExecStart=/usr/local/bin/hangar-node --drivers=host --name $HANGAR_NODE_NAME --uplink $HANGAR_UPLINK --public-ipv4 $HANGAR_PUBLIC_IPV4 --public-ipv6-prefix $(ipv6_prefix "$HANGAR_IPV6_ADDR") --reserved-disk $(services_reserved_disk) --reserved-memory $HANGAR_RESERVED_MEMORY --edge-dial-socket $SERVICES_EDGE_DIAL --edge-user $SERVICES_EDGE_USER
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

services_unit_hangar_edge() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh.
[Unit]
Description=Hangar edge: public endpoints
After=network.target hangar-apiserver.service hangar-node.service

[Service]
Type=exec
User=$SERVICES_EDGE_USER
Group=$SERVICES_EDGE_USER
# cloudflared sends it every request for a public endpoint, on loopback; it
# reads the API on the apiserver's edge socket and reaches machines through
# the agent's dial socket, both its group's.
ExecStart=/usr/local/bin/hangar-edge --listen $SERVICES_EDGE_LISTEN --api-socket $SERVICES_EDGE_SOCKET --dial-socket $SERVICES_EDGE_DIAL --domain $(local_env HANGAR_EDGE_DOMAIN) --cf-team-domain $(local_env HANGAR_CF_TEAM_DOMAIN) --cf-audience $(local_env HANGAR_EDGE_CF_AUDIENCE)
Restart=always
RestartSec=2
UMask=0077

# It needs its two sockets and the network, nothing else.
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
	local user=${1:-$SERVICES_USER} uid shell
	uid=$(id -u "$user" 2>/dev/null) || return 1
	shell=$(getent passwd "$user" | cut -d: -f7)
	[ "$uid" -lt 1000 ] && [ "$uid" != 0 ] && [ "$shell" = /usr/sbin/nologin ]
}

services_cf_token_ok() {
	[ -f "$SERVICES_CF_TOKEN" ] && [ "$(stat -c '%U %G %a' "$SERVICES_CF_TOKEN")" = "$SERVICES_USER $SERVICES_USER 600" ]
}

services_oidc_secret_ok() {
	[ -f "$SERVICES_OIDC_SECRET" ] && [ "$(stat -c '%U %G %a' "$SERVICES_OIDC_SECRET")" = "$SERVICES_USER $SERVICES_USER 600" ]
}

services_oidc_secret_how() {
	printf '%s\n' "install the OAuth client's secret at $SERVICES_OIDC_SECRET, $SERVICES_USER's, mode 0600 (docs/runbook/host-bootstrap.md)"
}

services_cf_token_how() {
	printf '%s\n' "install the Cloudflare API token at $SERVICES_CF_TOKEN, $SERVICES_USER's, mode 0600, then restart hangar-apiserver (docs/runbook/cloudflare.md)"
}

services_state_ok() {
	[ "$(stat -c '%U %G %a' "$SERVICES_STATE")" = "$SERVICES_USER $SERVICES_USER 700" ]
}

# services_smoke_token_ok checks the smoke token is root's, 0600,
# authenticates as smoke/smoke on the public listener, and holds what smoke
# needs there: listing the smoke project's containers. hangar reads it from
# the file; it never passes through an argument or the environment.
services_smoke_token_ok() {
	local cfg out ok=1
	[ -f "$SERVICES_SMOKE_TOKEN" ] && [ "$(stat -c '%U %G %a' "$SERVICES_SMOKE_TOKEN")" = "root root 600" ] || return 1
	cfg=$(mktemp)
	cat >"$cfg" <<EOF
currentContext: smoke
contexts:
  smoke: {endpoint: "http://$SERVICES_LISTEN", auth: token, tokenFile: "$SERVICES_SMOKE_TOKEN", project: smoke}
EOF
	out=$(HANGAR_CONFIG=$cfg HANGAR_TOKEN='' /usr/local/bin/hangar whoami 2>/dev/null) || true
	if [ "$out" = "serviceToken:smoke/smoke" ] &&
		HANGAR_CONFIG=$cfg HANGAR_TOKEN='' /usr/local/bin/hangar get containers -o name >/dev/null 2>&1; then
		ok=0
	fi
	rm -f "$cfg"
	return "$ok"
}

services_ssh_ca_ok() {
	[ -f "$SERVICES_SSH_CA" ] && [ "$(stat -c '%U %G %a' "$SERVICES_SSH_CA")" = "$SERVICES_USER $SERVICES_USER 600" ]
}

services_secrets_kek_ok() {
	[ -f "$SERVICES_SECRETS_KEK" ] && [ "$(stat -c '%U %G %a' "$SERVICES_SECRETS_KEK")" = "$SERVICES_USER $SERVICES_USER 600" ]
}

services_secrets_kek_previous_ok() {
	[ "$(stat -c '%U %G %a' "$SERVICES_SECRETS_KEK_PREVIOUS")" = "$SERVICES_USER $SERVICES_USER 600" ]
}

# services_make_secrets_kek writes a new key-encryption key, an id and 32
# random bytes in base64, its owner's alone from the start, and never shows
# it.
services_make_secrets_kek() {
	local tmp
	tmp=$(umask 077 && mktemp "$SERVICES_SECRETS_KEK.XXXXXX") || return 1
	if ! printf 'kek-%s %s\n' "$(date -u +%Y%m%d%H%M%S)" "$(head -c 32 /dev/urandom | base64 -w0)" >"$tmp" ||
		! chown "$SERVICES_USER:$SERVICES_USER" "$tmp" || ! chmod 600 "$tmp" || ! mv -n "$tmp" "$SERVICES_SECRETS_KEK"; then
		rm -f "$tmp"
		return 1
	fi
	# mv -n leaves tmp if a key appeared meanwhile: that one is kept.
	rm -f "$tmp"
}

services_ssh_ca_how() {
	printf '%s\n' "install the SSH CA's key from the vault at $SERVICES_SSH_CA, $SERVICES_USER's, mode 0600, then restart hangar-apiserver (docs/runbook/host-bootstrap.md)"
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
	for u in $(services_units); do
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
	for u in $(services_units); do units+=("$u.service"); done
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
	for u in $(services_units); do
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

services_edge_socket_ok() {
	[ -S "$SERVICES_EDGE_SOCKET" ] && [ "$(stat -c '%U %G %a' "$SERVICES_EDGE_SOCKET")" = "root $SERVICES_EDGE_USER 660" ]
}

services_edge_dial_ok() {
	[ -S "$SERVICES_EDGE_DIAL" ] && [ "$(stat -c '%U %G %a' "$SERVICES_EDGE_DIAL")" = "root $SERVICES_EDGE_USER 660" ]
}

# services_edge_off_ok holds when the edge, off, neither runs nor starts at
# boot.
services_edge_off_ok() {
	! systemctl is-active --quiet hangar-edge.service && ! systemctl is-enabled --quiet hangar-edge.service 2>/dev/null
}

# services_edge_loopback_only holds when the edge's TCP listeners are all on
# loopback, and it has one.
services_edge_loopback_only() {
	local pid addrs
	pid=$(systemctl show -p MainPID --value hangar-edge.service)
	addrs=$(ss -Hltnp | grep "pid=$pid," | awk '{print $4}')
	[ -n "$addrs" ] && ! grep -qvE '^(127\.[0-9.]+|\[::1\]):[0-9]+$' <<<"$addrs"
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
	problem=$(auth_problem; services_endpoints_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	# cloudflared never forwards to an apiserver in dev auth, which trusts a
	# header: it goes before the apiserver could restart in dev.
	[ "$(auth_mode)" = oidc ] || cloudflared_down "the auth mode is dev"
	# In oidc auth the apiserver does not start without its client secret,
	# which is the operator's to install, never this step's to make.
	if [ "$(auth_mode)" = oidc ]; then
		if [ ! -f "$SERVICES_OIDC_SECRET" ]; then
			refuse "no OAuth client secret at $SERVICES_OIDC_SECRET: $(services_oidc_secret_how)"
		elif ! services_oidc_secret_ok; then
			log "warning: $SERVICES_OIDC_SECRET was $(stat -c '%U:%G mode %a' "$SERVICES_OIDC_SECRET"): whoever could read it may have the secret; consider rolling it at the provider"
			run "give $SERVICES_OIDC_SECRET to $SERVICES_USER, mode 600" \
				sh -c "chown $SERVICES_USER:$SERVICES_USER '$SERVICES_OIDC_SECRET' && chmod 600 '$SERVICES_OIDC_SECRET'"
		fi
	fi

	if ! services_no_dropins; then
		local what
		what="drop-ins change the hangar units: $(services_dropins | tr '\n' ' ')"
		[ "$DRY_RUN" = 1 ] || die "$what; remove them (the units are generated in full by this step), run systemctl daemon-reload, then re-run"
		log "warning: $what; a real run refuses until they are removed"
	fi

	services_vm_ids_free || refuse "$(services_vm_ids_problem)"
	put_file /etc/subuid 644 "$(services_vm_ids_file /etc/subuid)"
	put_file /etc/subgid 644 "$(services_vm_ids_file /etc/subgid)"

	for u in "$SERVICES_USER" "$SERVICES_EDGE_USER"; do
		id -u "$u" >/dev/null 2>&1 ||
			run "create system user $u" useradd --system --user-group \
				--home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$u"
	done
	# Under --dry-run the zfs step may not have made the dataset yet.
	if [ ! -d "$SERVICES_STATE" ] || ! services_state_ok; then
		run "give $SERVICES_STATE to $SERVICES_USER, mode 700" \
			sh -c "chown $SERVICES_USER:$SERVICES_USER '$SERVICES_STATE' && chmod 700 '$SERVICES_STATE'"
	fi

	# Without the CA the apiserver runs, and hangar ssh cannot sign: the key
	# is the operator's to install, never this step's to make.
	if [ ! -f "$SERVICES_SSH_CA" ]; then
		log "warning: no SSH CA at $SERVICES_SSH_CA: hangar ssh cannot sign until you $(services_ssh_ca_how)"
	elif ! services_ssh_ca_ok; then
		log "warning: $SERVICES_SSH_CA was $(stat -c '%U:%G mode %a' "$SERVICES_SSH_CA"): whoever could read it may have the SSH CA's key; consider a new one (docs/runbook/host-bootstrap.md)"
		run "give $SERVICES_SSH_CA to $SERVICES_USER, mode 600" \
			sh -c "chown $SERVICES_USER:$SERVICES_USER '$SERVICES_SSH_CA' && chmod 600 '$SERVICES_SSH_CA'"
	fi
	# Without the Cloudflare token the apiserver runs, and PUBLIC endpoints
	# do not open.
	if [ ! -f "$SERVICES_CF_TOKEN" ]; then
		! services_edge_on || log "warning: no Cloudflare API token at $SERVICES_CF_TOKEN: PUBLIC endpoints do not open until you $(services_cf_token_how)"
	elif ! services_cf_token_ok; then
		log "warning: $SERVICES_CF_TOKEN was $(stat -c '%U:%G mode %a' "$SERVICES_CF_TOKEN"): whoever could read it may have the token; consider rolling it (docs/runbook/cloudflare.md)"
		run "give $SERVICES_CF_TOKEN to $SERVICES_USER, mode 600" \
			sh -c "chown $SERVICES_USER:$SERVICES_USER '$SERVICES_CF_TOKEN' && chmod 600 '$SERVICES_CF_TOKEN'"
	fi

	# The key-encryption key is made here once and never again: a new one
	# would leave every stored Secret unopenable, and the apiserver would
	# refuse to start (docs/runbook/host-bootstrap.md says how to rotate it).
	if [ ! -e "$SERVICES_SECRETS_KEK" ]; then
		run "make the secrets key-encryption key at $SERVICES_SECRETS_KEK, $SERVICES_USER's, mode 600" services_make_secrets_kek
		[ "$DRY_RUN" = 1 ] ||
			log "note: copy $SERVICES_SECRETS_KEK to the vault now: without it a backup of the database holds no usable Secret (docs/runbook/host-bootstrap.md)"
	elif ! services_secrets_kek_ok; then
		log "warning: $SERVICES_SECRETS_KEK was $(stat -c '%U:%G mode %a' "$SERVICES_SECRETS_KEK"): whoever could read it can open every Secret; consider rotating it (docs/runbook/host-bootstrap.md)"
		run "give $SERVICES_SECRETS_KEK to $SERVICES_USER, mode 600" \
			sh -c "chown $SERVICES_USER:$SERVICES_USER '$SERVICES_SECRETS_KEK' && chmod 600 '$SERVICES_SECRETS_KEK'"
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
	for u in $(services_units); do
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

	# The edge off: it neither runs nor starts at boot. Its unit is left as
	# it was, for when it is on again.
	if ! services_edge_on && ! services_edge_off_ok; then
		run "stop and disable hangar-edge: $LOCAL_ENV names no public endpoint domain" \
			systemctl disable --now hangar-edge.service
	fi
	# The apiserver first: the agent dials its socket (and retries until it
	# is there), and the edge both.
	for u in $(services_units); do
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
	check "system user $SERVICES_EDGE_USER" services_user_ok "$SERVICES_EDGE_USER"
	check "the VMs' ids $SERVICES_VM_IDS_FIRST+$SERVICES_VM_IDS_COUNT kept in /etc/subuid and /etc/subgid" services_vm_ids_kept
	check "no user, group or other subordinate range has one of the VMs' ids" services_vm_ids_free
	check "$SERVICES_STATE is $SERVICES_USER's, mode 700" services_state_ok
	if [ -e "$SERVICES_SSH_CA" ]; then
		check "SSH CA key $SERVICES_SSH_CA, $SERVICES_USER's, mode 600 (hangar ssh)" services_ssh_ca_ok
	else
		skip "SSH CA" "no $SERVICES_SSH_CA: hangar ssh cannot sign"
	fi
	check "secrets key-encryption key $SERVICES_SECRETS_KEK, $SERVICES_USER's, mode 600" services_secrets_kek_ok
	if [ -e "$SERVICES_SECRETS_KEK_PREVIOUS" ]; then
		check "previous key-encryption key $SERVICES_SECRETS_KEK_PREVIOUS, $SERVICES_USER's, mode 600 (a rotation under way)" services_secrets_kek_previous_ok
	fi
	local units
	units=$(services_units | tr '\n' ' ' | sed 's/ $//')
	check "units $units, sockets $SERVICES_SOCKETS and $SERVICES_TMPFILES as generated and loaded" services_units_ok
	check "no drop-ins for $units $SERVICES_SOCKETS" services_no_dropins
	check "units $units enabled, active, running what is installed" services_running_ok
	check "$SERVICES_RUN root's, 755" services_run_ok
	check "node socket $SERVICES_SOCKET, $SERVICES_USER's, 660" services_socket_ok
	check "admin socket $SERVICES_ADMIN_SOCKET, root's, 600" services_admin_socket_ok
	check "edge socket $SERVICES_EDGE_SOCKET, root's and $SERVICES_EDGE_USER's group's, 660" services_edge_socket_ok
	check "edge dial socket $SERVICES_EDGE_DIAL, root's and $SERVICES_EDGE_USER's group's, 660" services_edge_dial_ok
	if services_edge_on; then
		check "edge listens on loopback only" services_edge_loopback_only
		if [ -e "$SERVICES_CF_TOKEN" ]; then
			check "Cloudflare API token $SERVICES_CF_TOKEN, $SERVICES_USER's, mode 600 (PUBLIC endpoints)" services_cf_token_ok
		else
			skip "Cloudflare API token" "no $SERVICES_CF_TOKEN: PUBLIC endpoints do not open"
		fi
	else
		check "edge off: $LOCAL_ENV names no public endpoint domain" services_edge_off_ok
	fi
	if [ "$(auth_mode)" = oidc ]; then
		check "OAuth client secret $SERVICES_OIDC_SECRET, $SERVICES_USER's, mode 600" services_oidc_secret_ok
	fi
	check "apiserver runs $(auth_mode) auth as $LOCAL_ENV asks: $(services_auth_flags)" services_auth_running_ok
	check "apiserver listens on loopback only" services_loopback_only
	check "node $HANGAR_NODE_NAME Ready (hangar get node)" services_node_ready
	if [ -e "$SERVICES_SMOKE_TOKEN" ]; then
		check "smoke token $SERVICES_SMOKE_TOKEN, root's, mode 600, is serviceToken:smoke/smoke and lists the smoke project's containers" services_smoke_token_ok
	else
		skip "smoke token" "no $SERVICES_SMOKE_TOKEN: no smoke test runs"
	fi
}
