package daemon

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// DefaultPort is the standard rsync daemon TCP port.
const DefaultPort = 873

// URL is a parsed rsync://[user@]host[:port]/module[/path] endpoint.
type URL struct {
	User string // empty if no "user@" was present
	Host string
	// Port is 0 if the URL didn't specify one; callers should treat that
	// as DefaultPort.
	Port int
	// Module is empty for a bare "rsync://host" or "rsync://host/" URL,
	// which means "list this daemon's available modules" rather than an error.
	Module string
	Path   string
}

// ParseURL parses s as an rsync:// URL.
func ParseURL(s string) (URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return URL{}, fmt.Errorf("parsing %q: %w", s, err)
	}
	if u.Scheme != "rsync" {
		return URL{}, fmt.Errorf("%q is not an rsync:// URL (scheme %q)", s, u.Scheme)
	}
	if u.Hostname() == "" {
		return URL{}, fmt.Errorf("%q has no host", s)
	}

	result := URL{Host: u.Hostname()}
	if u.User != nil {
		result.User = u.User.Username()
	}
	if portStr := u.Port(); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return URL{}, fmt.Errorf("%q has an invalid port %q", s, portStr)
		}
		result.Port = port
	}

	path := strings.TrimPrefix(u.Path, "/")
	if path == "" {
		return result, nil // no module: valid "list modules" form
	}
	if slash := strings.IndexByte(path, '/'); slash != -1 {
		result.Module = path[:slash]
		result.Path = path[slash+1:]
	} else {
		result.Module = path
	}

	return result, nil
}
