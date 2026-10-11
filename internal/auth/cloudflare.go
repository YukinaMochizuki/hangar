package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// CloudflareJWTHeader carries the token Cloudflare Access signs for every
// request it lets through.
const CloudflareJWTHeader = "Cf-Access-Jwt-Assertion"

// CloudflareConfig identifies the Access application whose tokens are
// accepted.
type CloudflareConfig struct {
	// TeamDomain is the token issuer, such as https://team.cloudflareaccess.com.
	TeamDomain string
	// Audience is the Access application's AUD tag.
	Audience string
	// CertsURL serves the signing keys; empty means the team's standard
	// endpoint.
	CertsURL string
}

// Cloudflare verifies Access tokens. It never trusts the plain identity
// headers Access also sets, because anything that can reach the listener
// could forge them; only the signature counts.
type Cloudflare struct {
	verifier *oidc.IDTokenVerifier
}

// NewCloudflare returns an authenticator for one Access application. Keys are
// fetched lazily and refreshed when an unknown key ID appears.
func NewCloudflare(ctx context.Context, cfg CloudflareConfig) (*Cloudflare, error) {
	if cfg.TeamDomain == "" || cfg.Audience == "" {
		return nil, errors.New("cloudflare auth needs a team domain and an audience")
	}
	issuer := strings.TrimSuffix(cfg.TeamDomain, "/")
	certs := cfg.CertsURL
	if certs == "" {
		certs = issuer + "/cdn-cgi/access/certs"
	}
	keys := oidc.NewRemoteKeySet(ctx, certs)
	return &Cloudflare{verifier: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: cfg.Audience})}, nil
}

// accessClaims are the identity claims in an Access token: users have an
// email, service tokens a common name (their client ID).
type accessClaims struct {
	Email      string `json:"email"`
	CommonName string `json:"common_name"`
}

func (c *Cloudflare) Authenticate(ctx context.Context, header http.Header) (Principal, bool, error) {
	raw := header.Get(CloudflareJWTHeader)
	if raw == "" {
		return Principal{}, false, nil
	}
	tok, err := c.verifier.Verify(ctx, raw)
	if err != nil {
		return Principal{}, false, fmt.Errorf("access token: %w", err)
	}
	var claims accessClaims
	if err := tok.Claims(&claims); err != nil {
		return Principal{}, false, fmt.Errorf("access token claims: %w", err)
	}
	switch {
	case claims.Email != "":
		return Principal{Type: User, ID: strings.ToLower(claims.Email)}, true, nil
	case claims.CommonName != "":
		return Principal{Type: ServiceToken, ID: claims.CommonName}, true, nil
	}
	return Principal{}, false, errors.New("access token has neither an email nor a service token")
}
