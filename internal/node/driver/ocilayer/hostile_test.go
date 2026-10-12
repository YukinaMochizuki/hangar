package ocilayer_test

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
)

// The layers in this file are what a hostile image could hold. Each must
// leave everything outside the root as it was, and either fail or land
// inside the root.

// TestDotDotIsRefused: a name or link target climbing out of the root fails
// the layer.
func TestDotDotIsRefused(t *testing.T) {
	for name, l := range map[string][]entry{
		"name":                {file("../secret", "pwned", 0o644)},
		"name through a dir":  {file("a/../../secret", "pwned", 0o644)},
		"deep name":           {file("a/b/../../../../../secret", "pwned", 0o644)},
		"hardlink target":     {hardlink("h", "../secret")},
		"dir":                 {dir("../evil", 0o755)},
		"symlink name":        {symlink("../evil", "/")},
		"whiteout":            {file("../.wh.secret", "", 0o644)},
		"opaque whiteout":     {file("../.wh..wh..opq", "", 0o644)},
		"whiteout of dot-dot": {file("a/.wh...", "", 0o644)},
		"whiteout of dot":     {file(".wh..", "", 0o644)},
		"whiteout of nothing": {file(".wh.", "", 0o644)},
		"hardlink to dot-dot": {hardlink("h", "a/../../secret")},
		"absolute dot-dot":    {file("/../secret", "pwned", 0o644)},
		// Refused even when the root holds a file of the name it would reach
		// were the dot-dot dropped.
		"hardlink to dot-dot of a file inside": {file("secret", "inside", 0o644), hardlink("h", "../secret")},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			// Refused as the image's fault, which trying again never fixes.
			if err := apply(t, s.root, unlimited, layer(t, l...)); !errors.Is(err, driver.ErrInvalidImage) {
				t.Errorf("%v, want an invalid image", err)
			}
			s.untouched(t)
		})
	}
}

// TestAbsoluteNamesStayInside: an absolute name is taken from the root, as
// Docker takes it.
func TestAbsoluteNamesStayInside(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited, layer(t,
		file("/abs", "inside", 0o644),
		file(s.secret, "pwned", 0o644), // the host path of the secret
		file("//double//slash", "inside", 0o644),
	))
	if err != nil {
		t.Fatal(err)
	}
	s.untouched(t)
	if got := s.read(t, "abs"); got != "inside" {
		t.Errorf("abs: %q", got)
	}
	if got := s.read(t, s.secret); got != "pwned" {
		t.Errorf("the host path, inside the root: %q", got)
	}
	if got := s.read(t, "double/slash"); got != "inside" {
		t.Errorf("double/slash: %q", got)
	}
}

// TestSymlinkParentsStayInside: a symlink is followed as the image's own
// processes would see it, the root being their /, never out of it.
func TestSymlinkParentsStayInside(t *testing.T) {
	cases := map[string]struct {
		layers [][]entry
		// inside is where the file lands in the root; "" means the layer
		// must fail.
		inside string
	}{
		"relative climbing out": {
			[][]entry{{symlink("up", "../../../.."), file("up/secret", "pwned", 0o644)}}, "secret",
		},
		"relative climbing out, layer below": {
			[][]entry{{symlink("up", "../../../..")}, {file("up/secret", "pwned", 0o644)}}, "secret",
		},
		"absolute to an existing dir": {
			[][]entry{{dir("etc", 0o755), symlink("host", "/etc")}, {file("host/secret", "pwned", 0o644)}}, "etc/secret",
		},
		"absolute, from a dir below": {
			[][]entry{{dir("etc", 0o755), dir("d", 0o755), symlink("d/l", "/etc")}, {file("d/l/secret", "pwned", 0o644)}}, "etc/secret",
		},
		"dot-dot, from a dir below": {
			[][]entry{{dir("x", 0o755), dir("y", 0o755), symlink("x/up", "../y")}, {file("x/up/secret", "pwned", 0o644)}}, "y/secret",
		},
		"chain of links": {
			[][]entry{{symlink("a", "b"), symlink("b", "../../c"), dir("c", 0o755)}, {file("a/secret", "pwned", 0o644)}}, "c/secret",
		},
		"dir made under a link": {
			[][]entry{{symlink("up", "../..")}, {dir("up/made", 0o755), file("up/made/secret", "pwned", 0o644)}}, "made/secret",
		},
		"missing parents made under a link": {
			[][]entry{{symlink("up", "../..")}, {file("up/x/y/secret", "pwned", 0o644)}}, "x/y/secret",
		},
		"loop": {
			[][]entry{{symlink("a", "b"), symlink("b", "a")}, {file("a/secret", "pwned", 0o644)}}, "",
		},
		"link to a file as a parent": {
			[][]entry{{file("f", "x", 0o644), symlink("l", "f")}, {file("l/secret", "pwned", 0o644)}}, "",
		},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			var layers [][]byte
			for _, l := range tt.layers {
				layers = append(layers, layer(t, l...))
			}
			err := apply(t, s.root, unlimited, layers...)
			s.untouched(t)
			if tt.inside == "" {
				if err == nil {
					t.Error("applied")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := s.read(t, tt.inside); got != "pwned" {
				t.Errorf("%s: %q", tt.inside, got)
			}
		})
	}
}

