//go:build !testbed

package main

import (
	"context"
	"log/slog"

	"github.com/caddyserver/certmagic"
)

// testDNSUsage is what --acme-dns takes besides cloudflare: nothing, but in
// a build with the testbed tag (dns_testbed.go).
const testDNSUsage = ""

// testDNS answers DNS-01 challenges for a test CA only in a build with the
// testbed tag.
func testDNS(context.Context, string, []string, *slog.Logger) (certmagic.DNSProvider, []string, bool, error) {
	return nil, nil, false, nil
}
