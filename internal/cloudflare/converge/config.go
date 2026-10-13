package converge

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is what to converge. It holds no credentials: the API token comes
// from the environment.
type Config struct {
	AccountID         string    `yaml:"account_id"`
	Zone              string    `yaml:"zone"`
	Hostname          string    `yaml:"hostname"`
	TunnelName        string    `yaml:"tunnel_name"`
	AllowedEmails     []string  `yaml:"allowed_emails"`
	IdentityProviders IdPConfig `yaml:"identity_providers"`
}

// IdPConfig names the login methods the application offers. Both must
// already exist in the account.
type IdPConfig struct {
	// Google is the name of the Google login method.
	Google string `yaml:"google"`
	// OneTimePIN is the name of the One-time PIN login method; empty matches
	// the account's only one.
	OneTimePIN string `yaml:"one_time_pin"`
}

var accountIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// LoadConfig reads and validates a config file. Unknown keys are errors, so
// a typo cannot silently drop a setting.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.normalize(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) normalize() error {
	c.Zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(c.Zone)), ".")
	c.Hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(c.Hostname)), ".")
	var errs []error
	if !accountIDRe.MatchString(c.AccountID) {
		errs = append(errs, errors.New("account_id must be 32 lowercase hex characters"))
	}
	if c.Zone == "" {
		errs = append(errs, errors.New("zone is required"))
	}
	if !strings.HasSuffix(c.Hostname, "."+c.Zone) || strings.Contains(c.Hostname, "*") {
		errs = append(errs, fmt.Errorf("hostname %q must be a name inside zone %q", c.Hostname, c.Zone))
	}
	if strings.TrimSpace(c.TunnelName) == "" {
		errs = append(errs, errors.New("tunnel_name is required"))
	}
	if len(c.AllowedEmails) == 0 {
		errs = append(errs, errors.New("allowed_emails needs at least one address"))
	}
	seen := map[string]bool{}
	var emails []string
	for _, e := range c.AllowedEmails {
		e = strings.ToLower(strings.TrimSpace(e))
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e {
			errs = append(errs, fmt.Errorf("allowed_emails: %q is not a plain email address", e))
			continue
		}
		if !seen[e] {
			seen[e] = true
			emails = append(emails, e)
		}
	}
	c.AllowedEmails = emails
	if strings.TrimSpace(c.IdentityProviders.Google) == "" {
		errs = append(errs, errors.New("identity_providers.google is required"))
	}
	return errors.Join(errs...)
}
