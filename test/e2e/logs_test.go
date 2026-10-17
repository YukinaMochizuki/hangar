package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func (w *world) logsAs(email string) computev1alpha1connect.MachineCapabilityServiceClient {
	return computev1alpha1connect.NewMachineCapabilityServiceClient(apiservertest.DevUser(email), w.env.URL)
}

// readLogs reads a container's logs to their end, rendered as
// boot:stream:data lines, an exit as boot:exit:code.
func readLogs(ctx context.Context, c computev1alpha1connect.MachineCapabilityServiceClient, req *computev1alpha1.LogsRequest) ([]string, error) {
	stream, err := c.Logs(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var out []string
	for stream.Receive() {
		out = append(out, render(stream.Msg().GetRecords())...)
	}
	return out, stream.Err()
}

func render(recs []*computev1alpha1.MachineLogRecord) []string {
	var out []string
	for _, r := range recs {
		s := strings.ToLower(strings.TrimPrefix(r.GetStream().String(), "MACHINE_LOG_STREAM_"))
		if r.GetExit() != nil {
			out = append(out, fmt.Sprintf("%d:exit:%d", r.GetBoot(), r.GetExit().GetCode()))
			continue
		}
		out = append(out, fmt.Sprintf("%d:%s:%s", r.GetBoot(), s, strings.TrimSuffix(string(r.GetData()), "\n")))
	}
	return out
}

// follower follows a container's logs in the background.
type follower struct {
	mu     sync.Mutex
	lines  []string
	err    error
	done   chan struct{}
	cancel func()
}

func (w *world) follow(email, name string) *follower {
	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{done: make(chan struct{}), cancel: cancel}
	stream, err := w.logsAs(email).Logs(ctx, connect.NewRequest(&computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: name, Follow: true}))
	if err != nil {
		w.t.Fatal(err)
	}
	go func() {
		defer close(f.done)
		defer func() { _ = stream.Close() }()
		for stream.Receive() {
			f.mu.Lock()
			f.lines = append(f.lines, render(stream.Msg().GetRecords())...)
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.err = stream.Err()
		f.mu.Unlock()
	}()
	w.t.Cleanup(func() { cancel(); <-f.done })
	return f
}

func (f *follower) has(line string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.lines {
		if l == line {
			return true, ""
		}
	}
	return false, strings.Join(f.lines, " | ")
}

func (f *follower) ended(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(within):
		t.Fatal("the follow did not end")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func contains(lines []string, want ...string) bool {
	i := 0
	for _, l := range lines {
		if i < len(want) && l == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestContainerLogs: a container's output, read through the apiserver
// from the node's host over the node's tunnel, as one stream across its
// boots; a follow sees what comes as it comes, and a client going away
// ends the node's read.
func TestContainerLogs(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	if err := w.host.Print(uid, console.Stdout, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.host.Print(uid, console.Stderr, "oops\n"); err != nil {
		t.Fatal(err)
	}
	root := w.logsAs(apiservertest.Root)
	var lines []string
	testutil.Eventually(w.t, "the output in the logs", func() (bool, string) {
		var err error
		lines, err = readLogs(context.Background(), root, &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: "api"})
		return err == nil && contains(lines, "1:stdout:hello", "1:stderr:oops"), fmt.Sprint(lines, err)
	})
	if !strings.HasPrefix(lines[0], "1:init:") {
		t.Errorf("the log starts with %q, not hangar-init's", lines[0])
	}

	f := w.follow(apiservertest.Root, "api")
	testutil.Eventually(w.t, "the follow reading the log", func() (bool, string) { return f.has("1:stdout:hello") })
	if err := w.host.Exit(uid, 1); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the restart", func() (bool, string) { return w.host.Snapshot().VMs[uid].Boots == 2, "" })
	if err := w.host.Print(uid, console.Stdout, "again\n"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the follow seeing the exit and the next boot", func() (bool, string) {
		if ok, last := f.has("1:exit:1"); !ok {
			return false, last
		}
		return f.has("2:stdout:again")
	})
	if w.host.LogReaders() != 1 {
		t.Errorf("%d reads of logs under way, want the follow's", w.host.LogReaders())
	}
	f.cancel()
	_ = f.ended(t, 5*time.Second)
	testutil.Eventually(w.t, "the node's read ended with the client", func() (bool, string) {
		return w.host.LogReaders() == 0, fmt.Sprint(w.host.LogReaders())
	})

	tail, err := readLogs(context.Background(), root, &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: "api", TailLines: ptr(int64(1))})
	if err != nil || len(tail) != 1 || tail[0] != "2:stdout:again" {
		t.Errorf("a tail of one line: %q %v", tail, err)
	}

	w.delete("api")
	w.waitGone("api")
	if _, err := readLogs(context.Background(), root, &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: "api"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("the logs of a container deleted: %v", err)
	}
	w.waitHostEmpty()
}

func ptr[T any](v T) *T { return &v }

// TestAFollowEndsWithTheContainer: a container that exits for good ends
// its follow, once its exit is read.
func TestAFollowEndsWithTheContainer(t *testing.T) {
	w := newWorld(t)
	c := container("job", "nginx")
	c.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_NEVER
	w.apply(c)
	w.waitReason("job", "Running")
	f := w.follow(apiservertest.Root, "job")
	testutil.Eventually(w.t, "the follow reading the log", func() (bool, string) { return f.has("1:init:hangar-init: starting the container") })
	if err := w.host.Exit(w.machine("job").GetMetadata().GetUid(), 3); err != nil {
		t.Fatal(err)
	}
	if err := f.ended(t, 10*time.Second); err != nil {
		t.Errorf("the follow ended with %v", err)
	}
	if ok, lines := f.has("1:exit:3"); !ok {
		t.Errorf("the follow ended without the exit: %s", lines)
	}
}

// TestReadingLogsNeedsItsOwnPermission: the tenant's owner reads them;
// a sponsor, a member, and the owner of another tenant do not, and are
// told so the same whether the container exists or not; and a container
// not yet placed has none to read.
func TestReadingLogsNeedsItsOwnPermission(t *testing.T) {
	w := newIdleWorld(t)
	bind := func(name, user, role string, scope *iamv1alpha1.BindingScope) {
		t.Helper()
		if _, err := w.root.Apply(context.Background(), registry.RoleBinding, &iamv1alpha1.RoleBinding{
			Metadata: &metav1.ObjectMeta{Name: name},
			Spec: &iamv1alpha1.RoleBindingSpec{
				Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}}, Role: role, Scope: scope,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{"owner@example.com", "other@example.com", "member@example.com"} {
		w.env.AddMember(u, "platform.member")
	}
	w.env.AddMember("sponsor@example.com", "platform.sponsor")
	bind("owner", "owner@example.com", "tenant.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}})
	if _, err := w.root.Apply(context.Background(), registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	bind("other-owner", "other@example.com", "tenant.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "other"}})

	// The agent is not started: the machine is never placed.
	w.apply(container("api", "nginx"))
	req := func(name string) *computev1alpha1.LogsRequest {
		return &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: name}
	}
	testutil.Eventually(w.t, "the owner told the container has no machine yet", func() (bool, string) {
		_, err := readLogs(context.Background(), w.logsAs("owner@example.com"), req("api"))
		return connect.CodeOf(err) == connect.CodeFailedPrecondition, fmt.Sprint(err)
	})
	for _, user := range []string{"sponsor@example.com", "member@example.com", "other@example.com", "stranger@example.com"} {
		for _, name := range []string{"api", "absent"} {
			_, err := readLogs(context.Background(), w.logsAs(user), req(name))
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("%s reading %s's logs: %v, want denied", user, name, err)
			}
		}
	}
	// The owner reads the container itself as well.
	if _, err := w.env.As("owner@example.com").Get(context.Background(), registry.Container, client.Key{Project: project, Name: "api"}); err != nil {
		t.Errorf("the owner reading the container: %v", err)
	}
	if _, err := readLogs(context.Background(), w.logsAs("owner@example.com"), &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: "api", TailLines: ptr(int64(-1))}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a negative tail: %v", err)
	}

	w.startAgent()
	w.waitReason("api", "Running")
	if _, err := readLogs(context.Background(), w.logsAs("owner@example.com"), req("api")); err != nil {
		t.Errorf("the owner reading the logs: %v", err)
	}
}

// TestLogsOfANodeDown: a node with no tunnel, its agent stopped, fails a
// read at once with Unavailable, and a follow under way when it goes too.
func TestLogsOfANodeDown(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	f := w.follow(apiservertest.Root, "api")
	testutil.Eventually(w.t, "the follow reading the log", func() (bool, string) { return f.has("1:init:hangar-init: starting the container") })

	w.stopAgent()
	if err := f.ended(t, testutil.Wait); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("the follow ended with %v, want unavailable", err)
	}
	// Until the apiserver drops the node's tunnel, a read fails on it as
	// "dropped its connection", also unavailable.
	testutil.Eventually(w.t, "a read failing as unavailable, the node not connected", func() (bool, string) {
		start := time.Now()
		_, err := readLogs(context.Background(), w.logsAs(apiservertest.Root), &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: "api"})
		var ce *connect.Error
		return errors.As(err, &ce) && ce.Code() == connect.CodeUnavailable && strings.Contains(ce.Message(), "not connected") &&
			time.Since(start) < time.Second, fmt.Sprint(err)
	})

	// The node back, so are its logs.
	w.startAgent()
	testutil.Eventually(w.t, "the logs readable again", func() (bool, string) {
		_, err := readLogs(context.Background(), w.logsAs(apiservertest.Root), &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: "api"})
		return err == nil, fmt.Sprint(err)
	})
}

// TestATenantsCallsToNodesAreCapped: a tenant with as many calls open to
// nodes as it may is refused another at once, while another tenant's go
// through; a follow ending, or cancelled, gives its slot back.
func TestATenantsCallsToNodesAreCapped(t *testing.T) {
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.MaxNodeCallsPerTenant = 2 })
	w.startAgent()
	if _, err := w.root.Apply(context.Background(), registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.root.Apply(context.Background(), registry.Project, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "elsewhere"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "other"},
	}); err != nil {
		t.Fatal(err)
	}
	job := container("job", "nginx")
	job.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_NEVER
	w.apply(container("api", "nginx"))
	w.apply(job)
	theirs := container("theirs", "nginx")
	theirs.Metadata.Project = "elsewhere"
	w.apply(theirs)
	w.waitReason("api", "Running")
	w.waitReason("job", "Running")
	root := w.logsAs(apiservertest.Root)
	read := func(project, name string) error {
		_, err := readLogs(context.Background(), root, &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: name})
		return err
	}
	testutil.Eventually(w.t, "the other tenant's container readable", func() (bool, string) {
		err := read("elsewhere", "theirs")
		return err == nil, fmt.Sprint(err)
	})

	started := "1:init:hangar-init: starting the container"
	f1 := w.follow(apiservertest.Root, "api")
	f2 := w.follow(apiservertest.Root, "job")
	testutil.Eventually(w.t, "both follows open", func() (bool, string) {
		ok1, _ := f1.has(started)
		ok2, _ := f2.has(started)
		return ok1 && ok2, ""
	})
	start := time.Now()
	err := read(project, "api")
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), `tenant "acme"`) {
		t.Errorf("a third call of the tenant: %v, want resource exhausted", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the refusal took %v", took)
	}
	if err := read("elsewhere", "theirs"); err != nil {
		t.Errorf("another tenant's call: %v", err)
	}

	// The job exiting for good ends its follow, and frees its slot.
	if err := w.host.Exit(w.machine("job").GetMetadata().GetUid(), 0); err != nil {
		t.Fatal(err)
	}
	if err := f2.ended(t, 10*time.Second); err != nil {
		t.Errorf("the job's follow ended with %v", err)
	}
	testutil.Eventually(w.t, "a slot free once the follow ended", func() (bool, string) {
		err := read(project, "api")
		return err == nil, fmt.Sprint(err)
	})
	f3 := w.follow(apiservertest.Root, "api")
	testutil.Eventually(w.t, "the third follow open", func() (bool, string) { return f3.has(started) })
	if err := read(project, "api"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a call at the cap again: %v", err)
	}
	f1.cancel()
	_ = f1.ended(t, 5*time.Second)
	testutil.Eventually(w.t, "a slot free once the follow was cancelled", func() (bool, string) {
		err := read(project, "api")
		return err == nil, fmt.Sprint(err)
	})
}

