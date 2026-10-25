package apiserver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/jonboulle/clockwork"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// testGuest stands for a VM's guest's sshd: it trusts the CA for the VM's
// uid as any user but root, as its vendor-data makes a guest do. A shell
// says whom it runs as, then echoes its stdin; so does the command cat;
// any other command says what it ran and exits with 3. Either of the first
// two ends with its stdin, or with a signal. It answers keepalives unless
// silent, as a guest gone without a reset.
type testGuest struct {
	caPub ssh.PublicKey
	uid   string
	// hosts are its host keys, the first the one hostKey says.
	hosts atomic.Pointer[[]ssh.Signer]
	// distrust makes it refuse every key, as a guest made before the CA.
	distrust   atomic.Bool
	silent     atomic.Bool
	keepalives chan struct{}
	logins     chan guestLogin
	commands   chan string // "" for a shell
	ptys       chan [2]uint32
	resizes    chan [2]uint32
	signals    chan string
}

type guestLogin struct{ user, key string }

func newTestGuest(t *testing.T, caPub ssh.PublicKey, uid string) *testGuest {
	g := &testGuest{caPub: caPub, uid: uid, logins: make(chan guestLogin, 100), commands: make(chan string, 100),
		ptys: make(chan [2]uint32, 100), resizes: make(chan [2]uint32, 100), signals: make(chan string, 100),
		keepalives: make(chan struct{}, 100)}
	g.newHostKey(t)
	return g
}

// newHostKey gives the guest a new Ed25519 host key, its only one, as one
// whose keys were made again, and returns it.
func (g *testGuest) newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	signer := ed25519HostKey(t)
	g.setHostKeys(signer)
	return signer.PublicKey()
}

func (g *testGuest) setHostKeys(signers ...ssh.Signer) { g.hosts.Store(&signers) }

func ed25519HostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func ecdsaHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func authorizedKey(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func (g *testGuest) hostKey() string { return authorizedKey((*g.hosts.Load())[0].PublicKey()) }

func (g *testGuest) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			cert, ok := key.(*ssh.Certificate)
			if g.distrust.Load() || !ok || meta.User() == "root" {
				return nil, errors.New("not trusted")
			}
			checker := ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool { return bytes.Equal(k.Marshal(), g.caPub.Marshal()) }}
			if err := checker.CheckCert(g.uid, cert); err != nil {
				return nil, err
			}
			return &ssh.Permissions{Extensions: map[string]string{"key": ssh.FingerprintSHA256(cert.Key)}}, nil
		},
	}
	for _, k := range *g.hosts.Load() {
		cfg.AddHostKey(k)
	}
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	g.logins <- guestLogin{user: sc.User(), key: sc.Permissions.Extensions["key"]}
	go func() {
		for r := range reqs {
			if r.Type == vmKeepalive {
				g.keepalives <- struct{}{}
			}
			if g.silent.Load() {
				continue
			}
			// As OpenSSH's sshd, which refuses what it does not know.
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go g.session(sc.User(), ch, creqs)
	}
}

