package pipeline

import (
	"io/fs"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

var fullAttrOpts = sync.AttrOptions{Perms: true, Times: true, Owner: true, Group: true, Links: true}

func TestItemizeFile_New(t *testing.T) {
	entry := sync.FileEntry{Path: "new.txt"}
	code, report := itemizeFile(entry, sync.FileEntry{}, false, true, fullAttrOpts)
	if !report {
		t.Fatalf("report = false for a new file, want true")
	}
	if code != ">f+++++++++" {
		t.Errorf("code = %q, want %q (real rsync's own \"newly created\" format)", code, ">f+++++++++")
	}
}

func TestItemizeFile_SizeAndTimeChanged(t *testing.T) {
	entry := sync.FileEntry{Path: "f.txt", Size: 100, ModTime: time.Unix(2000, 0)}
	old := sync.FileEntry{Size: 50, ModTime: time.Unix(1000, 0)}
	code, report := itemizeFile(entry, old, true, true, fullAttrOpts)
	if !report {
		t.Fatalf("report = false for a changed file, want true")
	}
	// Y='>' (content transferred), X='f', then c=".", s="s", t="t", p/o/g/u/a/x=".".
	if code != ">f.st......" {
		t.Errorf("code = %q, want %q", code, ">f.st......")
	}
}

func TestItemizeFile_PermsOwnerGroupChanged(t *testing.T) {
	entry := sync.FileEntry{
		Path: "f.txt", Size: 10, ModTime: time.Unix(1000, 0), Mode: fs.FileMode(0o644),
		UID: 2000, GID: 2000, OwnershipAvailable: true,
	}
	old := sync.FileEntry{
		Size: 10, ModTime: time.Unix(1000, 0), Mode: fs.FileMode(0o600),
		UID: 1000, GID: 1000, OwnershipAvailable: true,
	}
	// Content unchanged (contentChanged=false) so this exercises the
	// "attributes-only update" case: Y must be '.', not '>', matching
	// real rsync's own documented meaning of '.' - "the item is not
	// being updated (though it might have attributes that are being
	// modified)".
	code, report := itemizeFile(entry, old, true, false, fullAttrOpts)
	if !report {
		t.Fatalf("report = false for a file with changed perms/owner/group, want true")
	}
	if code != ".f...pog..." {
		t.Errorf("code = %q, want %q", code, ".f...pog...")
	}
}

func TestItemizeFile_UnchangedIsNotReported(t *testing.T) {
	entry := sync.FileEntry{Path: "f.txt", Size: 10, ModTime: time.Unix(1000, 0), Mode: fs.FileMode(0o644)}
	old := entry
	_, report := itemizeFile(entry, old, true, false, fullAttrOpts)
	if report {
		t.Errorf("report = true for a completely unchanged file, want false (real rsync's own default -i behavior)")
	}
}

func TestItemizeFile_AttrsIgnoredWhenNotRequested(t *testing.T) {
	entry := sync.FileEntry{Path: "f.txt", Size: 10, ModTime: time.Unix(2000, 0), Mode: fs.FileMode(0o600)}
	old := sync.FileEntry{Size: 10, ModTime: time.Unix(1000, 0), Mode: fs.FileMode(0o644)}
	// opts requests none of Times/Perms/Owner/Group - real rsync's own
	// rule is that each attribute letter "requires" its flag; without
	// it, even a real underlying difference must not be reported.
	_, report := itemizeFile(entry, old, true, false, sync.AttrOptions{})
	if report {
		t.Errorf("report = true when no attribute flags were requested, want false")
	}
}

func TestItemizeDir_New(t *testing.T) {
	code, report := itemizeDir(sync.FileEntry{Path: "d"}, sync.FileEntry{}, false, fullAttrOpts)
	if !report || code != "cd+++++++++" {
		t.Errorf("itemizeDir(new) = (%q, %v), want (%q, true)", code, report, "cd+++++++++")
	}
}

func TestItemizeDir_ExistingTimeChanged(t *testing.T) {
	entry := sync.FileEntry{Path: "d", ModTime: time.Unix(2000, 0)}
	old := sync.FileEntry{ModTime: time.Unix(1000, 0)}
	code, report := itemizeDir(entry, old, true, fullAttrOpts)
	if !report {
		t.Fatalf("report = false for a directory with a changed mtime, want true")
	}
	// Y='.': a directory is never "created" when it already existed,
	// only touched - matching real rsync's own '.' semantics.
	if code != ".d..t......" {
		t.Errorf("code = %q, want %q", code, ".d..t......")
	}
}

func TestItemizeDir_ExistingUnchangedIsNotReported(t *testing.T) {
	entry := sync.FileEntry{Path: "d", ModTime: time.Unix(1000, 0), Mode: fs.FileMode(0o755)}
	_, report := itemizeDir(entry, entry, true, fullAttrOpts)
	if report {
		t.Errorf("report = true for a completely unchanged directory, want false")
	}
}

func TestItemizeSymlink_New(t *testing.T) {
	code, report := itemizeSymlink(sync.FileEntry{Path: "l", LinkTarget: "target"}, sync.FileEntry{}, false, fullAttrOpts)
	if !report || code != "cL+++++++++" {
		t.Errorf("itemizeSymlink(new) = (%q, %v), want (%q, true)", code, report, "cL+++++++++")
	}
}

func TestItemizeSymlink_TargetChanged(t *testing.T) {
	entry := sync.FileEntry{Path: "l", LinkTarget: "new-target"}
	old := sync.FileEntry{LinkTarget: "old-target"}
	code, report := itemizeSymlink(entry, old, true, fullAttrOpts)
	if !report {
		t.Fatalf("report = false for a symlink with a changed target, want true")
	}
	// attribute-c means "changed value" for a symlink (not checksum),
	// per the man page's own distinction from a regular file's c.
	if code != "cLc........" {
		t.Errorf("code = %q, want %q", code, "cLc........")
	}
}

func TestItemizeSymlink_UnchangedTargetIsNotReported(t *testing.T) {
	entry := sync.FileEntry{Path: "l", LinkTarget: "same-target"}
	_, report := itemizeSymlink(entry, entry, true, fullAttrOpts)
	if report {
		t.Errorf("report = true for a symlink whose target didn't change, want false")
	}
}

func TestItemizeHardLink(t *testing.T) {
	if got := itemizeHardLink(); got != "hf+++++++++" {
		t.Errorf("itemizeHardLink() = %q, want %q", got, "hf+++++++++")
	}
}

func TestFormatItemizeLine(t *testing.T) {
	if got := formatItemizeLine(">f+++++++++", "new.txt", ""); got != ">f+++++++++ new.txt" {
		t.Errorf("formatItemizeLine (file) = %q, want %q", got, ">f+++++++++ new.txt")
	}
	if got := formatItemizeLine("cL+++++++++", "link.txt", "target.txt"); got != "cL+++++++++ link.txt -> target.txt" {
		t.Errorf("formatItemizeLine (symlink) = %q, want %q", got, "cL+++++++++ link.txt -> target.txt")
	}
}

func TestFormatVerboseLine(t *testing.T) {
	if got := formatVerboseLine("new.txt", ""); got != "new.txt" {
		t.Errorf("formatVerboseLine (file) = %q, want %q", got, "new.txt")
	}
	if got := formatVerboseLine("link.txt", "target.txt"); got != "link.txt -> target.txt" {
		t.Errorf("formatVerboseLine (symlink) = %q, want %q", got, "link.txt -> target.txt")
	}
}
