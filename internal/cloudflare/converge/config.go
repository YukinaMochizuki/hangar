package converge

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is what to converge. It holds no credentials: the API token comes
// from the environment.
type Config struct {
	AccountID string `yaml:"account_id"`
	// Zone is the control plane's zone; the API and the console are api.
	// and console. under it.
	Zone string `yaml:"zone"`
	// Addresses are the host's, which every record points at.
	Addresses Addresses `yaml:"addresses"`
	// Endpoints, if set, sets up the user-content domain public endpoints
	// are served under (docs/decisions/public-endpoints.md).
	Endpoints *EndpointsConfig `yaml:"endpoints"`
}

// Addresses are the host's public addresses.
type Addresses struct {
	IPv4 string `yaml:"ipv4"`
	IPv6 string `yaml:"ipv6"`
}

// EndpointsConfig is the public endpoints' domain.
type EndpointsConfig struct {
	// Zone is the user-content zone, apart from the control zone; every
	// endpoint is a hostname one label below it.
	Zone string `yaml:"zone"`
}

// Wildcard is the hostname that covers every endpoint.
func (e *EndpointsConfig) Wildcard() string { return "*." + e.Zone }

// APIHostname is the public API's hostname.
func (c Config) APIHostname() string { return "api." + c.Zone }

// ConsoleHostname is the console's hostname.
func (c Config) ConsoleHostname() string { return "console." + c.Zone }

var accountIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// LoadConfig reads and validates a config file. Unknown keys are errors, so
// a typo cannot silently drop a setting, and a config written for the
// tunnel and Access is refused rather than half read.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w (the tunnel and Access keys are gone: docs/runbook/cloudflare.md)", path, err)
	}
	if err := c.normalize(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func normZone(s string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".") }

func (c *Config) normalize() error {
	c.Zone = normZone(c.Zone)
	var errs []error
	if !accountIDRe.MatchString(c.AccountID) {
		errs = append(errs, errors.New("account_id must be 32 lowercase hex characters"))
	}
	if c.Zone == "" || strings.Contains(c.Zone, "*") {
		errs = append(errs, errors.New("zone must name a zone"))
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(c.Addresses.IPv4)); err != nil || !a.Is4() || !a.IsGlobalUnicast() || a.IsPrivate() {
		errs = append(errs, fmt.Errorf("addresses.ipv4 %q must be a public IPv4 address", c.Addresses.IPv4))
	} else {
		c.Addresses.IPv4 = a.String()
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(c.Addresses.IPv6)); err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" || !a.IsGlobalUnicast() || a.IsPrivate() {
		errs = append(errs, fmt.Errorf("addresses.ipv6 %q must be a public IPv6 address", c.Addresses.IPv6))
	} else {
		c.Addresses.IPv6 = a.String()
	}
	if e := c.Endpoints; e != nil {
		e.Zone = normZone(e.Zone)
		switch {
		case e.Zone == "" || strings.Contains(e.Zone, "*"):
			errs = append(errs, errors.New("endpoints.zone must name a zone"))
		case e.Zone == c.Zone || strings.HasSuffix(e.Zone, "."+c.Zone) || strings.HasSuffix(c.Zone, "."+e.Zone):
			errs = append(errs, fmt.Errorf("endpoints.zone %q must be a domain apart from the control zone %q: user content never shares one with the API", e.Zone, c.Zone))
		}
	}
	return errors.Join(errs...)
}
