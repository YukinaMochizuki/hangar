package apiserver

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

// dialPath opens a capability's WebSocket at path as user, with the
// subprotocol, failing as the server refuses it.
func dialPath(t *testing.T, url, user, path, subprotocol string) error {
	t.Helper()
	c, resp, err := websocket.Dial(t.Context(), url+path, &websocket.DialOptions{
		HTTPHeader:   http.Header{auth.DevUserHeader: []string{user}},
		Subprotocols: []string{subprotocol},
	})
	if err == nil {
		_ = c.CloseNow()
		return nil
	}
	if resp == nil {
		t.Fatalf("dialing %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return execws.ReadError(resp)
}

// TestACapabilityAKindLacksIsUnimplemented: exec on a virtual machine and
// ssh to a container fail with Unimplemented for every platform member,
// whatever they may do in the project, from the kind alone, whether the
// object exists or not; a kind that runs as no machine, or none at all, is
// InvalidArgument. One signed in who is no member is refused as at every
// call, before the kind's capabilities are looked at, a kind without a
// machine included; only a kind name that is none is InvalidArgument for
// them too, as it says nothing of any object.
// Nothing about the object is told, and no node is called.
func TestACapabilityAKindLacksIsUnimplemented(t *testing.T) {
	w := newExecWorld(t, nil)
	w.connectNode(t)
	const stranger = "stranger@example.com"
	for _, user := range []string{"alice@example.com", "carol@example.com", "bob@example.com", stranger} {
		lacking, noMachine := connect.CodeUnimplemented, connect.CodeInvalidArgument
		if user == stranger {
			lacking, noMachine = connect.CodePermissionDenied, connect.CodePermissionDenied
		}
		for _, tc := range []struct {
			what, path, subprotocol string
			code                    connect.Code
		}{
			{"exec on a VM", execws.Path("web", "virtualmachines", "web"), execws.Subprotocol, lacking},
			{"exec on a VM that is not there", execws.Path("web", "virtualmachines", "nope"), execws.Subprotocol, lacking},
			{"ssh to a container", sshws.Path("web", "containers", "api"), sshws.Subprotocol, lacking},
			{"exec on a network", execws.Path("web", "networks", "default"), execws.Subprotocol, noMachine},
			{"exec on no kind", execws.Path("web", "gadgets", "api"), execws.Subprotocol, connect.CodeInvalidArgument},
			{"an action that is none", execws.CapabilityPath("web", "containers", "api", "attach"), execws.Subprotocol, connect.CodeNotFound},
		} {
			err := dialPath(t, w.url, user, tc.path, tc.subprotocol)
			wantCodeOf(t, err, tc.code, user+": "+tc.what)
			if connect.CodeOf(err) == connect.CodeUnimplemented &&
				err.Error() != "unimplemented: exec is not supported on virtual machines yet" &&
				err.Error() != "unimplemented: ssh is not supported on containers yet" {
				t.Errorf("%s: %s: told %v", user, tc.what, err)
			}
			if user == stranger && connect.CodeOf(err) == connect.CodePermissionDenied && !strings.Contains(err.Error(), "not a platform member") {
				t.Errorf("%s: %s: told %v", user, tc.what, err)
			}
		}

		p := auth.Principal{Type: auth.User, ID: user}
		srv := w.url
		client := computev1alpha1connect.NewMachineCapabilityServiceClient(&http.Client{Transport: devAs{user}}, srv)
		_, err := client.SignSSHKey(t.Context(), connect.NewRequest(&computev1alpha1.SignSSHKeyRequest{
			Project: "web", Kind: registry.Container.Kind, Name: "api", PublicKey: "ssh-ed25519 AAAA",
		}))
		wantCodeOf(t, err, lacking, user+": signing a key for a container")
		wantCodeOf(t, machineLogsOf(t, w.s, p, "Network", "api"), noMachine, user+": logs of a network")
		for _, kind := range []string{"Gadget", "", "container"} {
			wantCodeOf(t, machineLogsOf(t, w.s, p, kind, "api"), connect.CodeInvalidArgument, user+": logs of kind "+kind)
		}
	}
	select {
	case start := <-w.node.starts:
		t.Errorf("the node was called: %v", start)
	default:
	}
}

// devAs authenticates every request as user, as auth.Dev reads it.
type devAs struct{ user string }

func (d devAs) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(auth.DevUserHeader, d.user)
	return http.DefaultTransport.RoundTrip(r)
}

// TestACapabilityIsItsMachinesPermissionAlone: one who holds
// compute.machines.logs and nothing else reads a container's logs without
// being able to get it, and may not exec in it; reading the container is
// not reading its logs.
func TestACapabilityIsItsMachinesPermissionAlone(t *testing.T) {
	const logsOnly = "test.logs-only"
	w := newExecWorld(t, func(c *Config) {
		c.ExtraRoles[logsOnly] = authz.NewRole(authz.TenantLevel, authz.MachineLogs)
	})
	addMember(t, w.s, "dave@example.com")
	if _, err := w.s.backend(registry.RoleBinding).Apply(auth.WithPrincipal(t.Context(), auth.SystemPrincipal),
		binding("logs-dave", "dave@example.com", logsOnly, &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}})); err != nil {
		t.Fatal(err)
	}
	dave := auth.Principal{Type: auth.User, ID: "dave@example.com"}
	// No node is connected: the call gets as far as the node.
	wantCodeOf(t, logsOf(t, w.s, dave, "api"), connect.CodeUnavailable, "dave reading the logs")
	_, err := w.s.backend(registry.Container).Get(auth.WithPrincipal(t.Context(), dave), resource.Key{Project: "web", Name: "api"})
	wantCodeOf(t, err, connect.CodePermissionDenied, "dave getting the container")
	wantCodeOf(t, dialPath(t, w.url, "dave@example.com", execws.Path("web", "containers", "api"), execws.Subprotocol),
		connect.CodePermissionDenied, "dave execing")
	wantCodeOf(t, dialPath(t, w.url, "dave@example.com", execws.Path("web", "containers", "nope"), execws.Subprotocol),
		connect.CodePermissionDenied, "dave execing in a container that is not there")
}

