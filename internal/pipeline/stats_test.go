package pipeline

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

func TestSpeedupRatio_MatchesRealRsyncFormula(t *testing.T) {
	s := Stats{TotalFileSize: 1000, BytesSent: 100, BytesReceived: 150}
	got := s.SpeedupRatio()
	want := 1000.0 / (100.0 + 150.0)
	if got != want {
		t.Errorf("SpeedupRatio() = %v, want %v", got, want)
	}
}

func TestSpeedupRatio_ZeroBytesIsZeroNotNaN(t *testing.T) {
	s := Stats{TotalFileSize: 1000, BytesSent: 0, BytesReceived: 0}
	if got := s.SpeedupRatio(); got != 0 {
		t.Errorf("SpeedupRatio() with zero bytes sent/received = %v, want 0 (not NaN/Inf)", got)
	}
}

func TestCommaInt(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{5, "5"},
		{999, "999"},
		{1000, "1,000"},
		{1238099, "1,238,099"},
		{-1234, "-1,234"},
	}
	for _, tt := range tests {
		if got := commaInt(tt.in); got != tt.want {
			t.Errorf("commaInt(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCommaFloat2(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0.00"},
		{1.5, "1.50"},
		{146.384, "146.38"},
		{1238.005, "1,238.01"},
	}
	for _, tt := range tests {
		if got := commaFloat2(tt.in); got != tt.want {
			t.Errorf("commaFloat2(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// statsField extracts the integer following "label: " in output (before
// any trailing " bytes" or "(...)" breakdown), e.g. statsField(out,
// "Total file size") on a line "Total file size: 42 bytes" returns 42.
func statsField(t *testing.T, output, label string) int64 {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(label) + `: ([\d,]+)`)
	m := re.FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("field %q not found in stats output:\n%s", label, output)
	}
	n, err := strconv.ParseInt(regexp.MustCompile(`,`).ReplaceAllString(m[1], ""), 10, 64)
	if err != nil {
		t.Fatalf("parsing field %q value %q: %v", label, m[1], err)
	}
	return n
}

func TestReceiver_StatsAccuracy(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	const newContent = "0123456789" // 10 bytes, brand new
	// 2 full blocks (sync.DefaultBlockSize is 700): GenerateDelta can only
	// produce a CopyOp for a window at least one full block long, so a
	// shorter "identical" file would always transfer as literal data.
	unchangedContent := strings.Repeat("ABCDEFGHIJ", 140) // 1400 bytes, byte-identical at both ends
	const nestedContent = "nested"                        // 6 bytes, brand new, inside a brand-new directory

	mustWriteFile(t, filepath.Join(srcRoot, "new.txt"), newContent)
	mustWriteFile(t, filepath.Join(srcRoot, "unchanged.txt"), unchangedContent)
	mustWriteFile(t, filepath.Join(destRoot, "unchanged.txt"), unchangedContent) // already present, identical
	mustMkdirAll(t, filepath.Join(srcRoot, "sub"))
	mustWriteFile(t, filepath.Join(srcRoot, "sub", "nested.txt"), nestedContent)

	var out bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Stats: true, Output: &out})

	output := out.String()
	t.Logf("stats output:\n%s", output)

	// 3 regular files (new.txt, unchanged.txt, nested.txt) + 1 directory (sub) = 4.
	if got := statsField(t, output, "Number of files"); got != 4 {
		t.Errorf("Number of files = %d, want 4", got)
	}
	// new.txt, nested.txt, and sub are new; unchanged.txt already existed.
	if got := statsField(t, output, "Number of created files"); got != 3 {
		t.Errorf("Number of created files = %d, want 3", got)
	}
	// new.txt and nested.txt actually changed; unchanged.txt did not.
	if got := statsField(t, output, "Number of regular files transferred"); got != 2 {
		t.Errorf("Number of regular files transferred = %d, want 2", got)
	}

	wantTotalSize := int64(len(newContent) + len(unchangedContent) + len(nestedContent))
	if got := statsField(t, output, "Total file size"); got != wantTotalSize {
		t.Errorf("Total file size = %d, want %d", got, wantTotalSize)
	}

	wantTransferredSize := int64(len(newContent) + len(nestedContent))
	if got := statsField(t, output, "Total transferred file size"); got != wantTransferredSize {
		t.Errorf("Total transferred file size = %d, want %d", got, wantTransferredSize)
	}

	wantLiteral := int64(len(newContent) + len(nestedContent))
	if got := statsField(t, output, "Literal data"); got != wantLiteral {
		t.Errorf("Literal data = %d, want %d", got, wantLiteral)
	}
	wantMatched := int64(len(unchangedContent))
	if got := statsField(t, output, "Matched data"); got != wantMatched {
		t.Errorf("Matched data = %d, want %d", got, wantMatched)
	}

	sent := statsField(t, output, "Total bytes sent")
	received := statsField(t, output, "Total bytes received")
	if sent <= 0 || received <= 0 {
		t.Errorf("Total bytes sent/received = %d/%d, want both > 0", sent, received)
	}

	// Self-consistent with the formula and the sent/received counts
	// already verified above, rather than an independently hard-coded
	// expectation.
	wantSpeedup := commaFloat2(float64(wantTotalSize) / float64(sent+received))
	if !bytes.Contains(out.Bytes(), []byte("speedup is "+wantSpeedup)) {
		t.Errorf("output does not contain expected speedup %q:\n%s", wantSpeedup, output)
	}
}

func TestReceiver_StatsOmitsDeletedFilesLine(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(srcRoot, "f.txt"), "content")

	var out bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Stats: true, Output: &out})

	if bytes.Contains(out.Bytes(), []byte("deleted files")) {
		t.Errorf("output mentions deleted files despite --delete not being implemented:\n%s", out.String())
	}
}

func TestReceiver_StatsCountsNewEmptyFileAsTransferred(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(srcRoot, "empty.txt"), "")

	var out bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Stats: true, Output: &out})

	if got := statsField(t, out.String(), "Number of regular files transferred"); got != 1 {
		t.Errorf("Number of regular files transferred (new empty file) = %d, want 1:\n%s", got, out.String())
	}
	if got := statsField(t, out.String(), "Number of created files"); got != 1 {
		t.Errorf("Number of created files (new empty file) = %d, want 1:\n%s", got, out.String())
	}
}

func TestReceiver_StatsWorksInDryRun(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(srcRoot, "f.txt"), "content")

	var out bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{DryRun: true, Stats: true, Output: &out})

	if got := statsField(t, out.String(), "Number of regular files transferred"); got != 1 {
		t.Errorf("Number of regular files transferred (dry-run) = %d, want 1", got)
	}
	if !bytes.Contains(out.Bytes(), []byte("(DRY RUN)")) {
		t.Errorf("dry-run stats output missing the \"(DRY RUN)\" suffix:\n%s", out.String())
	}
}
