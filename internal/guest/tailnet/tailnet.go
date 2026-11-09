// Package tailnet joins a container's guest to a tailnet as a node of its
// own, and has it leave, as the node tells it, while the container runs:
// hangar-init runs tailscaled beside the container's process, from the
// platform's read-only disk, brings the node up, with the auth key the node
// hands it only while the guest has no node state, and splits the guest's
// DNS between the tailnet and the guest's own resolvers. It never blocks the
// container's process, and keeps no key past a join. See
// docs/decisions/container-tailscale.md.
package tailnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// Where tailscale lives in the guest. The disk is mounted under /run, the
// platform's tmpfs, so nothing is written into the image; the state is on
// the root disk, so the node outlives a restart.
const (
	// Mount is where the platform's read-only tailscale disk is mounted.
	Mount = "/run/hangar/tailscale"
	// BinDir holds tailscaled and the tailscale CLI.
	BinDir = Mount + "/bin"
	// StateDir is tailscaled's state, on the root disk.
	StateDir = "/var/lib/hangar/tailscale"
	// Socket is tailscaled's LocalAPI socket, where the CLI looks for it.
	Socket = "/var/run/tailscale/tailscaled.sock"
	// LogFile is tailscaled's output, capped at maxLog, the one before in
	// LogFile.1.
	LogFile = "/run/hangar/tailscaled.log"
)

const (
	// upTimeout bounds tailscale up waiting for the node to run.
	upTimeout = time.Minute
	// apiWait bounds waiting for a started tailscaled's LocalAPI.
	apiWait = 30 * time.Second
	// refreshEvery is how often tailscaled is asked how the node is.
	refreshEvery = 15 * time.Second
	// maxBackoff bounds the wait before tailscaled is started again; a run
	// longer than it starts the backoff over.
	maxBackoff = 30 * time.Second
	// logoutTimeout bounds tailscale logout as the guest leaves: a control
	// server out of reach does not keep it on the tailnet.
	logoutTimeout = 30 * time.Second
	// stopGrace is how long tailscaled has between SIGTERM and SIGKILL as
	// the guest leaves.
	stopGrace = 2 * time.Second
	// KeyRetry is how long after the tailnet refused a key, or a join with
	// it failed, the guest asks for the key again: the Secret may have been
	// fixed since.
	KeyRetry = 5 * time.Minute
	maxLog   = 1 << 20
)

// ErrHasState refuses a key to a guest that has joined before.
var ErrHasState = errors.New("the guest has its node's state, and joins without an auth key")

// ErrBusy refuses a key while a join is under way or none is asked for.
var ErrBusy = errors.New("the guest does not wait for an auth key")

// DNS switches the container's resolvers between the split and its own.
type DNS interface {
	// Split has the container resolve through the split, which it starts
	// listening if it is not.
	Split() error
	// Restore has the container resolve through its own resolvers again.
	Restore() error
	// SetSuffix sets the tailnet's MagicDNS suffix the split sends to
	// tailscaled; empty sends every name to the container's resolvers.
	SetSuffix(suffix string)
}

// Tailnet makes the guest what Set last said: a node of the tailnet, or
// none. Its zero value is not usable; see New.
type Tailnet struct {
	logf func(format string, args ...any)
	dns  DNS
	// mount mounts the tailscale disk; start starts tailscaled; cli runs
	// the tailscale CLI with args, the key, if any, on keyFD, and returns
	// its output; api asks tailscaled how the node is; hasState is
	// HasNodeState of the state file; removeState deletes the state. Each
	// is replaced in tests.
	mount func() error
	start func() (*supervise.Process, error)
	cli   func(ctx context.Context, args []string, key []byte) (string, error)
	api   interface {
		Status(context.Context) (backend, error)
	}
	hasState    func() (bool, error)
	removeState func() error
	// backoff is the first wait before tailscaled is started again.
	backoff time.Duration

	// ops runs one converge at a time; wake asks the worker for one.
	ops  sync.Mutex
	wake chan struct{}

	mu sync.Mutex
	// want is what Set last said, nil for no tailnet; have is what the
	// running session joins with, nil when there is none.
	want, have *Config
	sess       *session
	mounted    bool
	split      bool
	state      guestv1.TailnetState
	msg        string
	// closed: the container is stopping, and nothing more is said or done.
	closed bool
}

