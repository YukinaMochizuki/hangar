package node

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
)

func (s nodeService) UploadCustomImage(context.Context, *connect.ClientStream[nodev1.UploadCustomImageRequest]) (*connect.Response[nodev1.UploadCustomImageResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this node takes no uploads"))
}

func (s nodeService) GetCustomImageUpload(context.Context, *connect.Request[nodev1.GetCustomImageUploadRequest]) (*connect.Response[nodev1.GetCustomImageUploadResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this node takes no uploads"))
}
