package sync

import (
	"fmt"
	"math/rand"
	"testing"
)

// generateBenchData returns (oldData, newData), both n bytes long: oldData
// is pseudo-random from a fixed seed, and newData changes roughly
// changePercent% of its bytes at scattered random positions.
// changePercent >= 100 instead generates a fully independent newData.
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

// BenchmarkGenerateDelta measures block-matching throughput across file size
// and change percentage, since both can affect how it scales.
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

// BenchmarkGenerateSignature measures signature generation, which depends
// only on file size, not on similarity between old and new data.
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
