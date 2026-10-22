# Takes the uplink's netplan config over from the installer and moves the host's
# IPv6 address to HANGAR_IPV6_ADDR. IPv4 stays on plain DHCP; the step refuses
# when the installer configured anything it would not carry over. A timer rolls
# netplan back unless the new config proves itself.

NETPLAN_FILE=/etc/netplan/10-hangar-uplink.yaml
CLOUD_INIT_NET_CFG=/etc/cloud/cloud.cfg.d/99-zz-hangar-network.cfg
# The probe loop must finish well inside the timer, so the timer never races
# the script's own verdict.
ROLLBACK_SECONDS=180
PROBE_ATTEMPTS=15

# netplan names the networkd unit after the ethernets key, which is the uplink.
network_dropin() { printf '/etc/systemd/network/10-netplan-%s.network.d/60-hangar.conf' "$HANGAR_UPLINK"; }

# The host answers neighbour solicitations for guest addresses on the uplink,
# because the upstream drops traffic for MACs it has not seen. netplan has no
# key for this, and networkd resets the sysctl on reconfigure unless told here.
network_dropin_conf() {
	printf '# Written by deploy/host/steps/40-network.sh.\n[Network]\nIPv6ProxyNDP=yes\n'
}

network_proxy_ndp_live() { [ "$(sysctl -n "net.ipv6.conf.$HANGAR_UPLINK.proxy_ndp")" = 1 ]; }

network_dropin_ok() { generated_is "$(network_dropin)" network_dropin_conf; }

network_proxy_ndp_ok() { network_dropin_ok && network_proxy_ndp_live; }

# The uplink is matched by MAC and named explicitly, so its name does not depend
# on udev's naming policy after a reboot. networkd matches the permanent MAC,
# which virtual links lack; HANGAR_UPLINK_MATCH=name exists for the testbed.
network_netplan() {
	local match
	if [ "${HANGAR_UPLINK_MATCH:-mac}" = mac ]; then
		match=$(printf '      match:\n        macaddress: "%s"\n      set-name: %s' "$HANGAR_UPLINK_MAC" "$HANGAR_UPLINK")
	else
		match=$(printf '      match:\n        name: %s' "$HANGAR_UPLINK")
	fi
	cat <<EOF
# Written by deploy/host/steps/40-network.sh.
network:
  version: 2
  renderer: networkd
  ethernets:
    $HANGAR_UPLINK:
$match
      dhcp4: true
      dhcp6: false
      accept-ra: false
      addresses:
        - "$HANGAR_IPV6_ADDR"
      routes:
        - to: "::/0"
          via: "$HANGAR_IPV6_GATEWAY"
          on-link: true
EOF
}

# network_conflicts prints the other netplan files that configure the uplink,
# matched by key, match.name, set-name or MAC address. It fails when one of
# them sets anything network_netplan does not reproduce or deliberately replace
# (only the IPv6 address and route are replaced), so nothing the installer
# configured is dropped silently.
network_conflicts() {
	python3 - "$HANGAR_UPLINK" "$HANGAR_UPLINK_MAC" "$NETPLAN_FILE" <<'EOF'
import glob, ipaddress, sys, yaml
uplink, mac, ours = sys.argv[1], sys.argv[2], sys.argv[3]
carried = {"match", "set-name", "dhcp4", "dhcp6", "accept-ra", "addresses", "routes", "gateway6"}
problems = []
files = set()

def is_v6(value):
    try:
        return ipaddress.ip_interface(str(value)).version == 6
    except ValueError:
        return str(value) in ("::/0", "default")

for path in sorted(glob.glob("/etc/netplan/*.yaml")):
    if path == ours:
        continue
    with open(path) as f:
        doc = yaml.safe_load(f) or {}
    for key, cfg in ((doc.get("network") or {}).get("ethernets") or {}).items():
        cfg = cfg or {}
        match = cfg.get("match") or {}
        if not (key == uplink or match.get("name") == uplink
                or cfg.get("set-name") == uplink
                or str(match.get("macaddress", "")).lower() == mac):
            continue
        files.add(path)
        where = f"{path}: ethernets.{key}"
        for k in sorted(set(cfg) - carried):
            problems.append(f"{where} sets {k}")
        if set(match) - {"macaddress", "name"}:
            problems.append(f"{where} matches on {sorted(match)}")
        if cfg.get("dhcp4") is not True:
            problems.append(f"{where} does not use dhcp4: true")
        for a in cfg.get("addresses") or []:
            if not is_v6(a):
                problems.append(f"{where} has address {a}")
        for r in cfg.get("routes") or []:
            if not (is_v6(r.get("to", "")) or is_v6(r.get("via", ""))):
                problems.append(f"{where} has route {r}")
for path in sorted(files):
    print(path)
for p in problems:
    print("netplan: " + p + "; this step would drop it", file=sys.stderr)
sys.exit(1 if problems else 0)
EOF
}

