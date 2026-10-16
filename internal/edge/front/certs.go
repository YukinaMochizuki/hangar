package front

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"

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

// Certificates keeps the front door's certificates: it gets those it lacks
// and renews them, in the background, until ctx is done. The TLS config it
// returns hands each handshake the certificate for its server name; one for
// a name without a certificate yet, or no name the front door has, fails.
func Certificates(ctx context.Context, cc CertConfig) (*tls.Config, error) {
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

	var cfg *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return cfg, nil },
		Logger:           logger,
	})
	cfg = certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: cc.Storage},
		Logger:  logger,
	})
	// After a failure certmagic tries the challenges against a test CA,
	// Let's Encrypt's staging, before the real one again, to spare the
	// real one's rate limits; any other CA is its own test CA.
	testCA := cc.CA
	if cc.CA == LetsEncrypt {
		testCA = LetsEncryptStaging
	}
	issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA:                      cc.CA,
		TestCA:                  testCA,
		Email:                   cc.Email,
		Agreed:                  true,
		TrustedRoots:            cc.CARoots,
		DisableHTTPChallenge:    true,
		DisableTLSALPNChallenge: true,
		DNS01Solver: &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider: cc.DNS,
			Resolvers:   cc.Resolvers,
			Logger:      logger,
		}},
		Logger: logger,
	})
	cfg.Issuers = []certmagic.Issuer{issuer}
	if err := cfg.ManageAsync(ctx, cc.Names); err != nil {
		cache.Stop()
		return nil, fmt.Errorf("certificates for %v: %w", cc.Names, err)
	}
	go func() {
		<-ctx.Done()
		cache.Stop()
	}()
	cc.Log.Info("keeping certificates", "names", cc.Names, "ca", cc.CA, "storage", cc.Storage)
	return &tls.Config{GetCertificate: cfg.GetCertificate}, nil
}
