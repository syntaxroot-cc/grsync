//go:build windows

package sync

import "os"

// lookupUIDGID always reports unavailable on Windows: os.FileInfo.Sys()
// there returns *syscall.Win32FileAttributeData, which carries no POSIX
// uid/gid concept at all (Windows uses SIDs and ACLs instead). Callers must
// treat ok == false as "not populated", not as "owned by uid/gid 0" —
// zero is not a meaningful default here.
func lookupUIDGID(_ os.FileInfo) (uid, gid uint32, ok bool) {
	return 0, 0, false
}
