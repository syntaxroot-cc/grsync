package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
	"github.com/syntaxroot-cc/grsync/internal/pipeline"
	"github.com/syntaxroot-cc/grsync/internal/sync"
	"github.com/syntaxroot-cc/grsync/internal/transport"
)

// pipeReadWriter joins separate Reader/Writer halves into a single io.ReadWriter.
type pipeReadWriter struct {
	io.Reader
	io.Writer
}

// effectiveWalkOptions computes sync.WalkOptions from opts; --archive implies --recursive.
func effectiveWalkOptions(opts *options) sync.WalkOptions {
	return sync.WalkOptions{
		Recursive: opts.archive || opts.recursive,
		Dirs:      opts.dirs,
	}
}

// effectiveAttrOptions computes sync.AttrOptions from opts; --archive implies
// perms/times/owner/group/links but not hard links, matching real rsync's -a
// (-rlptgoD, which does not include -H).
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

// effectiveReceiverOptions computes pipeline.ReceiverOptions from opts.
func effectiveReceiverOptions(opts *options, output io.Writer) pipeline.ReceiverOptions {
	return pipeline.ReceiverOptions{
		DryRun:       opts.dryRun,
		Itemize:      opts.itemize,
		Verbose:      opts.verbose,
		Progress:     opts.progress,
		Stats:        opts.stats,
		Output:       output,
		Partial:      opts.partial,
		PartialDir:   opts.partialDir,
		Append:       opts.appendMode,
		AppendVerify: opts.appendVerify,
	}
}

// effectiveCompressOptions computes pipeline.CompressOptions from opts,
// matching real rsync's rule that --compress is implied whenever
// --compress-level is given a non-zero level.
//
// cmd.Flags().Changed (not opts.compressLevel's zero value) is what
// distinguishes "--compress-level was never given" from "--compress-level=0
// was given explicitly," since pflag's IntVar can't tell those apart by
// value alone.
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
// FilterRuleType's string values are chosen to match sync.RuleKind's
// exactly, so this is a direct cast rather than a mapping table.
func toSyncRawRules(filterRules []FilterRule) []sync.RawRule {
	raw := make([]sync.RawRule, len(filterRules))
	for i, r := range filterRules {
		raw[i] = sync.RawRule{Kind: sync.RuleKind(r.Type), Pattern: r.Pattern}
	}
	return raw
}