network_ipv6_ok() {
	uplink_global_ipv6 | grep -qx "$HANGAR_IPV6_ADDR" &&
		! ip -6 -o addr show dev "$HANGAR_UPLINK" | grep -Eq 'tentative|dadfailed' &&
		ping -6 -c 1 -W 2 -I "$HANGAR_UPLINK" "$HANGAR_IPV6_GATEWAY" >/dev/null 2>&1
}

network_ipv4_ok() {
	ip -4 -o addr show dev "$HANGAR_UPLINK" scope global | grep -q . &&
		ip -4 route show default | grep -q .
}

network_owned() {
	local conflicts
	conflicts=$(network_conflicts) || return 1
	[ -z "$conflicts" ] && generated_is "$NETPLAN_FILE" network_netplan
}

# network_stale_ipv6 prints global uplink addresses other than the wanted one.
network_stale_ipv6() { uplink_global_ipv6 | grep -vx "$HANGAR_IPV6_ADDR" || true; }

# network_validate renders the would-be /etc/netplan in a scratch root, so a
# config netplan rejects never reaches the live system.
network_validate() {
	local root f conf
	root=$(mktemp -d)
	mkdir -p "$root/etc/netplan"
	cp -a /etc/netplan/. "$root/etc/netplan/"
	for f in "$@"; do mv "$root$f" "$root$f.hangar-disabled"; done
	# As put_file writes, so network_owned, which compares as put_file does,
	# holds for what is installed; captured first, so a failing generator
	# stops the run.
	conf=$(network_netplan)
	printf '%s\n' "$conf" >"$root$NETPLAN_FILE"
	chmod 600 "$root$NETPLAN_FILE"
	netplan generate --root-dir "$root" || { rm -rf "$root"; return 1; }
	rm -rf "$root"
}

network_switch() {
	local backup f i conf
	backup=$STATE_DIR/netplan-$(date +%Y%m%dT%H%M%S)
	mkdir -p "$backup"
	cp -a /etc/netplan/. "$backup/"
	cat >"$backup/rollback.sh" <<EOF
#!/bin/sh
find /etc/netplan -mindepth 1 -delete
cp -a "$backup/." /etc/netplan/
rm -f /etc/netplan/rollback.sh
netplan apply
EOF
	chmod 755 "$backup/rollback.sh"
	log "netplan backup and rollback script: $backup"

	systemctl reset-failed "$ROLLBACK_UNIT.service" 2>/dev/null || true
	systemd-run --quiet --unit="$ROLLBACK_UNIT" --on-active="$ROLLBACK_SECONDS" \
		--timer-property=AccuracySec=1s "$backup/rollback.sh"

	for f in "$@"; do mv "$f" "$f.hangar-disabled"; done
	conf=$(network_netplan)
	printf '%s\n' "$conf" >"$NETPLAN_FILE.hangar-new"
	chmod 600 "$NETPLAN_FILE.hangar-new"
	mv -f "$NETPLAN_FILE.hangar-new" "$NETPLAN_FILE"
	netplan apply

	for i in $(seq 1 "$PROBE_ATTEMPTS"); do
		for f in $(network_stale_ipv6); do ip -6 addr del "$f" dev "$HANGAR_UPLINK" || true; done
		if network_ipv6_ok && network_ipv4_ok; then
			rollback_pending || die "the rollback timer already fired; inspect the host by hand"
			systemctl stop "$ROLLBACK_UNIT.timer"
			log "new netplan config confirmed after ${i} attempt(s); rollback cancelled"
			return 0
		fi
		sleep 2
	done
	log "the new config did not bring up IPv6 and IPv4; rolling back now"
	systemctl stop "$ROLLBACK_UNIT.timer"
	"$backup/rollback.sh"
	die "netplan rolled back to $backup"
}

