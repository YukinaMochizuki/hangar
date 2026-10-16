package apiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// A service token's secret lasts DefaultTokenDuration unless its creator
// asks otherwise, and between MinTokenDuration and MaxTokenDuration.
const (
	DefaultTokenDuration = 365 * 24 * time.Hour
	MinTokenDuration     = time.Hour
	MaxTokenDuration     = 3 * DefaultTokenDuration

	// tokenUseEvery is how stale a token's status.last_use_time may get.
	tokenUseEvery = time.Hour
	// tokenSecretPrefixLen is how much of a secret status.secret_prefix
	// shows: hgr_ and 8 of its 43 random characters.
	tokenSecretPrefixLen = 12
	// maxTokenDescription bounds spec.description.
	maxTokenDescription = 256
)

// tokenPrincipal is who a service token authenticates as. Its ID,
// <project>/<name>, is also the subject RoleBindings name it by.
func tokenPrincipal(key resource.Key) auth.Principal {
	return auth.Principal{Type: auth.ServiceToken, ID: key.Project + "/" + key.Name}
}

// parseTokenSubject reads a RoleBinding's service-token subject,
// <project>/<name>.
func parseTokenSubject(s string) (resource.Key, error) {
	key, ok := tokenKeyOf(s)
	if !ok {
		return resource.Key{}, invalid("spec.subject.service_token %q must name a service token as <project>/<name>", s)
	}
	return key, nil
}

// tokenKeyOf reads <project>/<name>, reporting whether s is one.
func tokenKeyOf(s string) (resource.Key, bool) {
	project, name, ok := strings.Cut(s, "/")
	if !ok || ValidName(project) != nil || ValidName(name) != nil {
		return resource.Key{}, false
	}
	return resource.Key{Project: project, Name: name}, true
}

func validateServiceToken(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	if d := obj.(*iamv1alpha1.ServiceToken).GetSpec().GetDescription(); len(d) > maxTokenDescription {
		return invalid("spec.description is %d bytes; at most %d", len(d), maxTokenDescription)
	}
	return nil
}

// bindingTokenOwner makes a RoleBinding naming a service token the token's:
// the token must exist, and not be on its way out. Deleting the token then
// deletes the binding, and waits for it. Whoever makes the binding must be
// able to read the token: a token they may not read is refused as one that
// does not exist, so a binding cannot tell anyone which tokens exist in
// another tenant's projects.
func bindingTokenOwner(ctx context.Context, s *Server, p auth.Principal, obj proto.Message) (*metav1.OwnerReference, error) {
	sub := obj.(*iamv1alpha1.RoleBinding).GetSpec().GetSubject().GetServiceToken()
	if sub == "" {
		return nil, nil
	}
	key, err := parseTokenSubject(sub)
	if err != nil {
		return nil, err
	}
	missing := failedPrecondition("service token %q does not exist in project %q, or you may not read it", key.Name, key.Project)
	if !s.authz.Allowed(p, authz.Permission(registry.ServiceToken, authz.Get), authz.Target{Project: key.Project}) {
		return nil, missing
	}
	o, err := s.get(ctx, registry.ServiceToken, key)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, missing
	}
	meta := object.Meta(o)
	if meta.GetDeleteTime() != nil {
		return nil, failedPrecondition("service token %q in project %q is being deleted", key.Name, key.Project)
	}
	return &metav1.OwnerReference{
		ApiVersion: registry.ServiceToken.APIVersion(), Kind: registry.ServiceToken.Kind,
		Name: key.Name, Uid: meta.GetUid(), Project: key.Project,
	}, nil
}

// lockBindingToken takes the tokenBindings lock of the token a new
// RoleBinding names, if it names one.
func lockBindingToken(s *Server, obj proto.Message) (unlock func()) {
	sub := obj.(*iamv1alpha1.RoleBinding).GetSpec().GetSubject().GetServiceToken()
	if _, err := parseTokenSubject(sub); err != nil {
		return func() {}
	}
	return s.tokenBindings.lock(sub)
}

