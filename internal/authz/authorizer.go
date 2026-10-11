package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

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
// RoleBindings and Projects, kept current by watching the store.
type Authorizer struct {
	store store.Store

	mu sync.RWMutex
	// bindings maps each binding's key to its subject and grant, so updates
	// and deletes can replace it.
	bindings map[string]bindingEntry
	grants   map[subject][]grant
	// tenantOf maps project names to their tenant.
	tenantOf map[string]string
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
func New(st store.Store) *Authorizer {
	return &Authorizer{
		store:    st,
		bindings: map[string]bindingEntry{},
		grants:   map[subject][]grant{},
		tenantOf: map[string]string{},
		advanced: make(chan struct{}),
	}
}

// Run loads the current bindings and projects and follows every write until
// ctx is done.
func (a *Authorizer) Run(ctx context.Context) error {
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
	a.mu.Lock()
	a.bindings = map[string]bindingEntry{}
	a.tenantOf = map[string]string{}
	for _, o := range bindings {
		a.putBinding(o)
	}
	for _, o := range projects {
		a.putProject(o)
	}
	a.rebuildGrants()
	a.advance(rev)
	a.mu.Unlock()
	// Projects were listed at or after rev; replaying from rev reapplies
	// full-state events, which converges to the same view.
	return a.store.Watch(ctx, store.WatchOptions{SinceRevision: rev}, func(ev store.Event) error {
		a.mu.Lock()
		defer a.mu.Unlock()
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
		}
		a.advance(ev.Revision)
		return nil
	})
}

func (a *Authorizer) putBinding(o store.Object) {
	var rb iamv1alpha1.RoleBinding
	if err := proto.Unmarshal(o.Data, &rb); err != nil {
		return
	}
	e, ok := entryOf(&rb)
	if !ok {
		delete(a.bindings, o.Name)
		return
	}
	a.bindings[o.Name] = e
}

func entryOf(rb *iamv1alpha1.RoleBinding) (bindingEntry, bool) {
	spec := rb.GetSpec()
	role, ok := Roles[spec.GetRole()]
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
	e.grant.role = role
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
// so a request sees bindings and projects created by earlier requests.
func (a *Authorizer) Fresh(ctx context.Context) error {
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

// Allowed reports whether p holds the permission on the target.
func (a *Authorizer) Allowed(p auth.Principal, permission string, t Target) bool {
	switch p.Type {
	case auth.System:
		return true
	case auth.Node:
		return slices.Contains([]Verb{Get, List, Watch, UpdateStatus}, verbOf(permission))
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

// IsMember reports whether a person is a platform member. Other principals
// are not subject to membership and always report true.
func (a *Authorizer) IsMember(p auth.Principal) bool {
	if p.Type != auth.User {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.member(p)
}

// member reports platform membership. Callers hold a.mu.
func (a *Authorizer) member(p auth.Principal) bool {
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
