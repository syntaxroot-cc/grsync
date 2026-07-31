package sync

import "fmt"

// DeltaOp is one operation in a delta stream, produced by GenerateDelta
// and consumed by ApplyDelta. It's a sealed interface - isDeltaOp is
// unexported, so CopyOp and DataOp are its only implementations; callers
// type-switch on the concrete type.
type DeltaOp interface {
	isDeltaOp()
}

// CopyOp copies block BlockIndex - an index into the Blocks of the
// Signature that GenerateDelta was given - from the receiver's old file,
// unchanged.
type CopyOp struct {
	BlockIndex int
}

func (CopyOp) isDeltaOp() {}

// DataOp writes Bytes literally: data present in the new file that didn't
// match any block in the old file's signature.
type DataOp struct {
	Bytes []byte
}

func (DataOp) isDeltaOp() {}

// GenerateDelta compares newData against sig - a signature of some old
// data the receiver already has - and produces an ordered delta that,
// applied to that old data via ApplyDelta, reconstructs newData.
//
// It slides a blockSize-byte window across newData one byte at a time,
// maintaining the rolling weak checksum incrementally (weakChecksum.roll,
// O(1) per byte) rather than recomputing it from scratch at every
// position - recomputing would silently make this an O(n*blockSize)
// scan, defeating the entire reason a rolling checksum exists.
func GenerateDelta(sig Signature, newData []byte) []DeltaOp {
	blockSize := sig.BlockSize
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}

	// weak checksum -> indices of every block sharing it. A weak checksum
	// collision (two different blocks that happen to produce the same
	// 32-bit weak sum) is expected to happen occasionally by chance; the
	// slice of candidates lets the strong-checksum check below disambiguate
	// rather than assuming the first weak match is correct.
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
		// Reset to nil (not just len 0) so the next append starts a fresh
		// backing array instead of potentially growing into - and
		// corrupting - the slice just handed to the DataOp above.
		pending = nil
	}

	// tryMatch checks whether the blockSize-byte window at newData[at:] -
	// whose already-computed rolling checksum is weak - matches a
	// signature block. Confirms via strong checksum before accepting.
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
			// Fewer than blockSize bytes remain: no full window left to
			// possibly match, so the rest of the file is literal data.
			// (No need to advance pos before this break - nothing reads
			// it again once the loop exits.)
			pending = append(pending, newData[pos:]...)
			break
		}

		// Freshly computed only here - once per match/skip-ahead, not per
		// byte - then advanced with roll() for every subsequent byte the
		// inner loop steps through without a match.
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
				break // not enough bytes left for a full window anymore
			}
			weak = weak.roll(newData[pos-1], newData[pos+blockSize-1])
		}
	}

	flushPending()
	return ops
}

// ApplyDelta reconstructs a file from oldData and an ordered []DeltaOp
// produced by GenerateDelta(sig, ...) against that same oldData: each
// CopyOp copies its referenced block out of oldData, and each DataOp
// writes its literal bytes, in order.
//
// sig must be the exact Signature GenerateDelta was called with - a
// CopyOp only carries a block index, not byte offsets, so BlockSize is
// the only way to recover which bytes of oldData that index refers to.
// Passing a different Signature (or a hand-built one with a mismatched
// BlockSize) would silently reconstruct the wrong bytes; that's exactly
// the class of bug bundling BlockSize into Signature (see signature.go)
// is meant to make harder, by giving ApplyDelta and GenerateDelta the
// same single source of truth for it instead of two independent
// parameters that could drift apart.
//
// The result is returned as a []byte rather than written to an io.Writer:
// every function in this package so far (Walk aside) operates on
// in-memory byte slices - there's no streaming I/O anywhere yet for this
// to plug into - so an io.Writer parameter would just be unused
// flexibility at this stage. That can change if/when a streaming
// transport is introduced later.
func ApplyDelta(oldData []byte, ops []DeltaOp, sig Signature) ([]byte, error) {
	blockSize := sig.BlockSize
	if blockSize <= 0 {
		return nil, fmt.Errorf("invalid signature block size %d", blockSize)
	}

	var out []byte
	for i, op := range ops {
		switch o := op.(type) {
		case CopyOp:
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
