package customdomain_test

import (
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/customdomain"
)

func TestCheckDomain(t *testing.T) {
	for _, d := range []string{"example.com", "b.example.com", "example.co.uk", "xn--bcher-kva.example", "a-b.example.org"} {
		if err := customdomain.CheckDomain(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	for _, d := range []string{
		"", "com", "co.uk", "github.io", "Example.com", "example.com.", ".example.com", "a..example.com",
		"-a.example.com", "a-.example.com", "*.example.com", "a_b.example.com", "localhost",
		strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 127) + "com",
	} {
		if err := customdomain.CheckDomain(d); err == nil {
			t.Errorf("%q was taken", d)
		}
	}
}

func TestOverlap(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "b.example.com", true},
		{"a.b.example.com", "example.com", true},
		{"example.com", "myexample.com", false},
		{"example.com", "example.org", false},
	} {
		if got := customdomain.Overlap(tt.a, tt.b); got != tt.want {
			t.Errorf("Overlap(%s, %s) = %v", tt.a, tt.b, got)
		}
	}
}

func TestMatch(t *testing.T) {
	domains := []string{"example.com", "b.example.com", "other.org"}
	for _, tt := range []struct{ host, want string }{
		{"example.com", "example.com"},
		{"app.example.com", "example.com"},
		{"b.example.com", "b.example.com"},
		{"x.b.example.com", "b.example.com"},
		{"x.y.example.com", ""},
		{"myexample.com", ""},
		{"other.org", "other.org"},
	} {
		got, ok := customdomain.Match(tt.host, domains)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("Match(%s) = %q, %v; want %q", tt.host, got, ok, tt.want)
		}
	}
}

func TestChallengeTargetIsTheUidsAlone(t *testing.T) {
	a := customdomain.ChallengeTarget("uid-1", "acme.hangarlab.dev")
	if a != customdomain.ChallengeTarget("uid-1", "acme.hangarlab.dev") {
		t.Fatal("not stable")
	}
	if a == customdomain.ChallengeTarget("uid-2", "acme.hangarlab.dev") {
		t.Fatal("two uids, one target")
	}
	label, zone, _ := strings.Cut(a, ".")
	if zone != "acme.hangarlab.dev" || len(label) != 24 || customdomain.CheckHostname(a) != nil {
		t.Fatalf("target %q", a)
	}
}
