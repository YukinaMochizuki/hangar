# The Cloudflare tunnel: cloudflared, pinned, running the remotely-managed
# tunnel whose token is in cloudflared.env, as a sandboxed user of its own that
# can reach the network and nothing of hangar's. Without cloudflared.env the
# host runs no tunnel: nothing is installed, and a tunnel already up is
# stopped. It only ever forwards to an apiserver in oidc auth, which signs
# people in itself, never to one in dev auth, which trusts a header.

CLOUDFLARED_BIN=/usr/local/bin/cloudflared
CLOUDFLARED_UNIT_PATH=/etc/systemd/system/$CLOUDFLARED_UNIT.service
CLOUDFLARED_GUARD=/usr/local/libexec/hangar-cloudflared-guard
# Its metrics server, pinned to loopback: left alone it picks a port and,
# where it thinks it is in a container, every interface.
CLOUDFLARED_METRICS=127.0.0.1:20241

# cloudflared_guard is the unit's own check, every time it starts, however it
# was started: the apiserver runs, in oidc auth, and a restart would
# keep it there. The running process says what the tunnel would reach now;
# the loaded unit what a restart of the apiserver (on failure, without this
# unit's PartOf= seeing it) would bring up instead.
cloudflared_guard() {
	cat <<'EOF'
#!/bin/sh
# Written by deploy/host/steps/85-cloudflared.sh. hangar-cloudflared runs it
# as root before cloudflared (ExecStartPre=+) and does not start unless it
# succeeds: cloudflared never forwards to an apiserver in dev auth, which
# trusts a header.
set -eu
refuse() {
	echo "hangar-cloudflared-guard: refusing to start the tunnel: $*" >&2
	exit 1
}
# auth_mode_of reads one argument per line and prints the auth mode they set,
# but only when they set it exactly once, as --auth-mode VALUE: Go's flag
# package takes the last of several, and -auth-mode and --auth-mode=VALUE
# too, so anything else prints nothing and is refused.
auth_mode_of() {
	awk '
		/^--?auth-mode(=.*)?$/ { n++; if ($0 == "--auth-mode") at = NR + 1; else odd = 1 }
		NR == at { mode = $0 }
		END { if (n == 1 && !odd) print mode }'
}
pid=$(systemctl show -p MainPID --value hangar-apiserver.service)
[ -n "$pid" ] && [ "$pid" != 0 ] || refuse "hangar-apiserver is not running"
[ -r "/proc/$pid/cmdline" ] || refuse "cannot read hangar-apiserver's command line"
mode=$(tr '\0' '\n' <"/proc/$pid/cmdline" | auth_mode_of)
[ "$mode" = oidc ] ||
	refuse "hangar-apiserver runs --auth-mode ${mode:-(none, or not exactly once)}, not oidc"
# ExecStart reads { path=... ; argv[]=ARGS ; ... }; no argument has a space.
mode=$(systemctl show -p ExecStart --value hangar-apiserver.service |
	sed -n 's/.*argv\[\]=\([^;]*\) ;.*/\1/p' | tr ' ' '\n' | auth_mode_of)
[ "$mode" = oidc ] ||
	refuse "hangar-apiserver's unit would restart it without --auth-mode oidc"
EOF
}

