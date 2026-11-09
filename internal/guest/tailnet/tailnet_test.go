package tailnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const testKey = "tskey-auth-kEXAMPLE1CNTRL-EXAMPLEsecretVALUE"

func TestHasNodeState(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, content string
		want, err     bool
	}{
		{name: "missing"},
		// tailscaled writes its machine key at its first start, before any
		// login: no node yet.
		{name: "fresh", content: `{"_machinekey":"cHJpdmtleQ=="}`},
		{name: "logged in", content: `{"_machinekey":"cHJpdmtleQ==","_current-profile":"YWJjZA==","profile-abcd":"e30="}`, want: true},
		{name: "logged out", content: `{"_machinekey":"cHJpdmtleQ==","_current-profile":""}`},
		{name: "garbage", content: "not json", err: true},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"))
		if c.content != "" {
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := HasNodeState(path)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s: %v, %v; want %v, error %v", c.name, got, err, c.want, c.err)
		}
	}
}

// TestCommandLines: tailscale up, set, the CLI and tailscaled get exactly
// these arguments: never --accept-dns=true, --accept-routes only when
// asked, the key only as a file on the CLI's pipe, and tailscaled's logs
// kept from Tailscale.
func TestCommandLines(t *testing.T) {
	for _, c := range []struct {
		cfg     Config
		withKey bool
		want    []string
	}{
		{Config{Hostname: "box"}, false, []string{"up", "--reset", "--hostname=box", "--accept-dns=false", "--timeout=1m0s"}},
		{Config{Hostname: "box", AcceptRoutes: true}, false, []string{"up", "--reset", "--hostname=box", "--accept-dns=false", "--timeout=1m0s", "--accept-routes"}},
		{Config{Hostname: "box"}, true, []string{"up", "--reset", "--hostname=box", "--accept-dns=false", "--timeout=1m0s", "--auth-key=file:/proc/self/fd/3"}},
	} {
		if got := UpArgs(c.cfg, c.withKey); !slices.Equal(got, c.want) {
			t.Errorf("up %+v, key %v: %q, want %q", c.cfg, c.withKey, got, c.want)
		}
	}
	if got, want := SetArgs(Config{Hostname: "dev", AcceptRoutes: false}), []string{"set", "--hostname=dev", "--accept-routes=false"}; !slices.Equal(got, want) {
		t.Errorf("set: %q, want %q", got, want)
	}
	if got, want := CLIArgs("/x/tailscale", []string{"logout"}), []string{"/x/tailscale", "--socket=" + Socket, "logout"}; !slices.Equal(got, want) {
		t.Errorf("the CLI: %q, want %q", got, want)
	}
	want := []string{BinDir + "/tailscaled", "--no-logs-no-support", "--tun=tailscale0", "--statedir=" + StateDir, "--state=" + StateDir + "/tailscaled.state", "--socket=" + Socket}
	if got := DaemonArgs(); !slices.Equal(got, want) {
		t.Errorf("tailscaled: %q, want %q", got, want)
	}
	if env := Env(); !slices.Contains(env, "TS_DEBUG_FIREWALL_MODE=nftables") || !slices.Contains(env, "TS_LOGS_DIR="+StateDir) {
		t.Errorf("tailscaled's environment %q picks no nftables, or keeps its logs elsewhere than its state", env)
	}
}

func TestWithPath(t *testing.T) {
	got := WithPath([]string{"A=1", "PATH=/usr/bin:/bin"})
	if !slices.Equal(got, []string{"A=1", "PATH=/usr/bin:/bin:" + BinDir}) {
		t.Errorf("with a PATH: %q", got)
	}
	if got := WithPath(nil); !slices.Equal(got, []string{"PATH=" + supervise.DefaultPath + ":" + BinDir}) {
		t.Errorf("with none: %q", got)
	}
	if got := WithPath(WithPath(nil)); strings.Count(got[0], BinDir) != 1 {
		t.Errorf("twice: %q", got)
	}
}

