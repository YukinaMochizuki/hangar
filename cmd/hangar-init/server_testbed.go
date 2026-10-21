//go:build testbed

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// A build with the testbed tag makes an initramfs for the L4 tests alone
// (docs/runbook/testbed.md): a FileShare's server it boots runs what the
// host asks, so a test can try, from inside the server, what a server whose
// guest is taken would. It is never released.

// buildMarker tells a testbed build's agent apart by its version, for the
// node and the tests to see.
const buildMarker = "+testbed"

// serverReaper lets the server's agent run processes.
func serverReaper(logf func(string, ...any)) *supervise.Reaper {
	logf("hangar-init: a testbed build: this share's server runs what the host asks")
	return supervise.NewReaper()
}

// /init netprobe and /init netlisten, run by the host's Exec in a testbed
// server.
func init() {
	if os.Getpid() == 1 || len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "netprobe":
		os.Exit(netprobe(os.Args[2:]))
	case "netlisten":
		os.Exit(netlisten(os.Args[2:]))
	}
}

// netlisten answers TCP and UDP on the ports given, tcp:PORT and udp:PORT,
// by echoing what it is sent, so that what keeps a client from them is no
// closed port. -forward turns IPv4 forwarding on, so that what keeps a
// packet from crossing the server is no closed stack either. It prints
// "listening" once it does, and runs until it is killed.
func netlisten(args []string) int {
	fs := flag.NewFlagSet("netlisten", flag.ContinueOnError)
	forward := fs.Bool("forward", false, "turn IPv4 forwarding on")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *forward {
		for _, f := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv4/conf/all/rp_filter"} {
			v := "1"
			if strings.HasSuffix(f, "rp_filter") {
				v = "0"
			}
			if err := os.WriteFile(f, []byte(v), 0); err != nil {
				fmt.Println("forwarding:", err)
				return 1
			}
		}
	}
	for _, a := range fs.Args() {
		proto, port, _ := strings.Cut(a, ":")
		switch proto {
		case "tcp":
			ln, err := net.Listen("tcp", ":"+port)
			if err != nil {
				fmt.Println(a+":", err)
				return 1
			}
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
				}
			}()
		case "udp":
			pc, err := net.ListenPacket("udp", ":"+port)
			if err != nil {
				fmt.Println(a+":", err)
				return 1
			}
			go func() {
				buf := make([]byte, 1500)
				for {
					n, from, err := pc.ReadFrom(buf)
					if err != nil {
						return
					}
					_, _ = pc.WriteTo(buf[:n], from)
				}
			}()
		default:
			fmt.Println("unknown:", a)
			return 2
		}
	}
	fmt.Println("listening")
	select {}
}

// netprobe takes targets tcp:HOST:PORT, udp:HOST:PORT and icmp:HOST, a
// host an IPv4 or IPv6 address, an IPv6 one with its zone, in brackets
// where a port follows. Before them it can do what a taken server would to
// get out: -route4 adds a default route through the gateway of eth0, the .1
// of its range; -addr6 turns IPv6 on, takes the address on eth0 and routes
// ::/0 through fe80::1. Each target's line is the target and its verdict:
// "ok" if a TCP connection is made, a UDP datagram answered (the DNS port
// is sent a query), or an ICMP echo answered; "blocked" if nothing answered
// in time, or no neighbour did; "answered" if something refused it, a
// reset or a port unreachable; "local" if the probe failed itself (no
// route, no such address, a bad target, a socket it could not make). Only
// "blocked" says the network stopped it.
func netprobe(args []string) int {
	fs := flag.NewFlagSet("netprobe", flag.ContinueOnError)
	route4 := fs.Bool("route4", false, "add an IPv4 default route through eth0's .1")
	addr6 := fs.String("addr6", "", "take this IPv6 address, with ::/0 through fe80::1")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := probeSetup(*route4, *addr6); err != nil {
		fmt.Println("setup:", err)
		return 1
	}
	results := make([]string, fs.NArg())
	var wg sync.WaitGroup
	for i, target := range fs.Args() {
		wg.Go(func() {
			err := probe(target)
			results[i] = strings.TrimSpace(fmt.Sprintf("%s %s %s", target, verdict(err), errText(err)))
		})
	}
	wg.Wait()
	for _, r := range results {
		fmt.Println(r)
	}
	return 0
}

