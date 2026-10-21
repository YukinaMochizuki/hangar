package node

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"testing"

	"connectrpc.com/connect"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// readThen is a share client that runs between, after each read of a share,
// as another writer would between the node's read and its write.
type readThen struct {
	storagev1alpha1connect.FileShareServiceClient
	between func()
}

func (r readThen) GetFileShare(ctx context.Context, req *connect.Request[storagev1alpha1.GetFileShareRequest]) (*connect.Response[storagev1alpha1.GetFileShareResponse], error) {
	res, err := r.FileShareServiceClient.GetFileShare(ctx, req)
	if r.between != nil {
		r.between()
	}
	return res, err
}

// TestReleasingAShareIsConditional: the node's Released is written on the
// share as the node read it, so a condition its node wrote between the read
// and the write is never lost; the write is refused, and the next reconcile
// keeps both. The race is made, so the reconcile is called directly.
func TestReleasingAShareIsConditional(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	host := fake.New()
	a, err := New(Config{
		Name: apiservertest.Node, HTTP: client.UnixSocket(env.Socket), BaseURL: client.UnixSocketURL,
		Drivers: &Drivers{Images: host.Images(), Storage: host.Storage(), Network: host.Network(), Hypervisor: host.Hypervisor(), Guest: host.Guest()},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	key := client.Key{Project: apiservertest.Project, Name: "assets"}
	if _, err := env.As(apiservertest.Root).Apply(ctx, registry.FileShare, &storagev1alpha1.FileShare{
		Metadata: &metav1.ObjectMeta{Project: key.Project, Name: key.Name}, Spec: &storagev1alpha1.FileShareSpec{Size: "1Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	system := storagev1alpha1connect.NewFileShareServiceClient(http.DefaultClient, env.SystemURL)
	if _, err := system.UpdateFileShareStatus(ctx, connect.NewRequest(&storagev1alpha1.UpdateFileShareStatusRequest{
		Project: key.Project, Name: key.Name, Status: &storagev1alpha1.FileShareStatus{Node: apiservertest.Node, Size: "1Gi"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := env.As(apiservertest.Root).Delete(ctx, registry.FileShare, key); err != nil {
		t.Fatal(err)
	}
	get := func() *storagev1alpha1.FileShare {
		res, err := system.GetFileShare(ctx, connect.NewRequest(&storagev1alpha1.GetFileShareRequest{Project: key.Project, Name: key.Name}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetFileShare()
	}
	// Its server, never placed, goes at once, and with it dependents.
	testutil.Eventually(t, "the share waiting for its node alone", func() (bool, string) {
		f := get().GetMetadata().GetFinalizers()
		return len(f) == 1 && f[0] == registry.Domain+"/node-cleanup", fmt.Sprint(f)
	})

	mine := storagev1alpha1connect.NewFileShareServiceClient(client.UnixSocket(env.Socket), client.UnixSocketURL)
	ready := &metav1.Condition{Type: "Ready", Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Deleting"}
	once := false
	a.shares = readThen{FileShareServiceClient: mine, between: func() {
		if once {
			return
		}
		once = true
		if _, err := mine.UpdateFileShareStatus(ctx, connect.NewRequest(&storagev1alpha1.UpdateFileShareStatusRequest{
			Project: key.Project, Name: key.Name, Status: &storagev1alpha1.FileShareStatus{Conditions: []*metav1.Condition{ready}},
		})); err != nil {
			t.Error(err)
		}
	}}
	k := controller.Key{Project: key.Project, Name: key.Name}
	if err := a.reconcileShare(ctx, k); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("a Released written over a status changed since it was read: %v, want aborted", err)
	}
	if c := conditions.Find(get().GetStatus().GetConditions(), "Ready"); c.GetReason() != "Deleting" {
		t.Errorf("the condition written between the read and the write is %v", c)
	}
	if err := a.reconcileShare(ctx, k); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the share gone once released", func() (bool, string) {
		_, err := system.GetFileShare(ctx, connect.NewRequest(&storagev1alpha1.GetFileShareRequest{Project: key.Project, Name: key.Name}))
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
}

// TestAFirewallWrittenRequeuesWaitingMachines: a network's firewall written
// while a container waits for its shares' rules has that machine queued
// again, once; written with none waiting, nothing is.
func TestAFirewallWrittenRequeuesWaitingMachines(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	host := fake.New()
	a, err := New(Config{
		Name: apiservertest.Node, HTTP: client.UnixSocket(env.Socket), BaseURL: client.UnixSocketURL,
		Drivers: &Drivers{Images: host.Images(), Storage: host.Storage(), Network: host.Network(), Hypervisor: host.Hypervisor(), Guest: host.Guest()},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	k := controller.Key{Project: apiservertest.Project, Name: computeapi.DefaultNetwork}
	var uid string
	testutil.Eventually(t, "the default network", func() (bool, string) {
		n, err := a.getNetwork(ctx, k.Project, k.Name)
		uid = n.GetMetadata().GetUid()
		return err == nil && uid != "", fmt.Sprint(err)
	})
	if err := host.Network().EnsureNetwork(ctx, driver.NetworkSpec{UID: uid, CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")}); err != nil {
		t.Fatal(err)
	}
	var queued []controller.Key
	a.requeue = func(keys ...controller.Key) { queued = append(queued, keys...) }
	waiting := controller.Key{Project: apiservertest.Project, Name: "web"}
	a.waitForShares(waiting)
	if err := a.reconcileNetworkServices(ctx, k); err != nil || !slices.Equal(queued, []controller.Key{waiting}) {
		t.Errorf("written with a container waiting: %v, queued %v", err, queued)
	}
	queued = nil
	if err := a.reconcileNetworkServices(ctx, k); err != nil || queued != nil {
		t.Errorf("written with none waiting: %v, queued %v", err, queued)
	}
}
