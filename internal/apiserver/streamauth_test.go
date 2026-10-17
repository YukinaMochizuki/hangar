package apiserver_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// openWatch watches containers in project, everywhere when it is empty,
// with c, and returns the events after the opening bookmark, and, once the
// watch ends, what it ended with.
func openWatch(t *testing.T, c computev1alpha1connect.ContainerServiceClient, project string) (events <-chan *computev1alpha1.WatchContainersResponse, ended <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stream, err := c.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{Project: project}))
	if err != nil {
		t.Fatal(err)
	}
	// The opening bookmark: the watch is held from here on.
	if !stream.Receive() {
		t.Fatalf("the watch did not open: %v", stream.Err())
	}
	ev := make(chan *computev1alpha1.WatchContainersResponse, 100)
	end := make(chan error, 1)
	go func() {
		for stream.Receive() {
			ev <- stream.Msg()
		}
		end <- stream.Err()
	}()
	return ev, end
}

// endsWith waits for a stream to end, a recheck after its revoke, and checks
// that it ended with code: a refusal its client can tell from a dropped
// connection.
func endsWith(t *testing.T, ended <-chan error, code connect.Code, what string) {
	t.Helper()
	var err error
	testutil.Eventually(t, what+": the stream ended", func() bool {
		select {
		case err = <-ended:
			return true
		default:
			return false
		}
	})
	wantCode(t, err, code, what)
}

// receivesAdded waits for a watch to tell of the container name being
// added: it is still open.
func receivesAdded(t *testing.T, events <-chan *computev1alpha1.WatchContainersResponse, ended <-chan error, name, what string) {
	t.Helper()
	testutil.Eventually(t, what+": an event of "+name, func() bool {
		for {
			select {
			case ev := <-events:
				if ev.GetType().String() == "EVENT_TYPE_ADDED" && ev.GetContainer().GetMetadata().GetName() == name {
					return true
				}
			case err := <-ended:
				t.Fatalf("%s ended: %v", what, err)
			default:
				return false
			}
		}
	})
}

// TestAWatchEndsWithItsServiceToken: a watch opened with a service token
// ends, at the next recheck, once the token is deleted or its secret
// rotated.
func TestAWatchEndsWithItsServiceToken(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	deployer := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.viewer"))).GetSecret()
	ci := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "ci", "project.viewer"))).GetSecret()
	_, deployerEnded := openWatch(t, e.withToken(deployer).containers, "web")
	ciEvents, ciEnded := openWatch(t, e.withToken(ci).containers, "web")

	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: "deployer"})))
	e.clock.Advance(auth.RecheckInterval)
	endsWith(t, deployerEnded, connect.CodeUnauthenticated, "a watch whose token was deleted")
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "after-delete", "nginx"))))
	receivesAdded(t, ciEvents, ciEnded, "after-delete", "the other token's watch")

	must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, rotateToken("web", "ci")))
	e.clock.Advance(auth.RecheckInterval)
	endsWith(t, ciEnded, connect.CodeUnauthenticated, "a watch whose token was rotated")
}

// TestAWatchEndsWithTheBindingThatLetItIn: a watch of a project ends, at the
// next recheck, once the binding that let its caller watch there is
// deleted; another caller's goes on.
func TestAWatchEndsWithTheBindingThatLetItIn(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("bob-web", bob, "project.viewer", projectScope("web"))))
	_, bobEnded := openWatch(t, e.as(bob).containers, "web")
	aliceEvents, aliceEnded := openWatch(t, a.containers, "web")

	must[iamv1alpha1.DeleteRoleBindingResponse](t)(a.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "bob-web"})))
	e.clock.Advance(auth.RecheckInterval)
	endsWith(t, bobEnded, connect.CodePermissionDenied, "a watch whose binding was deleted")
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "app", "nginx"))))
	receivesAdded(t, aliceEvents, aliceEnded, "app", "the owner's watch")
}

// TestAWatchEndsWithPlatformMembership: a watch everywhere, which needs no
// grant but membership, ends once its caller is no longer a member.
func TestAWatchEndsWithPlatformMembership(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	_, ended := openWatch(t, e.as(bob).containers, "")
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(e.as(root).bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "member-" + usernameOf(bob)})))
	e.clock.Advance(auth.RecheckInterval)
	endsWith(t, ended, connect.CodePermissionDenied, "a watch whose caller left the platform")
}

// TestAWatchEndsWithItsSession: watches opened with a browser session and a
// CLI login end, at the next recheck, once the person's logins are revoked.
func TestAWatchEndsWithItsSession(t *testing.T) {
	clock := clockwork.NewFakeClockAt(time.Now())
	w := newOIDCWorldConfig(t, 0, func(c *apiserver.Config) { c.Clock = clock })
	alice := w.adminInvites("bob@example.com")
	ctx := t.Context()
	bobBrowser := w.asCookie(w.signIn(w.browser(), "bob@example.com"))
	got, verifier, l := w.cliSignIn("bob@example.com")
	tokens := must[iamv1alpha1.ExchangeCodeResponse](t)(w.sessions(headers{}).ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{
		Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: l.url,
	}))).GetTokens()
	must[iamv1alpha1.SignUpResponse](t)(w.accounts(bobBrowser).SignUp(ctx, connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: "bob"})))
	watches := func(h headers) computev1alpha1connect.ContainerServiceClient {
		return computev1alpha1connect.NewContainerServiceClient(w.tls, w.url, connect.WithInterceptors(h))
	}
	_, browserEnded := openWatch(t, watches(bobBrowser), "")
	_, cliEnded := openWatch(t, watches(asBearer(tokens.GetAccessToken())), "")

	must[iamv1alpha1.RevokeSessionsResponse](t)(w.sessions(alice).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "bob@example.com"})))
	clock.Advance(auth.RecheckInterval)
	endsWith(t, browserEnded, connect.CodeUnauthenticated, "a watch whose browser session was revoked")
	endsWith(t, cliEnded, connect.CodeUnauthenticated, "a watch whose CLI login was revoked")
}
