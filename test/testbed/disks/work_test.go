//go:build testbed

package disks

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
)

// TestWorstCaseWork measures how long layers built to cost the most take,
// at the default limits, where images are unpacked on the host: each must
// end, too large or applied, within minutes. It takes minutes itself, so
// it runs only with HANGAR_TESTBED_WORST_CASE=1; docs/decisions/storage-zfs.md
// records what it measured.
func TestWorstCaseWork(t *testing.T) {
	if os.Getenv("HANGAR_TESTBED_WORST_CASE") != "1" {
		t.Skip("HANGAR_TESTBED_WORST_CASE is not 1")
	}
	prepare(t)
	deep := strings.Repeat("a/", 254)
	chain := strings.Repeat("s/", 38)
	// As many xattrs as an entry may carry: tiny ones, or one large one;
	// and PAX records the tar reader parses for nothing.
	tiny := map[string]string{}
	for i := 0; len(tiny)*len("SCHILY.xattr.user.0000x") < 64<<10-32; i++ {
		tiny[fmt.Sprintf("SCHILY.xattr.user.%04d", i)] = "x"
	}
	large := map[string]string{"SCHILY.xattr.user.a": strings.Repeat("x", 64<<10-32)}
	comment := map[string]string{"comment": strings.Repeat("x", 1<<20-64)}
	shapes := map[string]struct {
		lower []*tar.Header
		next  func(i int) *tar.Header
	}{
		"files, made with their metadata": {nil, func(i int) *tar.Header {
			return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("d%d/f%d", i/1000, i), Mode: 0o644}
		}},
		"xattrs, tiny and many": {nil, func(i int) *tar.Header {
			return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("d%d/f%d", i/1000, i), Mode: 0o644, PAXRecords: tiny, Format: tar.FormatPAX}
		}},
		"xattrs, one large": {nil, func(i int) *tar.Header {
			return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("d%d/f%d", i/1000, i), Mode: 0o644, PAXRecords: large, Format: tar.FormatPAX}
		}},
		"PAX records, unused": {nil, func(i int) *tar.Header {
			return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf(".wh.x%d", i), Mode: 0o644, PAXRecords: comment, Format: tar.FormatPAX}
		}},
		"whiteouts of nothing": {nil, func(i int) *tar.Header {
			return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf(".wh.x%d", i), Mode: 0o644}
		}},
		"deep walks, each after a removal": {
			[]*tar.Header{{Typeflag: tar.TypeDir, Name: deep, Mode: 0o755}, {Typeflag: tar.TypeReg, Name: "x", Mode: 0o644}},
			func(i int) *tar.Header {
				if i%2 == 0 {
					return &tar.Header{Typeflag: tar.TypeReg, Name: "x", Mode: 0o644}
				}
				return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("%s.wh.y%d", deep, i), Mode: 0o644}
			},
		},
		"symlink chains, each after a removal": {
			[]*tar.Header{
				{Typeflag: tar.TypeDir, Name: deep, Mode: 0o755}, {Typeflag: tar.TypeSymlink, Name: "s", Linkname: "/" + deep},
				{Typeflag: tar.TypeSymlink, Name: deep + "s", Linkname: "/" + deep}, {Typeflag: tar.TypeReg, Name: "x", Mode: 0o644},
			},
			func(i int) *tar.Header {
				if i%2 == 0 {
					return &tar.Header{Typeflag: tar.TypeReg, Name: "x", Mode: 0o644}
				}
				return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("%s.wh.y%d", chain, i), Mode: 0o644}
			},
		},
		"trees made and removed": {nil, func(i int) *tar.Header {
			if i%2 == 0 {
				return &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("t/%d/%s", i%100, strings.Repeat("b/", 50)+"f"), Mode: 0o644}
			}
			return &tar.Header{Typeflag: tar.TypeReg, Name: ".wh.t", Mode: 0o644}
		}},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/var/lib/hangar/images", "worst-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(dir) }()
			a, err := ocilayer.Open(dir, registry.DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = a.Close() }()
			if shape.lower != nil {
				if err := a.Apply(t.Context(), stream(t, func(i int) *tar.Header {
					if i < len(shape.lower) {
						return shape.lower[i]
					}
					return nil
				})); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
			defer cancel()
			start, cpu := time.Now(), cpuTime(t)
			err = a.Apply(ctx, stream(t, shape.next))
			took, used := time.Since(start), cpuTime(t)-cpu
			t.Logf("%s: %v after %v, %v of CPU, %d steps (%.2f µs a step)", name, err, took, used, a.Work(), float64(took.Microseconds())/float64(max(a.Work(), 1)))
			if err != nil && !errors.Is(err, driver.ErrImageTooLarge) {
				t.Errorf("%v, want too large", err)
			}
		})
	}
}

// cpuTime is the CPU the test binary has used, user and system.
func cpuTime(t *testing.T) time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// stream is a layer of the headers next gives, until it gives nil.
func stream(t *testing.T, next func(i int) *tar.Header) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		w := tar.NewWriter(pw)
		for i := 0; ; i++ {
			h := next(i)
			if h == nil {
				break
			}
			if err := w.WriteHeader(h); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		_ = pw.CloseWithError(w.Close())
	}()
	t.Cleanup(func() { _ = pr.Close() })
	return pr
}
