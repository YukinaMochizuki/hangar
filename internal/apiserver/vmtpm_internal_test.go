package apiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func tpmVM(name string) *computev1alpha1.VirtualMachine {
	vm := vmFromImage(name, "noble")
	vm.Spec.Tpm = &computev1alpha1.VirtualMachineTPM{Enabled: true}
	return vm
}

// storedVM is the VM as the store holds it, sealed key and all.
func storedVM(t *testing.T, s *Server, system context.Context, name string) *computev1alpha1.VirtualMachine {
	t.Helper()
	vm := getVM(t, s, system, name)
	if vm == nil {
		t.Fatalf("no virtual machine %s", name)
	}
	return vm
}

// tpmKeyOf opens the VM's TPM key as the apiserver does.
func tpmKeyOf(t *testing.T, s *Server, system context.Context, name string) []byte {
	t.Helper()
	key, err := s.openVMTPMKey(storedVM(t, s, system, name))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// noTPMKey fails if an answered VM holds its sealed key, or the key in any
// encoding.
func noTPMKey(t *testing.T, what string, key []byte, msgs ...proto.Message) {
	t.Helper()
	for _, m := range msgs {
		vm, ok := m.(*computev1alpha1.VirtualMachine)
		if !ok {
			continue
		}
		if vm.GetSpec().GetTpm().GetSealedKey() != nil {
			t.Errorf("%s: answered the sealed key", what)
		}
		b, err := proto.Marshal(vm)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, key) {
			t.Errorf("%s: answered the key", what)
		}
	}
}

// TestATPMKeyIsNeverAnswered: a VM made with a TPM gets a key of 32 bytes,
// sealed for it, which no apply, get, list or watch answers, to its user or
// to a node, nor the log or the database files hold; applying what get
// answered back keeps the key, and changes nothing.
func TestATPMKeyIsNeverAnswered(t *testing.T) {
	t.Parallel()
	log := &lockedBuffer{}
	dir := t.TempDir()
	s, system, user := runningAt(t, filepath.Join(dir, "hangar.db"), func(c *Config) {
		withSecrets(keyring(t, secrets.NewKEK("k1")), log)(c)
		c.Nodes = []string{"n1"}
	})
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	b := s.backend(registry.VirtualMachine)
	since, err := s.store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// watcher is one watch's events, and how it ended.
	type watcher struct {
		who    string
		mu     sync.Mutex
		events []resource.Event
		cancel context.CancelFunc
		done   chan error
	}
	watch := func(who string, ctx context.Context) *watcher {
		w := &watcher{who: who, done: make(chan error, 1)}
		ctx, w.cancel = context.WithCancel(ctx)
		go func() {
			w.done <- b.Watch(ctx, resource.WatchOptions{Project: "web", SinceRevision: since}, func(ev resource.Event) error {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.events = append(w.events, ev)
				return nil
			})
		}()
		return w
	}
	watchers := []*watcher{watch("the user's watch", user), watch("the node's watch", asNode(user, "n1"))}

	res, err := b.Apply(user, tpmVM("web"))
	if err != nil {
		t.Fatal(err)
	}
	stored := storedVM(t, s, system, "web")
	sealed := stored.GetSpec().GetTpm().GetSealedKey()
	if sealed.GetKekId() != "k1" || len(sealed.GetCiphertext()) == 0 {
		t.Fatalf("stored sealed key %v", sealed)
	}
	key := tpmKeyOf(t, s, system, "web")
	if len(key) != 32 {
		t.Fatalf("a key of %d bytes", len(key))
	}
	// Sealed for this VM alone: not as a secret, not for another VM.
	if _, err := s.cfg.Secrets.Open(secrets.Object("web", "web", stored.GetMetadata().GetUid()), sealed); err == nil {
		t.Error("the TPM key opens as a secret's data of the VM's name")
	}
	noTPMKey(t, "apply", key, res.Object)
	if res.Object.(*computev1alpha1.VirtualMachine).GetSpec().GetTpm().GetEnabled() != true {
		t.Error("the answer lost spec.tpm.enabled")
	}
	for who, ctx := range map[string]context.Context{"user": user, "node": asNode(user, "n1")} {
		got, err := b.Get(ctx, resource.Key{Project: "web", Name: "web"})
		if err != nil {
			t.Fatal(err)
		}
		noTPMKey(t, who+" get", key, got)
		for _, project := range []string{"web", ""} {
			list, err := b.List(ctx, resource.ListOptions{Project: project})
			if err != nil || len(list.Items) != 1 {
				t.Fatalf("%s list in %q: %v %v", who, project, list.Items, err)
			}
			noTPMKey(t, who+" list", key, list.Items...)
		}
	}

	got, _ := b.Get(user, resource.Key{Project: "web", Name: "web"})
	rv := got.(*computev1alpha1.VirtualMachine).GetMetadata().GetResourceVersion()
	again, err := b.Apply(user, got)
	if err != nil {
		t.Fatalf("applying what get answered: %v", err)
	}
	noTPMKey(t, "apply again", key, again.Object)
	if m := again.Object.(*computev1alpha1.VirtualMachine).GetMetadata(); m.GetResourceVersion() != rv || m.GetGeneration() != 1 {
		t.Errorf("applying what get answered: version %d, generation %d; want %d and 1", m.GetResourceVersion(), m.GetGeneration(), rv)
	}
	if !proto.Equal(storedVM(t, s, system, "web").GetSpec().GetTpm().GetSealedKey(), sealed) {
		t.Error("applying what get answered changed the sealed key")
	}
	deleted, err := b.Delete(user, resource.Key{Project: "web", Name: "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	noTPMKey(t, "delete", key, deleted)
	for _, w := range watchers {
		// Its apply, the status writes, and its delete: at least one, and
		// the delete last.
		testutil.Eventually(t, w.who+" seeing the VM's delete", func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			return slices.ContainsFunc(w.events, func(ev resource.Event) bool { return ev.Type == metav1.EventType_EVENT_TYPE_DELETED })
		})
		w.cancel()
		if err := <-w.done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("%s ended: %v", w.who, err)
		}
		w.mu.Lock()
		if len(w.events) == 0 {
			t.Errorf("%s saw nothing", w.who)
		}
		for _, ev := range w.events {
			if ev.Object != nil {
				noTPMKey(t, w.who+" "+ev.Type.String(), key, ev.Object)
			}
		}
		w.mu.Unlock()
	}
	for _, needle := range [][]byte{key, []byte(hex.EncodeToString(key)), []byte(base64.StdEncoding.EncodeToString(key))} {
		if strings.Contains(log.String(), string(needle)) {
			t.Error("the log holds the key")
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "hangar.db*"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, key) {
			t.Errorf("%s holds the TPM key in the clear", filepath.Base(f))
		}
	}
}

