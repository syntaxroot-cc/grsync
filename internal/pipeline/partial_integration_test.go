package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

func TestReceiver_InterruptedMultiFileSyncKeepsCompletedFilesRegardlessOfPartial(t *testing.T) {
	for _, ropts := range []ReceiverOptions{{}, {Partial: true}, {PartialDir: ".rsync-partial"}} {
		t.Run(fmt.Sprintf("%+v", ropts), func(t *testing.T) {
			destRoot := t.TempDir()

			peerReadsFromReceiver, receiverWritesToPeer := io.Pipe()
			receiverReadsFromPeer, peerWritesToReceiver := io.Pipe()
			receiver := pipeReadWriter{Reader: receiverReadsFromPeer, Writer: receiverWritesToPeer}

			entries := []sync.FileEntry{
				{Path: "a.txt", Mode: 0o644, Size: 5},
				{Path: "b.txt", Mode: 0o644, Size: 5},
				{Path: "c.txt", Mode: 0o644, Size: 5}, // the connection dies before this one's delta ever arrives
			}

			peerErrCh := make(chan error, 1)
			go func() {
				if err := sendFileList(peerWritesToReceiver, entries, nil); err != nil {
					peerErrCh <- fmt.Errorf("sending file list: %w", err)
					return
				}

				for _, name := range []string{"a.txt", "b.txt"} {
					sigMsg, err := recvSignature(peerReadsFromReceiver)
					if err != nil {
						peerErrCh <- fmt.Errorf("receiving signature for %s: %w", name, err)
						return
					}
					if sigMsg.Path != name {
						peerErrCh <- fmt.Errorf("signature requested for %q, want %q", sigMsg.Path, name)
						return
					}
					ops := []sync.DeltaOp{sync.DataOp{Bytes: []byte("hello")}}
					if err := sendDelta(peerWritesToReceiver, name, ops, CompressOptions{}); err != nil {
						peerErrCh <- fmt.Errorf("sending delta for %s: %w", name, err)
						return
					}
				}

				// Receive the signature request for c.txt, then vanish
				// without ever responding.
				if _, err := recvSignature(peerReadsFromReceiver); err != nil {
					peerErrCh <- fmt.Errorf("receiving signature for c.txt: %w", err)
					return
				}
				_ = peerWritesToReceiver.Close()
				_ = peerReadsFromReceiver.Close()
				peerErrCh <- nil
			}()

			err := Receiver(receiver, destRoot, sync.AttrOptions{}, ropts)
			if err == nil {
				t.Fatal("Receiver returned nil error after the connection dropped mid-transfer, want an error")
			}

			for _, name := range []string{"a.txt", "b.txt"} {
				got, rerr := os.ReadFile(filepath.Join(destRoot, name))
				if rerr != nil {
					t.Errorf("%s: reading result: %v", name, rerr)
					continue
				}
				if string(got) != "hello" {
					t.Errorf("%s content = %q, want %q (files completed before the drop must survive it intact)", name, got, "hello")
				}
			}

			// c.txt must be completely absent, not zero-length or truncated,
			// regardless of ropts.
			if _, statErr := os.Stat(filepath.Join(destRoot, "c.txt")); !os.IsNotExist(statErr) {
				t.Errorf("c.txt exists after the connection dropped before its own transfer began, want it completely absent")
			}
			if ropts.PartialDir != "" {
				if _, statErr := os.Stat(filepath.Join(destRoot, ropts.PartialDir, "c.txt")); !os.IsNotExist(statErr) {
					t.Errorf("c.txt exists in PartialDir despite never having anything written for it, want it absent there too")
				}
			}

			// No stray temp file anywhere in destRoot either.
			_ = filepath.WalkDir(destRoot, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.Contains(d.Name(), ".grsync-tmp") {
					t.Errorf("stray temp file %q left behind after the interruption", path)
				}
				return nil
			})

			if peerErr := <-peerErrCh; peerErr != nil {
				t.Fatalf("peer goroutine: %v", peerErr)
			}
		})
	}
}

