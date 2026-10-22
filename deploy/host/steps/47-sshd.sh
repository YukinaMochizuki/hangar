# The host's sshd, the operator's one way in: keys only, no root, only the
# users HANGAR_SSH_USERS names (ubuntu unless the host env file says
# otherwise). One drop-in of our own; sshd_config and the image's drop-ins
# stay as installed. The guard is sshd -t on the config as it would be with
# the new drop-in, before anything is written; sshd -t again in place, with
# the old file put back on failure, only covers the host changing between
# the two. sshd is reloaded, never restarted, so its sessions keep going.
# The lockout guard counts logins with a key in an AuthorizedKeysFile only,
# not through AuthorizedKeysCommand, TrustedUserCAKeys or
# AuthorizedPrincipals*; a run warns when the host sets any of them.

# sshd reads sshd_config.d/*.conf in name order at the top of sshd_config
# and keeps the first value it reads for most keywords, so this name sorts
# before the cloud image's 50-cloud-init.conf and 60-cloudimg-settings.conf.
SSHD_DIR=/etc/ssh
SSHD_FILE=$SSHD_DIR/sshd_config.d/10-hangar.conf
SSHD_USERS=${HANGAR_SSH_USERS:-ubuntu}
# ssh.socket may hold :22 and start ssh.service at the first connection;
# a service that is not running reads the file when it starts.
SSHD_UNIT=ssh
# The drop-in as sshd was last reloaded with it, for a run that died
# between writing the file and reloading sshd: the next one reloads. A run
# that finds no drop-in removes it, as it then tells nothing of what sshd
# runs.
SSHD_LOADED=$STATE_DIR/sshd.loaded
# The users a run may stop letting in, in the environment of one run, never
# in the host env file (load_env refuses it there).
SSHD_DROP=${HANGAR_SSH_DROP_USERS:-}

# sshd_settings prints the drop-in's settings, one "Keyword value" a line.
# TCP forwarding stays as sshd has it: the users may sudo, so turning it off
# would keep no one from anything.
sshd_settings() {
	cat <<EOF
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
AllowUsers $SSHD_USERS
AuthorizedKeysFile .ssh/authorized_keys
X11Forwarding no
MaxAuthTries 4
LoginGraceTime 30
EOF
}

sshd_conf() {
	printf '# Written by deploy/host/steps/47-sshd.sh.\n'
	sshd_settings
}

# sshd_expected prints the settings as sshd -T prints them: keywords in
# lower case, and AllowUsers one user a line.
sshd_expected() {
	local key value u
	while read -r key value; do
		key=${key,,}
		if [ "$key" = allowusers ]; then
			for u in $value; do printf 'allowusers %s\n' "$u"; done
		else
			printf '%s %s\n' "$key" "$value"
		fi
	done < <(sshd_settings)
}

# sshd_rundir_ok holds when sshd's privilege separation directory is there,
# without which sshd -t and -T fail. ssh.service makes it when it starts,
# which under socket activation may not have happened yet; a real run makes
# it, a dry run and verify.sh only say it is missing.
sshd_rundir_ok() { [ -d /run/sshd ]; }

# sshd_copy DIR [FILE] copies sshd's config into DIR, DIR/sshd_config with
# its Include of sshd_config.d reading DIR/sshd_config.d, where FILE stands
# in for this step's drop-in (left out without FILE). It fails when
# sshd_config does not include sshd_config.d, so sshd would not read ours.
sshd_copy() {
	local dir=$1 ours=${2:-} f
	mkdir "$dir/sshd_config.d"
	for f in "$SSHD_DIR"/sshd_config.d/*.conf; do
		[ ! -f "$f" ] || [ "$f" = "$SSHD_FILE" ] || cp "$f" "$dir/sshd_config.d/"
	done
	[ -z "$ours" ] || cp "$ours" "$dir/sshd_config.d/${SSHD_FILE##*/}"
	sed -E "s#^([[:space:]]*[Ii][Nn][Cc][Ll][Uu][Dd][Ee][[:space:]]+)($SSHD_DIR/)?sshd_config\.d/#\1$dir/sshd_config.d/#" \
		"$SSHD_DIR/sshd_config" >"$dir/sshd_config"
	grep -q "$dir/sshd_config.d/" "$dir/sshd_config" && return 0
	log "$SSHD_DIR/sshd_config does not include sshd_config.d/*.conf, so sshd would not read $SSHD_FILE"
	return 1
}

