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
	// NotSpecial is a regular file, directory, or symlink - none of
	// which ClassifySpecialFile is concerned with.
	NotSpecial SpecialFileType = iota
	// NamedPipe is a FIFO. The only SpecialFileType ApplySpecialFile
	// actually recreates - see its doc comment for why.
	NamedPipe
	// Socket is a Unix domain socket file.
	Socket
	// CharDevice is a character device node.
	CharDevice
	// BlockDevice is a block device node.
	BlockDevice
)

// ClassifySpecialFile reports which SpecialFileType, if any, entry's Mode
// represents. fs.FileMode's type bits are portable Go constants with the
// same meaning regardless of host OS, even though most of them (the
// device/socket bits especially) only ever actually get set by a real
// Lstat on POSIX - so this classification itself needs no build tag, even
// though actually recreating some of these types does (see
// specialfiles_unix.go / specialfiles_windows.go).
func ClassifySpecialFile(entry FileEntry) SpecialFileType {
	switch {
	case entry.Mode&fs.ModeNamedPipe != 0:
		return NamedPipe
	case entry.Mode&fs.ModeSocket != 0:
		return Socket
	case entry.Mode&fs.ModeCharDevice != 0:
		// Must be checked before the bare ModeDevice case below: Go's
		// fs.FileMode convention sets ModeCharDevice as a modifier on
		// top of ModeDevice for character devices, so a character
		// device has *both* bits set, not ModeCharDevice alone.
		return CharDevice
	case entry.Mode&fs.ModeDevice != 0:
		return BlockDevice
	default:
		return NotSpecial
	}
}

// ErrSpecialFileUnsupported is returned by ApplySpecialFile for socket,
// character device, and block device entries: none of these are actually
// recreated by this package.
//
// Character/block device nodes need syscall.Mknod, which requires
// CAP_MKNOD/root on Linux (and has no meaningful equivalent to attempt on
// Windows at all) - untestable without that privilege, and attempting it
// anyway would just fail for most callers while quietly calling that
// "support". A Unix domain socket file without a live listening process
// behind it also isn't meaningfully equivalent to the original; simply
// creating an inert socket-typed filesystem node isn't the same thing as
// preserving a socket. Rather than do either job partially, this is
// scoped down to detecting these types (via ClassifySpecialFile) but not
// recreating them - only named pipes (FIFOs) are actually created, since
// syscall.Mkfifo needs no elevated privilege and is fully testable.
var ErrSpecialFileUnsupported = errors.New("special file type is detected but not recreated by this package")

// ApplySpecialFile creates destPath as the special file entry represents,
// for the one type this package actually supports creating: named pipes.
// For Socket/CharDevice/BlockDevice it returns an error wrapping
// ErrSpecialFileUnsupported. For NotSpecial (a regular file, directory,
// or symlink), it also returns an error: this function is only meaningful
// for an entry that actually is one of the special types above.
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
