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
// sends the resulting file list, then for each regular-file entry
// receives the receiver's signature, computes a delta against the
// current source bytes, and sends it back.
//
// Directories and symlinks are deliberately not part of this exchange at
// all: a directory has no byte content to diff, and a symlink's entire
// "content" is its LinkTarget, which already travels inside the FileEntry
// in the file list itself. Only regular files need a signature/delta
// round trip.
func Sender(rw io.ReadWriter, src string, walkOpts sync.WalkOptions, rules []sync.Rule) error {
	entries, err := sync.Walk(src, walkOpts)
	if err != nil {
		return fmt.Errorf("walking %q: %w", src, err)
	}
	entries = sync.FilterEntries(entries, rules)

	if err := sendFileList(rw, entries); err != nil {
		return fmt.Errorf("sending file list: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir || entry.Mode&fs.ModeSymlink != 0 {
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
