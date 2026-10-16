package controller_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// seen records the keys a controller reconciled.
type seen struct {
	mu   sync.Mutex
	keys map[controller.Key]int
}

func (s *seen) Reconcile(_ context.Context, k controller.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k]++
	return nil
}

func (s *seen) count(k controller.Key) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[k]
}

func container(name string) *computev1alpha1.Container {
	return &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx"},
	}
}

// TestAPISourceFollowsTheAPI: a controller following kinds through the API,
// as the node agent, reconciles what exists, what changes, and what goes.
func TestAPISourceFollowsTheAPI(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	root := env.As(apiservertest.Root)
	ctx := t.Context()
	if _, err := root.Apply(ctx, registry.Container, container("before")); err != nil {
		t.Fatal(err)
	}

	r := &seen{keys: map[controller.Key]int{}}
	c := &controller.Controller{
		Name: "t", Source: controller.APISource{Client: env.AsNode()}, Reconciler: r,
		Watches: []controller.Watch{
			{Kind: registry.Container.Kind, Map: controller.Self},
			// A machine changing reconciles the container of the same name.
			{Kind: registry.Machine.Kind, Map: func(_ context.Context, o store.Object) []controller.Key {
				var m computev1alpha1.Machine
				if err := proto.Unmarshal(o.Data, &m); err != nil {
					t.Errorf("decoding a machine: %v", err)
				}
				return []controller.Key{{Project: o.Project, Name: "machine:" + m.GetMetadata().GetName()}}
			}},
			{Kind: registry.RoleBinding.Kind, Map: controller.Self},
		},
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	before := controller.Key{Project: apiservertest.Project, Name: "before"}
	testutil.Eventually(t, "an existing container reconciled", func() bool { return r.count(before) >= 1 })
	testutil.Eventually(t, "its machine, a second kind, reconciled", func() bool {
		return r.count(controller.Key{Project: apiservertest.Project, Name: "machine:ct-before"}) >= 1
	})

	after := controller.Key{Project: apiservertest.Project, Name: "after"}
	if _, err := root.Apply(ctx, registry.Container, container("after")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a new container reconciled", func() bool { return r.count(after) >= 1 })

	// A role binding has no finalizers: deleting it is a single event.
	binding := controller.Key{Name: "member-bob"}
	if _, err := root.Apply(ctx, registry.RoleBinding, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: binding.Name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "bob@example.com"}},
			Role:    "platform.member",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a new role binding reconciled", func() bool { return r.count(binding) == 1 })
	if _, err := root.Delete(ctx, registry.RoleBinding, client.Key{Name: binding.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a deleted role binding reconciled", func() bool { return r.count(binding) == 2 })
}

// TestAPISourceReportsCompaction: watching from history the apiserver no
// longer keeps fails as the store does, so the controller lists again.
func TestAPISourceReportsCompaction(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	ctx := t.Context()
	rev, err := env.Store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Compact(ctx, rev); err != nil {
		t.Fatal(err)
	}
	err = controller.APISource{Client: env.AsNode()}.Watch(ctx, []string{registry.Container.Kind}, 1, func(store.Event) error { return nil })
	if !errors.Is(err, store.ErrCompacted) {
		t.Errorf("watching from a compacted revision: %v", err)
	}
}

// endWatches makes every watch of one procedure end at once, as an API
// server ending the stream would.
type endWatches struct{ procedure string }

func (e endWatches) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (e endWatches) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (e endWatches) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if strings.HasSuffix(spec.Procedure, e.procedure) {
			return ended{conn}
		}
		return conn
	}
}

type ended struct{ connect.StreamingClientConn }

func (ended) Receive(any) error { return io.EOF }

// TestAWatchEndingEndsThemAll: when the server ends one kind's watch, the
// others end too, so the controller lists everything again rather than
// following some kinds and not others.
func TestAWatchEndingEndsThemAll(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	cl := client.New(client.UnixSocket(env.Socket), client.UnixSocketURL, connect.WithInterceptors(endWatches{"/WatchContainers"}))
	rev, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = controller.APISource{Client: cl}.Watch(ctx, []string{registry.Machine.Kind, registry.Container.Kind}, rev, func(store.Event) error { return nil })
	if err != nil {
		t.Errorf("a watch the server ended: %v, want nil once every kind's watch ends", err)
	}
}

// TestAPISourceCallsFnOneAtATime: events of different kinds never reach fn
// at the same time, as with a store's single watch.
func TestAPISourceCallsFnOneAtATime(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	rev, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	seen := map[string]int{} // unsynchronised on purpose: -race checks fn's calls
	var done atomic.Bool
	finished := make(chan error, 1)
	go func() {
		finished <- controller.APISource{Client: env.AsNode()}.Watch(ctx, []string{registry.Container.Kind, registry.Machine.Kind}, rev,
			func(ev store.Event) error {
				seen[ev.Object.Kind]++
				if seen[registry.Container.Kind] > 0 && seen[registry.Machine.Kind] > 0 {
					done.Store(true)
				}
				return nil
			})
	}()
	if _, err := env.As(apiservertest.Root).Apply(ctx, registry.Container, container("api")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "events of both kinds", done.Load)
	cancel()
	<-finished
}
