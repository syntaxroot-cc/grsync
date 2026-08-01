package daemon

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLine_RejectsOverLongLineInsteadOfUnboundedGrowth(t *testing.T) {
	huge := strings.Repeat("a", maxLineLength+1) + "\n"
	r := bufio.NewReader(strings.NewReader(huge))

	_, err := readLine(r)
	if err == nil {
		t.Fatalf("readLine with a %d-byte line returned nil error, want an error", len(huge)-1)
	}
}

// pipeReadWriter joins two io.Pipe halves into a single io.ReadWriter -
// the same small helper internal/transport and internal/pipeline each
// define for their own tests.
type pipeReadWriter struct {
	io.Reader
	io.Writer
}

func testConfig() *Config {
	return &Config{Modules: map[string]Module{
		"public":  {Name: "public", Path: "/srv/public", ReadOnly: true, List: true, Comment: "public files"},
		"hidden":  {Name: "hidden", Path: "/srv/hidden", ReadOnly: true, List: false, Comment: "not listed, but reachable by name"},
		"private": {Name: "private", Path: "/srv/private", ReadOnly: false, List: true},
	}}
}

func TestGreeting_ModuleSelection(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	client := newConn(pipeReadWriter{Reader: clientR, Writer: clientW})
	server := newConn(pipeReadWriter{Reader: serverR, Writer: serverW})

	serverErrCh := make(chan error, 1)
	var selected Module
	go func() {
		var err error
		selected, err = ServeGreeting(server, testConfig())
		serverErrCh <- err
	}()

	if _, err := DialGreeting(client, "public"); err != nil {
		t.Fatalf("DialGreeting returned error: %v", err)
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("ServeGreeting returned error: %v", err)
	}
	if selected.Name != "public" {
		t.Errorf("selected module = %q, want %q", selected.Name, "public")
	}
}

func TestGreeting_UnknownModuleErrors(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	client := newConn(pipeReadWriter{Reader: clientR, Writer: clientW})
	server := newConn(pipeReadWriter{Reader: serverR, Writer: serverW})

	serverErrCh := make(chan error, 1)
	go func() {
		_, err := ServeGreeting(server, testConfig())
		serverErrCh <- err
	}()

	if _, err := DialGreeting(client, "does-not-exist"); err != nil {
		t.Fatalf("DialGreeting returned error: %v", err)
	}
	// DialGreeting itself doesn't interpret the server's post-selection
	// response (that's the auth phase's job, in a later step) - so the
	// test reads it directly here, both to confirm the server actually
	// sent an @ERROR line and to drain the pipe so ServeGreeting's write
	// doesn't block forever waiting for a reader that will never come.
	response, err := readLine(client.r)
	if err != nil {
		t.Fatalf("reading server response: %v", err)
	}
	if !strings.HasPrefix(response, "@ERROR") {
		t.Errorf("server response = %q, want an @ERROR line", response)
	}
	if err := <-serverErrCh; err == nil {
		t.Fatalf("ServeGreeting with an unknown module returned nil error, want an error")
	}
}

func TestGreeting_ModuleListingHidesListFalseModules(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	client := newConn(pipeReadWriter{Reader: clientR, Writer: clientW})
	server := newConn(pipeReadWriter{Reader: serverR, Writer: serverW})

	serverErrCh := make(chan error, 1)
	go func() {
		_, err := ServeGreeting(server, testConfig())
		serverErrCh <- err
	}()

	listing, err := DialGreeting(client, "") // "" requests #list, matching a bare rsync://host URL
	if err != nil {
		t.Fatalf("DialGreeting returned error: %v", err)
	}
	if err := <-serverErrCh; !errors.Is(err, ErrModuleListRequested) {
		t.Fatalf("ServeGreeting error = %v, want ErrModuleListRequested", err)
	}

	joined := ""
	for _, l := range listing {
		joined += l + "\n"
	}
	if !strings.Contains(joined, "public\tpublic files") {
		t.Errorf("listing = %q, want it to contain the public module", joined)
	}
	if !strings.Contains(joined, "private") {
		t.Errorf("listing = %q, want it to contain the private module", joined)
	}
	if strings.Contains(joined, "hidden") {
		t.Errorf("listing = %q, want it to NOT contain the list=false hidden module", joined)
	}
}

func TestGreeting_HiddenModuleStillReachableByName(t *testing.T) {
	// "list = false" hides a module from listing, but real rsyncd.conf
	// does not make it unreachable to a client who already knows its
	// name - confirming that distinction is implemented correctly, not
	// just "list = false" accidentally behaving like a full block.
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	client := newConn(pipeReadWriter{Reader: clientR, Writer: clientW})
	server := newConn(pipeReadWriter{Reader: serverR, Writer: serverW})

	serverErrCh := make(chan error, 1)
	var selected Module
	go func() {
		var err error
		selected, err = ServeGreeting(server, testConfig())
		serverErrCh <- err
	}()

	if _, err := DialGreeting(client, "hidden"); err != nil {
		t.Fatalf("DialGreeting returned error: %v", err)
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("ServeGreeting returned error: %v", err)
	}
	if selected.Name != "hidden" {
		t.Errorf("selected module = %q, want %q", selected.Name, "hidden")
	}
}
