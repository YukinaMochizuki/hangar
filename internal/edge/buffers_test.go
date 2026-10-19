package edge_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/edge"
)

// zeros reads as many zero bytes as asked.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestProxyBuffersAreSmall: the proxies' copy buffers are 8 KiB, and one of
// another size given back is not lent again.
func TestProxyBuffersAreSmall(t *testing.T) {
	b := edge.ProxyBuffers.Get()
	if len(b) != 8<<10 {
		t.Fatalf("a buffer of %d bytes; want 8 KiB", len(b))
	}
	edge.ProxyBuffers.Put(make([]byte, 32<<10))
	for range 100 {
		if n := len(edge.ProxyBuffers.Get()); n != 8<<10 {
			t.Fatalf("lent a buffer of %d bytes", n)
		}
	}
	edge.ProxyBuffers.Put(b)
}

// BenchmarkProxyDownload is what a large download through a reverse proxy
// set as the edge's are, flushing each write, comes to with httputil's
// 32 KiB copy buffer and with ProxyBuffers' 8 KiB.
func BenchmarkProxyDownload(b *testing.B) {
	const size = 64 << 20
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = io.CopyN(w, zeros{}, size)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	for _, tt := range []struct {
		name string
		pool httputil.BufferPool
	}{{"httputil-32KiB", nil}, {"ProxyBuffers-8KiB", edge.ProxyBuffers}} {
		b.Run(tt.name, func(b *testing.B) {
			proxy := httptest.NewServer(&httputil.ReverseProxy{
				Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
				FlushInterval: -1,
				BufferPool:    tt.pool,
			})
			defer proxy.Close()
			b.SetBytes(size)
			for b.Loop() {
				resp, err := http.Get(proxy.URL)
				if err != nil {
					b.Fatal(err)
				}
				if n, _ := io.Copy(io.Discard, resp.Body); n != size {
					b.Fatalf("read %d of %d", n, size)
				}
				_ = resp.Body.Close()
			}
		})
	}
}
