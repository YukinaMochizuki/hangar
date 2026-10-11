package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
	// Auth is cloudflare (the default) or dev.
	Auth string `json:"auth,omitempty"`
	// User is the email dev auth sends.
	User string `json:"user,omitempty"`
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

func (c *Config) names() []string {
	var out []string
	for name := range c.Contexts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
