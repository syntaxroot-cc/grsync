package transport

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FrameType tags what kind of message a Frame carries, so a single
// stdin/stdout byte stream can multiplex different message kinds (file
// list entries, signatures, delta ops, control messages) without a
// separate channel for each.
type FrameType byte

const (
	// FrameHello and FrameHelloAck are the minimal --server handshake:
	// the client sends FrameHello, the server replies with FrameHelloAck.
	FrameHello FrameType = iota
	// FrameHelloAck is the server's reply to FrameHello.
	FrameHelloAck
	// FrameError carries a human-readable error message from one side to
	// the other, rather than the connection just dying silently.
	FrameError
	// FrameFileList carries a gob-encoded []sync.FileEntry, sent once by
	// the sender after a successful handshake: the filtered list of
	// everything it intends to sync. internal/pipeline owns the encoding;
	// this package only tags and frames the bytes.
	FrameFileList
	// FrameSignature carries a gob-encoded per-file signature, sent by
	// the receiver for each regular-file entry in the list it received -
	// proactively, in list order, not in response to a separate request
	// message (the file list itself is the implicit request for all of
	// them).
	FrameSignature
	// FrameDelta carries a gob-encoded per-file delta (the sender's reply
	// to a FrameSignature), in the same list order.
	FrameDelta
)

// maxFramePayload bounds how large a single frame's payload may be. A
// length prefix isn't validated against anything else in this protocol
// (there's no higher-level "expected size" to check it against), so
// without a cap, a corrupted stream or a hostile peer could send a
// length like 0xFFFFFFFF and force ReadFrame to attempt a multi-gigabyte
// allocation before ever reading a single payload byte. 64 MiB is
// comfortably larger than any frame type this ticket defines needs.
const maxFramePayload = 64 * 1024 * 1024

// Frame is a single multiplexed protocol message.
type Frame struct {
	Type    FrameType
	Payload []byte
}

// WriteFrame writes f to w as: 4-byte big-endian length of Payload,
// 1-byte Type, then Payload itself.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > maxFramePayload {
		return fmt.Errorf("frame payload of %d bytes exceeds max %d", len(f.Payload), maxFramePayload)
	}

	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[:4], uint32(len(f.Payload)))
	header[4] = byte(f.Type)

	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("writing frame header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("writing frame payload: %w", err)
		}
	}
	return nil
}

// ReadFrame reads one Frame from r, written by WriteFrame.
func ReadFrame(r io.Reader) (Frame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return Frame{}, fmt.Errorf("reading frame header: %w", err)
	}

	length := binary.BigEndian.Uint32(header[:4])
	if length > maxFramePayload {
		return Frame{}, fmt.Errorf("frame payload of %d bytes exceeds max %d", length, maxFramePayload)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, fmt.Errorf("reading frame payload: %w", err)
		}
	}

	return Frame{Type: FrameType(header[4]), Payload: payload}, nil
}
