#!/usr/bin/env bash
# Exercises deploy/host on a disposable Ubuntu VM made to look like a freshly
# installed OVH host: two plain swap partitions (loop devices), an installer-made
# pool named hangar (file-backed), and an uplink whose IPv6 is the prefix's ::
# address behind an on-link gateway outside the /64 (a veth into a netns).
# HANGAR_TESTBED_RELEASE names a release directory (make release) for the
# release and services steps.
#
# Run as root on a throwaway VM only: it rewrites fstab, netplan and sysctl,
# and installs and starts hangar's services.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
host_dir=$repo/deploy/host
# shellcheck source=deploy/host/versions.env
. "$host_dir/versions.env"
work=/var/tmp/hangar-testbed
env_file=$work/testbed.env
release=${HANGAR_TESTBED_RELEASE:-}

swap0=
swap1=

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "### $*" >&2; }

[ "$(id -u)" = 0 ] || fail "must run as root"
[ -f "$release/SHA256SUMS" ] || fail "HANGAR_TESTBED_RELEASE must name a release directory (make release)"
release=$(cd "$release" && pwd)

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
		zfs get -H -o property,value -r setuid,exec,devices,quota,mounted hangar
		ls -lR /etc/systemd/system /etc/apt/apt.conf.d /etc/needrestart /usr/local/lib /etc/hangar
		ls -la /var/lib/hangar
		getent passwd hangar
		systemctl is-active hangar-apiserver hangar-node
		systemctl show -p TimersCalendar apt-daily.timer apt-daily-upgrade.timer
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
HANGAR_PUBLIC_IPV4=192.0.2.10
HANGAR_IMAGES_QUOTA=1G
HANGAR_VMS_QUOTA=1G
HANGAR_RESERVED_MEMORY=256Mi
HANGAR_NODE_NAME=testbed
EOF
	# What the operator writes by hand on a real host.
	mkdir -p /etc/hangar
	install -m 0600 /dev/null /etc/hangar/local.env
	echo 'HANGAR_BOOTSTRAP_ADMIN=admin@testbed.invalid' >/etc/hangar/local.env
}

# main_pid UNIT prints the main pid of a service, 0 when it is not running.
main_pid() { systemctl show -p MainPID --value "$1"; }

# make_release FROM TO VERSION copies a release and makes TO a new version of
# it: its binaries get bytes appended (an ELF file still runs with them), so
# they differ, and SHA256SUMS is made again.
make_release() {
	local from=$1 to=$2 version=$3 f
	rm -rf "$to"
	cp -r "$from" "$to"
	for f in hangar-node hangar-apiserver; do printf '%s' "$version" >>"$to/$f"; done
	echo "$version" >"$to/VERSION"
	(cd "$to" && rm SHA256SUMS && sha256sum -- * >SHA256SUMS)
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
	out=$("$host_dir/bootstrap.sh" --env "$env_file" --release "$release" --dry-run)
	[ "$out" != "changes: 0" ] || fail "dry run on a fresh host reported no changes"
	[ "$(state_digest)" = "$before" ] || fail "dry run changed the host"
}

test_converges_and_is_idempotent() {
	step "first run converges"
	local log applied
	log=$work/first-run.log
	"$host_dir/bootstrap.sh" --env "$env_file" --release "$release" 2>&1 | tee "$log"
	# The summary must count every change made, or "changes: 0" proves nothing.
	applied=$(grep -c '^apply: ' "$log")
	grep -qx "changes: $applied" "$log" || fail "summary does not match the $applied changes applied"
	step "second run changes nothing, with the same release or none"
	local out
	out=$("$host_dir/bootstrap.sh" --env "$env_file" --release "$release")
	[ "$out" = "changes: 0" ] || fail "second run reported $out"
	out=$("$host_dir/bootstrap.sh" --env "$env_file")
	[ "$out" = "changes: 0" ] || fail "a run without --release reported $out"
	step "verify passes"
	"$host_dir/verify.sh" --env "$env_file" | tee "$work/first-verify.log"
	[ "$(cat /usr/local/lib/hangar/VERSION)" = "$(cat "$release/VERSION")" ] || fail "installed VERSION is not the release's"

	step "without cloudflared.env there is no tunnel, and verify skips it"
	grep -q "^SKIP cloudflared $HANGAR_CLOUDFLARED_VERSION (no /etc/hangar/cloudflared.env)$" "$work/first-verify.log" ||
		fail "verify does not skip cloudflared"
	grep -q '^FAIL' "$work/first-verify.log" && fail "verify failed: $(grep '^FAIL' "$work/first-verify.log")"
	grep -qE '^(apply|would): .*cloudflared' "$log" && fail "the run did something about cloudflared: $(grep -E '^(apply|would): .*cloudflared' "$log")"
	[ ! -e /usr/local/bin/cloudflared ] && [ ! -e /etc/systemd/system/hangar-cloudflared.service ] ||
		fail "cloudflared was installed without cloudflared.env"
	systemctl show -p ExecStart --value hangar-apiserver | grep -q -- '--listen 127.0.0.1:8080 --auth-mode dev --socket' ||
		fail "an unset HANGAR_AUTH_MODE is not dev auth"
}

