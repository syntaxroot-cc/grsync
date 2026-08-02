package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
	"github.com/syntaxroot-cc/grsync/internal/transport"
)

// runSenderReceiver drives Sender and Receiver concurrently over a pair
// of in-memory pipes wired crosswise.
func runSenderReceiver(t *testing.T, src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions) {
	t.Helper()

	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- Sender(sender, src, walkOpts, rules, attrOpts.HardLinks, CompressOptions{}) }()

	receiverErrCh := make(chan error, 1)
	go func() { receiverErrCh <- Receiver(receiver, dest, attrOpts, ReceiverOptions{}) }()

	select {
	case err := <-receiverErrCh:
		if err != nil {
			t.Fatalf("Receiver returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receiver did not complete within 10s")
	}
	select {
	case err := <-senderErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sender did not complete within 10s")
	}
}

// pipeReadWriter joins two io.Pipe halves into a single io.ReadWriter.
type pipeReadWriter struct {
	io.Reader
	io.Writer
}

func TestSenderReceiver_BasicTree(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	mustWriteFile(t, filepath.Join(srcRoot, "top.txt"), "top level file")
	mustMkdirAll(t, filepath.Join(srcRoot, "sub"))
	mustWriteFile(t, filepath.Join(srcRoot, "sub", "nested.txt"), "nested file content")

	runSenderReceiver(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{Perms: true, Times: true})

	assertSameContent(t, filepath.Join(srcRoot, "top.txt"), filepath.Join(destRoot, "top.txt"))
	assertSameContent(t, filepath.Join(srcRoot, "sub", "nested.txt"), filepath.Join(destRoot, "sub", "nested.txt"))
}

func TestSenderReceiver_NewFile(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir() // nothing here yet at all

	mustWriteFile(t, filepath.Join(srcRoot, "brand-new.txt"), "this file is new to the destination")

	runSenderReceiver(t, srcRoot, destRoot, sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{})

	assertSameContent(t, filepath.Join(srcRoot, "brand-new.txt"), filepath.Join(destRoot, "brand-new.txt"))
}

func TestSenderReceiver_UnchangedFileIsAllCopyOps(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	content := "identical content that should transfer as copy ops, not literal data"
	mustWriteFile(t, filepath.Join(srcRoot, "same.txt"), content)
	mustWriteFile(t, filepath.Join(destRoot, "same.txt"), content) // already present, byte-identical

	runSenderReceiver(t, srcRoot, destRoot, sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{})

	assertSameContent(t, filepath.Join(srcRoot, "same.txt"), filepath.Join(destRoot, "same.txt"))
}

func TestSenderReceiver_DestinationOnlyFileIsLeftAlone(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	mustWriteFile(t, filepath.Join(srcRoot, "from-source.txt"), "from source")
	mustWriteFile(t, filepath.Join(destRoot, "dest-only.txt"), "only ever existed at the destination")

	runSenderReceiver(t, srcRoot, destRoot, sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{})

	assertSameContent(t, filepath.Join(srcRoot, "from-source.txt"), filepath.Join(destRoot, "from-source.txt"))

	got, err := os.ReadFile(filepath.Join(destRoot, "dest-only.txt"))
	if err != nil {
		t.Fatalf("dest-only.txt was removed or is unreadable: %v", err)
	}
	if string(got) != "only ever existed at the destination" {
		t.Errorf("dest-only.txt content changed: got %q", got)
	}
}

func TestSenderReceiver_VerboseAloneShowsNamesOnly(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	mustWriteFile(t, filepath.Join(srcRoot, "new.txt"), "brand new content")
	mustMkdirAll(t, filepath.Join(srcRoot, "sub"))
	mustWriteFile(t, filepath.Join(srcRoot, "sub", "nested.txt"), "nested content")

	var out bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Verbose: true, Output: &out})

	output := out.String()
	if !strings.Contains(output, "new.txt") {
		t.Errorf("output = %q, want it to mention new.txt", output)
	}
	if !strings.Contains(output, "sub/nested.txt") {
		t.Errorf("output = %q, want it to mention sub/nested.txt", output)
	}
	if strings.Contains(output, "+++++++++") {
		t.Errorf("output = %q, want bare paths only (no itemize codes) since Itemize was not requested", output)
	}
}

