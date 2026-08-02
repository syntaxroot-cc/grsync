package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syntaxroot-cc/grsync/internal/transport"
)

// TestE2E_WriteBatchThenReadBatchProducesIdenticalResult is SC-13's own
// core round-trip proof: a real --write-batch sync (which both updates
// its own destination AND captures the batch) followed by --read-batch
// against a completely fresh, separate destination must produce
// byte-for-byte identical results - the ticket's stated purpose of
// replaying a captured sync without another live connection.
func TestE2E_WriteBatchThenReadBatchProducesIdenticalResult(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "top.txt"), "top level content")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content")

	batchPath := filepath.Join(t.TempDir(), "batch")
	dst1 := t.TempDir()
	if err := runGrsync(t, "-a", "--write-batch="+batchPath, src, dst1); err != nil {
		t.Fatalf("--write-batch run returned error: %v", err)
	}
	if info, err := os.Stat(batchPath); err != nil || info.Size() == 0 {
		t.Fatalf("batch file %q missing or empty after --write-batch: err=%v", batchPath, err)
	}

	dst2 := t.TempDir()
	if err := runGrsync(t, "-a", "--read-batch="+batchPath, dst2); err != nil {
		t.Fatalf("--read-batch run returned error: %v", err)
	}

	for _, rel := range []string{"top.txt", filepath.Join("sub", "nested.txt")} {
		want, err := os.ReadFile(filepath.Join(dst1, rel))
		if err != nil {
			t.Fatalf("reading live-sync result %q: %v", rel, err)
		}
		got, err := os.ReadFile(filepath.Join(dst2, rel))
		if err != nil {
			t.Fatalf("reading replayed-batch result %q: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: replayed content = %q, want %q (matching the live sync)", rel, got, want)
		}
	}
}