// TestAUserCannotWriteAMachine: the capabilities are permissions on the
// Machine kind, and the tenant's owner holds them, but a Machine stays the
// system's to write.
func TestAUserCannotWriteAMachine(t *testing.T) {
	w := newExecWorld(t, nil)
	alice := auth.WithPrincipal(t.Context(), auth.Principal{Type: auth.User, ID: "alice@example.com"})
	machines := w.s.backend(registry.Machine)
	m, err := machines.Get(alice, resource.Key{Project: "web", Name: machineName("api")})
	if err != nil {
		t.Fatalf("the owner getting the machine: %v", err)
	}
	changed := proto.Clone(m).(*computev1alpha1.Machine)
	changed.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	_, err = machines.Apply(alice, changed)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the owner applying the machine")
	_, err = machines.Apply(alice, machine("ct-other", "n1"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "the owner making a machine")
	_, err = machines.Delete(alice, resource.Key{Project: "web", Name: machineName("api")}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the owner deleting the machine")
	if _, err := machines.Get(alice, resource.Key{Project: "web", Name: machineName("api")}); err != nil {
		t.Errorf("the machine after: %v", err)
	}
}

// TestEveryMachineKindIsMapped: the kinds that run as machines are the
// ones the table names, each with its machine's name and at least one
// capability; and a capability's target is computed from the key alone, so
// it is authorized before anything is read.
func TestEveryMachineKindIsMapped(t *testing.T) {
	var kinds []string
	for kind, mk := range machineKinds {
		kinds = append(kinds, kind)
		if mk.what == "" || mk.machine == nil || len(mk.capabilities) == 0 {
			t.Errorf("%s is mapped to %+v", kind, mk)
		}
		for _, v := range mk.capabilities {
			if !slices.Contains([]authz.Verb{authz.Logs, authz.Exec, authz.SSH}, v) {
				t.Errorf("%s has the capability %s, which is none", kind, v)
			}
		}
	}
	slices.Sort(kinds)
	if want := []string{registry.Container.Kind, registry.VirtualMachine.Kind}; !slices.Equal(kinds, want) {
		t.Errorf("the kinds that run as machines are %v, want %v", kinds, want)
	}
	if hooksFor(registry.Machine).targetNeedsObject {
		t.Error("a machine's target needs the object")
	}
}

// TestMachineLogRecords: a container's machine's records keep their
// streams and exits; a virtual machine's, which boots firmware, are its
// serial console, and only a boot the platform ended is stopped.
func TestMachineLogRecords(t *testing.T) {
	now := timestamppb.New(time.Unix(1700000000, 0))
	data := func(stream nodev1.LogStream, s string) *nodev1.LogRecord {
		return &nodev1.LogRecord{Boot: 1, Stream: stream, Time: now, Content: &nodev1.LogRecord_Data{Data: []byte(s)}}
	}
	exit := func(code, signal int32) *nodev1.LogRecord {
		return &nodev1.LogRecord{Boot: 1, Stream: nodev1.LogStream_LOG_STREAM_EXIT, Time: now, Content: &nodev1.LogRecord_Exit{Exit: &nodev1.ExitStatus{Code: code, Signal: signal}}}
	}
	wantData := func(stream computev1alpha1.MachineLogStream, s string) *computev1alpha1.MachineLogRecord {
		return &computev1alpha1.MachineLogRecord{Boot: 1, Stream: stream, Time: now, Content: &computev1alpha1.MachineLogRecord_Data{Data: []byte(s)}}
	}
	wantExit := func(st *computev1alpha1.MachineExitStatus) *computev1alpha1.MachineLogRecord {
		return &computev1alpha1.MachineLogRecord{Boot: 1, Stream: computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_EXIT, Time: now,
			Content: &computev1alpha1.MachineLogRecord_Exit{Exit: st}}
	}
	for _, tc := range []struct {
		what    string
		console bool
		in      []*nodev1.LogRecord
		want    []*computev1alpha1.MachineLogRecord
	}{
		{"a container's", false, []*nodev1.LogRecord{
			data(nodev1.LogStream_LOG_STREAM_STDOUT, "out"),
			data(nodev1.LogStream_LOG_STREAM_STDERR, "err"),
			data(nodev1.LogStream_LOG_STREAM_INIT, "init"),
			exit(137, 9),
			data(nodev1.LogStream_LOG_STREAM_UNSPECIFIED, "dropped"),
		}, []*computev1alpha1.MachineLogRecord{
			wantData(computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_STDOUT, "out"),
			wantData(computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_STDERR, "err"),
			wantData(computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_INIT, "init"),
			wantExit(&computev1alpha1.MachineExitStatus{Code: 137, Signal: 9}),
		}},
		{"a VM's", true, []*nodev1.LogRecord{
			data(nodev1.LogStream_LOG_STREAM_STDOUT, "login:"),
			exit(0, 0),
			exit(253, 15),
		}, []*computev1alpha1.MachineLogRecord{
			wantData(computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_CONSOLE, "login:"),
			wantExit(&computev1alpha1.MachineExitStatus{Code: 0}),
			wantExit(&computev1alpha1.MachineExitStatus{Code: 253, Stopped: true}),
		}},
	} {
		got := machineLogRecords(tc.console, tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s records: %v, want %v", tc.what, got, tc.want)
			continue
		}
		for i := range got {
			if !proto.Equal(got[i], tc.want[i]) {
				t.Errorf("%s record %d: %v, want %v", tc.what, i, got[i], tc.want[i])
			}
		}
	}
}
