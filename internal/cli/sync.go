package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
	"github.com/syntaxroot-cc/grsync/internal/transport"
)

// pipeReadWriter joins two separate io.Reader/io.Writer halves into a
// single io.ReadWriter, needed anywhere a connection is represented as
// two directional pipe ends (the local-to-local case below) or as a
// command's separate stdin/stdout (the --server case).
type pipeReadWriter struct {
	io.Reader
	io.Writer
}

// effectiveWalkOptions computes sync.WalkOptions from opts: --archive
// implies --recursive, matching real rsync's -a (-rlptgoD).
func effectiveWalkOptions(opts *options) sync.WalkOptions {
	return sync.WalkOptions{
		Recursive: opts.archive || opts.recursive,
		Dirs:      opts.dirs,
	}
}

// effectiveAttrOptions computes sync.AttrOptions from opts: --archive
// implies perms/times/owner/group/links, matching real rsync's -a
// (-rlptgoD, minus the r which effectiveWalkOptions handles, and minus
// devices/specials - see the README's note on why device files are
// deferred rather than wired up here). HardLinks is deliberately NOT
// included in that implication: real rsync's own -a does not imply -H
// either (-a is exactly -rlptgoD, no H), so --archive alone must not
// turn hard-link detection on.
func effectiveAttrOptions(opts *options) sync.AttrOptions {
	return sync.AttrOptions{
		Perms:     opts.archive || opts.perms,
		Times:     opts.archive || opts.times,
		Owner:     opts.archive || opts.owner,
		Group:     opts.archive || opts.group,
		Links:     opts.archive || opts.links,
		HardLinks: opts.hardLinks,
	}
}

// effectiveReceiverOptions computes pipeline.ReceiverOptions from opts:
// --dry-run, --itemize-changes, --verbose, --progress, and --stats, all
// reported to output.
func effectiveReceiverOptions(opts *options, output io.Writer) pipeline.ReceiverOptions {
	return pipeline.ReceiverOptions{
		DryRun:   opts.dryRun,
		Itemize:  opts.itemize,
		Verbose:  opts.verbose,
		Progress: opts.progress,
		Stats:    opts.stats,
		Output:   output,
	}
}

// effectiveCompressOptions computes pipeline.CompressOptions from opts,
// mirroring real rsync's own --compress-level implication rule ("The
// --compress option is implied as long as the level chosen is not a
// 'don't compress' level" - rsync.1): compression is enabled whenever
// either -z or --compress-level was given at all, at whatever level
// --compress-level requested (clamped) or the real-rsync-verified
// default of 6 if only -z was given, UNLESS that level clamps to 0
// ("off"), which disables compression outright even if -z was also
// given - matching real rsync's own documented "--zl=0 turns compression
// off" behavior exactly, including its override of -z.
//
// cmd.Flags().Changed, not opts.compressLevel's zero value, is what
// distinguishes "--compress-level was never given" from "--compress-level=0
// was given explicitly" - those two cases mean different things (default
// level 6 vs. explicitly off) and pflag's own IntVar can't tell them
// apart by value alone, since 0 is also its unset zero value.
func effectiveCompressOptions(cmd *cobra.Command, opts *options) pipeline.CompressOptions {
	levelGiven := cmd.Flags().Changed("compress-level")
	if !opts.compress && !levelGiven {
		return pipeline.CompressOptions{}
	}

	level := pipeline.DefaultCompressLevel
	if levelGiven {
		level = pipeline.ClampCompressLevel(opts.compressLevel)
	}
	if level == 0 {
		return pipeline.CompressOptions{}
	}

	suffixes := pipeline.DefaultSkipCompressSuffixes
	if cmd.Flags().Changed("skip-compress") {
		suffixes = pipeline.ParseSkipCompressList(opts.skipCompress)
	}
	return pipeline.CompressOptions{Enabled: true, Level: level, SkipSuffixes: suffixes}
}

// toSyncRawRules converts the CLI's FilterRule list to sync.RawRule.
// FilterRuleType's string values were chosen to exactly match
// sync.RuleKind's ("include", "exclude", "filter", "exclude-from",
// "include-from"), so this is a direct conversion rather than a mapping
// table - if the two ever drift apart, this line stops compiling as a
// straight cast, which is a more useful failure mode than a silent
// mismatch would be.
func toSyncRawRules(filterRules []FilterRule) []sync.RawRule {
	raw := make([]sync.RawRule, len(filterRules))
	for i, r := range filterRules {
		raw[i] = sync.RawRule{Kind: sync.RuleKind(r.Type), Pattern: r.Pattern}
	}
	return raw
}

