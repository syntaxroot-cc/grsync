// Package cli defines the grsync command-line interface: argument parsing,
// flags, and the command tree. It does not perform any sync or transport
// logic itself — it only collects options and hands them off (see the
// options struct printed in Run below, which will later be passed to
// internal/sync).
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// FilterRuleType identifies which kind of rule a FilterRule represents.
type FilterRuleType string

// The three rule kinds grsync's flags can produce. Kept as their own type
// (rather than a bare string) so callers can't accidentally pass an
// arbitrary value through.
const (
	FilterRuleInclude FilterRuleType = "include"
	FilterRuleExclude FilterRuleType = "exclude"
	FilterRuleFilter  FilterRuleType = "filter"
)

// FilterRule is a single --include/--exclude/--filter rule. rsync treats
// these three flags as one ordered, first-match-wins rule list rather than
// three independent lists, so grsync collects them the same way: Type
// records which flag produced the rule, and relative order across *all*
// three flags is preserved in the order the user supplied them.
type FilterRule struct {
	Type    FilterRuleType
	Pattern string
}

// options holds every flag value parsed from the command line. Keeping them
// in one struct (rather than loose variables) makes it straightforward to
// pass a single value into internal/sync once that package exists.
type options struct {
	archive     bool
	verbose     bool
	compress    bool
	recursive   bool
	dirs        bool
	dryRun      bool
	delete      bool
	progress    bool
	filterRules []FilterRule
}

// filterRuleFlag implements pflag.Value. Each of --exclude/--include/--filter
// gets its own instance, fixed to a single FilterRuleType, but all three
// share the same backing slice — so pflag's normal "call Set once per
// occurrence" behavior naturally builds one ordered rule list regardless of
// which of the three flag names was used at each position.
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
	if f.ruleType == FilterRuleFilter {
		return "rule"
	}
	return "pattern"
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
			"At this stage it only parses arguments and flags; no files are copied yet.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sources, destination := args[:len(args)-1], args[len(args)-1]
			return run(cmd, sources, destination, opts)
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
			"filters:     %s\n",
		sources, destination,
		opts.archive, opts.verbose, opts.compress, opts.recursive, opts.dirs, opts.dryRun,
		opts.delete, opts.progress, rules.String(),
	)

	_, err := fmt.Fprint(cmd.OutOrStdout(), summary)
	return err
}

// Execute runs the root command using os.Args, as called from main().
func Execute() error {
	return NewRootCmd().Execute()
}
