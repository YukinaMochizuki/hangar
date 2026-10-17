package apiserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// errEndpointCredential is all hangar-edge hears of a code or credential
// that is not valid, whatever the reason; errEndpointDenied, of a valid code
// whose person may no longer use the endpoint.
var (
	errEndpointCredential = errors.New("the credential is not valid for this endpoint")
	errEndpointDenied     = errors.New("the person may no longer use this endpoint")
)

// EdgeHandler is Handler for the edge socket: it also serves
// EndpointAuthService, through which hangar-edge learns who calls a PRIVATE
// endpoint, to the Edge principal alone. No other listener serves it.
func (s *Server) EdgeHandler(chain auth.Chain) http.Handler {
	mux := s.mux(chain)
	mux.Handle(networkv1alpha1connect.NewEndpointAuthServiceHandler(endpointAuth{s}, connect.WithInterceptors(s.interceptor(chain))))
	return mux
}

// endpointVerdict says whether p may use the PRIVATE endpoint at host, for
// the console's endpoint-login: a host that is not exactly one endpoint's,
// or is a PUBLIC one's, which needs no handoff, is no endpoint at all.
func (s *Server) endpointVerdict(ctx context.Context, p auth.Principal, host string) (login.EndpointVerdict, error) {
	label, ok := strings.CutSuffix(host, "."+s.endpointDomain())
	if !ok || label == "" || strings.Contains(label, ".") {
		return login.EndpointUnknown, nil
	}
	all, err := s.endpoints(ctx, "")
	if err != nil {
		return login.EndpointUnknown, err
	}
	for _, ep := range all {
		m := ep.GetMetadata()
		if endpointLabel(m.GetProject(), m.GetName()) != label {
			continue
		}
		if m.GetDeleteTime() != nil || IsPublic(ep) {
			return login.EndpointUnknown, nil
		}
		if err := s.authz.Fresh(ctx); err != nil {
			return login.EndpointUnknown, err
		}
		if !s.authz.Allowed(p, authz.PublicEndpointAccess, authz.Target{Project: m.GetProject()}) {
			return login.EndpointDenied, nil
		}
		return login.EndpointAllowed, nil
	}
	return login.EndpointUnknown, nil
}

// endpointAuth serves EndpointAuthService.
type endpointAuth struct{ s *Server }

// edgeOnly refuses every caller but hangar-edge, with FAILED_PRECONDITION:
// a listener misconfigured, not a person denied, which is what
// PERMISSION_DENIED says to the edge.
func edgeOnly(ctx context.Context) error {
	if p, ok := auth.FromContext(ctx); !ok || p.Type != auth.Edge {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("only hangar-edge may ask"))
	}
	return nil
}

func (h endpointAuth) ExchangeEndpointCode(ctx context.Context, req *connect.Request[networkv1alpha1.ExchangeEndpointCodeRequest]) (*connect.Response[networkv1alpha1.ExchangeEndpointCodeResponse], error) {
	if err := edgeOnly(ctx); err != nil {
		return nil, err
	}
	if h.s.login == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("sign-in is off, so no one is handed off to an endpoint"))
	}
	b, p, err := h.s.login.ExchangeEndpoint(ctx, req.Msg.GetCode(), req.Msg.GetHost(), req.Msg.GetState())
	switch {
	case errors.Is(err, login.ErrInvalidCode):
		return nil, connect.NewError(connect.CodeUnauthenticated, errEndpointCredential)
	case errors.Is(err, login.ErrNotAdmitted), errors.Is(err, login.ErrEndpointDenied):
		return nil, connect.NewError(connect.CodePermissionDenied, errEndpointDenied)
	case err != nil:
		return nil, err
	}
	return connect.NewResponse(&networkv1alpha1.ExchangeEndpointCodeResponse{
		Session: b.Secret.Reveal(), ExpireTime: timestamppb.New(b.ExpiresAt), Principal: p.String(),
	}), nil
}

func (h endpointAuth) CheckEndpointCredential(ctx context.Context, req *connect.Request[networkv1alpha1.CheckEndpointCredentialRequest]) (*connect.Response[networkv1alpha1.CheckEndpointCredentialResponse], error) {
	if err := edgeOnly(ctx); err != nil {
		return nil, err
	}
	if h.s.sessions == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the apiserver keeps no credentials"))
	}
	s, host := token.Secret(req.Msg.GetCredential()), req.Msg.GetHost()
	var typ, audience string
	switch s.Prefix() {
	case token.Endpoint:
		typ, audience = session.TypeEndpoint, host
	case token.Access:
		typ = session.TypeAccess
	case token.Service:
		typ = session.TypeService
	default:
		return nil, connect.NewError(connect.CodeUnauthenticated, errEndpointCredential)
	}
	if host == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errEndpointCredential)
	}
	f, err := h.s.sessions.Check(ctx, s, typ, audience)
	if errors.Is(err, session.ErrInvalid) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errEndpointCredential)
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&networkv1alpha1.CheckEndpointCredentialResponse{
		Principal: f.Principal.String(), ExpireTime: timestamppb.New(f.Until),
	}), nil
}
