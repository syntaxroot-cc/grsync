package sync

import (
	"bytes"
	"testing"
)

// FuzzWeakChecksumRoll is SC-15's fuzz target for "the rolling/strong
// checksum code (SC-3)". The property it checks is the exact one
// TestWeakChecksum_RollMatchesFromScratch already proves for one hand-
// picked string and window size: roll() must always produce the same
// result as recomputing the checksum from scratch on the shifted
// window, for ANY data and ANY valid window size - not just the cases a
// human thought to write down. If this property ever broke, the delta
// algorithm would silently miss real block matches (or produce false
// ones only caught by the strong checksum, masking the bug), so this is
// worth exploring far beyond one fixed fixture.
func FuzzWeakChecksumRoll(f *testing.F) {
	f.Add([]byte("the quick brown fox jumps over the lazy dog"), 8)
	f.Add([]byte("aaaaaaaaaaaaaaaa"), 1)
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0}, 4)
	f.Add([]byte{}, 1)

	f.Fuzz(func(t *testing.T, data []byte, windowSize int) {
		if windowSize <= 0 || windowSize > len(data) {
			t.Skip() // not a valid window for this data; nothing to check
		}

		current := newWeakChecksum(data[:windowSize])
		for offset := 1; offset+windowSize <= len(data); offset++ {
			out := data[offset-1]
			in := data[offset+windowSize-1]
			current = current.roll(out, in)

			want := newWeakChecksum(data[offset : offset+windowSize])
			if current.sum() != want.sum() {
				t.Fatalf("offset %d, window %d: rolled sum = %d, want %d (from scratch)",
					offset, windowSize, current.sum(), want.sum())
			}
		}
	})
}

// FuzzRoundTripDelta is a bonus target beyond the ticket's own three
// named ones, extending TestRoundTrip's hand-picked (old, new) pairs to
// arbitrary fuzzer-generated ones. The property checked is the whole
// delta algorithm's own reason for existing: ApplyDelta(oldData,
// GenerateDelta(sig, newData), sig) must equal newData exactly, for any
// pair of byte slices, not just the identical/prepend/append/middle-edit
// cases TestRoundTrip already covers by hand.
//
// blockSize is fixed at 4 (much smaller than DefaultBlockSize's 700)
// specifically so small fuzzer-generated inputs still produce multiple
// blocks and exercise real CopyOp matching, not just a single
// all-literal DataOp - a large default block size would make most
// fuzz-generated inputs too short to ever test the matching path at
// all.
func FuzzRoundTripDelta(f *testing.F) {
	f.Add([]byte("hello world"), []byte("hello there world"))
	f.Add([]byte(""), []byte("brand new file"))
	f.Add([]byte("identical content"), []byte("identical content"))
	f.Add([]byte("this file shrinks a lot"), []byte("short"))

	f.Fuzz(func(t *testing.T, oldData, newData []byte) {
		const maxFuzzInput = 1 << 16 // keep each iteration fast so many can run in a bounded CI time budget
		if len(oldData) > maxFuzzInput || len(newData) > maxFuzzInput {
			t.Skip()
		}

		sig := GenerateSignatureWithBlockSize(oldData, 4)
		ops := GenerateDelta(sig, newData)
		got, err := ApplyDelta(oldData, ops, sig)
		if err != nil {
			t.Fatalf("ApplyDelta returned error: %v", err)
		}
		if !bytes.Equal(got, newData) {
			t.Fatalf("round trip mismatch: got %d bytes, want %d bytes (oldData=%d bytes)", len(got), len(newData), len(oldData))
		}
	})
}

// FuzzCompileAndMatch is SC-15's fuzz target for "the filter pattern
// matcher (SC-7)". The property checked is simply that neither
// compilePattern nor Rule.matches ever panics for any pattern/path
// string, however malformed - path.Match (which matchSegments calls per
// segment) can itself return a syntax error for a bad glob, which
// matchSegments already treats as "no match" rather than propagating,
// but that defensive handling is exactly the kind of thing worth
// fuzzing to confirm holds for inputs nobody thought to hand-write,
// including empty strings, strings that are entirely "/" or "**", and
// patterns with unbalanced "[" character classes.
func FuzzCompileAndMatch(f *testing.F) {
	f.Add("*.txt", "dir/file.txt")
	f.Add("/anchored/**/pattern", "anchored/deep/nested/pattern")
	f.Add("**", "")
	f.Add("[unclosed", "path")
	f.Add("/", "/")
	f.Add("a//b", "a/b")

	f.Fuzz(func(_ *testing.T, pattern, path string) {
		rule, err := compilePattern(Include, pattern)
		if err != nil {
			return // a rejected pattern is a valid, expected outcome - nothing more to check
		}
		rule.matches(path, false)
		rule.matches(path, true)
	})
}
