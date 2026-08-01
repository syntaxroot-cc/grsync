package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
	"github.com/syntaxroot-cc/grsync/internal/transport"
)

// runSenderReceiver drives Sender and Receiver concurrently over a pair
// of io.Pipes wired crosswise, the same in-memory-transport pattern used
// in internal/transport's own handshake test - no subprocess or SSH
// needed to validate the pipeline logic itself.
func runSenderReceiver(t *testing.T, src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions) {
	t.Helper()

	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- Sender(sender, src, walkOpts, rules, attrOpts.HardLinks) }()

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

// pipeReadWriter joins two io.Pipe halves into a single io.ReadWriter,
// the same helper internal/transport's own handshake test defines for
// itself - duplicated here rather than exported from transport, since
// it's a small, test-only convenience, not part of either package's
// real API.
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

// TestSenderReceiver_DirectoryAttributesSurviveChildCreation is the direct
// proof for the ordering fix in Receiver: a directory's mtime is set to a
// deliberately distinctive value at the source. If Receiver applied
// directory attributes immediately upon creating the directory (before
// writing its children), the filesystem would silently bump that mtime
// again the moment the child file inside it gets created afterward,
// and this test would catch that regression by finding the destination
// directory's mtime does NOT match the source's.
func TestSenderReceiver_DirectoryAttributesSurviveChildCreation(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	srcSub := filepath.Join(srcRoot, "sub")
	mustMkdirAll(t, srcSub)
	mustWriteFile(t, filepath.Join(srcSub, "child.txt"), "child content")

	// A deliberately distinctive, easy-to-misidentify-as-"now" mtime, set
	// on the source directory *after* its child already exists - matching
	// what a real source tree looks like (the directory's own mtime
	// reflects whenever it was last deliberately touched, not literally
	// "the moment before this test ran").
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
		t.Errorf("destination directory ModTime = %v, want %v (likely bumped by child creation - "+
			"directory attributes must be applied AFTER children are written, not before)",
			info.ModTime(), wantDirTime)
	}
}

// TestSenderReceiver_HardLinks is SC-18's end-to-end proof, with
// AttrOptions.HardLinks explicitly requested (the -H/--hard-links
// opt-in - see TestSenderReceiver_HardLinksNotPreservedWithoutOptIn for
// the default-off case): two hard-linked source files must arrive at the
// destination still hard-linked to each other (the same underlying file,
// not two independent copies that merely happen to have identical
// content), and an unrelated third file must NOT be linked to either of
// them. Where this platform can't detect hard links at all (Windows -
// sync.HardLinksSupported()), the same sync must still succeed and
// produce byte-correct content - as independent copies, not an error -
// exactly the graceful-degradation behavior sync.DetectHardLinks itself
// already documents.
//
// os.SameFile is used for the linked/unlinked check rather than
// platform-specific stat code in the test itself: it reports genuine
// same-file identity on every platform Go supports, including Windows
// (via the Win32 file index), even though grsync's own hard-link
// *detection* can't see that identity there - so this test can assert
// the real, portable ground truth regardless of what grsync itself was
// able to observe.
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

	// Prove it directly, not just via os.SameFile: a write through one
	// path must be visible through the other, the same proof
	// internal/sync's own TestApplyHardLinks uses.
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

// TestSenderReceiver_HardLinksNotPreservedWithoutOptIn is
// TestSenderReceiver_HardLinks's counterpart for the default case:
// without AttrOptions.HardLinks set, hard-linked source files must sync
// as independent copies - correct content, but not linked - even on a
// platform that could have detected and preserved the relationship. This
// is the direct proof that hard-link detection is genuinely opt-in
// (matching real rsync's own -H/--hard-links, not implied by any other
// flag), not just documented as opt-in while actually running
// unconditionally.
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

// runSenderReceiverWithOptions is runSenderReceiver's counterpart for
// tests that need control over ReceiverOptions (dry-run, itemize
// reporting) - kept as a separate helper rather than adding a parameter
// to runSenderReceiver itself, since none of that function's many
// existing callers care, and ReceiverOptions{} (real writes, no
// reporting) is exactly what they already get.
func runSenderReceiverWithOptions(t *testing.T, src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions, ropts ReceiverOptions) {
	t.Helper()

	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- Sender(sender, src, walkOpts, rules, attrOpts.HardLinks) }()

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

// buildRichTree creates a source tree exercising every write path
// Receiver has: a top-level file, a nested directory with a file inside
// it, a symlink, and (best-effort - silently omitted if unsupported in
// this environment) two hard-linked files - so a dry-run test against it
// genuinely exercises all eight audited write call sites from Receiver's
// own doc comment, not just the easy ones.
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

// TestReceiver_DryRunMakesNoFilesystemChanges is SC-11's single most
// important test: a dry-run sync against a completely empty destination
// must leave it completely empty afterward - not just "no error was
// returned." buildRichTree's source is specifically built to exercise
// every one of Receiver's eight audited write call sites (two regular-
// file MkdirAlls plus a WriteFile, a symlink's MkdirAll plus
// ApplyAttributes - which itself calls os.Symlink, not just chmod/
// chtimes - a directory's deferred ApplyAttributes, and
// ApplyHardLinks); if any one of them were reachable despite DryRun
// being set, this test catches it directly, by finding something in
// destRoot that shouldn't be there, rather than trusting the audit alone.
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

