package servicegen_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/servicegen"
)

// recorder is a Backend that records what the shims pass it and returns
// objects of its kind.
type recorder struct {
	kind registry.Resource

	mu       sync.Mutex
	keys     []resource.Key
	expected []*int64
	list     resource.ListOptions
	watch    resource.WatchOptions
	applied  proto.Message
	created  proto.Message
	status   proto.Message
}

func (r *recorder) object(name string) proto.Message {
	m := r.kind.Message.New().Interface()
	md := m.ProtoReflect().Descriptor().Fields().ByName("metadata")
	m.ProtoReflect().Set(md, protoreflect.ValueOfMessage((&metav1.ObjectMeta{Name: name}).ProtoReflect()))
	return m
}

// locked runs f holding the recorder's lock; handlers record from server
// goroutines while tests read.
func (r *recorder) locked(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *recorder) record(key resource.Key, expected *int64) {
	r.locked(func() {
		r.keys = append(r.keys, key)
		r.expected = append(r.expected, expected)
	})
}

func (r *recorder) Get(_ context.Context, key resource.Key) (proto.Message, error) {
	r.record(key, nil)
	if key.Name == "missing" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	return r.object(key.Name), nil
}

func (r *recorder) List(_ context.Context, opts resource.ListOptions) (resource.ListResult, error) {
	r.locked(func() { r.list = opts })
	return resource.ListResult{Items: []proto.Message{r.object("a"), r.object("b")}, NextPageToken: "next", Revision: 7}, nil
}

func (r *recorder) Watch(_ context.Context, opts resource.WatchOptions, send func(resource.Event) error) error {
	r.locked(func() { r.watch = opts })
	if err := send(resource.Event{Type: metav1.EventType_EVENT_TYPE_ADDED, Object: r.object("a"), Revision: 8}); err != nil {
		return err
	}
	return send(resource.Event{Type: metav1.EventType_EVENT_TYPE_BOOKMARK, Revision: 9})
}

func (r *recorder) Apply(_ context.Context, obj proto.Message) (resource.ApplyResult, error) {
	r.locked(func() { r.applied = obj })
	return resource.ApplyResult{Object: obj, Warnings: []string{"w"}, Created: true}, nil
}

func (r *recorder) Create(_ context.Context, obj proto.Message) (resource.ApplyResult, error) {
	r.locked(func() { r.created = obj })
	return resource.ApplyResult{Object: obj, Created: true}, nil
}

func (r *recorder) Delete(_ context.Context, key resource.Key, expected *int64) (proto.Message, error) {
	r.record(key, expected)
	return r.object(key.Name), nil
}

func (r *recorder) UpdateStatus(_ context.Context, key resource.Key, status proto.Message, expected *int64) (proto.Message, error) {
	r.record(key, expected)
	r.locked(func() { r.status = status })
	return r.object(key.Name), nil
}

