package session_test

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

// usage is when a credential was last used, and when it goes idle.
func usage(t *testing.T, st *sqlite.Store, hash string) [2]time.Time {
	t.Helper()
	c, err := st.GetCredential(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	return [2]time.Time{c.LastUsedAt, c.IdleUntil}
}

// TestAStreamRecheckIsNoUseOfItsSession: a stream rechecks its browser
// session without counting that as a use, so a tab left open still ends at
// the session's idle limit.
func TestAStreamRecheckIsNoUseOfItsSession(t *testing.T) {
	ctx := t.Context()
	m, c, st := newManager(t, session.Options{})
	b, err := m.StartBrowser(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://console.example.com"
	h := http.Header{"Cookie": {session.CookieName + "=" + b.Secret.Reveal()}, "Origin": {origin}}
	c.Advance(time.Hour)
	p, recheck, ok, err := session.Cookie{M: m, Origin: origin}.AuthenticateStream(ctx, h)
	if !ok || err != nil || p != alice {
		t.Fatalf("opening a stream: %v, %t, %v", p, ok, err)
	}
	opened := usage(t, st, b.Secret.Hash())
	for range 3 {
		c.Advance(session.SessionIdle / 4)
		if err := recheck(ctx); err != nil {
			t.Fatalf("a recheck within the idle limit: %v", err)
		}
	}
	if got := usage(t, st, b.Secret.Hash()); got != opened {
		t.Errorf("rechecks moved the session's use from %v to %v", opened, got)
	}
	c.Advance(session.SessionIdle / 4)
	if err := recheck(ctx); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a recheck past the idle limit, with the stream the only use: %v, want ErrInvalid", err)
	}
}

// TestAServiceTokenStreamRecheckIsNoUse: rechecking a service token's
// stream neither records a use nor reports one.
func TestAServiceTokenStreamRecheckIsNoUse(t *testing.T) {
	ctx := t.Context()
	var reported atomic.Int32
	m, c, st := newManager(t, session.Options{ServiceUsed: func(auth.Principal, time.Time) { reported.Add(1) }})
	tok, err := m.IssueService(ctx, agent, "uid1", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, recheck, ok, err := session.Bearer{M: m}.AuthenticateStream(ctx, http.Header{"Authorization": {"Bearer " + tok.Secret.Reveal()}})
	if !ok || err != nil {
		t.Fatalf("opening a stream: %t, %v", ok, err)
	}
	opened, uses := usage(t, st, tok.Secret.Hash()), reported.Load()
	for range 3 {
		c.Advance(time.Hour)
		if err := recheck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := usage(t, st, tok.Secret.Hash()); got != opened || reported.Load() != uses {
		t.Errorf("rechecks recorded a use: %v to %v, %d reports to %d", opened, got, uses, reported.Load())
	}
	c.Advance(24 * time.Hour)
	if err := recheck(ctx); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a recheck after the token expired: %v, want ErrInvalid", err)
	}
}

// TestAnAccessTokenStreamStandsWithItsLogin: a stream opened with a CLI
// access token outlives the token's hour, its deletion as expired, and the
// refreshes that replace it, and ends when the login does: revoked, or
// with its refresh token gone idle.
func TestAnAccessTokenStreamStandsWithItsLogin(t *testing.T) {
	ctx := t.Context()
	m, c, st := newManager(t, session.Options{})
	open := func(access string) func() error {
		t.Helper()
		_, recheck, ok, err := session.Bearer{M: m}.AuthenticateStream(ctx, http.Header{"Authorization": {"Bearer " + access}})
		if !ok || err != nil {
			t.Fatalf("opening a stream: %t, %v", ok, err)
		}
		return func() error { return recheck(ctx) }
	}

	cli, err := m.StartCLI(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	recheck := open(cli.Access.Reveal())
	c.Advance(session.AccessTTL + time.Minute)
	if err := st.DeleteExpired(ctx, c.Now()); err != nil {
		t.Fatal(err)
	}
	if err := recheck(); err != nil {
		t.Fatalf("a stream past its access token's hour, the token deleted: %v", err)
	}
	refreshed, err := m.Refresh(ctx, cli.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := recheck(); err != nil {
		t.Fatalf("a stream after its login refreshed: %v", err)
	}
	if err := m.Revoke(ctx, refreshed.Refresh); err != nil {
		t.Fatal(err)
	}
	if err := recheck(); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a stream after its login was revoked: %v, want ErrInvalid", err)
	}

	idle, err := m.StartCLI(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	recheck = open(idle.Access.Reveal())
	c.Advance(session.RefreshIdle)
	if err := recheck(); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a stream whose login's refresh token went idle: %v, want ErrInvalid", err)
	}
}
