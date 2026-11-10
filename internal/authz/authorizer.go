package authz

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Target is what an action applies to: a project, a tenant, or (both empty)
// the platform. A project is also covered by its tenant's bindings; the
// tenant comes from the view, or from Tenant when the caller knows it from
// the project itself, which the view no longer holds once the project is
// deleted.
type Target struct {
	Tenant  string
	Project string
}

// grant is one RoleBinding as the authorizer uses it.
type grant struct {
	// name is the role's, for saying which a caller holds; checks use role.
	name    string
	role    Role
	tenant  string
	project string
	// platform is set for platform-scoped bindings.
	platform bool
}

type subject struct {
	typ auth.Type
	id  string
}

// Authorizer answers authorization questions from an in-memory view of
// RoleBindings, Projects and Users, kept current by watching the store. The
// view also keeps what it held over its recent revisions (RetainHistory,
// AllowedAt).
type Authorizer struct {
	store store.Store
	// source, when store is nil, is what the view follows instead: the
	// API, from outside the apiserver (NewFromSource).
	source Source
	// synced is closed once a view following source has been loaded the
	// first time.
	synced     chan struct{}
	syncedOnce sync.Once
	// changes signals, without blocking, each change to a view following a
	// source.
	changes chan struct{}
	// extra are roles beyond Roles, which never replace one of them.
	extra map[string]Role

	mu sync.RWMutex
	// bindings maps each binding's key to its subject and grant as they
	// stand, so updates and deletes can find them in grants.
	bindings map[string]bindingEntry
	// grants holds each subject's grants by binding name.
	grants map[subject]history[string, grant]
	// tenantOf maps project names to their tenant.
	tenantOf history[string, string]
	// usernames maps each signed-up person's email to their User's name, and
	// emails is its inverse, so a deleted User can be dropped by name.
	usernames history[string, string]
	emails    history[string, string]
	// tombstones are the values that no longer hold, in the order they
	// stopped.
	tombstones []tombstone
	// historyFrom is the earliest revision the history answers for;
	// AllowedAt answers for none before it.
	historyFrom int64
	// retention is how many revisions back the history reaches.
	retention int64
	// rev is the store revision the view reflects.
	rev int64
	// advanced is closed and replaced whenever rev moves.
	advanced chan struct{}
}

type bindingEntry struct {
	subject subject
	grant   grant
}

// New returns an authorizer over st. Call Run before using it.
func New(st store.Store, extra map[string]Role) *Authorizer {
	a := &Authorizer{
		store:    st,
		extra:    maps.Clone(extra),
		advanced: make(chan struct{}),
		synced:   make(chan struct{}),
		changes:  make(chan struct{}, 1),
	}
	a.reset(0)
	return a
}

// reset empties the view, whose history then starts at from. Callers hold
// a.mu, or own a.
func (a *Authorizer) reset(from int64) {
	a.bindings = map[string]bindingEntry{}
	a.grants = map[subject]history[string, grant]{}
	a.tenantOf = history[string, string]{}
	a.usernames = history[string, string]{}
	a.emails = history[string, string]{}
	a.tombstones = nil
	a.historyFrom = from
}

// Source is what an authorizer outside the apiserver follows: the API, as
// hangar-edge reads it. controller.APISource is one. Objects come encoded as
// the store holds them.
type Source interface {
	// List returns every object of kind, and a revision from which a watch
	// misses no change made after the list.
	List(ctx context.Context, kind string) ([]store.Object, int64, error)
	// Watch calls fn for each change to an object of one of kinds after
	// since, and with a store.EventProgress when it chooses to; it returns
	// when the watch ends, nil if the source ended it.
	Watch(ctx context.Context, kinds []string, since int64, fn func(store.Event) error) error
}

// NewFromSource returns an authorizer whose view follows src rather than a
// store. It answers Allowed and IsMember from what it has seen so far;
// Fresh and WaitFor are the apiserver's alone. Call Run before using it,
// and wait for Synced before trusting a refusal.
func NewFromSource(src Source, extra map[string]Role) *Authorizer {
	a := New(nil, extra)
	a.source = src
	return a
}

// Synced is closed once a view following a source has been loaded the
// first time.
func (a *Authorizer) Synced() <-chan struct{} { return a.synced }

