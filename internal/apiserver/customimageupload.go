package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/uploadwire"
)

// uploadPattern is uploadwire.Path as the tail of an http.ServeMux
// pattern, its last segment the name with ":upload" after it.
const uploadPattern = "/v1/projects/{project}/customimages/{nameAction}"

// uploadPiece is the most of a chunk the apiserver holds at once: one
// message to the node.
const uploadPiece = 256 << 10

var chunkSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// uploadTarget is an uploaded CustomImage a caller may send chunks of,
// and the call to its node.
type uploadTarget struct {
	principal auth.Principal
	key       resource.Key
	ci        *storagev1alpha1.CustomImage
	call      *nodeCall
	// release ends the hold on a chunk's credential (auth.Chain.Hold).
	release func()
}

// reachUpload authenticates the request, authorizes uploading to the
// CustomImage it names, which is applying it, and refuses an image that
// takes no chunks, then begins the call to its node. Nothing about the
// image is told before the caller is authorized. A browser's session
// cookie counts only on a request of the console's origin
// (session.SameOrigin), as everywhere. A chunk is held as a Connect stream
// is (auth.Chain.Hold): its credential and its caller's right to apply the
// image are checked again as it goes, and a refusal ends it, and its call
// to the node, there.
func (s *Server) reachUpload(chain auth.Chain, r *http.Request) (_ context.Context, _ *uploadTarget, err error) {
	name, action, ok := strings.Cut(r.PathValue("nameAction"), ":")
	if !ok || action != uploadwire.Action {
		return nil, nil, connect.NewError(connect.CodeNotFound, errors.New("no such path"))
	}
	ctx, release := r.Context(), func() {}
	if r.Method == http.MethodPut {
		ctx, release, err = chain.Hold(ctx, r.Header, s.clock)
	} else {
		ctx, err = chain.Authenticate(ctx, r.Header)
	}
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	defer func() {
		if err != nil {
			release()
		}
	}()
	b := s.backend(registry.CustomImage)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	key := resource.Key{Project: r.PathValue("project"), Name: name}
	if err := b.checkKey(key); err != nil {
		return nil, nil, err
	}
	mayUpload := func(p auth.Principal) error {
		if !b.allowed(p, authz.Apply, b.target(key, nil)) {
			return denied("%s may not upload to %s", p, b.asked(key))
		}
		return nil
	}
	if err := mayUpload(p); err != nil {
		return nil, nil, err
	}
	auth.Recheck(ctx, func(ctx context.Context) error {
		p, err := b.begin(ctx)
		if err != nil {
			return err
		}
		return mayUpload(p)
	})
	obj, err := s.get(ctx, registry.CustomImage, key)
	if err != nil {
		return nil, nil, err
	}
	ci, ok := obj.(*storagev1alpha1.CustomImage)
	if !ok {
		return nil, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", b.asked(key)))
	}
	if err := s.awaitsUpload(ci); err != nil {
		return nil, nil, err
	}
	project, err := s.get(ctx, registry.Project, resource.Key{Name: key.Project})
	if err != nil {
		return nil, nil, err
	}
	if project == nil {
		return nil, nil, failedPrecondition("project %q is gone", key.Project)
	}
	call, err := s.callNode(ctx, p, project.(*tenancyv1alpha1.Project).GetSpec().GetTenant(), ci.GetStatus().GetNode(),
		fmt.Sprintf("custom image %q", key.Name))
	if err != nil {
		return nil, nil, err
	}
	return ctx, &uploadTarget{principal: p, key: key, ci: ci, call: call, release: release}, nil
}

// awaitsUpload refuses an image that takes no chunks: one not uploaded,
// being deleted, whose file is whole, that failed, or whose upload has
// expired.
func (s *Server) awaitsUpload(ci *storagev1alpha1.CustomImage) error {
	name := ci.GetMetadata().GetName()
	switch {
	case ci.GetSpec().GetSource().GetUpload() == nil:
		return failedPrecondition("custom image %q is not uploaded; it is captured from a disk", name)
	case ci.GetMetadata().GetDeleteTime() != nil:
		return failedPrecondition("custom image %q is being deleted", name)
	case computeapi.CustomImageFailed(ci):
		c := conditions.Find(ci.GetStatus().GetConditions(), computeapi.CustomImageReady)
		return failedPrecondition("custom image %q failed (%s: %s); delete it and upload it again", name, c.GetReason(), c.GetMessage())
	case conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageUploaded) || computeapi.CustomImageReadyNow(ci):
		return failedPrecondition("custom image %q has its whole file already", name)
	}
	if exp := ci.GetStatus().GetUploadExpires(); exp != nil && !s.clock.Now().Before(exp.AsTime()) {
		return failedPrecondition("the upload of custom image %q expired at %s; delete it and upload it again", name, exp.AsTime().UTC().Format("2006-01-02T15:04:05Z"))
	}
	return nil
}

// received asks the image's node how many bytes of its upload it holds.
func (x *uploadTarget) received(ctx context.Context) (int64, error) {
	res, err := x.call.nodes.GetCustomImageUpload(ctx, connect.NewRequest(&nodev1.GetCustomImageUploadRequest{ImageUid: x.ci.GetMetadata().GetUid()}))
	if err != nil {
		return 0, x.call.failed(err)
	}
	return res.Msg.GetReceivedBytes(), nil
}

