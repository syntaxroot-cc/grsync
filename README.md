# grsync

An rsync-inspired file synchronization tool written in Go.

## Status

CLI parsing, file enumeration, filter-rule matching, the delta-transfer
algorithm, and file attribute preservation are implemented; nothing is
wired together into an actual sync yet. `internal/sync` can list a source
tree (`sync.Walk`), filter it (`sync.FilterEntries`), compute/apply binary
deltas between two versions of a file
(`sync.GenerateDelta`/`sync.ApplyDelta`), and apply permissions/times/
ownership/symlinks/hard links (`sync.ApplyAttributes` and friends) - but
the CLI only echoes parsed flags, and `internal/transport` is still empty,
so none of this runs end to end yet.

## Build

```sh
go build ./cmd/grsync
```

Or run directly without building a binary:

```sh
go run ./cmd/grsync <source>... <destination> [flags]
```

## Usage

```
grsync <source>... <destination> [flags]
```

At least one `source` and exactly one `destination` are required; the last
argument is always the destination.

| Flag | Shorthand | Description |
|---|---|---|
| `--archive` | `-a` | archive mode |
| `--verbose` | `-v` | increase verbosity |
| `--compress` | `-z` | compress data during transfer |
| `--recursive` | `-r` | recurse into directories |
| `--dirs` | `-d` | list directories without recursing into them (implied by `-r`) |
| `--dry-run` | `-n` | show what would be transferred |
| `--delete` | | delete extraneous files from destination |
| `--progress` | | show progress during transfer |
| `--exclude PATTERN` | | exclude matching files (repeatable) |
| `--include PATTERN` | | include matching files (repeatable) |
| `--filter RULE` | | add a filter rule (repeatable) |
| `--exclude-from FILE` | | read exclude patterns from FILE, one per line (repeatable) |
| `--include-from FILE` | | read include patterns from FILE, one per line (repeatable) |

All five filter-related flags share one ordered rule list - their relative
order on the command line is preserved, matching rsync's first-match-wins
semantics. See [Filter Rules](#filter-rules) below.

## File Enumeration

`sync.Walk`, in the `internal/sync` package, recursively lists a source tree
into a sorted `[]FileEntry` (path, size, mtime, mode, uid/gid, symlink
target). Symlinks are captured via `Lstat`, never followed.

`-r`/`-d` control how far it descends:

| Recursive | Dirs | Result |
|---|---|---|
| off | off | directories skipped entirely |
| off | on | directories listed, not descended into |
| on | any | full recursion |

On Windows, `UID`/`GID` are always `0` - there's no POSIX ownership concept
to read, so `0` means "unavailable," not a real value.

## Filter Rules

`sync.CompileRules` turns the ordered `--exclude`/`--include`/`--filter`/
`--exclude-from`/`--include-from` list into ready-to-match rules;
`sync.Included`/`sync.FilterEntries` apply them to `sync.Walk`'s output as
a separate pass, first-match-wins, defaulting to include when nothing
matches.

Pattern syntax: `*` matches within one path segment, `**` crosses segment
boundaries, `?` matches one character. A trailing `/` makes a pattern match
directories only. `--filter` also accepts `merge FILE` to inline another
rule file at that point in the list (one level deep - a merge file that
itself tries to merge another file is an error, not silently ignored).

A pattern anchors to the transfer root - matched once against the full
path, not tried at every depth - if it has a leading `/`, contains any
other `/`, or contains `**`. Only a pattern with none of those (a bare
filename like `*.log`) matches at any depth, against the final path
component only. This matches real rsync's actual anchoring rule.

## Delta-Transfer Algorithm

`internal/sync` implements rsync's signature-based delta algorithm for
transferring a changed file without resending the parts that didn't
change:

1. **Signature** (`sync.GenerateSignature`) - the receiver splits its copy
   of the file into fixed-size blocks and computes two checksums per
   block: a fast rolling checksum and an MD5 strong checksum.
2. **Delta** (`sync.GenerateDelta`) - the sender slides a window over its
   new copy of the file one byte at a time, using the rolling checksum to
   cheaply test every offset (not just block boundaries) for a match
   against the receiver's signature; a weak-checksum hit is confirmed
   against the strong checksum before being trusted, since two different
   blocks can share a weak checksum by chance. The result is an ordered
   list of operations: copy block N from the old file, or write these
   literal bytes.
3. **Reconstruction** (`sync.ApplyDelta`) - the receiver replays that
   operation list against its old copy to reproduce the sender's file
   exactly.

The block size is currently a fixed constant (`sync.DefaultBlockSize`).
Real rsync scales it dynamically based on file size; fixed-size blocks are
a deliberate simplification here, not a limitation of the algorithm
itself.

## File Attribute Preservation

`sync.ApplyAttributes(entry, destPath, opts)` applies `--perms`/`--times`/
`--owner`/`--group`/`--links` to an already-written destination path
(`AttrOptions` mirrors each flag so they can be toggled independently, the
same way `--archive` bundles several together). Hard links and device
files are handled separately, described below, since both are inherently
multi-file or multi-privilege concerns a single-entry function can't
capture.

- **Permissions**: only the permission bits (`Mode.Perm()`) are applied,
  never the type bits `FileMode` also carries.
- **Times**: `mtime` is preserved; `atime` is set to the same value rather
  than "now," so reapplying identical attributes twice is idempotent
  (rsync itself doesn't meaningfully preserve atime either).
- **Ownership**: applied via `Lchown` (so a symlink's own ownership is set,
  not its target's) - but only when `FileEntry.OwnershipAvailable` is
  true. On Windows it never is, so ownership is always explicitly skipped
  there, not silently attempted with a meaningless zero UID/GID. Changing
  ownership to another user is a privileged operation on POSIX (needs
  root/`CAP_CHOWN`) even when available - that's an operational
  constraint on how grsync is run, not something this code works around.
- **Symlinks**: created from `LinkTarget` directly, never followed.
  `Perms`/`Times` are silently *not* applied to a symlink entry even if
  requested, because `os.Chmod`/`os.Chtimes` both follow symlinks (Go has
  no portable `Lchmod`/`Lchtimes`) - calling them on a symlink path would
  modify its target instead of the link.
- **Hard links** (`sync.DetectHardLinks`/`sync.ApplyHardLinks`): detected
  via `(device, inode)` identity, POSIX-only. Windows has no equivalent
  exposed the way this package reads it, so every file is treated as
  unlinked there - space-saving is lost, but output is still correct.
  Call `sync.HardLinksSupported()` to tell "this platform can't detect
  hard links" apart from "this tree just has none," rather than guessing
  from an empty result.
- **Device/special files** (`sync.ApplySpecialFile`): deliberately scoped
  down. Named pipes (FIFOs) are fully created via `Mkfifo`, since that
  needs no elevated privilege. Sockets and character/block devices are
  *detected* (`sync.ClassifySpecialFile`) but not recreated -
  `Mknod`-based device creation needs root and can't be meaningfully
  tested without it, so this returns `sync.ErrSpecialFileUnsupported`
  rather than attempting a syscall that fails for most callers and
  calling that "support."

## Architecture

- `cmd/grsync` - CLI entrypoint.
- `internal/cli` - flag/argument parsing (built on cobra).
- `internal/sync` - file-list generation, filter matching, the
  delta-transfer algorithm, and attribute preservation today; wiring
  these together into an actual sync comes later.
- `internal/transport` - (placeholder) data movement, local and remote.

Goal: full feature parity with upstream rsync, including protocol/format
interoperability where specified (e.g. batch mode's file format).

## License

[GNU General Public License v3.0](LICENSE)
