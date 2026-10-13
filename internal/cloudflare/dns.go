package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Zone is a DNS zone.
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// DNSRecord is a zone's DNS record. TTL 1 means automatic, the only value a
// proxied record has.
type DNSRecord struct {
	ID      string   `json:"id,omitempty"`
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Content string   `json:"content"`
	Proxied bool     `json:"proxied"`
	TTL     int      `json:"ttl"`
	Comment string   `json:"comment,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// ZoneByName returns the account's zone called name.
func (c *Client) ZoneByName(ctx context.Context, account, name string) (Zone, error) {
	q := url.Values{"name": {name}, "account.id": {account}}
	zones, err := listAll[Zone](ctx, c, "/zones", q, 50)
	if err != nil {
		return Zone{}, err
	}
	var found []Zone
	for _, z := range zones {
		if z.Name == name {
			found = append(found, z)
		}
	}
	switch len(found) {
	case 0:
		return Zone{}, fmt.Errorf("cloudflare: no zone %q in account %s, or the token cannot read it", name, account)
	case 1:
		return found[0], nil
	}
	return Zone{}, errors.New("cloudflare: more than one zone named " + name)
}

// ListDNSRecords returns the zone's records of any type named exactly name.
func (c *Client) ListDNSRecords(ctx context.Context, zone, name string) ([]DNSRecord, error) {
	q := url.Values{"name.exact": {name}}
	return listAll[DNSRecord](ctx, c, "/zones/"+esc(zone)+"/dns_records", q, 100)
}

// GetDNSRecord returns one record.
func (c *Client) GetDNSRecord(ctx context.Context, zone, id string) (DNSRecord, error) {
	var r DNSRecord
	_, err := c.do(ctx, http.MethodGet, "/zones/"+esc(zone)+"/dns_records/"+esc(id), nil, nil, &r)
	return r, err
}

// CreateDNSRecord creates a record.
func (c *Client) CreateDNSRecord(ctx context.Context, zone string, r DNSRecord) (DNSRecord, error) {
	r.ID = ""
	var out DNSRecord
	_, err := c.do(ctx, http.MethodPost, "/zones/"+esc(zone)+"/dns_records", nil, r, &out)
	return out, err
}

// UpdateDNSRecord overwrites a record (PUT), so fields left empty are reset.
func (c *Client) UpdateDNSRecord(ctx context.Context, zone, id string, r DNSRecord) (DNSRecord, error) {
	r.ID = ""
	var out DNSRecord
	_, err := c.do(ctx, http.MethodPut, "/zones/"+esc(zone)+"/dns_records/"+esc(id), nil, r, &out)
	return out, err
}
