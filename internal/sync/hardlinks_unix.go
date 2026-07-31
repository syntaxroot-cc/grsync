//go:build !windows

package sync

import (
	"os"
	"syscall"
)

// hardLinkKey identifies a file's underlying inode, uniquely across
// possibly multiple filesystems: an inode number alone is only unique
// within a single device, so two files on different devices/filesystems
// could coincidentally share an inode number without actually being
// hard-linked to each other. (Dev, Ino) together is the identity POSIX
// actually guarantees.
type hardLinkKey struct {
	Dev uint64
	Ino uint64
}

// lookupHardLinkKey extracts the (dev, ino) identity from a Lstat'd
// os.FileInfo, the same way lookupUIDGID (uidgid_unix.go) extracts
// uid/gid - via the syscall.Stat_t underlying the info. ok is false only
// if the platform's Sys() doesn't actually return that type.
//
// stat.Dev is used as-is: it's already uint64 on the Linux target this
// project actually builds and tests for (see .github/workflows/ci.yaml).
// Some other POSIX platforms (e.g. Darwin) declare Dev as a narrower
// signed type, which would need an explicit conversion - not added here
// speculatively for a platform this project doesn't build or test
// against; that can be added if/when Darwin support is actually taken on.
func lookupHardLinkKey(info os.FileInfo) (hardLinkKey, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return hardLinkKey{}, false
	}
	return hardLinkKey{Dev: stat.Dev, Ino: stat.Ino}, true
}

// HardLinksSupported reports whether this platform can detect hard links
// at all - see DetectHardLinks's doc comment for how to use this to
// surface the Windows gap explicitly (e.g. a one-time warning) rather
// than leaving callers to infer it from an empty result list, which is
// ambiguous with "this tree just has no hard links".
func HardLinksSupported() bool { return true }
