package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

func TestTheIdentityFilesReachWhereTheImageReadsThem(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(filepath.Join(root, "lib/net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(etc, "hosts"), []byte("image's"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "lib/net/hostname"), []byte("x"), 0o644)
	_ = os.Symlink("/run/systemd/resolve/stub-resolv.conf", filepath.Join(etc, "resolv.conf"))
	_ = os.Symlink("../lib/net/hostname", filepath.Join(etc, "hostname"))
	_ = os.Symlink("/var/missing", filepath.Join(etc, "dangling"))
	// Debian's resolvconf, through /var/run, itself a link to /run.
	_ = os.MkdirAll(filepath.Join(root, "var"), 0o755)
	_ = os.Symlink("../run", filepath.Join(root, "var/run"))
	_ = os.Symlink("/var/run/resolvconf/resolv.conf", filepath.Join(etc, "resolvconf"))
	_ = os.Symlink("../../../../etc/hosts", filepath.Join(etc, "climbing"))
	_ = os.Symlink("loop", filepath.Join(etc, "loop"))

	for _, tt := range []struct {
		path string
		want action
	}{
		{"/etc/hosts", action{bind: filepath.Join(etc, "hosts")}},
		{"/etc/resolv.conf", action{write: filepath.Join(root, "run/systemd/resolve/stub-resolv.conf")}},
		{"/etc/hostname", action{bind: filepath.Join(root, "lib/net/hostname")}},
		{"/etc/absent", action{bind: filepath.Join(etc, "absent"), create: true}},
		{"/etc/resolvconf", action{write: filepath.Join(root, "run/resolvconf/resolv.conf")}},
		{"/etc/climbing", action{bind: filepath.Join(etc, "hosts")}}, // .. stops at the root
	} {
		got, err := placement(root, tt.path)
		if err != nil || got != tt.want {
			t.Errorf("%s: %+v %v, want %+v", tt.path, got, err, tt.want)
		}
	}
	if _, err := placement(root, "/etc/dangling"); err == nil {
		t.Error("a symlink to nothing outside /run was taken")
	}
	if _, err := placement(root, "/etc/loop"); err == nil {
		t.Error("a symlink loop was taken")
	}
	if fi, err := os.Stat(filepath.Join(etc, "hosts")); err != nil || fi.Size() != int64(len("image's")) {
		t.Error("deciding where to put a file wrote to the image")
	}
}

func TestTheFilesSay(t *testing.T) {
	cfg := &guestv1.BootConfig{
		Hostname:   "api",
		Interfaces: []*guestv1.Interface{{Address: "10.0.0.5/24"}},
	}
	if got, want := hosts(cfg), "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n10.0.0.5\tapi\n"; got != want {
		t.Errorf("hosts %q, want %q", got, want)
	}
	if got, want := resolvConf([]string{"1.1.1.1", "8.8.8.8"}, []string{"web.hangar.internal"}),
		"nameserver 1.1.1.1\nnameserver 8.8.8.8\nsearch web.hangar.internal\n"; got != want {
		t.Errorf("resolv.conf %q, want %q", got, want)
	}
	if got := subtreeControl("cpuset cpu io memory pids\n"); got != "+cpuset +cpu +io +memory +pids" {
		t.Errorf("subtree_control %q", got)
	}
}

func TestOneDefaultRouteAtMost(t *testing.T) {
	err := Network([]*guestv1.Interface{
		{Mac: "02:00:00:00:00:01", Address: "10.0.0.5/24", Gateway: "10.0.0.1"},
		{Mac: "02:00:00:00:00:02", Address: "10.1.0.5/24", Gateway: "10.1.0.1"},
	})
	if err == nil || !strings.Contains(err.Error(), "default route") {
		t.Errorf("two default routes: %v", err)
	}
}

// TestMountsComeOffInsideOut: a mount is unmounted before what it was
// mounted on, and the root is left for remounting read-only.
func TestMountsComeOffInsideOut(t *testing.T) {
	info := `22 1 253:0 / / rw,relatime - ext4 /dev/vda rw
23 22 0:5 / /dev rw,nosuid - devtmpfs devtmpfs rw
24 23 0:20 / /dev/pts rw - devpts devpts rw
25 22 0:21 / /var/lib/my\040data rw - tmpfs tmpfs rw
`
	got := strings.Join(unmountOrder(info), ",")
	if want := "/var/lib/my data,/dev/pts,/dev"; got != want {
		t.Errorf("unmounting %s, want %s", got, want)
	}
}
