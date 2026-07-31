package sync

import "crypto/md5"

// strongChecksum is the collision-resistant checksum used to confirm a
// weak-checksum match. MD5 is not cryptographically safe against a
// deliberate adversary, but that's not what it's used for here - it's
// only there to catch the rare case where two different blocks happen to
// share a weak checksum (see delta generation), which stdlib md5 is more
// than sufficient for.
func strongChecksum(block []byte) [md5.Size]byte {
	return md5.Sum(block)
}

// rollingChecksumModulus is 65536 (2^16) - a power of two, not a prime.
// Real Adler-32 uses the largest prime below 65536 (65521) instead; this
// is rsync's own simpler variant, chosen deliberately because a
// power-of-two modulus makes unsigned-integer wraparound during roll()
// mathematically safe (see the comment there) without extra bounds
// handling. It rolls in O(1), which is the only property that actually
// matters here - this is not meant to be byte-compatible with stdlib
// hash/adler32.
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

// roll advances the window by exactly one byte: out is the byte leaving
// at the window's start, in is the byte entering at its end. This is O(1)
// regardless of window size - the entire point of a rolling checksum,
// versus calling newWeakChecksum on the shifted window from scratch.
//
// The subtractions below can underflow as uint32 arithmetic (e.g. if
// w.a < out). That's fine, not a bug: Go's unsigned integers wrap modulo
// 2^32, and since rollingChecksumModulus (2^16) evenly divides 2^32, the
// wrapped value still reduces to the mathematically correct result mod
// 2^16 after the final "% rollingChecksumModulus". A prime modulus (real
// Adler-32's 65521) would not have this property.
func (w weakChecksum) roll(out, in byte) weakChecksum {
	a := (w.a - uint32(out) + uint32(in)) % rollingChecksumModulus
	b := (w.b - w.length*uint32(out) + a) % rollingChecksumModulus
	return weakChecksum{a: a, b: b, length: w.length}
}
