package sync

import (
	"strings"
	"testing"
)

func countOps(ops []DeltaOp) (copies, data int) {
	for _, op := range ops {
		switch op.(type) {
		case CopyOp:
			copies++
		case DataOp:
			data++
		}
	}
	return copies, data
}

func TestApplyDelta_OutOfRangeBlockIndexErrors(t *testing.T) {
	old := []byte("AAAABBBB") // 2 blocks of 4
	sig := GenerateSignatureWithBlockSize(old, 4)

	_, err := ApplyDelta(old, []DeltaOp{CopyOp{BlockIndex: 5}}, sig)
	if err == nil {
		t.Fatalf("ApplyDelta with an out-of-range CopyOp index returned nil error, want an error")
	}

	_, err = ApplyDelta(old, []DeltaOp{CopyOp{BlockIndex: -1}}, sig)
	if err == nil {
		t.Fatalf("ApplyDelta with a negative CopyOp index returned nil error, want an error")
	}
}

// TestApplyDelta_InvalidBlockSizeErrorsOnlyWhenACopyOpNeedsIt is SC-12's
// own relaxation of ApplyDelta's block-size validation, made concrete: a
// zero/negative BlockSize is only ever actually consulted when
// translating a CopyOp's BlockIndex into a byte range, so it must only
// be rejected when ops genuinely contains a CopyOp - not unconditionally
// up front, which would reject SC-12's own append-mode construction for
// a brand-new (zero-length existing) destination file, where a
// Signature with BlockSize == 0 and no CopyOp at all is entirely valid
// (there is no prefix block to describe).
func TestApplyDelta_InvalidBlockSizeErrorsOnlyWhenACopyOpNeedsIt(t *testing.T) {
	if _, err := ApplyDelta([]byte("data"), []DeltaOp{CopyOp{BlockIndex: 0}}, Signature{BlockSize: 0}); err == nil {
		t.Errorf("ApplyDelta with a zero block size and a CopyOp returned nil error, want an error")
	}

	got, err := ApplyDelta(nil, []DeltaOp{DataOp{Bytes: []byte("new content")}}, Signature{BlockSize: 0})
	if err != nil {
		t.Fatalf("ApplyDelta with a zero block size and no CopyOp returned error: %v", err)
	}
	if string(got) != "new content" {
		t.Errorf("ApplyDelta result = %q, want %q", got, "new content")
	}

	got, err = ApplyDelta(nil, nil, Signature{BlockSize: 0})
	if err != nil {
		t.Fatalf("ApplyDelta with a zero block size and no ops at all returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ApplyDelta result = %q, want empty", got)
	}
}

// TestGenerateDelta_WeakChecksumCollisionDisambiguatedByStrongChecksum
// exercises the actual weak-checksum-collision path, rather than assuming
// the strong-checksum disambiguation code is correct because it looks
// right. block1, block2, and block3 below are three deliberately
// constructed, genuinely different 3-byte sequences that all produce the
// identical weak checksum (a=60, b=100 - hand-verified against
// newWeakChecksum's exact weighting convention, no modular wraparound
// needed): with weight (n-i) for byte i in a window of n=3, all three
// satisfy sum=60 and 3x+2y+z=100 simultaneously by construction.
//
// If GenerateDelta ever regressed to trusting a weak-checksum match
// without confirming it via strong checksum, this test would silently
// start producing corrupted output (a CopyOp pointing at the wrong
// block) - exactly the class of bug a weak checksum alone can't catch,
// which is the entire reason a strong checksum exists.
func TestGenerateDelta_WeakChecksumCollisionDisambiguatedByStrongChecksum(t *testing.T) {
	block1 := []byte{10, 20, 30}
	block2 := []byte{15, 10, 35}
	block3 := []byte{12, 16, 32} // present in neither signature block

	if newWeakChecksum(block1).sum() != newWeakChecksum(block2).sum() {
		t.Fatalf("test setup invalid: block1 and block2 don't actually collide")
	}
	if newWeakChecksum(block1).sum() != newWeakChecksum(block3).sum() {
		t.Fatalf("test setup invalid: block1 and block3 don't actually collide")
	}

	old := append(append([]byte{}, block1...), block2...) // 6 bytes: block index 0 = block1, index 1 = block2
	sig := GenerateSignatureWithBlockSize(old, 3)
	if sig.Blocks[0].Weak != sig.Blocks[1].Weak {
		t.Fatalf("test setup invalid: signature blocks don't share a weak checksum")
	}
	if sig.Blocks[0].Strong == sig.Blocks[1].Strong {
		t.Fatalf("test setup invalid: signature blocks unexpectedly share a strong checksum too")
	}

	t.Run("matches block2, not block1, despite the weak collision", func(t *testing.T) {
		ops := GenerateDelta(sig, block2)
		if len(ops) != 1 {
			t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
		}
		cp, ok := ops[0].(CopyOp)
		if !ok || cp.BlockIndex != 1 {
			t.Errorf("op = %+v, want CopyOp{BlockIndex: 1}", ops[0])
		}
	})

	t.Run("matches block1, not block2, despite the weak collision", func(t *testing.T) {
		ops := GenerateDelta(sig, block1)
		if len(ops) != 1 {
			t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
		}
		cp, ok := ops[0].(CopyOp)
		if !ok || cp.BlockIndex != 0 {
			t.Errorf("op = %+v, want CopyOp{BlockIndex: 0}", ops[0])
		}
	})

	t.Run("rejects a weak match when no candidate's strong checksum matches", func(t *testing.T) {
		ops := GenerateDelta(sig, block3)
		if len(ops) != 1 {
			t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
		}
		d, ok := ops[0].(DataOp)
		if !ok || string(d.Bytes) != string(block3) {
			t.Errorf("op = %+v, want DataOp{Bytes: %v} - a weak-only match must not produce a CopyOp", ops[0], block3)
		}
	})
}

