package sync

import "testing"

func TestSplitBlocks(t *testing.T) {
	tests := []struct {
		name      string
		data      []byte
		blockSize int
		wantLens  []int
	}{
		{"empty", []byte{}, 4, nil},
		{"exact multiple", []byte("aaaabbbbcccc"), 4, []int{4, 4, 4}},
		{"partial final block", []byte("aaaabbbbcc"), 4, []int{4, 4, 2}},
		{"smaller than one block", []byte("ab"), 4, []int{2}},
		{"single byte block size", []byte("abc"), 1, []int{1, 1, 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := splitBlocks(tt.data, tt.blockSize)
			if len(blocks) != len(tt.wantLens) {
				t.Fatalf("got %d blocks, want %d: %v", len(blocks), len(tt.wantLens), blocks)
			}
			for i, wantLen := range tt.wantLens {
				if len(blocks[i]) != wantLen {
					t.Errorf("block %d: len = %d, want %d", i, len(blocks[i]), wantLen)
				}
			}
			// Reassembling every block must exactly reproduce the input -
			// this is the property that actually matters (no bytes lost,
			// duplicated, or reordered), not just the length list.
			var got []byte
			for _, b := range blocks {
				got = append(got, b...)
			}
			if string(got) != string(tt.data) {
				t.Errorf("reassembled blocks = %q, want %q", got, tt.data)
			}
		})
	}
}
