package apiserver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// A VM's TPM key: made at create, sealed under the KEK in
// spec.tpm.sealed_key, never answered, and given to its node through
// VirtualTPMKeyService. See docs/decisions/vm-tpm.md.

const (
	// tpmKeySize is the TPM key's size: swtpm's aes-256-cbc key.
	tpmKeySize = 32
	// tpmKeyEntry names the key in its sealed data.
	tpmKeyEntry = "key"
)

// errNoKEKForTPM refuses a TPM on an apiserver without a KEK.
var errNoKEKForTPM = errors.New("a TPM needs an apiserver with a key-encryption key (--secrets-kek), which seals the key its state is encrypted with")

// vmTPMObject is what a VM's TPM key is sealed for: it opens for that VM
// alone, by its project, name and uid, never as a Secret's data.
func vmTPMObject(vm *computev1alpha1.VirtualMachine) string {
	m := vm.GetMetadata()
	return "VirtualMachine/tpm/" + secrets.Object(m.GetProject(), m.GetName(), m.GetUid())
}

func vmHasTPM(vm *computev1alpha1.VirtualMachine) bool { return vm.GetSpec().GetTpm().GetEnabled() }

// validateVMTPM checks a VM's spec.tpm as its caller wrote it: no source,
// as importing a state is not supported; no sealed key, which is the
// apiserver's, unless the stored one, as the apiserver's own writes carry
// it; and a KEK to seal the key with.
func validateVMTPM(s *Server, old proto.Message, vm *computev1alpha1.VirtualMachine) error {
	tpm := vm.GetSpec().GetTpm()
	if tpm.GetSource() != nil {
		return invalid("spec.tpm.source: starting a TPM from an existing state is not supported")
	}
	if sealed := tpm.GetSealedKey(); sealed != nil {
		o, _ := old.(*computev1alpha1.VirtualMachine)
		if !proto.Equal(sealed, o.GetSpec().GetTpm().GetSealedKey()) {
			return invalid("spec.tpm.sealed_key is the apiserver's; leave it out")
		}
	}
	if tpm.GetEnabled() && s.cfg.Secrets == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errNoKEKForTPM)
	}
	return nil
}

// sealVirtualMachine gives a new VM with a TPM its key, sealed, and keeps
// the stored one on every write after: spec.tpm cannot change, and callers
// never see the key to write it back.
func sealVirtualMachine(s *Server, old, obj proto.Message) error {
	vm := obj.(*computev1alpha1.VirtualMachine)
	if !vmHasTPM(vm) {
		return nil
	}
	if o, ok := old.(*computev1alpha1.VirtualMachine); ok {
		vm.Spec.Tpm.SealedKey = o.GetSpec().GetTpm().GetSealedKey()
		return nil
	}
	key := make([]byte, tpmKeySize)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	sealed, err := s.cfg.Secrets.Seal(vmTPMObject(vm), map[string][]byte{tpmKeyEntry: key})
	clear(key)
	if err != nil {
		return err
	}
	vm.Spec.Tpm.SealedKey = sealed
	return nil
}

// redactVirtualMachine clears a VM's sealed TPM key from every answer.
func redactVirtualMachine(obj proto.Message) {
	if tpm := obj.(*computev1alpha1.VirtualMachine).GetSpec().GetTpm(); tpm != nil {
		tpm.SealedKey = nil
	}
}

// openVMTPMKey opens a VM's TPM key.
func (s *Server) openVMTPMKey(vm *computev1alpha1.VirtualMachine) ([]byte, error) {
	data, err := s.cfg.Secrets.Open(vmTPMObject(vm), vm.GetSpec().GetTpm().GetSealedKey())
	if err != nil {
		return nil, err
	}
	key := data[tpmKeyEntry]
	if len(key) != tpmKeySize {
		return nil, fmt.Errorf("the TPM key is %d bytes, not %d", len(key), tpmKeySize)
	}
	return key, nil
}

// storedTPMKey is a VM with a TPM key as the store holds it.
type storedTPMKey struct {
	vm *computev1alpha1.VirtualMachine
	at store.Object
}

// openVMTPMKeys is PrepareSecrets' check of VMs' TPM keys: each must open.
func (s *Server) openVMTPMKeys(ctx context.Context) ([]storedTPMKey, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.VirtualMachine.Kind})
	if err != nil {
		return nil, err
	}
	b := s.backend(registry.VirtualMachine)
	var out []storedTPMKey
	for _, o := range objs {
		obj, err := b.decode(o)
		if err != nil {
			return nil, err
		}
		vm := obj.(*computev1alpha1.VirtualMachine)
		if vm.GetSpec().GetTpm().GetSealedKey() == nil {
			continue
		}
		if s.cfg.Secrets == nil {
			return nil, fmt.Errorf("virtual machine %s in project %s has a TPM, and this apiserver has no key-encryption key to open its key (--secrets-kek)", o.Name, o.Project)
		}
		if _, err := s.openVMTPMKey(vm); err != nil {
			return nil, fmt.Errorf("the TPM key of virtual machine %s in project %s: %w: the key-encryption key is wrong, or a previous one is missing (--secrets-kek-previous)", o.Name, o.Project, err)
		}
		out = append(out, storedTPMKey{vm: vm, at: o})
	}
	return out, nil
}

