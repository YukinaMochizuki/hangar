// Package customdomain holds the rules for a tenant's own domains (the
// CustomDomain kind) and the hostnames public endpoints serve under them,
// which the apiserver admits by and hangar-edge routes by, so both read a
// hostname the same way. It does no DNS and keeps no state. See
// docs/decisions/custom-domains.md.
package customdomain

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// maxName is the longest name DNS carries, without the trailing dot.
const maxName = 253

// CheckHostname refuses a name that is not a hostname as CustomDomains and
// endpoints write one: lower case, no trailing dot, at least two labels,
// each of letters, digits and inner hyphens, 63 characters at most.
// Internationalized names are written in their xn-- form.
func CheckHostname(name string) error {
	if name == "" {
		return fmt.Errorf("is empty")
	}
	if len(name) > maxName {
		return fmt.Errorf("%q is %d characters, more than %d", name, len(name), maxName)
	}
	if name != strings.ToLower(name) {
		return fmt.Errorf("%q must be in lower case", name)
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%q has one label; a hostname has at least two", name)
	}
	for _, l := range labels {
		if err := checkLabel(l); err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
	}
	return nil
}

func checkLabel(l string) error {
	if l == "" {
		return fmt.Errorf("has an empty label (a leading, trailing or doubled dot)")
	}
	if len(l) > 63 {
		return fmt.Errorf("label %q is longer than 63 characters", l)
	}
	for i, c := range l {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(l)-1:
		default:
			return fmt.Errorf("label %q may hold only letters, digits and hyphens, and neither starts nor ends with a hyphen", l)
		}
	}
	return nil
}

// CheckDomain refuses a name that cannot be a tenant's CustomDomain: one that is
// not a hostname, or is a public suffix, such as "com" or "github.io",
// which no one holds alone.
func CheckDomain(domain string) error {
	if err := CheckHostname(domain); err != nil {
		return err
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(domain); err != nil {
		return fmt.Errorf("%q is a public suffix, which no one holds alone", domain)
	}
	return nil
}

// Site is the registrable domain name is in: its public suffix and one
// label more, such as "example.com" for "app.example.com".
func Site(name string) (string, error) {
	return publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(strings.ToLower(name), "."))
}

// Overlap reports whether two domains are one, or one is under the other:
// what no two tenants' verified CustomDomains may be.
func Overlap(a, b string) bool {
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// Serves reports whether a CustomDomain for domain serves hostname:
// hostname is the domain itself or one label below it, which its
// certificates, the domain's and its wildcard's, cover.
func Serves(domain, hostname string) bool {
	if hostname == domain {
		return true
	}
	label, ok := strings.CutSuffix(hostname, "."+domain)
	return ok && label != "" && !strings.Contains(label, ".")
}

// Domain is what deciding where a hostname serves needs of a CustomDomain.
type Domain struct {
	// Name is the CustomDomain's object name.
	Name, Domain, Tenant string
	// Projects are the projects it lets serve under it; none is all.
	Projects []string
	Verified bool
}

// Allows reports whether d lets project's endpoints serve under it.
func (d Domain) Allows(project string) bool {
	return len(d.Projects) == 0 || slices.Contains(d.Projects, project)
}

// Of is the CustomDomain that hostname, an endpoint's of a project of
// tenant, is under: the longest of the tenant's domains that serves it.
// Another tenant's domain never is, verified or not. The hostname serves
// once the domain is verified and allows the project
// (Domain.Verified, Domain.Allows).
func Of(hostname, tenant string, domains []Domain) (Domain, bool) {
	var best Domain
	for _, d := range domains {
		if d.Tenant == tenant && Serves(d.Domain, hostname) && len(d.Domain) > len(best.Domain) {
			best = d
		}
	}
	return best, best.Domain != ""
}

// Match is the domain of domains that serves hostname: the longest, when
// both a domain and one under it do, as "example.com" and "b.example.com"
// both serve "b.example.com".
func Match(hostname string, domains []string) (string, bool) {
	var best string
	for _, d := range domains {
		if Serves(d, hostname) && len(d) > len(best) {
			best = d
		}
	}
	return best, best != ""
}

// ChallengeName is the name whose CNAME delegates a domain's DNS-01
// challenges, and proves who holds it.
func ChallengeName(domain string) string { return "_acme-challenge." + domain }

// ChallengeTarget is the name in zone, a zone hangar-edge's DNS provider
// writes, that the CustomDomain of uid has its challenges answered at. It
// is made from the uid alone, so a CustomDomain made again, even for the
// same domain, has another: a CNAME left behind by a deleted one proves
// nothing for a new one.

func ChallengeTarget(uid, zone string) string {
	sum := sha256.Sum256([]byte("hangar-domain-challenge/" + uid))
	label := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:15]))
	return label + "." + zone
}
