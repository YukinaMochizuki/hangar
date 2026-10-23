package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

func (a *app) sshCmd() *cobra.Command {
	var login string
	var forget bool
	cmd := &cobra.Command{
		Use:   "ssh KIND NAME [-- COMMAND [ARG...]]",
		Short: "Log in to a virtual machine, or run a command in it, over SSH",
		Long: "Logs in to the virtual machine as its image's default user, or -l USER, with\n" +
			"OpenSSH's ssh and a key made for this login alone, signed by the platform\n" +
			"for minutes, through hangar ssh-proxy: no key to set up, and no port of\n" +
			"the VM's open to the internet. It needs compute.machines.ssh.\n" +
			"As with ssh, COMMAND and its ARGs are joined into one line, which the\n" +
			"user's shell on the VM runs.\n\n" +
			"The VM's host key is recorded on first use in known_hosts beside the\n" +
			"config file (~/.config/hangar/known_hosts), by the VM's uid: a VM made\n" +
			"again is a new entry. A key that changes under the same uid, the VM's\n" +
			"host keys made again in it or something else answering, is refused\n" +
			"until --forget-host-key.",
		Example: "  hangar ssh vm web\n  hangar ssh vm web -- uname -a\n  hangar ssh vm web -- 'df -h / | tail -1'\n  hangar ssh -l alice vm web\n  hangar ssh --forget-host-key vm web",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, remote, err := sshArgs(cmd, args)
			if err != nil {
				return err
			}
			if forget {
				if len(remote) > 0 || login != "" {
					return usageErr("--forget-host-key takes the VM alone")
				}
				return a.forgetHostKey(cmd, name)
			}
			return a.runSSH(cmd, name, login, remote)
		},
	}
	cmd.Flags().StringVarP(&login, "login", "l", "", "log in as this user instead of the image's default user")
	cmd.Flags().BoolVar(&forget, "forget-host-key", false, "forget the VM's host key recorded in this CLI's known_hosts, as when its host keys were made again in it; the console's terminal keeps its own record")
	return cmd
}

func (a *app) sshProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh-proxy KIND NAME",
		Short: "Connect stdin and stdout to a virtual machine's SSH port, as an OpenSSH ProxyCommand",
		Long: "Relays stdin and stdout to port 22 of the virtual machine's first\n" +
			"interface, through the API: what hangar ssh gives ssh as its ProxyCommand,\n" +
			"which an ssh_config may name too. It needs compute.machines.ssh.",
		Example: "  ssh -o ProxyCommand='hangar ssh-proxy vm web' ubuntu@web",
		Args:    args(2, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := vmName(args[0], args[1])
			if err != nil {
				return err
			}
			return a.sshProxy(cmd.Context(), name)
		},
	}
}

// sshArgs splits hangar ssh's arguments into the VM's name and the remote
// command, after "--".
func sshArgs(cmd *cobra.Command, args []string) (string, []string, error) {
	before, remote := args, []string(nil)
	if at := cmd.ArgsLenAtDash(); at >= 0 {
		before, remote = args[:at], args[at:]
	}
	if len(before) != 2 {
		return "", nil, usageErr("want KIND NAME, then -- and the command")
	}
	name, err := vmName(before[0], before[1])
	return name, remote, err
}

// vmName checks that KIND NAME names a virtual machine, and returns its name.
// The kind is asked for although only VMs take SSH, so that other kinds can
// later without the command changing shape.
func vmName(kind, name string) (string, error) {
	r, err := findKind(kind)
	if err != nil {
		return "", err
	}
	if r.Kind != registry.VirtualMachine.Kind {
		return "", usageErr("only virtual machines take SSH, not %s", r.Plural)
	}
	return name, nil
}

// knownHosts is the file hangar ssh keeps VMs' host keys in, by uid.
func (a *app) knownHosts() string {
	return filepath.Join(filepath.Dir(a.configPath()), "known_hosts")
}

// sshContext is what hangar ssh and ssh-proxy reach the API with.
type sshContext struct {
	project string
	c       *Context
	creds   credentials
}