network_apply() {
	local conflicts f
	[ -d "/sys/class/net/$HANGAR_UPLINK" ] || die "uplink $HANGAR_UPLINK does not exist"
	[ "$(cat "/sys/class/net/$HANGAR_UPLINK/address")" = "$HANGAR_UPLINK_MAC" ] ||
		die "uplink $HANGAR_UPLINK does not have MAC $HANGAR_UPLINK_MAC"

	if [ -d /etc/cloud/cloud.cfg.d ]; then
		put_file "$CLOUD_INIT_NET_CFG" 644 'network: {config: disabled}'
	fi
	# Written before any switch, so the netplan apply below picks it up under
	# the rollback timer.
	put_file "$(network_dropin)" 644 "$(network_dropin_conf)"

	if ! network_owned; then
		conflicts=$(network_conflicts) || die "refusing to take over the uplink's netplan config"
		[ -f "$NETPLAN_FILE" ] && network_netplan | diff -u "$NETPLAN_FILE" - >&2 || true
		[ -f "$NETPLAN_FILE" ] || network_netplan >&2
		for f in $conflicts; do log "--- would disable $f:"; cat "$f" >&2; done
		# shellcheck disable=SC2086
		network_validate $conflicts || die "netplan rejects the new config"
		if ! network_ipv6_gateway_reachable_now; then
			[ "$DRY_RUN" = 1 ] ||
				die "IPv6 gateway $HANGAR_IPV6_GATEWAY does not answer before the change; refusing to switch"
			log "warning: IPv6 gateway $HANGAR_IPV6_GATEWAY does not answer now; a real run would refuse"
		fi
		# shellcheck disable=SC2086
		run "switch the uplink to $NETPLAN_FILE (auto-rollback in ${ROLLBACK_SECONDS}s)" \
			network_switch $conflicts
	fi

	for f in $(network_stale_ipv6); do
		run "remove stale address $f from $HANGAR_UPLINK" ip -6 addr del "$f" dev "$HANGAR_UPLINK"
	done
	network_proxy_ndp_live ||
		run "reconfigure $HANGAR_UPLINK for proxy NDP" network_reconfigure_proxy_ndp
}

# networkctl reconfigure returns before networkd has applied the link settings,
# and it re-adds the addresses, which sit through duplicate address detection
# again; the step is done only when the link is fully back.
network_reconfigure_proxy_ndp() {
	local i
	networkctl reconfigure "$HANGAR_UPLINK"
	for i in $(seq 1 30); do
		network_proxy_ndp_live && network_ipv6_ok && network_ipv4_ok && return 0
		sleep 1
	done
	die "$HANGAR_UPLINK did not come back with proxy NDP, IPv6 and IPv4 within ${i}s"
}

# The pre-switch probe tells "the gateway never answers ping" apart from "the
# new config broke IPv6", which would otherwise look the same and roll back.
network_ipv6_gateway_reachable_now() {
	ping -6 -c 1 -W 2 -I "$HANGAR_UPLINK" "$HANGAR_IPV6_GATEWAY" >/dev/null 2>&1
}

network_no_rollback_pending() { ! rollback_pending; }

# cloud-init merges cloud.cfg.d in name order, so the last file wins the key.
network_cloud_init_off() {
	grep -qx 'network: {config: disabled}' "$CLOUD_INIT_NET_CFG" &&
		[ "$(printf '%s\n' /etc/cloud/cloud.cfg.d/*.cfg | LC_ALL=C sort | tail -n 1)" = "$CLOUD_INIT_NET_CFG" ]
}

network_verify() {
	check "netplan config for $HANGAR_UPLINK is ours alone" network_owned
	check "cloud-init network config disabled, last word in cloud.cfg.d" network_cloud_init_off
	check "uplink IPv6 is $HANGAR_IPV6_ADDR and the gateway answers" network_ipv6_ok
	check "no other global IPv6 address on $HANGAR_UPLINK" test -z "$(network_stale_ipv6)"
	check "IPv4 address on $HANGAR_UPLINK and a default route" network_ipv4_ok
	check "proxy NDP on $HANGAR_UPLINK, live and owned by networkd" network_proxy_ndp_ok
	check "no netplan rollback pending" network_no_rollback_pending
}