# admin_cli ARGS... runs the CLI through the admin socket, as root on the
# host.
admin_cli() { HANGAR_CONFIG=$work/admin.yaml /usr/local/bin/hangar "$@"; }

# list_nodes_status [HEADER...] prints the HTTP status of ListNodes on the
# public listener as the bootstrap admin in dev auth, with HEADERs.
list_nodes_status() {
	local h args=()
	for h in "$@"; do args+=(-H "$h"); done
	curl -s -o /dev/null -w '%{http_code}' -H 'X-Hangar-Dev-User: admin@testbed.invalid' \
		-H 'Content-Type: application/json' "${args[@]}" -d '{}' \
		http://127.0.0.1:8080/hangar.compute.v1alpha1.NodeService/ListNodes
}

test_admin_socket() {
	step "the admin socket is root's alone, and root on it holds platform.root"
	[ "$(stat -c '%U %G %a' /run/hangar)" = "root root 755" ] || fail "/run/hangar is $(stat -c '%U %G %a' /run/hangar)"
	[ "$(stat -c '%F %U %G %a' /run/hangar/admin.sock)" = "socket root root 600" ] ||
		fail "the admin socket is $(stat -c '%F %U %G %a' /run/hangar/admin.sock)"
	[ "$(stat -c '%F %U %G %a' /run/hangar/node.sock)" = "socket hangar hangar 660" ] ||
		fail "the node socket is $(stat -c '%F %U %G %a' /run/hangar/node.sock)"
	cat >"$work/admin.yaml" <<'EOF'
currentContext: local
contexts:
  local: {endpoint: "unix:///run/hangar/admin.sock"}
EOF
	chmod 644 "$work/admin.yaml"
	local since
	since=$(date '+%Y-%m-%d %H:%M:%S')
	admin_cli get node testbed | grep -qE '^testbed +True ' || fail "root could not get the node over the admin socket"
	# What a locked-out administrator does: bind someone as root again.
	admin_cli apply -f - <<'EOF' || fail "root could not bind over the admin socket"
apiVersion: iam.hangar.dev/v1alpha1
kind: RoleBinding
metadata: {name: tb-root-again}
spec: {subject: {user: again@testbed.invalid}, role: platform.root, scope: {platform: {}}}
EOF
	admin_cli delete rolebinding tb-root-again >/dev/null
	journalctl -u hangar-apiserver --since "$since" --no-pager |
		grep -q 'msg="admin socket request" method=POST path=/hangar.iam.v1alpha1.RoleBindingService/ApplyRoleBinding uid=0 pid=[0-9]* principal=local:root@testbed' ||
		fail "the apply is not in the apiserver's log as local:root@testbed"

	step "anyone else is kept out by the socket's mode, and by the apiserver if not"
	if runuser -u nobody -- env HANGAR_CONFIG="$work/admin.yaml" /usr/local/bin/hangar get node testbed 2>"$work/nobody.log"; then
		fail "nobody got the node over the admin socket"
	fi
	grep -q 'permission denied' "$work/nobody.log" || fail "unexpected refusal: $(cat "$work/nobody.log")"
	if runuser -u hangar -- env HANGAR_CONFIG="$work/admin.yaml" /usr/local/bin/hangar get node testbed 2>/dev/null; then
		fail "the apiserver's own user got in over the admin socket"
	fi
	chmod 666 /run/hangar/admin.sock
	if runuser -u nobody -- env HANGAR_CONFIG="$work/admin.yaml" /usr/local/bin/hangar get node testbed 2>"$work/nobody.log"; then
		chmod 600 /run/hangar/admin.sock
		fail "nobody got the node through a socket left open"
	fi
	chmod 600 /run/hangar/admin.sock
	grep -q 'is not root' "$work/nobody.log" || fail "the apiserver did not refuse nobody's uid: $(cat "$work/nobody.log")"

	step "dev auth refuses what came through Cloudflare"
	[ "$(list_nodes_status)" = 200 ] || fail "dev auth on loopback refused the bootstrap admin"
	local h
	for h in 'Cdn-Loop: cloudflare' 'Cf-Connecting-Ip: 192.0.2.7' 'Cf-Ray: 0123456789abcdef-SIN'; do
		[ "$(list_nodes_status "$h")" = 401 ] || fail "dev auth took a request with $h"
	done
}

# node_connected prints how many connections the running apiserver holds on
# the node socket.
node_connected() {
	ss -Hxp state established src /run/hangar/node.sock 2>/dev/null |
		grep -c "pid=$(main_pid hangar-apiserver)," || true
}

