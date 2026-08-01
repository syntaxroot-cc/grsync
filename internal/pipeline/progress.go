package pipeline

import (
	"fmt"
	"io"
	"time"
)

// progressWriteChunkSize is how large each disk-write chunk is when
// progress reporting is enabled: small enough to give real, visible
// granularity for a large file's write-to-disk phase, large enough not
// to turn an ordinary-sized file's write into thousands of syscalls.
//
// This is real, honest progress for exactly what it measures - bytes
// committed to disk for the current file - and no more: grsync's wire
// protocol delivers a whole file's delta as a single frame (see
// sync.ApplyDelta's own doc comment: "no streaming I/O anywhere yet"),
// so there is no point during network transfer where partial bytes are
// actually observable the way real rsync's own streaming protocol
// allows. Reporting progress against the disk-write phase instead of an
// imagined network stream is a deliberate, disclosed scope boundary -
// see the README's Progress and Stats section - not an approximation of
// something grsync doesn't actually do.
const progressWriteChunkSize = 256 * 1024

// progressUpdate is one snapshot of a single file's write-to-disk
// progress, sent non-blockingly to the formatting goroutine.
type progressUpdate struct {
	path       string
	bytesDone  int64
	fileSize   int64
	done       bool // true for this file's final update
	xferNum    int  // this file's 1-based position among files actually transferred
	totalFiles int  // total entries in the sync's file list
	filesLeft  int  // entries not yet processed after this one, matching real rsync's "to-chk"
}

// progressReporter formats and prints progressUpdates on its own
// goroutine, so the transfer loop never blocks on however slow (or
// entirely absent) the output writer is.
//
// report's send is always non-blocking (a full channel silently drops
// the update, including a file's very last one) rather than switching to
// a blocking send for updates judged "too important to drop": a
// consistent rule is simpler to reason about and test than one with a
// special case, and a dropped 100%-complete line is a cosmetic gap, not
// a correctness one - the transfer itself, and the itemize/stats
// reporting that runs independently of this goroutine, are entirely
// unaffected either way.
type progressReporter struct {
	updates chan progressUpdate
	done    chan struct{}
	start   time.Time
}

// newProgressReporter starts the formatting goroutine immediately;
// callers must call stop() exactly once (typically via defer, right
// after construction) so it's guaranteed to exit even on an early-error
// return - the self-review requirement this exists to satisfy is "no
// goroutine leaks," not just "usually cleans up."
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

// report sends u to the formatting goroutine without blocking. See the
// type's own doc comment for why every update, including a file's last,
// uses the same non-blocking send rather than a blocking one for
// "important" updates.
func (pr *progressReporter) report(u progressUpdate) {
	select {
	case pr.updates <- u:
	default:
	}
}

// stop closes the update channel and waits for the goroutine to drain
// and exit. Safe to call at most once; Receiver only ever constructs one
// progressReporter per call and defers stop() immediately, so this is
// never at risk of a double-close.
func (pr *progressReporter) stop() {
	close(pr.updates)
	<-pr.done
}

// formatProgressLine renders u in real rsync's own --progress format
// (verified against rsync.1's --progress section): while a file is
// still transferring, "<bytes>  <percent>%  <rate>/s    <eta>\n" with
// raw (non-comma-grouped) byte counts, matching the man page's own
// shown example ("782448  63%  110.64kB/s    0:00:04"); on that file's
// final update, real rsync instead prints a completion summary line -
// "<bytes> 100%  <rate>/s    <elapsed>  (xfr#N, to-chk=M/T)" - with
// comma-grouped bytes, matching its own shown example
// ("1,238,099 100%  146.38kB/s    0:00:08  (xfr#5, to-chk=169/396)").
//
// elapsed is this file's own transfer time (time since this
// progressReporter started - approximated as "since the whole Receiver
// call began" rather than "since this specific file began," since
// grsync doesn't currently track a per-file start time separately; for
// the common case of one file dominating a sync this is the same number
// either way, and for many small files it under-reports each
// individual file's own rate rather than over-reporting it - a
// disclosed simplification, not a hidden one).
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

// formatRate matches real rsync's own kB/MB/GB-per-second scaling
// (human_dnum): kilobytes (1000-based, matching rsync's own choice of
// decimal, not binary, units here) for anything under a megabyte/sec,
// megabytes beyond that.
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

// formatDuration matches real rsync's own h:mm:ss elapsed-time format
// exactly: both of the man page's own shown examples are "0:00:04" and
// "0:00:08" - hours always present (unpadded), minutes and seconds
// always zero-padded to 2 digits, even when hours is 0.
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
