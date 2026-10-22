# The host's own firewall: nothing reaches the host from outside but SSH
# for the operator, and :80 and :443 for hangar-edge (docs/decisions/front-door.md).
# One nft table of its own, inet hangar-host, whose input chain drops what it
# does not accept, IPv4 and IPv6 alike, on every interface but loopback. It
# accepts the replies to what the host sent, ICMP and ICMPv6 (neighbour
# discovery, the proxy NDP that gives guests their IPv6, path MTU), and the
# uplink's DHCPv4 replies. Nothing at all reaches the host at
# HANGAR_SHARED_IPV4, the guests' address, if set: it is theirs alone, so
# a scan or a flood of it never meets the host's SSH or front door. The
# guests' replies and forwards to them are forwarded, never input, so
# dropping it here takes none of them. Forwarded traffic, the guests', is the node
# agent's tables' alone (docs/decisions/networking.md); this table has no
# forward chain. It is loaded by a unit of its own before the network comes
# up, never by nftables.service, whose stock config flushes every table,
# the node agent's too.

FIREWALL_TABLE=hangar-host
FIREWALL_FILE=/etc/hangar/firewall.nft
FIREWALL_UNIT=hangar-firewall
FIREWALL_UNIT_PATH=/etc/systemd/system/$FIREWALL_UNIT.service
# What the table held when this step last loaded it, as nft lists it: a
# table changed since by hand is loaded again.
FIREWALL_LIVE=$STATE_DIR/firewall.live
# The TCP ports anyone may reach: the operator's SSH, and the front door.
FIREWALL_TCP_PORTS="22, 80, 443"

# firewall_shared_rule prints the input rule dropping all for the shared
# address, or nothing without one.
firewall_shared_rule() {
	[ -n "${HANGAR_SHARED_IPV4:-}" ] || return 0
	printf '\n\t\tip daddr %s drop' "$HANGAR_SHARED_IPV4"
}

firewall_rules() {
	cat <<EOF
#!/usr/sbin/nft -f
# Written by deploy/host/steps/45-firewall.sh. Replaces table inet
# $FIREWALL_TABLE as a whole, in one transaction, and touches no other table.
table inet $FIREWALL_TABLE
delete table inet $FIREWALL_TABLE
table inet $FIREWALL_TABLE {
	chain input {
		type filter hook input priority filter; policy drop;
		iif "lo" accept$(firewall_shared_rule)
		ct state established,related accept
		ct state invalid drop
		meta l4proto icmp accept
		meta l4proto ipv6-icmp accept
		udp sport 67 udp dport 68 accept
		tcp dport { $FIREWALL_TCP_PORTS } accept
	}
}
EOF
}

firewall_unit() {
	cat <<EOF
# Written by deploy/host/steps/45-firewall.sh.
[Unit]
Description=Hangar host firewall (table inet $FIREWALL_TABLE)
# Up before any interface is, so the host is never open.
DefaultDependencies=no
Before=network-pre.target shutdown.target
Wants=network-pre.target
Conflicts=shutdown.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f $FIREWALL_FILE
# Stopping it opens nothing: the table stays until another rules file
# replaces it.

[Install]
WantedBy=sysinit.target
EOF
}

firewall_live() { nft list table inet "$FIREWALL_TABLE" 2>/dev/null; }

# firewall_loaded holds when the table is what this step last loaded.
firewall_loaded() {
	local live
	live=$(firewall_live) || return 1
	[ -n "$live" ] && [ -f "$FIREWALL_LIVE" ] && [ "$live" = "$(cat "$FIREWALL_LIVE")" ]
}

firewall_load() {
	nft -f "$FIREWALL_FILE" && mkdir -p "$STATE_DIR" && firewall_live >"$FIREWALL_LIVE"
}

# firewall_closed_listeners prints the host's listeners the firewall keeps
# everyone out of: on an address other than loopback, and a port it does
# not open.
firewall_closed_listeners() {
	ss -Hltnu | awk '{print $1, $5}' | while read -r proto addr; do
		local port=${addr##*:} host=${addr%:*}
		case $host in 127.* | '[::1]' | '[::ffff:127.'*) continue ;; esac
		if [ "$proto" = tcp ] && [[ ", $FIREWALL_TCP_PORTS, " == *", $port, "* ]]; then
			continue
		fi
		[ "$proto" = udp ] && [ "$port" = 68 ] && continue
		printf '%s %s\n' "$proto" "$addr"
	done | sort -u
}

firewall_unit_ok() {
	generated_is "$FIREWALL_UNIT_PATH" firewall_unit &&
		[ "$(systemctl show -p NeedDaemonReload --value "$FIREWALL_UNIT.service")" = no ] &&
		[ -z "$(systemctl show -p DropInPaths --value "$FIREWALL_UNIT.service")" ]
}

firewall_apply() {
	local before=$CHANGES closed
	put_file "$FIREWALL_FILE" 600 "$(firewall_rules)"
	local rules_changed=$((CHANGES - before))
	before=$CHANGES
	put_file "$FIREWALL_UNIT_PATH" 644 "$(firewall_unit)"
	if [ "$CHANGES" != "$before" ] || [ "$(systemctl show -p NeedDaemonReload --value "$FIREWALL_UNIT.service")" != no ]; then
		run "reload systemd for $FIREWALL_UNIT" systemctl daemon-reload
	fi
	systemctl is-enabled --quiet "$FIREWALL_UNIT.service" 2>/dev/null ||
		run "enable $FIREWALL_UNIT" systemctl enable "$FIREWALL_UNIT.service"
	if [ "$rules_changed" != 0 ] || ! firewall_loaded; then
		closed=$(firewall_closed_listeners | paste -sd, - | sed 's/,/, /g')
		[ -z "$closed" ] || log "warning: the firewall keeps everyone out of these listeners: $closed"
		run "load the firewall, table inet $FIREWALL_TABLE (TCP $FIREWALL_TCP_PORTS open)" firewall_load
	fi
	# Loaded by the step itself; the unit is what loads it at boot.
	systemctl is-active --quiet "$FIREWALL_UNIT.service" ||
		run "mark $FIREWALL_UNIT active" systemctl start "$FIREWALL_UNIT.service"
}

firewall_verify() {
	check "firewall rules $FIREWALL_FILE, root's, 600, as generated" \
		sh -c "[ \"\$(stat -c '%U %a' '$FIREWALL_FILE')\" = 'root 600' ]"
	check "firewall rules $FIREWALL_FILE as generated" generated_is "$FIREWALL_FILE" firewall_rules
	check "unit $FIREWALL_UNIT as generated and loaded, no drop-ins" firewall_unit_ok
	check "unit $FIREWALL_UNIT enabled and active" sh -c "systemctl is-enabled --quiet $FIREWALL_UNIT.service && systemctl is-active --quiet $FIREWALL_UNIT.service"
	check "table inet $FIREWALL_TABLE as loaded: input dropped but TCP $FIREWALL_TCP_PORTS" firewall_loaded
}