func TestSenderReceiver_Symlink(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	mustWriteFile(t, filepath.Join(srcRoot, "target.txt"), "link target content")
	if err := os.Symlink("target.txt", filepath.Join(srcRoot, "link.txt")); err != nil {
		t.Skipf("symlink creation unsupported in this environment: %v", err)
	}

	runSenderReceiver(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{Links: true})

	info, err := os.Lstat(filepath.Join(destRoot, "link.txt"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt at the destination is not a symlink: Mode = %v", info.Mode())
	}
	target, err := os.Readlink(filepath.Join(destRoot, "link.txt"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "target.txt" {
		t.Errorf("link target = %q, want %q", target, "target.txt")
	}
}

func TestSenderReceiver_DirectoryAttributesSurviveChildCreation(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	srcSub := filepath.Join(srcRoot, "sub")
	mustMkdirAll(t, srcSub)
	mustWriteFile(t, filepath.Join(srcSub, "child.txt"), "child content")

	// Set after the child already exists, matching a real source tree.
	wantDirTime := time.Date(2019, time.May, 4, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(srcSub, wantDirTime, wantDirTime); err != nil {
		t.Fatalf("Chtimes on source directory: %v", err)
	}

	runSenderReceiver(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{Times: true})

	destSub := filepath.Join(destRoot, "sub")
	info, err := os.Stat(destSub)
	if err != nil {
		t.Fatalf("Stat destination directory: %v", err)
	}
	if !info.ModTime().Equal(wantDirTime) {
		t.Errorf("destination directory ModTime = %v, want %v (likely bumped by child creation)",
			info.ModTime(), wantDirTime)
	}
}

// TestSenderReceiver_HardLinks also covers graceful degradation on a
// platform where sync.HardLinksSupported() is false: the sync must still
// succeed, as independent copies rather than an error.
func TestSenderReceiver_HardLinks(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	mustWriteFile(t, filepath.Join(srcRoot, "original.txt"), "shared content")
	mustWriteFile(t, filepath.Join(srcRoot, "unrelated.txt"), "different content, must stay independent")
	if err := os.Link(filepath.Join(srcRoot, "original.txt"), filepath.Join(srcRoot, "linked.txt")); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}

	runSenderReceiver(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{Perms: true, Times: true, HardLinks: true})

	assertSameContent(t, filepath.Join(srcRoot, "original.txt"), filepath.Join(destRoot, "original.txt"))
	assertSameContent(t, filepath.Join(srcRoot, "linked.txt"), filepath.Join(destRoot, "linked.txt"))
	assertSameContent(t, filepath.Join(srcRoot, "unrelated.txt"), filepath.Join(destRoot, "unrelated.txt"))

	destOriginal := filepath.Join(destRoot, "original.txt")
	destLinked := filepath.Join(destRoot, "linked.txt")
	destUnrelated := filepath.Join(destRoot, "unrelated.txt")

	originalInfo, err := os.Stat(destOriginal)
	if err != nil {
		t.Fatalf("Stat %q: %v", destOriginal, err)
	}
	linkedInfo, err := os.Stat(destLinked)
	if err != nil {
		t.Fatalf("Stat %q: %v", destLinked, err)
	}
	unrelatedInfo, err := os.Stat(destUnrelated)
	if err != nil {
		t.Fatalf("Stat %q: %v", destUnrelated, err)
	}

	if os.SameFile(originalInfo, unrelatedInfo) {
		t.Fatalf("%q and %q are the same file, want independent - unrelated.txt must never be linked to the hard-link group", destOriginal, destUnrelated)
	}

	if !sync.HardLinksSupported() {
		if os.SameFile(originalInfo, linkedInfo) {
			t.Fatalf("%q and %q are the same file on a platform where HardLinksSupported() is false - "+
				"expected independent copies (graceful degradation), not a link this platform can't have detected", destOriginal, destLinked)
		}
		return
	}

	if !os.SameFile(originalInfo, linkedInfo) {
		t.Fatalf("%q and %q are independent files at the destination, want them hard-linked (same underlying file) like they are at the source", destOriginal, destLinked)
	}

	// Prove it directly: a write through one path must be visible through
	// the other.
	if err := os.WriteFile(destLinked, []byte("changed via the linked path"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", destLinked, err)
	}
	got, err := os.ReadFile(destOriginal)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", destOriginal, err)
	}
	if string(got) != "changed via the linked path" {
		t.Errorf("%q content = %q after writing through %q, want the change to be visible (they should be the same file)", destOriginal, got, destLinked)
	}
}

func TestSenderReceiver_HardLinksNotPreservedWithoutOptIn(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	mustWriteFile(t, filepath.Join(srcRoot, "original.txt"), "shared content")
	if err := os.Link(filepath.Join(srcRoot, "original.txt"), filepath.Join(srcRoot, "linked.txt")); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}

	runSenderReceiver(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{Perms: true, Times: true}) // HardLinks left false

	assertSameContent(t, filepath.Join(srcRoot, "original.txt"), filepath.Join(destRoot, "original.txt"))
	assertSameContent(t, filepath.Join(srcRoot, "linked.txt"), filepath.Join(destRoot, "linked.txt"))

	originalInfo, err := os.Stat(filepath.Join(destRoot, "original.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	linkedInfo, err := os.Stat(filepath.Join(destRoot, "linked.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if os.SameFile(originalInfo, linkedInfo) {
		t.Errorf("destination files are hard-linked despite AttrOptions.HardLinks being false - " +
			"hard-link detection must be opt-in, not run unconditionally")
	}
}

// runSenderReceiverWithOptions is runSenderReceiver with control over
// ReceiverOptions.
func runSenderReceiverWithOptions(t *testing.T, src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions, ropts ReceiverOptions) {
	t.Helper()

	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- Sender(sender, src, walkOpts, rules, attrOpts.HardLinks, CompressOptions{}) }()

	receiverErrCh := make(chan error, 1)
	go func() { receiverErrCh <- Receiver(receiver, dest, attrOpts, ropts) }()

	select {
	case err := <-receiverErrCh:
		if err != nil {
			t.Fatalf("Receiver returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receiver did not complete within 10s")
	}
	select {
	case err := <-senderErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sender did not complete within 10s")
	}
}

// runSenderReceiverWithCompressOptions is runSenderReceiverWithOptions
// with control over Sender's CompressOptions. Returns the number of bytes
// Sender actually wrote to the connection, so callers can compare
// compressed vs. uncompressed wire size.
func runSenderReceiverWithCompressOptions(t *testing.T, src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions, ropts ReceiverOptions, copts CompressOptions) (bytesWritten int64) {
	t.Helper()

	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := &countingReadWriter{rw: pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- Sender(sender, src, walkOpts, rules, attrOpts.HardLinks, copts) }()

	receiverErrCh := make(chan error, 1)
	go func() { receiverErrCh <- Receiver(receiver, dest, attrOpts, ropts) }()

	select {
	case err := <-receiverErrCh:
		if err != nil {
			t.Fatalf("Receiver returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receiver did not complete within 10s")
	}
	select {
	case err := <-senderErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sender did not complete within 10s")
	}

	return sender.written
}

func TestSenderReceiver_CompressReducesWireBytesForCompressibleContent(t *testing.T) {
	content := strings.Repeat("the quick brown fox jumps over the lazy dog ", 2000) // ~90KB, highly compressible

	uncompressedSrc, uncompressedDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(uncompressedSrc, "big.txt"), content)
	uncompressedBytes := runSenderReceiverWithCompressOptions(t, uncompressedSrc, uncompressedDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{}, CompressOptions{})

	compressedSrc, compressedDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(compressedSrc, "big.txt"), content)
	compressedBytes := runSenderReceiverWithCompressOptions(t, compressedSrc, compressedDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{},
		CompressOptions{Enabled: true, Level: DefaultCompressLevel})

	if compressedBytes >= uncompressedBytes {
		t.Errorf("compressed transfer wrote %d bytes, uncompressed wrote %d bytes, want compressed meaningfully smaller", compressedBytes, uncompressedBytes)
	}

	assertSameContent(t, filepath.Join(uncompressedSrc, "big.txt"), filepath.Join(uncompressedDest, "big.txt"))
	assertSameContent(t, filepath.Join(compressedSrc, "big.txt"), filepath.Join(compressedDest, "big.txt"))
}

func TestSenderReceiver_SkipCompressLeavesMatchingSuffixUncompressed(t *testing.T) {
	content := strings.Repeat("the quick brown fox jumps over the lazy dog ", 2000)

	src, dest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(src, "skip.bin"), content)

	uncompressedRun := t.TempDir()
	mustWriteFile(t, filepath.Join(uncompressedRun, "skip.bin"), content)
	baselineBytes := runSenderReceiverWithCompressOptions(t, uncompressedRun, t.TempDir(),
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{}, CompressOptions{})

	skipBytes := runSenderReceiverWithCompressOptions(t, src, dest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{},
		CompressOptions{Enabled: true, Level: DefaultCompressLevel, SkipSuffixes: []string{"bin"}})

	// Not exact equality (gob framing overhead varies slightly), but a
	// skipped file must stay close to the uncompressed baseline.
	if skipBytes < baselineBytes*9/10 {
		t.Errorf("skip-listed file wrote %d bytes, baseline (uncompressed) wrote %d bytes, want them close - skip-compress should have left this file uncompressed", skipBytes, baselineBytes)
	}

	assertSameContent(t, filepath.Join(src, "skip.bin"), filepath.Join(dest, "skip.bin"))
}

// Uses "Total bytes received" since Stats is computed receiver-side, and
// the compressible payload flows from Sender to Receiver.
func TestReceiver_StatsBytesReceivedReflectCompressedSize(t *testing.T) {
	content := strings.Repeat("the quick brown fox jumps over the lazy dog ", 2000)

	uncompressedSrc, uncompressedDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(uncompressedSrc, "big.txt"), content)
	var uncompressedOut bytes.Buffer
	runSenderReceiverWithCompressAndReceiverOptions(t, uncompressedSrc, uncompressedDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Stats: true, Output: &uncompressedOut}, CompressOptions{})

	compressedSrc, compressedDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(compressedSrc, "big.txt"), content)
	var compressedOut bytes.Buffer
	runSenderReceiverWithCompressAndReceiverOptions(t, compressedSrc, compressedDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Stats: true, Output: &compressedOut},
		CompressOptions{Enabled: true, Level: DefaultCompressLevel})

	uncompressedReceived := statsField(t, uncompressedOut.String(), "Total bytes received")
	compressedReceived := statsField(t, compressedOut.String(), "Total bytes received")

	if compressedReceived >= uncompressedReceived {
		t.Errorf("--stats reported compressed run received %d bytes, uncompressed run received %d bytes, want compressed smaller - "+
			"stats must reflect actual wire bytes, not original file size", compressedReceived, uncompressedReceived)
	}

	// Total file size is unaffected by compression, unlike bytes sent.
	if got, want := statsField(t, compressedOut.String(), "Total file size"), statsField(t, uncompressedOut.String(), "Total file size"); got != want {
		t.Errorf("Total file size = %d with compression, %d without, want equal - compression must not affect this field", got, want)
	}
}

