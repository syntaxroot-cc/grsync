package transport

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// requireLocalSSHServer skips the test unless an SSH server is reachable
// at 127.0.0.1 non-interactively (most dev/CI machines have a client but
// no sshd running). 127.0.0.1 is used instead of "localhost" to avoid a
// local DNS resolution quirk seen on this project's Windows dev machine.
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

// buildGrsyncBinary compiles cmd/grsync into a temp binary and returns its path.
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

	session, err := Dial("", "", "127.0.0.1", []string{grsyncPath, "--server"}, false, false)
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