// TestSymlinkSandbox: the host sandbox the root sits in is reachable by an
// absolute symlink only as a path inside the root.
func TestSymlinkSandbox(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, symlink("host", s.base)),
		layer(t, file("host/secret", "pwned", 0o644)))
	s.untouched(t)
	if err == nil {
		// Only acceptable if it landed inside.
		if got := s.read(t, filepath.Join(s.base, "secret")); got != "pwned" {
			t.Errorf("applied, but not inside: %q", got)
		}
	}
}

// TestEntriesReplaceSymlinks: an entry named like a symlink replaces it; it
// is never written through it.
func TestEntriesReplaceSymlinks(t *testing.T) {
	for name, over := range map[string]entry{
		"file":     file("f", "replaced", 0o644),
		"dir":      dir("f", 0o755),
		"symlink":  symlink("f", "elsewhere"),
		"hardlink": hardlink("f", "other"),
		"fifo":     {hdr: tar.Header{Typeflag: tar.TypeFifo, Name: "f", Mode: 0o644}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			err := apply(t, s.root, unlimited,
				layer(t, symlink("f", s.secret), file("other", "other", 0o644)),
				layer(t, over))
			if err != nil {
				t.Fatal(err)
			}
			s.untouched(t)
			var st syscall.Stat_t
			if err := syscall.Lstat(filepath.Join(s.root, "f"), &st); err != nil {
				t.Fatal(err)
			}
			want := map[byte]uint32{
				tar.TypeReg: syscall.S_IFREG, tar.TypeDir: syscall.S_IFDIR, tar.TypeSymlink: syscall.S_IFLNK,
				tar.TypeLink: syscall.S_IFREG, tar.TypeFifo: syscall.S_IFIFO,
			}[over.hdr.Typeflag]
			if st.Mode&syscall.S_IFMT != want {
				t.Errorf("f has mode %o", st.Mode)
			}
		})
	}
}

// TestHardlinksStayInside: a hardlink's target is found inside the root,
// never through a symlink out of it, and a hardlink to a symlink links the
// symlink itself.
func TestHardlinksStayInside(t *testing.T) {
	t.Run("absolute host path", func(t *testing.T) {
		s := newSandbox(t)
		if err := apply(t, s.root, unlimited, layer(t, hardlink("h", s.secret))); err == nil {
			t.Error("linked a path the root does not hold")
		}
		s.untouched(t)
	})
	t.Run("through a symlink parent", func(t *testing.T) {
		s := newSandbox(t)
		if err := apply(t, s.root, unlimited, layer(t, symlink("host", s.base), hardlink("h", "host/secret"))); err == nil {
			t.Error("linked through a symlink out of the root")
		}
		s.untouched(t)
	})
	t.Run("to a symlink pointing out, then written", func(t *testing.T) {
		s := newSandbox(t)
		err := apply(t, s.root, unlimited,
			layer(t, symlink("s", s.secret), hardlink("h", "s")),
			layer(t, file("h", "pwned", 0o644)))
		if err != nil {
			t.Fatal(err)
		}
		s.untouched(t)
		if l, err := os.Readlink(filepath.Join(s.root, "s")); err != nil || l != s.secret {
			t.Errorf("the symlink: %q %v", l, err)
		}
		if got := s.read(t, "h"); got != "pwned" {
			t.Errorf("h: %q", got)
		}
	})
	t.Run("to a symlink pointing out", func(t *testing.T) {
		s := newSandbox(t)
		if err := apply(t, s.root, unlimited, layer(t, symlink("s", s.secret), hardlink("h", "s"))); err != nil {
			t.Fatal(err)
		}
		// Linked to the symlink itself, never to what it points at.
		s.untouched(t)
		if l, err := os.Readlink(filepath.Join(s.root, "h")); err != nil || l != s.secret {
			t.Errorf("h: %q %v, want the symlink", l, err)
		}
	})
	t.Run("to a dir", func(t *testing.T) {
		s := newSandbox(t)
		if err := apply(t, s.root, unlimited, layer(t, dir("d", 0o755), hardlink("h", "d"))); err == nil {
			t.Error("made a hardlink to a dir")
		}
		s.untouched(t)
	})
}

