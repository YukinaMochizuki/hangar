package e2e

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const tailnetKey = "tskey-auth-kE2E1CNTRL-e2eSECRET"

func (w *world) applyTailscaleSecret(name, key string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Secret, &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &secretsv1alpha1.SecretSpec{
			Type: secretsv1alpha1.SecretType_SECRET_TYPE_TAILSCALE_AUTH,
			Data: map[string][]byte{secrets.TailscaleAuthKey: []byte(key)},
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func onTailnet(name, secret string) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Tailscale = &computev1alpha1.ContainerTailscale{AuthSecret: secret}
	return c
}

// waitTailnet waits for the container to say TailnetJoined with the reason.
func (w *world) waitTailnet(name, reason string) *metav1.Condition {
	w.t.Helper()
	var j *metav1.Condition
	testutil.Eventually(w.t, fmt.Sprintf("container %s TailnetJoined %s", name, reason), func() (bool, string) {
		c, _ := get[*computev1alpha1.Container](w, registry.Container, name)
		j = conditions.Find(c.GetStatus().GetConditions(), computeapi.TailnetJoined)
		return j.GetReason() == reason, fmt.Sprintf("%v", j)
	})
	return j
}

// TestAContainerJoinsItsTailnet: a container naming a tailscale-auth
// Secret that does not exist yet runs all the same, saying NoKey; once the
// Secret is there its guest is given the key and joins. Stopped and
// started, it comes up as the same node, given no key again.
func TestAContainerJoinsItsTailnet(t *testing.T) {
	t.Parallel()
	w := newSecretsWorld(t)
	w.apply(onTailnet("box", "ts"))
	w.waitReason("box", "Running")
	if j := w.waitTailnet("box", computeapi.ReasonTailnetNoKey); j.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("without the secret: %v", j)
	}
	uid := w.machine("box").GetMetadata().GetUid()
	if ts := w.host.Snapshot().VMs[uid].Spec.Tailscale; ts == nil || ts.Hostname != "box" || ts.AcceptRoutes {
		t.Errorf("the VM joins as %+v, want hostname box, no routes", ts)
	}

	w.applyTailscaleSecret("ts", tailnetKey)
	if j := w.waitTailnet("box", computeapi.ReasonTailnetJoined); j.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("joined: %v", j)
	}
	if keys := w.host.TailnetKeys(uid); !slices.Equal(keys, []string{tailnetKey}) {
		t.Errorf("the guest was given %q, want the key once", keys)
	}

	c := onTailnet("box", "ts")
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	w.apply(c)
	w.waitStopped("box", metav1.ConditionStatus_CONDITION_STATUS_TRUE)
	w.waitTailnet("box", computeapi.ReasonTailnetStarting)
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING
	w.apply(c)
	w.waitReason("box", "Running")
	w.waitTailnet("box", computeapi.ReasonTailnetJoined)
	if keys, with := w.host.TailnetKeys(uid), w.host.TailnetBootsWithState(uid); len(keys) != 1 || with != 1 {
		t.Errorf("started again: given %d keys, %d boots as the node; want no new key, one boot with its state", len(keys), with)
	}
}

