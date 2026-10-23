package chvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// converterPrefix starts the uid of each converter VM, the driver's own,
// which no machine accounts for: VMs and Logs leave them out.
const converterPrefix = "convert-"

func isConverter(uid string) bool { return strings.HasPrefix(uid, converterPrefix) }

// The converter VM's size, the platform's: qemu-img needs little, and a
// slow conversion only takes longer.
const (
	converterVCPUs     = 1
	converterMemoryMiB = 512
)

// DefaultConverterRuntime bounds one converter VM's run unless the
// Config says otherwise.
const DefaultConverterRuntime = 2 * time.Hour

// converterBoot bounds a converter VM's boot, until its agent answers.
const converterBoot = 2 * time.Minute

// Converter is the Converter driver: each call boots a converter VM of
// the image's own, the platform kernel with the converter initramfs,
// with no interface, asks its agent over vsock, and stops it, whatever
// the call came to (docs/decisions/custom-images.md).
func (h *Hypervisor) Converter() driver.Converter { return converter{h} }

type converter struct{ h *Hypervisor }

func (c converter) Inspect(ctx context.Context, imageUID, path string) (driver.ImageInfo, error) {
	var info driver.ImageInfo
	err := c.run(ctx, imageUID, path, "", func(ctx context.Context, cl guestv1connect.ImageConverterServiceClient) error {
		res, err := cl.Inspect(ctx, connect.NewRequest(&guestv1.InspectRequest{}))
		if err != nil {
			return err
		}
		m := res.Msg
		info = driver.ImageInfo{Format: m.GetFormat(), VirtualSize: m.GetVirtualSize(), BackingFile: m.GetBackingFile(), DataFile: m.GetDataFile()}
		return nil
	})
	return info, err
}

func (c converter) Convert(ctx context.Context, imageUID, path, format, dev string) error {
	return c.run(ctx, imageUID, path, dev, func(ctx context.Context, cl guestv1connect.ImageConverterServiceClient) error {
		_, err := cl.Convert(ctx, connect.NewRequest(&guestv1.ConvertRequest{Format: format}))
		return err
	})
}

// run boots the image's converter VM with the file, and the target if
// any, does what do asks of its agent, and stops it. A file qemu-img
// refused is driver.ErrInvalidImage.
func (c converter) run(ctx context.Context, imageUID, path, target string, do func(context.Context, guestv1connect.ImageConverterServiceClient) error) error {
	uid := converterPrefix + imageUID
	if err := checkUID(uid); err != nil {
		return err
	}
	// What an earlier run left, cut short by the agent stopping.
	if err := c.end(ctx, uid); err != nil {
		return err
	}
	if err := padToSector(path); err != nil {
		return err
	}
	if err := c.h.Start(ctx, driver.VMSpec{
		MachineUID: uid, Machine: "converter/" + imageUID, VCPUs: converterVCPUs, MemoryMiB: converterMemoryMiB,
		Restart: driver.RestartNever, Converter: &driver.ConverterSpec{File: path, Target: target},
	}); err != nil {
		return err
	}
	defer func() { _ = c.end(context.WithoutCancel(ctx), uid) }()
	if err := c.ready(ctx, uid); err != nil {
		return err
	}
	tr := guestTransport(c.h.Socket(uid))
	defer tr.CloseIdleConnections()
	cl := guestv1connect.NewImageConverterServiceClient(&http.Client{Transport: tr}, "http://guest", connect.WithReadMaxBytes(agent.MaxMessage))
	err := do(ctx, cl)
	if connect.CodeOf(err) == connect.CodeInvalidArgument {
		return fmt.Errorf("%w: %w", driver.ErrInvalidImage, err)
	}
	if err != nil {
		return fmt.Errorf("the converter VM: %w", err)
	}
	return nil
}

// ready waits until the converter VM's agent answers, failing once the
// VM is off or converterBoot has passed.
func (c converter) ready(ctx context.Context, uid string) error {
	ctx, cancel := context.WithTimeout(ctx, converterBoot)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		pctx, pcancel := context.WithTimeout(ctx, time.Second)
		err := c.h.guest.Ready(pctx, uid)
		pcancel()
		if err == nil {
			return nil
		}
		if c.h.poweredOff(ctx, uid) {
			return fmt.Errorf("the converter VM stopped before its agent answered: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the converter VM's agent did not answer: %w", err)
		case <-tick.C:
		}
	}
}

// end stops the converter VM of uid, if any, and removes all it left.
func (c converter) end(ctx context.Context, uid string) error {
	if err := c.h.Stop(ctx, uid); err != nil {
		return err
	}
	if err := os.RemoveAll(c.h.dir(uid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// clearConverters removes the converter VMs' directories whose units do
// not run: what a VM the agent lost track of left once its unit's bound
// ended it.
func (h *Hypervisor) clearConverters(ctx context.Context) error {
	entries, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		uid := e.Name()
		if !e.IsDir() || !isConverter(uid) || checkUID(uid) != nil {
			continue
		}
		unit, err := h.unit(ctx, uid)
		if err != nil {
			return err
		}
		if unit == nil || !running(*unit) {
			if err := (converter{h}).end(ctx, uid); err != nil {
				return err
			}
		}
	}
	return nil
}

// giveFile makes the file at path the VM group gid's to read, and root's
// alone to write, refusing anything but a regular file of root's.
func giveFile(path string, gid int) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("the converter's file: %w", err)
	}
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not a regular file of the driver's user's", path)
	}
	return setGroup(f, gid, readableMode(gid))
}

// padToSector pads the file at path with zeros to whole 512-byte sectors,
// which is what a disk holds: Cloud Hypervisor drops a partial last
// sector. Neither a raw image nor a qcow2 one changes by it.
func padToSector(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if r := fi.Size() % 512; r != 0 {
		return os.Truncate(path, fi.Size()+512-r)
	}
	return nil
}

func guestTransport(socket string) *http.Transport {
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return chguest.Dial(ctx, socket, agent.Port)
		},
		MaxResponseHeaderBytes: 64 << 10,
		IdleConnTimeout:        time.Minute,
	}
}