# sshd_candidate_ok holds when sshd -t accepts the config as it would be
# with this step's drop-in in place of whatever is there now.
sshd_candidate_ok() {
	local tmp ok=1
	tmp=$(mktemp -d)
	sshd_conf >"$tmp/new.conf"
	if sshd_copy "$tmp" "$tmp/new.conf" && sshd -t -f "$tmp/sshd_config" >&2; then ok=0; fi
	rm -rf "$tmp"
	return "$ok"
}

# sshd_disk_config prints sshd -T for the config on disk, what a reload or
# a restart would load. When sshd refuses it, it prints it with our drop-in
# left out: a broken drop-in of ours is what a run repairs.
sshd_disk_config() {
	local tmp out ok=1
	if out=$(sshd -T 2>/dev/null); then
		printf '%s\n' "$out"
		return 0
	fi
	tmp=$(mktemp -d)
	if sshd_copy "$tmp" && sshd -T -f "$tmp/sshd_config"; then ok=0; fi
	rm -rf "$tmp"
	return "$ok"
}

# sshd_keys FILE prints the keys in an authorized_keys file.
sshd_keys() { [ -f "$1" ] && grep -Ev '^[[:space:]]*(#|$)' "$1" || true; }

# sshd_lists USER succeeds when USER is one of the users sshd lets in.
sshd_lists() { [[ " $SSHD_USERS " == *" $1 "* ]]; }

# sshd_match NAME PATTERN succeeds when PATTERN matches NAME as sshd
# matches one: * and ? are wildcards, every other character is itself.
sshd_match() {
	local name=$1 pat=$2 re='' c i
	for ((i = 0; i < ${#pat}; i++)); do
		c=${pat:i:1}
		case $c in
		'*') re+='.*' ;;
		'?') re+='.' ;;
		[[:alnum:]_]) re+=$c ;;
		']') re+='[]]' ;;
		'^') re+='\^' ;;
		*) re+="[$c]" ;;
		esac
	done
	[[ $name =~ ^$re$ ]]
}

# sshd_any NAME PATTERN... succeeds when a PATTERN matches NAME: the
# words of AllowUsers and DenyUsers, one pattern each.
sshd_any() {
	local name=$1 p
	shift
	for p in "$@"; do
		if sshd_match "$name" "$p"; then return 0; fi
	done
	return 1
}

# sshd_groups_match GROUPS LIST... succeeds when one of the LISTs, the
# words of AllowGroups or DenyGroups, matches one of the user's GROUPS: a
# LIST is patterns separated by commas, a pattern after ! negates, and a
# negated match of any group fails the LIST however the others match.
sshd_groups_match() {
	local groups=$1 list g p pos neg
	local -a pats
	shift
	for list in "$@"; do
		IFS=, read -ra pats <<<"$list"
		pos=0
		neg=0
		for g in $groups; do
			for p in "${pats[@]}"; do
				if [ "${p:0:1}" = '!' ]; then
					! sshd_match "$g" "${p:1}" || neg=1
				else
					! sshd_match "$g" "$p" || pos=1
				fi
			done
		done
		if [ "$pos" = 1 ] && [ "$neg" = 0 ]; then return 0; fi
	done
	return 1
}