// runSync is the real sync entry point. For each source, it syncs that
// source into destination - in-process for a local destination, over an
// SSH-spawned connection for a remote user@host:path one, or over a
// plain TCP connection to an rsync:// daemon module. opts.dryRun makes
// this a full trial run - every planning step still happens, nothing is
// actually written - see pipeline.Receiver's own doc comment for exactly
// which calls that skips.
//
// Pulling FROM a remote source (SSH or an rsync:// daemon) is not yet
// supported, only a local source to a local, SSH, or daemon destination -
// this scope is explicitly "push," not pull mode, matching the existing
// SSH-transport restriction rather than introducing a new asymmetry.
func runSync(cmd *cobra.Command, sources []string, destination string, opts *options) error {
	for _, src := range sources {
		if isRsyncURL(src) {
			return fmt.Errorf("pulling from an rsync daemon source (%q) is not yet supported", src)
		}
		if _, ok := transport.ParseRemotePath(src); ok {
			return fmt.Errorf("pulling from a remote source (%q) is not yet supported", src)
		}
	}

	walkOpts := effectiveWalkOptions(opts)
	attrOpts := effectiveAttrOptions(opts)
	copts := effectiveCompressOptions(cmd, opts)
	rules, err := sync.CompileRules(toSyncRawRules(opts.filterRules))
	if err != nil {
		return fmt.Errorf("compiling filter rules: %w", err)
	}

	// Resolved once, up front, regardless of destination type: --ipv4 and
	// --ipv6 conflicting is a flag-level error, not something that should
	// only surface once a particular destination happens to need it (see
	// tcpNetwork's own doc comment for why the combination is rejected
	// outright rather than picking one). network only actually changes
	// behavior for an rsync:// daemon destination (syncToRsyncDaemon,
	// below) and --ipv4/--ipv6's forwarding to ssh (syncToRemote, below) -
	// it's meaningless for a local sync, exactly like real rsync's own
	// scope for these flags.
	network, err := tcpNetwork(opts.ipv4, opts.ipv6)
	if err != nil {
		return err
	}
	localAddr, err := resolveLocalAddr(network, opts.address)
	if err != nil {
		return err
	}

	// isRsyncURL is checked, and rsyncURL parsed, before
	// transport.ParseRemotePath ever looks at destination: an rsync://
	// URL is never valid [user@]host:path syntax (ParseRemotePath itself
	// now refuses anything containing "://"), but checking here first
	// means that's true by construction, not just by the two parsers
	// happening to agree.
	var rsyncURL daemon.URL
	isRsyncDaemon := isRsyncURL(destination)
	if isRsyncDaemon {
		rsyncURL, err = daemon.ParseURL(destination)
		if err != nil {
			return fmt.Errorf("parsing %q: %w", destination, err)
		}
		if rsyncURL.Module == "" {
			return fmt.Errorf("%q has no module - an rsync:// sync destination must be rsync://host/module", destination)
		}
		if rsyncURL.Path != "" {
			return fmt.Errorf("%q targets a sub-path within a module, which is not yet supported - "+
				"the daemon protocol only supports syncing an entire module", destination)
		}
	}
	remote, isRemote := transport.ParseRemotePath(destination)

	// Resolved once, outside the per-source loop below, so a multi-source
	// sync against the same daemon destination only ever prompts for (or
	// reads) a password once - not once per source, even though each
	// source gets its own connection, same as the SSH path already does.
	var password daemon.PasswordFunc
	if isRsyncDaemon {
		password = resolvePassword(opts.passwordFile, cmd.InOrStdin())
	}

	ropts := effectiveReceiverOptions(opts, cmd.OutOrStdout())
	if isRsyncDaemon && ropts.Reporting() {
		// The daemon protocol has no channel for any of this: once the
		// module handshake ends, the connection is pure binary wire
		// protocol (see internal/daemon's own doc comment on where the
		// real-vs-gob boundary sits) with nowhere to carry reporting text
		// back to the client, unlike SSH's genuinely separate stderr
		// stream. This was SC-11's disclosed limitation for
		// itemize/verbose; --progress/--stats have the exact same
		// limitation for the exact same reason, not a new gap - see the
		// README's Progress and Stats section. --dry-run's actual
		// no-write guarantee still fully applies regardless; only the
		// reporting text is unavailable here. Noting this once, up
		// front, rather than silently producing no output and leaving
		// the user to wonder why.
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: --itemize-changes/--verbose/--progress/--stats output is not available "+
			"for an rsync:// daemon destination (the daemon protocol has no channel for it); --dry-run's no-write guarantee still applies")
	}

	for _, src := range sources {
		switch {
		case isRsyncDaemon:
			if err := syncToRsyncDaemon(src, rsyncURL, password, walkOpts, rules, attrOpts.HardLinks, opts.dryRun, copts, network, localAddr); err != nil {
				return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
			}
		case isRemote:
			if err := syncToRemote(opts.rsh, src, remote, walkOpts, rules, attrOpts.HardLinks, ropts, copts, opts.ipv4, opts.ipv6); err != nil {
				return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
			}
		default:
			if err := syncLocal(src, destination, walkOpts, rules, attrOpts, ropts, copts); err != nil {
				return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
			}
		}
	}

	verb := "synced"
	if opts.dryRun {
		verb = "would sync"
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %d source(s) to %s\n", verb, len(sources), destination)
	return err
}