// session is one run of tailscaled, from a join until the guest leaves:
// what a join under way finds out after the guest left is the session's
// alone, and dropped with it.
type session struct {
	stop, done chan struct{}
	// The rest is guarded by Tailnet.mu. daemon is the running tailscaled,
	// nil between its runs; joining says a tailscale up is under way.
	daemon  *supervise.Process
	joining bool
	// keyFailed is when a join with a key last failed in this session,
	// zero if none has since; keyState and keyMsg are what it said, which
	// a tailscaled started again says again.
	keyFailed time.Time
	keyState  guestv1.TailnetState
	keyMsg    string
}

// New is the guest's tailnet, joining none until Set or Apply says, the
// tailscale disk on device, its processes started by reaper, its DNS
// switched by dns, if set, and its messages told with logf.
func New(device string, reaper *supervise.Reaper, dns DNS, logf func(string, ...any)) *Tailnet {
	t := &Tailnet{
		logf: logf, dns: dns,
		mount:       func() error { return MountDisk(device) },
		api:         newLocalAPI(Socket),
		hasState:    func() (bool, error) { return HasNodeState(filepath.Join(StateDir, "tailscaled.state")) },
		removeState: func() error { return os.RemoveAll(StateDir) },
		backoff:     time.Second,
		state:       guestv1.TailnetState_TAILNET_STATE_OFF, msg: "the guest joins no tailnet",
		wake: make(chan struct{}, 1),
	}
	t.start = func() (*supervise.Process, error) { return startDaemon(reaper) }
	t.cli = func(ctx context.Context, args []string, key []byte) (string, error) {
		return runCLI(ctx, reaper, args, key)
	}
	return t
}

// Run makes the guest what each Set says, in the background, until ctx
// ends. It returns at once.
func (t *Tailnet) Run(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.wake:
				t.converge(ctx)
			}
		}
	}()
}

// Set has the guest join the tailnet as cfg says, or leave it, cfg nil, in
// the background. It returns at once; Status says how it goes.
func (t *Tailnet) Set(cfg *Config) {
	t.mu.Lock()
	t.want = cloneConfig(cfg)
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// Apply is Set, done before it returns: as the guest boots, tailscaled
// starts before the container's process.
func (t *Tailnet) Apply(ctx context.Context, cfg *Config) {
	t.mu.Lock()
	t.want = cloneConfig(cfg)
	t.mu.Unlock()
	t.converge(ctx)
}

func cloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	c := *cfg
	return &c
}

// converge makes the guest what want says.
func (t *Tailnet) converge(ctx context.Context) {
	t.ops.Lock()
	defer t.ops.Unlock()
	t.mu.Lock()
	want, have, s, closed := t.want, t.have, t.sess, t.closed
	t.mu.Unlock()
	switch {
	case closed:
	case want == nil && s == nil:
		t.set(nil, guestv1.TailnetState_TAILNET_STATE_OFF, "the guest joins no tailnet")
	case want == nil:
		t.leave(ctx, s)
	case s == nil:
		t.join(ctx, *want)
	case *want != *have:
		t.change(ctx, s, *want)
	}
}

