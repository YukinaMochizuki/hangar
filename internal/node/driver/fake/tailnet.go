package fake

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A container's guest joins its tailnet as hangar-init does: as it boots,
// if its boot disk says so, and whenever SetTailnet says, while it runs.
// With the node's state, kept with its root disk, it joins at once and with
// no key; without, it asks for a key, and again DefaultTailnetKeyRetry
// after a key the tailnet refused. Leaving deletes the state.

// DefaultTailnetKeyRetry is how long a fake guest waits after a refused
// key before it asks again, as hangar-init does.
const DefaultTailnetKeyRetry = 5 * time.Minute

// tailnetGuest is a guest's tailnet, in its current boot.
type tailnetGuest struct {
	// ts is what it was last told to join, nil for none.
	ts        *driver.TailscaleSpec
	status    driver.TailnetStatus
	keyFailed time.Time
}

// tailnetRoot is what a machine's root disk holds of its tailnet node, and
// what its guests were given and told over its life.
type tailnetRoot struct {
	joined bool
	// keys are the auth keys given, in order; bootsWithState counts the
	// boots that came up as the node with none; told is each SetTailnet,
	// "join HOSTNAME", "set HOSTNAME" or "leave".
	keys           []string
	bootsWithState int
	told           []string
}

func (h *Host) tailnetRootLocked(machineUID string) *tailnetRoot {
	if h.tailnetRoots == nil {
		h.tailnetRoots = map[string]*tailnetRoot{}
	}
	r, ok := h.tailnetRoots[machineUID]
	if !ok {
		r = &tailnetRoot{}
		h.tailnetRoots[machineUID] = r
	}
	return r
}

// bootTailnet has a booting container's guest join the tailnet its boot
// disk names, or none.
func (h *Host) bootTailnet(v *vm) {
	if h.tailnetGuests == nil {
		h.tailnetGuests = map[string]*tailnetGuest{}
	}
	g := &tailnetGuest{status: driver.TailnetStatus{State: driver.TailnetOff}}
	h.tailnetGuests[v.MachineUID] = g
	if ts := v.spec.Tailscale; ts != nil {
		h.joinLocked(v.MachineUID, g, ts, true)
	}
}

// joinLocked has guest g join as ts says. Callers hold h.mu.
func (h *Host) joinLocked(machineUID string, g *tailnetGuest, ts *driver.TailscaleSpec, boot bool) {
	c := *ts
	g.ts = &c
	r := h.tailnetRootLocked(machineUID)
	switch {
	case h.noTailscaleDisk:
		g.status = driver.TailnetStatus{State: driver.TailnetFailed, Message: "this guest's host has no tailscale disk to give it"}
	case r.joined:
		if boot {
			r.bootsWithState++
		}
		g.status = driver.TailnetStatus{State: driver.TailnetJoined, Message: "joined as " + c.Hostname}
	default:
		g.status = driver.TailnetStatus{State: driver.TailnetNeedsKey, Message: "waiting for the auth key"}
	}
}

// forgetTailnetRootLocked drops the node a machine's root disk held, as
// deleting the disk does.
func (h *Host) forgetTailnetRootLocked(machineUID string) {
	if r, ok := h.tailnetRoots[machineUID]; ok {
		r.joined = false
	}
}

// RejectTailnetKey has the tailnet refuse key, or, rejected false, take it.
func (h *Host) RejectTailnetKey(key string, rejected bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rejectedKeys == nil {
		h.rejectedKeys = map[string]bool{}
	}
	h.rejectedKeys[key] = rejected
}

// SetTailnetKeyRetry makes the guests wait d, not DefaultTailnetKeyRetry,
// after a refused key before they ask again.
func (h *Host) SetTailnetKeyRetry(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tailnetKeyRetry = d
}

// NoTailscaleDisk has the host give its guests no tailscale disk: a join
// fails.
func (h *Host) NoTailscaleDisk() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noTailscaleDisk = true
}

// TailnetKeys are the auth keys the machine's guests were given, in order,
// over its root disk's life.
func (h *Host) TailnetKeys(machineUID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.tailnetRootLocked(machineUID).keys)
}

// TailnetBootsWithState counts the machine's boots that came up as the
// node its root disk holds, with no key.
func (h *Host) TailnetBootsWithState(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tailnetRootLocked(machineUID).bootsWithState
}

