# The edge, hangar-edge, as a systemd service of its own user, only when
# local.env names the public endpoints' domain (HANGAR_EDGE_DOMAIN) and the
# control plane's (HANGAR_CP_DOMAIN). It is the front door
# (docs/decisions/front-door.md): systemd binds :443 and :80, on IPv4 and
# IPv6, and passes them to it (socket units), so it never runs as root nor
# holds a capability. It keeps its certificates in its state directory,
# /var/lib/hangar-edge, its alone, and answers ACME's DNS-01 challenges with
# a Cloudflare token that may only edit DNS, which the operator installs
# (docs/runbook/cloudflare.md) and systemd hands it as a credential. A unit
# changed, or a binary the release step replaced, restarts it and its
# sockets, which closes :443 and :80 for that moment; a crash restart keeps
# the sockets.
#
# The step also retires the Cloudflare tunnel, which no step installs any
# more: a tunnel a host still runs is left alone while cloudflared.env is
# there, and removed once the operator deletes the file (the cutover in
# docs/runbook/cloudflare.md).

# The services step's helpers and names: the edge's user and sockets, the
# restart markers.
# shellcheck source=deploy/host/steps/80-services.sh
declare -F services_edge_on >/dev/null || . "$(dirname "${BASH_SOURCE[0]}")/80-services.sh"

EDGE_UNIT=hangar-edge
EDGE_SOCKETS="hangar-edge-https hangar-edge-http"
# The Cloudflare token DNS-01 is answered with: root's file, which systemd
# passes the edge under EDGE_DNS_CREDENTIAL (LoadCredential=).
EDGE_DNS_TOKEN=/etc/hangar/cloudflare-dns-token
EDGE_DNS_CREDENTIAL=cloudflare-dns-token
# Where the release puts the console's files; while it has none, the
# console's pages say it is not installed.
EDGE_CONSOLE_DIR=/usr/local/share/hangar/console
EDGE_STATE=/var/lib/hangar-edge
# verify.sh fails a certificate with this many days left or fewer: certmagic
# renews a 90-day one with about 30 left, so one this close has failed to.
EDGE_CERT_MIN_DAYS=14
# What an earlier release installed for the Cloudflare tunnel.
CLOUDFLARED_BIN=/usr/local/bin/cloudflared
CLOUDFLARED_UNIT_PATH=/etc/systemd/system/$CLOUDFLARED_UNIT.service
CLOUDFLARED_GUARD=/usr/local/libexec/hangar-cloudflared-guard

# edge_acme_dns prints how DNS-01 is answered: cloudflare unless local.env
# says otherwise, which only a testbed's edge, built with the testbed tag,
# takes (docs/runbook/testbed.md).
edge_acme_dns() {
	local d
	d=$(local_env HANGAR_ACME_DNS)
	printf '%s\n' "${d:-cloudflare}"
}

edge_needs_dns_token() { services_front_door_on && [ "$(edge_acme_dns)" = cloudflare ]; }

# edge_sockets prints the edge's socket units: :443's and :80's, with the
# front door on.
edge_sockets() {
	services_front_door_on || return 0
	local s
	for s in $EDGE_SOCKETS; do printf '%s\n' "$s"; done
}

# edge_front_flags prints the edge's flags for the front door, nothing
# with it off.
edge_front_flags() {
	services_front_door_on || return 0
	local v
	# The host's /64 holds its machines' addresses, which reach the front
	# door from inside the host: each is a client of the limits by itself.
	printf -- ' --cp-domain %s --public-socket %s --cert-storage %s --console-dir %s --acme-email %s --own-prefix %s' \
		"$(local_env HANGAR_CP_DOMAIN)" "$SERVICES_PUBLIC_SOCKET" "$EDGE_STATE" "$EDGE_CONSOLE_DIR" "$(local_env HANGAR_ACME_EMAIL)" "$(ipv6_prefix "$HANGAR_IPV6_ADDR")"
	v=$(local_env HANGAR_ACME_CA)
	[ -z "$v" ] || printf -- ' --acme-ca %s' "$v"
	v=$(local_env HANGAR_ACME_CA_ROOT)
	[ -z "$v" ] || printf -- ' --acme-ca-root %s' "$v"
	[ "$(edge_acme_dns)" = cloudflare ] || printf -- ' --acme-dns %s' "$(edge_acme_dns)"
}