// TestReceiver_DryRunItemizeMatchesRealRunItemize is real rsync's own
// documented dry-run guarantee, made concrete: "The output of
// --itemize-changes is supposed to be exactly the same on a dry run and
// a subsequent real run" (rsync.1's --dry-run section) - compared here
// against a real run on a *separate*, equally fresh destination, not a
// second real run against the same one (which would legitimately report
// nothing left to do, proving nothing about the dry run's accuracy).
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

	// The dry run's destination must still be untouched - what makes the
	// comparison above meaningful in the first place: if the dry run had
	// actually written files, it wouldn't be comparable to a real run
	// against a genuinely fresh destination at all.
	entries, err := os.ReadDir(dryRunDest)
	if err != nil {
		t.Fatalf("ReadDir(dryRunDest): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run destination is not empty: %v", entries)
	}
}

// TestReceiver_AppliesHardLinksFromReceivedGroups exercises Receiver's
// hard-link handling directly against a hand-built file list, the same
// way TestReceiver_ConnectionDropsMidTransfer drives Receiver against a
// raw peer goroutine rather than a real Sender. This matters on a
// platform where sync.HardLinksSupported() is false (Windows): Receiver's
// own linking logic - skip a group's secondary members in the main loop,
// link them once their primary is written - has nothing to do with
// whether *this* platform's Sender could have detected the grouping, so
// it can and should be verified directly, standing in for whatever
// Sender a real Linux/macOS peer would be running.
//
// The peer goroutine responds to a signature request for "original.txt"
// only; if Receiver incorrectly asked for a signature for "linked.txt"
// too (i.e. failed to skip it as a secondary member), nothing would ever
// answer that second request and the test would time out rather than
// silently pass.
func TestReceiver_AppliesHardLinksFromReceivedGroups(t *testing.T) {
	destRoot := t.TempDir()

	peerReadsFromReceiver, receiverWritesToPeer := io.Pipe()
	receiverReadsFromPeer, peerWritesToReceiver := io.Pipe()
	receiver := pipeReadWriter{Reader: receiverReadsFromPeer, Writer: receiverWritesToPeer}

	// Named so the alphabetical sort order sync.DetectHardLinks itself
	// would produce is obvious at a glance: "aaa-primary.txt" sorts
	// first, so it's group[0] - the member Receiver writes normally -
	// and "zzz-secondary.txt" is the one that must be skipped and linked
	// instead.
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
		if err := sendDelta(peerWritesToReceiver, "aaa-primary.txt", ops); err != nil {
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
		if err := sendDelta(peerWritesToReceiver, "unrelated.txt", ops); err != nil {
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

// TestSender_ConnectionDropsMidTransfer confirms a dropped connection
// produces a prompt, clear error - not a hang and not a silently
// swallowed failure. The "receiver" here reads the file list (so Sender
// gets past that point) and then closes its side without ever sending a
// signature, simulating a connection that dies mid-transfer.
func TestSender_ConnectionDropsMidTransfer(t *testing.T) {
	srcRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(srcRoot, "file.txt"), "content that will never get a delta exchanged for it")

	senderReadsFromPeer, peerWritesToSender := io.Pipe()
	peerReadsFromSender, senderWritesToPeer := io.Pipe()
	sender := pipeReadWriter{Reader: senderReadsFromPeer, Writer: senderWritesToPeer}

	go func() {
		// Read (and discard) exactly the file list frame, then vanish -
		// close both pipe halves without ever sending a signature back,
		// simulating a connection that dies immediately after the initial
		// exchange.
		_, _ = transport.ReadFrame(peerReadsFromSender)
		_ = peerWritesToSender.Close()
		_ = peerReadsFromSender.Close()
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- Sender(sender, srcRoot, sync.WalkOptions{Recursive: true}, nil, false) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("Sender returned nil error after the connection dropped mid-transfer, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sender hung instead of erroring out after the connection dropped")
	}
}

// TestReceiver_ConnectionDropsMidTransfer is TestSender_ConnectionDropsMidTransfer's
// counterpart for the other direction: the "sender" here sends a valid
// file list, then vanishes without ever responding to the signature
// Receiver sends back.
func TestReceiver_ConnectionDropsMidTransfer(t *testing.T) {
	destRoot := t.TempDir()

	peerReadsFromReceiver, receiverWritesToPeer := io.Pipe()
	receiverReadsFromPeer, peerWritesToReceiver := io.Pipe()
	receiver := pipeReadWriter{Reader: receiverReadsFromPeer, Writer: receiverWritesToPeer}

	go func() {
		_ = sendFileList(peerWritesToReceiver, []sync.FileEntry{{Path: "file.txt", Mode: 0o644}}, nil)
		// Read (and discard) the signature Receiver sends back for
		// file.txt, then vanish - closing both pipe halves without ever
		// sending a delta.
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
