package ocilayer_test

import (
	"fmt"
	"os"
	"testing"
)

// TestMain skips the package unless it runs as root: every layer applied
// gives its entries owners. make test-root runs it.
func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Println("skipping: ocilayer tests give files owners, run them as root (make test-root)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
