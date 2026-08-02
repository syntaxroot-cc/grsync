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

// requireLocalSSHServer and buildGrsyncBinary mirror
// internal/transport/integration_test.go's own helpers of the same name
// and purpose (a real SSH server capability probe, and building the real
// binary fresh) - duplicated rather than shared across packages, since Go
// test files aren't importable, and these are small enough that a shared
// test-support package would be more machinery than the two call sites
// justify.
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

// TestSSHLocalhost_SyncRoundTrip is this ticket's real, over-the-wire
// proof: it spawns the actual built grsync binary in --server mode via
// real ssh to 127.0.0.1 (not a mock, not an in-process pipe), runs the
// real Sender against that connection, and confirms the destination tree
// genuinely matches the source.
//
// The remote command here is the built binary's *full path*, not the bare
// "grsync" internal/cli's syncToRemote actually uses for a real
// invocation. That's a deliberate, documented difference for
// testability: a plain `go test` run has no "grsync" installed on the
// target's PATH to find (nothing was ever "installed" anywhere), so this
// test bypasses that PATH-resolution question entirely and points ssh
// directly at the freshly-built binary instead. internal/cli's real
// behavior - assuming "grsync" is on the remote PATH, exactly like real
// rsync assumes "rsync" is - is unit-tested elsewhere (BuildRSHCommand,
// syncToRemote's construction), just not exercised through an actual
// remote PATH lookup here.
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

// TestSSHLocalhost_DryRunMakesNoChanges is the real, over-the-wire proof
// that --dry-run's no-write guarantee holds for the SSH transport
// specifically: the remote grsync --server process here is invoked with
// --dry-run on its own command line (exactly what internal/cli's
// syncToRemote does for a real invocation - see its own doc comment),
// so this exercises the actual mechanism a real `grsync --dry-run src
// user@host:dest` run would use, not a stand-in for it.
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

// TestSSHLocalhost_ProgressAndStatsDoNotBreakTheTransfer is the real,
// over-the-wire proof that --progress/--stats on the remote --server
// command line (exactly what internal/cli's syncToRemote adds for a real
// invocation - see its own doc comment) don't corrupt or interfere with
// the actual transfer. It does not attempt to capture and verify the
// remote process's own stderr text (session.go's live passthrough of
// it) - that would need OS-level stderr redirection in this test
// process, disproportionate complexity for what SC-11's own SSH tests
// already established isn't otherwise verified end to end (they check
// write-safety and transfer correctness, not stderr content); the
// destination matching the source byte-for-byte is what actually proves
// progress reporting's chunked write path didn't corrupt anything.
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

// TestSSHLocalhost_CompressDoesNotBreakTheTransfer is the real,
// over-the-wire proof that --compress/-z's client-side decision (see
// pipeline.CompressOptions' own doc comment - Sender runs locally here,
// so no remote --server argv change is needed at all, unlike --dry-run/
// --itemize-changes/--verbose/--progress/--stats) doesn't corrupt or
// interfere with an actual transfer over real SSH: the remote --server
// process needs no compression-related flag on its own command line,
// since its Receiver just reacts to each deltaMessage's own Compressed
// marker.
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

// TestSSHLocalhost_IPv4ForwardedToSSHDoesNotBreakTheTransfer is SC-14's
// real, over-the-wire proof for the SSH transport: transport.Dial's
// ipv4 parameter reaches transport.BuildRSHCommand, which inserts a real
// "-4" into the spawned ssh process's own argv (see BuildRSHCommand's
// own doc comment - grsync never dials this connection itself, ssh
// does) - this drives that real path end to end against a real local
// sshd and confirms the forwarded -4 doesn't break anything, connecting
// to 127.0.0.1 (a genuine IPv4 address, so ssh's own -4 has nothing to
// object to here).
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