// verdict classes what a probe returned.
func verdict(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, unix.EHOSTUNREACH):
		return "blocked"
	case errors.Is(err, unix.ECONNREFUSED):
		return "answered"
	}
	return "local"
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func probeSetup(route4 bool, addr6 string) error {
	eth, err := netlink.LinkByName("eth0")
	if err != nil {
		return err
	}
	if route4 {
		addrs, err := netlink.AddrList(eth, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		if len(addrs) == 0 {
			return fmt.Errorf("no IPv4 address on %s", eth.Attrs().Name)
		}
		p, _ := netip.AddrFromSlice(addrs[0].IP.Mask(addrs[0].Mask).To4())
		gw := p.Next()
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: eth.Attrs().Index, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, Gw: gw.AsSlice()}); err != nil {
			return fmt.Errorf("default route through %s: %w", gw, err)
		}
	}
	if addr6 != "" {
		for _, name := range []string{"all", "default", eth.Attrs().Name} {
			if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", []byte("0"), 0); err != nil {
				return err
			}
		}
		a, err := netlink.ParseAddr(addr6 + "/128")
		if err != nil {
			return err
		}
		a.Flags |= unix.IFA_F_NODAD
		if err := netlink.AddrReplace(eth, a); err != nil {
			return fmt.Errorf("taking %s: %w", addr6, err)
		}
		// Link-local, for the gateway's route, is the kernel's: wait for it.
		deadline := time.Now().Add(5 * time.Second)
		for {
			ll, err := netlink.AddrList(eth, netlink.FAMILY_V6)
			if err != nil {
				return err
			}
			if hasUsableLinkLocal(ll) {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("no usable link-local address on %s after 5s: %v", eth.Attrs().Name, ll)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: eth.Attrs().Index, Dst: &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, Gw: net.ParseIP("fe80::1")}); err != nil {
			return fmt.Errorf("IPv6 default route: %w", err)
		}
	}
	return nil
}

func hasUsableLinkLocal(addrs []netlink.Addr) bool {
	for _, a := range addrs {
		if a.IP.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			return true
		}
	}
	return false
}

const probeTimeout = 3 * time.Second

func probe(target string) error {
	proto, rest, _ := strings.Cut(target, ":")
	switch proto {
	case "tcp":
		c, err := net.DialTimeout("tcp", rest, probeTimeout)
		if err != nil {
			return err
		}
		return c.Close()
	case "udp":
		return probeUDP(rest)
	case "icmp":
		return probeICMP(rest)
	}
	return fmt.Errorf("unknown target %q", target)
}

// dnsQuery asks for the A of "hangar.": any answer, even a refusal, is
// an answer.
var dnsQuery = []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 6, 'h', 'a', 'n', 'g', 'a', 'r', 0, 0, 1, 0, 1}

func probeUDP(hostPort string) error {
	c, err := net.DialTimeout("udp", hostPort, probeTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	msg := []byte("hangar netprobe\n")
	if _, port, _ := net.SplitHostPort(hostPort); port == "53" {
		msg = dnsQuery
	}
	_ = c.SetDeadline(time.Now().Add(probeTimeout))
	if _, err := c.Write(msg); err != nil {
		return err
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("an empty answer")
	}
	return nil
}

// icmpSeq tells apart the echoes of probes run at once, which share an id.
var icmpSeq atomic.Int32

func probeICMP(host string) error {
	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		return err
	}
	network, laddr := "ip4:icmp", "0.0.0.0"
	var typ, reply icmp.Type = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	protoNum := 1
	if ip.IP.To4() == nil {
		network, laddr = "ip6:ipv6-icmp", "::"
		typ, reply, protoNum = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, 58
	}
	c, err := icmp.ListenPacket(network, laddr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	id := os.Getpid() & 0xffff
	seq := int(icmpSeq.Add(1) & 0xffff)
	b, err := (&icmp.Message{Type: typ, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("hangar netprobe")}}).Marshal(nil)
	if err != nil {
		return err
	}
	if _, err := c.WriteTo(b, ip); err != nil {
		return err
	}
	_ = c.SetReadDeadline(time.Now().Add(probeTimeout))
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return err
		}
		m, err := icmp.ParseMessage(protoNum, buf[:n])
		if err != nil || m.Type != reply {
			continue
		}
		if e, ok := m.Body.(*icmp.Echo); ok && e.ID == id && e.Seq == seq && from.(*net.IPAddr).IP.Equal(ip.IP) {
			return nil
		}
	}
}
