package agent

import (
	"errors"
	"fmt"
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

// TestVolumeUsageAndGrowth: a guest that mounts no volumes of its own says
// it cannot, which the node takes as one from before it could; a device no
// volume is mounted read-write from is the caller's error; the rest is the
// guest's.
func TestVolumeUsageAndGrowth(t *testing.T) {
	usage := connect.NewRequest(&guestv1.VolumeUsageRequest{Device: "/dev/vdc"})
	grow := connect.NewRequest(&guestv1.GrowVolumeRequest{Device: "/dev/vdc"})
	if _, err := (&Server{}).VolumeUsage(t.Context(), usage); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("VolumeUsage without StatVolume: %v, want Unimplemented", err)
	}
	if _, err := (&Server{}).GrowVolume(t.Context(), grow); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("GrowVolume without ResizeVolume: %v, want Unimplemented", err)
	}

	var asked []string
	s := &Server{
		StatVolume: func(device string) (uint64, uint64, error) {
			asked = append(asked, device)
			return 4 << 30, 3 << 30, nil
		},
		ResizeVolume: func(device string) (uint64, error) {
			asked = append(asked, device)
			return 8 << 30, nil
		},
	}
	if res, err := s.VolumeUsage(t.Context(), usage); err != nil || res.Msg.GetSize() != 4<<30 || res.Msg.GetUsed() != 3<<30 {
		t.Errorf("VolumeUsage: %v %v", res, err)
	}
	if res, err := s.GrowVolume(t.Context(), grow); err != nil || res.Msg.GetSize() != 8<<30 {
		t.Errorf("GrowVolume: %v %v", res, err)
	}
	if len(asked) != 2 || asked[0] != "/dev/vdc" || asked[1] != "/dev/vdc" {
		t.Errorf("asked of %q, want /dev/vdc twice", asked)
	}

	for name, tt := range map[string]struct {
		err  error
		code connect.Code
	}{
		"no volume":     {fmt.Errorf("/dev/vda: %w", ErrNoVolume), connect.CodeInvalidArgument},
		"its own error": {errors.New("statfs: EIO"), connect.CodeInternal},
	} {
		s := &Server{
			StatVolume:   func(string) (uint64, uint64, error) { return 0, 0, tt.err },
			ResizeVolume: func(string) (uint64, error) { return 0, tt.err },
		}
		if _, err := s.VolumeUsage(t.Context(), usage); connect.CodeOf(err) != tt.code {
			t.Errorf("VolumeUsage, %s: %v, want %v", name, err, tt.code)
		}
		if _, err := s.GrowVolume(t.Context(), grow); connect.CodeOf(err) != tt.code {
			t.Errorf("GrowVolume, %s: %v, want %v", name, err, tt.code)
		}
	}
}
