// Package cloudflare is a small client for the Cloudflare API v4: the zone
// and DNS record endpoints Hangar uses, and nothing else. It knows the
// API's shapes, its envelope, paging and retry rules; deciding what should
// exist is left to its callers.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the API v4 root.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// Secret holds a credential. It formats as a placeholder, so passing one to a
// logger, an error or fmt by mistake never reveals it; Reveal does.
type Secret string

// Reveal returns the credential itself.
func (s Secret) Reveal() string { return string(s) }

func (s Secret) String() string { return "[redacted]" }

func (s Secret) GoString() string { return "[redacted]" }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// ErrReadOnly is returned for any request other than GET on a read-only
// client.
var ErrReadOnly = errors.New("cloudflare: read-only client refused a write")

// Client calls the API with one API token.
type Client struct {
	baseURL     string
	token       Secret
	http        *http.Client
	readOnly    bool
	maxAttempts int
	backoff     time.Duration
	sleep       func(context.Context, time.Duration) error
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another API root, such as a test fake.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") } }

// WithHTTPClient replaces the HTTP client, whose Timeout bounds each attempt.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// ReadOnly makes the client refuse every request but GET before it is sent.
func ReadOnly() Option { return func(c *Client) { c.readOnly = true } }

// WithRetry sets the attempts per request and the first backoff, which
// doubles on each retry.
func WithRetry(attempts int, backoff time.Duration) Option {
	return func(c *Client) { c.maxAttempts, c.backoff = attempts, backoff }
}

// WithSleep replaces how the client waits between attempts.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// New returns a client authenticating with token.
func New(token Secret, opts ...Option) (*Client, error) {
	if token == "" {
		return nil, errors.New("cloudflare: empty API token")
	}
	c := &Client{
		baseURL:     DefaultBaseURL,
		token:       token,
		http:        &http.Client{Timeout: 30 * time.Second},
		maxAttempts: 5,
		backoff:     time.Second,
		sleep:       sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Message is one entry of the envelope's errors or messages.
type Message struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ResultInfo is the paging block of list responses.
type ResultInfo struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	Count      int `json:"count"`
	TotalCount int `json:"total_count"`
	TotalPages int `json:"total_pages"`
}

type envelope struct {
	Success    bool            `json:"success"`
	Errors     []Message       `json:"errors"`
	Messages   []Message       `json:"messages"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *ResultInfo     `json:"result_info"`
}

// APIError is a request the API answered with a failure.
type APIError struct {
	Method string
	Path   string
	Status int
	Errors []Message
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cloudflare: %s %s: HTTP %d", e.Method, e.Path, e.Status)
	for _, m := range e.Errors {
		fmt.Fprintf(&b, "; %d %s", m.Code, m.Message)
	}
	return b.String()
}

// IsNotFound reports whether err is the API saying the object does not exist.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// do sends one request and decodes the envelope's result into out. Paths
// carry only IDs and query parameters, never credentials, so they are safe in
// errors; bodies never appear in errors.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) (*ResultInfo, error) {
	if c.readOnly && method != http.MethodGet {
		return nil, fmt.Errorf("%w: %s %s", ErrReadOnly, method, path)
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("cloudflare: encode %s %s: %w", method, path, err)
		}
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	wait := c.backoff
	for attempt := 1; ; attempt++ {
		info, retryAfter, err := c.once(ctx, method, path, u, payload, out)
		if err == nil {
			return info, nil
		}
		if attempt >= c.maxAttempts || !retryable(method, err) || ctx.Err() != nil {
			return nil, err
		}
		d := wait
		if retryAfter > d {
			d = retryAfter
		}
		if serr := c.sleep(ctx, d); serr != nil {
			return nil, err
		}
		wait *= 2
	}
}

// retryable says whether a failed request may be sent again. A POST that got
// a 5xx or no answer may have created its object, so only a 429, which the
// API sends before doing anything, retries one.
func retryable(method string, err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Status == http.StatusTooManyRequests {
			return true
		}
		return ae.Status >= 500 && method != http.MethodPost
	}
	var te *transportError
	return errors.As(err, &te) && method != http.MethodPost
}

type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func (c *Client) once(ctx context.Context, method, path, u string, payload []byte, out any) (*ResultInfo, time.Duration, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, 0, fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &transportError{fmt.Errorf("cloudflare: %s %s: %w", method, path, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, 0, &transportError{fmt.Errorf("cloudflare: %s %s: read: %w", method, path, err)}
	}
	var env envelope
	jerr := json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || jerr != nil || !env.Success {
		status := resp.StatusCode
		if status >= 200 && status <= 299 {
			// A 2xx without a successful envelope is not something to act on.
			status = http.StatusBadGateway
		}
		return nil, retryAfter(resp.Header), &APIError{Method: method, Path: path, Status: status, Errors: env.Errors}
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, 0, fmt.Errorf("cloudflare: %s %s: decode result: %w", method, path, err)
		}
	}
	return env.ResultInfo, 0, nil
}

func retryAfter(h http.Header) time.Duration {
	s := h.Get("Retry-After")
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, 5*time.Minute)
	}
	return 0
}

// maxPages bounds a listing, so a server that never reports an end cannot
// keep the client paging forever.
const maxPages = 200

// listAll fetches every page of a list endpoint. The end is the page that
// result_info's total_pages or total_count says is last, or a short page when
// it says neither.
func listAll[T any](ctx context.Context, c *Client, path string, query url.Values, perPage int) ([]T, error) {
	var all []T
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", strconv.Itoa(perPage))
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var items []T
		info, err := c.do(ctx, http.MethodGet, path, q, nil, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		switch {
		case len(items) == 0:
			return all, nil
		case info == nil:
			return all, nil
		case info.TotalPages > 0:
			if page >= info.TotalPages {
				return all, nil
			}
		case info.TotalCount > 0:
			if len(all) >= info.TotalCount {
				return all, nil
			}
		case len(items) < perPage:
			return all, nil
		}
	}
	return nil, fmt.Errorf("cloudflare: GET %s: more than %d pages", path, maxPages)
}

func esc(s string) string { return url.PathEscape(s) }