// TestWhiteoutsStayInside: a whiteout removes only inside the root.
func TestWhiteoutsStayInside(t *testing.T) {
	for name, l := range map[string][]entry{
		"through a symlink":        {symlink("host", "PLACEHOLDER"), file("host/.wh.secret", "", 0o644)},
		"opaque through a symlink": {symlink("host", "PLACEHOLDER"), file("host/.wh..wh..opq", "", 0o644)},
		"of an absolute path":      {file("PLACEHOLDER/.wh.secret", "", 0o644)},
		"of a symlink pointing out": {
			symlink("s", "PLACEHOLDER/secret"), file(".wh.s", "", 0o644),
		},
		// The opaque marker keeps what its layer wrote, here a symlink out,
		// and never empties what that symlink points at.
		"opaque beside a symlink out": {symlink("s", "PLACEHOLDER"), file(".wh..wh..opq", "", 0o644)},
		"opaque beside a symlink out, deeper": {
			dir("d", 0o755), symlink("d/s", "PLACEHOLDER"), file("d/.wh..wh..opq", "", 0o644), file(".wh..wh..opq", "", 0o644),
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			for i := range l {
				l[i].hdr.Name = strings.ReplaceAll(l[i].hdr.Name, "PLACEHOLDER", s.base)
				l[i].hdr.Linkname = strings.ReplaceAll(l[i].hdr.Linkname, "PLACEHOLDER", s.base)
			}
			_ = apply(t, s.root, unlimited, layer(t, l...))
			s.untouched(t)
		})
	}
}

// TestSizeIsBounded: a layer unpacking to more than the bound fails as too
// large, before it is read through.
func TestSizeIsBounded(t *testing.T) {
	lim := ocilayer.Limits{MaxBytes: 1 << 20, MaxEntries: 100}
	big := strings.Repeat("x", 3<<20)
	for name, layers := range map[string][][]entry{
		"one file":           {{file("big", big, 0o644)}},
		"many files":         {{file("a", big[:600<<10], 0o644), file("b", big[:600<<10], 0o644)}},
		"across layers":      {{file("a", big[:600<<10], 0o644)}, {file("b", big[:600<<10], 0o644)}},
		"rewriting one file": {{file("a", big[:600<<10], 0o644)}, {file("a", big[:600<<10], 0o644)}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			a, err := ocilayer.Open(s.root, lim)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = a.Close() }()
			for i, l := range layers {
				data := layer(t, l...)
				r := &countingReader{r: strings.NewReader(string(data))}
				err = a.Apply(t.Context(), r)
				if i < len(layers)-1 {
					if err != nil {
						t.Fatalf("layer %d: %v", i, err)
					}
					continue
				}
				if !errors.Is(err, driver.ErrImageTooLarge) {
					t.Fatalf("the last layer: %v, want too large", err)
				}
				if r.n > lim.MaxBytes+64<<10 {
					t.Errorf("read %d bytes of a layer past the bound", r.n)
				}
			}
			s.untouched(t)
		})
	}
	t.Run("entries", func(t *testing.T) {
		s := newSandbox(t)
		var many []entry
		for i := range 101 {
			many = append(many, dir("d"+strings.Repeat("x", i), 0o755))
		}
		if err := apply(t, s.root, lim, layer(t, many...)); !errors.Is(err, driver.ErrImageTooLarge) {
			t.Errorf("101 entries with a bound of 100: %v", err)
		}
	})
}

