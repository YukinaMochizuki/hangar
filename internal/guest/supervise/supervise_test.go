package supervise_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

func sh(t *testing.T, r *supervise.Reaper, script string) *supervise.Process {
	t.Helper()
	p, err := r.Start([]string{"/bin/sh", "-c", script}, &os.ProcAttr{Files: []*os.File{nil, os.Stdout, os.Stderr}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExitsReachTheirWaiters(t *testing.T) {
	r := supervise.NewReaper()
	defer r.Stop()
	if e := sh(t, r, "exit 3").Wait(); e != (supervise.Exit{Code: 3}) {
		t.Errorf("exit %+v", e)
	}
	p := sh(t, r, "sleep 10")
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if e := p.Wait(); e != (supervise.Exit{Code: 143, Signal: 15}) {
		t.Errorf("signalled %+v", e)
	}
	if e := p.Wait(); e.Signal != 15 {
		t.Error("a second Wait lost the exit")
	}
}

// TestNoExitIsLost: processes that end at once, many at a time, still
// reach their waiters.
func TestNoExitIsLost(t *testing.T) {
	r := supervise.NewReaper()
	defer r.Stop()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := r.Start([]string{"/bin/true"}, &os.ProcAttr{})
			if err != nil {
				t.Error(err)
				return
			}
			done := make(chan supervise.Exit, 1)
			go func() { done <- p.Wait() }()
			select {
			case e := <-done:
				if e.Code != 0 {
					t.Errorf("process %d: %+v", i, e)
				}
			case <-time.After(10 * time.Second):
				t.Errorf("process %d's exit was lost", i)
			}
		}()
	}
	wg.Wait()
}

// TestOrphansAreReaped: as PID 1 is, the reaper is the parent of orphans,
// and leaves none of them a zombie.
func TestOrphansAreReaped(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skip("cannot become a subreaper:", err)
	}
	defer func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) }()
	r := supervise.NewReaper()
	defer r.Stop()
	if e := sh(t, r, "sleep 0.2 & exit 0").Wait(); e.Code != 0 {
		t.Fatal(e)
	}
	if zombies(t)+children(t) == 0 {
		t.Fatal("the orphan was never this process's child")
	}
	deadline := time.Now().Add(5 * time.Second)
	for children(t) > 0 || zombies(t) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d zombies left", zombies(t))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// children counts this process's live children.
func children(t *testing.T) int {
	t.Helper()
	return countChildren(t, func(state string) bool { return state != "Z" })
}

// zombies counts this process's children that are zombies.
func zombies(t *testing.T) int {
	t.Helper()
	return countChildren(t, func(state string) bool { return state == "Z" })
}

func countChildren(t *testing.T, match func(state string) bool) int {
	t.Helper()
	n := 0
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...
		rest := stat[bytes.LastIndexByte(stat, ')')+2:]
		f := strings.Fields(string(rest))
		if len(f) > 1 && match(f[0]) && f[1] == strconv.Itoa(os.Getpid()) {
			n++
		}
	}
	return n
}

func root(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("etc/passwd", "root:x:0:0:root:/root:/bin/sh\nnginx:x:101:101:nginx:/var/cache/nginx:/sbin/nologin\napp:x:1000:1000::/home/app:/bin/sh\n")
	must("etc/group", "root:x:0:\nnginx:x:101:\napp:x:1000:\ndocker:x:999:app,other\n")
	return dir
}

