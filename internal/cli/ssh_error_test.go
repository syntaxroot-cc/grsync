package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildFakeRemoteCommandNotFound compiles a stand-in for `ssh` that ignores
// its arguments, writes a "command not found"-shaped message to stderr (the
// same shape a real ssh forwards from a remote shell when the requested
// remote command isn't on PATH), and exits 127 - reproducing that failure
// without needing a real SSH server or a remote host missing grsync.
func buildFakeRemoteCommandNotFound(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	const source = `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "bash: line 1: grsync: command not found")
	os.Exit(127)
}
`
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatalf("writing fake rsh source: %v", err)
	}

	out := filepath.Join(dir, "fake-ssh")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fake rsh stand-in: %v\n%s", err, output)
	}
	return out
}

// TestE2E_SSHRemoteCommandNotFoundSurfacesStderr reproduces the real
// failure mode found during two-machine testing: the remote grsync binary
// isn't on the remote shell's PATH. Before this test's fix, the resulting
// error was an opaque "reading hello-ack: reading frame header: EOF" -
// technically accurate but useless for diagnosing the actual cause. The
// remote shell's real stderr ("command not found") was already being
// captured by transport.Session, just discarded at the call site instead
// of being surfaced.
func TestE2E_SSHRemoteCommandNotFoundSurfacesStderr(t *testing.T) {
	fakeRSH := buildFakeRemoteCommandNotFound(t)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "content")

	err := runGrsync(t, "-a", "-e", fakeRSH, src, "user@host:/remote/dest")
	if err == nil {
		t.Fatal("sync via a remote shell that can't find the remote command returned nil error, want one")
	}

	msg := err.Error()
	if !strings.Contains(msg, "command not found") {
		t.Errorf("error = %q, want it to surface the remote shell's actual stderr (\"command not found\")", msg)
	}
	if strings.Contains(msg, "reading frame header") {
		t.Errorf("error = %q, still leads with the opaque frame-level EOF instead of the more useful stderr detail", msg)
	}
}
