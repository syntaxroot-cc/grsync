package pipeline

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// DefaultCompressLevel is real rsync's own zlib default compression level.
const DefaultCompressLevel = 6

// ClampCompressLevel mirrors real rsync's own --compress-level handling:
// 0 is a distinct "off" sentinel, -1 means "use the default level", and
// anything else out of [1, 9] is silently clamped into range.
func ClampCompressLevel(level int) int {
	switch {
	case level == zlib.NoCompression: // 0: explicit "off"
		return 0
	case level == zlib.DefaultCompression: // -1: "use the default"
		return DefaultCompressLevel
	case level < zlib.BestSpeed: // any other value below 1
		return zlib.BestSpeed
	case level > zlib.BestCompression: // above 9
		return zlib.BestCompression
	default:
		return level
	}
}

// CompressOptions governs whether/how Sender compresses each regular
// file's literal delta data (--compress/-z) before sending it.
type CompressOptions struct {
	Enabled bool
	// Level is a zlib compression level from 1 (fastest) to 9 (smallest);
	// callers should run it through ClampCompressLevel first.
	Level int
	// SkipSuffixes is a lowercase, dot-free list of file suffixes (e.g.
	// "gz", "jpg") to send uncompressed even when Enabled. Nil/empty means
	// "skip nothing".
	SkipSuffixes []string
}

// DefaultSkipCompressSuffixes is real rsync's own built-in --skip-compress
// suffix list: file types that are already compressed, so recompressing
// them wastes CPU for no size benefit.
var DefaultSkipCompressSuffixes = []string{
	"3g2", "3gp", "7z", "aac", "ace", "apk", "avi", "bz2", "deb", "dmg",
	"ear", "f4v", "flac", "flv", "gpg", "gz", "iso", "jar", "jpeg", "jpg",
	"lrz", "lz", "lz4", "lzma", "lzo", "m1a", "m1v", "m2a", "m2ts", "m2v",
	"m4a", "m4b", "m4p", "m4r", "m4v", "mka", "mkv", "mov", "mp1", "mp2",
	"mp3", "mp4", "mpa", "mpeg", "mpg", "mpv", "mts", "odb", "odf", "odg",
	"odi", "odm", "odp", "ods", "odt", "oga", "ogg", "ogm", "ogv", "ogx",
	"opus", "otg", "oth", "otp", "ots", "ott", "oxt", "png", "qt", "rar",
	"rpm", "rz", "rzip", "spx", "squashfs", "sxc", "sxd", "sxg", "sxm",
	"sxw", "sz", "tbz", "tbz2", "tgz", "tlz", "ts", "txz", "tzo", "vob",
	"war", "webm", "webp", "xz", "z", "zip", "zst",
}

// ParseSkipCompressList parses a real rsync --skip-compress=LIST value:
// suffixes without their leading dot, separated by "/". An empty string
// is a meaningful value in its own right ("skip nothing"), not "unset".
func ParseSkipCompressList(list string) []string {
	if list == "" {
		return nil
	}
	parts := strings.Split(list, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, strings.ToLower(p))
	}
	return out
}

// skipCompressSuffix reports whether path's suffix (case-insensitively,
// without its leading dot) appears in skipSuffixes.
func skipCompressSuffix(path string, skipSuffixes []string) bool {
	ext := filepath.Ext(path)
	if ext == "" {
		return false
	}
	suffix := strings.ToLower(strings.TrimPrefix(ext, "."))
	for _, s := range skipSuffixes {
		if s == suffix {
			return true
		}
	}
	return false
}

// compressLiteral zlib-compresses data at level, returning ok == false if
// compression didn't actually help (the result is not smaller than data
// itself) so the caller can fall back to sending it raw.
func compressLiteral(data []byte, level int) (compressed []byte, ok bool) {
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		// level is always 1-9 via ClampCompressLevel, so this should be
		// unreachable; falling back to raw is safe either way.
		return nil, false
	}
	if _, err := w.Write(data); err != nil {
		return nil, false
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	if buf.Len() >= len(data) {
		return nil, false
	}
	return buf.Bytes(), true
}

// decompressLiteral reverses compressLiteral.
func decompressLiteral(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("opening zlib reader: %w", err)
	}
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading decompressed data: %w", err)
	}
	return out, nil
}
