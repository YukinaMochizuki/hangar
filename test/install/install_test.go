// Package install tests deploy/cli/install.sh.in, the CLI's installer, with
// /bin/sh against a stand-in for GitHub's downloads and API.
package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// realCLI builds cmd/hangar once: the installed binary runs it for anything
// but `which-release`, so the installer's context steps meet the real CLI.
var realCLI = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "hangar-install-test")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "hangar")
	cmd := exec.Command("go", "build", "-o", out, "github.com/YukinaMochizuki/hangar/cmd/hangar")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, b)
	}
	return out, nil
})

// release is what the stand-in serves for one tag.
type release struct {
	noArchive bool // SHA256SUMS lists the archive but it is not there
	badSum    bool // SHA256SUMS lists a wrong checksum for the archive
}

type standIn struct {
	t        *testing.T
	releases map[string]release
	apiDown  bool
	srv      *httptest.Server
}

func archiveName(tag string) string {
	return fmt.Sprintf("hangar_%s_%s_%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

// archive packs a hangar that answers `which-release` with tag and runs the
// real CLI otherwise.
func (s *standIn) archive(tag string) []byte {
	cli, err := realCLI()
	if err != nil {
		s.t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = which-release ] && { echo %s; exit 0; }\nexec %s \"$@\"\n", tag, cli)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "hangar", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		s.t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		s.t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		s.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		s.t.Fatal(err)
	}
	return buf.Bytes()
}

func newStandIn(t *testing.T, releases map[string]release) *standIn {
	s := &standIn{t: t, releases: releases}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			if s.apiDown {
				http.Error(w, "rate limited", http.StatusForbidden)
				return
			}
			var list []map[string]any
			for tag := range s.releases {
				list = append(list, map[string]any{"tag_name": tag, "draft": false, "assets": []any{}})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		tag, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		rel, known := s.releases[tag]
		if !ok || !known {
			http.NotFound(w, r)
			return
		}
		a := s.archive(tag)
		switch file {
		case "SHA256SUMS":
			sum := sha256.Sum256(a)
			if rel.badSum {
				sum = sha256.Sum256([]byte("something else"))
			}
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), archiveName(tag))
		case archiveName(tag):
			if rel.noArchive {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(a)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

type result struct {
	code   int
	stderr string
	dir    string
}

// run runs the installer made for tag, as release.sh makes it, with args.
func (s *standIn) run(tag, home string, args ...string) result {
	s.t.Helper()
	src, err := os.ReadFile("../../deploy/cli/install.sh.in")
	if err != nil {
		s.t.Fatal(err)
	}
	script := filepath.Join(s.t.TempDir(), "install.sh")
	if err := os.WriteFile(script, bytes.ReplaceAll(src, []byte("@TAG@"), []byte(tag)), 0o755); err != nil {
		s.t.Fatal(err)
	}
	dir := filepath.Join(home, "bin")
	cmd := exec.Command("/bin/sh", append([]string{script, "--dir", dir}, args...)...)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"HANGAR_CONFIG=" + filepath.Join(home, "config", "config.yaml"),
		"HANGAR_INSTALL_BASE_URL=" + s.srv.URL,
		"HANGAR_INSTALL_API_URL=" + s.srv.URL + "/api",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			s.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return result{code: code, stderr: stderr.String(), dir: dir}
}

// installed says which release's hangar is in dir, "" for none.
func installed(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "hangar")
	if _, err := os.Stat(bin); err != nil {
		return ""
	}
	out, err := exec.Command(bin, "which-release").Output()
	if err != nil {
		t.Fatalf("%s which-release: %v", bin, err)
	}
	return strings.TrimSpace(string(out))
}

func TestPicksTheNewestOfItsMinor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tag      string
		releases []string
		want     string
	}{
		{"a newer patch", "v0.2.0", []string{"v0.2.0", "v0.2.1", "v0.2.3"}, "v0.2.3"},
		{"not another minor or major", "v0.2.0", []string{"v0.2.0", "v0.3.0", "v1.2.0"}, "v0.2.0"},
		{"not a pre-release after a release", "v0.2.0", []string{"v0.2.0", "v0.2.1-rc.1"}, "v0.2.0"},
		{"never an older one", "v0.2.2", []string{"v0.2.1", "v0.2.2"}, "v0.2.2"},
		{"a later pre-release after a pre-release", "v0.3.0-rc.1", []string{"v0.3.0-rc.1", "v0.3.0-rc.2"}, "v0.3.0-rc.2"},
		{"the release after its pre-release", "v0.3.0-rc.2", []string{"v0.3.0-rc.2", "v0.3.0", "v0.3.0-rc.10"}, "v0.3.0"},
		{"rc.10 after rc.9", "v0.3.0-rc.9", []string{"v0.3.0-rc.9", "v0.3.0-rc.10"}, "v0.3.0-rc.10"},
		{"names that are not tags are ignored", "v0.2.0", []string{"v0.2.0", "v0.2.9-beta", "cli-v0.2.5"}, "v0.2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rels := map[string]release{}
			for _, r := range tc.releases {
				rels[r] = release{}
			}
			s := newStandIn(t, rels)
			r := s.run(tc.tag, t.TempDir())
			if r.code != 0 {
				t.Fatalf("exit %d:\n%s", r.code, r.stderr)
			}
			if got := installed(t, r.dir); got != tc.want {
				t.Errorf("installed %q, want %q\n%s", got, tc.want, r.stderr)
			}
		})
	}
}

