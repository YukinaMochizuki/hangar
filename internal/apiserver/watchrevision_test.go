package apiserver_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// heldWatch is a store whose first Container watch opened after hold
// passes on no event until released is closed: a watch that judges its
// events only once the authorization view has moved past them.
type heldWatch struct {
	store.Store
	released chan struct{}

	mu    sync.Mutex
	armed bool
}

func (s *heldWatch) hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *heldWatch) Watch(ctx context.Context, opts store.WatchOptions, send func(store.Event) error) error {
	s.mu.Lock()
	held := s.armed && opts.Kind == registry.Container.Kind
	if held {
		s.armed = false
	}
	s.mu.Unlock()
	if !held {
		return s.Store.Watch(ctx, opts, send)
	}
	return s.Store.Watch(ctx, opts, func(ev store.Event) error {
		select {
		case <-s.released:
		case <-ctx.Done():
			return ctx.Err()
		}
		return send(ev)
	})
}

// TestAWatchIsJudgedByTheGrantsAtEachEvent: bob views project web, then
// loses the grant. His watch, held back until the view is past the
// revocation, still gets the container deleted before it, and nothing
// written in web from it on.
func TestAWatchIsJudgedByTheGrantsAtEachEvent(t *testing.T) {
	t.Parallel()
	held := &heldWatch{released: make(chan struct{})}
	e := newEnv(t, func(c *apiserver.Config) {
		held.Store = c.Store
		c.Store = held
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	e.setupTenant(bob, "bobco", "app")
	a, b := e.as(alice), e.as(bob)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("bob-views-web", bob, "project.viewer", projectScope("web"))))
	// What the garbage collector writes after an apply comes before the watch.
	e.waitSettled()

	held.hold()
	stream, err := b.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, stream)
	// The opening bookmark: the watch is open, from before what follows.
	<-events
	must[computev1alpha1.DeleteContainerResponse](t)(a.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "api"})))
	// The container is marked deleted, then collected, both before the
	// revocation.
	e.waitSettled()
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(a.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "bob-views-web"})))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "late", "nginx"))))
	// Bob's own container closes what the test reads of the watch.
	must[computev1alpha1.ApplyContainerResponse](t)(b.containers.ApplyContainer(ctx, applyContainer(container("app", "mine", "nginx"))))
	// Every call brings the view up to every write before it, so it is past
	// the revocation before the watch judges a single event.
	must[computev1alpha1.GetContainerResponse](t)(b.containers.GetContainer(ctx, getContainer("app", "mine")))
	close(held.released)

	got := containerEventsUntil(t, events, "ADDED app/mine")
	if want := []string{"MODIFIED web/api", "DELETED web/api", "ADDED app/mine"}; !slices.Equal(got, want) {
		t.Errorf("bob's watch got %q, want %q", got, want)
	}
}

// containerEventsUntil reads a container watch up to the event last, and
// returns what it sent before, last included, as "TYPE project/name".
func containerEventsUntil(t *testing.T, events <-chan *computev1alpha1.WatchContainersResponse, last string) []string {
	t.Helper()
	var got []string
	timeout := time.After(10 * time.Second)
	for !slices.Contains(got, last) {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the watch ended; got %q", got)
			}
			if ev.GetType() == metav1.EventType_EVENT_TYPE_BOOKMARK {
				continue
			}
			c := ev.GetContainer().GetMetadata()
			typ, _ := strings.CutPrefix(ev.GetType().String(), "EVENT_TYPE_")
			got = append(got, fmt.Sprintf("%s %s/%s", typ, c.GetProject(), c.GetName()))
		case <-timeout:
			t.Fatalf("%s never came; got %q", last, got)
		}
	}
	return got
}

