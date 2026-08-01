package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// Direction is which way file data moves in a module session, sent by the
// client as a single line immediately after authentication succeeds (or
// immediately after ServeGreeting, for a module that needs none).
type Direction string

const (
	// DirectionGet means the client downloads from the module - the
	// daemon runs pipeline.Sender against the module's Path, the client
	// runs pipeline.Receiver against its local destination.
	DirectionGet Direction = "get"
	// DirectionPut means the client uploads to the module - the daemon
	// runs pipeline.Receiver against the module's Path, the client runs
	// pipeline.Sender against its local source. Refused outright for a
	// read-only module, before any transfer code runs.
	DirectionPut Direction = "put"
)

// ErrReadOnly is returned by ServeModule when a client requests
// DirectionPut against a module with "read only = true" (the default).
var ErrReadOnly = errors.New("module is read only")

// transferDone is sent by whichever side ran pipeline.Receiver once it
// returns, and waited for by whichever side ran pipeline.Sender before
// that side's own call returns. This matters for real callers, not just
// tests: pipeline.Sender returning only means the last delta has been
// written to the connection, not that the receiver has finished applying
// it and closing its destination files - closing the TCP connection right
// after Sender returns (as --daemon's per-connection goroutine does) can
// otherwise race the receiver's still-in-flight disk writes.
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

// moduleRules compiles m.Exclude into sync.Rule via the same
// CompileRules/Included machinery internal/cli uses for --exclude,
// rather than a second, independently-trusted pattern matcher for module
// access control.
//
// This is applied on the DirectionGet (download) path only: that's where
// the daemon itself walks m.Path and can filter what it sends, matching
// exclude's most common real-world use (restricting what a mirror
// serves). pipeline.Receiver has no per-entry filtering hook, so a
// DirectionPut upload to a non-read-only module is not filtered against
// this list - a deliberate, documented scope boundary, not an oversight;
// see the README's daemon section.
func moduleRules(m Module) ([]sync.Rule, error) {
	raw := make([]sync.RawRule, len(m.Exclude))
	for i, pattern := range m.Exclude {
		raw[i] = sync.RawRule{Kind: sync.RuleExclude, Pattern: pattern}
	}
	return sync.CompileRules(raw)
}

// moduleAttrOptions is what a daemon module preserves - on an upload
// (applied by the receiving Receiver) and, via its HardLinks field, on a
// download too (consulted by the sending Sender). Fixed, rather than
// client-controlled, since the module (not the client) owns what its own
// rsyncd.conf-configured directory considers worth preserving - a client
// can't currently ask a module to preserve more or less than this.
func moduleAttrOptions() sync.AttrOptions {
	return sync.AttrOptions{Perms: true, Times: true, Owner: true, Group: true, Links: true, HardLinks: true}
}

// dryRunToken is appended as a second, space-separated field on the
// direction line - "put --dry-run" instead of just "put" - the one
// piece of protocol extension a client-requested dry-run needs for a
// DirectionPut: the connection's Receiver runs on this (server) side, so
// there is no other way for the client to communicate "plan this, but
// don't actually write it" without adding a wire signal for it. A
// DirectionGet needs nothing equivalent - that side's Receiver runs
// locally on the client, entirely its own decision to make (see
// DialModule).
const dryRunToken = "--dry-run"

// ServeModule runs one authenticated client's session against the
// already-selected module m: reads the client's requested Direction,
// enforces read-only, acknowledges with "@RSYNCD: OK" or refuses with an
// "@ERROR" line, and only then hands the connection to pipeline.Sender or
// pipeline.Receiver. That ack is required, not cosmetic: without it, a
// refused DirectionPut would leave the client's pipeline.Sender blocked
// writing a file list nobody is left to read - the same deadlock shape
// the greeting phase's unknown-module case has, avoided here the same
// way, by never letting either side commit to the transfer until the
// other has confirmed it's ready.
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
		if err := pipeline.Sender(c, m.Path, sync.WalkOptions{Recursive: true}, rules, moduleAttrOptions().HardLinks); err != nil {
			return err
		}
		return waitForTransferDone(c)
	case DirectionPut:
		// No Itemize/Verbose/Progress/Stats here: the daemon protocol has
		// no channel back to the client for reporting text once the
		// handshake ends (unlike SSH's separate stderr stream) - see the
		// README's Progress and Stats section for the full explanation of
		// this disclosed gap, which SC-11 already established for
		// itemize/verbose and SC-10 confirms applies identically to
		// progress/stats, not a new limitation. DryRun's actual no-write
		// guarantee, in contrast, needs no channel at all beyond the
		// dryRunToken above: it's purely a local decision this call makes
		// about its own writes.
		ropts := pipeline.ReceiverOptions{DryRun: dryRun}
		if err := pipeline.Receiver(c, m.Path, moduleAttrOptions(), ropts); err != nil {
			return err
		}
		return writeLine(c.w, transferDone)
	default:
		panic("unreachable: direction validated above")
	}
}

// DialModule runs the client side of a module session: sends the
// requested direction, waits for the server's ack (see ServeModule) and
// fails without touching the pipeline at all if it's an @ERROR instead,
// then runs the matching pipeline side against localPath. rules and
// walkOpts govern what the client's own Sender walk includes on a
// DirectionPut; attrOpts governs what the client's own Receiver preserves
// on a DirectionGet, and its HardLinks field also governs whether a
// DirectionPut's Sender detects hard links at all - the same field
// serves both directions since it's one "does the client want hard links
// preserved" decision either way.
//
// ropts matters differently depending on direction: for DirectionGet,
// the client's own Receiver runs locally, so ropts (DryRun, Itemize,
// Verbose, Output) all apply directly, exactly like a local sync. For
// DirectionPut, the client runs Sender, which has no dry-run concept at
// all (see pipeline.Sender's own doc comment) - only ropts.DryRun is
// used here, sent as an extra token on the direction line (dryRunToken)
// so the *server's* Receiver, which is the side that actually decides
// whether to write, knows to skip its writes. Itemize/Verbose are
// silently unusable for a DirectionPut, since the daemon protocol has no
// channel to carry that reporting text back from the server - see
// ServeModule's own comment on the same limitation.
func DialModule(c *conn, direction Direction, localPath string, rules []sync.Rule, walkOpts sync.WalkOptions, attrOpts sync.AttrOptions, ropts pipeline.ReceiverOptions) error {
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
		if err := pipeline.Sender(c, localPath, walkOpts, rules, attrOpts.HardLinks); err != nil {
			return err
		}
		return waitForTransferDone(c)
	default:
		return fmt.Errorf("invalid direction %q", direction)
	}
}
