package cli

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
)

// startTestDaemon listens on 127.0.0.1:0 (an OS-assigned free port) and
// serves cfg in the background until the test ends - a real TCP listener,
// not a stand-in, so this exercises the same connection code an actual
// `grsync ... rsync://host/module` invocation goes through end to end,
// the same way TestE2E_LocalToLocal drives the real CLI command rather
// than internal/pipeline directly.
func startTestDaemon(t *testing.T, cfg *daemon.Config) (port int, errLog *bytes.Buffer) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	errLog = &bytes.Buffer{}
	go func() { _ = daemon.Serve(ln, cfg, errLog) }()

	return ln.Addr().(*net.TCPAddr).Port, errLog
}

// runGrsync executes the real root command with args, exactly the way a
// user's invocation would, with stdin fixed to an empty, non-terminal
// reader so credential resolution never depends on whether the test
// runner happens to have a real controlling terminal attached.
func runGrsync(t *testing.T, args ...string) error {
	t.Helper()
	cmd := NewRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetIn(strings.NewReader(""))
	return cmd.Execute()
}

func TestE2E_LocalToRsyncDaemon_Anonymous(t *testing.T) {
	modRoot := t.TempDir()
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"incoming": {Name: "incoming", Path: modRoot, ReadOnly: false},
	}}
	port, errLog := startTestDaemon(t, cfg)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "hello.txt"), "pushed to a real rsync:// daemon URL")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content")

	dest := fmt.Sprintf("rsync://127.0.0.1:%d/incoming", port)
	if err := runGrsync(t, "-a", src, dest); err != nil {
		t.Fatalf("grsync %s returned error: %v", dest, err)
	}

	got, err := os.ReadFile(filepath.Join(modRoot, "hello.txt"))
	if err != nil {
		t.Fatalf("reading synced file: %v", err)
	}
	if string(got) != "pushed to a real rsync:// daemon URL" {
		t.Errorf("content = %q, want %q", got, "pushed to a real rsync:// daemon URL")
	}
	if _, err := os.ReadFile(filepath.Join(modRoot, "sub", "nested.txt")); err != nil {
		t.Errorf("reading synced nested file: %v", err)
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

func TestE2E_LocalToRsyncDaemon_AuthenticatedViaEnvVar(t *testing.T) {
	modRoot := t.TempDir()
	secretsPath := writeTestSecretsFile(t, "alice:hunter2\n")
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"private": {Name: "private", Path: modRoot, ReadOnly: false, AuthUsers: []string{"alice"}, SecretsFile: secretsPath},
	}}
	port, _ := startTestDaemon(t, cfg)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "secret.txt"), "authenticated upload")

	t.Setenv("RSYNC_PASSWORD", "hunter2")
	dest := fmt.Sprintf("rsync://alice@127.0.0.1:%d/private", port)
	if err := runGrsync(t, "-a", src, dest); err != nil {
		t.Fatalf("grsync %s returned error: %v", dest, err)
	}

	got, err := os.ReadFile(filepath.Join(modRoot, "secret.txt"))
	if err != nil {
		t.Fatalf("reading synced file: %v", err)
	}
	if string(got) != "authenticated upload" {
		t.Errorf("content = %q, want %q", got, "authenticated upload")
	}
}

func TestE2E_LocalToRsyncDaemon_WrongPasswordFailsClearly(t *testing.T) {
	modRoot := t.TempDir()
	secretsPath := writeTestSecretsFile(t, "alice:hunter2\n")
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"private": {Name: "private", Path: modRoot, ReadOnly: false, AuthUsers: []string{"alice"}, SecretsFile: secretsPath},
	}}
	port, _ := startTestDaemon(t, cfg)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "secret.txt"), "should never arrive")

	t.Setenv("RSYNC_PASSWORD", "wrong-password")
	dest := fmt.Sprintf("rsync://alice@127.0.0.1:%d/private", port)
	err := runGrsync(t, "-a", src, dest)
	if err == nil {
		t.Fatalf("grsync %s with a wrong password returned nil error, want an error", dest)
	}

	if _, statErr := os.Stat(filepath.Join(modRoot, "secret.txt")); !os.IsNotExist(statErr) {
		t.Errorf("secret.txt should not have been written after a failed auth, stat error = %v", statErr)
	}
}

