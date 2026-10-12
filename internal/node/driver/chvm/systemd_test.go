package chvm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is a connection that ends with the context it was dialled
// with, as godbus's does.
type fakeConn struct {
	ctx    context.Context
	closed atomic.Bool
}

func (c *fakeConn) Close()      { c.closed.Store(true) }
func (c *fakeConn) alive() bool { return c.ctx.Err() == nil && !c.closed.Load() }

// TestADialOutlivesItsBound: a connection keeps the driver's context, not
// the request's or the watch's that bounded its dial.
func TestADialOutlivesItsBound(t *testing.T) {
	life, end := context.WithCancel(context.Background())
	defer end()
	bound, cancel := context.WithTimeout(context.Background(), time.Second)
	c, err := dialWithin(bound, life, func(ctx context.Context) (*fakeConn, error) { return &fakeConn{ctx: ctx}, nil })
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if !c.alive() {
		t.Fatal("the connection ended with the dial's bound")
	}
	end()
	if c.alive() {
		t.Error("the connection outlived the driver's context")
	}
}

// TestADialPastItsBound: a dial that takes too long is given up, and the
// connection it makes later is closed.
func TestADialPastItsBound(t *testing.T) {
	life, end := context.WithCancel(context.Background())
	defer end()
	release := make(chan struct{})
	made := make(chan *fakeConn, 1)
	bound, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := dialWithin(bound, life, func(ctx context.Context) (*fakeConn, error) {
		<-release
		c := &fakeConn{ctx: ctx}
		made <- c
		return c, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a dial past its bound: %v", err)
	}
	close(release)
	c := <-made
	deadline := time.Now().Add(5 * time.Second)
	for !c.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the late connection was left open")
		}
		time.Sleep(time.Millisecond)
	}
}
