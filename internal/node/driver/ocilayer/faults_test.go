package ocilayer_test

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
)

// failing reads its bytes, then fails with err.
type failing struct {
	r   io.Reader
	err error
}

func (f *failing) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, f.err
	}
	return n, err
}

// TestFaultsAreTold: a layer the host fails to read, as EIO from a disk
// with a checksum error, may be read whole next time; a layer whose tar or
// compression is broken never will.
func TestFaultsAreTold(t *testing.T) {
	random := make([]byte, 64<<10)
	_, _ = rand.Read(random)
	plain := layer(t, file("a", "x", 0o644), file("big", string(random), 0o644), file("z", "x", 0o644))
	zipped := gz(t, plain)
	eio := &os.PathError{Op: "read", Path: "blob", Err: syscall.EIO}
	corrupt := bytes.Clone(zipped)
	// The first deflate block's header: gzip checks its CRC only at the
	// end, which a layer's tar ends before.
	corrupt[10] ^= 0xff
	for name, tt := range map[string]struct {
		r       func() io.Reader
		invalid bool
	}{
		// Cut within the big file's contents, and between entries.
		"EIO within a file":      {func() io.Reader { return &failing{bytes.NewReader(plain[:20000]), eio} }, false},
		"EIO between entries":    {func() io.Reader { return &failing{bytes.NewReader(plain[:512]), eio} }, false},
		"a bare errno":           {func() io.Reader { return &failing{bytes.NewReader(plain[:20000]), syscall.EIO} }, false},
		"EIO under gzip":         {func() io.Reader { return gunzip(t, &failing{bytes.NewReader(zipped[:20000]), eio}) }, false},
		"truncated within":       {func() io.Reader { return bytes.NewReader(plain[:20000]) }, true},
		"corrupt gzip":           {func() io.Reader { return gunzip(t, bytes.NewReader(corrupt)) }, true},
		"a header of nonsense":   {func() io.Reader { return bytes.NewReader(bytes.Repeat([]byte{'7'}, 1024)) }, true},
		"truncated, under gzip":  {func() io.Reader { return gunzip(t, bytes.NewReader(zipped[:20000])) }, true},
		"no error, the baseline": {func() io.Reader { return bytes.NewReader(plain) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			a, err := ocilayer.Open(s.root, unlimited)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = a.Close() }()
			err = a.Apply(t.Context(), tt.r())
			if errors.Is(err, driver.ErrInvalidImage) != tt.invalid {
				t.Errorf("%v, want invalid %t", err, tt.invalid)
			}
			if !tt.invalid && strings.HasPrefix(name, "no error") != (err == nil) {
				t.Errorf("%v", err)
			}
		})
	}
}

func gunzip(t *testing.T, r io.Reader) io.Reader {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

// TestXattrsAreWork: each xattr kept costs work, as setting it does, so an
// entry carrying thousands of tiny ones costs as much as thousands of
// entries; the bytes of a large one cost work too.
func TestXattrsAreWork(t *testing.T) {
	// work applies 100 files with n xattrs of size bytes, and an xattr
	// dropped, not kept, of pad bytes.
	work := func(n, size, pad int) int64 {
		s := newSandbox(t)
		var entries []entry
		for i := range 100 {
			e := file(fmt.Sprintf("f%d", i), "", 0o644)
			e.hdr.PAXRecords = map[string]string{"SCHILY.xattr.trusted.pad": strings.Repeat("x", pad)}
			for j := range n {
				e.hdr.PAXRecords[fmt.Sprintf("SCHILY.xattr.user.%d", j)] = strings.Repeat("x", size)
			}
			entries = append(entries, e)
		}
		a, err := ocilayer.Open(s.root, unlimited)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = a.Close() }()
		if err := a.Apply(t.Context(), bytes.NewReader(layer(t, entries...))); err != nil {
			t.Fatal(err)
		}
		return a.Work()
	}
	// As many, and as large, as an ext4 inode under the sandbox takes.
	none, few, many := work(0, 0, 1), work(2, 1, 1), work(40, 1, 1)
	// The same PAX records, the bytes in an xattr kept or dropped.
	one, large := work(1, 1, 3000), work(1, 3000, 1)
	t.Logf("100 files: %d steps; with 2 tiny xattrs %d, 40 tiny %d; one tiny %d, one of 3000 bytes %d", none, few, many, one, large)
	if many-none < 100*40 {
		t.Errorf("4000 xattrs took %d steps more than none", many-none)
	}
	if many-none < 15*(few-none) {
		t.Errorf("twenty times the xattrs took %d steps more, two tiny %d", many-none, few-none)
	}
	if large <= one {
		t.Errorf("an xattr of 3000 bytes took %d steps, a tiny one %d", large, one)
	}
}

// TestPAXRecordsAreWork: the PAX records of an entry, which the tar reader
// parses, cost work by their bytes, kept or not.
func TestPAXRecordsAreWork(t *testing.T) {
	work := func(size int) int64 {
		s := newSandbox(t)
		var entries []entry
		for i := range 100 {
			e := file(fmt.Sprintf(".wh.%d", i), "", 0o644)
			e.hdr.PAXRecords = map[string]string{"comment": strings.Repeat("x", size)}
			entries = append(entries, e)
		}
		a, err := ocilayer.Open(s.root, unlimited)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = a.Close() }()
		if err := a.Apply(t.Context(), bytes.NewReader(layer(t, entries...))); err != nil {
			t.Fatal(err)
		}
		return a.Work()
	}
	small, big := work(1), work(1<<20-64)
	t.Logf("100 whiteouts with a 1-byte comment: %d steps; with a 1 MiB one %d", small, big)
	if big < 2*small {
		t.Errorf("1 MiB of PAX records an entry took %d steps, 1 byte %d", big, small)
	}
}

// TestXattrsAreBounded: the xattrs kept over all of an image's layers are
// bounded, exactly; those not kept do not count.
func TestXattrsAreBounded(t *testing.T) {
	withXattrs := func(name string, n int) entry {
		e := file(name, "", 0o644)
		e.hdr.PAXRecords = map[string]string{"SCHILY.xattr.trusted.x": "dropped"}
		for j := range n {
			e.hdr.PAXRecords[fmt.Sprintf("SCHILY.xattr.user.%d", j)] = "x"
		}
		return e
	}
	lim := ocilayer.Limits{MaxBytes: 1 << 30, MaxEntries: 1 << 20, MaxXattrs: 10}
	if err := apply(t, newSandbox(t).root, lim, layer(t, withXattrs("a", 4)), layer(t, withXattrs("b", 6))); err != nil {
		t.Errorf("10 xattrs with a bound of 10: %v", err)
	}
	err := apply(t, newSandbox(t).root, lim, layer(t, withXattrs("a", 4)), layer(t, withXattrs("b", 7)))
	if !errors.Is(err, driver.ErrImageTooLarge) {
		t.Errorf("11 xattrs with a bound of 10: %v", err)
	}
}
