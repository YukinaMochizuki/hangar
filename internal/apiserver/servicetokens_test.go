package apiserver_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

func createToken(project, name, role string) *connect.Request[iamv1alpha1.CreateServiceTokenRequest] {
	return connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{Project: project, Name: name, Role: role})
}

func rotateToken(project, name string) *connect.Request[iamv1alpha1.RotateServiceTokenRequest] {
	return connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{Project: project, Name: name})
}

func getToken(project, name string) *connect.Request[iamv1alpha1.GetServiceTokenRequest] {
	return connect.NewRequest(&iamv1alpha1.GetServiceTokenRequest{Project: project, Name: name})
}

func tokenBinding(name, subject, role string, scope *iamv1alpha1.BindingScope) *connect.Request[iamv1alpha1.ApplyRoleBindingRequest] {
	return connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{RoleBinding: &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: subject}},
			Role:    role, Scope: scope,
		},
	}})
}

// whoAmI is who a secret authenticates as, or the refusal.
func (e *env) whoAmI(secret string) (*iamv1alpha1.WhoAmIResponse, error) {
	res, err := e.withToken(secret).accounts.WhoAmI(context.Background(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// TestServiceTokenIsIssuedOnce: creating a token returns its secret once,
// which authenticates as the token and holds what its role grants in its
// project, nothing else; nothing shows the secret again.
func TestServiceTokenIsIssuedOnce(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("db", "acme"))))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("db", "pg", "postgres"))))

	res := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.editor")))
	secret := res.GetSecret()
	if !strings.HasPrefix(secret, token.Service) || len(secret) != len(token.Service)+43 {
		t.Fatalf("secret %q is not hgr_ and 32 random bytes", token.Secret(secret))
	}
	st := res.GetServiceToken().GetStatus()
	if st.GetSecretPrefix() != secret[:12] {
		t.Errorf("status.secret_prefix %q, want the secret's first 12 characters", st.GetSecretPrefix())
	}
	if got, want := st.GetExpireTime().AsTime().Sub(st.GetIssueTime().AsTime()), apiserver.DefaultTokenDuration; got != want {
		t.Errorf("the secret lasts %s, want %s", got, want)
	}
	if rb := res.GetRoleBinding(); rb.GetSpec().GetSubject().GetServiceToken() != "web/deployer" || rb.GetSpec().GetRole() != "project.editor" {
		t.Errorf("role binding %v", rb)
	}

	me, err := e.whoAmI(secret)
	if err != nil {
		t.Fatal(err)
	}
	if me.GetType() != "serviceToken" || me.GetId() != "web/deployer" || !me.GetMember() {
		t.Errorf("whoami: %v", me)
	}

	// What its binding grants, in its project only.
	tok := e.withToken(secret)
	must[computev1alpha1.ApplyContainerResponse](t)(tok.containers.ApplyContainer(ctx, applyContainer(container("web", "app", "nginx"))))
	_, err = tok.containers.GetContainer(ctx, getContainer("db", "pg"))
	wantCode(t, err, connect.CodePermissionDenied, "the token reading another project")
	_, err = tok.bindings.ApplyRoleBinding(ctx, binding("web-bob", bob, "project.viewer", projectScope("web")))
	wantCode(t, err, connect.CodePermissionDenied, "the token granting a role")
	_, err = tok.issuer.CreateServiceToken(ctx, createToken("web", "another", ""))
	wantCode(t, err, connect.CodePermissionDenied, "the token, an editor, making a token")

	// Nothing answers with the secret again.
	got := must[iamv1alpha1.GetServiceTokenResponse](t)(a.tokens.GetServiceToken(ctx, getToken("web", "deployer")))
	list := must[iamv1alpha1.ListServiceTokensResponse](t)(a.tokens.ListServiceTokens(ctx, connect.NewRequest(&iamv1alpha1.ListServiceTokensRequest{Project: "web"})))
	for _, m := range []proto.Message{got, list} {
		if b, _ := protojson.Marshal(m); bytes.Contains(b, []byte(secret[len(token.Service):])) {
			t.Errorf("an answer holds the secret: %T", m)
		}
	}

	// A name is one token's.
	_, err = a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", ""))
	wantCode(t, err, connect.CodeAlreadyExists, "creating a token of a name taken")
}