func (g *testGuest) session(user string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	started := make(chan string, 1)
	signal := make(chan string, 1)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for r := range reqs {
			ok := true
			switch r.Type {
			case "pty-req":
				var p struct {
					Term                      string
					Cols, Rows, Width, Height uint32
					Modes                     string
				}
				ok = ssh.Unmarshal(r.Payload, &p) == nil
				g.ptys <- [2]uint32{p.Rows, p.Cols}
			case "window-change":
				var w struct{ Cols, Rows, Width, Height uint32 }
				ok = ssh.Unmarshal(r.Payload, &w) == nil
				g.resizes <- [2]uint32{w.Rows, w.Cols}
			case "shell":
				started <- ""
			case "exec":
				var e struct{ Command string }
				ok = ssh.Unmarshal(r.Payload, &e) == nil
				started <- e.Command
			case "signal":
				var s struct{ Signal string }
				ok = ssh.Unmarshal(r.Payload, &s) == nil
				g.signals <- s.Signal
				select {
				case signal <- s.Signal:
				default:
				}
			default:
				ok = false
			}
			if r.WantReply {
				_ = r.Reply(ok, nil)
			}
		}
	}()
	var cmd string
	select {
	case cmd = <-started:
	case <-gone:
		return
	}
	g.commands <- cmd
	exit := func(status uint32) {
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	}
	if cmd != "" && cmd != "cat" {
		_, _ = fmt.Fprintf(ch, "ran %s", cmd)
		_, _ = fmt.Fprint(ch.Stderr(), "to stderr")
		exit(3)
		return
	}
	if cmd == "" {
		_, _ = fmt.Fprintf(ch, "shell of %s\n", user)
	}
	echoed := make(chan struct{})
	go func() {
		defer close(echoed)
		_, _ = io.Copy(ch, ch)
	}()
	select {
	case <-echoed:
		exit(0)
	case s := <-signal:
		_, _ = ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
			Signal     string
			CoreDumped bool
			Error      string
			Lang       string
		}{Signal: s}))
	case <-gone:
	}
}

// guestNode serves NodeService.SSH as a node does, reaching the guest; with
// down, it cannot connect, as to a guest whose sshd is not up. opened
// counts the calls it was asked, as each begins.
type guestNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	guest    *testGuest
	down     atomic.Bool
	opened   atomic.Int64
	machines chan string
}

func (n *guestNode) SSH(ctx context.Context, stream *connect.BidiStream[nodev1.SSHRequest, nodev1.SSHResponse]) error {
	n.opened.Add(1)
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	n.machines <- first.GetMachineUid()
	if n.down.Load() {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's port 22: connection refused", first.GetMachineUid()))
	}
	ours, theirs := net.Pipe()
	go n.guest.serve(theirs)
	defer func() { _ = ours.Close() }()
	go func() {
		for {
			req, err := stream.Receive()
			if err != nil {
				_ = ours.Close()
				return
			}
			if _, err := ours.Write(req.GetData()); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		k, err := ours.Read(buf)
		if k > 0 {
			if serr := stream.Send(&nodev1.SSHResponse{Data: buf[:k]}); serr != nil {
				return serr
			}
		}
		if err != nil {
			return nil
		}
	}
}

// Roles of the VM exec tests: one holds compute.machines.ssh alone, one
// compute.machines.exec alone, and one ssh and getting VMs.
const (
	sshOnlyRole  = "test.ssh-only"
	execOnlyRole = "test.exec-only"
	sshGetRole   = "test.ssh-get"
)

// vmExecWorld is an apiserver with a CA and alice's VM web, of an image
// whose default user is ubuntu, running on n1, whose node reaches its
// guest; sam holds only the ssh permission, eve only exec's, and gus ssh
// and getting VMs; rex holds ssh by the binding rex-ssh and exec by
// rex-exec.
type vmExecWorld struct {
	*execWorld
	guest  *testGuest
	node   *guestNode
	system context.Context
}

func newVMExecWorld(t *testing.T) *vmExecWorld {
	t.Helper()
	return newVMExecWorldWith(t, nil)
}

// newVMExecWorldWith is newVMExecWorld with its config adjusted by
// configure, if any.
func newVMExecWorldWith(t *testing.T, configure func(*Config)) *vmExecWorld {
	t.Helper()
	ca, caPub := testCA(t)
	log := &syncBuffer{}
	s, system, user := runningWith(t, func(c *Config) {
		if configure != nil {
			defer configure(c)
		}
		c.Nodes = []string{"n1"}
		c.SSHCA = ca
		c.Logger = slog.New(slog.NewJSONHandler(log, nil))
		c.ExtraRoles = map[string]authz.Role{
			sshOnlyRole:  authz.NewRole(authz.TenantLevel, authz.MachineSSH),
			execOnlyRole: authz.NewRole(authz.TenantLevel, authz.MachineExec),
			sshGetRole:   authz.NewRole(authz.TenantLevel, authz.MachineSSH, authz.Permission(registry.VirtualMachine, authz.Get)),
		}
	})
	for _, who := range []string{"sam@example.com", "eve@example.com", "gus@example.com", "rex@example.com"} {
		addMember(t, s, who)
	}
	for _, b := range []struct{ name, who, role string }{
		{"sam", "sam@example.com", sshOnlyRole}, {"eve", "eve@example.com", execOnlyRole}, {"gus", "gus@example.com", sshGetRole},
		{"rex-ssh", "rex@example.com", sshOnlyRole}, {"rex-exec", "rex@example.com", execOnlyRole},
	} {
		if _, err := s.backend(registry.RoleBinding).Apply(system,
			binding(b.name, b.who, b.role, &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}})); err != nil {
			t.Fatal(err)
		}
	}
	img := image("noble")
	img.Spec.DefaultUser = "ubuntu"
	if _, err := s.backend(registry.Image).Apply(system, img); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("web-root", "20Gi")); err != nil {
		t.Fatal(err)
	}
	uid := getVM(t, s, system, "web").GetMetadata().GetUid()
	m := machine(vmMachineName("web"), "n1")
	m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{RootDisk: "web-root"}}
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.VirtualMachine.APIVersion(), Kind: registry.VirtualMachine.Kind, Name: "web", Uid: uid, Controller: true,
	}}
	applied, err := s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, s, system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	srv := httptest.NewServer(s.Handler(auth.Chain{auth.Dev{}}))
	t.Cleanup(srv.Close)
	w := &vmExecWorld{
		execWorld: &execWorld{s: s, url: srv.URL, log: log, uid: applied.Object.(*computev1alpha1.Machine).GetMetadata().GetUid()},
		guest:     newTestGuest(t, caPub, uid),
		system:    system,
	}
	w.node = &guestNode{guest: w.guest, machines: make(chan string, 100)}
	w.connectNodeServing(t, w.node)
	return w
}

