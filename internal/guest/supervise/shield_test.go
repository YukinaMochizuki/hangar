package supervise_test

import (
	"bufio"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// TestShieldedSignalsEndNothing: a shielded process lives through the
// signals that would end a Go program, as PID 1 must.
func TestShieldedSignalsEndNothing(t *testing.T) {
	if os.Getenv("SHIELD_CHILD") == "1" {
		supervise.Shield()
		_, _ = os.Stdout.WriteString("ready\n")
		time.Sleep(time.Minute) //nolint:forbidigo // stands for a PID 1 that outlives the signals sent to it
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestShieldedSignalsEndNothing$")
	cmd.Env = append(os.Environ(), "SHIELD_CHILD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready, err := bufio.NewReader(out).ReadString('\n')
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill(); <-exited }()
	if err != nil {
		t.Fatalf("%q %v", ready, err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGSEGV, syscall.SIGUSR1, syscall.SIGPIPE} {
		_ = cmd.Process.Signal(sig)
		select {
		case err := <-exited:
			exited <- err // for the deferred wait
			t.Fatalf("%v ended it: %v", sig, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