// TestE2E_BatchUsableAgainstMultipleReceivers is the ticket's own stated
// purpose, verified directly: one write-batch run, replayed against two
// separate, independent, freshly-empty destinations - both must end up
// correct.
func TestE2E_BatchUsableAgainstMultipleReceivers(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "shared across many hosts")

	batchPath := filepath.Join(t.TempDir(), "batch")
	primaryDst := t.TempDir()
	if err := runGrsync(t, "-a", "--write-batch="+batchPath, src, primaryDst); err != nil {
		t.Fatalf("--write-batch run returned error: %v", err)
	}

	for _, name := range []string{"receiver-a", "receiver-b", "receiver-c"} {
		dst := t.TempDir()
		if err := runGrsync(t, "-a", "--read-batch="+batchPath, dst); err != nil {
			t.Fatalf("%s: --read-batch run returned error: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(dst, "f.txt"))
		if err != nil {
			t.Fatalf("%s: reading result: %v", name, err)
		}
		if string(got) != "shared across many hosts" {
			t.Errorf("%s: content = %q, want %q", name, got, "shared across many hosts")
		}
	}
}

func TestE2E_WriteBatchAndReadBatchMutuallyExclusive(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")
	dst := t.TempDir()

	err := runGrsync(t, "-a", "--write-batch=foo", "--read-batch=bar", src, dst)
	if err == nil {
		t.Fatal("grsync --write-batch --read-batch together returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), "cannot be used together") {
		t.Errorf("error = %q, want it to mention the flags can't be used together", err.Error())
	}
}

func TestE2E_ReadBatchRequiresExactlyOneArg(t *testing.T) {
	batchPath := filepath.Join(t.TempDir(), "batch")
	if err := os.WriteFile(batchPath, []byte("irrelevant"), 0o644); err != nil {
		t.Fatalf("seeding batch file: %v", err)
	}

	if err := runGrsync(t, "--read-batch="+batchPath); err == nil {
		t.Error("--read-batch with zero positional args returned nil error, want an error")
	}
	if err := runGrsync(t, "--read-batch="+batchPath, t.TempDir(), t.TempDir()); err == nil {
		t.Error("--read-batch with two positional args returned nil error, want an error")
	}
}

func TestE2E_WriteBatchRequiresExactlyOneSource(t *testing.T) {
	src1, src2 := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(src1, "f.txt"), "x")
	mustWriteFile(t, filepath.Join(src2, "g.txt"), "y")
	batchPath := filepath.Join(t.TempDir(), "batch")

	err := runGrsync(t, "-a", "--write-batch="+batchPath, src1, src2, t.TempDir())
	if err == nil {
		t.Fatal("--write-batch with two sources returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), "exactly one source") {
		t.Errorf("error = %q, want it to mention requiring exactly one source", err.Error())
	}
	if _, statErr := os.Stat(batchPath); !os.IsNotExist(statErr) {
		t.Errorf("batch file %q was created despite the validation error, want it never opened at all", batchPath)
	}
}

// TestE2E_WriteBatchRemovedWhenSyncFailsPartway is a self-review-driven
// regression test: forcing the underlying sync to fail (a destination
// path component that already exists as a regular file, so
// os.MkdirAll can never create the needed subdirectory under it) must
// leave no batch file behind at all - not a truncated, half-written one
// that would look like a real deliverable but could only ever fail (or
// silently under-apply) on a later --read-batch.
func TestE2E_WriteBatchRemovedWhenSyncFailsPartway(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "content")

	destParent := t.TempDir()
	blockedDest := filepath.Join(destParent, "blocker")
	mustWriteFile(t, blockedDest, "a plain file, not a directory") // destination path itself can't be mkdir'd into

	batchPath := filepath.Join(t.TempDir(), "batch")
	err := runGrsync(t, "-a", "--write-batch="+batchPath, src, blockedDest)
	if err == nil {
		t.Fatal("sync into a blocked destination returned nil error, want an error")
	}
	if _, statErr := os.Stat(batchPath); !os.IsNotExist(statErr) {
		t.Errorf("batch file %q still exists after the underlying sync failed, want it removed", batchPath)
	}
}

// TestE2E_ReadBatchOfTruncatedFileFailsClearly is
// TestE2E_ReadBatchOfMalformedFileFailsClearly's counterpart for a more
// realistic failure mode real rsync's own docs explicitly anticipate for
// batch files (portable media filling up mid-write): a genuine batch
// file cut off partway through a frame, rather than pure garbage bytes,
// must still fail cleanly rather than panicking or silently applying an
// incomplete update.
func TestE2E_ReadBatchOfTruncatedFileFailsClearly(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), strings.Repeat("content that will be truncated ", 50))
	batchPath := filepath.Join(t.TempDir(), "batch")
	if err := runGrsync(t, "-a", "--write-batch="+batchPath, src, t.TempDir()); err != nil {
		t.Fatalf("--write-batch run returned error: %v", err)
	}

	full, err := os.ReadFile(batchPath)
	if err != nil {
		t.Fatalf("reading batch file: %v", err)
	}
	if len(full) < 10 {
		t.Fatalf("batch file too small to meaningfully truncate: %d bytes", len(full))
	}
	truncatedPath := filepath.Join(t.TempDir(), "truncated-batch")
	if err := os.WriteFile(truncatedPath, full[:len(full)/2], 0o644); err != nil {
		t.Fatalf("writing truncated batch file: %v", err)
	}

	if err := runGrsync(t, "-a", "--read-batch="+truncatedPath, t.TempDir()); err == nil {
		t.Fatal("--read-batch of a truncated file returned nil error, want a clear error")
	}
}

// TestE2E_WriteBatchDisabledUnderDryRun confirms the verified real-rsync
// behavior (options.c: "else if (dry_run) write_batch = 0") is
// replicated: no batch file is created at all, though the rest of the
// dry-run sync still proceeds normally.
func TestE2E_WriteBatchDisabledUnderDryRun(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "content")
	batchPath := filepath.Join(t.TempDir(), "batch")
	dst := t.TempDir()

	if err := runGrsync(t, "-a", "-n", "--write-batch="+batchPath, src, dst); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if _, statErr := os.Stat(batchPath); !os.IsNotExist(statErr) {
		t.Errorf("batch file %q was created despite --dry-run, want no batch file written at all", batchPath)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "f.txt")); !os.IsNotExist(statErr) {
		t.Errorf("f.txt exists at the destination despite --dry-run, want the destination left untouched")
	}
}

