package ocilayer_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
)

// The layers in this file cost little to send and much to apply. Each must
// stay within its bounds, in time as well as on disk.

var huge = ocilayer.Limits{MaxBytes: 1 << 40, MaxEntries: 1 << 40}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// applyGz applies a gzipped layer as the registry driver does, returning
// how long it took and its error.
func applyGz(t *testing.T, ctx context.Context, dir string, lim ocilayer.Limits, layer []byte) (time.Duration, error) {
	t.Helper()
	a, err := ocilayer.Open(dir, lim)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	start := time.Now()
	zr, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		t.Fatal(err)
	}
	err = a.Apply(ctx, zr)
	return time.Since(start), err
}

// TestDeepNamesAreRefused: a name deeper than any real image's, such as a
// 2000-deep dir with files in it, is refused at once.
func TestDeepNamesAreRefused(t *testing.T) {
	s := newSandbox(t)
	deep := strings.Repeat("a/", 2000)
	var entries []entry
	entries = append(entries, dir(deep, 0o755))
	for i := range 20 {
		entries = append(entries, file(fmt.Sprintf("%sf%d", deep, i), "", 0o644))
	}
	l := gz(t, layer(t, entries...))
	t.Logf("a layer of %d bytes, gzipped", len(l))
	took, err := applyGz(t, t.Context(), s.root, huge, l)
	// Bounded to catch a hang, not to time it.
	if !errors.Is(err, driver.ErrImageTooLarge) || took > 10*time.Second {
		t.Errorf("%v after %v, want too large at once", err, took)
	}
	s.untouched(t)
}

// TestDeepTreesTakeLinearWork: entries deep in the tree are applied in work
// linear in their depth: a step a component, each walked once.
func TestDeepTreesTakeLinearWork(t *testing.T) {
	at := func(depth int) int64 {
		s := newSandbox(t)
		deep := strings.Repeat("a/", depth-2)
		a, err := ocilayer.Open(s.root, huge)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = a.Close() }()
		if err := a.Apply(t.Context(), bytes.NewReader(layer(t, dir("t0/"+deep, 0o755), dir("t1/"+deep, 0o755)))); err != nil {
			t.Fatal(err)
		}
		// Each name parts from the last at the top, so no walk is shared,
		// and hides nothing: what it costs is the walk.
		var entries []entry
		for i := range 5000 {
			entries = append(entries, file(fmt.Sprintf("t%d/%s.wh.f%d", i%2, deep, i), "", 0o644))
		}
		before := a.Work()
		if err := a.Apply(t.Context(), bytes.NewReader(layer(t, entries...))); err != nil {
			t.Fatal(err)
		}
		return a.Work() - before
	}
	shallow, deep := at(25), at(250)
	t.Logf("5000 entries at depth 25 in %d steps, at depth 250 in %d", shallow, deep)
	if deep > 12*shallow {
		t.Errorf("ten times deeper took %.1f times the work", float64(deep)/float64(shallow))
	}
}

// TestSymlinkChainsAreResolvedOnce: names through a chain of symlinks, each
// leading 255 dirs deep, cost about the work of the same names as real
// dirs: each symlink is resolved once a layer, not at every step.
func TestSymlinkChainsAreResolvedOnce(t *testing.T) {
	deep := strings.Repeat("a/", 255)
	chain := strings.Repeat("s/", 38)
	at := func(lower []entry) int64 {
		s := newSandbox(t)
		a, err := ocilayer.Open(s.root, huge)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = a.Close() }()
		if err := a.Apply(t.Context(), bytes.NewReader(layer(t, lower...))); err != nil {
			t.Fatal(err)
		}
		var upper []entry
		for i := range 2000 {
			upper = append(upper, file(fmt.Sprintf("%s.wh.x%d", chain, i), "", 0o644))
		}
		before := a.Work()
		if err := a.Apply(t.Context(), bytes.NewReader(layer(t, upper...))); err != nil {
			t.Fatal(err)
		}
		return a.Work() - before
	}
	links := at([]entry{dir(deep, 0o755), symlink("s", "/"+deep), symlink(deep+"s", "/"+deep)})
	dirs := at([]entry{dir(chain, 0o755)})
	t.Logf("2000 names through 38 symlinks in %d steps; through 38 dirs in %d", links, dirs)
	if links > 2*dirs {
		t.Errorf("through symlinks took %.1f times the work of dirs", float64(links)/float64(dirs))
	}
}

// TestWorkIsBounded: a layer taking more steps than the bound fails as too
// large, at the bound.
func TestWorkIsBounded(t *testing.T) {
	s := newSandbox(t)
	var entries []entry
	for i := range 20000 {
		entries = append(entries, file(fmt.Sprintf("d/.wh.x%d", i), "", 0o644))
	}
	a, err := ocilayer.Open(s.root, ocilayer.Limits{MaxBytes: 1 << 30, MaxEntries: 1 << 30, MaxWork: 10000})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	err = a.Apply(t.Context(), bytes.NewReader(layer(t, entries...)))
	if !errors.Is(err, driver.ErrImageTooLarge) {
		t.Errorf("20000 whiteouts with 10000 steps: %v", err)
	}
	if w := a.Work(); w < 10000 || w > 10000+20 {
		t.Errorf("stopped after %d steps, want 10000", w)
	}
}

