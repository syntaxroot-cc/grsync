package sync

import (
	"fmt"
	"io/fs"
	"os"
	"time"
)

// applyPerms applies entry's permission bits to path via os.Chmod.
//
// mode is deliberately fs.FileMode.Perm() here, not the raw FileEntry.Mode:
// FileMode packs type bits (fs.ModeDir, fs.ModeSymlink, etc.) into its high
// bits alongside the low 9 permission bits, and os.Chmod on most platforms
// ignores or rejects those type bits rather than erroring - passing the raw
// value through would either silently do nothing useful with the type
// information or produce a confusing partial result. Perm() strips
// everything except the permission bits, which is all Chmod can act on
// anyway.
func applyPerms(path string, mode fs.FileMode) error {
	return os.Chmod(path, mode.Perm())
}

// applyTimes applies modTime to path as both the access and modification
// time via os.Chtimes.
//
// os.Chtimes has no way to change only mtime and leave atime alone -
// POSIX utimes() always sets both together - so some value has to be
// chosen for atime. rsync itself doesn't meaningfully preserve atime as
// part of -a/--archive either. Using modTime for atime too, rather than
// time.Now(), keeps this idempotent: reapplying the same FileEntry's
// attributes a second time (e.g. re-running a sync that changed nothing)
// produces byte-identical timestamps both times, instead of atime
// drifting to a new "now" on every run.
func applyTimes(path string, modTime time.Time) error {
	return os.Chtimes(path, modTime, modTime)
}

// applyOwnership applies entry's UID and/or GID to path via os.Lchown -
// not os.Chown - so that when entry is a symlink, ownership is set on the
// link itself rather than whatever it points at (matching Lstat's own
// convention elsewhere in this package).
//
// applyOwner and applyGroup are independent switches (mirroring the
// separate --owner/--group flags): os.Lchown treats a uid or gid of -1 as
// "leave this one unchanged", so requesting only one of the two still
// takes a single syscall rather than needing two different code paths.
//
// If neither applyOwner nor applyGroup is requested, this is a no-op:
// (false, false, nil). If they are requested but entry.OwnershipAvailable
// is false, Lchown is deliberately never called - applying UID/GID 0
// would silently claim "owned by root", which is not what an unavailable
// value means (see FileEntry.OwnershipAvailable's doc comment). That case
// is reported back as skipped=true rather than left indistinguishable
// from "successfully applied", so a caller can surface it instead of it
// disappearing into a bare nil error.
//
// Operational note: changing ownership to an arbitrary uid/gid is a
// privileged operation on most POSIX systems (requires root / CAP_CHOWN).
// Expect Lchown to return a permission error when grsync runs unprivileged
// against files not already owned by the current user - that's a
// constraint on how grsync must be run, not something this function
// tries to work around.
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

// applySymlink creates destPath as a symlink pointing at entry.LinkTarget,
// rather than following the link and copying whatever it points to - the
// entire point of preserving a symlink as a symlink.
//
// Unlike Path, LinkTarget is passed through exactly as Walk captured it
// (via os.Readlink) - not slash-normalized. That's deliberate, not an
// oversight: Path is always a relative path Walk itself constructs, so
// normalizing it to "/" is safe and purely internal. LinkTarget is
// arbitrary data authored by whatever created the original symlink - it
// may be an absolute path pointing outside the transfer tree entirely, on
// POSIX it may even legally contain a literal backslash (a normal
// filename character there, not a separator), and real rsync itself
// treats symlink targets as opaque strings rather than rewriting them.
// Slash-normalizing it could corrupt a target that was never meant to be
// interpreted as portable path syntax.
func applySymlink(destPath string, entry FileEntry) error {
	if entry.Mode&fs.ModeSymlink == 0 {
		return fmt.Errorf("entry %q is not a symlink (Mode=%v)", entry.Path, entry.Mode)
	}
	if entry.LinkTarget == "" {
		return fmt.Errorf("entry %q is a symlink but has no LinkTarget", entry.Path)
	}

	// os.Symlink fails with "file exists" if destPath is already
	// occupied, which is the normal case when re-applying attributes (or
	// re-running a sync) against a destination that already has a prior
	// version of this entry. Remove whatever's there first; a missing
	// path is not an error here.
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing entry at %q: %w", destPath, err)
	}

	return os.Symlink(entry.LinkTarget, destPath)
}

// AttrOptions selects which attribute categories ApplyAttributes should
// preserve, mirroring the CLI's --perms/--times/--owner/--group/--links/
// --hard-links/--devices flags so each can be toggled independently, the
// same way --archive bundles several of these together while individual
// flags still control them separately.
//
// HardLinks and Devices are included here for a complete, consistent
// shape matching all seven flags, but ApplyAttributes itself does not act
// on them: both are inherently multi-entry operations (a hard link only
// means something in relation to at least one other FileEntry; a device
// file needs privileged recreation handled separately - see
// DetectHardLinks/ApplyHardLinks in hardlinks.go and ApplySpecialFile in
// specialfiles.go). A caller orchestrating a full sync consults
// opts.HardLinks/opts.Devices itself to decide whether to invoke those
// separately; ApplyAttributes silently ignoring them here would be
// misleading, so this is called out explicitly rather than left implicit.
type AttrOptions struct {
	Perms     bool
	Times     bool
	Owner     bool
	Group     bool
	Links     bool
	HardLinks bool
	Devices   bool
}

// AttrResult reports what ApplyAttributes actually did, distinguishing
// "applied" from "explicitly skipped" so a caller can surface a skip
// (e.g. "ownership unavailable on this platform") instead of it
// disappearing silently into a bare nil error.
type AttrResult struct {
	PermsApplied bool
	TimesApplied bool
	OwnerApplied bool
	OwnerSkipped bool
	LinkApplied  bool
}

// ApplyAttributes applies the attribute categories opts selects to
// destPath, using entry's metadata (as captured by Walk). destPath must
// already exist as the right kind of filesystem entry for non-symlink
// attributes (e.g. already written by ApplyDelta for a regular file);
// for a symlink entry with opts.Links set, ApplyAttributes creates the
// symlink itself, since a symlink has no separate byte-content delta step.
//
// For a symlink entry, Perms and Times are silently not applied even if
// requested: os.Chmod and os.Chtimes both follow symlinks (Go's standard
// library has no portable Lchmod/Lchtimes), so calling them on a symlink
// path would either modify whatever the link points to - not the link
// itself - or error outright if that target doesn't exist yet. Ownership
// is the exception: os.Lchown (used by applyOwnership) correctly targets
// the symlink itself, matching Lstat's own convention, so Owner/Group are
// still applied normally.
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
