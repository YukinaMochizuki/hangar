package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// DefaultMonitoringTimeout bounds each request MonitoringService makes of
// Prometheus or Alertmanager.
const DefaultMonitoringTimeout = 10 * time.Second

// maxMonitoringResponse caps what one answer of Prometheus or Alertmanager
// may hold; the queries are fixed and small, so more is something wrong.
const maxMonitoringResponse = 4 << 20

// CheckMonitoringURL refuses a Prometheus or Alertmanager URL that is not
// http(s) to a loopback IP and an explicit port: both listen on the host's
// loopback alone (docs/decisions/monitoring.md). Not localhost, which a
// hosts file can point anywhere.
func CheckMonitoringURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q: the scheme must be http or https", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q: no user, query or fragment", raw)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q: the host must be a loopback IP, such as 127.0.0.1 or [::1]", raw)
	}
	if u.Port() == "" {
		return fmt.Errorf("%q: name the port", raw)
	}
	return nil
}

// monitoringBackend is one of Prometheus and Alertmanager, as the apiserver
// asks it.
type monitoringBackend struct {
	// name is what errors call it, and flag the flag that sets it.
	name, flag string
	// base is nil when the flag is unset.
	base    *url.URL
	timeout time.Duration
	client  *http.Client
}

func newMonitoringBackend(name, flag, raw string, timeout time.Duration) (*monitoringBackend, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	b := &monitoringBackend{name: name, flag: flag, timeout: timeout, client: &http.Client{
		Transport: transport,
		// Never follow a redirect: it could lead off loopback.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if raw == "" {
		return b, nil
	}
	if err := CheckMonitoringURL(raw); err != nil {
		return b, err
	}
	b.base, _ = url.Parse(strings.TrimSuffix(raw, "/"))
	return b, nil
}

// do sends a request to path under the base URL, with form as its query
// (GET) or body (POST), and decodes the JSON answered into out.
func (b *monitoringBackend) do(ctx context.Context, method, path string, form url.Values, out any) error {
	if b.base == nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("this apiserver has no %s to ask: it runs without %s", b.name, b.flag))
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	u := *b.base
	u.Path += path
	var body io.Reader
	if method == http.MethodGet {
		u.RawQuery = form.Encode()
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return b.unavailable(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMonitoringResponse+1))
	if err != nil {
		return b.unavailable(ctx, err)
	}
	if len(data) > maxMonitoringResponse {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("%s answered more than %d bytes", b.name, maxMonitoringResponse))
	}
	switch {
	case resp.StatusCode >= 500:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s answered %s: %s", b.name, resp.Status, snippet(data)))
	case resp.StatusCode != http.StatusOK:
		return connect.NewError(connect.CodeInternal, fmt.Errorf("%s answered %s: %s", b.name, resp.Status, snippet(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("%s's answer: %w", b.name, err))
	}
	return nil
}

// unavailable is the error for a request that got no answer: Unavailable,
// unless the caller's own context ended it.
func (b *monitoringBackend) unavailable(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return connect.NewError(connect.CodeCanceled, err)
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s did not answer: %w", b.name, err))
}

// snippet is the start of an error answer, for the error message.
func snippet(data []byte) string {
	const max = 200
	s := strings.TrimSpace(string(data))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// sample is one series of an instant vector: its labels and its value.
type sample struct {
	labels map[string]string
	value  float64
}

// rangeSeries is one series of a range vector.
type rangeSeries struct {
	labels map[string]string
	points []samplePoint
}

type samplePoint struct {
	time  time.Time
	value float64
}

// promAnswer is the envelope of Prometheus's HTTP API.
type promAnswer struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// query evaluates an instant vector query at t.
func (b *monitoringBackend) query(ctx context.Context, q string, t time.Time) ([]sample, error) {
	var a promAnswer
	form := url.Values{"query": {q}, "time": {formatPromTime(t)}}
	if err := b.do(ctx, http.MethodPost, "/api/v1/query", form, &a); err != nil {
		return nil, err
	}
	if err := a.check(q, "vector"); err != nil {
		return nil, err
	}
	out := make([]sample, 0, len(a.Data.Result))
	for _, r := range a.Data.Result {
		_, v, err := parsePromPoint(r.Value)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("query %s: %w", q, err))
		}
		out = append(out, sample{labels: r.Metric, value: v})
	}
	return out, nil
}

// queryRange evaluates a query from start to end, at step.
func (b *monitoringBackend) queryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]rangeSeries, error) {
	var a promAnswer
	form := url.Values{
		"query": {q}, "start": {formatPromTime(start)}, "end": {formatPromTime(end)},
		"step": {strconv.FormatInt(int64(step/time.Second), 10)},
	}
	if err := b.do(ctx, http.MethodPost, "/api/v1/query_range", form, &a); err != nil {
		return nil, err
	}
	if err := a.check(q, "matrix"); err != nil {
		return nil, err
	}
	out := make([]rangeSeries, 0, len(a.Data.Result))
	for _, r := range a.Data.Result {
		s := rangeSeries{labels: r.Metric}
		for _, p := range r.Values {
			t, v, err := parsePromPoint(p)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("query %s: %w", q, err))
			}
			s.points = append(s.points, samplePoint{time: t, value: v})
		}
		out = append(out, s)
	}
	return out, nil
}

func (a promAnswer) check(q, resultType string) error {
	if a.Status != "success" {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("the query %s was refused: %s: %s", q, a.ErrorType, a.Error))
	}
	if a.Data.ResultType != resultType {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("the query %s answered a %s, not a %s", q, a.Data.ResultType, resultType))
	}
	return nil
}

// parsePromPoint reads [unix seconds, "value"].
func parsePromPoint(p []json.RawMessage) (time.Time, float64, error) {
	if len(p) != 2 {
		return time.Time{}, 0, fmt.Errorf("a point of %d elements", len(p))
	}
	var ts float64
	if err := json.Unmarshal(p[0], &ts); err != nil {
		return time.Time{}, 0, fmt.Errorf("a point's time: %w", err)
	}
	var vs string
	if err := json.Unmarshal(p[1], &vs); err != nil {
		return time.Time{}, 0, fmt.Errorf("a point's value: %w", err)
	}
	v, err := strconv.ParseFloat(vs, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("a point's value: %w", err)
	}
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), v, nil
}

func formatPromTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}

// amAlert is an alert as Alertmanager's v2 API answers it.
type amAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
}

// alerts returns the alerts firing now, but silenced and inhibited ones.
func (b *monitoringBackend) alerts(ctx context.Context) ([]amAlert, error) {
	var out []amAlert
	form := url.Values{"active": {"true"}, "silenced": {"false"}, "inhibited": {"false"}, "unprocessed": {"false"}}
	if err := b.do(ctx, http.MethodGet, "/api/v2/alerts", form, &out); err != nil {
		return nil, err
	}
	return out, nil
}
