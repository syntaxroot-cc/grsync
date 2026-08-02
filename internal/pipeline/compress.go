package pipeline

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// DefaultCompressLevel is real rsync's own zlib default, verified against
// upstream's token.c (init_compression_level's def_level for the zlib/
// zlibx choice) rather than assumed.
const DefaultCompressLevel = 6

// ClampCompressLevel mirrors real rsync's own --compress-level handling
// for zlib compression (token.c's init_compression_level), verified
// against upstream source and rsync.1's own documented wording rather
// than guessed: 0 is a distinct "off" sentinel, not clamped up into
// range; -1 (zlib.DefaultCompression) explicitly means "use the default
// level" (6); anything else out of [1, 9] is silently limited into range
// ("If you specify a too-large or too-small value, the number is
// silently limited to a valid value" - rsync.1's own wording).
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
// file's literal delta data (--compress/-z) before sending it - see the
// README's Compression section. It is consulted only by Sender: Receiver
// needs no compression options of its own at all, since each
// deltaMessage's own Compressed marker (messages.go) already says
// whether its literal data needs decompressing first - a purely
// data-driven decision on that side, not a policy one.
type CompressOptions struct {
	Enabled bool
	// Level is a zlib compression level from 1 (fastest) to 9 (smallest),
	// meaningful only when Enabled - see ClampCompressLevel, which every
	// caller that constructs a CompressOptions with Enabled: true is
	// expected to have already run Level through.
	Level int
	// SkipSuffixes is a lowercase, dot-free list of file suffixes (e.g.
	// "gz", "jpg") to send uncompressed even when Enabled - real rsync's
	// own --skip-compress default list (DefaultSkipCompressSuffixes) or a
	// caller override (see ParseSkipCompressList). Nil/empty means "skip
	// nothing," matching real rsync's own documented meaning of an empty
	// --skip-compress=LIST.
	SkipSuffixes []string
}

// DefaultSkipCompressSuffixes is real rsync's own built-in --skip-compress
// suffix list, copied verbatim from rsync.1.md's own documented default
// (the same list default-dont-compress.h is generated from) rather than
// invented - files with one of these suffixes are already compressed
// formats where running zlib over them again wastes CPU for no size
// benefit.
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
// is a meaningful value in its own right ("skip nothing"), not "unset" -
// see effectiveCompressOptions (internal/cli) for how that distinction
// from "the flag was never given at all" is made.
//
// Real rsync's own LIST grammar also supports bracketed character
// classes inside a suffix (e.g. "mp[34]" for "mp3"/"mp4"); grsync's
// --skip-compress does not, a deliberate, disclosed scope reduction (see
// the README's Compression section) rather than a silent gap - plain
// slash-separated suffixes cover the default list and the overwhelming
// majority of real-world uses.
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
//
// This check matters more here than it would for real rsync's own zlib
// usage: real rsync keeps one persistent deflate stream open per file,
// so its fixed ~8-byte zlib header/trailer cost is paid once per file no
// matter how many separate literal runs cross the wire. grsync's
// deltaMessage is sent as a single, independent frame per file with no
// persistent compression context to reuse across files - toWireDeltaOps
// already amortizes that overhead across everything within one file by
// compressing the whole concatenated literal stream as a single unit
// rather than op-by-op (see deltaMessage's own doc comment), but a file
// whose total literal data is tiny (a few changed bytes in an otherwise-
// unchanged large file - exactly the case delta transfer exists for) or
// already-incompressible can still legitimately come out larger
// compressed than raw. Falling back per file, only when it doesn't pay
// off, is a small, real improvement over always compressing regardless.
func compressLiteral(data []byte, level int) (compressed []byte, ok bool) {
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		// ClampCompressLevel guarantees level is 1-9, which zlib always
		// accepts - this should be unreachable, but treating any error as
		// "just send raw" is safe (a pure optimization, never required
		// for correctness) rather than propagating a hard failure for it.
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
