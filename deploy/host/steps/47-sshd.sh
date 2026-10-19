# The host's sshd, the operator's one way in: keys only, no root, only the
# users HANGAR_SSH_USERS names (ubuntu unless the host env file says
# otherwise). One drop-in of our own; sshd_config and the image's drop-ins
# stay as installed. The guard is sshd -t on the config as it would be with
# the new drop-in, before anything is written; sshd -t again in place, with
# the old file put back on failure, only covers the host changing between
# the two. sshd is reloaded, never restarted, so its sessions keep going.

# sshd reads sshd_config.d/*.conf in name order at the top of sshd_config
# and keeps the first value it reads for most keywords, so this name sorts
# before the cloud image's 50-cloud-init.conf and 60-cloudimg-settings.conf.
SSHD_DIR=/etc/ssh
SSHD_FILE=$SSHD_DIR/sshd_config.d/10-hangar.conf
SSHD_USERS=${HANGAR_SSH_USERS:-ubuntu}
# ssh.socket may hold :22 and start ssh.service at the first connection;
# a service that is not running reads the file when it starts.
SSHD_UNIT=ssh
# The drop-in as sshd was last reloaded with it: a file changed since, by
# hand or by a run that died before the reload, is reloaded again.
SSHD_LOADED=$STATE_DIR/sshd.loaded
# Set to 1 in the environment of a run to apply what would lock someone out
# (keys lost with authorized_keys2, the operator not let in) anyway.
SSHD_FORCE=${HANGAR_SSH_FORCE:-0}

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

# sshd_rundir makes sshd's privilege separation directory, without which
# sshd -t and -T fail. ssh.service makes it when it starts, which under
# socket activation may not have happened yet.
sshd_rundir() { install -d -m 0755 /run/sshd; }

# sshd_candidate_ok holds when sshd -t accepts the config as it would be
# with this step's drop-in in place of whatever is there now: a copy of
# sshd_config whose Include of sshd_config.d reads a copy of that directory.
sshd_candidate_ok() {
	local tmp f ok=1
	tmp=$(mktemp -d)
	mkdir "$tmp/sshd_config.d"
	for f in "$SSHD_DIR"/sshd_config.d/*.conf; do
		[ ! -f "$f" ] || [ "$f" = "$SSHD_FILE" ] || cp "$f" "$tmp/sshd_config.d/"
	done
	sshd_conf >"$tmp/sshd_config.d/${SSHD_FILE##*/}"
	sed -E "s#^([[:space:]]*[Ii][Nn][Cc][Ll][Uu][Dd][Ee][[:space:]]+)($SSHD_DIR/)?sshd_config\.d/#\1$tmp/sshd_config.d/#" \
		"$SSHD_DIR/sshd_config" >"$tmp/sshd_config"
	if ! grep -q "$tmp/sshd_config.d/" "$tmp/sshd_config"; then
		log "$SSHD_DIR/sshd_config does not include sshd_config.d/*.conf, so sshd would not read $SSHD_FILE"
	elif sshd_rundir && sshd -t -f "$tmp/sshd_config" >&2; then
		ok=0
	fi
	rm -rf "$tmp"
	return "$ok"
}

# sshd_keys FILE prints the keys in an authorized_keys file.
sshd_keys() { [ -f "$1" ] && grep -Ev '^[[:space:]]*(#|$)' "$1" || true; }

# sshd_lists USER succeeds when USER is one of the users sshd lets in.
sshd_lists() { [[ " $SSHD_USERS " == *" $1 "* ]]; }

# sshd_users_problem prints what would lock someone out, one line each:
# root in the list (its one key is a stub, and the drop-in refuses root
# anyway), no user with a key in .ssh/authorized_keys (the one file sshd
# reads keys from once the drop-in is loaded), a user whose keys are all in
# authorized_keys2, or the operator running the step left out. What only
# keeps out a user who cannot log in today it warns of.
sshd_users_problem() {
	local u home keys keys2 n=0
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
			[ "$SSHD_FORCE" = 1 ] ||
				printf '%s has keys in %s/.ssh/authorized_keys2 alone, which sshd stops reading; move them to authorized_keys, or set HANGAR_SSH_FORCE=1\n' "$u" "$home"
		else
			log "warning: $u has no key in $home/.ssh/authorized_keys and cannot log in"
		fi
		if [ -n "$keys" ] && [ -n "$(comm -13 <(printf '%s\n' "$keys" | sort -u) <(printf '%s\n' "$keys2" | sort -u) | sed '/^$/d')" ]; then
			log "warning: $home/.ssh/authorized_keys2 holds keys that are not in authorized_keys; sshd stops reading them"
		fi
	done
	[ "$n" != 0 ] ||
		printf 'none of the users sshd would let in (%s) has a key in ~/.ssh/authorized_keys\n' "$SSHD_USERS"
	# Empty under systemd-run, which the runbook runs the real thing in;
	# the dry run before it, under sudo, catches it.
	if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] && ! sshd_lists "$SUDO_USER" && [ "$SSHD_FORCE" != 1 ]; then
		printf 'sshd would not let in %s, who runs this; add them to HANGAR_SSH_USERS, or set HANGAR_SSH_FORCE=1\n' "$SUDO_USER"
	fi
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
	local problem before backup had=0 u
	problem=$(sshd_users_problem)
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
	sshd_candidate_ok ||
		refuse "sshd -t refuses the config with the drop-in this step would write to $SSHD_FILE"

	backup=$(mktemp)
	if [ -f "$SSHD_FILE" ]; then cp -p "$SSHD_FILE" "$backup" && had=1; fi
	before=$CHANGES
	put_file "$SSHD_FILE" 644 "$(sshd_conf)"
	if [ "$CHANGES" != "$before" ] && [ "$DRY_RUN" != 1 ] && ! sshd -t; then
		if [ "$had" = 1 ]; then mv -f "$backup" "$SSHD_FILE"; else rm -f "$SSHD_FILE" "$backup"; fi
		die "sshd -t refuses the config with $SSHD_FILE; left it as it was before the run, sshd not reloaded"
	fi
	rm -f "$backup"

	sshd_loaded || run "reload sshd with $SSHD_FILE ($SSHD_UNIT.service, when it runs)" sshd_reload
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
	sshd_rundir
	check "sshd drop-in $SSHD_FILE, root's, 644" \
		sh -c "[ \"\$(stat -c '%U %a' '$SSHD_FILE')\" = 'root 644' ]"
	check "sshd drop-in $SSHD_FILE as generated" cmp -s "$SSHD_FILE" <(sshd_conf)
	check "sshd reloaded since $SSHD_FILE changed" sshd_loaded
	check "sshd -t accepts the config" sshd -t
	check "no Match block in $SSHD_DIR/sshd_config or sshd_config.d" test -z "$(sshd_matches)"
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