// chunkError is what the node's refusal of a chunk, err, answers: where
// the upload stays, for a chunk not at the node's count (aborted), one
// sent while another of the upload is written (unavailable), or one whose
// bytes are not its sha256 (data_loss); otherwise, or if the node then
// does not answer, which makes an unavailable the tunnel's, err as any
// call to the node fails.
func (x *uploadTarget) chunkError(ctx context.Context, err error) error {
	code := connect.CodeOf(err)
	if code == connect.CodeAborted || code == connect.CodeUnavailable || code == connect.CodeDataLoss {
		if at, rerr := x.received(context.WithoutCancel(ctx)); rerr == nil {
			switch code {
			case connect.CodeAborted:
				return &uploadwire.OffsetError{ReceivedBytes: at}
			case connect.CodeUnavailable:
				return &uploadwire.BusyError{ReceivedBytes: at}
			}
			return &uploadwire.GarbledError{ReceivedBytes: at}
		}
	}
	return x.call.failed(err)
}

// customImageUpload serves an upload's path: GET says where the upload is,
// PUT sends one chunk.
func (s *Server) customImageUpload(chain auth.Chain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, x, err := s.reachUpload(chain, r)
		if err != nil {
			s.log.Debug("upload refused", "path", r.URL.Path, "method", r.Method, "error", err)
			uploadwire.WriteError(w, err)
			return
		}
		defer x.release()
		defer x.call.release()
		var state uploadwire.State
		if r.Method == http.MethodPut {
			state.ReceivedBytes, err = s.uploadChunk(ctx, w, r, x)
		} else {
			state.ReceivedBytes, err = x.received(ctx)
			state.ChunkSize, state.Size = uploadwire.ChunkSize, x.ci.GetSpec().GetSource().GetUpload().GetSize()
		}
		if err != nil {
			s.log.Debug("upload refused", "path", r.URL.Path, "method", r.Method, "principal", x.principal.String(), "error", err)
			uploadwire.WriteError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	}
}

// uploadChunk relays the request's body, one chunk, to the image's node,
// a message at a time, once its range and sha256 say it can count, and
// returns the bytes the node then holds. A recheck that refuses the caller
// ends it, the body's read and the node's call among it, with the refusal.
func (s *Server) uploadChunk(ctx context.Context, w http.ResponseWriter, r *http.Request, x *uploadTarget) (received int64, err error) {
	defer func() {
		if ended := auth.Ended(ctx); ended != nil && err != nil {
			received, err = 0, ended
		}
	}()
	size := x.ci.GetSpec().GetSource().GetUpload().GetSize()
	first, n, total, err := uploadwire.ParseContentRange(r.Header.Get("Content-Range"))
	switch {
	case err != nil:
		return 0, invalid("%v", err)
	case n > uploadwire.ChunkSize:
		return 0, uploadwire.TooLarge
	case total != size:
		return 0, invalid("content-range says the file is %d bytes; the image's is %d", total, size)
	case r.ContentLength >= 0 && r.ContentLength != n:
		return 0, invalid("the body is %d bytes; content-range says %d", r.ContentLength, n)
	}
	sum := r.Header.Get(uploadwire.ChunkSHA256)
	if !chunkSHA256.MatchString(sum) {
		return 0, invalid("%s %q is not 64 lowercase hex digits", uploadwire.ChunkSHA256, sum)
	}
	at, err := x.received(ctx)
	if err != nil {
		return 0, err
	}
	if first != at {
		return 0, &uploadwire.OffsetError{ReceivedBytes: at}
	}
	// Ended by any failure here, so the node counts nothing of a chunk not
	// all relayed.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Ended too by a recheck refusing the caller, which also cuts short a
	// read of a body that comes slowly.
	defer context.AfterFunc(ctx, func() {
		if auth.Ended(ctx) != nil {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		}
	})()
	stream := x.call.nodes.UploadCustomImage(ctx)
	if err := stream.Send(&nodev1.UploadCustomImageRequest{Message: &nodev1.UploadCustomImageRequest_Chunk{Chunk: &nodev1.UploadChunk{
		ImageUid: x.ci.GetMetadata().GetUid(), Offset: first, Length: n, Sha256: sum,
	}}}); err != nil && !errors.Is(err, io.EOF) {
		return 0, x.call.failed(err)
	}
	buf := make([]byte, uploadPiece)
	body := io.LimitReader(r.Body, n)
	var sent int64
	for sent < n {
		k, rerr := io.ReadFull(body, buf[:min(int64(len(buf)), n-sent)])
		if k > 0 {
			if err := stream.Send(&nodev1.UploadCustomImageRequest{Message: &nodev1.UploadCustomImageRequest_Data{Data: buf[:k]}}); err != nil {
				// The node answered already; its answer says why.
				break
			}
			sent += int64(k)
		}
		if rerr != nil {
			if sent < n {
				return 0, invalid("the body ended after %d of the chunk's %d bytes", sent, n)
			}
			break
		}
	}
	if sent == n {
		if extra, _ := r.Body.Read(make([]byte, 1)); extra > 0 {
			return 0, invalid("the body holds more than the chunk's %d bytes", n)
		}
	}
	res, err := stream.CloseAndReceive()
	if err != nil {
		return 0, x.chunkError(ctx, err)
	}
	return res.Msg.GetReceivedBytes(), nil
}
