# The apiserver and the node agent as systemd services; the edge is the
# edge step's (85-edge.sh). The apiserver runs as its own unprivileged user
# with its database on hangar/state and the public API on loopback, in the
# auth mode local.env must name. Its unix sockets, the node
# agent's, the edge's, root's admin socket and, with the front door on, the
# public socket the edge forwards the API from :443 to, are made by systemd
# (socket units) in /run/hangar, a directory root owns, so the apiserver can
# neither take root's socket nor replace any. The agent runs as root on this
# host's drivers, and makes the edge's dial socket there itself. A unit
# changed, or a binary the release step replaced, restarts that service,
# here and nowhere else.

SERVICES_USER=hangar
SERVICES_STATE=/var/lib/hangar/state
SERVICES_LISTEN=127.0.0.1:8080
SERVICES_RUN=/run/hangar
SERVICES_SOCKET=$SERVICES_RUN/node.sock
SERVICES_ADMIN_SOCKET=$SERVICES_RUN/admin.sock
# The edge (85-edge.sh): its user, the apiserver's socket it reads the API
# on, and the agent's socket it reaches machines through.
SERVICES_EDGE_USER=hangar-edge
SERVICES_EDGE_SOCKET=$SERVICES_RUN/edge.sock
SERVICES_EDGE_DIAL=$SERVICES_RUN/edge-dial.sock
# The apiserver's public socket: the API from the front door, hangar-edge's
# alone, with each client's address (docs/decisions/front-door.md).
SERVICES_PUBLIC_SOCKET=$SERVICES_RUN/public.sock
# Where the Cloudflare API token was that the apiserver opened PUBLIC
# endpoints with, before hangar-edge signed endpoints' callers in itself:
# nothing reads it now, and this step says so while it is there
# (docs/runbook/cloudflare.md).
SERVICES_RETIRED_CF_TOKEN=$SERVICES_STATE/cloudflare-api-token
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
SERVICES_UNITS="hangar-apiserver hangar-node"
# Where the placement check copies a release's hangar-node to run it: on
# /usr/local, which the release's binaries are installed to and run from,
# so it is exec-capable on any host they run on; a directory of its own in
# it, 0700, made and removed by each check.
SERVICES_CHECK_PARENT=/usr/local/lib
# Where release_check finds an installed binary: where the release step
# installs them. Tests point it elsewhere.
SERVICES_INSTALLED_BIN=/usr/local/bin
# The apiserver's public socket, there only with the front door on.
SERVICES_PUBLIC_SOCKET_UNIT=hangar-apiserver-public

# services_edge_on holds when local.env names the public endpoints' domain:
# then the edge runs.
services_edge_on() { [ -n "$(local_env HANGAR_EDGE_DOMAIN)" ]; }

# services_front_door_on holds when the edge is on and local.env names the
# control plane's domain: then the edge is the front door on :443 and :80.
services_front_door_on() { services_edge_on && [ -n "$(local_env HANGAR_CP_DOMAIN)" ]; }

# services_units prints the units this host runs here.
services_units() {
	local u
	for u in $SERVICES_UNITS; do printf '%s\n' "$u"; done
}

# services_sockets prints the apiserver's sockets: socket units of their
# own, part of its service. The public one only with the front door on.
services_sockets() {
	printf '%s\n' hangar-apiserver-node hangar-apiserver-admin hangar-apiserver-edge
	! services_front_door_on || printf '%s\n' "$SERVICES_PUBLIC_SOCKET_UNIT"
}

# services_edge_uid prints the edge user's uid, or nothing before the user
# exists (a dry run on a fresh host).
services_edge_uid() { id -u "$SERVICES_EDGE_USER" 2>/dev/null || true; }