// dialVM opens an exec on the VM name as user, failing as the server
// refuses it.
func (w *vmExecWorld) dialVM(t *testing.T, user, name string) (*websocket.Conn, error) {
	t.Helper()
	c, resp, err := websocket.Dial(t.Context(), w.url+execws.Path("web", "virtualmachines", name), &websocket.DialOptions{
		HTTPHeader:   http.Header{auth.DevUserHeader: []string{user}},
		Subprotocols: []string{execws.Subprotocol},
	})
	if err != nil {
		if resp == nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		return nil, execws.ReadError(resp)
	}
	c.SetReadLimit(execws.MaxMessage)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, nil
}

// run execs start on web as alice, sends in, and reads the exec to its
// close.
func (w *vmExecWorld) run(t *testing.T, start *computev1alpha1.MachineExecStart, in ...*computev1alpha1.MachineExecRequest) wsResult {
	t.Helper()
	c, err := w.dialVM(t, "alice@example.com", "web")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(start))
	for _, m := range in {
		send(t, c, m)
	}
	return readAll(t, c)
}

func (w *vmExecWorld) recorded(t *testing.T) []*computev1alpha1.VirtualMachineSSHHostKey {
	t.Helper()
	return getVM(t, w.s, w.system, "web").GetStatus().GetSshHostKeys()
}

func stdinReq(s string) *computev1alpha1.MachineExecRequest {
	return &computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Stdin{Stdin: []byte(s)}}
}

func stdinCloseReq() *computev1alpha1.MachineExecRequest {
	return &computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_StdinClose{StdinClose: &computev1alpha1.MachineExecStdinClose{}}}
}

// receive waits for what comes on ch.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	var v T
	testutil.Eventually(t, what, func() bool {
		select {
		case v = <-ch:
			return true
		default:
			return false
		}
	})
	return v
}

