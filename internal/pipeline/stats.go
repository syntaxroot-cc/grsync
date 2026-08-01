package pipeline

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// countingReadWriter wraps rw, counting every byte read and written -
// used to populate Stats.BytesSent/BytesReceived from the actual bytes
// crossing this connection (frame headers included, not just gob
// payloads), the same quantity real rsync's own "Total bytes sent"/
// "Total bytes received" measure, just over grsync's own gob wire
// protocol instead of rsync's real one (see messages.go's encoding
// note). Wrapping the connection itself, rather than threading a length
// return value through every send/recv helper in messages.go, means
// Sender and its own tests need no changes at all for this - only
// Receiver ever needs to know these counts.
type countingReadWriter struct {
	rw      io.ReadWriter
	read    int64
	written int64
}

func (c *countingReadWriter) Read(p []byte) (int, error) {
	n, err := c.rw.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingReadWriter) Write(p []byte) (int, error) {
	n, err := c.rw.Write(p)
	c.written += int64(n)
	return n, err
}

// Stats accumulates statistics over one Receiver call, mirroring real
// rsync's own --stats fields where grsync can compute them accurately.
// Fields real rsync reports that grsync deliberately omits - Number of
// deleted files (no --delete), File list size/generation-time/transfer-
// time (not measured anywhere in this architecture), and ACL/xattr/
// device/special counts (not tracked, the same scope boundary SC-11's
// itemize format already established) - are left out entirely rather
// than reported as a misleading zero; see the README's Progress and
// Stats section for the full list.
type Stats struct {
	RegularFiles int
	Directories  int
	Symlinks     int

	CreatedRegularFiles int
	CreatedDirectories  int
	CreatedSymlinks     int

	// RegularFilesTransferred is how many regular files actually had
	// different content and were rewritten - the same contentChanged
	// bit itemizeFile already computes for its Y code, reused here
	// rather than recomputed.
	RegularFilesTransferred int

	// TotalFileSize is the sum of Size across every regular-file entry
	// considered (matching real rsync's own "does not count directories
	// or special files, but does include symlinks" - except grsync
	// scopes this to regular files only, since symlink "size" isn't a
	// meaningful transferred-bytes quantity in this pipeline).
	TotalFileSize            int64
	TotalTransferredFileSize int64

	// LiteralData and MatchedData are computed by inspecting the
	// DataOp/CopyOp list each regular file's delta already produces -
	// no new information needs to cross the wire for this.
	LiteralData int64
	MatchedData int64

	BytesSent     int64
	BytesReceived int64

	Elapsed time.Duration
}

// NumFiles is real rsync's own "Number of files": every entry
// considered, regardless of type or whether it changed.
func (s Stats) NumFiles() int { return s.RegularFiles + s.Directories + s.Symlinks }

// NumCreatedFiles is real rsync's own "Number of created files": every
// entry that didn't already exist at the destination.
func (s Stats) NumCreatedFiles() int {
	return s.CreatedRegularFiles + s.CreatedDirectories + s.CreatedSymlinks
}

// SpeedupRatio is real rsync's own formula, verified against its actual
// source (main.c's output_summary, not just the man page's looser
// prose): total file size divided by the sum of bytes sent and
// received. Real rsync's own comment calls this "really just a
// feel-good bigger-is-better number," not a rigorous metric -
// reproduced here exactly as-is, caveat included, rather than a
// differently-reasoned formula that happens to also produce a ratio.
func (s Stats) SpeedupRatio() float64 {
	total := s.BytesSent + s.BytesReceived
	if total == 0 {
		return 0
	}
	return float64(s.TotalFileSize) / float64(total)
}

// BytesPerSecond is total bytes transferred divided by elapsed wall-clock
// time, matching real rsync's own bytes_per_sec_human_dnum(). Zero
// elapsed time (a sync fast enough that no measurable time passed, or a
// synthetic test driving Receiver directly without going through the
// real clock) reports 0 rather than dividing by zero.
func (s Stats) BytesPerSecond() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.BytesSent+s.BytesReceived) / s.Elapsed.Seconds()
}

