# The host's sshd, the operator's one way in: keys only, no root, only the
# users HANGAR_SSH_USERS names (ubuntu unless the host env file says
# otherwise). One drop-in of our own; sshd_config and the image's drop-ins
# stay as installed. A config sshd -t refuses is never loaded, and sshd is
# reloaded, never restarted, so the sessions it has keep going either way.

# sshd reads sshd_config.d/*.conf in name order at the top of sshd_config
# and keeps the first value it reads for most keywords, so this name sorts
# before the cloud image's 50-cloud-init.conf and 60-cloudimg-settings.conf.
SSHD_FILE=/etc/ssh/sshd_config.d/10-hangar.conf
SSHD_USERS=${HANGAR_SSH_USERS:-ubuntu}
# ssh.socket may hold :22 and start ssh.service at the first connection;
# a service that is not running reads the file when it starts.
SSHD_UNIT=ssh
# The drop-in as sshd was last reloaded with it: a file changed since, by
# hand or by a run that died before the reload, is reloaded again.
SSHD_LOADED=$STATE_DIR/sshd.loaded

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

# sshd_candidate_ok holds when sshd -t accepts the drop-in this step would
# write, read ahead of everything the host already has.
sshd_candidate_ok() {
	local tmp ok=0
	tmp=$(mktemp)
	{ sshd_conf; printf 'Include /etc/ssh/sshd_config\n'; } >"$tmp"
	sshd -t -f "$tmp" >&2 || ok=1
	rm -f "$tmp"
	return "$ok"
}

# sshd_keys FILE prints the keys in an authorized_keys file.
sshd_keys() { [ -f "$1" ] && grep -Ev '^[[:space:]]*(#|$)' "$1" || true; }

# sshd_users_problem prints why the drop-in would lock everyone out: no
# user it lets in has a key in .ssh/authorized_keys, the one file sshd reads
# keys from once the drop-in is loaded. What locks out one user alone it
# only warns of.
sshd_users_problem() {
	local u home keys n=0
	for u in $SSHD_USERS; do
		home=$(getent passwd "$u" | cut -d: -f6) || true
		if [ -z "$home" ]; then
			log "warning: AllowUsers names $u, who is not a user here"
			continue
		fi
		keys=$(sshd_keys "$home/.ssh/authorized_keys")
		if [ -z "$keys" ]; then
			log "warning: $u has no key in $home/.ssh/authorized_keys and cannot log in"
		else
			n=$((n + 1))
		fi
		if [ -n "$(comm -13 <(printf '%s\n' "$keys" | sort -u) <(sshd_keys "$home/.ssh/authorized_keys2" | sort -u))" ]; then
			log "warning: $home/.ssh/authorized_keys2 holds keys that are not in authorized_keys; sshd stops reading them"
		fi
	done
	[ "$n" != 0 ] ||
		printf 'none of the users sshd would let in (%s) has a key in ~/.ssh/authorized_keys\n' "$SSHD_USERS"
}

sshd_loaded() { [ -f "$SSHD_FILE" ] && cmp -s "$SSHD_FILE" "$SSHD_LOADED"; }

sshd_reload() {
	if systemctl is-active --quiet "$SSHD_UNIT.service"; then
		systemctl reload "$SSHD_UNIT.service" || return 1
	fi
	mkdir -p "$STATE_DIR" && cp "$SSHD_FILE" "$SSHD_LOADED"
}

# sshd_effective_lines prints sshd -T's lines for the keywords this step
# sets, sorted.
sshd_effective_lines() {
	local keys
	keys=$(sshd_expected | cut -d' ' -f1 | sort -u | paste -sd'|' -)
	sshd -T | grep -E "^($keys) " | sort
}

sshd_effective_ok() { [ "$(sshd_effective_lines)" = "$(sshd_expected | sort)" ]; }

sshd_apply() {
	local problem before backup had=0
	problem=$(sshd_users_problem)
	[ -z "$problem" ] || refuse "$problem"
	sshd_candidate_ok ||
		refuse "sshd -t refuses the config with the drop-in this step would write to $SSHD_FILE"

	backup=$(mktemp)
	if [ -f "$SSHD_FILE" ]; then cp -p "$SSHD_FILE" "$backup" && had=1; fi
	before=$CHANGES
	put_file "$SSHD_FILE" 644 "$(sshd_conf)"
	# Checked again in place: the host's config may have changed since.
	if [ "$CHANGES" != "$before" ] && [ "$DRY_RUN" != 1 ] && ! sshd -t; then
		if [ "$had" = 1 ]; then mv -f "$backup" "$SSHD_FILE"; else rm -f "$SSHD_FILE" "$backup"; fi
		die "sshd -t refuses the config with $SSHD_FILE; left it as it was before the run, sshd not reloaded"
	fi
	rm -f "$backup"

	sshd_loaded || run "reload sshd with $SSHD_FILE ($SSHD_UNIT.service, when it runs)" sshd_reload
	if [ "$DRY_RUN" != 1 ] && ! sshd_effective_ok; then
		log "warning: sshd -T differs from $SSHD_FILE; a drop-in that sorts before it sets the same keywords:"
		diff <(sshd_expected | sort) <(sshd_effective_lines) >&2 || true
	fi
}

sshd_verify() {
	local line
	check "sshd drop-in $SSHD_FILE, root's, 644" \
		sh -c "[ \"\$(stat -c '%U %a' '$SSHD_FILE')\" = 'root 644' ]"
	check "sshd drop-in $SSHD_FILE as generated" cmp -s "$SSHD_FILE" <(sshd_conf)
	check "sshd reloaded since $SSHD_FILE changed" sshd_loaded
	check "sshd -t accepts the config" sshd -t
	# sshd -T prints the config as sshd would read it now, after every
	# drop-in: what the users get, whatever file sets it.
	while read -r line; do
		check "sshd -T: $line" sh -c "sshd -T | grep -qxF '$line'"
	done < <(sshd_expected | grep -v '^allowusers ')
	check "sshd -T: allowusers $SSHD_USERS, no one else" \
		sh -c "[ \"\$(sshd -T | sed -n 's/^allowusers //p' | sort)\" = \"\$(printf '%s\n' $SSHD_USERS | sort)\" ]"
}
