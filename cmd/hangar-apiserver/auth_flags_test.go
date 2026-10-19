package main

import (
	"strings"
	"testing"
)

// TestDevAuthOnlyOnALoopbackIP: dev auth listens on a loopback IP, not on
// localhost, which a hosts file can point anywhere, nor anywhere else.
func TestDevAuthOnlyOnALoopbackIP(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.1.2.3:8080", "[::1]:8080"} {
		if err := checkAuthFlags(flags{authMode: "dev", listen: addr}); err != nil {
			t.Errorf("dev auth on %s: %v", addr, err)
		}
	}
	for _, addr := range []string{"localhost:8080", "LOCALHOST:8080", "localhost.:8080", "0.0.0.0:8080", "[::]:8080", ":8080", "10.0.0.1:8080", "127.0.0.1"} {
		err := checkAuthFlags(flags{authMode: "dev", listen: addr})
		if err == nil || !strings.Contains(err.Error(), "loopback IP") {
			t.Errorf("dev auth on %s: %v", addr, err)
		}
	}
}

// TestBootstrapAdminIsASCII: a --bootstrap-admin that is not ASCII is
// refused before it is lower-cased, which could make it another address.
func TestBootstrapAdminIsASCII(t *testing.T) {
	for _, email := range []string{"", "root@example.com", "Root@Example.com"} {
		if err := checkBootstrapAdmin(email); err != nil {
			t.Errorf("%q: %v", email, err)
		}
	}
	for _, email := range []string{"\u212aate@example.com", "gil@ex\u00e4mple.com"} {
		if err := checkBootstrapAdmin(email); err == nil || !strings.Contains(err.Error(), "must be ASCII") {
			t.Errorf("%q: %v", email, err)
		}
	}
}

// TestOIDCPublicURLOverHTTPOnlyOnALoopbackIP: an http public URL is for a
// loopback IP; localhost needs https like any other name.
func TestOIDCPublicURLOverHTTPOnlyOnALoopbackIP(t *testing.T) {
	oidc := func(url string) flags {
		return flags{authMode: "oidc", oidcClientID: "c", oidcSecretFile: "/s", publicURL: url}
	}
	for _, u := range []string{"http://127.0.0.1:5173", "http://[::1]:5173", "https://localhost:5173"} {
		if err := checkAuthFlags(oidc(u)); err != nil {
			t.Errorf("--public-url %s: %v", u, err)
		}
	}
	if err := checkAuthFlags(oidc("http://localhost:5173")); err == nil || !strings.Contains(err.Error(), "must be https") {
		t.Errorf("--public-url http://localhost:5173: %v", err)
	}
}