func serve(t *testing.T) (*httptest.Server, map[string]*recorder) {
	t.Helper()
	backends := map[string]*recorder{}
	mux := http.NewServeMux()
	servicegen.Mount(mux, func(r registry.Resource) resource.Backend {
		rec := &recorder{kind: r}
		backends[r.Kind] = rec
		return rec
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, backends
}

func TestProjectScopedKind(t *testing.T) {
	srv, backends := serve(t)
	rec := backends["Container"]
	client := computev1alpha1connect.NewContainerServiceClient(srv.Client(), srv.URL)
	ctx := t.Context()

	got, err := client.GetContainer(ctx, connect.NewRequest(&computev1alpha1.GetContainerRequest{Project: "p", Name: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetContainer().GetMetadata().GetName() != "web" {
		t.Errorf("Get returned %v", got.Msg)
	}
	if rec.keys[0] != (resource.Key{Project: "p", Name: "web"}) {
		t.Errorf("Get passed key %+v", rec.keys[0])
	}

	_, err = client.GetContainer(ctx, connect.NewRequest(&computev1alpha1.GetContainerRequest{Project: "p", Name: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("backend error not propagated: %v", err)
	}

	list, err := client.ListContainers(ctx, connect.NewRequest(&computev1alpha1.ListContainersRequest{
		Project: "p", LabelSelector: "a=b", PageSize: 2, PageToken: "t",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Msg.GetItems()) != 2 || list.Msg.GetNextPageToken() != "next" || list.Msg.GetRevision() != 7 {
		t.Errorf("List returned %v", list.Msg)
	}
	if rec.list != (resource.ListOptions{Project: "p", LabelSelector: "a=b", PageSize: 2, PageToken: "t"}) {
		t.Errorf("List passed %+v", rec.list)
	}

	stream, err := client.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{
		Project: "p", SinceRevision: 5,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var events []*computev1alpha1.WatchContainersResponse
	for stream.Receive() {
		events = append(events, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 ||
		events[0].GetType() != metav1.EventType_EVENT_TYPE_ADDED || events[0].GetContainer().GetMetadata().GetName() != "a" ||
		events[1].GetType() != metav1.EventType_EVENT_TYPE_BOOKMARK || events[1].GetContainer() != nil || events[1].GetRevision() != 9 {
		t.Errorf("Watch streamed %v", events)
	}
	if rec.watch != (resource.WatchOptions{Project: "p", SinceRevision: 5}) {
		t.Errorf("Watch passed %+v", rec.watch)
	}

	rv := int64(42)
	if _, err := client.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{
		Project: "p", Name: "web", ExpectedResourceVersion: &rv,
	})); err != nil {
		t.Fatal(err)
	}
	if last := rec.expected[len(rec.expected)-1]; last == nil || *last != 42 {
		t.Errorf("Delete passed expected version %v", last)
	}
	if _, err := client.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "p", Name: "web"})); err != nil {
		t.Fatal(err)
	}
	if last := rec.expected[len(rec.expected)-1]; last != nil {
		t.Errorf("Delete without a version passed %v", *last)
	}

	status := &computev1alpha1.ContainerStatus{ObservedGeneration: 3}
	if _, err := client.UpdateContainerStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateContainerStatusRequest{
		Project: "p", Name: "web", Status: status,
	})); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(rec.status, status) {
		t.Errorf("UpdateStatus passed %v", rec.status)
	}
}

func TestGlobalKindAndApply(t *testing.T) {
	srv, backends := serve(t)
	rec := backends["Tenant"]
	client := tenancyv1alpha1connect.NewTenantServiceClient(srv.Client(), srv.URL)
	ctx := t.Context()

	if _, err := client.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"})); err != nil {
		t.Fatal(err)
	}
	if rec.keys[0] != (resource.Key{Name: "acme"}) {
		t.Errorf("Get passed key %+v", rec.keys[0])
	}

	in := &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "acme"}, Spec: &tenancyv1alpha1.TenantSpec{DisplayName: "Acme"}}
	res, err := client.ApplyTenant(ctx, connect.NewRequest(&tenancyv1alpha1.ApplyTenantRequest{Tenant: in}))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(rec.applied, in) || !res.Msg.GetCreated() || len(res.Msg.GetWarnings()) != 1 {
		t.Errorf("Apply passed %v, returned %v", rec.applied, res.Msg)
	}
	if rec.created != nil {
		t.Errorf("Apply without create_only called Create with %v", rec.created)
	}

	rec.applied = nil
	if _, err := client.ApplyTenant(ctx, connect.NewRequest(&tenancyv1alpha1.ApplyTenantRequest{Tenant: in, CreateOnly: true})); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(rec.created, in) || rec.applied != nil {
		t.Errorf("create_only Apply called Create with %v and Apply with %v", rec.created, rec.applied)
	}

	_, err = client.ApplyTenant(ctx, connect.NewRequest(&tenancyv1alpha1.ApplyTenantRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("Apply without an object: %v", err)
	}
}
