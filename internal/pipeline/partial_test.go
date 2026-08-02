package pipeline

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCreateTempFile_SameDirectoryHiddenRecognizableName(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")

	f, err := createTempFile(destPath)
	if err != nil {
		t.Fatalf("createTempFile returned error: %v", err)
	}
	defer func() { _ = f.Close() }()
	defer func() { _ = os.Remove(f.Name()) }()

	if filepath.Dir(f.Name()) != dir {
		t.Errorf("temp file dir = %q, want %q (same directory as destPath, so the eventual rename is same-filesystem)", filepath.Dir(f.Name()), dir)
	}
	base := filepath.Base(f.Name())
	if !strings.HasPrefix(base, ".file.txt.") || !strings.HasSuffix(base, ".grsync-tmp") {
		t.Errorf("temp file name = %q, want a hidden, recognizable \".file.txt.*.grsync-tmp\" pattern", base)
	}
}

func TestCreateTempFile_DefaultModeMatchesOsWriteFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits aren't meaningfully comparable on Windows")
	}
	dir := t.TempDir()
	f, err := createTempFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("createTempFile returned error: %v", err)
	}
	defer func() { _ = f.Close() }()
	defer func() { _ = os.Remove(f.Name()) }()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// os.CreateTemp's own default is 0600; createTempFile must override
	// that to 0644 so a sync run without --perms doesn't quietly start
	// producing more restrictive files than os.WriteFile always has.
	if info.Mode().Perm() != 0o644 {
		t.Errorf("temp file mode = %v, want 0644 (matching os.WriteFile's own default for a no---perms sync)", info.Mode().Perm())
	}
}

func TestPartialFilePath_RelativeDirIsInsideDestinationsOwnDirectory(t *testing.T) {
	destPath := filepath.Join("dest", "sub", "file.txt")
	got := partialFilePath(destPath, ".rsync-partial", "sub/file.txt")
	want := filepath.Join("dest", "sub", ".rsync-partial", "file.txt")
	if got != want {
		t.Errorf("partialFilePath = %q, want %q", got, want)
	}
}

func TestPartialFilePath_AbsoluteDirMirrorsRelativePath(t *testing.T) {
	absDir := string(filepath.Separator) + "partial-store"
	if runtime.GOOS == "windows" {
		absDir = `C:\partial-store`
	}
	destPath := filepath.Join("dest", "sub", "file.txt")
	got := partialFilePath(destPath, absDir, "sub/file.txt")
	want := filepath.Join(absDir, "sub", "file.txt")
	if got != want {
		t.Errorf("partialFilePath = %q, want %q", got, want)
	}
}

// TestPartialFilePath_AbsoluteDirAvoidsBasenameCollisions is the actual
// reason the absolute case mirrors the full relative path instead of
// just the basename: two different source files sharing a basename in
// different subdirectories must not collide under one shared
// partial-dir.
func TestPartialFilePath_AbsoluteDirAvoidsBasenameCollisions(t *testing.T) {
	absDir := string(filepath.Separator) + "partial-store"
	if runtime.GOOS == "windows" {
		absDir = `C:\partial-store`
	}
	p1 := partialFilePath(filepath.Join("dest", "a", "file.txt"), absDir, "a/file.txt")
	p2 := partialFilePath(filepath.Join("dest", "b", "file.txt"), absDir, "b/file.txt")
	if p1 == p2 {
		t.Errorf("partialFilePath collided for two different files sharing a basename: %q", p1)
	}
}

func TestLoadPartialBasis_NoPartialDirConfigured(t *testing.T) {
	_, ok := loadPartialBasis(filepath.Join(t.TempDir(), "file.txt"), ReceiverOptions{}, "file.txt")
	if ok {
		t.Error("loadPartialBasis with no PartialDir configured returned ok=true, want false")
	}
}

func TestLoadPartialBasis_NothingThereYet(t *testing.T) {
	dir := t.TempDir()
	_, ok := loadPartialBasis(filepath.Join(dir, "file.txt"), ReceiverOptions{PartialDir: ".rsync-partial"}, "file.txt")
	if ok {
		t.Error("loadPartialBasis with nothing in the partial dir returned ok=true, want false")
	}
}