// TestServiceTokenStoresOnlyTheHash greps every database file for the
// secret: it is not there, its SHA-256 is.
func TestServiceTokenStoresOnlyTheHash(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	res := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.as(alice).issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.viewer")))
	secret := res.GetSecret()
	if _, err := e.whoAmI(secret); err != nil {
		t.Fatal(err)
	}
	random := []byte(secret[len(token.Service):])
	hash := []byte(token.Hash(secret))
	var all []byte
	files, _ := filepath.Glob(filepath.Join(e.dbDir, "*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, random) {
			t.Errorf("%s holds the secret", filepath.Base(f))
		}
		all = append(all, b...)
	}
	if !bytes.Contains(all, hash) {
		t.Errorf("no database file holds the secret's SHA-256, so the grep proves nothing; files %v", files)
	}
}

// TestServiceTokenRotate: a rotated token's old secret stops working at
// once, the new one works, and the status names the new one.
func TestServiceTokenRotate(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	first := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.viewer"))).GetSecret()
	rot := must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{
		Project: "web", Name: "deployer", Duration: durationpb.New(2 * time.Hour),
	})))
	second := rot.GetSecret()
	if second == first {
		t.Fatal("rotating returned the same secret")
	}
	_, err := e.whoAmI(first)
	wantCode(t, err, connect.CodeUnauthenticated, "the secret rotated away")
	if _, err := e.whoAmI(second); err != nil {
		t.Errorf("the new secret: %v", err)
	}
	st := rot.GetServiceToken().GetStatus()
	if st.GetSecretPrefix() != second[:12] || st.GetExpireTime().AsTime().Sub(st.GetIssueTime().AsTime()) != 2*time.Hour {
		t.Errorf("status after rotating: %v", st)
	}
	_, err = a.issuer.RotateServiceToken(ctx, rotateToken("web", "nope"))
	wantCode(t, err, connect.CodeNotFound, "rotating a token that does not exist")
}

// TestServiceTokenMadeByApplyHasNoSecret: apply makes a token with nothing
// to authenticate with until it is rotated.
func TestServiceTokenMadeByApplyHasNoSecret(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	res := must[iamv1alpha1.ApplyServiceTokenResponse](t)(a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{
		ServiceToken: &iamv1alpha1.ServiceToken{Metadata: &metav1.ObjectMeta{Project: "web", Name: "ci"}, Spec: &iamv1alpha1.ServiceTokenSpec{Description: "CI"}},
	})))
	if res.GetServiceToken().GetStatus().GetSecretPrefix() != "" {
		t.Errorf("a token made by apply has a secret: %v", res.GetServiceToken().GetStatus())
	}
	secret := must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, rotateToken("web", "ci"))).GetSecret()
	if _, err := e.whoAmI(secret); err != nil {
		t.Errorf("the secret of a token made by apply: %v", err)
	}
}

// TestServiceTokenDelete: deleting a token refuses its secret at once,
// takes every binding naming it, and frees its name for a token that starts
// with none of them.
func TestServiceTokenDelete(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.editor"))).GetSecret()
	// An extra binding at the tenant, made by hand, is the token's too.
	rb := must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, tokenBinding("acme-deployer", "web/deployer", "tenant.viewer", tenantScope("acme"))))
	owners := rb.GetRoleBinding().GetMetadata().GetOwnerReferences()
	if len(owners) != 2 || owners[1].GetKind() != "ServiceToken" || owners[1].GetProject() != "web" {
		t.Errorf("owners of a binding naming a token: %v", owners)
	}

	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: "deployer"})))
	_, err := e.whoAmI(secret)
	wantCode(t, err, connect.CodeUnauthenticated, "a deleted token's secret, right after the delete")

	eventually(t, "the token and its bindings are gone", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("web", "deployer"))
		if connect.CodeOf(err) != connect.CodeNotFound {
			return false
		}
		list := must[iamv1alpha1.ListRoleBindingsResponse](t)(e.as(root).bindings.ListRoleBindings(ctx, connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{})))
		for _, b := range list.GetItems() {
			if b.GetSpec().GetSubject().GetServiceToken() != "" {
				return false
			}
		}
		return true
	})

	// A new token of the name holds nothing the old one did.
	again := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", ""))).GetSecret()
	_, err = e.withToken(again).containers.ApplyContainer(ctx, applyContainer(container("web", "app", "nginx")))
	wantCode(t, err, connect.CodePermissionDenied, "a new token of a deleted one's name")

	// Nor can a binding name a token that does not exist, or change whom it
	// names.
	_, err = a.bindings.ApplyRoleBinding(ctx, tokenBinding("web-ghost", "web/ghost", "project.viewer", projectScope("web")))
	wantCode(t, err, connect.CodeFailedPrecondition, "a binding naming no token")
	_, err = a.bindings.ApplyRoleBinding(ctx, tokenBinding("web-bad", "deployer", "project.viewer", projectScope("web")))
	wantCode(t, err, connect.CodeInvalidArgument, "a token subject without its project")
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, tokenBinding("web-deployer", "web/deployer", "project.viewer", projectScope("web"))))
	_, err = a.bindings.ApplyRoleBinding(ctx, binding("web-deployer", bob, "project.viewer", projectScope("web")))
	wantCode(t, err, connect.CodeInvalidArgument, "a token's binding made to name a person")
}

