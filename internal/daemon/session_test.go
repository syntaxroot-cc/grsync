package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

func modulePipe() (client, server *conn) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	client = newConn(pipeReadWriter{Reader: clientR, Writer: clientW})
	server = newConn(pipeReadWriter{Reader: serverR, Writer: serverW})
	return client, server
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating parent dirs: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
}

func TestServeModule_GetDownloadsFiles(t *testing.T) {
	modRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(modRoot, "hello.txt"), "hello from the module")
	mustWriteFile(t, filepath.Join(modRoot, "sub", "nested.txt"), "nested content")

	m := Module{Name: "public", Path: modRoot, ReadOnly: true}

	client, server := modulePipe()
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- ServeModule(server, m) }()

	dest := t.TempDir()
	if err := DialModule(client, DirectionGet, dest, nil, sync.WalkOptions{}, sync.AttrOptions{Perms: true}); err != nil {
		t.Fatalf("DialModule returned error: %v", err)
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("ServeModule returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("reading downloaded hello.txt: %v", err)
	}
	if string(got) != "hello from the module" {
		t.Errorf("hello.txt content = %q, want %q", got, "hello from the module")
	}
	got, err = os.ReadFile(filepath.Join(dest, "sub", "nested.txt"))
	if err != nil {
		t.Fatalf("reading downloaded sub/nested.txt: %v", err)
	}
	if string(got) != "nested content" {
		t.Errorf("sub/nested.txt content = %q, want %q", got, "nested content")
	}
}

func TestServeModule_GetHonorsModuleExclude(t *testing.T) {
	modRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(modRoot, "keep.txt"), "keep me")
	mustWriteFile(t, filepath.Join(modRoot, "secret.key"), "should never leave the module")

	m := Module{Name: "public", Path: modRoot, ReadOnly: true, Exclude: []string{"*.key"}}

	client, server := modulePipe()
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- ServeModule(server, m) }()

	dest := t.TempDir()
	if err := DialModule(client, DirectionGet, dest, nil, sync.WalkOptions{}, sync.AttrOptions{}); err != nil {
		t.Fatalf("DialModule returned error: %v", err)
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("ServeModule returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "keep.txt")); err != nil {
		t.Errorf("keep.txt should have been downloaded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "secret.key")); !os.IsNotExist(err) {
		t.Errorf("secret.key should have been excluded, stat error = %v", err)
	}
}

func TestServeModule_PutToReadOnlyModuleFails(t *testing.T) {
	modRoot := t.TempDir()
	m := Module{Name: "public", Path: modRoot, ReadOnly: true}

	client, server := modulePipe()
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- ServeModule(server, m) }()

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "upload.txt"), "trying to push this up")

	dialErr := DialModule(client, DirectionPut, src, nil, sync.WalkOptions{}, sync.AttrOptions{})
	if dialErr == nil {
		t.Fatalf("DialModule against a read-only module returned nil error, want an error")
	}
	if err := <-serverErrCh; !errors.Is(err, ErrReadOnly) {
		t.Errorf("ServeModule error = %v, want ErrReadOnly", err)
	}
	if _, err := os.Stat(filepath.Join(modRoot, "upload.txt")); !os.IsNotExist(err) {
		t.Errorf("upload.txt should not have been written to the read-only module, stat error = %v", err)
	}
}

func TestServeModule_PutToWritableModuleSucceeds(t *testing.T) {
	modRoot := t.TempDir()
	m := Module{Name: "incoming", Path: modRoot, ReadOnly: false}

	client, server := modulePipe()
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- ServeModule(server, m) }()

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "upload.txt"), "pushed content")

	rules, err := sync.CompileRules(nil)
	if err != nil {
		t.Fatalf("compiling empty rule set: %v", err)
	}
	if err := DialModule(client, DirectionPut, src, rules, sync.WalkOptions{}, sync.AttrOptions{}); err != nil {
		t.Fatalf("DialModule returned error: %v", err)
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("ServeModule returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(modRoot, "upload.txt"))
	if err != nil {
		t.Fatalf("reading uploaded file from module: %v", err)
	}
	if string(got) != "pushed content" {
		t.Errorf("uploaded content = %q, want %q", got, "pushed content")
	}
}
