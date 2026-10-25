package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm/serialmux"
)

// fakeConsole is a running firmware VM's interactive serial console, as
// chvm's helper serves it: the same multiplexer, whose guest is the test.
type fakeConsole struct {
	hub *serialmux.Hub
	// typed is what reached the guest.
	typed *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.b.Bytes())
}

// bootConsoleLocked gives a firmware VM booting a console of its own.
func (h *Host) bootConsoleLocked(machineUID string) {
	if c := h.consoles[machineUID]; c != nil {
		c.hub.Close()
	}
	c := &fakeConsole{hub: serialmux.New(io.Discard, slog.New(slog.DiscardHandler)), typed: &lockedBuffer{}}
	c.hub.Connected(c.typed, false)
	h.consoles[machineUID] = c
}

// endConsoleLocked lets the clients of the machine's console go, as its
// VM's end does.
func (h *Host) endConsoleLocked(machineUID string) {
	if c := h.consoles[machineUID]; c != nil {
		c.hub.Close()
		delete(h.consoles, machineUID)
	}
}

// AttachConsole serves a client on a pipe, as chvm's helper does on its
// socket.
func (d hypervisor) AttachConsole(_ context.Context, machineUID string) (net.Conn, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	c := d.h.consoles[machineUID]
	if !ok || v.State != driver.VMRunning || c == nil {
		return nil, driver.ErrNoConsole
	}
	client, served := net.Pipe()
	go c.hub.Serve(served)
	return client, nil
}

// ConsoleOutput has the machine's guest write p on its serial console.
func (h *Host) ConsoleOutput(machineUID string, p []byte) error {
	h.mu.Lock()
	c := h.consoles[machineUID]
	h.mu.Unlock()
	if c == nil {
		return fmt.Errorf("machine %s has no console", machineUID)
	}
	c.hub.Output(p)
	return nil
}

// ConsoleTyped is what reached the machine's guest on its serial console
// this boot.
func (h *Host) ConsoleTyped(machineUID string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.consoles[machineUID]; c != nil {
		return c.typed.bytes()
	}
	return nil
}

// StartedWithoutConsole makes the machine's VM one a node started before
// serial consoles were interactive: its console takes no client.
func (h *Host) StartedWithoutConsole(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.endConsoleLocked(machineUID)
}