// TestAVMExecLogsInAsTheDefaultUser: an exec with no command opens a shell
// on a terminal of the start's size, logged in as the image's default user
// with a certificate for the VM, through the VM's machine; its first login
// records the host key in the VM's status. The audit lines say who, as
// whom, with which key, and how it ended, never what was typed or written.
func TestAVMExecLogsInAsTheDefaultUser(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	if got := w.recorded(t); len(got) != 0 {
		t.Fatalf("host keys before any login: %v", got)
	}
	c, err := w.dialVM(t, "alice@example.com", "web")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.MachineExecStart{Tty: true, Stdin: true, Size: &computev1alpha1.MachineExecWindowSize{Rows: 30, Cols: 100}}))
	if got := receive(t, w.guest.ptys, "the terminal"); got != [2]uint32{30, 100} {
		t.Errorf("a terminal of %v, want 30x100", got)
	}
	send(t, c, &computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Resize{Resize: &computev1alpha1.MachineExecWindowSize{Rows: 40, Cols: 120}}})
	if got := receive(t, w.guest.resizes, "the resize"); got != [2]uint32{40, 120} {
		t.Errorf("resized to %v, want 40x120", got)
	}
	send(t, c, stdinReq("typed secret\n"))
	send(t, c, stdinCloseReq())
	r := readAll(t, c)
	if r.err != nil || r.stdout != "shell of ubuntu\ntyped secret\n" || r.exit == nil || r.exit.GetCode() != 0 {
		t.Errorf("the shell: %+v", r)
	}
	if got := receive(t, w.guest.commands, "the session"); got != "" {
		t.Errorf("ran %q, want a shell", got)
	}
	login := receive(t, w.guest.logins, "the login")
	if login.user != "ubuntu" {
		t.Errorf("logged in as %q", login.user)
	}
	if got := receive(t, w.node.machines, "the node's call"); got != w.uid {
		t.Errorf("the node was asked for machine %q, want %q", got, w.uid)
	}
	keys := w.recorded(t)
	if len(keys) != 1 || keys[0].GetPublicKey() != w.guest.hostKey() || keys[0].GetRecordTime() == nil {
		t.Errorf("recorded %v, want the guest's %s", keys, w.guest.hostKey())
	}
	for msg, want := range map[string]map[string]any{
		"exec started": {"principal": "user:alice@example.com", "kind": "VirtualMachine", "name": "web", "machine": w.uid, "user": "ubuntu", "key": login.key, "tty": true},
		"exec ended":   {"principal": "user:alice@example.com", "name": "web", "user": "ubuntu", "key": login.key, "exit_code": float64(0), "signal": float64(0)},
	} {
		wantFields(t, w.auditLine(t, msg), want)
	}
	w.auditLine(t, "ssh host key recorded")
	if strings.Contains(w.log.String(), "secret") || strings.Contains(w.log.String(), "shell of") {
		t.Errorf("the log holds the session:\n%s", w.log.String())
	}

	// The same key again passes, as another user, the start's.
	r = w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}, User: "alice"})
	if r.err != nil || r.exit.GetCode() != 3 {
		t.Errorf("a second login: %+v", r)
	}
	if got := receive(t, w.guest.logins, "the second login").user; got != "alice" {
		t.Errorf("logged in as %q, want alice", got)
	}
	if got := w.recorded(t); len(got) != 1 || got[0].GetPublicKey() != keys[0].GetPublicKey() {
		t.Errorf("recorded after the second login: %v", got)
	}
}

