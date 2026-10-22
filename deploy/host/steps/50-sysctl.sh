# Kernel settings the host needs to route for its guests: forwarding, and the
# vsock transport for guest agents. Proxy NDP is not here: networkd owns it and
# resets it on every reconfigure, so the network step sets it through networkd.

SYSCTL_FILE=/etc/sysctl.d/60-hangar.conf
MODULES_FILE=/etc/modules-load.d/hangar.conf
MODULES="vhost_vsock"

sysctl_conf() {
	cat <<EOF
# Written by deploy/host/steps/50-sysctl.sh.
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
net.ipv6.conf.$HANGAR_UPLINK.accept_ra = 0
EOF
}

# sysctl_runtime_ok compares every key in the wanted config with the live value.
sysctl_runtime_ok() {
	local key value
	while IFS=' =' read -r key value; do
		case $key in '' | '#'*) continue ;; esac
		[ "$(sysctl -n "$key")" = "$value" ] || return 1
	done < <(sysctl_conf)
}

sysctl_modules_loaded() {
	local m
	for m in $MODULES; do [ -d "/sys/module/$m" ] || return 1; done
}

# shellcheck disable=SC2086
sysctl_modules_persisted() { generated_is "$MODULES_FILE" printf '%s\n' $MODULES; }

sysctl_persisted() { generated_is "$SYSCTL_FILE" sysctl_conf; }

sysctl_no_subnet_router() { ! uplink_holds_subnet_router; }

sysctl_apply() {
	local m
	# With forwarding on, the kernel claims <prefix>:: as the Subnet-Router
	# anycast address, which breaks a host that uses it as its own unicast.
	rollback_pending && die "a netplan rollback is pending; it may restore the :: address"
	if uplink_holds_subnet_router; then
		[ "$DRY_RUN" = 1 ] ||
			die "$HANGAR_UPLINK still holds its prefix's :: address; run the network step first"
		log "warning: $HANGAR_UPLINK holds its prefix's :: address; a real run would refuse until the network step moves it"
	fi

	put_file "$SYSCTL_FILE" 644 "$(sysctl_conf)"
	sysctl_runtime_ok || run "load $SYSCTL_FILE" sysctl -q -p "$SYSCTL_FILE"

	# shellcheck disable=SC2086
	put_file "$MODULES_FILE" 644 "$(printf '%s\n' $MODULES)"
	for m in $MODULES; do
		[ -d "/sys/module/$m" ] || run "load module $m" modprobe "$m"
	done
}

sysctl_verify() {
	check "forwarding live" sysctl_runtime_ok
	check "forwarding persisted" sysctl_persisted
	check "uplink does not hold its prefix's :: address" sysctl_no_subnet_router
	check "modules $MODULES loaded" sysctl_modules_loaded
	check "modules $MODULES load at boot" sysctl_modules_persisted
}
