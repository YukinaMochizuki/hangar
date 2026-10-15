package cli

import (
	"context"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// credentials adds whatever the context needs to every request.
type credentials struct {
	header func(ctx context.Context) (map[string]string, error)
}

func (c credentials) set(ctx context.Context, set func(k, v string)) error {
	h, err := c.header(ctx)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	for k, v := range h {
		set(k, v)
	}
	return nil
}

func (c credentials) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := c.set(ctx, req.Header().Set); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (c credentials) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		// A failure here leaves the request without credentials, and the
		// server rejects it as unauthenticated.
		_ = c.set(ctx, conn.RequestHeader().Set)
		return conn
	}
}

func (c credentials) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (a *app) credentials(c *Context) (credentials, error) {
	if err := c.check(); err != nil {
		return credentials{}, err
	}
	// The admin socket knows its caller from the kernel; it has no use for a
	// credential, and must not be handed one.
	if c.local() {
		return credentials{header: func(context.Context) (map[string]string, error) { return nil, nil }}, nil
	}
	switch c.Auth {
	case "dev":
		if c.User == "" {
			return credentials{}, usageErr("context uses dev auth but has no user")
		}
		return credentials{header: func(context.Context) (map[string]string, error) {
			return map[string]string{auth.DevUserHeader: c.User}, nil
		}}, nil
	case "", "oidc":
		ts := a.tokens(c)
		return credentials{header: func(ctx context.Context) (map[string]string, error) {
			tok, err := ts.access(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]string{"Authorization": "Bearer " + tok.Reveal()}, nil
		}}, nil
	}
	return credentials{}, usageErr("unknown auth %q; want oidc, dev or local (set it with --auth oidc)", c.Auth)
}