func TestUsersAreResolvedAsInDocker(t *testing.T) {
	dir := root(t)
	for _, tt := range []struct {
		spec string
		want supervise.User
	}{
		{"", supervise.User{UID: 0, GID: 0, Groups: []uint32{0}, Home: "/root"}},
		{"nginx", supervise.User{UID: 101, GID: 101, Groups: []uint32{101}, Home: "/var/cache/nginx"}},
		{"app", supervise.User{UID: 1000, GID: 1000, Groups: []uint32{1000, 999}, Home: "/home/app"}},
		{"1000", supervise.User{UID: 1000, GID: 1000, Groups: []uint32{1000, 999}, Home: "/home/app"}},
		{"1000:101", supervise.User{UID: 1000, GID: 101, Groups: []uint32{101, 999}, Home: "/home/app"}},
		{"app:nginx", supervise.User{UID: 1000, GID: 101, Groups: []uint32{101, 999}, Home: "/home/app"}},
		{"4242", supervise.User{UID: 4242, GID: 0, Groups: []uint32{0}, Home: "/"}},
	} {
		got, err := supervise.ResolveUser(dir, tt.spec)
		if err != nil {
			t.Errorf("%q: %v", tt.spec, err)
			continue
		}
		if got.UID != tt.want.UID || got.GID != tt.want.GID || got.Home != tt.want.Home || !equal(got.Groups, tt.want.Groups) {
			t.Errorf("%q: %+v, want %+v", tt.spec, got, tt.want)
		}
	}
	for _, spec := range []string{"nobody-here", "app:nogroup"} {
		if _, err := supervise.ResolveUser(dir, spec); err == nil {
			t.Errorf("%q resolved", spec)
		}
	}
}

func equal(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAProcessRunsAsSpecified: user, environment, working directory and the
// program found on PATH, as the container asks.
func TestAProcessRunsAsSpecified(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching users needs root; runs in boxes and in the guest")
	}
	r := supervise.NewReaper()
	defer r.Stop()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	argv, attr, err := supervise.Spec("/", &guestv1.Process{
		Args:       []string{"sh", "-c", `echo "$(id -u):$(id -g) $HOME $MODE $(pwd) $PATH"`},
		Env:        []string{"MODE=prod"},
		WorkingDir: "/tmp",
		User:       "nobody",
	}, [3]*os.File{nil, out, out})
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/sh" && argv[0] != "/bin/sh" {
		t.Errorf("program %s", argv[0])
	}
	p, err := r.Start(argv, attr)
	if err != nil {
		t.Fatal(err)
	}
	if e := p.Wait(); e.Code != 0 {
		t.Fatalf("exit %+v", e)
	}
	got, _ := os.ReadFile(out.Name())
	want := "65534:65534 /nonexistent prod /tmp " + supervise.DefaultPath + "\n"
	if string(got) != want {
		t.Errorf("ran as %q, want %q", got, want)
	}
}

// TestPathSkipsWhatCannotRun: a file on PATH that is not executable is
// passed over for the next directory's, as a shell does.
func TestPathSkipsWhatCannotRun(t *testing.T) {
	dir := t.TempDir()
	for name, mode := range map[string]os.FileMode{"usr/local/bin/tool": 0o644, "usr/bin/tool": 0o755} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	got, err := supervise.LookPath(dir, "tool", "/usr/local/bin:/usr/bin")
	if err != nil || got != "/usr/bin/tool" {
		t.Errorf("found %q, %v", got, err)
	}
}

func TestAMissingProgramSaysSo(t *testing.T) {
	_, _, err := supervise.Spec("/", &guestv1.Process{Args: []string{"no-such-program"}}, [3]*os.File{})
	if !errors.Is(err, supervise.ErrNotFound) || !strings.Contains(err.Error(), "no-such-program") {
		t.Errorf("err %v", err)
	}
	if _, _, err := supervise.Spec("/", &guestv1.Process{Args: []string{"sh"}, User: "no-such-user"}, [3]*os.File{}); err == nil || errors.Is(err, supervise.ErrNotFound) {
		t.Errorf("an unknown user: %v", err)
	}
}

// TestAMissingWorkingDirectory: an exec into one fails as such, not as a
// program not found; a container's process has it made, as in Docker.
func TestAMissingWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin/app"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &guestv1.Process{Args: []string{"/bin/app"}, WorkingDir: "/srv/app", User: "0"}
	_, _, err := supervise.Spec(root, p, [3]*os.File{})
	if err == nil || errors.Is(err, supervise.ErrNotFound) || !strings.Contains(err.Error(), "/srv/app") {
		t.Errorf("a missing working directory: %v", err)
	}
	if err := supervise.MakeWorkingDir(root, p); err != nil {
		t.Fatal(err)
	}
	if _, attr, err := supervise.Spec(root, p, [3]*os.File{}); err != nil || attr.Dir != "/srv/app" {
		t.Errorf("once made: %v", err)
	}
}