# wait_node_connected waits for the agent to be connected to the apiserver.
wait_node_connected() {
	local i
	for i in $(seq 1 30); do
		[ "$(node_connected)" -gt 0 ] && return 0
		sleep 1
	done
	return 1
}

test_node_reconnects() {
	step "the agent reconnects when the apiserver restarts, and its sockets with it"
	local node_pid api_pid ino
	wait_node_connected || fail "the agent is not connected to begin with"
	node_pid=$(main_pid hangar-node)
	api_pid=$(main_pid hangar-apiserver)
	ino=$(stat -c %i /run/hangar/node.sock)
	systemctl restart hangar-apiserver
	[ "$(main_pid hangar-apiserver)" != "$api_pid" ] || fail "the apiserver did not restart"
	systemctl is-active --quiet hangar-apiserver-node.socket hangar-apiserver-admin.socket || fail "its sockets are not active"
	[ "$(stat -c %i /run/hangar/node.sock)" != "$ino" ] || fail "the node socket was not made again"
	wait_node_connected || fail "the agent did not reconnect"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "the agent restarted instead of reconnecting"
	"$host_dir/verify.sh" --env "$env_file" >"$work/reconnect.log" || fail "verify after the restart: $(grep FAIL "$work/reconnect.log")"

	step "a socket restarted alone stays down; verify says so and bootstrap restarts the apiserver"
	api_pid=$(main_pid hangar-apiserver)
	systemctl restart hangar-apiserver-node.socket 2>/dev/null && fail "systemd restarted a socket whose service runs"
	wait_node_connected || fail "the agent lost the apiserver over a socket unit"
	if "$host_dir/verify.sh" --env "$env_file" >"$work/reconnect.log"; then fail "verify passed with a socket down"; fi
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/reconnect.log"
	grep -q '^apply: restart hangar-apiserver$' "$work/reconnect.log" || fail "bootstrap did not restart the apiserver: $(cat "$work/reconnect.log")"
	systemctl is-active --quiet hangar-apiserver-node.socket || fail "the node socket is still down"
	wait_node_connected || fail "the agent did not reconnect"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged after the socket"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify after the socket"
}

TB_TEAM=https://testbed.cloudflareaccess.com
TB_AUD=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

# fake_token N prints a well-formed tunnel token for no tunnel: cloudflared
# takes it, and Cloudflare refuses it.
fake_token() {
	printf '{"a":"%s","t":"%s","s":"%s"}' 0123456789abcdef0123456789abcdef 6f1c1b8e-2b1a-4c3d-9e8f-0a1b2c3d4e5f \
		"$(printf 'testbed-%s' "$1" | base64 -w0)" | base64 -w0
}

put_tunnel_token() {
	install -m 0600 /dev/null /etc/hangar/cloudflared.env
	echo "TUNNEL_TOKEN=$(fake_token "$1")" >/etc/hangar/cloudflared.env
}

# auth_env MODE sets the auth settings in local.env: cloudflare with the
# testbed's team and AUD, or none, which is dev.
auth_env() {
	sed -i '/^HANGAR_AUTH_MODE=/d; /^HANGAR_CF_/d' /etc/hangar/local.env
	if [ "$1" = cloudflare ]; then
		printf 'HANGAR_AUTH_MODE=cloudflare\nHANGAR_CF_TEAM_DOMAIN=%s\nHANGAR_CF_AUDIENCE=%s\n' "$TB_TEAM" "$TB_AUD" >>/etc/hangar/local.env
	fi
}

# sandbox_probe [PROPERTY...] runs a probe under hangar-cloudflared's
# [Service] settings, but for ExecStart and the restart policy, with
# PROPERTYs on top, and prints what it could reach.
sandbox_probe() {
	local props=() line
	while IFS= read -r line; do
		case $line in '' | '#'* | '['* | ExecStart=* | Restart=* | RestartSec=* | Type=*) continue ;; esac
		props+=(-p "$line")
	done < <(sed -n '/^\[Service\]/,/^\[Install\]/p' /etc/systemd/system/hangar-cloudflared.service)
	systemd-run --quiet --wait --pipe --collect "${props[@]}" "$@" /usr/bin/python3 - <<'EOF'
import os, socket
print("uid", os.getuid())
for p in ["/run/hangar/admin.sock", "/run/hangar/node.sock"]:
    try:
        s = socket.socket(socket.AF_UNIX)
        s.connect(p)
        print("connected", p)
    except OSError as e:
        print("refused", p, e.strerror)
for d in ["/run/hangar", "/var/lib/hangar", "/var/lib/hangar/state", "/etc/hangar"]:
    try:
        print("listed", d, os.listdir(d))
    except OSError as e:
        print("denied", d, e.strerror)
EOF
}

