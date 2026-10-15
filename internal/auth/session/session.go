// Package session issues and checks the credentials Hangar gives out after
// a login: a browser's session cookie, and a CLI's short-lived access token
// with the refresh token that renews it. Every credential is opaque
// (internal/auth/token) and stored only as its hash (store.Credentials), so
// deleting the row revokes it at once. Service tokens for agents are issued
// by the same machinery. See docs/decisions/authentication.md.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Credential types, as stored.
const (
	TypeSession = "session"
	TypeAccess  = "access"
	TypeRefresh = "refresh"
	// TypeReserved holds a family for a sign-in approved but not yet
	// claimed. Its hash is of nothing anyone holds, so it never
	// authenticates.
	TypeReserved = "reserved"
)

// Lifetimes. A browser session ends after SessionIdle without use, and
// SessionMax after sign-in whatever its use. A CLI login's refresh tokens
// end RefreshIdle after they were issued, and the whole login RefreshMax
// after sign-in; each access token lives AccessTTL.
const (
	SessionIdle = 3 * 24 * time.Hour
	SessionMax  = 14 * 24 * time.Hour
	AccessTTL   = time.Hour
	RefreshIdle = 30 * 24 * time.Hour
	RefreshMax  = 90 * 24 * time.Hour

	// touchEvery is how often a use is written back, at most: the idle
	// limit moves in steps of it rather than on every call.
	touchEvery = time.Minute
	// gcEvery is how often expired credentials are deleted.
	gcEvery = 10 * time.Minute
)

var (
	// ErrInvalid means the credential is unknown, revoked, expired or not
	// of the kind asked for. Callers say no more than that.
	ErrInvalid = errors.New("the credential is not valid; sign in again")
	// ErrReused means a refresh token was presented after it had been
	// used: someone else may hold a copy, so the whole login was revoked.
	ErrReused = errors.New("the refresh token was already used, so its login has been revoked; sign in again")
	// ErrNotAdmitted means the principal may no longer sign in, so the
	// login was revoked.
	ErrNotAdmitted = errors.New("the account may no longer sign in, so its login has been revoked")
)

// Manager issues, checks and revokes credentials.
type Manager struct {
	store store.Credentials
	now   func() time.Time
	log   *slog.Logger
	// admit says whether a principal may still renew a login.
	admit func(context.Context, auth.Principal) (bool, error)
}

// Options configure a Manager; zero values are the defaults.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	Log *slog.Logger
	// Admit is asked at every refresh whether the principal may still
	// sign in; a no revokes the login. Nil admits everyone.
	Admit func(context.Context, auth.Principal) (bool, error)
}

