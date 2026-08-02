// partial.go implements --partial/--partial-dir.
//
// grsync's wire protocol has no streaming I/O (see sync.ApplyDelta's own
// doc comment): one regular file's delta arrives as a single, atomic
// gob-encoded frame, fully decoded into memory before a single byte of
// it is written to disk. That means there is no such thing as "half of
// this file's delta arrived" - a dropped connection mid-frame just means
// this file's transfer never started at all, and everything already
// written for *earlier* files in the same sync stays exactly as
// complete as it already was. --partial in grsync is therefore
// file-granularity, not real rsync's true byte-level mid-file
// resumption: "partial" describes which whole files survive an aborted
// multi-file sync, not a partially-written single file left in a
// half-complete state.
//
// That distinction only holds, though, if writing a file's new content
// is itself all-or-nothing from the destination's point of view - and
// before this ticket, it wasn't: receiveRegularFile wrote straight to
// destPath (os.WriteFile / a chunked os.OpenFile+Write loop for
// progress reporting), so a process killed mid-write left a genuinely
// truncated, corrupted file sitting at the real destination path, with
// no flag able to prevent or recover from it. Implementing --partial
// correctly requires the prerequisite real rsync itself always has: a
// separate temp file, written first, then atomically renamed into
// place only once it's complete. That temp-file+rename path is now
// unconditional (see receiver.go), not something --partial turns on -
// --partial/--partial-dir only control what happens to that temp file
// if the transfer aborts before the rename.

package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
)

// createTempFile creates a new, uniquely-named temp file in the same
// directory as destPath - same-directory, not a system temp dir, so the
// eventual rename into place is same-filesystem (and therefore atomic on
// every platform this project supports). The returned file's mode is
// explicitly set to 0644 (best-effort - see the inline comment) to match
// what a file written without --perms has always ended up as
// (os.WriteFile's own default), since os.CreateTemp's own default of
// 0600 would otherwise quietly become the new no-perms default the
// moment nothing later overrides it via sync.ApplyAttributes.
func createTempFile(destPath string) (*os.File, error) {
	dir := filepath.Dir(destPath)
	base := filepath.Base(destPath)
	f, err := os.CreateTemp(dir, "."+base+".*.grsync-tmp")
	if err != nil {
		return nil, err
	}
	// Best-effort: a handful of exotic filesystems don't support
	// changing an open file's mode the same way a normal POSIX one
	// does. Getting this wrong only matters when --perms is NOT given
	// (sync.ApplyAttributes overwrites it correctly whenever --perms
	// IS given, after the rename below), so a failure here is not worth
	// aborting the whole transfer over.
	_ = f.Chmod(0o644)
	return f, nil
}

// writeToTempFileWithProgress is writeFileWithProgress's SC-12
// counterpart: identical chunking/progress-reporting behavior (see its
// own doc comment for why nil-progress and small files both skip
// chunking), but targets a fresh temp file next to destPath instead of
// writing destPath directly. tmpPath is always returned, even when err
// != nil, so the caller can still apply Partial/PartialDir policy to
// however much was actually written before the failure.
func writeToTempFileWithProgress(destPath string, data []byte, progress *progressReporter, path string, xferNum, totalFiles, filesLeft int) (tmpPath string, err error) {
	f, err := createTempFile(destPath)
	if err != nil {
		return "", err
	}
	tmpPath = f.Name()
	defer func() { _ = f.Close() }()

	if progress == nil || len(data) <= progressWriteChunkSize {
		if _, err := f.Write(data); err != nil {
			return tmpPath, err
		}
		if progress != nil {
			progress.report(progressUpdate{
				path: path, bytesDone: int64(len(data)), fileSize: int64(len(data)), done: true,
				xferNum: xferNum, totalFiles: totalFiles, filesLeft: filesLeft,
			})
		}
		return tmpPath, nil
	}

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
			return tmpPath, werr
		}
		progress.report(progressUpdate{
			path: path, bytesDone: written, fileSize: total, done: written == total,
			xferNum: xferNum, totalFiles: totalFiles, filesLeft: filesLeft,
		})
	}
	return tmpPath, nil
}

