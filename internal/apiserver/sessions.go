package apiserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// OIDCConfig configures sign-in through an OpenID Connect provider.
type OIDCConfig struct {
	// Issuer is the provider, such as https://accounts.google.com.
	Issuer       string
	ClientID     string
	ClientSecret token.Secret
	// PublicURL is the console's origin, which the provider returns to.
	PublicURL string
	// HTTPClient calls the provider; nil is a default one.
	HTTPClient *http.Client
	// DeviceInterval is how often a CLI may poll a device login; zero is
	// login.DefaultDeviceInterval.
	DeviceInterval time.Duration
	// MaxPending caps the sign-ins in progress; zero is
	// login.DefaultMaxPending.
	MaxPending int
}

// EnableOIDC turns on sign-in through the provider, keeping what it issues
// in creds, and returns the chain that authenticates what it issues: CLI
// access tokens, then browser session cookies. Call it before PublicHandler
// and Run.
func (s *Server) EnableOIDC(cfg OIDCConfig, creds store.Credentials) (auth.Chain, error) {
	s.sessions = session.New(creds, session.Options{Now: s.clock.Now, Log: s.log, Admit: s.admitted})
	flow, err := login.New(login.Config{
		Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, PublicURL: cfg.PublicURL,
		Sessions: s.sessions, Store: creds, Admit: s.admitted, HTTPClient: cfg.HTTPClient, DeviceInterval: cfg.DeviceInterval, MaxPending: cfg.MaxPending, Log: s.log,
	})
	if err != nil {
		return nil, err
	}
	s.login = flow
	return auth.Chain{session.Bearer{M: s.sessions}, session.Cookie{M: s.sessions, Origin: flow.Origin()}}, nil
}

// admitted says whether a person may sign in: whether they hold a platform
// role, the invitation an administrator makes before they first sign in.
func (s *Server) admitted(ctx context.Context, p auth.Principal) (bool, error) {
	if err := s.authz.Fresh(ctx); err != nil {
		return false, err
	}
	return s.authz.IsInvited(p), nil
}

// PublicHandler is Handler for the public listener: with OIDC on, it also
// serves the sign-in pages and SessionService. Every SessionService call but
// RevokeSessions carries its own credential, so the service authenticates
// that one call itself, with chain.
func (s *Server) PublicHandler(chain auth.Chain) http.Handler {
	mux := s.mux(chain)
	if s.login != nil {
		s.login.Mount(mux)
		mux.Handle(iamv1alpha1connect.NewSessionServiceHandler(sessions{s: s, chain: chain}))
	}
	return mux
}

// AdminHandler is Handler for the admin socket: with OIDC on, root on the
// host may also revoke anyone's logins, and do nothing else of sign-in.
func (s *Server) AdminHandler(chain auth.Chain) http.Handler {
	mux := s.mux(chain)
	if s.login != nil {
		h := sessions{s: s, chain: chain}
		mux.Handle(iamv1alpha1connect.SessionServiceRevokeSessionsProcedure,
			connect.NewUnaryHandler(iamv1alpha1connect.SessionServiceRevokeSessionsProcedure, h.RevokeSessions))
	}
	return mux
}

// sessions serves SessionService.
type sessions struct {
	s     *Server
	chain auth.Chain
}

func (h sessions) ExchangeCode(ctx context.Context, req *connect.Request[iamv1alpha1.ExchangeCodeRequest]) (*connect.Response[iamv1alpha1.ExchangeCodeResponse], error) {
	t, err := h.s.login.ExchangeCode(ctx, req.Msg.GetCode(), req.Msg.GetCodeVerifier(), req.Msg.GetRedirectUri())
	if err != nil {
		return nil, h.error(err)
	}
	return connect.NewResponse(&iamv1alpha1.ExchangeCodeResponse{Tokens: tokensProto(t)}), nil
}

func (h sessions) Refresh(ctx context.Context, req *connect.Request[iamv1alpha1.RefreshRequest]) (*connect.Response[iamv1alpha1.RefreshResponse], error) {
	t, err := h.s.sessions.Refresh(ctx, token.Secret(req.Msg.GetRefreshToken()))
	if err != nil {
		return nil, h.error(err)
	}
	return connect.NewResponse(&iamv1alpha1.RefreshResponse{Tokens: tokensProto(t)}), nil
}

