// Package acmetest is a DNS server for ACME DNS-01 challenges in tests,
// which is also the DNS provider that fills it: it answers SOA for its
// zones, which certmagic asks to find a name's zone, and TXT for the
// records the provider was given, which the CA and certmagic's propagation
// check ask. Point the test CA's resolver (pebble's -dnsserver) and
// front.CertConfig.Resolvers at Addr. The front door's tests use it, and so
// does a hangar-edge built with the testbed tag, for the L4 test (docs/runbook/testbed.md).
package acmetest

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/libdns/libdns"
	"github.com/miekg/dns"
)

// Server is the DNS server and its provider.
type Server struct {
	// Addr is where it answers, over UDP and TCP, as host:port.
	Addr string

	zones []string
	udp   *dns.Server
	tcp   *dns.Server

	mu       sync.Mutex
	txt      map[string][]string
	appended int
}

// Start serves zones, the domains names are under, on addr ("127.0.0.1:0"
// for any port), until Close.
func Start(addr string, zones ...string) (*Server, error) {
	s := &Server{txt: map[string][]string{}}
	for _, z := range zones {
		s.zones = append(s.zones, dns.Fqdn(strings.ToLower(z)))
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	s.Addr = pc.LocalAddr().String()
	s.udp = &dns.Server{PacketConn: pc, Handler: s}
	s.tcp = &dns.Server{Listener: ln, Handler: s}
	for _, srv := range []*dns.Server{s.udp, s.tcp} {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		go func() { _ = srv.ActivateAndServe() }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			s.Close()
			return nil, fmt.Errorf("the DNS server on %s did not start", s.Addr)
		}
	}
	return s, nil
}

// Close stops the server.
func (s *Server) Close() {
	_ = s.udp.Shutdown()
	_ = s.tcp.Shutdown()
}

// zoneOf is the zone name is in, if it is in one.
func (s *Server) zoneOf(name string) (string, bool) {
	for _, z := range s.zones {
		if name == z || strings.HasSuffix(name, "."+z) {
			return z, true
		}
	}
	return "", false
}

// ServeDNS answers a query.
func (s *Server) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	for _, q := range req.Question {
		name := strings.ToLower(q.Name)
		zone, ok := s.zoneOf(name)
		if !ok {
			m.Rcode = dns.RcodeRefused
			continue
		}
		soa := &dns.SOA{
			Hdr: dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
			Ns:  "ns." + zone, Mbox: "hostmaster." + zone, Serial: 1, Refresh: 60, Retry: 60, Expire: 60, Minttl: 1,
		}
		switch q.Qtype {
		case dns.TypeSOA:
			if name == zone {
				m.Answer = append(m.Answer, soa)
			} else {
				m.Ns = append(m.Ns, soa)
			}
		case dns.TypeTXT:
			s.mu.Lock()
			for _, v := range s.txt[name] {
				m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 1}, Txt: []string{v}})
			}
			s.mu.Unlock()
			if len(m.Answer) == 0 {
				m.Ns = append(m.Ns, soa)
			}
		default:
			m.Ns = append(m.Ns, soa)
		}
	}
	_ = w.WriteMsg(m)
}

// TXT returns the TXT records of name now.
func (s *Server) TXT(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.txt[dns.Fqdn(strings.ToLower(name))]...)
}

// Appended is how many records the provider was given, ever.
func (s *Server) Appended() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appended
}

// AppendRecords adds TXT records to zone; it is libdns.RecordAppender.
func (s *Server) AppendRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range recs {
		rr := r.RR()
		if rr.Type != "TXT" {
			return nil, fmt.Errorf("only TXT records, not %s", rr.Type)
		}
		name := strings.ToLower(libdns.AbsoluteName(rr.Name, zone))
		s.txt[name] = append(s.txt[name], rr.Data)
		s.appended++
	}
	return recs, nil
}

// DeleteRecords removes TXT records from zone; it is libdns.RecordDeleter.
func (s *Server) DeleteRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range recs {
		rr := r.RR()
		name := strings.ToLower(libdns.AbsoluteName(rr.Name, zone))
		var kept []string
		for _, v := range s.txt[name] {
			if v != rr.Data {
				kept = append(kept, v)
			}
		}
		if len(kept) == 0 {
			delete(s.txt, name)
		} else {
			s.txt[name] = kept
		}
	}
	return recs, nil
}