// lockTokenCascade takes a token's tokenBindings lock for its cascade.
func lockTokenCascade(s *Server, key resource.Key) (unlock func()) {
	return s.tokenBindings.lock(tokenPrincipal(key).ID)
}

// revokeServiceToken revokes a deleted token's secret, at once. It runs
// after the delete is stored, so an issue racing it either sees the token
// being deleted, and revokes what it issued itself (issueTokenSecret), or
// issued before, and is revoked here.
func revokeServiceToken(ctx context.Context, s *Server, key resource.Key) error {
	if s.sessions == nil {
		return nil
	}
	n, err := s.sessions.RevokePrincipal(ctx, tokenPrincipal(key))
	if err != nil {
		return fmt.Errorf("revoking the secret of service token %s/%s: %w", key.Project, key.Name, err)
	}
	if n > 0 {
		s.log.Info("revoked a deleted service token's secret", "project", key.Project, "name", key.Name)
	}
	return nil
}

// serviceTokenLive says whether the service token of the uid stands: it
// exists under the principal's name, with that uid, and is not being
// deleted. A secret is refused otherwise, even if a revoke of it failed, so
// it never outlives its token nor passes to a later one of the same name.
func (s *Server) serviceTokenLive(ctx context.Context, p auth.Principal, uid string) (bool, error) {
	if p.Type != auth.ServiceToken {
		return false, nil
	}
	key, ok := tokenKeyOf(p.ID)
	if !ok {
		return false, nil
	}
	o, err := s.get(ctx, registry.ServiceToken, key)
	if err != nil || o == nil {
		return false, err
	}
	meta := object.Meta(o)
	return meta.GetUid() == uid && meta.GetDeleteTime() == nil, nil
}

// tokenDuration reads how long a secret should last, a year if unset.
func tokenDuration(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return DefaultTokenDuration, nil
	}
	if !d.IsValid() {
		return 0, invalid("duration is not a valid duration")
	}
	ttl := d.AsDuration()
	if ttl < MinTokenDuration || ttl > MaxTokenDuration {
		return 0, invalid("duration %s is out of range: at least %s, at most %s (three years)", ttl, MinTokenDuration, MaxTokenDuration)
	}
	return ttl, nil
}

// tokenBindingName is the name of the binding CreateServiceToken makes for
// a token's role: unique by the random end of the token's uid.
func tokenBindingName(role, uid string) string {
	u := strings.ReplaceAll(uid, "-", "")
	if len(u) > 12 {
		u = u[len(u)-12:]
	}
	return "token-" + strings.ReplaceAll(role, ".", "-") + "-" + u
}

// tokenIssuer serves ServiceTokenIssuerService.
type tokenIssuer struct{ s *Server }

