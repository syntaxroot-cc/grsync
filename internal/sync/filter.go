package sync

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strings"
)

// RuleKind identifies which flag produced a raw filter rule, before any
// pattern parsing or file expansion happens. It mirrors the shape of
// internal/cli's FilterRule (Type + Pattern) but is defined independently:
// internal/sync must never import internal/cli, since cli is expected to
// depend on sync (not the other way around) once they're wired together.
type RuleKind string

const (
	// RuleInclude is a direct --include pattern.
	RuleInclude RuleKind = "include"
	// RuleExclude is a direct --exclude pattern.
	RuleExclude RuleKind = "exclude"
	// RuleFilter is a raw --filter rule line, e.g. "+ *.txt", "- .git/",
	// or "merge FILE" - see parseFilterLine for the subset of rsync's
	// filter-rule syntax this supports.
	RuleFilter RuleKind = "filter"
	// RuleExcludeFrom has a Pattern that is a file path, not a filter
	// pattern itself. CompileRules reads that file and inserts one
	// exclude rule per line at this exact position in the list,
	// preserving overall command-line order rather than appending
	// everything to the end.
	RuleExcludeFrom RuleKind = "exclude-from"
	// RuleIncludeFrom is RuleExcludeFrom's --include-from counterpart.
	RuleIncludeFrom RuleKind = "include-from"
)

// RawRule is a single --include/--exclude occurrence, in command-line
// order, before any pattern parsing happens.
type RawRule struct {
	Kind    RuleKind
	Pattern string
}

// Action is what a compiled Rule does when its pattern matches an entry.
type Action int

const (
	// Include means a matching entry is kept.
	Include Action = iota
	// Exclude means a matching entry is dropped.
	Exclude
)

// Rule is a single compiled, ready-to-match filter rule. Pattern has
// already had its leading "/" (Anchored) marker stripped by CompileRules;
// it never contains that marker itself.
//
// Anchored matches real rsync's actual rule: a pattern anchors to the
// transfer root if it has a leading "/", contains any other "/", or
// contains "**" - only a pattern with none of those (a bare filename, e.g.
// "*.log") matches at any depth, against the final path component only.
type Rule struct {
	Action   Action
	Pattern  string
	Anchored bool
	// DirOnly means this rule only ever matches directories - set from a
	// trailing "/" on the original pattern, stripped by CompileRules just
	// like the anchor marker.
	DirOnly bool
}

// matches reports whether entryPath (a directory if isDir) matches r.
func (r Rule) matches(entryPath string, isDir bool) bool {
	if r.DirOnly && !isDir {
		return false
	}

	patternSegs := strings.Split(r.Pattern, "/")
	pathSegs := strings.Split(entryPath, "/")

	if r.Anchored {
		return matchSegments(patternSegs, pathSegs)
	}

	// Unanchored: the pattern may match starting at any depth, as if
	// "**/" were implicitly prepended to it.
	for start := 0; start <= len(pathSegs); start++ {
		if matchSegments(patternSegs, pathSegs[start:]) {
			return true
		}
	}
	return false
}

