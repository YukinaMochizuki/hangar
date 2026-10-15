package store

import (
	"context"
	"errors"
	"time"
)

// ErrRevoked means a credential was to be issued into a family that has been
// revoked.
var ErrRevoked = errors.New("store: credential family revoked")

// Credential is a secret Hangar issued, kept by its hash alone. The store
// gives no meaning to its fields beyond the operations below; internal/auth
// does. Zero times are unset.
type Credential struct {
	// Hash identifies the credential: a hash of the secret, never the
	// secret.
	Hash string
	Type string
	// Family groups what one login issued, so revoking it revokes them all.
	Family    string
	Principal string
	CreatedAt time.Time
	// ExpiresAt is when it ends, whatever its use.
	ExpiresAt time.Time
	// IdleUntil is when it ends unless used before; zero is never.
	IdleUntil  time.Time
	LastUsedAt time.Time
	// UsedAt is when ConsumeCredential took it; zero while it has not.
	UsedAt time.Time
}

// AuthRequest is a login in progress under an opaque key, with data only its
// caller reads.
type AuthRequest struct {
	Key       string
	Data      []byte
	ExpiresAt time.Time
}

// Credentials keeps issued credentials and logins in progress. It is separate
// from Store's objects: nothing here bumps the revision or reaches a watch.
// storetest holds the contract.
type Credentials interface {
	// CreateCredentials stores new credentials, all or none: ErrExists if a
	// hash is taken, ErrRevoked if a family has been revoked. The check and
	// the insert are one transaction, so nothing is ever issued into a
	// family a concurrent revocation has ended.
	CreateCredentials(ctx context.Context, cs ...Credential) error
	// GetCredential returns the credential with the hash, or ErrNotFound.
	GetCredential(ctx context.Context, hash string) (Credential, error)
	// TouchCredential records a use: its last use and its new idle limit.
	// ErrNotFound if it is gone.
	TouchCredential(ctx context.Context, hash string, lastUsed, idleUntil time.Time) error
	// ConsumeCredential sets UsedAt, once: it returns the credential as it
	// was before, or ErrConflict, with the credential, if it was consumed
	// already, or ErrNotFound. Of two concurrent calls only one succeeds.
	ConsumeCredential(ctx context.Context, hash string, at time.Time) (Credential, error)
	// RevokeCredentialFamily marks the family revoked, until the later of
	// until and its last credential's expiry, deletes its credentials and
	// returns how many there were.
	RevokeCredentialFamily(ctx context.Context, family string, until time.Time) (int, error)
	// RevokeCredentialsOf revokes every family the principal holds a
	// credential in, as RevokeCredentialFamily does, and returns how many
	// credentials it deleted.
	RevokeCredentialsOf(ctx context.Context, principal string, until time.Time) (int, error)

	// CreateAuthRequest stores a new login in progress; ErrExists if the
	// key is taken.
	CreateAuthRequest(ctx context.Context, r AuthRequest) error
	// GetAuthRequest returns the request with the key, or ErrNotFound.
	GetAuthRequest(ctx context.Context, key string) (AuthRequest, error)
	// SwapAuthRequest replaces the request's data, but only while it is
	// still old: ErrConflict if it changed, ErrNotFound if it is gone.
	SwapAuthRequest(ctx context.Context, key string, old, data []byte) error
	// TakeAuthRequest deletes the request and returns it, so that only one
	// caller ever gets it; ErrNotFound if it is gone.
	TakeAuthRequest(ctx context.Context, key string) (AuthRequest, error)
	// CountAuthRequests returns how many requests whose key starts with
	// prefix have not expired by now.
	CountAuthRequests(ctx context.Context, prefix string, now time.Time) (int, error)

	// DeleteExpired deletes the credentials, requests and revocation marks
	// that have expired, or gone idle, by now.
	DeleteExpired(ctx context.Context, now time.Time) error
}