func TestGenerateDelta_IdenticalFileIsAllCopies(t *testing.T) {
	data := []byte("AAAABBBBCCCCDDDD") // 16 bytes, 4 blocks of 4
	sig := GenerateSignatureWithBlockSize(data, 4)

	ops := GenerateDelta(sig, data)

	copies, dataOps := countOps(ops)
	if dataOps != 0 {
		t.Errorf("got %d DataOps for an identical file, want 0: %+v", dataOps, ops)
	}
	if copies != 4 {
		t.Errorf("got %d CopyOps, want 4 (one per block): %+v", copies, ops)
	}
	// Order matters too: block 0 first, then 1, 2, 3.
	for i, op := range ops {
		cp, ok := op.(CopyOp)
		if !ok || cp.BlockIndex != i {
			t.Errorf("op %d = %+v, want CopyOp{BlockIndex: %d}", i, op, i)
		}
	}
}

func TestGenerateDelta_CompletelyDifferentFileIsAllData(t *testing.T) {
	old := []byte("AAAABBBBCCCCDDDD")
	sig := GenerateSignatureWithBlockSize(old, 4)

	newData := []byte("wxyz1234!@#$%^&*") // shares no blocks with old
	ops := GenerateDelta(sig, newData)

	copies, dataOps := countOps(ops)
	if copies != 0 {
		t.Errorf("got %d CopyOps for a completely different file, want 0: %+v", copies, ops)
	}
	if dataOps == 0 {
		t.Fatalf("got 0 DataOps, want at least 1")
	}

	// Reassembling every DataOp's bytes must reproduce newData exactly,
	// since nothing was copied.
	var got []byte
	for _, op := range ops {
		got = append(got, op.(DataOp).Bytes...)
	}
	if string(got) != string(newData) {
		t.Errorf("reassembled data = %q, want %q", got, newData)
	}
}

func TestGenerateDelta_SingleByteChangeInMiddle(t *testing.T) {
	old := []byte(strings.Repeat("0123456789", 5)) // 50 bytes, block size 10 -> 5 blocks
	sig := GenerateSignatureWithBlockSize(old, 10)

	newData := []byte(strings.Repeat("0123456789", 5))
	newData[25] = 'X' // change one byte inside block index 2 (bytes 20-29)

	ops := GenerateDelta(sig, newData)

	copies, dataOps := countOps(ops)
	if copies == 0 {
		t.Errorf("got 0 CopyOps for a mostly-unchanged file, want several")
	}
	if dataOps == 0 {
		t.Fatalf("got 0 DataOps despite a changed byte, want at least 1")
	}

	// The whole point of the algorithm: a one-byte change should cost a
	// small, bounded amount of literal data, not force the whole file (or
	// even the whole surrounding block) to be retransmitted as data.
	var totalDataBytes int
	for _, op := range ops {
		if d, ok := op.(DataOp); ok {
			totalDataBytes += len(d.Bytes)
		}
	}
	if totalDataBytes > 20 {
		t.Errorf("total literal bytes = %d, want a small bounded amount for a single changed byte", totalDataBytes)
	}

	// Reconstructing from ops must still reproduce newData exactly - the
	// strongest check that "mostly CopyOps, one small DataOp" is actually
	// correct, not just small.
	reconstructed, err := ApplyDelta(old, ops, sig)
	if err != nil {
		t.Fatalf("ApplyDelta returned error: %v", err)
	}
	if string(reconstructed) != string(newData) {
		t.Errorf("reconstructed = %q, want %q", reconstructed, newData)
	}
}
