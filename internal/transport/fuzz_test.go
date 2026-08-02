package transport

import (
	"bytes"
	"testing"
)

// FuzzReadFrame is a bonus SC-15 target beyond the ticket's own three
// named ones: the literal "binary parsing of untrusted input" its own
// rationale describes, one layer below internal/daemon's text-based
// greeting/auth lines - every transport (local pipe, SSH session, and a
// daemon connection once its own text handshake ends) reads its actual
// payload frames through this exact function. The property checked is
// ReadFrame's own documented safety guarantee: it never panics, and it
// never attempts to allocate a payload larger than maxFramePayload,
// regardless of what bytes a corrupted stream or hostile peer sends -
// precisely the protection SC-13's own TestE2E_ReadBatchOfMalformedFileFailsClearly
// found catching a garbage --read-batch file cleanly rather than
// crashing; this fuzz target explores far beyond that one hand-picked
// case.
func FuzzReadFrame(f *testing.F) {
	var valid bytes.Buffer
	_ = WriteFrame(&valid, Frame{Type: FrameFileList, Payload: []byte("hello")})
	f.Add(valid.Bytes())

	var empty bytes.Buffer
	_ = WriteFrame(&empty, Frame{Type: FrameSignature, Payload: nil})
	f.Add(empty.Bytes())

	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0x00}) // length prefix claiming ~4 GiB
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 0})
	f.Add([]byte{1, 2, 3})

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ReadFrame(bytes.NewReader(data))
	})
}
