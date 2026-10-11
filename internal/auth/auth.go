// Package auth turns a request into a Principal: who is calling. Each listener
// runs its own chain of authenticators; see docs/decisions/authentication.md.
// Deciding what a principal may do is internal/authz's job.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
)

// Type is the kind of identity behind a principal.
type Type int

const (
	// User is a person, identified by an email Cloudflare Access verified.
	User Type = iota + 1
	// ServiceToken is an agent, identified by its Access service token's
	// client ID.
	ServiceToken
	// Node is a hangar-node process.
	Node
	// System is the apiserver itself, acting through its controllers.
	System
)

func (t Type) String() string {
	switch t {
	case User:
		return "user"
	case ServiceToken:
		return "serviceToken"
	case Node:
		return "node"
	case System:
		return "system"
	}
	return fmt.Sprintf("Type(%d)", int(t))
}

// Principal is an authenticated caller.
type Principal struct {
	Type Type
	ID   string
}

func (p Principal) String() string { return p.Type.String() + ":" + p.ID }

// SystemPrincipal is the identity in-process controllers act as.
var SystemPrincipal = Principal{Type: System, ID: "apiserver"}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal the request was authenticated as.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator recognizes one kind of credential. It returns ok false when
// the request carries none of its credentials, and an error when it carries
// some that are invalid.
type Authenticator interface {
	Authenticate(ctx context.Context, header http.Header) (p Principal, ok bool, err error)
}

// ErrNoCredentials is returned when no authenticator in the chain recognized
// the request.
var ErrNoCredentials = errors.New("no credentials")

// Chain tries authenticators in order; the first that recognizes the request
// decides it.
type Chain []Authenticator

// Authenticate returns a context carrying the request's principal.
func (c Chain) Authenticate(ctx context.Context, header http.Header) (context.Context, error) {
	for _, a := range c {
		p, ok, err := a.Authenticate(ctx, header)
		if err != nil {
			return ctx, err
		}
		if ok {
			return WithPrincipal(ctx, p), nil
		}
	}
	return ctx, ErrNoCredentials
}

// Interceptor authenticates every incoming Connect call with the chain and
// rejects calls it cannot authenticate.
func (c Chain) Interceptor() connect.Interceptor { return interceptor{c} }

type interceptor struct{ chain Chain }

func (i interceptor) authenticate(ctx context.Context, header http.Header) (context.Context, error) {
	ctx, err := i.chain.Authenticate(ctx, header)
	if err != nil {
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	return ctx, nil
}

func (i interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := i.authenticate(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.authenticate(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