cloudflared_unit() {
	cat <<EOF
# Written by deploy/host/steps/85-cloudflared.sh.
[Unit]
Description=Cloudflare Tunnel to the Hangar API
Wants=network-online.target
After=network-online.target hangar-apiserver.service
# Stopped and restarted with the apiserver, so the guard below looks again
# at what the apiserver came back as.
PartOf=hangar-apiserver.service
StartLimitIntervalSec=0

[Service]
# Not notify: a tunnel that cannot reach Cloudflare must not hold up a
# bootstrap run; it keeps retrying on its own.
Type=exec
# TUNNEL_TOKEN, kept on the host only. systemd reads it, so the sandbox
# below can hide /etc/hangar.
EnvironmentFile=$CLOUDFLARED_ENV
# Outside the sandbox (+), which cannot see the apiserver.
ExecStartPre=+$CLOUDFLARED_GUARD
ExecStart=$CLOUDFLARED_BIN tunnel --no-autoupdate --metrics $CLOUDFLARED_METRICS run
Restart=always
RestartSec=5
DynamicUser=yes
UMask=0077

# A network client: it dials Cloudflare and the apiserver on loopback, and
# needs nothing on disk. No AF_UNIX, so no unix socket of the host's
# whatever its mode, and hangar's directories are not there at all.
InaccessiblePaths=/run/hangar /var/lib/hangar /etc/hangar
RestrictAddressFamilies=AF_INET AF_INET6 AF_NETLINK
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

cloudflared_marker() { printf '%s/restart-%s\n' "$STATE_DIR" "$CLOUDFLARED_UNIT"; }

cloudflared_env_ok() {
	[ "$(stat -c '%U %a' "$CLOUDFLARED_ENV")" = "root 600" ] &&
		grep -qE '^TUNNEL_TOKEN=[A-Za-z0-9+/=_-]+$' "$CLOUDFLARED_ENV"
}

cloudflared_env_how() {
	printf '%s\n' "write TUNNEL_TOKEN=<the tunnel's token> to $CLOUDFLARED_ENV, root's, mode 0600 (docs/runbook/host-bootstrap.md)"
}

cloudflared_is_pinned() {
	[ -x "$CLOUDFLARED_BIN" ] && echo "$HANGAR_CLOUDFLARED_SHA256  $CLOUDFLARED_BIN" | sha256sum -c --quiet -
}

install_cloudflared() {
	local tmp
	tmp=$(mktemp)
	curl -fsSL -o "$tmp" "$HANGAR_CLOUDFLARED_URL"
	if ! echo "$HANGAR_CLOUDFLARED_SHA256  $tmp" | sha256sum -c --quiet -; then
		rm -f "$tmp"
		die "checksum mismatch for $HANGAR_CLOUDFLARED_URL"
	fi
	install -m 0755 "$tmp" "$CLOUDFLARED_BIN.hangar-new"
	mv -f "$CLOUDFLARED_BIN.hangar-new" "$CLOUDFLARED_BIN"
	rm -f "$tmp"
}

# cloudflared_started holds while the unit runs, or waits to retry: a tunnel
# that cannot reach Cloudflare yet is not a reason to start it again.
cloudflared_started() {
	case $(systemctl show -p ActiveState --value "$CLOUDFLARED_UNIT.service") in
	active | activating | reloading) return 0 ;;
	esac
	return 1
}

# cloudflared_stale holds when the running tunnel is not what is installed:
# its unit changed since it started, its binary was replaced, or its token is
# not the one in the env file.
cloudflared_stale() {
	local pid exe
	[ ! -e "$(cloudflared_marker)" ] || return 0
	pid=$(systemctl show -p MainPID --value "$CLOUDFLARED_UNIT.service")
	[ "$pid" != 0 ] || return 1
	exe=$(readlink "/proc/$pid/exe") || return 1
	[ "$exe" = "$CLOUDFLARED_BIN" ] || return 0
	[ "$(tr '\0' '\n' <"/proc/$pid/environ" | grep '^TUNNEL_TOKEN=')" != "$(grep '^TUNNEL_TOKEN=' "$CLOUDFLARED_ENV" | tail -n 1)" ]
}

# cloudflared_auth_problem prints why the tunnel may not run, nothing when
# the apiserver verifies Access tokens, as local.env asks and as it runs.
cloudflared_auth_problem() {
	local running
	running=$(apiserver_auth_mode)
	if [ "$(auth_mode)" != oidc ] || [ -n "$(auth_problem)" ]; then
		printf '%s\n' "$LOCAL_ENV asks for $(auth_mode) auth, not a whole oidc one"
	elif [ "$running" != oidc ]; then
		printf '%s\n' "the apiserver runs ${running:-nothing}, not oidc auth; run the services step first"
	fi
}