// join starts a session that joins as cfg says, with the DNS split on.
func (t *Tailnet) join(ctx context.Context, cfg Config) {
	t.mu.Lock()
	mounted, split := t.mounted, t.split
	t.mu.Unlock()
	if !mounted {
		if err := t.mount(); err != nil {
			t.set(nil, guestv1.TailnetState_TAILNET_STATE_FAILED, err.Error())
			return
		}
	}
	if t.dns != nil && !split {
		if err := t.dns.Split(); err != nil {
			t.logf("hangar-init: tailnet: the guest's DNS stays as it is, no tailnet name resolves: %v", err)
		} else {
			split = true
		}
	}
	s := &session{stop: make(chan struct{}), done: make(chan struct{})}
	t.mu.Lock()
	t.mounted, t.split = true, split
	t.sess, t.have = s, &cfg
	t.mu.Unlock()
	t.set(s, guestv1.TailnetState_TAILNET_STATE_STARTING, "tailscaled is starting")
	go t.supervise(ctx, s)
}

// leave logs the node out, stops its tailscaled, deletes its state, so a
// later join is a new node, and restores the container's resolvers.
func (t *Tailnet) leave(ctx context.Context, s *session) {
	t.set(s, guestv1.TailnetState_TAILNET_STATE_STARTING, "leaving the tailnet")
	lctx, cancel := context.WithTimeout(ctx, logoutTimeout)
	out, err := t.cli(lctx, []string{"logout"}, nil)
	cancel()
	if err != nil {
		// A node never logged in has nothing to log out of.
		t.logf("hangar-init: tailnet: tailscale logout: %s", lastLine(out+"\n"+err.Error()))
	}
	t.stopSession(s, stopGrace)
	t.mu.Lock()
	t.sess, t.have = nil, nil
	split := t.split
	t.split = false
	t.mu.Unlock()
	if err := t.removeState(); err != nil {
		t.logf("hangar-init: tailnet: deleting %s: %v", StateDir, err)
	}
	if t.dns != nil {
		t.dns.SetSuffix("")
		if split {
			if err := t.dns.Restore(); err != nil {
				t.logf("hangar-init: tailnet: restoring the guest's resolvers: %v", err)
			}
		}
	}
	t.set(nil, guestv1.TailnetState_TAILNET_STATE_OFF, "the guest left the tailnet")
}

// change sets a new hostname or accept_routes on the running node; a node
// not up yet takes them as it comes up.
func (t *Tailnet) change(ctx context.Context, s *session, cfg Config) {
	t.mu.Lock()
	t.have = &cfg
	t.mu.Unlock()
	if has, err := t.hasState(); err != nil || !has {
		return
	}
	if out, err := t.cli(ctx, SetArgs(cfg), nil); err != nil {
		if strings.TrimSpace(out) == "" {
			out = err.Error()
		}
		t.set(s, guestv1.TailnetState_TAILNET_STATE_FAILED, "tailscale set: "+lastLine(out))
		return
	}
	t.refresh(ctx, s)
}

// Status is where the join stands.
func (t *Tailnet) Status() (guestv1.TailnetState, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.msg
}

// Report is Status as the guest agent answers it, with whether the guest
// asks for the key now.
func (t *Tailnet) Report() *guestv1.TailnetStatusResponse {
	t.mu.Lock()
	state, msg, s := t.state, t.msg, t.sess
	running := s != nil && s.daemon != nil && !t.closed
	var joining bool
	var failed time.Time
	if s != nil {
		joining, failed = s.joining, s.keyFailed
	}
	t.mu.Unlock()
	wants := running && !joining && slices.Contains([]guestv1.TailnetState{
		guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY, guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED, guestv1.TailnetState_TAILNET_STATE_FAILED,
	}, state) && (failed.IsZero() || time.Since(failed) >= KeyRetry)
	if wants {
		if has, err := t.hasState(); err != nil || has {
			wants = false
		}
	}
	return &guestv1.TailnetStatusResponse{State: state, Message: msg, WantsKey: wants}
}

// Mounted says whether the tailscale disk, and so the CLI, is there.
func (t *Tailnet) Mounted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mounted
}

