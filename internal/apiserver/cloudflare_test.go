package apiserver_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/authtest"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

// TestCloudflareListener runs the public listener the way production does:
// only a signed Access token gets a caller in.
func TestCloudflareListener(t *testing.T) {
	ctx := t.Context()
	iss := authtest.NewIssuer(t)
	cf, err := auth.NewCloudflare(ctx, iss.Config())
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := apiserver.New(apiserver.Config{Store: st, BootstrapAdmin: "alice@example.com"})
	go func() { _ = srv.Run(ctx) }()
	public := httptest.NewServer(srv.Handler(auth.Chain{cf}))
	t.Cleanup(public.Close)

	client := func(name, value string) tenancyv1alpha1connect.TenantServiceClient {
		return tenancyv1alpha1connect.NewTenantServiceClient(http.DefaultClient, public.URL,
			connect.WithInterceptors(headerInterceptor{name, value}))
	}
	// The bootstrap grant is written in the background at start; until it
	// lands, alice is not invited yet. Then she signs up, with the email
	// her token carries.
	accounts := iamv1alpha1connect.NewAccountServiceClient(http.DefaultClient, public.URL,
		connect.WithInterceptors(headerInterceptor{auth.CloudflareJWTHeader, iss.UserToken(t, "Alice@Example.com")}))
	eventually(t, "the bootstrap admin is invited", func() bool {
		me, err := accounts.WhoAmI(ctx, connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
		return err == nil && me.Msg.GetInvited()
	})
	signedUp := must[iamv1alpha1.SignUpResponse](t)(accounts.SignUp(ctx, connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: "alice"})))
	if got := signedUp.GetUser().GetSpec().GetEmail(); got != "alice@example.com" {
		t.Errorf("signed up with email %q", got)
	}
	res, err := client(auth.CloudflareJWTHeader, iss.UserToken(t, "Alice@Example.com")).ApplyTenant(ctx, connect.NewRequest(tenant("acme")))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Msg.GetCreated() {
		t.Error("tenant not created")
	}
	_, err = client(auth.CloudflareJWTHeader, iss.UserToken(t, "alice@example.com")).GetTenant(ctx,
		connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	if err != nil {
		t.Errorf("the creator, identified by a later token, cannot read the tenant: %v", err)
	}

	_, err = client("Cf-Access-Authenticated-User-Email", "alice@example.com").GetTenant(ctx,
		connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodeUnauthenticated, "the identity header without a token")
	_, err = client(auth.CloudflareJWTHeader, iss.ForeignToken(t, map[string]any{"email": "alice@example.com"})).GetTenant(ctx,
		connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodeUnauthenticated, "a token signed by another key")
	_, err = client(auth.CloudflareJWTHeader, iss.ServiceToken(t, "agent.access")).ApplyTenant(ctx, connect.NewRequest(tenant("agentco")))
	wantCode(t, err, connect.CodePermissionDenied, "a service token creating a tenant")
	_, err = client(auth.DevUserHeader, "alice@example.com").GetTenant(ctx,
		connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodeUnauthenticated, "the dev header on a Cloudflare listener")
}