func TestLoadPartialBasis_FindsExistingPartialFile(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")
	ropts := ReceiverOptions{PartialDir: ".rsync-partial"}
	pPath := partialFilePath(destPath, ropts.PartialDir, "file.txt")
	if err := os.MkdirAll(filepath.Dir(pPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pPath, []byte("partial prefix content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, ok := loadPartialBasis(destPath, ropts, "file.txt")
	if !ok {
		t.Fatal("loadPartialBasis did not find the existing partial file")
	}
	if string(data) != "partial prefix content" {
		t.Errorf("loadPartialBasis content = %q, want %q", data, "partial prefix content")
	}
}

func mustCreateTempFileWithContent(t *testing.T, destPath, content string) string {
	t.Helper()
	f, err := createTempFile(destPath)
	if err != nil {
		t.Fatalf("createTempFile: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return f.Name()
}

func TestFinishRegularFileWrite_SuccessRenamesIntoPlace(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")
	tmpPath := mustCreateTempFileWithContent(t, destPath, "final content")

	if err := finishRegularFileWrite(tmpPath, destPath, nil, ReceiverOptions{}, "file.txt", false); err != nil {
		t.Fatalf("finishRegularFileWrite returned error: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading destPath: %v", err)
	}
	if string(got) != "final content" {
		t.Errorf("destPath content = %q, want %q", got, "final content")
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file %q still exists after a successful commit, want it gone (renamed away)", tmpPath)
	}
}

// TestFinishRegularFileWrite_SuccessCleansUpUsedPartialBasis is real
// rsync's own documented "delete it after it has served its purpose"
// behavior: once a partial-dir file's content has been successfully
// folded into a completed transfer, it should be removed so it doesn't
// linger as stale, misleading resume data for a future run.
func TestFinishRegularFileWrite_SuccessCleansUpUsedPartialBasis(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")
	ropts := ReceiverOptions{PartialDir: ".rsync-partial"}
	pPath := partialFilePath(destPath, ropts.PartialDir, "file.txt")
	if err := os.MkdirAll(filepath.Dir(pPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pPath, []byte("stale partial prefix"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tmpPath := mustCreateTempFileWithContent(t, destPath, "final content")
	if err := finishRegularFileWrite(tmpPath, destPath, nil, ropts, "file.txt", true); err != nil {
		t.Fatalf("finishRegularFileWrite returned error: %v", err)
	}

	if _, err := os.Stat(pPath); !os.IsNotExist(err) {
		t.Errorf("used partial-dir file %q still exists after a successful transfer, want it removed", pPath)
	}
}

func TestAbandonOrKeep_NoPartialDeletesTempFileEntirely(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")
	tmpPath := mustCreateTempFileWithContent(t, destPath, "interrupted content")
	origErr := os.ErrClosed // any stand-in error

	err := abandonOrKeep(tmpPath, destPath, ReceiverOptions{}, "file.txt", origErr)
	if err != origErr {
		t.Errorf("abandonOrKeep returned %v, want the original error unchanged", err)
	}
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("temp file %q still exists without --partial, want it deleted", tmpPath)
	}
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Errorf("destPath %q exists without --partial, want it untouched (never created)", destPath)
	}
}

func TestAbandonOrKeep_PlainPartialKeepsContentAtDestPath(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")
	tmpPath := mustCreateTempFileWithContent(t, destPath, "interrupted content")

	_ = abandonOrKeep(tmpPath, destPath, ReceiverOptions{Partial: true}, "file.txt", os.ErrClosed)

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("--partial should have left the partial content at destPath, but reading it failed: %v", err)
	}
	if string(got) != "interrupted content" {
		t.Errorf("destPath content = %q, want %q", got, "interrupted content")
	}
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("temp file %q still exists after being kept at destPath, want it gone from its original name", tmpPath)
	}
}

func TestAbandonOrKeep_PartialDirMovesContentThereInsteadOfDestPath(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")
	ropts := ReceiverOptions{PartialDir: ".rsync-partial"}
	tmpPath := mustCreateTempFileWithContent(t, destPath, "interrupted content")

	_ = abandonOrKeep(tmpPath, destPath, ropts, "file.txt", os.ErrClosed)

	pPath := partialFilePath(destPath, ropts.PartialDir, "file.txt")
	got, err := os.ReadFile(pPath)
	if err != nil {
		t.Fatalf("--partial-dir should have moved the partial content there, but reading it failed: %v", err)
	}
	if string(got) != "interrupted content" {
		t.Errorf("partial-dir content = %q, want %q", got, "interrupted content")
	}
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Errorf("destPath %q exists after an aborted --partial-dir transfer, want it left completely untouched", destPath)
	}
}

// TestAbandonOrKeep_PartialDirNeverOverwritesExistingDestContent is the
// self-review's own "could a partial file leak outside --partial-dir"
// question, answered concretely: an aborted transfer with --partial-dir
// set must never modify whatever was already sitting at the real
// destination path, even though a plain --partial (no dir) would
// deliberately overwrite it with the partial content.
func TestAbandonOrKeep_PartialDirNeverOverwritesExistingDestContent(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(destPath, []byte("original untouched content"), 0o644); err != nil {
		t.Fatalf("seeding destPath: %v", err)
	}
	ropts := ReceiverOptions{PartialDir: ".rsync-partial"}
	tmpPath := mustCreateTempFileWithContent(t, destPath, "new interrupted content")

	_ = abandonOrKeep(tmpPath, destPath, ropts, "file.txt", os.ErrClosed)

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading destPath: %v", err)
	}
	if string(got) != "original untouched content" {
		t.Errorf("destPath content = %q after an aborted --partial-dir transfer, want the original content left completely untouched", got)
	}
}

// TestAbandonOrKeep_NeverLeavesATempFileNextToDestPath is the same
// self-review question from the opposite angle: regardless of which
// policy applies, the raw ".file.txt.*.grsync-tmp" temp file must never
// be the thing left behind in the destination's own directory - it's
// always either deleted or moved to one of the two real, documented
// destinations (destPath itself, or inside PartialDir).
func TestAbandonOrKeep_NeverLeavesATempFileNextToDestPath(t *testing.T) {
	for _, ropts := range []ReceiverOptions{
		{},
		{Partial: true},
		{PartialDir: ".rsync-partial"},
	} {
		dir := t.TempDir()
		destPath := filepath.Join(dir, "file.txt")
		tmpPath := mustCreateTempFileWithContent(t, destPath, "content")

		_ = abandonOrKeep(tmpPath, destPath, ropts, "file.txt", os.ErrClosed)

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".grsync-tmp") {
				t.Errorf("ropts=%+v: a raw temp file %q was left in the destination directory, want it always deleted or moved away", ropts, e.Name())
			}
		}
	}
}
