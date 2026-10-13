// Package converge makes Cloudflare match Hangar's public API setup: a
// remotely-managed tunnel and its ingress, the proxied DNS record that points
// at it, an Access application with two reusable policies, and a smoke-test
// service token. It changes only objects it created, which it records in a
// state file, and writes what the host needs to files, never to its output.
// It deletes nothing.
package converge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
)

// Fixed parts of the setup.
const (
	// Origin is where cloudflared sends the API's traffic: the apiserver's
	// public listener on the host.
	Origin          = "http://127.0.0.1:8080"
	catchAll        = "http_status:404"
	SessionDuration = "24h"
	SmokeTokenName  = "hangar-smoke"
	smokeTokenTTL   = "8760h"
	// Marker is the DNS record's comment, telling people in the dashboard
	// who manages it.
	Marker = "managed by hangar-cloudflare"

	emailsPolicy = "emails"
	tokensPolicy = "service_tokens"
)

// ErrNotOwned is returned when an object the tool would manage exists but was
// not created by it.
var ErrNotOwned = errors.New("refusing to change objects hangar-cloudflare did not create")

// Change is one difference between Cloudflare (or the output directory) and
// the config.
type Change struct {
	// Action is create, update or write.
	Action string
	// Object names what changes, such as `tunnel "hangar"`.
	Object string
	// Fields says what differs, for updates. Values never include secrets.
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
	// Dir holds the state file and the outputs.
	Dir string
	// Apply makes changes; without it a run only reports them and writes
	// nothing, locally or remotely.
	Apply bool
	// Log receives progress lines. They never include secrets.
	Log io.Writer
}

type run struct {
	*Engine
	ctx   context.Context
	st    State
	res   *Result
	zone  cloudflare.Zone
	team  string
	idps  []string
	owned owned
}

// owned holds the objects the state names that still exist.
type owned struct {
	tunnel   *cloudflare.Tunnel
	dns      *cloudflare.DNSRecord
	app      *cloudflare.AccessApp
	policies map[string]*cloudflare.AccessPolicy
	token    *cloudflare.ServiceToken
}