// runSenderReceiverWithCompressAndReceiverOptions combines
// runSenderReceiverWithCompressOptions and runSenderReceiverWithOptions
// for tests needing both compression and receiver-side reporting control.
func runSenderReceiverWithCompressAndReceiverOptions(t *testing.T, src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions, ropts ReceiverOptions, copts CompressOptions) {
	t.Helper()

	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- Sender(sender, src, walkOpts, rules, attrOpts.HardLinks, copts) }()

	receiverErrCh := make(chan error, 1)
	go func() { receiverErrCh <- Receiver(receiver, dest, attrOpts, ropts) }()

	select {
	case err := <-receiverErrCh:
		if err != nil {
			t.Fatalf("Receiver returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receiver did not complete within 10s")
	}
	select {
	case err := <-senderErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sender did not complete within 10s")
	}
}

func TestSenderReceiver_CompressWorksWithDryRun(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()
	content := strings.Repeat("compressible dry-run content ", 500)
	mustWriteFile(t, filepath.Join(srcRoot, "file.txt"), content)

	var out bytes.Buffer
	runSenderReceiverWithCompressAndReceiverOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{DryRun: true, Itemize: true, Output: &out},
		CompressOptions{Enabled: true, Level: DefaultCompressLevel})

	if !strings.Contains(out.String(), "file.txt") {
		t.Errorf("dry-run itemize output = %q, want it to mention file.txt", out.String())
	}

	entries, err := os.ReadDir(destRoot)
	if err != nil {
		t.Fatalf("ReadDir(destRoot): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destRoot is not empty after a dry run with --compress: %v", entries)
	}
}

