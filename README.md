# grsync

An rsync-inspired file synchronization tool written in Go.

## Status

CLI parsing, file enumeration, filter-rule matching, the delta-transfer
algorithm, file attribute preservation, and the SSH transport primitives
are implemented; nothing is wired together into an actual sync yet.
`internal/sync` can list a source tree (`sync.Walk`), filter it
(`sync.FilterEntries`), compute/apply binary deltas between two versions
of a file (`sync.GenerateDelta`/`sync.ApplyDelta`), and apply
permissions/times/ownership/symlinks/hard links (`sync.ApplyAttributes`
and friends). `internal/transport` can parse remote endpoints, spawn and
frame a connection to a remote `grsync --server`, and complete a minimal
handshake over it. But the CLI's normal sync path still only echoes parsed
flags - none of this is wired into an actual end-to-end sync yet (see
[SSH Transport](#ssh-transport) below for exactly what that gap is).

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
| `--rsh COMMAND` | `-e` | remote shell to use for SSH transport, e.g. `"ssh -p 2222 -i key.pem"` (default: `ssh`) |

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

## SSH Transport

`internal/transport` reaches a remote grsync the same way upstream rsync
does: by spawning a remote-shell subprocess (`ssh` by default) and
speaking a protocol over its stdin/stdout, rather than a native Go SSH
client. This was a deliberate choice, not just the default option: the
`--rsh`/`-e` flag's real meaning in rsync only makes sense if grsync is
actually invoking an arbitrary shell command, and shelling out means
`~/.ssh/config`, `ssh-agent`, and `known_hosts` all keep working exactly
as already configured, instead of being reimplemented.

- **Endpoint syntax**: `transport.ParseRemotePath` recognizes
  `[user@]host:path`, including IPv6 literals (`user@[::1]:path`), while
  correctly treating a Windows drive letter (`C:\...`) as local rather
  than a remote host.
- **`--rsh`/`-e`** is the *only* customization mechanism for the remote
  shell - there's no separate `--port` or `--identity` flag. This matches
  real rsync: upstream's `--port` only applies to daemon-mode (`rsync://`)
  connections, and its `-i` flag already means `--itemize-changes`, not
  "identity file." Port/identity/`ProxyJump`/etc. go through `-e` (e.g.
  `-e "ssh -p 2222 -i key.pem"`) or `~/.ssh/config`, exactly as with real
  rsync.
- **Host-key verification** is not reimplemented at all, in either
  direction: no flag here ever weakens it (no `StrictHostKeyChecking=no`,
  no null `UserKnownHostsFile`), and none of its logic is duplicated
  either. Whatever the invoked shell command does by default is exactly
  what happens - this is genuinely real, not a stub, precisely because
  nothing here touches it.
- **Framing**: `transport.WriteFrame`/`transport.ReadFrame` multiplex the single
  stdin/stdout stream into typed, length-prefixed messages (4-byte
  length + 1-byte type + payload, capped at 64 MiB per frame against a
  corrupt or hostile length prefix).
- **`--server` mode**: hidden from `--help` (like rsync's own `--server`),
  this is how a remotely-invoked grsync switches into speaking the
  protocol instead of doing a normal sync. Right now it implements only a
  minimal handshake (`transport.ServeHandshake`/`transport.Handshake`) - enough to
  prove the subprocess, pipes, and framing work correctly end to end
  through a real `ssh` connection, not a full remote sync.

**What's genuinely missing, not just untested:** nothing in the normal
(non-`--server`) CLI path detects a remote `user@host:path` argument or
calls `transport.Dial`/`transport.Handshake` -
`transport.ParseRemotePath`, `transport.BuildRSHCommand`,
`transport.Dial`, and `transport.Handshake` are all implemented and
independently tested, but not yet invoked from a real sync. Wiring an
actual file-list/signature/delta exchange on top of this frame/session
foundation - so a remote sync really happens - is separately-scoped
follow-up work, not part of this ticket.

**Testing note**: `TestSSHLocalhost_HandshakeRoundTrip` builds the real
`grsync` binary and drives the full `transport.Dial`/`transport.Session`/
`transport.Handshake` path through actual `ssh` against `127.0.0.1`,
skipping gracefully if no SSH server is reachable there
non-interactively. No such server was available in this development
environment (an `ssh` client is present, but nothing
was listening), so while the test is believed correct by code review, it
has not been observed to pass against a live server.

## Architecture

- `cmd/grsync` - CLI entrypoint.
- `internal/cli` - flag/argument parsing (built on cobra).
- `internal/sync` - file-list generation, filter matching, the
  delta-transfer algorithm, and attribute preservation today; wiring
  these together into an actual sync comes later.
- `internal/transport` - remote endpoint parsing, RSH command
  construction, frame protocol, subprocess session management, and a
  minimal `--server` handshake today; the full remote sync pipeline
  (file list/signature/delta exchange) is not wired up yet.

Goal: full feature parity with upstream rsync, including protocol/format
interoperability where specified (e.g. batch mode's file format).

## License

[GNU General Public License v3.0](LICENSE)