func TestE2E_LocalToRsyncDaemon_ReadOnlyModuleRejectsUpload(t *testing.T) {
	modRoot := t.TempDir()
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"public": {Name: "public", Path: modRoot, ReadOnly: true},
	}}
	port, _ := startTestDaemon(t, cfg)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "upload.txt"), "should be refused")

	dest := fmt.Sprintf("rsync://127.0.0.1:%d/public", port)
	if err := runGrsync(t, "-a", src, dest); err == nil {
		t.Fatalf("grsync %s against a read-only module returned nil error, want an error", dest)
	}
}

func TestE2E_PullingFromRsyncDaemonSourceIsRejected(t *testing.T) {
	dest := t.TempDir()
	err := runGrsync(t, "rsync://127.0.0.1:8730/whatever", dest)
	if err == nil {
		t.Fatalf("grsync with an rsync:// source returned nil error, want a clear \"not yet supported\" error")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("error = %q, want it to explain pulling isn't supported", err.Error())
	}
}

func TestE2E_RsyncDaemonDestinationMissingModuleIsRejected(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")

	err := runGrsync(t, src, "rsync://127.0.0.1:8730")
	if err == nil {
		t.Fatalf("grsync with a moduleless rsync:// destination returned nil error, want an error")
	}
}

func TestE2E_RsyncDaemonDestinationSubPathIsRejected(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")

	err := runGrsync(t, src, "rsync://127.0.0.1:8730/module/subdir")
	if err == nil {
		t.Fatalf("grsync with a sub-path rsync:// destination returned nil error, want an error")
	}
}

func TestE2E_LocalToRsyncDaemon_PasswordFile(t *testing.T) {
	// No platform skip here: unlike TestE2E_PasswordFileWorldReadableIsRejected,
	// this doesn't depend on the world-readable check actually enforcing
	// anything (checkPasswordFilePermissions is a no-op on Windows, but the
	// --password-file flag and its happy path work identically everywhere).
	modRoot := t.TempDir()
	secretsPath := writeTestSecretsFile(t, "alice:hunter2\n")
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"private": {Name: "private", Path: modRoot, ReadOnly: false, AuthUsers: []string{"alice"}, SecretsFile: secretsPath},
	}}
	port, _ := startTestDaemon(t, cfg)

	passwordFile := filepath.Join(t.TempDir(), "grsync.password")
	if err := os.WriteFile(passwordFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing password file: %v", err)
	}

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "via-file.txt"), "authenticated via --password-file")

	dest := fmt.Sprintf("rsync://alice@127.0.0.1:%d/private", port)
	if err := runGrsync(t, "-a", "--password-file", passwordFile, src, dest); err != nil {
		t.Fatalf("grsync %s returned error: %v", dest, err)
	}

	got, err := os.ReadFile(filepath.Join(modRoot, "via-file.txt"))
	if err != nil {
		t.Fatalf("reading synced file: %v", err)
	}
	if string(got) != "authenticated via --password-file" {
		t.Errorf("content = %q, want %q", got, "authenticated via --password-file")
	}
}

func TestE2E_PasswordFileWorldReadableIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("world-readable permission check is POSIX-only, see checkPasswordFilePermissions")
	}

	modRoot := t.TempDir()
	secretsPath := writeTestSecretsFile(t, "alice:hunter2\n")
	cfg := &daemon.Config{Modules: map[string]daemon.Module{
		"private": {Name: "private", Path: modRoot, ReadOnly: false, AuthUsers: []string{"alice"}, SecretsFile: secretsPath},
	}}
	port, _ := startTestDaemon(t, cfg)

	passwordFile := filepath.Join(t.TempDir(), "world-readable.password")
	if err := os.WriteFile(passwordFile, []byte("hunter2\n"), 0o644); err != nil {
		t.Fatalf("writing password file: %v", err)
	}

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "should never be sent")

	dest := fmt.Sprintf("rsync://alice@127.0.0.1:%d/private", port)
	err := runGrsync(t, "--password-file", passwordFile, src, dest)
	if err == nil {
		t.Fatalf("grsync with a world-readable --password-file returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), "world readable") {
		t.Errorf("error = %q, want it to mention the file being world readable", err.Error())
	}
}

func writeTestSecretsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rsyncd.secrets")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing secrets file: %v", err)
	}
	return path
}
