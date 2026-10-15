package node

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// The cross-tenant cache rule (docs/decisions/secrets.md): what one machine
// pulled with its login, another uses only once the registry has resolved
// the reference with that other machine's own credentials.

const (
	privateRef    = "registry.example/team-a/app:1"
	privateDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	privatePinned = "registry.example/team-a/app@" + privateDigest
)

var loginA = driver.Login{Username: "team-a", Password: "secret-a"}

func login(l driver.Login) driver.Credentials {
	return func(context.Context, string) (driver.Login, error) { return l, nil }
}

func privateHost() *fake.Host {
	host := fake.New()
	host.AddImage(privateRef, privateDigest, driver.ImageConfig{Cmd: []string{"true"}})
	host.MakePrivate(privateRef, loginA)
	return host
}

// pullerOn is a puller on host, started, and a way to wait for a machine's
// image to be ready or to fail.
func pullerOn(t *testing.T, host *fake.Host) (*puller, func(uid, ref, digest string, creds driver.Credentials, own bool) (string, error)) {
	t.Helper()
	p := newPuller(host.Images(), host.Storage(), clockwork.NewRealClock(), time.Minute, 2)
	done := make(chan struct{}, 16)
	p.start(t.Context(), func() {
		select {
		case done <- struct{}{}:
		default:
		}
	})
	t.Cleanup(p.wait)
	settle := func(uid, ref, digest string, creds driver.Credentials, own bool) (string, error) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			got, _, ok, err := p.get(uid, ref, digest, creds, own)
			if ok || err != nil {
				return got, err
			}
			select {
			case <-done:
			case <-time.After(10 * time.Millisecond):
			case <-deadline:
				t.Fatalf("machine %s's image %s neither ready nor failed", uid, ref)
			}
		}
	}
	return p, settle
}

// refused checks that a machine was refused its image as unauthorized, as
// the owner is told.
func refused(t *testing.T, who string, digest string, err error) {
	t.Helper()
	var s *stuck
	if err == nil || !errors.Is(err, driver.ErrUnauthorized) || !errors.As(err, &s) || s.reason != computeapi.ReasonImagePullUnauthorized {
		t.Errorf("%s: got %q, %v; want refused as %s", who, digest, err, computeapi.ReasonImagePullUnauthorized)
	}
}

// TestAPrivateDigestIsNotAnotherTenantsWithoutItsOwnResolve: once A has
// pulled a private image, neither a machine without credentials nor one
// with wrong ones gets it, by its tag or its digest, though the host holds
// it; a machine with its own valid login gets it after resolving it itself.
func TestAPrivateDigestIsNotAnotherTenantsWithoutItsOwnResolve(t *testing.T) {
	host := privateHost()
	_, settle := pullerOn(t, host)
	if got, err := settle("a", privateRef, "", login(loginA), true); err != nil || got != privateDigest {
		t.Fatalf("A with its login: %s %v", got, err)
	}
	if !host.Cached(privateDigest) {
		t.Fatal("A's pull left nothing in the cache, so the rest proves nothing")
	}
	wrong := login(driver.Login{Username: "team-b", Password: "guess"})
	for _, c := range []struct {
		who   string
		ref   string
		creds driver.Credentials
		own   bool
	}{
		{"B by the tag, no credentials", privateRef, driver.Anonymous, false},
		{"B by the digest, no credentials", privatePinned, driver.Anonymous, false},
		{"B by the tag, a wrong login", privateRef, wrong, true},
		{"B by the digest, a wrong login", privatePinned, wrong, true},
	} {
		got, err := settle("b-"+c.who, c.ref, "", c.creds, c.own)
		refused(t, c.who, got, err)
	}
	before := host.Resolved(privateRef)
	if got, err := settle("c", privateRef, "", login(loginA), true); err != nil || got != privateDigest {
		t.Errorf("C with a valid login of its own: %s %v", got, err)
	}
	if host.Resolved(privateRef) != before+1 {
		t.Errorf("C used the image with %d resolves of its own, want 1", host.Resolved(privateRef)-before)
	}
}

