// rsync_compare_test.go implements SC-15's "run grsync against a real
// rsync binary and diff the results" requirement.
//
// What this can and can't mean: grsync's actual on-the-wire protocol is
// gob-encoded (see internal/pipeline/messages.go's own "Encoding note"),
// not real rsync's binary wire format - a deliberate, disclosed scope
// boundary every wire-touching ticket (SC-6, SC-9, SC-13, SC-16) has
// established. grsync and a real rsync process cannot talk to each
// other over a live connection. What these tests actually do instead is
// run each tool *independently* against the same source tree and diff
// their *resulting output* - the synced destination's file contents and
// structure, and (separately, tolerant of filesystem precision) each
// tool's own attribute preservation against the source - which is a
// genuine, meaningful behavioral-equivalence check without requiring
// (or claiming) wire-protocol interoperability.
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// requireRealRsync skips the calling test if no real rsync binary is on
// PATH - this project's established pattern (see requireLocalSSHServer)
// for a capability the test environment may or may not have, rather than
// failing CI on a runner without one.
func requireRealRsync(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("rsync")
	if err != nil {
		t.Skipf("no real rsync binary found on PATH, skipping real-rsync comparison test: %v", err)
	}
	return path
}

// runRealRsync runs the real rsync binary with args, failing the test
// with its combined output if it exits non-zero - so a genuine rsync-side
// failure (e.g. a flag this rsync build doesn't support) is diagnosable
// from the test output, not just "some exec error".
func runRealRsync(t *testing.T, rsyncPath string, args ...string) {
	t.Helper()
	cmd := exec.Command(rsyncPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real rsync %v failed: %v\n%s", args, err, out)
	}
}

// buildComparisonSourceTree creates a small, deterministic tree exercising
// recursion and nested directories, shared by every comparison test below
// so each test's own assertions are about the flags/behavior actually
// under test, not about constructing fixtures. Deliberately no symlinks
// or hard links: real rsync's own handling of those depends on privilege
// and platform in ways that would risk exactly the kind of environment
// noise this ticket's self-review specifically asks to be distinguished
// from a genuine correctness gap - content/structure/basic-attribute
// comparison is where a real, meaningful behavioral check can be made
// without that risk.
func buildComparisonSourceTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "top.txt"), "top level content")
	mustWriteFile(t, filepath.Join(src, "top.log"), "a log file, to be excluded by the filtered test")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content")
	mustMkdirAll(t, filepath.Join(src, "sub", "deeper"))
	mustWriteFile(t, filepath.Join(src, "sub", "deeper", "f.txt"), "deeper content")
	return src
}

// treeFile is one regular file's content and (optionally checked) mode,
// keyed by its slash-separated path relative to the tree's root.
type treeFile struct {
	relPath string
	content string
	mode    os.FileMode
	modTime time.Time
}

