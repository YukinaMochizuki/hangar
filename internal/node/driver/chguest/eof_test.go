package chguest_test

import (
	"errors"
	"io"
	"testing"
	"time"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
)

// TestAnExecEndsWhenBothSidesHave: after the exit the call stays open until
// the caller closes its side, then ends cleanly, with io.EOF.
func TestAnExecEndsWhenBothSidesHave(t *testing.T) {
	c, done := guestOn(fakeCH(t, agent.Port)).Client("m1")
	defer done()
	s := c.Exec(t.Context())
	if err := s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Start{Start: &guestv1.ExecStart{Process: process("true")}}}); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Receive(); err != nil || res.GetExit() == nil {
		t.Fatalf("first response %v %v, want the exit", res, err)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := s.Receive()
		ended <- err
	}()
	select {
	case err := <-ended:
		t.Fatalf("the call ended before the caller closed its side: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := s.CloseRequest(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Errorf("the call ended with %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end once both sides had")
	}
}
