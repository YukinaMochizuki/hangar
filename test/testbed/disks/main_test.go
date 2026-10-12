//go:build testbed

// Package disks holds the real-host tests of the image and storage drivers
// (internal/node/driver/registry, zfsstore): their conformance to the
// drivers' contract on a real ZFS pool, pulls from Docker Hub, and guests
// booting from the root disks they make (docs/runbook/testbed.md).
package disks

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

func TestMain(m *testing.M) { vmtest.Main(m) }