func (h sessions) StartDeviceLogin(ctx context.Context, _ *connect.Request[iamv1alpha1.StartDeviceLoginRequest]) (*connect.Response[iamv1alpha1.StartDeviceLoginResponse], error) {
	d, err := h.s.login.StartDevice(ctx)
	if err != nil {
		return nil, h.error(err)
	}
	return connect.NewResponse(&iamv1alpha1.StartDeviceLoginResponse{
		DeviceCode: d.DeviceCode.Reveal(), UserCode: d.UserCode,
		VerificationUri: d.VerificationURI, VerificationUriComplete: d.VerificationURIComplete,
		ExpireTime: timestamppb.New(d.ExpiresAt), IntervalSeconds: int32(d.Interval.Seconds()),
	}), nil
}

func (h sessions) PollDeviceLogin(ctx context.Context, req *connect.Request[iamv1alpha1.PollDeviceLoginRequest]) (*connect.Response[iamv1alpha1.PollDeviceLoginResponse], error) {
	t, err := h.s.login.PollDevice(ctx, token.Secret(req.Msg.GetDeviceCode()))
	if err != nil {
		return nil, h.error(err)
	}
	return connect.NewResponse(&iamv1alpha1.PollDeviceLoginResponse{Tokens: tokensProto(t)}), nil
}

func (h sessions) Logout(ctx context.Context, req *connect.Request[iamv1alpha1.LogoutRequest]) (*connect.Response[iamv1alpha1.LogoutResponse], error) {
	if err := h.s.sessions.Revoke(ctx, token.Secret(req.Msg.GetRefreshToken())); err != nil {
		return nil, h.error(err)
	}
	return connect.NewResponse(&iamv1alpha1.LogoutResponse{}), nil
}

// RevokeSessions revokes a person's every login: anyone may revoke their
// own, and whoever may administer users (platform.root, including root on
// the host) anyone's.
func (h sessions) RevokeSessions(ctx context.Context, req *connect.Request[iamv1alpha1.RevokeSessionsRequest]) (*connect.Response[iamv1alpha1.RevokeSessionsResponse], error) {
	ctx, err := h.chain.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	p, err := h.s.authenticated(ctx)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(req.Msg.GetEmail()))
	if email == "" {
		if p.Type != auth.User {
			return nil, invalid("%s has no logins of its own; name the email whose logins to revoke", p)
		}
		email = p.ID
	}
	target := auth.Principal{Type: auth.User, ID: email}
	if target != p && !h.s.authz.Allowed(p, authz.Permission(registry.User, authz.Apply), authz.Target{}) {
		return nil, denied("%s may revoke only their own logins", p)
	}
	n, err := h.s.sessions.RevokePrincipal(ctx, target)
	if err != nil {
		return nil, apiError(err)
	}
	h.s.log.Info("revoked logins", "principal", target.String(), "by", p.String(), "client_ip", clientIP(ctx), "credentials", n)
	return connect.NewResponse(&iamv1alpha1.RevokeSessionsResponse{Revoked: int32(n)}), nil
}

func tokensProto(t session.Tokens) *iamv1alpha1.Tokens {
	return &iamv1alpha1.Tokens{
		AccessToken: t.Access.Reveal(), AccessExpireTime: timestamppb.New(t.AccessExpiresAt),
		RefreshToken: t.Refresh.Reveal(), Email: t.Principal.ID,
	}
}

// error maps the sign-in errors to codes. Anything else is internal: it is
// logged, and the caller learns nothing more.
func (h sessions) error(err error) error {
	switch {
	case errors.Is(err, login.ErrPending):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, login.ErrSlowDown):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, login.ErrBusy):
		return connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, login.ErrDenied), errors.Is(err, login.ErrNotAdmitted), errors.Is(err, session.ErrNotAdmitted):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, login.ErrInvalidCode), errors.Is(err, login.ErrExpired),
		errors.Is(err, session.ErrInvalid), errors.Is(err, session.ErrReused):
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	h.s.log.Error("a SessionService call failed", "error", err)
	return connect.NewError(connect.CodeInternal, errors.New("sign-in failed on the server; see its log"))
}
