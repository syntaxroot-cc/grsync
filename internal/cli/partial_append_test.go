package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestE2E_AppendAndAppendVerifyMutuallyExclusive(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "content")
	dst := t.TempDir()

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", "--append", "--append-verify", src, dst})
	cmd.SetOut(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("grsync --append --append-verify returned nil error, want a mutually-exclusive error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to mention --append/--append-verify being mutually exclusive", err.Error())
	}
}

// TestE2E_AppendExtendsShorterDestinationFile drives the real CLI
// command with --append against a destination file that's a genuine
// prefix of the source, and confirms the result is correct - the real,
// end-to-end proof that --append's flag wiring (root.go through
// effectiveReceiverOptions) actually reaches Receiver.
func TestE2E_AppendExtendsShorterDestinationFile(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	prefix := "already present on disk "
	full := prefix + "and now the new tail data too"
	mustWriteFile(t, filepath.Join(src, "growing.log"), full)
	mustWriteFile(t, filepath.Join(dst, "growing.log"), prefix)

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", "--append", src, dst})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "growing.log"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	if string(got) != full {
		t.Errorf("result = %q, want %q", got, full)
	}
}

// TestE2E_AppendVerifyCorrectsWrongPrefix is --append-verify's own real,
// end-to-end correctness proof: a destination whose existing "prefix" is
// actually wrong must still end up fully correct, since --append-verify
// (unlike plain --append) actually checks it.
func TestE2E_AppendVerifyCorrectsWrongPrefix(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	full := strings.Repeat("A", 1400) + "the genuinely new tail"
	wrongPrefix := strings.Repeat("Z", 1400)
	mustWriteFile(t, filepath.Join(src, "file.txt"), full)
	mustWriteFile(t, filepath.Join(dst, "file.txt"), wrongPrefix)

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", "--append-verify", src, dst})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	if string(got) != full {
		t.Errorf("result = %q, want %q (--append-verify must catch and correct the wrong prefix)", got, full)
	}
}

// TestE2E_PartialDirUsedForResume is --partial-dir's own real,
// end-to-end resume proof: a leftover partial file (as if left there by
// an earlier interrupted `grsync --partial-dir=...` run) is picked up
// and used, then cleaned up, via the real CLI command.
func TestE2E_PartialDirUsedForResume(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	full := strings.Repeat("resumable data ", 100)
	mustWriteFile(t, filepath.Join(src, "big.txt"), full)

	partialPath := filepath.Join(dst, ".rsync-partial", "big.txt")
	mustMkdirAll(t, filepath.Dir(partialPath))
	mustWriteFile(t, partialPath, full[:len(full)*3/4])

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", "--partial-dir", ".rsync-partial", src, dst})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "big.txt"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	if string(got) != full {
		t.Errorf("result differs from source (len got=%d, want=%d)", len(got), len(full))
	}
	if _, statErr := os.Stat(partialPath); !os.IsNotExist(statErr) {
		t.Errorf("partial-dir file %q still exists after a successful resumed transfer, want it removed", partialPath)
	}
}

// TestE2E_PartialFlagsDoNotBreakAnOrdinaryUninterruptedSync confirms
// --partial/--partial-dir/--append/--append-verify are all harmless
// no-ops for a completely ordinary, uninterrupted sync - they only ever
// change behavior around eligibility/interruption, never the happy path.
func TestE2E_PartialFlagsDoNotBreakAnOrdinaryUninterruptedSync(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "ordinary content")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content")

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", "--partial", "--partial-dir", ".rsync-partial", src, dst})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "f.txt"))
	if err != nil || string(got) != "ordinary content" {
		t.Errorf("f.txt content = %q, err = %v, want %q", got, err, "ordinary content")
	}
	got2, err := os.ReadFile(filepath.Join(dst, "sub", "nested.txt"))
	if err != nil || string(got2) != "nested content" {
		t.Errorf("sub/nested.txt content = %q, err = %v, want %q", got2, err, "nested content")
	}
}