test_cloudflared_needs_cloudflare_auth() {
	step "cloudflared is refused while the apiserver is in dev auth, and nothing installed"
	local before
	put_tunnel_token 1
	before=$(state_digest)
	if "$host_dir/bootstrap.sh" --env "$env_file" --only cloudflared 2>"$work/cf.log"; then
		fail "the cloudflared step ran with the apiserver in dev auth"
	fi
	grep -q 'never forwards to an apiserver in dev auth' "$work/cf.log" || fail "unexpected refusal: $(cat "$work/cf.log")"
	[ "$(state_digest)" = "$before" ] || fail "the refusal changed the host"
	[ ! -e /usr/local/bin/cloudflared ] || fail "cloudflared was installed"
	if "$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cf.log"; then
		fail "a whole run went through with cloudflared.env in dev auth"
	fi
	grep -q '^apply: ' "$work/cf.log" && fail "the refused run changed something: $(grep '^apply: ' "$work/cf.log")"
	if "$host_dir/verify.sh" --env "$env_file" >"$work/cf-verify.log"; then fail "verify passed with cloudflared.env in dev auth"; fi
	grep -q '^FAIL hangar-cloudflared only with the apiserver in cloudflare auth$' "$work/cf-verify.log" ||
		fail "verify does not say why: $(grep FAIL "$work/cf-verify.log")"
	rm /etc/hangar/cloudflared.env

	step "a cloudflare mode without its team domain and AUD is refused before any step"
	echo HANGAR_AUTH_MODE=cloudflare >>/etc/hangar/local.env
	if "$host_dir/bootstrap.sh" --env "$env_file" 2>"$work/cf.log"; then fail "bootstrap ran with half a cloudflare mode"; fi
	{ grep -q 'HANGAR_CF_TEAM_DOMAIN=https://<team>.cloudflareaccess.com' "$work/cf.log" &&
		grep -q 'HANGAR_CF_AUDIENCE=' "$work/cf.log"; } || fail "unexpected refusal: $(cat "$work/cf.log")"
	grep -q '^== ' "$work/cf.log" && fail "steps ran before the refusal"
	if "$host_dir/bootstrap.sh" --env "$env_file" --only services 2>/dev/null; then fail "the services step ran with half a cloudflare mode"; fi
	auth_env dev
}