// matchSegments matches a "/"-split pattern against a "/"-split path,
// segment by segment. Every segment except "**" is matched with
// path.Match, which already gives us "*" (any run of characters within the
// segment) and "?" (exactly one character) for free, without needing a
// hand-rolled matcher of our own. "**" is handled explicitly: it may
// consume zero or more path segments, so both possibilities (consume none
// and keep matching, or consume one and recurse) are tried.
func matchSegments(patternSegs, pathSegs []string) bool {
	if len(patternSegs) == 0 {
		return len(pathSegs) == 0
	}

	if patternSegs[0] == "**" {
		if matchSegments(patternSegs[1:], pathSegs) {
			return true
		}
		if len(pathSegs) == 0 {
			return false
		}
		return matchSegments(patternSegs, pathSegs[1:])
	}

	if len(pathSegs) == 0 {
		return false
	}
	ok, err := path.Match(patternSegs[0], pathSegs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(patternSegs[1:], pathSegs[1:])
}

// compilePattern parses a single raw pattern's anchor ("/" prefix) and
// dir-only ("/" suffix) markers into a ready-to-match Rule with the given
// action. Shared by direct --include/--exclude rules, each line read from
// an --exclude-from/--include-from file, and --filter rule lines.
//
// A pattern containing any empty "/"-separated segment - a bare "/" or ""
// overall, or an internal "//" typo like "a//b" - is rejected rather than
// silently compiled: no real FileEntry.Path segment is ever empty (Walk()
// never produces one), so a Rule requiring an empty segment could never
// match anything. Compiling it anyway would leave the user with a filter
// rule that silently does nothing forever, which is worse than a clear
// error at compile time.
func compilePattern(action Action, pattern string) (Rule, error) {
	leadingSlash := strings.HasPrefix(pattern, "/")
	if leadingSlash {
		pattern = pattern[1:]
	}
	dirOnly := strings.HasSuffix(pattern, "/")
	if dirOnly {
		pattern = pattern[:len(pattern)-1]
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "" {
			return Rule{}, fmt.Errorf("filter pattern has an empty path segment (check for a stray \"//\" or a bare %q): %q", "/", pattern)
		}
	}

	// A leading "/" always anchors. So does any *other* "/" still present
	// in the pattern, or a "**" anywhere in it - matching real rsync's
	// rule, not just the "leading slash only" simplification this started
	// as. Only a genuinely slash-free, "**"-free pattern (a bare filename)
	// matches at any depth.
	anchored := leadingSlash || strings.Contains(pattern, "/") || strings.Contains(pattern, "**")

	return Rule{Action: action, Pattern: pattern, Anchored: anchored, DirOnly: dirOnly}, nil
}

// readPatternFile reads one pattern per line from path, for
// --exclude-from/--include-from. Blank lines and lines starting with "#"
// or ";" are skipped, matching rsync's own filter-file comment convention.
func readPatternFile(path string) (patterns []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		patterns = append(patterns, line)
	}
	if serr := scanner.Err(); serr != nil {
		return nil, serr
	}
	return patterns, nil
}

// parsedFilterLine is the result of parsing one line of --filter RULE
// syntax - whether it came directly from a --filter flag or from a line
// inside a merge file.
type parsedFilterLine struct {
	isMerge   bool
	mergeFile string
	rule      Rule // valid only when !isMerge
}

// parseFilterLine implements a deliberately small subset of rsync's
// --filter rule syntax: "+ PATTERN" / "- PATTERN" (and their word-form
// equivalents "include PATTERN" / "exclude PATTERN"), plus "merge FILE".
// Real rsync's filter language is much larger (modifiers like "-C",
// "dir-merge" with per-directory semantics, "!", exclude-if-present rules,
// and more) - none of that is implemented here; anything outside this
// subset is a hard parse error rather than a silent no-op.
func parseFilterLine(line string) (parsedFilterLine, error) {
	switch {
	case strings.HasPrefix(line, "+ "):
		rule, err := compilePattern(Include, strings.TrimSpace(line[2:]))
		return parsedFilterLine{rule: rule}, err
	case strings.HasPrefix(line, "- "):
		rule, err := compilePattern(Exclude, strings.TrimSpace(line[2:]))
		return parsedFilterLine{rule: rule}, err
	case strings.HasPrefix(line, "include "):
		rule, err := compilePattern(Include, strings.TrimSpace(line[len("include "):]))
		return parsedFilterLine{rule: rule}, err
	case strings.HasPrefix(line, "exclude "):
		rule, err := compilePattern(Exclude, strings.TrimSpace(line[len("exclude "):]))
		return parsedFilterLine{rule: rule}, err
	case strings.HasPrefix(line, "merge "):
		return parsedFilterLine{isMerge: true, mergeFile: strings.TrimSpace(line[len("merge "):])}, nil
	default:
		return parsedFilterLine{}, fmt.Errorf(
			"unsupported filter rule syntax: %q (supported: \"+ PATTERN\", \"- PATTERN\", \"include PATTERN\", \"exclude PATTERN\", \"merge FILE\")",
			line)
	}
}

