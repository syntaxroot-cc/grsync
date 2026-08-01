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
	// Verified against real rsync's own source (main.c's output_summary):
	// total_size / (total_written + total_read).
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

// TestReceiver_StatsAccuracy is SC-10's core accuracy proof: a small
// tree where every number is exactly predictable by construction (not
// approximated) - two brand-new regular files (entirely literal data,
// no old data to match against), one byte-identical existing file
// (entirely matched data, zero literal), and one new directory - checked
// against the actual printed --stats output, not the internal Stats
// struct directly, so this proves the real end-user-visible behavior,
// not just the accumulation logic in isolation.
func TestReceiver_StatsAccuracy(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	const newContent = "0123456789" // 10 bytes, brand new
	// unchangedContent is deliberately 2 full blocks (sync.DefaultBlockSize
	// is 700): GenerateDelta can only ever produce a CopyOp for a window
	// at least one full block long (see delta.go's own "fewer than
	// blockSize bytes remain" comment), so a short "identical" file - one
	// shorter than a single block - would always transfer as 100% literal
	// data regardless of whether it actually matches, proving nothing
	// about Matched data specifically.
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

	// new.txt and nested.txt are entirely literal (no old data existed to
	// match against); unchanged.txt is entirely matched (byte-identical),
	// contributing 0 literal and its full size to matched.
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

	// The speedup line is self-consistent with the formula and the
	// actual sent/received counts already verified above - not an
	// independently hard-coded expectation, which would just duplicate
	// (and risk silently drifting from) the real byte counts gob
	// encoding happens to produce.
	wantSpeedup := commaFloat2(float64(wantTotalSize) / float64(sent+received))
	if !bytes.Contains(out.Bytes(), []byte("speedup is "+wantSpeedup)) {
		t.Errorf("output does not contain expected speedup %q:\n%s", wantSpeedup, output)
	}
}

// TestReceiver_StatsOmitsDeletedFilesLine confirms real rsync's own
// documented rule is followed: "Number of deleted files" is only
// printed when deletions are in effect - grsync never implements
// --delete at all (see the README), so this line must never appear.
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

// TestReceiver_StatsCountsNewEmptyFileAsTransferred is a deliberate edge
// case: a brand-new *empty* file has oldData == nil and newData == nil
// (ApplyDelta's accumulator is never appended to when there are no
// delta ops at all), so a naive bytes.Equal(oldData, newData) check
// alone would say "unchanged" even though a real file was just created -
// this is exactly the gap receiveRegularFile's transferred variable
// (distinct from contentChanged) exists to close.
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

// TestReceiver_StatsWorksInDryRun confirms Stats is fully dry-run
// compatible, unlike Progress: every field it reports (file counts,
// sizes, literal/matched data, even bytes sent/received) is derived
// from planning data and the wire exchange, neither of which dry-run
// skips - only the final disk write is skipped, and Stats doesn't
// depend on that.
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
