package apiserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"connectrpc.com/connect"
	"github.com/google/go-containerregistry/pkg/name"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// MaxImagePullSecrets bounds the secrets a container names.
const MaxImagePullSecrets = 8

// Conditions and reasons of secrets, and of containers waiting on one.
const (
	// SecretReady says whether the apiserver can open the secret's data.
	SecretReady = "Ready"
	// ReasonImagePullSecretNotFound means a container's image pull secret
	// does not exist, so its image cannot be pulled with it.
	ReasonImagePullSecretNotFound = "ImagePullSecretNotFound"
)

// errNoKEK refuses writing secrets on an apiserver without a KEK.
var errNoKEK = errors.New("secrets are not set up on this apiserver: it has no key-encryption key (--secrets-kek)")

func secretObject(s *secretsv1alpha1.Secret) string {
	m := s.GetMetadata()
	return secrets.Object(m.GetProject(), m.GetName(), m.GetUid())
}

// validateSecret checks what a caller writes: the data, every time, never
// the sealed form. Its errors never quote a value.
func validateSecret(_ context.Context, s *Server, _ auth.Principal, _, obj proto.Message) error {
	if s.cfg.Secrets == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errNoKEK)
	}
	spec := obj.(*secretsv1alpha1.Secret).GetSpec()
	if spec.GetSealed() != nil {
		return invalid("spec.sealed is the apiserver's; leave it out")
	}
	if len(spec.GetData()) == 0 {
		return invalid("spec.data is write-only; supply it again: every apply carries it")
	}
	if err := secrets.Validate(spec.GetType(), spec.GetData()); err != nil {
		return invalid("spec: %v", err)
	}
	return nil
}

// sealSecret replaces the data with its sealed form. Data equal to what is
// stored, by its HMAC under the current KEK, keeps the stored form, so
// applying the same values again changes nothing.
func sealSecret(s *Server, old, obj proto.Message) error {
	sec := obj.(*secretsv1alpha1.Secret)
	spec := sec.GetSpec()
	data := spec.GetData()
	spec.Data = nil
	sum, err := s.cfg.Secrets.HMAC(data)
	if err != nil {
		return err
	}
	if old != nil {
		stored := old.(*secretsv1alpha1.Secret).GetSpec().GetSealed()
		if stored.GetKekId() == s.cfg.Secrets.CurrentID() && secrets.SameContent(stored.GetContentHmac(), sum) {
			spec.Sealed = stored
			return nil
		}
	}
	sealed, err := s.cfg.Secrets.Seal(secretObject(sec), data)
	if err != nil {
		return err
	}
	spec.Sealed = sealed
	return nil
}

// redactSecret clears a secret's values and their sealed form.
func redactSecret(obj proto.Message) {
	if spec := obj.(*secretsv1alpha1.Secret).GetSpec(); spec != nil {
		spec.Data, spec.Sealed = nil, nil
	}
}

// validateImagePullSecrets checks the names a container gives; whether
// they exist is the controller's to report, not admission's to refuse.
func validateImagePullSecrets(names []string) error {
	if len(names) > MaxImagePullSecrets {
		return invalid("spec.image_pull_secrets: %d, at most %d", len(names), MaxImagePullSecrets)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if err := ValidName(n); err != nil {
			return invalid("spec.image_pull_secrets: %v", err)
		}
		if seen[n] {
			return invalid("spec.image_pull_secrets: %q is named twice", n)
		}
		seen[n] = true
	}
	return nil
}

// PrepareSecrets checks, before the apiserver serves, that it can open
// every stored secret, and seals again under the current KEK those a
// previous one sealed. It fails when there are secrets and no KEK, or a
// secret the KEKs do not open: the KEK is wrong or missing. Nothing is
// written until every secret has opened.
func (s *Server) PrepareSecrets(ctx context.Context) error {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Secret.Kind})
	if err != nil {
		return err
	}
	if len(objs) > 0 && s.cfg.Secrets == nil {
		return fmt.Errorf("the store holds %d secrets, and this apiserver has no key-encryption key to open them (--secrets-kek)", len(objs))
	}
	b := s.backend(registry.Secret)
	stored := make([]*secretsv1alpha1.Secret, len(objs))
	for i, o := range objs {
		obj, err := b.decode(o)
		if err != nil {
			return err
		}
		sec := obj.(*secretsv1alpha1.Secret)
		// Opening is the check: a key of the right id may still be the
		// wrong key.
		if _, err := s.cfg.Secrets.Open(secretObject(sec), sec.GetSpec().GetSealed()); err != nil {
			return fmt.Errorf("secret %s in project %s: %w: the key-encryption key is wrong, or a previous one is missing (--secrets-kek-previous)", o.Name, o.Project, err)
		}
		stored[i] = sec
	}
	rewrapped := 0
	for i, sec := range stored {
		o := objs[i]
		re, ok, err := s.cfg.Secrets.Rewrap(secretObject(sec), sec.GetSpec().GetSealed())
		if err != nil {
			return fmt.Errorf("sealing secret %s in project %s again: %w", o.Name, o.Project, err)
		}
		if !ok {
			continue
		}
		// The data is the same, so the spec's generation stays.
		sec.Spec.Sealed = re
		data, err := encode(sec)
		if err != nil {
			return err
		}
		if _, err := s.store.Update(ctx, o.Key, data, o.Revision); err != nil {
			return fmt.Errorf("sealing secret %s in project %s again: %w", o.Name, o.Project, err)
		}
		rewrapped++
	}
	if rewrapped > 0 {
		s.log.Info("sealed secrets again under the current key-encryption key", "secrets", rewrapped, "kek", s.cfg.Secrets.CurrentID())
	}
	return nil
}

