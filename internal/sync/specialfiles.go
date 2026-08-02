package sync

import (
	"errors"
	"fmt"
	"io/fs"
)

// SpecialFileType classifies a FileEntry beyond the basic
// regular-file/directory/symlink distinction Walk already exposes via
// IsDir and Mode&fs.ModeSymlink.
type SpecialFileType int

const (
	// NotSpecial is a regular file, directory, or symlink.
	NotSpecial SpecialFileType = iota
	// NamedPipe is a FIFO, the only SpecialFileType ApplySpecialFile actually recreates.
	NamedPipe
	// Socket is a Unix domain socket file.
	Socket
	// CharDevice is a character device node.
	CharDevice
	// BlockDevice is a block device node.
	BlockDevice
)

// ClassifySpecialFile reports which SpecialFileType, if any, entry's Mode represents.
func ClassifySpecialFile(entry FileEntry) SpecialFileType {
	switch {
	case entry.Mode&fs.ModeNamedPipe != 0:
		return NamedPipe
	case entry.Mode&fs.ModeSocket != 0:
		return Socket
	case entry.Mode&fs.ModeCharDevice != 0:
		// Must be checked before the bare ModeDevice case: Go sets
		// ModeCharDevice as a modifier on top of ModeDevice, so a character
		// device has both bits set, not ModeCharDevice alone.
		return CharDevice
	case entry.Mode&fs.ModeDevice != 0:
		return BlockDevice
	default:
		return NotSpecial
	}
}

// ErrSpecialFileUnsupported is returned by ApplySpecialFile for socket,
// character device, and block device entries: none of these are actually
// recreated by this package. Device nodes need privileged syscall.Mknod, and
// an inert socket-typed node isn't a meaningful stand-in for a live socket -
// only named pipes (via syscall.Mkfifo) are unprivileged and worth creating.
var ErrSpecialFileUnsupported = errors.New("special file type is detected but not recreated by this package")

// ApplySpecialFile creates destPath as the special file entry represents,
// for the one type this package supports creating: named pipes. For
// Socket/CharDevice/BlockDevice it returns an error wrapping
// ErrSpecialFileUnsupported; for NotSpecial it also errors.
func ApplySpecialFile(destPath string, entry FileEntry) error {
	switch ClassifySpecialFile(entry) {
	case NamedPipe:
		return applyNamedPipe(destPath, entry.Mode)
	case Socket, CharDevice, BlockDevice:
		return fmt.Errorf("%q: %w", entry.Path, ErrSpecialFileUnsupported)
	default:
		return fmt.Errorf("entry %q is not a special file (Mode=%v)", entry.Path, entry.Mode)
	}
}
