package sync

import (
	"fmt"
	"math/rand"
	"testing"
)

// generateBenchData returns (oldData, newData), both n bytes long: oldData
// is pseudo-random (a fixed seed, so every run of a given size/percent
// combination benchmarks the identical input, not fresh randomness each
// time), and newData is derived from it by changing roughly
// changePercent% of its bytes, scattered at random positions rather than
// as one contiguous run - closer to how a real file's edits tend to be
// spread through it than a single block of difference would be.
// changePercent >= 100 instead generates a completely independent
// newData, guaranteeing "genuinely nothing matches" rather than relying
// on random flips to (probabilistically, imperfectly) cover every byte.
func generateBenchData(n, changePercent int, seed int64) (oldData, newData []byte) {
	rng := rand.New(rand.NewSource(seed))
	oldData = make([]byte, n)
	_, _ = rng.Read(oldData)

	newData = make([]byte, n)
	if changePercent >= 100 {
		_, _ = rng.Read(newData)
		return oldData, newData
	}

	copy(newData, oldData)
	numChanged := n * changePercent / 100
	for i := 0; i < numChanged; i++ {
		pos := rng.Intn(n)
		newData[pos] = byte(rng.Intn(256))
	}
	return oldData, newData
}

// BenchmarkGenerateDelta measures block-matching throughput - almost
// certainly the most performance-sensitive code in this project, since
// it runs once per regular file on every sync, over the file's entire
// content. Varied across two independent dimensions, not just one
// arbitrary case: file size (does throughput hold up as files grow, or
// does something scale worse than linearly), and change percentage (a
// file that's mostly identical to its old version, real delta transfer's
// whole reason to exist, versus one that's mostly or entirely different,
// where nearly every window has to fall through to a literal DataOp
// instead of matching a block).
func BenchmarkGenerateDelta(b *testing.B) {
	sizes := []int{10 * 1024, 100 * 1024, 1024 * 1024}
	changePercents := []int{0, 10, 50, 100}

	for _, size := range sizes {
		for _, pct := range changePercents {
			name := fmt.Sprintf("size=%s/changed=%d%%", humanByteSize(size), pct)
			b.Run(name, func(b *testing.B) {
				oldData, newData := generateBenchData(size, pct, 42)
				sig := GenerateSignature(oldData)

				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					GenerateDelta(sig, newData)
				}
			})
		}
	}
}

// BenchmarkGenerateSignature measures the other half of a real sync's
// per-file cost on the receiving side: splitting the existing
// destination file into blocks and checksumming each one. Unlike
// GenerateDelta, signature generation doesn't depend on how similar the
// two files are - only on the existing file's own size - so this only
// varies that one dimension.
func BenchmarkGenerateSignature(b *testing.B) {
	sizes := []int{10 * 1024, 100 * 1024, 1024 * 1024}

	for _, size := range sizes {
		b.Run(humanByteSize(size), func(b *testing.B) {
			data := make([]byte, size)
			rng := rand.New(rand.NewSource(42))
			_, _ = rng.Read(data)

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				GenerateSignature(data)
			}
		})
	}
}

func humanByteSize(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%dMB", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
