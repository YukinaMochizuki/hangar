package node

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// TestThePullerSaysWhenAnImageIsReady: the agent hears as soon as an image
// it waits on is ready, rather than at its next look.
func TestThePullerSaysWhenAnImageIsReady(t *testing.T) {
	host := fake.New()
	host.AddImage("app", "sha256:1", driver.ImageConfig{Cmd: []string{"true"}})
	letGo := host.HoldUnpack("app")
	p := newPuller(host.Images(), host.Storage(), clockwork.NewRealClock(), time.Minute)
	ready := make(chan struct{}, 1)
	p.start(t.Context(), func() { ready <- struct{}{} })

	if _, _, ok, err := p.get("m1", "app", ""); ok || err != nil {
		t.Fatalf("ready before it was pulled: %t %v", ok, err)
	}
	letGo()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("never told the image was ready")
	}
	digest, config, ok, err := p.get("m1", "app", "")
	if !ok || err != nil || digest != "sha256:1" || len(config.Cmd) != 1 {
		t.Errorf("get after ready: %s %+v %t %v", digest, config, ok, err)
	}
}