// reaper starts the tests' stand-ins for tailscaled; the package's tests
// share it, as one reaper at a time reaps a process's children.
var reaper *supervise.Reaper

func TestMain(m *testing.M) {
	reaper = supervise.NewReaper()
	os.Exit(m.Run())
}

// fakes is a Tailnet's world: a tailscaled that answers as api says, run
// as a sleep, a CLI whose runs record what they were given, and DNS and
// disk that record what was done to them.
type fakes struct {
	mu       sync.Mutex
	hasState bool
	runs     []cliRun
	// out and fail are what the next run of up says.
	out     string
	fail    bool
	noDisk  bool
	api     backend
	logs    []string
	daemons []*supervise.Process
	mounts  int
	removed int
	dns     []string
	// upGate, if set, holds each up: it takes one send as it begins and
	// another before it returns.
	upGate chan struct{}
}

type cliRun struct {
	args []string
	key  string
}

func (f *fakes) Status(context.Context) (backend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.api, nil
}

func (f *fakes) record(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dns = append(f.dns, what)
}

func (f *fakes) Split() error            { f.record("split"); return nil }
func (f *fakes) Restore() error          { f.record("restore"); return nil }
func (f *fakes) SetSuffix(suffix string) { f.record("suffix " + suffix) }

func newTestTailnet(t *testing.T, f *fakes) *Tailnet {
	tn := &Tailnet{
		api: f, dns: f, wake: make(chan struct{}, 1), backoff: 10 * time.Millisecond,
		state: guestv1.TailnetState_TAILNET_STATE_OFF,
		logf: func(format string, args ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.logs = append(f.logs, fmt.Sprintf(format, args...))
		},
		mount: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.noDisk {
				return errors.New("this guest's host has no tailscale disk to give it")
			}
			f.mounts++
			return nil
		},
		start: func() (*supervise.Process, error) {
			p, err := reaper.Start([]string{"/bin/sleep", "1000"}, &os.ProcAttr{Sys: &syscall.SysProcAttr{Setsid: true}})
			if err == nil {
				f.mu.Lock()
				f.daemons = append(f.daemons, p)
				f.mu.Unlock()
			}
			return p, err
		},
		hasState: func() (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.hasState, nil
		},
		removeState: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.removed++
			f.hasState = false
			f.api = backend{State: "NeedsLogin"}
			return nil
		},
		cli: func(_ context.Context, args []string, key []byte) (string, error) {
			f.mu.Lock()
			f.runs = append(f.runs, cliRun{args: args, key: string(key)})
			gate := f.upGate
			f.mu.Unlock()
			if args[0] != "up" {
				return "", nil
			}
			if gate != nil {
				gate <- struct{}{} // under way
				gate <- struct{}{} // let go
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.fail {
				return f.out, errors.New("exit code 1")
			}
			// As tailscaled: once up, the node runs and has its state.
			f.hasState = true
			f.api = backend{State: "Running", DNSName: "box.tail1234.ts.net.", IPs: []string{"100.64.0.7"}, Suffix: "tail1234.ts.net"}
			return f.out, nil
		},
	}
	t.Cleanup(func() { tn.Stop(time.Second) })
	return tn
}

func (f *fakes) snapshot() ([]cliRun, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runs), slices.Clone(f.logs)
}

func (f *fakes) ran(cmd string) []cliRun {
	runs, _ := f.snapshot()
	return slices.DeleteFunc(runs, func(r cliRun) bool { return r.args[0] != cmd })
}

func waitState(t *testing.T, tn *Tailnet, want guestv1.TailnetState) {
	t.Helper()
	testutil.Eventually(t, want.String(), func() (bool, string) {
		st, msg := tn.Status()
		return st == want, fmt.Sprintf("%v %q", st, msg)
	})
}

