package apiserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
)

// maxPublicKey bounds a key given to be signed.
const maxPublicKey = 16 << 10

// SignSSHKey certifies the caller's key for the machine of the named
// object; see vm-ssh.md.
func (h machineCapabilities) SignSSHKey(ctx context.Context, req *connect.Request[computev1alpha1.SignSSHKeyRequest]) (*connect.Response[computev1alpha1.SignSSHKeyResponse], error) {
	s := h.s
	m := req.Msg
	kind, err := kindNamed(m.GetKind())
	if err != nil {
		return nil, err
	}
	key := resource.Key{Project: m.GetProject(), Name: m.GetName()}
	r, err := s.reach(ctx, kind, key, authz.SSH, nil)
	if err != nil {
		return nil, err
	}
	p := r.principal
	if len(m.GetPublicKey()) > maxPublicKey {
		return nil, invalid("public_key is %d bytes, at most %d", len(m.GetPublicKey()), maxPublicKey)
	}
	if s.cfg.SSHCA == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, sshca.ErrNoCA)
	}
	uid := object.Meta(r.obj).GetUid()
	cert, err := s.cfg.SSHCA.Sign(m.GetPublicKey(), uid, p.String())
	if err != nil {
		if errors.Is(err, sshca.ErrNoCA) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, invalid("public_key: %v", err)
	}
	user, err := s.defaultUser(ctx, r.obj)
	if err != nil {
		return nil, err
	}
	validBefore := time.Unix(int64(cert.ValidBefore), 0)
	s.log.Info("ssh key signed", "principal", p.String(), "client_ip", clientIP(ctx), "project", key.Project, "kind", kind.Kind, "name", key.Name, "uid", uid,
		"key", ssh.FingerprintSHA256(cert.Key), "valid_before", validBefore.UTC().Format(time.RFC3339))
	return connect.NewResponse(&computev1alpha1.SignSSHKeyResponse{
		Certificate: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert))),
		Principal:   uid, DefaultUser: user, ValidBefore: timestamppb.New(validBefore),
	}), nil
}

// defaultUser is the default user of the image a virtual machine's root
// disk was made from, if any says one; an object of another kind has none
// yet.
func (s *Server) defaultUser(ctx context.Context, obj proto.Message) (string, error) {
	vm, ok := obj.(*computev1alpha1.VirtualMachine)
	if !ok {
		return "", nil
	}
	image := vm.GetSpec().GetRootDisk().GetFromImage().GetImage()
	if name := vm.GetSpec().GetRootDisk().GetDisk(); name != "" {
		dobj, err := s.get(ctx, registry.Disk, resource.Key{Project: vm.GetMetadata().GetProject(), Name: name})
		if err != nil {
			return "", apiError(err)
		}
		if d, ok := dobj.(*storagev1alpha1.Disk); ok {
			image = d.GetSpec().GetSource().GetImage()
		}
	}
	if image.GetName() == "" {
		return "", nil
	}
	if image.GetProject() != "" {
		cobj, err := s.get(ctx, registry.CustomImage, resource.Key{Project: image.GetProject(), Name: image.GetName()})
		if err != nil {
			return "", apiError(err)
		}
		ci, _ := cobj.(*storagev1alpha1.CustomImage)
		return ci.GetSpec().GetDefaultUser(), nil
	}
	iobj, err := s.get(ctx, registry.Image, resource.Key{Name: image.GetName()})
	if err != nil {
		return "", apiError(err)
	}
	img, _ := iobj.(*storagev1alpha1.Image)
	return img.GetSpec().GetDefaultUser(), nil
}

// sshFits refuses a machine with no firmware: only a machine that boots a
// guest OS has an sshd to reach.
func sshFits(r *reached, machine *computev1alpha1.Machine) error {
	if machine.GetSpec().GetFirmware() == nil {
		return failedPrecondition("%s has no machine yet", r)
	}
	return nil
}