// TestASharedTagIsResolvedPerMachine: machines naming the same tag, each
// with a login, never share a job: each resolves with its own, so one
// refused stays refused while the other's pull is ready, and the other way
// round.
func TestASharedTagIsResolvedPerMachine(t *testing.T) {
	host := privateHost()
	p, settle := pullerOn(t, host)
	wrong := login(driver.Login{Username: "team-b", Password: "guess"})
	// B first, so that A's job is not B's to have started.
	got, err := settle("b", privateRef, "", wrong, true)
	refused(t, "B first", got, err)
	if got, err := settle("a", privateRef, "", login(loginA), true); err != nil || got != privateDigest {
		t.Fatalf("A after B was refused: %s %v", got, err)
	}
	got, err = settle("b", privateRef, "", wrong, true)
	refused(t, "B after A pulled", got, err)
	if n := len(p.jobs); n != 2 {
		t.Errorf("%d jobs for two machines with logins, want one each", n)
	}
}

// TestAPrivateDigestStaysRefusedAfterARestart: a new agent on a host whose
// cache holds A's image refuses B as the first did, while A, whose status
// records the digest it was pulled for, gets it again from the cache
// without the registry.
func TestAPrivateDigestStaysRefusedAfterARestart(t *testing.T) {
	host := privateHost()
	_, settle := pullerOn(t, host)
	if _, err := settle("a", privateRef, "", login(loginA), true); err != nil {
		t.Fatal(err)
	}
	// The agent restarts: a new puller, the same host.
	_, settle = pullerOn(t, host)
	for _, c := range []struct {
		who string
		ref string
	}{{"B by the tag", privateRef}, {"B by the digest", privatePinned}} {
		got, err := settle("b", c.ref, "", driver.Anonymous, false)
		refused(t, c.who+" after a restart", got, err)
	}
	before := host.Resolved(privateRef)
	if got, err := settle("a", privateRef, privateDigest, login(loginA), true); err != nil || got != privateDigest {
		t.Errorf("A, proven by its status, after a restart: %s %v", got, err)
	}
	if host.Resolved(privateRef) != before {
		t.Error("A, proven by its status, went to the registry")
	}
}

// TestAClaimedDigestIsNotTheCaches: a repository B controls that says it
// has A's private digest, by digest or by a tag, gets nothing of what A
// pulled, whether B names it anonymously or with a login of its own, before
// and after the agent restarts; the review's repository-confusion attack.
func TestAClaimedDigestIsNotTheCaches(t *testing.T) {
	host := privateHost()
	_, settle := pullerOn(t, host)
	if _, err := settle("a", privateRef, "", login(loginA), true); err != nil {
		t.Fatal(err)
	}
	byDigest := host.ClaimDigest("evil.example/b/app@"+privateDigest, privateDigest)
	host.ClaimDigest("evil.example/b/app:1", privateDigest)
	loginB := login(driver.Login{Username: "b", Password: "b"})
	attack := func(when string) {
		t.Helper()
		for i, c := range []struct {
			ref   string
			creds driver.Credentials
			own   bool
		}{
			{byDigest, driver.Anonymous, false},
			{"evil.example/b/app:1", driver.Anonymous, false},
			{byDigest, loginB, true},
			{"evil.example/b/app:1", loginB, true},
		} {
			got, err := settle(fmt.Sprintf("b%d-%s", i, when), c.ref, "", c.creds, c.own)
			if err == nil || got == privateDigest {
				t.Errorf("%s: B got A's private digest by %s: %q %v", when, c.ref, got, err)
			}
		}
	}
	attack("before")
	_, settle = pullerOn(t, host)
	attack("after")
}

// TestAPublicImageIsShared: machines without credentials share one job for
// a public image, as before secrets.
func TestAPublicImageIsShared(t *testing.T) {
	host := fake.New()
	host.AddImage("public:1", "sha256:1", driver.ImageConfig{Cmd: []string{"true"}})
	p, settle := pullerOn(t, host)
	for _, uid := range []string{"a", "b", "c"} {
		if _, err := settle(uid, "public:1", "", driver.Anonymous, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := host.Resolved("public:1"); n != 1 || len(p.jobs) != 1 {
		t.Errorf("%d resolves and %d jobs for three machines, want one", n, len(p.jobs))
	}
}
