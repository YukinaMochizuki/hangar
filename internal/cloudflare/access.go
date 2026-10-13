package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// AccessApp is an Access application as the API returns it.
type AccessApp struct {
	ID                     string            `json:"id"`
	AUD                    string            `json:"aud"`
	Name                   string            `json:"name"`
	Domain                 string            `json:"domain"`
	Type                   string            `json:"type"`
	SessionDuration        string            `json:"session_duration"`
	AllowedIdPs            []string          `json:"allowed_idps"`
	AutoRedirectToIdentity bool              `json:"auto_redirect_to_identity"`
	Destinations           []AppDestination  `json:"destinations,omitempty"`
	Policies               []AppPolicyResult `json:"policies"`
}

// AppDestination is a hostname (and path) an application secures.
type AppDestination struct {
	Type string `json:"type"`
	URI  string `json:"uri,omitempty"`
}

// AppPolicyResult is a policy attached to an application, with the
// precedence it has there.
type AppPolicyResult struct {
	AccessPolicy
	Precedence int `json:"precedence"`
}

// AccessAppInput is the body that creates or replaces a self-hosted
// application.
type AccessAppInput struct {
	Name                   string           `json:"name"`
	Domain                 string           `json:"domain"`
	Type                   string           `json:"type"`
	SessionDuration        string           `json:"session_duration"`
	AllowedIdPs            []string         `json:"allowed_idps"`
	AutoRedirectToIdentity bool             `json:"auto_redirect_to_identity"`
	Destinations           []AppDestination `json:"destinations,omitempty"`
	Policies               []PolicyLink     `json:"policies"`
}

// PolicyLink attaches a reusable policy to an application.
type PolicyLink struct {
	ID         string `json:"id"`
	Precedence int    `json:"precedence"`
}

// AccessPolicy is a reusable Access policy. Rules stay raw JSON objects, so
// rule kinds this package does not model still compare and round-trip.
type AccessPolicy struct {
	ID              string            `json:"id,omitempty"`
	Name            string            `json:"name"`
	Decision        string            `json:"decision"`
	Include         []json.RawMessage `json:"include"`
	Exclude         []json.RawMessage `json:"exclude"`
	Require         []json.RawMessage `json:"require"`
	SessionDuration string            `json:"session_duration,omitempty"`
	Reusable        bool              `json:"reusable,omitempty"`
}

// EmailRule matches one email address.
func EmailRule(email string) json.RawMessage {
	b, _ := json.Marshal(map[string]map[string]string{"email": {"email": email}})
	return b
}

// AnyValidServiceTokenRule matches every service token of the account.
func AnyValidServiceTokenRule() json.RawMessage {
	return json.RawMessage(`{"any_valid_service_token":{}}`)
}