// syncLocal runs the sender and receiver in-process, connected by a pair
// of io.Pipes, rather than a separate code path for the local case: this
// way, the exact same pipeline.Sender/pipeline.Receiver functions that
// carry out a remote sync are what a local sync exercises too, instead of
// a second, independently-trusted implementation of the same logic.
func syncLocal(src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions, ropts pipeline.ReceiverOptions, copts pipeline.CompressOptions) error {
	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- pipeline.Sender(sender, src, walkOpts, rules, attrOpts.HardLinks, copts) }()

	receiverErr := pipeline.Receiver(receiver, dest, attrOpts, ropts)
	senderErr := <-senderErrCh

	if receiverErr != nil {
		return receiverErr
	}
	return senderErr
}

// syncToRemote spawns `grsync --server DEST` on the remote host via SSH
// (or whatever --rsh overrides it to), performs the handshake, then runs
// the sender side of the pipeline against that connection.
//
// ropts.DryRun/Itemize/Verbose/Progress/Stats are passed as extra flags
// on that remote command line (e.g. "grsync --server --dry-run -i
// DEST"), not over any new wire message: the remote --server process
// parses them the normal way, via its own real CLI flag handling (see
// runServer), and the receiving side's decision is made entirely on the
// remote side, exactly where pipeline.Receiver actually runs for this
// transport - there is nothing for the local, sending side to decide
// here at all. This is the same mechanism SC-11 established for
// DryRun/Itemize/Verbose; Progress/Stats just reuse it rather than
// inventing a second one.
//
// copts needs none of that: --compress/-z is a Sender-side decision (see
// pipeline.CompressOptions' own doc comment), and Sender runs right here,
// locally, for this transport - there is nothing for the remote
// --server process to be told via argv at all. The remote Receiver just
// decompresses whatever each deltaMessage's own Compressed marker says,
// exactly like every other transport.
//
// ipv4/ipv6 are SC-14's own contribution, and land somewhere different
// again: grsync never dials this connection itself at all (ssh, or
// whatever --rsh overrides it to, does), so there's no net.Dial call
// here to pass a network string to. Instead they're forwarded straight
// through to transport.Dial/BuildRSHCommand, which inserts a real -4/-6
// onto the spawned command's own argv - but only when that command is
// genuinely ssh (see BuildRSHCommand's own doc comment for exactly when,
// verified against real rsync's own identical behavior). --address has
// no equivalent here at all: real rsync's own documented --address scope
// never includes the rsh/ssh transport (see resolveLocalAddr's own doc
// comment), so it isn't threaded through to this function.
func syncToRemote(rsh, src string, remote transport.RemotePath, walkOpts sync.WalkOptions, rules []sync.Rule, hardLinks bool, ropts pipeline.ReceiverOptions, copts pipeline.CompressOptions, ipv4, ipv6 bool) error {
	remoteArgs := []string{"grsync", "--server"}
	if ropts.DryRun {
		remoteArgs = append(remoteArgs, "--dry-run")
	}
	if ropts.Itemize {
		remoteArgs = append(remoteArgs, "--itemize-changes")
	}
	if ropts.Verbose {
		remoteArgs = append(remoteArgs, "--verbose")
	}
	if ropts.Progress {
		remoteArgs = append(remoteArgs, "--progress")
	}
	if ropts.Stats {
		remoteArgs = append(remoteArgs, "--stats")
	}
	remoteArgs = append(remoteArgs, remote.Path)

	session, err := transport.Dial(rsh, remote.User, remote.Host, remoteArgs, ipv4, ipv6)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", remote.Host, err)
	}

	if err := transport.Handshake(session); err != nil {
		_ = session.Close()
		return fmt.Errorf("handshake with %s failed: %w", remote.Host, err)
	}

	sendErr := pipeline.Sender(session, src, walkOpts, rules, hardLinks, copts)
	closeErr := session.Close()

	if sendErr != nil {
		return sendErr
	}
	return closeErr
}

// runServer implements --server mode: perform the handshake, then run the
// receiver side of the pipeline against dest, reading/writing the
// command's own stdin/stdout.
//
// opts here is this process's own locally-parsed flags - for a real
// remote invocation, that means whatever syncToRemote put on the ssh
// command line (see its own doc comment), so --dry-run/-i/-v "just work"
// through the same argv-parsing path every other flag already does, no
// separate propagation mechanism required. Itemize/verbose output goes
// to cmd.ErrOrStderr(), never stdout: stdout here is the framed wire
// protocol itself (see transport.WriteFrame/ReadFrame), so writing
// human-readable text there would corrupt it. In real (non-test) use,
// ErrOrStderr() is this process's actual stderr, which Session (the
// local side's view of this same subprocess) passes through live to the
// local user's terminal - see session.go's own doc comment on why that
// pass-through exists.
func runServer(cmd *cobra.Command, dest string, opts *options) error {
	stdin, stdout := cmd.InOrStdin(), cmd.OutOrStdout()

	if err := transport.ServeHandshake(stdin, stdout); err != nil {
		return err
	}

	rw := pipeReadWriter{Reader: stdin, Writer: stdout}
	ropts := effectiveReceiverOptions(opts, cmd.ErrOrStderr())
	return pipeline.Receiver(rw, dest, effectiveAttrOptions(opts), ropts)
}