// TestE2E_ReadBatchWorksWithDryRun confirms --read-batch needs no
// special-casing at all for --dry-run: pipeline.Receiver already treats
// planning and writing as separate concerns regardless of where its
// input bytes came from, so a dry-run batch replay just does full
// itemize planning with zero destination writes, the same as any other
// dry-run receive.
func TestE2E_ReadBatchWorksWithDryRun(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "content")
	batchPath := filepath.Join(t.TempDir(), "batch")
	writeDst := t.TempDir()
	if err := runGrsync(t, "-a", "--write-batch="+batchPath, src, writeDst); err != nil {
		t.Fatalf("--write-batch run returned error: %v", err)
	}

	readDst := t.TempDir()
	cmd := NewRootCmd()
	var out strings.Builder
	cmd.SetArgs([]string{"-a", "-n", "-i", "--read-batch=" + batchPath, readDst})
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out.String(), "f.txt") {
		t.Errorf("dry-run --read-batch itemize output = %q, want it to mention f.txt", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(readDst, "f.txt")); !os.IsNotExist(statErr) {
		t.Errorf("f.txt exists at readDst despite --dry-run, want the destination left untouched")
	}
}

func TestE2E_WriteBatchRejectedForRsyncDaemonDestination(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")
	batchPath := filepath.Join(t.TempDir(), "batch")

	err := runGrsync(t, "-a", "--write-batch="+batchPath, src, "rsync://127.0.0.1:8730/module")
	if err == nil {
		t.Fatal("--write-batch against an rsync:// destination returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), "not supported for an rsync:// daemon destination") {
		t.Errorf("error = %q, want it to explain the daemon-destination limitation", err.Error())
	}
}

// TestE2E_CompressComposesWithWriteBatchAndReadBatch confirms --compress
// works transparently with batch mode: the batch file is just a capture
// of the same deltaMessage frames Sender always produces, which already
// carry their own Compressed marker (SC-9) - Receiver decompresses
// identically whether those bytes came from a live connection or a
// replayed file, needing no batch-specific handling at all.
func TestE2E_CompressComposesWithWriteBatchAndReadBatch(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), strings.Repeat("compressible batch content ", 500))
	batchPath := filepath.Join(t.TempDir(), "batch")
	writeDst := t.TempDir()

	if err := runGrsync(t, "-a", "--compress", "--write-batch="+batchPath, src, writeDst); err != nil {
		t.Fatalf("--write-batch run returned error: %v", err)
	}

	readDst := t.TempDir()
	if err := runGrsync(t, "-a", "--read-batch="+batchPath, readDst); err != nil {
		t.Fatalf("--read-batch run returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(readDst, "f.txt"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	want := strings.Repeat("compressible batch content ", 500)
	if string(got) != want {
		t.Errorf("replayed content differs from source (len got=%d, want=%d)", len(got), len(want))
	}
}

func TestE2E_ReadBatchFromStdin(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "via stdin")
	batchPath := filepath.Join(t.TempDir(), "batch")
	writeDst := t.TempDir()
	if err := runGrsync(t, "-a", "--write-batch="+batchPath, src, writeDst); err != nil {
		t.Fatalf("--write-batch run returned error: %v", err)
	}
	batchBytes, err := os.ReadFile(batchPath)
	if err != nil {
		t.Fatalf("reading batch file: %v", err)
	}

	readDst := t.TempDir()
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"-a", "--read-batch=-", readDst})
	cmd.SetOut(io.Discard)
	cmd.SetIn(bytes.NewReader(batchBytes))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(readDst, "f.txt"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	if string(got) != "via stdin" {
		t.Errorf("content = %q, want %q", got, "via stdin")
	}
}

