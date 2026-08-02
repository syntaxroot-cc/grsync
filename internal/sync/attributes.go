package sync

import (
	"fmt"
	"io/fs"
	"os"
	"time"
)

// applyPerms applies entry's permission bits to path via os.Chmod.
//
// mode.Perm() strips FileMode's type bits (fs.ModeDir, fs.ModeSymlink, ...)
// before the call, since os.Chmod only understands the low 9 permission bits.
func applyPerms(path string, mode fs.FileMode) error {
	return os.Chmod(path, mode.Perm())
}

// applyTimes applies modTime to path as both access and modification time via os.Chtimes.
//
// os.Chtimes can't set mtime without also setting atime (POSIX utimes sets
// both together). Using modTime for atime too keeps repeated syncs
// idempotent, instead of atime drifting to a new "now" on every run.
func applyTimes(path string, modTime time.Time) error {
	return os.Chtimes(path, modTime, modTime)
}

// applyOwnership applies entry's UID/GID to path via os.Lchown, so that on a
// symlink ownership is set on the link itself rather than its target.
//
// applyOwner and applyGroup are independent: Lchown treats a uid/gid of -1 as
// "leave unchanged". If entry.OwnershipAvailable is false, Lchown is never
// called - applying UID/GID 0 would wrongly claim "owned by root" - and
// skipped=true is returned instead of a silent no-op.
func applyOwnership(path string, entry FileEntry, applyOwner, applyGroup bool) (applied, skipped bool, err error) {
	if !applyOwner && !applyGroup {
		return false, false, nil
	}
	if !entry.OwnershipAvailable {
		return false, true, nil
	}

	uid, gid := -1, -1
	if applyOwner {
		uid = int(entry.UID)
	}
	if applyGroup {
		gid = int(entry.GID)
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// applySymlink creates destPath as a symlink pointing at entry.LinkTarget.
//
// Unlike Path, LinkTarget is passed through exactly as Walk captured it, not
// slash-normalized: it's opaque data from whatever created the original
// symlink, may be absolute or point outside the transfer tree, and on POSIX
// may legally contain a literal backslash (not a separator there).
func applySymlink(destPath string, entry FileEntry) error {
	if entry.Mode&fs.ModeSymlink == 0 {
		return fmt.Errorf("entry %q is not a symlink (Mode=%v)", entry.Path, entry.Mode)
	}
	if entry.LinkTarget == "" {
		return fmt.Errorf("entry %q is a symlink but has no LinkTarget", entry.Path)
	}

	// os.Symlink fails with "file exists" if destPath is already occupied
	// (e.g. re-running a sync); remove any existing entry first.
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing entry at %q: %w", destPath, err)
	}

	return os.Symlink(entry.LinkTarget, destPath)
}

// AttrOptions selects which attribute categories ApplyAttributes preserves,
// mirroring the CLI's --perms/--times/--owner/--group/--links/--hard-links/
// --devices flags.
//
// HardLinks and Devices are included for a complete shape matching all seven
// flags, but ApplyAttributes does not act on them - both are multi-entry
// operations handled separately (see DetectHardLinks/ApplyHardLinks in
// hardlinks.go and ApplySpecialFile in specialfiles.go).
type AttrOptions struct {
	Perms     bool
	Times     bool
	Owner     bool
	Group     bool
	Links     bool
	HardLinks bool
	Devices   bool
}

// AttrResult reports what ApplyAttributes did, distinguishing "applied" from
// "explicitly skipped" (e.g. ownership unavailable) so a caller can surface it.
type AttrResult struct {
	PermsApplied bool
	TimesApplied bool
	OwnerApplied bool
	OwnerSkipped bool
	LinkApplied  bool
}

// ApplyAttributes applies the attribute categories opts selects to destPath,
// using entry's metadata as captured by Walk. destPath must already exist as
// the right kind of filesystem entry for non-symlink attributes; for a
// symlink entry with opts.Links set, ApplyAttributes creates the symlink itself.
//
// For a symlink, Perms and Times are silently skipped even if requested:
// os.Chmod/os.Chtimes follow symlinks (Go has no portable Lchmod/Lchtimes),
// so calling them would affect the target, not the link. Ownership is the
// exception since os.Lchown targets the link itself.
func ApplyAttributes(entry FileEntry, destPath string, opts AttrOptions) (AttrResult, error) {
	var result AttrResult

	isSymlink := entry.Mode&fs.ModeSymlink != 0

	if isSymlink {
		if opts.Links {
			if err := applySymlink(destPath, entry); err != nil {
				return result, fmt.Errorf("creating symlink for %q: %w", entry.Path, err)
			}
			result.LinkApplied = true
		}
	} else {
		if opts.Perms {
			if err := applyPerms(destPath, entry.Mode); err != nil {
				return result, fmt.Errorf("applying permissions to %q: %w", entry.Path, err)
			}
			result.PermsApplied = true
		}
		if opts.Times {
			if err := applyTimes(destPath, entry.ModTime); err != nil {
				return result, fmt.Errorf("applying modification time to %q: %w", entry.Path, err)
			}
			result.TimesApplied = true
		}
	}

	if opts.Owner || opts.Group {
		applied, skipped, err := applyOwnership(destPath, entry, opts.Owner, opts.Group)
		if err != nil {
			return result, fmt.Errorf("applying ownership to %q: %w", entry.Path, err)
		}
		result.OwnerApplied = applied
		result.OwnerSkipped = skipped
	}

	return result, nil
}
