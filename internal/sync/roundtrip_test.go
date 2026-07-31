package sync

import (
	"strings"
	"testing"
)

// TestRoundTrip runs the full receiver/sender/receiver cycle for each
// case: generate a Signature from the old file, generate a delta from
// (old, new) against that signature, apply the delta to the old file, and
// confirm the result is byte-for-byte identical to the new file. This is
// the property the whole algorithm exists to guarantee - none of the
// individual-step tests elsewhere in this package substitute for actually
// proving the full cycle reproduces the target file exactly.
func TestRoundTrip(t *testing.T) {
	const blockSize = 8

	base := strings.Repeat("0123456789abcdef", 4) // 64 bytes, 8 blocks

	tests := []struct {
		name string
		old  []byte
		new  []byte
	}{
		{
			name: "identical files",
			old:  []byte(base),
			new:  []byte(base),
		},
		{
			name: "appended bytes",
			old:  []byte(base),
			new:  []byte(base + "EXTRA-DATA-AT-THE-END"),
		},
		{
			name: "prepended bytes",
			old:  []byte(base),
			new:  []byte("EXTRA-DATA-AT-THE-START" + base),
		},
		{
			name: "single byte change in the middle",
			old:  []byte(base),
			new: func() []byte {
				b := []byte(base)
				b[32] = 'X'
				return b
			}(),
		},
		{
			name: "completely different files",
			old:  []byte(base),
			new:  []byte(strings.Repeat("!@#$%^&*()_+-=[]{}", 4)),
		},
		{
			name: "old file empty",
			old:  []byte{},
			new:  []byte(base),
		},
		{
			name: "new file empty",
			old:  []byte(base),
			new:  []byte{},
		},
		{
			name: "both files empty",
			old:  []byte{},
			new:  []byte{},
		},
		{
			name: "old file smaller than one block",
			old:  []byte("ab"),
			new:  []byte(base),
		},
		{
			name: "new file smaller than one block",
			old:  []byte(base),
			new:  []byte("xy"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := GenerateSignatureWithBlockSize(tt.old, blockSize)
			ops := GenerateDelta(sig, tt.new)
			got, err := ApplyDelta(tt.old, ops, sig)
			if err != nil {
				t.Fatalf("ApplyDelta returned error: %v", err)
			}
			if string(got) != string(tt.new) {
				t.Errorf("reconstructed = %q, want %q", got, tt.new)
			}
		})
	}
}
