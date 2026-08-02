package sync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// HardLinkGroup is a set of paths (relative, "/"-separated, matching
// FileEntry.Path) that share one (device, inode) identity on POSIX and must
// be recreated as hard links of each other, not independent copies.
type HardLinkGroup []string

// DetectHardLinks re-examines entries under root and groups together every
// regular file that shares an inode with at least one other entry. Only
// groups with more than one member are returned.
//
// On a platform where lookupHardLinkKey can't determine inode identity
// (Windows), every entry is its own singleton, so this always returns no
// groups there. An empty result is thus ambiguous between "no hard links"
// and "can't detect them" - call HardLinksSupported() to tell those apart.
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
		sort.Strings(paths) // deterministic member order
		groups = append(groups, HardLinkGroup(paths))
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i][0] < groups[j][0] // deterministic group order
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
		// os.Link fails if dest already exists.
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing existing entry at %q: %w", dest, err)
		}
		if err := os.Link(source, dest); err != nil {
			return fmt.Errorf("linking %q to %q: %w", p, group[0], err)
		}
	}
	return nil
}
