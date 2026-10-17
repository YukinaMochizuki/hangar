// Package session issues and checks the credentials Hangar gives out after
// a login: a browser's session cookie, and a CLI's short-lived access token
// with the refresh token that renews it. Every credential is opaque
// (internal/auth/token) and stored only as its hash (store.Credentials), so
// deleting the row revokes it at once. Service tokens for agents are issued
// by the same machinery. See docs/decisions/authentication.md.
package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	// TypeService is an agent's service token. Its principal holds one at a
	// time, each in a family of its own.
	TypeService = "service"
	// TypeEndpoint is a browser's session on one private endpoint, the
	// host its audience names, in the family of the console session it was
	// handed off from.
	TypeEndpoint = "endpoint"
)

// familySep ends the token's uid in a service credential's family.
const familySep = "."

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
	// EndpointTTL is how long a private endpoint's session lasts, whatever
	// its use, and never past the console session it came from.
	EndpointTTL = 24 * time.Hour

	// touchEvery is how often a use is written back, at most: the idle
	// limit moves in steps of it rather than on every call.
	touchEvery = time.Minute
	// gcEvery is how often expired credentials are deleted.
	gcEvery = 10 * time.Minute
)

var (
	// ErrInvalid means the credential is unknown, revoked, expired or not
	// of the kind asked for. Callers say no more than that.
	ErrInvalid = auth.ErrInvalidCredential
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
	// used hears of a service token's use, at most once per touchEvery.
	used func(auth.Principal, time.Time)
	// live says whether the service token of a uid still stands.
	live func(ctx context.Context, p auth.Principal, uid string) (bool, error)
}

// Options configure a Manager; zero values are the defaults.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	Log *slog.Logger
	// Admit is asked at every refresh whether the principal may still
	// sign in; a no revokes the login. Nil admits everyone.
	Admit func(context.Context, auth.Principal) (bool, error)
	// ServiceUsed is told when a service token is used, at most once a
	// minute per token, to record it where people can see it. It must not
	// block. Nil is no one.
	ServiceUsed func(p auth.Principal, at time.Time)
	// ServiceLive is asked on every use of a service token whether the
	// token it was issued to, by uid, still exists and is not being
	// deleted; a no refuses it, as an unknown one is. Nil asks no one.
	ServiceLive func(ctx context.Context, p auth.Principal, uid string) (bool, error)
}

