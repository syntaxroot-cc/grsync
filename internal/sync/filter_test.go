package sync

import (
	"path/filepath"
	"testing"
)

func mustCompile(t *testing.T, raw []RawRule) []Rule {
	t.Helper()
	rules, err := CompileRules(raw)
	if err != nil {
		t.Fatalf("CompileRules returned error: %v", err)
	}
	return rules
}

func TestRule_MatchesLiteralAndSingleSegmentWildcards(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"file.txt", "file.txt", true},
		{"file.txt", "other.txt", false},
		{"*.txt", "file.txt", true},
		{"*.txt", "file.log", false},
		{"file.?xt", "file.txt", true},
		{"file.?xt", "file.text", false}, // ? matches exactly one char
	}

	for _, tt := range tests {
		r := Rule{Action: Include, Pattern: tt.pattern}
		got := r.matches(tt.path, false)
		if got != tt.want {
			t.Errorf("Rule{Pattern: %q}.matches(%q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestCompileRules_Anchoring(t *testing.T) {
	rules := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "/build"},    // anchored: root-level "build" only
		{Kind: RuleExclude, Pattern: "cache.tmp"}, // unanchored: matches at any depth
	})

	anchored, unanchored := rules[0], rules[1]

	if !anchored.Anchored {
		t.Errorf("anchored.Anchored = false, want true")
	}
	if anchored.Pattern != "build" {
		t.Errorf("anchored.Pattern = %q, want %q (leading / should be stripped)", anchored.Pattern, "build")
	}
	if !anchored.matches("build", false) {
		t.Errorf("anchored pattern should match root-level %q", "build")
	}
	if anchored.matches("sub/build", false) {
		t.Errorf("anchored pattern must NOT match nested %q", "sub/build")
	}

	if unanchored.Anchored {
		t.Errorf("unanchored.Anchored = true, want false")
	}
	if !unanchored.matches("cache.tmp", false) {
		t.Errorf("unanchored pattern should match root-level %q", "cache.tmp")
	}
	if !unanchored.matches("a/b/cache.tmp", false) {
		t.Errorf("unanchored pattern should match nested %q", "a/b/cache.tmp")
	}
}

func TestCompileRules_InternalSlashAnchorsWithoutLeadingSlash(t *testing.T) {
	// Real rsync's actual rule: a pattern anchors to the root if it has a
	// leading "/", OR contains any other "/", OR contains "**" — not just
	// on an explicit leading "/". "src/main.go" (no leading slash, but an
	// internal one) must behave the same as "/src/main.go" here.
	rules := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "src/main.go"},
	})
	r := rules[0]

	if !r.Anchored {
		t.Fatalf("Anchored = false, want true for a pattern with an internal \"/\" but no leading \"/\"")
	}
	if !r.matches("src/main.go", false) {
		t.Errorf("should match root-level %q", "src/main.go")
	}
	if r.matches("vendor/src/main.go", false) {
		t.Errorf("must NOT match nested %q now that internal \"/\" anchors", "vendor/src/main.go")
	}
}

func TestCompileRules_DoubleStarAnchorsWithoutSlash(t *testing.T) {
	// Same rsync rule, the "**" half: a pattern containing "**" anchors
	// even with no "/" anywhere in it.
	rules := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "**cache**"},
	})
	if !rules[0].Anchored {
		t.Errorf("Anchored = false, want true for a pattern containing \"**\" but no \"/\"")
	}
}

func TestCompileRules_BareFilenameStaysUnanchored(t *testing.T) {
	// The one case that must NOT anchor: no "/" at all, no "**" at all.
	rules := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "*.log"},
	})
	r := rules[0]
	if r.Anchored {
		t.Fatalf("Anchored = true, want false for a bare filename pattern with no \"/\" or \"**\"")
	}
	if !r.matches("a/b/debug.log", false) {
		t.Errorf("bare filename pattern should still match at any depth")
	}
}

func TestCompileRules_DirOnly(t *testing.T) {
	rules := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "build/"},
	})
	r := rules[0]

	if !r.DirOnly {
		t.Fatalf("DirOnly = false, want true")
	}
	if r.Pattern != "build" {
		t.Errorf("Pattern = %q, want %q (trailing / should be stripped)", r.Pattern, "build")
	}
	if !r.matches("build", true) {
		t.Errorf("dir-only pattern should match a directory named %q", "build")
	}
	if r.matches("build", false) {
		t.Errorf("dir-only pattern must NOT match a regular file named %q", "build")
	}
}

