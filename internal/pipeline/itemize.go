package pipeline

import (
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/syntaxroot-cc/grsync/internal/sync"
)

// ReceiverOptions bundles Receiver's dry-run and reporting behavior -
// kept separate from sync.AttrOptions, which controls what gets
// preserved, not whether writes happen or what gets reported.
type ReceiverOptions struct {
	// DryRun, when true, runs every planning step but skips filesystem writes.
	DryRun bool
	// Itemize, when true, writes one real-rsync-format %i line per changed
	// entry to Output. Takes precedence over Verbose when both are set.
	Itemize bool
	// Verbose, when true and Itemize is false, writes just the path
	// (plus " -> target" for a changed symlink) per changed entry to Output.
	Verbose bool
	// Progress, when true, writes a live-updating line per regular file
	// as its data is written to disk.
	Progress bool
	// Stats, when true, writes a summary block to Output once the sync completes.
	Stats bool
	// Output is where Itemize/Verbose/Progress/Stats output is written.
	// A nil Output is treated as io.Discard.
	Output io.Writer

	// Partial, when true, keeps a regular file's temp file instead of
	// deleting it if the transfer aborts before rename into place.
	// Implied by a non-empty PartialDir.
	Partial bool
	// PartialDir, when non-empty, is where an abandoned temp file goes
	// instead of the destination path directly. A file found here is also
	// used as a resume basis on a later run, then deleted on success.
	PartialDir string
	// Append, when true, blindly trusts a shorter destination file's
	// existing bytes and transfers only the new tail. Mutually exclusive
	// with AppendVerify (internal/cli validates this).
	Append bool
	// AppendVerify is like Append, but verifies the existing prefix via a
	// normal signature/delta comparison instead of trusting it blindly.
	AppendVerify bool
}

// AppendMode reports whether either append flag is set.
func (o ReceiverOptions) AppendMode() bool {
	return o.Append || o.AppendVerify
}

// KeepPartial reports whether an aborted temp file should be kept at all.
func (o ReceiverOptions) KeepPartial() bool {
	return o.Partial || o.PartialDir != ""
}

func (o ReceiverOptions) output() io.Writer {
	if o.Output == nil {
		return io.Discard
	}
	return o.Output
}

// Reporting reports whether o requests any output at all: Itemize,
// Verbose, Progress, or Stats.
func (o ReceiverOptions) Reporting() bool {
	return o.Itemize || o.Verbose || o.Progress || o.Stats
}

// itemizeAttrs holds the 9 attribute-letter positions of real rsync's
// %i format ("cstpoguax") in order: checksum/value, size, time, perms,
// owner, group, atime/crtime, ACL, xattr. The last three are never set
// to anything but '.' here - grsync has no --atimes/--acl/--xattr flags.
type itemizeAttrs [9]byte

func newItemizeAttrs() itemizeAttrs {
	return itemizeAttrs{'.', '.', '.', '.', '.', '.', '.', '.', '.'}
}

func (a itemizeAttrs) String() string { return string(a[:]) }

// changed reports whether any position differs from the "unchanged" default.
func (a itemizeAttrs) changed() bool {
	for _, b := range a {
		if b != '.' {
			return true
		}
	}
	return false
}

const itemizeNewSuffix = "+++++++++" // real rsync's "newly created" marker, all 9 positions

// itemizeFile computes the %i code for a regular-file entry, comparing it
// against old (the destination's current state) when existed is true.
// contentChanged reports whether the file's actual bytes differ.
// report is false when nothing about the entry differs.
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

	// '>' means data was actually transferred; '.' means attributes-only.
	y := byte('.')
	if contentChanged {
		y = '>'
	}
	return string(y) + "f" + a.String(), true
}

// itemizeDir computes the %i code for a directory entry.
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

// itemizeSymlink computes the %i code for a symlink entry. Its "c"
// position means the link target changed, unlike "c" for a regular file
// (checksum, which grsync never sets).
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
	return "cL" + a.String(), true
}

// itemizeHardLink is the %i code for a hard-link group's secondary
// member (real rsync's 'h' update type).
func itemizeHardLink() string {
	return "hf" + itemizeNewSuffix
}

// formatItemizeLine joins an %i code with real rsync's "%i %n%L" layout.
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

// formatVerboseLine is real rsync's "%n%L" format, used when -v is given
// without -i.
func formatVerboseLine(path, linkTarget string) string {
	if linkTarget == "" {
		return path
	}
	return path + " -> " + linkTarget
}

// reportChange writes one itemize/verbose line for entry to ropts.Output,
// if requested and report says this entry is worth mentioning.
func reportChange(ropts ReceiverOptions, code string, report bool, entry sync.FileEntry) {
	if (!ropts.Itemize && !ropts.Verbose) || !report {
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
