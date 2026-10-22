package apiserver

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/customdomain"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// CustomDomain and PublicEndpoint condition types and reasons for custom
// domains (docs/decisions/custom-domains.md).
const (
	// CustomDomainVerified says whether the domain's challenge CNAME names
	// its challenge target; once true it stays true.
	CustomDomainVerified = "Verified"
	// PublicEndpointHostnames says whether every hostname in an endpoint's
	// spec.hostnames serves: its CustomDomain is verified.
	PublicEndpointHostnames = "Hostnames"

	ReasonDomainVerified   = "Verified"
	ReasonChallengeMissing = "ChallengeRecordMissing"
	ReasonLookupFailed     = "LookupFailed"
	// ReasonHeldByAnotherTenant says another tenant verified the name, one
	// under it or above it, first.
	ReasonHeldByAnotherTenant = "HeldByAnotherTenant"
	ReasonHostnamesServe      = "Serving"
	ReasonDomainNotVerified   = "DomainNotVerified"
)

// The labels under the endpoint domain that custom domains' DNS names: the
// zone challenges are answered in, and the name their traffic is a CNAME
// of. Neither has a "-", so no endpoint's label is either.
const (
	challengeLabel = "acme"
	trafficLabel   = "cname"
)

// customDomainsLock is the writeLock of CustomDomains.
func customDomainsLock(s *Server) *sync.Mutex { return &s.customDomains }

// endpointHostnamesLock is the applyLock of a PublicEndpoint: customDomains,
// when it asks for hostnames under custom domains.
func endpointHostnamesLock(s *Server, obj proto.Message) []*sync.Mutex {
	if len(obj.(*networkv1alpha1.PublicEndpoint).GetSpec().GetHostnames()) == 0 {
		return nil
	}
	return []*sync.Mutex{&s.customDomains}
}

// ChallengeZone is the zone under the endpoint domain where CustomDomains'
// DNS-01 challenges are answered, which hangar-edge's DNS provider writes.
func ChallengeZone(endpointDomain string) string { return challengeLabel + "." + endpointDomain }

// customDomainTarget is where actions on a CustomDomain apply: its tenant.
func customDomainTarget(_ *Server, _ resource.Key, obj proto.Message, _ bool) authz.Target {
	d, _ := obj.(*networkv1alpha1.CustomDomain)
	return authz.Target{Tenant: d.GetSpec().GetTenant()}
}

func customDomainOwner(_ context.Context, _ *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
	return registry.Tenant, resource.Key{Name: obj.(*networkv1alpha1.CustomDomain).GetSpec().GetTenant()}, true
}

// allCustomDomains lists every CustomDomain.
func (s *Server) allCustomDomains(ctx context.Context) ([]*networkv1alpha1.CustomDomain, error) {
	objs, err := s.listWith(ctx, registry.CustomDomain, "", nil)
	if err != nil {
		return nil, err
	}
	out := make([]*networkv1alpha1.CustomDomain, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(*networkv1alpha1.CustomDomain))
	}
	return out, nil
}

// reservedSites are the sites no CustomDomain may be in: the platform's
// own, the endpoints' and the control plane's.
func (s *Server) reservedSites() []string {
	var out []string
	for _, d := range []string{s.endpointDomain(), s.cfg.PublicEndpoints.ControlPlaneDomain} {
		if d == "" {
			continue
		}
		if site, err := customdomain.Site(d); err == nil {
			out = append(out, site)
		}
	}
	return out
}

// isVerified reports whether a CustomDomain's challenge CNAME was found.
func isVerified(d *networkv1alpha1.CustomDomain) bool {
	return conditions.Find(d.GetStatus().GetConditions(), CustomDomainVerified).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
}

func domainView(d *networkv1alpha1.CustomDomain) customdomain.Domain {
	spec := d.GetSpec()
	return customdomain.Domain{Name: d.GetMetadata().GetName(), Domain: spec.GetDomain(), Tenant: spec.GetTenant(),
		Projects: spec.GetProjects(), Verified: isVerified(d)}
}

// domainViews are all as customdomain reads them, with d, if set, in place
// of the one stored under its name.
func domainViews(all []*networkv1alpha1.CustomDomain, d *networkv1alpha1.CustomDomain) []customdomain.Domain {
	out := make([]customdomain.Domain, 0, len(all)+1)
	for _, o := range all {
		if d == nil || o.GetMetadata().GetName() != d.GetMetadata().GetName() {
			out = append(out, domainView(o))
		}
	}
	if d != nil {
		out = append(out, domainView(d))
	}
	return out
}

