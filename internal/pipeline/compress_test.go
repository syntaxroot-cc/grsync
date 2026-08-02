package pipeline

import (
	"bytes"
	"strings"
	"testing"
)

func TestClampCompressLevel(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{0, 0},   // explicit "off"
		{-1, 6},  // zlib.DefaultCompression -> real rsync's own default
		{1, 1},   // BestSpeed, unchanged
		{6, 6},   // already the default, unchanged
		{9, 9},   // BestCompression, unchanged
		{10, 9},  // too large, silently limited (rsync.1's own wording)
		{999, 9}, // wildly too large, still limited to 9
		{-5, 1},  // any other negative, limited up to the minimum
		{3, 3},
	}
	for _, tt := range tests {
		if got := ClampCompressLevel(tt.in); got != tt.want {
			t.Errorf("ClampCompressLevel(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseSkipCompressList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"gz", []string{"gz"}},
		{"gz/jpg/mp3", []string{"gz", "jpg", "mp3"}},
		{"GZ/Jpg", []string{"gz", "jpg"}},  // lowercased, matching real rsync's own add_nocompress_suffixes
		{"gz//jpg", []string{"gz", "jpg"}}, // empty segments dropped
	}
	for _, tt := range tests {
		got := ParseSkipCompressList(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("ParseSkipCompressList(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("ParseSkipCompressList(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

func TestSkipCompressSuffix(t *testing.T) {
	suffixes := []string{"gz", "jpg"}
	tests := []struct {
		path string
		want bool
	}{
		{"archive.gz", true},
		{"ARCHIVE.GZ", true}, // case-insensitive, matching real rsync's own lowercasing
		{"photo.jpg", true},
		{"notes.txt", false},
		{"noextension", false},
		{"dir.gz/file.txt", false}, // the suffix must belong to the final path component
	}
	for _, tt := range tests {
		if got := skipCompressSuffix(tt.path, suffixes); got != tt.want {
			t.Errorf("skipCompressSuffix(%q, %v) = %v, want %v", tt.path, suffixes, got, tt.want)
		}
	}
}

func TestCompressDecompressLiteral_RoundTrip(t *testing.T) {
	data := []byte(strings.Repeat("hello compressible world ", 500))
	for level := 1; level <= 9; level++ {
		compressed, ok := compressLiteral(data, level)
		if !ok {
			t.Fatalf("level %d: compressLiteral did not compress genuinely-compressible data", level)
		}
		if len(compressed) >= len(data) {
			t.Errorf("level %d: compressed len %d >= raw len %d, want smaller", level, len(compressed), len(data))
		}
		got, err := decompressLiteral(compressed)
		if err != nil {
			t.Fatalf("level %d: decompressLiteral returned error: %v", level, err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("level %d: round trip mismatch: got %d bytes, want %d bytes identical to original", level, len(got), len(data))
		}
	}
}

func TestCompressLiteral_FallsBackWhenNotSmaller(t *testing.T) {
	tiny := []byte{1, 2, 3}
	if _, ok := compressLiteral(tiny, DefaultCompressLevel); ok {
		t.Errorf("compressLiteral(%v) reported ok, want false - 3 bytes can never compress smaller than zlib's own fixed overhead", tiny)
	}
}

func TestCompressLiteral_EmptyInput(t *testing.T) {
	if _, ok := compressLiteral(nil, DefaultCompressLevel); ok {
		t.Errorf("compressLiteral(nil) reported ok, want false - there is nothing to gain by compressing zero bytes")
	}
}
