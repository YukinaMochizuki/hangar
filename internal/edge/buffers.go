package edge

import (
	"net/http/httputil"
	"sync"
)

// proxyBufferSize is the size of a reverse proxy's copy buffer: what a
// stream holds while its answer is copied, where httputil's own is 32 KiB
// (docs/decisions/front-door.md).
const proxyBufferSize = 8 << 10

// ProxyBuffers lends the edge's reverse proxies, the API's in the front
// door and the endpoints', their copy buffers.
var ProxyBuffers httputil.BufferPool = &bufferPool{}

type bufferPool struct{ pool sync.Pool }

func (b *bufferPool) Get() []byte {
	if p, ok := b.pool.Get().(*[]byte); ok {
		return *p
	}
	return make([]byte, proxyBufferSize)
}

func (b *bufferPool) Put(buf []byte) {
	if cap(buf) != proxyBufferSize {
		return
	}
	buf = buf[:proxyBufferSize]
	b.pool.Put(&buf)
}
