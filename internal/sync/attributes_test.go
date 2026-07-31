package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// wantPerm returns the permission bits Stat should actually report after
// applyPerms(path, mode) on the current platform. On POSIX this is just
// mode.Perm() - full fidelity. On Windows, os.Chmod can only toggle the
// read-only attribute: any owner-write bit (0200) present makes the file
// fully writable (reported as 0666), and its absence makes it read-only
// (reported as 0444) - there's no way to represent finer-grained POSIX
// permissions through the Win32 attribute model Go's os.Chmod uses there.
// This is a real, verified platform difference (confirmed by running the
// naive POSIX-only assertion here and watching it fail with 666 instead
// of the requested 600), not a gap in applyPerms itself.
func wantPerm(mode os.FileMode) os.FileMode {
	if runtime.GOOS != "windows" {
		return mode.Perm()
	}
	if mode.Perm()&0o200 != 0 {
		return 0o666
	}
	return 0o444
}

func TestApplyPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := applyPerms(path, 0o600); err != nil {
		t.Fatalf("applyPerms returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got, want := info.Mode().Perm(), wantPerm(0o600); got != want {
		t.Errorf("permissions = %o, want %o", got, want)
	}
}

func TestApplyPerms_IgnoresTypeBits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A FileEntry.Mode for a regular file with 0755 perms, as Lstat would
	// actually report it (no high type bits set for a plain file - but
	// this proves applyPerms takes a plain fs.FileMode and behaves
	// correctly on a value that's already realistic, not just a bare
	// octal literal).
	if err := applyPerms(path, os.FileMode(0o755)); err != nil {
		t.Fatalf("applyPerms returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got, want := info.Mode().Perm(), wantPerm(0o755); got != want {
		t.Errorf("permissions = %o, want %o", got, want)
	}
}

func TestApplyPerms_ReadOnlyOnWindowsWhenNoWriteBit(t *testing.T) {
	// Directly exercises the read-only side of Windows' reduced-fidelity
	// Chmod, rather than only ever testing modes that happen to include
	// a write bit.
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort: restore write permission so t.TempDir()'s own
		// cleanup can actually remove this file afterward.
		if err := os.Chmod(path, 0o644); err != nil {
			t.Logf("cleanup: failed to restore writable permissions on %s: %v", path, err)
		}
	})

	if err := applyPerms(path, 0o444); err != nil {
		t.Fatalf("applyPerms returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got, want := info.Mode().Perm(), wantPerm(0o444); got != want {
		t.Errorf("permissions = %o, want %o", got, want)
	}
}