func TestSenderReceiver_CompressWorksWithHardLinks(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	content := strings.Repeat("shared hard-linked content ", 500)
	mustWriteFile(t, filepath.Join(srcRoot, "original.txt"), content)
	if err := os.Link(filepath.Join(srcRoot, "original.txt"), filepath.Join(srcRoot, "linked.txt")); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}

	runSenderReceiverWithCompressAndReceiverOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{Perms: true, Times: true, HardLinks: true},
		ReceiverOptions{}, CompressOptions{Enabled: true, Level: DefaultCompressLevel})

	assertSameContent(t, filepath.Join(srcRoot, "original.txt"), filepath.Join(destRoot, "original.txt"))
	assertSameContent(t, filepath.Join(srcRoot, "linked.txt"), filepath.Join(destRoot, "linked.txt"))

	if !sync.HardLinksSupported() {
		return
	}
	originalInfo, err := os.Stat(filepath.Join(destRoot, "original.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	linkedInfo, err := os.Stat(filepath.Join(destRoot, "linked.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !os.SameFile(originalInfo, linkedInfo) {
		t.Errorf("original.txt and linked.txt are independent files at the destination with --compress enabled, want them still hard-linked")
	}
}

// Covers the edge case where toWireDeltaOps' "nothing to compress" path
// is taken: an empty file (zero DataOps) and an unchanged file (all
// CopyOps).
func TestSenderReceiver_CompressWorksWithEmptyAndUnchangedFiles(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	const unchanged = "byte-identical at both ends, all copy ops"
	mustWriteFile(t, filepath.Join(srcRoot, "empty.txt"), "")
	mustWriteFile(t, filepath.Join(srcRoot, "unchanged.txt"), unchanged)
	mustWriteFile(t, filepath.Join(destRoot, "unchanged.txt"), unchanged)

	runSenderReceiverWithCompressAndReceiverOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{},
		CompressOptions{Enabled: true, Level: DefaultCompressLevel})

	assertSameContent(t, filepath.Join(srcRoot, "empty.txt"), filepath.Join(destRoot, "empty.txt"))
	assertSameContent(t, filepath.Join(srcRoot, "unchanged.txt"), filepath.Join(destRoot, "unchanged.txt"))
}

