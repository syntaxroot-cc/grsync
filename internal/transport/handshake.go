package transport

import (
	"fmt"
	"io"
)

// ProtocolVersion identifies the handshake protocol version. Bump it if the
// frame types or handshake sequence ever change incompatibly.
const ProtocolVersion = 1

// ServeHandshake implements the server side of the handshake: it reads one
// FrameHello from r, verifies its protocol version, and replies with one
// FrameHelloAck on w.
func ServeHandshake(r io.Reader, w io.Writer) error {
	f, err := ReadFrame(r)
	if err != nil {
		return fmt.Errorf("reading hello: %w", err)
	}
	if f.Type != FrameHello {
		return fmt.Errorf("expected FrameHello, got frame type %d", f.Type)
	}
	if len(f.Payload) != 1 || f.Payload[0] != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version (got payload %v, want [%d])", f.Payload, ProtocolVersion)
	}

	return WriteFrame(w, Frame{Type: FrameHelloAck, Payload: []byte{ProtocolVersion}})
}

// Handshake performs the client side of ServeHandshake: it sends FrameHello
// then reads back FrameHelloAck.
func Handshake(rw io.ReadWriter) error {
	if err := WriteFrame(rw, Frame{Type: FrameHello, Payload: []byte{ProtocolVersion}}); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}

	f, err := ReadFrame(rw)
	if err != nil {
		return fmt.Errorf("reading hello-ack: %w", err)
	}
	if f.Type != FrameHelloAck {
		return fmt.Errorf("expected FrameHelloAck, got frame type %d", f.Type)
	}
	if len(f.Payload) != 1 || f.Payload[0] != ProtocolVersion {
		return fmt.Errorf("server reported incompatible protocol version (got payload %v, want [%d])", f.Payload, ProtocolVersion)
	}

	return nil
}
