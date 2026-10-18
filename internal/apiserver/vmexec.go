package apiserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/jonboulle/clockwork"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// A virtual machine's exec is an SSH login by the apiserver to its guest,
// over the node's SSH call, speaking the exec protocol to the client; see
// docs/decisions/vm-ssh.md.
const (
	// vmExecHandshake bounds the SSH handshake with a guest that took the
	// connection but does not finish it.
	vmExecHandshake = 30 * time.Second
	// vmExecTerm is the terminal a VM's exec asks for: the console's,
	// xterm.js.
	vmExecTerm = "xterm-256color"
	// vmExecChunk bounds what one message to the node or the client
	// carries.
	vmExecChunk = 32 << 10
	// vmKeepalive is the global request that asks the guest's sshd to
	// answer, as OpenSSH's ServerAliveInterval sends it.
	vmKeepalive = "keepalive@openssh.com"
)

// errHostKeyChanged refuses a login whose guest shows a host key the VM's
// status does not record. It says where the record can be forgotten: the
// CLI has no command for it, and hangar ssh --forget-host-key clears only
// its own known_hosts (vm-ssh.md). It is a close reason, so short.
var errHostKeyChanged = failedPrecondition("the VM's SSH host key is not the one recorded; if its keys were made again, forget it on the VM's page in the console")

// errVMSilent ends a VM's exec whose guest stopped answering keepalives.
var errVMSilent = connect.NewError(connect.CodeUnavailable, errors.New("the VM stopped answering"))

// sshSignals names the signals an exec may send a VM's process, by their
// numbers on Linux, as SSH's signal request names them.
var sshSignals = map[int32]ssh.Signal{
	1: ssh.SIGHUP, 2: ssh.SIGINT, 3: ssh.SIGQUIT, 4: ssh.SIGILL, 6: ssh.SIGABRT, 8: ssh.SIGFPE, 9: ssh.SIGKILL,
	10: ssh.SIGUSR1, 11: ssh.SIGSEGV, 12: ssh.SIGUSR2, 13: ssh.SIGPIPE, 14: ssh.SIGALRM, 15: ssh.SIGTERM,
}

// signalNumber is the number of the signal SSH names, 0 if none is known.
func signalNumber(name string) int32 {
	for n, s := range sshSignals {
		if string(s) == name {
			return n
		}
	}
	return 0
}

// checkVMExecStart checks what a VM's exec may not be asked: a command is
// optional, a shell then; sshd drops most of the environment, and SSH has
// no working directory.
func checkVMExecStart(start *computev1alpha1.MachineExecStart) error {
	switch {
	case start == nil:
		return invalid("the first message must start the process")
	case len(start.GetEnv()) > 0:
		return invalid("env is not supported on virtual machines: their sshd drops most of the environment; set it in the command")
	case start.GetWorkingDir() != "":
		return invalid("working_dir is not supported on virtual machines: SSH has none; cd in the command")
	case start.GetUser() != "" && !unixUser.MatchString(start.GetUser()):
		return invalid("user %q is not a login name, such as ubuntu", start.GetUser())
	case len(start.GetCommand()) > 0 && start.GetCommand()[0] == "":
		return invalid("the command's program is empty")
	}
	for _, a := range start.GetCommand() {
		if strings.ContainsRune(a, 0) {
			return invalid("the command holds a NUL byte")
		}
	}
	return nil
}

