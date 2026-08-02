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
//
// copts governs --compress/-z: entirely a sending-side decision (see
// CompressOptions' own doc comment) - Receiver needs no counterpart
// parameter at all, since decompression is driven purely by what each
// deltaMessage's own Compressed marker says, on every transport.
func Sender(rw io.ReadWriter, src string, walkOpts sync.WalkOptions, rules []sync.Rule, hardLinks bool, copts CompressOptions) error {
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

		if sigMsg.Append == appendSkip {
			// The receiver already decided this file doesn't need
			// touching at all (--append/--append-verify's "destination
			// not shorter than source" eligibility rule) - acknowledge
			// with an empty delta without even opening the file,
			// matching real rsync's own documented behavior of never
			// comparing such files at all.
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

// appendTailOps builds the delta for --append: a single CopyOp
// representing "trust the receiver's first trustedLen bytes exactly as
// they are, without verifying them at all" - a legitimate, direct use of
// CopyOp's own documented contract (it only ever claims to copy a block
// unchanged, never that the block was checksum-verified; that
// verification is a property of how sync.GenerateDelta happens to find
// a match, not of CopyOp itself), followed by a DataOp for whatever of
// data comes after that offset - literal bytes the receiver has never
// seen. Verified against real rsync's own source (generator.c/sender.c)
// for the underlying "trust the prefix, send only the tail" behavior.
//
// A source file that has shrunk below trustedLen since the receiver
// last checked its own file's length (a narrow, real race - real
// rsync calls this a "diminished" file and skips it with a warning,
// continuing the rest of the transfer) is treated as a hard error here
// instead: grsync's Receiver has no general "skip this one file, keep
// going" mechanism anywhere else in the codebase, and inventing one
// solely for this narrow race is a bigger change than this ticket
// calls for - see the README's Partial and Append Transfers section for
// this disclosed scope difference.
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
