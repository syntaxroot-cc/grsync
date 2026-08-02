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
// detects hard links (if hardLinks is true), sends the resulting file
// list, then for each regular-file entry that isn't a hard-link group's
// secondary member, receives the receiver's signature, computes a delta
// against the current source bytes, and sends it back.
//
// Directories and symlinks never exchange a signature/delta: a directory
// has no content to diff, and a symlink's content already travels as its
// LinkTarget in the file list. A hard-link group's secondary members are
// skipped too, since they're byte-identical to the first member by
// definition; the receiver relinks them instead via sync.ApplyHardLinks.
//
// hardLinks mirrors real rsync's -H flag: off by default and not implied
// by --archive, since detecting hard links costs an extra Lstat per entry.
func Sender(rw io.ReadWriter, src string, walkOpts sync.WalkOptions, rules []sync.Rule, hardLinks bool, copts CompressOptions) error {
	entries, err := sync.Walk(src, walkOpts)
	if err != nil {
		return fmt.Errorf("walking %q: %w", src, err)
	}
	entries = sync.FilterEntries(entries, rules)

	// Skipped entirely (not just left to return nothing) when hardLinks is
	// off or the platform can't detect them, avoiding a wasted
	// Lstat-per-entry cost.
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
		// Both sides process the same file list in the same order; this
		// check turns a silent delta/signature mismatch into a clear error.
		if sigMsg.Path != entry.Path {
			return fmt.Errorf("signature arrived out of order: got %q, want %q", sigMsg.Path, entry.Path)
		}

		if sigMsg.Append == appendSkip {
			// The receiver already decided this file needs no comparison
			// at all; acknowledge with an empty delta without opening it.
			if err := sendDelta(rw, entry.Path, nil, copts); err != nil {
				return fmt.Errorf("sending delta for %q: %w", entry.Path, err)
			}
			continue
		}

		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(entry.Path)))
		if err != nil {
			return fmt.Errorf("reading %q: %w", entry.Path, err)
		}

		var ops []sync.DeltaOp
		if sigMsg.Append == appendTail {
			ops, err = appendTailOps(sigMsg.Sig.BlockSize, data)
			if err != nil {
				return fmt.Errorf("preparing append tail for %q: %w", entry.Path, err)
			}
		} else {
			ops = sync.GenerateDelta(sigMsg.Sig, data)
		}

		if err := sendDelta(rw, entry.Path, ops, copts); err != nil {
			return fmt.Errorf("sending delta for %q: %w", entry.Path, err)
		}
	}

	return nil
}

// appendTailOps builds the --append delta: a CopyOp{BlockIndex: 0}
// trusting the receiver's first trustedLen bytes unverified, followed by
// a DataOp for whatever of data comes after that offset.
//
// A source file that has shrunk below trustedLen (real rsync calls this a
// "diminished" file and skips it with a warning, continuing the transfer)
// is a hard error here instead: grsync has no general per-file
// skip-and-continue mechanism.
func appendTailOps(trustedLen int, data []byte) ([]sync.DeltaOp, error) {
	if trustedLen > len(data) {
		return nil, fmt.Errorf("source file shrank to %d bytes, below the %d bytes already trusted on the receiving side (a \"diminished\" file - see real rsync's own --append docs)", len(data), trustedLen)
	}
	var ops []sync.DeltaOp
	if trustedLen > 0 {
		ops = append(ops, sync.CopyOp{BlockIndex: 0})
	}
	if tail := data[trustedLen:]; len(tail) > 0 {
		ops = append(ops, sync.DataOp{Bytes: tail})
	}
	return ops, nil
}
