package cloudflare_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
)

const (
	token   = "api-token-0e1f2a3b"
	account = "0123456789abcdef0123456789abcdef"
)

func newClient(t *testing.T, f *cftest.Fake, sleeps *[]time.Duration, opts ...cloudflare.Option) *cloudflare.Client {
	t.Helper()
	opts = append([]cloudflare.Option{
		cloudflare.WithBaseURL(f.URL()),
		cloudflare.WithSleep(func(_ context.Context, d time.Duration) error {
			if sleeps != nil {
				*sleeps = append(*sleeps, d)
			}
			return nil
		}),
	}, opts...)
	c, err := cloudflare.New(token, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newFake(t *testing.T) *cftest.Fake {
	f := cftest.New(token, account, "hangar.example")
	t.Cleanup(f.Close)
	return f
}

func TestSendsBearerToken(t *testing.T) {
	f := newFake(t)
	if _, err := newClient(t, f, nil).Organization(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if got := f.Requests()[0].Auth; got != "Bearer "+token {
		t.Fatalf("Authorization %q", got)
	}
	bad, _ := cloudflare.New("wrong", cloudflare.WithBaseURL(f.URL()))
	_, err := bad.Organization(context.Background(), account)
	var ae *cloudflare.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || ae.Errors[0].Code != 10000 {
		t.Fatalf("wrong token: %v", err)
	}
}

func TestRetriesWithBackoffAndRetryAfter(t *testing.T) {
	f := newFake(t)
	var sleeps []time.Duration
	c := newClient(t, f, &sleeps, cloudflare.WithRetry(5, 100*time.Millisecond))
	f.Fail("", http.StatusTooManyRequests, 1, "7")
	f.Fail("", http.StatusInternalServerError, 1, "")
	f.Fail("", http.StatusBadGateway, 1, "")
	if _, err := c.Organization(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{7 * time.Second, 200 * time.Millisecond, 400 * time.Millisecond}
	if !slices.Equal(sleeps, want) {
		t.Fatalf("sleeps %v, want %v", sleeps, want)
	}
	if n := len(f.Requests()); n != 4 {
		t.Fatalf("%d requests, want 4", n)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil, cloudflare.WithRetry(3, time.Millisecond))
	f.Fail("", http.StatusServiceUnavailable, 10, "")
	_, err := c.Organization(context.Background(), account)
	var ae *cloudflare.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
		t.Fatalf("err %v", err)
	}
	if n := len(f.Requests()); n != 3 {
		t.Fatalf("%d attempts, want 3", n)
	}
}

func TestPostRetriesOnlyOn429(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	f.Fail(http.MethodPost, http.StatusTooManyRequests, 1, "")
	if _, err := c.CreateTunnel(context.Background(), account, "a"); err != nil {
		t.Fatalf("429 then success: %v", err)
	}
	f.Fail(http.MethodPost, http.StatusInternalServerError, 1, "")
	before := len(f.Requests())
	if _, err := c.CreateTunnel(context.Background(), account, "b"); err == nil {
		t.Fatal("a POST that got a 500 was retried")
	}
	if n := len(f.Requests()) - before; n != 1 {
		t.Fatalf("POST after 500 sent %d times", n)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	_, err := c.GetTunnel(context.Background(), account, "00000000-0000-0000-0000-000000000000")
	if !cloudflare.IsNotFound(err) {
		t.Fatalf("err %v, want not found", err)
	}
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("404 retried: %d requests", n)
	}
}

func TestReadOnlyRefusesWritesBeforeSending(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil, cloudflare.ReadOnly())
	if _, err := c.CreateTunnel(context.Background(), account, "x"); !errors.Is(err, cloudflare.ErrReadOnly) {
		t.Fatalf("err %v", err)
	}
	if _, err := c.PutTunnelConfiguration(context.Background(), account, "x", cloudflare.TunnelConfig{}); !errors.Is(err, cloudflare.ErrReadOnly) {
		t.Fatalf("err %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("read-only client sent %d requests", n)
	}
	if _, err := c.ListTunnels(context.Background(), account, "x"); err != nil {
		t.Fatalf("read-only GET: %v", err)
	}
}

func TestListsEveryPage(t *testing.T) {
	f := newFake(t)
	f.MaxPerPage = 2
	for i := range 5 {
		f.AddTunnel(fmt.Sprintf("t%d", i), "cloudflare")
		f.AddServiceToken(fmt.Sprintf("s%d", i))
	}
	c := newClient(t, f, nil)
	ctx := context.Background()
	// Tunnels page with total_pages; service tokens, like all Access lists,
	// only with total_count.
	tunnels, err := c.ListTunnels(ctx, account, "")
	if err != nil || len(tunnels) != 5 {
		t.Fatalf("tunnels: %d %v", len(tunnels), err)
	}
	tokens, err := c.ListServiceTokens(ctx, account, "")
	if err != nil || len(tokens) != 5 {
		t.Fatalf("service tokens: %d %v", len(tokens), err)
	}
}

func TestPagingStopsWithoutResultInfo(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":[{"id":"a","name":"x","type":"google"}]}`)
	}))
	defer srv.Close()
	c, _ := cloudflare.New(token, cloudflare.WithBaseURL(srv.URL))
	idps, err := c.ListIdentityProviders(context.Background(), account)
	if err != nil || len(idps) != 1 || calls != 1 {
		t.Fatalf("idps %v err %v calls %d", idps, err, calls)
	}
}

func TestUnsuccessfulEnvelopeIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"success":false,"errors":[{"code":1234,"message":"nope"}],"messages":[],"result":null}`)
	}))
	defer srv.Close()
	c, _ := cloudflare.New(token, cloudflare.WithBaseURL(srv.URL), cloudflare.WithRetry(1, 0))
	_, err := c.Organization(context.Background(), account)
	var ae *cloudflare.APIError
	if !errors.As(err, &ae) || ae.Errors[0].Code != 1234 {
		t.Fatalf("err %v", err)
	}
}

func TestSecretsNeverFormat(t *testing.T) {
	s := cloudflare.Secret("hunter2")
	for _, got := range []string{fmt.Sprint(s), fmt.Sprintf("%v %s %q %#v %+v", s, s, s, s, s), fmt.Sprintf("%v", struct{ S cloudflare.Secret }{s})} {
		if strings.Contains(got, "hunter2") {
			t.Fatalf("formatted secret: %s", got)
		}
	}
	f := newFake(t)
	c := newClient(t, f, nil)
	created, err := c.CreateServiceToken(context.Background(), account, "x", "8760h")
	if err != nil {
		t.Fatal(err)
	}
	if created.ClientSecret.Reveal() == "" || strings.Contains(fmt.Sprintf("%+v", created), created.ClientSecret.Reveal()) {
		t.Fatalf("service token secret missing or formatted: %+v", created)
	}
	// Errors carry the method and path, never the token.
	f.Fail("", http.StatusBadRequest, 1, "")
	_, err = c.Organization(context.Background(), account)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("error %v", err)
	}
}

func TestTimeoutBoundsEachAttempt(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	c, _ := cloudflare.New(token, cloudflare.WithBaseURL(srv.URL), cloudflare.WithRetry(2, time.Millisecond),
		cloudflare.WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	start := time.Now()
	if _, err := c.Organization(context.Background(), account); err == nil {
		t.Fatal("no timeout")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}
