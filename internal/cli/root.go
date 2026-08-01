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
	archive      bool
	verbose      bool
	compress     bool
	recursive    bool
	dirs         bool
	dryRun       bool
	delete       bool
	progress     bool
	perms        bool
	times        bool
	owner        bool
	group        bool
	links        bool
	hardLinks    bool
	filterRules  []FilterRule
	rsh          string
	server       bool
	daemon       bool
	config       string
	port         int
	passwordFile string
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

// NewRootCmd builds the root grsync command. It is exported as a
// constructor (rather than a package-level var) so tests can create fresh
// instances without shared state.
func NewRootCmd() *cobra.Command {
	opts := &options{}

	cmd := &cobra.Command{
		Use:   "grsync <source>... <destination>",
		Short: "grsync synchronizes files between one or more sources and a destination",
		Long: "grsync is an rsync-inspired file synchronization tool.\n" +
			"Local-to-local and local-to-remote (SSH) syncs are supported; " +
			"--dry-run, compression, progress reporting, and full --delete are not yet.",
		// --server takes exactly one positional arg (the destination path)
		// rather than the normal <source>...<destination> shape: it is how
		// a remote-invoked grsync (e.g. `ssh host grsync --server /dest`)
		// switches into speaking internal/pipeline's protocol over its own
		// stdin/stdout against that destination, instead of a normal sync.
		// --daemon takes none at all: everything it needs (which modules
		// exist, where they live) comes from --config's rsyncd.conf, not
		// from positional args.
		Args: func(cmd *cobra.Command, args []string) error {
			if opts.daemon {
				return cobra.NoArgs(cmd, args)
			}
			if opts.server {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.MinimumNArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.daemon {
				return runDaemon(cmd, opts.config, opts.port)
			}
			if opts.server {
				return runServer(cmd, args[0], opts)
			}
			sources, destination := args[:len(args)-1], args[len(args)-1]
			if opts.dryRun {
				// A real dry-run (list what would change, transfer
				// nothing) is explicitly out of scope for now - falling
				// through to a real sync here would silently do the
				// opposite of what --dry-run promises, which is worse
				// than not supporting it yet. Until real dry-run support
				// lands, this stays on the flag-echoing placeholder.
				return run(cmd, sources, destination, opts)
			}
			return runSync(cmd, sources, destination, opts)
		},
	}

	flags := cmd.Flags()
	flags.BoolVarP(&opts.archive, "archive", "a", false, "archive mode (equivalent to common rsync defaults)")
	flags.BoolVarP(&opts.verbose, "verbose", "v", false, "increase output verbosity")
	flags.BoolVarP(&opts.compress, "compress", "z", false, "compress file data during transfer")
	flags.BoolVarP(&opts.recursive, "recursive", "r", false, "recurse into directories")
	flags.BoolVarP(&opts.dirs, "dirs", "d", false, "include directories themselves without recursing into their contents (implied by --recursive)")
	flags.BoolVarP(&opts.dryRun, "dry-run", "n", false, "show what would be transferred without transferring")
	flags.BoolVar(&opts.delete, "delete", false, "delete extraneous files from destination")
	flags.BoolVar(&opts.progress, "progress", false, "show progress during transfer")
	flags.Var(&filterRuleFlag{ruleType: FilterRuleExclude, rules: &opts.filterRules},
		"exclude", "exclude files matching PATTERN (repeatable, order preserved relative to --include/--filter)")
	flags.Var(&filterRuleFlag{ruleType: FilterRuleInclude, rules: &opts.filterRules},
		"include", "include files matching PATTERN (repeatable, order preserved relative to --exclude/--filter)")
	flags.Var(&filterRuleFlag{ruleType: FilterRuleFilter, rules: &opts.filterRules},
		"filter", "add a file-filtering RULE (repeatable, order preserved relative to --exclude/--include)")
	flags.Var(&filterRuleFlag{ruleType: FilterRuleExcludeFrom, rules: &opts.filterRules},
		"exclude-from", "read exclude patterns from FILE, one per line (repeatable, order preserved)")
	flags.Var(&filterRuleFlag{ruleType: FilterRuleIncludeFrom, rules: &opts.filterRules},
		"include-from", "read include patterns from FILE, one per line (repeatable, order preserved)")
	flags.BoolVarP(&opts.perms, "perms", "p", false, "preserve permissions (implied by --archive)")
	flags.BoolVarP(&opts.times, "times", "t", false, "preserve modification times (implied by --archive)")
	flags.BoolVarP(&opts.owner, "owner", "o", false, "preserve owner (implied by --archive; requires appropriate privileges)")
	flags.BoolVarP(&opts.group, "group", "g", false, "preserve group (implied by --archive; requires appropriate privileges)")
	flags.BoolVarP(&opts.links, "links", "l", false, "recreate symlinks as symlinks (implied by --archive)")
	flags.BoolVarP(&opts.hardLinks, "hard-links", "H", false,
		"preserve hard links between files in the source (NOT implied by --archive, matching real rsync's own -a)")
	flags.StringVarP(&opts.rsh, "rsh", "e", "",
		"specify the remote shell to use, e.g. \"ssh -p 2222 -i key.pem\" (default: ssh); "+
			"the sole way to customize port/identity/proxy for remote transport, matching rsync")
	flags.BoolVar(&opts.server, "server", false,
		"run in server mode, speaking the transport protocol over stdin/stdout "+
			"(internal use only - invoked remotely via --rsh, never typed directly, matching rsync's own --server)")
	// Hidden, not just undocumented: real rsync's --server is likewise
	// absent from its own --help output, since it's a protocol
	// implementation detail, not a user-facing feature to advertise.
	if err := flags.MarkHidden("server"); err != nil {
		panic(err) // only fails if "server" isn't a registered flag name, which would be a programming error caught immediately by any test run
	}
	flags.BoolVar(&opts.daemon, "daemon", false, "run as an rsync-protocol daemon, serving modules defined in --config")
	flags.StringVar(&opts.config, "config", "", "path to the rsyncd.conf file to serve (required with --daemon)")
	flags.IntVar(&opts.port, "port", daemon.DefaultPort, "TCP port to listen on in --daemon mode")
	flags.StringVar(&opts.passwordFile, "password-file", "",
		"read the rsync:// daemon password from FILE (or stdin, if FILE is \"-\") instead of the "+
			"RSYNC_PASSWORD environment variable or an interactive prompt; matches real rsync's own "+
			"--password-file, including refusing a world-readable FILE. There is deliberately no "+
			"--password flag: a password given directly as a command-line argument would be visible "+
			"to other users on the same machine via the process list, exactly why real rsync has never had one either")

	return cmd
}

// run is the placeholder command body. It only echoes back what was parsed
// so the flag wiring can be verified end to end; the actual sync/transport
// work lands in later tickets.
func run(cmd *cobra.Command, sources []string, destination string, opts *options) error {
	var rules strings.Builder
	if len(opts.filterRules) == 0 {
		rules.WriteString("[]")
	} else {
		for i, r := range opts.filterRules {
			if i > 0 {
				rules.WriteString(", ")
			}
			fmt.Fprintf(&rules, "%s:%s", r.Type, r.Pattern)
		}
	}

	summary := fmt.Sprintf(
		"sources:     %v\n"+
			"destination: %s\n"+
			"archive:     %t\n"+
			"verbose:     %t\n"+
			"compress:    %t\n"+
			"recursive:   %t\n"+
			"dirs:        %t\n"+
			"dry-run:     %t\n"+
			"delete:      %t\n"+
			"progress:    %t\n"+
			"perms:       %t\n"+
			"times:       %t\n"+
			"owner:       %t\n"+
			"group:       %t\n"+
			"links:       %t\n"+
			"rsh:         %q\n"+
			"filters:     %s\n",
		sources, destination,
		opts.archive, opts.verbose, opts.compress, opts.recursive, opts.dirs, opts.dryRun,
		opts.delete, opts.progress, opts.perms, opts.times, opts.owner, opts.group, opts.links,
		opts.rsh, rules.String(),
	)

	_, err := fmt.Fprint(cmd.OutOrStdout(), summary)
	return err
}

// Execute runs the root command using os.Args, as called from main().
func Execute() error {
	return NewRootCmd().Execute()
}