func (a *Authorizer) markSynced() { a.syncedOnce.Do(func() { close(a.synced) }) }

// Changes delivers once after any change to a view following a source,
// however many changes there were since it was last read: a caller holding
// grants given earlier checks them again.
func (a *Authorizer) Changes() <-chan struct{} { return a.changes }

func (a *Authorizer) changed() {
	select {
	case a.changes <- struct{}{}:
	default:
	}
}

// sourceRetry is how long an authorizer following a source waits before it
// lists again after a failure, such as the apiserver restarting.
var sourceRetry = time.Second

// Run loads the current bindings, projects and users and follows every write
// until ctx is done.
func (a *Authorizer) Run(ctx context.Context) error {
	if a.store == nil {
		return a.runSource(ctx)
	}
	for {
		err := a.sync(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// History the watch needed was compacted while it was behind; start
		// over from a fresh list.
		if !errors.Is(err, store.ErrCompacted) {
			return err
		}
	}
}

func (a *Authorizer) sync(ctx context.Context) error {
	bindings, rev, err := a.store.List(ctx, store.ListOptions{Kind: registry.RoleBinding.Kind})
	if err != nil {
		return err
	}
	projects, prev, err := a.store.List(ctx, store.ListOptions{Kind: registry.Project.Kind})
	if err != nil {
		return err
	}
	users, urev, err := a.store.List(ctx, store.ListOptions{Kind: registry.User.Kind})
	if err != nil {
		return err
	}
	a.mu.Lock()
	// The three lists were taken at different revisions, so the view is
	// exact only from the latest of them on, and its history starts there.
	a.reset(max(rev, prev, urev))
	a.load(bindings, projects, users, rev)
	a.advance(rev)
	a.mu.Unlock()
	// Projects and users were listed at or after rev; replaying from rev
	// reapplies full-state events, which converges to the same view.
	return a.store.Watch(ctx, store.WatchOptions{SinceRevision: rev}, func(ev store.Event) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.apply(ev)
		a.advance(ev.Revision)
		a.prune()
		return nil
	})
}

// load puts listed objects into an empty view, as holding from rev.
// Callers hold a.mu.
func (a *Authorizer) load(bindings, projects, users []store.Object, rev int64) {
	for _, o := range bindings {
		a.putBinding(o, rev)
	}
	for _, o := range projects {
		a.putProject(o, rev)
	}
	for _, o := range users {
		a.putUser(o, rev)
	}
}

// runSource follows the source until ctx is done, listing again whenever a
// watch ends or fails: an outside caller outlives apiserver restarts.
func (a *Authorizer) runSource(ctx context.Context) error {
	for {
		err := a.syncSource(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, store.ErrCompacted) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sourceRetry):
			}
		}
	}
}

// syncSource loads the view from the source and follows it. Each kind is
// listed on its own, so the watch starts from the earlier revision;
// replaying full-state events converges to the same view.
func (a *Authorizer) syncSource(ctx context.Context) error {
	bindings, brev, err := a.source.List(ctx, registry.RoleBinding.Kind)
	if err != nil {
		return err
	}
	projects, prev, err := a.source.List(ctx, registry.Project.Kind)
	if err != nil {
		return err
	}
	users, urev, err := a.source.List(ctx, registry.User.Kind)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.reset(0)
	a.load(bindings, projects, users, 0)
	a.mu.Unlock()
	a.markSynced()
	a.changed()
	kinds := []string{registry.RoleBinding.Kind, registry.Project.Kind, registry.User.Kind}
	return a.source.Watch(ctx, kinds, min(brev, prev, urev), func(ev store.Event) error {
		if ev.Type == store.EventProgress {
			return nil
		}
		a.mu.Lock()
		a.apply(ev)
		// No watch is judged here: it answers by the view as it stands.
		a.pruneBefore(Now)
		a.mu.Unlock()
		a.changed()
		return nil
	})
}

