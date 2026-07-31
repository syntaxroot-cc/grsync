package sync

import (
	"crypto/md5"
	"testing"
)

func TestStrongChecksum(t *testing.T) {
	block := []byte("some block contents")
	got := strongChecksum(block)
	want := md5.Sum(block)
	if got != want {
		t.Errorf("strongChecksum(%q) = %x, want %x", block, got, want)
	}

	if strongChecksum([]byte("a")) == strongChecksum([]byte("b")) {
		t.Errorf("different blocks produced the same strong checksum")
	}
}

// TestWeakChecksum_RollMatchesFromScratch is the load-bearing test for the
// entire rolling-checksum design: it proves roll() produces exactly the
// same result as recomputing from scratch, at every single offset across
// a test string, not just a couple of spot checks. If this property ever
// broke, the delta algorithm would silently miss real block matches (or
// worse, produce false ones that only got caught by the strong checksum,
// masking the bug) - so this needs direct proof, not just "it compiles".
func TestWeakChecksum_RollMatchesFromScratch(t *testing.T) {
	data := []byte("the quick brown fox jumps over the lazy dog, then jumps back again")
	const windowSize = 8

	if len(data) <= windowSize {
		t.Fatalf("test data too short: need more than %d bytes", windowSize)
	}

	current := newWeakChecksum(data[:windowSize])
	if want := newWeakChecksum(data[0:windowSize]); current.sum() != want.sum() {
		t.Fatalf("offset 0: sum = %d, want %d", current.sum(), want.sum())
	}

	for offset := 1; offset+windowSize <= len(data); offset++ {
		out := data[offset-1]
		in := data[offset+windowSize-1]
		current = current.roll(out, in)

		want := newWeakChecksum(data[offset : offset+windowSize])
		if current.sum() != want.sum() {
			t.Fatalf("offset %d: rolled sum = %d, want %d (from scratch) - a=%d/%d b=%d/%d",
				offset, current.sum(), want.sum(), current.a, want.a, current.b, want.b)
		}
	}
}

func TestWeakChecksum_IdenticalWindowsMatch(t *testing.T) {
	a := newWeakChecksum([]byte("abcdefgh"))
	b := newWeakChecksum([]byte("abcdefgh"))
	if a.sum() != b.sum() {
		t.Errorf("identical windows produced different sums: %d vs %d", a.sum(), b.sum())
	}
}

func TestWeakChecksum_DifferentWindowsUsuallyDiffer(t *testing.T) {
	a := newWeakChecksum([]byte("abcdefgh"))
	b := newWeakChecksum([]byte("hgfedcba"))
	if a.sum() == b.sum() {
		t.Errorf("reversed window produced the same sum (%d) as the original - weak checksum isn't discriminating position", a.sum())
	}
}
