package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/boot"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
	"github.com/YukinaMochizuki/hangar/internal/guest/tailnet"
)

// startTailnet starts the container's tailnet, once its root is the
// container's and its files are made: joined as the boot disk says, before
// the container's process, or none until the node says otherwise through
// the agent. It returns the boot config to start the process from: with the
// CLI on its PATH when it joins as it boots. Nothing of it fails the boot:
// a guest that cannot join says why in its status.
func startTailnet(ctx context.Context, cfg *guestv1.BootConfig, reaper *supervise.Reaper, logf func(string, ...any)) (*guestv1.BootConfig, *tailnet.Tailnet) {
	dns := &guestDNS{
		root: "/", listen: net.JoinHostPort(tailnet.ResolverAddr, "53"),
		nameservers: cfg.GetNameservers(), search: cfg.GetSearch(),
	}
	tn := tailnet.New(cfg.GetTailscaleDisk(), reaper, dns, logf)
	tn.Run(ctx)
	if ts := cfg.GetTailscale(); ts != nil {
		tn.Apply(ctx, configOf(ts))
		if p := cfg.GetProcess(); p != nil && tn.Mounted() {
			cfg = proto.Clone(cfg).(*guestv1.BootConfig)
			cfg.Process.Env = tailnet.WithPath(p.GetEnv())
		}
	}
	return cfg, tn
}

// tailnetGrace is how long tailscaled has between SIGTERM and SIGKILL as
// the container stops.
const tailnetGrace = 2 * time.Second

// withTailnet starts the container's tailnet as startTailnet does, runs
// the container with it, and stops tailscaled once the container's process
// has ended, however run returns, before its exit is recorded: nothing is
// said on the console after that.
func withTailnet(ctx context.Context, cfg *guestv1.BootConfig, reaper *supervise.Reaper, logf func(string, ...any), run func(*guestv1.BootConfig, *tailnet.Tailnet) (console.ExitStatus, error)) (console.ExitStatus, error) {
	cfg, tn := startTailnet(ctx, cfg, reaper, logf)
	defer tn.Stop(tailnetGrace)
	return run(cfg, tn)
}

func configOf(ts *guestv1.Tailscale) *tailnet.Config {
	if ts == nil {
		return nil
	}
	return &tailnet.Config{Hostname: ts.GetHostname(), AcceptRoutes: ts.GetAcceptRoutes()}
}

// guestDNS switches the /etc/resolv.conf of the container at root between
// the split's resolver, which it starts on listen as the container joins,
// and the boot disk's resolvers.
type guestDNS struct {
	root, listen        string
	nameservers, search []string

	mu    sync.Mutex
	split *tailnet.Resolver
}

func (d *guestDNS) Split() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.split == nil {
		ups, err := tailnet.Upstreams(d.nameservers)
		if err != nil {
			return err
		}
		r := &tailnet.Resolver{Upstreams: ups, MagicDNS: tailnet.MagicDNS}
		if _, err := r.Serve(d.listen); err != nil {
			return err
		}
		d.split = r
	}
	return boot.SetResolvers(d.root, []string{tailnet.ResolverAddr}, d.search)
}

// Restore points resolv.conf back at the boot disk's resolvers, then
// stops the split's resolver: a guest that left its tailnet listens on
// nothing of the platform's, as one that never joined.
func (d *guestDNS) Restore() error {
	if err := boot.SetResolvers(d.root, d.nameservers, d.search); err != nil {
		return err
	}
	d.mu.Lock()
	r := d.split
	d.split = nil
	d.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.Close()
}

func (d *guestDNS) SetSuffix(suffix string) {
	d.mu.Lock()
	r := d.split
	d.mu.Unlock()
	if r != nil {
		r.SetSuffix(suffix)
	}
}

// tailnetAgent has the guest agent answer for tn.
func tailnetAgent(s *agent.Server, tn *tailnet.Tailnet) {
	if tn == nil {
		return
	}
	s.TailnetState = tn.Report
	s.TailnetSet = func(ts *guestv1.Tailscale) { tn.Set(configOf(ts)) }
	s.TailnetJoin = func(key string) error {
		err := tn.Join(key)
		switch {
		case errors.Is(err, tailnet.ErrHasState):
			return fmt.Errorf("%w: %w", agent.ErrTailnetHasState, err)
		case errors.Is(err, tailnet.ErrBusy):
			return fmt.Errorf("%w: %w", agent.ErrTailnetBusy, err)
		}
		return err
	}
	s.ExecEnv = func(env []string) []string {
		if !tn.Mounted() {
			return env
		}
		return tailnet.WithPath(slices.Clone(env))
	}
}
