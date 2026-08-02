package transport

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestWriteReadFrame_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Frame{Type: FrameHello, Payload: []byte("hello world")}

	if err := WriteFrame(&buf, want); err != nil {
		t.Fatalf("WriteFrame returned error: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame returned error: %v", err)
	}
	if got.Type != want.Type || string(got.Payload) != string(want.Payload) {
		t.Errorf("ReadFrame = %+v, want %+v", got, want)
	}
}

func TestWriteReadFrame_EmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	want := Frame{Type: FrameHelloAck, Payload: nil}

	if err := WriteFrame(&buf, want); err != nil {
		t.Fatalf("WriteFrame returned error: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame returned error: %v", err)
	}
	if got.Type != want.Type || len(got.Payload) != 0 {
		t.Errorf("ReadFrame = %+v, want Type=%v with empty payload", got, want.Type)
	}
}

func TestWriteReadFrame_MultipleFramesInSequence(t *testing.T) {
	var buf bytes.Buffer
	frames := []Frame{
		{Type: FrameHello, Payload: []byte("first")},
		{Type: FrameError, Payload: []byte("second, a bit longer")},
		{Type: FrameHelloAck, Payload: nil},
	}
	for _, f := range frames {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("WriteFrame returned error: %v", err)
		}
	}

	for i, want := range frames {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame #%d returned error: %v", i, err)
		}
		if got.Type != want.Type || string(got.Payload) != string(want.Payload) {
			t.Errorf("frame #%d = %+v, want %+v", i, got, want)
		}
	}

	if buf.Len() != 0 {
		t.Errorf("%d bytes left unread after consuming all frames", buf.Len())
	}
}

func TestReadFrame_TruncatedHeaderErrors(t *testing.T) {
	buf := bytes.NewBuffer([]byte{0x00, 0x00}) // only 2 of 5 header bytes
	if _, err := ReadFrame(buf); err == nil {
		t.Fatalf("ReadFrame with a truncated header returned nil error, want an error")
	}
}

func TestReadFrame_TruncatedPayloadErrors(t *testing.T) {
	var buf bytes.Buffer
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[:4], 100) // claims 100 payload bytes
	header[4] = byte(FrameHello)
	buf.Write(header)
	buf.WriteString("only a few bytes") // far fewer than the claimed 100

	if _, err := ReadFrame(&buf); err == nil {
		t.Fatalf("ReadFrame with a truncated payload returned nil error, want an error")
	}
}

func TestReadFrame_OversizedLengthRejected(t *testing.T) {
	var buf bytes.Buffer
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[:4], maxFramePayload+1)
	header[4] = byte(FrameHello)
	buf.Write(header)

	if _, err := ReadFrame(&buf); err == nil {
		t.Fatalf("ReadFrame with an over-max length prefix returned nil error, want an error")
	}
}

func TestWriteFrame_OversizedPayloadRejected(t *testing.T) {
	f := Frame{Type: FrameHello, Payload: make([]byte, maxFramePayload+1)}
	if err := WriteFrame(io.Discard, f); err == nil {
		t.Fatalf("WriteFrame with an over-max payload returned nil error, want an error")
	}
}
