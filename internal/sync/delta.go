package sync

import "fmt"

// DeltaOp is one operation in a delta stream, produced by GenerateDelta and
// consumed by ApplyDelta. isDeltaOp is unexported so CopyOp and DataOp are
// its only implementations; callers type-switch on the concrete type.
type DeltaOp interface {
	isDeltaOp()
}

// CopyOp copies block BlockIndex - an index into the Signature's Blocks
// that GenerateDelta was given - from the receiver's old file, unchanged.
type CopyOp struct {
	BlockIndex int
}

func (CopyOp) isDeltaOp() {}

// DataOp writes Bytes literally: data that didn't match any signature block.
type DataOp struct {
	Bytes []byte
}

func (DataOp) isDeltaOp() {}

// GenerateDelta compares newData against sig, a signature of old data the
// receiver already has, and produces an ordered delta that reconstructs
// newData when applied to that old data via ApplyDelta.
//
// It slides a blockSize-byte window across newData one byte at a time,
// maintaining the rolling weak checksum incrementally (weakChecksum.roll,
// O(1) per byte) instead of recomputing it from scratch at every position,
// which would degrade this to an O(n*blockSize) scan.
func GenerateDelta(sig Signature, newData []byte) []DeltaOp {
	blockSize := sig.BlockSize
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}

	// weak checksum -> indices of every block sharing it. Two different
	// blocks colliding on their 32-bit weak sum is expected occasionally by
	// chance; the strong-checksum check below disambiguates candidates.
	weakIndex := make(map[uint32][]int, len(sig.Blocks))
	for i, b := range sig.Blocks {
		weakIndex[b.Weak] = append(weakIndex[b.Weak], i)
	}

	var ops []DeltaOp
	var pending []byte // literal bytes seen so far that haven't matched a block yet

	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		ops = append(ops, DataOp{Bytes: pending})
		// Reset to nil, not just len 0, so the next append starts a fresh
		// backing array instead of growing into the slice just handed to DataOp.
		pending = nil
	}

	// tryMatch checks whether the blockSize-byte window at newData[at:], with
	// already-computed rolling checksum weak, matches a signature block.
	tryMatch := func(at int, weak weakChecksum) (blockIndex int, ok bool) {
		candidates, found := weakIndex[weak.sum()]
		if !found {
			return 0, false
		}
		strong := strongChecksum(newData[at : at+blockSize])
		for _, idx := range candidates {
			if sig.Blocks[idx].Strong == strong {
				return idx, true
			}
		}
		return 0, false
	}

	n := len(newData)
	pos := 0
	for pos < n {
		if pos+blockSize > n {
			// Fewer than blockSize bytes remain: no full window left to match.
			pending = append(pending, newData[pos:]...)
			break
		}

		// Computed fresh only here, then advanced with roll() per byte below.
		weak := newWeakChecksum(newData[pos : pos+blockSize])
		for {
			if idx, ok := tryMatch(pos, weak); ok {
				flushPending()
				ops = append(ops, CopyOp{BlockIndex: idx})
				pos += blockSize // skip past the whole matched block, not just one byte
				break
			}

			pending = append(pending, newData[pos])
			pos++
			if pos+blockSize > n {
				break
			}
			weak = weak.roll(newData[pos-1], newData[pos+blockSize-1])
		}
	}

	flushPending()
	return ops
}

// ApplyDelta reconstructs a file from oldData and an ordered []DeltaOp
// produced by GenerateDelta(sig, ...) against that same oldData.
//
// sig must be the exact Signature GenerateDelta was called with: a CopyOp
// only carries a block index, not byte offsets, so BlockSize is the only way
// to recover which bytes of oldData that index refers to. A mismatched
// BlockSize would silently reconstruct the wrong bytes.
//
// blockSize is validated lazily, only once a CopyOp needs it to translate a
// BlockIndex into a byte range - append-mode construction can legitimately
// produce a Signature with BlockSize == 0 when the old file is empty, and a
// delta with no CopyOps should not be rejected for a value it never uses.
func ApplyDelta(oldData []byte, ops []DeltaOp, sig Signature) ([]byte, error) {
	blockSize := sig.BlockSize

	var out []byte
	for i, op := range ops {
		switch o := op.(type) {
		case CopyOp:
			if blockSize <= 0 {
				return nil, fmt.Errorf("op %d: invalid signature block size %d", i, blockSize)
			}
			start := o.BlockIndex * blockSize
			if o.BlockIndex < 0 || start >= len(oldData) {
				return nil, fmt.Errorf("op %d: CopyOp block index %d is out of range for a %d-byte old file", i, o.BlockIndex, len(oldData))
			}
			end := start + blockSize
			if end > len(oldData) {
				end = len(oldData) // the final block may be shorter than blockSize
			}
			out = append(out, oldData[start:end]...)
		case DataOp:
			out = append(out, o.Bytes...)
		default:
			return nil, fmt.Errorf("op %d: unknown DeltaOp type %T", i, op)
		}
	}
	return out, nil
}
