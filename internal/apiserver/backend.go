package apiserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/labels"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
	// maxRetries bounds how often a write without an expected version
	// retries after losing a race with another writer.
	maxRetries = 10
)

// backend implements every operation of one kind on top of the store; the
// rules it enforces are in docs/decisions/apply-semantics.md.
type backend struct {
	s     *Server
	r     registry.Resource
	hooks kindHooks
}

var _ resource.Backend = (*backend)(nil)

func (b *backend) storeKey(k resource.Key) store.Key {
	return store.Key{Kind: b.r.Kind, Project: k.Project, Name: k.Name}
}

func (b *backend) decode(o store.Object) (proto.Message, error) {
	m := b.r.Message.New().Interface()
	if err := proto.Unmarshal(o.Data, m); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", b.r.Kind, o.Name, err)
	}
	object.SetTypeMeta(m, b.r.APIVersion(), b.r.Kind)
	object.Meta(m).ResourceVersion = o.Revision
	return m, nil
}

// encode serializes an object for storage. The resource version is left out:
// the store's revision is the only source of it.
func encode(m proto.Message) ([]byte, error) {
	c := proto.Clone(m)
	object.Meta(c).ResourceVersion = 0
	return proto.Marshal(c)
}

// begin authenticates the call, brings the authorization view up to date
// with every write committed before it, and refuses anyone who is not a
// platform member.
func (b *backend) begin(ctx context.Context) (auth.Principal, error) {
	return b.s.begin(ctx, false)
}

// begin authenticates a call and brings the authorization view up to date
// with every write committed before it. Only platform members get further,
// and, when invitedSuffices, people invited who have not signed up yet; see
// docs/decisions/tenancy-and-iam.md.
func (s *Server) begin(ctx context.Context, invitedSuffices bool) (auth.Principal, error) {
	p, err := s.authenticated(ctx)
	if err != nil {
		return p, err
	}
	switch {
	case s.authz.IsMember(p):
		return p, nil
	case s.authz.IsInvited(p):
		if invitedSuffices {
			return p, nil
		}
		return p, denied("%s has not signed up yet; run hangar signup --username NAME first", p)
	}
	// Someone who signed in but was never added to the platform may do
	// nothing at all.
	return p, denied("%s is not a platform member yet; an administrator has to add them", p)
}

// authenticated returns the caller, with the authorization view brought up to
// date with every write committed before the call, whoever they are.
func (s *Server) authenticated(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Principal{}, connect.NewError(connect.CodeUnauthenticated, auth.ErrNoCredentials)
	}
	if p.Type != auth.System {
		if err := s.authz.Fresh(ctx); err != nil {
			return p, apiError(err)
		}
	}
	return p, nil
}

func (b *backend) allowed(p auth.Principal, verb authz.Verb, t authz.Target) bool {
	if b.hooks.systemManaged && p.Type != auth.System && !readsOrReports(verb) {
		return false
	}
	if b.hooks.neverDeleted != "" && p.Type != auth.System && verb == authz.Delete {
		return false
	}
	if b.hooks.hiddenFromNodes && p.Type == auth.Node {
		return false
	}
	if b.hooks.readableByAll && (verb == authz.Get || verb == authz.List || verb == authz.Watch) {
		return true
	}
	return b.s.authz.Allowed(p, authz.Permission(b.r, verb), t)
}

// readsOrReports lists the verbs anyone but the system may use on a
// system-managed kind; any verb added later is refused until it is listed.
func readsOrReports(v authz.Verb) bool {
	return v == authz.Get || v == authz.List || v == authz.Watch || v == authz.UpdateStatus
}

// authorize checks a grant. The denial names only what the caller asked
// for, never anything read from the stored object, such as where it lives.
func (b *backend) authorize(p auth.Principal, verb authz.Verb, t authz.Target, asked string) error {
	if !b.allowed(p, verb, t) {
		return denied("%s may not %s %s", p, verb, asked)
	}
	return nil
}

// asked describes the object a call named, in the words of the request.
func (b *backend) asked(k resource.Key) string {
	if k.Project != "" {
		return fmt.Sprintf("%s %q in project %q", b.r.Plural, k.Name, k.Project)
	}
	return fmt.Sprintf("%s %q", b.r.Plural, k.Name)
}