// set says state, for session s, the current one, or nil while there is
// none; what an ended session would say is dropped.
func (t *Tailnet) set(s *session, state guestv1.TailnetState, msg string) {
	t.mu.Lock()
	if t.sess != s || t.closed {
		// Once closed, the container's exit record may be written: nothing
		// is said after it.
		t.mu.Unlock()
		return
	}
	changed := t.state != state
	t.state, t.msg = state, msg
	t.mu.Unlock()
	if changed {
		t.logf("hangar-init: tailnet: %s: %s", strings.ToLower(strings.TrimPrefix(state.String(), "TAILNET_STATE_")), msg)
	}
}

// Join joins with key, in the background, a guest that has no node state
// and is not joining already. The key is kept for the join alone.
func (t *Tailnet) Join(key string) error {
	if key == "" {
		return errors.New("no auth key")
	}
	has, err := t.hasState()
	if err != nil {
		return err
	}
	if has {
		return ErrHasState
	}
	t.mu.Lock()
	s := t.sess
	switch {
	case s == nil, s.daemon == nil, s.joining, t.closed:
		t.mu.Unlock()
		return ErrBusy
	case !slices.Contains([]guestv1.TailnetState{
		guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY, guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED, guestv1.TailnetState_TAILNET_STATE_FAILED,
	}, t.state):
		t.mu.Unlock()
		return ErrBusy
	}
	s.joining = true
	t.mu.Unlock()
	b := []byte(key)
	go func() {
		defer clear(b)
		t.up(context.Background(), s, b)
	}()
	return nil
}

// up runs tailscale up for session s, with key if given, and says how it
// went, unless the guest left meanwhile: then what it found out is the
// ended session's, and a later join starts afresh.
func (t *Tailnet) up(ctx context.Context, s *session, key []byte) {
	t.mu.Lock()
	if t.sess != s || t.have == nil {
		s.joining = false
		t.mu.Unlock()
		return
	}
	s.joining = true
	cfg := *t.have
	t.mu.Unlock()
	t.set(s, guestv1.TailnetState_TAILNET_STATE_STARTING, "joining the tailnet")
	out, err := t.cli(ctx, UpArgs(cfg, key != nil), key)
	var state guestv1.TailnetState
	var msg string
	if err != nil {
		if strings.TrimSpace(out) == "" {
			out = err.Error()
		}
		state, msg = upFailed(out, string(key), key != nil)
	}
	t.mu.Lock()
	s.joining = false
	current := t.sess == s
	switch {
	case !current:
	case err == nil:
		s.keyFailed = time.Time{}
	case key != nil:
		s.keyFailed, s.keyState, s.keyMsg = time.Now(), state, msg
	}
	t.mu.Unlock()
	switch {
	case !current:
	case err != nil:
		t.set(s, state, msg)
	default:
		t.refresh(ctx, s)
	}
}

// bringUp brings session s's started tailscaled's node up once its
// LocalAPI answers: as the node its state holds, with no key; else it waits
// for one (Join).
func (t *Tailnet) bringUp(ctx context.Context, s *session, done <-chan struct{}) {
	if err := t.waitForAPI(ctx, done); err != nil {
		t.set(s, guestv1.TailnetState_TAILNET_STATE_FAILED, err.Error())
		return
	}
	has, err := t.hasState()
	if err != nil {
		t.set(s, guestv1.TailnetState_TAILNET_STATE_FAILED, "reading tailscaled's state: "+err.Error())
		return
	}
	if has {
		t.up(ctx, s, nil)
		return
	}
	// A key that failed still waits out its retry, tailscaled started
	// again or not.
	t.mu.Lock()
	failed, state, msg := s.keyFailed, s.keyState, s.keyMsg
	t.mu.Unlock()
	if !failed.IsZero() {
		t.set(s, state, msg)
		return
	}
	t.set(s, guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY, "waiting for the auth key")
}

