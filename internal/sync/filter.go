package sync

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strings"
)

// RuleKind identifies which flag produced a raw filter rule, before pattern
// parsing or file expansion happens. Defined independently from internal/cli's
// FilterRule since internal/sync must never import internal/cli.
type RuleKind string

const (
	// RuleInclude is a direct --include pattern.
	RuleInclude RuleKind = "include"
	// RuleExclude is a direct --exclude pattern.
	RuleExclude RuleKind = "exclude"
	// RuleFilter is a raw --filter rule line, e.g. "+ *.txt", "- .git/", or "merge FILE".
	RuleFilter RuleKind = "filter"
	// RuleExcludeFrom is a --exclude-from file path; CompileRules expands
	// it in place at that position, preserving command-line order.
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

// Rule is a single compiled, ready-to-match filter rule. Pattern has already
// had its leading "/" and trailing "/" markers stripped by CompileRules.
//
// A pattern anchors to the transfer root if it has a leading "/", contains
// any other "/", or contains "**"; only a bare filename like "*.log" matches
// at any depth, against the final path component only (matching rsync).
type Rule struct {
	Action   Action
	Pattern  string
	Anchored bool
	// DirOnly means the rule only matches directories (from a trailing "/" on the original pattern).
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

	// Unanchored: try matching starting at any depth, as if "**/" were prepended.
	for start := 0; start <= len(pathSegs); start++ {
		if matchSegments(patternSegs, pathSegs[start:]) {
			return true
		}
	}
	return false
}

// matchSegments matches a "/"-split pattern against a "/"-split path.
// Every segment except "**" is matched with path.Match. "**" may consume
// zero or more path segments, so both possibilities are tried.
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

// compilePattern parses a pattern's anchor ("/" prefix) and dir-only ("/"
// suffix) markers into a ready-to-match Rule. Shared by direct
// --include/--exclude rules, --exclude-from/--include-from file lines, and
// --filter rule lines.
//
// A pattern with an empty "/"-segment (bare "/", "", or an "a//b" typo) is
// rejected rather than compiled: it could never match a real FileEntry.Path,
// so it would silently become a no-op rule instead of a clear error.
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

	// Anchors on leading "/", any other "/", or "**" anywhere - matching rsync's rule.
	anchored := leadingSlash || strings.Contains(pattern, "/") || strings.Contains(pattern, "**")

	return Rule{Action: action, Pattern: pattern, Anchored: anchored, DirOnly: dirOnly}, nil
}

// readPatternFile reads one pattern per line from path, for
// --exclude-from/--include-from. Blank lines and lines starting with "#" or
// ";" are skipped, matching rsync's filter-file comment convention.
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
// syntax, whether from a --filter flag or a line inside a merge file.
type parsedFilterLine struct {
	isMerge   bool
	mergeFile string
	rule      Rule // valid only when !isMerge
}

// parseFilterLine implements a subset of rsync's --filter syntax:
// "+ PATTERN" / "- PATTERN" (and "include"/"exclude" word forms), plus
// "merge FILE". Anything outside this subset is a hard parse error, not a
// silent no-op.
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

// expandMergeFile reads path and parses each line with parseFilterLine.
// Nested merge directives are a hard error rather than silently recursing
// (risking a loop on a self-referential file) or silently dropping them.
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

// CompileRules converts raw rules into a single ordered, ready-to-match Rule
// list. From-file and merged rules are expanded in place at the position
// their flag occurred, preserving command-line order.
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

// Included evaluates rules against entry using rsync's first-match-wins
// semantics; if no rule matches, the entry is included by default.
func Included(rules []Rule, entry FileEntry) bool {
	for _, r := range rules {
		if r.matches(entry.Path, entry.IsDir) {
			return r.Action == Include
		}
	}
	return true
}

// FilterEntries returns the subset of entries that rules includes, preserving order.
func FilterEntries(entries []FileEntry, rules []Rule) []FileEntry {
	kept := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		if Included(rules, e) {
			kept = append(kept, e)
		}
	}
	return kept
}
