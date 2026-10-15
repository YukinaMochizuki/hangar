package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// AccessApp is an Access application as the API returns it. Besides what
// defines it, it carries the settings that, set, let requests through with
// less than a full Access login, so callers can check them.
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
	SelfHostedDomains      []string          `json:"self_hosted_domains,omitempty"`
	Policies               []AppPolicyResult `json:"policies"`

	OptionsPreflightBypass   bool            `json:"options_preflight_bypass"`
	AllowAuthenticateViaWarp bool            `json:"allow_authenticate_via_warp"`
	AllowIframe              bool            `json:"allow_iframe"`
	CORSHeaders              json.RawMessage `json:"cors_headers,omitempty"`
	// HTTPOnlyCookieAttribute is nil when the API leaves it out; its default
	// is true.
	HTTPOnlyCookieAttribute *bool  `json:"http_only_cookie_attribute"`
	SameSiteCookieAttribute string `json:"same_site_cookie_attribute"`
	ClientlessIsolationURL  bool   `json:"use_clientless_isolation_app_launcher_url"`
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
// application. A replace resets what the body leaves out.
type AccessAppInput struct {
	Name                   string           `json:"name"`
	Domain                 string           `json:"domain"`
	Type                   string           `json:"type"`
	SessionDuration        string           `json:"session_duration"`
	AllowedIdPs            []string         `json:"allowed_idps"`
	AutoRedirectToIdentity bool             `json:"auto_redirect_to_identity"`
	Destinations           []AppDestination `json:"destinations,omitempty"`
	Policies               []PolicyLink     `json:"policies"`

	OptionsPreflightBypass   bool   `json:"options_preflight_bypass"`
	AllowAuthenticateViaWarp bool   `json:"allow_authenticate_via_warp"`
	AllowIframe              bool   `json:"allow_iframe"`
	HTTPOnlyCookieAttribute  bool   `json:"http_only_cookie_attribute"`
	SameSiteCookieAttribute  string `json:"same_site_cookie_attribute,omitempty"`
	// Left out when false: the API refuses the field, even false, on an
	// application with a public destination.
	ClientlessIsolationURL bool `json:"use_clientless_isolation_app_launcher_url,omitempty"`
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

// EveryoneRule matches everyone, signed in or not; with a bypass policy, it
// lets every request through without Access.
func EveryoneRule() json.RawMessage {
	return json.RawMessage(`{"everyone":{}}`)
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

// EndpointAppPrefix starts the name of every Access application the
// apiserver makes for a PUBLIC endpoint, followed by its hostname:
// hangar-cloudflare leaves them alone, and the apiserver finds its own by
// name.
const EndpointAppPrefix = "hangar public endpoint: "

func accessPath(account, rest string) string { return "/accounts/" + esc(account) + "/access/" + rest }

// ListAccessApps returns every application of the account. Filtering is the
// caller's: the API's domain filter sees only the primary domain, not
// destinations, so it can miss an application covering a hostname.
func (c *Client) ListAccessApps(ctx context.Context, account string) ([]AccessApp, error) {
	return listAll[AccessApp](ctx, c, accessPath(account, "apps"), nil, 100)
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

// FindAccessApps returns the account's applications named exactly name.
func (c *Client) FindAccessApps(ctx context.Context, account, name string) ([]AccessApp, error) {
	q := url.Values{"name": {name}, "exact": {"true"}}
	apps, err := listAll[AccessApp](ctx, c, accessPath(account, "apps"), q, 100)
	if err != nil {
		return nil, err
	}
	// The filter is the API's; holding to it here costs nothing.
	out := apps[:0]
	for _, a := range apps {
		if a.Name == name {
			out = append(out, a)
		}
	}
	return out, nil
}

// DeleteAccessApp deletes an application.
func (c *Client) DeleteAccessApp(ctx context.Context, account, id string) error {
	_, err := c.do(ctx, http.MethodDelete, accessPath(account, "apps/"+esc(id)), nil, nil, nil)
	return err
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