# sshd_current_users prints the users other than root that the config on
# disk lets in with a key: a login shell, a key in a file its
# AuthorizedKeysFile names, and not kept out by AllowUsers, DenyUsers,
# AllowGroups or DenyGroups. A user@host pattern counts as letting the
# user in and as keeping no one out.
sshd_current_users() {
	local conf name home shell f has groups
	local -a files allow deny agroups dgroups
	conf=$(sshd_disk_config) || return 1
	read -ra files <<<"$(sed -n 's/^authorizedkeysfile //p' <<<"$conf")"
	read -ra allow <<<"$(sed -n 's/^allowusers //p' <<<"$conf" | sed 's/@.*//' | paste -sd' ' -)"
	read -ra deny <<<"$(sed -n 's/^denyusers //p' <<<"$conf" | sed '/@/d' | paste -sd' ' -)"
	read -ra agroups <<<"$(sed -n 's/^allowgroups //p' <<<"$conf" | paste -sd' ' -)"
	read -ra dgroups <<<"$(sed -n 's/^denygroups //p' <<<"$conf" | paste -sd' ' -)"
	while IFS=: read -r name _ _ _ _ home shell; do
		[ "$name" != root ] && [ -n "$home" ] || continue
		case ${shell##*/} in nologin | false) continue ;; esac
		[ "${#allow[@]}" = 0 ] || sshd_any "$name" "${allow[@]}" || continue
		! sshd_any "$name" "${deny[@]}" || continue
		if [ "${#agroups[@]}" != 0 ] || [ "${#dgroups[@]}" != 0 ]; then
			groups=$(id -nG "$name" 2>/dev/null) || true
			[ "${#agroups[@]}" = 0 ] || sshd_groups_match "$groups" "${agroups[@]}" || continue
			[ "${#dgroups[@]}" = 0 ] || ! sshd_groups_match "$groups" "${dgroups[@]}" || continue
		fi
		has=0
		for f in "${files[@]}"; do
			[ "$f" != none ] || continue
			f=${f//%%/$'\001'}
			f=${f//%h/$home}
			f=${f//%u/$name}
			f=${f//$'\001'/%}
			[ "${f:0:1}" = / ] || f=$home/$f
			if [ -n "$(sshd_keys "$f")" ]; then has=1; break; fi
		done
		[ "$has" = 0 ] || printf '%s\n' "$name"
	done < <(getent passwd)
}

# sshd_users_problem prints what would lock someone out, one line each,
# and warns of what only keeps out who cannot log in today: root in the
# list (its one key is a stub, and the drop-in refuses root anyway), no
# user with a key in .ssh/authorized_keys (the one file sshd reads keys
# from once the drop-in is loaded), and, unless HANGAR_SSH_DROP_USERS names
# them, a listed user whose keys are all in authorized_keys2 or a user who
# can log in with a key today and could not after.
sshd_users_problem() {
	local u home keys keys2 n=0 lost=() why current
	for u in $SSHD_USERS; do
		if [ "$u" = root ]; then
			printf 'HANGAR_SSH_USERS lists root, whom PermitRootLogin no keeps out\n'
			continue
		fi
		home=$(getent passwd "$u" | cut -d: -f6) || true
		if [ -z "$home" ]; then
			log "warning: AllowUsers names $u, who is not a user here"
			continue
		fi
		keys=$(sshd_keys "$home/.ssh/authorized_keys")
		keys2=$(sshd_keys "$home/.ssh/authorized_keys2")
		if [ -n "$keys" ]; then
			n=$((n + 1))
		elif [ -n "$keys2" ]; then
			lost+=("$u (keys in $home/.ssh/authorized_keys2 alone, which sshd stops reading)")
		else
			log "warning: $u has no key in $home/.ssh/authorized_keys and cannot log in"
		fi
		if [ -n "$keys" ] && [ -n "$(comm -13 <(printf '%s\n' "$keys" | sort -u) <(printf '%s\n' "$keys2" | sort -u) | sed '/^$/d')" ]; then
			log "warning: $home/.ssh/authorized_keys2 holds keys that are not in authorized_keys; sshd stops reading them"
		fi
	done
	[ "$n" != 0 ] ||
		printf 'none of the users sshd would let in (%s) has a key in ~/.ssh/authorized_keys\n' "$SSHD_USERS"

	if ! sshd_rundir_ok; then
		log "note: no /run/sshd, so who can log in today is not known; a real run makes it and refuses to leave any of them out"
	elif ! current=$(sshd_current_users); then
		printf 'sshd -T fails on the config on disk, with or without %s, so who can log in today is not known\n' "$SSHD_FILE"
	else
		while read -r u; do
			[ -n "$u" ] || continue
			if ! sshd_lists "$u"; then
				lost+=("$u (not in HANGAR_SSH_USERS)")
			elif [ -z "$(sshd_keys "$(getent passwd "$u" | cut -d: -f6)/.ssh/authorized_keys")" ] &&
				[[ " ${lost[*]} " != *" $u ("* ]]; then
				lost+=("$u (no key in ~/.ssh/authorized_keys)")
			fi
		done <<<"$current"
	fi
	for why in "${lost[@]}"; do
		if [[ " $SSHD_DROP " == *" ${why%% *} "* ]]; then
			log "warning: HANGAR_SSH_DROP_USERS: sshd stops letting in $why"
		else
			printf 'sshd would stop letting in %s: add or fix them, or name them in HANGAR_SSH_DROP_USERS for this run\n' "$why"
		fi
	done
}

sshd_loaded() { [ -f "$SSHD_FILE" ] && cmp -s "$SSHD_FILE" "$SSHD_LOADED"; }

sshd_reload() {
	if systemctl is-active --quiet "$SSHD_UNIT.service"; then
		systemctl reload "$SSHD_UNIT.service" || return 1
	elif ! systemctl is-active --quiet "$SSHD_UNIT.socket"; then
		log "warning: neither $SSHD_UNIT.service nor $SSHD_UNIT.socket runs; sshd reads $SSHD_FILE when it starts"
	fi
	mkdir -p "$STATE_DIR" && cp "$SSHD_FILE" "$SSHD_LOADED"
}

# sshd_effective USER prints sshd -T's lines for the keywords this step
# sets, sorted, for USER logging in over loopback: the config after every
# drop-in, Match blocks included.
sshd_effective() {
	local keys
	keys=$(sshd_expected | cut -d' ' -f1 | sort -u | paste -sd'|' -)
	sshd -T -C "user=$1,host=localhost,addr=127.0.0.1" | grep -E "^($keys) " | sort
}

# sshd_matches prints the Match lines in sshd_config and its drop-ins: a
# Match for an address or a host could loosen what sshd -T -C for loopback
# does not show.
sshd_matches() {
	grep -HinE '^[[:space:]]*match[[:space:]]' "$SSHD_DIR/sshd_config" "$SSHD_DIR"/sshd_config.d/*.conf 2>/dev/null || true
}

sshd_apply() {
	local problem before backup had=0 u written other
	# Not counted as a change: ssh.service makes it at every start.
	[ "$DRY_RUN" = 1 ] || sshd_rundir_ok || install -d -m 0755 /run/sshd
	[ "$DRY_RUN" = 1 ] || [ -f "$SSHD_FILE" ] || rm -f "$SSHD_LOADED"
	problem=$(sshd_users_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	if sshd_rundir_ok; then
		other=$(sshd_disk_config 2>/dev/null |
			grep -E '^(authorizedkeyscommand|trustedusercakeys|authorizedprincipalsfile|authorizedprincipalscommand) ' |
			grep -v ' none$' | paste -sd, -) || true
		[ -z "$other" ] ||
			log "warning: sshd also lets users in by $other, which the lockout check does not count"
	fi
	if ! sshd_rundir_ok; then
		log "note: no /run/sshd, so sshd -t cannot check the new config; a real run makes it and refuses a config sshd -t refuses"
	elif ! sshd_candidate_ok; then
		refuse "sshd -t refuses the config with the drop-in this step would write to $SSHD_FILE"
	fi

	backup=$(mktemp)
	if [ -f "$SSHD_FILE" ]; then cp -p "$SSHD_FILE" "$backup" && had=1; fi
	before=$CHANGES
	put_file "$SSHD_FILE" 644 "$(sshd_conf)"
	written=$((CHANGES - before))
	if [ "$written" != 0 ] && [ "$DRY_RUN" != 1 ] && ! sshd -t; then
		if [ "$had" = 1 ]; then mv -f "$backup" "$SSHD_FILE"; else rm -f "$SSHD_FILE" "$backup"; fi
		die "sshd -t refuses the config with $SSHD_FILE; left it as it was before the run, sshd not reloaded"
	fi
	rm -f "$backup"

	if [ "$written" != 0 ] || ! sshd_loaded; then
		run "reload sshd with $SSHD_FILE ($SSHD_UNIT.service, when it runs)" sshd_reload
	fi
	[ "$DRY_RUN" != 1 ] || return 0
	for u in $SSHD_USERS; do
		[ "$(sshd_effective "$u")" = "$(sshd_expected | sort)" ] && continue
		log "warning: sshd -T for $u differs from $SSHD_FILE; another drop-in or a Match block sets the same keywords:"
		diff <(sshd_expected | sort) <(sshd_effective "$u") >&2 || true
	done
	[ -z "$(sshd_matches)" ] || log "warning: Match blocks in sshd's config: $(sshd_matches | paste -sd' ' -)"
}

sshd_verify() {
	local line u
	check "sshd drop-in $SSHD_FILE, root's, 644" \
		sh -c "[ \"\$(stat -c '%U %a' '$SSHD_FILE')\" = 'root 644' ]"
	check "sshd drop-in $SSHD_FILE as generated" generated_is "$SSHD_FILE" sshd_conf
	check "sshd reloaded since $SSHD_FILE changed" sshd_loaded
	check "no Match block in $SSHD_DIR/sshd_config or sshd_config.d" test -z "$(sshd_matches)"
	if ! sshd_rundir_ok; then
		skip "sshd -t and sshd -T" "no /run/sshd: ssh.service has not started since boot; it reads the config when it does"
		return 0
	fi
	check "sshd -t accepts the config" sshd -t
	# sshd -T prints the config as sshd would read it now, after every
	# drop-in: what the users get, whatever file sets it.
	for u in $SSHD_USERS; do
		while read -r line; do
			check "sshd -T for $u: $line" sh -c "sshd -T -C 'user=$u,host=localhost,addr=127.0.0.1' | grep -qxF '$line'"
		done < <(sshd_expected | grep -v '^allowusers ')
	done
	check "sshd -T: allowusers $SSHD_USERS, no one else" \
		sh -c "[ \"\$(sshd -T | sed -n 's/^allowusers //p' | sort)\" = \"\$(printf '%s\n' $SSHD_USERS | sort)\" ]"
}