// TestDevicesAreSkipped: device nodes are not made on the host; fifos are.
func TestDevicesAreSkipped(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited, layer(t,
		dir("dev", 0o755),
		device("dev/sda", tar.TypeBlock),
		device("dev/mem", tar.TypeChar),
		entry{hdr: tar.Header{Typeflag: tar.TypeFifo, Name: "dev/fifo", Mode: 0o640}},
	))
	if err != nil {
		t.Fatal(err)
	}
	s.untouched(t)
	for _, name := range []string{"sda", "mem"} {
		if _, err := os.Lstat(filepath.Join(s.root, "dev", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dev/%s was made: %v", name, err)
		}
	}
	fi, err := os.Lstat(filepath.Join(s.root, "dev", "fifo"))
	if err != nil || fi.Mode().Type() != os.ModeNamedPipe || fi.Mode().Perm() != 0o640 {
		t.Errorf("dev/fifo: %v %v", fi, err)
	}
}

// TestDevicesReplaceWhatWasThere: a device node, skipped, still takes the
// place of what a lower layer had at its name, as it would in the image.
func TestDevicesReplaceWhatWasThere(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, file("dev/null", "a file", 0o644), file("dev/keep", "k", 0o644), dir("dev/sda", 0o755), file("dev/sda/x", "x", 0o644)),
		layer(t, device("dev/null", tar.TypeChar), device("dev/sda", tar.TypeBlock)),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, s.root, "dev/", "dev/keep")
}

// TestUnknownTypesFail: an entry of a type that cannot be unpacked faithfully
// fails the layer.
func TestUnknownTypesFail(t *testing.T) {
	s := newSandbox(t)
	if err := apply(t, s.root, unlimited, layer(t, entry{hdr: tar.Header{Typeflag: 'Z', Name: "z"}})); err == nil {
		t.Error("applied an entry of an unknown type")
	}
	s.untouched(t)
}

// TestDeepTreesAreRemoved: a whiteout of a tree as deep as names may go
// removes it.
func TestDeepTreesAreRemoved(t *testing.T) {
	s := newSandbox(t)
	deep := strings.Repeat("d/", ocilayer.DefaultMaxDepth-1) + "f"
	err := apply(t, s.root, unlimited,
		layer(t, file(deep, "x", 0o644)),
		layer(t, file(".wh.d", "", 0o644)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.root, "d")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("d is still there: %v", err)
	}
	s.untouched(t)
}

// TestOddHeadersFail: an owner chown(2) would read as "leave it", or a
// root that is not a dir, fails the layer.
func TestOddHeadersFail(t *testing.T) {
	for name, e := range map[string]entry{
		"uid 2^32-1":      {hdr: tar.Header{Typeflag: tar.TypeReg, Name: "f", Uid: 1<<32 - 1, Mode: 0o644}},
		"gid 2^32-1":      {hdr: tar.Header{Typeflag: tar.TypeReg, Name: "f", Gid: 1<<32 - 1, Mode: 0o644}},
		"uid 2^32":        {hdr: tar.Header{Typeflag: tar.TypeReg, Name: "f", Uid: 1 << 32, Mode: 0o644}},
		"the root a file": {hdr: tar.Header{Typeflag: tar.TypeReg, Name: ".", Mode: 0o644}},
		"the root a link": {hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "/", Linkname: "/etc"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			if err := apply(t, s.root, unlimited, layer(t, e)); err == nil {
				t.Error("applied")
			}
			s.untouched(t)
		})
	}
}

// TestTheBoundIsExact: as many entries as the bound allows apply.
func TestTheBoundIsExact(t *testing.T) {
	s := newSandbox(t)
	var many []entry
	for i := range 10 {
		many = append(many, file("f"+strings.Repeat("x", i), "0123456789", 0o644))
	}
	// 100 bytes of contents, and 55 of names.
	if err := apply(t, s.root, ocilayer.Limits{MaxBytes: 155, MaxEntries: 10}, layer(t, many...)); err != nil {
		t.Errorf("10 entries of 15.5 bytes, with bounds of 10 and 155: %v", err)
	}
	for _, lim := range []ocilayer.Limits{{MaxBytes: 154, MaxEntries: 10}, {MaxBytes: 155, MaxEntries: 9}} {
		if err := apply(t, newSandbox(t).root, lim, layer(t, many...)); !errors.Is(err, driver.ErrImageTooLarge) {
			t.Errorf("with bounds %+v: %v", lim, err)
		}
	}
}

// TestTruncatedLayerFails: a layer cut short fails rather than leaving a
// short file as if whole.
func TestTruncatedLayerFails(t *testing.T) {
	s := newSandbox(t)
	data := layer(t, file("f", strings.Repeat("x", 10000), 0o644))
	if err := apply(t, s.root, unlimited, data[:5000]); err == nil {
		t.Error("applied a truncated layer")
	}
	s.untouched(t)
}
