package authz

import (
	"context"
	"errors"
	"fmt"
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
// RoleBindings, Projects and Users, kept current by watching the store.
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
	// bindings maps each binding's key to its subject and grant, so updates
	// and deletes can replace it.
	bindings map[string]bindingEntry
	grants   map[subject][]grant
	// tenantOf maps project names to their tenant.
	tenantOf map[string]string
	// usernames maps each signed-up person's email to their User's name, and
	// emails is its inverse, so a deleted User can be dropped by name.
	usernames map[string]string
	emails    map[string]string
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
	return &Authorizer{
		store:     st,
		extra:     maps.Clone(extra),
		bindings:  map[string]bindingEntry{},
		grants:    map[subject][]grant{},
		tenantOf:  map[string]string{},
		usernames: map[string]string{},
		emails:    map[string]string{},
		advanced:  make(chan struct{}),
		synced:    make(chan struct{}),
		changes:   make(chan struct{}, 1),
	}
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
	projects, _, err := a.store.List(ctx, store.ListOptions{Kind: registry.Project.Kind})
	if err != nil {
		return err
	}
	users, _, err := a.store.List(ctx, store.ListOptions{Kind: registry.User.Kind})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.bindings = map[string]bindingEntry{}
	a.tenantOf = map[string]string{}
	a.usernames = map[string]string{}
	a.emails = map[string]string{}
	for _, o := range bindings {
		a.putBinding(o)
	}
	for _, o := range projects {
		a.putProject(o)
	}
	for _, o := range users {
		a.putUser(o)
	}
	a.rebuildGrants()
	a.advance(rev)
	a.mu.Unlock()
	// Projects and users were listed at or after rev; replaying from rev
	// reapplies full-state events, which converges to the same view.
	return a.store.Watch(ctx, store.WatchOptions{SinceRevision: rev}, func(ev store.Event) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.apply(ev)
		a.advance(ev.Revision)
		return nil
	})
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
	a.bindings = map[string]bindingEntry{}
	a.tenantOf = map[string]string{}
	a.usernames = map[string]string{}
	a.emails = map[string]string{}
	for _, o := range bindings {
		a.putBinding(o)
	}
	for _, o := range projects {
		a.putProject(o)
	}
	for _, o := range users {
		a.putUser(o)
	}
	a.rebuildGrants()
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
		a.mu.Unlock()
		a.changed()
		return nil
	})
}

// apply changes the view by one event. Callers hold a.mu.
func (a *Authorizer) apply(ev store.Event) {
	switch ev.Object.Kind {
	case registry.RoleBinding.Kind:
		if ev.Type == store.EventDeleted {
			delete(a.bindings, ev.Object.Name)
		} else {
			a.putBinding(ev.Object)
		}
		a.rebuildGrants()
	case registry.Project.Kind:
		if ev.Type == store.EventDeleted {
			delete(a.tenantOf, ev.Object.Name)
		} else {
			a.putProject(ev.Object)
		}
	case registry.User.Kind:
		if ev.Type == store.EventDeleted {
			a.dropUser(ev.Object.Name)
		} else {
			a.putUser(ev.Object)
		}
	}
}

func (a *Authorizer) putBinding(o store.Object) {
	var rb iamv1alpha1.RoleBinding
	if err := proto.Unmarshal(o.Data, &rb); err != nil {
		return
	}
	e, ok := a.entryOf(&rb)
	if !ok {
		delete(a.bindings, o.Name)
		return
	}
	a.bindings[o.Name] = e
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

func (a *Authorizer) putProject(o store.Object) {
	var p tenancyv1alpha1.Project
	if err := proto.Unmarshal(o.Data, &p); err != nil {
		return
	}
	a.tenantOf[o.Name] = p.GetSpec().GetTenant()
}

func (a *Authorizer) putUser(o store.Object) {
	var u iamv1alpha1.User
	if err := proto.Unmarshal(o.Data, &u); err != nil {
		return
	}
	// A User's email never changes, but replace whatever the name had.
	a.dropUser(o.Name)
	email := u.GetSpec().GetEmail()
	if email == "" {
		return
	}
	a.usernames[email] = o.Name
	a.emails[o.Name] = email
}

func (a *Authorizer) dropUser(name string) {
	if email, ok := a.emails[name]; ok {
		delete(a.emails, name)
		if a.usernames[email] == name {
			delete(a.usernames, email)
		}
	}
}

func (a *Authorizer) rebuildGrants() {
	a.grants = map[subject][]grant{}
	for _, e := range a.bindings {
		a.grants[e.subject] = append(a.grants[e.subject], e.grant)
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
// endpoints, and the bindings, projects and users that decide who may use
// them (docs/decisions/public-endpoints.md).
var EdgeReads = func() []string {
	var out []string
	for _, r := range []registry.Resource{registry.PublicEndpoint, registry.RoleBinding, registry.Project, registry.User} {
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
	switch p.Type {
	case auth.System:
		return true
	case auth.Node:
		return slices.Contains([]Verb{Get, List, Watch, UpdateStatus}, verbOf(permission))
	case auth.Local:
		// Root on the host holds platform.root whatever the bindings say, so
		// it can mend them.
		return Roles[LocalRootRole].Grants(permission)
	case auth.Edge:
		return slices.Contains(EdgeReads, permission)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	// A person who is not a platform member may do nothing, whatever else
	// they have been granted; see docs/decisions/tenancy-and-iam.md.
	if p.Type == auth.User && !a.member(p) {
		return false
	}
	for _, g := range a.grants[subject{p.Type, p.ID}] {
		if g.role.Grants(permission) && a.covers(g, t) {
			return true
		}
	}
	return false
}

// IsMember reports whether a person is a platform member: invited, and signed
// up. Other principals are not subject to membership and always report true.
func (a *Authorizer) IsMember(p auth.Principal) bool {
	if p.Type != auth.User {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.member(p)
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
	return a.invited(p)
}

// PlatformRoles returns the names of the roles bound to p at the platform,
// sorted, each once. It is for saying who someone is, such as a sponsor's
// badge; whether p may do something is Allowed's, by permission.
func (a *Authorizer) PlatformRoles(p auth.Principal) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var names []string
	for _, g := range a.grants[subject{p.Type, p.ID}] {
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
	name, ok := a.usernames[p.ID]
	return name, ok
}

// member reports platform membership: a platform role and a User. Callers
// hold a.mu.
func (a *Authorizer) member(p auth.Principal) bool {
	_, signedUp := a.usernames[p.ID]
	return signedUp && a.invited(p)
}

// invited reports whether p holds a platform role. Callers hold a.mu.
func (a *Authorizer) invited(p auth.Principal) bool {
	for _, g := range a.grants[subject{p.Type, p.ID}] {
		if g.platform && g.role.Member {
			return true
		}
	}
	return false
}

func (a *Authorizer) covers(g grant, t Target) bool {
	switch {
	case g.platform:
		return true
	case g.tenant != "":
		if t.Tenant == g.tenant {
			return true
		}
		return t.Project != "" && a.tenantOf[t.Project] == g.tenant
	case g.project != "":
		return t.Project == g.project
	}
	return false
}
