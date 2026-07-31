package pipeline

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// Receiver runs the receiving side of a sync over rw: receives the
// sender's file list, then for each entry either creates it directly
// (directories, symlinks - both have everything they need already inside
// the FileEntry, no round trip required) or exchanges a signature/delta
// with the sender (regular files) to reconstruct its bytes, applying
// attributes per opts along the way.
//
// A destination file not mentioned in the sender's list is never touched
// at all: Receiver only ever acts on paths that appear in the received
// list, by construction - there's no separate destination-side walk to
// reconcile against it, so nothing here can delete or corrupt an
// unrelated file. (Full --delete semantics are explicitly out of scope.)
func Receiver(rw io.ReadWriter, dest string, opts sync.AttrOptions) error {
	entries, err := recvFileList(rw)
	if err != nil {
		return fmt.Errorf("receiving file list: %w", err)
	}

	// Directory attributes are deferred to a final pass below, applied
	// deepest-first, rather than immediately when each directory is
	// created: applying them immediately would have the filesystem
	// silently re-bump a directory's mtime the moment something is later
	// created inside it, undoing the very preservation just performed -
	// or, for a read-only permission mode, block creating those children
	// at all. Walk's own sort guarantees a parent directory's entry
	// always precedes its children's in entries, so collecting them here
	// in list order and processing that collection in reverse gives
	// children-before-parents for free, without a second sort.
	var dirEntries []sync.FileEntry

	for _, entry := range entries {
		destPath := filepath.Join(dest, filepath.FromSlash(entry.Path))

		switch {
		case entry.IsDir:
			if err := os.MkdirAll(destPath, 0o755); err != nil {
				return fmt.Errorf("creating directory %q: %w", entry.Path, err)
			}
			dirEntries = append(dirEntries, entry)
			continue

		case entry.Mode&fs.ModeSymlink != 0:
			if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
				return fmt.Errorf("creating parent directory for %q: %w", entry.Path, err)
			}
			if _, err := sync.ApplyAttributes(entry, destPath, opts); err != nil {
				return fmt.Errorf("creating symlink %q: %w", entry.Path, err)
			}
			continue
		}

		if err := receiveRegularFile(rw, destPath, entry, opts); err != nil {
			return err
		}
	}

	for i := len(dirEntries) - 1; i >= 0; i-- {
		entry := dirEntries[i]
		destPath := filepath.Join(dest, filepath.FromSlash(entry.Path))
		if _, err := sync.ApplyAttributes(entry, destPath, opts); err != nil {
			return fmt.Errorf("applying attributes to directory %q: %w", entry.Path, err)
		}
	}

	return nil
}

// receiveRegularFile handles one regular-file entry: computes a
// signature against whatever's currently at destPath (or an empty
// signature if nothing is - see below), sends it, receives the sender's
// delta, reconstructs the file, and applies attributes.
func receiveRegularFile(rw io.ReadWriter, destPath string, entry sync.FileEntry, opts sync.AttrOptions) error {
	oldData, err := os.ReadFile(destPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading existing %q: %w", entry.Path, err)
	}
	// oldData is nil when the file doesn't exist yet at the destination
	// (the new-file case). sync.GenerateSignature on nil/empty data
	// naturally produces a Signature with zero Blocks, which makes
	// sync.GenerateDelta emit a single all-DataOp delta (nothing to match
	// against) - exactly the "new file" behavior needed, falling directly
	// out of the existing SC-3 API with no special-casing required here.
	sig := sync.GenerateSignature(oldData)

	if err := sendSignature(rw, entry.Path, sig); err != nil {
		return fmt.Errorf("sending signature for %q: %w", entry.Path, err)
	}

	deltaPath, ops, err := recvDelta(rw)
	if err != nil {
		return fmt.Errorf("receiving delta for %q: %w", entry.Path, err)
	}
	if deltaPath != entry.Path {
		return fmt.Errorf("delta arrived out of order: got %q, want %q", deltaPath, entry.Path)
	}

	newData, err := sync.ApplyDelta(oldData, ops, sig)
	if err != nil {
		return fmt.Errorf("applying delta for %q: %w", entry.Path, err)
	}

	// Belt-and-suspenders: the entry's parent directory should already
	// exist by this point whenever it was itself part of the transfer
	// (Walk's sort guarantees it was created earlier in this same loop),
	// but MkdirAll is a cheap no-op when the directory is already there,
	// and this removes any fragile dependency on that ordering holding
	// for paths whose parent wasn't part of the list at all (e.g. dest
	// itself, for a non-recursive sync with no directory entries).
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("creating parent directory for %q: %w", entry.Path, err)
	}
	if err := os.WriteFile(destPath, newData, 0o644); err != nil {
		return fmt.Errorf("writing %q: %w", entry.Path, err)
	}

	if _, err := sync.ApplyAttributes(entry, destPath, opts); err != nil {
		return fmt.Errorf("applying attributes to %q: %w", entry.Path, err)
	}
	return nil
}