// apply changes the view by one event. Callers hold a.mu.
func (a *Authorizer) apply(ev store.Event) {
	rev := ev.Revision
	switch ev.Object.Kind {
	case registry.RoleBinding.Kind:
		if ev.Type == store.EventDeleted {
			a.dropBinding(ev.Object.Name, rev)
		} else {
			a.putBinding(ev.Object, rev)
		}
	case registry.Project.Kind:
		if ev.Type == store.EventDeleted {
			a.unset(a.tenantOf, ev.Object.Name, rev)
		} else {
			a.putProject(ev.Object, rev)
		}
	case registry.User.Kind:
		if ev.Type == store.EventDeleted {
			a.dropUser(ev.Object.Name, rev)
		} else {
			a.putUser(ev.Object, rev)
		}
	}
}

// set makes v the value of k in h from rev on. Callers hold a.mu.
func (a *Authorizer) set(h history[string, string], k, v string, rev int64) {
	if cur, ok := h.at(k, Now); ok && cur == v {
		return
	}
	a.unset(h, k, rev)
	h.put(k, v, rev)
}

// unset ends the value of k in h at rev. Callers hold a.mu.
func (a *Authorizer) unset(h history[string, string], k string, rev int64) {
	if h.close(k, rev) {
		a.closed(rev, func(before int64) { h.prune(k, before) })
	}
}

func (a *Authorizer) putBinding(o store.Object, rev int64) {
	var rb iamv1alpha1.RoleBinding
	if err := proto.Unmarshal(o.Data, &rb); err != nil {
		return
	}
	e, ok := a.entryOf(&rb)
	if !ok {
		a.dropBinding(o.Name, rev)
		return
	}
	if cur, ok := a.bindings[o.Name]; ok && cur.same(e) {
		return
	}
	a.dropBinding(o.Name, rev)
	a.bindings[o.Name] = e
	g, ok := a.grants[e.subject]
	if !ok {
		g = history[string, grant]{}
		a.grants[e.subject] = g
	}
	g.put(o.Name, e.grant, rev)
}

// dropBinding ends the binding named name at rev. Callers hold a.mu.
func (a *Authorizer) dropBinding(name string, rev int64) {
	cur, ok := a.bindings[name]
	if !ok {
		return
	}
	delete(a.bindings, name)
	s := cur.subject
	g := a.grants[s]
	if g.close(name, rev) {
		a.closed(rev, func(before int64) {
			g := a.grants[s]
			g.prune(name, before)
			if len(g) == 0 {
				delete(a.grants, s)
			}
		})
	} else if len(g) == 0 {
		delete(a.grants, s)
	}
}

// same reports whether e grants what o does.
func (e bindingEntry) same(o bindingEntry) bool {
	return e.subject == o.subject && e.grant.name == o.grant.name && e.grant.tenant == o.grant.tenant &&
		e.grant.project == o.grant.project && e.grant.platform == o.grant.platform
}

// Role returns the role named name: a built-in one, or one of the
// authorizer's extra roles.
func (a *Authorizer) Role(name string) (Role, bool) {
	if r, ok := Roles[name]; ok {
		return r, true
	}
	r, ok := a.extra[name]
	return r, ok
}

func (a *Authorizer) entryOf(rb *iamv1alpha1.RoleBinding) (bindingEntry, bool) {
	spec := rb.GetSpec()
	role, ok := a.Role(spec.GetRole())
	if !ok {
		return bindingEntry{}, false
	}
	var e bindingEntry
	switch s := spec.GetSubject().GetSubject().(type) {
	case *iamv1alpha1.Subject_User:
		e.subject = subject{auth.User, s.User}
	case *iamv1alpha1.Subject_ServiceToken:
		e.subject = subject{auth.ServiceToken, s.ServiceToken}
	default:
		return bindingEntry{}, false
	}
	e.grant.name, e.grant.role = spec.GetRole(), role
	switch t := spec.GetScope().GetTarget().(type) {
	case *iamv1alpha1.BindingScope_Platform:
		e.grant.platform = true
	case *iamv1alpha1.BindingScope_Tenant:
		e.grant.tenant = t.Tenant
	case *iamv1alpha1.BindingScope_Project:
		e.grant.project = t.Project
	default:
		return bindingEntry{}, false
	}
	return e, true
}

func (a *Authorizer) putProject(o store.Object, rev int64) {
	var p tenancyv1alpha1.Project
	if err := proto.Unmarshal(o.Data, &p); err != nil {
		return
	}
	a.set(a.tenantOf, o.Name, p.GetSpec().GetTenant(), rev)
}

