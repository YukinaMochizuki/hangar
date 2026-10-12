package registry_test

import (
	"fmt"
	"os"
	"testing"
)

// TestMain skips the package unless it runs as root: unpacking gives files
// owners. make test-root runs it.
func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Println("skipping: registry tests unpack images with owners, run them as root (make test-root)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
