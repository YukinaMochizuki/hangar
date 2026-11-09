package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

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
	// What another client of a share over /etc could plant.
	outside := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "proc/sys"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "proc/sys/x"), nil, 0o644)
	_ = os.Symlink("/proc/sys/x", filepath.Join(etc, "toproc"))
	_ = os.Symlink("/proc/missing", filepath.Join(etc, "toproc-missing"))
	_ = os.Symlink(outside+"/f", filepath.Join(etc, "out"))
	_ = os.Symlink("/etc", filepath.Join(etc, "dir"))
	rootfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(rootfd) }()

	for _, tt := range []struct {
		path, where string
		write       bool
	}{
		{"/etc/hosts", "/etc/hosts", false},
		{"/etc/resolv.conf", "/run/systemd/resolve/stub-resolv.conf", true},
		{"/etc/hostname", "/lib/net/hostname", false},
		{"/etc/absent", "/etc/absent", false},
		{"/etc/resolvconf", "/run/resolvconf/resolv.conf", true},
		{"/etc/climbing", "/etc/hosts", false}, // .. stops at the root
	} {
		got, err := placement(root, rootfd, tt.path)
		if err != nil {
			t.Errorf("%s: %v", tt.path, err)
			continue
		}
		_ = unix.Close(got.fd)
		if got.where != tt.where || got.write != tt.write {
			t.Errorf("%s: %s (write %t), want %s (write %t)", tt.path, got.where, got.write, tt.where, tt.write)
		}
	}
	for _, path := range []string{"/etc/dangling", "/etc/loop", "/etc/toproc", "/etc/toproc-missing", "/etc/out", "/etc/dir"} {
		if got, err := placement(root, rootfd, path); err == nil {
			_ = unix.Close(got.fd)
			t.Errorf("%s was taken, to %s", path, got.where)
		}
	}
	if fi, err := os.Stat(filepath.Join(etc, "hosts")); err != nil || fi.Size() != int64(len("image's")) {
		t.Error("deciding where to put a file wrote to the image")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a file was made outside the root: %v", entries)
	}
	if _, err := os.Lstat(filepath.Join(root, "proc/missing")); err == nil {
		t.Error("a file was made in the image's /proc")
	}
}

func TestTheFilesSay(t *testing.T) {
	cfg := &guestv1.BootConfig{
		Hostname:   "api",
		Interfaces: []*guestv1.Interface{{Address: "10.0.0.5/24", Ipv6: "2001:db8::5/128"}},
	}
	if got, want := hosts(cfg), "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n10.0.0.5\tapi\n2001:db8::5\tapi\n"; got != want {
		t.Errorf("hosts %q, want %q", got, want)
	}
	if got, want := resolvConf([]string{"192.0.2.53", "198.51.100.53"}, []string{"web.hangar.internal"}),
		"nameserver 192.0.2.53\nnameserver 198.51.100.53\nsearch web.hangar.internal\n"; got != want {
		t.Errorf("resolv.conf %q, want %q", got, want)
	}
	if got := subtreeControl("cpuset cpu io memory pids\n"); got != "+cpuset +cpu +io +memory +pids" {
		t.Errorf("subtree_control %q", got)
	}
}

// TestResolversChangeInPlace: SetResolvers rewrites the platform's file in
// place, the inode the mount over the image's file shows, and the file
// under /run that Identity wrote instead of mounting; never an image's
// file.
func TestResolversChangeInPlace(t *testing.T) {
	for _, c := range []struct {
		name  string
		image func(etc string)
		// written is the file, in root, that /etc/resolv.conf shows once
		// Identity has run, other than the platform's own.
		written string
	}{
		{name: "mounted over the image's", image: func(etc string) {
			_ = os.WriteFile(filepath.Join(etc, "resolv.conf"), []byte("image's"), 0o644)
		}},
		{name: "a link into /run", image: func(etc string) {
			_ = os.Symlink("/run/systemd/resolve/stub-resolv.conf", filepath.Join(etc, "resolv.conf"))
		}, written: "run/systemd/resolve/stub-resolv.conf"},
	} {
		root := t.TempDir()
		etc := filepath.Join(root, "etc")
		for _, d := range []string{etc, filepath.Join(root, "run/hangar"), filepath.Join(root, "run/systemd/resolve")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		c.image(etc)
		source := filepath.Join(root, "run/hangar/resolv.conf")
		if err := os.WriteFile(source, []byte(resolvConf([]string{"10.0.0.1"}, []string{"web.hangar.internal"})), 0o644); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetResolvers(root, []string{"127.0.0.11"}, []string{"web.hangar.internal"}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := "nameserver 127.0.0.11\nsearch web.hangar.internal\n"
		after, err := os.Stat(source)
		if b, _ := os.ReadFile(source); string(b) != want || err != nil || !os.SameFile(before, after) {
			t.Errorf("%s: the platform's file says %q, the same inode %v; want %q in place", c.name, b, os.SameFile(before, after), want)
		}
		if c.written != "" {
			if b, _ := os.ReadFile(filepath.Join(root, c.written)); string(b) != want {
				t.Errorf("%s: %s says %q, want %q", c.name, c.written, b, want)
			}
		} else if b, _ := os.ReadFile(filepath.Join(etc, "resolv.conf")); string(b) != "image's" {
			t.Errorf("%s: the image's file was written: %q", c.name, b)
		}
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
	err = Network([]*guestv1.Interface{
		{Mac: "02:00:00:00:00:01", Address: "10.0.0.5/24", Ipv6: "2001:db8::5/128", Ipv6Gateway: "fe80::1"},
		{Mac: "02:00:00:00:00:02", Address: "10.1.0.5/24", Ipv6: "2001:db8::6/128", Ipv6Gateway: "fe80::1"},
	})
	if err == nil || !strings.Contains(err.Error(), "IPv6 default route") {
		t.Errorf("two IPv6 default routes: %v", err)
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
