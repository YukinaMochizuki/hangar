package apiserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

// vmReaderRole gets virtual machines, and nothing more.
const vmReaderRole = "test.vm-reader"

func testCA(t *testing.T) (*sshca.CA, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return sshca.New(signer, time.Now), signer.PublicKey()
}

func userKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(k))
}

// signAs asks for key to be signed for the VM name, as principal p.
func signAs(t *testing.T, s *Server, p auth.Principal, name, key string) (*computev1alpha1.SignSSHKeyResponse, error) {
	t.Helper()
	srv := httptest.NewServer(s.Handler(auth.Chain{fixedAuth{p, true}}))
	t.Cleanup(srv.Close)
	res, err := computev1alpha1connect.NewVirtualMachineSSHServiceClient(http.DefaultClient, srv.URL).
		SignSSHKey(t.Context(), connect.NewRequest(&computev1alpha1.SignSSHKeyRequest{Project: "web", Name: name, PublicKey: key}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// TestSigningAKeyForAVM: the VM's owner gets a key certified for the VM's
// uid alone, for minutes, naming them, and the image's default user; one
// who may only get the VM, a node, or anyone for a VM they cannot see, is
// refused; so is a key that is none. The CA's key is never logged.
func TestSigningAKeyForAVM(t *testing.T) {
	ca, caPub := testCA(t)
	var log bytes.Buffer
	s, system, user := runningWith(t, func(c *Config) {
		c.SSHCA = ca
		c.Logger = slog.New(slog.NewJSONHandler(&log, nil))
		c.ExtraRoles = map[string]authz.Role{vmReaderRole: authz.NewRole(authz.TenantLevel, authz.Permission(registry.VirtualMachine, authz.Get))}
	})
	img := image("noble")
	img.Spec.DefaultUser = "ubuntu"
	if _, err := s.backend(registry.Image).Apply(system, img); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	for _, rb := range []*iamv1alpha1.RoleBinding{
		binding("member-bob", "bob@example.com", "platform.member", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}}),
		binding("member-carol", "carol@example.com", "platform.member", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}}),
		binding("reader-carol", "carol@example.com", vmReaderRole, &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}}),
	} {
		if _, err := s.backend(registry.RoleBinding).Apply(system, rb); err != nil {
			t.Fatal(err)
		}
	}
	uid := getVM(t, s, system, "web").GetMetadata().GetUid()
	alice := auth.Principal{Type: auth.User, ID: "alice@example.com"}
	key := userKey(t)
	res, err := signAs(t, s, alice, "web", key)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(res.GetCertificate()))
	if err != nil {
		t.Fatal(err)
	}
	cert := pub.(*ssh.Certificate)
	if res.GetPrincipal() != uid || len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != uid ||
		cert.KeyId != alice.String() || res.GetDefaultUser() != "ubuntu" ||
		!bytes.Equal(cert.SignatureKey.Marshal(), caPub.Marshal()) || strings.TrimSpace(key) != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert.Key))) {
		t.Errorf("signed %+v, response %v", cert, res)
	}
	if d := time.Until(res.GetValidBefore().AsTime()); d <= 0 || d > sshca.Validity {
		t.Errorf("valid for %v more", d)
	}
	if !strings.Contains(log.String(), `"ssh key signed"`) || strings.Contains(log.String(), "PRIVATE") {
		t.Errorf("the log:\n%s", log.String())
	}

	for _, tc := range []struct {
		who  auth.Principal
		name string
		want connect.Code
	}{
		{auth.Principal{Type: auth.User, ID: "carol@example.com"}, "web", connect.CodePermissionDenied},
		{auth.Principal{Type: auth.User, ID: "bob@example.com"}, "web", connect.CodePermissionDenied},
		{auth.Principal{Type: auth.Node, ID: "n1"}, "web", connect.CodePermissionDenied},
		{auth.Principal{Type: auth.User, ID: "bob@example.com"}, "nothing", connect.CodePermissionDenied},
		{alice, "nothing", connect.CodeNotFound},
	} {
		_, err := signAs(t, s, tc.who, tc.name, key)
		wantCodeOf(t, err, tc.want, tc.who.String()+" signing for "+tc.name)
	}
	for name, bad := range map[string]string{"no key": "", "garbage": "ssh-ed25519 !!!", "too long": strings.Repeat("a", 17<<10)} {
		_, err := signAs(t, s, alice, "web", bad)
		wantCodeOf(t, err, connect.CodeInvalidArgument, name)
	}
}