// runSync is the real sync entry point: for each source, syncs it into
// destination, in-process for a local destination, over SSH for a
// user@host:path one, or over TCP for an rsync:// daemon module. Pulling
// from a remote source is not yet supported, only pushing to one.
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

	network, err := tcpNetwork(opts.ipv4, opts.ipv6)
	if err != nil {
		return err
	}
	localAddr, err := resolveLocalAddr(network, opts.address)
	if err != nil {
		return err
	}

	if opts.appendMode && opts.appendVerify {
		return fmt.Errorf("--append and --append-verify are mutually exclusive")
	}

	// root.go's Args validator already rejects --write-batch and
	// --read-batch together, so only --write-batch needs handling here.
	//
	// Real rsync silently disables --write-batch under --dry-run
	// (options.c: "else if (dry_run) write_batch = 0"), since a dry run
	// never computes a real delta to capture. grsync matches that
	// behavior but prints an explicit note instead of staying silent.
	writingBatch := opts.writeBatch != ""
	if writingBatch && opts.dryRun {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: --write-batch has no effect combined with --dry-run "+
			"(matching real rsync's own behavior - a dry run never computes a real delta to capture) - no batch file will be written")
		writingBatch = false
	}
	// A batch file holds exactly one Sender/Receiver session's file-list
	// frame; runReadBatch's single recvFileList call can't replay more
	// than one, so --write-batch requires exactly one source.
	if writingBatch && len(sources) != 1 {
		return fmt.Errorf("--write-batch requires exactly one source, got %d", len(sources))
	}

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

	if writingBatch && isRsyncDaemon {
		// A daemon connection's Sender writes share the same net.Conn the
		// handshake used (daemon.DialClient), so tapping only the
		// batch-worthy frames would need daemon-package changes, not just
		// a wrapped io.Writer here.
		return fmt.Errorf("--write-batch is not supported for an rsync:// daemon destination")
	}

	// batchFile is closed and cleared to nil on the success path below;
	// if the sync fails partway, this deferred cleanup also removes the
	// file, rather than leaving a truncated one that looks like a real
	// deliverable but would only ever fail (or silently under-apply) on
	// a later --read-batch.
	var batchFile *os.File
	if writingBatch {
		f, err := os.Create(opts.writeBatch)
		if err != nil {
			return fmt.Errorf("creating batch file %q: %w", opts.writeBatch, err)
		}
		batchFile = f
		defer func() {
			if batchFile != nil {
				_ = batchFile.Close()
				_ = os.Remove(opts.writeBatch)
			}
		}()
	}

	// Resolved once, outside the per-source loop, so a multi-source sync
	// against the same daemon destination only prompts for (or reads) a
	// password once.
	var password daemon.PasswordFunc
	if isRsyncDaemon {
		password = resolvePassword(opts.passwordFile, cmd.InOrStdin())
	}

	ropts := effectiveReceiverOptions(opts, cmd.OutOrStdout())
	if isRsyncDaemon && ropts.Reporting() {
		// Once the module handshake ends, the daemon connection is pure
		// binary wire protocol with no channel for reporting text back to
		// the client, unlike SSH's separate stderr stream. --dry-run's
		// no-write guarantee still applies; only the reporting text is
		// unavailable.
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: --itemize-changes/--verbose/--progress/--stats output is not available "+
			"for an rsync:// daemon destination (the daemon protocol has no channel for it); --dry-run's no-write guarantee still applies")
	}
	if isRsyncDaemon && (ropts.KeepPartial() || ropts.AppendMode()) {
		// The module's Receiver runs on the server (daemon.ServeModule),
		// and syncToRsyncDaemon only forwards DryRun to it, not these -
		// extending the wire protocol to carry them is separate work.
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: --partial/--partial-dir/--append/--append-verify are not available "+
			"for an rsync:// daemon destination (the module's receiver runs on the server, which has no way to learn these were requested)")
	}

	var batchWriter io.Writer
	if batchFile != nil {
		batchWriter = batchFile
	}

	for _, src := range sources {
		switch {
		case isRsyncDaemon:
			if err := syncToRsyncDaemon(src, rsyncURL, password, walkOpts, rules, attrOpts.HardLinks, opts.dryRun, copts, network, localAddr); err != nil {
				return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
			}
		case isRemote:
			if err := syncToRemote(opts.rsh, src, remote, walkOpts, rules, attrOpts.HardLinks, ropts, copts, opts.ipv4, opts.ipv6, batchWriter); err != nil {
				return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
			}
		default:
			if err := syncLocal(src, destination, walkOpts, rules, attrOpts, ropts, copts, batchWriter); err != nil {
				return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
			}
		}
	}

	if batchFile != nil {
		closeErr := batchFile.Close()
		batchFile = nil // makes the deferred cleanup above a no-op
		if closeErr != nil {
			return fmt.Errorf("closing batch file %q: %w", opts.writeBatch, closeErr)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote batch file %q (grsync's own format - see the README's Batch Mode section)\n", opts.writeBatch)
	}

	verb := "synced"
	if opts.dryRun {
		verb = "would sync"
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %d source(s) to %s\n", verb, len(sources), destination)
	return err
}

// syncLocal runs the sender and receiver in-process, connected by a pair of
// io.Pipes, so a local sync exercises the same pipeline.Sender/Receiver
// functions a remote sync uses.
//
// batchWriter, when non-nil (--write-batch), receives a byte-for-byte copy
// of everything Sender writes to the receiver.
func syncLocal(src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions, ropts pipeline.ReceiverOptions, copts pipeline.CompressOptions, batchWriter io.Writer) error {
	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	senderWriter := io.Writer(senderWritesToReceiver)
	if batchWriter != nil {
		senderWriter = io.MultiWriter(senderWritesToReceiver, batchWriter)
	}

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWriter}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() {
		err := pipeline.Sender(sender, src, walkOpts, rules, attrOpts.HardLinks, copts)
		// Without closing both pipe halves here, a failure on either side
		// partway through the protocol can leave the other blocked on a
		// pipe read/write forever, since neither pipeline.Sender nor
		// Receiver ever closes them itself (found by go test -race
		// hanging on a receiver-failure test for its full timeout).
		_ = senderWritesToReceiver.CloseWithError(err)
		_ = senderReadsFromReceiver.Close()
		senderErrCh <- err
	}()

	receiverErr := pipeline.Receiver(receiver, dest, attrOpts, ropts)
	_ = receiverWritesToSender.CloseWithError(receiverErr)
	_ = receiverReadsFromSender.Close()

	senderErr := <-senderErrCh

	if receiverErr != nil {
		return receiverErr
	}
	return senderErr
}