// IdentityProvider is a login method configured for the account.
type IdentityProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ServiceToken is a service token as listed; its secret is never returned
// after creation.
type ServiceToken struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	ClientID  string     `json:"client_id"`
	Duration  string     `json:"duration"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// CreatedServiceToken is a service token with the secret the API returns
// only once, when it is created.
type CreatedServiceToken struct {
	ServiceToken
	ClientSecret Secret `json:"client_secret"`
}

// Organization is the account's Zero Trust organization.
type Organization struct {
	Name       string `json:"name"`
	AuthDomain string `json:"auth_domain"`
}

func accessPath(account, rest string) string { return "/accounts/" + esc(account) + "/access/" + rest }

// ListAccessApps returns the account's applications whose domain is exactly
// domain.
func (c *Client) ListAccessApps(ctx context.Context, account, domain string) ([]AccessApp, error) {
	q := url.Values{"domain": {domain}, "exact": {"true"}}
	return listAll[AccessApp](ctx, c, accessPath(account, "apps"), q, 100)
}

// GetAccessApp returns one application.
func (c *Client) GetAccessApp(ctx context.Context, account, id string) (AccessApp, error) {
	var a AccessApp
	_, err := c.do(ctx, http.MethodGet, accessPath(account, "apps/"+esc(id)), nil, nil, &a)
	return a, err
}

// CreateAccessApp creates an application.
func (c *Client) CreateAccessApp(ctx context.Context, account string, in AccessAppInput) (AccessApp, error) {
	var a AccessApp
	_, err := c.do(ctx, http.MethodPost, accessPath(account, "apps"), nil, in, &a)
	return a, err
}

// UpdateAccessApp replaces an application.
func (c *Client) UpdateAccessApp(ctx context.Context, account, id string, in AccessAppInput) (AccessApp, error) {
	var a AccessApp
	_, err := c.do(ctx, http.MethodPut, accessPath(account, "apps/"+esc(id)), nil, in, &a)
	return a, err
}

// ListAccessPolicies returns every reusable policy of the account.
func (c *Client) ListAccessPolicies(ctx context.Context, account string) ([]AccessPolicy, error) {
	return listAll[AccessPolicy](ctx, c, accessPath(account, "policies"), nil, 100)
}

// GetAccessPolicy returns one reusable policy.
func (c *Client) GetAccessPolicy(ctx context.Context, account, id string) (AccessPolicy, error) {
	var p AccessPolicy
	_, err := c.do(ctx, http.MethodGet, accessPath(account, "policies/"+esc(id)), nil, nil, &p)
	return p, err
}

// body is p as a request: no read-only fields, and empty rule lists sent as
// [] rather than null.
func (p AccessPolicy) body() AccessPolicy {
	p.ID, p.Reusable = "", false
	for _, l := range []*[]json.RawMessage{&p.Include, &p.Exclude, &p.Require} {
		if *l == nil {
			*l = []json.RawMessage{}
		}
	}
	return p
}

// CreateAccessPolicy creates a reusable policy.
func (c *Client) CreateAccessPolicy(ctx context.Context, account string, p AccessPolicy) (AccessPolicy, error) {
	var out AccessPolicy
	_, err := c.do(ctx, http.MethodPost, accessPath(account, "policies"), nil, p.body(), &out)
	return out, err
}

// UpdateAccessPolicy replaces a reusable policy.
func (c *Client) UpdateAccessPolicy(ctx context.Context, account, id string, p AccessPolicy) (AccessPolicy, error) {
	var out AccessPolicy
	_, err := c.do(ctx, http.MethodPut, accessPath(account, "policies/"+esc(id)), nil, p.body(), &out)
	return out, err
}

// ListIdentityProviders returns the account's login methods.
func (c *Client) ListIdentityProviders(ctx context.Context, account string) ([]IdentityProvider, error) {
	return listAll[IdentityProvider](ctx, c, accessPath(account, "identity_providers"), nil, 200)
}

// ListServiceTokens returns the account's service tokens named name.
func (c *Client) ListServiceTokens(ctx context.Context, account, name string) ([]ServiceToken, error) {
	q := url.Values{"name": {name}}
	return listAll[ServiceToken](ctx, c, accessPath(account, "service_tokens"), q, 100)
}

// GetServiceToken returns one service token, without its secret.
func (c *Client) GetServiceToken(ctx context.Context, account, id string) (ServiceToken, error) {
	var t ServiceToken
	_, err := c.do(ctx, http.MethodGet, accessPath(account, "service_tokens/"+esc(id)), nil, nil, &t)
	return t, err
}

// CreateServiceToken creates a service token valid for duration (such as
// 8760h, or forever).
func (c *Client) CreateServiceToken(ctx context.Context, account, name, duration string) (CreatedServiceToken, error) {
	var t CreatedServiceToken
	body := map[string]string{"name": name, "duration": duration}
	_, err := c.do(ctx, http.MethodPost, accessPath(account, "service_tokens"), nil, body, &t)
	return t, err
}

// Organization returns the account's Zero Trust organization.
func (c *Client) Organization(ctx context.Context, account string) (Organization, error) {
	var o Organization
	_, err := c.do(ctx, http.MethodGet, accessPath(account, "organizations"), nil, nil, &o)
	return o, err
}