// New returns a Manager over the store.
func New(st store.Credentials, opts Options) *Manager {
	m := &Manager{store: st, now: opts.Now, log: opts.Log, admit: opts.Admit, used: opts.ServiceUsed, live: opts.ServiceLive}
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
	// Audience is the one host it counts on; empty is wherever its type
	// does.
	Audience string
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
		CreatedAt: m.now(), ExpiresAt: spec.ExpiresAt, IdleUntil: spec.IdleUntil, Audience: spec.Audience,
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

// StartEndpoint signs a principal in on one private endpoint's host, from
// the console session found: in its family, so ending that session ends
// this one too, for EndpointTTL and never past the console session's end.
func (m *Manager) StartEndpoint(ctx context.Context, from Found, host string) (Browser, error) {
	if host == "" || from.Family == "" {
		return Browser{}, errors.New("an endpoint session needs a host and a console session")
	}
	exp := earliest(m.now().Add(EndpointTTL), from.LoginEnd)
	s, _, err := m.Issue(ctx, Spec{Type: TypeEndpoint, Prefix: token.Endpoint, Family: from.Family, Principal: from.Principal, ExpiresAt: exp, Audience: host})
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

// Release ends a family Reserve made that will not be claimed, so it keeps
// nothing: its reservation is deleted, and the family marked revoked until
// the reservation would have expired.
func (m *Manager) Release(ctx context.Context, family string) error {
	_, err := m.store.RevokeCredentialFamily(ctx, family, m.now())
	return err
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

// ServiceToken is a service token just issued: its secret, which exists
// nowhere else, and when it ends.
type ServiceToken struct {
	Secret    token.Secret
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// IssueService issues a service token for the principal, the token of the
// uid, valid for ttl, in place of any it held: from the moment it is stored
// the earlier one authenticates nothing, and of two issued at once only one
// is left. The uid goes in the credential's family, so the secret works only
// while that very token stands (Options.ServiceLive), never for a later one
// of the same name.
func (m *Manager) IssueService(ctx context.Context, p auth.Principal, uid string, ttl time.Duration) (ServiceToken, error) {
	if uid == "" || strings.Contains(uid, familySep) {
		return ServiceToken{}, fmt.Errorf("issue a service token: bad uid %q", uid)
	}
	now := m.now()
	secret, c := m.credential(Spec{Type: TypeService, Prefix: token.Service, Family: uid + familySep + token.Random(), Principal: p, ExpiresAt: now.Add(ttl)})
	if err := m.store.ReplaceCredentials(ctx, c, now); err != nil {
		return ServiceToken{}, fmt.Errorf("issue a service token: %w", err)
	}
	return ServiceToken{Secret: secret, IssuedAt: now, ExpiresAt: c.ExpiresAt}, nil
}

// Found is a credential found valid.
type Found struct {
	Principal auth.Principal
	// Family is the login it belongs to.
	Family string
	// Until is when it ends unless used again; what it lets in ends then.
	Until time.Time
	// LoginEnd is when it ends whatever its use.
	LoginEnd time.Time
}

// Lookup returns who holds a credential of the type, and records its use.
// A credential bound to a host is refused: only Check, given that host,
// takes it.
func (m *Manager) Lookup(ctx context.Context, s token.Secret, typ string) (auth.Principal, error) {
	f, err := m.Check(ctx, s, typ, "")
	return f.Principal, err
}

// Check is Lookup for a credential whose audience is exactly audience,
// empty for one bound to no host, and says more of what it found. Unknown,
// revoked, expired, of another type and for another host are refused alike.
func (m *Manager) Check(ctx context.Context, s token.Secret, typ, audience string) (Found, error) {
	return m.check(ctx, s, typ, audience, true)
}

// Stands is Check without counting as a use: it records nothing, so a
// browser session's idle limit does not move. A held stream rechecks its
// credential with it.
func (m *Manager) Stands(ctx context.Context, s token.Secret, typ, audience string) error {
	_, err := m.check(ctx, s, typ, audience, false)
	return err
}

// LoginStands returns ErrInvalid once the CLI login family has ended:
// revoked, past its end, or with its refresh token gone idle. It records
// nothing. A stream opened with an access token is rechecked with it rather
// than with the token, whose hour a CLI renews without reopening its
// streams.
func (m *Manager) LoginStands(ctx context.Context, family string) error {
	ok, err := m.store.FamilyHolds(ctx, family, TypeRefresh, m.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalid
	}
	return nil
}

func (m *Manager) check(ctx context.Context, s token.Secret, typ, audience string, use bool) (Found, error) {
	hash := s.Hash()
	c, err := m.store.GetCredential(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return Found{}, ErrInvalid
	}
	if err != nil {
		return Found{}, err
	}
	now := m.now()
	// The store found the row by its hash; comparing again in constant time
	// costs nothing and depends on no index's comparison.
	if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(hash)) != 1 || c.Audience != audience {
		return Found{}, ErrInvalid
	}
	if c.Type != typ || !c.UsedAt.IsZero() || expired(c, now) {
		return Found{}, ErrInvalid
	}
	p, err := auth.ParsePrincipal(c.Principal)
	if err != nil {
		return Found{}, ErrInvalid
	}
	if c.Type == TypeService && m.live != nil {
		uid, _, ok := strings.Cut(c.Family, familySep)
		if !ok {
			return Found{}, ErrInvalid
		}
		if live, err := m.live(ctx, p, uid); err != nil {
			return Found{}, err
		} else if !live {
			return Found{}, ErrInvalid
		}
	}
	if !use {
		return found(p, c), nil
	}
	if c.Type == TypeService && now.Sub(c.LastUsedAt) >= touchEvery {
		if err := m.store.TouchCredential(ctx, c.Hash, now, time.Time{}); errors.Is(err, store.ErrNotFound) {
			return Found{}, ErrInvalid
		} else if err != nil {
			return Found{}, err
		}
		if m.used != nil {
			m.used(p, now)
		}
	}
	if idle := idleFor(c.Type); idle > 0 && now.Sub(c.LastUsedAt) >= touchEvery {
		c.IdleUntil = earliest(now.Add(idle), c.ExpiresAt)
		err := m.store.TouchCredential(ctx, c.Hash, now, c.IdleUntil)
		if errors.Is(err, store.ErrNotFound) {
			return Found{}, ErrInvalid
		}
		if err != nil {
			return Found{}, err
		}
	}
	return found(p, c), nil
}

func found(p auth.Principal, c store.Credential) Found {
	until := c.ExpiresAt
	if !c.IdleUntil.IsZero() {
		until = earliest(until, c.IdleUntil)
	}
	return Found{Principal: p, Family: c.Family, Until: until, LoginEnd: c.ExpiresAt}
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
