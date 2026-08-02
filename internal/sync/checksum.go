package sync

import "crypto/md5"

// strongChecksum confirms a weak-checksum match. MD5 isn't used for
// cryptographic security here, only to catch the rare case where two
// different blocks share a weak checksum.
func strongChecksum(block []byte) [md5.Size]byte {
	return md5.Sum(block)
}

// rollingChecksumModulus is 65536 (2^16), not a prime like real Adler-32's
// 65521. rsync's own simpler variant uses a power of two deliberately: it
// makes the unsigned-integer wraparound in roll() mathematically safe
// without extra bounds handling (see roll()'s comment).
const rollingChecksumModulus = 1 << 16

// weakChecksum is a rolling checksum over a fixed-size window: two 16-bit
// accumulators (a: sum of the window's bytes, b: a position-weighted sum)
// combined into one 32-bit value via sum().
type weakChecksum struct {
	a, b   uint32
	length uint32 // window size; constant across every roll() call
}

// newWeakChecksum computes the checksum for window from scratch in O(len(window)).
func newWeakChecksum(window []byte) weakChecksum {
	var a, b uint32
	n := uint32(len(window))
	for i, c := range window {
		a += uint32(c)
		b += (n - uint32(i)) * uint32(c)
	}
	return weakChecksum{
		a:      a % rollingChecksumModulus,
		b:      b % rollingChecksumModulus,
		length: n,
	}
}

// sum returns the combined checksum value.
func (w weakChecksum) sum() uint32 {
	return w.a + w.b*rollingChecksumModulus
}

// roll advances the window by one byte in O(1): out is the byte leaving at
// the window's start, in is the byte entering at its end.
//
// The subtractions below can underflow as uint32 arithmetic. That's fine:
// Go's unsigned integers wrap modulo 2^32, and since rollingChecksumModulus
// (2^16) evenly divides 2^32, the wrapped value still reduces to the
// mathematically correct result mod 2^16. A prime modulus would not have
// this property.
func (w weakChecksum) roll(out, in byte) weakChecksum {
	a := (w.a - uint32(out) + uint32(in)) % rollingChecksumModulus
	b := (w.b - w.length*uint32(out) + a) % rollingChecksumModulus
	return weakChecksum{a: a, b: b, length: w.length}
}