// TestAVMExecRunsACommandAsGiven: a command is one line for the user's
// shell, each argument quoted that needs it, so it runs as given; its
// output and exit code come back; and a terminal is not asked for unless
// the start asks.
func TestAVMExecRunsACommandAsGiven(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"printf", `%s\n`, "a b", "it's", "", "x;rm"}})
	want := `printf '%s\n' 'a b' 'it'\''s' '' 'x;rm'`
	if got := receive(t, w.guest.commands, "the command"); got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	if r.err != nil || r.stdout != "ran "+want || r.stderr != "to stderr" || r.exit.GetCode() != 3 || r.exit.GetSignal() != 0 {
		t.Errorf("%+v", r)
	}
	select {
	case p := <-w.guest.ptys:
		t.Errorf("a terminal %v without tty", p)
	default:
	}
}

// TestAVMExecCarriesStdinAndSignals: stdin reaches the process and its
// close ends it; a signal reaches it by name, and the exit says it.
func TestAVMExecCarriesStdinAndSignals(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"cat"}, Stdin: true}, stdinReq("some input"), stdinCloseReq())
	if r.err != nil || r.stdout != "some input" || r.exit.GetCode() != 0 {
		t.Errorf("stdin: %+v", r)
	}
	c, err := w.dialVM(t, "alice@example.com", "web")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.MachineExecStart{Command: []string{"cat"}, Stdin: true}))
	send(t, c, stdinReq("ping"))
	send(t, c, &computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Signal{Signal: 2}})
	r = readAll(t, c)
	if r.err != nil || r.exit.GetCode() != 130 || r.exit.GetSignal() != 2 {
		t.Errorf("a signal: %+v", r)
	}
	if got := receive(t, w.guest.signals, "the signal"); got != "INT" {
		t.Errorf("the guest got signal %q, want INT", got)
	}
	// Stdin to an exec started without it ends the exec.
	r = w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"cat"}}, stdinReq("x"))
	wantCodeOf(t, r.err, connect.CodeInvalidArgument, "stdin without asking for it")
}

// TestAVMExecRefusesWhatSSHCannotDo: an environment, a working directory,
// and root, whether the start or the image names it, are refused before
// the node is asked to reach the guest.
func TestAVMExecRefusesWhatSSHCannotDo(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	for what, start := range map[string]*computev1alpha1.MachineExecStart{
		"env":                          {Command: []string{"id"}, Env: []*computev1alpha1.EnvVar{{Name: "A", Value: "b"}}},
		"working_dir":                  {Command: []string{"id"}, WorkingDir: "/srv"},
		"root":                         {User: "root"},
		"a user that is no login name": {User: "a b"},
	} {
		r := w.run(t, start)
		wantCodeOf(t, r.err, connect.CodeInvalidArgument, what)
		if r.err != nil && what != "root" && what != "a user that is no login name" && !strings.Contains(r.err.Error(), what) {
			t.Errorf("%s: refused with %v", what, r.err)
		}
	}
	setDefaultUser := func(user string) {
		img := image("noble")
		img.Spec.DefaultUser = user
		if _, err := w.s.backend(registry.Image).Apply(w.system, img); err != nil {
			t.Fatal(err)
		}
	}
	setDefaultUser("root")
	r := w.run(t, &computev1alpha1.MachineExecStart{})
	wantCodeOf(t, r.err, connect.CodeInvalidArgument, "an image whose default user is root")
	if r.err != nil && !strings.Contains(r.err.Error(), "as root") {
		t.Errorf("an image whose default user is root: refused with %v", r.err)
	}
	setDefaultUser("ubuntu")
	// An exec that reaches the guest, once those before it have ended: the
	// node takes the calls of one tunnel in order.
	if r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}}); r.err != nil || r.exit.GetCode() != 3 {
		t.Fatalf("an exec after the refusals: %+v", r)
	}
	if n := w.node.opened.Load(); n != 1 {
		t.Errorf("the node was asked for %d SSH connections, want only the last exec's", n)
	}
}

