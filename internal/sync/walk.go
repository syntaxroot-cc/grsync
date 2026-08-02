// Package sync builds the file list that grsync compares between source and
// destination before any data transfer happens.
package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FileEntry describes a single file, directory, or symlink discovered under
// a source root. Path is always relative to that root and "/"-separated
// regardless of host OS, matching rsync's wire protocol.
type FileEntry struct {
	Path    string
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
	UID     uint32
	GID     uint32
	// OwnershipAvailable reports whether UID/GID were actually populated: a
	// real uid/gid of 0 (root) is valid, so a zero value alone doesn't mean
	// "unavailable" (see uidgid_windows.go, where this is always false).
	OwnershipAvailable bool
	LinkTarget         string
	IsDir              bool
}

// WalkOptions controls how far Walk descends, mirroring rsync's own
// -r/--recursive and -d/--dirs flags:
//
//   - Recursive=false, Dirs=false: directories are skipped entirely (rsync's
//     default with neither flag).
//   - Recursive=false, Dirs=true (-d): directories are listed but not
//     descended into.
//   - Recursive=true (-r): full recursion regardless of Dirs, since -r makes
//     -d redundant.
type WalkOptions struct {
	Recursive bool
	Dirs      bool
}

// buildFileEntry constructs a FileEntry for path from its already-Lstat'd
// info, leaving Path unset for the caller to fill in.
func buildFileEntry(path string, info fs.FileInfo) (FileEntry, error) {
	entry := FileEntry{
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    info.Mode(),
		IsDir:   info.IsDir(),
	}

	entry.UID, entry.GID, entry.OwnershipAvailable = lookupUIDGID(info)

	// info.Mode()&fs.ModeSymlink is only ever set by Lstat, not Stat - every
	// caller of this function must Lstat, or this branch is unreachable.
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return FileEntry{}, err
		}
		entry.LinkTarget = target
	}

	return entry, nil
}

// LstatEntry builds a FileEntry for a single existing path the same way Walk
// builds one for each path it visits, without needing a whole tree walk.
// Path is left empty. The returned error satisfies os.IsNotExist(err) when
// path doesn't exist, exactly like a direct os.Lstat call would.
func LstatEntry(path string) (FileEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileEntry{}, err
	}
	return buildFileEntry(path, info)
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
			return nil
		}

		if !opts.Recursive && d.IsDir() && !opts.Dirs {
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		// os.Lstat, not os.Stat: a symlink must be reported as itself, not
		// resolved to whatever it points at.
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		entry, err := buildFileEntry(path, info)
		if err != nil {
			return err
		}
		entry.Path = filepath.ToSlash(rel)

		entries = append(entries, entry)

		if !opts.Recursive && d.IsDir() {
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// filepath.WalkDir only guarantees order within each directory, not
	// across the whole tree, so sort the flattened list by full path. Go's
	// byte-wise string comparison matches C's strcmp, which rsync's own
	// pathname sort relies on.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return entries, nil
}