test_cloudflare_cutover() {
	step "cloudflare auth in local.env restarts the apiserver in it, still on loopback"
	local api_pid node_pid out
	api_pid=$(main_pid hangar-apiserver)
	node_pid=$(main_pid hangar-node)
	auth_env cloudflare
	"$host_dir/bootstrap.sh" --env "$env_file" --only services >/dev/null 2>"$work/cut.log"
	grep -q '^apply: restart hangar-apiserver$' "$work/cut.log" || fail "the apiserver was not restarted: $(cat "$work/cut.log")"
	[ "$(main_pid hangar-apiserver)" != "$api_pid" ] || fail "the apiserver runs its old auth"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "the node was restarted for the apiserver's auth"
	systemctl show -p ExecStart --value hangar-apiserver |
		grep -q -- "--listen 127.0.0.1:8080 --auth-mode cloudflare --cf-team-domain $TB_TEAM --cf-audience $TB_AUD --socket" ||
		fail "the apiserver's unit does not have cloudflare auth"
	[ "$(list_nodes_status)" = 401 ] || fail "cloudflare auth took a dev header"
	admin_cli get node testbed | grep -qE '^testbed +True ' || fail "the admin socket does not work in cloudflare auth"
	"$host_dir/verify.sh" --env "$env_file" >"$work/cut-verify.log" || fail "verify in cloudflare auth: $(grep FAIL "$work/cut-verify.log")"
	grep -q "^PASS apiserver runs cloudflare auth as /etc/hangar/local.env asks: --auth-mode cloudflare --cf-team-domain $TB_TEAM --cf-audience $TB_AUD$" "$work/cut-verify.log" ||
		fail "verify does not check the auth flags"
	grep -q '^SKIP cloudflared' "$work/cut-verify.log" || fail "verify does not skip cloudflared without its env"

	step "then the tunnel token: cloudflared installed, enabled and started, and a second run changes nothing"
	put_tunnel_token 1
	"$host_dir/bootstrap.sh" --env "$env_file" 2>&1 | tee "$work/cut.log"
	grep -qx "changes: $(grep -c '^apply: ' "$work/cut.log")" "$work/cut.log" || fail "summary does not match its changes"
	for out in "install cloudflared $HANGAR_CLOUDFLARED_VERSION" 'write /etc/systemd/system/hangar-cloudflared.service' \
		'enable hangar-cloudflared' 'start hangar-cloudflared'; do
		grep -qx "apply: $out" "$work/cut.log" || fail "the run did not $out"
	done
	echo "$HANGAR_CLOUDFLARED_SHA256  /usr/local/bin/cloudflared" | sha256sum -c --quiet - || fail "cloudflared is not the pinned one"
	systemctl is-active --quiet hangar-cloudflared || fail "cloudflared is not active"
	out=$("$host_dir/bootstrap.sh" --env "$env_file")
	[ "$out" = "changes: 0" ] || fail "the run after cloudflared reported $out"
	"$host_dir/verify.sh" --env "$env_file" >"$work/cut-verify.log" || fail "verify with cloudflared: $(grep FAIL "$work/cut-verify.log")"
	grep -q "^PASS unit hangar-cloudflared enabled, active, running what is installed$" "$work/cut-verify.log" || fail "verify does not check cloudflared"
	systemctl show -p ExecStart --value hangar-cloudflared | grep -q -- '--no-autoupdate --metrics 127.0.0.1:20241 run' || fail "cloudflared's command line"

	step "cloudflared's sandbox keeps it from the admin socket and hangar's state"
	systemd-analyze security hangar-cloudflared --no-pager | tee "$work/cf-security.log" | tail -n 1
	systemd-analyze security hangar-apiserver --no-pager | tail -n 1
	local pid uid score
	score=$(sed -n 's/.*Overall exposure level for hangar-cloudflared.service: \([0-9.]*\).*/\1/p' "$work/cf-security.log")
	awk -v s="$score" 'BEGIN { exit !(s != "" && s <= 2.0) }' || fail "cloudflared's exposure is $score, want at most 2.0"
	pid=$(main_pid hangar-cloudflared)
	uid=$(stat -c %u "/proc/$pid")
	{ [ "$uid" != 0 ] && ! getent passwd "$uid" | grep -q '^hangar:'; } || fail "cloudflared runs as uid $uid"
	# In its own mount namespace, hangar's directories are empty and closed.
	for d in /run/hangar /var/lib/hangar /etc/hangar; do
		[ -z "$(nsenter -t "$pid" -m ls -A "$d")" ] || fail "cloudflared sees $d: $(nsenter -t "$pid" -m ls -A "$d")"
		[ "$(nsenter -t "$pid" -m stat -c %a "$d")" = 0 ] || fail "$d is open in cloudflared's namespace"
	done
	sandbox_probe | tee "$work/probe.log"
	grep -q '^uid 0$' "$work/probe.log" && fail "the probe ran as root"
	grep -qE '^(connected|listed) ' "$work/probe.log" && fail "the sandbox let the probe in: $(grep -E '^(connected|listed) ' "$work/probe.log")"
	[ "$(grep -c '^refused .* Address family not supported by protocol$' "$work/probe.log")" = 2 ] || fail "unix sockets were not refused outright"
	[ "$(grep -c '^denied .* Permission denied$' "$work/probe.log")" = 4 ] || fail "hangar's directories were not all denied"
	# With unix sockets allowed, the paths alone still keep it out.
	sandbox_probe -p RestrictAddressFamilies=AF_UNIX | tee "$work/probe.log"
	grep -qE '^(connected|listed) ' "$work/probe.log" && fail "the paths let the probe in: $(grep -E '^(connected|listed) ' "$work/probe.log")"
	[ "$(grep -c '^refused /run/hangar/.* Permission denied$' "$work/probe.log")" = 2 ] || fail "the sockets were not denied by path"

	step "a new tunnel token restarts cloudflared, and only it"
	local cf_pid
	cf_pid=$(main_pid hangar-cloudflared)
	api_pid=$(main_pid hangar-apiserver)
	put_tunnel_token 2
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log"
	grep -q '^apply: restart hangar-cloudflared$' "$work/cut.log" || fail "a new token did not restart cloudflared: $(cat "$work/cut.log")"
	[ "$(main_pid hangar-cloudflared)" != "$cf_pid" ] || fail "cloudflared runs the old token"
	[ "$(main_pid hangar-apiserver)" = "$api_pid" ] || fail "the apiserver was restarted for the token"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged after the new token"

	step "rolling back: the tunnel stops without cloudflared.env, then the apiserver goes back to dev auth"
	rm /etc/hangar/cloudflared.env
	"$host_dir/bootstrap.sh" --env "$env_file" --only cloudflared >/dev/null 2>"$work/cut.log"
	grep -q '^apply: stop and disable hangar-cloudflared: /etc/hangar/cloudflared.env is gone$' "$work/cut.log" ||
		fail "the tunnel was not stopped: $(cat "$work/cut.log")"
	systemctl is-active --quiet hangar-cloudflared && fail "cloudflared still runs"
	systemctl is-enabled --quiet hangar-cloudflared && fail "cloudflared is still enabled"
	auth_env dev
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log"
	grep -q '^apply: restart hangar-apiserver$' "$work/cut.log" || fail "the apiserver was not restarted: $(cat "$work/cut.log")"
	[ "$(list_nodes_status)" = 200 ] || fail "dev auth is not back"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged after the rollback"
	"$host_dir/verify.sh" --env "$env_file" >"$work/cut-verify.log" || fail "verify after the rollback: $(grep FAIL "$work/cut-verify.log")"
	grep -q '^SKIP cloudflared' "$work/cut-verify.log" || fail "verify does not skip cloudflared after the rollback"

	step "dev auth with the tunnel up stops the tunnel before the apiserver changes"
	auth_env cloudflare
	put_tunnel_token 1
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>&1
	systemctl is-active --quiet hangar-cloudflared || fail "cloudflared did not come back"
	auth_env dev
	if "$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log"; then
		fail "a run went through with cloudflared.env in dev auth"
	fi
	local stop restart
	stop=$(grep -n '^apply: stop and disable hangar-cloudflared: the auth mode is dev$' "$work/cut.log" | cut -d: -f1)
	restart=$(grep -n '^apply: restart hangar-apiserver$' "$work/cut.log" | cut -d: -f1)
	[ -n "$stop" ] && [ -n "$restart" ] && [ "$stop" -lt "$restart" ] ||
		fail "cloudflared was not stopped before the apiserver went to dev: $(cat "$work/cut.log")"
	grep -q 'never forwards to an apiserver in dev auth' "$work/cut.log" || fail "the run does not say why it refused"
	systemctl is-active --quiet hangar-cloudflared && fail "cloudflared runs in front of dev auth"
	if "$host_dir/verify.sh" --env "$env_file" >/dev/null; then fail "verify passed with cloudflared.env in dev auth"; fi
	rm /etc/hangar/cloudflared.env
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged once cloudflared.env is gone"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify once cloudflared.env is gone"
}