// TestTheJoinDecision: a guest with its node's state comes up as that node
// with no key, and refuses one; one without asks for a key, and joins with
// it, the key on the CLI's pipe and nowhere in its arguments.
func TestTheJoinDecision(t *testing.T) {
	cfg := &Config{Hostname: "box"}

	f := &fakes{hasState: true, api: backend{State: "Stopped"}}
	tn := newTestTailnet(t, f)
	tn.Apply(t.Context(), cfg)
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_JOINED)
	if ups := f.ran("up"); len(ups) != 1 || ups[0].key != "" || !slices.Equal(ups[0].args, UpArgs(*cfg, false)) {
		t.Fatalf("with state: ran %+v; want one up with no key", ups)
	}
	if tn.Report().GetWantsKey() {
		t.Error("a joined guest asks for a key")
	}
	if err := tn.Join(testKey); !errors.Is(err, ErrHasState) {
		t.Errorf("a key given to a guest with state: %v, want ErrHasState", err)
	}
	if ups := f.ran("up"); len(ups) != 1 {
		t.Errorf("a key given to a guest with state ran %+v", ups[1:])
	}

	f = &fakes{api: backend{State: "NeedsLogin"}}
	tn = newTestTailnet(t, f)
	tn.Apply(t.Context(), cfg)
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY)
	if ups := f.ran("up"); len(ups) != 0 {
		t.Fatalf("without state: ran %+v before a key", ups)
	}
	if !tn.Report().GetWantsKey() {
		t.Error("a guest with no state does not ask for the key")
	}
	if err := tn.Join(testKey); err != nil {
		t.Fatal(err)
	}
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_JOINED)
	ups := f.ran("up")
	if len(ups) != 1 || ups[0].key != testKey || !slices.Equal(ups[0].args, UpArgs(*cfg, true)) {
		t.Fatalf("without state: ran %+v; want one up with the key on its pipe", ups)
	}
}

// TestARejectedKey: the tailnet refusing the key says so, and the guest
// asks for one again only once the retry has passed, tailscaled started
// again meanwhile or not; nothing logged or said holds the key.
func TestARejectedKey(t *testing.T) {
	f := &fakes{api: backend{State: "NeedsLogin"}, fail: true,
		out: "backend error: invalid key: API key " + testKey + " not valid\n"}
	tn := newTestTailnet(t, f)
	tn.Apply(t.Context(), &Config{Hostname: "box"})
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY)
	if err := tn.Join(testKey); err != nil {
		t.Fatal(err)
	}
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED)
	// A refresh while the node needs a login keeps the refusal.
	tn.refresh(t.Context(), tn.sess)
	st, msg := tn.Status()
	if st != guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED {
		t.Errorf("after a refresh: %v %q, want KEY_REJECTED", st, msg)
	}
	if tn.Report().GetWantsKey() {
		t.Error("a guest asks for the key again at once after a refusal")
	}

	// tailscaled dying and starting again does not hurry the retry.
	f.mu.Lock()
	first := f.daemons[0]
	f.mu.Unlock()
	_ = first.Signal(syscall.SIGKILL)
	testutil.Eventually(t, "tailscaled started again", func() (bool, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.daemons) == 2, fmt.Sprintf("%d started", len(f.daemons))
	})
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED)
	if tn.Report().GetWantsKey() {
		t.Error("a guest whose tailscaled started again asks for the key before the retry")
	}

	_, logs := f.snapshot()
	for _, s := range append(logs, msg) {
		if strings.Contains(s, testKey) || strings.Contains(s, "EXAMPLEsecretVALUE") {
			t.Errorf("the key is in %q", s)
		}
	}
	if !strings.Contains(msg, "[auth key]") {
		t.Errorf("message %q: want the key cut out where the CLI echoed it", msg)
	}

	// Once the retry has passed, it asks again, and takes a new key.
	tn.mu.Lock()
	tn.sess.keyFailed = time.Now().Add(-KeyRetry)
	tn.mu.Unlock()
	if !tn.Report().GetWantsKey() {
		t.Error("the retry has passed, and the guest does not ask for the key")
	}
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	if err := tn.Join("tskey-auth-other"); err != nil {
		t.Fatalf("a new key after a refusal: %v", err)
	}
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_JOINED)
}

