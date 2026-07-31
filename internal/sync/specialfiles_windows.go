//go:build windows

package sync

import (
	"fmt"
	"io/fs"
)

// applyNamedPipe always fails on Windows: syscall.Mkfifo doesn't exist
// there. Windows named pipes are a distinct IPC mechanism under \\.\pipe\
// rather than a filesystem-node type created at an arbitrary path the way
// a POSIX FIFO is, so there's no equivalent "create this path as a named
// pipe" operation to perform here at all.
func applyNamedPipe(destPath string, _ fs.FileMode) error {
	return fmt.Errorf("creating a named pipe at %q: not supported on Windows", destPath)
}
