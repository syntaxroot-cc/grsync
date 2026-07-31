package sync

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestWalk_NestedDirectories(t *testing.T) {
	root := t.TempDir()

	mustMkdirAll(t, filepath.Join(root, "a", "b"))
	mustWriteFile(t, filepath.Join(root, "top.txt"), "top")
	mustWriteFile(t, filepath.Join(root, "a", "mid.txt"), "mid")
	mustWriteFile(t, filepath.Join(root, "a", "b", "deep.txt"), "deep")

	entries, err := Walk(root, WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}

	want := map[string]bool{
		"a":            true,
		"a/b":          true,
		"a/b/deep.txt": false,
		"a/mid.txt":    false,
		"top.txt":      false,
	}

	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Path] = e.IsDir
	}

	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for path, wantIsDir := range want {
		gotIsDir, ok := got[path]
		if !ok {
			t.Errorf("missing entry for %q", path)
			continue
		}
		if gotIsDir != wantIsDir {
			t.Errorf("entry %q: IsDir = %v, want %v", path, gotIsDir, wantIsDir)
		}
	}
}

func TestWalk_Symlink(t *testing.T) {
	root := t.TempDir()

	mustWriteFile(t, filepath.Join(root, "real.txt"), "hello")

	linkPath := filepath.Join(root, "link.txt")
	if err := os.Symlink("real.txt", linkPath); err != nil {
		// Creating symlinks on Windows requires Developer Mode or an
		// elevated process; treat that as "unsupported here", not a
		// failure, rather than forcing every environment to be elevated.
		t.Skipf("symlink creation unsupported in this environment: %v", err)
	}

	entries, err := Walk(root, WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}

	var link *FileEntry
	for i := range entries {
		if entries[i].Path == "link.txt" {
			link = &entries[i]
		}
	}
	if link == nil {
		t.Fatalf("link.txt not found in entries: %v", entries)
	}

	if link.Mode&os.ModeSymlink == 0 {
		t.Errorf("link.txt: Mode = %v, want ModeSymlink set", link.Mode)
	}
	if link.IsDir {
		t.Errorf("link.txt: IsDir = true, want false")
	}
	if link.LinkTarget != "real.txt" {
		t.Errorf("link.txt: LinkTarget = %q, want %q", link.LinkTarget, "real.txt")
	}

	// The symlink must not have been followed: its own Size should reflect
	// the length of the link text, not the 5-byte target file's contents.
	if link.Size == 5 {
		t.Errorf("link.txt: Size = %d looks like the resolved target's size, want the symlink's own size", link.Size)
	}
}

func TestWalk_RecursiveAndDirsFlags(t *testing.T) {
	root := t.TempDir()

	mustMkdirAll(t, filepath.Join(root, "sub"))
	mustWriteFile(t, filepath.Join(root, "top.txt"), "top")
	mustWriteFile(t, filepath.Join(root, "sub", "nested.txt"), "nested")

	paths := func(entries []FileEntry) []string {
		got := make([]string, len(entries))
		for i, e := range entries {
			got[i] = e.Path
		}
		sort.Strings(got)
		return got
	}

	t.Run("neither flag: directories skipped entirely", func(t *testing.T) {
		entries, err := Walk(root, WalkOptions{Recursive: false, Dirs: false})
		if err != nil {
			t.Fatalf("Walk returned error: %v", err)
		}
		want := []string{"top.txt"}
		if got := paths(entries); !reflect.DeepEqual(got, want) {
			t.Errorf("paths = %v, want %v", got, want)
		}
	})

	t.Run("dirs only: directory listed but not descended into", func(t *testing.T) {
		entries, err := Walk(root, WalkOptions{Recursive: false, Dirs: true})
		if err != nil {
			t.Fatalf("Walk returned error: %v", err)
		}
		want := []string{"sub", "top.txt"}
		if got := paths(entries); !reflect.DeepEqual(got, want) {
			t.Errorf("paths = %v, want %v", got, want)
		}
	})

	t.Run("recursive with dirs=false: full traversal, dirs listed anyway", func(t *testing.T) {
		entries, err := Walk(root, WalkOptions{Recursive: true, Dirs: false})
		if err != nil {
			t.Fatalf("Walk returned error: %v", err)
		}
		want := []string{"sub", "sub/nested.txt", "top.txt"}
		if got := paths(entries); !reflect.DeepEqual(got, want) {
			t.Errorf("paths = %v, want %v", got, want)
		}
	})

	t.Run("recursive with dirs=true: identical to dirs=false", func(t *testing.T) {
		entries, err := Walk(root, WalkOptions{Recursive: true, Dirs: true})
		if err != nil {
			t.Fatalf("Walk returned error: %v", err)
		}
		want := []string{"sub", "sub/nested.txt", "top.txt"}
		if got := paths(entries); !reflect.DeepEqual(got, want) {
			t.Errorf("paths = %v, want %v", got, want)
		}
	})
}

func TestWalk_OwnershipAvailable(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "file.txt"), "content")

	entries, err := Walk(root, WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %v", len(entries), entries)
	}
	entry := entries[0]

	// Windows has no POSIX uid/gid concept; every other platform this repo
	// builds for does (see uidgid_unix.go / uidgid_windows.go).
	wantAvailable := runtime.GOOS != "windows"
	if entry.OwnershipAvailable != wantAvailable {
		t.Errorf("OwnershipAvailable = %v, want %v (GOOS=%s)", entry.OwnershipAvailable, wantAvailable, runtime.GOOS)
	}
	if runtime.GOOS == "windows" && (entry.UID != 0 || entry.GID != 0) {
		t.Errorf("UID/GID = %d/%d, want 0/0 when OwnershipAvailable is false", entry.UID, entry.GID)
	}
}

func TestWalk_VaryingFileSizes(t *testing.T) {
	root := t.TempDir()

	sizes := map[string]int{
		"empty.txt": 0,
		"small.txt": 3,
		"large.txt": 10_000,
	}
	for name, size := range sizes {
		mustWriteFile(t, filepath.Join(root, name), strings.Repeat("x", size))
	}

	entries, err := Walk(root, WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}

	got := make(map[string]int64, len(entries))
	for _, e := range entries {
		got[e.Path] = e.Size
	}

	for name, wantSize := range sizes {
		gotSize, ok := got[name]
		if !ok {
			t.Errorf("missing entry for %q", name)
			continue
		}
		if gotSize != int64(wantSize) {
			t.Errorf("%q: Size = %d, want %d", name, gotSize, wantSize)
		}
	}
}

func TestWalk_DeterministicSortOrder(t *testing.T) {
	root := t.TempDir()

	// Create files in an order that does not match their sorted order, so a
	// pass would only happen if Walk actually sorts rather than incidentally
	// returning creation order or directory-read order.
	for _, name := range []string{"charlie.txt", "alpha.txt", "bravo.txt", "delta.txt"} {
		mustWriteFile(t, filepath.Join(root, name), name)
	}

	want := []string{"alpha.txt", "bravo.txt", "charlie.txt", "delta.txt"}

	for i := 0; i < 5; i++ {
		entries, err := Walk(root, WalkOptions{Recursive: true})
		if err != nil {
			t.Fatalf("run %d: Walk returned error: %v", i, err)
		}
		got := make([]string, len(entries))
		for j, e := range entries {
			got[j] = e.Path
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: order = %v, want %v", i, got, want)
		}
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
