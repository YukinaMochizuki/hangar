package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestTheFrontDoorNeedsAnACMEEmail: with --cp-domain and no --acme-email
// the edge refuses to start; with the front door off it needs none.
func TestTheFrontDoorNeedsAnACMEEmail(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	err := run(context.Background(), flags{cpDomain: "cp.example.com", domain: "user.example.com"}, log)
	if err == nil || !strings.Contains(err.Error(), "--acme-email") {
		t.Errorf("front door without --acme-email: %v", err)
	}
	if err := checkFlags(flags{cpDomain: "cp.example.com", acmeMail: "ops@example.com"}); err != nil {
		t.Errorf("front door with --acme-email: %v", err)
	}
	if err := checkFlags(flags{listen: "127.0.0.1:8090"}); err != nil {
		t.Errorf("front door off, no --acme-email: %v", err)
	}
}

// TestTheTwoDomainsAreApart: the edge refuses to start when --cp-domain
// and --domain share a registrable domain (the same, one under the other,
// or siblings), in any case and with or without a final dot, or when one
// is a public suffix with none; domains that only end alike, or are under
// one public suffix, are apart.
func TestTheTwoDomainsAreApart(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	door := func(cp, domain string) flags {
		return flags{cpDomain: cp, domain: domain, acmeMail: "ops@example.com"}
	}
	for _, d := range [][3]string{
		{"example.com", "example.com", "must not share a registrable domain"},
		{"example.com", "Example.COM.", "must not share a registrable domain"},
		{"example.com", "user.example.com", "must not share a registrable domain"},
		{"cp.example.com", "example.com", "must not share a registrable domain"},
		{"a.b.example.com", "example.com.", "must not share a registrable domain"},
		{"cp.example.com", "user.example.com", "must not share a registrable domain"},
		{"cp.example.co.uk", "user.example.co.uk", "must not share a registrable domain"},
		{"com", "example.com", "--cp-domain com has no registrable domain"},
		{"example.com", "co.uk", "--domain co.uk has no registrable domain"},
	} {
		err := run(context.Background(), door(d[0], d[1]), log)
		if err == nil || !strings.Contains(err.Error(), d[2]) {
			t.Errorf("--cp-domain %s --domain %s: %v, want %q", d[0], d[1], err, d[2])
		}
	}
	for _, d := range [][2]string{
		{"hangarlab.cloud", "hangarlab.dev"},
		{"cp.test", "user.test"},
		{"flowcp.test", "hangarlab.test"},
		{"example.com", "myexample.com"},
		{"ample.com", "example.com"},
		{"example.co.uk", "other.co.uk"},
	} {
		if err := checkFlags(door(d[0], d[1])); err != nil {
			t.Errorf("--cp-domain %s --domain %s: %v", d[0], d[1], err)
		}
	}
}

// TestConsoleOriginIsAnHTTPSOrigin: the console the edge sends browsers to
// sign in is an https origin, from --console-url or made of --cp-domain;
// anything else is refused at start.
func TestConsoleOriginIsAnHTTPSOrigin(t *testing.T) {
	for _, c := range []struct {
		f    flags
		want string
	}{
		{flags{consoleURL: "https://console.example.com"}, "https://console.example.com"},
		{flags{consoleURL: "https://console.example.com/"}, "https://console.example.com"},
		{flags{consoleURL: "https://console.example.com:8443"}, "https://console.example.com:8443"},
		{flags{cpDomain: "example.com"}, "https://console.example.com"},
		{flags{consoleURL: "https://console.example.com", cpDomain: "other.com"}, "https://console.example.com"},
	} {
		got, err := consoleOrigin(c.f)
		if err != nil || got != c.want {
			t.Errorf("%+v: %q, %v; want %q", c.f, got, err, c.want)
		}
	}
	for _, f := range []flags{
		{},
		{consoleURL: "http://console.example.com"},
		{consoleURL: "http://console.example.com", cpDomain: "example.com"},
		{consoleURL: "console.example.com"},
		{consoleURL: "https://"},
		{consoleURL: "https://console.example.com/auth"},
		{consoleURL: "https://console.example.com?x=1"},
		{consoleURL: "https://user@console.example.com"},
		{consoleURL: "javascript:alert(1)"},
	} {
		if got, err := consoleOrigin(f); err == nil {
			t.Errorf("%+v: accepted as %q", f, got)
		}
	}
}
