package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestTheRepositorysPinsParse: converter.env as committed reads as 21
// packages, qemu-img among them, and the release that mirrors them.
func TestTheRepositorysPinsParse(t *testing.T) {
	f, err := os.Open("../../guest/converter/converter.env")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	p, err := parseEnv(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.packages) != 21 || p.alpine != "v3.22" || p.mirror != "https://dl-cdn.alpinelinux.org/alpine" {
		t.Errorf("%d packages of Alpine %s from %s", len(p.packages), p.alpine, p.mirror)
	}
	if p.release.owner != "YukinaMochizuki" || p.release.repo != "hangar" || p.release.tag != "converter-deps" {
		t.Errorf("release %+v", p.release)
	}
	if !slices.ContainsFunc(p.packages, func(k pkg) bool {
		return k.repo == "community" && k.name == "qemu-img" && k.version == "10.0.0-r1"
	}) {
		t.Errorf("no community/qemu-img-10.0.0-r1 in %v", p.packages)
	}
	if k := p.packages[len(p.packages)-1]; k.name != "zstd-libs" || k.version != "1.5.7-r0" || k.file() != "zstd-libs-1.5.7-r0.apk" {
		t.Errorf("the last pin %+v, file %s", k, k.file())
	}
}

const testEnv = `# pins
HANGAR_CONVERTER_ALPINE=v3.22
HANGAR_CONVERTER_MIRROR=https://alpine.example/alpine/
HANGAR_CONVERTER_RELEASE=https://github.com/o/r/releases/download/converter-deps
HANGAR_CONVERTER_PACKAGES="
%s
"
`

