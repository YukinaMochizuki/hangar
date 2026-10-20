#!/usr/bin/env bash
# hangar-zpool-metrics FILE writes, for node_exporter's textfile collector,
# what its zfs collector leaves out of each pool: its size and allocation,
# the read, write and checksum errors its devices count, its permanent data
# errors, and when this ran. The monitoring step installs it and runs it each
# minute (hangar-zpool-metrics.timer). The file is replaced whole, so a scrape
# never reads half of it.
set -euo pipefail
shopt -s inherit_errexit

out=${1:?usage: hangar-zpool-metrics FILE}
tmp=$(mktemp "$out.XXXXXX")
trap 'rm -f "$tmp"' EXIT

# The config table's rows are NAME STATE READ WRITE CKSUM, after its header;
# the errors: line ends the pool.
errors=$(zpool list -H -o name | while read -r pool; do
	zpool status -p "$pool" | awk -v pool="$pool" '
		$1 == "NAME" && $2 == "STATE" { table = 1; next }
		table && NF >= 5 && $3 ~ /^[0-9]+$/ && $4 ~ /^[0-9]+$/ && $5 ~ /^[0-9]+$/ {
			r += $3; w += $4; c += $5
		}
		/^errors:/ {
			table = 0
			data = ($2 == "No") ? 0 : $2
		}
		END {
			printf "hangar_zpool_device_errors{zpool=\"%s\",type=\"read\"} %d\n", pool, r
			printf "hangar_zpool_device_errors{zpool=\"%s\",type=\"write\"} %d\n", pool, w
			printf "hangar_zpool_device_errors{zpool=\"%s\",type=\"checksum\"} %d\n", pool, c
			printf "hangar_zpool_data_errors{zpool=\"%s\"} %d\n", pool, data
		}'
done)

# Each metric's samples follow its own HELP and TYPE, as the text format
# wants.
{
	printf '%s\n' \
		'# HELP hangar_zpool_size_bytes The pool'"'"'s size (zpool list).' \
		'# TYPE hangar_zpool_size_bytes gauge'
	zpool list -Hp -o name,size | awk '{ printf "hangar_zpool_size_bytes{zpool=\"%s\"} %s\n", $1, $2 }'
	printf '%s\n' \
		'# HELP hangar_zpool_allocated_bytes What the pool holds (zpool list).' \
		'# TYPE hangar_zpool_allocated_bytes gauge'
	zpool list -Hp -o name,allocated | awk '{ printf "hangar_zpool_allocated_bytes{zpool=\"%s\"} %s\n", $1, $2 }'
	printf '%s\n' \
		'# HELP hangar_zpool_device_errors Errors the pool and its devices count since the last zpool clear, by type (zpool status).' \
		'# TYPE hangar_zpool_device_errors gauge'
	grep '^hangar_zpool_device_errors' <<<"$errors" || true
	printf '%s\n' \
		'# HELP hangar_zpool_data_errors Permanent data errors in the pool (zpool status).' \
		'# TYPE hangar_zpool_data_errors gauge'
	grep '^hangar_zpool_data_errors' <<<"$errors" || true
	printf '%s\n' \
		'# HELP hangar_zpool_metrics_timestamp_seconds When these were read.' \
		'# TYPE hangar_zpool_metrics_timestamp_seconds gauge'
	printf 'hangar_zpool_metrics_timestamp_seconds %s\n' "$(date +%s)"
} >"$tmp"
chmod 644 "$tmp"
mv -f "$tmp" "$out"
trap - EXIT
