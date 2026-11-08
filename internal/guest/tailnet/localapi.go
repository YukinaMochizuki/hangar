package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// localAPI asks tailscaled's LocalAPI, over its socket, what it says of the
// node.
type localAPI struct {
	client *http.Client
}

func newLocalAPI(socket string) *localAPI {
	return &localAPI{client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// status is the part of ipnstate.Status the guest reads.
type status struct {
	BackendState string
	Self         *struct {
		DNSName      string
		TailscaleIPs []string
	}
	MagicDNSSuffix string
	CurrentTailnet *struct {
		MagicDNSSuffix  string
		MagicDNSEnabled bool
	}
}

// Status is the node's state as tailscaled says it.
func (l *localAPI) Status(ctx context.Context) (backend, error) {
	// LocalAPI answers only this host name, which no DNS name rebinds to.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/status?peers=false", nil)
	if err != nil {
		return backend{}, err
	}
	res, err := l.client.Do(req)
	if err != nil {
		return backend{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return backend{}, err
	}
	if res.StatusCode != http.StatusOK {
		return backend{}, fmt.Errorf("tailscaled's LocalAPI: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return parseStatus(body)
}

func parseStatus(body []byte) (backend, error) {
	var st status
	if err := json.Unmarshal(body, &st); err != nil {
		return backend{}, fmt.Errorf("tailscaled's status: %w", err)
	}
	b := backend{State: st.BackendState, Suffix: st.MagicDNSSuffix}
	if t := st.CurrentTailnet; t != nil && t.MagicDNSSuffix != "" {
		b.Suffix = t.MagicDNSSuffix
		if !t.MagicDNSEnabled {
			b.Suffix = ""
		}
	}
	if s := st.Self; s != nil {
		b.DNSName, b.IPs = s.DNSName, s.TailscaleIPs
	}
	return b, nil
}