func TestPinsThatDoNotParseAreRefused(t *testing.T) {
	sum := strings.Repeat("a", 64)
	if p, err := parseEnv(strings.NewReader(fmt.Sprintf(testEnv, "main/p11-kit-0.26.2-r0 "+sum))); err != nil ||
		p.packages[0].name != "p11-kit" || p.packages[0].version != "0.26.2-r0" || p.mirror != "https://alpine.example/alpine" {
		t.Errorf("a name with a dash and digits: %+v %v", p, err)
	}
	for _, bad := range []string{
		"testing/foo-1.0-r0 " + sum,
		"main/foo-1.0 " + sum,
		"main/foo-1.0-r0 " + sum[:63],
		"main/foo-1.0-r0",
		"main/foo-1.0-r0 " + strings.ToUpper(sum),
		"",
	} {
		if _, err := parseEnv(strings.NewReader(fmt.Sprintf(testEnv, bad))); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
	for _, env := range []string{
		strings.Replace(fmt.Sprintf(testEnv, "main/foo-1.0-r0 "+sum), "https://github.com/o/r/releases/download/converter-deps", "https://example.com/x", 1),
		strings.Replace(fmt.Sprintf(testEnv, "main/foo-1.0-r0 "+sum), "HANGAR_CONVERTER_ALPINE=v3.22\n", "", 1),
		strings.TrimSuffix(fmt.Sprintf(testEnv, "main/foo-1.0-r0 "+sum), "\"\n"),
		"export FOO=1\n",
	} {
		if _, err := parseEnv(strings.NewReader(env)); err == nil {
			t.Errorf("taken:\n%s", env)
		}
	}
}

// TestVersionsCompareAsApkDoes: numbers as numbers, more of them newer, a
// letter after, pre-releases before and post-releases after a release, the
// revision last.
func TestVersionsCompareAsApkDoes(t *testing.T) {
	for _, tt := range [][2]string{
		{"1.2.3-r0", "1.2.3-r1"},
		{"1.2.3-r9", "1.2.10-r0"},
		{"2.84.4-r2", "2.84.10-r0"},
		{"10.0.0-r1", "10.0.3-r0"},
		{"9.2.0-r0", "10.0.0-r1"},
		{"1.3.1", "1.3.1.1"},
		{"1.2", "1.2a"},
		{"1.2a", "1.2b"},
		{"1.2_alpha1", "1.2_beta"},
		{"1.2_rc1", "1.2_rc2"},
		{"1.2_rc2", "1.2"},
		{"1.2", "1.2_p1"},
		{"1.2_git20240101", "1.2_p1"},
		{"0.24.1-r0", "0.24.1-r10"},
		{"1.010", "1.11"},
	} {
		if c := compareVersions(tt[0], tt[1]); c >= 0 {
			t.Errorf("%s vs %s: %d, want older", tt[0], tt[1], c)
		}
		if c := compareVersions(tt[1], tt[0]); c <= 0 {
			t.Errorf("%s vs %s: %d, want newer", tt[1], tt[0], c)
		}
	}
	for _, v := range []string{"1.2.3-r0", "1.2_rc1-r2", "1.02-r0"} {
		if c := compareVersions(v, v); c != 0 {
			t.Errorf("%s vs itself: %d", v, c)
		}
	}
	if compareVersions("1.02-r0", "1.2-r0") != 0 {
		t.Error("1.02 and 1.2 differ")
	}
}

// world is Alpine's mirror, its secdb and GitHub, as httptest serves them:
// apks by path under /alpine, the release's assets by name, with their
// digests, and the issues open.
type world struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	alpine  map[string][]byte // by path under /alpine/v3.22/
	secdb   map[string][]byte // by repository
	release map[string][]byte // the release's assets; nil if there is no release
	badDig  map[string]bool   // assets whose digest is not their bytes'
	issues  []ghIssue
	created []map[string]any // issues created
	made    []map[string]any // releases created
	upped   []string         // assets uploaded
	auth    []string         // Authorization of each GitHub request
	hits    []string         // every path asked
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, alpine: map[string][]byte{}, secdb: map[string][]byte{}, badDig: map[string]bool{}}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *world) serve(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := r.URL.Path
	w.hits = append(w.hits, r.Method+" "+p)
	switch {
	case strings.HasPrefix(p, "/alpine/v3.22/"):
		b, ok := w.alpine[strings.TrimPrefix(p, "/alpine/v3.22/")]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(b)
	case strings.HasPrefix(p, "/secdb/v3.22/"):
		b, ok := w.secdb[strings.TrimSuffix(strings.TrimPrefix(p, "/secdb/v3.22/"), ".json")]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(b)
	case strings.HasPrefix(p, "/rel/"):
		// The release's public URL, which a private repository's 404s.
		b, ok := w.release[strings.TrimPrefix(p, "/rel/")]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(b)
	case strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/up/"):
		w.auth = append(w.auth, r.Header.Get("Authorization"))
		w.github(rw, r)
	default:
		http.NotFound(rw, r)
	}
}

func (w *world) github(rw http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	asJSON := func(v any) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && p == "/api/repos/o/r/releases/tags/converter-deps":
		if w.release == nil {
			http.NotFound(rw, r)
			return
		}
		rel := ghRelease{ID: 7}
		names := make([]string, 0, len(w.release))
		for name := range w.release {
			names = append(names, name)
		}
		slices.Sort(names)
		for i, name := range names {
			dig := "sha256:" + sumOf(w.release[name])
			if w.badDig[name] {
				dig = "sha256:" + strings.Repeat("0", 64)
			}
			rel.Assets = append(rel.Assets, ghAsset{ID: int64(i), Name: name, URL: w.srv.URL + "/api/repos/o/r/releases/assets/" + name, Digest: dig})
		}
		asJSON(rel)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/repos/o/r/releases/assets/"):
		if r.Header.Get("Accept") != "application/octet-stream" {
			asJSON(map[string]string{"name": "the asset's metadata"})
			return
		}
		b, ok := w.release[strings.TrimPrefix(p, "/api/repos/o/r/releases/assets/")]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(b)
	case r.Method == http.MethodPost && p == "/api/repos/o/r/releases":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.made = append(w.made, body)
		w.release = map[string][]byte{}
		rw.WriteHeader(http.StatusCreated)
		asJSON(ghRelease{ID: 7})
	case r.Method == http.MethodPost && p == "/up/repos/o/r/releases/7/assets":
		b, _ := io.ReadAll(r.Body)
		name := r.URL.Query().Get("name")
		w.release[name] = b
		w.upped = append(w.upped, name)
		rw.WriteHeader(http.StatusCreated)
		asJSON(ghAsset{Name: name})
	case r.Method == http.MethodGet && p == "/api/repos/o/r/issues":
		if r.URL.Query().Get("state") != "open" {
			http.Error(rw, "want state=open", http.StatusBadRequest)
			return
		}
		asJSON(w.issues)
	case r.Method == http.MethodPost && p == "/api/repos/o/r/issues":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.created = append(w.created, body)
		rw.WriteHeader(http.StatusCreated)
		asJSON(ghIssue{Number: 42, HTMLURL: "https://github.com/o/r/issues/42"})
	default:
		http.Error(rw, "no such call in the test: "+r.Method+" "+p, http.StatusTeapot)
	}
}

