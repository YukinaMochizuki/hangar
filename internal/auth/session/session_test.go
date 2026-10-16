package session_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

var alice = auth.Principal{Type: auth.User, ID: "alice@example.com"}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newManager(t *testing.T, opts session.Options) (*session.Manager, *clock, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := &clock{now: time.UnixMilli(1_800_000_000_000)}
	opts.Now = c.Now
	return session.New(st, opts), c, st
}

// TestBrowserSessionExpires: a browser session ends after SessionIdle
// without use, and SessionMax after sign-in however much it is used.
func TestBrowserSessionExpires(t *testing.T) {
	ctx := t.Context()
	m, c, _ := newManager(t, session.Options{})
	b, err := m.StartBrowser(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if b.Secret.Prefix() != token.Session {
		t.Errorf("session secret %v has the wrong prefix", b.Secret)
	}
	if p, err := m.Lookup(ctx, b.Secret, session.TypeSession); err != nil || p != alice {
		t.Fatalf("lookup: %v, %v", p, err)
	}
	// Used every day, it outlives one idle period but not its maximum.
	for day := 1; day < 14; day++ {
		c.Advance(24 * time.Hour)
		if _, err := m.Lookup(ctx, b.Secret, session.TypeSession); err != nil {
			t.Fatalf("day %d: a session in daily use ended: %v", day, err)
		}
	}
	c.Advance(24 * time.Hour)
	if _, err := m.Lookup(ctx, b.Secret, session.TypeSession); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("after 14 days: %v, want ErrInvalid", err)
	}

	idle, err := m.StartBrowser(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(session.SessionIdle - time.Minute)
	if _, err := m.Lookup(ctx, idle.Secret, session.TypeSession); err != nil {
		t.Fatalf("just before its idle limit: %v", err)
	}
	c.Advance(session.SessionIdle)
	if _, err := m.Lookup(ctx, idle.Secret, session.TypeSession); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("after an idle period: %v, want ErrInvalid", err)
	}
}

// TestCredentialsAreTyped: a credential counts only as its own type.
func TestCredentialsAreTyped(t *testing.T) {
	ctx := t.Context()
	m, _, _ := newManager(t, session.Options{})
	b, _ := m.StartBrowser(ctx, alice)
	cli, err := m.StartCLI(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		s   token.Secret
		typ string
	}{{b.Secret, session.TypeAccess}, {cli.Access, session.TypeSession}, {cli.Refresh, session.TypeAccess}} {
		if _, err := m.Lookup(ctx, tt.s, tt.typ); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("%v as %s: %v, want ErrInvalid", tt.s, tt.typ, err)
		}
	}
	if _, err := m.Refresh(ctx, cli.Access); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("an access token as a refresh token: %v", err)
	}
}

// TestAccessTokenExpires: an access token lives AccessTTL.
func TestAccessTokenExpires(t *testing.T) {
	ctx := t.Context()
	m, c, _ := newManager(t, session.Options{})
	cli, _ := m.StartCLI(ctx, alice)
	if !cli.AccessExpiresAt.Equal(c.Now().Add(session.AccessTTL)) {
		t.Errorf("access expires %v", cli.AccessExpiresAt)
	}
	c.Advance(session.AccessTTL - time.Second)
	if _, err := m.Lookup(ctx, cli.Access, session.TypeAccess); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Second)
	if _, err := m.Lookup(ctx, cli.Access, session.TypeAccess); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("an expired access token: %v", err)
	}
}

// TestRefreshRotates: each refresh spends its token and issues a new pair;
// presenting a spent one revokes the whole login, the newest tokens
// included, and no other login.
func TestRefreshRotates(t *testing.T) {
	ctx := t.Context()
	m, c, _ := newManager(t, session.Options{})
	first, _ := m.StartCLI(ctx, alice)
	other, _ := m.StartCLI(ctx, alice)
	c.Advance(time.Hour)
	second, err := m.Refresh(ctx, first.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if second.Refresh == first.Refresh || second.Access == first.Access || second.Principal != alice {
		t.Fatalf("refresh did not rotate: %+v", second)
	}
	if _, err := m.Lookup(ctx, second.Access, session.TypeAccess); err != nil {
		t.Fatal(err)
	}
	// The thief replays the first refresh token.
	if _, err := m.Refresh(ctx, first.Refresh); !errors.Is(err, session.ErrReused) {
		t.Fatalf("a spent refresh token: %v, want ErrReused", err)
	}
	if _, err := m.Lookup(ctx, second.Access, session.TypeAccess); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("the login's newest access token outlived the reuse: %v", err)
	}
	if _, err := m.Refresh(ctx, second.Refresh); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("the login's newest refresh token outlived the reuse: %v", err)
	}
	if _, err := m.Refresh(ctx, other.Refresh); err != nil {
		t.Errorf("another login went with it: %v", err)
	}
}

