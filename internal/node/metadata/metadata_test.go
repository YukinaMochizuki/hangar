package metadata

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func newServer() *Server {
	s := New(Config{})
	s.Set("n1", map[netip.Addr]Instance{
		netip.MustParseAddr("10.0.0.5"): {UID: "uid-a", Hostname: "api", UserData: "#cloud-config\nruncmd: [true]\n", VendorData: "#cloud-config\n{}\n"},
	})
	return s
}

func get(t *testing.T, h http.Handler, method, from, path string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, "http://169.254.169.254"+path, nil)
	r.RemoteAddr = from
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func TestAMachineGetsItsOwn(t *testing.T) {
	h := newServer().Handler("n1")
	for path, want := range map[string]string{
		"/nocloud/meta-data":   "instance-id: uid-a\nlocal-hostname: api\n",
		"/nocloud/user-data":   "#cloud-config\nruncmd: [true]\n",
		"/nocloud/vendor-data": "#cloud-config\n{}\n",
	} {
		if code, body := get(t, h, http.MethodGet, "10.0.0.5:40000", path); code != 200 || body != want {
			t.Errorf("%s: %d %q, want %q", path, code, body, want)
		}
	}
}

func TestWhatIsNotServed(t *testing.T) {
	s := newServer()
	h := s.Handler("n1")
	for name, c := range map[string]struct{ method, from, path string }{
		"another address":         {http.MethodGet, "10.0.0.6:40000", "/nocloud/meta-data"},
		"another document":        {http.MethodGet, "10.0.0.5:40000", "/latest/meta-data/"},
		"a path outside nocloud":  {http.MethodGet, "10.0.0.5:40000", "/meta-data"},
		"a write":                 {http.MethodPost, "10.0.0.5:40000", "/nocloud/meta-data"},
		"a remote address broken": {http.MethodGet, "nonsense", "/nocloud/meta-data"},
	} {
		if code, body := get(t, h, c.method, c.from, c.path); code == 200 {
			t.Errorf("%s served: %q", name, body)
		}
	}
	if code, _ := get(t, s.Handler("n2"), http.MethodGet, "10.0.0.5:40000", "/nocloud/meta-data"); code != 404 {
		t.Errorf("the machine's address on another network: %d", code)
	}
	s.Forget("n1")
	if code, _ := get(t, h, http.MethodGet, "10.0.0.5:40000", "/nocloud/meta-data"); code != 404 {
		t.Errorf("a network forgotten: %d", code)
	}
}

// TestStoppingClosesConnections: Serve ends soon after its ctx, however long
// a client keeps its connection.
func TestStoppingClosesConnections(t *testing.T) {
	s := New(Config{})
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, "n1", l) }()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("GET /nocloud/meta-data HTTP/1.1\r\nHost: x\r\n")) // never finished
	time.Sleep(50 * time.Millisecond)
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve outlived its ctx while a client held a connection")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadAll(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Error("the client's connection was left open")
	}
}
