//go:build !testbed

package main

import "github.com/YukinaMochizuki/hangar/internal/guest/supervise"

// buildMarker is what a testbed build adds to the agent's version: none.
const buildMarker = ""

// serverReaper is what a FileShare's server's agent starts processes with:
// none, so Exec is refused.
func serverReaper(func(string, ...any)) *supervise.Reaper { return nil }