func TestApplyTimes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Truncated to whole seconds: some filesystems (notably FAT variants,
	// and historically some others) don't store sub-second mtime
	// precision, so a nanosecond-precision want value could produce a
	// flaky round-trip mismatch that has nothing to do with applyTimes
	// itself being wrong.
	want := time.Date(2020, time.March, 15, 10, 30, 0, 0, time.UTC)

	if err := applyTimes(path, want); err != nil {
		t.Fatalf("applyTimes returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.ModTime(); !got.Equal(want) {
		t.Errorf("ModTime = %v, want %v", got, want)
	}
}

func TestApplyTimes_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	modTime := time.Date(2020, time.March, 15, 10, 30, 0, 0, time.UTC)

	if err := applyTimes(path, modTime); err != nil {
		t.Fatalf("applyTimes (1st call) returned error: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if err := applyTimes(path, modTime); err != nil {
		t.Fatalf("applyTimes (2nd call) returned error: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("ModTime changed between identical applyTimes calls: %v vs %v", first.ModTime(), second.ModTime())
	}
}

func TestApplyOwnership_SkippedWhenUnavailable(t *testing.T) {
	// This is a pure logic check, not dependent on syscall behavior or
	// privilege at all: applyOwnership must return before ever calling
	// Lchown when OwnershipAvailable is false, which is provable purely
	// from the returned (applied, skipped, err) tuple - no real file
	// ownership needs to change for this assertion to be meaningful.
	entry := FileEntry{UID: 0, GID: 0, OwnershipAvailable: false}

	applied, skipped, err := applyOwnership("/does/not/need/to/exist", entry, true, true)
	if err != nil {
		t.Fatalf("applyOwnership returned error: %v, want nil (should skip before touching the filesystem)", err)
	}
	if !skipped {
		t.Errorf("skipped = false, want true")
	}
	if applied {
		t.Errorf("applied = true, want false")
	}
}

func TestApplyOwnership_NoOpWhenNeitherRequested(t *testing.T) {
	entry := FileEntry{UID: 1000, GID: 1000, OwnershipAvailable: true}

	applied, skipped, err := applyOwnership("/does/not/need/to/exist", entry, false, false)
	if err != nil {
		t.Fatalf("applyOwnership returned error: %v, want nil", err)
	}
	if applied || skipped {
		t.Errorf("applied=%v skipped=%v, want both false when neither owner nor group was requested", applied, skipped)
	}
}

func TestApplyOwnership_AppliedWhenAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Lchown is not supported on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Chown to our own current uid/gid: even an unprivileged process can
	// always do this (you can always "change" ownership to yourself),
	// unlike chowning to an arbitrary other user, which needs root - so
	// this exercises the real syscall without requiring elevation.
	entry := FileEntry{
		UID:                uint32(os.Getuid()),
		GID:                uint32(os.Getgid()),
		OwnershipAvailable: true,
	}

	applied, skipped, err := applyOwnership(path, entry, true, true)
	if err != nil {
		t.Fatalf("applyOwnership returned error: %v", err)
	}
	if skipped {
		t.Errorf("skipped = true, want false")
	}
	if !applied {
		t.Errorf("applied = false, want true")
	}
}

// trySymlink creates a throwaway symlink to check whether this
// environment supports it at all (matching the same check TestWalk_Symlink
// in walk_test.go uses), skipping the calling test rather than failing it
// when unsupported - e.g. Windows without Developer Mode or elevation.
func trySymlink(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("target", filepath.Join(dir, "probe")); err != nil {
		t.Skipf("symlink creation unsupported in this environment: %v", err)
	}
}

func TestApplySymlink(t *testing.T) {
	trySymlink(t)

	dir := t.TempDir()
	destPath := filepath.Join(dir, "link")
	entry := FileEntry{
		Path:       "link",
		Mode:       os.ModeSymlink | 0o777,
		LinkTarget: "some-target-file", // no path separator - see applySymlink's doc comment on why LinkTarget isn't slash-normalized
	}

	if err := applySymlink(destPath, entry); err != nil {
		t.Fatalf("applySymlink returned error: %v", err)
	}

	info, err := os.Lstat(destPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destPath is not a symlink: Mode = %v", info.Mode())
	}
	target, err := os.Readlink(destPath)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != entry.LinkTarget {
		t.Errorf("link target = %q, want %q", target, entry.LinkTarget)
	}
}

func TestApplySymlink_ReplacesExistingEntry(t *testing.T) {
	trySymlink(t)

	dir := t.TempDir()
	destPath := filepath.Join(dir, "link")
	if err := os.WriteFile(destPath, []byte("old regular file content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entry := FileEntry{
		Path:       "link",
		Mode:       os.ModeSymlink | 0o777,
		LinkTarget: "new-target-file",
	}
	if err := applySymlink(destPath, entry); err != nil {
		t.Fatalf("applySymlink returned error: %v", err)
	}

	info, err := os.Lstat(destPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destPath is still a regular file, not replaced with a symlink")
	}
}

func TestApplySymlink_RejectsNonSymlinkEntry(t *testing.T) {
	dir := t.TempDir()
	entry := FileEntry{Path: "notalink", Mode: 0o644} // no ModeSymlink bit
	if err := applySymlink(filepath.Join(dir, "notalink"), entry); err == nil {
		t.Fatalf("applySymlink with a non-symlink entry returned nil error, want an error")
	}
}

func TestApplyAttributes_RegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	modTime := time.Date(2021, time.June, 1, 12, 0, 0, 0, time.UTC)
	entry := FileEntry{
		Path:    "file.txt",
		Mode:    0o600,
		ModTime: modTime,
	}

	result, err := ApplyAttributes(entry, path, AttrOptions{Perms: true, Times: true})
	if err != nil {
		t.Fatalf("ApplyAttributes returned error: %v", err)
	}
	if !result.PermsApplied || !result.TimesApplied {
		t.Errorf("result = %+v, want PermsApplied and TimesApplied both true", result)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got, want := info.Mode().Perm(), wantPerm(0o600); got != want {
		t.Errorf("permissions = %o, want %o", got, want)
	}
	if !info.ModTime().Equal(modTime) {
		t.Errorf("ModTime = %v, want %v", info.ModTime(), modTime)
	}
}

func TestApplyAttributes_DisabledOptionsAreNoOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	entry := FileEntry{
		Path:    "file.txt",
		Mode:    0o600, // deliberately different from the file's actual 0644
		ModTime: time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	result, err := ApplyAttributes(entry, path, AttrOptions{}) // everything off
	if err != nil {
		t.Fatalf("ApplyAttributes returned error: %v", err)
	}
	if result != (AttrResult{}) {
		t.Errorf("result = %+v, want the zero value when every option is disabled", result)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("permissions changed (%o -> %o) despite Perms being disabled", before.Mode().Perm(), after.Mode().Perm())
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("ModTime changed (%v -> %v) despite Times being disabled", before.ModTime(), after.ModTime())
	}
}

func TestApplyAttributes_SymlinkSkipsPermsAndTimes(t *testing.T) {
	trySymlink(t)

	dir := t.TempDir()
	destPath := filepath.Join(dir, "link")
	entry := FileEntry{
		Path:       "link",
		Mode:       os.ModeSymlink | 0o777,
		ModTime:    time.Date(2021, time.June, 1, 12, 0, 0, 0, time.UTC),
		LinkTarget: "some-target-file",
	}

	// Perms and Times are requested here, same as Links - proving they're
	// deliberately skipped for a symlink entry rather than accidentally
	// omitted from the test's opts.
	result, err := ApplyAttributes(entry, destPath, AttrOptions{Perms: true, Times: true, Links: true})
	if err != nil {
		t.Fatalf("ApplyAttributes returned error: %v", err)
	}
	if !result.LinkApplied {
		t.Errorf("LinkApplied = false, want true")
	}
	if result.PermsApplied || result.TimesApplied {
		t.Errorf("result = %+v, want PermsApplied and TimesApplied both false for a symlink entry", result)
	}

	info, err := os.Lstat(destPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destPath is not a symlink")
	}
}

func TestApplySymlink_RejectsEmptyLinkTarget(t *testing.T) {
	dir := t.TempDir()
	entry := FileEntry{Path: "link", Mode: os.ModeSymlink | 0o777, LinkTarget: ""}
	if err := applySymlink(filepath.Join(dir, "link"), entry); err == nil {
		t.Fatalf("applySymlink with an empty LinkTarget returned nil error, want an error")
	}
}
