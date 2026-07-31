package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

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
// devices/specials - see the README's note on why hard links and device
// files are deferred rather than wired up here).
func effectiveAttrOptions(opts *options) sync.AttrOptions {
	return sync.AttrOptions{
		Perms: opts.archive || opts.perms,
		Times: opts.archive || opts.times,
		Owner: opts.archive || opts.owner,
		Group: opts.archive || opts.group,
		Links: opts.archive || opts.links,
	}
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

// runSync is the real sync entry point (as opposed to run, the
// flag-echoing placeholder still used for --dry-run). For each source, it
// syncs that source into destination - in-process for a local
// destination, or over an SSH-spawned connection for a remote one.
//
// Pulling FROM a remote source is not yet supported, only a local source
// to a local or remote destination - this ticket's scope is explicitly
// "local-to-local and local-to-remote," not pull mode.
func runSync(cmd *cobra.Command, sources []string, destination string, opts *options) error {
	for _, src := range sources {
		if _, ok := transport.ParseRemotePath(src); ok {
			return fmt.Errorf("pulling from a remote source (%q) is not yet supported", src)
		}
	}

	walkOpts := effectiveWalkOptions(opts)
	attrOpts := effectiveAttrOptions(opts)
	rules, err := sync.CompileRules(toSyncRawRules(opts.filterRules))
	if err != nil {
		return fmt.Errorf("compiling filter rules: %w", err)
	}

	remote, isRemote := transport.ParseRemotePath(destination)

	for _, src := range sources {
		if isRemote {
			if err := syncToRemote(opts.rsh, src, remote, walkOpts, rules); err != nil {
				return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
			}
			continue
		}
		if err := syncLocal(src, destination, walkOpts, rules, attrOpts); err != nil {
			return fmt.Errorf("syncing %q to %q: %w", src, destination, err)
		}
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "synced %d source(s) to %s\n", len(sources), destination)
	return err
}

// syncLocal runs the sender and receiver in-process, connected by a pair
// of io.Pipes, rather than a separate code path for the local case: this
// way, the exact same pipeline.Sender/pipeline.Receiver functions that
// carry out a remote sync are what a local sync exercises too, instead of
// a second, independently-trusted implementation of the same logic.
func syncLocal(src, dest string, walkOpts sync.WalkOptions, rules []sync.Rule, attrOpts sync.AttrOptions) error {
	senderReadsFromReceiver, receiverWritesToSender := io.Pipe()
	receiverReadsFromSender, senderWritesToReceiver := io.Pipe()

	sender := pipeReadWriter{Reader: senderReadsFromReceiver, Writer: senderWritesToReceiver}
	receiver := pipeReadWriter{Reader: receiverReadsFromSender, Writer: receiverWritesToSender}

	senderErrCh := make(chan error, 1)
	go func() { senderErrCh <- pipeline.Sender(sender, src, walkOpts, rules) }()

	receiverErr := pipeline.Receiver(receiver, dest, attrOpts)
	senderErr := <-senderErrCh

	if receiverErr != nil {
		return receiverErr
	}
	return senderErr
}

// syncToRemote spawns `grsync --server DEST` on the remote host via SSH
// (or whatever --rsh overrides it to), performs the handshake, then runs
// the sender side of the pipeline against that connection.
func syncToRemote(rsh, src string, remote transport.RemotePath, walkOpts sync.WalkOptions, rules []sync.Rule) error {
	session, err := transport.Dial(rsh, remote.User, remote.Host, []string{"grsync", "--server", remote.Path})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", remote.Host, err)
	}

	if err := transport.Handshake(session); err != nil {
		_ = session.Close()
		return fmt.Errorf("handshake with %s failed: %w", remote.Host, err)
	}

	sendErr := pipeline.Sender(session, src, walkOpts, rules)
	closeErr := session.Close()

	if sendErr != nil {
		return sendErr
	}
	return closeErr
}

// runServer implements --server mode: perform the handshake, then run the
// receiver side of the pipeline against dest, reading/writing the
// command's own stdin/stdout.
func runServer(cmd *cobra.Command, dest string, opts *options) error {
	stdin, stdout := cmd.InOrStdin(), cmd.OutOrStdout()

	if err := transport.ServeHandshake(stdin, stdout); err != nil {
		return err
	}

	rw := pipeReadWriter{Reader: stdin, Writer: stdout}
	return pipeline.Receiver(rw, dest, effectiveAttrOptions(opts))
}
