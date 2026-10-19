package front

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/edge"
)

// pipeBody is a rateBody over the server end of a pipe, its deadlines the
// pipe's, with the client end the test sends the body on. Both ends are
// closed when the test's bubble returns, which must leave no goroutine
// blocked.
func pipeBody(t *testing.T, rate int64, timeout time.Duration) (*rateBody, *sender) {
	t.Helper()
	c, s := net.Pipe()
	snd := &sender{c: c, stop: make(chan struct{})}
	t.Cleanup(func() {
		_ = c.Close()
		_ = s.Close()
	})
	return &rateBody{body: s, deadline: s.SetReadDeadline, rate: float64(rate), timeout: timeout}, snd
}

// sender sends a body on a pipe's client end until stopped.
type sender struct {
	c    net.Conn
	stop chan struct{}
}

// send writes n bytes every interval, times times, then closes the pipe;
// less if it is stopped or closed first.
func (s *sender) send(n int, every time.Duration, times int) {
	defer func() { _ = s.c.Close() }()
	for range times {
		if _, err := s.c.Write(make([]byte, n)); err != nil {
			return
		}
		select {
		case <-time.After(every):
		case <-s.stop:
			return
		}
	}
}

// close stops the sender and closes the pipe's client end.
func (s *sender) close() {
	close(s.stop)
	_ = s.c.Close()
}

func TestATrickledBodyIsCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, c := pipeBody(t, 1024, 30*time.Second)
		defer c.close()
		// A byte every 10 s: each wait is within the timeout, the average
		// far below the rate.
		go c.send(1, 10*time.Second, 1000)
		start := time.Now()
		n, err := io.Copy(io.Discard, b)
		if !errors.Is(err, edge.ErrBodyTooSlow) {
			t.Fatalf("read %d bytes, then %v; want the body cut", n, err)
		}
		if took := time.Since(start); took > 31*time.Second {
			t.Errorf("cut after %v; want within the 30 s the first bytes earn", took)
		}
		// It stays cut.
		if _, err := b.Read(make([]byte, 1)); !errors.Is(err, edge.ErrBodyTooSlow) {
			t.Errorf("a read after the cut: %v", err)
		}
	})
}

func TestABodyAtTheRateIsRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, c := pipeBody(t, 1024, 30*time.Second)
		defer c.close()
		// Two minutes at twice the rate, in bursts a second apart.
		go c.send(2048, time.Second, 120)
		n, err := io.Copy(io.Discard, b)
		if err != nil || n != 120*2048 {
			t.Errorf("read %d bytes, %v; want all %d", n, err, 120*2048)
		}
	})
}

// TestOnlyWaitingForTheClientCounts: a reader that takes its time, as a
// backend slow to take the body does, costs the client nothing.
func TestOnlyWaitingForTheClientCounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, c := pipeBody(t, 1024, 30*time.Second)
		defer c.close()
		go c.send(1, 0, 10)
		p := make([]byte, 1)
		for i := range 10 {
			if _, err := b.Read(p); err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
			time.Sleep(time.Minute) //nolint:forbidigo // virtual time
		}
	})
}

// TestAStallIsCutWhateverCameBefore: a body sent fast earns no stall
// longer than the timeout, so it cannot bank time to hold the connection.
func TestAStallIsCutWhateverCameBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, c := pipeBody(t, 1024, 30*time.Second)
		defer c.close()
		go c.send(1<<20, time.Hour, 2)
		if _, err := io.ReadFull(b, make([]byte, 1<<20)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := b.Read(make([]byte, 1))
		if !errors.Is(err, edge.ErrBodyTooSlow) {
			t.Fatalf("a stall after 1 MiB: %v; want the body cut", err)
		}
		if took := time.Since(start); took != 30*time.Second {
			t.Errorf("cut after a stall of %v; want the timeout, 30 s", took)
		}
	})
}

// discardingBody is an HTTP/1 body as net/http's server has it: closed,
// it reads what is left of it from the connection.
type discardingBody struct{ net.Conn }

func (b discardingBody) Close() error {
	_, err := io.Copy(io.Discard, b.Conn)
	return err
}

// TestWhatIsLeftIsBounded: what net/http reads of a body as it is closed,
// or after the handler, has the timeout over HTTP/1; over HTTP/2 the
// deadline is left alone, as a stream's body is not read after either.
func TestWhatIsLeftIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, c := pipeBody(t, 1024, 30*time.Second)
		b.http1 = true
		b.body = discardingBody{b.body.(net.Conn)}
		defer c.close()
		go c.send(1, 10*time.Second, 1000)
		start := time.Now()
		if err := b.Close(); !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != 30*time.Second {
			t.Errorf("closing a trickled body over HTTP/1: %v after %v; want a deadline of 30 s", err, time.Since(start))
		}
		if !b.cut() {
			t.Error("a body whose close ran out of time is not cut")
		}

		b, c = pipeBody(t, 1024, 30*time.Second)
		b.http1 = true
		b.body = discardingBody{b.body.(net.Conn)}
		defer c.close()
		go c.send(1024, 0, 3)
		if err := b.Close(); err != nil {
			t.Errorf("closing a body that comes in time: %v", err)
		}

		b, _ = pipeBody(t, 1024, 30*time.Second)
		b.http1 = true
		b.handlerDone()
		start = time.Now()
		_, err := b.body.Read(make([]byte, 1))
		if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != 30*time.Second {
			t.Errorf("a read past the handler over HTTP/1: %v after %v; want a deadline of 30 s", err, time.Since(start))
		}

		b, _ = pipeBody(t, 1024, 30*time.Second)
		b.handlerDone()
		read := make(chan error, 1)
		go func() {
			_, err := b.body.Read(make([]byte, 1))
			read <- err
		}()
		time.Sleep(time.Hour) //nolint:forbidigo // virtual time
		synctest.Wait()
		select {
		case err := <-read:
			t.Errorf("over HTTP/2 the read ended: %v", err)
		default:
		}
		_ = b.body.Close()
		<-read
	})
}
