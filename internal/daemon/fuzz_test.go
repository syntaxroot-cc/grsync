package daemon

import (
	"bufio"
	"strings"
	"testing"
)

// FuzzReadGreeting is SC-15's fuzz target for the daemon protocol's own
// greeting-line parsing (the "@RSYNCD: VERSION.SUB ..." line every
// connection starts with, before any authentication happens - genuinely
// untrusted network input, arriving before the peer has proven anything
// about itself). The property checked is that readGreeting never panics
// for any line content, however malformed - its own prefix check,
// strings.Fields split, and two strconv.Atoi calls all look
// defensively coded already, but fuzzing confirms that holds for inputs
// nobody thought to hand-write, not just the cases
// TestReadGreeting-style unit tests already cover.
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
		// readGreeting itself calls readLine, so a line with no trailing
		// "\n" is a valid, expected input here too (readLine returns an
		// error for it) - not appended manually, so this fuzzes the real
		// end-to-end parsing path exactly as a live connection would
		// present it.
		_, _, _ = readGreeting(r)
	})
}

// FuzzReadLine is SC-15's fuzz target for the single shared line-reading
// primitive every text-based phase of the daemon protocol (greeting,
// module selection, authentication) reads through - the one chokepoint
// genuinely untrusted network bytes always pass. The property checked is
// exactly what readLine's own doc comment promises: it never panics, and
// it never returns a line longer than maxLineLength, regardless of how
// much unterminated data a hostile or corrupted peer sends - the
// protection that keeps an attacker from forcing unbounded memory growth
// just by never sending a newline.
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