// TestE2E_ReadBatchOfMalformedFileFailsClearly is the ticket's own
// explicit testing requirement: since Option A means grsync's batch
// format is its own gob-framed messages, not real rsync's actual wire
// format, an attempt to replay a file that isn't one (garbage bytes
// standing in for what a real-rsync-produced batch file would look
// like, since grsync can't produce a genuine one to test against) must
// fail with a clear, ordinary error - not a panic, and not silent
// misbehavior (e.g. treating garbage as an empty, successfully-applied
// batch).
func TestE2E_ReadBatchOfMalformedFileFailsClearly(t *testing.T) {
	batchPath := filepath.Join(t.TempDir(), "not-a-real-batch")
	if err := os.WriteFile(batchPath, []byte("this is not a grsync batch file at all, just garbage bytes"), 0o644); err != nil {
		t.Fatalf("writing garbage batch file: %v", err)
	}

	err := runGrsync(t, "-a", "--read-batch="+batchPath, t.TempDir())
	if err == nil {
		t.Fatal("--read-batch of a malformed file returned nil error, want a clear error")
	}
}

// requireLocalSSHServer mirrors internal/pipeline/ssh_test.go's own
// same-named helper (and internal/transport's before that) - duplicated
// rather than shared across packages, matching that established
// precedent, since this is the first CLI-level (not internal/pipeline-
// level) real-SSH test: the --write-batch tee lives entirely in
// cli/sync.go's own syncToRemote, so proving it doesn't corrupt a real
// remote transfer needs the actual CLI command driving a real
// ssh-spawned --server process, not pipeline.Sender called directly the
// way the existing SSH tests do.
func requireLocalSSHServer(t *testing.T) {
	t.Helper()
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "127.0.0.1", "true")
	if err := cmd.Run(); err != nil {
		t.Skipf("no SSH server reachable at 127.0.0.1 for a non-interactive connection: %v", err)
	}
}

// TestE2E_WriteBatchOverRealSSH is SC-13's real, over-the-wire proof for
// the SSH transport: syncToRemote's batchWriter tap (cli/sync.go) is
// installed on the real transport.Session after a genuine SSH handshake
// to 127.0.0.1, and the resulting batch file must both (a) not have
// corrupted the live transfer and (b) itself be a valid, replayable
// batch. Skips gracefully without a local sshd, the same established
// pattern every other real-SSH test in this project uses.
func TestE2E_WriteBatchOverRealSSH(t *testing.T) {
	requireLocalSSHServer(t)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "synced over real ssh with --write-batch")
	batchPath := filepath.Join(t.TempDir(), "batch")
	sshDst := t.TempDir()

	// No explicit user@ - matching internal/pipeline/ssh_test.go's own
	// convention of relying on ssh's default (the current OS user)
	// against the loopback address.
	dest := "127.0.0.1:" + filepath.ToSlash(sshDst)
	if _, ok := transport.ParseRemotePath(dest); !ok {
		t.Fatalf("constructed destination %q did not parse as a remote path", dest)
	}

	if err := runGrsync(t, "-a", "--write-batch="+batchPath, src, dest); err != nil {
		t.Fatalf("--write-batch over real ssh returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(sshDst, "f.txt"))
	if err != nil {
		t.Fatalf("reading remote sync result: %v", err)
	}
	if string(got) != "synced over real ssh with --write-batch" {
		t.Errorf("remote content = %q, want the source content", got)
	}

	// The captured batch must itself be genuinely replayable.
	replayDst := t.TempDir()
	if err := runGrsync(t, "-a", "--read-batch="+batchPath, replayDst); err != nil {
		t.Fatalf("--read-batch of the ssh-captured batch returned error: %v", err)
	}
	got2, err := os.ReadFile(filepath.Join(replayDst, "f.txt"))
	if err != nil {
		t.Fatalf("reading replayed result: %v", err)
	}
	if string(got2) != "synced over real ssh with --write-batch" {
		t.Errorf("replayed content = %q, want the source content", got2)
	}
}