func (b *backend) askedIn(project string) string {
	if project != "" {
		return fmt.Sprintf("%s in project %q", b.r.Plural, project)
	}
	return b.r.Plural
}

// target is where an action on an existing object applies.
func (b *backend) target(key resource.Key, obj proto.Message) authz.Target {
	return b.hooks.target(b.s, key, obj, false)
}

func (b *backend) checkKey(k resource.Key) error {
	if b.r.ProjectScoped() {
		if k.Project == "" {
			return invalid("%s are project-scoped; project is required", b.r.Plural)
		}
		if err := ValidName(k.Project); err != nil {
			return invalid("project: %v", err)
		}
	} else if k.Project != "" {
		return invalid("%s are global; project must be empty", b.r.Plural)
	}
	if b.hooks.nameIsEmail {
		if err := validEmail(k.Name); err != nil {
			return invalid("name: %v", err)
		}
		return nil
	}
	if err := ValidName(k.Name); err != nil {
		return invalid("name: %v", err)
	}
	return nil
}

// lockWrites takes the kind's writeLocks, in order, but those ctx says its
// caller holds already, and returns what releases what it took.
func (b *backend) lockWrites(ctx context.Context) (unlock func()) {
	if b.hooks.writeLocks == nil {
		return func() {}
	}
	var unlocks []func()
	for _, mu := range b.hooks.writeLocks(b.s) {
		unlocks = append(unlocks, lockUnlessHeld(ctx, mu))
	}
	return func() {
		for _, u := range slices.Backward(unlocks) {
			u()
		}
	}
}

func (b *backend) Get(ctx context.Context, key resource.Key) (proto.Message, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	_, obj, err := b.read(ctx, p, authz.Get, key)
	if err != nil {
		return nil, err
	}
	b.answer(p, obj)
	return obj, nil
}

// answer clears from obj, in place, what p may not be answered: what no
// answer holds (kindHooks.redact), and what a node agent may not read
// (kindHooks.forNode). Every object a call or event answers goes through
// it.
func (b *backend) answer(p auth.Principal, obj proto.Message) {
	if obj == nil {
		return
	}
	if b.hooks.redact != nil {
		b.hooks.redact(obj)
	}
	if p.Type == auth.Node && b.hooks.forNode != nil {
		b.hooks.forNode(p.ID, obj)
	}
}

// read fetches an object for an action and authorizes it. Its answer never
// tells a caller without the grant whether the object exists: where the key
// alone decides the target, authorization comes before the read; otherwise a
// missing object is reported as denied to anyone without a platform grant.
func (b *backend) read(ctx context.Context, p auth.Principal, verb authz.Verb, key resource.Key) (store.Object, proto.Message, error) {
	if !b.hooks.targetNeedsObject {
		if err := b.authorize(p, verb, b.target(key, nil), b.asked(key)); err != nil {
			return store.Object{}, nil, err
		}
	}
	cur, err := b.s.store.Get(ctx, b.storeKey(key))
	if errors.Is(err, store.ErrNotFound) && b.hooks.targetNeedsObject && !b.allowed(p, verb, authz.Target{}) {
		return store.Object{}, nil, denied("%s may not %s %s", p, verb, b.asked(key))
	}
	if err != nil {
		return store.Object{}, nil, apiError(err)
	}
	obj, err := b.decode(cur)
	if err != nil {
		return store.Object{}, nil, apiError(err)
	}
	if err := b.authorize(p, verb, b.target(key, obj), b.asked(key)); err != nil {
		return store.Object{}, nil, err
	}
	return cur, obj, nil
}