// TestBindingTellsNoOneOfAnotherTenantsTokens: the owner of another tenant
// binding a token they may not read is refused exactly as for a token that
// does not exist, so bindings tell no one which tokens exist.
func TestBindingTellsNoOneOfAnotherTenantsTokens(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	e.setupTenant(bob, "other", "site")
	must[iamv1alpha1.CreateServiceTokenResponse](t)(e.as(alice).issuer.CreateServiceToken(ctx, createToken("web", "deployer", "")))
	b := e.as(bob)
	_, existing := b.bindings.ApplyRoleBinding(ctx, tokenBinding("other-deployer", "web/deployer", "tenant.viewer", tenantScope("other")))
	_, missing := b.bindings.ApplyRoleBinding(ctx, tokenBinding("other-ghost", "web/ghost", "tenant.viewer", tenantScope("other")))
	wantCode(t, existing, connect.CodeFailedPrecondition, "binding another tenant's token")
	wantCode(t, missing, connect.CodeFailedPrecondition, "binding a token that does not exist")
	if existing != nil && missing != nil && strings.ReplaceAll(existing.Error(), "deployer", "ghost") != missing.Error() {
		t.Errorf("the refusals differ:\n%v\n%v", existing, missing)
	}
	// Its own owner binds it in their tenant, and bob may bind a token of
	// his own.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(e.as(alice).bindings.ApplyRoleBinding(ctx, tokenBinding("acme-deployer", "web/deployer", "tenant.viewer", tenantScope("acme"))))
	must[iamv1alpha1.CreateServiceTokenResponse](t)(b.issuer.CreateServiceToken(ctx, createToken("site", "bot", "")))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(b.bindings.ApplyRoleBinding(ctx, tokenBinding("other-bot", "site/bot", "tenant.viewer", tenantScope("other"))))
}

// syncBuffer is a bytes.Buffer many goroutines may write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestServiceTokenSecretsAreNeverLogged: the apiserver's log, at debug,
// across creating, rotating, using, refusing and deleting a token, holds
// none of its secrets.
func TestServiceTokenSecretsAreNeverLogged(t *testing.T) {
	var logs syncBuffer
	e := newEnv(t, func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	first := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.editor"))).GetSecret()
	if _, err := e.whoAmI(first); err != nil {
		t.Fatal(err)
	}
	second := must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, rotateToken("web", "deployer"))).GetSecret()
	must[computev1alpha1.ApplyContainerResponse](t)(e.withToken(second).containers.ApplyContainer(ctx, applyContainer(container("web", "app", "nginx"))))
	_, err := e.whoAmI(first)
	wantCode(t, err, connect.CodeUnauthenticated, "the rotated-away secret")
	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: "deployer"})))
	_, err = e.whoAmI(second)
	wantCode(t, err, connect.CodeUnauthenticated, "a deleted token's secret")
	eventually(t, "the token is gone", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("web", "deployer"))
		return connect.CodeOf(err) == connect.CodeNotFound
	})

	out := logs.String()
	if !strings.Contains(out, "created a service token") || !strings.Contains(out, "rotated a service token") {
		t.Fatalf("the log was not captured:\n%s", out)
	}
	for _, s := range []string{first, second} {
		if strings.Contains(out, s[len(token.Service):]) {
			t.Errorf("the log holds a secret:\n%s", out)
		}
	}
}

// failingRevoke is a credentials store whose RevokeCredentialsOf fails as
// many times as fails says.
type failingRevoke struct {
	store.Credentials
	fails atomic.Int32
}

func (f *failingRevoke) RevokeCredentialsOf(ctx context.Context, principal string, until time.Time) (int, error) {
	if f.fails.Add(-1) >= 0 {
		return 0, errors.New("injected: the revoke failed")
	}
	return f.Credentials.RevokeCredentialsOf(ctx, principal, until)
}