test_refuses_tampered_release() {
	step "a release whose files do not match its SHA256SUMS is refused, and nothing installed"
	local bad=$work/release-tampered before node_pid
	rm -rf "$bad"
	cp -r "$release" "$bad"
	printf x >>"$bad/hangar-node"
	before=$(sha256sum /usr/local/bin/hangar-* /usr/local/lib/hangar/*)
	node_pid=$(main_pid hangar-node)
	if "$host_dir/bootstrap.sh" --env "$env_file" --release "$bad" >/dev/null 2>"$work/tampered.log"; then
		fail "bootstrap installed a tampered release"
	fi
	grep -q 'checksum mismatch' "$work/tampered.log" || fail "unexpected refusal: $(cat "$work/tampered.log")"
	[ "$(sha256sum /usr/local/bin/hangar-* /usr/local/lib/hangar/*)" = "$before" ] || fail "a refused release changed what is installed"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "a refused release restarted the node"
	sed -i '/  hangar$/d' "$bad/SHA256SUMS"
	if "$host_dir/bootstrap.sh" --env "$env_file" --release "$bad" >/dev/null 2>"$work/tampered.log"; then
		fail "bootstrap installed a release whose SHA256SUMS lacks a file"
	fi
	grep -q 'does not list hangar;' "$work/tampered.log" || fail "unexpected refusal: $(cat "$work/tampered.log")"
	rm -rf "$bad"
}

test_refuses_half_applied_release() {
	step "without --release, files that do not match the installed release are refused, and nothing restarted"
	local api_pid node_pid
	api_pid=$(main_pid hangar-apiserver)
	node_pid=$(main_pid hangar-node)
	cp /usr/local/bin/hangar-node "$work/hangar-node.mixed"
	printf x >>"$work/hangar-node.mixed"
	install -m 755 "$work/hangar-node.mixed" /usr/local/bin/hangar-node.tb-new
	mv -f /usr/local/bin/hangar-node.tb-new /usr/local/bin/hangar-node
	if "$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/mixed.log"; then
		fail "bootstrap ran on a half-applied release without --release"
	fi
	grep -q 'rerun with --release DIR' "$work/mixed.log" || fail "unexpected refusal: $(cat "$work/mixed.log")"
	grep -q '^apply: ' "$work/mixed.log" && fail "the refused run changed something: $(grep '^apply: ' "$work/mixed.log")"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "the refused run restarted the node"
	[ "$(main_pid hangar-apiserver)" = "$api_pid" ] || fail "the refused run restarted the apiserver"
	"$host_dir/bootstrap.sh" --env "$env_file" --release "$release" >/dev/null 2>"$work/mixed.log"
	grep -q '^apply: install hangar-node as' "$work/mixed.log" || fail "--release did not reinstall hangar-node: $(cat "$work/mixed.log")"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify after reinstalling the release"
	rm -f "$work/hangar-node.mixed"
}

test_refuses_without_local_env() {
	step "the services step refuses without the bootstrap admin in /etc/hangar/local.env"
	mv /etc/hangar/local.env "$work/local.env"
	if "$host_dir/bootstrap.sh" --env "$env_file" --only services 2>"$work/localenv.log"; then
		fail "the services step ran without /etc/hangar/local.env"
	fi
	grep -q 'HANGAR_BOOTSTRAP_ADMIN=<email>' "$work/localenv.log" || fail "unexpected refusal: $(cat "$work/localenv.log")"
	# A whole run refuses before its first step.
	if "$host_dir/bootstrap.sh" --env "$env_file" 2>"$work/localenv.log"; then
		fail "bootstrap ran without /etc/hangar/local.env"
	fi
	grep -q 'HANGAR_BOOTSTRAP_ADMIN=<email>' "$work/localenv.log" || fail "unexpected refusal: $(cat "$work/localenv.log")"
	grep -q '^== ' "$work/localenv.log" && fail "steps ran before the refusal: $(grep '^== ' "$work/localenv.log")"
	mv "$work/local.env" /etc/hangar/local.env
	# Readable by anyone is wrong too.
	chmod 644 /etc/hangar/local.env
	if "$host_dir/verify.sh" --env "$env_file" >/dev/null; then fail "verify passed with local.env mode 644"; fi
	if "$host_dir/bootstrap.sh" --env "$env_file" --only services 2>/dev/null; then
		fail "the services step ran with local.env mode 644"
	fi
	chmod 600 /etc/hangar/local.env
}

test_upgrade_restarts_services() {
	step "a new release replaces the binaries and restarts both services"
	local next=$work/release-next api_pid node_pid before out log=$work/upgrade.log
	make_release "$release" "$next" "$(cat "$release/VERSION")-tb2"

	api_pid=$(main_pid hangar-apiserver)
	node_pid=$(main_pid hangar-node)
	before=$(state_digest)
	"$host_dir/bootstrap.sh" --env "$env_file" --release "$next" --dry-run 2>"$work/upgrade-dry.log" >/dev/null
	[ "$(main_pid hangar-apiserver)" = "$api_pid" ] || fail "the upgrade's dry run restarted the apiserver"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "the upgrade's dry run restarted the node"
	[ "$(state_digest)" = "$before" ] || fail "the upgrade's dry run changed the host"
	grep -q '^would: restart hangar-node$' "$work/upgrade-dry.log" || fail "the dry run does not say it restarts the node"
	grep -q '^would: restart hangar-apiserver$' "$work/upgrade-dry.log" || fail "the dry run does not say it restarts the apiserver"

	"$host_dir/bootstrap.sh" --env "$env_file" --release "$next" >"$work/upgrade.out" 2>"$log"
	grep -qx "changes: $(grep -c '^apply: ' "$log")" "$work/upgrade.out" || fail "upgrade summary does not match its changes"
	[ "$(main_pid hangar-apiserver)" != "$api_pid" ] || fail "the apiserver was not restarted on its new binary"
	[ "$(main_pid hangar-node)" != "$node_pid" ] || fail "the node was not restarted on its new binary"
	grep -q '^apply: install vmlinux' "$log" && fail "the upgrade reinstalled an unchanged kernel"
	out=$("$host_dir/bootstrap.sh" --env "$env_file" --release "$next")
	[ "$out" = "changes: 0" ] || fail "the run after the upgrade reported $out"
	"$host_dir/verify.sh" --env "$env_file" >"$work/upgrade-verify.log" || fail "verify after the upgrade: $(grep FAIL "$work/upgrade-verify.log")"
	grep -q "^PASS hangar release $(cat "$next/VERSION") installed" "$work/upgrade-verify.log" || fail "verify does not report the new version"

	step "going back to the first release restarts them again"
	node_pid=$(main_pid hangar-node)
	"$host_dir/bootstrap.sh" --env "$env_file" --release "$release" >/dev/null 2>&1
	[ "$(main_pid hangar-node)" != "$node_pid" ] || fail "the node was not restarted going back"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify after going back"
	rm -rf "$next"
}

test_unit_change_restarts_its_service() {
	step "a changed unit restarts its service only"
	local api_pid node_pid
	api_pid=$(main_pid hangar-apiserver)
	node_pid=$(main_pid hangar-node)
	sed 's|^HANGAR_PUBLIC_IPV4=.*|HANGAR_PUBLIC_IPV4=192.0.2.11|' "$env_file" >"$work/ip.env"
	"$host_dir/bootstrap.sh" --env "$work/ip.env" >/dev/null 2>"$work/unit.log"
	grep -q '^apply: restart hangar-node$' "$work/unit.log" || fail "a changed node unit did not restart the node: $(cat "$work/unit.log")"
	[ "$(main_pid hangar-node)" != "$node_pid" ] || fail "the node runs its old unit"
	[ "$(main_pid hangar-apiserver)" = "$api_pid" ] || fail "the apiserver was restarted for the node's unit"
	systemctl show -p ExecStart --value hangar-node | grep -q -- '--public-ipv4 192.0.2.11' || fail "the node's new unit is not loaded"

	step "a run that died between writing a unit and restarting finishes the job"
	node_pid=$(main_pid hangar-node)
	sed -i 's/--public-ipv4 192.0.2.11/--public-ipv4 192.0.2.10/' /etc/systemd/system/hangar-node.service
	mkdir -p /var/lib/hangar-bootstrap
	touch /var/lib/hangar-bootstrap/restart-hangar-node
	if "$host_dir/verify.sh" --env "$env_file" >/dev/null; then fail "verify passed with a restart pending"; fi
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/unit.log"
	grep -q '^apply: restart hangar-node$' "$work/unit.log" || fail "a pending restart was not done: $(cat "$work/unit.log")"
	[ "$(main_pid hangar-node)" != "$node_pid" ] || fail "the node was not restarted"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged after the pending restart"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify after the pending restart"
}

test_refuses_dropins() {
	step "a drop-in for a hangar unit fails verify and stops the services step"
	local node_pid
	node_pid=$(main_pid hangar-node)
	mkdir -p /etc/systemd/system/hangar-node.service.d
	printf '[Service]\nEnvironment=TB_DROPIN=1\n' >/etc/systemd/system/hangar-node.service.d/tb.conf
	systemctl daemon-reload
	if "$host_dir/verify.sh" --env "$env_file" >"$work/dropin-verify.log"; then
		fail "verify passed with a drop-in for hangar-node"
	fi
	grep -q '^FAIL no drop-ins for' "$work/dropin-verify.log" || fail "verify does not name the drop-in: $(grep FAIL "$work/dropin-verify.log")"
	if "$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/dropin.log"; then
		fail "bootstrap reported converged with a drop-in for hangar-node"
	fi
	grep -q 'drop-ins change the hangar units: .*hangar-node.service.d' "$work/dropin.log" || fail "unexpected refusal: $(cat "$work/dropin.log")"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "the refused run restarted the node"
	rm -r /etc/systemd/system/hangar-node.service.d
	systemctl daemon-reload
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged once the drop-in is gone"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify once the drop-in is gone"
}

test_node_needs_its_datasets() {
	step "the node does not start while hangar/images is not mounted"
	systemctl stop hangar-node
	zfs unmount hangar/images
	# Restart=always keeps retrying; whether the start job waits for that
	# depends on systemd's version, so it gets a bound.
	if timeout 20 systemctl start hangar-node 2>/dev/null; then
		fail "the node started without hangar/images mounted"
	fi
	sleep 3
	systemctl is-active --quiet hangar-node && fail "the node is active without hangar/images mounted"
	if [ -d /var/lib/hangar/images ] && [ -n "$(ls -A /var/lib/hangar/images)" ]; then
		fail "the node wrote to the root filesystem under /var/lib/hangar/images: $(ls -A /var/lib/hangar/images)"
	fi
	zfs mount hangar/images
	systemctl start hangar-node
	systemctl is-active --quiet hangar-node || fail "the node is not active once hangar/images is mounted"
	"$host_dir/verify.sh" --env "$env_file" >"$work/mounts-verify.log" || fail "verify after remounting: $(grep FAIL "$work/mounts-verify.log")"
}

test_verify_catches_service_drift() {
	step "verify reports drift in datasets, release, apt timers and services; bootstrap repairs it"
	zfs set exec=on hangar/images
	chmod 755 /var/lib/hangar/vms
	printf x >>/usr/local/lib/hangar/vmlinux
	rm /etc/systemd/system/apt-daily-upgrade.timer.d/hangar.conf
	systemctl daemon-reload
	systemctl restart apt-daily-upgrade.timer
	systemctl stop hangar-apiserver
	if "$host_dir/verify.sh" --env "$env_file" >"$work/drift2.log"; then
		fail "verify passed on a drifted host"
	fi
	# images, vms, release checksums, upgrade files, timers, units running,
	# socket, loopback listener, node Ready.
	[ "$(grep -c '^FAIL' "$work/drift2.log")" = 9 ] || fail "expected 9 FAIL lines: $(cat "$work/drift2.log")"
	"$host_dir/bootstrap.sh" --env "$env_file" --release "$release" >/dev/null
	"$host_dir/verify.sh" --env "$env_file" >"$work/drift2.log" || fail "bootstrap did not repair the drift: $(grep FAIL "$work/drift2.log")"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged after the repair"
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
test_admin_socket
test_node_reconnects
test_cloudflared_needs_cloudflare_auth
test_cloudflare_cutover
test_refuses_tampered_release
test_refuses_half_applied_release
test_refuses_without_local_env
test_upgrade_restarts_services
test_unit_change_restarts_its_service
test_refuses_dropins
test_node_needs_its_datasets
test_refuses_while_rollback_pending
test_verify_catches_drift
test_verify_catches_service_drift
test_bad_ipv6_rolls_back
echo
echo "host-bootstrap testbed: all passed"
