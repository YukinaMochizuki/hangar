package node

import (
	"context"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// shareController lets go of what the host holds for the FileShares bound
// to this node once they are deleted, their volumes, and says so with
// Released, which the share's deletion waits for
// (docs/decisions/file-shares.md). Their servers make and grow them.
func (a *Agent) shareController() *controller.Controller {
	return &controller.Controller{
		Name:       "fileshares",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileShare),
		Watches:    []controller.Watch{{Kind: registry.FileShare.Kind, Map: a.shareIfMine}},
		Clock:      a.cfg.Clock,
		Log:        a.cfg.Log,
	}
}

func (a *Agent) shareIfMine(_ context.Context, o store.Object) []controller.Key {
	var fs storagev1alpha1.FileShare
	if proto.Unmarshal(o.Data, &fs) != nil || fs.GetStatus().GetNode() != a.cfg.Name {
		return nil
	}
	return []controller.Key{{Project: o.Project, Name: o.Name}}
}

// reconcileShare brings the host in line with one share bound to this node.
func (a *Agent) reconcileShare(ctx context.Context, k controller.Key) error {
	res, err := a.shares.GetFileShare(ctx, connect.NewRequest(&storagev1alpha1.GetFileShareRequest{Project: k.Project, Name: k.Name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	fs := res.Msg.GetFileShare()
	if fs.GetStatus().GetNode() != a.cfg.Name || fs.GetMetadata().GetDeleteTime() == nil ||
		conditions.IsTrue(fs.GetStatus().GetConditions(), computeapi.FileShareReleased) ||
		// Its server, which holds its volume, is not gone yet; the
		// finalizer going brings the share back here.
		slices.Contains(fs.GetMetadata().GetFinalizers(), computeapi.DependentsFinalizer) {
		return nil
	}
	// Only a release touches the host: a live share, every event of which
	// lands here, holds up no machine's reconcile.
	a.host.Lock()
	err = a.releaseShare(ctx, fs)
	a.host.Unlock()
	if err != nil {
		return err
	}
	// The apiserver keeps all of the status but the node's conditions.
	released := &metav1.Condition{
		Type: computeapi.FileShareReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned",
		Message: "the host holds nothing of the share", ObservedGeneration: fs.GetMetadata().GetGeneration(),
	}
	rv := fs.GetMetadata().GetResourceVersion()
	_, err = a.shares.UpdateFileShareStatus(ctx, connect.NewRequest(&storagev1alpha1.UpdateFileShareStatusRequest{
		Project: k.Project, Name: k.Name, ExpectedResourceVersion: &rv,
		Status: &storagev1alpha1.FileShareStatus{Conditions: conditions.Set(fs.GetStatus().GetConditions(), released, a.cfg.Clock.Now())},
	}))
	return err
}

// releaseShare removes what the host holds for a deleted share, whose
// server is gone: its volume, which the storage refuses to destroy while a
// VM still has it.
func (a *Agent) releaseShare(ctx context.Context, fs *storagev1alpha1.FileShare) error {
	return a.cfg.Drivers.Storage.DeleteShareVolume(ctx, fs.GetMetadata().GetUid())
}