// TailnetTold is what the machine's guests were told while they ran, in
// order: "join HOSTNAME", "set HOSTNAME" or "leave".
func (h *Host) TailnetTold(machineUID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.tailnetRootLocked(machineUID).told)
}

// tailnetGuestLocked is the machine's running container guest. Callers
// hold h.mu.
func (h *Host) tailnetGuestLocked(machineUID string) (*tailnetGuest, error) {
	v, ok := h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return nil, errUnknownVM
	}
	if h.silent[machineUID] {
		return nil, fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	if h.oldTailnetGuests[machineUID] {
		return nil, fmt.Errorf("%w: its tailnet", driver.ErrGuestCannot)
	}
	g := h.tailnetGuests[machineUID]
	if g == nil {
		return nil, fmt.Errorf("machine %s's guest is no container's", machineUID)
	}
	return g, nil
}

// OldTailnetGuest has the machine's guest answer as a hangar-init from
// before tailnets: it cannot be told one.
func (h *Host) OldTailnetGuest(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.oldTailnetGuests == nil {
		h.oldTailnetGuests = map[string]bool{}
	}
	h.oldTailnetGuests[machineUID] = true
}

func (d hypervisor) SetBootTailscale(_ context.Context, machineUID string, ts *driver.TailscaleSpec) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	if !ok {
		return fmt.Errorf("machine %s has no VM whose boot disk to rewrite", machineUID)
	}
	if v.spec.Firmware || v.spec.FileShareServer != nil || v.spec.Converter != nil {
		return fmt.Errorf("machine %s's VM is no container's", machineUID)
	}
	if ts != nil {
		c := *ts
		ts = &c
	}
	v.spec.Tailscale = ts
	return nil
}

func (d guest) SetTailnet(_ context.Context, machineUID string, ts *driver.TailscaleSpec) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	g, err := d.h.tailnetGuestLocked(machineUID)
	if err != nil {
		return err
	}
	r := d.h.tailnetRootLocked(machineUID)
	switch {
	case ts == nil && g.ts == nil:
	case ts == nil:
		// Logged out, stopped, its state deleted.
		r.told = append(r.told, "leave")
		r.joined = false
		g.ts, g.keyFailed = nil, time.Time{}
		g.status = driver.TailnetStatus{State: driver.TailnetOff}
	case g.ts == nil:
		r.told = append(r.told, "join "+ts.Hostname)
		d.h.joinLocked(machineUID, g, ts, false)
	case *g.ts != *ts:
		r.told = append(r.told, "set "+ts.Hostname)
		c := *ts
		g.ts = &c
		if g.status.State == driver.TailnetJoined {
			g.status.Message = "joined as " + c.Hostname
		}
	}
	return nil
}

func (d guest) TailnetStatus(_ context.Context, machineUID string) (driver.TailnetStatus, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	g, err := d.h.tailnetGuestLocked(machineUID)
	if err != nil {
		return driver.TailnetStatus{}, err
	}
	st := g.status
	retry := d.h.tailnetKeyRetry
	if retry == 0 {
		retry = DefaultTailnetKeyRetry
	}
	switch st.State {
	case driver.TailnetNeedsKey:
		st.WantsKey = true
	case driver.TailnetKeyRejected:
		st.WantsKey = time.Since(g.keyFailed) >= retry
	}
	return st, nil
}

func (d guest) JoinTailnet(_ context.Context, machineUID, authKey string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	g, err := d.h.tailnetGuestLocked(machineUID)
	if err != nil {
		return err
	}
	r := d.h.tailnetRootLocked(machineUID)
	switch {
	case r.joined:
		return driver.ErrTailnetHasState
	case g.ts == nil, g.status.State != driver.TailnetNeedsKey && g.status.State != driver.TailnetKeyRejected && g.status.State != driver.TailnetFailed:
		return driver.ErrTailnetBusy
	}
	r.keys = append(r.keys, authKey)
	if d.h.rejectedKeys[authKey] {
		g.keyFailed = time.Now()
		g.status = driver.TailnetStatus{State: driver.TailnetKeyRejected, Message: "the tailnet refused the auth key: invalid key"}
		return nil
	}
	r.joined = true
	g.keyFailed = time.Time{}
	g.status = driver.TailnetStatus{State: driver.TailnetJoined, Message: "joined as " + g.ts.Hostname}
	return nil
}
