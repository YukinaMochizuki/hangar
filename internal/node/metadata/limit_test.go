package metadata

import (
	"errors"
	"net"
	"testing"
	"time"
)

// TestOneSourceTakesOnlyItsShare: an address holding its share of
// connections has the next closed, and another address is still let in.
func TestOneSourceTakesOnlyItsShare(t *testing.T) {
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := limitPerSource(inner, 2)
	defer func() { _ = l.Close() }()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func(from string) net.Conn {
		t.Helper()
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
		c, err := d.Dial("tcp4", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	closedByServer := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		var ne net.Error
		timedOut := errors.As(err, &ne) && ne.Timeout()
		return err != nil && !timedOut
	}

	a1, a2 := dial("127.0.0.1"), dial("127.0.0.1")
	first, second := <-accepted, <-accepted
	if closedByServer(a1) || closedByServer(a2) {
		t.Fatal("connections within the share were closed")
	}
	if extra := dial("127.0.0.1"); !closedByServer(extra) {
		t.Error("a connection past an address's share was let in")
	}
	other := dial("127.0.0.2")
	<-accepted
	if closedByServer(other) {
		t.Error("another address was refused for the first's connections")
	}
	// A connection closed gives its share back.
	_ = first.Close()
	_ = second.Close()
	again := dial("127.0.0.1")
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("a share given back was not taken")
	}
	if closedByServer(again) {
		t.Error("a connection within a share given back was closed")
	}
}