// TestJoinChangeLeave: a guest told a tailnet as it runs mounts the disk,
// splits its DNS and starts tailscaled; told a new hostname and routes it
// sets them on the running node; told none, it logs out, stops tailscaled,
// deletes its state and restores its DNS; told one again, it joins as a new
// node, with a key.
func TestJoinChangeLeave(t *testing.T) {
	f := &fakes{api: backend{State: "NeedsLogin"}}
	tn := newTestTailnet(t, f)
	if st, _ := tn.Status(); st != guestv1.TailnetState_TAILNET_STATE_OFF || len(f.dns) != 0 || f.mounts != 0 {
		t.Fatalf("before any tailnet: %v, DNS %q, mounted %d times", st, f.dns, f.mounts)
	}
	tn.Apply(t.Context(), &Config{Hostname: "box"})
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY)
	if err := tn.Join(testKey); err != nil {
		t.Fatal(err)
	}
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_JOINED)
	f.mu.Lock()
	daemon, dns := f.daemons[0], slices.Clone(f.dns)
	f.mu.Unlock()
	if !slices.Contains(dns, "split") || !slices.Contains(dns, "suffix tail1234.ts.net") {
		t.Errorf("joined: DNS %q, want the split and the tailnet's suffix", dns)
	}

	changed := Config{Hostname: "dev", AcceptRoutes: true}
	tn.Apply(t.Context(), &changed)
	if sets := f.ran("set"); len(sets) != 1 || !slices.Equal(sets[0].args, SetArgs(changed)) {
		t.Errorf("a new hostname and routes: ran %+v, want %q", sets, SetArgs(changed))
	}
	if f.mounts != 1 || len(f.daemons) != 1 {
		t.Errorf("a change mounted %d times and started %d tailscaleds; want neither again", f.mounts, len(f.daemons))
	}

	tn.Apply(t.Context(), nil)
	if st, _ := tn.Status(); st != guestv1.TailnetState_TAILNET_STATE_OFF {
		t.Errorf("left: %v, want OFF", st)
	}
	if len(f.ran("logout")) != 1 || f.removed != 1 {
		t.Errorf("left: logged out %d times, state deleted %d times; want once each", len(f.ran("logout")), f.removed)
	}
	select {
	case <-daemon.Done():
	default:
		t.Error("left: tailscaled still runs")
	}
	f.mu.Lock()
	dns = slices.Clone(f.dns)
	f.mu.Unlock()
	if n := len(dns); n < 2 || !slices.Contains(dns[n-2:], "restore") || !slices.Contains(dns[n-2:], "suffix ") {
		t.Errorf("left: DNS %q, want the suffix cleared and the guest's resolvers restored", dns)
	}
	if tn.Report().GetWantsKey() {
		t.Error("a guest that left asks for a key")
	}

	tn.Apply(t.Context(), &Config{Hostname: "box"})
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY)
	if err := tn.Join("tskey-auth-fresh"); err != nil {
		t.Fatal(err)
	}
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_JOINED)
	if ups := f.ran("up"); len(ups) != 2 || ups[1].key != "tskey-auth-fresh" {
		t.Errorf("joined again: ran %+v, want a second up with the fresh key", ups)
	}
}

