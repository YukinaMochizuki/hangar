package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// indexEntry is one package of an APKINDEX: its version, and the package
// it is built from (its origin), which secdb names.
type indexEntry struct {
	version, origin string
}

// parseIndex reads an APKINDEX.tar.gz, as Alpine's mirror serves one for a
// repository, into its packages by name. The archive is gzip streams
// concatenated, a signature and then the index; the APKINDEX file holds
// records of KEY:value lines, a blank line between them.
func parseIndex(r io.Reader) (map[string]indexEntry, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("APKINDEX: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("APKINDEX: no APKINDEX file in the archive")
		}
		if err != nil {
			return nil, fmt.Errorf("APKINDEX: %w", err)
		}
		if h.Name == "APKINDEX" {
			return parseIndexFile(tr)
		}
	}
}

func parseIndexFile(r io.Reader) (map[string]indexEntry, error) {
	out := map[string]indexEntry{}
	var name string
	var e indexEntry
	flush := func() {
		if name != "" {
			if old, ok := out[name]; !ok || compareVersions(e.version, old.version) > 0 {
				out[name] = e
			}
		}
		name, e = "", indexEntry{}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "P":
			name = val
		case "V":
			e.version = val
		case "o":
			e.origin = val
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("APKINDEX: %w", err)
	}
	return out, nil
}

// secdb is Alpine's security database for a release's repository: for each
// origin package, the versions that fixed CVEs, and which.
type secdb map[string]map[string][]string

func parseSecdb(r io.Reader) (secdb, error) {
	var doc struct {
		Packages []struct {
			Pkg struct {
				Name     string              `json:"name"`
				Secfixes map[string][]string `json:"secfixes"`
			} `json:"pkg"`
		} `json:"packages"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("secdb: %w", err)
	}
	out := secdb{}
	for _, p := range doc.Packages {
		if out[p.Pkg.Name] == nil {
			out[p.Pkg.Name] = map[string][]string{}
		}
		for v, cves := range p.Pkg.Secfixes {
			out[p.Pkg.Name][v] = append(out[p.Pkg.Name][v], cves...)
		}
	}
	return out, nil
}

// fixedBetween is the CVEs of origin that versions after from and up to to
// fix, in the order of those versions.
func (db secdb) fixedBetween(origin, from, to string) []string {
	type fix struct {
		version string
		cves    []string
	}
	var fixes []fix
	for v, cves := range db[origin] {
		if compareVersions(v, from) > 0 && compareVersions(v, to) <= 0 {
			fixes = append(fixes, fix{v, cves})
		}
	}
	slices.SortFunc(fixes, func(a, b fix) int { return compareVersions(a.version, b.version) })
	var out []string
	seen := map[string]bool{}
	for _, f := range fixes {
		for _, c := range f.cves {
			// secdb lists some with another id after, as "CVE-... XSA-...".
			words := strings.Fields(c)
			if len(words) == 0 || seen[words[0]] {
				continue
			}
			seen[words[0]] = true
			out = append(out, words[0])
		}
	}
	return out
}

// Suffix ranks, as apk orders them: the pre-releases before the release,
// which has none, and the post-releases after it.
var suffixRank = map[string]int{"alpha": -4, "beta": -3, "pre": -2, "rc": -1, "cvs": 1, "svn": 2, "git": 3, "hg": 4, "p": 5}

type apkVersion struct {
	nums     []string // the dotted numbers, as written
	letter   byte
	suffixes [][2]int64 // rank, number
	rev      int64
}

// parseVersion reads an Alpine version, such as 1.2.3a_rc1_p2-r4. Anything
// it cannot read sorts as a version of no numbers.
func parseVersion(s string) apkVersion {
	var v apkVersion
	if i := strings.LastIndex(s, "-r"); i >= 0 {
		if r, err := strconv.ParseInt(s[i+2:], 10, 64); err == nil {
			v.rev, s = r, s[:i]
		}
	}
	main, rest, _ := strings.Cut(s, "_")
	if n := len(main); n > 0 && main[n-1] >= 'a' && main[n-1] <= 'z' {
		v.letter, main = main[n-1], main[:n-1]
	}
	if main != "" {
		v.nums = strings.Split(main, ".")
	}
	for rest != "" {
		var part string
		part, rest, _ = strings.Cut(rest, "_")
		word := strings.TrimRight(part, "0123456789")
		num, _ := strconv.ParseInt(part[len(word):], 10, 64)
		rank, ok := suffixRank[word]
		if !ok {
			continue
		}
		v.suffixes = append(v.suffixes, [2]int64{int64(rank), num})
	}
	return v
}

// compareNum compares two dotted numbers as numbers, whatever their
// length.
func compareNum(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// compareVersions orders two Alpine versions, as apk does for the versions
// Alpine's packages have: negative if a is older than b, 0 if the same,
// positive if newer.
func compareVersions(a, b string) int {
	va, vb := parseVersion(a), parseVersion(b)
	for i := 0; i < len(va.nums) && i < len(vb.nums); i++ {
		if c := compareNum(va.nums[i], vb.nums[i]); c != 0 {
			return c
		}
	}
	if len(va.nums) != len(vb.nums) {
		if len(va.nums) < len(vb.nums) {
			return -1
		}
		return 1
	}
	if va.letter != vb.letter {
		if va.letter < vb.letter {
			return -1
		}
		return 1
	}
	for i := 0; i < len(va.suffixes) || i < len(vb.suffixes); i++ {
		var sa, sb [2]int64
		if i < len(va.suffixes) {
			sa = va.suffixes[i]
		}
		if i < len(vb.suffixes) {
			sb = vb.suffixes[i]
		}
		for k := range 2 {
			if sa[k] != sb[k] {
				if sa[k] < sb[k] {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case va.rev < vb.rev:
		return -1
	case va.rev > vb.rev:
		return 1
	}
	return 0
}