func (a *Authorizer) putUser(o store.Object, rev int64) {
	var u iamv1alpha1.User
	if err := proto.Unmarshal(o.Data, &u); err != nil {
		return
	}
	email := u.GetSpec().GetEmail()
	if cur, ok := a.emails.at(o.Name, Now); ok && cur == email {
		return
	}
	// A User's email never changes, but replace whatever the name had.
	a.dropUser(o.Name, rev)
	if email == "" {
		return
	}
	a.set(a.usernames, email, o.Name, rev)
	a.set(a.emails, o.Name, email, rev)
}

func (a *Authorizer) dropUser(name string, rev int64) {
	if email, ok := a.emails.at(name, Now); ok {
		a.unset(a.emails, name, rev)
		if n, _ := a.usernames.at(email, Now); n == name {
			a.unset(a.usernames, email, rev)
		}
	}
}

// grantsAt returns the grants s held at rev. Callers hold a.mu.
func (a *Authorizer) grantsAt(s subject, rev int64) iter.Seq[grant] {
	return func(yield func(grant) bool) {
		for name := range a.grants[s] {
			if g, ok := a.grants[s].at(name, rev); ok && !yield(g) {
				return
			}
		}
	}
}

// advance moves the view's revision forward. Callers hold a.mu.
func (a *Authorizer) advance(rev int64) {
	if rev <= a.rev {
		return
	}
	a.rev = rev
	close(a.advanced)
	a.advanced = make(chan struct{})
}

// Fresh waits until the view reflects every write committed before the call,
// so a request sees bindings, projects and users created by earlier requests.
func (a *Authorizer) Fresh(ctx context.Context) error {
	if a.store == nil {
		return errors.New("an authorizer following a source cannot tell what the store has committed")
	}
	want, err := a.store.Revision(ctx)
	if err != nil {
		return err
	}
	return a.WaitFor(ctx, want)
}

// WaitFor waits until the view reflects the store at revision want.
func (a *Authorizer) WaitFor(ctx context.Context, want int64) error {
	for {
		a.mu.RLock()
		rev, advanced := a.rev, a.advanced
		a.mu.RUnlock()
		if rev >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("authorization view behind at revision %d, want %d: %w", rev, want, ctx.Err())
		case <-advanced:
		}
	}
}

// EdgeReads are all hangar-edge may do, everywhere: read the public
// endpoints, the custom domains they serve under, and the bindings,
// projects and users that decide who may use them
// (docs/decisions/public-endpoints.md, docs/decisions/custom-domains.md).
var EdgeReads = func() []string {
	var out []string
	for _, r := range []registry.Resource{registry.PublicEndpoint, registry.CustomDomain, registry.RoleBinding, registry.Project, registry.User} {
		for _, v := range []Verb{Get, List, Watch} {
			out = append(out, Permission(r, v))
		}
	}
	return out
}()

// LocalRootRole is the role root on the host holds, through the admin socket.
const LocalRootRole = "platform.root"

// Allowed reports whether p holds the permission on the target.
func (a *Authorizer) Allowed(p auth.Principal, permission string, t Target) bool {
	allowed, _ := a.AllowedAt(p, permission, t, Now)
	return allowed
}

// AllowedAt is Allowed by the view as it stood at rev, for judging an event
// of that revision. ok is false, and allowed with it, when the view's
// history does not reach back to rev: it keeps the number of revisions
// RetainHistory sets, and none from before it last listed the store. At
// Now it answers as Allowed.
func (a *Authorizer) AllowedAt(p auth.Principal, permission string, t Target, rev int64) (allowed, ok bool) {
	return a.allowedBy(p, permission, rev, func(g grant, rev int64) bool { return a.covers(g, t, rev) })
}

// AllowedInProjectsOf reports whether p holds permission at some project of
// tenant, or at the tenant or the platform, which cover them all. It looks
// only at p's own grants, however many projects the tenant has.
func (a *Authorizer) AllowedInProjectsOf(p auth.Principal, permission, tenant string) bool {
	allowed, _ := a.AllowedInProjectsOfAt(p, permission, tenant, Now)
	return allowed
}