// TestATPMIsSetAtCreationOnly: spec.tpm cannot be given, taken away or
// changed once the VM is made; a source is refused, and so is a sealed key
// a caller writes; an apiserver without a KEK refuses a TPM, and only it.
func TestATPMIsSetAtCreationOnly(t *testing.T) {
	t.Parallel()
	s, system, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	b := s.backend(registry.VirtualMachine)
	if _, err := b.Apply(user, tpmVM("with")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, vmFromImage("without", "noble")); err != nil {
		t.Fatal(err)
	}
	off := tpmVM("with")
	off.Spec.Tpm.Enabled = false
	_, err := b.Apply(user, off)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "turning a VM's TPM off")
	gone := vmFromImage("with", "noble")
	_, err = b.Apply(user, gone)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "leaving spec.tpm out")
	_, err = b.Apply(user, tpmVM("without"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "giving a VM a TPM")
	if err != nil && !strings.Contains(err.Error(), "spec.tpm.enabled") {
		t.Errorf("the refusal does not name spec.tpm.enabled: %v", err)
	}

	sourced := tpmVM("sourced")
	sourced.Spec.Tpm.Source = &computev1alpha1.VirtualMachineTPMSource{}
	_, err = b.Apply(user, sourced)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a TPM with a source")
	forged := tpmVM("forged")
	forged.Spec.Tpm.SealedKey = proto.Clone(storedVM(t, s, system, "with").GetSpec().GetTpm().GetSealedKey()).(*secretsv1alpha1.SealedData)
	_, err = b.Apply(user, forged)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a TPM with a sealed key of the caller's")
	stolen := tpmVM("with")
	stolen.Spec.Tpm.SealedKey = forged.Spec.Tpm.SealedKey
	stolen.Spec.Tpm.SealedKey.Ciphertext = []byte("other")
	_, err = b.Apply(user, stolen)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "replacing a VM's sealed key")

	bare, system2, user2 := runningWith(t, func(*Config) {})
	if _, err := bare.backend(registry.Image).Apply(system2, image("noble")); err != nil {
		t.Fatal(err)
	}
	_, err = bare.backend(registry.VirtualMachine).Apply(user2, tpmVM("with"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a TPM without a KEK")
	if err != nil && !strings.Contains(err.Error(), "key-encryption key") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	noTPM := tpmVM("no-tpm")
	noTPM.Spec.Tpm.Enabled = false
	if _, err := bare.backend(registry.VirtualMachine).Apply(user2, noTPM); err != nil {
		t.Errorf("a VM with spec.tpm disabled, without a KEK: %v", err)
	}
	// Nor does a machine with a TPM pass, whoever writes it.
	spec, err := vmMachineSpec(tpmVM("with"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !spec.GetFirmware().GetTpm() {
		t.Fatal("a VM with a TPM makes a machine without one")
	}
	m := &computev1alpha1.Machine{Metadata: &metav1.ObjectMeta{Project: "web", Name: "vm-with"}, Spec: spec}
	_, err = bare.backend(registry.Machine).Apply(system2, m)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a machine with a TPM without a KEK")
}

// TestTPMKeysAreCheckedAtStart: an apiserver over VMs with TPMs refuses to
// start without the KEK their keys were sealed under, or with a wrong one,
// and seals them again under a new one given the old, the key and the
// VM's generation the same.
func TestTPMKeysAreCheckedAtStart(t *testing.T) {
	t.Parallel()
	k1 := secrets.NewKEK("k1")
	s, system, user := runningWith(t, withSecrets(keyring(t, k1), nil))
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, tpmVM("web")); err != nil {
		t.Fatal(err)
	}
	key := tpmKeyOf(t, s, system, "web")
	restart := func(k *secrets.Keyring) *Server {
		return New(Config{Store: s.store, Secrets: k, Clock: s.clock, DisableControllers: true})
	}
	for what, k := range map[string]*secrets.Keyring{
		"no KEK":                     nil,
		"another KEK of the same id": keyring(t, secrets.NewKEK("k1")),
		"a new KEK without the old":  keyring(t, secrets.NewKEK("k2")),
	} {
		if err := restart(k).PrepareSecrets(t.Context()); err == nil {
			t.Errorf("started with %s", what)
		}
	}
	k2 := secrets.NewKEK("k2")
	if err := restart(keyring(t, k2, k1)).PrepareSecrets(t.Context()); err != nil {
		t.Fatalf("rotating to a new KEK: %v", err)
	}
	after := restart(keyring(t, k2))
	if err := after.PrepareSecrets(t.Context()); err != nil {
		t.Fatalf("the new KEK alone, once rotated: %v", err)
	}
	vm := storedVM(t, after, system, "web")
	if vm.GetSpec().GetTpm().GetSealedKey().GetKekId() != "k2" || vm.GetMetadata().GetGeneration() != 1 {
		t.Errorf("after rotating: KEK %s, generation %d", vm.GetSpec().GetTpm().GetSealedKey().GetKekId(), vm.GetMetadata().GetGeneration())
	}
	if got, err := after.openVMTPMKey(vm); err != nil || !bytes.Equal(got, key) {
		t.Errorf("the rotated key: %v; the same as before: %t", err, bytes.Equal(got, key))
	}
}

// TestResolveVirtualTPMKey: a node gets a machine's TPM key only for a
// machine with a TPM placed on it, the key of the VM that made it; no one
// else gets one, and the log says it was given, never what.
func TestResolveVirtualTPMKey(t *testing.T) {
	t.Parallel()
	log := &lockedBuffer{}
	s, system, user := runningWith(t, func(c *Config) {
		withSecrets(keyring(t, secrets.NewKEK("k1")), log)(c)
		c.DisableControllers = false
		c.Nodes = []string{"n1", "n2"}
		c.NodeMonitorInterval = 1000 * time.Hour
	})
	for _, n := range []string{"n1", "n2"} {
		waitNode(t, s, n)
	}
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vb := s.backend(registry.VirtualMachine)
	if _, err := vb.Apply(user, tpmVM("web")); err != nil {
		t.Fatal(err)
	}
	if _, err := vb.Apply(user, vmFromImage("plain", "noble")); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "vm-web", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if !m.GetSpec().GetFirmware().GetTpm() {
		t.Fatalf("the machine of a VM with a TPM has none: %v", m.GetSpec().GetFirmware())
	}
	if p := waitMachine(t, s, system, "vm-plain", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" }); p.GetSpec().GetFirmware().GetTpm() {
		t.Error("the machine of a VM without a TPM has one")
	}
	want := tpmKeyOf(t, s, system, "web")
	h := virtualTPMKeys{s}
	ask := func(ctx context.Context, machine string) ([]byte, error) {
		res, err := h.ResolveVirtualTPMKey(ctx, connect.NewRequest(&nodev1.ResolveVirtualTPMKeyRequest{Project: "web", Machine: machine}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetKey(), nil
	}
	n1 := asNode(user, "n1")
	if got, err := ask(n1, "vm-web"); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the machine's node: %v; the VM's key: %t", err, bytes.Equal(got, want))
	}
	for _, c := range []struct {
		what    string
		ctx     context.Context
		machine string
	}{
		{"another node", asNode(user, "n2"), "vm-web"},
		{"a machine without a TPM", n1, "vm-plain"},
		{"a machine that does not exist", n1, "vm-nowhere"},
		{"a node this apiserver does not run", asNode(user, "n3"), "vm-web"},
		{"the project's owner", user, "vm-web"},
		{"the system", system, "vm-web"},
		{"a service token named as the node", auth.WithPrincipal(user, auth.Principal{Type: auth.ServiceToken, ID: "n1"}), "vm-web"},
	} {
		if got, err := ask(c.ctx, c.machine); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: %x %v, want denied", c.what, got, err)
		}
	}
	// A machine whose VM is not the one that made it gets nothing: here,
	// the controller reference names another uid.
	if _, err := s.backend(registry.Disk).Apply(user, disk("forged-root", "5Gi")); err != nil {
		t.Fatal(err)
	}
	forged := proto.Clone(m).(*computev1alpha1.Machine)
	forged.Metadata.Name = "vm-forged"
	forged.Metadata.Uid, forged.Metadata.ResourceVersion = "", 0
	forged.Spec.GetFirmware().RootDisk = "forged-root"
	forged.Spec.Interfaces = nil
	forged.Metadata.OwnerReferences = slices.Clone(forged.GetMetadata().GetOwnerReferences())
	for i, ref := range forged.Metadata.OwnerReferences {
		r := proto.Clone(ref).(*metav1.OwnerReference)
		r.Uid = "not-the-vm"
		forged.Metadata.OwnerReferences[i] = r
	}
	if _, err := s.backend(registry.Machine).Apply(system, forged); err != nil {
		t.Fatal(err)
	}
	if got, err := ask(n1, "vm-forged"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a machine another uid's VM controls: %x %v, want denied", got, err)
	}
	// Machines the VM does control, each refused for one reason alone: one
	// that has no TPM, and one being deleted, which before its delete is
	// given the key.
	controlled := func(name string) *computev1alpha1.Machine {
		if _, err := s.backend(registry.Disk).Apply(user, disk(name+"-root", "5Gi")); err != nil {
			t.Fatal(err)
		}
		c := proto.Clone(m).(*computev1alpha1.Machine)
		c.Metadata.Name = name
		c.Metadata.Uid, c.Metadata.ResourceVersion = "", 0
		c.Spec.GetFirmware().RootDisk = name + "-root"
		c.Spec.Interfaces = nil
		return c
	}
	noTPM := controlled("vm-no-tpm")
	noTPM.Spec.GetFirmware().Tpm = false
	if _, err := s.backend(registry.Machine).Apply(system, noTPM); err != nil {
		t.Fatal(err)
	}
	if got, err := ask(n1, "vm-no-tpm"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a machine without a TPM, of a VM with one: %x %v, want denied", got, err)
	}
	deleting := controlled("vm-deleting")
	deleting.Metadata.Finalizers = []string{NodeCleanupFinalizer}
	if _, err := s.backend(registry.Machine).Apply(system, deleting); err != nil {
		t.Fatal(err)
	}
	if got, err := ask(n1, "vm-deleting"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the machine before its delete: %v; given the key %t", err, bytes.Equal(got, want))
	}
	if _, err := s.backend(registry.Machine).Delete(system, resource.Key{Project: "web", Name: "vm-deleting"}, nil); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "vm-deleting", "being deleted", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	if got, err := ask(n1, "vm-deleting"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a machine being deleted: %x %v, want denied", got, err)
	}
	if !strings.Contains(log.String(), "a TPM key given") {
		t.Errorf("the log does not say the key was given:\n%s", log.String())
	}
	for _, needle := range []string{hex.EncodeToString(want), base64.StdEncoding.EncodeToString(want), string(want)} {
		if strings.Contains(log.String(), needle) {
			t.Error("the log holds the key")
		}
	}
}

// TestAFailedStartRewritesNothing: a KEK rotation that finds a secret, or a
// TPM key, the KEKs do not open stops the start before it seals anything
// again: what opened stays under the old KEK.
func TestAFailedStartRewritesNothing(t *testing.T) {
	t.Parallel()
	for _, broken := range []string{"secret", "tpm key"} {
		t.Run(broken, func(t *testing.T) {
			t.Parallel()
			k1 := secrets.NewKEK("k1")
			s, system, user := runningWith(t, withSecrets(keyring(t, k1), nil))
			if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.backend(registry.VirtualMachine).Apply(user, tpmVM("web")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.backend(registry.Secret).Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p"))); err != nil {
				t.Fatal(err)
			}
			// Damage the one, in the store, so no KEK opens it.
			kind, name := registry.Secret, "ghcr"
			if broken == "tpm key" {
				kind, name = registry.VirtualMachine, "web"
			}
			key := s.backend(kind).storeKey(resource.Key{Project: "web", Name: name})
			stored, err := s.store.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			obj, err := s.backend(kind).decode(stored)
			if err != nil {
				t.Fatal(err)
			}
			switch o := obj.(type) {
			case *secretsv1alpha1.Secret:
				o.Spec.Sealed.Ciphertext[0] ^= 1
			case *computev1alpha1.VirtualMachine:
				o.Spec.Tpm.SealedKey.Ciphertext[0] ^= 1
			}
			data, err := encode(obj)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.store.Update(t.Context(), key, data, stored.Revision); err != nil {
				t.Fatal(err)
			}
			restarted := New(Config{Store: s.store, Secrets: keyring(t, secrets.NewKEK("k2"), k1), Clock: s.clock, DisableControllers: true})
			if err := restarted.PrepareSecrets(t.Context()); err == nil {
				t.Fatalf("started with a %s no KEK opens", broken)
			}
			if id := storedVM(t, s, system, "web").GetSpec().GetTpm().GetSealedKey().GetKekId(); id != "k1" {
				t.Errorf("the TPM key is sealed under %s after a start that failed, not k1", id)
			}
			sobj, err := s.get(system, registry.Secret, resource.Key{Project: "web", Name: "ghcr"})
			if err != nil {
				t.Fatal(err)
			}
			if id := sobj.(*secretsv1alpha1.Secret).GetSpec().GetSealed().GetKekId(); id != "k1" {
				t.Errorf("the secret is sealed under %s after a start that failed, not k1", id)
			}
		})
	}
}

// TestATPMKeyOpensForItsVMAlone: a VM's sealed TPM key opens for that VM
// alone: not for another VM, nor for a VM of the same project and name made
// again, which has another uid, and another key.
func TestATPMKeyOpensForItsVMAlone(t *testing.T) {
	t.Parallel()
	s, system, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	b := s.backend(registry.VirtualMachine)
	for _, name := range []string{"a", "b"} {
		if _, err := b.Apply(user, tpmVM(name)); err != nil {
			t.Fatal(err)
		}
	}
	a, other := storedVM(t, s, system, "a"), storedVM(t, s, system, "b")
	sealed := a.GetSpec().GetTpm().GetSealedKey()
	if _, err := s.cfg.Secrets.Open(vmTPMObject(other), sealed); err == nil {
		t.Error("a VM's TPM key opens as another VM's")
	}
	renamed := proto.Clone(a).(*computev1alpha1.VirtualMachine)
	renamed.Metadata.Uid = "another-uid"
	if _, err := s.cfg.Secrets.Open(vmTPMObject(renamed), sealed); err == nil {
		t.Error("a VM's TPM key opens for a VM of its project and name with another uid")
	}
	key := tpmKeyOf(t, s, system, "a")
	if _, err := b.Delete(user, resource.Key{Project: "web", Name: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the VM gone", func() bool { return getVM(t, s, system, "a") == nil })
	if _, err := b.Apply(user, tpmVM("a")); err != nil {
		t.Fatal(err)
	}
	again := storedVM(t, s, system, "a")
	if again.GetMetadata().GetUid() == a.GetMetadata().GetUid() {
		t.Fatal("the VM made again has its old uid")
	}
	if _, err := s.cfg.Secrets.Open(vmTPMObject(again), sealed); err == nil {
		t.Error("the old VM's TPM key opens for the VM made again")
	}
	if got := tpmKeyOf(t, s, system, "a"); bytes.Equal(got, key) {
		t.Error("the VM made again has the old VM's TPM key")
	}
}
