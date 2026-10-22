package apiserver

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"connectrpc.com/connect"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// customDomainCheck serves CustomDomainCheckService: a domain's challenge
// CNAME looked for now, as the controller looks every minute
// (docs/decisions/custom-domains.md).
type customDomainCheck struct{ s *Server }

func (h customDomainCheck) CheckCustomDomain(ctx context.Context, req *connect.Request[networkv1alpha1.CheckCustomDomainRequest]) (*connect.Response[networkv1alpha1.CheckCustomDomainResponse], error) {
	b := h.s.backend(registry.CustomDomain)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	key := resource.Key{Name: req.Msg.GetName()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	// As changing the domain: a check writes its status, and asks the
	// domain's nameservers on the caller's behalf. read refuses one the
	// caller may not, missing or not, alike.
	if _, _, err := b.read(ctx, p, authz.Apply, key); err != nil {
		return nil, err
	}
	// Each check asks nameservers the tenant names, so they are few: one a
	// domain at a time, and a handful at once in all.
	release, wait := h.s.domainChecks.take(key.Name, h.s.clock.Now(), h.s.cfg.PublicEndpoints.CheckInterval)
	if release == nil {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("%s was checked a moment ago, or many domains are being checked; try again in %ds", b.asked(key), int(math.Ceil(wait.Seconds()))))
	}
	defer release()
	d, err := h.s.checkCustomDomain(ctx, key.Name)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", b.asked(key)))
	}
	b.answer(p, d)
	return connect.NewResponse(&networkv1alpha1.CheckCustomDomainResponse{CustomDomain: d}), nil
}

// The bounds of CheckCustomDomain: how soon a domain may be checked again
// by default, how many checks run at once, and how many domains' last
// checks are kept before those past the interval are forgotten.
const (
	domainCheckInterval    = 5 * time.Second
	domainChecksAtOnce     = 4
	domainChecksRemembered = 1024
)

// checkLimiter keeps when each domain was last checked and how many checks
// are running. The zero value is ready.
type checkLimiter struct {
	mu      sync.Mutex
	running int
	last    map[string]time.Time
}

// take starts a check of name at now, if one may start; it answers how to
// end it, or nil and how long to wait.
func (l *checkLimiter) take(name string, now time.Time, interval time.Duration) (release func(), wait time.Duration) {
	if interval == 0 {
		interval = domainCheckInterval
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[name]; ok && now.Sub(at) < interval {
		return nil, interval - now.Sub(at)
	}
	if l.running >= domainChecksAtOnce {
		return nil, interval
	}
	if l.last == nil || len(l.last) >= domainChecksRemembered {
		for n, at := range l.last {
			if now.Sub(at) >= interval {
				delete(l.last, n)
			}
		}
		if l.last == nil {
			l.last = map[string]time.Time{}
		}
	}
	l.last[name] = now
	l.running++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.running--
	}, 0
}
