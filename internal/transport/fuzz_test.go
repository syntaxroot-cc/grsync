package transport

import (
	"bytes"
	"testing"
)

// FuzzReadFrame checks that ReadFrame never panics or over-allocates
// beyond maxFramePayload on a corrupted or hostile stream.
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
