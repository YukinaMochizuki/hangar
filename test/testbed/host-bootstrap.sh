#!/usr/bin/env bash
# Exercises deploy/host on a disposable Ubuntu VM made to look like a freshly
# installed OVH host: two plain swap partitions (loop devices), an installer-made
# pool named hangar (file-backed), and an uplink whose IPv6 is the prefix's ::
# address behind an on-link gateway outside the /64 (a veth into a netns).
#
# Run as root on a throwaway VM only: it rewrites fstab, netplan and sysctl.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
host_dir=$repo/deploy/host
work=/var/tmp/hangar-testbed
env_file=$work/testbed.env

swap0=
swap1=

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "### $*" >&2; }

[ "$(id -u)" = 0 ] || fail "must run as root"

# state_digest summarises everything a dry run must leave untouched.
state_digest() {
	{
		cat /etc/fstab /proc/swaps /etc/mdadm/mdadm.conf
		ls -lR /etc/netplan /etc/sysctl.d /etc/modprobe.d /etc/modules-load.d \
			/etc/cloud/cloud.cfg.d /etc/systemd/network /etc/initramfs-tools/conf.d
		ls -l /usr/local/bin /var/lib/hangar-bootstrap
		cat /etc/netplan/*.yaml
		ip -6 addr show dev tb-uplink
		zfs list -H -o name,mountpoint -r hangar
		zfs get -H -o property,value compression,atime hangar
		zpool get -H -o value autotrim hangar
		cat /sys/module/zfs/parameters/zfs_arc_max
		sysctl -n net.ipv6.conf.all.forwarding net.ipv6.conf.tb-uplink.proxy_ndp
	} 2>&1 | sha256sum
}

prepare() {
	step "prepare a host that looks freshly installed"
	DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends \
		zfsutils-linux python3-yaml dnsmasq-base >/dev/null
	mkdir -p "$work"

	local i dev
	for i in 0 1; do
		truncate -s 512M "$work/swap$i.img"
		dev=$(losetup --find --show "$work/swap$i.img")
		mkswap -q "$dev"
		swapon "$dev"
		echo "$dev none swap sw 0 0" >>/etc/fstab
		if [ "$i" = 0 ]; then swap0=$dev; else swap1=$dev; fi
	done

	truncate -s 2G "$work/pool.img"
	zpool create -o ashift=12 -O mountpoint=none hangar "$work/pool.img"

	# The upstream: an IPv6 router outside the host's /64, and DHCPv4 that hands
	# out an address but no default route, so the VM keeps its own.
	ip netns add tb-upstream
	ip link add tb-uplink address 02:00:00:00:00:01 type veth peer name tb-gw netns tb-upstream
	ip -n tb-upstream link set lo up
	ip -n tb-upstream link set tb-gw up
	ip -n tb-upstream -6 addr add 2001:db8:0:ff::ffff/64 dev tb-gw nodad
	ip -n tb-upstream -6 route add 2001:db8:0:1::/64 dev tb-gw
	ip -n tb-upstream addr add 192.0.2.1/24 dev tb-gw
	ip netns exec tb-upstream dnsmasq --port=0 --interface=tb-gw --bind-interfaces \
		--dhcp-range=192.0.2.100,192.0.2.150,1h --dhcp-option=3 --dhcp-option=6 \
		--dhcp-leasefile="$work/leases" --pid-file="$work/dnsmasq.pid"

	# What the OVH installer writes: DHCPv4, and IPv6 on the prefix's :: address.
	# A veth has no permanent MAC for networkd to match, so this matches by name.
	cat >/etc/netplan/49-ovh-installer.yaml <<'EOF'
network:
  version: 2
  ethernets:
    eno1:
      match:
        name: tb-uplink
      dhcp4: true
      accept-ra: false
      addresses: ["2001:db8:0:1::/64"]
      routes:
        - to: "::/0"
          via: "2001:db8:0:ff::ffff"
          on-link: true
EOF
	chmod 600 /etc/netplan/49-ovh-installer.yaml
	netplan apply
	for _ in $(seq 1 15); do
		ip -4 -o addr show dev tb-uplink scope global | grep -q . && break
		sleep 2
	done
	ip -4 -o addr show dev tb-uplink scope global | grep -q . || fail "no DHCPv4 lease on tb-uplink"

	cat >"$env_file" <<EOF
HANGAR_UPLINK=tb-uplink
HANGAR_UPLINK_MAC=02:00:00:00:00:01
HANGAR_UPLINK_MATCH=name
HANGAR_IPV6_ADDR=2001:db8:0:1:0::1/64
HANGAR_IPV6_GATEWAY=2001:db8:0:ff::ffff
HANGAR_SWAP_PARTS="$swap0 $swap1"
HANGAR_SWAP_MD_NAME=swap
HANGAR_ZPOOL=hangar
HANGAR_ARC_MAX_BYTES=1073741824
EOF
}

test_failed_generator_writes_nothing() {
	step "a failing generator aborts before put_file writes"
	if bash -c "set -euo pipefail; . '$host_dir/lib.sh'
		gen() { local x; x=\$(false); echo partial; }
		c=\$(gen); put_file '$work/gen-target' 644 \"\$c\"" 2>/dev/null; then
		fail "the run went on after its generator failed"
	fi
	[ ! -e "$work/gen-target" ] || fail "put_file wrote the output of a failed generator"
}

test_refuses_unknown_installer_keys() {
	step "the network step refuses installer settings it would drop"
	cp /etc/netplan/49-ovh-installer.yaml "$work/49-ovh-installer.yaml.orig"
	sed -i 's|^      dhcp4: true|      dhcp4: true\n      nameservers: {addresses: [192.0.2.53]}|' /etc/netplan/49-ovh-installer.yaml
	if "$host_dir/bootstrap.sh" --env "$env_file" --only network --dry-run 2>"$work/keys.log"; then
		fail "network step accepted an installer stanza with nameservers"
	fi
	grep -q 'sets nameservers' "$work/keys.log" || fail "unexpected refusal: $(cat "$work/keys.log")"
	cp "$work/49-ovh-installer.yaml.orig" /etc/netplan/49-ovh-installer.yaml
}

test_refuses_non_swap_partition() {
	step "swap-raid refuses a partition that does not hold swap"
	local bad
	truncate -s 64M "$work/notswap.img"
	bad=$(losetup --find --show "$work/notswap.img")
	mkfs.ext4 -q "$bad"
	sed "s|^HANGAR_SWAP_PARTS=.*|HANGAR_SWAP_PARTS=\"$swap0 $bad\"|" "$env_file" >"$work/bad.env"
	if "$host_dir/bootstrap.sh" --env "$work/bad.env" --only swap-raid 2>"$work/bad.log"; then
		fail "bootstrap accepted an ext4 partition as swap"
	fi
	grep -q "refusing to wipe" "$work/bad.log" || fail "unexpected refusal: $(cat "$work/bad.log")"
	swap_active_count=$(awk 'NR > 1' /proc/swaps | wc -l)
	[ "$swap_active_count" = 2 ] || fail "refusal still touched swap"
	losetup -d "$bad"
}

test_dry_run_changes_nothing() {
	step "dry run reports changes and makes none"
	local before out
	before=$(state_digest)
	out=$("$host_dir/bootstrap.sh" --env "$env_file" --dry-run)
	[ "$out" != "changes: 0" ] || fail "dry run on a fresh host reported no changes"
	[ "$(state_digest)" = "$before" ] || fail "dry run changed the host"
}

test_converges_and_is_idempotent() {
	step "first run converges"
	local log applied
	log=$work/first-run.log
	"$host_dir/bootstrap.sh" --env "$env_file" 2>&1 | tee "$log"
	# The summary must count every change made, or "changes: 0" proves nothing.
	applied=$(grep -c '^apply: ' "$log")
	grep -qx "changes: $applied" "$log" || fail "summary does not match the $applied changes applied"
	step "second run changes nothing"
	local out
	out=$("$host_dir/bootstrap.sh" --env "$env_file")
	[ "$out" = "changes: 0" ] || fail "second run reported $out"
	step "verify passes"
	"$host_dir/verify.sh" --env "$env_file"
}

test_bad_ipv6_rolls_back() {
	step "a config whose gateway stops answering is rolled back"
	local before
	before=$(sha256sum /etc/netplan/*.yaml)
	# The upstream only routes 2001:db8:0:1::/64 back, so this address is dead.
	sed 's|^HANGAR_IPV6_ADDR=.*|HANGAR_IPV6_ADDR=2001:db8:0:2::1/64|' "$env_file" >"$work/dead.env"
	if "$host_dir/bootstrap.sh" --env "$work/dead.env" --only network; then
		fail "bootstrap accepted a config whose gateway does not answer"
	fi
	sleep 3
	[ "$(sha256sum /etc/netplan/*.yaml)" = "$before" ] || fail "netplan files not restored"
	"$host_dir/verify.sh" --env "$env_file" || fail "host does not verify after rollback"
}

test_refuses_while_rollback_pending() {
	step "bootstrap refuses to run while a netplan rollback is pending"
	systemd-run --quiet --unit=hangar-netplan-rollback --on-active=600 /bin/true
	if "$host_dir/bootstrap.sh" --env "$env_file" 2>"$work/pending.log"; then
		fail "bootstrap ran with a rollback pending"
	fi
	grep -q 'pending' "$work/pending.log" || fail "unexpected refusal: $(cat "$work/pending.log")"
	systemctl stop hangar-netplan-rollback.timer
}

test_verify_catches_drift() {
	step "verify reports drift and bootstrap repairs it"
	zfs set compression=off hangar
	echo 0 >/sys/module/zfs/parameters/zfs_arc_max
	sysctl -q -w net.ipv6.conf.tb-uplink.proxy_ndp=0
	# A zvol holding ext4, probed while udev's rule for zvols was gone.
	rm /etc/udev/rules.d/59-hangar-zvol.rules
	udevadm control --reload
	zfs create -s -V 16M hangar/poc/tb-probe
	udevadm settle
	mkfs.ext4 -q "$(readlink -f /dev/zvol/hangar/poc/tb-probe)"
	udevadm trigger --action=change --sysname-match="$(basename "$(readlink -f /dev/zvol/hangar/poc/tb-probe)")" --settle
	if "$host_dir/verify.sh" --env "$env_file" >"$work/drift.log"; then
		fail "verify passed on a drifted host"
	fi
	[ "$(grep -c '^FAIL' "$work/drift.log")" = 5 ] || fail "expected 5 FAIL lines: $(cat "$work/drift.log")"
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "bootstrap did not repair the drift"
	zfs destroy hangar/poc/tb-probe

	# A rule of the right content but the wrong mode is drift too.
	chmod 600 /etc/udev/rules.d/99-hangar-zvol.rules
	if "$host_dir/verify.sh" --env "$env_file" >"$work/mode.log"; then
		fail "verify passed with a udev rule of mode 600"
	fi
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null
	[ "$(stat -c %a /etc/udev/rules.d/99-hangar-zvol.rules)" = 644 ] || fail "bootstrap left the rule's mode"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "bootstrap did not repair the rule's mode"
}

prepare
test_failed_generator_writes_nothing
test_refuses_non_swap_partition
test_refuses_unknown_installer_keys
test_dry_run_changes_nothing
test_converges_and_is_idempotent
test_refuses_while_rollback_pending
test_verify_catches_drift
test_bad_ipv6_rolls_back
echo
echo "host-bootstrap testbed: all passed"
