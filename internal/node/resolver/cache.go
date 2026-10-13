package resolver

import (
	"sync"
	"time"

	"github.com/miekg/dns"
)

// maxCacheTTL caps how long an answer is kept, whatever its TTL says.
const maxCacheTTL = time.Hour

type cacheKey struct {
	name          string
	qtype, qclass uint16
}

type cacheEntry struct {
	msg             *dns.Msg
	stored, expires time.Time
}

// cache keeps upstream answers for as long as their TTLs allow: a name's
// records for the least of theirs, a name or type found missing for what its
// zone's SOA says (RFC 2308). Answers without either are not kept.
type cache struct {
	mu      sync.Mutex
	max     int
	entries map[cacheKey]cacheEntry
}

func newCache(size int) *cache {
	return &cache{max: size, entries: map[cacheKey]cacheEntry{}}
}

// get returns a copy of the answer for k, its TTLs lowered by the time it
// has been kept.
func (c *cache) get(k cacheKey, now time.Time) (*dns.Msg, bool) {
	c.mu.Lock()
	e, ok := c.entries[k]
	if ok && !now.Before(e.expires) {
		delete(c.entries, k)
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	m := e.msg.Copy()
	aged := uint32(now.Sub(e.stored) / time.Second)
	for _, section := range [][]dns.RR{m.Answer, m.Ns} {
		for _, rr := range section {
			h := rr.Header()
			h.Ttl -= min(aged, h.Ttl)
		}
	}
	return m, true
}

func (c *cache) put(k cacheKey, m *dns.Msg, now time.Time) {
	ttl := cacheTTL(m)
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[k]; !ok && len(c.entries) >= c.max {
		c.evictLocked(now)
	}
	c.entries[k] = cacheEntry{msg: m, stored: now, expires: now.Add(min(ttl, maxCacheTTL))}
}

// evictLocked makes room for one entry: every expired one goes, or else
// whichever map iteration comes to first.
func (c *cache) evictLocked(now time.Time) {
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < c.max {
		return
	}
	for k := range c.entries {
		delete(c.entries, k)
		return
	}
}

func cacheTTL(m *dns.Msg) time.Duration {
	if m.Rcode == dns.RcodeSuccess && len(m.Answer) > 0 {
		least := m.Answer[0].Header().Ttl
		for _, rr := range m.Answer[1:] {
			least = min(least, rr.Header().Ttl)
		}
		return time.Duration(least) * time.Second
	}
	if m.Rcode == dns.RcodeSuccess || m.Rcode == dns.RcodeNameError {
		for _, rr := range m.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				return time.Duration(min(soa.Hdr.Ttl, soa.Minttl)) * time.Second
			}
		}
	}
	return 0
}
