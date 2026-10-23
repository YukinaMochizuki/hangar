package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
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
		if a.env(TokenEnv) != "" {
			return credentials{}, usageErr("%s is set, but the context reaches the admin socket, which takes no token; unset it or use another context", TokenEnv)
		}
		return credentials{header: func(context.Context) (map[string]string, error) { return nil, nil }}, nil
	}
	// An agent's token in the environment stands in for whatever the
	// context would send.
	if v := a.env(TokenEnv); v != "" {
		s, err := parseServiceToken(v, TokenEnv)
		if err != nil {
			return credentials{}, err
		}
		return bearer(s), nil
	}
	switch c.Auth {
	case "token":
		s, err := readTokenFile(c.TokenFile)
		if err != nil {
			return credentials{}, err
		}
		return bearer(s), nil
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
	return credentials{}, usageErr("unknown auth %q; want oidc, token, dev or local (set it with --auth oidc)", c.Auth)
}

// TokenEnv names the environment variable an agent puts its service token
// in. It overrides the context's credentials.
const TokenEnv = "HANGAR_TOKEN"

// bearer sends a service token on every request.
func bearer(s token.Secret) credentials {
	return credentials{header: func(context.Context) (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer " + s.Reveal()}, nil
	}}
}

// parseServiceToken checks that v is a service token; from says where it
// came from. The error never holds the value.
func parseServiceToken(v, from string) (token.Secret, error) {
	s := token.Secret(strings.TrimSpace(v))
	if s.Prefix() != token.Service {
		return "", usageErr("%s does not hold a service token: one starts with %s", from, token.Service)
	}
	return s, nil
}

// maxTokenFile bounds what is read of a token file: a token is 47 bytes.
const maxTokenFile = 4096

// readTokenFile reads a service token from a regular file only its owner
// may read. A symlink is followed, as orchestrators mount secrets through
// them; what is checked is the file opened, so the check and the read are of
// the same file. It opens without blocking, so a FIFO cannot hang it before
// it is refused.
func readTokenFile(path string) (token.Secret, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	who, fix, err := exposure(f)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if who != "" {
		return "", fmt.Errorf("token file %s may be read by others (%s); run %s", path, who, fix)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile))
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	return parseServiceToken(string(b), "token file "+path)
}
