package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
)

var chunkSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// UploadCustomImage writes one chunk of an upload the node takes, as its
// CustomImage's controller has it take, to the stager.
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

func (s nodeService) GetCustomImageUpload(_ context.Context, req *connect.Request[nodev1.GetCustomImageUploadRequest]) (*connect.Response[nodev1.GetCustomImageUploadResponse], error) {
	if s.a.stager == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this node takes no uploads"))
	}
	n, err := s.a.stager.received(req.Msg.GetImageUid())
	if err != nil {
		return nil, uploadError(err)
	}
	return connect.NewResponse(&nodev1.GetCustomImageUploadResponse{ReceivedBytes: n}), nil
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