func TestCompileRules_AnchoredAndDirOnlyTogether(t *testing.T) {
	rules := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "/dist/"},
	})
	r := rules[0]

	if !r.Anchored || !r.DirOnly || r.Pattern != "dist" {
		t.Fatalf("got Anchored=%v DirOnly=%v Pattern=%q, want Anchored=true DirOnly=true Pattern=%q",
			r.Anchored, r.DirOnly, r.Pattern, "dist")
	}
	if !r.matches("dist", true) {
		t.Errorf("should match root-level directory %q", "dist")
	}
	if r.matches("sub/dist", true) {
		t.Errorf("anchored: must NOT match nested directory %q", "sub/dist")
	}
}

func TestIncluded_NoRulesMatchDefaultsToIncluded(t *testing.T) {
	entry := FileEntry{Path: "anything.txt"}
	if !Included(nil, entry) {
		t.Errorf("Included with no rules = false, want true (default include)")
	}
}

func TestIncluded_FirstMatchWinsOrderMatters(t *testing.T) {
	entry := FileEntry{Path: "keep.log"}

	// Same two rules, opposite order: the first one to match should win in
	// both cases, so swapping the order must flip the outcome. If it
	// didn't, evaluation wouldn't actually be "first match wins" — it'd be
	// "last match wins" or "most specific wins" or something else.
	includeFirst := mustCompile(t, []RawRule{
		{Kind: RuleInclude, Pattern: "keep.log"},
		{Kind: RuleExclude, Pattern: "*.log"},
	})
	if !Included(includeFirst, entry) {
		t.Errorf("include-before-exclude: Included = false, want true")
	}

	excludeFirst := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "*.log"},
		{Kind: RuleInclude, Pattern: "keep.log"},
	})
	if Included(excludeFirst, entry) {
		t.Errorf("exclude-before-include: Included = true, want false")
	}
}

func TestFilterEntries(t *testing.T) {
	entries := []FileEntry{
		{Path: "main.go"},
		{Path: "debug.log"},
		{Path: "keep.log"},
		{Path: "build", IsDir: true},
	}
	rules := mustCompile(t, []RawRule{
		{Kind: RuleInclude, Pattern: "keep.log"},
		{Kind: RuleExclude, Pattern: "*.log"},
		{Kind: RuleExclude, Pattern: "/build/"},
	})

	got := FilterEntries(entries, rules)

	want := []string{"main.go", "keep.log"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("entry %d: Path = %q, want %q", i, got[i].Path, w)
		}
	}
}

func TestCompileRules_ExcludeFromPreservesPosition(t *testing.T) {
	root := t.TempDir()
	excludeFile := filepath.Join(root, "excludes.txt")
	mustWriteFile(t, excludeFile, "# a comment, skipped\n\n*.log\n/dist/\n")

	rules := mustCompile(t, []RawRule{
		{Kind: RuleExclude, Pattern: "first.txt"},
		{Kind: RuleExcludeFrom, Pattern: excludeFile},
		{Kind: RuleInclude, Pattern: "last.txt"},
	})

	// Position matters here: the two patterns read from the file must land
	// between "first.txt" and "last.txt", not get appended after
	// "last.txt" — that would silently reorder rules relative to what the
	// user typed on the command line, breaking first-match-wins semantics.
	want := []struct {
		action  Action
		pattern string
		dirOnly bool
	}{
		{Exclude, "first.txt", false},
		{Exclude, "*.log", false},
		{Exclude, "dist", true},
		{Include, "last.txt", false},
	}

	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d: %+v", len(rules), len(want), rules)
	}
	for i, w := range want {
		if rules[i].Action != w.action || rules[i].Pattern != w.pattern || rules[i].DirOnly != w.dirOnly {
			t.Errorf("rule %d = %+v, want {Action:%v Pattern:%q DirOnly:%v}", i, rules[i], w.action, w.pattern, w.dirOnly)
		}
	}
}

func TestCompileRules_IncludeFrom(t *testing.T) {
	root := t.TempDir()
	includeFile := filepath.Join(root, "includes.txt")
	mustWriteFile(t, includeFile, "keep.log\n")

	rules := mustCompile(t, []RawRule{
		{Kind: RuleIncludeFrom, Pattern: includeFile},
		{Kind: RuleExclude, Pattern: "*.log"},
	})

	entry := FileEntry{Path: "keep.log"}
	if !Included(rules, entry) {
		t.Errorf("keep.log should be included via --include-from before the *.log exclude")
	}
}

func TestCompileRules_ExcludeFromMissingFileErrors(t *testing.T) {
	_, err := CompileRules([]RawRule{
		{Kind: RuleExcludeFrom, Pattern: filepath.Join(t.TempDir(), "does-not-exist.txt")},
	})
	if err == nil {
		t.Fatalf("CompileRules with a missing --exclude-from file returned nil error, want an error")
	}
}

