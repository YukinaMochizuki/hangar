package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestVersion(t *testing.T) {
	for _, tt := range []struct {
		settings []debug.BuildSetting
		want     string
	}{
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "false"}}, "abc123"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}}, "abc123-dirty"},
		{nil, "v1.2.3"},
	} {
		info := &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}, Settings: tt.settings}
		if got := versionOf(info); got != tt.want {
			t.Errorf("version of %v: %q, want %q", tt.settings, got, tt.want)
		}
	}
}
