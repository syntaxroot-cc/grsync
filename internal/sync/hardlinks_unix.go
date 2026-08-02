//go:build !windows

package sync

import (
	"os"
	"syscall"
)

// hardLinkKey identifies a file's underlying inode. An inode number alone is
// only unique within a device, so (Dev, Ino) together is the identity POSIX
// actually guarantees across filesystems.
type hardLinkKey struct {
	Dev uint64
	Ino uint64
}

// lookupHardLinkKey extracts the (dev, ino) identity from a Lstat'd
// os.FileInfo via its syscall.Stat_t. ok is false only if the platform's
// Sys() doesn't return that type.
//
// stat.Dev is used as-is since it's already uint64 on Linux, the only
// platform this project builds/tests for; other POSIX platforms (e.g.
// Darwin) declare it as a narrower signed type and would need conversion.
func lookupHardLinkKey(info os.FileInfo) (hardLinkKey, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return hardLinkKey{}, false
	}
	return hardLinkKey{Dev: stat.Dev, Ino: stat.Ino}, true
}

// HardLinksSupported reports whether this platform can detect hard links at all.
func HardLinksSupported() bool { return true }
