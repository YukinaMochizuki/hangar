package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

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

// Service token credentials come from the environment, never the config file,
// so agents can be handed them without writing secrets to disk.
const (
	envClientID     = "HANGAR_CF_CLIENT_ID"
	envClientSecret = "HANGAR_CF_CLIENT_SECRET"
)

func (a *app) credentials(c *Context) (credentials, error) {
	if id, secret := a.env(envClientID), a.env(envClientSecret); id != "" || secret != "" {
		if id == "" || secret == "" {
			return credentials{}, usageErr("set both %s and %s, or neither", envClientID, envClientSecret)
		}
		return credentials{header: func(context.Context) (map[string]string, error) {
			return map[string]string{"CF-Access-Client-Id": id, "CF-Access-Client-Secret": secret}, nil
		}}, nil
	}
	switch c.Auth {
	case "dev":
		if c.User == "" {
			return credentials{}, usageErr("context uses dev auth but has no user")
		}
		return credentials{header: func(context.Context) (map[string]string, error) {
			return map[string]string{auth.DevUserHeader: c.User}, nil
		}}, nil
	case "", "cloudflare":
		t := &accessToken{run: a.runCloudflared, app: c.Endpoint, now: time.Now}
		return credentials{header: func(ctx context.Context) (map[string]string, error) {
			tok, err := t.get(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]string{"cf-access-token": tok}, nil
		}}, nil
	}
	return credentials{}, usageErr("unknown auth %q; want cloudflare or dev", c.Auth)
}

// accessToken fetches a Cloudflare Access token through cloudflared and
// reuses it until shortly before it expires.
type accessToken struct {
	run func(ctx context.Context, args ...string) (string, error)
	app string
	now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (t *accessToken) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.expires.Add(-time.Minute)) {
		return t.token, nil
	}
	out, err := t.run(ctx, "access", "token", "-app="+t.app)
	tok := strings.TrimSpace(out)
	if err != nil || tok == "" || strings.Contains(tok, " ") {
		return "", fmt.Errorf("no Cloudflare Access token for %s; run `hangar login` first", t.app)
	}
	t.token, t.expires = tok, expiry(tok, t.now())
	return tok, nil
}

// expiry reads a JWT's exp claim without verifying it: the server verifies,
// and the client only needs to know when to fetch a new one.
func expiry(tok string, now time.Time) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.Exp > 0 {
				return time.Unix(claims.Exp, 0)
			}
		}
	}
	return now
}

func runCloudflared(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "cloudflared", args...)
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}
