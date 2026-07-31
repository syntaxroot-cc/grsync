//go:build windows

package sync

import "os"

// hardLinkKey identifies a file's underlying inode. On Windows this is
// never actually populated - see lookupHardLinkKey.
type hardLinkKey struct {
	Dev uint64
	Ino uint64
}

// lookupHardLinkKey always reports unavailable on Windows: the (dev, ino)
// identity this package's hard-link detection relies on comes from POSIX
// os.FileInfo.Sys() (*syscall.Stat_t), which Windows doesn't provide -
// its Sys() there is *syscall.Win32FileAttributeData instead. Windows
// NTFS does have its own file-identity concept (a volume serial number
// plus a 64-bit file index, retrievable via
// GetFileInformationByHandle/BY_HANDLE_FILE_INFORMATION), and os.Link
// does work on Windows/NTFS - but wiring up that Win32-specific API is a
// meaningfully larger, separate piece of platform code, not a small
// extension of the POSIX path. Scoping it out here means hard-linked
// files on a Windows source are simply treated as independent, unlinked
// files (correct output, just missed space-saving/consistency benefit)
// rather than the sync producing wrong results.
func lookupHardLinkKey(_ os.FileInfo) (hardLinkKey, bool) {
	return hardLinkKey{}, false
}

// HardLinksSupported reports whether this platform can detect hard links
// at all - always false on Windows. See DetectHardLinks's doc comment.
func HardLinksSupported() bool { return false }
