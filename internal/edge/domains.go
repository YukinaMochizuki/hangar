package edge

import (
	"context"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/customdomain"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// verifiedCondition is the CustomDomain condition the apiserver sets once
// the domain's challenge CNAME is in place.
const verifiedCondition = "Verified"

// runDomains follows the custom domains until ctx is done.
func (e *Edge) runDomains(ctx context.Context) {
	for ctx.Err() == nil {
		if err := e.followDomains(ctx); err != nil && ctx.Err() == nil {
			e.cfg.Log.Error("following custom domains failed; reading them again", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(e.cfg.Retry):
			}
		}
	}
}

// followDomains lists the custom domains and follows their changes until
// the watch ends.
func (e *Edge) followDomains(ctx context.Context) error {
	src := controller.APISource{Client: e.cfg.API}
	objs, rev, err := src.List(ctx, registry.CustomDomain.Kind)
	if err != nil {
		return err
	}
	// By uid, so an event names what it changes; the view is rebuilt from it.
	byUID := map[string]liveDomain{}
	for _, o := range objs {
		if d, ok := decodeDomain(o); ok {
			byUID[d.uid] = d
		}
	}
	e.setDomains(byUID)
	return src.Watch(ctx, []string{registry.CustomDomain.Kind}, rev, func(ev store.Event) error {
		if ev.Type == store.EventProgress {
			return nil
		}
		var d networkv1alpha1.CustomDomain
		_ = proto.Unmarshal(ev.Object.Data, &d)
		uid := d.GetMetadata().GetUid()
		if uid == "" {
			return nil // not a domain the edge can read
		}
		delete(byUID, uid)
		if v, ok := decodeDomain(ev.Object); ok && ev.Type != store.EventDeleted {
			byUID[v.uid] = v
		}
		e.setDomains(byUID)
		return nil
	})
}

// liveDomain is a CustomDomain not being deleted, as the edge needs it.
type liveDomain struct {
	uid       string
	view      customdomain.Domain
	challenge string
}

// decodeDomain reads a stored CustomDomain, which counts while it is not
// being deleted. Only a verified one serves, as customdomain.Of has it,
// but an unverified one of a tenant still takes the hostnames under it
// from a verified one above it, as the apiserver's admission does.
func decodeDomain(o store.Object) (liveDomain, bool) {
	var d networkv1alpha1.CustomDomain
	if proto.Unmarshal(o.Data, &d) != nil || d.GetMetadata().GetDeleteTime() != nil {
		return liveDomain{}, false
	}
	verified := slices.ContainsFunc(d.GetStatus().GetConditions(), func(c *metav1.Condition) bool {
		return c.GetType() == verifiedCondition && c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
	})
	spec := d.GetSpec()
	return liveDomain{
		uid: d.GetMetadata().GetUid(),
		view: customdomain.Domain{Name: d.GetMetadata().GetName(), Domain: Hostname(spec.GetDomain()), Tenant: spec.GetTenant(),
			Projects: slices.Clone(spec.GetProjects()), Verified: verified && d.GetStatus().GetChallengeTarget() != ""},
		challenge: d.GetStatus().GetChallengeTarget(),
	}, true
}

// setDomains makes the domains exactly those of byUID, ends the requests
// to custom hostnames that no longer serve, and signals DomainChanges if
// the verified ones changed.
func (e *Edge) setDomains(byUID map[string]liveDomain) {
	views := make([]customdomain.Domain, 0, len(byUID))
	verified := map[string]string{}
	for _, d := range byUID {
		views = append(views, d.view)
		if d.view.Verified {
			verified[d.view.Domain] = d.challenge
		}
	}
	e.mu.Lock()
	e.domains = views
	changed := !maps.Equal(verified, e.verified)
	e.verified = verified
	for r := range e.active {
		if r.custom && !e.servesHostLocked(r.host) {
			r.cancel()
		}
	}
	e.mu.Unlock()
	if !changed {
		return
	}
	select {
	case e.domainChanges <- struct{}{}:
	default:
	}
}

// VerifiedDomains are the verified custom domains now, the challenge target
// of each by its domain.
func (e *Edge) VerifiedDomains() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return maps.Clone(e.verified)
}

// DomainChanges takes a signal, coalesced, whenever VerifiedDomains
// changes.
func (e *Edge) DomainChanges() <-chan struct{} { return e.domainChanges }