// TestAVMExecNeedsTheSSHPermission: one who holds compute.machines.ssh
// alone execs in the VM; one who holds compute.machines.exec alone is
// refused before the upgrade, and the node is not called.
func TestAVMExecNeedsTheSSHPermission(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	_, err := w.dialVM(t, "eve@example.com", "web")
	wantCodeOf(t, err, connect.CodePermissionDenied, "eve, who may exec but not ssh")
	select {
	case m := <-w.node.machines:
		t.Errorf("the node was called for %s", m)
	default:
	}
	c, err := w.dialVM(t, "sam@example.com", "web")
	if err != nil {
		t.Fatalf("sam, who may ssh: %v", err)
	}
	send(t, c, startReq(&computev1alpha1.MachineExecStart{Command: []string{"id"}}))
	if r := readAll(t, c); r.err != nil || r.exit.GetCode() != 3 {
		t.Errorf("sam's exec: %+v", r)
	}
	wantFields(t, w.auditLine(t, "exec refused"), map[string]any{"principal": "user:eve@example.com", "plural": "virtualmachines", "code": "permission_denied"})
}

// TestAVMExecRefusesAChangedHostKey: a host key that is not the one
// recorded is refused, naming the call that forgets it, and the record is
// kept, through the VM's controller too; forgetting needs to apply the VM,
// which logging in to it does not give, and the next login records the
// new key.
func TestAVMExecRefusesAChangedHostKey(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	if r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}}); r.err != nil {
		t.Fatal(r.err)
	}
	receive(t, w.guest.commands, "the first command")
	first := w.recorded(t)
	if len(first) != 1 {
		t.Fatalf("recorded %v", first)
	}
	if err := w.s.reconcileVirtualMachine(w.system, controller.Key{Project: "web", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	w.guest.newHostKey(t)
	r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}})
	wantCodeOf(t, r.err, connect.CodeFailedPrecondition, "a changed host key")
	if r.err != nil && !strings.Contains(r.err.Error(), "forget it on the VM's page in the console") {
		t.Errorf("the refusal does not say where to forget the key: %v", r.err)
	}
	if got := w.recorded(t); len(got) != 1 || got[0].GetPublicKey() != first[0].GetPublicKey() {
		t.Errorf("recorded after a changed key: %v, want %v", got, first)
	}
	select {
	case c := <-w.guest.commands:
		t.Errorf("ran %q on a guest whose key changed", c)
	default:
	}

	forget := func(user string) (*computev1alpha1.ForgetSSHHostKeysResponse, error) {
		res, err := computev1alpha1connect.NewMachineCapabilityServiceClient(&http.Client{Transport: devAs{user}}, w.url).
			ForgetSSHHostKeys(t.Context(), connect.NewRequest(&computev1alpha1.ForgetSSHHostKeysRequest{Project: "web", Kind: registry.VirtualMachine.Kind, Name: "web"}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	for _, who := range []string{"sam@example.com", "gus@example.com"} {
		_, err := forget(who)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+", who may ssh but not apply the VM, forgetting")
	}
	if got := w.recorded(t); len(got) != 1 {
		t.Errorf("recorded after forgetting refused: %v", got)
	}
	// Gus may get the VM: the refusal is for want of apply.
	if _, err := w.s.backend(registry.VirtualMachine).Get(auth.WithPrincipal(t.Context(), auth.Principal{Type: auth.User, ID: "gus@example.com"}),
		resource.Key{Project: "web", Name: "web"}); err != nil {
		t.Errorf("gus getting the VM: %v", err)
	}
	res, err := forget("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetForgotten(); len(got) != 1 || got[0] != first[0].GetPublicKey() {
		t.Errorf("forgot %v, want %v", got, first)
	}
	if got := w.recorded(t); len(got) != 0 {
		t.Errorf("recorded after forgetting: %v", got)
	}
	w.auditLine(t, "ssh host keys forgotten")
	if r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}}); r.err != nil || r.exit.GetCode() != 3 {
		t.Errorf("after forgetting: %+v", r)
	}
	if got := w.recorded(t); len(got) != 1 || got[0].GetPublicKey() != w.guest.hostKey() {
		t.Errorf("recorded after forgetting and logging in: %v, want %s", got, w.guest.hostKey())
	}
	_, err = computev1alpha1connect.NewMachineCapabilityServiceClient(&http.Client{Transport: devAs{"alice@example.com"}}, w.url).
		ForgetSSHHostKeys(t.Context(), connect.NewRequest(&computev1alpha1.ForgetSSHHostKeysRequest{Project: "web", Kind: registry.Container.Kind, Name: "web"}))
	wantCodeOf(t, err, connect.CodeUnimplemented, "forgetting a container's host keys")
}