func (a *app) sshContext() (*sshContext, error) {
	c, err := a.current()
	if err != nil {
		return nil, err
	}
	project, err := a.projectFor(registry.VirtualMachine, c)
	if err != nil {
		return nil, err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return nil, err
	}
	return &sshContext{project: project, c: c, creds: creds}, nil
}

func (a *app) forgetHostKey(cmd *cobra.Command, name string) error {
	sc, err := a.sshContext()
	if err != nil {
		return err
	}
	hc, base := a.httpClient(sc.c)
	o, err := client.New(hc, base, connect.WithInterceptors(sc.creds)).Get(cmd.Context(), registry.VirtualMachine, client.Key{Project: sc.project, Name: name})
	if err != nil {
		return err
	}
	uid := o.(*computev1alpha1.VirtualMachine).GetMetadata().GetUid()
	path := a.knownHosts()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b, err = nil, nil
	}
	if err != nil {
		return err
	}
	var kept []byte
	forgot := false
	for line := range bytes.Lines(b) {
		if f := bytes.Fields(line); len(f) > 0 && string(f[0]) == uid {
			forgot = true
			continue
		}
		kept = append(kept, line...)
	}
	if forgot {
		if err := writePrivate(path, kept); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "forgot virtualmachine/%s's host key\n", name)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "virtualmachine/%s has no host key recorded\n", name)
	return nil
}

// hostKeyChanged is what ssh says of a host key that is not the one
// recorded.
var hostKeyChanged = []string{"REMOTE HOST IDENTIFICATION HAS CHANGED", "Host key verification failed"}

func (a *app) runSSH(cmd *cobra.Command, name, login string, remote []string) error {
	ctx := cmd.Context()
	sc, err := a.sshContext()
	if err != nil {
		return err
	}
	sshBin := a.opts.SSH
	if sshBin == "" {
		if sshBin, err = exec.LookPath("ssh"); err != nil {
			return fmt.Errorf("hangar ssh runs OpenSSH's ssh, which is not on PATH: %w", err)
		}
	}
	self := a.opts.Self
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return err
		}
	}

	dir, err := os.MkdirTemp("", "hangar-ssh-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hangar ssh "+name)
	if err != nil {
		return err
	}
	keyFile, certFile := filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "id_ed25519-cert.pub")
	if err := writePrivate(keyFile, pem.EncodeToMemory(block)); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	hc, base := a.httpClient(sc.c)
	signer := computev1alpha1connect.NewMachineCapabilityServiceClient(hc, base, connect.WithInterceptors(sc.creds))
	res, err := signer.SignSSHKey(ctx, connect.NewRequest(&computev1alpha1.SignSSHKeyRequest{
		Project: sc.project, Kind: registry.VirtualMachine.Kind, Name: name, PublicKey: string(ssh.MarshalAuthorizedKey(sshPub)),
	}))
	if err != nil {
		return err
	}
	if err := writePrivate(certFile, []byte(res.Msg.GetCertificate()+"\n")); err != nil {
		return err
	}
	user := login
	if user == "" {
		user = res.Msg.GetDefaultUser()
	}
	if user == "" {
		return usageErr("virtualmachine/%s's image names no default user: say whom to log in as, with -l USER", name)
	}
	if err := mkdirPrivate(filepath.Dir(a.knownHosts())); err != nil {
		return err
	}

	proxy := []string{self}
	if a.contextName != "" {
		proxy = append(proxy, "--context", a.contextName)
	}
	proxy = append(proxy, "--project", sc.project, "ssh-proxy", "virtualmachine", name)
	proxyCmd := proxyCommand(proxy)
	for _, p := range []string{keyFile, certFile, a.knownHosts()} {
		if strings.Contains(p, `"`) {
			return fmt.Errorf("ssh takes no path with a double quote, as %s has", p)
		}
	}
	argv := []string{
		"-i", sshLiteral(keyFile), "-o", "CertificateFile=" + sshPath(certFile), "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
		"-o", "ProxyCommand=" + sshLiteral(proxyCmd),
		"-o", "HostKeyAlias=" + res.Msg.GetPrincipal(), "-o", "UserKnownHostsFile=" + sshPath(a.knownHosts()),
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=accept-new", "-o", "HashKnownHosts=no",
		"-o", "CheckHostIP=no", "-o", "UpdateHostKeys=no",
		"-l", user, "vm-" + name,
	}
	if len(remote) > 0 {
		argv = append(append(argv, "--"), remote...)
	}
	c := exec.CommandContext(ctx, sshBin, argv...)
	// ssh-proxy reads the config this does.
	c.Env = append(os.Environ(), "HANGAR_CONFIG="+a.configPath())
	var said tail
	c.Stdin, c.Stdout, c.Stderr = a.io.In, a.io.Out, io.MultiWriter(a.io.Err, &said)
	// ssh takes the terminal's interrupts itself: hangar waits for it.
	take := func(os.Signal) bool { return true }
	a.takeInterrupts.Store(&take)
	defer a.takeInterrupts.Store(nil)
	err = c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	if ee.ExitCode() == 255 && said.contains(hostKeyChanged) {
		return fmt.Errorf("virtualmachine/%s's host key is not the one recorded in %s: something else answers, or its host keys were "+
			"made again in it; if they were, forget the old one with hangar ssh --forget-host-key vm %s and connect again", name, a.knownHosts(), name)
	}
	return exitStatus(ee.ExitCode())
}

