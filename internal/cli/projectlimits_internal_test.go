package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// racedProjects serves one project and, for each of the first `races`
// applies, plays another writer: it changes the display name and moves the
// resource version first, so the apply meets a newer project and is
// refused, as the apiserver refuses it.
type racedProjects struct {
	tenancyv1alpha1connect.UnimplementedProjectServiceHandler
	mu      sync.Mutex
	stored  *tenancyv1alpha1.Project
	races   int
	applies int
}

func (f *racedProjects) GetProject(context.Context, *connect.Request[tenancyv1alpha1.GetProjectRequest]) (*connect.Response[tenancyv1alpha1.GetProjectResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&tenancyv1alpha1.GetProjectResponse{Project: proto.Clone(f.stored).(*tenancyv1alpha1.Project)}), nil
}

func (f *racedProjects) ApplyProject(_ context.Context, req *connect.Request[tenancyv1alpha1.ApplyProjectRequest]) (*connect.Response[tenancyv1alpha1.ApplyProjectResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies++
	if f.races > 0 {
		f.races--
		f.stored.Spec.DisplayName += "+"
		f.stored.Metadata.ResourceVersion++
	}
	p := req.Msg.GetProject()
	if p.GetMetadata().GetResourceVersion() != f.stored.GetMetadata().GetResourceVersion() {
		return nil, connect.NewError(connect.CodeAborted, errors.New("store: revision conflict"))
	}
	f.stored = proto.Clone(p).(*tenancyv1alpha1.Project)
	f.stored.Metadata.ResourceVersion++
	return connect.NewResponse(&tenancyv1alpha1.ApplyProjectResponse{Project: f.stored}), nil
}

func serveProjects(t *testing.T, f *racedProjects) *client.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(tenancyv1alpha1connect.NewProjectServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.Client(), srv.URL)
}

func webProject() *tenancyv1alpha1.Project {
	return &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web", ResourceVersion: 1},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", DisplayName: "Web", Limits: &tenancyv1alpha1.Limits{Memory: proto.String("8Gi")}},
	}
}

// TestApplyLimitChangeRetries: a change refused because the project changed
// meanwhile is made again on the project read anew, keeping what the other
// writer changed and the limits not named.
func TestApplyLimitChangeRetries(t *testing.T) {
	f := &racedProjects{stored: webProject(), races: 2}
	res, err := applyLimitChange(t.Context(), serveProjects(t, f), "web", map[protoreflect.Name]string{"cpu": "4"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.applies != 3 {
		t.Errorf("applied %d times, want 3", f.applies)
	}
	p := res.Object.(*tenancyv1alpha1.Project)
	if p.GetSpec().GetDisplayName() != "Web++" || p.GetSpec().GetLimits().GetCpu() != "4" || p.GetSpec().GetLimits().GetMemory() != "8Gi" {
		t.Errorf("stored %v", p.GetSpec())
	}
}

// TestApplyLimitChangeGivesUp: a project that changes before every apply is
// left as the other writer made it after limitChangeTries applies, and the
// error says to run the command again, as a conflict.
func TestApplyLimitChangeGivesUp(t *testing.T) {
	f := &racedProjects{stored: webProject(), races: 1000}
	_, err := applyLimitChange(t.Context(), serveProjects(t, f), "web", map[protoreflect.Name]string{"cpu": "4"}, nil)
	if f.applies != limitChangeTries {
		t.Errorf("applied %d times, want %d", f.applies, limitChangeTries)
	}
	if exitCode(err) != ExitConflict || !strings.Contains(err.Error(), "run the command again") || connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("error %v, exit %d", err, exitCode(err))
	}
	if f.stored.GetSpec().GetLimits().Cpu != nil {
		t.Errorf("the change was applied: %v", f.stored.GetSpec())
	}
}

// TestApplyLimitChangeUnsetsToNoField: unsetting every limit sends no
// limits field, as a project that never had any is stored.
func TestApplyLimitChangeUnsetsToNoField(t *testing.T) {
	f := &racedProjects{stored: webProject()}
	if _, err := applyLimitChange(t.Context(), serveProjects(t, f), "web", nil, []protoreflect.Name{"memory"}); err != nil {
		t.Fatal(err)
	}
	if f.stored.GetSpec().Limits != nil {
		t.Errorf("limits stored as %v, want none", f.stored.GetSpec().Limits)
	}
}
