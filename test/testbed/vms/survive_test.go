//go:build testbed

package vms

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// agentEnv makes the test binary an agent of its own: it starts the VM of
// the spec it holds with a driver of its own, says so, and waits to be
// killed.
const agentEnv = "HANGAR_TESTBED_AGENT"

type agentJob struct {
	Config chvm.Config
	Spec   driver.VMSpec
}

func agent(job string) int {
	var j agentJob
	if err := json.Unmarshal([]byte(job), &j); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	hv, err := chvm.New(context.Background(), j.Config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := hv.Start(context.Background(), j.Spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("started")
	select {}
}

// TestTheVMOutlivesTheAgent: an agent killed outright leaves its VM
// running, and a new one finds it running, the same instance.
func TestTheVMOutlivesTheAgent(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "survivor", driver.RestartAlways, guestScript)
	ch, err := exec.LookPath("cloud-hypervisor")
	if err != nil {
		t.Fatal(err)
	}
	in := vmtest.Artifacts()
	job, _ := json.Marshal(agentJob{
		Config: chvm.Config{
			Dir: vmDir, CloudHypervisor: ch, Kernel: in.Kernel, Initramfs: in.Initramfs, Cmdline: in.Cmdline,
			Launcher: []string{in.Node, "vmm"}, Nameservers: []string{"1.1.1.1"},
		},
		Spec: spec,
	})
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), agentEnv+"="+string(job))
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "started" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("the agent said %q", line)
	}
	ready(t, "survivor")
	launcher, vmm := mainPID(t, "survivor"), chPID(t, "survivor")

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	time.Sleep(2 * time.Second)
	if !unitActive("survivor") || mainPID(t, "survivor") != launcher || chPID(t, "survivor") != vmm {
		t.Fatalf("the VM did not outlive its agent: launcher %d then %d, Cloud Hypervisor %d then %d",
			launcher, mainPID(t, "survivor"), vmm, chPID(t, "survivor"))
	}
	ready(t, "survivor")

	// The agent started again sees the VM as running its spec, and
	// starting the same spec changes nothing.
	again := newHypervisor(t, testGrace)
	vms, err := again.VMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].MachineUID != "survivor" || vms[0].State != driver.VMRunning || vms[0].SpecHash != spec.Hash() {
		t.Fatalf("the new agent lists %+v", vms)
	}
	if err := again.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if chPID(t, "survivor") != vmm {
		t.Error("starting the same spec again restarted the VM")
	}
	if err := again.Stop(context.Background(), "survivor"); err != nil {
		t.Fatal(err)
	}
	stoppedForGood(t, h, "survivor")
}