// syncToRemote spawns `grsync --server DEST` on the remote host via SSH (or
// whatever --rsh overrides it to), performs the handshake, then runs the
// sender side of the pipeline against that connection.
//
// ropts' reporting/partial/append fields are passed as extra flags on the
// remote command line rather than over a new wire message; the remote
// --server process parses them normally and its own Receiver call decides
// everything locally. --address has no equivalent here: it's out of scope
// for the ssh/rsh transport, matching real rsync.
func syncToRemote(rsh, src string, remote transport.RemotePath, walkOpts sync.WalkOptions, rules []sync.Rule, hardLinks bool, ropts pipeline.ReceiverOptions, copts pipeline.CompressOptions, ipv4, ipv6 bool, batchWriter io.Writer) error {
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
	if ropts.PartialDir != "" {
		// --partial-dir implies --partial, so there's no need to also
		// forward a bare --partial alongside it.
		remoteArgs = append(remoteArgs, "--partial-dir="+ropts.PartialDir)
	} else if ropts.Partial {
		remoteArgs = append(remoteArgs, "--partial")
	}
	if ropts.Append {
		remoteArgs = append(remoteArgs, "--append")
	}
	if ropts.AppendVerify {
		remoteArgs = append(remoteArgs, "--append-verify")
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

	var senderConn io.ReadWriter = session
	if batchWriter != nil {
		senderConn = pipeReadWriter{Reader: session, Writer: io.MultiWriter(session, batchWriter)}
	}

	sendErr := pipeline.Sender(senderConn, src, walkOpts, rules, hardLinks, copts)
	closeErr := session.Close()

	if sendErr != nil {
		return sendErr
	}
	return closeErr
}

// runServer implements --server mode: performs the handshake, then runs the
// receiver side of the pipeline against dest, reading/writing the command's
// own stdin/stdout.
//
// Itemize/verbose output goes to cmd.ErrOrStderr(), never stdout: stdout is
// the framed wire protocol itself, so writing human-readable text there
// would corrupt it.
func runServer(cmd *cobra.Command, dest string, opts *options) error {
	stdin, stdout := cmd.InOrStdin(), cmd.OutOrStdout()

	if err := transport.ServeHandshake(stdin, stdout); err != nil {
		return err
	}

	rw := pipeReadWriter{Reader: stdin, Writer: stdout}
	ropts := effectiveReceiverOptions(opts, cmd.ErrOrStderr())
	return pipeline.Receiver(rw, dest, effectiveAttrOptions(opts), ropts)
}

// runReadBatch implements --read-batch=FILE: applies the file list and
// per-file deltas previously captured by --write-batch directly to dest,
// with no source argument or live sender connection.
//
// This reuses Receiver unchanged: its signature writes go to io.Discard
// (there's no live sender to read them), and its delta reads come from
// FILE instead of a socket. A malformed or foreign FILE fails with the
// same frame-decode errors a live sync already relies on to reject a
// corrupted connection - there's no separate batch-format validation
// because there's no separate batch format at all.
func runReadBatch(cmd *cobra.Command, dest string, opts *options) error {
	var r io.Reader
	if opts.readBatch == "-" {
		r = cmd.InOrStdin()
	} else {
		f, err := os.Open(opts.readBatch)
		if err != nil {
			return fmt.Errorf("opening batch file %q: %w", opts.readBatch, err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}

	rw := pipeReadWriter{Reader: r, Writer: io.Discard}
	attrOpts := effectiveAttrOptions(opts)
	ropts := effectiveReceiverOptions(opts, cmd.OutOrStdout())

	if err := pipeline.Receiver(rw, dest, attrOpts, ropts); err != nil {
		return fmt.Errorf("applying batch %q to %q: %w", opts.readBatch, dest, err)
	}

	verb := "applied"
	if opts.dryRun {
		verb = "would apply"
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s batch %q to %s\n", verb, opts.readBatch, dest)
	return err
}
