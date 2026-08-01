package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

func dialRawConn(t *testing.T, addr string) net.Conn {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return nc
}

func TestDialClient_DownloadOverRealTCP(t *testing.T) {
	modRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(modRoot, "hello.txt"), "hello via DialClient")

	cfg := &Config{Modules: map[string]Module{
		"public": {Name: "public", Path: modRoot, ReadOnly: true, List: true},
	}}
	addr, errLog := startTestDaemon(t, cfg)
	nc := dialRawConn(t, addr)

	dest := t.TempDir()
	err := DialClient(nc, "public", "", StaticPassword(""), DirectionGet, dest, nil, sync.WalkOptions{}, sync.AttrOptions{})
	if err != nil {
		t.Fatalf("DialClient returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(got) != "hello via DialClient" {
		t.Errorf("downloaded content = %q, want %q", got, "hello via DialClient")
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

func TestDialClient_UploadOverRealTCP(t *testing.T) {
	modRoot := t.TempDir()
	secretsPath := writeSecretsFile(t, "alice:hunter2\n")

	cfg := &Config{Modules: map[string]Module{
		"incoming": {Name: "incoming", Path: modRoot, ReadOnly: false, AuthUsers: []string{"alice"}, SecretsFile: secretsPath},
	}}
	addr, errLog := startTestDaemon(t, cfg)
	nc := dialRawConn(t, addr)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "upload.txt"), "pushed via DialClient")
	rules, err := sync.CompileRules(nil)
	if err != nil {
		t.Fatalf("compiling empty rule set: %v", err)
	}

	err = DialClient(nc, "incoming", "alice", StaticPassword("hunter2"), DirectionPut, src, rules, sync.WalkOptions{}, sync.AttrOptions{})
	if err != nil {
		t.Fatalf("DialClient returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(modRoot, "upload.txt"))
	if err != nil {
		t.Fatalf("reading uploaded file: %v", err)
	}
	if string(got) != "pushed via DialClient" {
		t.Errorf("uploaded content = %q, want %q", got, "pushed via DialClient")
	}
	if errLog.Len() != 0 {
		t.Errorf("daemon error log = %q, want empty", errLog.String())
	}
}

// TestDialClient_PasswordFuncNotCalledForAnonymousModule is the laziness
// guarantee made concrete: PasswordFunc must never be invoked when the
// server never challenges for a password, exactly matching real rsync's
// own auth_client(), which is only ever called in response to an
// AUTHREQD line. A PasswordFunc backed by an interactive terminal prompt
// or a --password-file read must not fire against an anonymous module.
func TestDialClient_PasswordFuncNotCalledForAnonymousModule(t *testing.T) {
	modRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(modRoot, "open.txt"), "no auth needed")

	cfg := &Config{Modules: map[string]Module{
		"public": {Name: "public", Path: modRoot, ReadOnly: true, List: true},
	}}
	addr, _ := startTestDaemon(t, cfg)
	nc := dialRawConn(t, addr)

	called := false
	poisonedPassword := PasswordFunc(func() (string, error) {
		called = true
		return "", nil
	})

	dest := t.TempDir()
	err := DialClient(nc, "public", "", poisonedPassword, DirectionGet, dest, nil, sync.WalkOptions{}, sync.AttrOptions{})
	if err != nil {
		t.Fatalf("DialClient returned error: %v", err)
	}
	if called {
		t.Errorf("PasswordFunc was called for a module that never requested authentication")
	}
}

func TestDialClient_RejectsEmptyModule(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{}}
	addr, _ := startTestDaemon(t, cfg)
	nc := dialRawConn(t, addr)

	err := DialClient(nc, "", "", StaticPassword(""), DirectionGet, t.TempDir(), nil, sync.WalkOptions{}, sync.AttrOptions{})
	if err == nil {
		t.Fatalf("DialClient with an empty module returned nil error, want an error")
	}
}
