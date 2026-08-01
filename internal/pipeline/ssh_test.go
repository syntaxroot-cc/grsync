package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	session, err := transport.Dial("", "", "127.0.0.1", []string{grsyncPath, "--server", dest})
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	if err := transport.Handshake(session); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- Sender(session, src, sync.WalkOptions{Recursive: true}, nil, false)
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
