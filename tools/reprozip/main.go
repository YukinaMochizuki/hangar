// Command reprozip packs files into a reproducible zip archive, the format
// the Windows CLI is released in. Entries keep the order they are named in
// and carry one fixed time and mode, so the same files give the same bytes
// on any host with the same Go toolchain. Only regular files at the top of
// the archive are accepted.
//
// Used by deploy/release.sh.
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// epoch is the time every entry carries: a zip's own time field cannot go
// back before 1980.
var epoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

func main() {
	dir := flag.String("C", ".", "directory the files are named relative to")
	out := flag.String("o", "", "output archive")
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: reprozip -o FILE [-C DIR] NAME...")
		os.Exit(2)
	}
	if err := pack(*out, *dir, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "reprozip:", err)
		os.Exit(1)
	}
}

// pack writes names, files of dir, to the zip out, in that order.
func pack(out, dir string, names []string) (err error) {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(out)
		}
	}()
	zw := zip.NewWriter(f)
	for _, name := range names {
		if err := add(zw, dir, name); err != nil {
			return err
		}
	}
	return zw.Close()
}

func add(zw *zip.Writer, dir, name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("%q is not a plain file name", name)
	}
	path := filepath.Join(dir, name)
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: epoch}
	h.SetMode(0o755)
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}
