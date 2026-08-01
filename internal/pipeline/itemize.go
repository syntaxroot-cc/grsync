package pipeline

import (
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// ReceiverOptions bundles Receiver's dry-run and reporting behavior -
// kept separate from sync.AttrOptions, which controls *what* gets
// preserved, not whether writes happen at all or what gets reported
// about them.
type ReceiverOptions struct {
	// DryRun, when true, makes Receiver perform every planning step
	// (signature/delta exchange, hard-link grouping, itemize
	// computation) exactly as a real run would, but skip every
	// filesystem write - see Receiver's doc comment for the audited
	// list of exactly which calls that covers.
	DryRun bool
	// Itemize, when true, writes one real-rsync-format %i line (see
	// itemizeFile/itemizeDir/itemizeSymlink) per changed entry to
	// Output. Takes precedence over Verbose when both are set, matching
	// real rsync's own -i implying strictly more detail than -v alone.
	Itemize bool
	// Verbose, when true and Itemize is false, writes just the path
	// (plus " -> target" for a changed symlink) per changed entry to
	// Output - real rsync's own default "%n%L" format for -v without -i.
	Verbose bool
	// Output is where Itemize/Verbose lines are written, one per line.
	// A nil Output is treated as io.Discard, so a caller that wants no
	// reporting at all doesn't need to construct a discard writer
	// itself.
	Output io.Writer
}

func (o ReceiverOptions) output() io.Writer {
	if o.Output == nil {
		return io.Discard
	}
	return o.Output
}

// Reporting reports whether o requests any change reporting at all
// (Itemize or Verbose) - exported since callers outside this package
// (internal/cli, deciding whether to print its own one-time daemon-PUT
// reporting-gap note) need it too, not just Receiver itself.
func (o ReceiverOptions) Reporting() bool {
	return o.Itemize || o.Verbose
}

// itemizeAttrs holds the 9 attribute-letter positions of real rsync's
// %i format (the "cstpoguax" tail of "YXcstpoguax", see rsync.1's
// --itemize-changes section) in order: checksum/value, size, time,
// perms, owner, group, atime/crtime, ACL, xattr. The last three (atime/
// crtime, ACL, xattr) are never set to anything but '.' anywhere in this
// package - grsync has no --atimes/--acl/--xattr flags, matching real
// rsync's own behavior when those options aren't given.
type itemizeAttrs [9]byte

func newItemizeAttrs() itemizeAttrs {
	return itemizeAttrs{'.', '.', '.', '.', '.', '.', '.', '.', '.'}
}

func (a itemizeAttrs) String() string { return string(a[:]) }

// changed reports whether any position was set to something other than
// ".", i.e. whether this attrs value actually represents a difference
// worth reporting at all.
func (a itemizeAttrs) changed() bool {
	for _, b := range a {
		if b != '.' {
			return true
		}
	}
	return false
}

const itemizeNewSuffix = "+++++++++" // real rsync's own "newly created" marker, all 9 positions

// itemizeFile computes the %i code for a regular-file entry, comparing
// it against old (the destination's current state, from
// sync.LstatEntry) when existed is true. contentChanged reports whether
// the file's actual bytes differ - computed by the caller via
// sync.ApplyDelta, since that's the only way grsync (which has no
// --checksum-gated shortcut and no quick-check) can know for certain,
// and it costs nothing extra to compute since ApplyDelta is pure
// in-memory work Receiver already has to do for dry-run's planning-only
// requirement anyway.
//
// report is false exactly when nothing about the entry differs at all -
// matching real rsync's own default (single -i) behavior of not
// mentioning completely unchanged items.
func itemizeFile(entry sync.FileEntry, old sync.FileEntry, existed bool, contentChanged bool, opts sync.AttrOptions) (line string, report bool) {
	if !existed {
		return ">f" + itemizeNewSuffix, true
	}

	a := newItemizeAttrs()
	if old.Size != entry.Size {
		a[1] = 's'
	}
	if opts.Times && !old.ModTime.Equal(entry.ModTime) {
		a[2] = 't'
	}
	if opts.Perms && old.Mode.Perm() != entry.Mode.Perm() {
		a[3] = 'p'
	}
	if opts.Owner && old.OwnershipAvailable && entry.OwnershipAvailable && old.UID != entry.UID {
		a[4] = 'o'
	}
	if opts.Group && old.OwnershipAvailable && entry.OwnershipAvailable && old.GID != entry.GID {
		a[5] = 'g'
	}

	if !a.changed() && !contentChanged {
		return "", false
	}

	// Real rsync's own distinction: '>' means the file's data was
	// actually transferred; '.' means it wasn't (attributes-only
	// update) - see the man page's own "." definition: "the item is not
	// being updated (though it might have attributes that are being
	// modified)".
	y := byte('.')
	if contentChanged {
		y = '>'
	}
	return string(y) + "f" + a.String(), true
}

// itemizeDir computes the %i code for a directory entry. Directories
// have no byte content, so there is no "s" (size) or content-changed
// concept for them at all - only the attribute letters real rsync's own
// format actually applies to a directory.
func itemizeDir(entry sync.FileEntry, old sync.FileEntry, existed bool, opts sync.AttrOptions) (line string, report bool) {
	if !existed {
		return "cd" + itemizeNewSuffix, true
	}

	a := newItemizeAttrs()
	if opts.Times && !old.ModTime.Equal(entry.ModTime) {
		a[2] = 't'
	}
	if opts.Perms && old.Mode.Perm() != entry.Mode.Perm() {
		a[3] = 'p'
	}
	if opts.Owner && old.OwnershipAvailable && entry.OwnershipAvailable && old.UID != entry.UID {
		a[4] = 'o'
	}
	if opts.Group && old.OwnershipAvailable && entry.OwnershipAvailable && old.GID != entry.GID {
		a[5] = 'g'
	}

	if !a.changed() {
		return "", false
	}
	return ".d" + a.String(), true
}

// itemizeSymlink computes the %i code for a symlink entry. A symlink
// is always fully recreated (never diffed byte-by-byte - see
// sync.applySymlink), so its own attribute-"c" position means "the
// link's target value differs", the same "changed value" meaning the
// man page documents for symlinks/devices/specials, distinct from what
// "c" means for a regular file (checksum, which grsync never sets for
// files at all - see itemizeFile).
func itemizeSymlink(entry sync.FileEntry, old sync.FileEntry, existed bool, opts sync.AttrOptions) (line string, report bool) {
	if !existed {
		return "cL" + itemizeNewSuffix, true
	}

	a := newItemizeAttrs()
	if old.LinkTarget != entry.LinkTarget {
		a[0] = 'c'
	}
	if opts.Owner && old.OwnershipAvailable && entry.OwnershipAvailable && old.UID != entry.UID {
		a[4] = 'o'
	}
	if opts.Group && old.OwnershipAvailable && entry.OwnershipAvailable && old.GID != entry.GID {
		a[5] = 'g'
	}

	if !a.changed() {
		return "", false
	}
	// Y is always 'c' when a symlink is being reported at all: unlike a
	// regular file, there is no "attributes changed but the link itself
	// wasn't re-created" case - applySymlink always removes and
	// recreates it (see its own doc comment), so any reported symlink
	// change is by definition a local recreation.
	return "cL" + a.String(), true
}

// itemizeHardLink is the %i code for a hard-link group's secondary
// member: real rsync's own 'h' update type ("the item is a hard link to
// another item"). Unlike the other item kinds, this never depends on
// comparing against any prior destination state - sync.ApplyHardLinks
// always removes and relinks unconditionally (see its own doc comment),
// so a secondary member is always reported, exactly like a symlink is
// always reported as a full recreation rather than a partial update.
func itemizeHardLink() string {
	return "hf" + itemizeNewSuffix
}

// formatItemizeLine joins an %i code with the real "%i %n%L" layout
// real rsync's own -i uses: the code, a space, the path, and - for a
// symlink - " -> target".
func formatItemizeLine(code, path, linkTarget string) string {
	var b strings.Builder
	b.WriteString(code)
	b.WriteByte(' ')
	b.WriteString(path)
	if linkTarget != "" {
		b.WriteString(" -> ")
		b.WriteString(linkTarget)
	}
	return b.String()
}

// formatVerboseLine is real rsync's own "%n%L" format, used when -v is
// given without -i: just the path, plus " -> target" for a symlink.
func formatVerboseLine(path, linkTarget string) string {
	if linkTarget == "" {
		return path
	}
	return path + " -> " + linkTarget
}

// reportChange writes one itemize/verbose line for entry to ropts.Output,
// if ropts requests any reporting at all and report says this entry is
// actually worth mentioning - matching real rsync's own default (single
// -i) behavior of never mentioning a completely unchanged item.
func reportChange(ropts ReceiverOptions, code string, report bool, entry sync.FileEntry) {
	if !ropts.Reporting() || !report {
		return
	}

	linkTarget := ""
	if entry.Mode&fs.ModeSymlink != 0 {
		linkTarget = entry.LinkTarget
	}

	if ropts.Itemize {
		_, _ = fmt.Fprintln(ropts.output(), formatItemizeLine(code, entry.Path, linkTarget))
		return
	}
	_, _ = fmt.Fprintln(ropts.output(), formatVerboseLine(entry.Path, linkTarget))
}
