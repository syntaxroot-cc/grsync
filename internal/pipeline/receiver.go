package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// receiveContext bundles the per-call state shared across Receiver's
// helpers: the optional stats accumulator, progress reporter, and
// transfer-count bookkeeping for progress's "xfr#N, to-chk=M/T" line.
type receiveContext struct {
	attrOpts sync.AttrOptions
	ropts    ReceiverOptions

	stats    *Stats            // nil unless ropts.Stats
	progress *progressReporter // nil unless ropts.Progress (never during DryRun)

	totalFiles int // every entry in the received list, all types
	processed  int // entries handled so far, for filesLeft = totalFiles - processed
	xferNum    int // incremented each time a regular file's content actually changes
}

// Receiver runs the receiving side of a sync over rw: receives the
// sender's file list, then for each entry either creates it directly
// (directories, symlinks), exchanges a signature/delta with the sender
// (a regular file), or - for a hard-link group's non-first member - is
// relinked to that first member in a dedicated pass afterward. Attributes
// are applied per attrOpts along the way.
//
// ropts.DryRun turns every write into a no-op while all planning
// (signature/delta exchange, itemize comparison, stats) still runs, so
// reporting stays accurate. ropts.Progress is the one exception: it
// measures bytes actually committed to disk, so it never fires in a dry
// run.
//
// Receiver only ever touches paths present in the sender's file list;
// there is no destination-side walk, so nothing here can delete or
// modify an unrelated file (--delete is out of scope).
func Receiver(rw io.ReadWriter, dest string, attrOpts sync.AttrOptions, ropts ReceiverOptions) error {
	start := time.Now()

	// Wrapping rw lets Stats count bytes crossing the connection without
	// threading a length return through every send/recv helper.
	var counter *countingReadWriter
	if ropts.Stats {
		counter = &countingReadWriter{rw: rw}
		rw = counter
	}

	entries, groups, err := recvFileList(rw)
	if err != nil {
		return fmt.Errorf("receiving file list: %w", err)
	}

	ctx := &receiveContext{attrOpts: attrOpts, ropts: ropts, totalFiles: len(entries)}
	if ropts.Stats {
		ctx.stats = &Stats{}
	}
	if ropts.Progress && !ropts.DryRun {
		ctx.progress = newProgressReporter(ropts.output())
		defer ctx.progress.stop()
	}

	// secondary marks every hard-link group member except the first, which
	// is linked to it in the dedicated pass below instead of being written
	// out a second time.
	secondary := make(map[string]bool)
	for _, group := range groups {
		for _, path := range group[1:] {
			secondary[path] = true
		}
	}

	// Directory attributes are deferred to a final pass, applied
	// deepest-first: applying them as each directory is created would let
	// a later child re-bump the parent's mtime, or a read-only mode block
	// creating that child at all. entries is parent-before-child (Walk's
	// sort), so reversing this collected list gives children-before-parents
	// for free. The hard-link pass above runs first for the same reason:
	// os.Link also touches the parent directory's mtime.
	var dirEntries []sync.FileEntry

	for _, entry := range entries {
		destPath := filepath.Join(dest, filepath.FromSlash(entry.Path))

		switch {
		case entry.IsDir:
			if err := receiveDir(ctx, destPath, entry); err != nil {
				return err
			}
			dirEntries = append(dirEntries, entry)
			ctx.processed++
			continue

		case entry.Mode&fs.ModeSymlink != 0:
			if err := receiveSymlink(ctx, destPath, entry); err != nil {
				return err
			}
			ctx.processed++
			continue

		case secondary[entry.Path]:
			reportChange(ropts, itemizeHardLink(), true, entry)
			ctx.processed++
			continue
		}

		if err := receiveRegularFile(ctx, rw, destPath, entry); err != nil {
			return err
		}
		ctx.processed++
	}

	if !ropts.DryRun {
		for _, group := range groups {
			if err := sync.ApplyHardLinks(dest, group); err != nil {
				return fmt.Errorf("linking hard-link group starting at %q: %w", group[0], err)
			}
		}
	}

	if !ropts.DryRun {
		for i := len(dirEntries) - 1; i >= 0; i-- {
			entry := dirEntries[i]
			destPath := filepath.Join(dest, filepath.FromSlash(entry.Path))
			if _, err := sync.ApplyAttributes(entry, destPath, attrOpts); err != nil {
				return fmt.Errorf("applying attributes to directory %q: %w", entry.Path, err)
			}
		}
	}

	if ropts.Stats {
		ctx.stats.Elapsed = time.Since(start)
		if counter != nil {
			ctx.stats.BytesSent = counter.written
			ctx.stats.BytesReceived = counter.read
		}
		_, _ = fmt.Fprint(ropts.output(), formatStats(*ctx.stats, ropts.DryRun))
	}

	return nil
}

