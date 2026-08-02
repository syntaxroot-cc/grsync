package pipeline

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

func opsEqual(t *testing.T, got []sync.DeltaOp, want []sync.DeltaOp) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ops = %+v, want %+v", got, want)
	}
	for i := range want {
		switch w := want[i].(type) {
		case sync.CopyOp:
			g, ok := got[i].(sync.CopyOp)
			if !ok || g != w {
				t.Errorf("op %d = %+v, want %+v", i, got[i], w)
			}
		case sync.DataOp:
			g, ok := got[i].(sync.DataOp)
			if !ok || !bytes.Equal(g.Bytes, w.Bytes) {
				t.Errorf("op %d = %+v, want %+v", i, got[i], w)
			}
		}
	}
}

func TestAppendTailOps_TrustsPrefixAndSendsOnlyTail(t *testing.T) {
	ops, err := appendTailOps(5, []byte("helloWORLD"))
	if err != nil {
		t.Fatalf("appendTailOps returned error: %v", err)
	}
	opsEqual(t, ops, []sync.DeltaOp{sync.CopyOp{BlockIndex: 0}, sync.DataOp{Bytes: []byte("WORLD")}})
}

func TestAppendTailOps_ZeroTrustedLenOmitsCopyOp(t *testing.T) {
	ops, err := appendTailOps(0, []byte("all new"))
	if err != nil {
		t.Fatalf("appendTailOps returned error: %v", err)
	}
	opsEqual(t, ops, []sync.DeltaOp{sync.DataOp{Bytes: []byte("all new")}})
}

func TestAppendTailOps_ExactMatchOmitsDataOp(t *testing.T) {
	ops, err := appendTailOps(5, []byte("hello"))
	if err != nil {
		t.Fatalf("appendTailOps returned error: %v", err)
	}
	opsEqual(t, ops, []sync.DeltaOp{sync.CopyOp{BlockIndex: 0}})
}

func TestAppendTailOps_DiminishedFileReturnsError(t *testing.T) {
	if _, err := appendTailOps(100, []byte("short")); err == nil {
		t.Error("appendTailOps with a source shorter than the trusted length returned nil error, want an error")
	}
}

func TestReceiver_AppendTransfersOnlyNewTail(t *testing.T) {
	prefix := strings.Repeat("already on disk, must not be resent ", 100)
	tail := strings.Repeat("brand new tail data ", 100)
	full := prefix + tail

	appendSrc, appendDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(appendSrc, "growing.log"), full)
	mustWriteFile(t, filepath.Join(appendDest, "growing.log"), prefix)
	appendBytes := runSenderReceiverWithCompressOptions(t, appendSrc, appendDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Append: true}, CompressOptions{})

	assertSameContent(t, filepath.Join(appendSrc, "growing.log"), filepath.Join(appendDest, "growing.log"))

	// Baseline: the same transfer without append mode, mainly to confirm
	// append isn't somehow more expensive; the literal-data byte count
	// comparison below is append's real, direct proof.
	normalSrc, normalDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(normalSrc, "growing.log"), full)
	mustWriteFile(t, filepath.Join(normalDest, "growing.log"), prefix)
	normalBytes := runSenderReceiverWithCompressOptions(t, normalSrc, normalDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{}, CompressOptions{})

	if appendBytes >= normalBytes {
		t.Errorf("--append wrote %d bytes, normal delta-transfer wrote %d bytes, want --append no larger", appendBytes, normalBytes)
	}

	// "Literal data" must equal exactly len(tail): if the prefix had been
	// resent as literal data too, this would be much larger.
	var out bytes.Buffer
	statsSrc, statsDest := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(statsSrc, "growing.log"), full)
	mustWriteFile(t, filepath.Join(statsDest, "growing.log"), prefix)
	runSenderReceiverWithCompressAndReceiverOptions(t, statsSrc, statsDest,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Append: true, Stats: true, Output: &out}, CompressOptions{})
	if got := statsField(t, out.String(), "Literal data"); got != int64(len(tail)) {
		t.Errorf("Literal data = %d, want exactly %d (len(tail)) - the existing prefix must never be counted as literal", got, len(tail))
	}
}

