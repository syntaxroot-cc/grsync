// Package cli defines the grsync command-line interface: argument
// parsing, flags, and the command tree. Flag/argument parsing lives here;
// the actual sync (internal/pipeline, built on internal/sync and
// internal/transport) is invoked from sync.go, keeping "what the user
// typed" separate from "what actually runs."
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/syntaxroot-cc/grsync/internal/daemon"
)

// FilterRuleType identifies which kind of rule a FilterRule represents.
type FilterRuleType string

// The rule kinds grsync's filter-related flags can produce. Kept as their
// own type (rather than a bare string) so callers can't accidentally pass
// an arbitrary value through.
const (
	FilterRuleInclude     FilterRuleType = "include"
	FilterRuleExclude     FilterRuleType = "exclude"
	FilterRuleFilter      FilterRuleType = "filter"
	FilterRuleExcludeFrom FilterRuleType = "exclude-from"
	FilterRuleIncludeFrom FilterRuleType = "include-from"
)

// FilterRule is a single --include/--exclude/--filter/--exclude-from/
// --include-from occurrence. rsync treats all of these as one ordered,
// first-match-wins rule list rather than independent lists, so grsync
// collects them the same way: Type records which flag produced the rule,
// and relative order across *all* of them is preserved in the order the
// user supplied them. For the two "-from" kinds, Pattern is a file path,
// not a filter pattern - internal/sync reads and expands it.
type FilterRule struct {
	Type    FilterRuleType
	Pattern string
}

// options holds every flag value parsed from the command line. Keeping them
// in one struct (rather than loose variables) makes it straightforward to
// pass a single value into internal/sync once that package exists.
type options struct {
	archive       bool
	verbose       bool
	compress      bool
	compressLevel int
	skipCompress  string
	recursive     bool
	dirs          bool
	dryRun        bool
	delete        bool
	progress      bool
	perms         bool
	times         bool
	owner         bool
	group         bool
	links         bool
	hardLinks     bool
	itemize       bool
	stats         bool
	filterRules   []FilterRule
	rsh           string
	server        bool
	daemon        bool
	config        string
	port          int
	passwordFile  string
	ipv4          bool
	ipv6          bool
	address       string
	partial       bool
	partialDir    string
	appendMode    bool
	appendVerify  bool
	writeBatch    string
	readBatch     string
}

// filterRuleFlag implements pflag.Value. Each of --exclude/--include/
// --filter/--exclude-from/--include-from gets its own instance, fixed to a
// single FilterRuleType, but all of them share the same backing slice - so
// pflag's normal "call Set once per occurrence" behavior naturally builds
// one ordered rule list regardless of which flag name was used at each
// position.
type filterRuleFlag struct {
	ruleType FilterRuleType
	rules    *[]FilterRule
}

func (f *filterRuleFlag) String() string { return "" }

func (f *filterRuleFlag) Set(pattern string) error {
	*f.rules = append(*f.rules, FilterRule{Type: f.ruleType, Pattern: pattern})
	return nil
}

func (f *filterRuleFlag) Type() string {
	switch f.ruleType {
	case FilterRuleFilter:
		return "rule"
	case FilterRuleExcludeFrom, FilterRuleIncludeFrom:
		return "file"
	default:
		return "pattern"
	}
}

// flagGroup is one labeled section of --help's flag listing. Neither
// cobra nor pflag has a native concept of flag groups (cobra's own
// Groups() only applies to subcommands), so grsync builds its own: each
// group gets its own FlagSet, populated in display order (SortFlags =
// false), and all of them are merged into the command's real FlagSet via
// AddFlagSet once built - AddFlagSet shares the same underlying
// *pflag.Flag objects rather than copying them, so parsing behavior is
// unaffected; only which FlagSet's FlagUsages() gets printed changes.
type flagGroup struct {
	title string
	set   *pflag.FlagSet
}

func newFlagGroup(title string) *flagGroup {
	set := pflag.NewFlagSet(title, pflag.ContinueOnError)
	set.SortFlags = false
	return &flagGroup{title: title, set: set}
}

const helpExamples = `Examples:
  grsync -av src/ dest/                                 basic recursive sync
  grsync -av -n -i src/ dest/                            preview changes only
  grsync -av -e "ssh -i key.pem" src/ user@host:dest/    sync over SSH
  grsync -av -z src/ dest/                               sync with compression
`