func (t *Tailnet) waitForAPI(ctx context.Context, done <-chan struct{}) error {
	deadline := time.Now().Add(apiWait)
	for {
		actx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := t.api.Status(actx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tailscaled did not answer on %s within %v: %w", Socket, apiWait, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errors.New("tailscaled exited as it started")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// refresh asks tailscaled how session s's node is, and says so, unless a
// join is under way, or the last one failed and nothing has changed since.
func (t *Tailnet) refresh(ctx context.Context, s *session) {
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	b, err := t.api.Status(actx)
	cancel()
	if err != nil {
		return
	}
	has, err := t.hasState()
	if err != nil {
		return
	}
	state, msg := stateOf(b, has)
	t.mu.Lock()
	current := t.sess == s
	keep := s.joining ||
		state == guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY && (t.state == guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED || t.state == guestv1.TailnetState_TAILNET_STATE_FAILED) ||
		state == guestv1.TailnetState_TAILNET_STATE_STARTING && t.state != guestv1.TailnetState_TAILNET_STATE_JOINED
	t.mu.Unlock()
	if !current {
		return
	}
	if t.dns != nil {
		suffix := ""
		if state == guestv1.TailnetState_TAILNET_STATE_JOINED {
			suffix = b.Suffix
		}
		t.dns.SetSuffix(suffix)
	}
	if !keep {
		t.set(s, state, msg)
	}
}

// supervise runs session s's tailscaled until the session stops, starting
// it again whenever it exits.
func (t *Tailnet) supervise(ctx context.Context, s *session) {
	defer close(s.done)
	backoff := t.backoff
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		started := time.Now()
		proc, err := t.start()
		if err != nil {
			t.set(s, guestv1.TailnetState_TAILNET_STATE_FAILED, "starting tailscaled: "+err.Error())
		} else {
			t.mu.Lock()
			s.daemon = proc
			t.mu.Unlock()
			select {
			case <-s.stop:
				// Stopped as it started: stopSession saw no daemon.
				_ = proc.Signal(syscall.SIGKILL)
			default:
			}
			go t.bringUp(ctx, s, proc.Done())
			go t.refreshUntil(ctx, s, proc.Done())
			exit := proc.Wait()
			t.mu.Lock()
			s.daemon = nil
			t.mu.Unlock()
			select {
			case <-s.stop:
				return
			default:
			}
			if time.Since(started) > maxBackoff {
				backoff = t.backoff
			}
			t.set(s, guestv1.TailnetState_TAILNET_STATE_FAILED, fmt.Sprintf("tailscaled exited with code %d; it starts again in %v", exit.Code, backoff))
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func (t *Tailnet) refreshUntil(ctx context.Context, s *session, done <-chan struct{}) {
	tick := time.NewTicker(refreshEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-tick.C:
			t.refresh(ctx, s)
		}
	}
}

// stopSession stops session s's tailscaled, SIGTERM then SIGKILL after
// grace, and returns once it has ended, or grace and a second more have
// passed.
func (t *Tailnet) stopSession(s *session, grace time.Duration) {
	t.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	proc := s.daemon
	t.mu.Unlock()
	if proc != nil {
		_ = proc.Signal(syscall.SIGTERM)
		select {
		case <-proc.Done():
		case <-time.After(grace):
			_ = proc.Signal(syscall.SIGKILL)
		}
	}
	select {
	case <-s.done:
	case <-time.After(grace + time.Second):
	}
}

// Stop stops tailscaled as the container stops, SIGTERM then SIGKILL
// after grace, and says nothing more from then on. The node's state stays.
func (t *Tailnet) Stop(grace time.Duration) {
	t.mu.Lock()
	t.closed = true
	s := t.sess
	t.mu.Unlock()
	if s != nil {
		t.stopSession(s, grace)
	}
}

// Stopped says whether Stop was called.
func (t *Tailnet) Stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// SetArgs is the CLI's command line that gives a running node cfg's
// hostname and routes.
func SetArgs(cfg Config) []string {
	return []string{"set", "--hostname=" + cfg.Hostname, "--accept-routes=" + strconv.FormatBool(cfg.AcceptRoutes)}
}