// TestRefreshRaceHasOneWinner: of two concurrent refreshes with one token,
// one gets tokens and the other revokes the login.
func TestRefreshRaceHasOneWinner(t *testing.T) {
	ctx := t.Context()
	m, _, _ := newManager(t, session.Options{})
	cli, _ := m.StartCLI(ctx, alice)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { _, errs[i] = m.Refresh(ctx, cli.Refresh) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, session.ErrReused) {
			t.Errorf("refresh: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d refreshes succeeded, want 1", ok)
	}
}

// TestRevocationBeatsARefreshInFlight: a refresh that has spent its token
// and is still deciding (held here inside admit) issues nothing once its
// login is revoked meanwhile, whether by a replay of the same token, a
// logout or an administrator: nothing it could hand out works afterwards.
func TestRevocationBeatsARefreshInFlight(t *testing.T) {
	for _, revoke := range []struct {
		name string
		do   func(ctx context.Context, m *session.Manager, cli session.Tokens) error
	}{
		{"replay", func(ctx context.Context, m *session.Manager, cli session.Tokens) error {
			if _, err := m.Refresh(ctx, cli.Refresh); !errors.Is(err, session.ErrReused) {
				return fmt.Errorf("the replay: %w, want ErrReused", err)
			}
			return nil
		}},
		{"logout", func(ctx context.Context, m *session.Manager, cli session.Tokens) error {
			return m.Revoke(ctx, cli.Access)
		}},
		{"revoke the person", func(ctx context.Context, m *session.Manager, cli session.Tokens) error {
			_, err := m.RevokePrincipal(ctx, alice)
			return err
		}},
	} {
		t.Run(revoke.name, func(t *testing.T) {
			ctx := t.Context()
			inAdmit, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			m, _, _ := newManager(t, session.Options{Admit: func(context.Context, auth.Principal) (bool, error) {
				first := false
				once.Do(func() { first = true })
				if first {
					close(inAdmit)
					<-release
				}
				return true, nil
			}})
			cli, err := m.StartCLI(ctx, alice)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				tok session.Tokens
				err error
			}
			done := make(chan result, 1)
			go func() {
				tok, err := m.Refresh(ctx, cli.Refresh)
				done <- result{tok, err}
			}()
			<-inAdmit
			if err := revoke.do(ctx, m, cli); err != nil {
				t.Fatal(err)
			}
			close(release)
			r := <-done
			if r.err == nil {
				t.Errorf("the refresh in flight got tokens into a revoked login")
			}
			for _, s := range []token.Secret{r.tok.Access, cli.Access} {
				if s == "" {
					continue
				}
				if _, err := m.Lookup(ctx, s, session.TypeAccess); !errors.Is(err, session.ErrInvalid) {
					t.Errorf("%v works after the revocation: %v", s, err)
				}
			}
			if r.tok.Refresh != "" {
				if _, err := m.Refresh(ctx, r.tok.Refresh); err == nil {
					t.Error("the refresh token handed out in flight works")
				}
			}
		})
	}
}

// TestRefreshLimits: a refresh token ends RefreshIdle after it was issued,
// and a login RefreshMax after it started, however often it is refreshed.
func TestRefreshLimits(t *testing.T) {
	ctx := t.Context()
	m, c, _ := newManager(t, session.Options{})
	idle, _ := m.StartCLI(ctx, alice)
	c.Advance(session.RefreshIdle)
	if _, err := m.Refresh(ctx, idle.Refresh); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("an idle refresh token: %v", err)
	}

	cli, _ := m.StartCLI(ctx, alice)
	start := c.Now()
	for c.Now().Before(start.Add(session.RefreshMax - 20*24*time.Hour)) {
		c.Advance(20 * 24 * time.Hour)
		next, err := m.Refresh(ctx, cli.Refresh)
		if err != nil {
			t.Fatalf("refresh at %v: %v", c.Now().Sub(start), err)
		}
		if next.AccessExpiresAt.After(start.Add(session.RefreshMax)) {
			t.Errorf("an access token outlives its login")
		}
		cli = next
	}
	c.Advance(start.Add(session.RefreshMax).Sub(c.Now()))
	if _, err := m.Refresh(ctx, cli.Refresh); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a refresh past the login's maximum: %v", err)
	}
}

