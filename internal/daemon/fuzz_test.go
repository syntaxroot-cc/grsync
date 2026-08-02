package daemon

import (
	"bufio"
	"strings"
	"testing"
)

// FuzzReadGreeting checks that readGreeting never panics on malformed input.
func FuzzReadGreeting(f *testing.F) {
	f.Add("@RSYNCD: 31.0\n")
	f.Add("@RSYNCD: 30\n")
	f.Add("not a greeting at all\n")
	f.Add("@RSYNCD: \n")
	f.Add("@RSYNCD: abc.def\n")
	f.Add("@RSYNCD: 31.0.0.0.0\n")
	f.Add("\n")
	f.Add("")

	f.Fuzz(func(_ *testing.T, line string) {
		r := bufio.NewReader(strings.NewReader(line))
		_, _, _ = readGreeting(r)
	})
}

// FuzzReadLine checks that readLine never panics and never returns a line
// longer than maxLineLength.
func FuzzReadLine(f *testing.F) {
	f.Add("hello\n")
	f.Add("\n")
	f.Add("no trailing newline at all")
	f.Add("line with \r\n crlf ending")
	f.Add(strings.Repeat("x", maxLineLength*2) + "\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, data string) {
		r := bufio.NewReader(strings.NewReader(data))
		line, err := readLine(r)
		if err != nil {
			return // a rejected/incomplete line is a valid, expected outcome
		}
		if len(line) > maxLineLength {
			t.Fatalf("readLine returned a %d-byte line, want it bounded by maxLineLength (%d)", len(line), maxLineLength)
		}
	})
}
