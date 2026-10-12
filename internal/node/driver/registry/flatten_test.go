package registry_test

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// layerOf is a gzipped layer of the headers, each regular file holding its
// name.
func layerOf(t *testing.T, hdrs ...tar.Header) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, h := range hdrs {
		body := []byte(nil)
		if h.Typeflag == tar.TypeReg {
			body = []byte(h.Name + "\n")
			h.Size = int64(len(body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
			if h.Typeflag == tar.TypeDir {
				h.Mode = 0o755
			}
		}
		h.Format = tar.FormatPAX
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	gz := gzipped(t, buf.Bytes())
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(gz)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func reg(name string) tar.Header { return tar.Header{Typeflag: tar.TypeReg, Name: name} }
func dirH(name string) tar.Header {
	return tar.Header{Typeflag: tar.TypeDir, Name: name}
}

// TestFlatteningMatchesTheReference: whiteouts, opaque dirs, replacements
// and links come out as go-containerregistry's flattening has them, which
// is what crane export writes.
func TestFlatteningMatchesTheReference(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	img := image(t, "amd64", nil,
		layerOf(t,
			dirH("etc/"), reg("etc/passwd"), reg("etc/shadow"), dirH("etc/conf.d/"), reg("etc/conf.d/a"), reg("etc/conf.d/b"),
			dirH("var/"), dirH("var/cache/"), dirH("var/cache/apt/"), reg("var/cache/apt/pkg1"), reg("var/cache/apt/pkg2"),
			dirH("usr/"), dirH("usr/bin/"), tar.Header{Typeflag: tar.TypeReg, Name: "usr/bin/su", Mode: 0o4755},
			tar.Header{Typeflag: tar.TypeSymlink, Name: "usr/bin/sh", Linkname: "dash"}, reg("usr/bin/dash"),
			tar.Header{Typeflag: tar.TypeLink, Name: "usr/bin/dash2", Linkname: "usr/bin/dash"},
			tar.Header{Typeflag: tar.TypeReg, Name: "home/u/owned", Uid: 1000, Gid: 1000, Mode: 0o600},
			dirH("tmp/"), reg("tmp/gone-dir-file"), dirH("opt/"), reg("opt/old"),
			reg("file-to-dir"), dirH("dir-to-file/"), reg("dir-to-file/x"),
		),
		layerOf(t,
			reg("etc/.wh.shadow"), reg("etc/conf.d/.wh..wh..opq"), reg("etc/conf.d/c"),
			reg("var/cache/.wh.apt"), reg(".wh.tmp"),
			dirH("file-to-dir/"), reg("file-to-dir/y"), reg("dir-to-file"),
			reg(".wh.absent"),
		),
		layerOf(t,
			dirH("opt/"), reg("opt/.wh..wh..opq"), reg("opt/new"),
			reg("tmp/back"), // a dir whited out below, made again
			reg("usr/bin/.wh.dash2"),
		),
	)
	ref, digest := r.Push(t, "layered:1", img)
	dir := t.TempDir()
	if err := d.Unpack(t.Context(), ref, digest, dir, anon); err != nil {
		t.Fatal(err)
	}
	// etc/{,passwd,conf.d/,conf.d/c} var/{,cache/} usr/{,bin/,bin/su,bin/sh,bin/dash}
	// home/u/owned tmp/back opt/{,new} file-to-dir/{,y} dir-to-file
	if n := registrytest.MatchesExtract(t, img, dir); n != 18 {
		t.Errorf("compared %d entries, want 18", n)
	}
}
