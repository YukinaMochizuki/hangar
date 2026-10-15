package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"sigs.k8s.io/yaml"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
)

// The CLI's logins live in credentials.yaml next to config.yaml, kept apart
// so that config.yaml, which `config view` prints and people share, never
// holds a secret. Only its owner may read it (0600, in a 0700 directory);
// the CLI refuses a file anyone else can, as ssh does a private key.

// credentialsFile is credentials.yaml: the logins, by endpoint.
type credentialsFile struct {
	Logins map[string]*storedLogin `json:"logins,omitempty"`
}

// storedLogin is one endpoint's login. The tokens are plain strings only
// here, on their way to and from the file.
type storedLogin struct {
	Email         string    `json:"email"`
	AccessToken   string    `json:"accessToken"`
	AccessExpires time.Time `json:"accessExpires"`
	RefreshToken  string    `json:"refreshToken"`
}

func (a *app) credentialsPath() string {
	return filepath.Join(filepath.Dir(configPath(a.env)), "credentials.yaml")
}

func loadCredentials(path string) (*credentialsFile, error) {
	f := &credentialsFile{Logins: map[string]*storedLogin{}}
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %v; it holds your logins, so only you may read it: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.UnmarshalStrict(b, f); err != nil {
		// The YAML error could quote a token; say only where it is.
		return nil, fmt.Errorf("%s is not a credentials file; delete it and run hangar login", path)
	}
	if f.Logins == nil {
		f.Logins = map[string]*storedLogin{}
	}
	return f, nil
}

// save writes the file whole, through a temporary file only its owner reads,
// so a reader never sees half of it and no one else ever sees any.
func (f *credentialsFile) save(path string) error {
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*.yaml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// lockCredentials holds an exclusive lock on the credentials file until
// unlock: a refresh spends the refresh token, so two hangar processes
// refreshing at once would look like a stolen token and end the login.
func lockCredentials(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// updateCredentials changes the file under its lock.
func (a *app) updateCredentials(change func(f *credentialsFile) error) error {
	if err := a.prepareConfigDir(); err != nil {
		return err
	}
	path := a.credentialsPath()
	unlock, err := lockCredentials(path)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := loadCredentials(path)
	if err != nil {
		return err
	}
	if err := change(f); err != nil {
		return err
	}
	return f.save(path)
}

// refreshBefore is how long before it expires an access token is renewed.
const refreshBefore = time.Minute

// tokenSource hands out a context's access token, renewing it with the
// refresh token when it is about to expire.
type tokenSource struct {
	a        *app
	endpoint string

	mu      sync.Mutex
	token   token.Secret
	expires time.Time
}

func (a *app) tokens(c *Context) *tokenSource {
	return &tokenSource{a: a, endpoint: c.Endpoint}
}

// errNoLogin and the like never carry a token, so they may be printed.
func (t *tokenSource) errNoLogin() error {
	return fmt.Errorf("not logged in to %s; run hangar login", t.endpoint)
}

func (t *tokenSource) access(ctx context.Context) (token.Secret, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.a.opts.Now()
	if t.token != "" && now.Before(t.expires.Add(-refreshBefore)) {
		return t.token, nil
	}
	var err error
	uerr := t.a.updateCredentials(func(f *credentialsFile) error {
		l := f.Logins[t.endpoint]
		if l == nil || l.RefreshToken == "" {
			err = t.errNoLogin()
			return err
		}
		// Another hangar process may have renewed it while this one waited
		// for the lock.
		if l.AccessToken != "" && now.Before(l.AccessExpires.Add(-refreshBefore)) {
			t.token, t.expires = token.Secret(l.AccessToken), l.AccessExpires
			return nil
		}
		res, rerr := t.a.sessions(t.endpoint).Refresh(ctx, connect.NewRequest(&iamv1alpha1.RefreshRequest{RefreshToken: l.RefreshToken}))
		switch code := connect.CodeOf(rerr); {
		case rerr == nil:
		case code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied:
			// The login is over; forget it, so the next command says so
			// at once rather than asking the server again.
			delete(f.Logins, t.endpoint)
			err = fmt.Errorf("your login to %s has ended (%s); run hangar login", t.endpoint, connectMessage(rerr))
			return nil
		default:
			err = fmt.Errorf("renewing your login to %s: %s", t.endpoint, connectMessage(rerr))
			return err
		}
		f.Logins[t.endpoint] = loginFrom(res.Msg.GetTokens())
		t.token, t.expires = token.Secret(res.Msg.GetTokens().GetAccessToken()), res.Msg.GetTokens().GetAccessExpireTime().AsTime()
		return nil
	})
	if err != nil {
		return "", err
	}
	if uerr != nil {
		return "", uerr
	}
	return t.token, nil
}

func loginFrom(t *iamv1alpha1.Tokens) *storedLogin {
	return &storedLogin{
		Email: t.GetEmail(), AccessToken: t.GetAccessToken(), AccessExpires: t.GetAccessExpireTime().AsTime(),
		RefreshToken: t.GetRefreshToken(),
	}
}

// sessions is SessionService at an endpoint, without credentials: each of
// its calls carries its own.
func (a *app) sessions(endpoint string) iamv1alpha1connect.SessionServiceClient {
	return iamv1alpha1connect.NewSessionServiceClient(a.opts.HTTPClient, endpoint)
}

// connectMessage is a Connect error's message alone: the server's words,
// which never hold a token, without the request around them.
func connectMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}
