// Package dnstest serves DNS for tests of what asks it: a server answering
// as a function says, over UDP and TCP on one port of its own, counting
// what it was asked.
package dnstest

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"
)

// Server is a DNS server on 127.0.0.1.
type Server struct {
	// Addr is where it listens, as host:port, over UDP and TCP.
	Addr string

	mu sync.Mutex
	// asked counts the questions, by name and type, recursed those that
	// asked for recursion, and overUDP those asked over UDP.
	asked, recursed, overUDP map[string]int
}

// Serve starts a server answering each question as answer fills the reply;
// it stops when the test ends.
func Serve(t testing.TB, answer func(q dns.Question, m *dns.Msg)) *Server {
	t.Helper()
	s := &Server{asked: map[string]int{}, recursed: map[string]int{}, overUDP: map[string]int{}}
	handler := func(udp bool) dns.Handler {
		return dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(req)
			for _, q := range req.Question {
				key := Key(q.Name, q.Qtype)
				s.mu.Lock()
				s.asked[key]++
				if req.RecursionDesired {
					s.recursed[key]++
				}
				if udp {
					s.overUDP[key]++
				}
				s.mu.Unlock()
				answer(q, m)
			}
			_ = w.WriteMsg(m)
		})
	}
	ln, pc := listen(t)
	s.Addr = ln.Addr().String()
	for _, srv := range []*dns.Server{
		{PacketConn: pc, Handler: handler(true)},
		{Listener: ln, Handler: handler(false)},
	} {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		go func() { _ = srv.ActivateAndServe() }()
		<-started
		t.Cleanup(func() { _ = srv.Shutdown() })
	}
	return s
}

// listen finds a port free over both TCP and UDP.
func listen(t testing.TB) (net.Listener, net.PacketConn) {
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		pc, err := net.ListenPacket("udp", ln.Addr().String())
		if err == nil {
			return ln, pc
		}
		_ = ln.Close()
	}
	t.Fatal("no port free over both TCP and UDP")
	return nil, nil
}

// Key is how questions are counted: the name in lower case, a slash, the
// type, as "_acme-challenge.example.com./CNAME".
func Key(name string, qtype uint16) string {
	return strings.ToLower(dns.Fqdn(name)) + "/" + dns.TypeToString[qtype]
}

// Asked is how often the question of key was asked.
func (s *Server) Asked(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[key]
}

// Recursed is how often the question of key was asked with recursion.
func (s *Server) Recursed(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recursed[key]
}

// OverUDP is how often the question of key was asked over UDP.
func (s *Server) OverUDP(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overUDP[key]
}

// RR parses a record, failing the test if it does not.
func RR(t testing.TB, s string) dns.RR {
	t.Helper()
	r, err := dns.NewRR(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