// commaInt formats n with comma thousands separators, matching real
// rsync's own comma_num()/human_num() output for --stats byte counts.
// (--progress's own live line, in contrast, shows raw ungrouped digits -
// see formatProgressLine - matching the different function real rsync
// itself uses there.)
func commaInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// commaFloat2 formats f with comma thousands separators and exactly 2
// decimal places, matching real rsync's own comma_dnum(f, 2) - used for
// the speedup ratio and the bytes/sec rate in --stats output.
func commaFloat2(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	whole := int64(f)
	frac := int64((f-float64(whole))*100 + 0.5)
	if frac >= 100 {
		whole++
		frac -= 100
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%s.%02d", sign, commaInt(whole), frac)
}

// typeBreakdown is real rsync's own "(reg: R, dir: D, link: L)" suffix,
// omitting any type whose count is zero entirely - matching the man
// page's own documented rule ("If any value is 0, it is completely
// omitted from the list"), which also naturally hides grsync's lack of
// device/special support rather than needing a separate carve-out for it.
func typeBreakdown(reg, dir, link int) string {
	var parts []string
	if reg > 0 {
		parts = append(parts, fmt.Sprintf("reg: %d", reg))
	}
	if dir > 0 {
		parts = append(parts, fmt.Sprintf("dir: %d", dir))
	}
	if link > 0 {
		parts = append(parts, fmt.Sprintf("link: %d", link))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// formatStats renders s in real rsync's own --stats structure (verified
// against main.c's output_summary): a detailed field-by-field block,
// then the shorter sent/received/rate and total-size/speedup summary
// every --stats run ends with regardless of verbosity. dryRun appends
// real rsync's own "(DRY RUN)" suffix to the speedup line when set,
// reusing a real, already-verified rsync convention rather than
// inventing a different way to flag the same thing.
func formatStats(s Stats, dryRun bool) string {
	var b strings.Builder

	b.WriteString("\n")
	fmt.Fprintf(&b, "Number of files: %s%s\n", commaInt(int64(s.NumFiles())),
		typeBreakdown(s.RegularFiles, s.Directories, s.Symlinks))
	if s.NumCreatedFiles() > 0 {
		fmt.Fprintf(&b, "Number of created files: %s%s\n", commaInt(int64(s.NumCreatedFiles())),
			typeBreakdown(s.CreatedRegularFiles, s.CreatedDirectories, s.CreatedSymlinks))
	}
	fmt.Fprintf(&b, "Number of regular files transferred: %s\n", commaInt(int64(s.RegularFilesTransferred)))
	fmt.Fprintf(&b, "Total file size: %s bytes\n", commaInt(s.TotalFileSize))
	fmt.Fprintf(&b, "Total transferred file size: %s bytes\n", commaInt(s.TotalTransferredFileSize))
	fmt.Fprintf(&b, "Literal data: %s bytes\n", commaInt(s.LiteralData))
	fmt.Fprintf(&b, "Matched data: %s bytes\n", commaInt(s.MatchedData))
	fmt.Fprintf(&b, "Total bytes sent: %s\n", commaInt(s.BytesSent))
	fmt.Fprintf(&b, "Total bytes received: %s\n", commaInt(s.BytesReceived))
	b.WriteString("\n")

	fmt.Fprintf(&b, "sent %s bytes  received %s bytes  %s bytes/sec\n",
		commaInt(s.BytesSent), commaInt(s.BytesReceived), commaFloat2(s.BytesPerSecond()))

	suffix := ""
	if dryRun {
		suffix = " (DRY RUN)"
	}
	fmt.Fprintf(&b, "total size is %s  speedup is %s%s\n", commaInt(s.TotalFileSize), commaFloat2(s.SpeedupRatio()), suffix)

	return b.String()
}