func TestCompileRules_FilterRuleSyntax(t *testing.T) {
	rules := mustCompile(t, []RawRule{
		{Kind: RuleFilter, Pattern: "+ keep.log"},
		{Kind: RuleFilter, Pattern: "- *.log"},
		{Kind: RuleFilter, Pattern: "exclude /dist/"},
	})

	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3: %+v", len(rules), rules)
	}
	if rules[0].Action != Include || rules[0].Pattern != "keep.log" {
		t.Errorf("rule 0 = %+v, want Include keep.log", rules[0])
	}
	if rules[1].Action != Exclude || rules[1].Pattern != "*.log" {
		t.Errorf("rule 1 = %+v, want Exclude *.log", rules[1])
	}
	if rules[2].Action != Exclude || rules[2].Pattern != "dist" || !rules[2].Anchored || !rules[2].DirOnly {
		t.Errorf("rule 2 = %+v, want anchored dir-only Exclude dist", rules[2])
	}
}

func TestCompileRules_FilterUnsupportedSyntaxErrors(t *testing.T) {
	_, err := CompileRules([]RawRule{
		{Kind: RuleFilter, Pattern: "P *.log"}, // rsync's terse "P" shorthand isn't supported here
	})
	if err == nil {
		t.Fatalf("CompileRules with unsupported filter syntax returned nil error, want an error")
	}
}

func TestCompileRules_FilterMergeBasic(t *testing.T) {
	root := t.TempDir()
	mergeFile := filepath.Join(root, "rules.txt")
	mustWriteFile(t, mergeFile, "+ keep.log\n- *.log\n")

	rules := mustCompile(t, []RawRule{
		{Kind: RuleFilter, Pattern: "merge " + mergeFile},
	})

	entry := FileEntry{Path: "keep.log"}
	if !Included(rules, entry) {
		t.Errorf("keep.log should survive filtering via the merged rules")
	}
	other := FileEntry{Path: "other.log"}
	if Included(rules, other) {
		t.Errorf("other.log should be excluded via the merged rules")
	}
}

func TestCompileRules_FilterNestedMergeErrors(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "inner.txt")
	outer := filepath.Join(root, "outer.txt")
	mustWriteFile(t, inner, "- *.log\n")
	mustWriteFile(t, outer, "merge "+inner+"\n")

	_, err := CompileRules([]RawRule{
		{Kind: RuleFilter, Pattern: "merge " + outer},
	})
	if err == nil {
		t.Fatalf("CompileRules with a nested merge file returned nil error, want an error")
	}
}

func TestCompileRules_EmptyPatternErrors(t *testing.T) {
	tests := []string{
		"",     // empty outright
		"/",    // empty after stripping the anchor marker
		"a//b", // internal empty segment from a stray double slash
	}
	for _, pattern := range tests {
		_, err := CompileRules([]RawRule{{Kind: RuleExclude, Pattern: pattern}})
		if err == nil {
			t.Errorf("CompileRules with pattern %q returned nil error, want an error (a silent no-op rule is worse than a clear failure)", pattern)
		}
	}
}

func TestRule_WildcardOnlyPatternsMatchEverything(t *testing.T) {
	// This is intended behavior, matching real rsync: a bare "*" or "**"
	// is a deliberate "match everything" rule, not a bug to guard against.
	entries := []FileEntry{{Path: "a"}, {Path: "a/b"}, {Path: "a/b/c.txt"}}

	star := mustCompile(t, []RawRule{{Kind: RuleExclude, Pattern: "*"}})
	for _, e := range entries {
		if Included(star, e) {
			t.Errorf("bare \"*\" should exclude every entry, but %q survived", e.Path)
		}
	}

	doubleStar := mustCompile(t, []RawRule{{Kind: RuleExclude, Pattern: "**"}})
	for _, e := range entries {
		if Included(doubleStar, e) {
			t.Errorf("bare \"**\" should exclude every entry, but %q survived", e.Path)
		}
	}
}

func TestRule_MatchesDoubleStarCrossesDirectories(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"a/**/z", "a/z", true},       // ** matches zero segments
		{"a/**/z", "a/b/z", true},     // ** matches one segment
		{"a/**/z", "a/b/c/d/z", true}, // ** matches many segments
		{"a/**/z", "x/b/z", false},    // literal "a" segment still required
		{"a/*/z", "a/b/c/z", false},   // single "*" must NOT cross "/"
		{"**/vendor", "vendor", true}, // leading ** matches zero segments too
		{"**/vendor", "a/b/vendor", true},
		{"a/**", "a/b/c", true}, // trailing ** matches everything under a/
	}

	for _, tt := range tests {
		r := Rule{Action: Include, Pattern: tt.pattern}
		got := r.matches(tt.path, false)
		if got != tt.want {
			t.Errorf("Rule{Pattern: %q}.matches(%q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}
