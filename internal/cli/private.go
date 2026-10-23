package cli

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The files that hold secrets, and the directory they are in, are private:
// only the user may read them. What private means, and how a file is made
// so, is each platform's (private_unix.go, private_windows.go): mkdirPrivate,
// createPrivate, exposure and privateConfigDir.

// writePrivate writes b to the file at path, creating it private, as
// os.WriteFile does with mode 0600.
func writePrivate(path string, b []byte) error {
	f, err := createPrivate(path, false)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// createPrivateTemp creates a new file in dir, private from the start,
// named pattern with its last * replaced by a random string, as
// os.CreateTemp names one; os.CreateTemp's own is 0600 on Unix but takes
// dir's inherited DACL on Windows until it is changed.
func createPrivateTemp(dir, pattern string) (*os.File, error) {
	prefix, suffix := pattern, ""
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for range 10000 {
		f, err := createPrivate(filepath.Join(dir, prefix+rand.Text()+suffix), true)
		if !errors.Is(err, os.ErrExist) {
			return f, err
		}
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: os.ErrExist}
}
