package converge

import (
	"fmt"
	"strings"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
)

// observeEndpoints fetches the public endpoints' record the state names and
// looks for records that would collide with it but are not the tool's. It
// notes the Access objects an earlier version made for endpoints, which
// nothing uses any more, for someone to delete by hand.
func (r *run) observeEndpoints(foreign func(kind, name, id string)) error {
	ep := r.Config.Endpoints
	if ep == nil {
		return nil
	}
	wildcard := ep.Wildcard()
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
	r.noteOldEndpointAccess()
	return nil
}

// noteOldEndpointAccess names the Access objects an earlier version made
// for public endpoints, which hangar-edge no longer needs: the wildcard
// application and the bypass policy; the apiserver's applications for
// PUBLIC endpoints are named cloudflare.EndpointAppPrefix and a hostname.
// The tool deletes nothing, so they are for someone to delete by hand
// (docs/runbook/cloudflare.md).
func (r *run) noteOldEndpointAccess() {
	if id := r.st.EndpointsAppID; id != "" {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf(
			"Access application %s (id %s) is no longer used: hangar-edge signs endpoints' callers in itself; delete it by hand", r.Config.Endpoints.Wildcard(), id))
	}
	if id := r.st.Policies[oldBypassPolicy]; id != "" {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf(
			"Access policy for public endpoints (id %s) is no longer used; delete it by hand, with every application named %q", id, cloudflare.EndpointAppPrefix+"<hostname>"))
	}
}

// endpointsDNS converges the wildcard record that sends every endpoint's
// hostname to the tunnel, after the ingress, so no endpoint is routed
// before the tunnel knows where to send it.
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

// edgeEnv is what hangar-edge needs of the endpoints' setup.
func (r *run) edgeEnv() (content string, known bool) {
	return "HANGAR_EDGE_DOMAIN=" + r.Config.Endpoints.Zone + "\n", true
}
