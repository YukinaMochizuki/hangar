package supervise

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// ErrNotFound means a process's program is not on its PATH.
var ErrNotFound = errors.New("program not found")

// DefaultPath is the PATH of a process whose environment sets none, as in
// Docker.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// User is who a process runs as.
type User struct {
	UID, GID uint32
	Groups   []uint32
	Home     string
}

// Spec turns a Process into what Start takes: the program found on its
// PATH, its environment completed as Docker completes it (PATH, HOME), and
// its user looked up under root. stdio are its stdin, stdout and stderr.
func Spec(root string, p *guestv1.Process, stdio [3]*os.File) ([]string, *os.ProcAttr, error) {
	if len(p.GetArgs()) == 0 {
		return nil, nil, errors.New("the process has no program")
	}
	u, err := ResolveUser(root, p.GetUser())
	if err != nil {
		return nil, nil, err
	}
	env := slices.Clone(p.GetEnv())
	if lookup(env, "PATH") == "" {
		env = append(env, "PATH="+DefaultPath)
	}
	if lookup(env, "HOME") == "" {
		env = append(env, "HOME="+u.Home)
	}
	prog, err := LookPath(root, p.GetArgs()[0], lookup(env, "PATH"))
	if err != nil {
		return nil, nil, err
	}
	dir := workingDir(p)
	if fi, err := os.Stat(filepath.Join(root, dir)); err != nil || !fi.IsDir() {
		return nil, nil, fmt.Errorf("working directory %s: not a directory", dir)
	}
	argv := append([]string{prog}, p.GetArgs()[1:]...)
	attr := &os.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: stdio[:],
		Sys: &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: u.Groups},
			Setsid:     true,
		},
	}
	return argv, attr, nil
}

func workingDir(p *guestv1.Process) string {
	if p.GetWorkingDir() == "" {
		return "/"
	}
	return p.GetWorkingDir()
}

// MakeWorkingDir creates p's working directory under root if it is missing,
// as Docker does for a container's process (not for an exec).
func MakeWorkingDir(root string, p *guestv1.Process) error {
	return os.MkdirAll(filepath.Join(root, workingDir(p)), 0o755)
}

func lookup(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// LookPath finds name as the process would on path, among the files under
// root. A name with a slash is taken as it is.
func LookPath(root, name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(filepath.Join(root, candidate)); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: %s, on PATH %s", ErrNotFound, name, path)
}

// ResolveUser looks up an OCI image's user ("uid", "uid:gid", "name" or
// "name:group", empty for root) in root's /etc/passwd and /etc/group, as
// Docker does: a numeric uid with no entry runs with gid 0.
func ResolveUser(root, spec string) (User, error) {
	if spec == "" {
		spec = "0"
	}
	name, group, hasGroup := strings.Cut(spec, ":")
	passwd, _ := readEntries(filepath.Join(root, "etc/passwd"))
	groups, _ := readEntries(filepath.Join(root, "etc/group"))

	u := User{Home: "/"}
	var userName string
	if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
		u.UID = uint32(uid)
		for _, e := range passwd {
			if len(e) >= 6 && e[2] == name {
				userName = e[0]
				u.GID = parseID(e[3])
				u.Home = e[5]
				break
			}
		}
	} else {
		found := false
		for _, e := range passwd {
			if len(e) >= 6 && e[0] == name {
				userName, found = name, true
				u.UID, u.GID, u.Home = parseID(e[2]), parseID(e[3]), e[5]
				break
			}
		}
		if !found {
			return User{}, fmt.Errorf("user %q is not in /etc/passwd", name)
		}
	}
	if hasGroup {
		if gid, err := strconv.ParseUint(group, 10, 32); err == nil {
			u.GID = uint32(gid)
		} else {
			found := false
			for _, e := range groups {
				if len(e) >= 3 && e[0] == group {
					u.GID, found = parseID(e[2]), true
					break
				}
			}
			if !found {
				return User{}, fmt.Errorf("group %q is not in /etc/group", group)
			}
		}
	}
	// The groups the user is a member of, as login would give them.
	u.Groups = []uint32{u.GID}
	if userName != "" {
		for _, e := range groups {
			if len(e) >= 4 && slices.Contains(strings.Split(e[3], ","), userName) {
				if gid := parseID(e[2]); !slices.Contains(u.Groups, gid) {
					u.Groups = append(u.Groups, gid)
				}
			}
		}
	}
	return u, nil
}

func parseID(s string) uint32 {
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}

// readEntries reads a colon-separated database such as /etc/passwd.
func readEntries(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out [][]string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	return out, s.Err()
}
