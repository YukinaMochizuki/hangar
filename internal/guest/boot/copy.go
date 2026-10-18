package boot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// copyTree fills dst, an existing directory, with the tree at src as Docker
// fills a new volume, dst itself taking src's owner, mode and times. Nothing
// is followed; sockets are left out (docs/decisions/container-volumes.md).
func copyTree(src, dst string) error {
	links := map[[2]uint64]string{}
	var dirs []string // to stamp once their contents are in, innermost first
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if rel != "." {
				if err := unix.Mkdir(to, 0o700); err != nil && (!errors.Is(err, unix.EEXIST) || rel != "lost+found") {
					return err
				}
			}
			dirs = append(dirs, rel)
			return nil
		case unix.S_IFREG:
			if st.Nlink > 1 {
				id := [2]uint64{st.Dev, st.Ino}
				if first, ok := links[id]; ok {
					return unix.Link(first, to)
				}
				links[id] = to
			}
			if err := copyFile(path, to); err != nil {
				return err
			}
		case unix.S_IFLNK:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := unix.Symlink(target, to); err != nil {
				return err
			}
		case unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO:
			if err := unix.Mknod(to, st.Mode, int(st.Rdev)); err != nil {
				return err
			}
		default:
			return nil // a socket means nothing once its process is gone
		}
		return stamp(path, to, &st)
	})
	if err != nil {
		return err
	}
	slices.Reverse(dirs)
	for _, rel := range dirs {
		var st unix.Stat_t
		from := filepath.Join(src, rel)
		if err := unix.Lstat(from, &st); err != nil {
			return err
		}
		if err := stamp(from, filepath.Join(dst, rel), &st); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies a regular file's contents to a new file, its holes left
// holes: a sparse file such as lastlog takes no more room in the volume
// than in the image.
func copyFile(from, to string) error {
	in, err := os.OpenFile(from, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := copyData(out, in, fi.Size()); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyData copies in's data extents to out, which it sizes to size.
func copyData(out, in *os.File, size int64) error {
	if err := out.Truncate(size); err != nil {
		return err
	}
	fd := int(in.Fd())
	for off := int64(0); off < size; {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		switch {
		case errors.Is(err, unix.ENXIO):
			return nil // only a hole is left
		case errors.Is(err, unix.EINVAL):
			// A filesystem that cannot say where its holes are: all data.
			data = off
		case err != nil:
			return err
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			hole = size
		}
		if _, err := io.Copy(io.NewOffsetWriter(out, data), io.NewSectionReader(in, data, hole-data)); err != nil {
			return err
		}
		off = hole
	}
	return nil
}

// stamp gives to the owner, mode, xattrs and times of from, as st has
// them. The owner comes first, since changing it clears setuid bits; a
// symlink has no mode of its own.
func stamp(from, to string, st *unix.Stat_t) error {
	if err := unix.Lchown(to, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("owning %s: %w", to, err)
	}
	if err := copyXattrs(from, to); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		if err := unix.Fchmodat(unix.AT_FDCWD, to, st.Mode&0o7777, 0); err != nil {
			return err
		}
	}
	times := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(unix.AT_FDCWD, to, times, unix.AT_SYMLINK_NOFOLLOW)
}

// copyXattrs copies every extended attribute of from that to can take.
func copyXattrs(from, to string) error {
	size, err := unix.Llistxattr(from, nil)
	switch {
	case errors.Is(err, unix.ENOTSUP):
		return nil
	case err != nil:
		return err
	case size == 0:
		return nil
	}
	list := make([]byte, size)
	if size, err = unix.Llistxattr(from, list); err != nil {
		return err
	}
	for _, name := range splitNull(list[:size]) {
		n, err := unix.Lgetxattr(from, name, nil)
		if err != nil {
			return err
		}
		value := make([]byte, n)
		if n, err = unix.Lgetxattr(from, name, value); err != nil {
			return err
		}
		// One the volume's filesystem has no namespace for is left out.
		if err := unix.Lsetxattr(to, name, value[:n], 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
			return fmt.Errorf("xattr %s of %s: %w", name, to, err)
		}
	}
	return nil
}

func splitNull(b []byte) []string {
	var out []string
	for len(b) > 0 {
		i := slices.Index(b, 0)
		if i < 0 {
			i = len(b)
		}
		if i > 0 {
			out = append(out, string(b[:i]))
		}
		b = b[min(i+1, len(b)):]
	}
	return out
}