// verifiedOverlap is a verified CustomDomain of all of another tenant than
// tenant's that is domain, under it or above it: what keeps the tenant from
// it, verified first.
func verifiedOverlap(all []*networkv1alpha1.CustomDomain, tenant, domain string) *networkv1alpha1.CustomDomain {
	for _, o := range all {
		if o.GetSpec().GetTenant() != tenant && isVerified(o) && customdomain.Overlap(o.GetSpec().GetDomain(), domain) {
			return o
		}
	}
	return nil
}

// validateCustomDomain checks a CustomDomain: its tenant lives, its domain
// is one a tenant may hold, the tenant has it no other time, and no other
// tenant has verified it, one under it or above it; one not verified holds
// nothing, so no one can keep a name from its owner without proving it
// theirs. The projects it names are the tenant's, and none of the tenant's
// endpoints serving at a hostname it takes is of a project it leaves out.
// The caller holds customDomains.
func validateCustomDomain(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	d := obj.(*networkv1alpha1.CustomDomain)
	spec := d.GetSpec()
	if spec.GetTenant() == "" {
		return invalid("spec.tenant is required")
	}
	if err := s.requireLive(ctx, registry.Tenant, "", spec.GetTenant()); err != nil {
		return err
	}
	if err := customdomain.CheckDomain(spec.GetDomain()); err != nil {
		return invalid("spec.domain: %v", err)
	}
	site, _ := customdomain.Site(spec.GetDomain())
	if slices.Contains(s.reservedSites(), site) {
		return invalid("spec.domain %q is in %s, which the platform keeps", spec.GetDomain(), site)
	}
	all, err := s.allCustomDomains(ctx)
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.GetMetadata().GetName() != d.GetMetadata().GetName() && other.GetSpec().GetTenant() == spec.GetTenant() && other.GetSpec().GetDomain() == spec.GetDomain() {
			return failedPrecondition("tenant %q has domain %q already, as custom domain %q", spec.GetTenant(), spec.GetDomain(), other.GetMetadata().GetName())
		}
	}
	// One made since another tenant verified the name could never be
	// verified itself (reconcileCustomDomain); one made before stays the
	// tenant's to change or delete.
	if o := verifiedOverlap(all, spec.GetTenant(), spec.GetDomain()); o != nil && old == nil {
		return failedPrecondition("domain %q is, or overlaps, %q, which another tenant has verified", spec.GetDomain(), o.GetSpec().GetDomain())
	}
	seen := map[string]bool{}
	for _, p := range spec.GetProjects() {
		if err := ValidName(p); err != nil {
			return invalid("spec.projects: %v", err)
		}
		if seen[p] {
			return invalid("spec.projects names %q twice", p)
		}
		seen[p] = true
		proj, err := s.get(ctx, registry.Project, resource.Key{Name: p})
		if err != nil {
			return err
		}
		if proj == nil || proj.(*tenancyv1alpha1.Project).GetSpec().GetTenant() != spec.GetTenant() {
			return failedPrecondition("spec.projects: project %q is not tenant %q's", p, spec.GetTenant())
		}
	}
	// On create too: a domain under one the tenant has takes the hostnames
	// under it.
	users, err := s.endpointsServingUnder(ctx, domainView(d), domainViews(all, d))
	if err != nil {
		return err
	}
	for _, u := range users {
		if !domainView(d).Allows(u.GetMetadata().GetProject()) {
			return failedPrecondition("endpoint %q of project %q serves at a hostname under %q; take the hostname out of its spec.hostnames, or let the project use the domain",
				u.GetMetadata().GetName(), u.GetMetadata().GetProject(), spec.GetDomain())
		}
	}
	return nil
}

