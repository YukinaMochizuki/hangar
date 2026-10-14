package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

// sshWorld is a world whose apiserver has an SSH CA, an image with a
// default user, and a running VM, web.
func sshWorld(t *testing.T) (*world, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caSigner, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.SSHCA = sshca.New(caSigner, time.Now) })
	w.startAgent()
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	if _, err := w.root.Apply(context.Background(), registry.Image, &storagev1alpha1.Image{
		Metadata: &metav1.ObjectMeta{Name: "noble"},
		Spec:     &storagev1alpha1.ImageSpec{Url: cloudURL, Sha256: cloudSHA, Os: "ubuntu", Version: "24.04", DefaultUser: "ubuntu"},
	}); err != nil {
		t.Fatal(err)
	}
	return w, caSigner.PublicKey()
}

func (w *world) applyVMOn(name string, networks ...string) {
	w.t.Helper()
	vm := &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
				FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}},
			}},
		},
	}
	for _, n := range networks {
		vm.Spec.Interfaces = append(vm.Spec.Interfaces, &computev1alpha1.NetworkInterface{Network: n})
	}
	if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, vm); err != nil {
		w.t.Fatal(err)
	}
	w.waitVMReason(name, "Running")
}

// vmAddrs are the addresses of a VM's interfaces, by network.
func (w *world) vmAddrs(name string) map[string]netip.Addr {
	w.t.Helper()
	out := map[string]netip.Addr{}
	for _, nic := range w.machine2(vmMachineOf(name)).GetSpec().GetInterfaces() {
		out[nic.GetNetwork()] = netip.MustParsePrefix(nic.GetIpv4()).Addr()
	}
	return out
}

// tunnel opens a VM's SSH tunnel as user.
func (w *world) tunnel(user, name string) (*websocket.Conn, error) {
	w.t.Helper()
	c, resp, err := websocket.Dial(context.Background(), w.env.URL+sshws.Path(project, name), &websocket.DialOptions{
		HTTPHeader: http.Header{auth.DevUserHeader: []string{user}}, Subprotocols: []string{sshws.Subprotocol},
	})
	if err != nil {
		if resp == nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, execws.ReadError(resp)
		}
		return nil, err
	}
	w.t.Cleanup(func() { _ = c.CloseNow() })
	return c, nil
}

// banner serves what an sshd would at one place: says who it is, then
// echoes.
func banner(who string) func(net.Conn) {
	return func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = fmt.Fprintf(c, "%s\n", who)
		buf := make([]byte, 1024)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			if _, err := c.Write(buf[:n]); err != nil {
				return
			}
		}
	}
}

// TestTheSSHTunnelReachesOnlyItsVM: a VM's tunnel reaches port 22 of its own
// first interface, in that interface's network, and nothing else: not
// another VM, port, address or network, which the caller cannot name. One
// without compute.virtualmachines.ssh is refused before anything is
// dialled, and a VM that is not running is not reached.
func TestTheSSHTunnelReachesOnlyItsVM(t *testing.T) {
	w, _ := sshWorld(t)
	if _, err := w.root.Apply(context.Background(), registry.Network, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "backend"},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.1.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	w.applyVMOn("web", "default", "backend")
	w.applyVMOn("db")
	web, db := w.vmAddrs("web"), w.vmAddrs("db")
	def, backend := w.networkUID("default"), w.networkUID("backend")
	w.host.ServeGuest(def, netip.AddrPortFrom(web["default"], 22), banner("web"))
	w.host.ServeGuest(def, netip.AddrPortFrom(web["default"], 2222), banner("web, another port"))
	w.host.ServeGuest(backend, netip.AddrPortFrom(web["backend"], 22), banner("web on backend"))
	w.host.ServeGuest(def, netip.AddrPortFrom(db["default"], 22), banner("db"))
	w.host.ServeGuest(def, netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), 22), banner("the gateway"))

	// Refused before anything is dialled.
	for _, user := range []string{"mallory@example.com"} {
		_, err := w.tunnel(user, "web")
		if err == nil || !strings.Contains(err.Error(), "permission_denied") {
			t.Errorf("%s opening web's tunnel: %v", user, err)
		}
	}
	if dials := w.host.Dials(); len(dials) != 0 {
		t.Errorf("refused, yet dialled %v", dials)
	}

	c, err := w.tunnel(apiservertest.Root, "web")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	read := func() string {
		t.Helper()
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read(); got != "web\n" {
		t.Errorf("web's tunnel reached %q", got)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("SSH-2.0-test\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "SSH-2.0-test\r\n" {
		t.Errorf("echoed %q", got)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	if want := []string{def + " " + web["default"].String() + ":22"}; !slices.Equal(w.host.Dials(), want) {
		t.Errorf("dialled %v, want %v", w.host.Dials(), want)
	}

	// Its own guest gone from port 22, nothing else is tried.
	w.host.ServeGuest(def, netip.AddrPortFrom(web["default"], 22), nil)
	if c, err := w.tunnel(apiservertest.Root, "web"); err == nil {
		if _, _, err := c.Read(ctx); err == nil || !strings.Contains(execws.ErrorOf(err).Error(), "port 22") {
			t.Errorf("a tunnel to nothing: %v", err)
		}
	}
	if dials := w.host.Dials(); len(dials) != 2 || dials[1] != dials[0] {
		t.Errorf("dialled %v", dials)
	}

	// Stopped, it is not reached.
	w.applyVM("db", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	w.waitVMReason("db", "Stopped")
	if _, err := w.tunnel(apiservertest.Root, "db"); err == nil || !strings.Contains(err.Error(), "failed_precondition") {
		t.Errorf("a stopped VM's tunnel: %v", err)
	}
}

// guestSSHD is a stand-in for a guest's sshd with vendor-data's settings: it
// takes a certificate of the CA for the VM's uid, for any user but root, and
// runs a command by saying it back.
func guestSSHD(t *testing.T, ca ssh.PublicKey, principal string, host ssh.Signer) func(net.Conn) {
	checker := &ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool { return bytes.Equal(k.Marshal(), ca.Marshal()) }}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		cert, ok := key.(*ssh.Certificate)
		if !ok || meta.User() == "root" {
			return nil, errors.New("no")
		}
		if err := checker.CheckCert(principal, cert); err != nil {
			return nil, err
		}
		return &ssh.Permissions{Extensions: map[string]string{"user": meta.User(), "key-id": cert.KeyId}}, nil
	}}
	cfg.AddHostKey(host)
	return func(c net.Conn) {
		conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			if nc.ChannelType() != "session" {
				_ = nc.Reject(ssh.UnknownChannelType, "")
				continue
			}
			ch, chReqs, err := nc.Accept()
			if err != nil {
				return
			}
			go func() {
				for req := range chReqs {
					if req.Type != "exec" || len(req.Payload) < 4 {
						_ = req.Reply(false, nil)
						continue
					}
					_ = req.Reply(true, nil)
					cmd := string(req.Payload[4 : 4+binary.BigEndian.Uint32(req.Payload)])
					_, _ = fmt.Fprintf(ch, "%s ran %q as %s\n", conn.Permissions.Extensions["key-id"], cmd, conn.User())
					_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, 0))
					_ = ch.Close()
				}
			}()
		}
	}
}

