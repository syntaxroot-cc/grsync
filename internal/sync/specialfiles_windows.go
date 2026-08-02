//go:build windows

package sync

import (
	"fmt"
	"io/fs"
)

// applyNamedPipe always fails on Windows: named pipes there live under
// \\.\pipe\ as a distinct IPC mechanism, not a filesystem-node type
// creatable at an arbitrary path the way a POSIX FIFO is.
func applyNamedPipe(destPath string, _ fs.FileMode) error {
	return fmt.Errorf("creating a named pipe at %q: not supported on Windows", destPath)
}