// tool is converterdeps on w, with pins of the packages given, each
// pinned at the sha256 of the bytes given, and the token given.
func (w *world) tool(token string, pkgs map[string][]byte) (*tool, *bytes.Buffer) {
	var log bytes.Buffer
	p := pins{alpine: "v3.22", mirror: w.srv.URL + "/alpine",
		release: release{url: w.srv.URL + "/rel", owner: "o", repo: "r", tag: "converter-deps"}}
	names := make([]string, 0, len(pkgs))
	for name := range pkgs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, s := range names {
		repo, rest, _ := strings.Cut(s, "/")
		i := strings.LastIndex(rest, "-r")
		j := strings.LastIndex(rest[:i], "-")
		p.packages = append(p.packages, pkg{repo: repo, name: rest[:j], version: rest[j+1:], sha256: sumOf(pkgs[s])})
	}
	return &tool{pins: p, hc: w.srv.Client(), secdb: w.srv.URL + "/secdb", log: &log,
		gh: &github{api: w.srv.URL + "/api", uploads: w.srv.URL + "/up", token: token, hc: w.srv.Client()}}, &log
}

// TestFetchTakesTheReleaseFirstThenAlpine: a package the release holds,
// right, comes from it; one it lacks, or holds wrong, from Alpine; one
// Alpine holds wrong too is an error, and leaves no file; one already
// there and right is not downloaded again.
func TestFetchTakesTheReleaseFirstThenAlpine(t *testing.T) {
	for _, token := range []string{"", "gh-token"} {
		t.Run(fmt.Sprintf("token %t", token != ""), func(t *testing.T) {
			w := newWorld(t)
			a, b, c := []byte("apk a"), []byte("apk b"), []byte("apk c")
			w.release = map[string][]byte{"a-1.0-r0.apk": a, "c-3.0-r0.apk": []byte("swapped")}
			w.alpine = map[string][]byte{"main/x86_64/a-1.0-r0.apk": []byte("not a"), "main/x86_64/b-2.0-r0.apk": b, "community/x86_64/c-3.0-r0.apk": c}
			tl, log := w.tool(token, map[string][]byte{"main/a-1.0-r0": a, "main/b-2.0-r0": b, "community/c-3.0-r0": c})
			dir := t.TempDir()
			if err := tl.fetch(dir); err != nil {
				t.Fatalf("%v\n%s", err, log)
			}
			for name, want := range map[string][]byte{"a-1.0-r0.apk": a, "b-2.0-r0.apk": b, "c-3.0-r0.apk": c} {
				if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s: %q %v, want %q", name, got, err, want)
				}
			}
			for _, says := range []string{"main/a-1.0-r0 from the release", "main/b-2.0-r0 from Alpine", "community/c-3.0-r0 from Alpine", "has sha256"} {
				if !strings.Contains(log.String(), says) {
					t.Errorf("the log does not say %q:\n%s", says, log)
				}
			}
			if slices.Contains(w.hits, "GET /alpine/v3.22/main/x86_64/a-1.0-r0.apk") {
				t.Error("a package the release holds was downloaded from Alpine")
			}
			for _, a := range w.auth {
				if a != "Bearer "+token {
					t.Errorf("GitHub asked with %q", a)
				}
			}
			if token == "" && len(w.auth) != 0 {
				t.Errorf("without a token, the API was asked %d times", len(w.auth))
			}

			// Again: everything there and right, nothing downloaded.
			hits := len(w.hits)
			if err := tl.fetch(dir); err != nil {
				t.Fatal(err)
			}
			if len(w.hits) != hits {
				t.Errorf("fetching again asked %v", w.hits[hits:])
			}
			// A file there gone wrong is downloaded again.
			if err := os.WriteFile(filepath.Join(dir, "b-2.0-r0.apk"), []byte("rotten"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tl.fetch(dir); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "b-2.0-r0.apk")); !bytes.Equal(got, b) {
				t.Errorf("a file gone wrong stays %q", got)
			}
		})
	}

	w := newWorld(t)
	w.alpine = map[string][]byte{"main/x86_64/d-1.0-r0.apk": []byte("swapped")}
	tl, _ := w.tool("", map[string][]byte{"main/d-1.0-r0": []byte("apk d")})
	dir := t.TempDir()
	if err := tl.fetch(dir); err == nil || !strings.Contains(err.Error(), "from Alpine has sha256") {
		t.Errorf("a package wrong on Alpine and not in the release: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// TestMirrorUploadsWhatTheReleaseLacks: with a token, the release is made
// if there is none, never marked latest, and given each package it lacks,
// downloaded from Alpine and checked; a package Alpine holds wrong is not
// uploaded, and an asset whose digest is not its pin's stops it.
func TestMirrorUploadsWhatTheReleaseLacks(t *testing.T) {
	a, b := []byte("apk a"), []byte("apk b")
	w := newWorld(t)
	w.alpine = map[string][]byte{"main/x86_64/a-1.0-r0.apk": a, "main/x86_64/b-2.0-r0.apk": b}
	tl, log := w.tool("gh-token", map[string][]byte{"main/a-1.0-r0": a, "main/b-2.0-r0": b})
	if err := tl.mirror(); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if len(w.made) != 1 || w.made[0]["tag_name"] != "converter-deps" || w.made[0]["make_latest"] != "false" || w.made[0]["prerelease"] != true {
		t.Errorf("releases made: %v", w.made)
	}
	if !slices.Equal(w.upped, []string{"a-1.0-r0.apk", "b-2.0-r0.apk"}) || !bytes.Equal(w.release["a-1.0-r0.apk"], a) {
		t.Errorf("uploaded %v", w.upped)
	}
	for _, auth := range w.auth {
		if auth != "Bearer gh-token" {
			t.Errorf("GitHub asked with %q", auth)
		}
	}

	// Run again, one more pinned: only that one goes up.
	c := []byte("apk c")
	w.alpine["community/x86_64/c-3.0-r0.apk"] = c
	w.upped = nil
	tl, _ = w.tool("gh-token", map[string][]byte{"main/a-1.0-r0": a, "main/b-2.0-r0": b, "community/c-3.0-r0": c})
	if err := tl.mirror(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.upped, []string{"c-3.0-r0.apk"}) || len(w.made) != 1 {
		t.Errorf("uploaded %v, releases made %d", w.upped, len(w.made))
	}

	// Wrong on Alpine: nothing uploaded.
	w.upped = nil
	w.alpine["main/x86_64/d-1.0-r0.apk"] = []byte("swapped")
	tl, _ = w.tool("gh-token", map[string][]byte{"main/a-1.0-r0": a, "main/d-1.0-r0": []byte("apk d")})
	if err := tl.mirror(); err == nil || len(w.upped) != 0 {
		t.Errorf("a package wrong on Alpine: %v, uploaded %v", err, w.upped)
	}
	// An asset not of its pin.
	w.badDig["a-1.0-r0.apk"] = true
	tl, _ = w.tool("gh-token", map[string][]byte{"main/a-1.0-r0": a})
	if err := tl.mirror(); err == nil || !strings.Contains(err.Error(), "delete the asset") {
		t.Errorf("an asset of another digest: %v", err)
	}
	tl, _ = w.tool("", map[string][]byte{"main/a-1.0-r0": a})
	if err := tl.mirror(); err == nil {
		t.Error("mirrored without a token")
	}
}

// index is an APKINDEX.tar.gz holding the fixture file, as Alpine serves
// one: a signature's stream, then the index's.
func index(t *testing.T, fixture string) []byte {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, files := range []map[string][]byte{
		{".SIGN.RSA.alpine-devel@lists.alpinelinux.org-6165ee59.rsa.pub": []byte("signature")},
		{"DESCRIPTION": []byte("v3.22"), "APKINDEX": text},
	} {
		zw := gzip.NewWriter(&out)
		tw := tar.NewWriter(zw)
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name]))}); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write(files[name])
		}
		// A signature's stream ends without the tar's end, as apk's does.
		if len(names) == 1 {
			_ = tw.Flush()
		} else {
			_ = tw.Close()
		}
		_ = zw.Close()
	}
	return out.Bytes()
}