cloudflared_apply() {
	if [ ! -e "$CLOUDFLARED_ENV" ]; then
		cloudflared_down "$CLOUDFLARED_ENV is gone"
		return 0
	fi
	cloudflared_env_ok || refuse "$CLOUDFLARED_ENV is wrong: $(cloudflared_env_how)"
	local problem
	problem=$(cloudflared_auth_problem)
	if [ -n "$problem" ]; then
		cloudflared_down "the apiserver is not in oidc auth"
		refuse "$CLOUDFLARED_ENV is there, but $problem: cloudflared never forwards to an apiserver in dev auth (docs/runbook/host-bootstrap.md)"
	fi

	cloudflared_is_pinned || run "install cloudflared $HANGAR_CLOUDFLARED_VERSION" install_cloudflared
	# The guard runs at each start, so a new one needs no restart.
	put_file "$CLOUDFLARED_GUARD" 755 "$(cloudflared_guard)"
	local before=$CHANGES
	put_file "$CLOUDFLARED_UNIT_PATH" 644 "$(cloudflared_unit)"
	if [ "$CHANGES" != "$before" ]; then
		cloudflared_started &&
			run "mark $CLOUDFLARED_UNIT for restart" sh -c "mkdir -p $STATE_DIR && touch $(cloudflared_marker)"
		run "reload systemd for $CLOUDFLARED_UNIT" systemctl daemon-reload
	elif [ "$(systemctl show -p NeedDaemonReload --value "$CLOUDFLARED_UNIT.service")" != no ]; then
		run "reload systemd for $CLOUDFLARED_UNIT" systemctl daemon-reload
	fi
	systemctl is-enabled --quiet "$CLOUDFLARED_UNIT.service" 2>/dev/null ||
		run "enable $CLOUDFLARED_UNIT" systemctl enable "$CLOUDFLARED_UNIT.service"
	if ! cloudflared_started; then
		run "start $CLOUDFLARED_UNIT" sh -c "systemctl start $CLOUDFLARED_UNIT.service && rm -f $(cloudflared_marker)"
	elif cloudflared_stale; then
		run "restart $CLOUDFLARED_UNIT" sh -c "systemctl restart $CLOUDFLARED_UNIT.service && rm -f $(cloudflared_marker)"
	fi
}

cloudflared_unit_ok() {
	cloudflared_guard | cmp -s - "$CLOUDFLARED_GUARD" &&
		[ "$(stat -c '%U %a' "$CLOUDFLARED_GUARD")" = "root 755" ] &&
		cloudflared_unit | cmp -s - "$CLOUDFLARED_UNIT_PATH" &&
		[ "$(systemctl show -p NeedDaemonReload --value "$CLOUDFLARED_UNIT.service")" = no ] &&
		[ -z "$(systemctl show -p DropInPaths --value "$CLOUDFLARED_UNIT.service")" ]
}

cloudflared_running_ok() {
	systemctl is-enabled --quiet "$CLOUDFLARED_UNIT.service" &&
		systemctl is-active --quiet "$CLOUDFLARED_UNIT.service" && ! cloudflared_stale
}

cloudflared_verify() {
	local name="cloudflared $HANGAR_CLOUDFLARED_VERSION"
	if [ ! -e "$CLOUDFLARED_ENV" ]; then
		if cloudflared_up; then
			check "$CLOUDFLARED_UNIT down ($(systemctl show -p ActiveState,SubState --value "$CLOUDFLARED_UNIT.service" | paste -sd/)): there is no $CLOUDFLARED_ENV" false
		else
			skip "$name" "no $CLOUDFLARED_ENV"
		fi
		return 0
	fi
	check "$CLOUDFLARED_ENV root's, 0600, with TUNNEL_TOKEN" cloudflared_env_ok
	local problem
	problem=$(cloudflared_auth_problem)
	check "$CLOUDFLARED_UNIT only with the apiserver in oidc auth" test -z "$problem"
	if [ -n "$problem" ]; then
		# Starting or waiting to retry counts: its guard is all that stands
		# between it and the apiserver.
		cloudflared_up && check "$CLOUDFLARED_UNIT down while the apiserver is not in oidc auth" false
		return 0
	fi
	check "$name" cloudflared_is_pinned
	check "unit $CLOUDFLARED_UNIT and its guard as generated and loaded, no drop-ins" cloudflared_unit_ok
	check "unit $CLOUDFLARED_UNIT enabled, active, running what is installed" cloudflared_running_ok
}