// buildRichTree creates a source tree exercising every write path
// Receiver has: a file, a nested directory with a file, a symlink, and
// (best-effort) two hard-linked files.
func buildRichTree(t *testing.T, root string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(root, "top.txt"), "top level content")
	mustMkdirAll(t, filepath.Join(root, "sub"))
	mustWriteFile(t, filepath.Join(root, "sub", "nested.txt"), "nested content")
	if err := os.Symlink("nested.txt", filepath.Join(root, "sub", "link.txt")); err != nil {
		t.Logf("symlink creation unsupported in this environment, tree will not include one: %v", err)
	}
	mustWriteFile(t, filepath.Join(root, "original.txt"), "shared content")
	if err := os.Link(filepath.Join(root, "original.txt"), filepath.Join(root, "linked.txt")); err != nil {
		t.Logf("hard link creation unsupported in this environment, tree will not include one: %v", err)
	}
}

func TestReceiver_DryRunMakesNoFilesystemChanges(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()
	buildRichTree(t, srcRoot)

	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil,
		sync.AttrOptions{Perms: true, Times: true, Owner: true, Group: true, Links: true, HardLinks: true},
		ReceiverOptions{DryRun: true})

	entries, err := os.ReadDir(destRoot)
	if err != nil {
		t.Fatalf("ReadDir(destRoot): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destRoot is not empty after a dry run: %v", entries)
	}
}