func (w *world) alpineIndex(t *testing.T) {
	w.alpine["main/x86_64/APKINDEX.tar.gz"] = index(t, "APKINDEX-main")
	w.alpine["community/x86_64/APKINDEX.tar.gz"] = index(t, "APKINDEX-community")
	for _, r := range []string{"main", "community"} {
		b, err := os.ReadFile(filepath.Join("testdata", "secdb-"+r+".json"))
		if err != nil {
			t.Fatal(err)
		}
		w.secdb[r] = b
	}
}

// fixturePins are pins of the fixtures' packages: glib, qemu-img and
// zstd-libs older than the index's, libblkid its newest, and one the
// index no longer has.
func fixturePins() map[string][]byte {
	return map[string][]byte{
		"main/glib-2.84.4-r0": nil, "main/libblkid-2.41.6-r1": nil, "main/zstd-libs-1.5.7-r0": nil,
		"community/qemu-img-10.0.0-r1": nil, "main/gone-1.0-r0": nil,
	}
}

// TestUpdatesSayWhatIsNewerAndWhatItFixes: the pins older than Alpine's
// index are listed, old and new, each with the CVEs its origin fixed after
// the pin up to the newer version; the issue is opened labelled security,
// once: a run finding the same, its issue open, opens none.
func TestUpdatesSayWhatIsNewerAndWhatItFixes(t *testing.T) {
	w := newWorld(t)
	w.alpineIndex(t)
	tl, log := w.tool("gh-token", fixturePins())
	var out bytes.Buffer
	if err := tl.updates(&out, true, "o", "r"); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	got := out.String()
	wantTitle := "converter: newer Alpine v3.22 packages: glib 2.84.4-r2, qemu-img 10.0.3-r0, zstd-libs 1.5.7-r1"
	if !strings.HasPrefix(got, wantTitle+"\n") {
		t.Errorf("title of\n%s\nwant %s", got, wantTitle)
	}
	for _, line := range []string{
		"| main/glib | 2.84.4-r0 | 2.84.4-r2 | CVE-2025-0001, CVE-2025-0002 |",
		"| community/qemu-img | 10.0.0-r1 | 10.0.3-r0 | CVE-2025-2222, CVE-2025-2223 |",
		"| main/zstd-libs | 1.5.7-r0 | 1.5.7-r1 | none listed |",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("no line %q in\n%s", line, got)
		}
	}
	for _, not := range []string{"libblkid", "CVE-2025-0003", "CVE-2024-9999", "CVE-2025-0000", "CVE-2024-1234", "GHSA"} {
		if strings.Contains(got, not) {
			t.Errorf("%s in\n%s", not, got)
		}
	}
	if !strings.Contains(log.String(), "main/gone-1.0-r0: not in Alpine v3.22's main index") {
		t.Errorf("the log:\n%s", log)
	}
	if len(w.created) != 1 || w.created[0]["title"] != wantTitle || fmt.Sprint(w.created[0]["labels"]) != "[security]" ||
		!strings.Contains(fmt.Sprint(w.created[0]["body"]), "docs/runbook/converter.md") {
		t.Fatalf("issues created: %v", w.created)
	}

	// The same found, its issue open: none opened. A pull request of the
	// title does not count, nor does another issue.
	w.issues = []ghIssue{{Number: 3, Title: "something else"}, {Number: 4, Title: wantTitle, PullRequest: json.RawMessage(`{}`)}}
	if err := tl.updates(io.Discard, true, "o", "r"); err != nil {
		t.Fatal(err)
	}
	if len(w.created) != 2 {
		t.Fatalf("with only a pull request of the title open, issues created %d, want another", len(w.created))
	}
	w.issues = append(w.issues, ghIssue{Number: 42, Title: wantTitle, HTMLURL: "https://github.com/o/r/issues/42"})
	log.Reset()
	if err := tl.updates(io.Discard, true, "o", "r"); err != nil {
		t.Fatal(err)
	}
	if len(w.created) != 2 || !strings.Contains(log.String(), "issue #42 says so already") {
		t.Errorf("with its issue open, issues created %d; log %s", len(w.created), log)
	}
	// Without -issue, nothing asked of GitHub.
	asked := len(w.auth)
	if err := tl.updates(io.Discard, false, "o", "r"); err != nil || len(w.auth) != asked {
		t.Errorf("without -issue: %v, GitHub asked %d times", err, len(w.auth)-asked)
	}
}

