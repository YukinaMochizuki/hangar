package converge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Files in the output directory.
const (
	StateFile        = "state.json"
	CloudflaredEnv   = "cloudflared.env"
	AccessEnv        = "access.env"
	SmokeTokenEnv    = "smoke-token.env"
	EdgeEnv          = "edge.env"
	stateFileVersion = 1
)

// State records the IDs of the objects the tool created. It is the proof of
// ownership: an object whose ID is not here is never changed.
type State struct {
	Version     int    `json:"version"`
	AccountID   string `json:"account_id"`
	TunnelID    string `json:"tunnel_id,omitempty"`
	DNSRecordID string `json:"dns_record_id,omitempty"`
	ZoneID      string `json:"zone_id,omitempty"`
	// Hostname is the API hostname the objects serve; changing it is refused.
	Hostname string `json:"hostname,omitempty"`
	AppID    string `json:"app_id,omitempty"`
	// Policies maps a policy's role (emailsPolicy, tokensPolicy) to its ID.
	Policies       map[string]string `json:"policies,omitempty"`
	ServiceTokenID string            `json:"service_token_id,omitempty"`
	// EndpointsZone is the public endpoints' zone the objects below serve;
	// changing it is refused, as the hostname is.
	EndpointsZone        string `json:"endpoints_zone,omitempty"`
	EndpointsZoneID      string `json:"endpoints_zone_id,omitempty"`
	EndpointsDNSRecordID string `json:"endpoints_dns_record_id,omitempty"`
	EndpointsAppID       string `json:"endpoints_app_id,omitempty"`
}

// checkDir refuses an output directory that is not a directory or that
// group or others can enter: it holds secrets. A missing one is fine; it is
// created 0700 when the first file is written.
func checkDir(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("output directory %s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("output directory %s has mode %04o; it holds secrets, so only its owner may have access: chmod 700 %s", dir, perm, dir)
	}
	return nil
}

func mkdir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return checkDir(dir)
	}
	// The umask can only take bits away, so this is 0700 or stricter.
	return os.MkdirAll(dir, 0o700)
}

func loadState(dir string) (State, error) {
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return State{Version: stateFileVersion, Policies: map[string]string{}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%s: %w", StateFile, err)
	}
	if s.Version != stateFileVersion {
		return State{}, fmt.Errorf("%s: version %d, want %d", StateFile, s.Version, stateFileVersion)
	}
	if s.Policies == nil {
		s.Policies = map[string]string{}
	}
	return s, nil
}

// readFile returns a file's content, or nil when it does not exist.
func readFile(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// pending is a 0600 temporary file in the output directory, renamed into
// place by commit. Creating it before the API call whose result it holds
// means a directory that cannot be written fails before anything exists.
type pending struct {
	f    *os.File
	dest string
}

// createTemp is os.CreateTemp; tests replace it to make writes fail.
var createTemp = os.CreateTemp

func newPending(dir, name string) (*pending, error) {
	if err := mkdir(dir); err != nil {
		return nil, err
	}
	f, err := createTemp(dir, "."+name+".*")
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return &pending{f: f, dest: filepath.Join(dir, name)}, nil
}

func (p *pending) commit(content []byte) error {
	if _, err := p.f.Write(content); err != nil {
		p.abort()
		return err
	}
	if err := p.f.Sync(); err != nil {
		p.abort()
		return err
	}
	if err := p.f.Close(); err != nil {
		_ = os.Remove(p.f.Name())
		return err
	}
	return os.Rename(p.f.Name(), p.dest)
}

func (p *pending) abort() {
	_ = p.f.Close()
	_ = os.Remove(p.f.Name())
}

// writeFile replaces a file atomically, mode 0600.
func writeFile(dir, name string, content []byte) error {
	p, err := newPending(dir, name)
	if err != nil {
		return err
	}
	return p.commit(content)
}

func saveState(dir string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(dir, StateFile, append(b, '\n'))
}