// TestAFailedRevokeLeavesNoSecret: when revoking a deleted token's secret
// fails, the secret still stops working, and a token made again under the
// name, with a binding, does not take it up.
func TestAFailedRevokeLeavesNoSecret(t *testing.T) {
	creds := &failingRevoke{}
	e := newEnv(t, func(c *apiserver.Config) {
		creds.Credentials = c.Credentials
		c.Credentials = creds
	})
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.viewer"))).GetSecret()
	if _, err := e.whoAmI(secret); err != nil {
		t.Fatal(err)
	}

	creds.fails.Store(1)
	// The delete is stored; the revoke after it fails, and so may the call.
	_, _ = a.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: "deployer"}))
	eventually(t, "the deleted token's secret is refused", func() bool {
		_, err := e.whoAmI(secret)
		return connect.CodeOf(err) == connect.CodeUnauthenticated
	})

	// The same name again, applied, with a binding naming it.
	eventually(t, "the name is free again", func() bool {
		_, err := a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{
			ServiceToken: &iamv1alpha1.ServiceToken{Metadata: &metav1.ObjectMeta{Project: "web", Name: "deployer"}},
		}))
		return err == nil
	})
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, tokenBinding("web-deployer-again", "web/deployer", "project.editor", projectScope("web"))))
	_, err := e.whoAmI(secret)
	wantCode(t, err, connect.CodeUnauthenticated, "the old secret, once the name is a new token's")
	_, err = e.withToken(secret).containers.ApplyContainer(ctx, applyContainer(container("web", "app", "nginx")))
	wantCode(t, err, connect.CodeUnauthenticated, "the old secret using the new token's binding")
}

// TestFailingRevokesKeepTheToken: while every revoke of a deleted token's
// secret fails, the token stays, being deleted, and its secret is refused;
// once revokes work again, the next sweep lets it go.
func TestFailingRevokesKeepTheToken(t *testing.T) {
	creds := &failingRevoke{}
	e := newEnv(t, func(c *apiserver.Config) {
		creds.Credentials = c.Credentials
		c.Credentials = creds
	})
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	res := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.viewer")))
	secret, binding := res.GetSecret(), res.GetRoleBinding().GetMetadata().GetName()

	creds.fails.Store(1 << 30)
	_, _ = a.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: "deployer"}))
	// The cascade gets as far as the token's binding, then cannot let the
	// token go.
	eventually(t, "the token's binding is gone", func() bool {
		_, err := e.as(root).bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: binding}))
		return connect.CodeOf(err) == connect.CodeNotFound
	})
	time.Sleep(500 * time.Millisecond)
	got, err := a.tokens.GetServiceToken(ctx, getToken("web", "deployer"))
	if err != nil {
		t.Fatalf("the token went while its secret could not be revoked: %v", err)
	}
	if got.Msg.GetServiceToken().GetMetadata().GetDeleteTime() == nil {
		t.Error("the token is not being deleted")
	}
	_, err = e.whoAmI(secret)
	wantCode(t, err, connect.CodeUnauthenticated, "the secret of a token being deleted, never revoked")

	creds.fails.Store(0)
	e.clock.Advance(5 * time.Minute) // the sweep interval
	eventually(t, "the token goes once its secret is revoked", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("web", "deployer"))
		return connect.CodeOf(err) == connect.CodeNotFound
	})
	_, err = e.whoAmI(secret)
	wantCode(t, err, connect.CodeUnauthenticated, "the secret once the token is gone")
}

// TestRotateRacingDeleteLeavesNoSecret: rotations racing the token's delete
// leave no secret that works, whichever lands first.
func TestRotateRacingDeleteLeavesNoSecret(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	for round := range 3 {
		name := fmt.Sprintf("racer-%d", round)
		first := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", name, ""))).GetSecret()
		var mu sync.Mutex
		secrets := []string{first}
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := a.issuer.RotateServiceToken(ctx, rotateToken("web", name))
				if err == nil {
					mu.Lock()
					secrets = append(secrets, res.Msg.GetSecret())
					mu.Unlock()
				}
			}()
		}
		if _, err := a.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: name})); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for _, s := range secrets {
			_, err := e.whoAmI(s)
			wantCode(t, err, connect.CodeUnauthenticated, "a secret of a deleted token, issued around its delete")
		}
	}
}

// TestServiceTokenGoesWithItsProject: deleting the project revokes its
// tokens' secrets.
func TestServiceTokenGoesWithItsProject(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.viewer"))).GetSecret()
	must[tenancyv1alpha1.DeleteProjectResponse](t)(a.projects.DeleteProject(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteProjectRequest{Name: "web"})))
	eventually(t, "the token's secret is refused", func() bool {
		_, err := e.whoAmI(secret)
		return connect.CodeOf(err) == connect.CodeUnauthenticated
	})
	// The project waits for its tokens, and each token for its bindings.
	eventually(t, "the project is gone", func() bool {
		_, err := e.as(root).projects.GetProject(ctx, connect.NewRequest(&tenancyv1alpha1.GetProjectRequest{Name: "web"}))
		return connect.CodeOf(err) == connect.CodeNotFound
	})
}