// Checks real rsync's documented guarantee that --itemize-changes output
// is identical between a dry run and a real run, comparing against a
// real run on a separate, equally fresh destination.
func TestReceiver_DryRunItemizeMatchesRealRunItemize(t *testing.T) {
	srcRoot := t.TempDir()
	buildRichTree(t, srcRoot)

	attrOpts := sync.AttrOptions{Perms: true, Times: true, Owner: true, Group: true, Links: true, HardLinks: true}

	dryRunDest := t.TempDir()
	var dryRunOutput bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, dryRunDest,
		sync.WalkOptions{Recursive: true}, nil, attrOpts,
		ReceiverOptions{DryRun: true, Itemize: true, Output: &dryRunOutput})

	realRunDest := t.TempDir()
	var realRunOutput bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, realRunDest,
		sync.WalkOptions{Recursive: true}, nil, attrOpts,
		ReceiverOptions{DryRun: false, Itemize: true, Output: &realRunOutput})

	if dryRunOutput.Len() == 0 {
		t.Fatalf("dry-run produced no itemize output at all - the test tree isn't exercising anything")
	}
	if dryRunOutput.String() != realRunOutput.String() {
		t.Errorf("dry-run itemize output does not match a real run's:\ndry-run:\n%s\nreal run:\n%s",
			dryRunOutput.String(), realRunOutput.String())
	}

	entries, err := os.ReadDir(dryRunDest)
	if err != nil {
		t.Fatalf("ReadDir(dryRunDest): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run destination is not empty: %v", entries)
	}
}

// TestReceiver_AppliesHardLinksFromReceivedGroups drives Receiver
// directly against a hand-built file list, so hard-link handling is
// verified even on a platform where Sender itself can't detect them
// (Windows). If Receiver failed to skip a secondary member, the peer
// goroutine below would never see its signature request and the test
// would time out rather than silently pass.
func TestReceiver_AppliesHardLinksFromReceivedGroups(t *testing.T) {
	destRoot := t.TempDir()

	peerReadsFromReceiver, receiverWritesToPeer := io.Pipe()
	receiverReadsFromPeer, peerWritesToReceiver := io.Pipe()
	receiver := pipeReadWriter{Reader: receiverReadsFromPeer, Writer: receiverWritesToPeer}

	// aaa-primary.txt sorts first, so it's group[0] (written normally);
	// zzz-secondary.txt must be skipped and linked instead.
	const content = "shared content, sent once for the whole group"
	entries := []sync.FileEntry{
		{Path: "aaa-primary.txt", Mode: 0o644, Size: int64(len(content))},
		{Path: "unrelated.txt", Mode: 0o644, Size: 5},
		{Path: "zzz-secondary.txt", Mode: 0o644, Size: int64(len(content))},
	}
	groups := []sync.HardLinkGroup{{"aaa-primary.txt", "zzz-secondary.txt"}}

	peerErrCh := make(chan error, 1)
	go func() {
		if err := sendFileList(peerWritesToReceiver, entries, groups); err != nil {
			peerErrCh <- fmt.Errorf("sending file list: %w", err)
			return
		}

		sigMsg, err := recvSignature(peerReadsFromReceiver)
		if err != nil {
			peerErrCh <- fmt.Errorf("receiving signature: %w", err)
			return
		}
		if sigMsg.Path != "aaa-primary.txt" {
			peerErrCh <- fmt.Errorf("signature requested for %q, want only \"aaa-primary.txt\" - "+
				"\"zzz-secondary.txt\" (a hard-link group's secondary member) must never reach the signature/delta exchange", sigMsg.Path)
			return
		}
		ops := sync.GenerateDelta(sigMsg.Sig, []byte(content))
		if err := sendDelta(peerWritesToReceiver, "aaa-primary.txt", ops, CompressOptions{}); err != nil {
			peerErrCh <- fmt.Errorf("sending delta: %w", err)
			return
		}

		sigMsg, err = recvSignature(peerReadsFromReceiver)
		if err != nil {
			peerErrCh <- fmt.Errorf("receiving signature: %w", err)
			return
		}
		if sigMsg.Path != "unrelated.txt" {
			peerErrCh <- fmt.Errorf("signature requested for %q, want \"unrelated.txt\"", sigMsg.Path)
			return
		}
		ops = sync.GenerateDelta(sigMsg.Sig, []byte("xxxxx"))
		if err := sendDelta(peerWritesToReceiver, "unrelated.txt", ops, CompressOptions{}); err != nil {
			peerErrCh <- fmt.Errorf("sending delta: %w", err)
			return
		}

		peerErrCh <- nil
	}()

	receiverErrCh := make(chan error, 1)
	go func() { receiverErrCh <- Receiver(receiver, destRoot, sync.AttrOptions{}, ReceiverOptions{}) }()

	select {
	case err := <-receiverErrCh:
		if err != nil {
			t.Fatalf("Receiver returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receiver did not complete within 10s - likely blocked waiting for a signature request for the secondary hard-link member that should have been skipped")
	}
	if err := <-peerErrCh; err != nil {
		t.Fatalf("peer goroutine: %v", err)
	}

	destPrimary := filepath.Join(destRoot, "aaa-primary.txt")
	destSecondary := filepath.Join(destRoot, "zzz-secondary.txt")
	destUnrelated := filepath.Join(destRoot, "unrelated.txt")

	primaryInfo, err := os.Stat(destPrimary)
	if err != nil {
		t.Fatalf("Stat %q: %v", destPrimary, err)
	}
	secondaryInfo, err := os.Stat(destSecondary)
	if err != nil {
		t.Fatalf("Stat %q: %v", destSecondary, err)
	}
	unrelatedInfo, err := os.Stat(destUnrelated)
	if err != nil {
		t.Fatalf("Stat %q: %v", destUnrelated, err)
	}

	if !os.SameFile(primaryInfo, secondaryInfo) {
		t.Errorf("%q and %q are independent files, want them hard-linked", destPrimary, destSecondary)
	}
	if os.SameFile(primaryInfo, unrelatedInfo) {
		t.Errorf("%q and %q are the same file, want independent", destPrimary, destUnrelated)
	}

	got, err := os.ReadFile(destSecondary)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", destSecondary, err)
	}
	if string(got) != content {
		t.Errorf("%q content = %q, want %q", destSecondary, got, content)
	}
}