// plainWord is an argument a shell takes as itself, unquoted.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// remoteCommand is argv as the one line SSH carries, which the user's
// shell runs: each argument quoted unless it needs not be, so that it runs
// as given, as for sh.
func remoteCommand(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		if plainWord.MatchString(a) {
			words[i] = a
		} else {
			words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// vmExec serves a VM's exec, as exec does a container's: it logs in to the
// guest over SSH as the start's user, or the image's default user, and
// relays the session until it ends, or end.
func (s *Server) vmExec(ctx context.Context, end func(error), c *websocket.Conn, x *machineTarget, live *execws.Liveness, audit *execAudit) error {
	ws := context.WithoutCancel(ctx)
	start, err := s.readStart(ctx, end, c, live)
	if err != nil {
		return err
	}
	if err := checkVMExecStart(start); err != nil {
		return err
	}
	user := start.GetUser()
	if user == "" {
		if user, err = s.defaultUser(ctx, x.obj); err != nil {
			return err
		}
		if user == "" {
			return failedPrecondition("the VM's image names no default user: say whom to log in as, as the start's user")
		}
	}
	// Whoever names root, the start or the image: the guest refuses the
	// certificate for root, and would be taken for not trusting the CA.
	if user == "root" {
		return invalid("an exec does not log in to a virtual machine as root: log in as another user, and use sudo")
	}
	signer, cert, err := s.vmExecKey(x)
	if err != nil {
		return err
	}
	audit.start, audit.user, audit.key = start, user, ssh.FingerprintSHA256(cert.Key)
	s.auditStarted(x, audit)

	// The node ends its connection to the guest once the call is reset,
	// which ctx ending does, as an exec's.
	conn := &nodeSSHConn{stream: x.call.nodes.SSH(ctx), machine: x.machine}
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := conn.open(); err != nil {
		return s.vmNodeFailed(ctx, x, conn, err)
	}
	go func() {
		if live.Run(ctx, c) != nil {
			end(errClientGone)
		}
	}()
	client, err := s.vmLogin(ctx, conn, x, user, signer)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	// The ticker is made here, so that it runs before the session can
	// begin. The keepalive stops before the client closes, which would
	// fail a request in flight.
	timing := s.execTiming()
	ticker := s.clock.NewTicker(timing.ping)
	keepalive, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go func() {
		defer ticker.Stop()
		if err := s.vmKeepalive(keepalive, client, ticker, timing.pong); err != nil {
			end(err)
		}
	}()
	sess, err := client.NewSession()
	if err != nil {
		return vmSessionFailed(ctx, x, conn, fmt.Errorf("opening a session: %w", err))
	}
	sess.Stdout = &execOutput{ctx: ctx, ws: ws, c: c, live: live}
	sess.Stderr = &execOutput{ctx: ctx, ws: ws, c: c, live: live, stderr: true}
	// Without stdin a process's is closed at once; a terminal's is never
	// written, as nothing is typed on it.
	var stdin io.WriteCloser
	if start.GetStdin() || start.GetTty() {
		if stdin, err = sess.StdinPipe(); err != nil {
			return vmSessionFailed(ctx, x, conn, err)
		}
	}
	if start.GetTty() {
		rows, cols := int(start.GetSize().GetRows()), int(start.GetSize().GetCols())
		if rows == 0 || cols == 0 {
			rows, cols = 24, 80
		}
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}
		if err := sess.RequestPty(vmExecTerm, rows, cols, modes); err != nil {
			return vmSessionFailed(ctx, x, conn, fmt.Errorf("asking for a terminal: %w", err))
		}
	}
	if len(start.GetCommand()) == 0 {
		err = sess.Shell()
	} else {
		err = sess.Start(remoteCommand(start.GetCommand()))
	}
	if err != nil {
		return vmSessionFailed(ctx, x, conn, fmt.Errorf("starting: %w", err))
	}
	go func() {
		// Once the exec has ended, end does nothing.
		end(vmRelayInput(ctx, ws, c, live, sess, stdin, start.GetStdin()))
	}()

	// Wait returns once the session's output is all written to the client.
	err = sess.Wait()
	var exit *computev1alpha1.MachineExecExit
	var ee *ssh.ExitError
	switch {
	case err == nil:
		exit = &computev1alpha1.MachineExecExit{}
	case errors.As(err, &ee):
		exit = &computev1alpha1.MachineExecExit{Code: int32(ee.ExitStatus())}
		if n := signalNumber(ee.Signal()); n != 0 {
			exit = &computev1alpha1.MachineExecExit{Code: 128 + n, Signal: n}
		}
	default:
		return vmSessionFailed(ctx, x, conn, err)
	}
	audit.exit = exit
	done := live.Pending()
	err = writeExec(ws, c, &computev1alpha1.MachineExecResponse{Message: &computev1alpha1.MachineExecResponse_Exit{Exit: exit}})
	done()
	if err != nil {
		return clientFailed(ctx, err)
	}
	return nil
}

// vmExecKey makes a key for one login and certifies it with the platform
// CA for the VM, as SignSSHKey does, naming the caller.
func (s *Server) vmExecKey(x *machineTarget) (ssh.Signer, *ssh.Certificate, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInternal, err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInternal, err)
	}
	cert, err := s.cfg.SSHCA.Sign(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), object.Meta(x.obj).GetUid(), x.principal.String())
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInternal, err)
	}
	return certSigner, cert, nil
}

