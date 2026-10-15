//go:build testbed

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/caddyserver/certmagic"

	"github.com/YukinaMochizuki/hangar/internal/edge/front/acmetest"
)

// testDNSUsage is what --acme-dns takes besides cloudflare in a build with
// the testbed tag.
const testDNSUsage = ", or testbed:<host:port> (a DNS server of the edge's own there, for a test CA such as pebble)"

// testDNS, for --acme-dns testbed:<host:port>, serves the two domains'
// zones on host:port itself and answers the DNS-01 challenges there: a test
// CA (pebble -dnsserver host:port) asks it, and no real DNS is touched. It
// is for the L4 test on a testbed only (docs/runbook/testbed.md); a release
// is built without the tag and has no such option.
func testDNS(ctx context.Context, spec string, zones []string, log *slog.Logger) (certmagic.DNSProvider, []string, bool, error) {
	addr, ok := strings.CutPrefix(spec, "testbed:")
	if !ok {
		return nil, nil, false, nil
	}
	s, err := acmetest.Start(addr, zones...)
	if err != nil {
		return nil, nil, true, fmt.Errorf("--acme-dns %s: %w", spec, err)
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	log.Warn("answering DNS-01 challenges with a test DNS server: certificates from a real CA are impossible", "address", s.Addr, "zones", zones)
	return s, []string{s.Addr}, true, nil
}