edge_unit() {
	cat <<EOF
# Written by deploy/host/steps/85-edge.sh.
[Unit]
Description=Hangar edge: the front door and public endpoints
After=network.target hangar-apiserver.service hangar-node.service

[Service]
Type=exec
User=$SERVICES_EDGE_USER
Group=$SERVICES_EDGE_USER
EOF
	if services_front_door_on; then
		cat <<EOF
# :443 and :80, which systemd binds: the edge holds no capability.
Sockets=$(edge_sockets | sed 's/$/.socket/' | paste -sd' ' -)
# Its certificates, their keys and its ACME account, its alone, across
# restarts.
StateDirectory=hangar-edge
StateDirectoryMode=0700
EOF
		! edge_needs_dns_token || cat <<EOF
# The DNS-edit token, root's file, readable here by the edge alone.
LoadCredential=$EDGE_DNS_CREDENTIAL:$EDGE_DNS_TOKEN
EOF
	fi
	cat <<EOF
# It reads the API on the apiserver's edge socket, and asks it there who
# calls a private endpoint, forwards the API from :443 to its public
# socket, and reaches machines through the agent's dial socket, all its
# group's. A browser not yet signed in to a private endpoint goes to the
# console, HANGAR_PUBLIC_URL, to be.
ExecStart=/usr/local/bin/hangar-edge --api-socket $SERVICES_EDGE_SOCKET --dial-socket $SERVICES_EDGE_DIAL --domain $(local_env HANGAR_EDGE_DOMAIN) --console-url $(local_env HANGAR_PUBLIC_URL)$(edge_front_flags)
Restart=always
RestartSec=2
UMask=0077

# It needs its sockets, its state directory and the network, nothing else.
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

# edge_socket_unit NAME PORT FDNAME prints a socket unit for the front door.
edge_socket_unit() {
	cat <<EOF
# Written by deploy/host/steps/85-edge.sh.
[Unit]
Description=Hangar edge: the front door's :$2
# Stopped and restarted with the edge when that is asked (systemctl, this
# step): :443 and :80 are then closed for that moment. A crash restart
# (Restart=always) is not asked, and keeps them.
PartOf=$EDGE_UNIT.service

[Socket]
# Every address, IPv4 and IPv6, on one socket.
ListenStream=$2
BindIPv6Only=both
FileDescriptorName=$3
Service=$EDGE_UNIT.service
Backlog=4096
NoDelay=yes
EOF
}

edge_socket() {
	case $1 in
	hangar-edge-https) edge_socket_unit "$1" 443 https ;;
	hangar-edge-http) edge_socket_unit "$1" 80 http ;;
	esac
}

edge_unit_path() { services_unit_path "$EDGE_UNIT"; }

# edge_stale holds when the running edge is not what is installed: its unit
# or a socket changed since it started, a socket is down, or its binary was
# replaced.
edge_stale() {
	local s
	for s in $(edge_sockets); do
		systemctl is-active --quiet "$s.socket" || return 0
	done
	services_stale "$EDGE_UNIT"
}

# edge_off_ok holds when the edge, off, neither runs nor starts at boot, and
# nothing holds :443 or :80 for it.
edge_off_ok() {
	local s
	! systemctl is-active --quiet "$EDGE_UNIT.service" && ! systemctl is-enabled --quiet "$EDGE_UNIT.service" 2>/dev/null || return 1
	for s in $EDGE_SOCKETS; do
		! systemctl is-active --quiet "$s.socket" || return 1
	done
}

