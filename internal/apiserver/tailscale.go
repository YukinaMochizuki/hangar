package apiserver

import (
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
)

// A container joins a tailnet with the auth key of a tailscale-auth Secret
// its spec.tailscale names, given to its node alone, through
// TailscaleAuthKeyService (docs/decisions/container-tailscale.md).

// validateTailscale checks a container's spec.tailscale, or, with
// machine, a machine's spec.container.tailscale, whose hostname its
// controller has set. field is its path, for errors.
func validateTailscale(field string, ts *computev1alpha1.ContainerTailscale, machine bool) error {
	if ts == nil {
		return nil
	}
	if ts.GetAuthSecret() == "" {
		return invalid("%s.auth_secret is required", field)
	}
	if err := ValidName(ts.GetAuthSecret()); err != nil {
		return invalid("%s.auth_secret: %v", field, err)
	}
	if ts.GetHostname() == "" && !machine {
		return nil
	}
	if err := ValidName(ts.GetHostname()); err != nil {
		return invalid("%s.hostname must be a DNS label: %v", field, err)
	}
	return nil
}

// machineTailscale is what a container's machine joins its tailnet with:
// its spec.tailscale, the container's name the hostname where it gives
// none.
func machineTailscale(container string, ts *computev1alpha1.ContainerTailscale) *computev1alpha1.ContainerTailscale {
	if ts == nil {
		return nil
	}
	out := proto.Clone(ts).(*computev1alpha1.ContainerTailscale)
	if out.Hostname == "" {
		out.Hostname = container
	}
	return out
}

// withTailnetJoined is conds, a container's, with its TailnetJoined: its
// machine's, which the node says, or Starting while the machine has none.
// A container that joins no tailnet has none.
func (s *Server) withTailnetJoined(conds []*metav1.Condition, c *computev1alpha1.Container, m *computev1alpha1.Machine) []*metav1.Condition {
	if c.GetSpec().GetTailscale() == nil {
		return slices.DeleteFunc(conds, func(x *metav1.Condition) bool { return x.GetType() == computeapi.TailnetJoined })
	}
	if j := conditions.Find(m.GetStatus().GetConditions(), computeapi.TailnetJoined); j != nil && ownedBy(m.GetMetadata(), c.GetMetadata().GetUid()) &&
		m.GetSpec().GetContainer().GetTailscale() != nil {
		conds = slices.DeleteFunc(conds, func(x *metav1.Condition) bool { return x.GetType() == computeapi.TailnetJoined })
		j = proto.Clone(j).(*metav1.Condition)
		j.ObservedGeneration = c.GetMetadata().GetGeneration()
		return append(conds, j)
	}
	return conditions.Set(conds, &metav1.Condition{
		Type: computeapi.TailnetJoined, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: computeapi.ReasonTailnetStarting,
		Message: "the container is not running yet", ObservedGeneration: c.GetMetadata().GetGeneration(),
	}, s.clock.Now())
}

// tailscaleAuthKeys serves TailscaleAuthKeyService to node agents.
type tailscaleAuthKeys struct{ s *Server }

// ResolveTailscaleAuthKey gives the node the auth key of a container
// machine placed on it that joins a tailnet. Every refusal says the same,
// so a node learns nothing of machines that are not its own; a Secret that
// does not give a key is NOT_FOUND, naming the Secret. It logs what it
// gave, never the key.
func (h tailscaleAuthKeys) ResolveTailscaleAuthKey(ctx context.Context, req *connect.Request[nodev1.ResolveTailscaleAuthKeyRequest]) (*connect.Response[nodev1.ResolveTailscaleAuthKeyResponse], error) {
	s := h.s
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, auth.ErrNoCredentials)
	}
	key := resource.Key{Project: req.Msg.GetProject(), Name: req.Msg.GetMachine()}
	refuse := func() error {
		s.log.Warn("a tailscale auth key refused", "principal", p.String(), "project", key.Project, "machine", key.Name)
		return denied("%s may not have the tailscale auth key of machine %q in project %q", p, key.Name, key.Project)
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
	ts := m.GetSpec().GetContainer().GetTailscale()
	if m == nil || m.GetSpec().GetNode() != p.ID || ts == nil || m.GetMetadata().GetDeleteTime() != nil {
		return nil, refuse()
	}
	name := ts.GetAuthSecret()
	notFound := func(why string) error {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("tailscale auth secret %q in project %q %s", name, key.Project, why))
	}
	if s.cfg.Secrets == nil {
		return nil, notFound("cannot be opened: the apiserver has no key-encryption key")
	}
	sobj, err := s.get(ctx, registry.Secret, resource.Key{Project: key.Project, Name: name})
	if err != nil {
		return nil, apiError(err)
	}
	sec, _ := sobj.(*secretsv1alpha1.Secret)
	switch {
	case sec == nil || sec.GetMetadata().GetDeleteTime() != nil:
		return nil, notFound("does not exist")
	case sec.GetSpec().GetType() != secretsv1alpha1.SecretType_SECRET_TYPE_TAILSCALE_AUTH:
		return nil, notFound("is not of type tailscale-auth")
	}
	data, err := s.cfg.Secrets.Open(secretObject(sec), sec.GetSpec().GetSealed())
	if err != nil {
		s.log.Error("a secret does not open", "project", key.Project, "secret", name, "error", err)
		return nil, notFound("does not open")
	}
	authKey, ok := secrets.TailscaleAuthKeyOf(data)
	if !ok {
		return nil, notFound("holds no " + secrets.TailscaleAuthKey)
	}
	s.log.Info("a tailscale auth key given", "node", p.ID, "project", key.Project, "machine", key.Name, "secret", name)
	return connect.NewResponse(&nodev1.ResolveTailscaleAuthKeyResponse{AuthKey: authKey}), nil
}
