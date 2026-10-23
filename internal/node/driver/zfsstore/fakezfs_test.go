package zfsstore

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeZFSScript is a zfs command for the calls the driver makes: list (of
// a name, or -r of a parent's volumes or snapshots), get (with -s local,
// only what is set on the dataset itself), create (a volume's -s and -V
// too), clone, snapshot, set, destroy (refused for a volume with
// snapshots), and send piped into receive. Each dataset is a file of
// property<TAB>value lines in $FAKEZFS/ds, named with its slashes as %; a
// snapshot, name@snap, inherits what its volume has and it has not, as
// ZFS's user properties are inherited; destroying a snapshot that is not
// there says so as zfs does. A volume is made holding nothing,
// logicalreferenced 0, which its snapshots read. A send writes its snapshot's
// volsize and logicalreferenced, which a receive makes its volume and
// snapshot of. Every call is logged to $FAKEZFS/log. Switches, files in
// $FAKEZFS: set-fails makes set fail before its change, and set-killed
// SIGKILLs set once its change is made, as an oops in the kernel does;
// send-forever makes send write without end; send-fails makes it fail
// with an I/O error after a little; recv-fails makes receive fail at once,
// reading nothing; recv-late makes it fail once it has made the volume;
// recv-nospace makes it fail out of space; recv-hangs makes it never end.
const fakeZFSScript = `#!/bin/sh
echo "$*" >>"$FAKEZFS/log"
file() { echo "$FAKEZFS/ds/$(echo "$1" | tr / %)"; }
put() { # put FILE PROP VALUE
	{ grep -v "^$2	" "$1" 2>/dev/null; printf '%s\t%s\n' "$2" "$3"; } >"$1.new" && mv "$1.new" "$1"
}
getp() { # getp NAME PROP [local]: its value, inherited unless local
	v=$(grep "^$2	" "$(file "$1")" 2>/dev/null | cut -f2)
	if [ -z "$v" ] && [ -z "$3" ]; then
		case $1 in *@*) v=$(grep "^$2	" "$(file "${1%@*}")" 2>/dev/null | cut -f2) ;; esac
	fi
	printf '%s' "$v"
}
cmd=$1; shift
case $cmd in
list)
	if [ "$1" = -Hp ]; then # list -Hp -r -t TYPE -o COLUMNS PARENT
		type=$4; cols=$6; parent=$7
		for f in "$FAKEZFS"/ds/*; do
			[ -f "$f" ] || continue
			name=$(basename "$f" | tr % /)
			case $name in "$parent"/*) ;; *) continue ;; esac
			case $name in
			*@*) t=snapshot ;;
			*) if grep -q "^volsize	" "$f"; then t=volume; else t=filesystem; fi ;;
			esac
			[ "$t" = "$type" ] || continue
			row=
			for c in $(echo "$cols" | tr , ' '); do
				if [ "$c" = name ]; then v=$name; else v=$(getp "$name" "$c"); fi
				row="$row${row:+	}${v:--}"
			done
			printf '%s\n' "$row"
		done
		exit 0
	fi
	[ -f "$(file "$4")" ] || { echo "cannot open '$4': dataset does not exist" >&2; exit 1; }
	echo "$4" ;;
snapshot)
	opts=
	while [ "$1" = -o ]; do opts="$opts $2"; shift 2; done
	[ -f "$(file "${1%@*}")" ] || { echo "cannot open '${1%@*}': dataset does not exist" >&2; exit 1; }
	f=$(file "$1"); : >"$f"
	for o in $opts; do put "$f" "${o%%=*}" "${o#*=}"; done ;;
send)
	[ -f "$(file "$1")" ] || { echo "cannot open '$1': dataset does not exist" >&2; exit 1; }
	if [ -e "$FAKEZFS/send-forever" ]; then exec yes stream; fi
	if [ -e "$FAKEZFS/send-fails" ]; then echo partial; echo "warning: cannot send '$1': Input/output error" >&2; exit 1; fi
	printf 'volsize\t%s\nlogicalreferenced\t%s\n' "$(getp "$1" volsize)" "$(getp "$1" logicalreferenced)" ;;
receive)
	[ -e "$FAKEZFS/recv-fails" ] && { echo "cannot receive new filesystem stream: destination '$(echo "$*" | awk '{print $NF}')' exists" >&2; exit 1; }
	[ -e "$FAKEZFS/recv-hangs" ] && exec sleep 1000
	opts=
	while [ "$1" = -o ]; do opts="$opts $2"; shift 2; done
	stream=$(cat)
	[ -e "$FAKEZFS/recv-nospace" ] && { echo "cannot receive new filesystem stream: out of space" >&2; exit 1; }
	echo "$stream" | grep -q "^volsize	" || { echo "cannot receive: failed to read from stream" >&2; exit 1; }
	vol=$(file "${1%@*}"); snap=$(file "$1")
	echo "$stream" | grep "^volsize	" >"$vol"
	for o in $opts; do put "$vol" "${o%%=*}" "${o#*=}"; done
	[ -e "$FAKEZFS/recv-late" ] && { echo "cannot receive new filesystem stream: checksum mismatch" >&2; exit 1; }
	echo "$stream" >"$snap" ;;
create)
	opts=
	while :; do
		case $1 in
		-s) opts="$opts sparse=yes"; shift ;;
		-V) opts="$opts volsize=$2 logicalreferenced=0"; shift 2 ;;
		-o) opts="$opts $2"; shift 2 ;;
		*) break ;;
		esac
	done
	f=$(file "$1"); : >"$f"
	for o in $opts; do put "$f" "${o%%=*}" "${o#*=}"; done ;;
get)
	loc=
	if [ "$2 $3" = "-s local" ]; then loc=1; a=$1; shift 3; set -- "$a" "$@"; fi
	[ "$1 $2 $3" = "-Hp -o property,value" ] || { echo "fake zfs: get $*" >&2; exit 2; }
	[ -f "$(file "$5")" ] || { echo "cannot open '$5': dataset does not exist" >&2; exit 1; }
	for p in $(echo "$4" | tr , ' '); do
		v=$(getp "$5" "$p" $loc)
		if [ -n "$loc" ]; then
			[ -n "$v" ] && printf '%s\t%s\n' "$p" "$v"
		else
			printf '%s\t%s\n' "$p" "${v:--}"
		fi
	done
	exit 0 ;;
clone)
	opts=
	while [ "$1" = -o ]; do opts="$opts $2"; shift 2; done
	src=$(file "$1"); dst=$(file "$2")
	[ -f "$src" ] || { echo "cannot open '$1': dataset does not exist" >&2; exit 1; }
	grep "^volsize	" "$src" >"$dst"
	for o in $opts; do put "$dst" "${o%%=*}" "${o#*=}"; done ;;
set)
	[ -e "$FAKEZFS/set-fails" ] && { echo "cannot set property for '$2': out of luck" >&2; exit 1; }
	put "$(file "$2")" "${1%%=*}" "${1#*=}"
	[ -e "$FAKEZFS/set-killed" ] && kill -9 $$
	exit 0 ;;
destroy)
	f=$(file "$1")
	case $1 in
	*@*) [ -f "$f" ] || { echo "could not find any snapshots to destroy; check snapshot names." >&2; exit 1; } ;;
	*) [ -f "$f" ] || { echo "cannot open '$1': dataset does not exist" >&2; exit 1; } ;;
	esac
	for s in "$f"@*; do
		[ -f "$s" ] && { echo "cannot destroy '$1': volume has children" >&2; exit 1; }
	done
	rm "$f" ;;
*)
	echo "fake zfs: $cmd $*" >&2; exit 2 ;;
esac
`

// fakeZFS is the fake zfs on PATH and a Store of dataset hangar using it,
// whose zvols need no device.
type fakeZFS struct {
	t   *testing.T
	dir string
	s   *Store
}

func newFakeZFS(t *testing.T) *fakeZFS {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, filepath.Join(dir, "ds")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "zfs"), []byte(fakeZFSScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEZFS", dir)
	s := &Store{cfg: Config{Dataset: "hangar", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	s.waitDev = func(context.Context, string) (string, error) { return "/dev/zd0", nil }
	return &fakeZFS{t: t, dir: dir, s: s}
}

// dataset makes ds with the properties, given as name, value, ...
func (f *fakeZFS) dataset(ds string, props ...string) {
	f.t.Helper()
	var b strings.Builder
	for i := 0; i+1 < len(props); i += 2 {
		b.WriteString(props[i] + "\t" + props[i+1] + "\n")
	}
	if err := os.WriteFile(f.path(ds), []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeZFS) path(ds string) string {
	return filepath.Join(f.dir, "ds", strings.ReplaceAll(ds, "/", "%"))
}

// prop is ds's property as the fake holds it.
func (f *fakeZFS) prop(ds, name string) string {
	f.t.Helper()
	b, err := os.ReadFile(f.path(ds))
	if err != nil {
		f.t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok && k == name {
			return v
		}
	}
	return ""
}

// calls is every zfs command line run, those starting with prefix.
func (f *fakeZFS) calls(prefix string) []string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "log"))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" && strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// touch makes $FAKEZFS/name, a switch of the fake's.
func (f *fakeZFS) touch(name string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), nil, 0o644); err != nil {
		f.t.Fatal(err)
	}
}
