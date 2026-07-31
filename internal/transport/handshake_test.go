package transport

import (
	"io"
	"testing"
)

// pipeReadWriter joins two io.Pipe halves into a single io.ReadWriter, so
// Handshake (which needs one bidirectional stream, like a real Session)
// can be driven against an in-memory pipe instead of a real subprocess.
type pipeReadWriter struct {
	io.Reader
	io.Writer
}

func TestHandshake_PureLogic(t *testing.T) {
	// Two independent pipes, one per direction, wired crosswise so each
	// side's writes become the other side's reads - simulating a real
	// bidirectional Session without spawning a subprocess at all.
	clientReadsFromServer, serverWritesToClient := io.Pipe()
	serverReadsFromClient, clientWritesToServer := io.Pipe()

	client := pipeReadWriter{Reader: clientReadsFromServer, Writer: clientWritesToServer}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- ServeHandshake(serverReadsFromClient, serverWritesToClient)
	}()

	if err := Handshake(client); err != nil {
		t.Fatalf("Handshake returned error: %v", err)
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("ServeHandshake returned error: %v", err)
	}
}

func TestServeHandshake_RejectsWrongFrameType(t *testing.T) {
	r, w := io.Pipe()
	go func() {
		_ = WriteFrame(w, Frame{Type: FrameError, Payload: []byte{ProtocolVersion}})
		_ = w.Close()
	}()

	if err := ServeHandshake(r, io.Discard); err == nil {
		t.Fatalf("ServeHandshake with a non-Hello frame returned nil error, want an error")
	}
}

func TestServeHandshake_RejectsWrongVersion(t *testing.T) {
	r, w := io.Pipe()
	go func() {
		_ = WriteFrame(w, Frame{Type: FrameHello, Payload: []byte{ProtocolVersion + 1}})
		_ = w.Close()
	}()

	if err := ServeHandshake(r, io.Discard); err == nil {
		t.Fatalf("ServeHandshake with a mismatched protocol version returned nil error, want an error")
	}
}

func TestHandshake_RejectsWrongFrameType(t *testing.T) {
	r, w := io.Pipe()
	go func() {
		_ = WriteFrame(w, Frame{Type: FrameError, Payload: []byte{ProtocolVersion}})
		_ = w.Close()
	}()

	// Handshake's own outgoing FrameHello is simply discarded here - this
	// test only cares what happens when the response it reads back isn't
	// a FrameHelloAck.
	client := pipeReadWriter{Reader: r, Writer: io.Discard}
	if err := Handshake(client); err == nil {
		t.Fatalf("Handshake with a non-HelloAck frame returned nil error, want an error")
	}
}
