package agent

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

func TestSetIPv6(t *testing.T) {
	req := connect.NewRequest(&guestv1.SetIPv6Request{Interfaces: []*guestv1.Interface{
		{Mac: "02:00:00:00:00:01", Ipv6: "2001:db8::a/128", Ipv6Gateway: "fe80::1"},
	}})

	// A guest whose network is not the agent's to change says so, which the
	// node takes as one that cannot change it while it runs.
	if _, err := (&Server{}).SetIPv6(t.Context(), req); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("without ApplyIPv6: %v, want Unimplemented", err)
	}

	var got []*guestv1.Interface
	s := &Server{ApplyIPv6: func(ifaces []*guestv1.Interface) error { got = ifaces; return nil }}
	if _, err := s.SetIPv6(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !proto.Equal(got[0], req.Msg.GetInterfaces()[0]) {
		t.Errorf("applied %v, want the request's %v", got, req.Msg.GetInterfaces())
	}

	s.ApplyIPv6 = func([]*guestv1.Interface) error { return errors.New("no interface has MAC 02:00:00:00:00:01") }
	if _, err := s.SetIPv6(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a refused request: %v, want InvalidArgument", err)
	}
}
