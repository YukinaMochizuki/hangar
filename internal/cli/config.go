package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"sigs.k8s.io/yaml"
)

// Config is the CLI's configuration file: named contexts, one of them
// current.
type Config struct {
	CurrentContext string              `json:"currentContext,omitempty"`
	Contexts       map[string]*Context `json:"contexts,omitempty"`
}

// Context is one API endpoint and how to reach it.
type Context struct {
	Endpoint string `json:"endpoint"`
	// Project is the default for project-scoped kinds.
	Project string `json:"project,omitempty"`
	// Auth is oidc (the default: a login from hangar login), token (an
	// agent's service token, read from TokenFile), dev, or local: root on
	// the host, through a unix:// endpoint, the only auth such an endpoint
	// takes.
	Auth string `json:"auth,omitempty"`
	// User is the email dev auth sends.
	User string `json:"user,omitempty"`
	// TokenFile is the file token auth reads the service token from, on
	// every run. The token itself is never in this file.
	TokenFile string `json:"tokenFile,omitempty"`
}

// unixScheme starts an endpoint that is a unix socket on this host, such as
// unix:///run/hangar/admin.sock.
const unixScheme = "unix://"

// socketPath returns the socket a unix:// endpoint names.
func (c *Context) socketPath() (string, bool) {
	return strings.CutPrefix(c.Endpoint, unixScheme)
}

// local reports whether the context reaches the API as root on the host.
func (c *Context) local() bool {
	_, unix := c.socketPath()
	return unix || c.Auth == "local"
}

// check refuses a context whose endpoint and auth do not go together.
func (c *Context) check() error {
	path, unix := c.socketPath()
	switch {
	case unix && !filepath.IsAbs(path):
		return usageErr("endpoint %q: a unix socket needs an absolute path, such as unix:///run/hangar/admin.sock", c.Endpoint)
	case unix && c.Auth != "" && c.Auth != "local":
		return usageErr("endpoint %q is a unix socket, which takes local auth, not %s", c.Endpoint, c.Auth)
	case !unix && c.Auth == "local":
		return usageErr("local auth needs a unix:// endpoint, not %q", c.Endpoint)
	case c.Auth == "token" && c.TokenFile == "":
		return usageErr("token auth needs --token-file, the file holding the service token")
	case c.Auth != "token" && c.TokenFile != "":
		return usageErr("--token-file goes with --auth token")
	case c.Auth == "cloudflare":
		return usageErr("cloudflare auth is gone: Hangar signs you in itself; run hangar config set-context NAME --endpoint %s --auth oidc, then hangar login", c.Endpoint)
	}
	return nil
}

func configPath(env func(string) string) string {
	if p := env("HANGAR_CONFIG"); p != "" {
		return p
	}
	dir := env("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(env("HOME"), ".config")
	}
	return filepath.Join(dir, "hangar", "config.yaml")
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{Contexts: map[string]*Context{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.UnmarshalStrict(b, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*Context{}
	}
	return cfg, nil
}

func (c *Config) save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// prepareConfigDir makes the directory of the config file, which also holds
// credentials.yaml, private. The default one, …/hangar, is the CLI's own,
// so an existing one others may read is made 0700. One HANGAR_CONFIG names
// may be anything, such as a home directory or a checkout, so it is left as
// it is, with a warning if others may read it; credentials.yaml itself is
// 0600 either way.
func (a *app) prepareConfigDir() error {
	dir := filepath.Dir(a.configPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if a.env("HANGAR_CONFIG") == "" {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
			return os.Chmod(dir, 0o700)
		}
	}
	if fi.Mode().Perm()&0o044 != 0 && !a.warnedConfigDir {
		a.warnedConfigDir = true
		_, _ = fmt.Fprintf(a.io.Err, "warning: %s, which holds your logins, is mode %v: others can see which files are in it; credentials.yaml itself is readable by you only\n", dir, fi.Mode().Perm())
	}
	return nil
}

func (c *Config) names() []string {
	var out []string
	for name := range c.Contexts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