edge_dns_token_ok() {
	[ -f "$EDGE_DNS_TOKEN" ] && [ "$(stat -c '%U %G %a' "$EDGE_DNS_TOKEN")" = "root root 600" ] && [ -s "$EDGE_DNS_TOKEN" ]
}

edge_dns_token_how() {
	printf '%s\n' "install the Cloudflare DNS token at $EDGE_DNS_TOKEN, root's, mode 0600 (docs/runbook/cloudflare.md)"
}

# edge_problem prints why the edge cannot run as local.env asks, nothing
# when it can or is off.
edge_problem() {
	services_edge_on || return 0
	if ! services_front_door_on; then
		printf '%s\n' "public endpoints (HANGAR_EDGE_DOMAIN) are served by the front door alone: set HANGAR_CP_DOMAIN in $LOCAL_ENV too"
		return 0
	fi
	edge_needs_dns_token && ! edge_dns_token_ok &&
		printf '%s\n' "the front door (HANGAR_CP_DOMAIN) needs its DNS token: $(edge_dns_token_how)"
	# Without an email certmagic may register the ACME account as
	# mailto:default on a first start (docs/decisions/front-door.md).
	[ -n "$(local_env HANGAR_ACME_EMAIL)" ] ||
		printf '%s\n' "the front door (HANGAR_CP_DOMAIN) needs HANGAR_ACME_EMAIL in $LOCAL_ENV, the ACME account's contact"
	# Sign-in's provider returns to the public URL's /auth/callback, which
	# only the console's site hands the apiserver.
	local want
	want="https://console.$(local_env HANGAR_CP_DOMAIN)"
	[ "$(auth_mode)" != oidc ] || [ "$(local_env HANGAR_PUBLIC_URL)" = "$want" ] ||
		printf '%s\n' "with the front door, HANGAR_PUBLIC_URL in $LOCAL_ENV must be $want, the console's origin, not $(local_env HANGAR_PUBLIC_URL)"
	local root
	root=$(local_env HANGAR_ACME_CA_ROOT)
	[ -z "$root" ] || [ -f "$root" ] || printf '%s\n' "HANGAR_ACME_CA_ROOT in $LOCAL_ENV names $root, which is not there"
	return 0
}

# edge_cert_names prints the names the front door keeps certificates for,
# as certmagic files them in its storage: a wildcard's * as wildcard_.
edge_cert_names() {
	local cp
	cp=$(local_env HANGAR_CP_DOMAIN)
	printf '%s\n' "api.$cp" "console.$cp" "wildcard_.$(local_env HANGAR_EDGE_DOMAIN)"
}

