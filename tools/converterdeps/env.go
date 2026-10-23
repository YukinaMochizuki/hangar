package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// pins is what guest/converter/converter.env says: the Alpine release and
// mirror the packages come from, the GitHub release that mirrors them, and
// the packages, each with its sha256.
type pins struct {
	alpine   string // such as v3.22
	mirror   string // Alpine's, such as https://dl-cdn.alpinelinux.org/alpine
	release  release
	packages []pkg
}

// pkg is one pinned package: community/qemu-img-10.0.0-r1 and its sha256.
type pkg struct {
	repo, name, version, sha256 string
}

// file is the package's file name, as Alpine and the release both serve it.
func (p pkg) file() string { return p.name + "-" + p.version + ".apk" }

func (p pkg) String() string { return p.repo + "/" + p.name + "-" + p.version }

// release is a GitHub release whose assets mirror the packages, named as
// the HANGAR_CONVERTER_RELEASE URL scheme says: a file's URL is the scheme,
// a slash, and the file's name.
type release struct {
	url              string
	owner, repo, tag string
}

var releaseURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/releases/download/([A-Za-z0-9_.-]+)$`)

func parseRelease(u string) (release, error) {
	m := releaseURL.FindStringSubmatch(u)
	if m == nil {
		return release{}, fmt.Errorf("HANGAR_CONVERTER_RELEASE %q is not https://github.com/OWNER/REPO/releases/download/TAG", u)
	}
	return release{url: u, owner: m[1], repo: m[2], tag: m[3]}, nil
}

var (
	pkgLine = regexp.MustCompile(`^(main|community)/([A-Za-z0-9._+-]+)-([0-9][A-Za-z0-9._+]*-r[0-9]+) ([0-9a-f]{64})$`)
	envLine = regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)=(.*)$`)
)

// parseEnv reads converter.env: NAME=value lines, a value in double quotes
// running over lines; comments and blank lines are skipped. It takes no
// more of the shell than the file uses.
func parseEnv(r io.Reader) (pins, error) {
	vars := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := envLine.FindStringSubmatch(line)
		if m == nil {
			return pins{}, fmt.Errorf("converter.env: %q is not NAME=value", line)
		}
		name, val := m[1], m[2]
		if strings.HasPrefix(val, `"`) {
			val = val[1:]
			for !strings.HasSuffix(val, `"`) {
				if !sc.Scan() {
					return pins{}, fmt.Errorf("converter.env: %s has no closing quote", name)
				}
				val += "\n" + sc.Text()
			}
			val = strings.TrimSuffix(val, `"`)
		}
		vars[name] = val
	}
	if err := sc.Err(); err != nil {
		return pins{}, err
	}
	p := pins{alpine: vars["HANGAR_CONVERTER_ALPINE"], mirror: strings.TrimSuffix(vars["HANGAR_CONVERTER_MIRROR"], "/")}
	if p.alpine == "" || p.mirror == "" {
		return pins{}, fmt.Errorf("converter.env: HANGAR_CONVERTER_ALPINE and HANGAR_CONVERTER_MIRROR are required")
	}
	var err error
	if p.release, err = parseRelease(vars["HANGAR_CONVERTER_RELEASE"]); err != nil {
		return pins{}, fmt.Errorf("converter.env: %w", err)
	}
	for _, line := range strings.Split(vars["HANGAR_CONVERTER_PACKAGES"], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := pkgLine.FindStringSubmatch(line)
		if m == nil {
			return pins{}, fmt.Errorf("converter.env: %q is not REPO/NAME-VERSION SHA256", line)
		}
		p.packages = append(p.packages, pkg{repo: m[1], name: m[2], version: m[3], sha256: m[4]})
	}
	if len(p.packages) == 0 {
		return pins{}, fmt.Errorf("converter.env: HANGAR_CONVERTER_PACKAGES pins nothing")
	}
	return p, nil
}

// alpineURL is where Alpine's mirror serves the package.
func (p pins) alpineURL(k pkg) string {
	return p.mirror + "/" + p.alpine + "/" + k.repo + "/x86_64/" + k.file()
}
