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

// ProtocolVersion/SubProtocolVersion are what grsync's daemon claims in
// its @RSYNCD greeting line. This only governs the greeting/handshake
// text exchange this package implements - it is not a claim that
// everything a real rsync client would expect at this protocol version
// (digest negotiation, the real binary wire format for the transfer
// itself) is implemented. See the package doc comment and README for the
// exact boundary: handshake/auth are real-protocol-shaped, the transfer
// that follows is internal/pipeline's own gob protocol.
const (
	ProtocolVersion    = 31
	SubProtocolVersion = 0
)

// conn bundles a connection's read and write sides into one io.ReadWriter
// that every protocol step - greeting, module selection, auth, and
// finally the handoff to pipeline.Sender/Receiver - shares. This matters
// for correctness, not just convenience: line-based reads (bufio.Reader)
// can buffer bytes past the line they were asked for, and if later code
// switched to reading directly from the underlying net.Conn instead of
// continuing through this same *bufio.Reader, any bytes already
// buffered-but-unread would be silently lost.
type conn struct {
	r *bufio.Reader
	w io.Writer
}

func newConn(rw io.ReadWriter) *conn {
	return &conn{r: bufio.NewReader(rw), w: rw}
}

// Read/Write let *conn itself satisfy io.ReadWriter, so it can be handed
// directly to pipeline.Sender/Receiver once the handshake is done.
func (c *conn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *conn) Write(p []byte) (int, error) { return c.w.Write(p) }

func writeLine(w io.Writer, s string) error {
	_, err := io.WriteString(w, s+"\n")
	return err
}

// maxLineLength bounds every line read during the connection's text-based
// phases (greeting, module selection, auth). Without a cap, an
// unauthenticated client could force unbounded memory growth just by
// sending bytes with no "\n" - bufio.Reader.ReadString itself keeps
// growing its buffer until the delimiter appears. Real rsync's own line
// reader (read_line_old) is bounded the same way, not just this package's
// own invention.
const maxLineLength = 8192

// readLine reads one line, stripping the trailing "\n" and any "\r"
// immediately before it (tolerating a CRLF-sending peer without requiring
// one, since real rsync's own daemon protocol is LF-only). Reads
// byte-by-byte through r rather than via ReadString so an over-length
// line can be rejected before consuming unbounded memory, while still
// only ever going through the one shared *bufio.Reader every phase of
// this package uses.
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
// line. Any digest-list tokens after the version (real rsync protocol
// 30+ uses these to negotiate MD4 vs MD5) are accepted but ignored - this
// package always uses classic MD4 (see auth.go), not digest negotiation.
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

// ErrModuleListRequested is returned by ServeGreeting when the client
// asked to list modules (and the listing has already been written)
// rather than selecting one - the caller should close the connection at
// that point, not proceed to authentication or transfer.
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

// writeModuleList writes every listable module (List == true - "list =
// false" modules are deliberately excluded here, not just documented as
// excluded) as "<name>\t<comment>", then a terminating "@RSYNCD: EXIT"
// line, matching real rsync's own listing terminator.
func writeModuleList(w io.Writer, cfg *Config) error {
	names := make([]string, 0, len(cfg.Modules))
	for name, m := range cfg.Modules {
		if m.List {
			names = append(names, name)
		}
	}
	sort.Strings(names) // deterministic order; map iteration alone isn't

	for _, name := range names {
		if err := writeLine(w, fmt.Sprintf("%s\t%s", name, cfg.Modules[name].Comment)); err != nil {
			return fmt.Errorf("writing module list: %w", err)
		}
	}
	return writeLine(w, "@RSYNCD: EXIT")
}

// DialGreeting runs the client side of the initial handshake against an
// already-connected transport: reads the daemon's greeting first, then
// sends ours in reply, matching real rsync's actual ordering (the daemon
// speaks first on accept; the client's greeting is a response to it, not
// sent independently). Getting this backwards would deadlock a real
// synchronous transport where both ends' first move is a write with
// nobody yet reading - which is exactly how this ordering bug was caught.
// Then sends module (or "#list" if module is empty, matching a bare
// rsync://host URL). Returns the raw module-list lines when listing was
// requested (module == ""); returns nil lines otherwise, ready for the
// caller to proceed to authentication.
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
