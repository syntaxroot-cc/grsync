package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHardLinksSupported_MatchesPlatform(t *testing.T) {
	want := runtime.GOOS != "windows"
	if got := HardLinksSupported(); got != want {
		t.Errorf("HardLinksSupported() = %v, want %v on %s", got, want, runtime.GOOS)
	}
}

func TestDetectHardLinks(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "original.txt"), "shared content")
	mustWriteFile(t, filepath.Join(root, "unrelated.txt"), "different content")

	linkedPath := filepath.Join(root, "linked.txt")
	if err := os.Link(filepath.Join(root, "original.txt"), linkedPath); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}

	entries, err := Walk(root, WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}

	groups, err := DetectHardLinks(root, entries)
	if err != nil {
		t.Fatalf("DetectHardLinks returned error: %v", err)
	}

	if runtime.GOOS == "windows" {
		// lookupHardLinkKey always reports unavailable on Windows (see
		// hardlinks_windows.go) - even though a real hard link was just
		// created above, detection can't observe it, by design.
		if len(groups) != 0 {
			t.Errorf("got %d groups on Windows, want 0 (hard-link identity is unavailable there)", len(groups))
		}
		return
	}

	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	want := HardLinkGroup{"linked.txt", "original.txt"} // sorted order
	got := groups[0]
	if len(got) != len(want) {
		t.Fatalf("group = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDetectHardLinks_NoLinksProducesNoGroups(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "content a")
	mustWriteFile(t, filepath.Join(root, "b.txt"), "content b")

	entries, err := Walk(root, WalkOptions{Recursive: true})
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}

	groups, err := DetectHardLinks(root, entries)
	if err != nil {
		t.Fatalf("DetectHardLinks returned error: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("got %d groups for unrelated files, want 0: %+v", len(groups), groups)
	}
}

func TestApplyHardLinks(t *testing.T) {
	destRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(destRoot, "original.txt"), "shared content")

	group := HardLinkGroup{"original.txt", "linked.txt"}
	if err := ApplyHardLinks(destRoot, group); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}

	// Prove they're genuinely the same underlying file, not just two
	// copies with equal content: writing through one path must be visible
	// through the other.
	linkedPath := filepath.Join(destRoot, "linked.txt")
	if err := os.WriteFile(linkedPath, []byte("changed via the linked path"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	originalContent, err := os.ReadFile(filepath.Join(destRoot, "original.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(originalContent) != "changed via the linked path" {
		t.Errorf("original.txt content = %q, want the change made via linked.txt to be visible (they should be the same file)", originalContent)
	}
}

func TestApplyHardLinks_SingleMemberIsNoOp(t *testing.T) {
	destRoot := t.TempDir()
	if err := ApplyHardLinks(destRoot, HardLinkGroup{"solo.txt"}); err != nil {
		t.Errorf("ApplyHardLinks with a single-member group returned error: %v, want nil (nothing to link)", err)
	}
}
