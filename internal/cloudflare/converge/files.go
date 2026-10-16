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
	stateFileVersion = 2
)

// State records the IDs of the records the tool created. It is the proof of
// ownership: a record whose ID is not here is never changed.
type State struct {
	Version   int    `json:"version"`
	AccountID string `json:"account_id"`
	// Zone is the control zone the records serve; changing it is refused.
	Zone   string `json:"zone"`
	ZoneID string `json:"zone_id,omitempty"`
	// EndpointsZone is the public endpoints' zone; changing it is refused,
	// as the zone is.
	EndpointsZone   string `json:"endpoints_zone,omitempty"`
	EndpointsZoneID string `json:"endpoints_zone_id,omitempty"`
	// Records maps a record's key, its name and type ("api.example.com A"),
	// to its ID.
	Records map[string]string `json:"records"`
	// Retired holds the IDs of what an earlier version made for the
	// Cloudflare tunnel and Access, which nothing uses any more, so a run
	// can name them, to be deleted by hand.
	Retired map[string]string `json:"retired,omitempty"`
}

// stateV1 is the state the tool kept while it set up the tunnel and Access.
type stateV1 struct {
	Version              int               `json:"version"`
	AccountID            string            `json:"account_id"`
	TunnelID             string            `json:"tunnel_id"`
	DNSRecordID          string            `json:"dns_record_id"`
	ZoneID               string            `json:"zone_id"`
	Hostname             string            `json:"hostname"`
	AppID                string            `json:"app_id"`
	Policies             map[string]string `json:"policies"`
	ServiceTokenID       string            `json:"service_token_id"`
	EndpointsZone        string            `json:"endpoints_zone"`
	EndpointsZoneID      string            `json:"endpoints_zone_id"`
	EndpointsDNSRecordID string            `json:"endpoints_dns_record_id"`
	EndpointsAppID       string            `json:"endpoints_app_id"`
}

// fromV1 reads a v1 state for cfg. Its API record, a CNAME to the tunnel,
// becomes the API's A record, and the endpoints' wildcard CNAME their A
// record, each changed in place, so neither name is ever without an
// answer. Its tunnel and Access objects are kept as retired.
func fromV1(v stateV1, cfg Config) (State, error) {
	if v.Hostname != "" && v.Hostname != cfg.APIHostname() {
		return State{}, fmt.Errorf("%s was written for hostname %s; this version serves %s and cannot take over another name", StateFile, v.Hostname, cfg.APIHostname())
	}
	s := State{
		Version: stateFileVersion, AccountID: v.AccountID, Zone: cfg.Zone, ZoneID: v.ZoneID,
		EndpointsZone: v.EndpointsZone, EndpointsZoneID: v.EndpointsZoneID,
		Records: map[string]string{}, Retired: map[string]string{},
	}
	if v.DNSRecordID != "" {
		s.Records[recordKey(cfg.APIHostname(), "A")] = v.DNSRecordID
	}
	if v.EndpointsDNSRecordID != "" && v.EndpointsZone != "" {
		s.Records[recordKey("*."+v.EndpointsZone, "A")] = v.EndpointsDNSRecordID
	}
	for k, id := range map[string]string{
		"tunnel": v.TunnelID, "access_app": v.AppID, "service_token": v.ServiceTokenID, "endpoints_access_app": v.EndpointsAppID,
	} {
		if id != "" {
			s.Retired[k] = id
		}
	}
	for role, id := range v.Policies {
		if id != "" {
			s.Retired["access_policy_"+role] = id
		}
	}
	return s, nil
}

func recordKey(name, typ string) string { return name + " " + typ }

// checkDir refuses an output directory that is not a directory or that
// group or others can enter: an earlier version kept the tunnel's token
// there, and the previous release, which a rollback runs, still writes it
// there. A missing one is fine; it is
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
		return fmt.Errorf("output directory %s has mode %04o; it may hold the tunnel's token, so only its owner may have access: chmod 700 %s", dir, perm, dir)
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

// loadState reads the state, a v1 one included, for cfg. upgraded says the
// file is v1, so an apply writes it again as v2.
func loadState(dir string, cfg Config) (s State, upgraded bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return State{Version: stateFileVersion, Records: map[string]string{}}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return State{}, false, fmt.Errorf("%s: %w", StateFile, err)
	}
	switch head.Version {
	case 1:
		var v stateV1
		if err := json.Unmarshal(b, &v); err != nil {
			return State{}, false, fmt.Errorf("%s: %w", StateFile, err)
		}
		s, err := fromV1(v, cfg)
		return s, true, err
	case stateFileVersion:
		if err := json.Unmarshal(b, &s); err != nil {
			return State{}, false, fmt.Errorf("%s: %w", StateFile, err)
		}
		if s.Records == nil {
			s.Records = map[string]string{}
		}
		return s, false, nil
	}
	return State{}, false, fmt.Errorf("%s: version %d, want 1 or %d", StateFile, head.Version, stateFileVersion)
}

// pending is a 0600 temporary file in the output directory, renamed into
// place by commit.
type pending struct {
	f    *os.File
	dest string
}

func newPending(dir, name string) (*pending, error) {
	if err := mkdir(dir); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "."+name+".*")
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
