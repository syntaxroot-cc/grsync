package sync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// HardLinkGroup is a set of paths (relative, "/"-separated, matching
// FileEntry.Path) that are all hard-linked to the same underlying file -
// i.e. they share one (device, inode) identity on POSIX - and so must be
// recreated as hard links of each other on the receiving end, not as
// independent copies of the same content.
type HardLinkGroup []string

// DetectHardLinks re-examines entries under root (paths relative to root,
// as produced by Walk) and groups together every regular file that shares
// an inode with at least one other entry.
//
// This is a separate pass over Walk's output, not something Walk itself
// tracks - the same tradeoff FilterEntries (filter.go) already made for
// the same reason: it keeps Walk unchanged and independently testable.
// The cost here is re-Lstat'ing each non-directory, non-symlink entry;
// directories and symlinks are skipped outright since neither can be
// hard-linked to a regular file's data.
//
// Only groups with more than one member are returned - a file sharing its
// inode with nothing else needs no special handling, it's just a normal
// file. On a platform where lookupHardLinkKey can't determine inode
// identity (Windows - see hardlinks_windows.go), every entry is treated
// as its own singleton, so DetectHardLinks always returns no groups
// there; that's a documented platform gap, not a bug in this function.
//
// An empty result is genuinely ambiguous on its own: it means either "no
// hard links exist in this tree" or "this platform can't detect them at
// all," and those call for different handling by whoever's orchestrating
// a sync (the latter is worth a one-time warning; the former isn't worth
// mentioning). Call HardLinksSupported() to tell them apart explicitly
// rather than guessing from an empty slice.
func DetectHardLinks(root string, entries []FileEntry) ([]HardLinkGroup, error) {
	byKey := make(map[hardLinkKey][]string)

	for _, e := range entries {
		if e.IsDir || e.Mode&fs.ModeSymlink != 0 {
			continue
		}

		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", e.Path, err)
		}

		key, ok := lookupHardLinkKey(info)
		if !ok {
			continue
		}
		byKey[key] = append(byKey[key], e.Path)
	}

	var groups []HardLinkGroup
	for _, paths := range byKey {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths) // deterministic member order, same reasoning as Walk's own sort
		groups = append(groups, HardLinkGroup(paths))
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i][0] < groups[j][0] // deterministic group order too
	})

	return groups, nil
}

// ApplyHardLinks recreates a HardLinkGroup under destRoot: group[0] is
// assumed to already exist as a real file there (e.g. already written via
// ApplyDelta), and every other member is created via os.Link against it
// instead of being written out as a separate copy of the same data.
func ApplyHardLinks(destRoot string, group HardLinkGroup) error {
	if len(group) < 2 {
		return nil
	}

	source := filepath.Join(destRoot, filepath.FromSlash(group[0]))
	for _, p := range group[1:] {
		dest := filepath.Join(destRoot, filepath.FromSlash(p))

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("creating parent directory for %q: %w", p, err)
		}
		// os.Link fails if dest already exists, the same reason
		// applySymlink removes any existing entry first.
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing existing entry at %q: %w", dest, err)
		}
		if err := os.Link(source, dest); err != nil {
			return fmt.Errorf("linking %q to %q: %w", p, group[0], err)
		}
	}
	return nil
}
