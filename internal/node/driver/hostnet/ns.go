package hostnet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// netnsDir is where ip netns, and so the driver, mounts named namespaces.
const netnsDir = "/run/netns"

// inNS runs f on a thread in namespace ns. The thread is never returned to
// the scheduler: it ends with the goroutine, so nothing else ever runs in a
// namespace it did not ask for.
func inNS(ns netns.NsHandle, f func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := netns.Set(ns); err != nil {
			errc <- fmt.Errorf("entering a network namespace: %w", err)
			return
		}
		errc <- f()
	}()
	return <-errc
}

// namespaces lists the uids of the networks that have a namespace.
func namespaces() ([]string, error) {
	entries, err := os.ReadDir(netnsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var uids []string
	for _, e := range entries {
		uid, ok := strings.CutPrefix(e.Name(), nsPrefix)
		if ok && uidPattern.MatchString(uid) {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// openNS opens a network's namespace; ok is false if it has none. A file
// left where nothing is mounted, as a crash while making one leaves, is
// none.
func openNS(uid string) (ns netns.NsHandle, ok bool, err error) {
	ns, err = netns.GetFromPath(filepath.Join(netnsDir, NamespaceOf(uid)))
	if errors.Is(err, os.ErrNotExist) {
		return netns.None(), false, nil
	}
	if err != nil {
		return netns.None(), false, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(ns), &fs); err != nil {
		_ = ns.Close()
		return netns.None(), false, err
	}
	if fs.Type != unix.NSFS_MAGIC {
		_ = ns.Close()
		return netns.None(), false, nil
	}
	return ns, true, nil
}

// ensureNS opens a network's namespace, making it if it has none.
func ensureNS(uid string) (netns.NsHandle, error) {
	if ns, ok, err := openNS(uid); ok || err != nil {
		return ns, err
	}
	path := filepath.Join(netnsDir, NamespaceOf(uid))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return netns.None(), err
	}
	errc := make(chan error, 1)
	go func() {
		// NewNamed leaves the thread in the new namespace; it ends with
		// the goroutine.
		runtime.LockOSThread()
		ns, err := netns.NewNamed(NamespaceOf(uid))
		if err == nil {
			_ = ns.Close()
		}
		errc <- err
	}()
	if err := <-errc; err != nil {
		return netns.None(), fmt.Errorf("making namespace %s: %w", NamespaceOf(uid), err)
	}
	ns, ok, err := openNS(uid)
	if err == nil && !ok {
		err = fmt.Errorf("namespace %s vanished as it was made", NamespaceOf(uid))
	}
	return ns, err
}

// deleteNS unmounts and removes a network's namespace file. The namespace
// itself goes once nothing holds it.
func deleteNS(uid string) error {
	path := filepath.Join(netnsDir, NamespaceOf(uid))
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unmounting %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// nsID identifies a namespace across handles, as its nsfs inode.
func nsID(ns netns.NsHandle) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(ns), &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}

// ensureSysctl sets a sysctl of the calling thread's namespace, writing only
// if it differs.
func ensureSysctl(key, value string) error {
	path := filepath.Join("/proc/sys", strings.ReplaceAll(key, ".", "/"))
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) == value {
		return nil
	}
	return os.WriteFile(path, []byte(value), 0)
}

// sysctl reads a sysctl of the calling thread's namespace.
func sysctl(key string) (string, error) {
	b, err := os.ReadFile(filepath.Join("/proc/sys", strings.ReplaceAll(key, ".", "/")))
	return strings.TrimSpace(string(b)), err
}