func (h tokenIssuer) CreateServiceToken(ctx context.Context, req *connect.Request[iamv1alpha1.CreateServiceTokenRequest]) (*connect.Response[iamv1alpha1.CreateServiceTokenResponse], error) {
	s := h.s
	msg := req.Msg
	ttl, err := tokenDuration(msg.GetDuration())
	if err != nil {
		return nil, err
	}
	if s.sessions == nil {
		return nil, failedPrecondition("this apiserver keeps no credentials, so it issues no service tokens")
	}
	key := resource.Key{Project: msg.GetProject(), Name: msg.GetName()}
	if msg.GetRole() != "" {
		if _, ok := s.authz.Role(msg.GetRole()); !ok {
			return nil, invalid("role %q is not a known role", msg.GetRole())
		}
	}
	defer s.tokenSecrets.lock(tokenPrincipal(key).ID)()

	// Created as the caller: their grant, the project's quota.
	res, err := s.backend(registry.ServiceToken).Create(ctx, &iamv1alpha1.ServiceToken{
		Metadata: &metav1.ObjectMeta{Project: key.Project, Name: key.Name},
		Spec:     &iamv1alpha1.ServiceTokenSpec{Description: msg.GetDescription()},
	})
	if err != nil {
		return nil, err
	}
	tok := res.Object.(*iamv1alpha1.ServiceToken)
	uid := tok.GetMetadata().GetUid()
	// From here on, a failure deletes the token again, which revokes
	// whatever was issued and takes the binding with it.
	ctx = context.WithoutCancel(ctx)
	undo := func(cause error) error {
		if _, err := s.backend(registry.ServiceToken).Delete(s.system(ctx), key, nil); err != nil && connect.CodeOf(err) != connect.CodeNotFound {
			s.log.Error("deleting a service token whose creation failed", "project", key.Project, "name", key.Name, "error", err)
		}
		return cause
	}
	var binding *iamv1alpha1.RoleBinding
	if msg.GetRole() != "" {
		// Also as the caller: no one grants a token more than they could
		// grant by hand.
		res, err := s.backend(registry.RoleBinding).Create(ctx, &iamv1alpha1.RoleBinding{
			Metadata: &metav1.ObjectMeta{Name: tokenBindingName(msg.GetRole(), uid)},
			Spec: &iamv1alpha1.RoleBindingSpec{
				Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: tokenPrincipal(key).ID}},
				Role:    msg.GetRole(),
				Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: key.Project}},
			},
		})
		if err != nil {
			return nil, undo(err)
		}
		binding = res.Object.(*iamv1alpha1.RoleBinding)
	}
	issued, tok, err := s.issueTokenSecret(ctx, key, uid, ttl)
	if err != nil {
		return nil, undo(err)
	}
	p, _ := auth.FromContext(ctx)
	s.log.Info("created a service token", "project", key.Project, "name", key.Name, "role", msg.GetRole(), "by", p.String(), "expires", issued.ExpiresAt)
	return connect.NewResponse(&iamv1alpha1.CreateServiceTokenResponse{
		ServiceToken: tok, Secret: issued.Secret.Reveal(), RoleBinding: binding,
	}), nil
}

func (h tokenIssuer) RotateServiceToken(ctx context.Context, req *connect.Request[iamv1alpha1.RotateServiceTokenRequest]) (*connect.Response[iamv1alpha1.RotateServiceTokenResponse], error) {
	s := h.s
	msg := req.Msg
	b := s.backend(registry.ServiceToken)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	key := resource.Key{Project: msg.GetProject(), Name: msg.GetName()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	// Rotating hands out what the token's bindings grant, as creating one
	// does, so it takes the same permission, which editors do not hold.
	if err := b.authorize(p, authz.Apply, authz.Target{Project: key.Project}, b.asked(key)); err != nil {
		return nil, err
	}
	ttl, err := tokenDuration(msg.GetDuration())
	if err != nil {
		return nil, err
	}
	if s.sessions == nil {
		return nil, failedPrecondition("this apiserver keeps no credentials, so it issues no service tokens")
	}
	defer s.tokenSecrets.lock(tokenPrincipal(key).ID)()
	o, err := s.get(ctx, registry.ServiceToken, key)
	if err != nil {
		return nil, apiError(err)
	}
	if o == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", b.asked(key)))
	}
	if object.Meta(o).GetDeleteTime() != nil {
		return nil, failedPrecondition("%s is being deleted", b.asked(key))
	}
	issued, tok, err := s.issueTokenSecret(context.WithoutCancel(ctx), key, object.Meta(o).GetUid(), ttl)
	if err != nil {
		return nil, err
	}
	s.log.Info("rotated a service token", "project", key.Project, "name", key.Name, "by", p.String(), "expires", issued.ExpiresAt)
	return connect.NewResponse(&iamv1alpha1.RotateServiceTokenResponse{ServiceToken: tok, Secret: issued.Secret.Reveal()}), nil
}

