package sync

// DefaultBlockSize is the fixed block size used to split files for the
// delta-transfer algorithm. Real rsync computes this dynamically per file
// (roughly proportional to the square root of the file size, within
// tunable bounds); using one fixed size here is a deliberate
// simplification for this ticket - dynamic block sizing is a future
// refinement, not something the algorithm itself depends on.
const DefaultBlockSize = 700

// splitBlocks splits data into fixed-size blocks of blockSize bytes each.
// The final block is shorter than blockSize whenever len(data) isn't an
// exact multiple of it; it's still included, never dropped or padded out
// to a full block. Returned slices share data's backing array rather than
// being copied.
func splitBlocks(data []byte, blockSize int) [][]byte {
	if blockSize <= 0 {
		return nil
	}
	var blocks [][]byte
	for start := 0; start < len(data); start += blockSize {
		end := start + blockSize
		if end > len(data) {
			end = len(data)
		}
		blocks = append(blocks, data[start:end])
	}
	return blocks
}
