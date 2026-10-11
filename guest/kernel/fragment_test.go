// Package kernel holds no code: its test keeps hangar.fragment, the
// committed config and docs/decisions/guest-kernel.md in step, without a
// kernel build.
package kernel

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

// readConfig reads CONFIG_ lines into a map; "# CONFIG_X is not set" is "n".
func readConfig(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	notSet := regexp.MustCompile(`^# (CONFIG_[A-Za-z0-9_]+) is not set$`)
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if m := notSet.FindStringSubmatch(line); m != nil {
			out[m[1]] = "n"
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "CONFIG_") {
			t.Fatalf("%s: cannot parse %q", path, line)
		}
		out[k] = v
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every option the decision record names is one the fragment sets: what
// the guest relies on is never a Kconfig default.
func TestTheDecisionRecordNamesOnlyFragmentOptions(t *testing.T) {
	fragment := readConfig(t, "hangar.fragment")
	doc, err := os.ReadFile("../../docs/decisions/guest-kernel.md")
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`CONFIG_[A-Z0-9_]*[A-Z0-9]`).FindAllString(string(doc), -1)
	if len(named) < 50 {
		t.Fatalf("the decision record names only %d options; is it the right file?", len(named))
	}
	for _, opt := range named {
		if _, ok := fragment[opt]; !ok {
			t.Errorf("docs/decisions/guest-kernel.md names %s, which hangar.fragment does not set", opt)
		}
	}
}

// offHardening is what the decision record's hardening section names but
// keeps off, each with its reason there.
var offHardening = map[string]bool{"CONFIG_MITIGATION_SLS": true}

// Every hardening option the decision record names is on in the fragment,
// but the few it says stay off.
func TestTheHardeningIsOn(t *testing.T) {
	fragment := readConfig(t, "hangar.fragment")
	doc, err := os.ReadFile("../../docs/decisions/guest-kernel.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "\n## Hardening")
	if !ok {
		t.Fatal("the decision record has no hardening section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	named := regexp.MustCompile(`CONFIG_[A-Z0-9_]*[A-Z0-9]`).FindAllString(section, -1)
	if len(named) < 40 {
		t.Fatalf("the hardening section names only %d options", len(named))
	}
	for _, opt := range named {
		want := "y"
		if offHardening[opt] {
			want = "n"
		}
		if got := fragment[opt]; got != want {
			t.Errorf("%s=%s in hangar.fragment; the decision record has it %s", opt, got, map[string]string{"y": "on", "n": "off"}[want])
		}
	}
}

// The committed config is what the fragment asks for; build.sh checks the
// same against a fresh resolution, this checks the committed file in any
// workspace.
func TestTheCommittedConfigHonoursTheFragment(t *testing.T) {
	fragment := readConfig(t, "hangar.fragment")
	config := readConfig(t, "config")
	for opt, want := range fragment {
		got, ok := config[opt]
		switch {
		case want == "n" && ok && got != "n":
			t.Errorf("%s=n in the fragment, %s in config", opt, got)
		case want != "n" && got != want:
			t.Errorf("%s=%s in the fragment, %q in config", opt, want, got)
		}
	}
}
