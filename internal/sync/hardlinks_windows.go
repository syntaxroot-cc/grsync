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
// identity comes from POSIX os.FileInfo.Sys() (*syscall.Stat_t), which
// Windows doesn't provide (its Sys() is *syscall.Win32FileAttributeData).
// NTFS has its own file-identity concept via GetFileInformationByHandle,
// but wiring that up is out of scope here; hard-linked files on a Windows
// source are simply treated as independent files instead.
func lookupHardLinkKey(_ os.FileInfo) (hardLinkKey, bool) {
	return hardLinkKey{}, false
}

// HardLinksSupported reports whether this platform can detect hard links at all.
func HardLinksSupported() bool { return false }
