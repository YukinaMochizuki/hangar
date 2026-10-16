package apiserver

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// TestNoBindingOutlivesItsToken races bindings naming a token against the
// token's delete, and reads the store's history in order: when the token is
// gone, no binding naming it may be left, not even for the moment before
// the garbage collector would find it.
func TestNoBindingOutlivesItsToken(t *testing.T) {
	s, system, _ := running(t)
	ctx := t.Context()
	rev, err := s.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []store.Event
	watchCtx, stop := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		_ = s.store.Watch(watchCtx, store.WatchOptions{SinceRevision: rev}, func(ev store.Event) error {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			return nil
		})
	}()

	const rounds, binders = 8, 6
	for round := range rounds {
		name := fmt.Sprintf("racer-%d", round)
		serviceToken(t, s, system, "web", name)
		var wg sync.WaitGroup
		for i := range binders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
					Metadata: &metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", name, i)},
					Spec: &iamv1alpha1.RoleBindingSpec{
						Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: "web/" + name}},
						Role:    "project.viewer",
						Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}},
					},
				})
			}()
		}
		if _, err := s.backend(registry.ServiceToken).Delete(system, resource.Key{Project: "web", Name: name}, nil); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
	}
	// Every token goes in the end.
	deadline := time.Now().Add(30 * time.Second)
	for {
		objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.ServiceToken.Kind})
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d tokens never went", len(objs))
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-watched

	// Replay: the bindings naming each token, as of each revision.
	mu.Lock()
	defer mu.Unlock()
	naming := map[string]map[string]bool{} // token subject -> binding names
	subjectOf := map[string]string{}       // binding name -> token subject
	gone := 0
	for _, ev := range events {
		switch ev.Object.Kind {
		case registry.RoleBinding.Kind:
			if ev.Type == store.EventDeleted {
				if sub, ok := subjectOf[ev.Object.Name]; ok {
					delete(naming[sub], ev.Object.Name)
				}
				continue
			}
			var rb iamv1alpha1.RoleBinding
			if err := proto.Unmarshal(ev.Object.Data, &rb); err != nil {
				t.Fatal(err)
			}
			sub := rb.GetSpec().GetSubject().GetServiceToken()
			if sub == "" {
				continue
			}
			if naming[sub] == nil {
				naming[sub] = map[string]bool{}
			}
			subjectOf[ev.Object.Name] = sub
			naming[sub][ev.Object.Name] = true
		case registry.ServiceToken.Kind:
			if ev.Type != store.EventDeleted {
				continue
			}
			gone++
			sub := ev.Object.Project + "/" + ev.Object.Name
			if left := naming[sub]; len(left) > 0 {
				t.Errorf("token %s went at revision %d with bindings naming it left: %v", sub, ev.Revision, left)
			}
		}
	}
	if gone != rounds {
		t.Errorf("saw %d tokens go, want %d", gone, rounds)
	}
}