// TestDirsAreMadeOnlyAsDeepAsNamesGo: through symlinks, names within the
// bounds would make dirs deeper, or paths longer, than the host can walk;
// they are refused as they are made.
func TestDirsAreMadeOnlyAsDeepAsNamesGo(t *testing.T) {
	deep := strings.Repeat("a/", 250)
	long := strings.Repeat(strings.Repeat("n", 200)+"/", 10)
	for name, layers := range map[string][][]entry{
		"deep": {{dir(deep, 0o755), symlink("s", "/"+deep)}, {file("s/"+strings.Repeat("b/", 100)+"f", "", 0o644)}},
		"long": {{dir(long, 0o755), symlink("s", "/"+long)}, {file("s/"+strings.Repeat(strings.Repeat("m", 200)+"/", 10)+"f", "", 0o644)}},
		"dir":  {{dir(deep, 0o755), symlink("s", "/"+deep)}, {dir("s/b/c/d/e/f/g/h/i", 0o755)}},
		// The dirs as deep as allowed, the file one deeper.
		"file": {{dir(deep, 0o755), symlink("s", "/"+deep)}, {file("s/b/c/d/e/f/g/x", "", 0o644)}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			var ls [][]byte
			for _, l := range layers {
				ls = append(ls, layer(t, l...))
			}
			if err := apply(t, s.root, unlimited, ls...); !errors.Is(err, driver.ErrImageTooLarge) {
				t.Errorf("%v, want too large", err)
			}
		})
	}
}

// TestRemovingStopsWhenCancelled: removing a large tree stops once the
// layer's context is done.
func TestRemovingStopsWhenCancelled(t *testing.T) {
	s := newSandbox(t)
	var many []entry
	for i := range 50000 {
		many = append(many, file(fmt.Sprintf("d/%d", i), "", 0o644))
	}
	a, err := ocilayer.Open(s.root, huge)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(t.Context(), bytes.NewReader(layer(t, many...))); err != nil {
		t.Fatal(err)
	}
	for name, marker := range map[string]string{"whiteout": ".wh.d", "opaque": ".wh..wh..opq"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			// Done only after the layer is read, so the removal must notice:
			// it checks every few thousand steps, more than ten times here.
			err := a.Apply(&lateCtx{Context: ctx, checks: 10}, bytes.NewReader(layer(t, file(marker, "", 0o644))))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%v, want cancelled", err)
			}
			if names, err := os.ReadDir(filepath.Join(s.root, "d")); err != nil || len(names) == 0 {
				t.Errorf("the removal ran to the end: %d left, %v", len(names), err)
			}
		})
	}
}

// lateCtx is a context done only after it was checked so many times.
type lateCtx struct {
	context.Context
	checks int
}

func (c *lateCtx) Err() error {
	if c.checks > 0 {
		c.checks--
		return nil
	}
	return c.Context.Err()
}

// endless is a tar stream of whiteouts of nothing, without end.
type endless struct{ buf bytes.Buffer }

func (e *endless) Read(p []byte) (int, error) {
	if e.buf.Len() == 0 {
		w := tar.NewWriter(&e.buf)
		for i := range 1000 {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("d/.wh.%d", i), Mode: 0o644})
		}
		_ = w.Flush()
	}
	return e.buf.Read(p)
}

// TestApplyStopsWhenCancelled: a layer that would take long, here one that
// never ends, behind gzip as it reaches Apply, stops once its context is
// done.
func TestApplyStopsWhenCancelled(t *testing.T) {
	s := newSandbox(t)
	pr, pw := io.Pipe()
	go func() {
		zw := gzip.NewWriter(pw)
		_, err := io.Copy(zw, &endless{})
		_ = pw.CloseWithError(err)
	}()
	defer func() { _ = pr.Close() }()
	zr, err := gzip.NewReader(pr)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ocilayer.Open(s.root, huge)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = a.Apply(ctx, zr)
	// Bounded to catch a hang, not to time it.
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Errorf("%v after %v, want the deadline", err, time.Since(start))
	}

	done, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	if err := a.Apply(done, bytes.NewReader(layer(t, file("f", "x", 0o644)))); !errors.Is(err, context.Canceled) {
		t.Errorf("with the context done before: %v", err)
	}
}

