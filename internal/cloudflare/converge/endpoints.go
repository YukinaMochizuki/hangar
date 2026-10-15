package converge

import (
	"fmt"
	"strings"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
)

// observeEndpoints fetches the public endpoints' objects the state names and
// looks for objects that would collide with them but are not the tool's.
// The apiserver's applications for PUBLIC endpoints, one hostname each
// below the wildcard, are its own and never collide; any other application
// covering a hostname below it is a note, as it shadows an endpoint.
func (r *run) observeEndpoints(apps []cloudflare.AccessApp, foreign func(kind, name, id string)) error {
	ep := r.Config.Endpoints
	if ep == nil {
		return nil
	}
	acct, wildcard := r.Config.AccountID, ep.Wildcard()
	if id := r.st.EndpointsDNSRecordID; id != "" {
		d, err := r.Client.GetDNSRecord(r.ctx, r.epZone.ID, id)
		switch {
		case cloudflare.IsNotFound(err):
		case err != nil:
			return err
		default:
			r.owned.epDNS = &d
		}
	}
	records, err := r.Client.ListDNSRecords(r.ctx, r.epZone.ID, wildcard)
	if err != nil {
		return err
	}
	for _, d := range records {
		if strings.EqualFold(d.Name, wildcard) && (r.owned.epDNS == nil || d.ID != r.owned.epDNS.ID) {
			foreign("DNS "+d.Type+" record", d.Name, d.ID)
		}
	}
	if id := r.st.EndpointsAppID; id != "" {
		a, err := r.Client.GetAccessApp(r.ctx, acct, id)
		switch {
		case cloudflare.IsNotFound(err):
		case err != nil:
			return err
		default:
			r.owned.epApp = &a
		}
	}
	for _, a := range apps {
		if covers(a, wildcard) && (r.owned.epApp == nil || a.ID != r.owned.epApp.ID) {
			foreign("Access application", a.Name, a.ID)
			continue
		}
		if host, ok := strings.CutPrefix(a.Name, cloudflare.EndpointAppPrefix); ok && endpointHost(host, ep.Zone) {
			continue
		}
		for _, h := range appHosts(a) {
			if endpointHost(h, ep.Zone) {
				r.res.Notes = append(r.res.Notes, fmt.Sprintf(
					"Access application %q (id %s) covers %s, below the endpoint domain; only the apiserver makes applications there, for PUBLIC endpoints",
					a.Name, a.ID, h))
				break
			}
		}
	}
	return nil
}

// endpointHost reports whether host is one label below zone, where public
// endpoints are.
func endpointHost(host, zone string) bool {
	label, ok := strings.CutSuffix(strings.ToLower(host), "."+zone)
	return ok && label != "" && label != "*" && !strings.ContainsAny(label, "./")
}

// appHosts are the hostnames an application covers, paths dropped.
func appHosts(a cloudflare.AccessApp) []string {
	hosts := []string{a.Domain}
	hosts = append(hosts, a.SelfHostedDomains...)
	for _, d := range a.Destinations {
		hosts = append(hosts, d.URI)
	}
	for i, h := range hosts {
		hosts[i], _, _ = strings.Cut(h, "/")
	}
	return hosts
}

// endpointsApp converges the wildcard application every PRIVATE endpoint is
// behind: the same people and service tokens as the API's. The apiserver's
// applications for PUBLIC endpoints, each for one hostname, are more
// specific, so Access applies them instead.
func (r *run) endpointsApp() error {
	ep := r.Config.Endpoints
	if ep == nil {
		return nil
	}
	want := r.wantAppFor(ep.Wildcard())
	obj := fmt.Sprintf("Access application %s", ep.Wildcard())
	have := r.owned.epApp
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
		r.owned.epApp = &created
		r.st.EndpointsAppID = created.ID
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
	r.owned.epApp = &updated
	return nil
}

// endpointsDNS converges the wildcard record that sends every endpoint's
// hostname to the tunnel, after the application and the ingress, so no
// endpoint is routed before Access covers it.
func (r *run) endpointsDNS() error {
	ep := r.Config.Endpoints
	if ep == nil {
		return nil
	}
	want := cloudflare.DNSRecord{
		Type: "CNAME", Name: ep.Wildcard(), Content: r.tunnelID() + ".cfargotunnel.com",
		Proxied: true, TTL: 1, Comment: Marker,
	}
	obj := fmt.Sprintf("DNS record %s", ep.Wildcard())
	have := r.owned.epDNS
	if have == nil {
		r.add(Change{Action: "create", Object: obj, Fields: []Field{{Name: "record", Have: "(none)", Want: formatRecord(want)}}})
		if !r.Apply {
			return nil
		}
		created, err := r.Client.CreateDNSRecord(r.ctx, r.epZone.ID, want)
		if err != nil {
			return err
		}
		r.logf("created %s (id %s)", obj, created.ID)
		r.st.EndpointsDNSRecordID, r.st.EndpointsZoneID = created.ID, r.epZone.ID
		return r.save()
	}
	fields := diffRecord(*have, want)
	if len(fields) == 0 {
		return nil
	}
	r.add(Change{Action: "update", Object: obj, Fields: fields})
	if !r.Apply {
		return nil
	}
	want.Tags = have.Tags
	if _, err := r.Client.UpdateDNSRecord(r.ctx, r.epZone.ID, have.ID, want); err != nil {
		return err
	}
	r.logf("updated %s", obj)
	return nil
}

// edgeEnv is what hangar-edge and the apiserver need of the endpoints'
// setup; known is false while planning objects that do not exist yet.
func (r *run) edgeEnv() (content string, known bool) {
	aud, bypass := "", r.st.Policies[bypassPolicy]
	if r.owned.epApp != nil {
		aud = r.owned.epApp.AUD
	}
	content = "HANGAR_EDGE_DOMAIN=" + r.Config.Endpoints.Zone + "\n" +
		"HANGAR_EDGE_CF_AUDIENCE=" + aud + "\n" +
		"HANGAR_CF_ACCOUNT_ID=" + r.Config.AccountID + "\n" +
		"HANGAR_CF_BYPASS_POLICY_ID=" + bypass + "\n"
	return content, aud != "" && bypass != ""
}