// TestAVMExecAsksForTheKindOfKeyRecorded: a later login asks for a host
// key of the kind recorded, so a guest that also offers a kind the client
// prefers, ECDSA, passes with the Ed25519 key recorded; one that no longer
// offers the kind recorded is refused as a changed key, not taken for an
// sshd not up yet.
func TestAVMExecAsksForTheKindOfKeyRecorded(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	if r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}}); r.err != nil {
		t.Fatal(r.err)
	}
	recorded := w.guest.hostKey()
	w.guest.setHostKeys(ecdsaHostKey(t), (*w.guest.hosts.Load())[0])
	if r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}}); r.err != nil || r.exit.GetCode() != 3 {
		t.Errorf("a guest that also offers an ECDSA key: %+v", r)
	}
	if got := w.recorded(t); len(got) != 1 || got[0].GetPublicKey() != recorded {
		t.Errorf("recorded %v, want %s", got, recorded)
	}
}

// TestAVMExecFirstRecordsTheEd25519Key: a first login asks for Ed25519
// first, as OpenSSH does, so a guest with ECDSA and Ed25519 host keys gets
// its Ed25519 key recorded, the one people compare with.
func TestAVMExecFirstRecordsTheEd25519Key(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	ed := ed25519HostKey(t)
	w.guest.setHostKeys(ecdsaHostKey(t), ed)
	if r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}}); r.err != nil || r.exit.GetCode() != 3 {
		t.Fatalf("the first login: %+v", r)
	}
	if got, want := w.recorded(t), authorizedKey(ed.PublicKey()); len(got) != 1 || got[0].GetPublicKey() != want {
		t.Errorf("recorded %v, want the Ed25519 key %s", got, want)
	}
}

// TestAVMExecRefusesAGuestWithoutTheKindRecorded: a guest whose ECDSA key
// is recorded and that offers Ed25519 alone is refused as a changed key,
// not taken for an sshd not up yet.
func TestAVMExecRefusesAGuestWithoutTheKindRecorded(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	w.guest.setHostKeys(ecdsaHostKey(t))
	if r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}}); r.err != nil {
		t.Fatal(r.err)
	}
	w.guest.newHostKey(t) // Ed25519 alone
	r := w.run(t, &computev1alpha1.MachineExecStart{Command: []string{"id"}})
	wantCodeOf(t, r.err, connect.CodeFailedPrecondition, "a guest without the kind of key recorded")
	if r.err != nil && !strings.Contains(r.err.Error(), "not the one recorded") {
		t.Errorf("the refusal: %v", r.err)
	}
}