// TestAResumedWatchIsJudgedByTheGrantsAtEachEvent: with no other watch
// open, bob resumes a watch from before he lost project web. He still
// gets the container deleted before that, and nothing written in web from
// it on.
func TestAResumedWatchIsJudgedByTheGrantsAtEachEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	e.setupTenant(bob, "bobco", "app")
	a, b := e.as(alice), e.as(bob)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("bob-views-web", bob, "project.viewer", projectScope("web"))))
	e.waitSettled()
	since, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	must[computev1alpha1.DeleteContainerResponse](t)(a.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "api"})))
	e.waitSettled()
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(a.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "bob-views-web"})))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "late", "nginx"))))
	must[computev1alpha1.ApplyContainerResponse](t)(b.containers.ApplyContainer(ctx, applyContainer(container("app", "mine", "nginx"))))

	stream, err := b.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{SinceRevision: since}))
	if err != nil {
		t.Fatal(err)
	}
	got := containerEventsUntil(t, collect(t, stream), "ADDED app/mine")
	if want := []string{"MODIFIED web/api", "DELETED web/api", "ADDED app/mine"}; !slices.Equal(got, want) {
		t.Errorf("bob's resumed watch got %q, want %q", got, want)
	}
}

// TestAResumedWatchHidesWhatPrecedesAGrant: bob resumes a watch from before
// he may view project web. He gets what is written there from then on,
// not what was written before, which judging by the grants as they stand
// would let through.
func TestAResumedWatchHidesWhatPrecedesAGrant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	e.setupTenant(bob, "bobco", "app")
	a, b := e.as(alice), e.as(bob)
	e.waitSettled()
	since, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "early", "nginx"))))
	e.waitSettled()
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("bob-views-web", bob, "project.viewer", projectScope("web"))))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "after", "nginx"))))
	e.waitSettled()
	must[computev1alpha1.ApplyContainerResponse](t)(b.containers.ApplyContainer(ctx, applyContainer(container("app", "mine", "nginx"))))

	stream, err := b.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{SinceRevision: since}))
	if err != nil {
		t.Fatal(err)
	}
	got := containerEventsUntil(t, collect(t, stream), "ADDED app/mine")
	if want := []string{"ADDED web/after", "ADDED app/mine"}; !slices.Equal(got, want) {
		t.Errorf("bob's resumed watch got %q, want %q", got, want)
	}
}

// TestAWatchPastTheHistoryEndsAsCompacted: bob loses project web, something
// is written there, and he gets web back. With three revisions of
// authorization history kept and the store not compacted since, he resumes
// a watch from before all that once the revocation is past the history.
// The watch ends as compacted, so he lists again, and sends nothing of the
// revoked period, though he may view web now.
func TestAWatchPastTheHistoryEndsAsCompacted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *apiserver.Config) { c.EventRetention = 3 })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	e.setupTenant(bob, "bobco", "app")
	a, b := e.as(alice), e.as(bob)
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("bob-views-web", bob, "project.viewer", projectScope("web"))))
	e.waitSettled()
	since, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(a.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "bob-views-web"})))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "secret", "nginx"))))
	e.waitSettled()
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("bob-views-web", bob, "project.viewer", projectScope("web"))))
	// A binding that comes and goes pushes the revocation out of the
	// history, once three more revisions have passed.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("passing", bob, "project.viewer", projectScope("web"))))
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(a.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "passing"})))
	for _, name := range []string{"one", "two", "three", "mine"} {
		must[computev1alpha1.ApplyContainerResponse](t)(b.containers.ApplyContainer(ctx, applyContainer(container("app", name, "nginx"))))
	}
	e.waitSettled()

	stream, err := b.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{SinceRevision: since}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for stream.Receive() {
		ev := stream.Msg()
		if ev.GetType() == metav1.EventType_EVENT_TYPE_BOOKMARK {
			continue
		}
		c := ev.GetContainer().GetMetadata()
		got = append(got, c.GetProject()+"/"+c.GetName())
		// Bob's last container: the watch went on past the revoked period.
		if c.GetName() == "mine" {
			break
		}
	}
	if len(got) != 0 {
		t.Errorf("bob's watch past the history sent %q", got)
	}
	// As apiError answers store.ErrCompacted.
	if code := connect.CodeOf(stream.Err()); code != connect.CodeOutOfRange {
		t.Errorf("bob's watch past the history ended with %v (%v), want %v", code, stream.Err(), connect.CodeOutOfRange)
	}
}