// refuseDeletingDomainInUse keeps a CustomDomain an endpoint serves under,
// unless its tenant is going, which takes the endpoints too.
func refuseDeletingDomainInUse(ctx context.Context, s *Server, obj proto.Message) error {
	d := obj.(*networkv1alpha1.CustomDomain)
	t, err := s.get(ctx, registry.Tenant, resource.Key{Name: d.GetSpec().GetTenant()})
	if err != nil {
		return err
	}
	if t == nil || t.(*tenancyv1alpha1.Tenant).GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	all, err := s.allCustomDomains(ctx)
	if err != nil {
		return err
	}
	users, err := s.endpointsServingUnder(ctx, domainView(d), domainViews(all, nil))
	if err != nil || len(users) == 0 {
		return err
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.GetMetadata().GetProject()+"/"+u.GetMetadata().GetName())
	}
	return failedPrecondition("custom domain %q has endpoints serving under it: %s; take its hostnames out of their spec.hostnames first",
		d.GetMetadata().GetName(), strings.Join(names, ", "))
}

// projectTenants maps every project to its tenant.
func (s *Server) projectTenants(ctx context.Context) (map[string]string, error) {
	projects, err := s.listWith(ctx, registry.Project, "", nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(projects))
	for _, p := range projects {
		p := p.(*tenancyv1alpha1.Project)
		out[p.GetMetadata().GetName()] = p.GetSpec().GetTenant()
	}
	return out, nil
}

// endpointsServingUnder are the endpoints of d's tenant with a hostname
// that, of views, is under d.
func (s *Server) endpointsServingUnder(ctx context.Context, d customdomain.Domain, views []customdomain.Domain) ([]*networkv1alpha1.PublicEndpoint, error) {
	eps, err := s.endpoints(ctx, "")
	if err != nil {
		return nil, err
	}
	tenants, err := s.projectTenants(ctx)
	if err != nil {
		return nil, err
	}
	var out []*networkv1alpha1.PublicEndpoint
	for _, ep := range eps {
		tenant := tenants[ep.GetMetadata().GetProject()]
		if tenant != d.Tenant {
			continue
		}
		for _, h := range ep.GetSpec().GetHostnames() {
			if m, ok := customdomain.Of(h, tenant, views); ok && m.Name == d.Name {
				out = append(out, ep)
				break
			}
		}
	}
	return out, nil
}

// checkEndpointHostnames checks an endpoint's spec.hostnames: each under a
// CustomDomain of the project's tenant that lets the project use it, held
// by no other endpoint, and only on a PUBLIC endpoint. The caller holds
// customDomains.
func (s *Server) checkEndpointHostnames(ctx context.Context, ep *networkv1alpha1.PublicEndpoint) error {
	hostnames := ep.GetSpec().GetHostnames()
	if len(hostnames) == 0 {
		return nil
	}
	if !IsPublic(ep) {
		return invalid("spec.hostnames: only a PUBLIC endpoint serves under a custom domain")
	}
	project := ep.GetMetadata().GetProject()
	proj, err := s.get(ctx, registry.Project, resource.Key{Name: project})
	if err != nil {
		return err
	}
	if proj == nil {
		return failedPrecondition("project %q does not exist", project)
	}
	tenant := proj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	all, err := s.allCustomDomains(ctx)
	if err != nil {
		return err
	}
	views := domainViews(all, nil)
	deleting := map[string]bool{}
	for _, d := range all {
		deleting[d.GetMetadata().GetName()] = d.GetMetadata().GetDeleteTime() != nil
	}
	eps, err := s.endpoints(ctx, "")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, h := range hostnames {
		if err := customdomain.CheckHostname(h); err != nil {
			return invalid("spec.hostnames: %v", err)
		}
		if seen[h] {
			return invalid("spec.hostnames names %q twice", h)
		}
		seen[h] = true
		d, ok := customdomain.Of(h, tenant, views)
		if !ok {
			return failedPrecondition("spec.hostnames: %q is under no custom domain of tenant %q; a hostname is a custom domain or one label below one", h, tenant)
		}
		if deleting[d.Name] {
			return failedPrecondition("spec.hostnames: custom domain %q is being deleted", d.Name)
		}
		if !d.Allows(project) {
			return failedPrecondition("spec.hostnames: custom domain %q does not let project %q use it; its spec.projects names the projects that may", d.Name, project)
		}
		for _, other := range eps {
			if other.GetMetadata().GetProject() == project && other.GetMetadata().GetName() == ep.GetMetadata().GetName() {
				continue
			}
			if slices.Contains(other.GetSpec().GetHostnames(), h) {
				return failedPrecondition("spec.hostnames: %q is held by another endpoint", h)
			}
		}
	}
	return nil
}