// TestSigningWithoutACA: without the CA's key, the apiserver runs, and
// signing fails saying why; the VM's guest gets its static keys alone.
func TestSigningWithoutACA(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("web", "noble")
	vm.Spec.SshAuthorizedKeys = []string{sshKey("ssh-ed25519")}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	_, err := signAs(t, s, auth.Principal{Type: auth.User, ID: "alice@example.com"}, "web", userKey(t))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "signing without a CA")
	if err != nil && !strings.Contains(err.Error(), "SSH CA") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	spec, err := vmMachineSpec(getVM(t, s, system, "web"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.GetFirmware().GetCloudInit().GetVendorData(); got != keysConfig(vm.Spec) {
		t.Errorf("vendor-data without a CA:\n%s", got)
	}
}

// TestVendorDataTrustsTheCA: with a CA, vendor-data is multipart: the keys'
// cloud-config, and a script, which sh reads, trusting the CA for the VM's
// uid and every user but root.
func TestVendorDataTrustsTheCA(t *testing.T) {
	ca, _ := testCA(t)
	vm := vmFromImage("web", "noble")
	vm.Metadata.Uid = "0199-uid"
	vm.Spec.SshAuthorizedKeys = []string{sshKey("ssh-ed25519")}
	got := vendorData(vm, ca.PublicKey())
	for _, want := range []string{
		"Content-Type: multipart/mixed; boundary=\"" + vendorBoundary + "\"",
		"Content-Type: text/cloud-config", keysConfig(vm.Spec),
		"Content-Type: text/x-shellscript", ca.PublicKey(), "printf '%s\\n' '0199-uid'",
		`[ "$1" = root ] || cat /etc/ssh/hangar/principals`,
		"TrustedUserCAKeys /etc/ssh/hangar/ca.pub", "AuthorizedPrincipalsCommand /etc/ssh/hangar/principals-for %u",
		// sshd -t refuses every config while a socket-activated sshd has
		// not started yet, without it.
		"install -d -m 0755 /run/sshd\nif ! \"$sshd\" -t; then",
		"--" + vendorBoundary + "--\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("vendor-data lacks %q:\n%s", want, got)
		}
	}
	script := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(script, []byte(sshScript(ca.PublicKey(), "0199-uid")), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", script).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v: %s", err, out)
	}
	vm.Spec.SshAuthorizedKeys = nil
	if got := vendorData(vm, ca.PublicKey()); strings.Contains(got, "text/cloud-config") || !strings.Contains(got, "x-shellscript") {
		t.Errorf("vendor-data of a VM without keys:\n%s", got)
	}
}

// TestImageDefaultUser: an image's default user is a login name.
func TestImageDefaultUser(t *testing.T) {
	s, system, _ := running(t)
	for user, ok := range map[string]bool{"ubuntu": true, "": true, "ec2-user": true, "Root": false, "a b": false, "x;rm": false, strings.Repeat("a", 33): false} {
		img := image("noble")
		img.Spec.DefaultUser = user
		_, err := s.backend(registry.Image).Apply(system, img)
		if (err == nil) != ok {
			t.Errorf("default user %q: %v", user, err)
		}
	}
}

// TestTheCAsScriptCountsAgainstVendorData: keys whose cloud-config alone
// fits the machine's vendor-data are refused when the CA's script would
// take it past its limit, and a VM admitted gets its machine.
func TestTheCAsScriptCountsAgainstVendorData(t *testing.T) {
	ca, _ := testCA(t)
	s, system, user := runningWith(t, func(c *Config) { c.SSHCA = ca })
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("web", "noble")
	vm.Spec.SshAuthorizedKeys = keysQuotedTo(t, maxVendorData-100)
	_, err := s.backend(registry.VirtualMachine).Apply(user, vm)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "keys that fit vendor-data only without the CA's script")
	room := maxVendorData - (vendorDataSize(vm, ca.PublicKey()) - len(keysConfig(vm.Spec)))
	vm.Spec.SshAuthorizedKeys = keysQuotedTo(t, room)
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatalf("keys that fit with the CA's script: %v", err)
	}
	spec, err := vmMachineSpec(getVM(t, s, system, "web"), ca.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(spec.GetFirmware().GetCloudInit().GetVendorData()); n != maxVendorData {
		t.Errorf("vendor-data of %d bytes, want %d", n, maxVendorData)
	}
}

