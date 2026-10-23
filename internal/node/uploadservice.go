package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

var chunkSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// UploadCustomImage writes one chunk of an upload the node takes, as its
// CustomImage's controller, or the apiserver's ask of where the upload is
// (GetCustomImageUpload), which comes first, has it take, to the stager.
func (s nodeService) UploadCustomImage(ctx context.Context, stream *connect.ClientStream[nodev1.UploadCustomImageRequest]) (*connect.Response[nodev1.UploadCustomImageResponse], error) {
	if s.a.stager == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this node takes no uploads"))
	}
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the first request must say where the chunk goes"))
	}
	c := stream.Msg().GetChunk()
	if c == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the first request must say where the chunk goes"))
	}
	if !chunkSHA.MatchString(c.GetSha256()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the chunk's sha256 is not 64 lowercase hex digits"))
	}
	n, err := s.a.stager.write(ctx, c.GetImageUid(), c.GetOffset(), c.GetLength(), c.GetSha256(), &chunkReader{stream: stream})
	if err != nil {
		return nil, uploadError(err)
	}
	return connect.NewResponse(&nodev1.UploadCustomImageResponse{ReceivedBytes: n}), nil
}

func (s nodeService) GetCustomImageUpload(ctx context.Context, req *connect.Request[nodev1.GetCustomImageUploadRequest]) (*connect.Response[nodev1.GetCustomImageUploadResponse], error) {
	if s.a.stager == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this node takes no uploads"))
	}
	if err := s.a.expectUpload(ctx, req.Msg.GetImageUid()); err != nil {
		return nil, uploadError(err)
	}
	n, err := s.a.stager.received(req.Msg.GetImageUid())
	if err != nil {
		return nil, uploadError(err)
	}
	return connect.NewResponse(&nodev1.GetCustomImageUploadResponse{ReceivedBytes: n}), nil
}

// expectUpload has the stager take uid's upload if the CustomImage of uid
// lives on this node and awaits its file, as the custom images'
// controller has it do, though it may not have seen the image yet: a
// client sends the first chunk as soon as the image is made.
func (a *Agent) expectUpload(ctx context.Context, uid string) error {
	if _, err := a.stager.received(uid); !errors.Is(err, errNotExpected) {
		return nil
	}
	objs, _, err := a.api.ListAll(ctx, registry.CustomImage, client.ListOptions{})
	if err != nil {
		return err
	}
	for _, o := range objs {
		ci := o.(*storagev1alpha1.CustomImage)
		up := ci.GetSpec().GetSource().GetUpload()
		if ci.GetMetadata().GetUid() != uid || up == nil || ci.GetStatus().GetNode() != a.cfg.Name {
			continue
		}
		if ci.GetMetadata().GetDeleteTime() == nil && !computeapi.CustomImageFailed(ci) && !computeapi.CustomImageReadyNow(ci) &&
			!conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageUploaded) {
			a.stager.expect(uid, up.GetSize())
		}
	}
	return nil
}

// uploadError is the answer to a chunk, or an ask, the stager refused.
func uploadError(err error) error {
	var off *offsetError
	switch {
	case errors.As(err, &off):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, errChunkBusy):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, errNotExpected):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, errChunkDigest), errors.Is(err, errChunkShape):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, errNoRoom):
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("staging the chunk: %w", err))
}

// chunkReader reads the bytes of a chunk's later requests.
type chunkReader struct {
	stream *connect.ClientStream[nodev1.UploadCustomImageRequest]
	buf    []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		if r.stream.Msg().GetChunk() != nil {
			return 0, fmt.Errorf("%w: only its first request says where it goes", errChunkShape)
		}
		r.buf = r.stream.Msg().GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
