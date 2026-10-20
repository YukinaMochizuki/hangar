//go:build testbed

package guest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// newShareServer prepares a FileShare's server, the size the platform gives
// one, on a blank disk of its own; it is booted with the export given.
func newShareServer(t *testing.T) *vmtest.VM {
	t.Helper()
	v := vmtest.New(t, &guestv1.BootConfig{})
	v.Disks, v.CPUs, v.MemoryMB = []string{blankDisk(t, v, 2<<30)}, 1, 256
	return v
}

// blankDisk makes a sparse disk of size in v's directory.
func blankDisk(t *testing.T, v *vmtest.VM, size int64) string {
	t.Helper()
	disk := v.Path("data.img")
	if err := os.WriteFile(disk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(disk, size); err != nil {
		t.Fatal(err)
	}
	return disk
}

// TestAServerRefusesABadExport: a boot disk whose export nfsd would not
// take boots no server, and says why.
func TestAServerRefusesABadExport(t *testing.T) {
	srv := newShareServer(t)
	bad := export(squashAll)
	bad.AnonGid = 4294967295
	srv.SetBootConfig(&guestv1.BootConfig{FileShareServer: &guestv1.FileShareServer{Export: bad}})
	srv.Start()
	srv.WaitPowerOff(60 * time.Second)
	if _, exit := outputAndExit(t, srv); exit != (console.ExitStatus{Code: 255}) {
		t.Errorf("exit %+v, want 255", exit)
	}
	if msg := initMessages(t, srv); !strings.Contains(msg, "the boot disk's export") || !strings.Contains(msg, "4294967295") {
		t.Errorf("hangar-init does not say why: %q", msg)
	}
}

func bootShareServer(t *testing.T, srv *vmtest.VM, e *guestv1.FileShareExport) {
	t.Helper()
	srv.SetBootConfig(&guestv1.BootConfig{FileShareServer: &guestv1.FileShareServer{Export: e}})
	srv.Start()
	srv.Ready(60 * time.Second)
}

func export(squash guestv1.FileShareSquash, cs ...*guestv1.FileShareClient) *guestv1.FileShareExport {
	return &guestv1.FileShareExport{Clients: cs, Squash: squash, AnonUid: 65534, AnonGid: 65534}
}

func client(v *vmtest.VM, readOnly bool) *guestv1.FileShareClient {
	return &guestv1.FileShareClient{Address: v.Addr(), ReadOnly: readOnly}
}

// setExport replaces the server's export as the node will, through its agent.
func setExport(t *testing.T, srv *vmtest.VM, e *guestv1.FileShareExport) {
	t.Helper()
	if err := trySetExport(t, srv, e); err != nil {
		t.Fatalf("SetExport: %v", err)
	}
}

func trySetExport(t *testing.T, srv *vmtest.VM, e *guestv1.FileShareExport) error {
	t.Helper()
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: protocols, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return chguest.Dial(ctx, srv.Path("vsock"), agent.Port)
	}}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c := guestv1connect.NewFileShareServerServiceClient(&http.Client{Transport: tr}, "http://guest")
	_, err := c.SetExport(ctx, connect.NewRequest(&guestv1.SetExportRequest{Export: e}))
	return err
}

// shareClient boots a container that mounts srv's share as vols without a
// device say, and its disks as the others do, with main as its process.
func shareClient(t *testing.T, v *vmtest.VM, srv *vmtest.VM, main string, vols ...*guestv1.Volume) {
	t.Helper()
	for _, vol := range vols {
		if vol.GetDevice() == "" {
			vol.FileShare = &guestv1.FileShareMount{Address: srv.Addr()}
		}
	}
	cfg := process("sh", "-c", main)
	cfg.MachineUid = fmt.Sprintf("tb-machine-%s", v.Addr())
	cfg.Volumes = vols
	v.SetBootConfig(cfg)
	v.Start()
	v.Ready(60 * time.Second)
}

// sh runs script in v as user, returning its output and exit code.
func sh(t *testing.T, v *vmtest.VM, user, script string) (string, int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	res, err := v.Exec(ctx, []string{"sh", "-c", script}, vmtest.ExecOpts{User: user})
	if err != nil {
		t.Fatalf("exec %q: %v", script, err)
	}
	return string(res.Stdout) + string(res.Stderr), res.Exit.GetCode()
}

