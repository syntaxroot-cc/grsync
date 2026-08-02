package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// Direction is which way file data moves in a module session, sent by the
// client as a single line immediately after authentication succeeds.
type Direction string

const (
	// DirectionGet means the client downloads from the module.
	DirectionGet Direction = "get"
	// DirectionPut means the client uploads to the module. Refused outright
	// for a read-only module, before any transfer code runs.
	DirectionPut Direction = "put"
)

// ErrReadOnly is returned by ServeModule when a client requests
// DirectionPut against a module with "read only = true" (the default).
var ErrReadOnly = errors.New("module is read only")

// transferDone is sent by whichever side ran pipeline.Receiver once it
// returns, and waited for by whichever side ran pipeline.Sender before
// returning itself. Without it, closing the TCP connection right after
// Sender returns could race the receiver's still-in-flight disk writes,
// since Sender returning only means the last delta has been written, not
// that the receiver has finished applying it.
const transferDone = "@GRSYNC: DONE"

func waitForTransferDone(c *conn) error {
	line, err := readLine(c.r)
	if err != nil {
		return fmt.Errorf("waiting for transfer completion: %w", err)
	}
	if line != transferDone {
		return fmt.Errorf("unexpected line waiting for transfer completion: %q", line)
	}
	return nil
}

// moduleRules compiles m.Exclude into sync.Rule via the same CompileRules
// machinery internal/cli uses for --exclude. Only applied on the
// DirectionGet (download) path, where the daemon walks m.Path itself;
// pipeline.Receiver has no per-entry filtering hook, so a DirectionPut
// upload is not filtered against this list (see the README's daemon
// section).
func moduleRules(m Module) ([]sync.Rule, error) {
	raw := make([]sync.RawRule, len(m.Exclude))
	for i, pattern := range m.Exclude {
		raw[i] = sync.RawRule{Kind: sync.RuleExclude, Pattern: pattern}
	}
	return sync.CompileRules(raw)
}

// moduleAttrOptions is what a daemon module preserves: fixed rather than
// client-controlled, since the module owns what its rsyncd.conf-configured
// directory considers worth preserving.
func moduleAttrOptions() sync.AttrOptions {
	return sync.AttrOptions{Perms: true, Times: true, Owner: true, Group: true, Links: true, HardLinks: true}
}

// dryRunToken is appended as a second field on the direction line ("put
// --dry-run") - the wire signal a DirectionPut needs since its Receiver
// runs on the server side, with no other way for the client to say "plan
// this, don't write it."
const dryRunToken = "--dry-run"

// ServeModule runs one authenticated client's session against the
// already-selected module m: reads the requested Direction, enforces
// read-only, acknowledges with "@RSYNCD: OK" or refuses with "@ERROR", and
// only then hands the connection to pipeline.Sender or Receiver. The ack
// is required: without it, a refused DirectionPut would leave the
// client's Sender blocked writing a file list nobody is left to read.
func ServeModule(c *conn, m Module) error {
	line, err := readLine(c.r)
	if err != nil {
		return fmt.Errorf("reading direction: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		_ = writeLine(c.w, fmt.Sprintf("@ERROR: invalid direction %q", line))
		return fmt.Errorf("invalid direction %q", line)
	}
	direction := Direction(fields[0])
	dryRun := direction == DirectionPut && len(fields) > 1 && fields[1] == dryRunToken

	if direction == DirectionPut && m.ReadOnly {
		_ = writeLine(c.w, "@ERROR: module is read only")
		return fmt.Errorf("%w: module %q", ErrReadOnly, m.Name)
	}
	if direction != DirectionGet && direction != DirectionPut {
		_ = writeLine(c.w, fmt.Sprintf("@ERROR: invalid direction %q", line))
		return fmt.Errorf("invalid direction %q", line)
	}

	if err := writeLine(c.w, "@RSYNCD: OK"); err != nil {
		return fmt.Errorf("writing OK: %w", err)
	}

	switch direction {
	case DirectionGet:
		rules, err := moduleRules(m)
		if err != nil {
			return fmt.Errorf("compiling module %q exclude rules: %w", m.Name, err)
		}
		// pipeline.CompressOptions{} (disabled): module downloads have no
		// CLI-facing --compress wiring yet (see the README's Compression
		// section).
		if err := pipeline.Sender(c, m.Path, sync.WalkOptions{Recursive: true}, rules, moduleAttrOptions().HardLinks, pipeline.CompressOptions{}); err != nil {
			return err
		}
		return waitForTransferDone(c)
	case DirectionPut:
		// No Itemize/Verbose/Progress/Stats: the daemon protocol has no
		// channel back to the client for reporting text once the handshake
		// ends (see the README's Progress and Stats section). DryRun needs
		// no channel, since it's a local decision driven by dryRunToken.
		ropts := pipeline.ReceiverOptions{DryRun: dryRun}
		if err := pipeline.Receiver(c, m.Path, moduleAttrOptions(), ropts); err != nil {
			return err
		}
		return writeLine(c.w, transferDone)
	default:
		panic("unreachable: direction validated above")
	}
}

// DialModule runs the client side of a module session: sends the requested
// direction, waits for the server's "@RSYNCD: OK" ack, then runs the
// matching pipeline side against localPath. For DirectionPut, ropts.DryRun
// is sent via dryRunToken so the server's Receiver (which decides whether
// to write) knows to skip writes; Itemize/Verbose/copts don't apply in
// that direction, for the same reasons as ServeModule.
func DialModule(c *conn, direction Direction, localPath string, rules []sync.Rule, walkOpts sync.WalkOptions, attrOpts sync.AttrOptions, ropts pipeline.ReceiverOptions, copts pipeline.CompressOptions) error {
	directionLine := string(direction)
	if direction == DirectionPut && ropts.DryRun {
		directionLine += " " + dryRunToken
	}
	if err := writeLine(c.w, directionLine); err != nil {
		return fmt.Errorf("sending direction: %w", err)
	}

	ack, err := readLine(c.r)
	if err != nil {
		return fmt.Errorf("reading server ack: %w", err)
	}
	if ack != "@RSYNCD: OK" {
		return fmt.Errorf("server refused module session: %s", ack)
	}

	switch direction {
	case DirectionGet:
		if err := pipeline.Receiver(c, localPath, attrOpts, ropts); err != nil {
			return err
		}
		return writeLine(c.w, transferDone)
	case DirectionPut:
		if err := pipeline.Sender(c, localPath, walkOpts, rules, attrOpts.HardLinks, copts); err != nil {
			return err
		}
		return waitForTransferDone(c)
	default:
		return fmt.Errorf("invalid direction %q", direction)
	}
}
