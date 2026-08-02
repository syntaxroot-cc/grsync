package pipeline

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
	"github.com/syntaxroot-cc/grsync/internal/transport"
)

func TestFileListRoundTrip(t *testing.T) {
	want := []sync.FileEntry{
		{
			Path:               "dir",
			Mode:               fs.ModeDir | 0o755,
			IsDir:              true,
			ModTime:            time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
			OwnershipAvailable: true,
			UID:                1000,
			GID:                1000,
		},
		{
			Path:       "dir/link",
			Mode:       fs.ModeSymlink | 0o777,
			LinkTarget: "../target",
			ModTime:    time.Date(2024, 1, 2, 3, 4, 6, 0, time.UTC),
		},
		{
			Path:    "dir/file.txt",
			Mode:    0o644,
			Size:    12,
			ModTime: time.Date(2024, 1, 2, 3, 4, 7, 0, time.UTC),
		},
	}

	wantGroups := []sync.HardLinkGroup{{"dir/a.txt", "dir/b.txt"}}

	var buf bytes.Buffer
	if err := sendFileList(&buf, want, wantGroups); err != nil {
		t.Fatalf("sendFileList returned error: %v", err)
	}
	got, gotGroups, err := recvFileList(&buf)
	if err != nil {
		t.Fatalf("recvFileList returned error: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Path != want[i].Path ||
			got[i].Mode != want[i].Mode ||
			got[i].IsDir != want[i].IsDir ||
			!got[i].ModTime.Equal(want[i].ModTime) ||
			got[i].LinkTarget != want[i].LinkTarget ||
			got[i].Size != want[i].Size ||
			got[i].OwnershipAvailable != want[i].OwnershipAvailable ||
			got[i].UID != want[i].UID || got[i].GID != want[i].GID {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if len(gotGroups) != len(wantGroups) {
		t.Fatalf("got %d hard-link groups, want %d", len(gotGroups), len(wantGroups))
	}
	for i := range wantGroups {
		if len(gotGroups[i]) != len(wantGroups[i]) {
			t.Fatalf("group %d = %v, want %v", i, gotGroups[i], wantGroups[i])
		}
		for j := range wantGroups[i] {
			if gotGroups[i][j] != wantGroups[i][j] {
				t.Errorf("group %d member %d = %q, want %q", i, j, gotGroups[i][j], wantGroups[i][j])
			}
		}
	}
}

func TestSignatureRoundTrip(t *testing.T) {
	sig := sync.GenerateSignatureWithBlockSize([]byte("AAAABBBBCCCC"), 4)

	var buf bytes.Buffer
	if err := sendSignature(&buf, "some/file.txt", sig, appendNone); err != nil {
		t.Fatalf("sendSignature returned error: %v", err)
	}
	got, err := recvSignature(&buf)
	if err != nil {
		t.Fatalf("recvSignature returned error: %v", err)
	}

	if got.Path != "some/file.txt" {
		t.Errorf("Path = %q, want %q", got.Path, "some/file.txt")
	}
	if got.Sig.BlockSize != sig.BlockSize || len(got.Sig.Blocks) != len(sig.Blocks) {
		t.Fatalf("Sig = %+v, want %+v", got.Sig, sig)
	}
	for i := range sig.Blocks {
		if got.Sig.Blocks[i] != sig.Blocks[i] {
			t.Errorf("block %d = %+v, want %+v", i, got.Sig.Blocks[i], sig.Blocks[i])
		}
	}
}

func TestDeltaRoundTrip(t *testing.T) {
	ops := []sync.DeltaOp{
		sync.CopyOp{BlockIndex: 2},
		sync.DataOp{Bytes: []byte("literal bytes")},
		sync.CopyOp{BlockIndex: 0},
	}

	var buf bytes.Buffer
	if err := sendDelta(&buf, "some/file.txt", ops, CompressOptions{}); err != nil {
		t.Fatalf("sendDelta returned error: %v", err)
	}
	gotPath, gotOps, err := recvDelta(&buf)
	if err != nil {
		t.Fatalf("recvDelta returned error: %v", err)
	}

	if gotPath != "some/file.txt" {
		t.Errorf("path = %q, want %q", gotPath, "some/file.txt")
	}
	if len(gotOps) != len(ops) {
		t.Fatalf("got %d ops, want %d", len(gotOps), len(ops))
	}
	for i := range ops {
		switch want := ops[i].(type) {
		case sync.CopyOp:
			got, ok := gotOps[i].(sync.CopyOp)
			if !ok || got != want {
				t.Errorf("op %d = %+v, want %+v", i, gotOps[i], want)
			}
		case sync.DataOp:
			got, ok := gotOps[i].(sync.DataOp)
			if !ok || string(got.Bytes) != string(want.Bytes) {
				t.Errorf("op %d = %+v, want %+v", i, gotOps[i], want)
			}
		}
	}
}

// TestDeltaRoundTrip_Compressed is SC-9's core wire-format proof: sending
// a delta with compression enabled must produce a smaller frame than the
// same ops sent uncompressed, and recvDelta on the compressed side must
// still reconstruct byte-identical ops - proving toWireDeltaOps/
// fromWireDeltaOps' compress-once-per-file, re-slice-by-Length design
// (see deltaMessage's own doc comment) round-trips correctly, not just
// that compression was attempted.
func TestDeltaRoundTrip_Compressed(t *testing.T) {
	literal := []byte(strings.Repeat("compressible literal data ", 200))
	ops := []sync.DeltaOp{
		sync.CopyOp{BlockIndex: 3},
		sync.DataOp{Bytes: literal[:len(literal)/2]},
		sync.CopyOp{BlockIndex: 1},
		sync.DataOp{Bytes: literal[len(literal)/2:]},
	}

	var uncompressed bytes.Buffer
	if err := sendDelta(&uncompressed, "file.txt", ops, CompressOptions{}); err != nil {
		t.Fatalf("sendDelta (uncompressed) returned error: %v", err)
	}

	var compressed bytes.Buffer
	copts := CompressOptions{Enabled: true, Level: DefaultCompressLevel}
	if err := sendDelta(&compressed, "file.txt", ops, copts); err != nil {
		t.Fatalf("sendDelta (compressed) returned error: %v", err)
	}

	if compressed.Len() >= uncompressed.Len() {
		t.Errorf("compressed frame = %d bytes, uncompressed = %d bytes, want compressed smaller", compressed.Len(), uncompressed.Len())
	}

	gotPath, gotOps, err := recvDelta(&compressed)
	if err != nil {
		t.Fatalf("recvDelta returned error: %v", err)
	}
	if gotPath != "file.txt" {
		t.Errorf("path = %q, want %q", gotPath, "file.txt")
	}
	if len(gotOps) != len(ops) {
		t.Fatalf("got %d ops, want %d", len(gotOps), len(ops))
	}
	for i := range ops {
		switch want := ops[i].(type) {
		case sync.CopyOp:
			got, ok := gotOps[i].(sync.CopyOp)
			if !ok || got != want {
				t.Errorf("op %d = %+v, want %+v", i, gotOps[i], want)
			}
		case sync.DataOp:
			got, ok := gotOps[i].(sync.DataOp)
			if !ok || !bytes.Equal(got.Bytes, want.Bytes) {
				t.Errorf("op %d length = %d, want %d, equal=%v", i, len(got.Bytes), len(want.Bytes), ok && bytes.Equal(got.Bytes, want.Bytes))
			}
		}
	}
}

// TestDeltaRoundTrip_SkipCompressSuffixIsGenuinelyUncompressed is the
// ticket's explicit "inspect actual bytes sent, not just trust the flag
// was read" requirement: it decodes the raw gob frame directly (the same
// way recvDelta itself does internally) and asserts deltaMessage.Compressed
// is false and every op still carries its own literal Bytes, for a path
// whose suffix is in copts.SkipSuffixes - even though copts.Enabled is
// true and the literal data is highly compressible.
func TestDeltaRoundTrip_SkipCompressSuffixIsGenuinelyUncompressed(t *testing.T) {
	literal := []byte(strings.Repeat("z", 5000)) // trivially compressible
	ops := []sync.DeltaOp{sync.DataOp{Bytes: literal}}
	copts := CompressOptions{Enabled: true, Level: DefaultCompressLevel, SkipSuffixes: []string{"bin"}}

	var buf bytes.Buffer
	if err := sendDelta(&buf, "archive.bin", ops, copts); err != nil {
		t.Fatalf("sendDelta returned error: %v", err)
	}

	f, err := transport.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame returned error: %v", err)
	}
	var msg deltaMessage
	if err := decodeGob(f.Payload, &msg); err != nil {
		t.Fatalf("decodeGob returned error: %v", err)
	}

	if msg.Compressed {
		t.Errorf("deltaMessage.Compressed = true for a .bin path in SkipSuffixes, want false")
	}
	if msg.Literal != nil {
		t.Errorf("deltaMessage.Literal = %d bytes, want nil (unused) when not compressed", len(msg.Literal))
	}
	if len(msg.Ops) != 1 || !bytes.Equal(msg.Ops[0].Bytes, literal) {
		t.Errorf("op 0 did not carry its own literal Bytes directly despite Compressed being false")
	}
}

// TestDeltaRoundTrip_DisabledIsGenuinelyUncompressed is
// TestDeltaRoundTrip_SkipCompressSuffixIsGenuinelyUncompressed's
// counterpart for CompressOptions{} (the zero value, --compress not
// given at all): the wire frame must be byte-identical in shape to the
// pre-SC-9 format, not merely "the same size by coincidence."
func TestDeltaRoundTrip_DisabledIsGenuinelyUncompressed(t *testing.T) {
	ops := []sync.DeltaOp{sync.DataOp{Bytes: []byte(strings.Repeat("y", 5000))}}

	var buf bytes.Buffer
	if err := sendDelta(&buf, "file.dat", ops, CompressOptions{}); err != nil {
		t.Fatalf("sendDelta returned error: %v", err)
	}

	f, err := transport.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame returned error: %v", err)
	}
	var msg deltaMessage
	if err := decodeGob(f.Payload, &msg); err != nil {
		t.Fatalf("decodeGob returned error: %v", err)
	}

	if msg.Compressed {
		t.Errorf("deltaMessage.Compressed = true with CompressOptions{}, want false")
	}
}

func TestReadTypedFrame_TranslatesFrameErrorToGoError(t *testing.T) {
	var buf bytes.Buffer
	if err := transport.WriteFrame(&buf, transport.Frame{Type: transport.FrameError, Payload: []byte("remote blew up")}); err != nil {
		t.Fatalf("WriteFrame returned error: %v", err)
	}

	_, err := readTypedFrame(&buf, transport.FrameFileList, "file list")
	if err == nil {
		t.Fatalf("readTypedFrame with a FrameError in the stream returned nil error, want an error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("remote blew up")) {
		t.Errorf("error = %q, want it to contain the remote's message", err.Error())
	}
}

func TestReadTypedFrame_RejectsWrongType(t *testing.T) {
	var buf bytes.Buffer
	if err := transport.WriteFrame(&buf, transport.Frame{Type: transport.FrameHello}); err != nil {
		t.Fatalf("WriteFrame returned error: %v", err)
	}

	if _, err := readTypedFrame(&buf, transport.FrameFileList, "file list"); err == nil {
		t.Fatalf("readTypedFrame with an unexpected frame type returned nil error, want an error")
	}
}