// vmLogin logs in to the guest over conn as user, trusting its host key
// as the VM's status records it (trustHostKey).
func (s *Server) vmLogin(ctx context.Context, conn *nodeSSHConn, x *machineTarget, user string, signer ssh.Signer) (*ssh.Client, error) {
	var hostErr error
	var shown bool
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			shown = true
			hostErr = s.trustHostKey(ctx, x, key)
			return hostErr
		},
	}
	// The key recorded is asked for, not whichever the client prefers.
	var pinned bool
	cfg.HostKeyAlgorithms, pinned = hostKeyAlgorithms(x.obj.(*computev1alpha1.VirtualMachine))
	var timedOut atomic.Bool
	timer := time.AfterFunc(vmExecHandshake, func() {
		timedOut.Store(true)
		_ = conn.Close()
	})
	sc, chans, reqs, err := ssh.NewClientConn(conn, "vm-"+x.key.Name, cfg)
	timer.Stop()
	if err == nil {
		return ssh.NewClient(sc, chans, reqs), nil
	}
	switch {
	case hostErr != nil:
		return nil, hostErr
	case ctx.Err() != nil:
		return nil, vmEnded(ctx)
	case timedOut.Load():
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("the VM's SSH did not finish its handshake in %s", vmExecHandshake))
	case shown && strings.Contains(err.Error(), "unable to authenticate"):
		return nil, failedPrecondition("the VM refused the platform's SSH certificate: made before the CA, or its sshd config changed")
	case !shown && pinned && noCommonHostKey(err):
		// The guest no longer offers a key of the kind recorded: it is
		// not the key recorded either.
		return nil, errHostKeyChanged
	}
	return nil, s.vmNodeFailed(ctx, x, conn, err)
}

// noCommonHostKey says whether err is a handshake that found no host key
// algorithm both ends take.
func noCommonHostKey(err error) bool {
	var ne *ssh.AlgorithmNegotiationError
	return errors.As(err, &ne) && ne.What == "host key"
}

// vmKeepalive asks the guest's sshd to answer at each tick, as the
// exec's own liveness pings its client, and returns errVMSilent once a
// request fails or goes unanswered for timeout: a guest gone without a
// reset, as a VM that froze, would otherwise leave the exec looking up.
// Any answer counts, OpenSSH's refusal of the request included. It times
// on the server's clock.
func (s *Server) vmKeepalive(ctx context.Context, client *ssh.Client, ticker clockwork.Ticker, timeout time.Duration) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
		// The timer runs before the request is sent, so that it bounds
		// the wait from the moment the guest could see the request.
		timer := s.clock.NewTimer(timeout)
		answered := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest(vmKeepalive, true, nil)
			answered <- err
		}()
		// A request that fails or times out once ctx has ended is the
		// client closing, not the guest falling silent.
		silent := false
		select {
		case err := <-answered:
			timer.Stop()
			silent = err != nil
		case <-timer.Chan():
			silent = true
		case <-ctx.Done():
			timer.Stop()
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if silent {
			return errVMSilent
		}
	}
}