func (b *backend) List(ctx context.Context, opts resource.ListOptions) (resource.ListResult, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return resource.ListResult{}, err
	}
	sel, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return resource.ListResult{}, invalid("%v", err)
	}
	if opts.Project != "" {
		if err := b.authorize(p, authz.List, authz.Target{Project: opts.Project}, b.askedIn(opts.Project)); err != nil {
			return resource.ListResult{}, err
		}
	}
	after, err := decodePageToken(opts.PageToken)
	if err != nil {
		return resource.ListResult{}, err
	}
	size := int(opts.PageSize)
	if size <= 0 {
		size = defaultPageSize
	}
	size = min(size, maxPageSize)
	objs, rev, err := b.s.store.List(ctx, store.ListOptions{Kind: b.r.Kind, Project: opts.Project, StartAfter: after, Limit: size})
	if err != nil {
		return resource.ListResult{}, apiError(err)
	}
	res := resource.ListResult{Revision: rev}
	for _, o := range objs {
		obj, err := b.decode(o)
		if err != nil {
			return resource.ListResult{}, apiError(err)
		}
		key := resource.Key{Project: o.Project, Name: o.Name}
		if !b.allowed(p, authz.List, b.target(key, obj)) || !sel.Matches(object.Meta(obj).GetLabels()) {
			continue
		}
		b.answer(p, obj)
		res.Items = append(res.Items, obj)
	}
	// A full page means there may be more, even if filtering emptied it.
	if len(objs) == size {
		res.NextPageToken = encodePageToken(objs[len(objs)-1].Key)
	}
	return res, nil
}

func encodePageToken(k store.Key) string {
	return base64.RawURLEncoding.EncodeToString([]byte(k.Project + "/" + k.Name))
}