// receiveDir creates one directory entry (unless dry-run) and reports its
// itemize line based on what existed at destPath before creation.
func receiveDir(ctx *receiveContext, destPath string, entry sync.FileEntry) error {
	old, existed, err := lstatExisting(destPath)
	if err != nil {
		return fmt.Errorf("checking existing %q: %w", entry.Path, err)
	}

	if !ctx.ropts.DryRun {
		if err := os.MkdirAll(destPath, 0o755); err != nil {
			return fmt.Errorf("creating directory %q: %w", entry.Path, err)
		}
	}

	if ctx.stats != nil {
		ctx.stats.Directories++
		if !existed {
			ctx.stats.CreatedDirectories++
		}
	}

	code, report := itemizeDir(entry, old, existed, ctx.attrOpts)
	reportChange(ctx.ropts, code, report, entry)
	return nil
}

// receiveSymlink handles one symlink entry. Guarded on attrOpts.Links,
// matching sync.ApplyAttributes' own no-op behavior without --links, so
// nothing is written, reported, or counted either.
func receiveSymlink(ctx *receiveContext, destPath string, entry sync.FileEntry) error {
	if !ctx.attrOpts.Links {
		return nil
	}

	old, existed, err := lstatExisting(destPath)
	if err != nil {
		return fmt.Errorf("checking existing %q: %w", entry.Path, err)
	}

	if !ctx.ropts.DryRun {
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("creating parent directory for %q: %w", entry.Path, err)
		}
		if _, err := sync.ApplyAttributes(entry, destPath, ctx.attrOpts); err != nil {
			return fmt.Errorf("creating symlink %q: %w", entry.Path, err)
		}
	}

	if ctx.stats != nil {
		ctx.stats.Symlinks++
		if !existed {
			ctx.stats.CreatedSymlinks++
		}
	}

	code, report := itemizeSymlink(entry, old, existed, ctx.attrOpts)
	reportChange(ctx.ropts, code, report, entry)
	return nil
}