// TestACallersCallsToNodesAreCappedAcrossTenants: a caller with as many
// calls open to nodes as one may, across tenants, is refused another
// whatever tenant it is in, while another caller in the same tenant is
// not; a call ending gives the slot back.
func TestACallersCallsToNodesAreCappedAcrossTenants(t *testing.T) {
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.MaxNodeCallsPerPrincipal = 2 })
	w.startAgent()
	ctx := context.Background()
	if _, err := w.root.Apply(ctx, registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.root.Apply(ctx, registry.Project, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "elsewhere"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "other"},
	}); err != nil {
		t.Fatal(err)
	}
	w.env.AddMember("owner@example.com", "platform.member")
	for name, b := range map[string]*iamv1alpha1.RoleBindingSpec{
		"owner": {Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "owner@example.com"}}, Role: "tenant.owner",
			Scope: &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}}},
	} {
		if _, err := w.root.Apply(ctx, registry.RoleBinding, &iamv1alpha1.RoleBinding{Metadata: &metav1.ObjectMeta{Name: name}, Spec: b}); err != nil {
			t.Fatal(err)
		}
	}
	w.apply(container("api", "nginx"))
	theirs := container("theirs", "nginx")
	theirs.Metadata.Project = "elsewhere"
	w.apply(theirs)
	w.waitReason("api", "Running")
	read := func(who, project, name string) error {
		_, err := readLogs(ctx, w.logsAs(who), &computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: project, Name: name})
		return err
	}
	testutil.Eventually(w.t, "the other tenant's container readable", func() (bool, string) {
		err := read(apiservertest.Root, "elsewhere", "theirs")
		return err == nil, fmt.Sprint(err)
	})

	started := "1:init:hangar-init: starting the container"
	f1 := w.follow(apiservertest.Root, "api")
	stream, err := w.logsAs(apiservertest.Root).Logs(ctx, connect.NewRequest(&computev1alpha1.LogsRequest{Kind: registry.Container.Kind, Project: "elsewhere", Name: "theirs", Follow: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() {
		t.Fatalf("the follow in the other tenant: %v", stream.Err())
	}
	testutil.Eventually(w.t, "the follow open", func() (bool, string) { return f1.has(started) })

	for _, where := range [][2]string{{project, "api"}, {"elsewhere", "theirs"}} {
		err := read(apiservertest.Root, where[0], where[1])
		if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "user:"+apiservertest.Root) {
			t.Errorf("root's third call, in %s: %v, want resource exhausted", where[0], err)
		}
	}
	testutil.Eventually(w.t, "the owner reading the logs", func() (bool, string) {
		err := read("owner@example.com", project, "api")
		return err == nil, fmt.Sprint(err)
	})
	f1.cancel()
	_ = f1.ended(t, 5*time.Second)
	testutil.Eventually(w.t, "root's slot back once its follow was cancelled", func() (bool, string) {
		err := read(apiservertest.Root, project, "api")
		return err == nil, fmt.Sprint(err)
	})
}
