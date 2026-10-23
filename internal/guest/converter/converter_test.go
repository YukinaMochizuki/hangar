package converter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// fakeQemuImg is a qemu-img that logs its arguments to $DIR/args and
// prints $DIR/info for info, or fails saying $DIR/fail when it is there.
const fakeQemuImg = `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >>"$dir/args"
if [ -e "$dir/fail" ]; then cat "$dir/fail" >&2; exit 1; fi
[ "$1" = info ] && cat "$dir/info"
exit 0
`

func fakeServer(t *testing.T, info string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "qemu-img"), []byte(fakeQemuImg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "vdd")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return &Server{QemuImg: filepath.Join(dir, "qemu-img"), Source: filepath.Join(dir, "vdc"), Target: target}, dir
}

func args(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// TestInspectSaysWhatQemuImgFound: what qemu-img info says, the files the
// image names included, is reported as it is, for the host to judge.
func TestInspectSaysWhatQemuImgFound(t *testing.T) {
	s, dir := fakeServer(t, `{"format": "qcow2", "virtual-size": 2147483648, "backing-filename": "/etc/shadow",
		"format-specific": {"type": "qcow2", "data": {"data-file": "/dev/vda"}}}`)
	res, err := s.Inspect(t.Context(), connect.NewRequest(&guestv1.InspectRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := &guestv1.InspectResponse{Format: "qcow2", VirtualSize: 2 << 30, BackingFile: "/etc/shadow", DataFile: "/dev/vda"}
	if got := res.Msg; got.GetFormat() != want.GetFormat() || got.GetVirtualSize() != want.GetVirtualSize() ||
		got.GetBackingFile() != want.GetBackingFile() || got.GetDataFile() != want.GetDataFile() {
		t.Errorf("inspected %v, want %v", got, want)
	}
	if got := args(t, dir); got != "info --output=json "+s.Source {
		t.Errorf("qemu-img ran with %q", got)
	}
}

// TestConvertReadsTheFormatGiven: convert is told the format and never
// probes it, writes onto the target as raw, and refuses a format other
// than raw or qcow2 before running anything.
func TestConvertReadsTheFormatGiven(t *testing.T) {
	s, dir := fakeServer(t, "")
	if _, err := s.Convert(t.Context(), connect.NewRequest(&guestv1.ConvertRequest{Format: "vmdk"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("converting a vmdk: %v, want invalid_argument", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); err == nil {
		t.Error("qemu-img ran for a format refused")
	}
	if _, err := s.Convert(t.Context(), connect.NewRequest(&guestv1.ConvertRequest{Format: "qcow2"})); err != nil {
		t.Fatal(err)
	}
	want := "convert -q -n --target-is-zero -t none -T writeback -f qcow2 -O raw " + s.Source + " " + s.Target
	if got := args(t, dir); got != want {
		t.Errorf("qemu-img ran with %q, want %q", got, want)
	}
}

// TestAFileQemuImgRefusesIsInvalid: qemu-img failing on the file is
// INVALID_ARGUMENT, saying what it said; info it wrote that is no JSON
// too.
func TestAFileQemuImgRefusesIsInvalid(t *testing.T) {
	s, dir := fakeServer(t, "not json")
	if _, err := s.Inspect(t.Context(), connect.NewRequest(&guestv1.InspectRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("info that is no JSON: %v, want invalid_argument", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fail"), []byte("Image is not in qcow2 format"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.Convert(t.Context(), connect.NewRequest(&guestv1.ConvertRequest{Format: "qcow2"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "not in qcow2 format") {
		t.Errorf("qemu-img refusing the file: %v, want invalid_argument saying why", err)
	}
}
