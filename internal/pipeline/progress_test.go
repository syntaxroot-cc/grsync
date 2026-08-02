package pipeline

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// blockingWriter never returns from Write until the test closes unblock.
type blockingWriter struct {
	unblock chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.unblock
	return len(p), nil
}

func TestProgressReporter_ReportDoesNotBlockOnSlowConsumer(t *testing.T) {
	w := &blockingWriter{unblock: make(chan struct{})}
	pr := newProgressReporter(w)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			pr.report(progressUpdate{path: "f", bytesDone: int64(i), fileSize: 1000})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("report() blocked despite a full channel and a stuck consumer - the non-blocking send is broken")
	}

	close(w.unblock) // let the stuck Write return, so stop() below can complete
	pr.stop()
}

func TestProgressReporter_StopDoesNotLeakTheGoroutine(t *testing.T) {
	var buf bytes.Buffer
	pr := newProgressReporter(&buf)
	pr.report(progressUpdate{path: "f", bytesDone: 1, fileSize: 1, done: true})

	stopped := make(chan struct{})
	go func() {
		pr.stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return - the formatting goroutine leaked")
	}
}

func TestProgressReporter_StopWithNoUpdatesSent(t *testing.T) {
	var buf bytes.Buffer
	pr := newProgressReporter(&buf)

	stopped := make(chan struct{})
	go func() {
		pr.stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return for a reporter that never received any updates")
	}
}

func TestReceiver_ProgressFiresMultipleTimesForLargeFile(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()

	content := strings.Repeat("x", progressWriteChunkSize*3+1000) // 3+ chunks' worth
	mustWriteFile(t, filepath.Join(srcRoot, "big.bin"), content)

	var out bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{Progress: true, Output: &out})

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var nonEmpty int
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}
	if nonEmpty < 2 {
		t.Errorf("got %d progress line(s), want at least 2 (one intermediate, one completion) for a %d-byte file:\n%s",
			nonEmpty, len(content), out.String())
	}
	if !strings.Contains(out.String(), "100%") {
		t.Errorf("output does not contain a 100%% completion line:\n%s", out.String())
	}

	assertSameContent(t, filepath.Join(srcRoot, "big.bin"), filepath.Join(destRoot, "big.bin"))
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{4 * time.Second, "0:00:04"},
		{8 * time.Second, "0:00:08"},
		{0, "0:00:00"},
		{90 * time.Minute, "1:30:00"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.in); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Checks against real rsync's man page examples: "782448  63%
// 110.64kB/s    0:00:04" and "1,238,099 100%  146.38kB/s    0:00:08
// (xfr#5, to-chk=169/396)". Expected values below use elapsed=4s/8s
// instead of the man page's own, so rate/ETA are computed fresh rather
// than copied.
func TestFormatProgressLine_MatchesRealRsyncExamples(t *testing.T) {
	inProgress := formatProgressLine(
		progressUpdate{bytesDone: 782448, fileSize: 1238099},
		4*time.Second,
	)
	if want := "782448  63%  195.61kB/s    0:00:02\n"; inProgress != want {
		t.Errorf("in-progress line = %q, want %q", inProgress, want)
	}

	completion := formatProgressLine(
		progressUpdate{bytesDone: 1238099, fileSize: 1238099, done: true, xferNum: 5, filesLeft: 169, totalFiles: 396},
		8*time.Second,
	)
	want := "1,238,099 100%  154.76kB/s    0:00:08  (xfr#5, to-chk=169/396)\n"
	if completion != want {
		t.Errorf("completion line = %q, want %q", completion, want)
	}
}

// TestFormatRate matches real rsync's own kB/MB/GB-per-second scaling.
func TestFormatRate(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0.00kB"},
		{110640, "110.64kB"},
		{2_500_000, "2.50MB"},
		{3_200_000_000, "3.20GB"},
	}
	for _, tt := range tests {
		if got := formatRate(tt.in); got != tt.want {
			t.Errorf("formatRate(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReceiver_ProgressDoesNotFireDuringDryRun(t *testing.T) {
	srcRoot := t.TempDir()
	destRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(srcRoot, "f.txt"), strings.Repeat("x", progressWriteChunkSize*2))

	var out bytes.Buffer
	runSenderReceiverWithOptions(t, srcRoot, destRoot,
		sync.WalkOptions{Recursive: true}, nil, sync.AttrOptions{},
		ReceiverOptions{DryRun: true, Progress: true, Output: &out})

	if out.Len() != 0 {
		t.Errorf("progress output during a dry run = %q, want empty", out.String())
	}
}
