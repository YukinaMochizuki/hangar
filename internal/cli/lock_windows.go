package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile waits for an exclusive lock on f's first byte, which every
// hangar process locks, and unlockFile lets go.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
