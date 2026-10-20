// Package fileshare is a FileShare's server: what hangar-init does in place
// of a container when its boot disk makes the guest a share's server
// (docs/decisions/file-shares.md). It serves the filesystem on the guest's
// data disk over the kernel's nfsd, NFSv4.2 over TCP alone, and is the
// userspace nfsd needs, in Go: the answers to its export upcalls and its
// client tracking. It is not the node's side, which computes the export and
// sends it through the agent.
package fileshare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/boot"
)

// Device is the share's disk, the first after the boot disk.
const Device = "/dev/vdc"

// Where the server mounts what it needs.
const (
	// Root is where the share is mounted, exported as the pseudo-root.
	Root      = "/share"
	nfsdDir   = "/proc/fs/nfsd"
	rpcDir    = "/proc/net/rpc"
	pipefsDir = "/var/lib/nfs/rpc_pipefs"
)

// settings are written to nfsd's control files, in this order, before it
// starts, and fixed once it runs.
var settings = [][2]string{
	{"versions", "-3 +4 -4.0 -4.1 +4.2"},
	// The wire block size, otherwise a 4096th of the guest's memory, which
	// caps a small server's I/O and hangs clients of a server that comes
	// back smaller. Before portlist, which fixes it.
	{"max_block_size", strconv.Itoa(1 << 20)},
	// Seconds: a restarted server's clients pause for seconds, not the
	// default minute and a half.
	{"nfsv4leasetime", "15"},
	{"nfsv4gracetime", "15"},
	{"portlist", "tcp 2049"},
	{"threads", "8"},
}

// Server serves one share. Its upcalls are answered from the export
// SetExport last gave; nfsd starts with the first.
type Server struct {
	logf func(string, ...any)
	// ctl writes one of nfsd's control files; flush empties one cache.
	ctl   func(name, value string) error
	flush func(cache string) error
	// uptime is the guest's boot clock; sleep waits.
	uptime func() time.Duration
	sleep  func(time.Duration)
	// reply writes an upcall's answer to its channel.
	reply func(fd int, answer []byte) error

	// mu covers the export across answering an upcall and across replacing
	// the export and flushing, so no answer from the export replaced lands
	// in a cache after its flush.
	mu     sync.Locker
	export *export

	startMu sync.Mutex
	started bool
	stopped bool

	tracker *tracker
}

var _ guestv1connect.FileShareServerServiceHandler = (*Server)(nil)

// Boot mounts the share from dev, made when the disk is blank, and readies
// nfsd to serve it: its filesystems are mounted and its upcalls answered.
// nfsd itself starts with the first export.
func Boot(dev string, logf func(string, ...any)) (*Server, error) {
	// ea_inode lets the client records outgrow the one block an inode's
	// attributes otherwise share.
	if _, err := boot.MountData(dev, Root, unix.MS_NOSUID|unix.MS_NODEV, "the share", logf, "-O", "ea_inode"); err != nil {
		return nil, err
	}
	for _, m := range [][3]string{{"rpc_pipefs", pipefsDir, "rpc_pipefs"}, {"nfsd", nfsdDir, "nfsd"}} {
		if err := os.MkdirAll(m[1], 0o755); err != nil {
			return nil, err
		}
		if err := unix.Mount(m[0], m[1], m[2], unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return nil, fmt.Errorf("mounting %s: %w", m[2], err)
		}
	}
	t := newTracker(xattrStore{Root}, logf)
	s := &Server{logf: logf, ctl: writeCtl, flush: flushCache, uptime: uptime, sleep: time.Sleep, reply: writeReply, mu: &sync.Mutex{}, tracker: t}
	chans := map[int]string{}
	for _, c := range upcallCaches {
		fd, err := unix.Open(filepath.Join(rpcDir, c, "channel"), unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("opening nfsd's %s upcalls: %w", c, err)
		}
		chans[fd] = c
	}
	go s.serveUpcalls(chans)
	return s, nil
}

// serveUpcalls answers the upcalls of the channels, fds to cache names,
// for as long as the guest runs. A read takes one whole request, and a
// write must be one whole answer.
func (s *Server) serveUpcalls(chans map[int]string) {
	var fds []unix.PollFd
	for fd := range chans {
		fds = append(fds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
	}
	buf := make([]byte, 1<<16)
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if !errors.Is(err, unix.EINTR) {
				s.logf("hangar-init: waiting for nfsd's upcalls: %v", err)
				time.Sleep(time.Second)
			}
			continue
		}
		for i := range fds {
			if fds[i].Revents&unix.POLLIN == 0 {
				continue
			}
			fd := int(fds[i].Fd)
			n, err := unix.Read(fd, buf)
			if err != nil || n == 0 {
				continue
			}
			s.answer(fd, chans[fd], string(buf[:n]))
		}
	}
}