// hangarBinary builds the CLI, which ssh runs as its ProxyCommand.
func hangarBinary(t *testing.T) string {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	bin := filepath.Join(t.TempDir(), "hangar")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/hangar")
	cmd.Dir = filepath.Join(filepath.Dir(self), "..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building hangar: %v: %s", err, out)
	}
	return bin
}

// TestHangarSSH: hangar ssh logs in to a VM with OpenSSH's ssh and a key
// signed for minutes, through ssh-proxy, as the image's default user or -l
// another, not root; it records the VM's host key, refuses a changed one,
// saying how to forget it, and takes it again once forgotten.
func TestHangarSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh on PATH")
	}
	w, ca := sshWorld(t)
	w.applyVMOn("web")
	vm, _ := get[*computev1alpha1.VirtualMachine](w, registry.VirtualMachine, "web")
	uid := vm.GetMetadata().GetUid()
	hostKey := func() ssh.Signer {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		s, _ := ssh.NewSignerFromKey(priv)
		return s
	}
	at := netip.AddrPortFrom(w.vmAddrs("web")["default"], 22)
	def := w.networkUID("default")
	w.host.ServeGuest(def, at, guestSSHD(t, ca, uid, hostKey()))

	// A % in the config's path, which ssh would take as a token in the
	// ProxyCommand and known_hosts paths if hangar ssh left it.
	dir := filepath.Join(t.TempDir(), "100%d %h")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.yaml")
	self := hangarBinary(t)
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := cli.Run(context.Background(), args, cli.IO{In: strings.NewReader(""), Out: &out, Err: &errb, Env: func(k string) string {
			if k == "HANGAR_CONFIG" {
				return config
			}
			return os.Getenv(k)
		}}, cli.Options{Self: self})
		return code, out.String(), errb.String()
	}
	if code, _, e := run("config", "set-context", "dev", "--endpoint", w.env.URL, "--auth", "dev", "--user", apiservertest.Root, "--project", project, "--use"); code != 0 {
		t.Fatal(e)
	}

	code, out, errOut := run("ssh", "vm/web", "--", "uname", "-a")
	if want := fmt.Sprintf("user:%s ran \"uname -a\" as ubuntu\n", apiservertest.Root); code != 0 || out != want {
		t.Fatalf("hangar ssh: exit %d: %q, want %q\n%s", code, out, want, errOut)
	}
	known, err := os.ReadFile(filepath.Join(filepath.Dir(config), "known_hosts"))
	if err != nil || !strings.HasPrefix(string(known), uid+" ssh-ed25519 ") {
		t.Errorf("known_hosts %q %v", known, err)
	}
	if code, out, _ := run("ssh", "-l", "alice", "virtualmachine/web", "--", "id"); code != 0 || !strings.HasSuffix(out, " as alice\n") {
		t.Errorf("-l alice: exit %d: %q", code, out)
	}
	if code, out, _ := run("ssh", "-l", "root", "vm/web", "--", "id"); code == 0 {
		t.Errorf("logged in as root: %q", out)
	}

	// The VM's host key changes, as when it is made again.
	w.host.ServeGuest(def, at, guestSSHD(t, ca, uid, hostKey()))
	code, _, errOut = run("ssh", "vm/web", "--", "true")
	if code == 0 || !strings.Contains(errOut, "hangar ssh --forget-host-key vm/web") {
		t.Errorf("a changed host key: exit %d:\n%s", code, errOut)
	}
	if code, out, _ := run("ssh", "--forget-host-key", "vm/web"); code != 0 || out != "forgot virtualmachine/web's host key\n" {
		t.Errorf("--forget-host-key: exit %d: %q", code, out)
	}
	if code, _, errOut := run("ssh", "vm/web", "--", "true"); code != 0 {
		t.Errorf("after forgetting the host key: exit %d:\n%s", code, errOut)
	}
}
