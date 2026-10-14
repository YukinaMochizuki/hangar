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

// fakeZFSScript is a zfs command for the calls New, EnsureRoot and
// EnsureVolume make: list (of a name, or of a parent's children, which it
// has none of), get, create, clone and set. Each dataset is a file of
// property<TAB>value lines in $FAKEZFS/ds, named with its slashes as %;
// every call is logged to $FAKEZFS/log. $FAKEZFS/set-fails makes set fail
// before its change, and $FAKEZFS/set-killed SIGKILLs set once its change
// is made, as an oops in the kernel does.
const fakeZFSScript = `#!/bin/sh
echo "$*" >>"$FAKEZFS/log"
file() { echo "$FAKEZFS/ds/$(echo "$1" | tr / %)"; }
put() { # put FILE PROP VALUE
	{ grep -v "^$2	" "$1" 2>/dev/null; printf '%s\t%s\n' "$2" "$3"; } >"$1.new" && mv "$1.new" "$1"
}
cmd=$1; shift
case $cmd in
list)
	[ "$1" = -Hp ] && exit 0
	[ -f "$(file "$4")" ] || { echo "cannot open '$4': dataset does not exist" >&2; exit 1; }
	echo "$4" ;;
create)
	opts=
	while [ "$1" = -o ]; do opts="$opts $2"; shift 2; done
	f=$(file "$1"); : >"$f"
	for o in $opts; do put "$f" "${o%%=*}" "${o#*=}"; done ;;
get)
	[ "$1 $2 $3" = "-Hp -o property,value" ] || { echo "fake zfs: get $*" >&2; exit 2; }
	f=$(file "$5")
	[ -f "$f" ] || { echo "cannot open '$5': dataset does not exist" >&2; exit 1; }
	for p in $(echo "$4" | tr , ' '); do
		v=$(grep "^$p	" "$f" | cut -f2)
		printf '%s\t%s\n' "$p" "${v:--}"
	done ;;
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
