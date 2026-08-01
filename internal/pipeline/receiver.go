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

// receiveContext bundles the per-call state Receiver's helpers need
// beyond the read-only attrOpts/ropts every one of them already took:
// the optional stats accumulator, the optional progress reporter, and
// the running transfer-count bookkeeping progress's completion line
// needs (real rsync's own "xfr#N, to-chk=M/T"). Introduced here rather
// than adding yet more individual parameters to receiveDir/
// receiveSymlink/receiveRegularFile's already-substantial signatures.
type receiveContext struct {
	attrOpts sync.AttrOptions
	ropts    ReceiverOptions

	stats    *Stats            // nil unless ropts.Stats
	progress *progressReporter // nil unless ropts.Progress (and never during DryRun - see writeFileWithProgress)

	totalFiles int // every entry in the received list, all types
	processed  int // entries handled so far, for filesLeft = totalFiles - processed
	xferNum    int // incremented each time a regular file's content actually changes
}

// Receiver runs the receiving side of a sync over rw: receives the
// sender's file list (with its hard-link grouping), then for each entry
// either creates it directly (directories, symlinks - both have
// everything they need already inside the FileEntry, no round trip
// required), exchanges a signature/delta with the sender (a regular file
// that is either unlinked or the first-seen member of a hard-link
// group), or - for every other member of a hard-link group - is skipped
// here entirely and instead recreated as a real hard link once its
// group's first member has been fully written, in the dedicated pass
// below. Attributes are applied per attrOpts along the way.
//
// ropts.DryRun makes every one of those write points a no-op while every
// planning step (signature/delta exchange, hard-link grouping, itemize
// comparison against the destination's current state, stats
// accumulation) still runs exactly as it would for a real transfer - see
// the individual receive* helpers below for the specific guarded calls,
// all eight of them audited: two os.MkdirAll calls and an os.WriteFile in
// receiveRegularFile, an os.MkdirAll and sync.ApplyAttributes (which
// itself calls os.Symlink for a symlink entry, not just chmod/chtimes)
// in receiveSymlink, sync.ApplyAttributes for a directory in the
// deferred pass below, and sync.ApplyHardLinks in the hard-link pass
// below. ropts.Progress is the one exception that does *not* fire during
// a dry run: it specifically measures bytes committed to disk (see
// progress.go's own doc comment for why, given grsync's non-streaming
// wire protocol), and dry-run skips that write entirely, so there is
// nothing for it to report.
//
// A destination file not mentioned in the sender's list is never touched
// at all: Receiver only ever acts on paths that appear in the received
// list, by construction - there's no separate destination-side walk to
// reconcile against it, so nothing here can delete or corrupt an
// unrelated file. (Full --delete semantics are explicitly out of scope.)
func Receiver(rw io.ReadWriter, dest string, attrOpts sync.AttrOptions, ropts ReceiverOptions) error {
	start := time.Now()

	// Stats needs to know how many bytes crossed this connection; wrapping
	// rw itself (rather than threading a length return value through every
	// send/recv helper in messages.go) means Sender and its own tests need
	// no changes at all for this - see countingReadWriter's own doc
	// comment.
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

	// secondary marks every hard-link group member except the first:
	// group[0] is written normally, like any other regular file, and
	// every other member is linked to it in the dedicated pass below
	// instead of being written out a second time.
	secondary := make(map[string]bool)
	for _, group := range groups {
		for _, path := range group[1:] {
			secondary[path] = true
		}
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
	// children-before-parents for free, without a second sort. The
	// hard-link pass runs before this one for the same reason: os.Link
	// creates a new directory entry too, and doing that after a
	// directory's final mtime was already set would bump it right back.
	// (A directory's *itemize* comparison still happens at first
	// encounter, below, before anything is created inside it - only the
	// attribute *application* is deferred.)
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

// receiveDir handles one directory entry: creates it (unless dry-run),
// and reports its itemize line based on comparing entry against whatever
// already existed at destPath *before* that creation - the comparison
// itself is read-only (sync.LstatEntry), so it runs identically whether
// or not the MkdirAll below actually happens.
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

// receiveSymlink handles one symlink entry. Guarded on attrOpts.Links up
// front, matching sync.ApplyAttributes' own behavior exactly: without
// --links, a symlink entry is already a silent no-op there (see its doc
// comment), so there is nothing to write and nothing to report or count
// either - this short-circuit keeps that consistent rather than
// reporting or counting a change that sync.ApplyAttributes would never
// actually have made.
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
// signature against whatever's currently at destPath (or an empty
// signature if nothing is - see below), sends it, receives the sender's
// delta, and reconstructs what the file's bytes would become - all of
// this runs identically in dry-run mode, since it's exactly the planning
// work needed to report accurate itemize/stats output; only the final
// write (os.WriteFile/os.MkdirAll/sync.ApplyAttributes, and any progress
// reporting about it) is skipped.
func receiveRegularFile(ctx *receiveContext, rw io.ReadWriter, destPath string, entry sync.FileEntry) error {
	old, existed, err := lstatExisting(destPath)
	if err != nil {
		return fmt.Errorf("checking existing %q: %w", entry.Path, err)
	}

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
	// ApplyDelta is pure in-memory work - computing it, and this
	// comparison, costs nothing extra and runs the same whether or not
	// the result is about to be written, which is exactly what lets
	// dry-run report a genuinely correct "did the content change" bit
	// without a --checksum-style flag or a quick-check shortcut this
	// codebase doesn't otherwise have (see the README's Dry-Run Mode
	// section for why that's a deliberate, disclosed choice, not an
	// oversight).
	contentChanged := !bytes.Equal(oldData, newData)
	// transferred is deliberately not just contentChanged: a brand-new
	// *empty* file has oldData == nil and newData == nil (ApplyDelta's
	// accumulator is never appended to when there are no ops at all), so
	// bytes.Equal(nil, nil) is true and contentChanged alone would miss
	// it - even though creating a new file, empty or not, is exactly the
	// kind of thing "Number of regular files transferred" is supposed to
	// count. itemizeFile doesn't have this problem since it checks
	// !existed as its own first, higher-priority branch (see its own
	// code) - this mirrors that same priority for stats/xfer purposes.
	transferred := !existed || contentChanged

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

		if transferred {
			ctx.xferNum++
		}
		if err := writeFileWithProgress(destPath, newData, ctx.progress, entry.Path, ctx.xferNum, ctx.totalFiles, ctx.totalFiles-ctx.processed-1); err != nil {
			return fmt.Errorf("writing %q: %w", entry.Path, err)
		}
		if _, err := sync.ApplyAttributes(entry, destPath, ctx.attrOpts); err != nil {
			return fmt.Errorf("applying attributes to %q: %w", entry.Path, err)
		}
	}

	code, report := itemizeFile(entry, old, existed, contentChanged, ctx.attrOpts)
	reportChange(ctx.ropts, code, report, entry)
	return nil
}

// deltaByteCounts sums a delta's DataOp bytes (literal, unmatched data)
// and CopyOp bytes (matched, copied from the old file) - the same block-
// boundary math sync.ApplyDelta itself uses (including the final block
// potentially being shorter than blockSize), computed here rather than
// having ApplyDelta report it directly, since Stats is a pipeline-level
// concern ApplyDelta itself has no reason to know about.
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

// writeFileWithProgress writes data to destPath, reporting progress
// through progress (nil-safe: with nil progress - including always
// during a dry run, see Receiver's own doc comment - this behaves as a
// plain os.WriteFile with no extra syscalls). Small files are written in
// a single call even with progress enabled - chunking a handful of
// bytes would only add syscall overhead for a duration too short to
// ever be visibly "in progress" - so chunking only kicks in once data
// exceeds progressWriteChunkSize.
func writeFileWithProgress(destPath string, data []byte, progress *progressReporter, path string, xferNum, totalFiles, filesLeft int) error {
	if progress == nil || len(data) <= progressWriteChunkSize {
		if err := os.WriteFile(destPath, data, 0o644); err != nil {
			return err
		}
		if progress != nil {
			progress.report(progressUpdate{
				path: path, bytesDone: int64(len(data)), fileSize: int64(len(data)), done: true,
				xferNum: xferNum, totalFiles: totalFiles, filesLeft: filesLeft,
			})
		}
		return nil
	}

	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	total := int64(len(data))
	var written int64
	for written < total {
		end := written + progressWriteChunkSize
		if end > total {
			end = total
		}
		n, werr := f.Write(data[written:end])
		written += int64(n)
		if werr != nil {
			return werr
		}
		progress.report(progressUpdate{
			path: path, bytesDone: written, fileSize: total, done: written == total,
			xferNum: xferNum, totalFiles: totalFiles, filesLeft: filesLeft,
		})
	}
	return nil
}

// lstatExisting is sync.LstatEntry with the "not found" case turned into
// a plain (zero value, false, nil) result instead of an error the caller
// has to unwrap - every call site here wants exactly that: "did
// something already exist, and if so what was it," never treating
// nonexistence itself as a failure.
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
