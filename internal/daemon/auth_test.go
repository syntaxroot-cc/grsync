package daemon

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecretsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rsyncd.secrets")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing secrets file: %v", err)
	}
	return path
}

// authPipe wires up a client/server conn pair over io.Pipe, each side's
// writes also captured into its own log buffer - so a test can inspect
// every byte either side put on the wire, not just the final result.
func authPipe() (client, server *conn, clientWireLog, serverWireLog *bytes.Buffer) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	clientWireLog = &bytes.Buffer{}
	serverWireLog = &bytes.Buffer{}
	client = newConn(pipeReadWriter{Reader: clientR, Writer: io.MultiWriter(clientW, clientWireLog)})
	server = newConn(pipeReadWriter{Reader: serverR, Writer: io.MultiWriter(serverW, serverWireLog)})
	return client, server, clientWireLog, serverWireLog
}

func TestAuth_NoAuthRequiredSendsOK(t *testing.T) {
	client, server, _, _ := authPipe()
	m := Module{Name: "public"} // no AuthUsers

	errCh := make(chan error, 1)
	go func() {
		_, err := ServeAuth(server, m)
		errCh <- err
	}()

	if err := DialAuth(client, "", ""); err != nil {
		t.Fatalf("DialAuth returned error: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ServeAuth returned error: %v", err)
	}
}

func TestAuth_CorrectPasswordSucceeds(t *testing.T) {
	secretsPath := writeSecretsFile(t, "alice:hunter2\nbob:swordfish\n")
	m := Module{Name: "private", AuthUsers: []string{"alice", "bob"}, SecretsFile: secretsPath}

	client, server, _, _ := authPipe()
	errCh := make(chan error, 1)
	var gotUser string
	go func() {
		var err error
		gotUser, err = ServeAuth(server, m)
		errCh <- err
	}()

	if err := DialAuth(client, "alice", "hunter2"); err != nil {
		t.Fatalf("DialAuth returned error: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ServeAuth returned error: %v", err)
	}
	if gotUser != "alice" {
		t.Errorf("ServeAuth returned user %q, want %q", gotUser, "alice")
	}
}

func TestAuth_WrongPasswordFails(t *testing.T) {
	secretsPath := writeSecretsFile(t, "alice:hunter2\n")
	m := Module{Name: "private", AuthUsers: []string{"alice"}, SecretsFile: secretsPath}

	client, server, _, _ := authPipe()
	errCh := make(chan error, 1)
	go func() {
		_, err := ServeAuth(server, m)
		errCh <- err
	}()

	dialErr := DialAuth(client, "alice", "wrong-password")
	if dialErr == nil {
		t.Fatalf("DialAuth with a wrong password returned nil error, want an error")
	}
	if !errors.Is(dialErr, ErrAuthFailed) {
		t.Errorf("DialAuth error = %v, want it to wrap ErrAuthFailed", dialErr)
	}
	if err := <-errCh; !errors.Is(err, ErrAuthFailed) {
		t.Errorf("ServeAuth error = %v, want ErrAuthFailed", err)
	}
}

func TestAuth_UnauthorizedUserFails(t *testing.T) {
	secretsPath := writeSecretsFile(t, "alice:hunter2\n")
	m := Module{Name: "private", AuthUsers: []string{"alice"}, SecretsFile: secretsPath}

	client, server, _, _ := authPipe()
	errCh := make(chan error, 1)
	go func() {
		_, err := ServeAuth(server, m)
		errCh <- err
	}()

	dialErr := DialAuth(client, "eve", "hunter2")
	if dialErr == nil {
		t.Fatalf("DialAuth for an unauthorized user returned nil error, want an error")
	}
	if err := <-errCh; !errors.Is(err, ErrAuthFailed) {
		t.Errorf("ServeAuth error = %v, want ErrAuthFailed", err)
	}
}

// TestAuth_NoPlaintextPasswordOnWire is the self-review requirement made
// concrete: it inspects the actual bytes each side wrote, not just the
// outcome, and fails if the raw password ever appears in either
// direction.
func TestAuth_NoPlaintextPasswordOnWire(t *testing.T) {
	const password = "correct-horse-battery-staple"
	secretsPath := writeSecretsFile(t, "alice:"+password+"\n")
	m := Module{Name: "private", AuthUsers: []string{"alice"}, SecretsFile: secretsPath}

	client, server, clientLog, serverLog := authPipe()
	errCh := make(chan error, 1)
	go func() {
		_, err := ServeAuth(server, m)
		errCh <- err
	}()

	if err := DialAuth(client, "alice", password); err != nil {
		t.Fatalf("DialAuth returned error: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ServeAuth returned error: %v", err)
	}

	if strings.Contains(clientLog.String(), password) {
		t.Errorf("client wire log contains the plaintext password: %q", clientLog.String())
	}
	if strings.Contains(serverLog.String(), password) {
		t.Errorf("server wire log contains the plaintext password: %q", serverLog.String())
	}
}

func TestAuth_MissingSecretsFileFails(t *testing.T) {
	m := Module{Name: "private", AuthUsers: []string{"alice"}, SecretsFile: filepath.Join(t.TempDir(), "does-not-exist")}

	client, server, _, _ := authPipe()
	errCh := make(chan error, 1)
	go func() {
		_, err := ServeAuth(server, m)
		errCh <- err
	}()

	dialErr := DialAuth(client, "alice", "hunter2")
	if dialErr == nil {
		t.Fatalf("DialAuth with a missing secrets file returned nil error, want an error")
	}
	if err := <-errCh; !errors.Is(err, ErrAuthFailed) {
		t.Errorf("ServeAuth error = %v, want ErrAuthFailed", err)
	}
}
