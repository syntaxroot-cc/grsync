package sync

import "crypto/md5"

// BlockSignature is the pair of checksums computed for one block, sent
// from receiver to sender so the sender can find matching blocks in its
// own copy of the file without transferring the block's bytes.
type BlockSignature struct {
	Weak   uint32
	Strong [md5.Size]byte
}

// Signature is an ordered list of per-block checksums for a file, plus
// the block size used to produce them. BlockSize travels with the
// checksums (rather than being a separate parameter callers must keep in
// sync) because delta generation and reconstruction both need to know
// exactly how the blocks were cut - a mismatched block size would make
// every checksum meaningless. A block's position in Blocks is its index,
// which is how a CopyOp (see delta.go) refers back to it.
type Signature struct {
	BlockSize int
	Blocks    []BlockSignature
}

// GenerateSignature splits data into DefaultBlockSize blocks and computes
// both checksums for each one, in order.
func GenerateSignature(data []byte) Signature {
	return GenerateSignatureWithBlockSize(data, DefaultBlockSize)
}

// GenerateSignatureWithBlockSize is GenerateSignature with an explicit
// block size - mainly so tests can exercise multi-block behavior with
// small fixtures instead of needing kilobyte-sized test data.
func GenerateSignatureWithBlockSize(data []byte, blockSize int) Signature {
	blocks := splitBlocks(data, blockSize)
	sig := Signature{
		BlockSize: blockSize,
		Blocks:    make([]BlockSignature, len(blocks)),
	}
	for i, block := range blocks {
		sig.Blocks[i] = BlockSignature{
			Weak:   newWeakChecksum(block).sum(),
			Strong: strongChecksum(block),
		}
	}
	return sig
}
