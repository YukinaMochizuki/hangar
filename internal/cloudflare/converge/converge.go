// Package converge makes Cloudflare's DNS match Hangar's front door: DNS-only
// (grey-cloud) A and AAAA records at the host for the API, the console and
// the public endpoints' wildcard (docs/decisions/front-door.md). It changes
// only records it created, which it records in a state file, and it
// deletes nothing.
package converge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
)

// Marker is every record's comment, telling people in the dashboard who
// manages it.
const Marker = "managed by hangar-cloudflare"

// ErrNotOwned is returned when a record the tool would manage exists but was
// not created by it.
var ErrNotOwned = errors.New("refusing to change records hangar-cloudflare did not create")

// Change is one difference between Cloudflare (or the state file) and the
// config.
type Change struct {
	// Action is create, update or write.
	Action string
	// Object names what changes, such as `DNS A record api.example.com`.
	Object string
	// Fields says what differs, for updates.
	Fields []Field
}

// Field is one differing attribute.
type Field struct {
	Name, Have, Want string
}

// Result is what a run found and, when applying, did.
type Result struct {
	Changes []Change
	// Notes are facts worth a human's attention that are not drift.
	Notes []string
}

// Engine converges one config.
type Engine struct {
	Client *cloudflare.Client
	Config Config
	// Dir holds the state file.
	Dir string
	// Apply makes changes; without it a run only reports them and writes
	// nothing, locally or remotely.
	Apply bool
	// Log receives progress lines.
	Log io.Writer
}

// want is one record the config asks for.
type want struct {
	zone   *cloudflare.Zone
	record cloudflare.DNSRecord
	key    string
	// have is the record the state names, when it still exists.
	have *cloudflare.DNSRecord
}

type run struct {
	*Engine
	ctx      context.Context
	st       State
	upgraded bool
	res      *Result
	zone     cloudflare.Zone
	epZone   cloudflare.Zone
	wants    []*want
}

// Run compares Cloudflare with the config and, with Apply, converges it.
// Ownership conflicts are all found before anything is changed. For each
// name, the A record comes before the AAAA: a CNAME the state names becomes
// the A record in place, and a name holding a CNAME takes no other record.
func (e *Engine) Run(ctx context.Context) (*Result, error) {
	if e.Log == nil {
		e.Log = io.Discard
	}
	if err := checkDir(e.Dir); err != nil {
		return nil, err
	}
	st, upgraded, err := loadState(e.Dir, e.Config)
	if err != nil {
		return nil, err
	}
	if st.AccountID != "" && st.AccountID != e.Config.AccountID {
		return nil, fmt.Errorf("%s belongs to account %s, the config names %s", StateFile, st.AccountID, e.Config.AccountID)
	}
	if st.Zone != "" && st.Zone != e.Config.Zone {
		return nil, fmt.Errorf("%s was written for zone %s, the config names %s; changing it is not supported: "+
			"remove the old records by hand (docs/runbook/cloudflare.md), or run with a new --out directory", StateFile, st.Zone, e.Config.Zone)
	}
	var epZone string
	if e.Config.Endpoints != nil {
		epZone = e.Config.Endpoints.Zone
	}
	if st.EndpointsZone != "" && st.EndpointsZone != epZone {
		return nil, fmt.Errorf("%s was written for public endpoints in %s, the config names %q; changing or dropping it is not supported: "+
			"remove the old records by hand (docs/runbook/cloudflare.md)", StateFile, st.EndpointsZone, epZone)
	}
	st.AccountID, st.Zone, st.EndpointsZone = e.Config.AccountID, e.Config.Zone, epZone
	r := &run{Engine: e, ctx: ctx, st: st, upgraded: upgraded, res: &Result{}}
	if err := r.lookups(); err != nil {
		return nil, err
	}
	r.plan()
	if err := r.observe(); err != nil {
		return nil, err
	}
	r.noteRetired()
	if upgraded {
		r.add(Change{Action: "write", Object: StateFile, Fields: []Field{{Name: "version", Have: "1", Want: strconv.Itoa(stateFileVersion)}}})
		if err := r.save(); err != nil {
			return r.res, err
		}
	}
	for _, w := range r.wants {
		if err := r.converge(w); err != nil {
			return r.res, err
		}
	}
	return r.res, nil
}

func (r *run) logf(format string, args ...any) { _, _ = fmt.Fprintf(r.Log, format+"\n", args...) }

func (r *run) save() error {
	if !r.Apply {
		return nil
	}
	return saveState(r.Dir, r.st)
}

func (r *run) add(c Change) { r.res.Changes = append(r.res.Changes, c) }

// lookups finds the zones.
func (r *run) lookups() error {
	z, err := r.Client.ZoneByName(r.ctx, r.Config.AccountID, r.Config.Zone)
	if err != nil {
		return err
	}
	if r.st.ZoneID != "" && r.st.ZoneID != z.ID {
		return fmt.Errorf("%s records zone %s, but %s is %s; moving records between zones is not supported", StateFile, r.st.ZoneID, r.Config.Zone, z.ID)
	}
	r.zone, r.st.ZoneID = z, z.ID
	if ep := r.Config.Endpoints; ep != nil {
		z, err := r.Client.ZoneByName(r.ctx, r.Config.AccountID, ep.Zone)
		if err != nil {
			return fmt.Errorf("endpoints.zone: %w", err)
		}
		if r.st.EndpointsZoneID != "" && r.st.EndpointsZoneID != z.ID {
			return fmt.Errorf("%s records endpoints zone %s, but %s is %s; moving records between zones is not supported", StateFile, r.st.EndpointsZoneID, ep.Zone, z.ID)
		}
		r.epZone, r.st.EndpointsZoneID = z, z.ID
	}
	return nil
}

