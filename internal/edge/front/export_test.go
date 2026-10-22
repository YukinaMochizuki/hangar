package front

import (
	"time"

	"github.com/caddyserver/certmagic"
)

// SetRefusalSummaryEvery makes f sum up its refused connections every d.
func SetRefusalSummaryEvery(f *Front, d time.Duration) { f.refusalEvery = d }

// OpenConns is how many connections f counts open.
func OpenConns(f *Front) int {
	f.counts.mu.Lock()
	defer f.counts.mu.Unlock()
	return f.counts.conns
}

// RenewalChallenge is where the config that renews a certificate for names
// answers its DNS-01 challenges: "" for the names' own _acme-challenge.
func RenewalChallenge(c *Certs, names ...string) string {
	for _, i := range c.configFor(names).Issuers {
		if a, ok := i.(*certmagic.ACMEIssuer); ok {
			if s, ok := a.DNS01Solver.(*certmagic.DNS01Solver); ok {
				return s.OverrideDomain
			}
		}
	}
	return ""
}
