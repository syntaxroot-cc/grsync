package sync

import "testing"

func TestGenerateSignature_BlockCountAndChecksums(t *testing.T) {
	data := []byte("AAAABBBBCC") // 10 bytes, block size 4 -> blocks "AAAA","BBBB","CC"
	sig := GenerateSignatureWithBlockSize(data, 4)

	if sig.BlockSize != 4 {
		t.Errorf("BlockSize = %d, want 4", sig.BlockSize)
	}
	if len(sig.Blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(sig.Blocks), sig.Blocks)
	}

	wantBlocks := [][]byte{[]byte("AAAA"), []byte("BBBB"), []byte("CC")}
	for i, wantBlock := range wantBlocks {
		wantWeak := newWeakChecksum(wantBlock).sum()
		wantStrong := strongChecksum(wantBlock)
		if sig.Blocks[i].Weak != wantWeak {
			t.Errorf("block %d: Weak = %d, want %d", i, sig.Blocks[i].Weak, wantWeak)
		}
		if sig.Blocks[i].Strong != wantStrong {
			t.Errorf("block %d: Strong = %x, want %x", i, sig.Blocks[i].Strong, wantStrong)
		}
	}
}

func TestGenerateSignature_EmptyData(t *testing.T) {
	sig := GenerateSignatureWithBlockSize(nil, DefaultBlockSize)
	if len(sig.Blocks) != 0 {
		t.Errorf("got %d blocks for empty input, want 0", len(sig.Blocks))
	}
}

func TestGenerateSignature_DefaultBlockSize(t *testing.T) {
	sig := GenerateSignature([]byte("some data"))
	if sig.BlockSize != DefaultBlockSize {
		t.Errorf("BlockSize = %d, want DefaultBlockSize (%d)", sig.BlockSize, DefaultBlockSize)
	}
}