// rewrapVMTPMKeys seals again under the current KEK the keys
// openVMTPMKeys opened that a previous one sealed.
func (s *Server) rewrapVMTPMKeys(ctx context.Context, keys []storedTPMKey) error {
	rewrapped := 0
	for _, k := range keys {
		vm, at := k.vm, k.at
		re, ok, err := s.cfg.Secrets.Rewrap(vmTPMObject(vm), vm.GetSpec().GetTpm().GetSealedKey())
		if err != nil {
			return fmt.Errorf("sealing the TPM key of virtual machine %s in project %s again: %w", at.Name, at.Project, err)
		}
		if !ok {
			continue
		}
		// The key is the same, so the spec's generation stays.
		vm.Spec.Tpm.SealedKey = re
		data, err := encode(vm)
		if err != nil {
			return err
		}
		if _, err := s.store.Update(ctx, at.Key, data, at.Revision); err != nil {
			return fmt.Errorf("sealing the TPM key of virtual machine %s in project %s again: %w", at.Name, at.Project, err)
		}
		rewrapped++
	}
	if rewrapped > 0 {
		s.log.Info("sealed TPM keys again under the current key-encryption key", "virtual_machines", rewrapped, "kek", s.cfg.Secrets.CurrentID())
	}
	return nil
}

// virtualTPMKeys serves VirtualTPMKeyService to node agents.
type virtualTPMKeys struct{ s *Server }

// ResolveVirtualTPMKey gives the node a machine's TPM key: only a node the
// platform knows, for a machine with a TPM placed on it, made for the VM
// whose key it is. Every refusal says the same, so a node learns nothing of
// machines that are not its own. It logs what it gave, never the key.
func (h virtualTPMKeys) ResolveVirtualTPMKey(ctx context.Context, req *connect.Request[nodev1.ResolveVirtualTPMKeyRequest]) (*connect.Response[nodev1.ResolveVirtualTPMKeyResponse], error) {
	s := h.s
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, auth.ErrNoCredentials)
	}
	key := resource.Key{Project: req.Msg.GetProject(), Name: req.Msg.GetMachine()}
	refuse := func() error {
		s.log.Warn("a TPM key refused", "principal", p.String(), "project", key.Project, "machine", key.Name)
		return denied("%s may not have the TPM key of machine %q in project %q", p, key.Name, key.Project)
	}
	if p.Type != auth.Node || !slices.Contains(s.cfg.Nodes, p.ID) {
		return nil, refuse()
	}
	if ValidName(key.Project) != nil || ValidName(key.Name) != nil {
		return nil, refuse()
	}
	obj, err := s.get(ctx, registry.Machine, key)
	if err != nil {
		return nil, apiError(err)
	}
	m, _ := obj.(*computev1alpha1.Machine)
	if m == nil || m.GetSpec().GetNode() != p.ID || !m.GetSpec().GetFirmware().GetTpm() || m.GetMetadata().GetDeleteTime() != nil {
		return nil, refuse()
	}
	// The VM that controls the machine, by name and uid: a VM made again
	// under the name has another key, which this machine never gets.
	var vm *computev1alpha1.VirtualMachine
	for _, ref := range m.GetMetadata().GetOwnerReferences() {
		if !ref.GetController() || ref.GetKind() != registry.VirtualMachine.Kind {
			continue
		}
		vobj, err := s.get(ctx, registry.VirtualMachine, resource.Key{Project: key.Project, Name: ref.GetName()})
		if err != nil {
			return nil, apiError(err)
		}
		if v, ok := vobj.(*computev1alpha1.VirtualMachine); ok && v.GetMetadata().GetUid() == ref.GetUid() {
			vm = v
		}
	}
	if vm == nil || !vmHasTPM(vm) || vm.GetSpec().GetTpm().GetSealedKey() == nil {
		return nil, refuse()
	}
	if s.cfg.Secrets == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errNoKEKForTPM)
	}
	k, err := s.openVMTPMKey(vm)
	if err != nil {
		s.log.Error("a TPM key does not open", "project", key.Project, "virtual_machine", vm.GetMetadata().GetName(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("the TPM key does not open"))
	}
	s.log.Info("a TPM key given", "node", p.ID, "project", key.Project, "machine", key.Name, "virtual_machine", vm.GetMetadata().GetName())
	return connect.NewResponse(&nodev1.ResolveVirtualTPMKeyResponse{Key: k}), nil
}
