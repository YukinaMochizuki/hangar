package agent

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// TestTheTailnetRPCs: a guest with no tailnet says it cannot; one with a
// tailnet answers its status as is, takes a tailnet to join or none, and
// maps a refused key to the codes the node reads: FAILED_PRECONDITION for
// a guest with state, ABORTED for one not asking.
func TestTheTailnetRPCs(t *testing.T) {
	none := &Server{}
	if _, err := none.TailnetStatus(t.Context(), connect.NewRequest(&guestv1.TailnetStatusRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("status without a tailnet: %v, want Unimplemented", err)
	}
	if _, err := none.SetTailnet(t.Context(), connect.NewRequest(&guestv1.SetTailnetRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("set without a tailnet: %v, want Unimplemented", err)
	}
	if _, err := none.JoinTailnet(t.Context(), connect.NewRequest(&guestv1.JoinTailnetRequest{AuthKey: "k"})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("join without a tailnet: %v, want Unimplemented", err)
	}

	report := &guestv1.TailnetStatusResponse{State: guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY, Message: "m", WantsKey: true}
	var told []*guestv1.Tailscale
	var joinErr error
	s := &Server{
		TailnetState: func() *guestv1.TailnetStatusResponse { return report },
		TailnetSet:   func(ts *guestv1.Tailscale) { told = append(told, ts) },
		TailnetJoin:  func(string) error { return joinErr },
	}
	res, err := s.TailnetStatus(t.Context(), connect.NewRequest(&guestv1.TailnetStatusRequest{}))
	if err != nil || !proto.Equal(res.Msg, report) {
		t.Errorf("status: %v, %v; want %v", res, err, report)
	}
	ts := &guestv1.Tailscale{Hostname: "box", AcceptRoutes: true}
	for _, req := range []*guestv1.SetTailnetRequest{{Tailscale: ts}, {}} {
		if _, err := s.SetTailnet(t.Context(), connect.NewRequest(req)); err != nil {
			t.Fatal(err)
		}
	}
	if len(told) != 2 || !proto.Equal(told[0], ts) || told[1] != nil {
		t.Errorf("told %v, want the tailnet, then none", told)
	}
	for _, c := range []struct {
		err  error
		code connect.Code
	}{
		{nil, 0},
		{fmt.Errorf("%w: has state", ErrTailnetHasState), connect.CodeFailedPrecondition},
		{fmt.Errorf("%w: busy", ErrTailnetBusy), connect.CodeAborted},
		{errors.New("other"), connect.CodeInternal},
	} {
		joinErr = c.err
		_, err := s.JoinTailnet(t.Context(), connect.NewRequest(&guestv1.JoinTailnetRequest{AuthKey: "k"}))
		if c.err == nil && err != nil || c.err != nil && connect.CodeOf(err) != c.code {
			t.Errorf("join refused with %v: %v, want %v", c.err, err, c.code)
		}
	}
}