// Run compares Cloudflare with the config and, with Apply, converges it.
// Ownership conflicts are all found before anything is changed.
func (e *Engine) Run(ctx context.Context) (*Result, error) {
	if e.Log == nil {
		e.Log = io.Discard
	}
	st, err := loadState(e.Dir)
	if err != nil {
		return nil, err
	}
	if st.AccountID != "" && st.AccountID != e.Config.AccountID {
		return nil, fmt.Errorf("%s belongs to account %s, the config names %s", StateFile, st.AccountID, e.Config.AccountID)
	}
	st.AccountID = e.Config.AccountID
	r := &run{Engine: e, ctx: ctx, st: st, res: &Result{}}
	if err := r.lookups(); err != nil {
		return nil, err
	}
	if err := r.observe(); err != nil {
		return nil, err
	}
	steps := []func() error{r.tunnel, r.tunnelConfig, r.dnsRecord, r.policies, r.app, r.serviceToken, r.outputs}
	for _, s := range steps {
		if err := s(); err != nil {
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

// lookups reads what the tool needs but never changes: the zone, the team
// domain and the login methods.
func (r *run) lookups() error {
	acct := r.Config.AccountID
	z, err := r.Client.ZoneByName(r.ctx, acct, r.Config.Zone)
	if err != nil {
		return err
	}
	r.zone = z
	if r.st.ZoneID != "" && r.st.ZoneID != z.ID {
		return fmt.Errorf("%s records zone %s, but %s is %s; moving the record between zones is not supported", StateFile, r.st.ZoneID, r.Config.Zone, z.ID)
	}
	org, err := r.Client.Organization(r.ctx, acct)
	if cloudflare.IsNotFound(err) || err == nil && org.AuthDomain == "" {
		return errors.New("the account has no Zero Trust organization; set up Zero Trust and pick a team name first (docs/runbook/cloudflare.md)")
	}
	if err != nil {
		return err
	}
	r.team = "https://" + strings.TrimSuffix(org.AuthDomain, "/")
	idps, err := r.Client.ListIdentityProviders(r.ctx, acct)
	if err != nil {
		return err
	}
	google, err := pickIdP(idps, "google", r.Config.IdentityProviders.Google)
	if err != nil {
		return err
	}
	otp, err := pickIdP(idps, "onetimepin", r.Config.IdentityProviders.OneTimePIN)
	if err != nil {
		return err
	}
	r.idps = []string{google, otp}
	return nil
}

func pickIdP(idps []cloudflare.IdentityProvider, typ, name string) (string, error) {
	var ids, names []string
	for _, p := range idps {
		if p.Type != typ {
			continue
		}
		names = append(names, strconv.Quote(p.Name))
		if name == "" || p.Name == name {
			ids = append(ids, p.ID)
		}
	}
	switch {
	case len(ids) == 1:
		return ids[0], nil
	case len(ids) > 1:
		return "", fmt.Errorf("more than one %s login method matches %q; name one in identity_providers", typ, name)
	case len(names) == 0:
		return "", fmt.Errorf("the account has no %s login method; add it under Zero Trust > Settings > Authentication (docs/runbook/cloudflare.md)", typ)
	}
	return "", fmt.Errorf("no %s login method is named %q; the account has %s", typ, name, strings.Join(names, ", "))
}

// observe fetches the objects the state names and looks for objects that
// would collide with them but are not the tool's.
func (r *run) observe() error {
	acct, cfg := r.Config.AccountID, r.Config
	var conflicts []string
	foreign := func(kind, name, id string) {
		conflicts = append(conflicts, fmt.Sprintf("%s %q (id %s) exists but is not in %s", kind, name, id, StateFile))
	}

	if id := r.st.TunnelID; id != "" {
		t, err := r.Client.GetTunnel(r.ctx, acct, id)
		switch {
		case cloudflare.IsNotFound(err):
		case err != nil:
			return err
		case t.DeletedAt == nil:
			r.owned.tunnel = &t
		}
	}
	tunnels, err := r.Client.ListTunnels(r.ctx, acct, cfg.TunnelName)
	if err != nil {
		return err
	}
	for _, t := range tunnels {
		if t.Name == cfg.TunnelName && t.DeletedAt == nil && (r.owned.tunnel == nil || t.ID != r.owned.tunnel.ID) {
			foreign("tunnel", t.Name, t.ID)
		}
	}

	if id := r.st.DNSRecordID; id != "" {
		d, err := r.Client.GetDNSRecord(r.ctx, r.zone.ID, id)
		switch {
		case cloudflare.IsNotFound(err):
		case err != nil:
			return err
		default:
			r.owned.dns = &d
		}
	}
	records, err := r.Client.ListDNSRecords(r.ctx, r.zone.ID, cfg.Hostname)
	if err != nil {
		return err
	}
	for _, d := range records {
		if strings.EqualFold(d.Name, cfg.Hostname) && (r.owned.dns == nil || d.ID != r.owned.dns.ID) {
			foreign("DNS "+d.Type+" record", d.Name, d.ID)
		}
	}

	r.owned.policies = map[string]*cloudflare.AccessPolicy{}
	for role, id := range r.st.Policies {
		p, err := r.Client.GetAccessPolicy(r.ctx, acct, id)
		switch {
		case cloudflare.IsNotFound(err):
		case err != nil:
			return err
		default:
			r.owned.policies[role] = &p
		}
	}
	policies, err := r.Client.ListAccessPolicies(r.ctx, acct)
	if err != nil {
		return err
	}
	for _, p := range policies {
		for _, role := range []string{emailsPolicy, tokensPolicy} {
			if p.Name == r.policyName(role) && !r.ownsPolicy(p.ID) {
				foreign("Access policy", p.Name, p.ID)
			}
		}
	}

	if id := r.st.AppID; id != "" {
		a, err := r.Client.GetAccessApp(r.ctx, acct, id)
		switch {
		case cloudflare.IsNotFound(err):
		case err != nil:
			return err
		default:
			r.owned.app = &a
		}
	}
	apps, err := r.Client.ListAccessApps(r.ctx, acct, cfg.Hostname)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if strings.EqualFold(a.Domain, cfg.Hostname) && (r.owned.app == nil || a.ID != r.owned.app.ID) {
			foreign("Access application", a.Domain, a.ID)
		}
	}

	if id := r.st.ServiceTokenID; id != "" {
		t, err := r.Client.GetServiceToken(r.ctx, acct, id)
		switch {
		case cloudflare.IsNotFound(err):
		case err != nil:
			return err
		default:
			r.owned.token = &t
		}
	}
	tokens, err := r.Client.ListServiceTokens(r.ctx, acct, SmokeTokenName)
	if err != nil {
		return err
	}
	for _, t := range tokens {
		if t.Name == SmokeTokenName && (r.owned.token == nil || t.ID != r.owned.token.ID) {
			foreign("service token", t.Name, t.ID)
		}
	}

	if len(conflicts) > 0 {
		return fmt.Errorf("%w (it changes only what it recorded in %s; rename or delete these, or pick other names in the config):\n  %s",
			ErrNotOwned, StateFile, strings.Join(conflicts, "\n  "))
	}
	return nil
}

func (r *run) ownsPolicy(id string) bool {
	for _, v := range r.st.Policies {
		if v == id {
			return true
		}
	}
	return false
}

func (r *run) policyName(role string) string {
	if role == emailsPolicy {
		return r.Config.Hostname + ": allow listed emails"
	}
	return r.Config.Hostname + ": service tokens"
}

// pendingID stands in, while planning, for the ID an object would get.
func pendingID(kind string) string { return "(new " + kind + ")" }

func (r *run) tunnel() error {
	name := r.Config.TunnelName
	obj := fmt.Sprintf("tunnel %q", name)
	t := r.owned.tunnel
	if t == nil {
		r.add(Change{Action: "create", Object: obj})
		if !r.Apply {
			return nil
		}
		created, err := r.Client.CreateTunnel(r.ctx, r.Config.AccountID, name)
		if err != nil {
			return err
		}
		r.logf("created %s (id %s)", obj, created.ID)
		r.owned.tunnel = &created
		r.st.TunnelID = created.ID
		return r.save()
	}
	if t.ConfigSrc != "cloudflare" {
		return fmt.Errorf("%s (id %s) is locally managed (config_src %q); its configuration cannot be set through the API", obj, t.ID, t.ConfigSrc)
	}
	if t.Name != name {
		return fmt.Errorf("tunnel %s is named %q, the config says %q; rename it in the dashboard or the config", t.ID, t.Name, name)
	}
	return nil
}

func (r *run) tunnelID() string {
	if r.owned.tunnel == nil {
		return pendingID("tunnel")
	}
	return r.owned.tunnel.ID
}

func (r *run) wantIngress() []cloudflare.IngressRule {
	return []cloudflare.IngressRule{{Hostname: r.Config.Hostname, Service: Origin}, {Service: catchAll}}
}

func (r *run) tunnelConfig() error {
	obj := fmt.Sprintf("tunnel %q configuration", r.Config.TunnelName)
	want := cloudflare.TunnelConfig{Ingress: r.wantIngress()}
	var fields []Field
	if r.owned.tunnel == nil || r.st.TunnelID == "" {
		fields = []Field{{Name: "ingress", Have: "(none)", Want: formatIngress(want.Ingress)}}
	} else {
		have, err := r.Client.GetTunnelConfiguration(r.ctx, r.Config.AccountID, r.tunnelID())
		if err != nil {
			return err
		}
		fields = diffTunnelConfig(have.Config, want)
	}
	if len(fields) == 0 {
		return nil
	}
	r.add(Change{Action: "update", Object: obj, Fields: fields})
	if !r.Apply {
		return nil
	}
	if _, err := r.Client.PutTunnelConfiguration(r.ctx, r.Config.AccountID, r.tunnelID(), want); err != nil {
		return err
	}
	r.logf("updated %s", obj)
	return nil
}

func diffTunnelConfig(have, want cloudflare.TunnelConfig) []Field {
	var fields []Field
	if !sameIngress(have.Ingress, want.Ingress) {
		fields = append(fields, Field{Name: "ingress", Have: formatIngress(have.Ingress), Want: formatIngress(want.Ingress)})
	}
	if !zeroJSON(have.OriginRequest) {
		fields = append(fields, Field{Name: "originRequest", Have: string(have.OriginRequest), Want: "(unset)"})
	}
	if !zeroJSON(have.WarpRouting) {
		fields = append(fields, Field{Name: "warp-routing", Have: string(have.WarpRouting), Want: "(unset)"})
	}
	return fields
}

func sameIngress(a, b []cloudflare.IngressRule) bool {
	return slices.EqualFunc(a, b, func(x, y cloudflare.IngressRule) bool {
		return strings.EqualFold(x.Hostname, y.Hostname) && x.Path == y.Path && x.Service == y.Service &&
			zeroJSON(x.OriginRequest) && zeroJSON(y.OriginRequest)
	})
}

func formatIngress(rules []cloudflare.IngressRule) string {
	if len(rules) == 0 {
		return "[]"
	}
	parts := make([]string, len(rules))
	for i, x := range rules {
		h := x.Hostname
		if h == "" {
			h = "*"
		}
		h += x.Path
		s := h + " -> " + x.Service
		if !zeroJSON(x.OriginRequest) {
			s += " " + string(x.OriginRequest)
		}
		parts[i] = s
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// zeroJSON reports whether raw sets nothing: absent, null, or an object or
// array holding only false, zero, empty or null values. The API may echo
// settings back as {} or {"enabled": false}, which mean the same as unset.
func zeroJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	var zero func(any) bool
	zero = func(v any) bool {
		switch x := v.(type) {
		case nil:
			return true
		case bool:
			return !x
		case float64:
			return x == 0
		case string:
			return x == ""
		case []any:
			for _, e := range x {
				if !zero(e) {
					return false
				}
			}
			return true
		case map[string]any:
			for _, e := range x {
				if !zero(e) {
					return false
				}
			}
			return true
		}
		return false
	}
	return zero(v)
}

func (r *run) dnsRecord() error {
	want := cloudflare.DNSRecord{
		Type: "CNAME", Name: r.Config.Hostname, Content: r.tunnelID() + ".cfargotunnel.com",
		Proxied: true, TTL: 1, Comment: Marker,
	}
	obj := fmt.Sprintf("DNS record %s", r.Config.Hostname)
	have := r.owned.dns
	if have == nil {
		r.add(Change{Action: "create", Object: obj, Fields: []Field{{Name: "record", Have: "(none)", Want: formatRecord(want)}}})
		if !r.Apply {
			return nil
		}
		created, err := r.Client.CreateDNSRecord(r.ctx, r.zone.ID, want)
		if err != nil {
			return err
		}
		r.logf("created %s (id %s)", obj, created.ID)
		r.st.DNSRecordID, r.st.ZoneID = created.ID, r.zone.ID
		return r.save()
	}
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
	if have.Comment != want.Comment {
		fields = append(fields, Field{"comment", strconv.Quote(have.Comment), strconv.Quote(want.Comment)})
	}
	if len(fields) == 0 {
		return nil
	}
	r.add(Change{Action: "update", Object: obj, Fields: fields})
	if !r.Apply {
		return nil
	}
	want.Tags = have.Tags
	if _, err := r.Client.UpdateDNSRecord(r.ctx, r.zone.ID, have.ID, want); err != nil {
		return err
	}
	r.logf("updated %s", obj)
	return nil
}

func formatRecord(d cloudflare.DNSRecord) string {
	return fmt.Sprintf("%s %s -> %s proxied=%t", d.Type, d.Name, d.Content, d.Proxied)
}

func (r *run) wantPolicy(role string) cloudflare.AccessPolicy {
	p := cloudflare.AccessPolicy{Name: r.policyName(role), SessionDuration: SessionDuration}
	if role == emailsPolicy {
		p.Decision = "allow"
		for _, e := range r.Config.AllowedEmails {
			p.Include = append(p.Include, cloudflare.EmailRule(e))
		}
	} else {
		p.Decision = "non_identity"
		p.Include = []json.RawMessage{cloudflare.AnyValidServiceTokenRule()}
	}
	p.Exclude, p.Require = []json.RawMessage{}, []json.RawMessage{}
	return p
}

func (r *run) policies() error {
	for _, role := range []string{emailsPolicy, tokensPolicy} {
		want := r.wantPolicy(role)
		obj := fmt.Sprintf("Access policy %q", want.Name)
		have := r.owned.policies[role]
		if have == nil {
			r.add(Change{Action: "create", Object: obj, Fields: []Field{{"rules", "(none)", formatPolicy(want)}}})
			if !r.Apply {
				continue
			}
			created, err := r.Client.CreateAccessPolicy(r.ctx, r.Config.AccountID, want)
			if err != nil {
				return err
			}
			r.logf("created %s (id %s)", obj, created.ID)
			r.owned.policies[role] = &created
			r.st.Policies[role] = created.ID
			if err := r.save(); err != nil {
				return err
			}
			continue
		}
		fields := diffPolicy(*have, want)
		if len(fields) == 0 {
			continue
		}
		r.add(Change{Action: "update", Object: obj, Fields: fields})
		if !r.Apply {
			continue
		}
		if _, err := r.Client.UpdateAccessPolicy(r.ctx, r.Config.AccountID, have.ID, want); err != nil {
			return err
		}
		r.logf("updated %s", obj)
	}
	return nil
}

func diffPolicy(have, want cloudflare.AccessPolicy) []Field {
	var fields []Field
	if have.Name != want.Name {
		fields = append(fields, Field{"name", strconv.Quote(have.Name), strconv.Quote(want.Name)})
	}
	if have.Decision != want.Decision {
		fields = append(fields, Field{"decision", have.Decision, want.Decision})
	}
	for _, l := range []struct {
		name       string
		have, want []json.RawMessage
	}{{"include", have.Include, want.Include}, {"exclude", have.Exclude, want.Exclude}, {"require", have.Require, want.Require}} {
		h, w := canonRules(l.have), canonRules(l.want)
		if !slices.Equal(h, w) {
			fields = append(fields, Field{l.name, "[" + strings.Join(h, ", ") + "]", "[" + strings.Join(w, ", ") + "]"})
		}
	}
	if have.SessionDuration != "" && !sameDuration(have.SessionDuration, want.SessionDuration) {
		fields = append(fields, Field{"session_duration", have.SessionDuration, want.SessionDuration})
	}
	return fields
}

func formatPolicy(p cloudflare.AccessPolicy) string {
	return p.Decision + " include [" + strings.Join(canonRules(p.Include), ", ") + "]"
}

// canonRules renders rules as sorted canonical JSON. Rules in one list are
// alternatives (include) or conjuncts (exclude, require), so order is not
// meaningful.
func canonRules(rules []json.RawMessage) []string {
	out := make([]string, 0, len(rules))
	for _, raw := range rules {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			out = append(out, string(raw))
			continue
		}
		b, _ := json.Marshal(v)
		out = append(out, string(b))
	}
	slices.Sort(out)
	return out
}

func sameDuration(a, b string) bool {
	da, errA := time.ParseDuration(a)
	db, errB := time.ParseDuration(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return da == db
}

func (r *run) policyLinks() []cloudflare.PolicyLink {
	var links []cloudflare.PolicyLink
	for i, role := range []string{emailsPolicy, tokensPolicy} {
		id := pendingID("policy")
		if p := r.owned.policies[role]; p != nil {
			id = p.ID
		}
		links = append(links, cloudflare.PolicyLink{ID: id, Precedence: i + 1})
	}
	return links
}

func (r *run) wantApp() cloudflare.AccessAppInput {
	return cloudflare.AccessAppInput{
		Name:                   r.Config.Hostname,
		Domain:                 r.Config.Hostname,
		Type:                   "self_hosted",
		SessionDuration:        SessionDuration,
		AllowedIdPs:            r.idps,
		AutoRedirectToIdentity: false,
		Destinations:           []cloudflare.AppDestination{{Type: "public", URI: r.Config.Hostname}},
		Policies:               r.policyLinks(),
	}
}

func (r *run) app() error {
	want := r.wantApp()
	obj := fmt.Sprintf("Access application %s", r.Config.Hostname)
	have := r.owned.app
	if have == nil {
		r.add(Change{Action: "create", Object: obj, Fields: []Field{
			{"session_duration", "(none)", want.SessionDuration},
			{"allowed_idps", "(none)", strings.Join(want.AllowedIdPs, ", ")},
			{"policies", "(none)", formatLinks(want.Policies)},
		}})
		if !r.Apply {
			return nil
		}
		created, err := r.Client.CreateAccessApp(r.ctx, r.Config.AccountID, want)
		if err != nil {
			return err
		}
		r.logf("created %s (id %s)", obj, created.ID)
		r.owned.app = &created
		r.st.AppID = created.ID
		return r.save()
	}
	fields := diffApp(*have, want)
	if len(fields) == 0 {
		return nil
	}
	r.add(Change{Action: "update", Object: obj, Fields: fields})
	if !r.Apply {
		return nil
	}
	updated, err := r.Client.UpdateAccessApp(r.ctx, r.Config.AccountID, have.ID, want)
	if err != nil {
		return err
	}
	r.logf("updated %s", obj)
	r.owned.app = &updated
	return nil
}

func diffApp(have cloudflare.AccessApp, want cloudflare.AccessAppInput) []Field {
	var fields []Field
	add := func(name, h, w string) { fields = append(fields, Field{name, h, w}) }
	if have.Name != want.Name {
		add("name", strconv.Quote(have.Name), strconv.Quote(want.Name))
	}
	if !strings.EqualFold(have.Domain, want.Domain) {
		add("domain", have.Domain, want.Domain)
	}
	if have.Type != want.Type {
		add("type", have.Type, want.Type)
	}
	if !sameDuration(have.SessionDuration, want.SessionDuration) {
		add("session_duration", have.SessionDuration, want.SessionDuration)
	}
	h, w := slices.Sorted(slices.Values(have.AllowedIdPs)), slices.Sorted(slices.Values(want.AllowedIdPs))
	if !slices.Equal(h, w) {
		add("allowed_idps", strings.Join(h, ", "), strings.Join(w, ", "))
	}
	if have.AutoRedirectToIdentity != want.AutoRedirectToIdentity {
		add("auto_redirect_to_identity", strconv.FormatBool(have.AutoRedirectToIdentity), strconv.FormatBool(want.AutoRedirectToIdentity))
	}
	// The API fills destinations in from domain when they are not sent, so
	// none at all is as good as exactly ours.
	if len(have.Destinations) > 0 && !slices.EqualFunc(have.Destinations, want.Destinations, func(a, b cloudflare.AppDestination) bool {
		return a.Type == b.Type && strings.EqualFold(strings.TrimSuffix(a.URI, "/"), b.URI)
	}) {
		add("destinations", formatDestinations(have.Destinations), formatDestinations(want.Destinations))
	}
	var links []cloudflare.PolicyLink
	for _, p := range have.Policies {
		links = append(links, cloudflare.PolicyLink{ID: p.ID, Precedence: p.Precedence})
	}
	byPrec := func(a, b cloudflare.PolicyLink) int { return a.Precedence - b.Precedence }
	slices.SortFunc(links, byPrec)
	if !slices.Equal(links, want.Policies) {
		add("policies", formatLinks(links), formatLinks(want.Policies))
	}
	return fields
}

func formatLinks(links []cloudflare.PolicyLink) string {
	parts := make([]string, len(links))
	for i, l := range links {
		parts[i] = fmt.Sprintf("%d:%s", l.Precedence, l.ID)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func formatDestinations(ds []cloudflare.AppDestination) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.Type + ":" + d.URI
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (r *run) serviceToken() error {
	obj := fmt.Sprintf("service token %q", SmokeTokenName)
	if t := r.owned.token; t != nil {
		if t.ExpiresAt != nil && time.Until(*t.ExpiresAt) < 30*24*time.Hour {
			r.res.Notes = append(r.res.Notes, fmt.Sprintf("%s expires at %s", obj, t.ExpiresAt.Format(time.RFC3339)))
		}
		if b, err := readFile(r.Dir, SmokeTokenEnv); err != nil {
			return err
		} else if b == nil {
			r.res.Notes = append(r.res.Notes, fmt.Sprintf("%s exists but %s is not in the output directory; Cloudflare returns a secret only once", obj, SmokeTokenEnv))
		}
		return nil
	}
	r.add(Change{Action: "create", Object: obj})
	r.add(Change{Action: "write", Object: "output " + SmokeTokenEnv})
	if !r.Apply {
		return nil
	}
	// The secret exists only in the create response, so its file is opened
	// first: a directory that cannot be written fails before the token exists.
	p, err := newPending(r.Dir, SmokeTokenEnv)
	if err != nil {
		return err
	}
	created, err := r.Client.CreateServiceToken(r.ctx, r.Config.AccountID, SmokeTokenName, smokeTokenTTL)
	if err != nil {
		p.abort()
		return err
	}
	r.st.ServiceTokenID = created.ID
	content := "HANGAR_CF_CLIENT_ID=" + created.ClientID + "\nHANGAR_CF_CLIENT_SECRET=" + created.ClientSecret.Reveal() + "\n"
	if err := p.commit([]byte(content)); err != nil {
		_ = r.save()
		return fmt.Errorf("%s was created (id %s) but its secret could not be saved, so it is lost; delete the token in the dashboard and apply again: %w", obj, created.ID, err)
	}
	r.logf("created %s (id %s); its client ID and secret are in %s", obj, created.ID, SmokeTokenEnv)
	return r.save()
}

func (r *run) outputs() error {
	var token string
	if r.owned.tunnel != nil {
		t, err := r.Client.TunnelToken(r.ctx, r.Config.AccountID, r.owned.tunnel.ID)
		if err != nil {
			return err
		}
		token = t.Reveal()
	}
	aud := ""
	if r.owned.app != nil {
		aud = r.owned.app.AUD
	}
	// known is false while planning objects that do not exist yet.
	files := []struct {
		name, content string
		known         bool
	}{
		{CloudflaredEnv, "TUNNEL_TOKEN=" + token + "\n", token != ""},
		{AccessEnv, "HANGAR_CF_TEAM_DOMAIN=" + r.team + "\nHANGAR_CF_AUDIENCE=" + aud + "\n", aud != ""},
	}
	for _, f := range files {
		have, err := readFile(r.Dir, f.name)
		if err != nil {
			return err
		}
		if f.known && string(have) == f.content {
			continue
		}
		r.add(Change{Action: "write", Object: "output " + f.name})
		if !r.Apply {
			continue
		}
		if !f.known {
			return fmt.Errorf("internal: %s content unknown after apply", f.name)
		}
		if err := writeFile(r.Dir, f.name, []byte(f.content)); err != nil {
			return err
		}
		r.logf("wrote %s", f.name)
	}
	return nil
}
