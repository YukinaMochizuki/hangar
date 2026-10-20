package cli_test

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// catalogueFile is the platform's image catalogue, from this package.
const catalogueFile = "../../deploy/catalog/images.yaml"

// imageName is a catalogue name: no dots, as ubuntu-24-04.
var imageName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// dated is a date in a file's path, such as release-20260926.
var dated = regexp.MustCompile(`20[0-9]{6}`)

// TestTheImageCatalogueApplies: deploy/catalog/images.yaml applies as
// docs/runbook/vm-images.md has it applied, as root through the admin
// socket, and again changing nothing. Each image is pinned to a dated file,
// not a path that moves, and says what it is and whom to log in as;
// ubuntu-24-04, which disks name, is there.
func TestTheImageCatalogueApplies(t *testing.T) {
	w := newWorld(t)
	w.must("", "config", "set-context", "local", "--endpoint", "unix://"+w.admin, "--auth", "local", "--use")
	first := w.must("", "apply", "-f", catalogueFile)
	t.Logf("apply:\n%s", first)

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				URL         string `json:"url"`
				OS          string `json:"os"`
				Version     string `json:"version"`
				DefaultUser string `json:"defaultUser"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(w.must("", "get", "images", "-o", "json")), &list); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, img := range list.Items {
		n, s := img.Metadata.Name, img.Spec
		names[n] = true
		if !strings.Contains(first, "image/"+n+" created\n") {
			t.Errorf("%s: the apply printed no creation", n)
		}
		if !imageName.MatchString(n) {
			t.Errorf("%s: not a name such as ubuntu-24-04", n)
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		for _, seg := range strings.FieldsFunc(strings.ToLower(u.Path), func(r rune) bool { return r == '/' || r == '-' || r == '.' || r == '_' }) {
			switch seg {
			case "latest", "current", "daily":
				t.Errorf("%s: %s is a path that moves", n, s.URL)
			}
		}
		if !dated.MatchString(u.Path) {
			t.Errorf("%s: %s names no dated file", n, s.URL)
		}
		if s.OS == "" || s.Version == "" || s.DefaultUser == "" {
			t.Errorf("%s: os %q, version %q, defaultUser %q: each is needed", n, s.OS, s.Version, s.DefaultUser)
		}
	}
	if !names["ubuntu-24-04"] {
		t.Errorf("no ubuntu-24-04, which disks name, in %v", names)
	}
	if len(list.Items) != strings.Count(first, "\n") {
		t.Errorf("applied %d objects, listed %d images", strings.Count(first, "\n"), len(list.Items))
	}

	// Again: the same objects, at the same generation.
	before := w.must("", "get", "images", "-o", "yaml")
	if n := strings.Count(generations(before), "generation:"); n != len(list.Items) {
		t.Fatalf("%d generations for %d images:\n%s", n, len(list.Items), before)
	}
	if out := w.must("", "apply", "-f", catalogueFile); strings.Contains(out, "created") {
		t.Errorf("the second apply created %q", out)
	}
	if after := w.must("", "get", "images", "-o", "yaml"); generations(after) != generations(before) {
		t.Errorf("the second apply changed generations: %s, then %s", generations(before), generations(after))
	}
}

// generations is every generation line of a YAML listing, in order.
func generations(yaml string) string {
	var out []string
	for _, line := range strings.Split(yaml, "\n") {
		if strings.Contains(line, "generation:") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return strings.Join(out, ",")
}