func decodePageToken(tok string) (store.Key, error) {
	if tok == "" {
		return store.Key{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return store.Key{}, invalid("malformed page token")
	}
	project, name, ok := strings.Cut(string(raw), "/")
	if !ok {
		return store.Key{}, invalid("malformed page token")
	}
	return store.Key{Project: project, Name: name}, nil
}

func (b *backend) Watch(ctx context.Context, opts resource.WatchOptions, send func(resource.Event) error) error {
	p, err := b.begin(ctx)
	if err != nil {
		return err
	}
	sel, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return invalid("%v", err)
	}
	if opts.Project != "" {
		if err := b.authorize(p, authz.Watch, authz.Target{Project: opts.Project}, b.askedIn(opts.Project)); err != nil {
			return err
		}
	}
	since := opts.SinceRevision
	if since == 0 {
		if since, err = b.s.store.Revision(ctx); err != nil {
			return apiError(err)
		}
	}
	// The opening bookmark tells the client where the watch starts, and makes
	// the response begin before the first change, which may be far off.
	if err := send(resource.Event{Type: metav1.EventType_EVENT_TYPE_BOOKMARK, Revision: since}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	progress := make(chan struct{})
	ticker := b.s.clock.NewTicker(b.s.cfg.BookmarkInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Chan():
				select {
				case progress <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	err = b.s.store.Watch(ctx, store.WatchOptions{
		Kind: b.r.Kind, Project: opts.Project, SinceRevision: since, Progress: progress,
	}, func(ev store.Event) error {
		if ev.Type == store.EventProgress {
			return send(resource.Event{Type: metav1.EventType_EVENT_TYPE_BOOKMARK, Revision: ev.Revision})
		}
		obj, err := b.decode(ev.Object)
		if err != nil {
			return err
		}
		// Judge each event by the grants as they stood at its revision, so a
		// binding or project the view has not caught up with yet neither
		// hides an event nor lets one through.
		if err := b.s.authz.WaitFor(ctx, ev.Revision); err != nil {
			return err
		}
		key := resource.Key{Project: ev.Object.Project, Name: ev.Object.Name}
		if !b.allowed(p, authz.Watch, b.target(key, obj)) || !sel.Matches(object.Meta(obj).GetLabels()) {
			return nil
		}
		b.answer(p, obj)
		return send(resource.Event{Type: eventType(ev.Type), Object: obj, Revision: ev.Revision})
	})
	// The caller's deadline reaches the server a little before the caller,
	// so say it ran out rather than end the stream as if it were complete.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	}
	if ctx.Err() != nil {
		return nil
	}
	return apiError(err)
}

func eventType(t store.EventType) metav1.EventType {
	switch t {
	case store.EventCreated:
		return metav1.EventType_EVENT_TYPE_ADDED
	case store.EventUpdated:
		return metav1.EventType_EVENT_TYPE_MODIFIED
	case store.EventDeleted:
		return metav1.EventType_EVENT_TYPE_DELETED
	}
	return metav1.EventType_EVENT_TYPE_UNSPECIFIED
}

func (b *backend) Apply(ctx context.Context, in proto.Message) (resource.ApplyResult, error) {
	return b.answered(ctx, in, false)
}

// answered applies in and passes the result through answer, as the caller
// is answered.
func (b *backend) answered(ctx context.Context, in proto.Message, createOnly bool) (resource.ApplyResult, error) {
	res, err := b.apply(ctx, in, createOnly)
	if p, ok := auth.FromContext(ctx); ok && err == nil {
		b.answer(p, res.Object)
	}
	return res, err
}

// Create writes in only if no object has its key, and fails with
// AlreadyExists otherwise. Controllers create with it: one that read
// "absent" must not overwrite an object written since.
func (b *backend) Create(ctx context.Context, in proto.Message) (resource.ApplyResult, error) {
	return b.answered(ctx, in, true)
}

func (b *backend) apply(ctx context.Context, in proto.Message, createOnly bool) (resource.ApplyResult, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	in = proto.Clone(in)
	apiVersion, kind := object.TypeMeta(in)
	if apiVersion != "" && apiVersion != b.r.APIVersion() || kind != "" && kind != b.r.Kind {
		return resource.ApplyResult{}, invalid("apiVersion %q kind %q does not match %s %s", apiVersion, kind, b.r.APIVersion(), b.r.Kind)
	}
	object.SetTypeMeta(in, b.r.APIVersion(), b.r.Kind)
	meta := object.Meta(in)
	key := resource.Key{Project: meta.GetProject(), Name: meta.GetName()}
	if err := b.checkKey(key); err != nil {
		return resource.ApplyResult{}, err
	}
	// In a project, the key decides who may write; checking it first keeps
	// the rest of the call from telling outsiders anything about the object.
	if b.r.ProjectScoped() {
		if err := b.authorize(p, authz.Apply, authz.Target{Project: key.Project}, b.asked(key)); err != nil {
			return resource.ApplyResult{}, err
		}
	}
	expected := meta.GetResourceVersion()
	defer b.lockWrites(ctx)()
	if p.Type != auth.System && countsTowardQuota(b.r.Kind) {
		b.s.quota.Lock()
		defer b.s.quota.Unlock()
	}

	for range maxRetries {
		cur, err := b.s.store.Get(ctx, b.storeKey(key))
		switch {
		case errors.Is(err, store.ErrNotFound):
			if expected != 0 {
				return resource.ApplyResult{}, connect.NewError(connect.CodeNotFound,
					fmt.Errorf("%s %s does not exist at resource version %d", b.r.Kind, key.Name, expected))
			}
			res, err := b.create(ctx, p, key, in)
			if errors.Is(err, store.ErrExists) && !createOnly {
				continue
			}
			return res, apiError(err)
		case err != nil:
			return resource.ApplyResult{}, apiError(err)
		case createOnly:
			return resource.ApplyResult{}, apiError(store.ErrExists)
		}
		res, err := b.update(ctx, p, key, cur, in, expected)
		// Without an expected version Apply is create-or-update, so an object
		// changed or deleted since it was read is read again: deleted, it is
		// created.
		if (errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound)) && expected == 0 {
			continue
		}
		return res, apiError(err)
	}
	return resource.ApplyResult{}, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}

func (b *backend) create(ctx context.Context, p auth.Principal, key resource.Key, in proto.Message) (resource.ApplyResult, error) {
	verb, t := authz.Apply, b.hooks.target(b.s, key, in, true)
	if b.hooks.selfServiceCreate {
		// Opening a tenant needs no grant inside it, only the right to open
		// one at all.
		verb, t = authz.Create, authz.Target{}
	}
	if err := b.authorize(p, verb, t, b.asked(key)); err != nil {
		return resource.ApplyResult{}, err
	}
	meta := object.Meta(in)
	if err := checkMetaKeys(p, meta, nil); err != nil {
		return resource.ApplyResult{}, err
	}
	if b.hooks.onCreate != nil {
		b.hooks.onCreate(p, in)
	}
	if b.hooks.createLock != nil {
		defer b.hooks.createLock(b.s, in)()
	}
	if err := b.s.admit(ctx, b, p, nil, in); err != nil {
		return resource.ApplyResult{}, err
	}
	if p.Type != auth.System {
		meta.Finalizers = nil
		meta.OwnerReferences = nil
	}
	owner, err := b.s.ownerRef(ctx, b, in)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	if owner != nil && !slices.ContainsFunc(meta.OwnerReferences, func(r *metav1.OwnerReference) bool { return r.GetUid() == owner.GetUid() }) {
		meta.OwnerReferences = append(meta.OwnerReferences, owner)
	}
	if b.hooks.alsoOwnedBy != nil {
		owner, err := b.hooks.alsoOwnedBy(ctx, b.s, p, in)
		if err != nil {
			return resource.ApplyResult{}, err
		}
		if owner != nil && !slices.ContainsFunc(meta.OwnerReferences, func(r *metav1.OwnerReference) bool { return r.GetUid() == owner.GetUid() }) {
			meta.OwnerReferences = append(meta.OwnerReferences, owner)
		}
	}
	if b.hooks.cascades && !slices.Contains(meta.Finalizers, dependentsFinalizer) {
		meta.Finalizers = append(meta.Finalizers, dependentsFinalizer)
	}
	for _, f := range b.hooks.finalizers {
		if !slices.Contains(meta.Finalizers, f) {
			meta.Finalizers = append(meta.Finalizers, f)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return resource.ApplyResult{}, err
	}
	meta.Uid = id.String()
	meta.CreateTime = timestamppb.New(b.s.clock.Now())
	meta.Generation = 1
	meta.DeleteTime = nil
	object.SetStatus(in, nil)
	if b.hooks.seal != nil {
		if err := b.hooks.seal(b.s, nil, in); err != nil {
			return resource.ApplyResult{}, err
		}
	}
	data, err := encode(in)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	claim := nameClaim(b.r, key)
	// The claims come first, so two creators of one name cannot both win;
	// they are released again if no object is created after all, and kept
	// when one is: by another creator of the same key, which holds them.
	var global *globalClaim
	release := func(ctx context.Context) {
		if global != nil {
			if _, _, err := b.s.store.Delete(ctx, global.key, 0); err != nil {
				b.s.log.Error("releasing a claim failed", "kind", b.r.Kind, "claim", global.key.Name, "error", err)
			}
		}
		if !b.hooks.neverReuseNames {
			return
		}
		if _, _, err := b.s.store.Delete(ctx, claim, 0); err != nil {
			b.s.log.Error("releasing a name claim failed", "kind", b.r.Kind, "name", key.Name, "error", err)
		}
	}
	if b.hooks.neverReuseNames {
		if _, err := b.s.store.Create(ctx, claim, nil); errors.Is(err, store.ErrExists) {
			if b.r.Kind == registry.User.Kind {
				return resource.ApplyResult{}, failedPrecondition(
					"username %q has been used before and is never reused; choose another username", key.Name)
			}
			return resource.ApplyResult{}, failedPrecondition(
				"%s name %q has been used before and is never reused; choose another and use display_name for the name people see",
				strings.ToLower(b.r.Kind), key.Name)
		} else if err != nil {
			return resource.ApplyResult{}, err
		}
	}
	if b.hooks.claim != nil {
		c := b.hooks.claim(key)
		taken, err := b.takeClaim(ctx, c)
		if err != nil {
			release(ctx)
			return resource.ApplyResult{}, err
		}
		if taken {
			global = &c
		}
	}
	// From here on, the caller hanging up must not leave a claim or a half
	// set up object behind.
	ctx = context.WithoutCancel(ctx)
	rev, err := b.s.store.Create(ctx, b.storeKey(key), data)
	if err != nil {
		// An object of this key made since the read holds the claims too:
		// it keeps them, as it would have taken them.
		if !errors.Is(err, store.ErrExists) {
			release(ctx)
		}
		return resource.ApplyResult{}, err
	}
	meta.ResourceVersion = rev
	if b.hooks.afterCreate != nil {
		if err := b.hooks.afterCreate(ctx, b.s, p, in); err != nil {
			if _, _, delErr := b.s.store.Delete(ctx, b.storeKey(key), rev); delErr != nil {
				b.s.log.Error("rolling back a half-created object failed", "kind", b.r.Kind, "name", key.Name, "error", delErr)
			} else {
				release(ctx)
			}
			return resource.ApplyResult{}, fmt.Errorf("creating %s %s: %w", b.r.Kind, key.Name, err)
		}
	}
	return resource.ApplyResult{Object: in, Created: true}, nil
}

// globalClaim is a claim an object takes when it is created, on something
// besides its name that no other object may ever hold (kindHooks.claim).
type globalClaim struct {
	key store.Key
	// holder is who holds it: the same holder takes it again, as an object
	// made again under its old name.
	holder string
	// what names what is claimed, in the words of a refusal.
	what string
}

// takeClaim takes c, or finds its holder already holds it, or refuses: it
// reports whether it took it now, and so is the one to release it if the
// create goes no further.
func (b *backend) takeClaim(ctx context.Context, c globalClaim) (bool, error) {
	_, err := b.s.store.Create(ctx, c.key, []byte(c.holder))
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, store.ErrExists) {
		return false, err
	}
	o, err := b.s.store.Get(ctx, c.key)
	if err != nil {
		return false, err
	}
	if string(o.Data) != c.holder {
		return false, failedPrecondition("%s is held by another %s and is never given to another; choose another name",
			c.what, strings.ToLower(b.r.Kind))
	}
	return false, nil
}

// nameClaimKind holds one record per global name ever used by a kind whose
// names are never reused. It is not an API kind; nothing serves or watches it.
const nameClaimKind = "NameClaim"

func nameClaim(r registry.Resource, key resource.Key) store.Key {
	return store.Key{Kind: nameClaimKind, Name: r.Kind + "/" + key.Name}
}

func (b *backend) update(ctx context.Context, p auth.Principal, key resource.Key, cur store.Object, in proto.Message, expected int64) (resource.ApplyResult, error) {
	old, err := b.decode(cur)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	if err := b.authorize(p, authz.Apply, b.target(key, old), b.asked(key)); err != nil {
		return resource.ApplyResult{}, err
	}
	if expected != 0 && expected != cur.Revision {
		return resource.ApplyResult{}, connect.NewError(connect.CodeAborted,
			fmt.Errorf("%s %s is at resource version %d, not %d", b.r.Kind, key.Name, cur.Revision, expected))
	}
	oldMeta := object.Meta(old)
	if err := checkMetaKeys(p, object.Meta(in), oldMeta); err != nil {
		return resource.ApplyResult{}, err
	}
	if oldMeta.GetDeleteTime() != nil {
		return resource.ApplyResult{}, failedPrecondition("%s %s is being deleted", b.r.Kind, key.Name)
	}
	if changed := object.ImmutableChanges(old, in); len(changed) > 0 {
		return resource.ApplyResult{}, invalid("immutable fields changed: %s", strings.Join(changed, ", "))
	}
	if err := b.s.admit(ctx, b, p, old, in); err != nil {
		return resource.ApplyResult{}, err
	}

	merged := proto.Clone(in)
	meta := object.Meta(merged)
	meta.Uid = oldMeta.GetUid()
	meta.CreateTime = oldMeta.GetCreateTime()
	meta.Generation = oldMeta.GetGeneration()
	meta.DeleteTime = nil
	meta.ResourceVersion = cur.Revision
	if p.Type != auth.System {
		meta.Finalizers = oldMeta.GetFinalizers()
		meta.OwnerReferences = oldMeta.GetOwnerReferences()
		meta.Labels = keepReserved(meta.GetLabels(), oldMeta.GetLabels())
		meta.Annotations = keepReserved(meta.GetAnnotations(), oldMeta.GetAnnotations())
	}
	object.CopyStatus(merged, old)
	if b.hooks.seal != nil {
		if err := b.hooks.seal(b.s, old, merged); err != nil {
			return resource.ApplyResult{}, err
		}
	}
	if !object.SpecEqual(old, merged) {
		meta.Generation++
	}
	if proto.Equal(old, merged) {
		return resource.ApplyResult{Object: old}, nil
	}
	data, err := encode(merged)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	rev, err := b.s.store.Update(ctx, b.storeKey(key), data, cur.Revision)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	meta.ResourceVersion = rev
	return resource.ApplyResult{Object: merged}, nil
}

// keepReserved returns the user's map plus the system-reserved entries of
// the stored one, which a user's apply must not drop.
func keepReserved(user, stored map[string]string) map[string]string {
	out := maps.Clone(user)
	for k, v := range stored {
		if reservedKey(k) {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

func (b *backend) Delete(ctx context.Context, key resource.Key, expected *int64) (proto.Message, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if b.hooks.neverDeleted != "" && p.Type != auth.System {
		return nil, denied("%s may not delete %s: %s", p, b.asked(key), b.hooks.neverDeleted)
	}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	defer b.lockWrites(ctx)()
	for range maxRetries {
		cur, old, err := b.read(ctx, p, authz.Delete, key)
		if err != nil {
			return nil, err
		}
		if expected != nil && *expected != cur.Revision {
			return nil, connect.NewError(connect.CodeAborted,
				fmt.Errorf("%s %s is at resource version %d, not %d", b.r.Kind, key.Name, cur.Revision, *expected))
		}
		meta := object.Meta(old)
		if meta.GetDeleteTime() == nil && b.hooks.beforeDelete != nil {
			if err := b.hooks.beforeDelete(ctx, b.s, old); err != nil {
				return nil, apiError(err)
			}
		}
		if len(meta.GetFinalizers()) > 0 {
			if meta.GetDeleteTime() != nil {
				if err := b.afterDelete(ctx, key); err != nil {
					return nil, err
				}
				b.answer(p, old)
				return old, nil
			}
			meta.DeleteTime = timestamppb.New(b.s.clock.Now())
			data, err := encode(old)
			if err != nil {
				return nil, apiError(err)
			}
			rev, err := b.s.store.Update(ctx, b.storeKey(key), data, cur.Revision)
			if errors.Is(err, store.ErrConflict) && expected == nil {
				continue
			}
			if err != nil {
				return nil, apiError(err)
			}
			meta.ResourceVersion = rev
			if err := b.afterDelete(ctx, key); err != nil {
				return nil, err
			}
			b.answer(p, old)
			return old, nil
		}
		_, _, err = b.s.store.Delete(ctx, b.storeKey(key), cur.Revision)
		if errors.Is(err, store.ErrConflict) && expected == nil {
			continue
		}
		if err != nil {
			return nil, apiError(err)
		}
		if err := b.afterDelete(ctx, key); err != nil {
			return nil, err
		}
		b.answer(p, old)
		return old, nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}

// afterDelete runs the kind's afterDelete hook, if it has one, once a delete
// is stored. Its failure fails the call, which the caller retries: the
// object is still there, being deleted, and the hook runs again.
func (b *backend) afterDelete(ctx context.Context, key resource.Key) error {
	if b.hooks.afterDelete == nil {
		return nil
	}
	if err := b.hooks.afterDelete(context.WithoutCancel(ctx), b.s, key); err != nil {
		return apiError(err)
	}
	return nil
}

func (b *backend) UpdateStatus(ctx context.Context, key resource.Key, status proto.Message, expected *int64) (proto.Message, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	if status == nil || !status.ProtoReflect().IsValid() {
		return nil, invalid("status is required")
	}
	if v := b.hooks.validateStatus; v != nil {
		if err := v(status); err != nil {
			return nil, invalid("status: %v", err)
		}
	}
	for range maxRetries {
		cur, obj, err := b.read(ctx, p, authz.UpdateStatus, key)
		if err != nil {
			return nil, err
		}
		// A node agent reports only on what it runs.
		if p.Type == auth.Node && !b.hooks.anyNodeReports {
			if b.hooks.nodeOf == nil {
				return nil, denied("%s may not update the status of %s", p, b.asked(key))
			}
			if node := b.hooks.nodeOf(obj); node == "" || node != p.ID {
				return nil, denied("%s may not update the status of %s", p, b.asked(key))
			}
		}
		if expected != nil && *expected != cur.Revision {
			return nil, connect.NewError(connect.CodeAborted,
				fmt.Errorf("%s %s is at resource version %d, not %d", b.r.Kind, key.Name, cur.Revision, *expected))
		}
		object.SetStatus(obj, status)
		data, err := encode(obj)
		if err != nil {
			return nil, apiError(err)
		}
		rev, err := b.s.store.Update(ctx, b.storeKey(key), data, cur.Revision)
		if errors.Is(err, store.ErrConflict) && expected == nil {
			continue
		}
		if err != nil {
			return nil, apiError(err)
		}
		object.Meta(obj).ResourceVersion = rev
		if p.Type == auth.Node && b.r.Kind == registry.Node.Kind {
			b.s.heartbeats.record(key.Name, b.s.clock.Now())
		}
		b.answer(p, obj)
		return obj, nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}
