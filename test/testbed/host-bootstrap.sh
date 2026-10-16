#!/usr/bin/env bash
# Exercises deploy/host on a disposable Ubuntu VM made to look like a freshly
# installed OVH host: two plain swap partitions (loop devices), an installer-made
# pool named hangar (file-backed), and an uplink whose IPv6 is the prefix's ::
# address behind an on-link gateway outside the /64 (a veth into a netns).
# HANGAR_TESTBED_RELEASE names a release directory (make release) for the
# release and services steps; HANGAR_TESTBED_FRONT_DOOR the directory
# test/testbed/front-door-kit.sh made, for the front door's test CA.
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
front_door_kit=${HANGAR_TESTBED_FRONT_DOOR:-}

swap0=
swap1=

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "### $*" >&2; }

[ "$(id -u)" = 0 ] || fail "must run as root"
[ -f "$release/SHA256SUMS" ] || fail "HANGAR_TESTBED_RELEASE must name a release directory (make release)"
release=$(cd "$release" && pwd)
[ -x "$front_door_kit/hangar-edge" ] && [ -x "$front_door_kit/pebble" ] ||
	fail "HANGAR_TESTBED_FRONT_DOOR must name the directory test/testbed/front-door-kit.sh made"
front_door_kit=$(cd "$front_door_kit" && pwd)

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
	# A VM image's package lists are as old as the image, and the versions
	# they name leave the mirror: refresh them first.
	apt-get update -q >/dev/null
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
HANGAR_DEFAULT_MAX_OWNED_TENANTS=3
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

	step "no step installs the Cloudflare tunnel, and verify finds nothing of it"
	grep -q '^PASS Cloudflare tunnel retired: nothing of it left$' "$work/first-verify.log" ||
		fail "verify does not check the tunnel is gone"
	grep -q '^FAIL' "$work/first-verify.log" && fail "verify failed: $(grep '^FAIL' "$work/first-verify.log")"
	grep -q '^SKIP SSH CA (no /var/lib/hangar/state/ssh-ca: hangar ssh cannot sign)$' "$work/first-verify.log" ||
		fail "verify does not skip the SSH CA it has not got"
	grep -qE '^(apply|would): .*cloudflared' "$log" && fail "the run did something about cloudflared: $(grep -E '^(apply|would): .*cloudflared' "$log")"
	[ ! -e /usr/local/bin/cloudflared ] && [ ! -e /etc/systemd/system/hangar-cloudflared.service ] ||
		fail "cloudflared was installed"
	systemctl show -p ExecStart --value hangar-apiserver | grep -q -- '--listen 127.0.0.1:8080 --auth-mode dev --socket' ||
		fail "an unset HANGAR_AUTH_MODE is not dev auth"

	# Where machines reach the front door: the agent's rules for it are
	# test/testbed/network's to test. The address is the env file's in its
	# canonical form, as load_env makes it.
	systemctl show -p ExecStart --value hangar-node | grep -q -- '--public-ipv4 192.0.2.10 --public-ipv6 2001:db8:0:1::1 --public-ipv6-prefix 2001:db8:0:1::/64' ||
		fail "the agent is not given the host's addresses: $(systemctl show -p ExecStart --value hangar-node)"

	step "the SSH CA, installed as the runbook says, is the apiserver's, and verify checks it"
	local since
	ssh-keygen -q -t ed25519 -N '' -C hangar-ssh-ca -f "$work/ssh-ca"
	install -m 0644 "$work/ssh-ca" /var/lib/hangar/state/ssh-ca
	"$host_dir/bootstrap.sh" --env "$env_file" 2>"$work/ca.log" >/dev/null
	grep -q 'warning: /var/lib/hangar/state/ssh-ca was root:root mode 644' "$work/ca.log" ||
		fail "bootstrap tightened the SSH CA's key without saying it had been open: $(cat "$work/ca.log")"
	[ "$(stat -c '%U %a' /var/lib/hangar/state/ssh-ca)" = "hangar 600" ] || fail "bootstrap left the SSH CA's key $(stat -c '%U %a' /var/lib/hangar/state/ssh-ca)"
	since=$(date '+%Y-%m-%d %H:%M:%S')
	systemctl restart hangar-apiserver
	sleep 2
	journalctl -u hangar-apiserver --since "$since" | grep -q 'loaded the SSH CA' || fail "the apiserver did not load the SSH CA"
	journalctl -u hangar-apiserver --since "$since" | grep -q 'PRIVATE' && fail "the apiserver logged the SSH CA's key"
	# Read from a file: grep -q ending the pipe early would kill verify.sh
	# with SIGPIPE, and pipefail fail the check.
	"$host_dir/verify.sh" --env "$env_file" >"$work/ca-verify.log" || true
	grep -q '^PASS SSH CA key /var/lib/hangar/state/ssh-ca, hangar.s, mode 600 (hangar ssh)$' "$work/ca-verify.log" ||
		fail "verify does not check the SSH CA"

	step "the first run made the secrets key-encryption key, the apiserver's alone, and runs after it keep it"
	local kek=/var/lib/hangar/state/secrets-kek before
	[ "$(stat -c '%U %G %a' "$kek")" = "hangar hangar 600" ] || fail "the KEK is $(stat -c '%U:%G %a' "$kek")"
	grep -qE '^kek-[0-9]{14} [A-Za-z0-9+/]{43}=$' "$kek" || fail "the KEK file is not an id and 32 bytes in base64"
	grep -q "note: copy $kek to the vault now" "$log" || fail "the first run did not say to keep the KEK in the vault"
	grep -qF "$(cut -d' ' -f2 "$kek")" "$log" && fail "the run logged the KEK"
	before=$(sha256sum "$kek")
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>&1
	[ "$(sha256sum "$kek")" = "$before" ] || fail "a later run replaced the KEK"
	journalctl -u hangar-apiserver --since "$since" | grep -q 'loaded the key-encryption key' || fail "the apiserver did not load the KEK"
	journalctl -u hangar-apiserver --since "$since" | grep -qF "$(cut -d' ' -f2 "$kek")" && fail "the apiserver logged the KEK"
	"$host_dir/verify.sh" --env "$env_file" >"$work/kek-verify.log" || true
	grep -q "^PASS secrets key-encryption key $kek, hangar.s, mode 600$" "$work/kek-verify.log" ||
		fail "verify does not check the KEK"
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
apiVersion: iam.hangarlab.cloud/v1alpha1
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

	step "the bootstrap admin signs up, as after a host's first start"
	local signup
	signup=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Hangar-Dev-User: admin@testbed.invalid' \
		-H 'Content-Type: application/json' -d '{"username": "tb-admin"}' \
		http://127.0.0.1:8080/hangar.iam.v1alpha1.AccountService/SignUp)
	# 409: signed up on an earlier run against the same store.
	case $signup in 200 | 409) ;; *) fail "the bootstrap admin could not sign up: HTTP $signup" ;; esac

	step "dev auth refuses what came through Cloudflare"
	[ "$(list_nodes_status)" = 200 ] || fail "dev auth on loopback refused the bootstrap admin"
	local h
	for h in 'Cf-Warp-Tag-Id: 6f1c1b8e-2b1a-4c3d-9e8f-0a1b2c3d4e5f' 'Cdn-Loop: cloudflare' 'Cf-Connecting-Ip: 192.0.2.7' 'Cf-Ray: 0123456789abcdef-SIN'; do
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