// reconcileSecret reports what can be said of a secret without its values.
func (s *Server) reconcileSecret(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.Secret, key)
	if err != nil || obj == nil {
		return err
	}
	sec := obj.(*secretsv1alpha1.Secret)
	gen := sec.GetMetadata().GetGeneration()
	status := &secretsv1alpha1.SecretStatus{ObservedGeneration: gen}
	ready := &metav1.Condition{Type: SecretReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Sealed", ObservedGeneration: gen}
	var data map[string][]byte
	if s.cfg.Secrets == nil {
		err = errNoKEK
	} else {
		data, err = s.cfg.Secrets.Open(secretObject(sec), sec.GetSpec().GetSealed())
	}
	if err != nil {
		ready.Status, ready.Reason, ready.Message = metav1.ConditionStatus_CONDITION_STATUS_FALSE, "Undecryptable", err.Error()
	} else {
		status.Keys = slices.Sorted(maps.Keys(data))
		status.ContentHmac = sec.GetSpec().GetSealed().GetContentHmac()
		if sec.GetSpec().GetType() == secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH {
			status.Registries = secrets.Registries(data)
		}
	}
	status.Conditions = conditions.Set(slices.Clone(sec.GetStatus().GetConditions()), ready, s.clock.Now())
	if proto.Equal(status, sec.GetStatus()) {
		return nil
	}
	rv := sec.GetMetadata().GetResourceVersion()
	_, err = s.backend(registry.Secret).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// missingPullSecret names the first of a container's image pull secrets
// that does not exist, or is empty.
func (s *Server) missingPullSecret(ctx context.Context, c *computev1alpha1.Container) (string, error) {
	for _, n := range c.GetSpec().GetImagePullSecrets() {
		obj, err := s.get(ctx, registry.Secret, resource.Key{Project: c.GetMetadata().GetProject(), Name: n})
		if err != nil {
			return "", err
		}
		if obj == nil || obj.(*secretsv1alpha1.Secret).GetMetadata().GetDeleteTime() != nil {
			return n, nil
		}
	}
	return "", nil
}

// imageRegistry is the registry an image reference names, as logins are
// matched to it; ok is false for a reference that names none.
func imageRegistry(image string) (host string, ok bool) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", false
	}
	host, err = secrets.RegistryHost(ref.Context().RegistryStr())
	return host, err == nil
}

// imagePullCredentials serves ImagePullCredentialsService to node agents.
type imagePullCredentials struct{ s *Server }

func (h imagePullCredentials) ResolveImagePullCredentials(ctx context.Context, req *connect.Request[nodev1.ResolveImagePullCredentialsRequest]) (*connect.Response[nodev1.ResolveImagePullCredentialsResponse], error) {
	s := h.s
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, auth.ErrNoCredentials)
	}
	key := resource.Key{Project: req.Msg.GetProject(), Name: req.Msg.GetMachine()}
	// Every refusal says the same, so a node learns nothing of machines
	// that are not its own.
	refuse := func() error {
		s.log.Warn("image pull credentials refused", "principal", p.String(), "project", key.Project, "machine", key.Name)
		return denied("%s may not have the image pull credentials of machine %q in project %q", p, key.Name, key.Project)
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
	if obj == nil {
		return nil, refuse()
	}
	m := obj.(*computev1alpha1.Machine)
	boot := m.GetSpec().GetContainer()
	if m.GetSpec().GetNode() != p.ID || boot == nil {
		return nil, refuse()
	}
	res := &nodev1.ResolveImagePullCredentialsResponse{}
	if len(boot.GetImagePullSecrets()) == 0 || s.cfg.Secrets == nil {
		return connect.NewResponse(res), nil
	}
	host, ok := imageRegistry(boot.GetImage())
	if !ok {
		return connect.NewResponse(res), nil // the pull fails on its own
	}
	for _, n := range boot.GetImagePullSecrets() {
		sobj, err := s.get(ctx, registry.Secret, resource.Key{Project: key.Project, Name: n})
		if err != nil {
			return nil, apiError(err)
		}
		if sobj == nil {
			continue
		}
		sec := sobj.(*secretsv1alpha1.Secret)
		if sec.GetMetadata().GetDeleteTime() != nil || sec.GetSpec().GetType() != secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH {
			continue
		}
		data, err := s.cfg.Secrets.Open(secretObject(sec), sec.GetSpec().GetSealed())
		if err != nil {
			s.log.Error("a secret does not open", "project", key.Project, "secret", n, "error", err)
			continue
		}
		login, ok := secrets.LoginFor(data, host)
		if !ok {
			continue
		}
		s.log.Info("image pull credentials given", "node", p.ID, "project", key.Project, "machine", key.Name, "secret", n, "registry", host)
		res.Registry, res.Username, res.Password, res.IdentityToken = host, login.Username, login.Password, login.IdentityToken
		return connect.NewResponse(res), nil
	}
	return connect.NewResponse(res), nil
}
