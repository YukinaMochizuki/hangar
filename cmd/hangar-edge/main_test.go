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
