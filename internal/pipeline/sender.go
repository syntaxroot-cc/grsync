package pipeline

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// Sender runs the sending side of a sync over rw: walks and filters src,
// detects which entries are hard-linked to each other (only if
// hardLinks is true - see below), sends the resulting file list (with
// that grouping attached), then for each regular-file entry that isn't a
// secondary member of a hard-link group receives the receiver's
// signature, computes a delta against the current source bytes, and
// sends it back.
//
// Directories and symlinks are deliberately not part of this exchange at
// all: a directory has no byte content to diff, and a symlink's entire
// "content" is its LinkTarget, which already travels inside the FileEntry
// in the file list itself. A hard-linked group's second-and-later members
// are skipped the same way, for a different reason: their data is
// byte-identical to the group's first member by definition (they're the
// same inode), so exchanging a signature/delta for them would just
// re-transfer bytes the receiver is about to get for free via
// sync.ApplyHardLinks instead.
//
// hardLinks mirrors real rsync's own -H/--hard-links flag: off by
// default, and deliberately not implied by --archive (real rsync's -a is
// -rlptgoD, no H) - detecting hard links means an extra Lstat per entry,
// a cost real rsync doesn't spend unless asked to, so grsync doesn't
// either.
func Sender(rw io.ReadWriter, src string, walkOpts sync.WalkOptions, rules []sync.Rule, hardLinks bool) error {
	entries, err := sync.Walk(src, walkOpts)
	if err != nil {
		return fmt.Errorf("walking %q: %w", src, err)
	}
	entries = sync.FilterEntries(entries, rules)

	// DetectHardLinks is skipped entirely, not just left to return no
	// groups, in two cases: hardLinks wasn't requested, or the platform
	// can't detect them at all (DetectHardLinks would always return
	// nothing there anyway, but skipping the call also skips the
	// Lstat-per-entry cost that would otherwise buy nothing).
	var groups []sync.HardLinkGroup
	if hardLinks && sync.HardLinksSupported() {
		groups, err = sync.DetectHardLinks(src, entries)
		if err != nil {
			return fmt.Errorf("detecting hard links in %q: %w", src, err)
		}
	}

	if err := sendFileList(rw, entries, groups); err != nil {
		return fmt.Errorf("sending file list: %w", err)
	}

	secondary := make(map[string]bool)
	for _, group := range groups {
		for _, path := range group[1:] {
			secondary[path] = true
		}
	}

	for _, entry := range entries {
		if entry.IsDir || entry.Mode&fs.ModeSymlink != 0 || secondary[entry.Path] {
			continue
		}

		sigMsg, err := recvSignature(rw)
		if err != nil {
			return fmt.Errorf("receiving signature for %q: %w", entry.Path, err)
		}
		// Both sides process the same file list in the same order, so
		// this should never actually mismatch - but checking it costs
		// nothing and turns a silent "wrong file's delta computed
		// against the wrong signature" corruption bug into a clear,
		// immediate error instead.
		if sigMsg.Path != entry.Path {
			return fmt.Errorf("signature arrived out of order: got %q, want %q", sigMsg.Path, entry.Path)
		}

		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(entry.Path)))
		if err != nil {
			return fmt.Errorf("reading %q: %w", entry.Path, err)
		}

		ops := sync.GenerateDelta(sigMsg.Sig, data)
		if err := sendDelta(rw, entry.Path, ops); err != nil {
			return fmt.Errorf("sending delta for %q: %w", entry.Path, err)
		}
	}

	return nil
}