func TestSender_ConnectionDropsMidTransfer(t *testing.T) {
	srcRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(srcRoot, "file.txt"), "content that will never get a delta exchanged for it")

	senderReadsFromPeer, peerWritesToSender := io.Pipe()
	peerReadsFromSender, senderWritesToPeer := io.Pipe()
	sender := pipeReadWriter{Reader: senderReadsFromPeer, Writer: senderWritesToPeer}

	go func() {
		// Read the file list, then vanish without ever sending a signature.
		_, _ = transport.ReadFrame(peerReadsFromSender)
		_ = peerWritesToSender.Close()
		_ = peerReadsFromSender.Close()
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Sender(sender, srcRoot, sync.WalkOptions{Recursive: true}, nil, false, CompressOptions{})
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("Sender returned nil error after the connection dropped mid-transfer, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sender hung instead of erroring out after the connection dropped")
	}
}

func TestReceiver_ConnectionDropsMidTransfer(t *testing.T) {
	destRoot := t.TempDir()

	peerReadsFromReceiver, receiverWritesToPeer := io.Pipe()
	receiverReadsFromPeer, peerWritesToReceiver := io.Pipe()
	receiver := pipeReadWriter{Reader: receiverReadsFromPeer, Writer: receiverWritesToPeer}

	go func() {
		_ = sendFileList(peerWritesToReceiver, []sync.FileEntry{{Path: "file.txt", Mode: 0o644}}, nil)
		// Read the signature, then vanish without ever sending a delta.
		_, _ = transport.ReadFrame(peerReadsFromReceiver)
		_ = peerWritesToReceiver.Close()
		_ = peerReadsFromReceiver.Close()
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- Receiver(receiver, destRoot, sync.AttrOptions{}, ReceiverOptions{}) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("Receiver returned nil error after the connection dropped mid-transfer, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Receiver hung instead of erroring out after the connection dropped")
	}
}

func assertSameContent(t *testing.T, srcPath, destPath string) {
	t.Helper()
	want, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("reading source %q: %v", srcPath, err)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading destination %q: %v", destPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s content = %q, want %q", destPath, got, want)
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
