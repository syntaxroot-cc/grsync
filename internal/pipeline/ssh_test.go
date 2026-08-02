package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
	"github.com/syntaxroot-cc/grsync/internal/transport"
)

func requireLocalSSHServer(t *testing.T) {
	t.Helper()
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "127.0.0.1", "true")
	if err := cmd.Run(); err != nil {
		t.Skipf("no SSH server reachable at 127.0.0.1 for a non-interactive connection: %v", err)
	}
}

func buildGrsyncBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "grsync")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "github.com/syntaxroot-cc/grsync/cmd/grsync")
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building grsync binary: %v\n%s", err, output)
	}
	return out
}

// The remote command uses the built binary's full path rather than the
// bare "grsync" a real invocation would use, since nothing is installed
// on the test target's PATH.
func TestSSHLocalhost_SyncRoundTrip(t *testing.T) {
	requireLocalSSHServer(t)
	grsyncPath := buildGrsyncBinary(t)

	src := t.TempDir()
	dest := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "top.txt"), "top level content")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content")

	session, err := transport.Dial("", "", "127.0.0.1", []string{grsyncPath, "--server", dest}, false, false)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	if err := transport.Handshake(session); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- Sender(session, src, sync.WalkOptions{Recursive: true}, nil, false, CompressOptions{})
	}()

	select {
	case err := <-sendErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Sender did not complete within 20s")
	}

	if err := session.Close(); err != nil {
		t.Errorf("Session.Close returned error: %v", err)
	}

	assertSameContent(t, filepath.Join(src, "top.txt"), filepath.Join(dest, "top.txt"))
	assertSameContent(t, filepath.Join(src, "sub", "nested.txt"), filepath.Join(dest, "sub", "nested.txt"))
}

func TestSSHLocalhost_DryRunMakesNoChanges(t *testing.T) {
	requireLocalSSHServer(t)
	grsyncPath := buildGrsyncBinary(t)

	src := t.TempDir()
	dest := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "top.txt"), "top level content")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content")

	session, err := transport.Dial("", "", "127.0.0.1", []string{grsyncPath, "--server", "--dry-run", dest}, false, false)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	if err := transport.Handshake(session); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- Sender(session, src, sync.WalkOptions{Recursive: true}, nil, false, CompressOptions{})
	}()

	select {
	case err := <-sendErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Sender did not complete within 20s")
	}

	if err := session.Close(); err != nil {
		t.Errorf("Session.Close returned error: %v", err)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("ReadDir(dest): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dest is not empty after a --dry-run --server sync over real SSH: %v", entries)
	}
}

// Doesn't attempt to capture the remote process's stderr text; the
// destination matching the source byte-for-byte is what proves progress
// reporting's chunked write path didn't corrupt anything.
func TestSSHLocalhost_ProgressAndStatsDoNotBreakTheTransfer(t *testing.T) {
	requireLocalSSHServer(t)
	grsyncPath := buildGrsyncBinary(t)

	src := t.TempDir()
	dest := t.TempDir()
	content := strings.Repeat("z", progressWriteChunkSize*2+500)
	mustWriteFile(t, filepath.Join(src, "big.bin"), content)

	session, err := transport.Dial("", "", "127.0.0.1", []string{grsyncPath, "--server", "--progress", "--stats", dest}, false, false)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	if err := transport.Handshake(session); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- Sender(session, src, sync.WalkOptions{Recursive: true}, nil, false, CompressOptions{})
	}()

	select {
	case err := <-sendErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Sender did not complete within 20s")
	}

	if err := session.Close(); err != nil {
		t.Errorf("Session.Close returned error: %v", err)
	}

	assertSameContent(t, filepath.Join(src, "big.bin"), filepath.Join(dest, "big.bin"))
}

// Unlike --dry-run/--verbose/--progress/--stats, --compress needs no
// remote --server argv flag: Receiver just reacts to each deltaMessage's
// own Compressed marker.
func TestSSHLocalhost_CompressDoesNotBreakTheTransfer(t *testing.T) {
	requireLocalSSHServer(t)
	grsyncPath := buildGrsyncBinary(t)

	src := t.TempDir()
	dest := t.TempDir()
	content := strings.Repeat("compressible ssh transfer content ", 2000)
	mustWriteFile(t, filepath.Join(src, "big.txt"), content)

	session, err := transport.Dial("", "", "127.0.0.1", []string{grsyncPath, "--server", dest}, false, false)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	if err := transport.Handshake(session); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}

	copts := CompressOptions{Enabled: true, Level: DefaultCompressLevel}
	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- Sender(session, src, sync.WalkOptions{Recursive: true}, nil, false, copts)
	}()

	select {
	case err := <-sendErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Sender did not complete within 20s")
	}

	if err := session.Close(); err != nil {
		t.Errorf("Session.Close returned error: %v", err)
	}

	assertSameContent(t, filepath.Join(src, "big.txt"), filepath.Join(dest, "big.txt"))
}

func TestSSHLocalhost_IPv4ForwardedToSSHDoesNotBreakTheTransfer(t *testing.T) {
	requireLocalSSHServer(t)
	grsyncPath := buildGrsyncBinary(t)

	src := t.TempDir()
	dest := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "top.txt"), "synced over ssh with -4 forwarded")

	session, err := transport.Dial("", "", "127.0.0.1", []string{grsyncPath, "--server", dest}, true, false)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	if err := transport.Handshake(session); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- Sender(session, src, sync.WalkOptions{Recursive: true}, nil, false, CompressOptions{})
	}()

	select {
	case err := <-sendErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Sender did not complete within 20s")
	}

	if err := session.Close(); err != nil {
		t.Errorf("Session.Close returned error: %v", err)
	}

	assertSameContent(t, filepath.Join(src, "top.txt"), filepath.Join(dest, "top.txt"))
}

// The destination file is a genuine prefix of the source, so a real
// --append tail-only transfer happens, not just a harmless no-op.
func TestSSHLocalhost_AppendAndPartialDoNotBreakTheTransfer(t *testing.T) {
	requireLocalSSHServer(t)
	grsyncPath := buildGrsyncBinary(t)

	src := t.TempDir()
	dest := t.TempDir()
	prefix := "already on the remote side "
	full := prefix + "and now the new tail, sent over real ssh"
	mustWriteFile(t, filepath.Join(src, "growing.log"), full)
	mustWriteFile(t, filepath.Join(dest, "growing.log"), prefix)

	session, err := transport.Dial("", "", "127.0.0.1", []string{grsyncPath, "--server", "--append", "--partial", dest}, false, false)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	if err := transport.Handshake(session); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- Sender(session, src, sync.WalkOptions{Recursive: true}, nil, false, CompressOptions{})
	}()

	select {
	case err := <-sendErrCh:
		if err != nil {
			t.Fatalf("Sender returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Sender did not complete within 20s")
	}

	if err := session.Close(); err != nil {
		t.Errorf("Session.Close returned error: %v", err)
	}

	assertSameContent(t, filepath.Join(src, "growing.log"), filepath.Join(dest, "growing.log"))
}
