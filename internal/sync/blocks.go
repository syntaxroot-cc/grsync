package sync

// DefaultBlockSize is the fixed block size used to split files for the
// delta-transfer algorithm. Real rsync computes this dynamically per file
// (roughly proportional to the square root of file size); using one fixed
// size here is a deliberate simplification, not an algorithmic requirement.
const DefaultBlockSize = 700

// splitBlocks splits data into fixed-size blocks of blockSize bytes each.
// The final block is shorter than blockSize when len(data) isn't an exact
// multiple of it. Returned slices share data's backing array.
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
