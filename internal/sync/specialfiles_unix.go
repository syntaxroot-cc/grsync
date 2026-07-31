//go:build !windows

package sync

import (
	"io/fs"
	"syscall"
)

// applyNamedPipe creates destPath as a FIFO via syscall.Mkfifo, which
// (unlike device-node creation via Mknod) requires no elevated privilege
// on POSIX systems - any user can create a FIFO in a directory they can
// write to.
func applyNamedPipe(destPath string, mode fs.FileMode) error {
	return syscall.Mkfifo(destPath, uint32(mode.Perm()))
}
