package pipeline

import (
	"fmt"
	"io"
	"time"
)

// progressWriteChunkSize is the disk-write chunk size used for progress
// reporting. Progress tracks disk-write bytes, not network bytes, since a
// whole file's delta arrives as a single frame with no partial point.
const progressWriteChunkSize = 256 * 1024

// progressUpdate is one snapshot of a single file's write-to-disk progress.
type progressUpdate struct {
	path       string
	bytesDone  int64
	fileSize   int64
	done       bool // true for this file's final update
	xferNum    int  // this file's 1-based position among files actually transferred
	totalFiles int  // total entries in the sync's file list
	filesLeft  int  // entries not yet processed after this one, matching real rsync's "to-chk"
}

// progressReporter formats and prints progressUpdates on its own goroutine,
// so the transfer loop never blocks on the output writer.
type progressReporter struct {
	updates chan progressUpdate
	done    chan struct{}
	start   time.Time
}

// newProgressReporter starts the formatting goroutine; callers must call
// stop() exactly once (typically via defer) to avoid leaking it.
func newProgressReporter(output io.Writer) *progressReporter {
	pr := &progressReporter{
		updates: make(chan progressUpdate, 8),
		done:    make(chan struct{}),
		start:   time.Now(),
	}
	go pr.run(output)
	return pr
}

func (pr *progressReporter) run(output io.Writer) {
	defer close(pr.done)
	for u := range pr.updates {
		_, _ = fmt.Fprint(output, formatProgressLine(u, time.Since(pr.start)))
	}
}

// report sends u to the formatting goroutine, dropping it instead of
// blocking the transfer if the channel is full.
func (pr *progressReporter) report(u progressUpdate) {
	select {
	case pr.updates <- u:
	default:
	}
}

// stop closes the update channel and waits for the goroutine to drain and
// exit. Must be called at most once.
func (pr *progressReporter) stop() {
	close(pr.updates)
	<-pr.done
}

// formatProgressLine renders u in real rsync's --progress format: a live
// "<bytes>  <percent>%  <rate>/s    <eta>" line while transferring, then a
// comma-grouped "<bytes> 100%  <rate>/s    <elapsed>  (xfr#N, to-chk=M/T)"
// summary on the file's final update.
//
// elapsed approximates per-file time as time since the whole Receiver call
// began, since grsync doesn't track each file's own start time.
func formatProgressLine(u progressUpdate, elapsed time.Duration) string {
	percent := 0
	if u.fileSize > 0 {
		percent = int(float64(u.bytesDone) * 100 / float64(u.fileSize))
	} else if u.done {
		percent = 100
	}

	rate := 0.0
	if elapsed > 0 {
		rate = float64(u.bytesDone) / elapsed.Seconds()
	}

	if !u.done {
		eta := "0:00:00"
		if rate > 0 && u.fileSize > u.bytesDone {
			remaining := time.Duration(float64(u.fileSize-u.bytesDone)/rate) * time.Second
			eta = formatDuration(remaining)
		}
		return fmt.Sprintf("%d  %d%%  %s/s    %s\n", u.bytesDone, percent, formatRate(rate), eta)
	}

	return fmt.Sprintf("%s 100%%  %s/s    %s  (xfr#%d, to-chk=%d/%d)\n",
		commaInt(u.bytesDone), formatRate(rate), formatDuration(elapsed), u.xferNum, u.filesLeft, u.totalFiles)
}

// formatRate matches real rsync's kB/MB/GB-per-second scaling: decimal
// (1000-based) units, kB below 1MB/s and MB/GB beyond that.
func formatRate(bytesPerSec float64) string {
	switch {
	case bytesPerSec >= 1e9:
		return fmt.Sprintf("%.2fGB", bytesPerSec/1e9)
	case bytesPerSec >= 1e6:
		return fmt.Sprintf("%.2fMB", bytesPerSec/1e6)
	default:
		return fmt.Sprintf("%.2fkB", bytesPerSec/1e3)
	}
}

// formatDuration matches real rsync's h:mm:ss format: hours unpadded,
// minutes and seconds always zero-padded to 2 digits.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
}