func TestFallsBackToItsOwnTag(t *testing.T) {
	t.Run("GitHub's API does not answer", func(t *testing.T) {
		s := newStandIn(t, map[string]release{"v0.2.0": {}, "v0.2.1": {}})
		s.apiDown = true
		r := s.run("v0.2.0", t.TempDir())
		if r.code != 0 || installed(t, r.dir) != "v0.2.0" {
			t.Fatalf("exit %d, installed %q, want v0.2.0:\n%s", r.code, installed(t, r.dir), r.stderr)
		}
	})
	t.Run("the newer release has no archive", func(t *testing.T) {
		s := newStandIn(t, map[string]release{"v0.2.0": {}, "v0.2.1": {noArchive: true}})
		r := s.run("v0.2.0", t.TempDir())
		if r.code != 0 || installed(t, r.dir) != "v0.2.0" {
			t.Fatalf("exit %d, installed %q, want v0.2.0:\n%s", r.code, installed(t, r.dir), r.stderr)
		}
	})
}

func TestVersionInstallsExactlyThat(t *testing.T) {
	s := newStandIn(t, map[string]release{"v0.2.0": {}, "v0.2.1": {}, "v0.2.2": {}})
	r := s.run("v0.2.0", t.TempDir(), "--version", "v0.2.1")
	if r.code != 0 || installed(t, r.dir) != "v0.2.1" {
		t.Fatalf("exit %d, installed %q, want v0.2.1:\n%s", r.code, installed(t, r.dir), r.stderr)
	}
}

func TestVersionNeverFallsBack(t *testing.T) {
	s := newStandIn(t, map[string]release{"v0.2.0": {}, "v0.2.1": {noArchive: true}})
	for _, v := range []string{"v0.2.1", "v0.2.7"} {
		r := s.run("v0.2.0", t.TempDir(), "--version", v)
		if r.code == 0 || !strings.Contains(r.stderr, "could not download the hangar CLI "+v) {
			t.Errorf("--version %s: exit %d, want a refusal:\n%s", v, r.code, r.stderr)
		}
		if got := installed(t, r.dir); got != "" {
			t.Errorf("--version %s installed %q", v, got)
		}
	}
}

func TestRefusesAnArchiveThatDoesNotMatch(t *testing.T) {
	s := newStandIn(t, map[string]release{"v0.2.0": {badSum: true}})
	r := s.run("v0.2.0", t.TempDir())
	if r.code == 0 || !strings.Contains(r.stderr, "refusing it") {
		t.Fatalf("exit %d, want a refusal:\n%s", r.code, r.stderr)
	}
	if got := installed(t, r.dir); got != "" {
		t.Errorf("installed %q after a refusal", got)
	}
}

func TestEndpointMakesTheContext(t *testing.T) {
	s := newStandIn(t, map[string]release{"v0.2.0": {}})
	home := t.TempDir()
	view := func(dir string) string {
		out, err := exec.Command(filepath.Join(dir, "hangar"), "config", "view").Output()
		if err != nil {
			t.Fatalf("config view: %v", err)
		}
		return string(out)
	}
	t.Setenv("HANGAR_CONFIG", filepath.Join(home, "config", "config.yaml"))

	r := s.run("v0.2.0", home, "--endpoint", "https://api.example.test")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	if v := view(r.dir); !strings.Contains(v, "* example.test\thttps://api.example.test") {
		t.Fatalf("config view after install:\n%s", v)
	}

	// A second install keeps the context as it is, its project included, and
	// makes it current again.
	setContext := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(filepath.Join(r.dir, "hangar"), append([]string{"config", "set-context"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("set-context %v: %v\n%s", args, err, out)
		}
	}
	setContext("example.test", "--endpoint", "https://api.example.test", "--project", "web")
	setContext("other", "--endpoint", "https://api.other.test", "--use")
	r = s.run("v0.2.0", home, "--endpoint", "https://api.example.test")
	if r.code != 0 {
		t.Fatalf("second install: exit %d:\n%s", r.code, r.stderr)
	}
	if v := view(r.dir); !strings.Contains(v, "* example.test\thttps://api.example.test\tproject=web") {
		t.Errorf("the second install did not keep example.test, its project, and make it current:\n%s", v)
	}

	// A context of that name with another endpoint is left as it is.
	r = s.run("v0.2.0", home, "--endpoint", "https://api.elsewhere.test", "--context", "other")
	if r.code != 0 || !strings.Contains(r.stderr, "left as it is") {
		t.Fatalf("exit %d, want the context left:\n%s", r.code, r.stderr)
	}
	if v := view(r.dir); !strings.Contains(v, "other\thttps://api.other.test") {
		t.Errorf("context other changed:\n%s", v)
	}
}
