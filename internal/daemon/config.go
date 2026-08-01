// Package daemon implements grsync's rsync-daemon-protocol server: parsing
// rsyncd.conf, the rsync:// URL scheme, the @RSYNCD greeting/handshake,
// MD4 challenge-response authentication, and per-module access control.
// Once a client has authenticated and selected a module, this package
// hands the connection straight to internal/pipeline's existing
// Sender/Receiver - the daemon protocol is a second way to *establish* a
// connection, not a second way to *transfer files*.
package daemon

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Module is one [module_name] section of an rsyncd.conf file, plus
// whatever global defaults it didn't override.
type Module struct {
	Name string
	// Path is the directory on the daemon's filesystem this module
	// exposes. Required for every module - rsyncd.conf itself requires it.
	Path string
	// ReadOnly defaults to true, matching real rsync: "The default is for
	// all modules to be read only."
	ReadOnly bool
	// List defaults to true; "list = false" hides the module from a
	// #list request without preventing a client who already knows its
	// name from connecting to it - a real, documented rsyncd.conf option,
	// not an invented one.
	List bool
	// Comment is shown alongside the module name in a #list response
	// ("<name>\t<comment>", real rsync's own listing format).
	Comment string
	// Exclude is the raw, space-separated pattern list from the "exclude"
	// parameter, not yet compiled into sync.Rule - see access.go.
	Exclude []string
	// AuthUsers is the raw, comma/space-separated list from "auth users".
	// A non-empty list means this module requires authentication.
	AuthUsers []string
	// SecretsFile is the path to a "name:password" per-line file, per
	// "secrets file".
	SecretsFile string
	// MaxConnections is the simultaneous-connection cap for this module;
	// 0 (the default) means unlimited, matching real rsync.
	MaxConnections int
}

// Config is a parsed rsyncd.conf: every module it defines, keyed by name.
type Config struct {
	Modules map[string]Module
}

// moduleDefaults returns the built-in defaults every module starts from
// before its own [section] parameters (or the file's global parameters,
// set before any module header) are applied on top.
func moduleDefaults() Module {
	return Module{ReadOnly: true, List: true}
}

// ParseConfig parses rsyncd.conf content from r.
//
// Syntax, matching the real format (verified against the actual
// rsyncd.conf(5) man page, not assumed): global parameters may appear
// before any module header and become that module's starting defaults;
// a module begins with "[name]" and continues until the next module or
// EOF; "#"-prefixed lines are comments; blank lines are ignored; a line
// ending in "\" continues on the next line; only the first "=" in a
// "name = value" line is significant, and whitespace around it is
// trimmed.
//
// A parameter name this package doesn't implement (real rsyncd.conf has
// dozens - "uid", "hosts allow", "log file", "timeout", and more) is
// accepted and silently ignored, not an error: rejecting a real,
// syntactically valid config file just because it uses an option this
// package hasn't implemented yet would be worse than ignoring that one
// line. A line that isn't valid "name = value" or "[section]" syntax at
// all, or a recognized parameter with a malformed value (e.g.
// "max connections = abc"), is a hard parse error - that distinction is
// deliberate, not an oversight.
func ParseConfig(r io.Reader) (*Config, error) {
	lines, err := readLogicalLines(r)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{Modules: make(map[string]Module)}
	globalDefaults := moduleDefaults()
	var current *Module // nil while still in the global (pre-module) section

	// flush commits the module currently being accumulated into cfg.Modules.
	// Called when a new [section] header starts (to commit the previous
	// one) and once more after the loop ends (to commit the last one) -
	// current accumulates directly rather than round-tripping through the
	// map on every parameter line.
	flush := func() {
		if current != nil {
			cfg.Modules[current.Name] = *current
		}
	}

	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			name, err := parseSectionHeader(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			// flush() must run before the duplicate check below: the
			// module currently being accumulated in `current` hasn't
			// been written into cfg.Modules yet (that's the whole point
			// of deferring it), so checking for a duplicate against the
			// map first would miss "[mod] ... [mod]" - the very case
			// this check exists for - since the first "mod" wouldn't be
			// in the map yet when the second one is seen.
			flush()
			if _, exists := cfg.Modules[name]; exists {
				return nil, fmt.Errorf("line %d: module %q defined more than once", lineNo, name)
			}
			m := globalDefaults
			m.Name = name
			current = &m
			continue
		}

		key, value, err := parseParamLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}

		if current == nil {
			if err := applyParam(&globalDefaults, key, value); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		if err := applyParam(current, key, value); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	flush()

	for name, m := range cfg.Modules {
		if m.Path == "" {
			return nil, fmt.Errorf("module %q has no \"path\" set", name)
		}
	}

	return cfg, nil
}

// readLogicalLines reads r and joins any line ending in "\" with the line
// that follows it, so the rest of the parser never has to think about
// continuations.
func readLogicalLines(r io.Reader) ([]string, error) {
	var lines []string
	var current strings.Builder

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		if strings.HasSuffix(text, "\\") {
			current.WriteString(strings.TrimSuffix(text, "\\"))
			continue
		}
		current.WriteString(text)
		lines = append(lines, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func parseSectionHeader(line string) (string, error) {
	if !strings.HasSuffix(line, "]") {
		return "", fmt.Errorf("malformed section header %q: missing closing \"]\"", line)
	}
	name := strings.TrimSpace(line[1 : len(line)-1])
	if name == "" {
		return "", fmt.Errorf("malformed section header %q: empty module name", line)
	}
	return name, nil
}

func parseParamLine(line string) (key, value string, err error) {
	i := strings.IndexByte(line, '=')
	if i == -1 {
		return "", "", fmt.Errorf("malformed line %q: expected \"name = value\"", line)
	}
	key = strings.ToLower(strings.TrimSpace(line[:i]))
	value = strings.TrimSpace(line[i+1:])
	if key == "" {
		return "", "", fmt.Errorf("malformed line %q: empty parameter name", line)
	}
	return key, value, nil
}

// applyParam interprets one recognized parameter into m. An unrecognized
// key is silently accepted (see ParseConfig's doc comment for why); a
// recognized key with a malformed value is a hard error.
func applyParam(m *Module, key, value string) error {
	switch key {
	case "path":
		m.Path = value
	case "comment":
		m.Comment = value
	case "read only":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("\"read only\": %w", err)
		}
		m.ReadOnly = b
	case "list":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("\"list\": %w", err)
		}
		m.List = b
	case "exclude":
		m.Exclude = strings.Fields(value)
	case "auth users":
		m.AuthUsers = splitCommaOrSpace(value)
	case "secrets file":
		m.SecretsFile = value
	case "max connections":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("\"max connections\": %q is not a valid integer", value)
		}
		m.MaxConnections = n
	}
	return nil
}

// parseBool matches real rsyncd.conf's own accepted boolean spellings:
// "true"/"false", "yes"/"no", and "1"/"0", case-insensitively.
func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true", "yes", "1":
		return true, nil
	case "false", "no", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%q is not a valid boolean (want true/false, yes/no, or 1/0)", value)
	}
}

// splitCommaOrSpace splits on commas and/or whitespace, matching
// "auth users"'s documented "comma and/or space-separated list" syntax.
func splitCommaOrSpace(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	return fields
}
