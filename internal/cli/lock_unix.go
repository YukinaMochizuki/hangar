//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// lockFile waits for an exclusive lock on f, which unlockFile lets go.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
