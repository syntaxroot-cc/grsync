package pipeline

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// countingReadWriter wraps rw, counting every byte read and written, to
// populate Stats.BytesSent/BytesReceived from what actually crosses this
// connection (frame headers included, not just gob payloads).
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
// Fields grsync can't measure (deleted files, ACL/xattr/device counts,
// etc.) are omitted entirely rather than reported as a misleading zero.
type Stats struct {
	RegularFiles int
	Directories  int
	Symlinks     int

	CreatedRegularFiles int
	CreatedDirectories  int
	CreatedSymlinks     int

	// RegularFilesTransferred is how many regular files actually had
	// different content and were rewritten.
	RegularFilesTransferred int

	// TotalFileSize is the sum of Size across every regular-file entry
	// considered; unlike real rsync, this excludes symlinks, since
	// symlink "size" isn't a meaningful transferred-bytes quantity here.
	TotalFileSize            int64
	TotalTransferredFileSize int64

	// LiteralData and MatchedData come from each regular file's delta
	// DataOp/CopyOp list.
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

// SpeedupRatio is real rsync's own formula: total file size divided by
// the sum of bytes sent and received.
func (s Stats) SpeedupRatio() float64 {
	total := s.BytesSent + s.BytesReceived
	if total == 0 {
		return 0
	}
	return float64(s.TotalFileSize) / float64(total)
}

// BytesPerSecond is total bytes transferred divided by elapsed wall-clock
// time. Zero elapsed time reports 0 rather than dividing by zero.
func (s Stats) BytesPerSecond() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.BytesSent+s.BytesReceived) / s.Elapsed.Seconds()
}

// commaInt formats n with comma thousands separators, matching real
// rsync's --stats byte counts (--progress's live line shows raw ungrouped
// digits instead - see formatProgressLine).
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
// decimal places, matching real rsync's comma_dnum(f, 2).
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

// typeBreakdown is real rsync's "(reg: R, dir: D, link: L)" suffix,
// omitting any type whose count is zero.
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

// formatStats renders s in real rsync's --stats structure: a detailed
// field-by-field block, then the sent/received/rate and
// total-size/speedup summary. dryRun appends real rsync's own
// "(DRY RUN)" suffix to the speedup line.
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
