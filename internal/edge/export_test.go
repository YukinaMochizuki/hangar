package edge

import (
	"net/http"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// SetRegistering has f run between a request's first authorization and its
// registration among the active ones.
func SetRegistering(e *Edge, f func()) { e.registering = f }

// ConfineEndpointCookies is what the edge does to the Set-Cookie of an
// endpoint's answer.
func ConfineEndpointCookies(resp *http.Response) error { return confineEndpointCookies(resp) }

// MayUse reports whether e's view lets p use the endpoints of project.
func MayUse(e *Edge, p auth.Principal, project string) bool {
	return e.authz.Allowed(p, authz.PublicEndpointAccess, authz.Target{Project: project})
}
