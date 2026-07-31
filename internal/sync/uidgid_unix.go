//go:build !windows

package sync

import (
	"os"
	"syscall"
)

// lookupUIDGID extracts the owning user/group ID from a Lstat'd
// os.FileInfo. On POSIX platforms this is available via the
// syscall.Stat_t underlying the info; ok is false only if the platform's
// Sys() doesn't actually return that type (unexpected, but Sys() is
// documented as platform-dependent, so it's not guaranteed).
func lookupUIDGID(info os.FileInfo) (uid, gid uint32, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return stat.Uid, stat.Gid, true
}