// usageFunc renders --help/-h's usage section: the synopsis, the examples
// block, then each flag group under its own heading, in the order given.
// Replaces cobra's default single alphabetized "Flags:" block.
func usageFunc(groups []*flagGroup) func(*cobra.Command) error {
	return func(cmd *cobra.Command) error {
		var b strings.Builder
		fmt.Fprintf(&b, "Usage:\n  %s\n\n", cmd.UseLine())
		b.WriteString(helpExamples)
		for _, g := range groups {
			if !g.set.HasAvailableFlags() {
				continue
			}
			fmt.Fprintf(&b, "\n%s:\n%s", g.title, g.set.FlagUsagesWrapped(0))
		}
		_, err := fmt.Fprint(cmd.OutOrStderr(), b.String())
		return err
	}
}

// NewRootCmd builds the root grsync command. It is exported as a
// constructor (rather than a package-level var) so tests can create fresh
// instances without shared state.
func NewRootCmd() *cobra.Command {
	opts := &options{}

	cmd := &cobra.Command{
		Use:   "grsync <source>... <destination> [flags]",
		Short: "Sync files between a source and a destination, locally or over SSH/rsync://",
		Long: "grsync is a Go rewrite of rsync. It supports local, SSH, and rsync:// daemon " +
			"destinations. See the README for full behavior and scope notes.",
		// --server takes exactly one positional arg (the destination path)
		// rather than the normal <source>...<destination> shape: it is how
		// a remote-invoked grsync (e.g. `ssh host grsync --server /dest`)
		// switches into speaking internal/pipeline's protocol over its own
		// stdin/stdout against that destination, instead of a normal sync.
		// --daemon takes none at all: everything it needs (which modules
		// exist, where they live) comes from --config's rsyncd.conf, not
		// from positional args. --read-batch takes exactly one positional
		// arg too - the destination tree to apply the batch to - since the
		// batch file itself already carries the file list a normal sync
		// would otherwise build by walking a source (see runReadBatch's own
		// doc comment); --write-batch does not change the normal
		// <source>...<destination> shape at all, it only adds a side effect
		// to an otherwise-ordinary sync, so it needs no Args case of its
		// own (its own "exactly one source" requirement is checked in
		// runSync instead, once destination/sources are already split).
		Args: func(cmd *cobra.Command, args []string) error {
			// Checked before any other dispatch decision, regardless of
			// which of the two RunE would otherwise "win": without this,
			// giving both flags together would silently run whichever
			// check happens to come first below and ignore the other
			// entirely - matching real rsync's own explicit
			// "--write-batch and --read-batch can not be used together"
			// rejection (options.c), verified against source rather than
			// assumed.
			if opts.writeBatch != "" && opts.readBatch != "" {
				return fmt.Errorf("--write-batch and --read-batch cannot be used together")
			}
			if opts.daemon {
				return cobra.NoArgs(cmd, args)
			}
			if opts.server {
				return cobra.ExactArgs(1)(cmd, args)
			}
			if opts.readBatch != "" {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.MinimumNArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.daemon {
				return runDaemon(cmd, opts)
			}
			if opts.server {
				return runServer(cmd, args[0], opts)
			}
			if opts.readBatch != "" {
				return runReadBatch(cmd, args[0], opts)
			}
			sources, destination := args[:len(args)-1], args[len(args)-1]
			return runSync(cmd, sources, destination, opts)
		},
	}

	core := newFlagGroup("Core")
	core.set.BoolVarP(&opts.archive, "archive", "a", false, "archive mode: -rlptgo")
	core.set.BoolVarP(&opts.verbose, "verbose", "v", false, "mention each updated item's path")
	core.set.BoolVarP(&opts.dryRun, "dry-run", "n", false, "plan the sync without changing anything on disk")
	core.set.BoolVarP(&opts.recursive, "recursive", "r", false, "recurse into directories")
	core.set.BoolVarP(&opts.dirs, "dirs", "d", false, "list directories without recursing into them (implied by -r)")

	attrs := newFlagGroup("Attributes")
	attrs.set.BoolVarP(&opts.perms, "perms", "p", false, "preserve permissions")
	attrs.set.BoolVarP(&opts.times, "times", "t", false, "preserve modification times")
	attrs.set.BoolVarP(&opts.owner, "owner", "o", false, "preserve owner (needs privileges)")
	attrs.set.BoolVarP(&opts.group, "group", "g", false, "preserve group (needs privileges)")
	attrs.set.BoolVarP(&opts.links, "links", "l", false, "recreate symlinks as symlinks")
	attrs.set.BoolVarP(&opts.hardLinks, "hard-links", "H", false, "preserve hard links (not implied by -a)")

	filtering := newFlagGroup("Filtering")
	filtering.set.Var(&filterRuleFlag{ruleType: FilterRuleExclude, rules: &opts.filterRules},
		"exclude", "exclude files matching PATTERN (repeatable)")
	filtering.set.Var(&filterRuleFlag{ruleType: FilterRuleInclude, rules: &opts.filterRules},
		"include", "include files matching PATTERN (repeatable)")
	filtering.set.Var(&filterRuleFlag{ruleType: FilterRuleFilter, rules: &opts.filterRules},
		"filter", "add a raw filter RULE (repeatable)")
	filtering.set.Var(&filterRuleFlag{ruleType: FilterRuleExcludeFrom, rules: &opts.filterRules},
		"exclude-from", "read exclude patterns from FILE (repeatable)")
	filtering.set.Var(&filterRuleFlag{ruleType: FilterRuleIncludeFrom, rules: &opts.filterRules},
		"include-from", "read include patterns from FILE (repeatable)")

	output := newFlagGroup("Output")
	output.set.BoolVarP(&opts.itemize, "itemize-changes", "i", false, "print a per-item change summary (overrides -v)")
	output.set.BoolVar(&opts.progress, "progress", false, "show live per-file transfer progress")
	output.set.BoolVar(&opts.stats, "stats", false, "print a summary after the sync completes")

	transport := newFlagGroup("Transport")
	transport.set.StringVarP(&opts.rsh, "rsh", "e", "", "remote shell command for SSH transport")
	transport.set.StringVar(&opts.address, "address", "", "bind to a specific local address")
	transport.set.BoolVarP(&opts.ipv4, "ipv4", "4", false, "use IPv4 only")
	transport.set.BoolVarP(&opts.ipv6, "ipv6", "6", false, "use IPv6 only")
	transport.set.StringVar(&opts.passwordFile, "password-file", "", "read an rsync:// password from FILE")
	transport.set.BoolVar(&opts.server, "server", false, "internal: speak the transport protocol over stdin/stdout")
	// Hidden, not just undocumented: real rsync's --server is likewise
	// absent from its own --help output, since it's a protocol
	// implementation detail, not a user-facing feature to advertise.
	if err := transport.set.MarkHidden("server"); err != nil {
		panic(err) // only fails if "server" isn't a registered flag name, which would be a programming error caught immediately by any test run
	}

	daemonGroup := newFlagGroup("Daemon mode")
	daemonGroup.set.BoolVar(&opts.daemon, "daemon", false, "run as an rsync daemon, serving modules from --config")
	daemonGroup.set.StringVar(&opts.config, "config", "", "rsyncd.conf to serve (required with --daemon)")
	daemonGroup.set.IntVar(&opts.port, "port", daemon.DefaultPort, "TCP port to listen on")

	advanced := newFlagGroup("Advanced/edge cases")
	advanced.set.BoolVarP(&opts.compress, "compress", "z", false, "compress transferred data")
	advanced.set.IntVar(&opts.compressLevel, "compress-level", 0, "1 (fastest) to 9 (smallest); default 6")
	advanced.set.StringVar(&opts.skipCompress, "skip-compress", "", "suffix list to send uncompressed")
	advanced.set.BoolVar(&opts.partial, "partial", false, "keep partially-transferred files instead of deleting them")
	advanced.set.StringVar(&opts.partialDir, "partial-dir", "", "put partial files in DIR instead (implies --partial)")
	advanced.set.BoolVar(&opts.appendMode, "append", false, "append to shorter files, trusting the existing content")
	advanced.set.BoolVar(&opts.appendVerify, "append-verify", false, "like --append, but verifies the existing content first")
	advanced.set.StringVar(&opts.writeBatch, "write-batch", "", "save the transfer as a batch file (grsync's own format)")
	advanced.set.StringVar(&opts.readBatch, "read-batch", "", "replay a batch file saved with --write-batch")
	advanced.set.BoolVar(&opts.delete, "delete", false, "delete extraneous files from destination")

	groups := []*flagGroup{core, attrs, filtering, output, transport, daemonGroup, advanced}
	for _, g := range groups {
		cmd.Flags().AddFlagSet(g.set)
	}
	cmd.SetUsageFunc(usageFunc(groups))

	return cmd
}

// Execute runs the root command using os.Args, as called from main().
func Execute() error {
	return NewRootCmd().Execute()
}