# services_endpoint_flags prints the apiserver's flags for hangar-edge and
# public endpoints: the edge's sockets, and the endpoints' domain.
services_endpoint_flags() {
	local domain
	domain=$(local_env HANGAR_EDGE_DOMAIN)
	printf -- '--edge-socket %s --edge-uid %s' "$SERVICES_EDGE_SOCKET" "$(services_edge_uid)"
	! services_front_door_on || printf -- ' --public-socket %s' "$SERVICES_PUBLIC_SOCKET"
	[ -z "$domain" ] || printf -- ' --endpoint-domain %s' "$domain"
	printf '\n'
}

# services_endpoints_problem prints what is wrong with local.env's public
# endpoint settings, nothing when they are whole or absent.
services_endpoints_problem() {
	services_edge_on || return 0
	[ "$(auth_mode)" = oidc ] ||
		printf '%s\n' "public endpoints (HANGAR_EDGE_DOMAIN) need oidc auth: the console signs their callers in"
	[[ $(local_env HANGAR_EDGE_DOMAIN) =~ ^[a-z0-9.-]+$ ]] ||
		printf '%s\n' "HANGAR_EDGE_DOMAIN in $LOCAL_ENV is not a domain"
	local cp
	cp=$(local_env HANGAR_CP_DOMAIN)
	[ -z "$cp" ] || [[ $cp =~ ^[a-z0-9.-]+\.[a-z]+$ ]] ||
		printf '%s\n' "HANGAR_CP_DOMAIN in $LOCAL_ENV is not a domain"
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

# SERVICES_IMAGE_CACHE_HIGH is the agent's --image-cache-high, in GiB: what
# the container images no machine uses may keep, their bases on the pool
# among it (docs/decisions/node-agent.md).
SERVICES_IMAGE_CACHE_HIGH=20

# services_reserved_disk is what root disks may not take from the pool: what
# the image cache and the VMs' directories may hold, their quotas, and the
# bases of the images no machine uses.
services_reserved_disk() {
	printf '%dGi\n' $(($(gib_of "$HANGAR_IMAGES_QUOTA") + $(gib_of "$HANGAR_VMS_QUOTA") + SERVICES_IMAGE_CACHE_HIGH))
}

# services_placement_args sets SERVICES_PLACEMENT to the agent's placement
# flags for what the host env file sets, each only when it is set
# (docs/decisions/machine-placement.md).
services_placement_args() {
	SERVICES_PLACEMENT=()
	local pair var
	for pair in region:HANGAR_REGION site:HANGAR_SITE chassis:HANGAR_CHASSIS \
		memory:HANGAR_MEMORY network:HANGAR_NETWORK; do
		var=${pair#*:}
		[ -z "${!var:-}" ] || SERVICES_PLACEMENT+=("--${pair%%:*}" "${!var}")
	done
}

# services_placement_flags prints those flags for the agent's ExecStart,
# each value quoted, with what systemd reads in quotes escaped: '\', '"',
# '%' and '$'. Nothing set prints nothing, and the unit is as it was
# without them.
services_placement_flags() {
	services_placement_args
	local i v
	for ((i = 0; i < ${#SERVICES_PLACEMENT[@]}; i += 2)); do
		v=${SERVICES_PLACEMENT[i + 1]}
		v=${v//\\/\\\\}
		v=${v//\"/\\\"}
		v=${v//%/%%}
		v=${v//\$/\$\$}
		printf -- ' %s "%s"' "${SERVICES_PLACEMENT[i]}" "$v"
	done
}

# binary_has_check NAME CHECK BIN prints what NAME BIN's own answer to -h
# says of its subcommand CHECK: "has" when its usage names it, "lacks" when
# it is the usage of a NAME from before it, which would take the word for
# its own arguments and start. A BIN that does not run, or answers with
# something else, is no old NAME: it prints why and fails.
binary_has_check() {
	local name=$1 check=$2 bin=$3 out status=0
	out=$("$bin" -h 2>&1) || status=$?
	# Go's flag package answers -h with the usage and exit 0 (2 before Go
	# 1.15); anything else, 126 for a file that cannot be executed among
	# it, did not answer.
	if [ "$status" != 0 ] && [ "$status" != 2 ]; then
		printf '%s cannot run (exit %s): %s\n' "$bin" "$status" "$(printf '%s' "$out" | head -n 3 | tr '\n' ' ')"
		return 1
	fi
	case $out in
	*"$check"*) echo has ;;
	"Usage of "*"$name:"* | "usage: $name"*) echo lacks ;;
	*)
		printf '%s answered -h with no %s usage: %s\n' "$bin" "$name" "$(printf '%s' "$out" | head -n 3 | tr '\n' ' ')"
		return 1
		;;
	esac
}

# node_check_placement BIN is binary_has_check for hangar-node's
# check-placement.
node_check_placement() { binary_has_check hangar-node check-placement "$1"; }

# release_check NAME CHECK WHAT TAG ARGS... prints why NAME's own check,
# NAME CHECK ARGS..., refuses WHAT, in the NAME this run installs: the
# release's with --release, else the installed one. Nothing when it passes,
# or when that NAME is one from before CHECK, which it warns of; a NAME
# that cannot run is a problem. The release's copy is made in a directory
# named for TAG. A subshell, so its traps are its own.
release_check() (
	name=$1 check=$2 what=$3 tag=$4
	shift 4
	bin=$SERVICES_INSTALLED_BIN/$name dir=${HANGAR_RELEASE_DIR:-} tmp='' shown=$SERVICES_INSTALLED_BIN/$name
	if [ -n "$dir" ]; then
		shown=$dir/$name
		# The release's file need not be executable (a fetched release keeps
		# no modes), and is run only once it matches its checksum: so a copy
		# of its own, made executable, is checked and run, the very bytes. It
		# goes on the filesystem the release's binaries run from, not under
		# TMPDIR, which may be noexec, and is gone however the check ends:
		# bash runs the EXIT trap on SIGINT and SIGTERM too, and the traps for
		# those make the subshell end with their usual statuses.
		trap '[ -z "$tmp" ] || rm -rf "$tmp"' EXIT
		trap 'exit 130' INT
		trap 'exit 143' TERM
		if ! tmp=$(mktemp -d "$SERVICES_CHECK_PARENT/.hangar-$tag-check.XXXXXX" 2>&1); then
			printf '%s cannot be checked: no directory for a copy of %s: %s\n' "$what" "$shown" "$tmp"
			tmp=''
			exit 0
		fi
		install -m 0700 "$dir/$name" "$tmp/$name" 2>/dev/null || true
		if ! grep -E "^[0-9a-f]{64}  $name\$" "$dir/SHA256SUMS" 2>/dev/null | sed "s|  $name\$|  $tmp/$name|" |
			sha256sum -c --quiet --status 2>/dev/null; then
			printf '%s\n' "$dir/$name does not match $dir/SHA256SUMS; $what cannot be checked with it"
			exit 0
		fi
		bin=$tmp/$name
	elif [ ! -x "$bin" ]; then
		printf '%s\n' "no $bin to check $what with; install a release first"
		exit 0
	fi
	if ! has=$(binary_has_check "$name" "$check" "$bin"); then
		printf '%s cannot be checked with %s: %s\n' "$what" "$shown" "$has"
	elif [ "$has" = lacks ]; then
		log "warning: $shown has no $check; $what is not checked this run"
	elif ! out=$("$bin" "$check" "$@" 2>&1); then
		printf '%s: %s\n' "$what" "$(printf '%s' "$out" | tr '\n' ';' | sed 's/;/; /g; s/; $//')"
	fi
	exit 0
)

# services_placement_problem prints why the agent would not report the
# placement the host env file gives as given, by the agent's own check
# (hangar-node check-placement; release_check says which hangar-node).
services_placement_problem() {
	services_placement_args
	[ "${#SERVICES_PLACEMENT[@]}" -gt 0 ] || return 0
	release_check hangar-node check-placement "the host env file's placement" placement "${SERVICES_PLACEMENT[@]}"
}

# services_auth_flags prints the apiserver's flags for the auth mode
# local.env asks for, nothing when it names none (auth_problem refuses
# that). The values land in the unit, so a changed mode is a changed unit
# and restarts the apiserver.
services_auth_flags() {
	case $(auth_mode) in
	oidc)
		printf -- '--auth-mode oidc --oidc-client-id %s --oidc-client-secret-file %s --public-url %s' \
			"$(local_env HANGAR_OIDC_CLIENT_ID)" "$SERVICES_OIDC_SECRET" "$(local_env HANGAR_PUBLIC_URL)"
		[ -z "$(local_env HANGAR_OIDC_ISSUER)" ] || printf -- ' --oidc-issuer %s' "$(local_env HANGAR_OIDC_ISSUER)"
		printf '\n'
		;;
	dev) printf -- '--auth-mode dev\n' ;;
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

services_socket_hangar_apiserver_public() {
	cat <<EOF
# Written by deploy/host/steps/80-services.sh.
[Unit]
Description=Hangar API server: the public API from hangar-edge's front door
PartOf=hangar-apiserver.service

[Socket]
ListenStream=$SERVICES_PUBLIC_SOCKET
FileDescriptorName=public
Service=hangar-apiserver.service
# The apiserver also checks the peer is the edge's uid before it believes
# the client address the edge sends; this keeps everyone else from even
# connecting.
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
# The public API is on loopback in either auth mode, for the host itself,
# and the front door reaches it through the public socket. The node socket
# accepts root's agent; the admin socket, root.
ExecStart=/usr/local/bin/hangar-apiserver --db $SERVICES_STATE/hangar.db --listen $SERVICES_LISTEN $(services_auth_flags) --socket $SERVICES_SOCKET --admin-socket $SERVICES_ADMIN_SOCKET --node-uid 0 --node-name $HANGAR_NODE_NAME --bootstrap-admin \${HANGAR_BOOTSTRAP_ADMIN} --ssh-ca-key $SERVICES_SSH_CA $(services_endpoint_flags) --secrets-kek $SERVICES_SECRETS_KEK --secrets-kek-previous $SERVICES_SECRETS_KEK_PREVIOUS --default-max-owned-tenants $HANGAR_DEFAULT_MAX_OWNED_TENANTS
Sockets=$(for s in $(services_sockets); do printf '%s.socket ' "$s"; done | sed 's/ $//')
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
# The NAT log on its own dataset, whose quota caps it.
ExecStartPre=/usr/bin/mountpoint -q /var/log/hangar/nat
# The default paths: Cloud Hypervisor, the guest kernel and initramfs under
# /usr/local, the pool's hangar dataset, /var/lib/hangar/images and vms.
ExecStart=/usr/local/bin/hangar-node --drivers=host --name $HANGAR_NODE_NAME --uplink $HANGAR_UPLINK --public-ipv4 $HANGAR_PUBLIC_IPV4 --public-ipv6 ${HANGAR_IPV6_ADDR%/*} --public-ipv6-prefix $(ipv6_prefix "$HANGAR_IPV6_ADDR") --reserved-disk $(services_reserved_disk) --image-cache-high ${SERVICES_IMAGE_CACHE_HIGH}Gi --reserved-memory $HANGAR_RESERVED_MEMORY --edge-dial-socket $SERVICES_EDGE_DIAL --edge-user $SERVICES_EDGE_USER --nat-log-dir /var/log/hangar/nat$(services_placement_flags)
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
	local user=${1:-$SERVICES_USER} uid shell
	uid=$(id -u "$user" 2>/dev/null) || return 1
	shell=$(getent passwd "$user" | cut -d: -f7)
	[ "$uid" -lt 1000 ] && [ "$uid" != 0 ] && [ "$shell" = /usr/sbin/nologin ]
}

services_oidc_secret_ok() {
	[ -f "$SERVICES_OIDC_SECRET" ] && [ "$(stat -c '%U %G %a' "$SERVICES_OIDC_SECRET")" = "$SERVICES_USER $SERVICES_USER 600" ]
}

services_oidc_secret_how() {
	printf '%s\n' "install the OAuth client's secret at $SERVICES_OIDC_SECRET, $SERVICES_USER's, mode 0600 (docs/runbook/host-bootstrap.md)"
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
		for s in $(services_sockets); do
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
	for u in $(services_sockets); do
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
	for u in $(services_sockets); do units+=("$u.socket"); done
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

services_public_socket_ok() {
	[ -S "$SERVICES_PUBLIC_SOCKET" ] && [ "$(stat -c '%U %G %a' "$SERVICES_PUBLIC_SOCKET")" = "root $SERVICES_EDGE_USER 660" ]
}

services_edge_dial_ok() {
	[ -S "$SERVICES_EDGE_DIAL" ] && [ "$(stat -c '%U %G %a' "$SERVICES_EDGE_DIAL")" = "root $SERVICES_EDGE_USER 660" ]
}

# services_auth_running_ok holds when the apiserver runs the auth flags
# local.env asks for.
services_auth_running_ok() {
	local pid want
	pid=$(systemctl show -p MainPID --value hangar-apiserver.service)
	[ "$pid" != 0 ] || return 1
	want=$(services_auth_flags)
	[ -n "$want" ] || return 1
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
	problem=$(auth_problem; services_endpoints_problem; services_placement_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	# A tunnel an earlier release left running never forwards to an
	# apiserver in dev auth, which trusts a header: it goes before the
	# apiserver could restart in dev.
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
	# The apiserver no longer calls Cloudflare: a token left from before is
	# for the operator to revoke and delete, never this step's to touch.
	[ ! -e "$SERVICES_RETIRED_CF_TOKEN" ] ||
		log "warning: $SERVICES_RETIRED_CF_TOKEN is no longer used: revoke the token in Cloudflare and delete the file (docs/runbook/cloudflare.md)"

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

	# A public socket left from when the front door was on goes first: the
	# apiserver refuses a socket systemd passes that no flag serves.
	if ! services_front_door_on && [ -e "$(services_socket_path "$SERVICES_PUBLIC_SOCKET_UNIT")" ]; then
		services_mark hangar-apiserver
		run "stop and remove $SERVICES_PUBLIC_SOCKET_UNIT: the front door is off" sh -c \
			"systemctl stop $SERVICES_PUBLIC_SOCKET_UNIT.socket; rm -f $(services_socket_path "$SERVICES_PUBLIC_SOCKET_UNIT") $SERVICES_PUBLIC_SOCKET"
		reload=1
	fi
	# A socket is part of the apiserver: a changed one restarts it, and the
	# restart restarts the socket.
	for u in $(services_sockets); do
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

	# The apiserver first: the agent dials its socket (and retries until it
	# is there), and the edge step's edge both.
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
	local sockets
	sockets=$(services_sockets | tr '\n' ' ' | sed 's/ $//')
	check "units $units, sockets $sockets and $SERVICES_TMPFILES as generated and loaded" services_units_ok
	check "no drop-ins for $units $sockets" services_no_dropins
	check "units $units enabled, active, running what is installed" services_running_ok
	check "$SERVICES_RUN root's, 755" services_run_ok
	check "node socket $SERVICES_SOCKET, $SERVICES_USER's, 660" services_socket_ok
	check "admin socket $SERVICES_ADMIN_SOCKET, root's, 600" services_admin_socket_ok
	check "edge socket $SERVICES_EDGE_SOCKET, root's and $SERVICES_EDGE_USER's group's, 660" services_edge_socket_ok
	check "edge dial socket $SERVICES_EDGE_DIAL, root's and $SERVICES_EDGE_USER's group's, 660" services_edge_dial_ok
	if services_front_door_on; then
		check "public socket $SERVICES_PUBLIC_SOCKET, root's and $SERVICES_EDGE_USER's group's, 660" services_public_socket_ok
	else
		check "no public socket: the front door is off" test ! -e "$SERVICES_PUBLIC_SOCKET"
	fi
	check "$LOCAL_ENV names a whole auth mode (HANGAR_AUTH_MODE dev or oidc)" test -z "$(auth_problem)"
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