TB_CLIENT_ID=testbed-client.apps.googleusercontent.com
TB_PUBLIC_URL=https://console.testbed.example
TB_OIDC_SECRET=/var/lib/hangar/state/oidc-client-secret

# auth_env MODE sets the auth settings in local.env: oidc with the
# testbed's client and public URL, and the client secret the operator
# installs, or none, which is dev. The apiserver reaches the provider only
# when someone signs in, so it runs without one here.
auth_env() {
	sed -i '/^HANGAR_AUTH_MODE=/d; /^HANGAR_OIDC_/d; /^HANGAR_PUBLIC_URL=/d' /etc/hangar/local.env
	if [ "$1" = oidc ]; then
		printf 'HANGAR_AUTH_MODE=oidc\nHANGAR_OIDC_CLIENT_ID=%s\nHANGAR_PUBLIC_URL=%s\n' "$TB_CLIENT_ID" "$TB_PUBLIC_URL" >>/etc/hangar/local.env
		install -o hangar -g hangar -m 0600 /dev/null "$TB_OIDC_SECRET"
		echo testbed-client-secret >"$TB_OIDC_SECRET"
	fi
}

# old_tunnel leaves what an earlier release installed for the Cloudflare
# tunnel, as a host has it at the cutover: a unit, running, its guard and
# its binary, and the token's file. The binary only sleeps.
old_tunnel() {
	install -m 0755 /bin/sleep /usr/local/bin/cloudflared
	install -D -m 0755 /dev/null /usr/local/libexec/hangar-cloudflared-guard
	printf '#!/bin/sh\nexit 0\n' >/usr/local/libexec/hangar-cloudflared-guard
	cat >/etc/systemd/system/hangar-cloudflared.service <<'EOF'
[Unit]
Description=Cloudflare Tunnel to the Hangar API (an earlier release's)
[Service]
ExecStartPre=+/usr/local/libexec/hangar-cloudflared-guard
ExecStart=/usr/local/bin/cloudflared infinity
Restart=always
[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	systemctl enable --now hangar-cloudflared 2>/dev/null
	install -m 0600 /dev/null /etc/hangar/cloudflared.env
	echo 'TUNNEL_TOKEN=testbed-not-a-token' >/etc/hangar/cloudflared.env
}

test_oidc_cutover() {
	step "the retired cloudflare mode is refused before any step"
	sed -i '/^HANGAR_AUTH_MODE=/d' /etc/hangar/local.env
	echo HANGAR_AUTH_MODE=cloudflare >>/etc/hangar/local.env
	if "$host_dir/bootstrap.sh" --env "$env_file" 2>"$work/cut.log"; then fail "bootstrap ran in the retired cloudflare mode"; fi
	grep -q 'must be dev or oidc, not cloudflare' "$work/cut.log" || fail "unexpected refusal: $(cat "$work/cut.log")"
	auth_env dev

	step "an oidc mode without its client ID and public URL is refused before any step"
	echo HANGAR_AUTH_MODE=oidc >>/etc/hangar/local.env
	if "$host_dir/bootstrap.sh" --env "$env_file" 2>"$work/cut.log"; then fail "bootstrap ran with half an oidc mode"; fi
	{ grep -q 'HANGAR_OIDC_CLIENT_ID=' "$work/cut.log" &&
		grep -q 'HANGAR_PUBLIC_URL=https://' "$work/cut.log"; } || fail "unexpected refusal: $(cat "$work/cut.log")"
	grep -q '^== ' "$work/cut.log" && fail "steps ran before the refusal"
	if "$host_dir/bootstrap.sh" --env "$env_file" --only services 2>/dev/null; then fail "the services step ran with half an oidc mode"; fi
	auth_env dev

	step "oidc auth in local.env restarts the apiserver in it, still on loopback"
	local api_pid node_pid
	api_pid=$(main_pid hangar-apiserver)
	node_pid=$(main_pid hangar-node)
	auth_env oidc
	"$host_dir/bootstrap.sh" --env "$env_file" --only services >/dev/null 2>"$work/cut.log"
	grep -q '^apply: restart hangar-apiserver$' "$work/cut.log" || fail "the apiserver was not restarted: $(cat "$work/cut.log")"
	[ "$(main_pid hangar-apiserver)" != "$api_pid" ] || fail "the apiserver runs its old auth"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "the node was restarted for the apiserver's auth"
	systemctl show -p ExecStart --value hangar-apiserver |
		grep -q -- "--listen 127.0.0.1:8080 --auth-mode oidc --oidc-client-id $TB_CLIENT_ID --oidc-client-secret-file $TB_OIDC_SECRET --public-url $TB_PUBLIC_URL --socket" ||
		fail "the apiserver's unit does not have oidc auth"
	[ "$(list_nodes_status)" = 401 ] || fail "oidc auth took a dev header"
	admin_cli get node testbed | grep -qE '^testbed +True ' || fail "the admin socket does not work in oidc auth"
	"$host_dir/verify.sh" --env "$env_file" >"$work/cut-verify.log" || fail "verify in oidc auth: $(grep FAIL "$work/cut-verify.log")"
	grep -q "^PASS OAuth client secret $TB_OIDC_SECRET, hangar's, mode 600$" "$work/cut-verify.log" || fail "verify does not check the client secret"
	grep -q "^PASS apiserver runs oidc auth as /etc/hangar/local.env asks: --auth-mode oidc --oidc-client-id $TB_CLIENT_ID --oidc-client-secret-file $TB_OIDC_SECRET --public-url $TB_PUBLIC_URL$" "$work/cut-verify.log" ||
		fail "verify does not check the auth flags"

	step "a tunnel an earlier release left runs on while its cloudflared.env is there"
	old_tunnel
	systemctl is-active --quiet hangar-cloudflared || fail "the earlier release's tunnel does not run"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file" 2>"$work/cut.log")" = "changes: 0" ] || fail "a run changed something with the tunnel there: $(cat "$work/cut.log")"
	grep -q 'warning: the Cloudflare tunnel (hangar-cloudflared) is no longer installed by any step; it is left as it is until you delete /etc/hangar/cloudflared.env' "$work/cut.log" ||
		fail "the run does not say the tunnel is left: $(cat "$work/cut.log")"
	systemctl is-active --quiet hangar-cloudflared || fail "the run stopped the tunnel before its env file went"
	"$host_dir/verify.sh" --env "$env_file" >"$work/cut-verify.log" || fail "verify with the tunnel left: $(grep FAIL "$work/cut-verify.log")"
	grep -q '^SKIP Cloudflare tunnel retired (/etc/hangar/cloudflared.env is still there' "$work/cut-verify.log" ||
		fail "verify does not skip the tunnel while its env file is there"

	step "dev auth asked for with the tunnel up stops the tunnel before the apiserver restarts"
	auth_env dev
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log" || fail "the switch to dev auth: $(cat "$work/cut.log")"
	local stop restart
	stop=$(grep -n '^apply: stop and disable hangar-cloudflared: the auth mode is dev$' "$work/cut.log" | cut -d: -f1)
	restart=$(grep -n '^apply: restart hangar-apiserver$' "$work/cut.log" | cut -d: -f1)
	[ -n "$stop" ] && [ -n "$restart" ] && [ "$stop" -lt "$restart" ] ||
		fail "the tunnel was not stopped before the apiserver went to dev: $(cat "$work/cut.log")"
	systemctl is-active --quiet hangar-cloudflared && fail "the tunnel runs in front of dev auth"
	auth_env oidc
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log" || fail "back to oidc auth: $(cat "$work/cut.log")"
	systemctl start hangar-cloudflared

	step "with cloudflared.env deleted, the run retires the tunnel: nothing of it is left"
	rm /etc/hangar/cloudflared.env
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log" || fail "retiring the tunnel: $(cat "$work/cut.log")"
	grep -q '^apply: retire the Cloudflare tunnel: stop and disable hangar-cloudflared, remove /etc/systemd/system/hangar-cloudflared.service /usr/local/libexec/hangar-cloudflared-guard /usr/local/bin/cloudflared$' "$work/cut.log" ||
		fail "the run did not retire the tunnel: $(cat "$work/cut.log")"
	systemctl is-active --quiet hangar-cloudflared && fail "the tunnel still runs"
	local f
	for f in /etc/systemd/system/hangar-cloudflared.service /usr/local/libexec/hangar-cloudflared-guard /usr/local/bin/cloudflared; do
		[ ! -e "$f" ] || fail "$f is left"
	done
	[ "$(systemctl show -p LoadState --value hangar-cloudflared)" = not-found ] || fail "systemd still knows the tunnel's unit"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged after the tunnel went"
	"$host_dir/verify.sh" --env "$env_file" >"$work/cut-verify.log" || fail "verify after the tunnel went: $(grep FAIL "$work/cut-verify.log")"
	grep -q '^PASS Cloudflare tunnel retired: nothing of it left$' "$work/cut-verify.log" || fail "verify does not check the tunnel is gone"

	step "a tunnel put back by hand, with no env file, fails verify, and the run removes it"
	old_tunnel
	rm /etc/hangar/cloudflared.env
	if "$host_dir/verify.sh" --env "$env_file" >"$work/cut-verify.log"; then fail "verify passed with the tunnel back"; fi
	grep -q '^FAIL Cloudflare tunnel retired: nothing of it left$' "$work/cut-verify.log" || fail "verify does not say why: $(grep FAIL "$work/cut-verify.log")"
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log"
	systemctl is-active --quiet hangar-cloudflared && fail "the tunnel still runs"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify once the tunnel is gone again"

	step "rolling back to dev auth"
	auth_env dev
	"$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/cut.log"
	grep -q '^apply: restart hangar-apiserver$' "$work/cut.log" || fail "the apiserver was not restarted: $(cat "$work/cut.log")"
	[ "$(list_nodes_status)" = 200 ] || fail "dev auth is not back"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged after the rollback"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify after the rollback"
}

# The console's origin with the front door: sign-in's public URL.
TB_FRONT_DOOR_URL=https://console.cp.test

# front_door_env off|endpoints|on sets the edge's settings in local.env:
# none, the public endpoints' domain alone, which the edge step refuses,
# or the front door, with certificates from pebble over DNS-01 the edge
# answers itself. Settings the edge took before it signed private
# endpoints' callers in itself go too.
front_door_env() {
	sed -i '/^HANGAR_EDGE_/d; /^HANGAR_CP_DOMAIN=/d; /^HANGAR_ACME_/d; /^HANGAR_CF_TEAM_DOMAIN=/d; /^HANGAR_CF_ACCOUNT_ID=/d; /^HANGAR_CF_BYPASS_POLICY_ID=/d' /etc/hangar/local.env
	[ "$1" != off ] || return 0
	cat >>/etc/hangar/local.env <<EOF
HANGAR_EDGE_DOMAIN=user.test
EOF
	[ "$1" = on ] || return 0
	cat >>/etc/hangar/local.env <<EOF
HANGAR_CP_DOMAIN=cp.test
HANGAR_ACME_EMAIL=edge@example.com
HANGAR_ACME_CA=https://127.0.0.1:14000/dir
HANGAR_ACME_CA_ROOT=/etc/hangar/pebble-ca.pem
HANGAR_ACME_DNS=testbed:127.0.0.1:8053
EOF
}

# from_outside CMD... runs CMD in the upstream's namespace: as the internet
# reaches the host, through its uplink.
from_outside() { ip netns exec tb-upstream "$@"; }

# door_curl [--outside] ADDR NAME PATH asks the front door at ADDR (an
# address as curl takes it) for https://NAME/PATH, trusting pebble's root,
# and prints the body and the status; from outside, through the uplink.
door_curl() {
	local in=()
	if [ "$1" = --outside ]; then
		in=(ip netns exec tb-upstream)
		shift
	fi
	"${in[@]}" curl -s -m 5 --cacert "$work/front-door/root.pem" --resolve "$2:443:$1" -w ' %{http_code}' "https://$2$3"
}

test_front_door() {
	step "the front door: hangar-edge, never root, takes :443 and :80 from systemd for IPv4 and IPv6, with certificates over DNS-01"
	local tls=$work/front-door edge_release=$work/release-edge log=$work/door.log out v4 v6=2001:db8:0:1::1 pid since
	v4=$(ip -4 -o addr show dev tb-uplink scope global | awk '{print $4}' | cut -d/ -f1 | head -n 1)
	# The VM's own default route, learned on its real link, beats the
	# uplink's for IPv6; on a host the uplink is the only one. Replies to
	# the upstream leave by the uplink for this test.
	ip -6 route replace 2001:db8:0:ff::/64 dev tb-uplink
	# The release, but with the edge that answers DNS-01 itself.
	rm -rf "$edge_release"
	cp -r "$release" "$edge_release"
	install -m 0755 "$front_door_kit/hangar-edge" "$edge_release/hangar-edge"
	(cd "$edge_release" && rm SHA256SUMS && sha256sum -- * >SHA256SUMS)
	mkdir -p "$tls"
	cat >"$tls/pebble.json" <<EOF
{"pebble": {"listenAddress": "127.0.0.1:14000", "managementListenAddress": "127.0.0.1:15000",
 "certificate": "$front_door_kit/pebble-cert.pem", "privateKey": "$front_door_kit/pebble-key.pem",
 "httpPort": 5002, "tlsPort": 5001, "ocspResponderURL": "", "externalAccountBindingRequired": false,
 "retryAfter": {"authz": 1, "order": 1}, "keyAlgorithm": "ecdsa",
 "profiles": {"default": {"description": "testbed", "validityPeriod": 7776000}}}}
EOF
	systemd-run --quiet --unit=tb-pebble -E PEBBLE_VA_NOSLEEP=1 -E PEBBLE_WFE_NONCEREJECT=0 \
		"$front_door_kit/pebble" -config "$tls/pebble.json" -dnsserver 127.0.0.1:8053
	install -m 0644 "$front_door_kit/pebble-ca.pem" /etc/hangar/pebble-ca.pem
	auth_env oidc
	front_door_env on
	# Sign-in's provider returns to the public URL, which must be the
	# console's site the front door serves; and the ACME account needs an
	# email.
	sed -i '/^HANGAR_ACME_EMAIL=/d' /etc/hangar/local.env
	if "$host_dir/bootstrap.sh" --env "$env_file" --release "$edge_release" --only edge 2>"$log"; then
		fail "the front door ran with sign-in's public URL elsewhere and no ACME email"
	fi
	grep -q "HANGAR_PUBLIC_URL in /etc/hangar/local.env must be $TB_FRONT_DOOR_URL" "$log" || fail "unexpected refusal: $(cat "$log")"
	grep -q "needs HANGAR_ACME_EMAIL in /etc/hangar/local.env" "$log" || fail "no refusal for the missing ACME email: $(cat "$log")"
	sed -i "s|^HANGAR_PUBLIC_URL=.*|HANGAR_PUBLIC_URL=$TB_FRONT_DOOR_URL|" /etc/hangar/local.env
	echo 'HANGAR_ACME_EMAIL=edge@example.com' >>/etc/hangar/local.env

	"$host_dir/bootstrap.sh" --env "$env_file" --release "$edge_release" 2>&1 | tee "$log"
	grep -qx "changes: $(grep -c '^apply: ' "$log")" "$log" || fail "summary does not match its changes"
	grep -qx 'apply: start hangar-edge' "$log" || fail "the edge was not started"
	out=$("$host_dir/bootstrap.sh" --env "$env_file" --release "$edge_release")
	[ "$out" = "changes: 0" ] || fail "the run after the front door reported $out"
	pid=$(main_pid hangar-edge)
	[ "$(stat -c %U "/proc/$pid")" = hangar-edge ] || fail "the edge runs as $(stat -c %U "/proc/$pid")"
	[ "$(stat -c '%U %a' /var/lib/hangar-edge)" = "hangar-edge 700" ] || fail "the edge's state is $(stat -c '%U %a' /var/lib/hangar-edge)"
	ss -Hltn | awk '{print $4}' | grep -qx '\*:443' || fail ":443 is not bound for every address: $(ss -Hltn)"

	step "the certificates come, and each site answers on IPv4 and IPv6, from the host and from outside"
	# One certificate each, as they come: every name has its own.
	for _ in $(seq 1 60); do
		curl -sk https://127.0.0.1:15000/roots/0 >"$tls/root.pem" 2>/dev/null &&
			[ "$(door_curl 127.0.0.1 api.cp.test /healthz)" = "ok
 200" ] && door_curl 127.0.0.1 console.cp.test / >/dev/null &&
			door_curl 127.0.0.1 web-demo.user.test / >/dev/null && break
		sleep 1
	done
	[ "$(door_curl 127.0.0.1 api.cp.test /healthz)" = "ok
 200" ] || fail "no API through the front door: $(door_curl 127.0.0.1 api.cp.test /healthz; journalctl -u hangar-edge --no-pager -n 30)"

	step "verify checks the front door, its certificates and what it serves on :443 included"
	# verify.sh checks what :443 serves against the system's roots, which a
	# real CA's chain to; pebble's, here, joins them for the test.
	install -m 0644 "$tls/root.pem" /usr/local/share/ca-certificates/tb-pebble.crt
	update-ca-certificates >/dev/null 2>&1
	"$host_dir/verify.sh" --env "$env_file" >"$work/door-verify.log" || fail "verify with the front door: $(grep FAIL "$work/door-verify.log")"
	for out in 'certificate for api.cp.test in /var/lib/hangar-edge, more than 14 days left' \
		'certificate for console.cp.test in /var/lib/hangar-edge, more than 14 days left' \
		'certificate for \*.user.test in /var/lib/hangar-edge, more than 14 days left' 'edge runs as hangar-edge with no capability' \
		'edge listens on :443 and :80 for every address, and nothing else' \
		'edge on :443 serves api.cp.test a trusted certificate with more than 14 days left, and /healthz is ok' \
		'edge on :443 serves console.cp.test a trusted certificate with more than 14 days left' \
		'edge on :443 serves \*.user.test a trusted certificate with more than 14 days left' \
		'public socket /run/hangar/public.sock, root.s and hangar-edge.s group.s, 660' 'table inet hangar-host as loaded: input dropped but TCP 22, 80, 443'; do
		grep -q "^PASS $out" "$work/door-verify.log" || fail "verify does not check: $out"
	done
	grep -q '^SKIP Cloudflare DNS token (DNS-01 is answered by testbed:127.0.0.1:8053)$' "$work/door-verify.log" || fail "verify does not skip the DNS token"
	# What :443 serves, untrusted, fails verify.
	rm /usr/local/share/ca-certificates/tb-pebble.crt
	update-ca-certificates --fresh >/dev/null 2>&1
	if "$host_dir/verify.sh" --env "$env_file" >"$work/door-verify.log"; then fail "verify passed with :443 serving certificates no root trusts"; fi
	[ "$(grep -c '^FAIL edge on :443 serves ' "$work/door-verify.log")" = 3 ] || fail "verify does not fail each name :443 serves: $(grep FAIL "$work/door-verify.log")"
	install -m 0644 "$tls/root.pem" /usr/local/share/ca-certificates/tb-pebble.crt
	update-ca-certificates >/dev/null 2>&1
	# A certificate verify cannot find fails it.
	mv /var/lib/hangar-edge/certificates /var/lib/hangar-edge/certificates.tb
	if "$host_dir/verify.sh" --env "$env_file" >"$work/door-verify.log"; then fail "verify passed with no certificates in storage"; fi
	[ "$(grep -c '^FAIL certificate for ' "$work/door-verify.log")" = 3 ] || fail "verify does not fail each certificate: $(grep FAIL "$work/door-verify.log")"
	mv /var/lib/hangar-edge/certificates.tb /var/lib/hangar-edge/certificates
	[ "$(door_curl '[::1]' api.cp.test /healthz)" = "ok
 200" ] || fail "the API at [::1]: $(door_curl '[::1]' api.cp.test /healthz)"
	for addr in "$v4" "[$v6]"; do
		out=$(door_curl --outside "$addr" api.cp.test /healthz) || out="curl failed: $out"
		[ "$out" = "ok
 200" ] || fail "the API from outside at $addr: $out"
	done
	[ "$(door_curl "$v4" console.cp.test /projects/demo)" = "the console is not installed
 503" ] || fail "the console without its files: $(door_curl "$v4" console.cp.test /projects/demo)"
	[ "$(door_curl --outside "[$v6]" web-demo.user.test /)" = "no such endpoint
 404" ] || fail "an endpoint that is not there: $(door_curl --outside "[$v6]" web-demo.user.test /)"
	door_curl --outside "$v4" other.cp.test / >/dev/null && fail "a handshake for a name the front door has no certificate for"
	out=$(from_outside curl -s -m 5 -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "api.cp.test:80:$v4" 'http://api.cp.test/a?b')
	[ "$out" = '308 https://api.cp.test/a?b' ] || fail ":80 does not redirect: $out"
	# Sign-in reaches the apiserver through :443: on the API's host it sends
	# the browser to the console's, the public URL, as the apiserver does.
	out=$(from_outside curl -s -m 5 -o /dev/null -w '%{http_code} %{redirect_url}' --cacert "$work/front-door/root.pem" \
		--resolve "api.cp.test:443:$v4" 'https://api.cp.test/auth/login?return=/x')
	[ "$out" = "302 $TB_FRONT_DOOR_URL/auth/login?return=/x" ] || fail "sign-in through the front door: $out"

	step "the firewall keeps the outside from everything but 22, 80 and 443, IPv4 and IPv6"
	systemd-run --quiet --unit=tb-listen python3 -m http.server 9999 --bind ::
	sleep 1
	curl -s -m 3 -o /dev/null "http://$v4:9999/" || fail "the test listener does not answer on the host"
	for addr in "$v4" "[$v6]"; do
		from_outside curl -s -m 3 -o /dev/null "http://$addr:9999/" && fail "the outside reached :9999 at $addr"
		from_outside timeout 3 bash -c "exec 3<>/dev/tcp/${addr//[\[\]]/}/22" || fail "the outside cannot reach SSH at $addr"
	done
	systemctl stop tb-listen

	step "with the firewall in place, a guest still reaches the outside over IPv4 NAT and IPv6, and is reached over IPv6"
	# A stand-in for a guest: a namespace behind a veth, masqueraded out of
	# the uplink, with a /128 of the host's prefix routed to it and answered
	# for upstream by proxy NDP, as the node agent routes a machine. Its
	# traffic is forwarded, which the host's firewall leaves alone.
	local g6=2001:db8:0:1::99
	ip netns add tb-guest
	ip link add tbg-host type veth peer name tbg-guest netns tb-guest
	ip link set tbg-host addrgenmode none
	ip addr add 10.199.0.1/30 dev tbg-host
	ip -6 addr add fe80::1/64 dev tbg-host nodad
	ip link set tbg-host up
	ip -n tb-guest link set lo up
	ip -n tb-guest addr add 10.199.0.2/30 dev tbg-guest
	ip -n tb-guest -6 addr add "$g6/128" dev tbg-guest nodad
	ip -n tb-guest link set tbg-guest up
	ip -n tb-guest route add default via 10.199.0.1
	ip -n tb-guest -6 route add default via fe80::1 dev tbg-guest
	ip -6 route add "$g6/128" dev tbg-host
	ip -6 neigh add proxy "$g6" dev tb-uplink
	nft -f - <<'EOF'
table ip tb-guest-nat {
	chain postrouting {
		type nat hook postrouting priority srcnat;
		ip saddr 10.199.0.0/30 oifname "tb-uplink" masquerade
	}
}
EOF
	nft list table inet hangar-host >/dev/null || fail "the host's firewall is not loaded"
	ip netns exec tb-guest ping -c 2 -W 2 192.0.2.1 >/dev/null || fail "the guest does not reach the outside over IPv4 NAT"
	ip netns exec tb-guest ping -6 -c 2 -W 2 2001:db8:0:ff::ffff >/dev/null || fail "the guest does not reach the outside over IPv6"
	from_outside ping -6 -c 2 -W 2 "$g6" >/dev/null || fail "the outside does not reach the guest over IPv6"
	ip -6 neigh del proxy "$g6" dev tb-uplink
	ip -6 route del "$g6/128" dev tbg-host
	nft delete table ip tb-guest-nat
	ip netns del tb-guest

	step "a restarted edge serves its certificates from its storage, and asks the CA for nothing"
	sleep 1
	since=$(date '+%Y-%m-%d %H:%M:%S')
	systemctl restart hangar-edge
	for _ in $(seq 1 20); do
		[ "$(door_curl 127.0.0.1 api.cp.test /healthz)" = "ok
 200" ] && break
		sleep 0.5
	done
	[ "$(door_curl 127.0.0.1 api.cp.test /healthz)" = "ok
 200" ] || fail "no API after a restart"
	journalctl -u hangar-edge --since "$since" --no-pager -o cat | grep -q 'obtaining certificate' && fail "the restarted edge asked the CA again"

	step "public endpoints without the front door are refused: the edge serves nowhere else"
	front_door_env endpoints
	if "$host_dir/bootstrap.sh" --env "$env_file" --release "$edge_release" --only edge >/dev/null 2>"$log"; then
		fail "the edge step ran with public endpoints and no front door"
	fi
	grep -q 'public endpoints (HANGAR_EDGE_DOMAIN) are served by the front door alone: set HANGAR_CP_DOMAIN' "$log" || fail "unexpected refusal: $(cat "$log")"
	grep -q '^apply: ' "$log" && fail "the refused run changed something: $(grep '^apply: ' "$log")"
	systemctl is-active --quiet hangar-edge || fail "the refused run stopped the edge"

	step "the front door off: the edge, its sockets and the public socket go, and :443 and :80 with them"
	front_door_env off
	auth_env dev
	"$host_dir/bootstrap.sh" --env "$env_file" --release "$release" >/dev/null 2>"$log" || fail "turning the edge off: $(cat "$log")"
	systemctl is-active --quiet hangar-edge && fail "the edge still runs"
	ss -Hltn | awk '{print $4}' | grep -qE ':(443|80)$' && fail "something still listens on :443 or :80: $(ss -Hltnp)"
	[ ! -e /run/hangar/public.sock ] || fail "the public socket is still there"
	[ "$("$host_dir/bootstrap.sh" --env "$env_file")" = "changes: 0" ] || fail "not converged with the edge off"
	"$host_dir/verify.sh" --env "$env_file" >"$work/door-verify.log" || fail "verify with the edge off: $(grep FAIL "$work/door-verify.log")"
	grep -q '^PASS edge off: /etc/hangar/local.env names no public endpoint domain$' "$work/door-verify.log" || fail "verify does not check the edge is off"
	systemctl stop tb-pebble
	rm -f /etc/hangar/pebble-ca.pem /usr/local/share/ca-certificates/tb-pebble.crt
	update-ca-certificates --fresh >/dev/null 2>&1
	rm -rf "$edge_release"
	ip -6 route del 2001:db8:0:ff::/64 dev tb-uplink
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

# with_subuid_clash CMD... runs CMD with a user's subordinate range among
# the VMs' ids in /etc/subuid, as useradd gave a second user before
# hangar-vms was kept there, then puts /etc/subuid back.
with_subuid_clash() {
	local had=0 rc=0
	if [ -f /etc/subuid ]; then had=1; cp -p /etc/subuid "$work/subuid"; fi
	echo 'old:165536:65536' >>/etc/subuid
	"$@" || rc=$?
	if [ "$had" = 1 ]; then cp -p "$work/subuid" /etc/subuid; else rm -f /etc/subuid; fi
	return "$rc"
}

ids_refused_fresh() {
	local before
	before=$(state_digest)
	if "$host_dir/bootstrap.sh" --env "$env_file" --release "$release" >/dev/null 2>"$work/ids.log"; then
		fail "bootstrap ran with old's subordinate range among the VMs' ids"
	fi
	grep -q "the VMs' ids 200000 to 265535 are taken: /etc/subuid range of old; move each to other ids" "$work/ids.log" ||
		fail "unexpected refusal: $(cat "$work/ids.log")"
	grep -q '^== ' "$work/ids.log" && fail "steps ran before the refusal: $(grep '^== ' "$work/ids.log")"
	[ "$(state_digest)" = "$before" ] || fail "the refusal changed the host"
	if "$host_dir/bootstrap.sh" --env "$env_file" --release "$release" --only release >/dev/null 2>"$work/ids.log"; then
		fail "the release step ran with old's range among the VMs' ids"
	fi
	[ ! -e /usr/local/bin/hangar-node ] || fail "a refused run installed hangar-node"
}

test_refuses_vm_id_clash() {
	step "a user's subordinate range among the VMs' ids is refused before any step, with nothing installed"
	with_subuid_clash ids_refused_fresh
}

ids_refused_installed() {
	local node_pid
	node_pid=$(main_pid hangar-node)
	if "$host_dir/verify.sh" --env "$env_file" >"$work/ids-verify.log"; then fail "verify passed with old's range among the VMs' ids"; fi
	grep -q "^FAIL no user, group or other subordinate range has one of the VMs' ids$" "$work/ids-verify.log" ||
		fail "verify did not say why: $(grep '^FAIL' "$work/ids-verify.log")"
	if "$host_dir/bootstrap.sh" --env "$env_file" >/dev/null 2>"$work/ids.log"; then fail "bootstrap ran with old's range among the VMs' ids"; fi
	grep -q '^apply: ' "$work/ids.log" && fail "the refused run changed something: $(grep '^apply: ' "$work/ids.log")"
	[ "$(main_pid hangar-node)" = "$node_pid" ] || fail "the refused run restarted the node"
}

test_refuses_vm_id_clash_installed() {
	step "on an installed host, verify reports a range among the VMs' ids, and a run refuses it"
	with_subuid_clash ids_refused_installed
	grep -qx 'hangar-vms:200000:65536' /etc/subuid || fail "the VMs' ids are not kept in /etc/subuid"
	grep -qx 'hangar-vms:200000:65536' /etc/subgid || fail "the VMs' ids are not kept in /etc/subgid"
	"$host_dir/verify.sh" --env "$env_file" >/dev/null || fail "verify once old's range is gone"
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
test_refuses_vm_id_clash
test_dry_run_changes_nothing
test_converges_and_is_idempotent
test_admin_socket
test_node_reconnects
test_oidc_cutover
test_front_door
test_refuses_tampered_release
test_refuses_half_applied_release
test_refuses_without_local_env
test_refuses_vm_id_clash_installed
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
