// Command converterdeps keeps the Alpine packages the converter initramfs
// is built from (guest/converter/converter.env, docs/runbook/converter.md):
//
//   - fetch downloads each pinned package into a directory, first from the
//     GitHub release that mirrors them, then from Alpine, and checks it
//     against its sha256; guest/converter/build.sh runs it. With
//     GITHUB_TOKEN set it reads the release through the API, which a
//     private repository's needs; without, by its public URL.
//   - mirror uploads to that release, with GITHUB_TOKEN, each pinned
//     package it lacks, downloaded from Alpine and checked; the
//     converter-mirror workflow runs it.
//   - updates compares the pins with Alpine's index of the release, and
//     says which have newer versions and the CVEs those fix, from Alpine's
//     secdb; with -issue it opens one GitHub issue saying so, unless an
//     open one of the same title is there; the converter-updates workflow
//     runs it.
//
// Its only credential is GITHUB_TOKEN.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "converterdeps:", err)
		os.Exit(1)
	}
}

// tool is a run's configuration: where Alpine, its secdb and GitHub are,
// which tests point elsewhere.
type tool struct {
	pins  pins
	gh    *github
	hc    *http.Client
	secdb string // https://secdb.alpinelinux.org
	log   io.Writer
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: converterdeps fetch|mirror|updates [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	env := fs.String("env", "guest/converter/converter.env", "the pins")
	dir := fs.String("dir", ".cache/converter", "for fetch, where the packages go")
	issue := fs.Bool("issue", false, "for updates, open a GitHub issue of what is newer")
	repo := fs.String("repo", os.Getenv("GITHUB_REPOSITORY"), "for updates -issue, the OWNER/REPO the issue goes to; empty is the release's")
	api := fs.String("github-api", "https://api.github.com", "GitHub's API")
	uploads := fs.String("github-uploads", "https://uploads.github.com", "GitHub's API for uploads")
	secdb := fs.String("secdb", "https://secdb.alpinelinux.org", "Alpine's security database")
	mirror := fs.String("alpine", "", "Alpine's mirror; empty is converter.env's")
	prefix := fs.String("prefix", "HANGAR_CONVERTER", "what the names in -env start with")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	f, err := os.Open(*env)
	if err != nil {
		return err
	}
	p, err := parseEnv(f, *prefix)
	_ = f.Close()
	if err != nil {
		return err
	}
	if *mirror != "" {
		p.mirror = strings.TrimSuffix(*mirror, "/")
	}
	hc := &http.Client{Timeout: 5 * time.Minute}
	t := &tool{
		pins: p, hc: hc, secdb: strings.TrimSuffix(*secdb, "/"), log: stderr,
		gh: &github{api: strings.TrimSuffix(*api, "/"), uploads: strings.TrimSuffix(*uploads, "/"), token: os.Getenv("GITHUB_TOKEN"), hc: hc},
	}
	switch args[0] {
	case "fetch":
		return t.fetch(*dir)
	case "mirror":
		return t.mirror()
	case "updates":
		owner, name := p.release.owner, p.release.repo
		if *repo != "" {
			if owner, name, err = repoOf(*repo); err != nil {
				return err
			}
		}
		return t.updates(stdout, *issue, owner, name)
	}
	return fmt.Errorf("no command %q: fetch, mirror or updates", args[0])
}

func (t *tool) logf(format string, args ...any) { _, _ = fmt.Fprintf(t.log, format+"\n", args...) }

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// get downloads u, anonymously.
func (t *tool) get(u string) ([]byte, error) {
	resp, err := t.hc.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %w", u, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// fromAlpine downloads the package from Alpine's mirror, checked.
func (t *tool) fromAlpine(k pkg) ([]byte, error) {
	b, err := t.get(t.pins.alpineURL(k))
	if err != nil {
		return nil, fmt.Errorf("%s from Alpine: %w (gone from the mirror? bump converter.env, docs/runbook/converter.md)", k, err)
	}
	if got := sumOf(b); got != k.sha256 {
		return nil, fmt.Errorf("%s from Alpine has sha256 %s, not %s as converter.env pins it", k, got, k.sha256)
	}
	return b, nil
}

// mirrored finds the release's assets: through the API with a token, or
// none to look up without one, the release then read by its public URLs.
func (t *tool) mirrored() (map[string]ghAsset, error) {
	if t.gh.token == "" {
		return nil, nil
	}
	rel, err := t.gh.releaseByTag(t.pins.release)
	if err != nil {
		return nil, err
	}
	out := map[string]ghAsset{}
	for _, a := range rel.Assets {
		out[a.Name] = a
	}
	return out, nil
}

// fromRelease downloads the package from the release, checked.
func (t *tool) fromRelease(k pkg, assets map[string]ghAsset) ([]byte, error) {
	var b []byte
	var err error
	if t.gh.token != "" {
		a, ok := assets[k.file()]
		if !ok {
			return nil, fmt.Errorf("%s: no such asset: %w", k.file(), errNotFound)
		}
		b, err = t.gh.asset(a)
	} else {
		b, err = t.get(t.pins.release.url + "/" + k.file())
	}
	if err != nil {
		return nil, err
	}
	if got := sumOf(b); got != k.sha256 {
		return nil, fmt.Errorf("%s from the release has sha256 %s, not %s as converter.env pins it", k, got, k.sha256)
	}
	return b, nil
}

// fetch puts each pinned package in dir, as NAME-VERSION.apk, checked: one
// there already, if its sha256 is right; otherwise from the release, or,
// failing that, from Alpine.
func (t *tool) fetch(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var assets map[string]ghAsset
	assetsRead := false
	for _, k := range t.pins.packages {
		path := filepath.Join(dir, k.file())
		if b, err := os.ReadFile(path); err == nil && sumOf(b) == k.sha256 {
			continue
		}
		if !assetsRead {
			var err error
			if assets, err = t.mirrored(); err != nil {
				t.logf("the release %s: %v; downloading from Alpine", t.pins.release.url, err)
			}
			assetsRead = true
		}
		b, err := t.fromRelease(k, assets)
		if err == nil {
			t.logf("download: %s from the release", k)
		} else {
			t.logf("%s from the release: %v; from Alpine", k, err)
			if b, err = t.fromAlpine(k); err != nil {
				return err
			}
			t.logf("download: %s from Alpine", k)
		}
		if err := writeFile(path, b); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes b at path whole or not at all.
func writeFile(path string, b []byte) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mirror uploads to the release each pinned package it lacks, from Alpine,
// checked; it makes the release if there is none. An asset of a package's
// name whose digest is not the pin's is an error: the release is not
// changed over it.
func (t *tool) mirror() error {
	if t.gh.token == "" {
		return errors.New("mirror needs GITHUB_TOKEN, one that may write the repository's contents")
	}
	r := t.pins.release
	rel, err := t.gh.releaseByTag(r)
	if errors.Is(err, errNotFound) {
		t.logf("making the release %s", r.tag)
		rel, err = t.gh.createRelease(r)
	}
	if err != nil {
		return err
	}
	have := map[string]ghAsset{}
	for _, a := range rel.Assets {
		have[a.Name] = a
	}
	uploaded := 0
	for _, k := range t.pins.packages {
		if a, ok := have[k.file()]; ok {
			if a.Digest != "" && a.Digest != "sha256:"+k.sha256 {
				return fmt.Errorf("the release's %s has %s, not sha256:%s as converter.env pins it; delete the asset, and run this again", a.Name, a.Digest, k.sha256)
			}
			continue
		}
		b, err := t.fromAlpine(k)
		if err != nil {
			return err
		}
		if err := t.gh.upload(r, rel, k.file(), b); err != nil {
			return err
		}
		t.logf("uploaded %s", k.file())
		uploaded++
	}
	t.logf("the release %s holds all %d packages; %d uploaded now", r.tag, len(t.pins.packages), uploaded)
	return nil
}

// maxTitle is the longest issue title report makes.
const maxTitle = 200

// update is a pinned package Alpine has a newer version of.
type update struct {
	pkg    pkg
	newer  string
	origin string
	cves   []string
}

// updates writes which pins Alpine's index has newer versions of, and the
// CVEs fixed since, and with issue opens one in owner/repo saying so.
func (t *tool) updates(w io.Writer, issue bool, owner, repo string) error {
	index := map[string]map[string]indexEntry{}
	db := secdb{}
	for _, r := range []string{"main", "community"} {
		b, err := t.get(t.pins.mirror + "/" + t.pins.alpine + "/" + r + "/x86_64/APKINDEX.tar.gz")
		if err != nil {
			return err
		}
		if index[r], err = parseIndex(bytes.NewReader(b)); err != nil {
			return fmt.Errorf("%s: %w", r, err)
		}
		b, err = t.get(t.secdb + "/" + t.pins.alpine + "/" + r + ".json")
		if err != nil {
			return err
		}
		part, err := parseSecdb(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("%s: %w", r, err)
		}
		for name, fixes := range part {
			if db[name] == nil {
				db[name] = map[string][]string{}
			}
			for v, cves := range fixes {
				db[name][v] = append(db[name][v], cves...)
			}
		}
	}
	var ups []update
	for _, k := range t.pins.packages {
		e, ok := index[k.repo][k.name]
		if !ok {
			t.logf("%s: not in Alpine %s's %s index any more", k, t.pins.alpine, k.repo)
			continue
		}
		if compareVersions(e.version, k.version) <= 0 {
			continue
		}
		origin := e.origin
		if origin == "" {
			origin = k.name
		}
		ups = append(ups, update{pkg: k, newer: e.version, origin: origin, cves: db.fixedBetween(origin, k.version, e.version)})
	}
	if len(ups) == 0 {
		_, err := fmt.Fprintf(w, "every package the converter pins is Alpine %s's newest\n", t.pins.alpine)
		return err
	}
	title, body, security := report(t.pins.alpine, ups)
	if _, err := fmt.Fprintf(w, "%s\n\n%s", title, body); err != nil {
		return err
	}
	if !issue {
		return nil
	}
	if t.gh.token == "" {
		return errors.New("updates -issue needs GITHUB_TOKEN, one that may write the repository's issues")
	}
	if open, err := t.gh.openIssue(owner, repo, title); err != nil {
		return err
	} else if open != nil {
		t.logf("issue #%d says so already: %s", open.Number, open.HTMLURL)
		return nil
	}
	var labels []string
	if security {
		labels = []string{"security"}
	}
	is, err := t.gh.createIssue(owner, repo, title, body, labels)
	if err != nil {
		return err
	}
	t.logf("opened issue #%d: %s", is.Number, is.HTMLURL)
	return nil
}

// report is the issue of ups: its title, naming each package and its
// newer version, so a later run finding the same opens none and one
// finding more opens another; its body, a table and the steps; and
// whether any fixes a CVE.
func report(alpine string, ups []update) (title, body string, security bool) {
	slices.SortFunc(ups, func(a, b update) int { return strings.Compare(a.pkg.name, b.pkg.name) })
	var names []string
	var b strings.Builder
	fmt.Fprintf(&b, "Alpine %s has newer versions of packages the converter initramfs pins in `guest/converter/converter.env`:\n\n", alpine)
	b.WriteString("| Package | Pinned | Newer | CVEs fixed since |\n|---|---|---|---|\n")
	for _, u := range ups {
		names = append(names, u.pkg.name+" "+u.newer)
		cves := "none listed"
		if len(u.cves) > 0 {
			security = true
			cves = strings.Join(u.cves, ", ")
		}
		fmt.Fprintf(&b, "| %s/%s | %s | %s | %s |\n", u.pkg.repo, u.pkg.name, u.pkg.version, u.newer, cves)
	}
	b.WriteString("\nThe CVEs are those Alpine's secdb lists for the package's origin")
	b.WriteString(" (https://secdb.alpinelinux.org), fixed after the pinned version up to the newer.\n\n")
	b.WriteString("To take them, follow docs/runbook/converter.md: bump the pins and their sha256, resolve the")
	b.WriteString(" dependencies again, merge (the converter-mirror workflow mirrors the new packages), run the")
	b.WriteString(" converter's L4 tests on a testbed, and release.\n")
	title = fmt.Sprintf("converter: newer Alpine %s packages: %s", alpine, strings.Join(names, ", "))
	// GitHub takes titles of up to 256 characters.
	for n := len(names) - 1; len(title) > maxTitle && n > 0; n-- {
		title = fmt.Sprintf("converter: newer Alpine %s packages: %s and %d more", alpine, strings.Join(names[:n], ", "), len(names)-n)
	}
	return title, b.String(), security
}
