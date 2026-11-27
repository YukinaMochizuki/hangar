package apiserver_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
)

// TestSignInCallsAreLogged: SessionService, which the public listener serves
// beside the API, writes an access line per call as the API does.
func TestSignInCallsAreLogged(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	w := newOIDCWorldConfig(t, 0, func(c *apiserver.Config) { c.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	c := iamv1alpha1connect.NewSessionServiceClient(w.tls, w.url)
	if _, err := c.StartDeviceLogin(context.Background(), connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{})); err != nil {
		t.Fatal(err)
	}
	_, err := c.Refresh(context.Background(), connect.NewRequest(&iamv1alpha1.RefreshRequest{}))
	if err == nil {
		t.Fatal("a refresh with no token succeeded")
	}
	for _, want := range []string{
		"msg=rpc procedure=" + iamv1alpha1connect.SessionServiceStartDeviceLoginProcedure + " code=ok",
		"msg=rpc procedure=" + iamv1alpha1connect.SessionServiceRefreshProcedure + " code=" + connect.CodeOf(err).String(),
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no access line %q in:\n%s", want, logs.String())
		}
	}
}
