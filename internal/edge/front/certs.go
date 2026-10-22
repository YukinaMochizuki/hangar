package front

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

// LetsEncrypt and LetsEncryptStaging are the ACME directories of Let's
// Encrypt: production, the default, and staging, for testbeds, whose
// certificates no browser trusts and whose rate limits are far higher.
const (
	LetsEncrypt        = certmagic.LetsEncryptProductionCA
	LetsEncryptStaging = certmagic.LetsEncryptStagingCA
)

// CertConfig says where the front door's certificates come from and are
// kept.
type CertConfig struct {
	// Storage is the directory certificates, their keys and the ACME
	// account are kept in, across restarts: only the edge's.
	Storage string
	// CA is the ACME directory URL; empty is Let's Encrypt.
	CA string
	// CARoots, if set, are the only roots the ACME directory's TLS
	// certificate is checked against: a test CA's.
	CARoots *x509.CertPool
	// Email is the ACME account's contact, told of problems with the
	// certificates. It is required: without it certmagic uses an email it
	// remembers for the whole process, which a race can set to "default".
	Email string
	// DNS sets and removes the TXT records DNS-01 challenges ask for. It is
	// the only challenge the front door solves, so certificates are issued
	// whatever the names point at, and nothing is served for ACME on :80.
	DNS certmagic.DNSProvider
	// Resolvers, if set, are the DNS servers the zone of a name and the
	// propagation of a record are asked of, as host:port; empty is the
	// system's.
	Resolvers []string
	// Names are the hostnames to keep certificates for; one may be a
	// wildcard.
	Names []string
	Log   *slog.Logger
}

// Certs are the front door's certificates, as Certificates keeps them.
type Certs struct {
	// TLS hands each handshake the certificate for its server name; one for
	// a name without a certificate yet, or no name the front door has,
	// fails.
	TLS   *tls.Config
	cache *certmagic.Cache
	// newConfig makes a certmagic config whose DNS-01 challenges are
	// answered at override, or at each name's own _acme-challenge for "".
	newConfig func(override string) *certmagic.Config
	base      *certmagic.Config
	ctx       context.Context
	log       *slog.Logger

	mu sync.Mutex
	// delegated are the domains whose challenges are delegated, by domain.
	delegated map[string]delegation
}

// delegation is how a delegated domain's certificates are kept.
type delegation struct {
	cfg       *certmagic.Config
	challenge string
	// stop ends what certmagic does for the domain, an order it retries
	// included, which would otherwise outlive the delegation.
	stop context.CancelFunc
}

// NotAfter is when the certificate held for name expires, the latest of
// them if several are; false while none is held.
func (c *Certs) NotAfter(name string) (time.Time, bool) {
	var last time.Time
	for _, cert := range c.cache.AllMatchingCertificates(name) {
		if cert.Leaf != nil && cert.Leaf.NotAfter.After(last) {
			last = cert.Leaf.NotAfter
		}
	}
	return last, !last.IsZero()
}

// Certificates keeps the front door's certificates: it gets those it lacks
// and renews them, in the background, until ctx is done.
func Certificates(ctx context.Context, cc CertConfig) (*Certs, error) {
	if cc.Storage == "" {
		return nil, errors.New("certificates need a storage directory")
	}
	if cc.DNS == nil {
		return nil, errors.New("certificates need a DNS provider for the DNS-01 challenge")
	}
	if len(cc.Names) == 0 {
		return nil, errors.New("no names to get certificates for")
	}
	if cc.Email == "" {
		return nil, errors.New("certificates need an email for the ACME account")
	}
	if cc.CA == "" {
		cc.CA = LetsEncrypt
	}
	if cc.Log == nil {
		cc.Log = slog.Default()
	}
	logger := zap.New(slogCore{log: cc.Log.With("component", "certmagic")})

	c := &Certs{ctx: ctx, log: cc.Log, delegated: map[string]delegation{}}
	c.cache = certmagic.NewCache(certmagic.CacheOptions{
		// Renewal must answer a delegated name's challenges where its domain
		// delegates them, so each certificate goes back to its own config.
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) { return c.configFor(cert.Names), nil },
		Logger:           logger,
	})
	storage := &certmagic.FileStorage{Path: cc.Storage}
	// After a failure certmagic tries the challenges against a test CA,
	// Let's Encrypt's staging, before the real one again, to spare the
	// real one's rate limits; any other CA is its own test CA.
	testCA := cc.CA
	if cc.CA == LetsEncrypt {
		testCA = LetsEncryptStaging
	}
	c.newConfig = func(override string) *certmagic.Config {
		cfg := certmagic.New(c.cache, certmagic.Config{Storage: storage, Logger: logger})
		cfg.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
			CA:                      cc.CA,
			TestCA:                  testCA,
			Email:                   cc.Email,
			Agreed:                  true,
			TrustedRoots:            cc.CARoots,
			DisableHTTPChallenge:    true,
			DisableTLSALPNChallenge: true,
			DNS01Solver: &certmagic.DNS01Solver{
				DNSManager: certmagic.DNSManager{
					DNSProvider:    cc.DNS,
					Resolvers:      cc.Resolvers,
					OverrideDomain: override,
					Logger:         logger,
				},
			},
			Logger: logger,
		})}
		return cfg
	}
	c.base = c.newConfig("")
	if err := c.base.ManageAsync(ctx, cc.Names); err != nil {
		c.cache.Stop()
		return nil, fmt.Errorf("certificates for %v: %w", cc.Names, err)
	}
	go func() {
		<-ctx.Done()
		c.cache.Stop()
	}()
	cc.Log.Info("keeping certificates", "names", cc.Names, "ca", cc.CA, "storage", cc.Storage)
	// The cache is shared, so the base config hands out every certificate,
	// the delegated ones included.
	c.TLS = &tls.Config{GetCertificate: c.base.GetCertificate}
	return c, nil
}

