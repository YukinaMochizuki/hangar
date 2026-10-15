package edge_test

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/edge"
)

func TestHostname(t *testing.T) {
	for in, want := range map[string]string{
		"web-demo.hangarlab.dev":       "web-demo.hangarlab.dev",
		"Web-Demo.HangarLab.dev":       "web-demo.hangarlab.dev",
		"web-demo.hangarlab.dev.":      "web-demo.hangarlab.dev",
		"web-demo.hangarlab.dev:443":   "web-demo.hangarlab.dev",
		"WEB-demo.hangarlab.dev.:8443": "web-demo.hangarlab.dev",
		"[2001:db8::1]:443":            "2001:db8::1",
		"":                             "",
	} {
		if got := edge.Hostname(in); got != want {
			t.Errorf("Hostname(%q) = %q, want %q", in, got, want)
		}
	}
}