// tunnelAs opens the VM's SSH tunnel as principal p, returning the error a
// refusal before the upgrade says.
func tunnelAs(t *testing.T, s *Server, p auth.Principal, name string) error {
	t.Helper()
	srv := httptest.NewServer(s.Handler(auth.Chain{fixedAuth{p, true}}))
	t.Cleanup(srv.Close)
	c, resp, err := websocket.Dial(t.Context(), srv.URL+sshws.Path("web", name), &websocket.DialOptions{Subprotocols: []string{sshws.Subprotocol}})
	if err == nil {
		_ = c.CloseNow()
		return nil
	}
	if resp == nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return execws.ReadError(resp)
}

// sshTunnelWorld has a VM, web, whose machine runs on n1, whose tunnel is
// not connected; carol may get VMs and nothing more; bob is a member.
func sshTunnelWorld(t *testing.T) (*Server, context.Context, *computev1alpha1.Machine) {
	t.Helper()
	s, system, user := runningWith(t, func(c *Config) {
		c.Nodes = []string{"n1"}
		c.ExtraRoles = map[string]authz.Role{vmReaderRole: authz.NewRole(authz.TenantLevel, authz.Permission(registry.VirtualMachine, authz.Get))}
	})
	for _, rb := range []*iamv1alpha1.RoleBinding{
		binding("member-bob", "bob@example.com", "platform.member", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}}),
		binding("member-carol", "carol@example.com", "platform.member", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}}),
		binding("reader-carol", "carol@example.com", vmReaderRole, &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}}),
	} {
		if _, err := s.backend(registry.RoleBinding).Apply(system, rb); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("web-root", "20Gi")); err != nil {
		t.Fatal(err)
	}
	m := machine(vmMachineName("web"), "n1")
	m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{RootDisk: "web-root"}}
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.VirtualMachine.APIVersion(), Kind: registry.VirtualMachine.Kind, Name: "web",
		Uid: getVM(t, s, system, "web").GetMetadata().GetUid(), Controller: true,
	}}
	return s, system, m
}

// TestTheSSHTunnelNeedsItsPermission: a member who may get the VM but not
// log in to it, or may do nothing in its tenant, is refused before anything
// about the VM is looked at; its owner gets as far as the node.
func TestTheSSHTunnelNeedsItsPermission(t *testing.T) {
	s, system, m := sshTunnelWorld(t)
	applied, err := s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, s, system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	for who, want := range map[string]connect.Code{
		"carol@example.com": connect.CodePermissionDenied,
		"bob@example.com":   connect.CodePermissionDenied,
		"alice@example.com": connect.CodeUnavailable, // n1 is not connected
	} {
		err := tunnelAs(t, s, auth.Principal{Type: auth.User, ID: who}, "web")
		wantCodeOf(t, err, want, who+" opening web's tunnel")
	}
	wantCodeOf(t, tunnelAs(t, s, auth.Principal{Type: auth.Node, ID: "n1"}, "web"), connect.CodePermissionDenied, "the node opening a tunnel")
}

// TestTheSSHTunnelTakesOnlyTheVMsOwnMachine: a machine of the VM's name that
// another VM of that name owned, as one deleted leaves it while it goes, is
// not reached; nor is the VM's own while it does not run.
func TestTheSSHTunnelTakesOnlyTheVMsOwnMachine(t *testing.T) {
	s, system, m := sshTunnelWorld(t)
	alice := auth.Principal{Type: auth.User, ID: "alice@example.com"}
	theirs := m.Metadata.OwnerReferences[0].Uid
	m.Metadata.OwnerReferences[0].Uid = "the-deleted-vms-uid"
	applied, err := s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, s, system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	err = tunnelAs(t, s, alice, "web")
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "the tunnel to an old VM's machine")
	if err != nil && !strings.Contains(err.Error(), "has no machine yet") {
		t.Errorf("the refusal: %v", err)
	}

	m.Metadata.OwnerReferences[0].Uid = theirs
	applied, err = s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, s, system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, applied.Object.(*computev1alpha1.Machine).GetMetadata().GetGeneration())
	err = tunnelAs(t, s, alice, "web")
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "the tunnel to a stopped VM")
	if err != nil && !strings.Contains(err.Error(), "is not running") {
		t.Errorf("the refusal: %v", err)
	}
}
