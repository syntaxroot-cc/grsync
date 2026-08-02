package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ProtocolVersion/SubProtocolVersion are what grsync's daemon claims in its
// @RSYNCD greeting line. This only covers the greeting/handshake text
// exchange this package implements, not real rsync's binary wire protocol
// for the transfer itself (see the package doc comment).
const (
	ProtocolVersion    = 31
	SubProtocolVersion = 0
)

// conn bundles a connection's read and write sides into one io.ReadWriter
// shared by every protocol step. Reads must stay routed through the same
// *bufio.Reader throughout: switching to the raw net.Conn partway would
// silently drop any bytes already buffered but unread.
type conn struct {
	r *bufio.Reader
	w io.Writer
}

func newConn(rw io.ReadWriter) *conn {
	return &conn{r: bufio.NewReader(rw), w: rw}
}

func (c *conn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *conn) Write(p []byte) (int, error) { return c.w.Write(p) }

func writeLine(w io.Writer, s string) error {
	_, err := io.WriteString(w, s+"\n")
	return err
}

// maxLineLength bounds every line read during the connection's text-based
// phases. Without a cap, an unauthenticated client could force unbounded
// memory growth by sending bytes with no "\n".
const maxLineLength = 8192

// readLine reads one line, stripping the trailing "\n" and any "\r"
// immediately before it. Reads byte-by-byte rather than via
// bufio.Reader.ReadString so an over-length line is rejected before
// consuming unbounded memory.
func readLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			break
		}
		if len(buf) >= maxLineLength {
			return "", fmt.Errorf("line exceeds maximum length of %d bytes", maxLineLength)
		}
		buf = append(buf, b)
	}
	return strings.TrimRight(string(buf), "\r"), nil
}

func writeGreeting(w io.Writer) error {
	return writeLine(w, fmt.Sprintf("@RSYNCD: %d.%d", ProtocolVersion, SubProtocolVersion))
}

// readGreeting parses a peer's "@RSYNCD: <version>.<subprotocol> ..."
// line. Any digest-list tokens after the version are accepted but ignored -
// this package always uses classic MD4 (see auth.go), not digest negotiation.
func readGreeting(r *bufio.Reader) (version, subVersion int, err error) {
	line, err := readLine(r)
	if err != nil {
		return 0, 0, fmt.Errorf("reading greeting: %w", err)
	}
	const prefix = "@RSYNCD: "
	if !strings.HasPrefix(line, prefix) {
		return 0, 0, fmt.Errorf("invalid greeting %q: expected %q prefix", line, prefix)
	}
	fields := strings.Fields(strings.TrimPrefix(line, prefix))
	if len(fields) == 0 {
		return 0, 0, fmt.Errorf("invalid greeting %q: missing version", line)
	}

	verParts := strings.SplitN(fields[0], ".", 2)
	version, err = strconv.Atoi(verParts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid greeting %q: bad version %q", line, verParts[0])
	}
	if len(verParts) == 2 {
		subVersion, err = strconv.Atoi(verParts[1])
		if err != nil {
			return 0, 0, fmt.Errorf("invalid greeting %q: bad subprotocol %q", line, verParts[1])
		}
	}
	return version, subVersion, nil
}

// ErrModuleListRequested is returned by ServeGreeting when the client asked
// to list modules rather than selecting one; the caller should close the
// connection at that point.
var ErrModuleListRequested = errors.New("client requested module listing, connection should now close")

// ServeGreeting runs the server side of the initial handshake: writes our
// @RSYNCD greeting, reads the client's, then reads either "#list"
// (writes the listing and returns ErrModuleListRequested) or a module
// name (validated against cfg, returned as selected).
func ServeGreeting(c *conn, cfg *Config) (selected Module, err error) {
	if err := writeGreeting(c.w); err != nil {
		return Module{}, fmt.Errorf("writing greeting: %w", err)
	}
	if _, _, err := readGreeting(c.r); err != nil {
		return Module{}, err
	}

	request, err := readLine(c.r)
	if err != nil {
		return Module{}, fmt.Errorf("reading module request: %w", err)
	}

	if request == "#list" {
		if err := writeModuleList(c.w, cfg); err != nil {
			return Module{}, err
		}
		return Module{}, ErrModuleListRequested
	}

	m, ok := cfg.Modules[request]
	if !ok {
		_ = writeLine(c.w, fmt.Sprintf("@ERROR: Unknown module %q", request))
		return Module{}, fmt.Errorf("client requested unknown module %q", request)
	}
	return m, nil
}

// writeModuleList writes every listable module (List == true) as
// "<name>\t<comment>", then a terminating "@RSYNCD: EXIT" line.
func writeModuleList(w io.Writer, cfg *Config) error {
	names := make([]string, 0, len(cfg.Modules))
	for name, m := range cfg.Modules {
		if m.List {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if err := writeLine(w, fmt.Sprintf("%s\t%s", name, cfg.Modules[name].Comment)); err != nil {
			return fmt.Errorf("writing module list: %w", err)
		}
	}
	return writeLine(w, "@RSYNCD: EXIT")
}

// DialGreeting runs the client side of the initial handshake: reads the
// daemon's greeting first, then sends ours in reply - the daemon speaks
// first on accept, so getting this order backwards deadlocks a real
// synchronous transport where both ends' first move is a write with
// nobody yet reading. Then sends module (or "#list" if module is empty).
// Returns the raw module-list lines when listing was requested (module ==
// ""); returns nil lines otherwise, ready for the caller to authenticate.
func DialGreeting(c *conn, module string) (listing []string, err error) {
	if _, _, err := readGreeting(c.r); err != nil {
		return nil, err
	}
	if err := writeGreeting(c.w); err != nil {
		return nil, fmt.Errorf("writing greeting: %w", err)
	}

	if module == "" {
		if err := writeLine(c.w, "#list"); err != nil {
			return nil, fmt.Errorf("requesting module list: %w", err)
		}
		return readModuleList(c.r)
	}

	if err := writeLine(c.w, module); err != nil {
		return nil, fmt.Errorf("selecting module %q: %w", module, err)
	}
	return nil, nil
}

// readModuleList reads lines until the "@RSYNCD: EXIT" terminator
// writeModuleList sends, returning every line before it.
func readModuleList(r *bufio.Reader) ([]string, error) {
	var lines []string
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, fmt.Errorf("reading module list: %w", err)
		}
		if line == "@RSYNCD: EXIT" {
			return lines, nil
		}
		if strings.HasPrefix(line, "@ERROR") {
			return nil, fmt.Errorf("server error: %s", line)
		}
		lines = append(lines, line)
	}
}
