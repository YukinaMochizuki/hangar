package node

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
)

// pullCredentials are the machine's registry logins, asked of the apiserver
// each time the Images driver needs one, and kept nowhere: not on disk, not
// in a log, not past the pull (docs/decisions/secrets.md).
func (a *Agent) pullCredentials(m *computev1alpha1.Machine) driver.Credentials {
	project, name := m.GetMetadata().GetProject(), m.GetMetadata().GetName()
	return func(ctx context.Context, registry string) (driver.Login, error) {
		// A registry with no host name is matched by no login.
		host, _ := secrets.RegistryHost(registry)
		res, err := a.pullCreds.ResolveImagePullCredentials(ctx, connect.NewRequest(&nodev1.ResolveImagePullCredentialsRequest{Project: project, Machine: name}))
		if err != nil {
			// The error is the apiserver's, which holds no login.
			return driver.Login{}, fmt.Errorf("the image pull credentials of machine %s: %w", name, err)
		}
		// A login is for its registry alone; the apiserver chooses it by
		// the machine's image, so another registry gets none.
		if host == "" || res.Msg.GetRegistry() != host {
			return driver.Login{}, nil
		}
		return driver.Login{Username: res.Msg.GetUsername(), Password: res.Msg.GetPassword(), IdentityToken: res.Msg.GetIdentityToken()}, nil
	}
}