// TestServiceTokenExpires: a secret works until its duration is up, and the
// duration is bounded.
func TestServiceTokenExpires(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	for _, d := range []time.Duration{59 * time.Minute, apiserver.MaxTokenDuration + time.Hour} {
		_, err := a.issuer.CreateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{Project: "web", Name: "short", Duration: durationpb.New(d)}))
		wantCode(t, err, connect.CodeInvalidArgument, "a duration of "+d.String())
	}
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Project: "web", Name: "short", Duration: durationpb.New(time.Hour),
	}))).GetSecret()
	if _, err := e.whoAmI(secret); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour)
	_, err := e.whoAmI(secret)
	wantCode(t, err, connect.CodeUnauthenticated, "an expired secret")
}

// TestUnknownTokensAreRefusedAlike: an unknown, a malformed and a deleted
// secret are refused the same way.
func TestUnknownTokensAreRefusedAlike(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{token.New(token.Service).Reveal(), "hgr_", "hgr_short", "hgx_" + token.Random()} {
		_, err := e.whoAmI(s)
		wantCode(t, err, connect.CodeUnauthenticated, "a made-up token")
		if err != nil && len(s) > 8 && strings.Contains(err.Error(), s) {
			t.Errorf("the refusal holds the token: %v", err)
		}
	}
}

// TestEditorsCannotMintTokens: an editor reads tokens but cannot make,
// apply, rotate or delete one: rotating one an owner bound as owner would
// hand the editor the owner's grant.
func TestEditorsCannotMintTokens(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const carol = "carol@example.com"
	e.addMember(carol)
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-carol", carol, "project.editor", projectScope("web"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-bob", bob, "project.owner", projectScope("web"))))
	ownerSecret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.as(bob).issuer.CreateServiceToken(ctx, createToken("web", "admin", "project.owner"))).GetSecret()
	editor := e.as(carol)

	_, err := editor.issuer.CreateServiceToken(ctx, createToken("web", "mine", ""))
	wantCode(t, err, connect.CodePermissionDenied, "an editor creating a token")
	_, err = editor.issuer.RotateServiceToken(ctx, rotateToken("web", "admin"))
	wantCode(t, err, connect.CodePermissionDenied, "an editor rotating an owner-bound token")
	_, err = editor.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{
		ServiceToken: &iamv1alpha1.ServiceToken{Metadata: &metav1.ObjectMeta{Project: "web", Name: "mine"}},
	}))
	wantCode(t, err, connect.CodePermissionDenied, "an editor applying a token")
	_, err = editor.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: "admin"}))
	wantCode(t, err, connect.CodePermissionDenied, "an editor deleting a token")
	must[iamv1alpha1.GetServiceTokenResponse](t)(editor.tokens.GetServiceToken(ctx, getToken("web", "admin")))
	if _, err := e.whoAmI(ownerSecret); err != nil {
		t.Errorf("the owner-bound token after the editor's attempts: %v", err)
	}

	// An owner grants a token no more than they could by hand: the role is
	// checked as theirs, and a refused binding leaves no token behind.
	_, err = e.as(bob).issuer.CreateServiceToken(ctx, createToken("web", "too-much", "tenant.owner"))
	wantCode(t, err, connect.CodeInvalidArgument, "a project owner binding a token a tenant role")
	eventually(t, "the refused token is gone", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("web", "too-much"))
		return connect.CodeOf(err) == connect.CodeNotFound
	})
}

// TestServiceTokensCountTowardQuota: a project's serviceTokens limit holds.
func TestServiceTokensCountTowardQuota(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	p := project("web", "acme")
	p.Project.Spec.Limits = &tenancyv1alpha1.Limits{ServiceTokens: proto.Int32(1)}
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(p)))
	must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "one", "")))
	_, err := a.issuer.CreateServiceToken(ctx, createToken("web", "two", ""))
	wantCode(t, err, connect.CodeResourceExhausted, "a token past the project's limit")
}

// TestServiceTokenLastUse: a use shows in the token's status.
func TestServiceTokenLastUse(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", ""))).GetSecret()
	if _, err := e.whoAmI(secret); err != nil {
		t.Fatal(err)
	}
	eventually(t, "status.last_use_time is set", func() bool {
		tok := must[iamv1alpha1.GetServiceTokenResponse](t)(a.tokens.GetServiceToken(ctx, getToken("web", "deployer")))
		return tok.GetServiceToken().GetStatus().GetLastUseTime().AsTime().Equal(e.clock.Now())
	})
}