// TestAVMExecEndsWhenTheGuestFallsSilent: the apiserver asks the guest to
// answer at each ping of the exec's timing, on its clock; a guest that
// answers keeps the exec, and one that stops answering, without closing
// anything, ends it with Unavailable once the pong window has passed. Its
// timing keeps clear of the streams' rechecks, every 30 s on the same
// clock.
func TestAVMExecEndsWhenTheGuestFallsSilent(t *testing.T) {
	t.Parallel()
	const ping, pong = 7 * time.Second, 5 * time.Second
	w := newVMExecWorldWith(t, func(c *Config) { c.ExecPing, c.ExecPong = ping, pong })
	clock := w.s.clock.(*clockwork.FakeClock)
	c, err := w.dialVM(t, "alice@example.com", "web")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.MachineExecStart{Command: []string{"cat"}, Stdin: true}))
	receive(t, w.guest.commands, "the session")
	clock.Advance(ping)
	receive(t, w.guest.keepalives, "a keepalive")
	send(t, c, stdinReq("still there"))
	ctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	_, b, err := c.Read(ctx)
	res := &computev1alpha1.MachineExecResponse{}
	if err == nil {
		err = proto.Unmarshal(b, res)
	}
	if err != nil || string(res.GetStdout()) != "still there" {
		t.Fatalf("the exec, after a keepalive answered: %v, %v", res, execws.ErrorOf(err))
	}

	w.guest.silent.Store(true)
	clock.Advance(ping)
	receive(t, w.guest.keepalives, "a keepalive unanswered")
	clock.Advance(pong)
	r := readAll(t, c)
	wantCodeOf(t, r.err, connect.CodeUnavailable, "a guest gone silent")
	if r.err != nil && !strings.Contains(r.err.Error(), "the VM stopped answering") {
		t.Errorf("the reason: %v", r.err)
	}
}

// TestFirstLoginsAtOnceRecordOneKey: of two first logins at once shown
// different keys, one records its key and the other is refused.
func TestFirstLoginsAtOnceRecordOneKey(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	vm := getVM(t, w.s, w.system, "web")
	x := &machineTarget{principal: auth.SystemPrincipal, kind: registry.VirtualMachine, key: resource.Key{Project: "web", Name: "web"}, obj: vm}
	keyOf := func() ssh.PublicKey { return w.guest.newHostKey(t) }
	for round := range 20 {
		keys := []ssh.PublicKey{keyOf(), keyOf()}
		errs := make([]error, len(keys))
		var ready, wg sync.WaitGroup
		ready.Add(1)
		for i, k := range keys {
			wg.Go(func() {
				ready.Wait()
				errs[i] = w.s.trustHostKey(t.Context(), x, k)
			})
		}
		ready.Done()
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: the logins ended with %v and %v; want one key trusted", round, errs[0], errs[1])
		}
		won := keys[0]
		if errs[0] != nil {
			won = keys[1]
		}
		if got := w.recorded(t); len(got) != 1 || got[0].GetPublicKey() != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(won))) {
			t.Fatalf("round %d: recorded %v", round, got)
		}
		status := proto.Clone(getVM(t, w.s, w.system, "web").GetStatus()).(*computev1alpha1.VirtualMachineStatus)
		status.SshHostKeys = nil
		if _, err := w.s.backend(registry.VirtualMachine).UpdateStatus(w.system, x.key, status, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAVMExecSaysWhyItFails: a guest whose sshd is not up yet, and one
// that does not trust the platform's CA, end the exec with a reason the
// client shows.
func TestAVMExecSaysWhyItFails(t *testing.T) {
	t.Parallel()
	w := newVMExecWorld(t)
	w.node.down.Store(true)
	r := w.run(t, &computev1alpha1.MachineExecStart{})
	wantCodeOf(t, r.err, connect.CodeUnavailable, "a guest whose sshd is not up")
	if r.err != nil && !strings.Contains(r.err.Error(), "comes up a few seconds after the VM's first boot ends") {
		t.Errorf("the reason: %v", r.err)
	}
	w.node.down.Store(false)
	w.guest.distrust.Store(true)
	r = w.run(t, &computev1alpha1.MachineExecStart{})
	wantCodeOf(t, r.err, connect.CodeFailedPrecondition, "a guest that does not trust the CA")
	if r.err != nil && !strings.Contains(r.err.Error(), "refused the platform's SSH certificate") {
		t.Errorf("the reason: %v", r.err)
	}
	wantFields(t, w.auditLine(t, "exec ended"), map[string]any{"name": "web", "user": "ubuntu", "code": "unavailable"})
}