// reconcileCustomDomain writes what a CustomDomain's DNS needs, and
// whether its challenge CNAME is in place. The first tenant to verify a
// name has it: another tenant's domain that is it, under it or above it is
// never verified after. One verified stays so: a domain that later drops
// the record loses its certificates' renewal, not its endpoints.
func (s *Server) reconcileCustomDomain(ctx context.Context, k controller.Key) error {
	key := resource.Key{Name: k.Name}
	obj, err := s.get(ctx, registry.CustomDomain, key)
	if err != nil || obj == nil {
		return err
	}
	d := obj.(*networkv1alpha1.CustomDomain)
	if d.GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	var found string
	var lookupErr error
	if !isVerified(d) {
		// Asked outside the lock, which DNS could otherwise hold for seconds.
		found, lookupErr = s.lookupCNAME(ctx, customdomain.ChallengeName(d.GetSpec().GetDomain()))
	}
	// Whether another tenant verified the name first, and this status
	// written, under the lock every write of a domain holds, so two tenants
	// never both are.
	s.customDomains.Lock()
	defer s.customDomains.Unlock()
	if obj, err = s.get(ctx, registry.CustomDomain, key); err != nil || obj == nil {
		return err
	}
	d = obj.(*networkv1alpha1.CustomDomain)
	if d.GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	gen := d.GetMetadata().GetGeneration()
	status := &networkv1alpha1.CustomDomainStatus{}
	if d.GetStatus() != nil {
		status = proto.Clone(d.GetStatus()).(*networkv1alpha1.CustomDomainStatus)
	}
	domain := d.GetSpec().GetDomain()
	status.ObservedGeneration = gen
	status.ChallengeTarget = customdomain.ChallengeTarget(d.GetMetadata().GetUid(), ChallengeZone(s.endpointDomain()))
	traffic := trafficLabel + "." + s.endpointDomain()
	status.Records = []*networkv1alpha1.CustomDomainRecord{
		{Name: customdomain.ChallengeName(domain), Type: "CNAME", Value: status.ChallengeTarget,
			Purpose: "proves the domain is the tenant's, and lets Hangar get its certificates; keep it"},
		{Name: "*." + domain, Type: "CNAME", Value: traffic,
			Purpose: "brings the traffic of hostnames one label below the domain to Hangar; or a CNAME for each hostname"},
		{Name: domain, Type: "ALIAS", Value: traffic,
			Purpose: "brings the traffic of the domain itself to Hangar: an ALIAS, ANAME or flattened CNAME, or the A and AAAA records of " + traffic},
	}
	if !isVerified(d) {
		cond := &metav1.Condition{Type: CustomDomainVerified, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, ObservedGeneration: gen}
		all, err := s.allCustomDomains(ctx)
		if err != nil {
			return err
		}
		switch o := verifiedOverlap(all, d.GetSpec().GetTenant(), domain); {
		case o != nil:
			cond.Reason = ReasonHeldByAnotherTenant
			cond.Message = fmt.Sprintf("another tenant has verified %s first; it is theirs while their custom domain is", o.GetSpec().GetDomain())
		case lookupErr != nil:
			cond.Reason, cond.Message = ReasonLookupFailed, fmt.Sprintf("looking up %s failed: %v", customdomain.ChallengeName(domain), lookupErr)
		case found != status.ChallengeTarget:
			cond.Reason = ReasonChallengeMissing
			cond.Message = fmt.Sprintf("%s must be a CNAME of %s", customdomain.ChallengeName(domain), status.ChallengeTarget)
			if found != "" {
				cond.Message += ", not " + found
			}
		default:
			cond.Status, cond.Reason = metav1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonDomainVerified
			cond.Message = "the challenge CNAME is in place"
		}
		status.Conditions = conditions.Set(status.GetConditions(), cond, s.clock.Now())
	}
	if !proto.Equal(status, d.GetStatus()) {
		rv := d.GetMetadata().GetResourceVersion()
		if _, err := s.backend(registry.CustomDomain).UpdateStatus(s.system(ctx), key, status, &rv); err != nil {
			return err
		}
	}
	return nil
}

// lookupTimeout bounds one lookup of a challenge CNAME.
const lookupTimeout = 5 * time.Second

