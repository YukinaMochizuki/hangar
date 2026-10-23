package cli

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/pages"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
)

// loginTimeout is how long hangar login waits for the browser.
const loginTimeout = 10 * time.Minute

func (a *app) loginCmd() *cobra.Command {
	var device bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in, in a browser, and keep the login for this context's endpoint",
		Long: `Sign in, in a browser, and keep the login for this context's endpoint.

hangar login opens a browser to sign in, and listens on 127.0.0.1 for it to
come back. With --device it prints a code to enter in a browser on any other
device instead, for a machine without one. The login is kept in
credentials.yaml next to the config file, which only you may read; hangar
renews it as it is used, and hangar logout ends it.`,
		Args: args(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.current()
			if err != nil {
				return err
			}
			if err := c.check(); err != nil {
				return err
			}
			if c.local() {
				return usageErr("the context uses local auth, which needs no login")
			}
			if c.Auth == "dev" {
				return usageErr("the context uses dev auth, which needs no login")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()
			var tok *iamv1alpha1.Tokens
			if device {
				tok, err = a.deviceLogin(ctx, cmd, c)
			} else {
				tok, err = a.loopbackLogin(ctx, cmd, c)
			}
			if err != nil {
				return err
			}
			if err := a.updateCredentials(func(f *credentialsFile) error {
				f.Logins[c.Endpoint] = loginFrom(tok)
				return nil
			}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "logged in to %s as %s\n", c.Endpoint, tok.GetEmail())
			return nil
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "sign in with a code entered in a browser on another device")
	return cmd
}

// loopbackLogin signs in through a browser on this machine, which the
// apiserver sends back to a listener on 127.0.0.1 with a one-time code; the
// code counts only with this run's PKCE verifier.
func (a *app) loopbackLogin(ctx context.Context, cmd *cobra.Command, c *Context) (*iamv1alpha1.Tokens, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on 127.0.0.1 for the browser: %w", err)
	}
	defer func() { _ = ln.Close() }()
	redirect := "http://" + ln.Addr().String() + login.LoopbackPath
	state, verifier := token.Random(), oauth2.GenerateVerifier()
	start := c.Endpoint + login.PathCLI + "?" + url.Values{
		"redirect_uri": {redirect}, "state": {state},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"},
	}.Encode()

	type result struct {
		tok *iamv1alpha1.Tokens
		err error
	}
	done := make(chan result, 1)
	finish := func(r result) {
		select {
		case done <- r:
		default:
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+login.LoopbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Anything on this machine can reach the listener; only the state
		// this run sent makes a request its own.
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			pages.Render(w, http.StatusBadRequest, pages.CLIFailed, pages.Data{Reason: "the request did not come from this sign-in"})
			return
		}
		if e := q.Get("error"); e != "" {
			reason := q.Get("error_description")
			if reason == "" {
				reason = e
			}
			pages.Render(w, http.StatusForbidden, pages.CLIFailed, pages.Data{Reason: reason})
			finish(result{err: errors.New("sign-in failed: " + reason)})
			return
		}
		res, err := a.sessions(c.Endpoint).ExchangeCode(r.Context(), connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{
			Code: q.Get("code"), CodeVerifier: verifier, RedirectUri: redirect,
		}))
		if err != nil {
			pages.Render(w, http.StatusBadGateway, pages.CLIFailed, pages.Data{Reason: connectMessage(err)})
			finish(result{err: errors.New("sign-in failed: " + connectMessage(err))})
			return
		}
		pages.Render(w, http.StatusOK, pages.CLISignedIn, pages.Data{Endpoint: c.Endpoint, Email: res.Msg.GetTokens().GetEmail()})
		finish(result{tok: res.Msg.GetTokens()})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Opening a browser to sign in. If none opens, visit:\n\n  %s\n\n", start)
	if err := a.opts.OpenBrowser(start); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "(could not open a browser: %v; on a machine without one, use hangar login --device)\n", err)
	}
	select {
	case r := <-done:
		return r.tok, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("gave up waiting for the browser to sign in: %w", ctx.Err())
	}
}

// deviceLogin signs in with a code the person enters in a browser anywhere,
// polling until they approve it.
func (a *app) deviceLogin(ctx context.Context, cmd *cobra.Command, c *Context) (*iamv1alpha1.Tokens, error) {
	sessions := a.sessions(c.Endpoint)
	res, err := sessions.StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{}))
	if err != nil {
		return nil, fmt.Errorf("start a device login at %s: %s", c.Endpoint, connectMessage(err))
	}
	d := res.Msg
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "To sign in, open\n\n  %s\n\nin a browser on any device, and enter the code\n\n  %s\n\n(or open %s)\nWaiting...\n",
		d.GetVerificationUri(), d.GetUserCode(), d.GetVerificationUriComplete())
	interval := time.Duration(d.GetIntervalSeconds()) * time.Second
	if interval <= 0 {
		interval = time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gave up waiting for the device login to be approved: %w", ctx.Err())
		case <-time.After(interval):
		}
		poll, err := sessions.PollDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.PollDeviceLoginRequest{DeviceCode: d.GetDeviceCode()}))
		switch connect.CodeOf(err) {
		case connect.CodeFailedPrecondition:
			continue
		case connect.CodeResourceExhausted:
			interval += 5 * time.Second
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("device login: %s", connectMessage(err))
		}
		return poll.Msg.GetTokens(), nil
	}
}

func (a *app) logoutCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "End this context's login, here and on the server",
		Long: `End this context's login, here and on the server.

With --all, end every login of yours at the endpoint, on every machine and
browser, as when one may have been stolen.`,
		Args: args(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.current()
			if err != nil {
				return err
			}
			if err := c.check(); err != nil {
				return err
			}
			if c.local() || c.Auth == "dev" {
				return usageErr("the context uses %s auth, which has no login", map[bool]string{true: "local", false: "dev"}[c.local()])
			}
			f, err := loadCredentials(a.credentialsPath())
			if err != nil {
				return err
			}
			l := f.Logins[c.Endpoint]
			if l == nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "not logged in to %s\n", c.Endpoint)
				return nil
			}
			if all {
				creds, err := a.credentials(c)
				if err != nil {
					return err
				}
				cl := a.sessionsWith(c.Endpoint, creds)
				if _, err := cl.RevokeSessions(cmd.Context(), connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{})); err != nil {
					return fmt.Errorf("revoke your logins at %s: %s", c.Endpoint, connectMessage(err))
				}
			} else if _, err := a.sessions(c.Endpoint).Logout(cmd.Context(), connect.NewRequest(&iamv1alpha1.LogoutRequest{RefreshToken: l.RefreshToken})); err != nil {
				return fmt.Errorf("log out of %s: %s", c.Endpoint, connectMessage(err))
			}
			if err := a.updateCredentials(func(f *credentialsFile) error {
				delete(f.Logins, c.Endpoint)
				return nil
			}); err != nil {
				return err
			}
			if all {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "logged out of %s; every login of %s there has ended\n", c.Endpoint, l.Email)
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "logged out of %s\n", c.Endpoint)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "end every login of yours at the endpoint, everywhere")
	return cmd
}

// sessionsWith is SessionService with the context's credentials.
func (a *app) sessionsWith(endpoint string, creds credentials) iamv1alpha1connect.SessionServiceClient {
	return iamv1alpha1connect.NewSessionServiceClient(a.opts.HTTPClient, endpoint, connect.WithInterceptors(creds))
}

// openBrowser opens a URL in the desktop's browser.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		// Not start, which is cmd's and would take the URL's & as its own.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
