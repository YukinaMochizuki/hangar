package chvm

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

func (h *Hypervisor) MemoryEvents() <-chan struct{} { return h.memEvents }

// pressure watches cgroups' memory.events with inotify: the kernel marks
// such a file modified each time one of its counts moves. A watch goes by
// itself with its cgroup.
type pressure struct {
	once sync.Once
	// f reads the inotify instance fd, nil if it could not be made.
	f  *os.File
	fd int
}

// watchEvents has MemoryEvents signal when the memory.events file at path
// changes. Watching a file watched already is no change; a file that
// cannot be watched, as one gone, is passed by: the poll still comes.
func (h *Hypervisor) watchEvents(path string) {
	p := &h.pressure
	p.once.Do(func() {
		fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
		if err != nil {
			return
		}
		// Non-blocking, so the runtime's poller reads it and Close ends
		// the read; never through f.Fd, which makes it blocking again.
		p.f, p.fd = os.NewFile(uintptr(fd), "inotify"), fd
		go h.readEvents(p.f)
	})
	if p.f == nil {
		return
	}
	_, _ = unix.InotifyAddWatch(p.fd, path, unix.IN_MODIFY)
}

// readEvents signals MemoryEvents for each read of inotify's events, until
// f is closed.
func (h *Hypervisor) readEvents(f *os.File) {
	buf := make([]byte, 4096)
	for {
		if _, err := f.Read(buf); err != nil {
			return
		}
		select {
		case h.memEvents <- struct{}{}:
		default:
		}
	}
}

// closePressure stops the watch.
func (h *Hypervisor) closePressure() {
	if f := h.pressure.f; f != nil {
		_ = f.Close()
	}
}