// TestUpdatesWithoutCVEsAreNoSecurityIssue: newer versions that fix no CVE
// open an issue of no label; pins at their newest open none.
func TestUpdatesWithoutCVEsAreNoSecurityIssue(t *testing.T) {
	w := newWorld(t)
	w.alpineIndex(t)
	tl, _ := w.tool("gh-token", map[string][]byte{"main/zstd-libs-1.5.7-r0": nil, "main/libblkid-2.41.6-r1": nil})
	if err := tl.updates(io.Discard, true, "o", "r"); err != nil {
		t.Fatal(err)
	}
	if len(w.created) != 1 || w.created[0]["labels"] != nil {
		t.Errorf("issues created %v, want one of no label", w.created)
	}
	tl, _ = w.tool("gh-token", map[string][]byte{"main/libblkid-2.41.6-r1": nil, "main/glib-2.84.4-r2": nil})
	var out bytes.Buffer
	if err := tl.updates(&out, true, "o", "r"); err != nil {
		t.Fatal(err)
	}
	if len(w.created) != 1 || !strings.Contains(out.String(), "newest") {
		t.Errorf("pins at their newest: %q, issues created %d", out.String(), len(w.created))
	}
}

// TestALongTitleIsCut: an issue's title stays within GitHub's bound,
// naming as many packages as fit and how many more.
func TestALongTitleIsCut(t *testing.T) {
	var ups []update
	for i := range 30 {
		ups = append(ups, update{pkg: pkg{repo: "main", name: fmt.Sprintf("library-number-%02d", i), version: "1.0-r0"}, newer: "1.0-r1"})
	}
	title, _, security := report("v3.22", ups)
	if len(title) > maxTitle || !strings.Contains(title, "library-number-00 1.0-r1") || !strings.HasSuffix(title, "more") || security {
		t.Errorf("title %q (%d), security %t", title, len(title), security)
	}
}