// snapshotTree walks root and returns every regular file found, sorted by
// relPath for a deterministic comparison order. Directories are not
// separately recorded: a directory's presence is already implied by any
// file underneath it, and this project's own attribute-preservation
// tests elsewhere already cover directory-specific attribute handling in
// detail - duplicating that here would test the same thing twice without
// adding confidence.
func snapshotTree(t *testing.T, root string) []treeFile {
	t.Helper()
	var files []treeFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, treeFile{
			relPath: filepath.ToSlash(rel),
			content: string(content),
			mode:    info.Mode(),
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", root, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].relPath < files[j].relPath })
	return files
}

// assertSameStructureAndContent confirms got and want cover exactly the
// same set of relative paths with exactly the same content - the core
// "did these two tools produce the same result" check, shared by every
// comparison test regardless of which flags were used to produce it.
func assertSameStructureAndContent(t *testing.T, label string, got, want []treeFile) {
	t.Helper()
	gotPaths := make(map[string]string, len(got))
	for _, f := range got {
		gotPaths[f.relPath] = f.content
	}
	wantPaths := make(map[string]string, len(want))
	for _, f := range want {
		wantPaths[f.relPath] = f.content
	}

	for path, wantContent := range wantPaths {
		gotContent, ok := gotPaths[path]
		if !ok {
			t.Errorf("%s: %q present in real rsync's output but missing from grsync's", label, path)
			continue
		}
		if gotContent != wantContent {
			t.Errorf("%s: %q content differs: grsync=%q real-rsync=%q", label, path, gotContent, wantContent)
		}
	}
	for path := range gotPaths {
		if _, ok := wantPaths[path]; !ok {
			t.Errorf("%s: %q present in grsync's output but missing from real rsync's", label, path)
		}
	}
}

// TestRealRsyncComparison_BasicRecursiveSync is Part 2's first required
// case: a plain recursive sync, comparing the resulting destination
// trees' structure and content.
//
// Deliberately -r, not -a: -a implies owner/group preservation, which
// needs root to succeed cleanly on most systems - a CI runner (or a
// developer's own machine) not running as root is normal, expected
// environment variance, not a correctness signal either tool should be
// judged on here. Structure/content is what this test is actually
// about; TestRealRsyncComparison_AttributePreservation covers perms/
// times specifically, deliberately excluding owner/group for the same
// reason.
func TestRealRsyncComparison_BasicRecursiveSync(t *testing.T) {
	rsyncPath := requireRealRsync(t)
	src := buildComparisonSourceTree(t)

	// Real rsync's own trailing-slash convention ("src" copies the
	// directory itself into dest; "src/" copies its contents into dest)
	// is not implemented by grsync at all - internal/sync.Walk always
	// treats its root argument as "copy the contents of this," the
	// equivalent of real rsync's trailing-slash form, regardless of
	// what the user actually typed (verified by reading Walk's own
	// code: the root path itself is always excluded from its own
	// output). This is a real, previously-undisclosed CLI convention
	// difference this comparison suite surfaced - real rsync is
	// therefore deliberately invoked with a trailing slash on src below
	// to match grsync's own actual behavior, not to work around a bug in
	// this test.
	grsyncDest := t.TempDir()
	if err := runGrsync(t, "-r", src, grsyncDest); err != nil {
		t.Fatalf("grsync sync returned error: %v", err)
	}

	realDest := t.TempDir()
	runRealRsync(t, rsyncPath, "-r", src+"/", realDest+"/")

	assertSameStructureAndContent(t, "basic recursive sync", snapshotTree(t, grsyncDest), snapshotTree(t, realDest))
}

// TestRealRsyncComparison_FilteredSync is Part 2's second required case:
// confirms grsync's --exclude produces the same resulting tree as real
// rsync's own --exclude for the same pattern - this project's filter
// engine was built specifically to match real rsync's own documented
// syntax and semantics (see internal/sync/filter.go and its own tests),
// so this is a real, meaningful cross-check of that claim against an
// actual rsync binary, not just this project's own tests of itself.
func TestRealRsyncComparison_FilteredSync(t *testing.T) {
	rsyncPath := requireRealRsync(t)
	src := buildComparisonSourceTree(t)

	grsyncDest := t.TempDir()
	if err := runGrsync(t, "-r", "--exclude=*.log", src, grsyncDest); err != nil {
		t.Fatalf("grsync sync returned error: %v", err)
	}

	realDest := t.TempDir()
	runRealRsync(t, rsyncPath, "-r", "--exclude=*.log", src+"/", realDest+"/")

	gotFiles := snapshotTree(t, grsyncDest)
	wantFiles := snapshotTree(t, realDest)
	assertSameStructureAndContent(t, "filtered sync", gotFiles, wantFiles)

	// Belt-and-suspenders: confirm the exclusion genuinely took effect
	// for both tools, not just that they happened to agree (which could
	// also happen if --exclude were silently a no-op on both sides).
	for _, f := range append(gotFiles, wantFiles...) {
		if filepath.Ext(f.relPath) == ".log" {
			t.Errorf("top.log present in a --exclude=*.log result (relPath=%q) - the exclusion did not take effect", f.relPath)
		}
	}
}

// TestRealRsyncComparison_AttributePreservation is Part 2's third
// required case: confirms grsync's -p/-t preserve permissions/mtimes at
// least as well as real rsync's own do, for the same source.
//
// This deliberately does NOT compare grsync's destination attributes
// directly against real rsync's destination attributes bit-for-bit -
// see this ticket's own self-review requirement about distinguishing a
// real correctness gap from environment noise. Instead, each tool's own
// output is compared against the SOURCE independently: this sidesteps
// filesystem timestamp-precision differences between the two temp
// directories entirely (both could legitimately round to different
// sub-second precision without either tool being wrong), while still
// proving both tools achieve the same real property - "the destination's
// attributes match the source's."
func TestRealRsyncComparison_AttributePreservation(t *testing.T) {
	rsyncPath := requireRealRsync(t)
	src := buildComparisonSourceTree(t)

	// A distinctive, easy-to-misidentify-as-"now" mtime, matching this
	// project's own established pattern elsewhere (see
	// TestSenderReceiver_DirectoryAttributesSurviveChildCreation) for
	// making an mtime-preservation assertion meaningful instead of
	// trivially true.
	distinctiveTime := time.Date(2019, time.May, 4, 10, 30, 0, 0, time.UTC)
	srcFile := filepath.Join(src, "top.txt")
	if err := os.Chtimes(srcFile, distinctiveTime, distinctiveTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	srcInfo, err := os.Stat(srcFile)
	if err != nil {
		t.Fatalf("Stat source: %v", err)
	}

	grsyncDest := t.TempDir()
	if err := runGrsync(t, "-rpt", src, grsyncDest); err != nil {
		t.Fatalf("grsync sync returned error: %v", err)
	}
	realDest := t.TempDir()
	runRealRsync(t, rsyncPath, "-rpt", src+"/", realDest+"/")

	const mtimeTolerance = 2 * time.Second // filesystem timestamp precision varies; see this test's own doc comment
	for label, dest := range map[string]string{"grsync": grsyncDest, "real rsync": realDest} {
		destFile := filepath.Join(dest, "top.txt")
		destInfo, err := os.Stat(destFile)
		if err != nil {
			t.Fatalf("%s: Stat destination: %v", label, err)
		}
		if destInfo.Mode().Perm() != srcInfo.Mode().Perm() {
			t.Errorf("%s: destination perms = %v, want %v (matching source, -p was given)", label, destInfo.Mode().Perm(), srcInfo.Mode().Perm())
		}
		diff := destInfo.ModTime().Sub(distinctiveTime)
		if diff < -mtimeTolerance || diff > mtimeTolerance {
			t.Errorf("%s: destination mtime = %v, want within %v of %v (source's, -t was given)", label, destInfo.ModTime(), mtimeTolerance, distinctiveTime)
		}
	}
}