// issueTokenSecret issues a secret for the token of the uid, in place of any
// it had, and records it in the token's status. If the token is gone, or
// being deleted, by then, it revokes the secret again and fails: a delete
// stored before the secret was revokes it itself (revokeServiceToken), and
// one stored after is seen here. The caller holds tokenSecrets.
func (s *Server) issueTokenSecret(ctx context.Context, key resource.Key, uid string, ttl time.Duration) (session.ServiceToken, *iamv1alpha1.ServiceToken, error) {
	p := tokenPrincipal(key)
	issued, err := s.sessions.IssueService(ctx, p, uid, ttl)
	if err != nil {
		return session.ServiceToken{}, nil, apiError(err)
	}
	tok, err := s.updateTokenStatus(ctx, key, uid, func(st *iamv1alpha1.ServiceTokenStatus) bool {
		st.SecretPrefix = issued.Secret.Reveal()[:tokenSecretPrefixLen]
		st.IssueTime = timestamppb.New(issued.IssuedAt)
		st.ExpireTime = timestamppb.New(issued.ExpiresAt)
		return true
	})
	if err == nil && tok.GetMetadata().GetDeleteTime() != nil {
		err = failedPrecondition("service token %q in project %q is being deleted", key.Name, key.Project)
	}
	if err != nil {
		if _, rerr := s.sessions.RevokePrincipal(ctx, p); rerr != nil {
			s.log.Error("revoking a service token's secret issued as it was deleted", "project", key.Project, "name", key.Name, "error", rerr)
		}
		return session.ServiceToken{}, nil, err
	}
	return issued, tok, nil
}

// errTokenGone means the token of the uid is no longer there.
var errTokenGone = errors.New("the service token is gone")

// updateTokenStatus changes the status of the token of the uid as the
// system, with change, which reports whether there is anything to write.
func (s *Server) updateTokenStatus(ctx context.Context, key resource.Key, uid string, change func(*iamv1alpha1.ServiceTokenStatus) bool) (*iamv1alpha1.ServiceToken, error) {
	b := s.backend(registry.ServiceToken)
	for range maxRetries {
		o, err := s.get(ctx, registry.ServiceToken, key)
		if err != nil {
			return nil, apiError(err)
		}
		if o == nil || object.Meta(o).GetUid() != uid {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("service token %q in project %q: %w", key.Name, key.Project, errTokenGone))
		}
		tok := o.(*iamv1alpha1.ServiceToken)
		st := proto.Clone(tok.GetStatus()).(*iamv1alpha1.ServiceTokenStatus)
		if st == nil {
			st = &iamv1alpha1.ServiceTokenStatus{}
		}
		if !change(st) {
			return tok, nil
		}
		st.ObservedGeneration = tok.GetMetadata().GetGeneration()
		rv := tok.GetMetadata().GetResourceVersion()
		out, err := b.UpdateStatus(s.system(ctx), key, st, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			return nil, err
		}
		return out.(*iamv1alpha1.ServiceToken), nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}

// tokenUse is one use of a service token, as session.Manager reports it.
type tokenUse struct {
	p  auth.Principal
	at time.Time
}

// serviceTokenUsed hears of a service token's use, without blocking the
// call that used it: a use that finds the queue full is dropped, and the
// next one, a minute later, records it.
func (s *Server) serviceTokenUsed(p auth.Principal, at time.Time) {
	select {
	case s.tokenUses <- tokenUse{p, at}:
	default:
	}
}

// recordTokenUses writes the uses of service tokens to their status, at
// most one write per token an hour, until ctx is done.
func (s *Server) recordTokenUses(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case u := <-s.tokenUses:
			key, err := parseTokenSubject(u.p.ID)
			if err != nil {
				continue
			}
			o, err := s.get(ctx, registry.ServiceToken, key)
			if err != nil || o == nil {
				continue
			}
			_, err = s.updateTokenStatus(ctx, key, object.Meta(o).GetUid(), func(st *iamv1alpha1.ServiceTokenStatus) bool {
				if last := st.GetLastUseTime(); last != nil && u.at.Sub(last.AsTime()) < tokenUseEvery {
					return false
				}
				st.LastUseTime = timestamppb.New(u.at)
				return true
			})
			if err != nil && ctx.Err() == nil && !errors.Is(err, errTokenGone) {
				s.log.Warn("could not record a service token's use", "project", key.Project, "name", key.Name, "error", err)
			}
		}
	}
}