// receiveRegularFile handles one regular-file entry: computes a
// signature against destPath's current bytes, sends it, receives the
// sender's delta, and reconstructs the new content. This all runs even in
// dry-run mode, since it's exactly the work needed for accurate
// itemize/stats output; only the final write is skipped.
//
// --append/--append-verify apply only to an existing, shorter destination
// file; a new or already-as-long destination is handled (or skipped)
// exactly as without either flag. For an eligible file, --append blindly
// trusts the existing prefix and has Sender send only the literal tail;
// --append-verify instead runs the normal signature/delta/apply pipeline,
// verifying the prefix like any other sync.
func receiveRegularFile(ctx *receiveContext, rw io.ReadWriter, destPath string, entry sync.FileEntry) error {
	old, existed, err := lstatExisting(destPath)
	if err != nil {
		return fmt.Errorf("checking existing %q: %w", entry.Path, err)
	}

	// usedPartialBasis is never true under append mode: append trusts or
	// verifies the real destination file's own bytes, not a --partial-dir
	// staging file's content.
	usedPartialBasis := false
	var oldData []byte
	if !ctx.ropts.AppendMode() {
		if data, ok := loadPartialBasis(destPath, ctx.ropts, entry.Path); ok {
			oldData, usedPartialBasis = data, true
		}
	}
	if !usedPartialBasis {
		oldData, err = os.ReadFile(destPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reading existing %q: %w", entry.Path, err)
		}
	}
	// oldData is nil for a brand-new file; GenerateSignature on nil
	// naturally yields a zero-block signature, and GenerateDelta a single
	// all-DataOp delta, with no special-casing needed here.

	action := appendNone
	var sig sync.Signature
	switch {
	case !ctx.ropts.AppendMode() || !existed:
		sig = sync.GenerateSignature(oldData)
	case int64(len(oldData)) >= entry.Size:
		// Destination already at least as long as source: skip without
		// computing a signature for data we'll never consult.
		action = appendSkip
	case ctx.ropts.AppendVerify:
		sig = sync.GenerateSignature(oldData)
	default: // ctx.ropts.Append, and genuinely shorter than entry.Size
		action = appendTail
		// BlockSize carries the trusted append offset, not a real block
		// size; Blocks is left empty since Sender never reads it here.
		sig = sync.Signature{BlockSize: len(oldData)}
	}

	if err := sendSignature(rw, entry.Path, sig, action); err != nil {
		return fmt.Errorf("sending signature for %q: %w", entry.Path, err)
	}

	deltaPath, ops, err := recvDelta(rw)
	if err != nil {
		return fmt.Errorf("receiving delta for %q: %w", entry.Path, err)
	}
	if deltaPath != entry.Path {
		return fmt.Errorf("delta arrived out of order: got %q, want %q", deltaPath, entry.Path)
	}

	var newData []byte
	if action == appendSkip {
		// Unconditional skip leaves the destination untouched: newData is
		// oldData verbatim, not the result of applying an (empty) delta.
		newData = oldData
	} else {
		newData, err = sync.ApplyDelta(oldData, ops, sig)
		if err != nil {
			return fmt.Errorf("applying delta for %q: %w", entry.Path, err)
		}
	}
	// ApplyDelta is pure in-memory work, so computing it (and this
	// comparison) is free even when dry-run means it's never written.
	contentChanged := action != appendSkip && !bytes.Equal(oldData, newData)
	// transferred isn't just contentChanged: a brand-new empty file has
	// oldData == newData == nil, so contentChanged alone would miss it.
	transferred := action != appendSkip && (!existed || contentChanged)

	if ctx.stats != nil {
		literal, matched := deltaByteCounts(ops, sig.BlockSize, len(oldData))
		ctx.stats.RegularFiles++
		ctx.stats.TotalFileSize += entry.Size
		ctx.stats.LiteralData += literal
		ctx.stats.MatchedData += matched
		if !existed {
			ctx.stats.CreatedRegularFiles++
		}
		if transferred {
			ctx.stats.RegularFilesTransferred++
			ctx.stats.TotalTransferredFileSize += entry.Size
		}
	}

	if !ctx.ropts.DryRun {
		// MkdirAll is a no-op if the parent already exists; this covers
		// paths whose parent wasn't itself part of the transferred list
		// (e.g. dest itself, for a non-recursive sync).
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("creating parent directory for %q: %w", entry.Path, err)
		}

		if transferred {
			ctx.xferNum++
			tmpPath, werr := writeToTempFileWithProgress(destPath, newData, ctx.progress, entry.Path, ctx.xferNum, ctx.totalFiles, ctx.totalFiles-ctx.processed-1)
			if err := finishRegularFileWrite(tmpPath, destPath, werr, ctx.ropts, entry.Path, usedPartialBasis); err != nil {
				return fmt.Errorf("writing %q: %w", entry.Path, err)
			}
		}
		// Attributes are applied even when not transferred: real rsync
		// still updates a file's non-content attributes when its content
		// didn't need to change.
		if _, err := sync.ApplyAttributes(entry, destPath, ctx.attrOpts); err != nil {
			return fmt.Errorf("applying attributes to %q: %w", entry.Path, err)
		}
	}

	code, report := itemizeFile(entry, old, existed, contentChanged, ctx.attrOpts)
	reportChange(ctx.ropts, code, report, entry)
	return nil
}

// deltaByteCounts sums a delta's literal (DataOp) and matched (CopyOp)
// bytes, using the same block-boundary math as sync.ApplyDelta.
func deltaByteCounts(ops []sync.DeltaOp, blockSize int, oldDataLen int) (literal, matched int64) {
	for _, op := range ops {
		switch o := op.(type) {
		case sync.DataOp:
			literal += int64(len(o.Bytes))
		case sync.CopyOp:
			start := o.BlockIndex * blockSize
			end := start + blockSize
			if end > oldDataLen {
				end = oldDataLen
			}
			if end > start {
				matched += int64(end - start)
			}
		}
	}
	return literal, matched
}

// lstatExisting is sync.LstatEntry with "not found" turned into a plain
// (zero value, false, nil) result instead of an error to unwrap.
func lstatExisting(path string) (entry sync.FileEntry, existed bool, err error) {
	entry, err = sync.LstatEntry(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sync.FileEntry{}, false, nil
		}
		return sync.FileEntry{}, false, err
	}
	return entry, true, nil
}