func (s *Server) answer(fd int, cache, req string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply, ok := answer(cache, req, s.export, Root, time.Now())
	if !ok {
		s.logf("hangar-init: nfsd asked %s what cannot be read: %q", cache, req)
		return
	}
	if err := s.reply(fd, []byte(reply)); err != nil {
		s.logf("hangar-init: answering nfsd's %s upcall: %v", cache, err)
	}
}

func writeReply(fd int, answer []byte) error {
	_, err := unix.Write(fd, answer)
	return err
}

// SetExport replaces the export; the first export starts nfsd.
func (s *Server) SetExport(_ context.Context, req *connect.Request[guestv1.SetExportRequest]) (*connect.Response[guestv1.SetExportResponse], error) {
	e, err := parseExport(req.Msg.GetExport())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := s.apply(e); errors.Is(err, errStopped) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	} else if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&guestv1.SetExportResponse{}), nil
}

// Export takes the export the boot disk gives, as SetExport would.
func (s *Server) Export(e *guestv1.FileShareExport) error {
	pe, err := parseExport(e)
	if err != nil {
		return err
	}
	return s.apply(pe)
}

func (s *Server) apply(e *export) error {
	s.mu.Lock()
	s.export = e
	// Each client's next request is judged by this export, a client left
	// out losing the share, mounted or not.
	var err error
	for _, c := range upcallCaches {
		if ferr := s.flush(c); ferr != nil {
			err = errors.Join(err, fmt.Errorf("flushing nfsd's %s: %w", c, ferr))
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.logf("hangar-init: the export has %d clients", len(e.readOnly))
	return s.start()
}

// errStopped refuses to start nfsd on a server that is stopping.
var errStopped = errors.New("the server is stopping")

// start starts nfsd once, with its client tracking answered, unless the
// server was stopped first.
func (s *Server) start() error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.stopped {
		return errStopped
	}
	if s.started {
		return nil
	}
	// nfsd stamps a client with the boot clock's seconds, and takes 0 for a
	// client it has expired: one first seen in the guest's first second
	// would be refused for good (re-checked per kernel bump,
	// docs/runbook/guest-kernel.md).
	if d := firstSecond - s.uptime(); d > 0 {
		s.sleep(d)
	}
	if s.tracker != nil {
		// nfsd gives its tracking daemon a second to open the pipe it
		// makes as it starts, and runs without tracking otherwise.
		go s.serveTracking(filepath.Join(pipefsDir, "nfsd/cld"), 30*time.Second)
	}
	for _, kv := range settings {
		if err := s.ctl(kv[0], kv[1]); err != nil {
			return fmt.Errorf("starting nfsd: %s: %w", kv[0], err)
		}
	}
	s.started = true
	s.logf("hangar-init: serving NFSv4.2 on port 2049")
	return nil
}

// Stop stops nfsd, which closes its clients' connections, so they wait for
// the server's return, and unmounts the share.
func (s *Server) Stop() error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.stopped = true
	var err error
	if s.started {
		err = s.ctl("threads", "0")
	}
	unix.Sync()
	if uerr := unix.Unmount(Root, 0); uerr != nil {
		err = errors.Join(err, fmt.Errorf("unmounting the share: %w", uerr))
	}
	return err
}

// serveTracking answers nfsd's client tracking on its pipe at path, once
// nfsd has made it, which must be within wait, until nfsd closes it.
func (s *Server) serveTracking(path string, wait time.Duration) {
	var fd int
	for deadline := time.Now().Add(wait); ; {
		var err error
		if fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0); err == nil {
			break
		}
		if time.Now().After(deadline) {
			s.logf("hangar-init: nfsd's client tracking: %v", err)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, 2*cldMsgLen)
	for {
		// A read never waits on rpc_pipefs: poll does, and says when nfsd
		// has closed the pipe.
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(pfd, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		if pfd[0].Revents&unix.POLLIN == 0 {
			return
		}
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil || n == 0 {
			return
		}
		for _, r := range s.tracker.handle(buf[:n]) {
			if _, err := unix.Write(fd, r); err != nil {
				s.logf("hangar-init: answering nfsd's client tracking: %v", err)
			}
		}
	}
}

// firstSecond is how long after boot nfsd may first start, with a margin
// for the clock's reading.
const firstSecond = 1100 * time.Millisecond

func uptime() time.Duration {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return time.Duration(ts.Nano())
}

func writeCtl(name, value string) error {
	return os.WriteFile(filepath.Join(nfsdDir, name), []byte(value+"\n"), 0)
}

// flushCache drops every entry of a cache: whatever was cached until now.
func flushCache(cache string) error {
	return os.WriteFile(filepath.Join(rpcDir, cache, "flush"), []byte(strconv.FormatInt(time.Now().Unix(), 10)+"\n"), 0)
}