// TestATailnetChangesWhileTheContainerRuns: a running container joins a
// tailnet once given one, takes a new hostname and routes, leaves it once
// it names none, and joins again as a new node, with the Secret's key as it
// is then, all without its VM starting again; its boot disk follows each
// step, and TailnetJoined comes and goes with the tailnet.
func TestATailnetChangesWhileTheContainerRuns(t *testing.T) {
	t.Parallel()
	w := newSecretsWorld(t)
	w.applyTailscaleSecret("ts", tailnetKey)
	c := container("box", "nginx")
	w.apply(c)
	w.waitReason("box", "Running")
	uid := w.machine("box").GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[uid]
	unrestarted := func(when string) {
		t.Helper()
		vm := w.host.Snapshot().VMs[uid]
		if vm.Boots != before.Boots || !vm.Started.Equal(before.Started) || vm.SpecHash != before.SpecHash {
			t.Errorf("%s: the VM started again: booted %d times, at %v; was %d, at %v", when, vm.Boots, vm.Started, before.Boots, before.Started)
		}
	}
	noCondition := func(what string) {
		t.Helper()
		testutil.Eventually(t, what, func() (bool, string) {
			got, _ := get[*computev1alpha1.Container](w, registry.Container, "box")
			m := w.machine("box")
			j, mj := conditions.Find(got.GetStatus().GetConditions(), computeapi.TailnetJoined), conditions.Find(m.GetStatus().GetConditions(), computeapi.TailnetJoined)
			return j == nil && mj == nil, fmt.Sprintf("container %v, machine %v", j, mj)
		})
	}
	bootDisk := func(want *driver.TailscaleSpec) {
		t.Helper()
		testutil.Eventually(t, fmt.Sprintf("the boot disk joining %+v", want), func() (bool, string) {
			ts := w.host.Snapshot().VMs[uid].Spec.Tailscale
			return ts == nil && want == nil || ts != nil && want != nil && *ts == *want, fmt.Sprintf("%+v", ts)
		})
	}
	noCondition("no TailnetJoined without a tailnet")

	c.Spec.Tailscale = &computev1alpha1.ContainerTailscale{AuthSecret: "ts"}
	w.apply(c)
	w.waitTailnet("box", computeapi.ReasonTailnetJoined)
	bootDisk(&driver.TailscaleSpec{Hostname: "box"})
	unrestarted("joined")

	c.Spec.Tailscale.Hostname, c.Spec.Tailscale.AcceptRoutes = "dev", true
	w.apply(c)
	testutil.Eventually(t, "the running node renamed", func() (bool, string) {
		j := w.waitTailnet("box", computeapi.ReasonTailnetJoined)
		return j.GetMessage() == "joined as dev", j.GetMessage()
	})
	bootDisk(&driver.TailscaleSpec{Hostname: "dev", AcceptRoutes: true})
	unrestarted("renamed")

	c.Spec.Tailscale = nil
	w.apply(c)
	noCondition("TailnetJoined gone with the tailnet")
	bootDisk(nil)
	unrestarted("left")

	w.applyTailscaleSecret("ts", tailnetKey+"-fresh")
	c.Spec.Tailscale = &computev1alpha1.ContainerTailscale{AuthSecret: "ts"}
	w.apply(c)
	w.waitTailnet("box", computeapi.ReasonTailnetJoined)
	unrestarted("joined again")
	if keys := w.host.TailnetKeys(uid); !slices.Equal(keys, []string{tailnetKey, tailnetKey + "-fresh"}) {
		t.Errorf("the guest was given %q; want the first key, then the fresh one after it left", keys)
	}
	if told := w.host.TailnetTold(uid); !slices.Equal(told, []string{"join box", "set dev", "leave", "join box"}) {
		t.Errorf("the guest was told %q", told)
	}
}

// TestARejectedTailnetKey: a key the tailnet refuses is KeyRejected, the
// container running all the same; once the guest's retry has passed, it is
// given the Secret's key again, and joins once that is one the tailnet
// takes.
func TestARejectedTailnetKey(t *testing.T) {
	t.Parallel()
	w := newSecretsWorld(t)
	w.host.RejectTailnetKey(tailnetKey, true)
	w.host.SetTailnetKeyRetry(200 * time.Millisecond)
	w.applyTailscaleSecret("ts", tailnetKey)
	w.apply(onTailnet("box", "ts"))
	j := w.waitTailnet("box", computeapi.ReasonTailnetKeyRejected)
	if j.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("a refused key: %v", j)
	}
	w.waitReason("box", "Running")
	w.applyTailscaleSecret("ts", tailnetKey+"-good")
	w.waitTailnet("box", computeapi.ReasonTailnetJoined)
	uid := w.machine("box").GetMetadata().GetUid()
	if keys := w.host.TailnetKeys(uid); len(keys) < 2 || keys[len(keys)-1] != tailnetKey+"-good" {
		t.Errorf("the guest was given %q; want the refused key, then the good one", keys)
	}
}

// TestATailnetWithoutTheDisk: on a host with no tailscale disk, a
// container joining a tailnet runs, and says the join Failed.
func TestATailnetWithoutTheDisk(t *testing.T) {
	t.Parallel()
	w := newSecretsWorld(t)
	w.host.NoTailscaleDisk()
	w.applyTailscaleSecret("ts", tailnetKey)
	w.apply(onTailnet("box", "ts"))
	w.waitTailnet("box", computeapi.ReasonTailnetFailed)
	w.waitReason("box", "Running")
}
