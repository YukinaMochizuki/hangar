//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// On Unix a file is private when its mode gives its group and others
// nothing: 0600 for a file, 0700 for a directory.

// userConfigDir is where the CLI's config directory goes:
// $XDG_CONFIG_HOME, else ~/.config.
func userConfigDir(env func(string) string) string {
	if dir := env("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(env("HOME"), ".config")
}

// mkdirPrivate makes dir, and any parent missing, only its owner may enter.
func mkdirPrivate(dir string) error { return os.MkdirAll(dir, 0o700) }

// createPrivate opens path for writing, creating it only its owner may
// read; excl refuses a file that exists, else one that does is truncated
// and keeps its mode, as os.WriteFile's does.
func createPrivate(path string, excl bool) (*os.File, error) {
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if excl {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	return os.OpenFile(path, flag, 0o600)
}

// exposure says who besides its owner may read or change the open file f,
// "" when no one may, and the command that makes it private.
func exposure(f *os.File) (who, fix string, err error) {
	fi, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return "", "", nil
	}
	return fmt.Sprintf("mode %04o", fi.Mode().Perm()), "chmod 600 " + f.Name(), nil
}

// privateConfigDir makes dir, the config's, private where it may: own is
// whether it is the CLI's own directory, which is made 0700 if its owner
// runs this. Otherwise it is left as it is, and seen says who may list it,
// "" when no one but its owner may.
func privateConfigDir(dir string, own bool) (seen string, err error) {
	if err := mkdirPrivate(dir); err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return "", nil
	}
	if own {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
			return "", os.Chmod(dir, 0o700)
		}
	}
	if fi.Mode().Perm()&0o044 != 0 {
		return fmt.Sprintf("is mode %v", fi.Mode().Perm()), nil
	}
	return "", nil
}
