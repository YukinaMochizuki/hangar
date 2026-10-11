//go:build testbed

// Package guest holds the real-host tests of the guest itself: the platform
// kernel, its initramfs and hangar-init, with no node code
// (docs/runbook/testbed.md).
package guest

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

func TestMain(m *testing.M) { vmtest.Main(m) }