// partialFilePath computes where relPath's partial file lives under
// ropts.PartialDir, mirroring real rsync's own documented placement
// rule: a relative PartialDir is created inside *each file's own
// destination directory* ("this makes it easy to use a relative path...
// to have rsync create the partial-directory in the destination file's
// directory"), so files with the same basename in different
// subdirectories can never collide with each other. An absolute
// PartialDir is a single shared directory, so relPath's full relative
// path is mirrored underneath it instead - real rsync's docs don't spell
// out this exact collision-avoidance scheme for the absolute case, but
// mirroring the relative path is the only scheme that can't collide
// between two files sharing a basename in different subdirectories.
func partialFilePath(destPath, partialDir, relPath string) string {
	if filepath.IsAbs(partialDir) {
		return filepath.Join(partialDir, filepath.FromSlash(relPath))
	}
	return filepath.Join(filepath.Dir(destPath), partialDir, filepath.Base(destPath))
}

// loadPartialBasis returns the content of relPath's partial file, if
// ropts.PartialDir is set and a usable file is actually there - used in
// place of the real destination file's own content as the delta
// comparison basis, giving a resumed transfer real content-level
// speedup (see delta.go's own algorithm: a signature built from a
// partial file's already-correct prefix naturally yields CopyOps for
// whatever still matches and DataOps only for the genuinely new tail).
// ok is false whenever there's nothing usable to resume from - PartialDir
// unset, no file there, or an unreadable one - in which case the caller
// falls back to the real destination file exactly as it always has.
func loadPartialBasis(destPath string, ropts ReceiverOptions, relPath string) (data []byte, ok bool) {
	if ropts.PartialDir == "" {
		return nil, false
	}
	data, err := os.ReadFile(partialFilePath(destPath, ropts.PartialDir, relPath))
	if err != nil {
		return nil, false
	}
	return data, true
}

// finishRegularFileWrite is receiveRegularFile's single exit point for
// everything written by writeToTempFileWithProgress: on success
// (writeErr == nil), it commits by renaming tmpPath onto destPath and,
// if a partial-dir file was used as this transfer's basis, removes it
// (real rsync's own documented "delete it after it has served its
// purpose"). On any failure - the write itself, or the commit rename -
// it hands off to abandonOrKeep to apply Partial/PartialDir policy, then
// returns the ORIGINAL error (a failure to honor Partial is never more
// important than surfacing why the transfer actually failed).
func finishRegularFileWrite(tmpPath, destPath string, writeErr error, ropts ReceiverOptions, relPath string, usedPartialBasis bool) error {
	if writeErr == nil {
		if err := os.Rename(tmpPath, destPath); err != nil {
			return abandonOrKeep(tmpPath, destPath, ropts, relPath, fmt.Errorf("renaming into place: %w", err))
		}
		if usedPartialBasis {
			_ = os.Remove(partialFilePath(destPath, ropts.PartialDir, relPath)) // best-effort: a stale leftover here is harmless, just wasted disk space
		}
		return nil
	}
	return abandonOrKeep(tmpPath, destPath, ropts, relPath, writeErr)
}

// abandonOrKeep applies Partial/PartialDir policy to a temp file whose
// transfer did not complete successfully, then returns origErr
// unchanged so the caller's own error still propagates. Every exit path
// here ends with tmpPath gone from its original same-directory-as-dest
// location - either deleted outright, or moved somewhere else entirely
// (partial-dir or destPath itself) - so a temp file this function has
// handled is never left sitting next to the destination under its own
// ".name.RANDOM.grsync-tmp" name, regardless of which policy applies.
//
// This is also the self-review's own "could a partial file leak outside
// --partial-dir" answer made concrete: whenever ropts.PartialDir is set,
// the only two destinations tmpPath can end up at are partialFilePath's
// own return value or deletion - destPath itself is never touched by
// this function in that case, so a --partial-dir user's real destination
// tree is never modified by an aborted transfer at all.
func abandonOrKeep(tmpPath, destPath string, ropts ReceiverOptions, relPath string, origErr error) error {
	if !ropts.KeepPartial() {
		_ = os.Remove(tmpPath)
		return origErr
	}

	target := destPath
	if ropts.PartialDir != "" {
		target = partialFilePath(destPath, ropts.PartialDir, relPath)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.Remove(tmpPath)
			return origErr
		}
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath) // never leave a stray temp file behind just because the "keep" step itself also failed
	}
	return origErr
}
