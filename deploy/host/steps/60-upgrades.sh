# Unattended upgrades: security updates only, installed in a fixed window
# (04:00 in HANGAR_UPGRADES_TZ, UTC unless the host env file sets it), never
# a reboot, and never a restart of hangar's own
# services or VMs. An upgrade can re-exec systemd and trigger udev on every
# device, which the node rides out (docs/runbook/host-bootstrap.md), so it
# happens at a known time rather than at a random one after boot.

UPGRADES_CONF=/etc/apt/apt.conf.d/52hangar-unattended-upgrades
UPGRADES_PERIODIC=/etc/apt/apt.conf.d/21hangar-auto-upgrades
UPGRADES_NEEDRESTART=/etc/needrestart/conf.d/hangar.conf
UPGRADES_TZ=${HANGAR_UPGRADES_TZ:-UTC}
# Each entry is TIMER:TIME: the lists are fetched first, then upgrades run.
UPGRADES_TIMERS="apt-daily:03:30 apt-daily-upgrade:04:00"

upgrades_conf() {
	cat <<'EOF'
// Written by deploy/host/steps/60-upgrades.sh. The origins stay Ubuntu's
// defaults, which take security updates only.
Unattended-Upgrade::Automatic-Reboot "false";
Unattended-Upgrade::Automatic-Reboot-WithUsers "false";
EOF
}

upgrades_periodic() {
	cat <<'EOF'
// Written by deploy/host/steps/60-upgrades.sh; it wins over 20auto-upgrades.
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
}

# needrestart restarts services whose binaries or libraries an upgrade
# replaced. A VM's unit runs hangar-node's launcher by path, so a hangar
# release makes every VM look stale to it: hangar's units are restarted
# only by deploy/host, explicitly.
upgrades_needrestart() {
	cat <<'EOF'
# Written by deploy/host/steps/60-upgrades.sh. Hangar's services and VMs are
# restarted by deploy/host only, never after a package upgrade.
$nrconf{override_rc}{qr(^hangar-)} = 0;
EOF
}

upgrades_dropin() { printf '/etc/systemd/system/%s.timer.d/hangar.conf\n' "$1"; }

upgrades_timer_conf() {
	cat <<EOF
# Written by deploy/host/steps/60-upgrades.sh. A fixed time, and none missed
# is made up at boot.
[Timer]
OnCalendar=
OnCalendar=*-*-* $1:00 $UPGRADES_TZ
RandomizedDelaySec=0
Persistent=false
EOF
}

# upgrades_timer_live TIMER TIME holds when systemd runs the timer at TIME
# only, and the timer is enabled and waiting.
upgrades_timer_live() {
	local cal
	cal=$(systemctl show -p TimersCalendar --value "$1.timer")
	[[ $cal == "{ OnCalendar=*-*-* $2:00 $UPGRADES_TZ ; next_elapse="* ]] &&
		[ "$(grep -o 'OnCalendar=' <<<"$cal" | wc -l)" = 1 ] &&
		systemctl is-enabled --quiet "$1.timer" && systemctl is-active --quiet "$1.timer"
}

upgrades_files_ok() {
	local entry
	generated_is "$UPGRADES_CONF" upgrades_conf && generated_is "$UPGRADES_PERIODIC" upgrades_periodic &&
		generated_is "$UPGRADES_NEEDRESTART" upgrades_needrestart || return 1
	for entry in $UPGRADES_TIMERS; do
		generated_is "$(upgrades_dropin "${entry%%:*}")" upgrades_timer_conf "${entry#*:}" || return 1
	done
}

upgrades_timers_ok() {
	local entry
	for entry in $UPGRADES_TIMERS; do
		upgrades_timer_live "${entry%%:*}" "${entry#*:}" || return 1
	done
}

# upgrades_apt_ok checks what apt itself makes of the configuration.
upgrades_apt_ok() {
	local dump
	dump=$(apt-config dump)
	grep -qx 'APT::Periodic::Unattended-Upgrade "1";' <<<"$dump" &&
		grep -qx 'Unattended-Upgrade::Automatic-Reboot "false";' <<<"$dump" &&
		grep -q '^Unattended-Upgrade::Allowed-Origins:: ".*-security";' <<<"$dump" &&
		! grep -qE '^Unattended-Upgrade::(Allowed-Origins|Origins-Pattern):: ".*-(updates|proposed|backports)' <<<"$dump"
}

upgrades_apply() {
	local entry timer reload=0 before
	pkg_installed unattended-upgrades || [ "$DRY_RUN" = 1 ] ||
		die "unattended-upgrades is not installed; the packages step installs it"

	put_file "$UPGRADES_CONF" 644 "$(upgrades_conf)"
	put_file "$UPGRADES_PERIODIC" 644 "$(upgrades_periodic)"
	put_file "$UPGRADES_NEEDRESTART" 644 "$(upgrades_needrestart)"
	for entry in $UPGRADES_TIMERS; do
		timer=${entry%%:*}
		before=$CHANGES
		put_file "$(upgrades_dropin "$timer")" 644 "$(upgrades_timer_conf "${entry#*:}")"
		[ "$CHANGES" = "$before" ] || reload=1
	done
	# A drop-in written by a run that died before the reload.
	for entry in $UPGRADES_TIMERS; do
		[ "$(systemctl show -p NeedDaemonReload --value "${entry%%:*}.timer")" = no ] || reload=1
	done
	[ "$reload" = 0 ] || run "reload systemd for the apt timers" systemctl daemon-reload
	for entry in $UPGRADES_TIMERS; do
		timer=${entry%%:*}
		systemctl is-enabled --quiet "$timer.timer" ||
			run "enable $timer.timer" systemctl enable "$timer.timer"
		# A timer keeps the next elapse it computed until it is restarted.
		if [ "$reload" = 1 ] || ! upgrades_timer_live "$timer" "${entry#*:}"; then
			run "restart $timer.timer" systemctl restart "$timer.timer"
		fi
	done
}

upgrades_verify() {
	check "unattended-upgrades: config files" upgrades_files_ok
	check "unattended-upgrades: security only, no reboot (apt-config)" upgrades_apt_ok
	check "apt timers at 03:30 and 04:00 $UPGRADES_TZ only" upgrades_timers_ok
}