// TestALeaveDuringAJoin: a join still under way as the guest leaves has
// what it finds out dropped with the session: told the tailnet again at
// once, the guest asks for a key and takes one, with no refusal or failure
// of the join before held against it.
func TestALeaveDuringAJoin(t *testing.T) {
	gate := make(chan struct{})
	f := &fakes{api: backend{State: "NeedsLogin"}, fail: true, out: "backend error: invalid key: API key not valid", upGate: gate}
	tn := newTestTailnet(t, f)
	cfg := &Config{Hostname: "box"}
	tn.Apply(t.Context(), cfg)
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY)
	if err := tn.Join(testKey); err != nil {
		t.Fatal(err)
	}
	<-gate // the up is under way
	tn.mu.Lock()
	old := tn.sess
	tn.mu.Unlock()
	tn.Apply(t.Context(), nil)
	if st, _ := tn.Status(); st != guestv1.TailnetState_TAILNET_STATE_OFF {
		t.Fatalf("left: %v, want OFF", st)
	}
	tn.Apply(t.Context(), cfg)
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY)
	<-gate // the old up fails, the tailnet refusing the key
	testutil.Eventually(t, "the old up done", func() bool {
		tn.mu.Lock()
		defer tn.mu.Unlock()
		return !old.joining
	})
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	testutil.Eventually(t, "the guest asking for the key afresh", func() (bool, string) {
		r := tn.Report()
		return r.GetWantsKey() && r.GetState() == guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY, fmt.Sprintf("%v", r)
	})
	if err := tn.Join("tskey-auth-fresh"); err != nil {
		t.Fatalf("a key after joining again: %v", err)
	}
	if err := tn.Join("tskey-auth-second"); !errors.Is(err, ErrBusy) {
		t.Errorf("a second key while the first joins: %v, want ErrBusy", err)
	}
	<-gate
	<-gate
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_JOINED)
	if st, msg := tn.Status(); strings.Contains(msg, "refused") {
		t.Errorf("joined again: %v %q, the old refusal said", st, msg)
	}
}

// TestNoDiskNoTailnet: a guest whose host gave it no tailscale disk says
// so, and neither splits its DNS nor starts anything.
func TestNoDiskNoTailnet(t *testing.T) {
	f := &fakes{noDisk: true}
	tn := newTestTailnet(t, f)
	tn.Apply(t.Context(), &Config{Hostname: "box"})
	if st, msg := tn.Status(); st != guestv1.TailnetState_TAILNET_STATE_FAILED || !strings.Contains(msg, "no tailscale disk") {
		t.Errorf("%v %q, want FAILED for the disk", st, msg)
	}
	if len(f.dns) != 0 || len(f.daemons) != 0 {
		t.Errorf("DNS %q and %d tailscaleds without the disk", f.dns, len(f.daemons))
	}
}

// TestTailscaledIsSupervised: tailscaled is started again when it dies,
// and stopped for good, with nothing more said, when the container stops.
func TestTailscaledIsSupervised(t *testing.T) {
	f := &fakes{api: backend{State: "NeedsLogin"}}
	tn := newTestTailnet(t, f)
	tn.Apply(t.Context(), &Config{Hostname: "box"})
	daemons := func() []*supervise.Process {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(f.daemons)
	}
	testutil.Eventually(t, "tailscaled started", func() bool { return len(daemons()) == 1 })
	_ = daemons()[0].Signal(syscall.SIGKILL)
	testutil.Eventually(t, "tailscaled started again", func() (bool, string) {
		return len(daemons()) == 2, fmt.Sprintf("%d started", len(daemons()))
	})
	waitState(t, tn, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY)
	_, before := f.snapshot()
	s := tn.sess
	tn.Stop(time.Second)
	select {
	case <-daemons()[1].Done():
	default:
		t.Fatal("stopped: tailscaled still runs")
	}
	<-s.done
	if n := len(daemons()); n != 2 {
		t.Errorf("stopped: %d tailscaleds started, want no more", n)
	}
	if _, after := f.snapshot(); len(after) != len(before) {
		t.Errorf("stopped, and said %q", after[len(before):])
	}
}

