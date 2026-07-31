package transport

import (
	"fmt"
	"io"
)

// ProtocolVersion identifies this ticket's minimal handshake protocol.
// Bump it if the frame types or handshake sequence ever change
// incompatibly, so an old client/server pair fails the version check
// below instead of misinterpreting each other's frames.
const ProtocolVersion = 1

// ServeHandshake implements grsync's --server-mode entry point for this
// ticket's scope: read one FrameHello from r, verify its protocol
// version, and reply with one FrameHelloAck on w.
//
// This proves the subprocess/pipe/framing machinery works end to end
// through a real remote-shell connection - it is deliberately not a full
// sync server. Wiring an actual file-list/signature/delta exchange on top
// of this frame/session foundation is later, separately-scoped work (see
// README's note on this).
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

// Handshake performs the client side of ServeHandshake over rw: sends
// FrameHello, then reads back FrameHelloAck, confirming the round trip
// actually completed rather than just assuming the connection is good
// because Dial succeeded.
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
