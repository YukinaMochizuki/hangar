package front_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/http2/hpack"
)

// fakeAPI is the apiserver's public socket as the test sees it: it echoes
// what reached it, streams on /stream until released, and takes a WebSocket
// on /v1/ws.
func fakeAPI(release <-chan struct{}) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", echo("api"))
	mux.HandleFunc("/stream", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("second\n"))
	})
	mux.HandleFunc("/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		typ, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, append([]byte(r.Host+" "), msg...))
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	return mux
}

// consoleFiles is a console release: the page, an asset, and a file outside
// it that a symbolic link points at.
func consoleFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	console := filepath.Join(dir, "console")
	for name, body := range map[string]string{
		"index.html":          "<p>placeholder console</p>",
		"assets/app-1234.js":  "console.log(1)",
		"robots.txt":          "User-agent: *",
		"../secret.txt":       "secret",
		"assets/sub/deep.css": "p{}",
	} {
		p := filepath.Join(console, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(console, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	return console
}

func TestRouting(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	o := defaultOptions()
	o.api = fakeAPI(release)
	o.consoleDir = consoleFiles(t)
	d := startDoor(t, newCA(t), o)

	t.Run("api, client headers replaced", func(t *testing.T) {
		for _, c := range []*http.Client{d.client, d.client1} {
			code, body, _ := do(t, c, http.MethodPost, "https://"+apiHost+"/hangar.iam.v1alpha1.AccountService/WhoAmI?x=1", strings.NewReader("{}"), http.Header{
				"Hangar-Client-Ip": {"203.0.113.9"}, "X-Forwarded-For": {"203.0.113.9"}, "Forwarded": {"for=203.0.113.9"},
				"X-Real-Ip": {"203.0.113.9"}, "X-Forwarded-Proto": {"http"},
			})
			want := "api POST " + apiHost + " /hangar.iam.v1alpha1.AccountService/WhoAmI?x=1 client-ip=127.0.0.1 xff=127.0.0.1 proto=https forwarded= real-ip= body=2"
			if code != http.StatusOK || body != want {
				t.Errorf("got %d %q\nwant %q", code, body, want)
			}
		}
	})
	t.Run("api, a server stream flows", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+apiHost+"/stream", nil)
		resp, err := d.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil || line != "first\n" {
			t.Fatalf("the stream's first message before the second is sent: %q, %v", line, err)
		}
	})
	t.Run("api, a WebSocket with its Host", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, "wss://"+apiHost+"/v1/ws", &websocket.DialOptions{HTTPClient: d.client1})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		if err := c.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		_, msg, err := c.Read(ctx)
		if err != nil || string(msg) != apiHost+" ping" {
			t.Errorf("got %q, %v", msg, err)
		}
	})
	t.Run("console", func(t *testing.T) {
		for _, tt := range []struct {
			method, path string
			code         int
			body, cache  string
		}{
			{"GET", "/", 200, "<p>placeholder console</p>", "no-cache"},
			{"GET", "/projects/demo/machines", 200, "<p>placeholder console</p>", "no-cache"},
			{"HEAD", "/index.html", 200, "", "no-cache"},
			{"GET", "/assets/app-1234.js", 200, "console.log(1)", "public, max-age=31536000, immutable"},
			{"GET", "/assets/sub/deep.css", 200, "p{}", "public, max-age=31536000, immutable"},
			{"GET", "/robots.txt", 200, "User-agent: *", ""},
			{"GET", "/assets/missing.js", 404, "not found\n", ""},
			{"GET", "/assets", 200, "<p>placeholder console</p>", "no-cache"},
			{"GET", "/../secret.txt", 404, "not found\n", ""},
			{"GET", "/leak.txt", 404, "not found\n", ""},
			{"POST", "/", 405, "method not allowed\n", ""},
			{"POST", "/hangar.iam.v1alpha1.AccountService/WhoAmI", 200, "api POST " + consoleHost + " /hangar.iam.v1alpha1.AccountService/WhoAmI client-ip=127.0.0.1 xff=127.0.0.1 proto=https forwarded= real-ip= body=0", ""},
			{"GET", "/v1/projects/demo/containers/web:exec", 200, "api GET " + consoleHost + " /v1/projects/demo/containers/web:exec client-ip=127.0.0.1 xff=127.0.0.1 proto=https forwarded= real-ip= body=0", ""},
		} {
			code, body, h := do(t, d.client, tt.method, "https://"+consoleHost+tt.path, nil, nil)
			if code != tt.code || body != tt.body || h.Get("Cache-Control") != tt.cache {
				t.Errorf("%s %s: %d %q cache %q; want %d %q cache %q", tt.method, tt.path, code, body, h.Get("Cache-Control"), tt.code, tt.body, tt.cache)
			}
		}
	})
	t.Run("endpoints", func(t *testing.T) {
		code, body, _ := do(t, d.client, http.MethodGet, "https://"+webHost+"/a?b", nil, http.Header{"Hangar-Client-Ip": {"203.0.113.9"}, "X-Real-Ip": {"203.0.113.9"}})
		want := "endpoints GET " + webHost + " /a?b client-ip= xff= proto= forwarded= real-ip= body=0"
		if code != http.StatusOK || body != want {
			t.Errorf("got %d %q\nwant %q", code, body, want)
		}
	})
	t.Run("hosts", func(t *testing.T) {
		for _, tt := range []struct {
			sni, host string
			code      int
			body      string
		}{
			{apiHost, "other." + cpDomain, 404, "no such host\n"},
			{apiHost, cpDomain, 404, "no such host\n"},
			{apiHost, "a.b." + userDomain, 404, "no such host\n"},
			{apiHost, consoleHost, 421, "misdirected request\n"},
			{consoleHost, webHost, 421, "misdirected request\n"},
			{webHost, apiHost, 421, "misdirected request\n"},
			{webHost, "other-demo." + userDomain, 200, ""},
			{apiHost, strings.ToUpper(apiHost) + ".", 200, ""},
		} {
			c := d.newClient(false)
			c.Transport.(*http.Transport).TLSClientConfig.ServerName = tt.sni
			req, _ := http.NewRequest(http.MethodGet, "https://"+tt.sni+"/", nil)
			req.Host = tt.host
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("SNI %s, Host %s: %v", tt.sni, tt.host, err)
			}
			b := make([]byte, 64)
			n, _ := resp.Body.Read(b)
			_ = resp.Body.Close()
			if resp.StatusCode != tt.code || (tt.body != "" && string(b[:n]) != tt.body) {
				t.Errorf("SNI %s, Host %s: %d %q; want %d %q", tt.sni, tt.host, resp.StatusCode, b[:n], tt.code, tt.body)
			}
		}
	})
	t.Run("HSTS on the control plane's sites alone", func(t *testing.T) {
		for _, tt := range []struct {
			method, url, want string
		}{
			{"GET", "https://" + apiHost + "/", "max-age=31536000"},
			{"GET", "https://" + consoleHost + "/", "max-age=31536000"},
			{"POST", "https://" + consoleHost + "/", "max-age=31536000"},
			{"GET", "https://" + consoleHost + "/v1/x", "max-age=31536000"},
			{"GET", "https://" + webHost + "/", ""},
			{"GET", "http://" + apiHost + "/", ""},
			{"GET", "http://" + consoleHost + "/", ""},
		} {
			_, _, h := do(t, d.client1, tt.method, tt.url, nil, nil)
			if got := h.Get("Strict-Transport-Security"); got != tt.want {
				t.Errorf("%s %s: Strict-Transport-Security %q, want %q", tt.method, tt.url, got, tt.want)
			}
		}
	})
	t.Run("Host with a port", func(t *testing.T) {
		for _, c := range []*http.Client{d.client, d.client1} {
			for _, url := range []string{"https://" + apiHost + ":443/p", "https://" + apiHost + ":8443/p"} {
				code, body, _ := do(t, c, http.MethodGet, url, nil, nil)
				if code != http.StatusOK || !strings.HasPrefix(body, "api GET "+apiHost+":") {
					t.Errorf("%s: %d %q", url, code, body)
				}
			}
		}
		code, body, _ := do(t, d.client, http.MethodGet, "https://"+webHost+":443/", nil, nil)
		if code != http.StatusOK || !strings.HasPrefix(body, "endpoints GET "+webHost+":443 ") {
			t.Errorf("an endpoint's Host with a port: %d %q", code, body)
		}
	})
	t.Run("absolute-form request URI", func(t *testing.T) {
		// The URI's host is the request's, whatever Host says (RFC 9112).
		raw := func(target, host string) string {
			t.Helper()
			c, err := d.handshake(apiHost)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := io.WriteString(c, "GET "+target+" HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			b, _ := io.ReadAll(c)
			return string(b)
		}
		if got := raw("https://"+apiHost+"/abs?x=1", "other."+cpDomain); !strings.HasPrefix(got, "HTTP/1.1 200") ||
			!strings.Contains(got, "api GET "+apiHost+" /abs?x=1 ") {
			t.Errorf("absolute-form to the connection's site:\n%s", got)
		}
		if got := raw("https://"+consoleHost+"/", apiHost); !strings.HasPrefix(got, "HTTP/1.1 421") {
			t.Errorf("absolute-form to another site than the connection's:\n%s", got)
		}
		if got := raw("https://other."+cpDomain+"/", apiHost); !strings.HasPrefix(got, "HTTP/1.1 404") {
			t.Errorf("absolute-form to no site:\n%s", got)
		}
	})
	t.Run("HTTP/2 :authority", func(t *testing.T) {
		for _, tt := range []struct {
			sni, authority string
			code           int
			prefix         string
		}{
			{apiHost, apiHost, 200, "api GET " + apiHost + " "},
			{apiHost, consoleHost, 421, "misdirected request"},
			{apiHost, "other." + cpDomain, 404, "no such host"},
			{webHost, "other-demo." + userDomain, 200, "endpoints GET other-demo." + userDomain + " "},
		} {
			c := d.newClient(true)
			c.Transport.(*http.Transport).TLSClientConfig.ServerName = tt.sni
			req, _ := http.NewRequest(http.MethodGet, "https://"+tt.sni+"/", nil)
			req.Host = tt.authority
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf(":authority %s on %s: %v", tt.authority, tt.sni, err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.ProtoMajor != 2 || resp.StatusCode != tt.code || !strings.HasPrefix(string(b), tt.prefix) {
				t.Errorf(":authority %s on %s: HTTP/%d %d %q; want HTTP/2 %d %q", tt.authority, tt.sni, resp.ProtoMajor, resp.StatusCode, b, tt.code, tt.prefix)
			}
		}
	})
	t.Run("HTTP/2 :authority over a host field", func(t *testing.T) {
		// A host field beside :authority decides nothing: the routing and
		// the upstream's Host are :authority's.
		for _, tt := range []struct {
			authority, host string
			answer, body    string
		}{
			{apiHost, consoleHost, "status 200", "api GET " + apiHost + " / "},
			{apiHost, "other." + cpDomain, "status 200", "api GET " + apiHost + " / "},
			{consoleHost, apiHost, "status 421", "misdirected request\n"},
		} {
			answer, body := h2Answer(t, d, tt.authority, []hpack.HeaderField{{Name: "host", Value: tt.host}})
			if answer != tt.answer || !strings.HasPrefix(body, tt.body) {
				t.Errorf(":authority %s, host %s: %s %q; want %s %q", tt.authority, tt.host, answer, body, tt.answer, tt.body)
			}
		}
	})
	t.Run(":80 redirects", func(t *testing.T) {
		for _, tt := range []struct {
			url, location string
			code          int
		}{
			{"http://" + apiHost + "/a/b?c=d", "https://" + apiHost + "/a/b?c=d", 308},
			{"http://" + consoleHost + "/", "https://" + consoleHost + "/", 308},
			{"http://" + webHost + "/x", "https://" + webHost + "/x", 308},
			{"http://example.com/", "", 404},
		} {
			code, body, h := do(t, d.client1, http.MethodPost, tt.url, strings.NewReader("x"), nil)
			if code != tt.code || h.Get("Location") != tt.location {
				t.Errorf("%s: %d to %q; want %d to %q", tt.url, code, h.Get("Location"), tt.code, tt.location)
			}
			if tt.code == 308 && body != "" {
				t.Errorf("%s: a body %q", tt.url, body)
			}
		}
	})
	t.Run("api down", func(t *testing.T) {
		o := defaultOptions()
		o.api = nil
		down := startDoor(t, d.roots, o)
		code, body, _ := do(t, down.client, http.MethodGet, "https://"+apiHost+"/", nil, nil)
		if code != http.StatusBadGateway || body != "the API server did not answer\n" {
			t.Errorf("got %d %q; want 502", code, body)
		}
	})
}

// TestConsoleNotInstalled: without the console's files, its pages say so;
// its API paths still work.
func TestConsoleNotInstalled(t *testing.T) {
	c := newCA(t)
	empty := t.TempDir()
	for _, dir := range []string{"", empty, filepath.Join(empty, "missing")} {
		o := defaultOptions()
		o.consoleDir = dir
		d := startDoor(t, c, o)
		if code, body, _ := do(t, d.client, http.MethodGet, "https://"+consoleHost+"/", nil, nil); code != 503 || body != "the console is not installed\n" {
			t.Errorf("console dir %q: %d %q", dir, code, body)
		}
		if code, _, _ := do(t, d.client, http.MethodGet, "https://"+consoleHost+"/v1/x", nil, nil); code != 200 {
			t.Errorf("console dir %q, API path: %d", dir, code)
		}
	}
}
