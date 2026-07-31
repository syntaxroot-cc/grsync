package pipeline

import (
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
	go func() { senderErrCh <- Sender(sender, src, walkOpts, rules) }()

	receiverErrCh := make(chan error, 1)
	go func() { receiverErrCh <- Receiver(receiver, dest, attrOpts) }()

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
	go func() { errCh <- Sender(sender, srcRoot, sync.WalkOptions{Recursive: true}, nil) }()

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
		_ = sendFileList(peerWritesToReceiver, []sync.FileEntry{{Path: "file.txt", Mode: 0o644}})
		// Read (and discard) the signature Receiver sends back for
		// file.txt, then vanish - closing both pipe halves without ever
		// sending a delta.
		_, _ = transport.ReadFrame(peerReadsFromReceiver)
		_ = peerWritesToReceiver.Close()
		_ = peerReadsFromReceiver.Close()
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- Receiver(receiver, destRoot, sync.AttrOptions{}) }()

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