// vmNodeFailed is what a VM's exec fails with when its connection to the
// guest failed before the login, with err: the node's call says why. A
// guest that does not take the connection, or closes it at once, has no
// sshd up yet.
func (s *Server) vmNodeFailed(ctx context.Context, x *machineTarget, conn *nodeSSHConn, err error) error {
	if ctx.Err() != nil {
		return vmEnded(ctx)
	}
	if nodeErr := conn.failure(); nodeErr != nil && !errors.Is(nodeErr, io.EOF) {
		err = x.call.failed(nodeErr)
		if connect.CodeOf(err) != connect.CodeUnavailable || x.call.tunnel.Closed() {
			return err
		}
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf(
		"SSH is not answering yet: it comes up a few seconds after the VM's first boot ends (%w)", err))
}

// vmSessionFailed is what a VM's exec fails with when its session failed
// after the login with err.
func vmSessionFailed(ctx context.Context, x *machineTarget, conn *nodeSSHConn, err error) error {
	if ctx.Err() != nil {
		return vmEnded(ctx)
	}
	if ce := new(connect.Error); errors.As(err, &ce) {
		return err // the client's, as an output not written
	}
	if nodeErr := conn.failure(); nodeErr != nil && !errors.Is(nodeErr, io.EOF) {
		return x.call.failed(nodeErr)
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("the VM's SSH session ended: %w", err))
}

// vmEnded is why a VM's exec ended once ctx has: as exec's, the client gone
// unless something else ended it.
func vmEnded(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return errClientGone
}

// firstHostKeyAlgorithms are what a first login asks for: Ed25519 first,
// as OpenSSH, and so hangar ssh, prefers it, and as people compare what
// is recorded against ssh_host_ed25519_key.pub; then the rest x/crypto
// supports, but certificates, which the platform's guests do not have.
var firstHostKeyAlgorithms = []string{
	ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512,
}

// hostKeyAlgorithms are the algorithms of the host keys vm's status
// records, pinned, or, with none, firstHostKeyAlgorithms.
func hostKeyAlgorithms(vm *computev1alpha1.VirtualMachine) (algos []string, pinned bool) {
	for _, r := range vm.GetStatus().GetSshHostKeys() {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.GetPublicKey()))
		switch {
		case err != nil:
		case k.Type() == ssh.KeyAlgoRSA:
			algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		default:
			algos = append(algos, k.Type())
		}
	}
	if len(algos) == 0 {
		return firstHostKeyAlgorithms, false
	}
	return algos, true
}

// trustHostKey checks the key the guest of x's VM presents against the
// keys its status records, recording it if there are none. It is written
// at the version read: of two first logins at once, one records its key
// and the other is checked against it.
func (s *Server) trustHostKey(ctx context.Context, x *machineTarget, key ssh.PublicKey) error {
	uid := object.Meta(x.obj).GetUid()
	for range maxRetries {
		obj, err := s.get(ctx, registry.VirtualMachine, x.key)
		if err != nil {
			return apiError(err)
		}
		vm, _ := obj.(*computev1alpha1.VirtualMachine)
		if vm == nil || vm.GetMetadata().GetUid() != uid {
			return failedPrecondition("the VM was deleted")
		}
		if recorded := vm.GetStatus().GetSshHostKeys(); len(recorded) > 0 {
			for _, r := range recorded {
				if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.GetPublicKey())); err == nil && bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil
				}
			}
			s.log.Info("ssh host key refused", append(x.auditAttrs(), "host_key", ssh.FingerprintSHA256(key))...)
			return errHostKeyChanged
		}
		status := &computev1alpha1.VirtualMachineStatus{}
		if vm.GetStatus() != nil {
			status = proto.Clone(vm.GetStatus()).(*computev1alpha1.VirtualMachineStatus)
		}
		status.SshHostKeys = []*computev1alpha1.VirtualMachineSSHHostKey{{
			PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), RecordTime: timestamppb.New(s.clock.Now()),
		}}
		rv := vm.GetMetadata().GetResourceVersion()
		_, err = s.backend(registry.VirtualMachine).UpdateStatus(s.system(ctx), x.key, status, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue // written since: read again
		}
		if err != nil {
			return err
		}
		s.log.Info("ssh host key recorded", append(x.auditAttrs(), "host_key", ssh.FingerprintSHA256(key))...)
		return nil
	}
	return connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers to the VM; retry"))
}

