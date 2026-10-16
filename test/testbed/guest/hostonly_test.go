//go:build testbed

package guest

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// TestOnlyTheHostReachesTheAgent: a USER nobody process reaches the agent at
// no CID, while the node's Exec, logs and Stop still work on its guest
// (docs/decisions/guest-agent.md).
func TestOnlyTheHostReachesTheAgent(t *testing.T) {
	probe := vmtest.Artifacts().VsockProbe
	if _, err := os.Stat(probe); err != nil {
		t.Fatalf("the vsock probe: %v (make e2e-host builds it)", err)
	}
	const self = vmtest.GuestCID
	// The positive control: a port the host listens on, which the probe
	// must reach at CID 2, or its failures prove nothing.
	const controlPort, greeting = agent.Port + 1, "hangar-testbed-control"
	cfg := &guestv1.BootConfig{Process: &guestv1.Process{
		Args: []string{"/hangar-vsockprobe", "1", fmt.Sprint(self), "2", fmt.Sprintf("2:%d", controlPort)},
		User: "nobody",
	}}
	v := vmtest.New(t, cfg)
	v.Put(probe, "hangar-vsockprobe")
	// Cloud Hypervisor takes a guest's connection to host port P to
	// the socket's path with _P appended.
	control, err := net.Listen("unix", fmt.Sprintf("%s_%d", v.Path("vsock"), controlPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close() }()
	go func() {
		for {
			c, err := control.Accept()
			if err != nil {
				return
			}
			_, _ = fmt.Fprintln(c, greeting)
			_ = c.Close()
		}
	}()
	v.Start()
	v.Ready(30 * time.Second)

	// The container's log, as the node reads it: the framed console.
	var out string
	deadline := time.Now().Add(60 * time.Second)
	for {
		recs, err := v.Console()
		if err != nil {
			t.Fatalf("reading the console: %v", err)
		}
		var b strings.Builder
		for _, r := range recs {
			if r.Stream == console.Stdout {
				b.Write(r.Payload)
			}
		}
		out = b.String()
		if strings.Contains(out, "vsockprobe: done\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the probe did not finish in 60s; its output so far: %q", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("the probe's output:\n%s", out)

	if m := regexp.MustCompile(`(?m)^vsockprobe: uid (\d+)$`).FindStringSubmatch(out); m == nil || m[1] != "65534" {
		t.Fatalf("the probe did not run as nobody (65534): %q", out)
	}
	result := func(cid, port uint32) string {
		t.Helper()
		m := regexp.MustCompile(fmt.Sprintf(`(?m)^vsockprobe: cid %d port %d: (.*)$`, cid, port)).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("cid %d port %d: the probe did not try it", cid, port)
		}
		if strings.HasPrefix(m[1], "socket ") {
			t.Fatalf("cid %d port %d: %s: the probe could not make a vsock socket, so it tested nothing", cid, port, m[1])
		}
		return m[1]
	}
	if got := result(2, controlPort); got != "read "+greeting {
		t.Fatalf("the control: %q, want %q: a USER nobody process cannot reach the host, so its failures below prove nothing", got, "read "+greeting)
	}
	// What a connection that reached nothing fails with. ENODEV, no
	// transport for the address, only within the guest: CID 2 always has one.
	// Seen (6.18.55, CH v53): CID 1 ETIMEDOUT, own CID ENODEV, CID 2 ECONNRESET.
	noPeer := []string{"connect ETIMEDOUT", "connect ECONNRESET", "connect ECONNREFUSED"}
	for _, tt := range []struct {
		cid  uint32
		want []string
	}{
		{1, slices.Concat(noPeer, []string{"connect ENODEV"})},
		{self, slices.Concat(noPeer, []string{"connect ENODEV"})},
		{2, noPeer},
	} {
		switch got := result(tt.cid, agent.Port); {
		case strings.HasPrefix(got, "ping ok"):
			t.Errorf("cid %d: a USER nobody process reached the guest agent: %s", tt.cid, got)
		case !slices.Contains(tt.want, got):
			t.Errorf("cid %d: %q, want a connect that failed for want of a peer, one of %q", tt.cid, got, tt.want)
		default:
			t.Logf("cid %d: %s", tt.cid, got)
		}
	}
	if msg := initMessages(t, v); strings.Contains(msg, "refused a connection") {
		t.Errorf("hangar-init refused a connection, so one got to it: %q", msg)
	}

	t.Run("Exec", func(t *testing.T) {
		if uid := strings.TrimSpace(v.MustExec(t, "id", "-u")); uid != "0" {
			t.Errorf("the node's exec runs as uid %s, want root as it asked", uid)
		}
		ps := v.MustExec(t, "ps", "-o", "user,args")
		if !regexp.MustCompile(`(?m)^\s*nobody\s+/hangar-vsockprobe`).MatchString(ps) {
			t.Errorf("the probe is not running as nobody:\n%s", ps)
		}
	})

	t.Run("Stop", func(t *testing.T) {
		c, done := v.Guest.Client("")
		defer done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.Stop(ctx, connect.NewRequest(&guestv1.StopRequest{Grace: durationpb.New(10 * time.Second)})); err != nil {
			t.Fatal(err)
		}
		v.WaitPowerOff(60 * time.Second)
		_, exit := outputAndExit(t, v)
		if exit != (console.ExitStatus{Code: 143, Signal: 15}) {
			t.Errorf("exit = %+v, want code 143, signal 15 from Stop's SIGTERM", exit)
		}
		v.CheckCleanDisk(t)
	})
}
