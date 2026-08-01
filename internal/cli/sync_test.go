package cli

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// TestE2E_LocalToLocal drives the real CLI command - the same code path
// an actual user invocation goes through, not just the internal
// pipeline functions directly (those are already covered by
// internal/pipeline's own tests) - and confirms the destination matches
// the source byte-for-byte and attribute-for-attribute.
func TestE2E_LocalToLocal(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWriteFile(t, filepath.Join(src, "top.txt"), "top level content")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content, a bit longer than the top-level file")

	symlinksSupported := true
	if err := os.Symlink("nested.txt", filepath.Join(src, "sub", "link.txt")); err != nil {
		symlinksSupported = false
		t.Logf("symlink creation unsupported in this environment, skipping symlink assertions: %v", err)
	}

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", src, dst}) // -a: recursive + perms + times + owner + group + links
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertTreesMatch(t, src, dst, symlinksSupported)
}

// TestE2E_HardLinksPreservedWithFlag drives the real CLI command with
// -H/--hard-links and confirms two hard-linked source files arrive at
// the destination still hard-linked to each other (os.SameFile), not
// independent copies that merely have matching content.
func TestE2E_HardLinksPreservedWithFlag(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWriteFile(t, filepath.Join(src, "original.txt"), "shared content")
	if err := os.Link(filepath.Join(src, "original.txt"), filepath.Join(src, "linked.txt")); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("sync.HardLinksSupported() is false on Windows - grsync's own detection can't observe the link created above, so this platform always produces independent copies regardless of -H; see TestSenderReceiver_HardLinks for the graceful-degradation proof")
	}

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", "-H", src, dst})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	originalInfo, err := os.Stat(filepath.Join(dst, "original.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	linkedInfo, err := os.Stat(filepath.Join(dst, "linked.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !os.SameFile(originalInfo, linkedInfo) {
		t.Errorf("destination files are independent, want them hard-linked (-H was given)")
	}
}

// TestE2E_ArchiveAloneDoesNotImplyHardLinks is the critical correctness
// check behind -H being opt-in: --archive alone (no -H) must NOT
// preserve hard links, exactly matching real rsync's own -a (-rlptgoD,
// no H). Without this, "-H defaults to off" would be true in name only
// if --archive silently turned it on anyway.
func TestE2E_ArchiveAloneDoesNotImplyHardLinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWriteFile(t, filepath.Join(src, "original.txt"), "shared content")
	if err := os.Link(filepath.Join(src, "original.txt"), filepath.Join(src, "linked.txt")); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", src, dst}) // -a, deliberately no -H
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	originalInfo, err := os.Stat(filepath.Join(dst, "original.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	linkedInfo, err := os.Stat(filepath.Join(dst, "linked.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if os.SameFile(originalInfo, linkedInfo) {
		t.Errorf("destination files are hard-linked despite -H not being given - --archive must not imply -H, matching real rsync's own -a (-rlptgoD, no H)")
	}
}

// assertTreesMatch walks both roots and compares every entry: path,
// directory-ness, permission bits (platform-aware, see wantPermCLI),
// modification time, and - for regular files - content, and for
// symlinks, target.
func assertTreesMatch(t *testing.T, srcRoot, destRoot string, checkSymlinks bool) {
	t.Helper()

	srcEntries, err := sync.Walk(srcRoot, sync.WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk(src): %v", err)
	}
	destEntries, err := sync.Walk(destRoot, sync.WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk(dest): %v", err)
	}

	if len(srcEntries) != len(destEntries) {
		t.Fatalf("got %d destination entries, want %d matching source", len(destEntries), len(srcEntries))
	}

	for i, srcEntry := range srcEntries {
		destEntry := destEntries[i]
		if srcEntry.Path != destEntry.Path {
			t.Fatalf("entry %d: path mismatch: src=%q dest=%q", i, srcEntry.Path, destEntry.Path)
		}
		if srcEntry.IsDir != destEntry.IsDir {
			t.Errorf("%s: IsDir src=%v dest=%v", srcEntry.Path, srcEntry.IsDir, destEntry.IsDir)
		}

		isSymlink := srcEntry.Mode&fs.ModeSymlink != 0
		if !isSymlink {
			if got, want := destEntry.Mode.Perm(), wantPermCLI(srcEntry.Mode.Perm(), srcEntry.IsDir); got != want {
				t.Errorf("%s: perm = %o, want %o", srcEntry.Path, got, want)
			}
			if !srcEntry.ModTime.Equal(destEntry.ModTime) {
				t.Errorf("%s: mtime = %v, want %v", srcEntry.Path, destEntry.ModTime, srcEntry.ModTime)
			}
		}

		switch {
		case isSymlink:
			if !checkSymlinks {
				continue
			}
			if destEntry.Mode&fs.ModeSymlink == 0 {
				t.Errorf("%s: destination is not a symlink (Mode=%v)", srcEntry.Path, destEntry.Mode)
			}
			if srcEntry.LinkTarget != destEntry.LinkTarget {
				t.Errorf("%s: link target = %q, want %q", srcEntry.Path, destEntry.LinkTarget, srcEntry.LinkTarget)
			}
		case !srcEntry.IsDir:
			srcData, err := os.ReadFile(filepath.Join(srcRoot, filepath.FromSlash(srcEntry.Path)))
			if err != nil {
				t.Fatalf("reading source %q: %v", srcEntry.Path, err)
			}
			destData, err := os.ReadFile(filepath.Join(destRoot, filepath.FromSlash(destEntry.Path)))
			if err != nil {
				t.Fatalf("reading destination %q: %v", destEntry.Path, err)
			}
			if string(srcData) != string(destData) {
				t.Errorf("%s: content mismatch: got %q, want %q", srcEntry.Path, destData, srcData)
			}
		}
	}
}

// wantPermCLI mirrors internal/sync/attributes_test.go's wantPerm: on
// Windows, os.Chmod can only toggle the read-only attribute, not
// represent full POSIX permission bits - a real, previously-verified
// platform limitation (see SC-8), not a bug in this test's expectations.
//
// Files and directories collapse differently, confirmed by direct
// experiment rather than assumed: a file's write-bit-present mode
// collapses to 0666 and its absence to 0444, but a directory collapses to
// 0777/0555 instead of 0777/0444 - Windows still grants execute/traverse
// on a "read-only" directory, since a directory without it would be
// unusable even for reading its contents.
func wantPermCLI(mode fs.FileMode, isDir bool) fs.FileMode {
	if runtime.GOOS != "windows" {
		return mode
	}
	writable := mode&0o200 != 0
	switch {
	case isDir && writable:
		return 0o777
	case isDir:
		return 0o555
	case writable:
		return 0o666
	default:
		return 0o444
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}