// execOutput writes what a VM's session sends to the exec's client, as
// stdout or stderr.
type execOutput struct {
	ctx, ws context.Context
	c       *websocket.Conn
	live    *execws.Liveness
	stderr  bool
}

func (o *execOutput) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), vmExecChunk)
		out := &computev1alpha1.MachineExecResponse{Message: &computev1alpha1.MachineExecResponse_Stdout{Stdout: p[:n]}}
		if o.stderr {
			out.Message = &computev1alpha1.MachineExecResponse_Stderr{Stderr: p[:n]}
		}
		done := o.live.Pending()
		err := writeExec(o.ws, o.c, out)
		done()
		if err != nil {
			return written, clientFailed(o.ctx, err)
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// vmRelayInput passes on what the client sends after the start to the
// session, as relayInput does to a node, until the client goes.
func vmRelayInput(ctx, ws context.Context, c *websocket.Conn, live *execws.Liveness, sess *ssh.Session, stdin io.WriteCloser, allowStdin bool) error {
	for {
		req, err := readExec(ws, c)
		if err != nil {
			return clientFailed(ctx, err)
		}
		live.Heard()
		// A failed write or request means the session is over; the relay
		// says why.
		switch m := req.GetMessage().(type) {
		case *computev1alpha1.MachineExecRequest_Stdin:
			if !allowStdin {
				return invalid("stdin for an exec started without it")
			}
			done := live.Pending()
			_, _ = stdin.Write(m.Stdin)
			done()
		case *computev1alpha1.MachineExecRequest_StdinClose:
			if !allowStdin {
				return invalid("stdin for an exec started without it")
			}
			_ = stdin.Close()
		case *computev1alpha1.MachineExecRequest_Resize:
			_ = sess.WindowChange(int(m.Resize.GetRows()), int(m.Resize.GetCols()))
		case *computev1alpha1.MachineExecRequest_Signal:
			sig, ok := sshSignals[m.Signal]
			if !ok {
				return invalid("signal %d has no name SSH sends", m.Signal)
			}
			_ = sess.Signal(sig)
		case *computev1alpha1.MachineExecRequest_Start:
			return invalid("an exec starts once")
		}
	}
}

// nodeSSHConn is the node's SSH call as a connection to the guest's sshd,
// for an SSH client, which reads it from one goroutine at a time.
type nodeSSHConn struct {
	stream interface {
		Send(*nodev1.SSHRequest) error
		Receive() (*nodev1.SSHResponse, error)
		CloseRequest() error
		CloseResponse() error
	}
	machine string // uid
	sendMu  sync.Mutex
	pending []byte
	failMu  sync.Mutex
	// fail is why reading ended: io.EOF when the guest closed the
	// connection, else the node's error.
	fail      error
	closeOnce sync.Once
}

// open names the machine to the node.
func (n *nodeSSHConn) open() error {
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	return n.stream.Send(&nodev1.SSHRequest{Message: &nodev1.SSHRequest_MachineUid{MachineUid: n.machine}})
}

func (n *nodeSSHConn) Read(p []byte) (int, error) {
	for len(n.pending) == 0 {
		res, err := n.stream.Receive()
		if err != nil {
			n.failMu.Lock()
			if n.fail == nil {
				n.fail = err
			}
			n.failMu.Unlock()
			return 0, err
		}
		n.pending = res.GetData()
	}
	k := copy(p, n.pending)
	n.pending = n.pending[k:]
	return k, nil
}

func (n *nodeSSHConn) Write(p []byte) (int, error) {
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	written := 0
	for len(p) > 0 {
		k := min(len(p), vmExecChunk)
		if err := n.stream.Send(&nodev1.SSHRequest{Message: &nodev1.SSHRequest_Data{Data: p[:k]}}); err != nil {
			return written, err
		}
		written += k
		p = p[k:]
	}
	return written, nil
}

// failure is why reading the node's call ended, nil if it has not.
func (n *nodeSSHConn) failure() error {
	n.failMu.Lock()
	defer n.failMu.Unlock()
	return n.fail
}

// Close resets the node's call, which ends a send held up on it, and ends
// the node's connection to the guest.
func (n *nodeSSHConn) Close() error {
	n.closeOnce.Do(func() {
		_ = n.stream.CloseResponse()
		n.sendMu.Lock()
		_ = n.stream.CloseRequest()
		n.sendMu.Unlock()
	})
	return nil
}

func (n *nodeSSHConn) LocalAddr() net.Addr  { return nodeAddr("apiserver") }
func (n *nodeSSHConn) RemoteAddr() net.Addr { return nodeAddr(n.machine) }

// The SSH client sets no deadlines: ctx and vmExecHandshake bound it.
func (n *nodeSSHConn) SetDeadline(time.Time) error      { return nil }
func (n *nodeSSHConn) SetReadDeadline(time.Time) error  { return nil }
func (n *nodeSSHConn) SetWriteDeadline(time.Time) error { return nil }

// nodeAddr names an end of a node's SSH call, for net.Conn.
type nodeAddr string

func (a nodeAddr) Network() string { return "hangar-node" }
func (a nodeAddr) String() string  { return string(a) }

// ForgetSSHHostKeys clears the host keys a VM's status records for its
// exec; see vm-ssh.md.
func (h machineCapabilities) ForgetSSHHostKeys(ctx context.Context, req *connect.Request[computev1alpha1.ForgetSSHHostKeysRequest]) (*connect.Response[computev1alpha1.ForgetSSHHostKeysResponse], error) {
	s := h.s
	m := req.Msg
	b := s.backend(registry.VirtualMachine)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	kind, err := kindNamed(m.GetKind())
	if err != nil {
		return nil, err
	}
	if kind.Kind != registry.VirtualMachine.Kind {
		if mk, ok := machineKinds[kind.Kind]; ok {
			return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%ss have no SSH host keys", mk.what))
		}
		return nil, invalid("%s do not run as machines", kind.Plural)
	}
	key := resource.Key{Project: m.GetProject(), Name: m.GetName()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	for range maxRetries {
		// Applying the VM, not logging in to it, is what may reset the
		// trust its logins rest on.
		cur, obj, err := b.read(ctx, p, authz.Apply, key)
		if err != nil {
			return nil, err
		}
		vm := obj.(*computev1alpha1.VirtualMachine)
		res := &computev1alpha1.ForgetSSHHostKeysResponse{}
		var prints []string
		for _, r := range vm.GetStatus().GetSshHostKeys() {
			res.Forgotten = append(res.Forgotten, r.GetPublicKey())
			if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.GetPublicKey())); err == nil {
				prints = append(prints, ssh.FingerprintSHA256(k))
			}
		}
		if len(res.Forgotten) > 0 {
			status := proto.Clone(vm.GetStatus()).(*computev1alpha1.VirtualMachineStatus)
			status.SshHostKeys = nil
			rv := cur.Revision
			_, err = b.UpdateStatus(s.system(ctx), key, status, &rv)
			if connect.CodeOf(err) == connect.CodeAborted {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		s.log.Info("ssh host keys forgotten", "principal", p.String(), "client_ip", clientIP(ctx), "project", key.Project, "kind", kind.Kind,
			"name", key.Name, "uid", vm.GetMetadata().GetUid(), "host_keys", prints)
		return connect.NewResponse(res), nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers to the VM; retry"))
}