# edge_cert_ok NAME holds when the edge's storage has a certificate for NAME,
# from any CA, with more than EDGE_CERT_MIN_DAYS left.
edge_cert_ok() {
	local f
	for f in "$EDGE_STATE"/certificates/*/"$1"/"$1".crt; do
		[ -f "$f" ] || continue
		openssl x509 -noout -checkend $((EDGE_CERT_MIN_DAYS * 86400)) -in "$f" >/dev/null 2>&1 && return 0
	done
	return 1
}

edge_units_ok() {
	local s
	edge_unit | cmp -s - "$(edge_unit_path)" || return 1
	[ "$(systemctl show -p NeedDaemonReload --value "$EDGE_UNIT.service")" = no ] || return 1
	for s in $(edge_sockets); do
		edge_socket "$s" | cmp -s - "$(services_socket_path "$s")" || return 1
		[ "$(systemctl show -p NeedDaemonReload --value "$s.socket")" = no ] || return 1
	done
	for s in $EDGE_SOCKETS; do
		services_front_door_on || [ ! -e "$(services_socket_path "$s")" ] || return 1
	done
}

edge_dropins() {
	local u d
	for u in "$EDGE_UNIT.service" $(edge_sockets | sed 's/$/.socket/'); do
		for d in /etc /run /usr/local/lib /usr/lib /lib; do
			ls -d "$d/systemd/system/$u.d" 2>/dev/null || true
		done
		systemctl show -p DropInPaths --value "$u" | tr ' ' '\n'
	done | { grep . || true; } | sort -u
}

edge_running_ok() {
	systemctl is-enabled --quiet "$EDGE_UNIT.service" && systemctl is-active --quiet "$EDGE_UNIT.service" && ! edge_stale
}

edge_pid() { systemctl show -p MainPID --value "$EDGE_UNIT.service"; }

# edge_unprivileged holds when the edge runs as its user, with no
# capability at all.
edge_unprivileged() {
	local pid
	pid=$(edge_pid)
	[ "$pid" != 0 ] || return 1
	[ "$(stat -c %U "/proc/$pid")" = "$SERVICES_EDGE_USER" ] &&
		grep -qE '^CapEff:[[:space:]]+0+$' "/proc/$pid/status" &&
		grep -qE '^CapPrm:[[:space:]]+0+$' "/proc/$pid/status"
}

# edge_listens_ok holds when the edge's TCP listeners are :443 and :80 on
# every address, and nothing else but loopback's (a testbed's edge answers
# DNS-01 there itself).
edge_listens_ok() {
	local pid addrs want
	pid=$(edge_pid)
	addrs=$(ss -Hltnp | grep "pid=$pid," | awk '{print $4}' | sort -u)
	for want in '*:443' '*:80'; do
		grep -qxF "$want" <<<"$addrs" || return 1
	done
	! grep -vxE '\*:(443|80)|(127\.[0-9.]+|\[::1\]):[0-9]+' <<<"$addrs" | grep -q .
}

# edge_tunnel_left prints what an earlier release installed for the
# Cloudflare tunnel and is still on the host.
edge_tunnel_left() {
	local f
	for f in "$CLOUDFLARED_UNIT_PATH" "$CLOUDFLARED_GUARD" "$CLOUDFLARED_BIN"; do
		[ ! -e "$f" ] || printf '%s\n' "$f"
	done
	! cloudflared_up || printf '%s\n' "$CLOUDFLARED_UNIT running"
}

# edge_retire_tunnel removes the Cloudflare tunnel once the operator deleted
# its token's file, and leaves it alone while the file is there.
edge_retire_tunnel() {
	local left
	left=$(edge_tunnel_left)
	[ -n "$left" ] || return 0
	if [ -e "$CLOUDFLARED_ENV" ]; then
		log "warning: the Cloudflare tunnel ($CLOUDFLARED_UNIT) is no longer installed by any step; it is left as it is until you delete $CLOUDFLARED_ENV (docs/runbook/cloudflare.md, the cutover)"
		return 0
	fi
	run "retire the Cloudflare tunnel: stop and disable $CLOUDFLARED_UNIT, remove $(printf '%s\n' "$left" | grep '^/' | paste -sd' ' -)" \
		sh -c "systemctl disable --now $CLOUDFLARED_UNIT.service 2>/dev/null || true; rm -f '$CLOUDFLARED_UNIT_PATH' '$CLOUDFLARED_GUARD' '$CLOUDFLARED_BIN' && systemctl daemon-reload && { systemctl reset-failed $CLOUDFLARED_UNIT.service 2>/dev/null || true; }"
}

edge_apply() {
	local s before reload=0 problem
	edge_retire_tunnel
	if ! services_edge_on; then
		edge_off_ok || run "stop and disable $EDGE_UNIT and its sockets: $LOCAL_ENV names no public endpoint domain" \
			sh -c "systemctl disable --now $EDGE_UNIT.service; for s in $EDGE_SOCKETS; do systemctl stop \$s.socket 2>/dev/null || true; done"
		return 0
	fi
	problem=$(services_endpoints_problem; edge_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	if [ -e "$EDGE_DNS_TOKEN" ] && ! edge_dns_token_ok; then
		log "warning: $EDGE_DNS_TOKEN was $(stat -c '%U:%G mode %a' "$EDGE_DNS_TOKEN"): whoever could read it may have the token; consider rolling it (docs/runbook/cloudflare.md)"
		run "give $EDGE_DNS_TOKEN to root, mode 600" sh -c "chown root:root '$EDGE_DNS_TOKEN' && chmod 600 '$EDGE_DNS_TOKEN'"
	fi
	[ -x /usr/local/bin/hangar-edge ] || [ "$DRY_RUN" = 1 ] || die "/usr/local/bin/hangar-edge is not installed; the release step installs it"

	# A socket is part of the edge: a changed one restarts it, and the
	# restart restarts the socket, closing :443 and :80 for that moment.
	for s in $(edge_sockets); do
		before=$CHANGES
		put_file "$(services_socket_path "$s")" 644 "$(edge_socket "$s")"
		if [ "$CHANGES" != "$before" ]; then
			reload=1
			services_mark "$EDGE_UNIT"
		fi
		[ "$(systemctl show -p NeedDaemonReload --value "$s.socket")" = no ] || reload=1
	done
	before=$CHANGES
	put_file "$(edge_unit_path)" 644 "$(edge_unit)"
	if [ "$CHANGES" != "$before" ]; then
		reload=1
		services_mark "$EDGE_UNIT"
	fi
	[ "$(systemctl show -p NeedDaemonReload --value "$EDGE_UNIT.service")" = no ] || reload=1
	[ "$reload" = 0 ] || run "reload systemd for $EDGE_UNIT" systemctl daemon-reload

	systemctl is-enabled --quiet "$EDGE_UNIT.service" || run "enable $EDGE_UNIT" systemctl enable "$EDGE_UNIT.service"
	if ! systemctl is-active --quiet "$EDGE_UNIT.service"; then
		# Its sockets first: systemd binds :443 and :80, then hands them over.
		run "start $EDGE_UNIT" sh -c "for s in $(edge_sockets | paste -sd' ' -); do systemctl start \$s.socket || exit 1; done && systemctl start $EDGE_UNIT.service && rm -f $(services_marker "$EDGE_UNIT")"
	elif edge_stale; then
		run "restart $EDGE_UNIT" sh -c "systemctl restart $EDGE_UNIT.service && rm -f $(services_marker "$EDGE_UNIT")"
	fi
}

edge_verify() {
	if [ -e "$CLOUDFLARED_ENV" ]; then
		skip "Cloudflare tunnel retired" "$CLOUDFLARED_ENV is still there, so the step leaves the tunnel as it is (the cutover)"
	else
		check "Cloudflare tunnel retired: nothing of it left" test -z "$(edge_tunnel_left)"
	fi
	if ! services_edge_on; then
		check "edge off: $LOCAL_ENV names no public endpoint domain" edge_off_ok
		return 0
	fi
	local units n
	units=$(edge_sockets | paste -sd' ' -)
	units="$EDGE_UNIT${units:+ $units}"
	check "edge units $units as generated and loaded" edge_units_ok
	check "no drop-ins for $units" test -z "$(edge_dropins)"
	check "unit $EDGE_UNIT enabled, active, running what is installed" edge_running_ok
	check "edge runs as $SERVICES_EDGE_USER with no capability" edge_unprivileged
	check "edge listens on :443 and :80 for every address, and nothing else but on loopback" edge_listens_ok
	for n in $(edge_cert_names); do
		check "certificate for ${n/#wildcard_/*} in $EDGE_STATE, more than $EDGE_CERT_MIN_DAYS days left" edge_cert_ok "$n"
	done
	if edge_needs_dns_token; then
		check "Cloudflare DNS token $EDGE_DNS_TOKEN, root's, mode 600" edge_dns_token_ok
	else
		skip "Cloudflare DNS token" "DNS-01 is answered by $(edge_acme_dns)"
	fi
}