// configFor is the config that keeps a certificate for names.
func (c *Certs) configFor(names []string) *certmagic.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range names {
		if d, ok := c.delegated[strings.TrimPrefix(strings.ToLower(n), "*.")]; ok {
			return d.cfg
		}
	}
	return c.base
}

// Delegate keeps certificates for domain and *.domain, a tenant's own,
// whose DNS-01 challenges the domain delegates by a CNAME from
// _acme-challenge.<domain> to challenge, a name in a zone the front door's
// DNS provider writes. Delegating a domain again with the same challenge
// changes nothing.
func (c *Certs) Delegate(domain, challenge string) error {
	domain, challenge = strings.ToLower(domain), strings.ToLower(challenge)
	names := []string{domain, "*." + domain}
	c.mu.Lock()
	if d, ok := c.delegated[domain]; ok && d.challenge == challenge {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	c.Undelegate(domain)
	ctx, stop := context.WithCancel(c.ctx)
	cfg := c.newConfig(challenge)
	c.mu.Lock()
	c.delegated[domain] = delegation{cfg: cfg, challenge: challenge, stop: stop}
	c.mu.Unlock()
	if err := cfg.ManageAsync(ctx, names); err != nil {
		c.Undelegate(domain)
		return fmt.Errorf("certificates for %v: %w", names, err)
	}
	c.log.Info("keeping certificates of a delegated domain", "names", names, "challenge", challenge)
	return nil
}

// Undelegate stops keeping and handing out the certificates of a domain
// Delegate was given, and ends any order for them in progress. What
// storage holds of them stays, so the domain delegated again is served at
// once.
func (c *Certs) Undelegate(domain string) {
	domain = strings.ToLower(domain)
	c.mu.Lock()
	d, ok := c.delegated[domain]
	delete(c.delegated, domain)
	c.mu.Unlock()
	if ok {
		d.stop()
	}
	c.cache.RemoveManaged([]certmagic.SubjectIssuer{{Subject: domain}, {Subject: "*." + domain}})
}

// KeepDelegated keeps certificates for exactly the domains current returns,
// each delegating its challenges to the target it maps to, from now and at
// each signal of changes, until ctx is done: the custom domains
// hangar-edge has verified (docs/decisions/custom-domains.md).
func (c *Certs) KeepDelegated(ctx context.Context, changes <-chan struct{}, current func() map[string]string) {
	kept := map[string]string{}
	for {
		want := current()
		for d := range kept {
			if _, ok := want[d]; !ok {
				c.Undelegate(d)
				delete(kept, d)
				c.log.Info("stopped keeping the certificates of a delegated domain", "domain", d)
			}
		}
		for d, challenge := range want {
			if kept[d] == challenge {
				continue
			}
			if err := c.Delegate(d, challenge); err != nil {
				// Only a name certmagic cannot manage; the next change tries
				// again.
				c.log.Error("keeping the certificates of a delegated domain failed", "domain", d, "error", err)
				continue
			}
			kept[d] = challenge
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
		}
	}
}