// TestEverythingMadeIsCounted: implicit dirs count as entries; names,
// symlink targets and xattrs as bytes.
func TestEverythingMadeIsCounted(t *testing.T) {
	t.Run("implicit dirs", func(t *testing.T) {
		s := newSandbox(t)
		err := apply(t, s.root, ocilayer.Limits{MaxBytes: 1 << 30, MaxEntries: 2},
			layer(t, file(strings.Repeat("a/", 200)+"f", "", 0o644)))
		if !errors.Is(err, driver.ErrImageTooLarge) {
			t.Errorf("an entry making 201 nodes, with a bound of 2: %v", err)
		}
	})
	t.Run("xattrs", func(t *testing.T) {
		s := newSandbox(t)
		var entries []entry
		for i := range 200 {
			e := file(fmt.Sprintf("f%d", i), "", 0o644)
			e.hdr.PAXRecords = map[string]string{"SCHILY.xattr.user.big": strings.Repeat("x", 3000)}
			entries = append(entries, e)
		}
		err := apply(t, s.root, ocilayer.Limits{MaxBytes: 100 << 10, MaxEntries: 1000}, layer(t, entries...))
		if !errors.Is(err, driver.ErrImageTooLarge) {
			t.Errorf("600 KB of xattrs with a bound of 100 KB: %v", err)
		}
	})
	t.Run("symlink targets", func(t *testing.T) {
		s := newSandbox(t)
		var entries []entry
		for i := range 200 {
			entries = append(entries, symlink(fmt.Sprintf("l%d", i), strings.Repeat("t", 4000)))
		}
		err := apply(t, s.root, ocilayer.Limits{MaxBytes: 100 << 10, MaxEntries: 1000}, layer(t, entries...))
		if !errors.Is(err, driver.ErrImageTooLarge) {
			t.Errorf("800 KB of symlink targets with a bound of 100 KB: %v", err)
		}
	})
	t.Run("xattrs of one entry", func(t *testing.T) {
		s := newSandbox(t)
		e := file("f", "", 0o644)
		e.hdr.PAXRecords = map[string]string{
			"SCHILY.xattr.user.a": strings.Repeat("x", 40000), "SCHILY.xattr.user.b": strings.Repeat("x", 40000),
		}
		if err := apply(t, s.root, unlimited, layer(t, e)); !errors.Is(err, driver.ErrImageTooLarge) {
			t.Errorf("80 KB of xattrs on one file: %v", err)
		}
	})
}

// TestDirsNestedThroughSymlinksAreBounded: names within the depth bound can
// nest dirs deeper, through a symlink; removing them is bounded all the same.
func TestDirsNestedThroughSymlinksAreBounded(t *testing.T) {
	deep := strings.Repeat("a/", 200)
	nested := []entry{dir(deep, 0o755), symlink("s", deep), file("s/"+strings.Repeat("b/", 200)+"f", "", 0o644)}
	for name, layers := range map[string][][]entry{
		"whiteout":                 {nested, {file(".wh.a", "", 0o644)}},
		"opaque":                   {nested, {file(".wh..wh..opq", "", 0o644)}},
		"opaque of the same layer": {append(slices.Clone(nested), file(".wh..wh..opq", "", 0o644))},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			var ls [][]byte
			for _, l := range layers {
				ls = append(ls, layer(t, l...))
			}
			err := apply(t, s.root, unlimited, ls...)
			if !errors.Is(err, driver.ErrImageTooLarge) {
				t.Errorf("removing dirs nested 400 deep: %v", err)
			}
		})
	}
}

// TestReplacedDirsAreWalkedAgain: a dir replaced within a layer is the new
// one for the entries after, not the one removed.
func TestReplacedDirsAreWalkedAgain(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited, layer(t,
		file("a/b/f1", "1", 0o644), file("a", "a file", 0o644), dir("a", 0o755), file("a/b/f2", "2", 0o644)))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, s.root, "a/", "a/b/", "a/b/f2")
}

// TestOpaqueMarkersRepeated: many opaque markers of one dir in one layer
// empty it once, not once each.
func TestOpaqueMarkersRepeated(t *testing.T) {
	s := newSandbox(t)
	var lower, upper []entry
	for i := range 5000 {
		lower = append(lower, file(fmt.Sprintf("d/f%d", i), "", 0o644))
		upper = append(upper, file(fmt.Sprintf("d/g%d", i), "", 0o644), file("d/.wh..wh..opq", "", 0o644))
	}
	a, err := ocilayer.Open(s.root, huge)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(t.Context(), bytes.NewReader(layer(t, lower...))); err != nil {
		t.Fatal(err)
	}
	before := a.Work()
	if err := a.Apply(t.Context(), bytes.NewReader(layer(t, upper...))); err != nil {
		t.Fatal(err)
	}
	// Each entry takes a few steps, and the dir is listed once.
	if w := a.Work() - before; w > 400000 {
		t.Errorf("5000 opaque markers of one dir took %d steps", w)
	}
	if got := len(tree(t, s.root)); got != 5001 {
		t.Errorf("%d entries left, want d and its 5000 of the upper layer", got)
	}
}
