//go:build !windows

package sync

import (
	"os"
	"syscall"
)

// lookupUIDGID extracts the owning user/group ID from a Lstat'd os.FileInfo
// via its syscall.Stat_t. ok is false only if the platform's Sys() doesn't
// return that type.
func lookupUIDGID(info os.FileInfo) (uid, gid uint32, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return stat.Uid, stat.Gid, true
}
