package chvm

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// execCreds are what ExecMain runs a program as.
type execCreds struct {
	uid, gid int
	groups   []int
	// fds is how many fds past stderr the program gets: 3 to 3+fds-1.
	fds int
}

// ExecMain is hangar-node vmm-exec --uid UID --gid GID [--groups G,...]
// --fds N -- PROGRAM ARGS...: it execs PROGRAM as UID and GID, with no
// capability, no environment, and no fd but 0 to 2+N, or returns failing.
func ExecMain(args []string) int {
	c, argv, err := parseExec(args)
	if err == nil {
		// The bounding set and no_new_privs are a thread's: this one's,
		// which execs.
		runtime.LockOSThread()
		err = confine(c)
	}
	if err == nil {
		err = unix.Exec(argv[0], argv, []string{})
	}
	fmt.Fprintf(os.Stderr, "vmm-exec: %v\n", err)
	return ExitFailed
}

func parseExec(args []string) (execCreds, []string, error) {
	var c execCreds
	var groups string
	fs := flag.NewFlagSet("vmm-exec", flag.ContinueOnError)
	fs.IntVar(&c.uid, "uid", -1, "the uid to run as")
	fs.IntVar(&c.gid, "gid", -1, "the gid to run as")
	fs.StringVar(&groups, "groups", "", "the supplementary groups, comma-separated")
	fs.IntVar(&c.fds, "fds", -1, "how many fds past stderr to pass on")
	if err := fs.Parse(args); err != nil {
		return c, nil, err
	}
	if c.uid <= 0 || c.gid <= 0 || c.fds < 0 || fs.NArg() == 0 {
		return c, nil, errors.New("usage: hangar-node vmm-exec --uid UID --gid GID [--groups G,...] --fds N -- PROGRAM [ARG]")
	}
	for _, g := range strings.Split(groups, ",") {
		if g == "" {
			continue
		}
		n, err := strconv.Atoi(g)
		if err != nil || n <= 0 {
			return c, nil, fmt.Errorf("group %q", g)
		}
		c.groups = append(c.groups, n)
	}
	return c, fs.Args(), nil
}

// confine makes the calling thread what c says, for good, and checks it.
func confine(c execCreds) error {
	ppid := os.Getppid()
	for fd := 3; fd < 3+c.fds; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			return fmt.Errorf("fd %d is not open: %w", fd, err)
		}
	}
	// Whatever else is open, the program does not get.
	if err := unix.CloseRange(uint(3+c.fds), ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return fmt.Errorf("close_range: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	// Dropping from the bounding set needs CAP_SETPCAP, so before the uid.
	for capability := 0; ; capability++ {
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0)
		if errors.Is(err, unix.EINVAL) {
			break // past the last capability the kernel has
		}
		if err != nil {
			return fmt.Errorf("dropping capability %d from the bounding set: %w", capability, err)
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clearing the ambient capabilities: %w", err)
	}
	if err := syscall.Setgroups(c.groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(c.gid, c.gid, c.gid); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	// Every uid non-zero: the permitted and effective sets are cleared.
	if err := syscall.Setresuid(c.uid, c.uid, c.uid); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var none [2]unix.CapUserData
	if err := unix.Capset(&hdr, &none[0]); err != nil {
		return fmt.Errorf("clearing the capabilities: %w", err)
	}
	// Changing the uid cleared the parent-death signal.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return fmt.Errorf("parent-death signal: %w", err)
	}
	if os.Getppid() != ppid {
		return errors.New("the launcher is gone")
	}
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return err
	}
	return checkConfined(status, c)
}

// checkConfined checks a /proc status says c, no capability of any set,
// and no_new_privs.
func checkConfined(status []byte, c execCreds) error {
	want := map[string]string{
		"CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000",
		"CapBnd": "0000000000000000", "CapAmb": "0000000000000000", "NoNewPrivs": "1",
		"Uid": strings.Repeat(strconv.Itoa(c.uid)+" ", 4), "Gid": strings.Repeat(strconv.Itoa(c.gid)+" ", 4),
	}
	var groups []string
	for _, g := range c.groups {
		groups = append(groups, strconv.Itoa(g))
	}
	want["Groups"] = strings.Join(groups, " ")
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		w, checked := want[key]
		if !ok || !checked {
			continue
		}
		got := strings.Join(strings.Fields(value), " ")
		if got != strings.TrimSpace(w) {
			return fmt.Errorf("%s is %q, not %q", key, got, strings.TrimSpace(w))
		}
		delete(want, key)
	}
	if len(want) > 0 {
		return fmt.Errorf("no %d of the lines checked in the status", len(want))
	}
	return sc.Err()
}