// New returns a Manager over the store.
func New(st store.Credentials, opts Options) *Manager {
	m := &Manager{store: st, now: opts.Now, log: opts.Log, admit: opts.Admit}
	if m.now == nil {
		m.now = time.Now
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	return m
}

// Now is the manager's clock.
func (m *Manager) Now() time.Time { return m.now() }

// Spec describes a credential to issue.
type Spec struct {
	Type   string
	Prefix string
	// Family is the login the credential belongs to; empty starts a new
	// one.
	Family    string
	Principal auth.Principal
	ExpiresAt time.Time
	// IdleUntil is when it ends unless used; zero is never.
	IdleUntil time.Time
}

// Issue creates a credential and returns its secret, which exists nowhere
// else: only its hash is stored. It fails with ErrInvalid if the family has
// been revoked, however recently.
func (m *Manager) Issue(ctx context.Context, spec Spec) (token.Secret, store.Credential, error) {
	if spec.Family == "" {
		spec.Family = token.Random()
	}
	secret, c := m.credential(spec)
	if err := m.create(ctx, c); err != nil {
		return "", store.Credential{}, err
	}
	return secret, c, nil
}

// credential makes a credential for spec, not yet stored.
func (m *Manager) credential(spec Spec) (token.Secret, store.Credential) {
	secret := token.New(spec.Prefix)
	return secret, store.Credential{
		Hash: secret.Hash(), Type: spec.Type, Family: spec.Family, Principal: spec.Principal.String(),
		CreatedAt: m.now(), ExpiresAt: spec.ExpiresAt, IdleUntil: spec.IdleUntil,
	}
}

// create stores credentials in one transaction, which refuses a revoked
// family: whatever revokes a login, nothing issued into it afterwards
// survives, even by a refresh that began before.
func (m *Manager) create(ctx context.Context, cs ...store.Credential) error {
	err := m.store.CreateCredentials(ctx, cs...)
	if errors.Is(err, store.ErrRevoked) {
		return ErrInvalid
	}
	if err != nil {
		return fmt.Errorf("issue credentials: %w", err)
	}
	return nil
}

// Browser is a browser's session.
type Browser struct {
	Secret    token.Secret
	ExpiresAt time.Time
}

// StartBrowser signs a principal in on a browser.
func (m *Manager) StartBrowser(ctx context.Context, p auth.Principal) (Browser, error) {
	now := m.now()
	exp := now.Add(SessionMax)
	s, _, err := m.Issue(ctx, Spec{Type: TypeSession, Prefix: token.Session, Principal: p, ExpiresAt: exp, IdleUntil: now.Add(SessionIdle)})
	return Browser{Secret: s, ExpiresAt: exp}, err
}

// Tokens are what a CLI login holds.
type Tokens struct {
	Access          token.Secret
	AccessExpiresAt time.Time
	Refresh         token.Secret
	Principal       auth.Principal
}

// StartCLI signs a principal in on a CLI: a new login, with its first access
// and refresh tokens.
func (m *Manager) StartCLI(ctx context.Context, p auth.Principal) (Tokens, error) {
	return m.cliTokens(ctx, p, token.Random(), m.now().Add(RefreshMax))
}

// Reserve starts a login for a principal that will claim its tokens later,
// within ttl: a sign-in already approved, waiting for the CLI to exchange
// its code or poll its device login. It records the family under the
// principal, holding no secret, so revoking the principal's logins in the
// meantime revokes this one too, and StartCLIIn then issues nothing.
func (m *Manager) Reserve(ctx context.Context, p auth.Principal, ttl time.Duration) (family string, err error) {
	family = token.Random()
	now := m.now()
	c := store.Credential{
		Hash: token.Hash(token.Random()), Type: TypeReserved, Family: family, Principal: p.String(),
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	return family, m.create(ctx, c)
}

// StartCLIIn signs a principal in on a CLI in the family Reserve made. It
// fails with ErrInvalid if that family has been revoked since.
func (m *Manager) StartCLIIn(ctx context.Context, p auth.Principal, family string) (Tokens, error) {
	return m.cliTokens(ctx, p, family, m.now().Add(RefreshMax))
}

// cliTokens issues an access and a refresh token, together, in a login that
// ends at loginEnd.
func (m *Manager) cliTokens(ctx context.Context, p auth.Principal, family string, loginEnd time.Time) (Tokens, error) {
	now := m.now()
	accessExp := earliest(now.Add(AccessTTL), loginEnd)
	access, ac := m.credential(Spec{Type: TypeAccess, Prefix: token.Access, Family: family, Principal: p, ExpiresAt: accessExp})
	refresh, rc := m.credential(Spec{
		Type: TypeRefresh, Prefix: token.Refresh, Family: family, Principal: p,
		ExpiresAt: loginEnd, IdleUntil: earliest(now.Add(RefreshIdle), loginEnd),
	})
	if err := m.create(ctx, ac, rc); err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, AccessExpiresAt: accessExp, Refresh: refresh, Principal: p}, nil
}

// Refresh trades a refresh token for a new access token and a new refresh
// token in the same login; the one presented is spent. Presenting a spent
// one again revokes the login, since then two parties hold it.
func (m *Manager) Refresh(ctx context.Context, refresh token.Secret) (Tokens, error) {
	if refresh.Prefix() != token.Refresh {
		return Tokens{}, ErrInvalid
	}
	now := m.now()
	c, err := m.store.ConsumeCredential(ctx, refresh.Hash(), now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Tokens{}, ErrInvalid
	case errors.Is(err, store.ErrConflict):
		if c.Type != TypeRefresh {
			return Tokens{}, ErrInvalid
		}
		if _, err := m.store.RevokeCredentialFamily(ctx, c.Family, now); err != nil {
			return Tokens{}, err
		}
		m.log.Warn("a used refresh token was presented again; revoked its login", "principal", c.Principal)
		return Tokens{}, ErrReused
	case err != nil:
		return Tokens{}, err
	}
	if c.Type != TypeRefresh || expired(c, now) {
		return Tokens{}, ErrInvalid
	}
	p, err := auth.ParsePrincipal(c.Principal)
	if err != nil {
		return Tokens{}, err
	}
	if m.admit != nil {
		ok, err := m.admit(ctx, p)
		if err != nil {
			return Tokens{}, err
		}
		if !ok {
			if _, err := m.store.RevokeCredentialFamily(ctx, c.Family, now); err != nil {
				return Tokens{}, err
			}
			return Tokens{}, ErrNotAdmitted
		}
	}
	// A revocation landing since the token was spent, by a replay, a logout
	// or an administrator, makes this refuse: the family is marked first.
	return m.cliTokens(ctx, p, c.Family, c.ExpiresAt)
}

// Lookup returns who holds a credential of the type, and records its use.
func (m *Manager) Lookup(ctx context.Context, s token.Secret, typ string) (auth.Principal, error) {
	c, err := m.store.GetCredential(ctx, s.Hash())
	if errors.Is(err, store.ErrNotFound) {
		return auth.Principal{}, ErrInvalid
	}
	if err != nil {
		return auth.Principal{}, err
	}
	now := m.now()
	if c.Type != typ || !c.UsedAt.IsZero() || expired(c, now) {
		return auth.Principal{}, ErrInvalid
	}
	if idle := idleFor(c.Type); idle > 0 && now.Sub(c.LastUsedAt) >= touchEvery {
		err := m.store.TouchCredential(ctx, c.Hash, now, earliest(now.Add(idle), c.ExpiresAt))
		if errors.Is(err, store.ErrNotFound) {
			return auth.Principal{}, ErrInvalid
		}
		if err != nil {
			return auth.Principal{}, err
		}
	}
	return auth.ParsePrincipal(c.Principal)
}

// Revoke ends the login a credential belongs to: every credential issued
// with it, and any it would be issued later, as by a refresh in flight. An
// unknown credential is no error, so logging out twice is fine.
func (m *Manager) Revoke(ctx context.Context, s token.Secret) error {
	c, err := m.store.GetCredential(ctx, s.Hash())
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = m.store.RevokeCredentialFamily(ctx, c.Family, m.now())
	return err
}

// RevokePrincipal ends every login of a principal, as Revoke does each, and
// returns how many credentials that deleted.
func (m *Manager) RevokePrincipal(ctx context.Context, p auth.Principal) (int, error) {
	return m.store.RevokeCredentialsOf(ctx, p.String(), m.now())
}

// Run deletes expired credentials and logins in progress until ctx is done.
func (m *Manager) Run(ctx context.Context) error {
	t := time.NewTicker(gcEvery)
	defer t.Stop()
	for {
		if err := m.store.DeleteExpired(ctx, m.now()); err != nil && ctx.Err() == nil {
			m.log.Warn("could not delete expired credentials", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func idleFor(typ string) time.Duration {
	if typ == TypeSession {
		return SessionIdle
	}
	return 0
}

func expired(c store.Credential, now time.Time) bool {
	return !now.Before(c.ExpiresAt) || (!c.IdleUntil.IsZero() && !now.Before(c.IdleUntil))
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