func TestSenderReceiver_PartialDirUsedAsResumeBasisOnRetry(t *testing.T) {
	fullContent := strings.Repeat("resumable content chunk, well over one block size ", 60)
	partialDir := ".rsync-partial"

	resumedSrc, resumedDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(resumedSrc, "big.txt"), fullContent)
	// A genuine prefix, as if an earlier run had gotten this far before
	// being interrupted.
	prefix := fullContent[:len(fullContent)*3/4]
	partialPath := filepath.Join(resumedDest, partialDir, "big.txt")
	mustMkdirAll(t, filepath.Dir(partialPath))
	mustWriteFile(t, partialPath, prefix)

	resumedBytes := runSenderReceiverWithCompressOptions(t, resumedSrc, resumedDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{PartialDir: partialDir}, CompressOptions{})

	assertSameContent(t, filepath.Join(resumedSrc, "big.txt"), filepath.Join(resumedDest, "big.txt"))
	if _, statErr := os.Stat(partialPath); !os.IsNotExist(statErr) {
		t.Errorf("partial-dir file %q still exists after a successful resumed transfer, want it removed", partialPath)
	}

	// Baseline: a completely fresh destination, no resume basis.
	freshSrc, freshDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(freshSrc, "big.txt"), fullContent)
	freshBytes := runSenderReceiverWithCompressOptions(t, freshSrc, freshDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{}, CompressOptions{})

	if resumedBytes >= freshBytes {
		t.Errorf("resumed transfer (with a partial-dir basis) wrote %d bytes, fresh transfer (no basis) wrote %d bytes, want the resumed one meaningfully smaller", resumedBytes, freshBytes)
	}
}

func TestReceiver_PartialDirBasisIgnoresRealDestinationContent(t *testing.T) {
	srcRoot, destRoot := t.TempDir(), t.TempDir()
	fullContent := strings.Repeat("A", 1400) + "the new tail"
	correctPrefix := strings.Repeat("A", 1400)
	wrongExistingDest := strings.Repeat("Z", 1400) // deliberately wrong, must not be used as the basis

	mustWriteFile(t, filepath.Join(srcRoot, "file.txt"), fullContent)
	mustWriteFile(t, filepath.Join(destRoot, "file.txt"), wrongExistingDest)
	partialDir := ".rsync-partial"
	partialPath := filepath.Join(destRoot, partialDir, "file.txt")
	mustMkdirAll(t, filepath.Dir(partialPath))
	mustWriteFile(t, partialPath, correctPrefix)

	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{PartialDir: partialDir})

	assertSameContent(t, filepath.Join(srcRoot, "file.txt"), filepath.Join(destRoot, "file.txt"))
}

func TestReceiver_PartialDirCreatesNoFilesAndDeletesNothingDuringDryRun(t *testing.T) {
	srcRoot, destRoot := t.TempDir(), t.TempDir()
	fullContent := strings.Repeat("resumable content ", 100)
	mustWriteFile(t, filepath.Join(srcRoot, "big.txt"), fullContent)

	partialDir := ".rsync-partial"
	prefix := fullContent[:len(fullContent)/2]
	partialPath := filepath.Join(destRoot, partialDir, "big.txt")
	mustMkdirAll(t, filepath.Dir(partialPath))
	mustWriteFile(t, partialPath, prefix)

	var out bytes.Buffer
	runSenderReceiverWithCompressAndReceiverOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{PartialDir: partialDir, DryRun: true, Itemize: true, Output: &out},
		CompressOptions{})

	if !strings.Contains(out.String(), "big.txt") {
		t.Errorf("dry-run itemize output = %q, want it to mention big.txt", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(destRoot, "big.txt")); !os.IsNotExist(statErr) {
		t.Errorf("big.txt was created at the destination during a dry run, want nothing written at all")
	}
	got, err := os.ReadFile(partialPath)
	if err != nil {
		t.Fatalf("the leftover partial-dir file was removed/modified during a dry run, want it left exactly as it was: %v", err)
	}
	if string(got) != prefix {
		t.Errorf("partial-dir file content = %q, want it unchanged (%q)", got, prefix)
	}
}