// TestRefreshAsksAdmit: someone no longer admitted cannot renew a login,
// and the login is revoked.
func TestRefreshAsksAdmit(t *testing.T) {
	ctx := t.Context()
	admitted := true
	m, _, _ := newManager(t, session.Options{Admit: func(context.Context, auth.Principal) (bool, error) { return admitted, nil }})
	cli, _ := m.StartCLI(ctx, alice)
	admitted = false
	if _, err := m.Refresh(ctx, cli.Refresh); !errors.Is(err, session.ErrNotAdmitted) {
		t.Fatalf("refresh after the invitation was withdrawn: %v", err)
	}
	if _, err := m.Lookup(ctx, cli.Access, session.TypeAccess); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("the login's access token survived: %v", err)
	}
}

// TestRevoke: revoking one credential ends its login; revoking a principal
// ends every login of theirs and no one else's.
func TestRevoke(t *testing.T) {
	ctx := t.Context()
	m, _, _ := newManager(t, session.Options{})
	bob := auth.Principal{Type: auth.User, ID: "bob@example.com"}
	a1, _ := m.StartCLI(ctx, alice)
	a2, _ := m.StartBrowser(ctx, alice)
	b1, _ := m.StartBrowser(ctx, bob)
	if err := m.Revoke(ctx, a1.Refresh); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Lookup(ctx, a1.Access, session.TypeAccess); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("logged out, the access token still works: %v", err)
	}
	if _, err := m.Lookup(ctx, a2.Secret, session.TypeSession); err != nil {
		t.Errorf("logging out one login ended another: %v", err)
	}
	if err := m.Revoke(ctx, a1.Refresh); err != nil {
		t.Errorf("logging out twice: %v", err)
	}
	if n, err := m.RevokePrincipal(ctx, alice); err != nil || n != 1 {
		t.Errorf("revoke alice: %d, %v", n, err)
	}
	if _, err := m.Lookup(ctx, a2.Secret, session.TypeSession); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("revoked, alice's browser session still works: %v", err)
	}
	if _, err := m.Lookup(ctx, b1.Secret, session.TypeSession); err != nil {
		t.Errorf("revoking alice ended bob's session: %v", err)
	}
}