// lookupCNAME is the target of name's CNAME, without the trailing dot, or
// "" when it has none. It asks the configured resolvers, else the
// system's, for the CNAME itself: following it to addresses would fail,
// since a challenge target has records only while a challenge is answered.
func (s *Server) lookupCNAME(ctx context.Context, name string) (string, error) {
	servers := s.cfg.PublicEndpoints.Resolvers
	if len(servers) == 0 {
		cc, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			return "", err
		}
		for _, srv := range cc.Servers {
			servers = append(servers, net.JoinHostPort(srv, cc.Port))
		}
	}
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeCNAME)
	c := &dns.Client{Timeout: lookupTimeout}
	var lastErr error
	for _, srv := range servers {
		in, _, err := c.ExchangeContext(ctx, q, srv)
		if err != nil {
			lastErr = err
			continue
		}
		if in.Rcode != dns.RcodeSuccess && in.Rcode != dns.RcodeNameError {
			lastErr = fmt.Errorf("%s answered %s", srv, dns.RcodeToString[in.Rcode])
			continue
		}
		for _, rr := range in.Answer {
			if cname, ok := rr.(*dns.CNAME); ok && strings.EqualFold(cname.Hdr.Name, dns.Fqdn(name)) {
				return strings.ToLower(strings.TrimSuffix(cname.Target, ".")), nil
			}
		}
		return "", nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no DNS server to ask")
	}
	return "", lastErr
}

// customDomainController keeps CustomDomains' status. Its resync, every
// minute, looks again for the challenge CNAME of those not yet verified.
func (s *Server) customDomainController() *controller.Controller {
	return &controller.Controller{
		Name: "customdomain", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
		Reconciler: controller.ReconcilerFunc(s.reconcileCustomDomain),
		Resync:     time.Minute,
		Watches:    []controller.Watch{{Kind: registry.CustomDomain.Kind, Map: controller.Self}},
	}
}

// endpointsUnderDomain maps a CustomDomain to the endpoints with a
// hostname it serves, whose Hostnames condition it decides.
func (s *Server) endpointsUnderDomain(ctx context.Context, o store.Object) []controller.Key {
	var d networkv1alpha1.CustomDomain
	if proto.Unmarshal(o.Data, &d) != nil {
		return nil
	}
	eps, err := s.endpoints(ctx, "")
	if err != nil {
		s.log.Error("listing public endpoints failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, ep := range eps {
		if slices.ContainsFunc(ep.GetSpec().GetHostnames(), func(h string) bool { return customdomain.Serves(d.GetSpec().GetDomain(), h) }) {
			keys = append(keys, controller.Key{Project: ep.GetMetadata().GetProject(), Name: ep.GetMetadata().GetName()})
		}
	}
	return keys
}

// endpointHostnamesStatus is what an endpoint's status says of its
// spec.hostnames: the URLs of those that serve, and the Hostnames
// condition, nil when it has none.
func (s *Server) endpointHostnamesStatus(ctx context.Context, ep *networkv1alpha1.PublicEndpoint) ([]string, *metav1.Condition, error) {
	hostnames := ep.GetSpec().GetHostnames()
	if len(hostnames) == 0 {
		return nil, nil, nil
	}
	all, err := s.allCustomDomains(ctx)
	if err != nil {
		return nil, nil, err
	}
	project := ep.GetMetadata().GetProject()
	proj, err := s.get(ctx, registry.Project, resource.Key{Name: project})
	if err != nil {
		return nil, nil, err
	}
	tenant := ""
	if proj != nil {
		tenant = proj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	}
	views := domainViews(all, nil)
	var urls, waiting []string
	for _, h := range hostnames {
		// As hangar-edge decides it (internal/edge).
		if d, ok := customdomain.Of(h, tenant, views); ok && d.Verified && d.Allows(project) {
			urls = append(urls, "https://"+h)
			continue
		}
		waiting = append(waiting, h)
	}
	cond := &metav1.Condition{Type: PublicEndpointHostnames, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason: ReasonHostnamesServe, ObservedGeneration: ep.GetMetadata().GetGeneration()}
	if len(waiting) > 0 {
		cond.Status, cond.Reason = metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonDomainNotVerified
		cond.Message = "these hostnames wait for their custom domain to be verified: " + strings.Join(waiting, ", ")
	}
	return urls, cond, nil
}