// TestTheKeyGoesOnFD3: the CLI reads the key from its descriptor 3, a pipe,
// and finds it nowhere in its arguments.
func TestTheKeyGoesOnFD3(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\necho \"args: $*\"\nif [ -e /proc/self/fd/3 ]; then echo \"fd3: $(cat /proc/self/fd/3)\"; else echo \"fd3: none\"; fi\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	was := cliPath
	cliPath = cli
	t.Cleanup(func() { cliPath = was })
	out, err := runCLI(t.Context(), reaper, UpArgs(Config{Hostname: "box"}, true), []byte(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "fd3: "+testKey+"\n") {
		t.Errorf("the CLI read %q, want the key on fd 3", out)
	}
	if args, _, _ := strings.Cut(out, "\n"); strings.Contains(args, testKey) || !strings.Contains(args, "--auth-key=file:/proc/self/fd/3") {
		t.Errorf("the CLI's arguments: %q", args)
	}
	if out, err := runCLI(t.Context(), reaper, []string{"status"}, nil); err != nil || !strings.Contains(out, "fd3: none") {
		t.Errorf("without a key: %q, %v; want no fd 3", out, err)
	}
}

func TestUpFailed(t *testing.T) {
	for _, c := range []struct {
		out     string
		withKey bool
		want    guestv1.TailnetState
	}{
		{"backend error: invalid key: API key kXX not valid", true, guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED},
		{"backend error: invalid key: unable to validate API key", true, guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED},
		{"timeout waiting for Tailscale service to enter a Running state", true, guestv1.TailnetState_TAILNET_STATE_FAILED},
		// Without a key, nothing is a key's refusal.
		{"backend error: invalid key", false, guestv1.TailnetState_TAILNET_STATE_FAILED},
		{"", true, guestv1.TailnetState_TAILNET_STATE_FAILED},
	} {
		if got, msg := upFailed(c.out, testKey, c.withKey); got != c.want || msg == "" {
			t.Errorf("%q, key %v: %v %q; want %v", c.out, c.withKey, got, msg, c.want)
		}
	}
}

func TestStateOf(t *testing.T) {
	for _, c := range []struct {
		b        backend
		hasState bool
		want     guestv1.TailnetState
	}{
		{backend{State: "Running", DNSName: "box.tail1234.ts.net."}, true, guestv1.TailnetState_TAILNET_STATE_JOINED},
		{backend{State: "NeedsLogin"}, false, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY},
		// A node whose login is gone never asks for the key again.
		{backend{State: "NeedsLogin"}, true, guestv1.TailnetState_TAILNET_STATE_FAILED},
		{backend{State: "NeedsMachineAuth"}, false, guestv1.TailnetState_TAILNET_STATE_FAILED},
		{backend{State: "Stopped"}, true, guestv1.TailnetState_TAILNET_STATE_FAILED},
		{backend{State: "Starting"}, true, guestv1.TailnetState_TAILNET_STATE_STARTING},
		{backend{State: "NoState"}, false, guestv1.TailnetState_TAILNET_STATE_STARTING},
	} {
		if got, _ := stateOf(c.b, c.hasState); got != c.want {
			t.Errorf("%s, state %v: %v, want %v", c.b.State, c.hasState, got, c.want)
		}
	}
	if _, msg := stateOf(backend{State: "Running", DNSName: "box.tail1234.ts.net.", IPs: []string{"100.64.0.7"}}, true); msg != "joined as box.tail1234.ts.net (100.64.0.7)" {
		t.Errorf("joined: %q", msg)
	}
}

func TestParseStatus(t *testing.T) {
	b, err := parseStatus([]byte(`{"BackendState":"Running","Self":{"DNSName":"box.tail1234.ts.net.","TailscaleIPs":["100.64.0.7","fd7a:115c:a1e0::7"]},
		"MagicDNSSuffix":"tail1234.ts.net","CurrentTailnet":{"Name":"example.com","MagicDNSSuffix":"tail1234.ts.net","MagicDNSEnabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if b.State != "Running" || b.Suffix != "tail1234.ts.net" || b.DNSName != "box.tail1234.ts.net." || len(b.IPs) != 2 {
		t.Errorf("%+v", b)
	}
	b, err = parseStatus([]byte(`{"BackendState":"Running","CurrentTailnet":{"MagicDNSSuffix":"tail1234.ts.net","MagicDNSEnabled":false}}`))
	if err != nil || b.Suffix != "" {
		t.Errorf("MagicDNS off: %+v, %v; want no suffix", b, err)
	}
}