// expandMergeFile reads path and parses each non-comment, non-blank line
// with parseFilterLine - the same syntax --filter itself accepts. Nested
// merge (a merge file whose own lines include another "merge OTHER"
// directive) is deliberately unsupported: rather than silently recursing
// (risking an infinite loop on a self-referential merge file) or silently
// dropping the nested directive, it's a hard error. This covers the
// "basic merge case"; full nested/dir-merge support is a larger feature
// left for later if it turns out to be needed.
func expandMergeFile(path string) ([]Rule, error) {
	lines, err := readPatternFile(path)
	if err != nil {
		return nil, err
	}

	rules := make([]Rule, 0, len(lines))
	for _, line := range lines {
		parsed, err := parseFilterLine(line)
		if err != nil {
			return nil, err
		}
		if parsed.isMerge {
			return nil, fmt.Errorf("nested merge directives are not supported (found %q inside %q)", line, path)
		}
		rules = append(rules, parsed.rule)
	}
	return rules, nil
}

// CompileRules converts raw rules - --include/--exclude patterns,
// --filter rule lines (including "merge FILE"), and --exclude-from/
// --include-from file references - into a single ordered, ready-to-match
// Rule list. From-file and merged rules are expanded in place at the
// position their flag occurred, so e.g. a --exclude-from sandwiched
// between two direct --exclude flags on the command line stays sandwiched
// between their compiled Rules, not appended after them.
func CompileRules(raw []RawRule) ([]Rule, error) {
	var rules []Rule
	for _, r := range raw {
		switch r.Kind {
		case RuleInclude:
			rule, err := compilePattern(Include, r.Pattern)
			if err != nil {
				return nil, err
			}
			rules = append(rules, rule)
		case RuleExclude:
			rule, err := compilePattern(Exclude, r.Pattern)
			if err != nil {
				return nil, err
			}
			rules = append(rules, rule)
		case RuleFilter:
			parsed, err := parseFilterLine(strings.TrimSpace(r.Pattern))
			if err != nil {
				return nil, err
			}
			if !parsed.isMerge {
				rules = append(rules, parsed.rule)
				continue
			}
			merged, err := expandMergeFile(parsed.mergeFile)
			if err != nil {
				return nil, fmt.Errorf("merging filter file %q: %w", parsed.mergeFile, err)
			}
			rules = append(rules, merged...)
		case RuleExcludeFrom, RuleIncludeFrom:
			action := Exclude
			if r.Kind == RuleIncludeFrom {
				action = Include
			}
			patterns, err := readPatternFile(r.Pattern)
			if err != nil {
				return nil, fmt.Errorf("reading %s file %q: %w", r.Kind, r.Pattern, err)
			}
			for _, p := range patterns {
				rule, err := compilePattern(action, p)
				if err != nil {
					return nil, fmt.Errorf("in %s file %q: %w", r.Kind, r.Pattern, err)
				}
				rules = append(rules, rule)
			}
		default:
			return nil, fmt.Errorf("unsupported rule kind %q", r.Kind)
		}
	}
	return rules, nil
}

// Included evaluates rules against a single FileEntry using rsync's
// first-match-wins semantics: rules are tried in order, and the action of
// the first one that matches decides the outcome. If no rule matches, the
// entry is included by default - matching rsync's own default behavior of
// transferring anything not explicitly excluded.
//
// entry.Path is never empty and never represents the transfer root itself:
// Walk() (internal/sync/walk.go) deliberately excludes the root from its
// own output, so there's no "path exactly equal to the root" case for this
// function to special-case.
func Included(rules []Rule, entry FileEntry) bool {
	for _, r := range rules {
		if r.matches(entry.Path, entry.IsDir) {
			return r.Action == Include
		}
	}
	return true
}

// FilterEntries returns the subset of entries that rules includes, in
// their original order. It is a post-pass over an already-collected
// Walk() result rather than a predicate threaded into Walk() itself - see
// the design note on this tradeoff where FilterEntries is introduced in
// the accompanying documentation/commit.
func FilterEntries(entries []FileEntry, rules []Rule) []FileEntry {
	kept := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		if Included(rules, e) {
			kept = append(kept, e)
		}
	}
	return kept
}