// tail keeps the last of what ssh said, to tell why it failed.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 16<<10 {
		t.buf = t.buf[len(t.buf)-16<<10:]
	}
	return len(p), nil
}

func (t *tail) contains(any []string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range any {
		if bytes.Contains(t.buf, []byte(s)) {
			return true
		}
	}
	return false
}

// sshLiteral keeps ssh from expanding % tokens in s, as it does in a
// ProxyCommand and in the paths of keys and known_hosts.
func sshLiteral(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// sshPath is a path as an ssh option's value: quoted, as a known_hosts
// option takes several paths split at blanks, and kept from expanding.
// Within quotes ssh takes a backslash before a backslash or quote as an
// escape, so a Windows path is given with slashes, which it also takes.
func sshPath(p string) string { return `"` + sshLiteral(filepath.ToSlash(p)) + `"` }

// sshProxy relays stdin and stdout to the VM's port 22 until either ends.
func (a *app) sshProxy(ctx context.Context, name string) error {
	sc, err := a.sshContext()
	if err != nil {
		return err
	}
	header := http.Header{}
	if err := sc.creds.set(ctx, header.Set); err != nil {
		return err
	}
	every, timeout := a.opts.ExecPing, a.opts.ExecPong
	if every <= 0 {
		every = defaultExecPing
	}
	if timeout <= 0 {
		timeout = defaultExecPong
	}
	live := execws.NewLiveness(every, timeout)
	hc, base := a.execHTTPClient(sc.c)
	conn, resp, err := websocket.Dial(ctx, base+sshws.Path(sc.project, registry.VirtualMachine.Plural, name), &websocket.DialOptions{
		HTTPClient: hc, HTTPHeader: header, Subprotocols: []string{sshws.Subprotocol},
		OnPingReceived: live.OnPing, OnPongReceived: live.OnPong,
	})
	if err != nil {
		if resp == nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSwitchingProtocols {
			return execws.ReadError(resp)
		}
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(sshws.MaxMessage)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ws := context.WithoutCancel(ctx)
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stopOnCancel()
	go func() {
		if live.Run(ctx, conn) != nil {
			cancel(errors.New("the server stopped answering"))
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := a.io.In.Read(buf)
			if n > 0 {
				done := live.Pending()
				werr := conn.Write(ws, websocket.MessageBinary, buf[:n])
				done()
				if werr != nil {
					return
				}
			}
			if err != nil {
				// ssh is done with the connection.
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}()
	for {
		typ, b, err := conn.Read(ws)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return execws.ErrorOf(err)
		}
		live.Heard()
		if typ != websocket.MessageBinary {
			continue
		}
		if _, err := a.io.Out.Write(b); err != nil {
			return err
		}
	}
}
