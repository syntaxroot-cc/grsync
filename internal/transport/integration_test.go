package transport

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// requireLocalSSHServer skips the calling test unless a real SSH server
// is actually reachable at 127.0.0.1 for the current user, non-
// interactively. This is a capability probe, not an assumption: most
// dev machines and CI runners have an ssh *client* installed but no
// sshd listening, so skipping here (rather than failing) is the normal,
// expected outcome almost everywhere this runs - the same pattern
// TestWalk_Symlink (internal/sync) and the Lchown/Mkfifo tests
// (internal/sync/attributes_test.go, specialfiles_test.go) already use
// for privilege/platform-gated behavior.
//
// 127.0.0.1 is used rather than "localhost": in at least this project's
// own Windows dev environment, the bundled ssh client failed to resolve
// "localhost" at all (a local DNS/hosts quirk, not an SSH problem) before
// ever getting to the real "is anything listening" question - the IP
// literal sidesteps that unrelated failure mode entirely.
func requireLocalSSHServer(t *testing.T) {
	t.Helper()
	cmd := exec.Command("ssh",
		"-o", "BatchMode=yes", // fail immediately rather than prompt (password, unknown host key, ...)
		"-o", "ConnectTimeout=5",
		"127.0.0.1", "true")
	if err := cmd.Run(); err != nil {
		t.Skipf("no SSH server reachable at 127.0.0.1 for a non-interactive connection: %v", err)
	}
}

// buildGrsyncBinary compiles cmd/grsync fresh into a temp file and
// returns its path, so this test exercises the real --server flag wiring
// in internal/cli/root.go exactly as a real invocation would, rather than
// some test-only stand-in for it.
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

func TestSSHLocalhost_HandshakeRoundTrip(t *testing.T) {
	requireLocalSSHServer(t)
	grsyncPath := buildGrsyncBinary(t)

	session, err := Dial("", "", "127.0.0.1", []string{grsyncPath, "--server"})
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- Handshake(session) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Handshake returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Handshake did not complete within 15s")
	}

	if err := session.Close(); err != nil {
		t.Errorf("Session.Close returned error: %v", err)
	}
}
