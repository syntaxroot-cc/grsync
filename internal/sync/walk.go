// Package sync builds the file list ("flist") that grsync compares between
// source and destination before any data transfer happens - the same
// planning phase upstream rsync performs before its delta algorithm runs.
package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FileEntry describes a single file, directory, or symlink discovered under
// a source root. Path is always relative to that root, using "/" as the
// separator regardless of host OS - rsync's wire protocol and file lists
// are always "/"-separated, and grsync targets protocol-level
// interoperability, so paths are normalized at collection time rather than
// left OS-native and converted later.
type FileEntry struct {
	Path    string
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
	UID     uint32
	GID     uint32
	// OwnershipAvailable reports whether UID/GID were actually populated.
	// A real uid/gid of 0 (root) is a valid value, so callers must check
	// this rather than treating a zero UID/GID as "unavailable" - see
	// uidgid_windows.go, where it is always false.
	OwnershipAvailable bool
	LinkTarget         string
	IsDir              bool
}

// WalkOptions controls how far Walk descends, mirroring rsync's own
// -r/--recursive and -d/--dirs flags:
//
//   - Recursive=false, Dirs=false (rsync's default with neither flag):
//     directories are skipped entirely - not listed, not descended into.
//     Only regular files/symlinks directly under root are collected.
//   - Recursive=false, Dirs=true (-d): directories are listed (so they can
//     be created on the receiving end) but their contents are not - Walk
//     does not descend into them.
//   - Recursive=true (-r): full recursion into every subdirectory,
//     regardless of Dirs - this matches rsync, where -r makes -d redundant.
type WalkOptions struct {
	Recursive bool
	Dirs      bool
}

// Walk collects a FileEntry for every entry found under root, not including
// root itself, subject to opts. Paths are relative to root and
// "/"-separated.
func Walk(root string, opts WalkOptions) ([]FileEntry, error) {
	var entries []FileEntry

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			// The root itself isn't part of its own file list.
			return nil
		}

		if !opts.Recursive && d.IsDir() && !opts.Dirs {
			// Neither -r nor -d: rsync skips directories entirely rather
			// than creating them empty, so this one is neither listed nor
			// descended into.
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		// os.Lstat, not os.Stat: a symlink must be reported as itself, not
		// resolved to whatever it points at. WalkDir's own DirEntry is
		// already lstat-derived (it never follows symlinks when deciding
		// what to recurse into), but calling Lstat explicitly here makes
		// that guarantee obvious at the call site rather than implicit in
		// WalkDir's internals.
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		entry := FileEntry{
			Path:    filepath.ToSlash(rel),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Mode:    info.Mode(),
			IsDir:   info.IsDir(),
		}

		// lookupUIDGID is platform-specific (see uidgid_unix.go /
		// uidgid_windows.go): on Windows it always reports unavailable,
		// leaving UID/GID at their zero value. OwnershipAvailable carries
		// that ok flag through so callers can't mistake the zero value for
		// a real uid/gid of 0 (root) - see uidgid_windows.go for why.
		entry.UID, entry.GID, entry.OwnershipAvailable = lookupUIDGID(info)

		// info.Mode()&fs.ModeSymlink is only ever set by Lstat (Stat
		// resolves through it), which is exactly why Lstat was required
		// above: this branch would never be reachable otherwise.
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry.LinkTarget = target
		}

		entries = append(entries, entry)

		if !opts.Recursive && d.IsDir() {
			// Whether or not -d caused this directory to be listed above,
			// without -r its contents are never descended into.
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// filepath.WalkDir only guarantees lexicographic order within each
	// directory, not across the whole tree (a directory's contents are
	// interleaved with sibling entries in traversal order, not sorted
	// globally). Sorting the flattened list by full path afterward is what
	// actually gives deterministic, rsync-comparable ordering; plain Go
	// string comparison is byte-wise, matching C's strcmp, which is what
	// rsync's own pathname sort relies on.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return entries, nil
}
