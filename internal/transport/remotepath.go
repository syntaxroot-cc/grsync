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

// ParseRemotePath reports whether s is a remote [user@]host:path (or IPv6
// [user@][host]:path) endpoint rather than a local filesystem path,
// returning the parsed form when it does.
//
// A "://" anywhere disqualifies it (so rsync daemon URLs aren't
// misparsed as host "rsync"), a leading single-letter drive spec like
// "C:" is always treated as a Windows path, and a "/" before the
// separating ":" also disqualifies it since no real hostname contains
// one. IPv6 hosts must be bracketed, since the address itself is full of
// colons.
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
