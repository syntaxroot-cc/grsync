package pipeline

import (
	"bytes"
	"io/fs"
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
	if err := sendSignature(&buf, "some/file.txt", sig); err != nil {
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
	if err := sendDelta(&buf, "some/file.txt", ops); err != nil {
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