// plan lists the records the config asks for, in the order they are
// converged.
func (r *run) plan() {
	add := func(z *cloudflare.Zone, name string) {
		for _, a := range []struct{ typ, content string }{{"A", r.Config.Addresses.IPv4}, {"AAAA", r.Config.Addresses.IPv6}} {
			r.wants = append(r.wants, &want{
				zone: z, key: recordKey(name, a.typ),
				record: cloudflare.DNSRecord{Type: a.typ, Name: name, Content: a.content, Proxied: false, TTL: 1, Comment: Marker},
			})
		}
	}
	add(&r.zone, r.Config.APIHostname())
	add(&r.zone, r.Config.ConsoleHostname())
	if ep := r.Config.Endpoints; ep != nil {
		add(&r.epZone, ep.Wildcard())
	}
}

// addressTypes are the record types that answer for a name's address: a
// record of one of them that is not the tool's collides with its own.
var addressTypes = []string{"A", "AAAA", "CNAME"}

// observe fetches the records the state names and looks for records that
// would collide with them but are not the tool's.
func (r *run) observe() error {
	owned := map[string]bool{}
	for _, id := range r.st.Records {
		owned[id] = true
	}
	var conflicts []string
	listed := map[string]bool{}
	for _, w := range r.wants {
		if id := r.st.Records[w.key]; id != "" {
			d, err := r.Client.GetDNSRecord(r.ctx, w.zone.ID, id)
			switch {
			case cloudflare.IsNotFound(err):
			case err != nil:
				return err
			default:
				w.have = &d
			}
		}
		if listed[w.record.Name] {
			continue
		}
		listed[w.record.Name] = true
		records, err := r.Client.ListDNSRecords(r.ctx, w.zone.ID, w.record.Name)
		if err != nil {
			return err
		}
		for _, d := range records {
			if strings.EqualFold(d.Name, w.record.Name) && slices.Contains(addressTypes, d.Type) && !owned[d.ID] {
				conflicts = append(conflicts, fmt.Sprintf("DNS %s record %s (id %s) exists but is not in %s", d.Type, d.Name, d.ID, StateFile))
			}
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w (it changes only what it recorded in %s; delete these in the dashboard, or restore %s):\n  %s",
			ErrNotOwned, StateFile, StateFile, strings.Join(conflicts, "\n  "))
	}
	return nil
}

// noteRetired names what an earlier version made for the tunnel and
// Access, for someone to delete by hand once the cutover is settled
// (docs/runbook/cloudflare.md).
func (r *run) noteRetired() {
	keys := make([]string, 0, len(r.st.Retired))
	for k := range r.st.Retired {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("%s %s is no longer used: delete it by hand once rolling back is off the table, then remove it from %s",
			strings.ReplaceAll(k, "_", " "), r.st.Retired[k], StateFile))
	}
}

func (r *run) converge(w *want) error {
	obj := fmt.Sprintf("DNS %s record %s", w.record.Type, w.record.Name)
	if w.have == nil {
		r.add(Change{Action: "create", Object: obj, Fields: []Field{{Name: "record", Have: "(none)", Want: formatRecord(w.record)}}})
		if !r.Apply {
			return nil
		}
		created, err := r.Client.CreateDNSRecord(r.ctx, w.zone.ID, w.record)
		if err != nil {
			return err
		}
		r.logf("created %s (id %s)", obj, created.ID)
		r.st.Records[w.key] = created.ID
		return r.save()
	}
	fields := diffRecord(*w.have, w.record)
	if len(fields) == 0 {
		return nil
	}
	r.add(Change{Action: "update", Object: obj, Fields: fields})
	if !r.Apply {
		return nil
	}
	rec := w.record
	rec.Tags = w.have.Tags
	if _, err := r.Client.UpdateDNSRecord(r.ctx, w.zone.ID, w.have.ID, rec); err != nil {
		if w.have.Type != rec.Type {
			return fmt.Errorf("%s: changing record %s from %s to %s: %w (docs/runbook/cloudflare.md, \"If the API refuses the type change\")",
				obj, w.have.ID, w.have.Type, rec.Type, err)
		}
		return err
	}
	r.logf("updated %s", obj)
	return nil
}

func diffRecord(have, want cloudflare.DNSRecord) []Field {
	var fields []Field
	if have.Type != want.Type {
		fields = append(fields, Field{"type", have.Type, want.Type})
	}
	if !strings.EqualFold(have.Name, want.Name) {
		fields = append(fields, Field{"name", have.Name, want.Name})
	}
	if !strings.EqualFold(have.Content, want.Content) {
		fields = append(fields, Field{"content", have.Content, want.Content})
	}
	if have.Proxied != want.Proxied {
		fields = append(fields, Field{"proxied", strconv.FormatBool(have.Proxied), strconv.FormatBool(want.Proxied)})
	}
	if have.TTL != want.TTL {
		fields = append(fields, Field{"ttl", formatTTL(have.TTL), formatTTL(want.TTL)})
	}
	if have.Comment != want.Comment {
		fields = append(fields, Field{"comment", strconv.Quote(have.Comment), strconv.Quote(want.Comment)})
	}
	return fields
}

func formatTTL(ttl int) string {
	if ttl == 1 {
		return "auto"
	}
	return strconv.Itoa(ttl)
}

func formatRecord(d cloudflare.DNSRecord) string {
	return fmt.Sprintf("%s %s -> %s proxied=%t ttl=%s", d.Type, d.Name, d.Content, d.Proxied, formatTTL(d.TTL))
}
