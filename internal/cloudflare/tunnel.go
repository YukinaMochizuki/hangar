package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Tunnel is a Cloudflare Tunnel (cfd_tunnel).
type Tunnel struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	ConfigSrc string     `json:"config_src"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at"`
}

// IngressRule maps a hostname (empty for the catch-all) to a service.
type IngressRule struct {
	Hostname      string          `json:"hostname,omitempty"`
	Path          string          `json:"path,omitempty"`
	Service       string          `json:"service"`
	OriginRequest json.RawMessage `json:"originRequest,omitempty"`
}

// TunnelConfig is a remotely-managed tunnel's configuration. OriginRequest
// and WarpRouting are kept raw: callers only need to know whether they are
// set.
type TunnelConfig struct {
	Ingress       []IngressRule   `json:"ingress"`
	OriginRequest json.RawMessage `json:"originRequest,omitempty"`
	WarpRouting   json.RawMessage `json:"warp-routing,omitempty"`
}

// TunnelConfiguration is the versioned configuration record of a tunnel.
type TunnelConfiguration struct {
	TunnelID string       `json:"tunnel_id"`
	Version  int          `json:"version"`
	Source   string       `json:"source"`
	Config   TunnelConfig `json:"config"`
}

// ListTunnels returns the account's tunnels named name that are not deleted.
func (c *Client) ListTunnels(ctx context.Context, account, name string) ([]Tunnel, error) {
	q := url.Values{"name": {name}, "is_deleted": {"false"}}
	return listAll[Tunnel](ctx, c, "/accounts/"+esc(account)+"/cfd_tunnel", q, 100)
}

// GetTunnel returns one tunnel, including a deleted one (DeletedAt set).
func (c *Client) GetTunnel(ctx context.Context, account, id string) (Tunnel, error) {
	var t Tunnel
	_, err := c.do(ctx, http.MethodGet, "/accounts/"+esc(account)+"/cfd_tunnel/"+esc(id), nil, nil, &t)
	return t, err
}

// CreateTunnel creates a remotely-managed tunnel (config_src cloudflare), so
// its ingress lives in Cloudflare and the connector needs only its token.
func (c *Client) CreateTunnel(ctx context.Context, account, name string) (Tunnel, error) {
	var t Tunnel
	body := map[string]string{"name": name, "config_src": "cloudflare"}
	_, err := c.do(ctx, http.MethodPost, "/accounts/"+esc(account)+"/cfd_tunnel", nil, body, &t)
	return t, err
}

// GetTunnelConfiguration returns a remotely-managed tunnel's configuration.
func (c *Client) GetTunnelConfiguration(ctx context.Context, account, id string) (TunnelConfiguration, error) {
	var tc TunnelConfiguration
	_, err := c.do(ctx, http.MethodGet, "/accounts/"+esc(account)+"/cfd_tunnel/"+esc(id)+"/configurations", nil, nil, &tc)
	return tc, err
}

// PutTunnelConfiguration replaces a remotely-managed tunnel's configuration.
func (c *Client) PutTunnelConfiguration(ctx context.Context, account, id string, cfg TunnelConfig) (TunnelConfiguration, error) {
	var tc TunnelConfiguration
	body := map[string]TunnelConfig{"config": cfg}
	_, err := c.do(ctx, http.MethodPut, "/accounts/"+esc(account)+"/cfd_tunnel/"+esc(id)+"/configurations", nil, body, &tc)
	return tc, err
}

// TunnelToken returns the token a connector runs the tunnel with.
func (c *Client) TunnelToken(ctx context.Context, account, id string) (Secret, error) {
	var s string
	_, err := c.do(ctx, http.MethodGet, "/accounts/"+esc(account)+"/cfd_tunnel/"+esc(id)+"/token", nil, nil, &s)
	return Secret(s), err
}
