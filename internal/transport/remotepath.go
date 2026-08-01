// Package transport implements grsync's remote-shell (SSH) transport:
// spawning a remote-shell subprocess to reach a remote grsync process
// running in --server mode, and the framed protocol multiplexed over its
// stdin/stdout.
package transport

import "strings"

// RemotePath is a parsed [user@]host:path or [user@][host]:path (IPv6)
// remote endpoint, as used for the source/destination arguments of a
// remote sync.
type RemotePath struct {
	User string // empty if no "user@" prefix was present
	Host string // hostname or IP literal; brackets are stripped for IPv6
	Path string
}

// ParseRemotePath reports whether s looks like a remote
// [user@]host:path (or IPv6 [user@][host]:path) endpoint rather than a
// local filesystem path, returning the parsed form when it does.
//
// Disambiguation rule, in order:
//
//  0. A "://" anywhere in s (e.g. "rsync://host/module") is never this
//     syntax at all - real [user@]host:path syntax never contains one,
//     and without this check "rsync://host/module" would otherwise parse
//     as host "rsync", path "//host/module", which is wrong in a way
//     that's easy to miss (it "succeeds" instead of failing loudly).
//     Checked alongside the Windows-drive-letter case below, before any
//     of the numbered rules that follow ever run.
//  1. A single ASCII letter immediately followed by ":" (e.g. "C:",
//     "C:\Users\...") is always a Windows drive letter, never a remote
//     host - real single-letter hostnames in this position are
//     vanishingly rare in practice, while grsync runs natively on
//     Windows (unlike upstream rsync), so this ambiguity has to be
//     resolved in favor of the overwhelmingly common case.
//  2. A "/" appearing before the separating ":" means this can't be
//     [user@]host:path at all - no real hostname contains a "/", so
//     finding one first proves whatever precedes the colon is a path
//     segment, not a host (this also naturally handles a "user@" prefix
//     that isn't really one, e.g. a local path that happens to contain
//     "@").
//  3. Otherwise, an "[...]" immediately after any "user@" prefix is an
//     IPv6 literal - the separating ":" is the one right after the
//     closing "]", not the first ":" in the string (an IPv6 address is
//     full of colons itself).
//  4. Otherwise, the first ":" is the separator.
func ParseRemotePath(s string) (RemotePath, bool) {
	if s == "" || isWindowsDriveLetterPath(s) || strings.Contains(s, "://") {
		return RemotePath{}, false
	}

	rest := s
	user := ""
	if at := strings.IndexByte(rest, '@'); at != -1 {
		if strings.IndexByte(rest[:at], '/') == -1 {
			user = rest[:at]
			rest = rest[at+1:]
		}
	}

	var host, path string
	switch {
	case strings.HasPrefix(rest, "["):
		end := strings.IndexByte(rest, ']')
		if end == -1 || end+1 >= len(rest) || rest[end+1] != ':' {
			return RemotePath{}, false
		}
		host, path = rest[1:end], rest[end+2:]
	default:
		colon := strings.IndexByte(rest, ':')
		if colon == -1 {
			return RemotePath{}, false
		}
		if strings.IndexByte(rest[:colon], '/') != -1 {
			return RemotePath{}, false
		}
		host, path = rest[:colon], rest[colon+1:]
	}

	if host == "" {
		return RemotePath{}, false
	}
	return RemotePath{User: user, Host: host, Path: path}, true
}

func isWindowsDriveLetterPath(s string) bool {
	if len(s) < 2 {
		return false
	}
	c := s[0]
	isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	return isLetter && s[1] == ':'
}