// AllowedInProjectsOfAt is AllowedInProjectsOf by the view as it stood at
// rev, as AllowedAt is Allowed.
func (a *Authorizer) AllowedInProjectsOfAt(p auth.Principal, permission, tenant string, rev int64) (allowed, ok bool) {
	return a.allowedBy(p, permission, rev, func(g grant, rev int64) bool {
		if a.covers(g, Target{Tenant: tenant}, rev) {
			return true
		}
		t, ok := a.tenantOf.at(g.project, rev)
		return g.project != "" && ok && t == tenant
	})
}

// allowedBy is AllowedAt with the grants that count chosen by covers, which
// runs under a.mu. Principals the view does not decide for are answered
// at any rev.
func (a *Authorizer) allowedBy(p auth.Principal, permission string, rev int64, covers func(g grant, rev int64) bool) (allowed, ok bool) {
	switch p.Type {
	case auth.System:
		return true, true
	case auth.Node:
		return slices.Contains([]Verb{Get, List, Watch, UpdateStatus}, verbOf(permission)), true
	case auth.Local:
		// Root on the host holds platform.root whatever the bindings say, so
		// it can mend them.
		return Roles[LocalRootRole].Grants(permission), true
	case auth.Edge:
		return slices.Contains(EdgeReads, permission), true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if rev < a.historyFrom {
		return false, false
	}
	// A person who is not a platform member may do nothing, whatever else
	// they have been granted; see docs/decisions/tenancy-and-iam.md.
	if p.Type == auth.User && !a.member(p, rev) {
		return false, true
	}
	for g := range a.grantsAt(subject{p.Type, p.ID}, rev) {
		if g.role.Grants(permission) && covers(g, rev) {
			return true, true
		}
	}
	return false, true
}

// TenantOf returns the tenant of a project in the view, if the view has the
// project.
func (a *Authorizer) TenantOf(project string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, ok := a.tenantOf.at(project, Now)
	return t, ok
}

// IsMember reports whether a person is a platform member: invited, and signed
// up. Other principals are not subject to membership and always report true.
func (a *Authorizer) IsMember(p auth.Principal) bool {
	if p.Type != auth.User {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.member(p, Now)
}

// IsInvited reports whether a person holds a platform role, which invites
// them to sign up, whether or not they have. It is false for every other
// principal.
func (a *Authorizer) IsInvited(p auth.Principal) bool {
	if p.Type != auth.User {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.invited(p, Now)
}

// PlatformRoles returns the names of the roles bound to p at the platform,
// sorted, each once. It is for saying who someone is, such as a sponsor's
// badge; whether p may do something is Allowed's, by permission.
func (a *Authorizer) PlatformRoles(p auth.Principal) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var names []string
	for g := range a.grantsAt(subject{p.Type, p.ID}, Now) {
		if g.platform {
			names = append(names, g.name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Username returns the name of a person's User, if they have signed up.
func (a *Authorizer) Username(p auth.Principal) (string, bool) {
	if p.Type != auth.User {
		return "", false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	name, ok := a.usernames.at(p.ID, Now)
	return name, ok
}

// member reports platform membership at rev: a platform role and a User.
// Callers hold a.mu.
func (a *Authorizer) member(p auth.Principal, rev int64) bool {
	_, signedUp := a.usernames.at(p.ID, rev)
	return signedUp && a.invited(p, rev)
}

// invited reports whether p held a platform role at rev. Callers hold a.mu.
func (a *Authorizer) invited(p auth.Principal, rev int64) bool {
	for g := range a.grantsAt(subject{p.Type, p.ID}, rev) {
		if g.platform && g.role.Member {
			return true
		}
	}
	return false
}

// covers reports whether g covered t at rev. Callers hold a.mu.
func (a *Authorizer) covers(g grant, t Target, rev int64) bool {
	switch {
	case g.platform:
		return true
	case g.tenant != "":
		if t.Tenant == g.tenant {
			return true
		}
		tenant, ok := a.tenantOf.at(t.Project, rev)
		return t.Project != "" && ok && tenant == g.tenant
	case g.project != "":
		return t.Project == g.project
	}
	return false
}