func mustSh(t *testing.T, v *vmtest.VM, user, script string) string {
	t.Helper()
	out, code := sh(t, v, user, script)
	if code != 0 {
		t.Fatalf("%q as %q: exit %d: %s", script, user, code, out)
	}
	return out
}

const idle = "exec sleep 3600"

// TestAFileShare: a server and the container guests it exports to, which
// read each other's writes; the export, changed while they run, decides who
// reaches the share and how, whatever a client does with its mount.
func TestAFileShare(t *testing.T) {
	srv := newShareServer(t)
	a := vmtest.New(t, &guestv1.BootConfig{})
	b := vmtest.New(t, &guestv1.BootConfig{})
	c := vmtest.New(t, &guestv1.BootConfig{})
	none := export(squashNone, client(a, false), client(b, false), client(c, true))
	bootShareServer(t, srv, none)

	// a holds a lock from before b mounts: b's mount must not take a's
	// state, as it would were they the same NFS client.
	shareClient(t, a, srv, "mkdir -p /data/sub && exec 9>/data/sub/lock && flock 9 && touch /data/sub/locked && "+idle,
		&guestv1.Volume{Name: "files", Target: "/data"})
	testutil.EventuallyWithin(t, time.Minute, "a holds its lock", func() bool {
		_, code := sh(t, a, "", "test -e /data/sub/locked")
		return code == 0
	})
	// b's target holds the image's own files, which never reach the share.
	shareClient(t, b, srv, idle, &guestv1.Volume{Name: "files", Target: "/usr/local/bin", Subpath: "sub"})
	// c also has a read-only Disk volume, whose read-only mount comes the
	// same way.
	c.Disks = []string{blankDisk(t, c, 64<<20)}
	shareClient(t, c, srv, idle, &guestv1.Volume{Name: "files", Target: "/data", ReadOnly: true},
		&guestv1.Volume{Name: "scratch", Device: "/dev/vdc", Target: "/disk", ReadOnly: true})

	t.Run("each reads the other's writes", func(t *testing.T) {
		mustSh(t, a, "", "echo from-a > /data/sub/a.txt")
		if got := mustSh(t, b, "", "cat /usr/local/bin/a.txt"); got != "from-a\n" {
			t.Errorf("b read %q", got)
		}
		mustSh(t, b, "", "echo from-b > /usr/local/bin/b.txt")
		if got := mustSh(t, a, "", "cat /data/sub/b.txt"); got != "from-b\n" {
			t.Errorf("a read %q", got)
		}
		if got := mustSh(t, c, "", "cat /data/sub/b.txt"); got != "from-b\n" {
			t.Errorf("c read %q", got)
		}
	})

	t.Run("nothing but what clients wrote", func(t *testing.T) {
		if got := mustSh(t, a, "", "ls -A /data; echo; ls -A /data/sub"); got != "sub\n\na.txt\nb.txt\nlock\nlocked\n" {
			t.Errorf("the share holds:\n%s", got)
		}
	})

	t.Run("each client is its own", func(t *testing.T) {
		for _, v := range []*vmtest.VM{a, b} {
			if got := strings.TrimSpace(mustSh(t, v, "", "cat /sys/fs/nfs/net/nfs_client/identifier")); got != "tb-machine-"+v.Addr() {
				t.Errorf("%s's NFS client identity is %q", v.Addr(), got)
			}
		}
		mustSh(t, b, "", "touch /usr/local/bin/free")
		if held, out := heldElsewhere(t, b, "/usr/local/bin/free"); held {
			t.Fatalf("b cannot lock a file nobody holds: %s", out)
		}
		if held, out := heldElsewhere(t, b, "/usr/local/bin/lock"); !held {
			t.Errorf("b took the lock a holds: %s", out)
		}
	})

	t.Run("mounted as the design says", func(t *testing.T) {
		line := mustSh(t, a, "", "grep ' /data ' /proc/mounts")
		for _, want := range []string{"nfs4", "vers=4.2", "rsize=1048576", "wsize=1048576", "hard", "timeo=50", "nosuid", "nodev", "proto=tcp", "addr=" + srv.Addr()} {
			if !strings.Contains(line, want) {
				t.Errorf("a's /data lacks %s: %s", want, line)
			}
		}
	})

	t.Run("only NFSv4.2", func(t *testing.T) {
		for _, vers := range []string{"4.0", "4.1", "3"} {
			typ := "nfs4"
			if vers == "3" {
				typ = "nfs"
			}
			if out, code := sh(t, a, "", fmt.Sprintf("mkdir -p /m && timeout 30 mount -t %s -o vers=%s,addr=%s,nolock,soft,retrans=1 %s:/ /m 2>&1", typ, vers, srv.Addr(), srv.Addr())); code == 0 {
				t.Errorf("an NFS %s mount was served: %s", vers, out)
				sh(t, a, "", "umount /m")
			}
		}
		mustSh(t, a, "", fmt.Sprintf("mount -t nfs4 -o vers=4.2,addr=%s %s:/ /m && test -e /m/sub/lock && umount /m", srv.Addr(), srv.Addr()))
	})

	t.Run("read-only holds after a remount read-write", func(t *testing.T) {
		// Read-only, and still nosuid and nodev.
		for target, want := range map[string][]string{"/data": {"ro", "nosuid", "nodev"}, "/disk": {"ro"}} {
			line := mustSh(t, c, "", "grep ' "+target+" ' /proc/mounts")
			opts := strings.Split(strings.Fields(line)[3], ",")
			for _, o := range want {
				if !slices.Contains(opts, o) {
					t.Errorf("c's %s is not %s: %s", target, o, line)
				}
			}
		}
		if out, code := sh(t, c, "", "touch /disk/c.txt 2>&1"); code == 0 || !strings.Contains(out, "Read-only") {
			t.Errorf("c wrote to a Disk volume it reads only (exit %d): %s", code, out)
		}
		if out, code := sh(t, c, "", "touch /data/c.txt 2>&1"); code == 0 || !strings.Contains(out, "Read-only") {
			t.Errorf("c wrote to a share it reads only (exit %d): %s", code, out)
		}
		mustSh(t, c, "", "mount -o remount,rw /data && mount -o remount,bind,rw /data && grep ' /data ' /proc/mounts | grep -q ' rw,'")
		if out, code := sh(t, c, "", "touch /data/c.txt 2>&1"); code == 0 || !strings.Contains(out, "Read-only") {
			t.Errorf("c wrote to a share it reads only, remounted read-write (exit %d): %s", code, out)
		}
	})

	t.Run("a client's own groups count", func(t *testing.T) {
		mustSh(t, a, "", "addgroup -g 4321 crew && adduser -D -u 1001 alice && addgroup alice crew && mkdir /data/crew && chown 0:4321 /data/crew && chmod 0770 /data/crew")
		mustSh(t, a, "alice", "timeout 30 touch /data/crew/alice.txt")
	})

	t.Run("squash", func(t *testing.T) {
		mustSh(t, a, "", "mkdir /data/squash && chmod 1777 /data/squash")
		// Files of root and of uid 1000, new in each mode: a squashed root
		// may not remove root's in a sticky directory.
		owners := func(mode string) string {
			mustSh(t, a, "", "touch /data/squash/root-"+mode)
			mustSh(t, a, "1000:1000", "touch /data/squash/user-"+mode)
			return strings.TrimSpace(mustSh(t, a, "", fmt.Sprintf("cd /data/squash && stat -c %%u:%%g root-%s user-%s | tr '\\n' ' '", mode, mode)))
		}
		if got := owners("none"); got != "0:0 1000:1000" {
			t.Errorf("None: files are %s", got)
		}
		setExport(t, srv, export(squashRoot, client(a, false), client(b, false), client(c, true)))
		if got := owners("root"); got != "65534:65534 1000:1000" {
			t.Errorf("Root: files are %s", got)
		}
		all := export(squashAll, client(a, false), client(b, false), client(c, true))
		all.AnonUid, all.AnonGid = 1234, 2345
		setExport(t, srv, all)
		if got := owners("all"); got != "1234:2345 1234:2345" {
			t.Errorf("All: files are %s", got)
		}
		setExport(t, srv, none)
	})

	t.Run("a bad export is refused, the last kept", func(t *testing.T) {
		bad := export(squashAll, client(a, false), client(b, false), client(c, true))
		bad.AnonUid = 4294967295
		if err := trySetExport(t, srv, bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("an anonymous uid of (uid_t)-1: %v, want InvalidArgument", err)
		}
		if err := trySetExport(t, srv, export(squashNone, client(a, false), &guestv1.FileShareClient{Address: "0.0.0.0"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a client 0.0.0.0: %v, want InvalidArgument", err)
		}
		// c, which the bad exports leave out, still reads.
		mustSh(t, c, "", "timeout 10 cat /data/sub/a.txt")
		mustSh(t, a, "", "timeout 10 touch /data/after-bad")
	})

	t.Run("a removed client loses the share", func(t *testing.T) {
		setExport(t, srv, export(squashNone, client(a, false), client(b, false)))
		testutil.EventuallyWithin(t, 10*time.Second, "c refused", func() (bool, string) {
			out, code := sh(t, c, "", "timeout 5 ls /data/sub 2>&1")
			return code != 0, out
		})
		mustSh(t, a, "", "ls /data/sub")
		setExport(t, srv, none)
		testutil.EventuallyWithin(t, 10*time.Second, "c let back in", func() (bool, string) {
			out, code := sh(t, c, "", "timeout 5 ls /data/sub 2>&1")
			return code == 0, out
		})
	})

	t.Run("a subpath through a planted symlink stays in the share", func(t *testing.T) {
		mustSh(t, a, "", "ln -sfn / /data/sub/escape")
		d := vmtest.New(t, &guestv1.BootConfig{})
		setExport(t, srv, export(squashNone, client(a, false), client(b, false), client(c, true), client(d, false)))
		defer setExport(t, srv, none)
		shareClient(t, d, srv, idle, &guestv1.Volume{Name: "files", Target: "/data", Subpath: "sub/escape"})
		// The share's root, not the initramfs's.
		if got := mustSh(t, d, "", "ls -A /data"); !strings.Contains(got, "sub\n") || strings.Contains(got, "newroot") {
			t.Errorf("a subpath through a symlink to / mounted:\n%s", got)
		}
	})

	t.Run("a planted link never leads where the platform mounts its own", func(t *testing.T) {
		mustSh(t, a, "", "mkdir -p /data/planted/etc /data/clean && ln -sfn / /data/planted/toroot && ln -sfn /proc/sys/kernel/hostname /data/planted/etc/hosts")
		for name, tt := range map[string]struct {
			vols []*guestv1.Volume
			why  string
		}{
			// The inner target, in the outer share, leads to /.
			"a nested target": {[]*guestv1.Volume{
				{Name: "outer", Target: "/mnt/outer"},
				{Name: "inner", Target: "/mnt/outer/planted/toroot", Subpath: "clean"},
			}, "volume inner at /mnt/outer/planted/toroot: it leads to /, where the platform mounts its own"},
			// /etc/hosts, in the share, leads into the guest's procfs.
			"an identity file": {[]*guestv1.Volume{{Name: "etc", Target: "/etc", Subpath: "planted/etc"}},
				"/etc/hosts: it leads to /proc/sys/kernel/hostname, where the platform mounts its own"},
		} {
			vols := tt.vols
			t.Run(name, func(t *testing.T) {
				v := vmtest.New(t, &guestv1.BootConfig{})
				setExport(t, srv, export(squashNone, client(a, false), client(b, false), client(c, true), client(v, false)))
				defer setExport(t, srv, none)
				for _, vol := range vols {
					vol.FileShare = &guestv1.FileShareMount{Address: srv.Addr()}
				}
				cfg := process("sh", "-c", "echo ran")
				cfg.MachineUid, cfg.Volumes = "tb-planted-"+v.Addr(), vols
				v.SetBootConfig(cfg)
				v.Start()
				v.WaitPowerOff(2 * time.Minute)
				if segs, exit := outputAndExit(t, v); len(segs) != 0 || exit != (console.ExitStatus{Code: 255}) {
					t.Errorf("the guest ran (%q), exit %+v", segs, exit)
				}
				if msg := initMessages(t, v); !strings.Contains(msg, tt.why) {
					t.Errorf("hangar-init does not say %q: %q", tt.why, msg)
				}
			})
		}
	})

	t.Run("a share that cannot be mounted fails the boot", func(t *testing.T) {
		for name, vol := range map[string]*guestv1.Volume{
			"not exported to it":  {Name: "files", Target: "/data"},
			"a subpath not in it": {Name: "files", Target: "/data", Subpath: "missing"},
		} {
			t.Run(name, func(t *testing.T) {
				v := vmtest.New(t, &guestv1.BootConfig{})
				if name != "not exported to it" {
					setExport(t, srv, export(squashNone, client(a, false), client(b, false), client(c, true), client(v, false)))
					defer setExport(t, srv, none)
				}
				vol.FileShare = &guestv1.FileShareMount{Address: srv.Addr()}
				cfg := process("sh", "-c", "echo ran")
				cfg.MachineUid, cfg.Volumes = "tb-refused", []*guestv1.Volume{vol}
				v.SetBootConfig(cfg)
				v.Start()
				v.WaitPowerOff(2 * time.Minute)
				segs, exit := outputAndExit(t, v)
				if len(segs) != 0 || exit != (console.ExitStatus{Code: 255}) {
					t.Errorf("the guest ran (%q), exit %+v", segs, exit)
				}
				if msg := initMessages(t, v); !strings.Contains(msg, "volume files at /data") {
					t.Errorf("hangar-init does not say which share: %q", msg)
				}
			})
		}
	})
}

var (
	squashNone = guestv1.FileShareSquash_FILE_SHARE_SQUASH_NONE
	squashRoot = guestv1.FileShareSquash_FILE_SHARE_SQUASH_ROOT
	squashAll  = guestv1.FileShareSquash_FILE_SHARE_SQUASH_ALL
)

// maxPauseAllowed bounds how long a restart of the server may hold a
// writer up: seconds, where nfsd's defaults stall one for a minute or more.
// Why not less: docs/runbook/testbed.md.
const maxPauseAllowed = 30 * time.Second

// maxGraceWait bounds how long a client gone for good holds the others up
// after a restart: the 15 s grace period, where nfsd's default is 90 s,
// and the client's backoff between retries, which grows to 15 s.
const maxGraceWait = 45 * time.Second

// TestAServerRestartPausesClientsSeconds: a writer pauses for seconds while
// its share's server restarts, stopped or killed, and its lock is kept:
// it reclaims it, and nobody else takes it. A client gone for good holds
// the others up for the grace period alone, its lock then free.
func TestAServerRestartPausesClientsSeconds(t *testing.T) {
	srv := newShareServer(t)
	w := vmtest.New(t, &guestv1.BootConfig{})
	o := vmtest.New(t, &guestv1.BootConfig{})
	z := vmtest.New(t, &guestv1.BootConfig{})
	bootShareServer(t, srv, export(squashNone, client(w, false), client(o, false), client(z, false)))
	// The writer appends its uptime five times a second, holding a lock.
	shareClient(t, w, srv, `exec 9>/data/lock && flock 9 && while :; do cut -d' ' -f1 /proc/uptime >> /data/w; sleep 0.2; done`,
		&guestv1.Volume{Name: "files", Target: "/data"})
	shareClient(t, z, srv, "exec 9>/data/zlock && flock 9 && touch /data/zlocked && "+idle, &guestv1.Volume{Name: "files", Target: "/data"})
	shareClient(t, o, srv, idle, &guestv1.Volume{Name: "files", Target: "/data"})

	lines := func() []float64 {
		var out []float64
		for _, f := range strings.Fields(mustSh(t, o, "", "cat /data/w")) {
			if x, err := strconv.ParseFloat(f, 64); err == nil {
				out = append(out, x)
			}
		}
		return out
	}
	testutil.EventuallyWithin(t, time.Minute, "the writer writing, z holding its lock", func() bool {
		_, code := sh(t, o, "", "test -e /data/zlocked")
		return len(lines()) > 5 && code == 0
	})
	restart := func(t *testing.T, kill bool) {
		t.Helper()
		if kill {
			srv.Kill()
		} else {
			stopServer(t, srv)
			if _, exit := outputAndExit(t, srv); exit != (console.ExitStatus{}) {
				t.Errorf("the server stopped with %+v", exit)
			}
		}
		srv.Start()
		srv.Ready(60 * time.Second)
	}

	for _, how := range []string{"stopped", "killed"} {
		t.Run(how, func(t *testing.T) {
			before := len(lines())
			restart(t, how == "killed")
			// A new open waits out the grace period, after which a lock not
			// reclaimed would be free.
			mustSh(t, o, "", "timeout 60 touch /data/after-"+how)
			if held, out := heldElsewhere(t, o, "/data/after-"+how); held {
				t.Fatalf("o cannot lock a file nobody holds: %s", out)
			}
			for _, f := range []string{"/data/lock", "/data/zlock"} {
				if held, out := heldElsewhere(t, o, f); !held {
					t.Errorf("%s was free after the restart: %s", f, out)
				}
			}
			testutil.EventuallyWithin(t, time.Minute, "the writer writing again", func() bool { return len(lines()) > before+10 })
			// From the last line before the restart on.
			ls := lines()[before-1:]
			var worst float64
			for i := 1; i < len(ls); i++ {
				worst = max(worst, ls[i]-ls[i-1])
			}
			pause := time.Duration(worst * float64(time.Second))
			t.Logf("the writer's longest pause: %v", pause)
			if pause > maxPauseAllowed {
				t.Errorf("the writer paused %v", pause)
			}
		})
	}

	t.Run("a client gone for good", func(t *testing.T) {
		z.Kill()
		restart(t, false)
		start := time.Now()
		mustSh(t, o, "", "timeout 150 touch /data/after-z")
		waited := time.Since(start)
		t.Logf("a new file waited %v", waited)
		if waited > maxGraceWait {
			t.Errorf("a new file waited %v for a client gone for good", waited)
		}
		if held, out := heldElsewhere(t, o, "/data/zlock"); held {
			t.Errorf("the lock of a client gone for good is held: %s", out)
		}
	})

	t.Run("client records it cannot read", func(t *testing.T) {
		garbage := []byte("HNC9 written by a later version")
		stopServer(t, srv)
		onDisk(t, srv.Disks[0], func(dir string) {
			if err := unix.Setxattr(dir, "trusted.hangar.nfsd-clients", garbage, 0); err != nil {
				t.Fatal(err)
			}
		})
		srv.Start()
		srv.Ready(60 * time.Second)
		mustSh(t, o, "", "timeout 150 touch /data/after-bad-records")
		if msg := initMessages(t, srv); !strings.Contains(msg, "set aside") {
			t.Errorf("hangar-init does not say it set the records aside: %q", msg)
		}
		stopServer(t, srv)
		onDisk(t, srv.Disks[0], func(dir string) {
			buf := make([]byte, 256)
			n, err := unix.Getxattr(dir, "trusted.hangar.nfsd-clients.bad", buf)
			if err != nil || string(buf[:n]) != string(garbage) {
				t.Errorf("the records set aside: %q (%v)", buf[:max(n, 0)], err)
			}
			if n, err := unix.Getxattr(dir, "trusted.hangar.nfsd-clients", buf); err != nil || bytes.Equal(buf[:n], garbage) {
				t.Errorf("the records the server keeps: %q (%v)", buf[:max(n, 0)], err)
			}
		})
	})
}

// stopServer stops a server as the node does, and waits for it to be off.
func stopServer(t *testing.T, srv *vmtest.VM) {
	t.Helper()
	if err := srv.Guest.Stop(t.Context(), "", 0); err != nil {
		t.Fatal(err)
	}
	srv.WaitPowerOff(30 * time.Second)
}

// onDisk runs f on the filesystem of a server's disk, mounted on the host
// while the server is off.
func onDisk(t *testing.T, img string, f func(dir string)) {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("mount", "-o", "loop", img, dir).CombinedOutput(); err != nil {
		t.Fatalf("mounting the share's disk: %v: %s", err, out)
	}
	defer func() {
		if out, err := exec.Command("umount", dir).CombinedOutput(); err != nil {
			t.Errorf("unmounting the share's disk: %v: %s", err, out)
		}
	}()
	f(dir)
}

// heldElsewhere reports whether another client holds a lock on path, by
// asking for a shared one: flock opens the file read-only, and the NFS
// client refuses an exclusive lock on a file open read-only.
func heldElsewhere(t *testing.T, v *vmtest.VM, path string) (bool, string) {
	t.Helper()
	out, code := sh(t, v, "", "flock -n -s "+path+" true 2>&1")
	return code != 0, out
}