// TestCookieNeedsTheOrigin: a session cookie counts only on a request from
// the console's own origin.
func TestCookieNeedsTheOrigin(t *testing.T) {
	ctx := t.Context()
	m, _, _ := newManager(t, session.Options{})
	b, _ := m.StartBrowser(ctx, alice)
	const origin = "https://console.example.com"
	ck := session.Cookie{M: m, Origin: origin}
	header := func(kv ...string) http.Header {
		h := http.Header{"Cookie": {session.CookieName + "=" + b.Secret.Reveal()}}
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	for _, tt := range []struct {
		name string
		h    http.Header
		ok   bool
	}{
		{"same origin", header("Origin", origin), true},
		{"same origin, fetch metadata", header("Origin", origin, "Sec-Fetch-Site", "same-origin"), true},
		{"no origin", header(), false},
		{"another origin", header("Origin", "https://evil.example"), false},
		{"another scheme", header("Origin", "http://console.example.com"), false},
		{"a sibling site", header("Origin", origin, "Sec-Fetch-Site", "same-site"), false},
		{"cross site", header("Origin", origin, "Sec-Fetch-Site", "cross-site"), false},
	} {
		p, ok, err := ck.Authenticate(ctx, tt.h)
		if ok != tt.ok || (tt.ok && (err != nil || p != alice)) || (!tt.ok && err == nil) {
			t.Errorf("%s: %v, %t, %v", tt.name, p, ok, err)
		}
	}
	if _, ok, err := ck.Authenticate(ctx, http.Header{"Origin": {origin}}); ok || err != nil {
		t.Errorf("no cookie: %t, %v", ok, err)
	}
}

// TestBearer: only an access token is a bearer credential, and an invalid
// one is refused rather than passed over.
func TestBearer(t *testing.T) {
	ctx := t.Context()
	m, _, _ := newManager(t, session.Options{})
	cli, _ := m.StartCLI(ctx, alice)
	b, _ := m.StartBrowser(ctx, alice)
	bearer := session.Bearer{M: m}
	auth := func(v string) (auth.Principal, bool, error) {
		return bearer.Authenticate(ctx, http.Header{"Authorization": {v}})
	}
	if p, ok, err := auth("Bearer " + cli.Access.Reveal()); !ok || err != nil || p != alice {
		t.Errorf("access token: %v, %t, %v", p, ok, err)
	}
	for _, v := range []string{"Bearer " + cli.Refresh.Reveal(), "Bearer " + b.Secret.Reveal(), "Bearer hga_nope", "Basic abc"} {
		if _, ok, err := auth(v); ok || err == nil {
			t.Errorf("%.12s…: %t, %v", v, ok, err)
		}
	}
	if _, ok, err := bearer.Authenticate(ctx, http.Header{}); ok || err != nil {
		t.Errorf("no header: %t, %v", ok, err)
	}
}

var agent = auth.Principal{Type: auth.ServiceToken, ID: "web/deployer"}

// TestServiceTokens: a service token works until it expires, as a bearer
// token, and issuing another replaces it; its uses are reported at most
// once a minute; access-token-free bearers take only service tokens.
func TestServiceTokens(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	var uses []time.Time
	m, c, _ := newManager(t, session.Options{ServiceUsed: func(p auth.Principal, at time.Time) {
		if p != agent {
			t.Errorf("use reported for %v", p)
		}
		mu.Lock()
		uses = append(uses, at)
		mu.Unlock()
	}})
	first, err := m.IssueService(ctx, agent, "uid-1", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first.Secret.Prefix() != token.Service || !first.ExpiresAt.Equal(first.IssuedAt.Add(2*time.Hour)) {
		t.Fatalf("issued %v until %v", first.Secret, first.ExpiresAt)
	}
	bearer := session.Bearer{M: m}
	authn := func(b session.Bearer, s token.Secret) (auth.Principal, error) {
		p, ok, err := b.Authenticate(ctx, http.Header{"Authorization": {"Bearer " + s.Reveal()}})
		if err == nil && !ok {
			err = errors.New("not recognized")
		}
		return p, err
	}
	if p, err := authn(bearer, first.Secret); err != nil || p != agent {
		t.Fatalf("service token: %v, %v", p, err)
	}
	if _, err := m.Lookup(ctx, first.Secret, session.TypeAccess); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a service token as an access token: %v", err)
	}
	// Uses within a minute are reported once.
	if _, err := authn(bearer, first.Secret); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if _, err := authn(bearer, first.Secret); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(uses) != 2 {
		t.Errorf("%d uses reported, want 2: %v", len(uses), uses)
	}
	mu.Unlock()

	second, err := m.IssueService(ctx, agent, "uid-1", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authn(bearer, first.Secret); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a replaced service token: %v", err)
	}
	if _, err := authn(bearer, second.Secret); err != nil {
		t.Errorf("its replacement: %v", err)
	}

	// Without access tokens, a bearer takes service tokens alone.
	cli, _ := m.StartCLI(ctx, alice)
	tokensOnly := session.Bearer{M: m, AccessOff: true}
	if _, err := authn(tokensOnly, cli.Access); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("an access token where only service tokens count: %v", err)
	}
	if _, err := authn(tokensOnly, second.Secret); err != nil {
		t.Errorf("a service token where only they count: %v", err)
	}
	for _, s := range []token.Secret{"hgr_nope", token.New(token.Service)} {
		if _, err := authn(bearer, s); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("an unknown service token: %v", err)
		}
	}

	c.Advance(2 * time.Hour)
	if _, err := authn(bearer, second.Secret); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("an expired service token: %v", err)
	}
	// Revoking the principal ends it, as deleting the token does.
	third, _ := m.IssueService(ctx, agent, "uid-1", time.Hour)
	if _, err := m.RevokePrincipal(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := authn(bearer, third.Secret); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a revoked service token: %v", err)
	}
}

// TestServiceTokenNeedsItsToken: a service token works only while the token
// of the uid it was issued to stands, and is refused, as an unknown one is,
// once that token is gone or another holds its name.
func TestServiceTokenNeedsItsToken(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	standing := "uid-1"
	m, _, _ := newManager(t, session.Options{ServiceLive: func(_ context.Context, p auth.Principal, uid string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return p == agent && uid == standing, nil
	}})
	issued, err := m.IssueService(ctx, agent, "uid-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Lookup(ctx, issued.Secret, session.TypeService); err != nil {
		t.Fatalf("while its token stands: %v", err)
	}
	mu.Lock()
	standing = "uid-2" // a token of the same name, made again
	mu.Unlock()
	if _, err := m.Lookup(ctx, issued.Secret, session.TypeService); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("once another token holds the name: %v", err)
	}
	if _, err := m.IssueService(ctx, agent, "a.b", time.Hour); err == nil {
		t.Error("a uid holding the family separator was taken")
	}
}

// TestSecretsDoNotPrint: a secret never shows in fmt or in an error.
func TestSecretsDoNotPrint(t *testing.T) {
	s := token.New(token.Access)
	err := fmt.Errorf("wrapped: %w", fmt.Errorf("token %v %s %q %+v %#v %x", s, s, s, s, s, s))
	for _, out := range []string{fmt.Sprint(s), err.Error(), fmt.Sprintf("%v", []token.Secret{s})} {
		if strings.Contains(out, s.Reveal()[len(token.Access):]) {
			t.Errorf("printed the secret: %s", out)
		}
	}
	if !strings.HasPrefix(fmt.Sprint(s), token.Access) {
		t.Errorf("the redacted form %q lost the prefix", fmt.Sprint(s))
	}
}