func TestReceiver_AppendDoesNotVerifyCorruptedPrefix(t *testing.T) {
	srcRoot, destRoot := t.TempDir(), t.TempDir()
	realContent := "AAAAAAAAAA" + "tail data that gets appended"
	corruptedExisting := "XXXXXXXXXX" // same length as the real prefix, deliberately wrong content

	mustWriteFile(t, filepath.Join(srcRoot, "file.txt"), realContent)
	mustWriteFile(t, filepath.Join(destRoot, "file.txt"), corruptedExisting)

	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{Append: true})

	got, err := os.ReadFile(filepath.Join(destRoot, "file.txt"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	want := corruptedExisting + "tail data that gets appended"
	if string(got) != want {
		t.Errorf("result = %q, want %q (the corrupted prefix preserved verbatim, matching real rsync's own documented --append risk)", got, want)
	}
}

func TestReceiver_AppendVerifyDetectsCorruptedPrefix(t *testing.T) {
	srcRoot, destRoot := t.TempDir(), t.TempDir()
	realContent := strings.Repeat("A", 1400) + "tail data that gets appended, well past one block"
	corruptedExisting := strings.Repeat("X", 1400) // same length, deliberately wrong

	mustWriteFile(t, filepath.Join(srcRoot, "file.txt"), realContent)
	mustWriteFile(t, filepath.Join(destRoot, "file.txt"), corruptedExisting)

	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ReceiverOptions{AppendVerify: true})

	assertSameContent(t, filepath.Join(srcRoot, "file.txt"), filepath.Join(destRoot, "file.txt"))
}

func TestReceiver_AppendSkipsDestinationNotShorterThanSource(t *testing.T) {
	for _, ropts := range []ReceiverOptions{{Append: true}, {AppendVerify: true}} {
		srcRoot, destRoot := t.TempDir(), t.TempDir()
		mustWriteFile(t, filepath.Join(srcRoot, "file.txt"), "short")
		mustWriteFile(t, filepath.Join(destRoot, "file.txt"), "this destination is already longer than source")

		runSenderReceiverWithOptions(t, srcRoot, destRoot,
			sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ropts)

		got, err := os.ReadFile(filepath.Join(destRoot, "file.txt"))
		if err != nil {
			t.Fatalf("ropts=%+v: reading destination: %v", ropts, err)
		}
		if string(got) != "this destination is already longer than source" {
			t.Errorf("ropts=%+v: destination content = %q, want it left completely untouched", ropts, got)
		}
	}
}

func TestReceiver_AppendTransfersBrandNewFileNormally(t *testing.T) {
	for _, ropts := range []ReceiverOptions{{Append: true}, {AppendVerify: true}} {
		srcRoot, destRoot := t.TempDir(), t.TempDir()
		mustWriteFile(t, filepath.Join(srcRoot, "new.txt"), "brand new content")

		runSenderReceiverWithOptions(t, srcRoot, destRoot,
			sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{}, ropts)

		assertSameContent(t, filepath.Join(srcRoot, "new.txt"), filepath.Join(destRoot, "new.txt"))
	}
}

func TestReceiver_AppendWorksWithDryRun(t *testing.T) {
	srcRoot, destRoot := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(srcRoot, "growing.log"), "prefixTAIL")
	mustWriteFile(t, filepath.Join(destRoot, "growing.log"), "prefix")

	var out bytes.Buffer
	runSenderReceiverWithCompressAndReceiverOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Append: true, DryRun: true, Itemize: true, Output: &out}, CompressOptions{})

	if !strings.Contains(out.String(), "growing.log") {
		t.Errorf("dry-run itemize output = %q, want it to mention growing.log", out.String())
	}
	got, err := os.ReadFile(filepath.Join(destRoot, "growing.log"))
	if err != nil {
		t.Fatalf("reading destination: %v", err)
	}
	if string(got) != "prefix" {
		t.Errorf("destination content = %q after a dry run, want it unchanged (\"prefix\")", got)
	}

	entries, err := os.ReadDir(destRoot)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".grsync-tmp") {
			t.Errorf("a temp file %q was created during a dry run, want none at all", e.Name())
		}
	}
}

func TestReceiver_AppendExcludesHardLinkSecondaryMembers(t *testing.T) {
	srcRoot, destRoot := t.TempDir(), t.TempDir()

	content := "shared content for both hard-linked files"
	mustWriteFile(t, filepath.Join(srcRoot, "original.txt"), content)
	if err := os.Link(filepath.Join(srcRoot, "original.txt"), filepath.Join(srcRoot, "linked.txt")); err != nil {
		t.Skipf("hard link creation unsupported in this environment: %v", err)
	}

	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{HardLinks: true},
		ReceiverOptions{Append: true})

	assertSameContent(t, filepath.Join(srcRoot, "original.txt"), filepath.Join(destRoot, "original.txt"))
	assertSameContent(t, filepath.Join(srcRoot, "linked.txt"), filepath.Join(destRoot, "linked.txt"))

	if !sync.HardLinksSupported() {
		return
	}
	originalInfo, err := os.Stat(filepath.Join(destRoot, "original.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	linkedInfo, err := os.Stat(filepath.Join(destRoot, "linked.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !os.SameFile(originalInfo, linkedInfo) {
		t.Error("original.txt and linked.txt are independent files at the destination with --append enabled, want them still hard-linked")
	}
}
