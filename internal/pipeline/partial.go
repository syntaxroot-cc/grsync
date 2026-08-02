// partial.go implements --partial/--partial-dir.
//
// grsync's wire protocol has no streaming I/O: a file's delta arrives as
// one atomic frame, fully decoded before any byte is written to disk. So
// --partial here is file-granularity, not real rsync's byte-level
// mid-file resumption - it decides which whole files survive an aborted
// multi-file sync, not how a half-written single file is left.
//
// Writing a file's new content must itself be all-or-nothing from the
// destination's point of view, so every regular file is written to a temp
// file first and atomically renamed into place; --partial/--partial-dir
// only control what happens to that temp file if the transfer aborts
// before the rename.

package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
)

// createTempFile creates a uniquely-named temp file next to destPath
// (same directory, so the eventual rename is same-filesystem and atomic).
// Its mode is set to 0644 to match os.WriteFile's own default, since
// os.CreateTemp's 0600 default would otherwise become the new no-perms
// default whenever --perms doesn't override it later.
func createTempFile(destPath string) (*os.File, error) {
	dir := filepath.Dir(destPath)
	base := filepath.Base(destPath)
	f, err := os.CreateTemp(dir, "."+base+".*.grsync-tmp")
	if err != nil {
		return nil, err
	}
	// Best-effort: some exotic filesystems can't chmod an open file, and
	// this only matters when --perms is not given (sync.ApplyAttributes
	// fixes it otherwise), so a failure here isn't worth aborting over.
	_ = f.Chmod(0o644)
	return f, nil
}

// writeToTempFileWithProgress writes data to a fresh temp file next to
// destPath, chunked with progress updates when progress is non-nil.
// tmpPath is always returned, even on error, so the caller can still apply
// Partial/PartialDir policy to whatever was actually written.
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
// partialDir, mirroring real rsync's placement rule: a relative
// partialDir is created inside each file's own destination directory; an
// absolute one is a single shared directory, mirroring relPath's full
// relative path underneath it so same-basename files in different
// subdirectories can't collide.
func partialFilePath(destPath, partialDir, relPath string) string {
	if filepath.IsAbs(partialDir) {
		return filepath.Join(partialDir, filepath.FromSlash(relPath))
	}
	return filepath.Join(filepath.Dir(destPath), partialDir, filepath.Base(destPath))
}

// loadPartialBasis returns relPath's partial file content, if PartialDir
// is set and a usable file is there, to use as the delta comparison basis
// instead of the real destination file. ok is false whenever there's
// nothing usable to resume from, and the caller falls back to the real
// destination file.
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

// finishRegularFileWrite is receiveRegularFile's single exit point after
// writeToTempFileWithProgress: on success it renames tmpPath onto
// destPath and removes a used partial-dir basis file; on any failure it
// hands off to abandonOrKeep and returns the original error, since
// honoring Partial is never more important than surfacing why the
// transfer failed.
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
// transfer didn't complete, then returns origErr unchanged. Every exit
// path removes tmpPath from its original location - deleted, or moved to
// partial-dir/destPath - so a --partial-dir user's real destination tree
// is never touched by an aborted transfer.
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
