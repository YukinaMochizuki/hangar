// Command newc packs a directory tree into a reproducible newc cpio archive,
// the format the Linux kernel unpacks as an initramfs. Entries are sorted by
// path and carry uid/gid 0, one fixed mtime and synthetic inode numbers, so
// the same tree gives the same bytes on any host. Only directories, regular
// files and symlinks are accepted; device nodes are refused rather than packed.
//
// Used by guest/initramfs/build.sh.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	root := flag.String("root", "", "directory to pack (its contents become /)")
	out := flag.String("o", "", "output archive")
	mtime := flag.Int64("mtime", 0, "mtime for every entry, in seconds since the epoch")
	flag.Parse()
	if *root == "" || *out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: newc -root DIR -o FILE [-mtime SECONDS]")
		os.Exit(2)
	}
	if err := packFile(*root, *out, *mtime); err != nil {
		fmt.Fprintln(os.Stderr, "newc:", err)
		os.Exit(1)
	}
}

func packFile(root, out string, mtime int64) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := pack(f, root, mtime); err != nil {
		_ = f.Close()
		_ = os.Remove(out)
		return err
	}
	return f.Close()
}

// pack writes the archive of root's contents to w.
func pack(w io.Writer, root string, mtime int64) error {
	if mtime < 0 || mtime > 0xffffffff {
		return fmt.Errorf("mtime %d does not fit newc's 32-bit field", mtime)
	}
	var paths []string
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != root {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(paths, func(i, j int) bool { return rel(root, paths[i]) < rel(root, paths[j]) })

	bw := bufio.NewWriter(w)
	cw := &countingWriter{w: bw}
	for i, p := range paths {
		if err := writeEntry(cw, root, p, uint32(i+1), uint32(mtime)); err != nil {
			return err
		}
	}
	if err := writeHeader(cw, "TRAILER!!!", header{nlink: 1}); err != nil {
		return err
	}
	return bw.Flush()
}

func rel(root, p string) string {
	r, _ := filepath.Rel(root, p)
	return filepath.ToSlash(r)
}

// Mode type bits as cpio stores them (the same values as st_mode).
const (
	modeDir     = 0o040000
	modeRegular = 0o100000
	modeSymlink = 0o120000
)

type header struct {
	ino, mode, nlink, size, mtime uint32
}

// permBits returns the permission, setuid, setgid and sticky bits in st_mode
// layout; Go keeps the last three outside fs.ModePerm.
func permBits(m fs.FileMode) uint32 {
	b := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		b |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		b |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		b |= 0o1000
	}
	return b
}

func writeEntry(cw *countingWriter, root, p string, ino, mtime uint32) error {
	name := rel(root, p)
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	h := header{ino: ino, mode: permBits(fi.Mode()), nlink: 1, mtime: mtime}
	var data []byte
	switch {
	case fi.IsDir():
		h.mode |= modeDir
		h.nlink = 2
	case fi.Mode().IsRegular():
		h.mode |= modeRegular
		if data, err = os.ReadFile(p); err != nil {
			return err
		}
	case fi.Mode()&fs.ModeSymlink != 0:
		h.mode |= modeSymlink
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		data = []byte(target)
	default:
		return fmt.Errorf("%s: unsupported file type %s", name, fi.Mode().Type())
	}
	if uint64(len(data)) > 0xffffffff {
		return fmt.Errorf("%s: too large for newc", name)
	}
	h.size = uint32(len(data))
	if err := writeHeader(cw, name, h); err != nil {
		return err
	}
	if _, err := cw.Write(data); err != nil {
		return err
	}
	return cw.pad()
}

func writeHeader(cw *countingWriter, name string, h header) error {
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%q: name contains NUL", name)
	}
	// Fields: magic, ino, mode, uid, gid, nlink, mtime, filesize, devmajor,
	// devminor, rdevmajor, rdevminor, namesize (with the NUL), check.
	_, err := fmt.Fprintf(cw, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%s\x00",
		h.ino, h.mode, 0, 0, h.nlink, h.mtime, h.size, 0, 0, 0, 0, len(name)+1, 0, name)
	if err != nil {
		return err
	}
	return cw.pad()
}

// countingWriter tracks the offset so header and data can be padded to the
// four-byte boundaries newc requires.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) pad() error {
	if r := c.n % 4; r != 0 {
		_, err := c.Write(make([]byte, 4-r))
		return err
	}
	return nil
}
