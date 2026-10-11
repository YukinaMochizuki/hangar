// Package buildinfo says which build a binary is, for agents reporting
// their version.
package buildinfo

import "runtime/debug"

// Version is the VCS revision the binary was built from, marked when the
// tree had uncommitted changes.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return versionOf(info)
}

func versionOf(info *debug.BuildInfo) string {
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return info.Main.Version
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
