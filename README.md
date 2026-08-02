# grsync

An rsync-inspired file synchronization tool written in Go.

## Status

**grsync can actually sync files now** - local-to-local and local-to-remote
(SSH) - for the first time in this project's history, not just a
collection of independently tested components. `grsync SRC... DEST`
really walks, filters, diffs, transfers, and reconstructs files, applying
requested attributes along the way. See
[End-to-End Sync Pipeline](#end-to-end-sync-pipeline) below for exactly
how the pieces connect and, just as importantly, what's still explicitly
out of scope (partial/append transfers, batch mode, full `--delete`, and
device/special files) - this is real, working sync, not yet full feature
parity. Hard links *are* now preserved, opt-in via `-H`/`--hard-links`
exactly like real rsync's own flag (see
[File Attribute Preservation](#file-attribute-preservation) below), and
`--dry-run`/`-n` is a genuine trial run - full planning, zero filesystem
changes - with real `--itemize-changes`/`-i` output matching rsync's own
format (see [Dry-Run Mode](#dry-run-mode) below). `--progress` and
`--stats` are now implemented too, both matching real rsync's own output
formats (see [Progress and Stats](#progress-and-stats) below).
`--compress`/`-z` now genuinely compresses a file's literal delta data
with zlib, including real rsync's own `--compress-level` and
`--skip-compress` (see [Compression](#compression) below).

`grsync --daemon` also now speaks a real subset of the rsync daemon
protocol - `rsyncd.conf` parsing, the `@RSYNCD` greeting/handshake, module
listing, and real MD4 challenge-response authentication all match
upstream rsync, verified against its actual source rather than assumed.
**`rsync://host/module` is now a real destination argument to the main
`grsync SRC... DEST` command itself**, not just something exercised from
`internal/daemon`'s own tests - `grsync -a src rsync://host/module` works
today, credentials and all. See
[rsync Daemon Mode](#rsync-daemon-mode) below for exactly what that
covers and where it hands off to grsync's own (non-rsync-wire-format)
transfer protocol.

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
| `--verbose` | `-v` | print each updated item's path (superseded by `--itemize-changes` when both are given, see [Dry-Run Mode](#dry-run-mode)) |
| `--compress` | `-z` | compress data during transfer |
| `--recursive` | `-r` | recurse into directories |
| `--dirs` | `-d` | list directories without recursing into them (implied by `-r`) |
| `--dry-run` | `-n` | perform a trial run: full planning (file list, filters, deltas), zero filesystem changes (see [Dry-Run Mode](#dry-run-mode)) |
| `--itemize-changes` | `-i` | print a change-summary line per updated item, real rsync's own 11-character `%i` format (see [Dry-Run Mode](#dry-run-mode)) |
| `--delete` | | delete extraneous files from destination |
| `--progress` | | show progress during transfer |
| `--exclude PATTERN` | | exclude matching files (repeatable) |
| `--include PATTERN` | | include matching files (repeatable) |
| `--filter RULE` | | add a filter rule (repeatable) |
| `--exclude-from FILE` | | read exclude patterns from FILE, one per line (repeatable) |
| `--include-from FILE` | | read include patterns from FILE, one per line (repeatable) |
| `--rsh COMMAND` | `-e` | remote shell to use for SSH transport, e.g. `"ssh -p 2222 -i key.pem"` (default: `ssh`) |
| `--perms` | `-p` | preserve permissions (implied by `--archive`) |
| `--times` | `-t` | preserve modification times (implied by `--archive`) |
| `--owner` | `-o` | preserve owner (implied by `--archive`; requires appropriate privileges) |
| `--group` | `-g` | preserve group (implied by `--archive`; requires appropriate privileges) |
| `--links` | `-l` | recreate symlinks as symlinks (implied by `--archive`) |
| `--hard-links` | `-H` | preserve hard links between files in the source (**NOT** implied by `--archive`, matching real rsync's own `-a`) |
| `--daemon` | | run as an rsync-protocol daemon, serving modules from `--config` (see [rsync Daemon Mode](#rsync-daemon-mode)) |
| `--config PATH` | | path to the `rsyncd.conf` to serve (required with `--daemon`) |
| `--port PORT` | | TCP port to listen on in `--daemon` mode (default `873`, matching rsync) |
| `--password-file FILE` | | read an `rsync://` daemon password from FILE instead of `RSYNC_PASSWORD` or an interactive prompt (see [rsync Daemon Mode](#rsync-daemon-mode)) |

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

  **Wired into the actual sync pipeline, opt-in via `-H`/`--hard-links`**
  - exactly matching real rsync's own flag, including that `--archive`
  does *not* imply it (real rsync's `-a` is `-rlptgoD`, no `H`; see
  `effectiveAttrOptions` in `internal/cli/sync.go`). Only when requested,
  `pipeline.Sender` runs `sync.DetectHardLinks` over the filtered file
  list (skipping the call entirely, not just discarding its result, when
  `sync.HardLinksSupported()` is false - no point paying for a per-entry
  `Lstat` pass that would always come back empty either way) and attaches
  the grouping to the same `FrameFileList` message the entries themselves
  travel in, rather than a separate round trip. `pipeline.Receiver`
  writes each group's first member through the normal signature/delta
  path and recreates every other member with `sync.ApplyHardLinks`
  (`os.Link`) instead of re-transferring bytes that are identical by
  definition - in a pass that runs after every regular file is written
  but *before* the deferred directory-attributes pass, since `os.Link`
  touches its parent directory's mtime the same way creating any other
  file does. Detection only has to succeed on the *sending* side:
  `os.Link` itself works on Windows too, so a Linux/macOS sender pushing
  to a Windows destination still produces real hard links there, even
  though a Windows source's own links can't be detected in the first
  place.

  Tested end to end with `TestSenderReceiver_HardLinks` in
  `internal/pipeline/pipeline_test.go` (proving the destination files are
  genuinely the same file, not independent copies with matching content
  - and that this degrades to correct independent copies, not an error,
  where `HardLinksSupported()` is false),
  `TestSenderReceiver_HardLinksNotPreservedWithoutOptIn` (the direct
  proof that omitting `-H` really does leave files unlinked, not just
  documented as opt-in while actually running unconditionally),
  `TestReceiver_AppliesHardLinksFromReceivedGroups` (proving `Receiver`
  never asks for a signature for a group's secondary member, regardless
  of what the current platform can detect), and - at the real CLI command
  level, in `internal/cli/sync_test.go` -
  `TestE2E_HardLinksPreservedWithFlag`/`TestE2E_ArchiveAloneDoesNotImplyHardLinks`,
  the latter being the specific regression test for `--archive` never
  silently turning this on.
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
  protocol instead of doing a normal sync. It now runs the handshake
  *and* the full receiver pipeline (see
  [End-to-End Sync Pipeline](#end-to-end-sync-pipeline)) against the
  destination path passed as its one positional argument.
- **Remote invocation assumes `grsync` is on the remote `PATH`**, exactly
  like real rsync assumes `rsync` is (no `--rsync-path`-equivalent
  override exists yet). `internal/cli/syncToRemote` invokes the remote
  side as `ssh ... grsync --server DEST`, not a locally-resolved path -
  this is a real, documented deployment assumption, not an oversight.

**Testing note**: two tests exercise real `ssh` against `127.0.0.1`
(`TestSSHLocalhost_HandshakeRoundTrip` in `internal/transport`,
`TestSSHLocalhost_SyncRoundTrip` in `internal/pipeline`, the latter
built the real binary and driving a full sync through it), both skipping
gracefully if no SSH server is reachable there non-interactively. No such
server was available in this development environment (an `ssh` client is
present, but nothing was listening), so while both are believed correct
by code review and by mirroring an already-working pattern, neither has
been observed to pass against a live server.

## End-to-End Sync Pipeline

`internal/pipeline` is the new package that wires `internal/sync` and
`internal/transport` together into an actual sync - it imports both, and
neither of them imports it or each other, keeping that layering intact.
`pipeline.Sender`/`pipeline.Receiver` run the same protocol whether the
connection is a real SSH `Session` or, for a local-to-local sync, an
in-memory `io.Pipe` with both sides running as goroutines in the same
process - deliberately one code path, not two, so the local case
(fast to test, no SSH required) exercises the exact logic the harder-to-
verify remote case depends on.

**Wire encoding**: every message is `encoding/gob`, not upstream rsync's
actual wire protocol. That's a deliberate scope boundary: rsync's real
format is an intricate, versioned binary protocol, and reimplementing it
is separate, substantial work that doesn't belong in this already-large
integration ticket. gob is a reasonable, low-effort, *correct* choice
specifically because grsync only ever talks to grsync here, never to real
rsync - but genuine rsync protocol interoperability, if ever wanted, is
future work, not something to let "protocol" quietly come to mean "gob."

**Three new frame types**, added to `transport.FrameType` (in
`internal/transport`):
`FrameFileList` (sender→receiver, the filtered `[]FileEntry`, sent once),
`FrameSignature` (receiver→sender, per regular file, sent proactively in
list order - no separate request message, since the file list itself is
the implicit request for all of them), and `FrameDelta` (sender→receiver,
the reply to each signature). Directories and symlinks never enter this
exchange at all: a directory has no byte content to diff, and a symlink's
entire "content" is its `LinkTarget`, already present in the file list
itself - only regular files need a signature/delta round trip.

**A real correctness issue found and fixed during this pass, not just a
checklist item**: applying a directory's attributes (permissions, mtime)
immediately upon creating it is wrong, because writing files into it
afterward changes it again - a restrictive permission mode would block
creating those children at all, and even a permissive mode's mtime gets
silently bumped by the filesystem the moment something is created inside
it, undoing the preservation just performed. `pipeline.Receiver` defers
directory attribute application to a final pass, applied deepest-first
(the reverse of `Walk`'s own parent-before-child sort, obtained for free
rather than needing a second sort). `TestSenderReceiver_DirectoryAttributesSurviveChildCreation`
proves this: verified to actually fail (mtime bumped to "now") when the
fix is reverted, not just pass trivially.

**The new-file case** (nothing exists yet at the destination) needs no
special-casing: `sync.GenerateSignature` on empty/absent data naturally
produces a `Signature` with zero blocks, which makes `sync.GenerateDelta`
emit an all-`DataOp` delta - exactly the desired behavior, falling
straight out of the existing SC-3 API.

**A destination-only file is never touched**: `pipeline.Receiver` only
ever acts on paths that appear in the list it received from the sender -
there's no separate destination-side walk to reconcile against it, so
nothing in this pipeline can delete or modify a file the sender never
mentioned. (Full `--delete` semantics remain a separate, later ticket.)

**Explicitly out of scope for this pipeline** (some pre-existing gaps,
restated here so they're not mistaken for oversights specific to this
ticket): compression, progress reporting, partial/append transfers,
batch mode, pulling from a remote source (only local-source syncs are
supported - push, not pull), and device/special files.
`sync.ApplySpecialFile` exists and is tested, but nothing in
`pipeline.Receiver` calls it yet - unlike hard links (see
[File Attribute Preservation](#file-attribute-preservation) above, now
wired in), this needs elevated privilege to test meaningfully and was
left out rather than expanding this integration further.

## Dry-Run Mode

`--dry-run`/`-n` is a genuine trial run, not the flag-echoing placeholder
it used to be: `pipeline.Receiver` performs every planning step exactly
as a real sync would - the full signature/delta exchange, hard-link
grouping, comparing each entry against the destination's current
state - and simply skips the eight calls that would actually touch the
filesystem. Every other write path (`Sender`, and everything upstream of
`Receiver`) is completely unaffected by dry-run; only the receiving
side's own final commit step is skipped, exactly where the "no changes"
guarantee actually needs to be enforced.

### The eight audited write calls

`Receiver`'s own doc comment names them explicitly, and each one is
individually gated behind `if !ropts.DryRun`, not a single outer branch
that happens to skip the whole function (which would also have skipped
the planning work the ticket requires to stay real):

- Two `os.MkdirAll` calls and an `os.WriteFile` in `receiveRegularFile`.
- An `os.MkdirAll` and `sync.ApplyAttributes` in `receiveSymlink` -
  `ApplyAttributes` is the one that actually calls `os.Symlink` for a
  symlink entry (see [File Attribute Preservation](#file-attribute-preservation)),
  not just `chmod`/`chtimes`, which is exactly why skipping it alone
  (not a separate `os.Symlink` call) is what makes a symlink entry a
  genuine no-op in dry-run.
- `sync.ApplyAttributes` for a directory, in the deferred
  attribute-application pass.
- `sync.ApplyHardLinks`, in the hard-link pass.

`TestReceiver_DryRunMakesNoFilesystemChanges` in
`internal/pipeline/pipeline_test.go` is the test built specifically to
catch a regression here: it syncs a tree exercising every one of those
eight paths (a top-level file, a nested directory with a file inside it,
a symlink, and - best-effort - two hard-linked files) against a
completely empty destination and asserts the destination is *still*
completely empty afterward, not just that no error was returned.

### What still runs, and why

The full signature/delta exchange happens over the wire in dry-run mode
exactly as it would for a real sync - **this is a deliberate divergence
from real rsync's own documented dry-run behavior**, worth stating
explicitly rather than glossing over. Real rsync's docs say a dry run
"does not send the actual data for file transfers," but that's only true
because real rsync's *default* mode has a cheap size+mtime "quick check"
that skips the signature/delta algorithm entirely for files it can
already tell are unchanged - grsync has no such shortcut (a pre-existing,
deliberate architectural choice from the original delta-transfer and
pipeline-integration work: full delta always runs, full stop). Matching
real rsync's dry-run network behavior *by letter* would mean building a
new quick-check mechanism that doesn't otherwise exist in this codebase,
just to serve this one mode - worse than disclosing the honest
divergence. The content comparison itself is genuinely free either way:
`sync.ApplyDelta` is pure in-memory work, so computing it costs nothing
whether or not the result is about to be written, and it's the only way
grsync can correctly answer "did this file's content actually change"
for itemize purposes without a `--checksum`-style flag.

### `--itemize-changes`/`-i`: real rsync's actual format, not an approximation

Verified against `rsync.1`'s own `--itemize-changes` section rather than
invented: the format is `YXcstpoguax`, 11 characters - `Y` (update type:
`>` transferred, `c` local change/creation, `h` hard link, `.` not
updated), `X` (file type: `f`/`d`/`L`), then 9 attribute letters
(`c` checksum/value-differs, `s` size, `t` time, `p` perms, `o` owner,
`g` group, `u`/`n`/`b` atime/crtime, `a` ACL, `x` xattr) - `.` for
unchanged, `+` for newly-created. A completely unchanged item is not
printed at all with a single `-i`, matching real rsync's own default
(a second `-i`/`-vv` would show them too; grsync doesn't implement that
second tier). Output lines match real rsync's own default
(`--out-format='%i %n%L'`): the code, the path, and `" -> target"` for a
symlink.

Scoped to what grsync actually tracks, each disclosed explicitly rather
than silently approximated:

- **`u`/`a`/`x` (atime/crtime, ACL, xattr) are always `.`** - grsync has
  no `--atimes`/`--acl`/`--xattr` flags, matching real rsync's own
  behavior when those options aren't given.
- **Attribute-`c` (checksum) never fires for a regular file** - real
  rsync gates it behind `--checksum`, which grsync doesn't have; only
  `s`/`t`/`p`/`o`/`g` are ever used to describe what changed about a
  file's content or attributes, matching this ticket's own explicit
  scope.
- **`h` (hard-link secondary member) is implemented even though the
  ticket's own explicit list didn't name it** - SC-18 already computed
  exactly this grouping information, and reporting a hard-linked file as
  an ordinary new file (`>f+++++++++`) would be a real, easily-avoidable
  inaccuracy.

`--verbose`/`-v` alone (without `-i`) prints just the path (plus
`" -> target"` for a symlink) per changed item - real rsync's own
`"%n%L"` default for `-v` without `-i`; `-i` takes precedence when both
are given.

Verified against real rsync's own documented guarantee ("The output of
`--itemize-changes` is supposed to be exactly the same on a dry run and
a subsequent real run") by `TestReceiver_DryRunItemizeMatchesRealRunItemize`
and its CLI-level counterpart `TestE2E_DryRunAndRealRunItemizeMatch`:
both compare a dry run's itemize output against a real run's, on a
*separate*, equally fresh destination - not a second real run against
the same one, which would legitimately report nothing left to do and
prove nothing about the dry run's accuracy.

### Across transports

- **Local**: `pipeline.Receiver` runs in-process; `ReceiverOptions`
  (dry-run, itemize, verbose, and where to write it) is passed straight
  through.
- **SSH**: the remote `grsync --server` process is invoked with
  `--dry-run`/`--itemize-changes`/`--verbose` as ordinary flags on its
  own command line (see `syncToRemote` in `internal/cli/sync.go`) - no
  new wire protocol needed, since `--server` mode already parses its own
  real CLI flags. Itemize/verbose *output* needed one small addition:
  `transport.Session` now passes the remote subprocess's stderr through
  to this process's own stderr live, not just buffering it for a
  post-mortem error message - stdout is the framed wire protocol itself
  (see [End-to-End Sync Pipeline](#end-to-end-sync-pipeline)), so
  `runServer` writes its reporting there, never to stdout, and that
  passthrough is what lets it actually reach the local terminal.
  `TestSSHLocalhost_DryRunMakesNoChanges` proves the no-write guarantee
  over a real SSH connection to `127.0.0.1` (skipped gracefully if no
  local `sshd` is reachable, the same as this project's other real-SSH
  tests).
- **`rsync://` daemon**: dry-run's no-write guarantee is fully supported
  in both directions, verified over a real TCP connection by
  `TestDaemon_RealTCP_DryRunPutMakesNoChanges` and
  `TestDaemon_RealTCP_DryRunGetMakesNoChanges`. A download
  (`DirectionGet`) needs nothing special - the client's own `Receiver`
  runs locally, exactly like a local sync. An upload (`DirectionPut`)
  needed a small, genuine protocol extension: the client sends
  `"put --dry-run"` instead of `"put"` on the direction line
  (`daemon.dryRunToken`) so the *server's* `Receiver` - the side that
  actually decides whether to write, for this direction - knows to skip
  its writes.

  **Itemize/verbose output is not available for an `rsync://` upload**,
  a real, disclosed gap rather than something silently half-working: once
  the module handshake ends, the daemon connection is pure binary wire
  protocol with no channel for arbitrary text, unlike SSH's genuinely
  separate stderr stream - adding one would be real, separate protocol
  work, not a small extension of this ticket. `--dry-run`'s actual
  no-write guarantee needs no such channel and is unaffected; `grsync`
  prints a one-time note (to stderr) when `-i`/`-v` is combined with an
  `rsync://` destination, rather than silently producing no output and
  leaving the user to wonder why.

## Progress and Stats

`--progress` prints live per-file transfer progress; `--stats` prints an
end-of-sync summary block. Both are opt-in, both are additive to
`-i`/`-v`, and both match real rsync's own output formats - verified
against `rsync.1`'s own documented examples and upstream's actual
`main.c` (`output_summary`) rather than approximated.

### `--progress`

Real rsync's own progress line is fundamentally a *network* measurement:
bytes as they cross the wire, for a protocol that streams a file's data
incrementally. grsync's wire protocol doesn't - see
[End-to-End Sync Pipeline](#end-to-end-sync-pipeline) - it's frame-at-a-time
(a whole signature, then a whole delta, each gob-encoded), and
`sync.ApplyDelta` reconstructs the entire file in memory before anything
is written to disk. There is no partial, in-flight network state to
report progress against. `--progress` here instead measures the local
*disk-write* phase: `receiveRegularFile` writes files larger than 256KiB
in chunks (`writeFileWithProgress`, `internal/pipeline/receiver.go`) and
reports after each chunk, rather than in one `os.WriteFile` call. This is
a deliberate, disclosed scope boundary, not an attempt to fake network
streaming - files at or under 256KiB (most real transfers) report only
once, at completion, since chunking a handful of bytes would add syscall
overhead for a duration too short to ever be visibly "in progress."

Output format matches real rsync's own (`rsync.1`'s own shown examples),
one line per progress update:

```
782448  63%  195.61kB/s    0:00:02
1,238,099 100%  154.76kB/s    0:00:08  (xfr#5, to-chk=169/396)
```

The first form is a mid-transfer update: raw byte count so far, percent
of the file's total size, current transfer rate, and estimated time
remaining (`H:MM:SS`, hours always shown even at zero, matching the man
page's own `"0:00:04"`-style examples). The second is the line printed
when a file finishes: comma-grouped total bytes, `100%`, the achieved
rate, elapsed time for that file, and `(xfr#N, to-chk=M/T)` - this file
was the Nth one actually transferred, with M files left to check out of
T total entries in the sync.

Reporting runs on its own goroutine, fed through a buffered channel
(`progressReporter`, `internal/pipeline/progress.go`): `report()` is a
non-blocking send (`select`/`default`) for *every* update including the
final one, so a slow or entirely absent consumer on the other end of
`Output` can never stall the actual file write - an update is dropped
rather than the transfer blocking on it. `stop()` (deferred immediately
after the reporter is constructed in `Receiver`, so it always runs even
on an early-error return) closes the update channel and waits for the
goroutine's own `run` loop to drain and exit, so no goroutine outlives a
sync. Verified by dedicated concurrency tests
(`TestProgressReporter_ReportDoesNotBlockOnSlowConsumer`,
`TestProgressReporter_StopDoesNotLeakTheGoroutine`) rather than assumed
safe.

`--progress` never fires during `--dry-run`: it specifically measures
bytes committed to disk, and dry-run skips that write entirely, so there
is nothing to report (`TestReceiver_ProgressDoesNotFireDuringDryRun`).
`--stats`, below, has no such restriction.

### `--stats`

Printed once, after the whole sync completes, matching real rsync's own
field names and structure:

```
Number of files: 4 (reg: 3, dir: 1)
Number of created files: 3 (reg: 2, dir: 1)
Number of regular files transferred: 2
Total file size: 1,416 bytes
Total transferred file size: 16 bytes
Literal data: 16 bytes
Matched data: 1,400 bytes
Total bytes sent: 612
Total bytes received: 1,498

sent 612 bytes  received 1,498 bytes  4,220.00 bytes/sec
total size is 1,416  speedup is 0.67
```

- **Number of files / created files**: every entry in the sender's file
  list, broken down by type (`reg`/`dir`/`link`); "created" counts only
  those that did not already exist at the destination. The type
  breakdown omits any type with a zero count (e.g. a sync with no
  symlinks omits `link:` entirely), and the "created files" line itself
  is omitted when nothing was newly created.
- **Number of regular files transferred**: files whose content actually
  changed, *or* were newly created - a brand-new empty file counts here
  even though it has no bytes to compare, which needed a real fix (see
  below); a pre-existing byte-identical file does not.
- **Total file size / Total transferred file size**: sums of `entry.Size`
  across all regular files, and across only the transferred ones.
- **Literal data / Matched data**: bytes coming from the delta as new
  data (`DataOp`) versus copied from the existing destination file
  (`CopyOp`), summed with the same block-boundary math `sync.ApplyDelta`
  itself uses (`deltaByteCounts`, `internal/pipeline/receiver.go`).
- **Total bytes sent / received**: actual wire traffic for this
  connection, measured by wrapping the `io.ReadWriter` passed to
  `Receiver` in a byte-counting decorator (`countingReadWriter`,
  `internal/pipeline/stats.go`) rather than threading a counter through
  every individual send/recv call in `messages.go` - chosen specifically
  so neither `Sender` nor its own tests needed any changes for this
  ticket.
- **speedup ratio**: `total_size / (bytes_sent + bytes_received)`,
  verified against upstream rsync's own `main.c` (`output_summary`)
  rather than guessed, and 0 (not NaN/Inf) when nothing was sent or
  received at all. `(DRY RUN)` is appended to the speedup line under
  `--dry-run`, reusing the same suffix real rsync itself prints.
- **Not present**: no "Number of deleted files" line - grsync has no
  `--delete` (see [Status](#status)) - and no file-list build-time
  fields, since grsync doesn't separately time that phase the way
  upstream rsync's own stats block does.

Unlike `--progress`, `--stats` is fully compatible with `--dry-run`:
every field it reports is derived from planning data (the signature/delta
exchange, which dry-run still performs in full - see
[Dry-Run Mode](#dry-run-mode)) or from wire bytes actually exchanged,
neither of which dry-run skips - only the final disk write is, and stats
doesn't depend on that (`TestReceiver_StatsWorksInDryRun`).

### A real bug this ticket's self-review caught

A brand-new *empty* file has `oldData == nil` (nothing existed before)
and `newData == nil` too - `sync.ApplyDelta`'s accumulator is never
appended to when there are zero delta ops, which is exactly what happens
for a zero-byte file. `bytes.Equal(nil, nil)` is `true`, so a naive
"did the content change" check alone would call a genuinely new empty
file unchanged, undercounting both "files transferred" and the progress
reporter's own `xfr#` counter. Fixed with a separate `transferred :=
!existed || contentChanged` check used specifically for stats/xfer
accounting (`receiveRegularFile`, `internal/pipeline/receiver.go`);
`contentChanged` alone is still what itemize output uses, since
`itemizeFile` already handles the `!existed` case as its own first,
higher-priority branch. Locked in by
`TestReceiver_StatsCountsNewEmptyFileAsTransferred`.

### Across transports

- **Local**: threaded straight through `ReceiverOptions`, same as
  itemize/verbose.
- **SSH**: `--progress`/`--stats` are appended to the remote
  `grsync --server` command line exactly like `--dry-run`/
  `--itemize-changes`/`--verbose` already are (see
  [Dry-Run Mode](#dry-run-mode)'s own "Across transports" section) - no
  new wire protocol needed, and output reaches the local terminal through
  the same live stderr passthrough already established there.
  `TestSSHLocalhost_ProgressAndStatsDoNotBreakTheTransfer` proves this
  over a real SSH connection to `127.0.0.1` (skipped gracefully without a
  local `sshd`).
- **`rsync://` daemon, download (`DirectionGet`)**: works exactly like a
  local sync - the client's own `Receiver` runs locally, with a real
  channel (this process's own stdout/wherever `Output` points) to print
  to. Verified over a real TCP connection by `TestDaemon_RealTCP_StatsWorkForGet`.
- **`rsync://` daemon, upload (`DirectionPut`)**: **the same disclosed gap
  `--itemize-changes`/`--verbose` already have, not a new one.** Once the
  module handshake ends, the daemon connection is pure binary wire
  protocol with no channel for arbitrary text (see
  [Dry-Run Mode](#dry-run-mode)'s own explanation of why) - the *server's*
  `Receiver`, which is the side actually applying the upload, has no way
  to get progress or stats text back to the uploading client. `grsync`'s
  existing one-time stderr note for this case already covers
  `--progress`/`--stats` alongside `-i`/`-v`
  (`internal/cli/sync.go`'s daemon-PUT warning). Confirmed harmless by
  `TestDaemon_RealTCP_PutIgnoresProgressAndStatsButStillWorks`: the
  client's own `ReceiverOptions{Progress: true, Stats: true}` is silently
  inert for this direction (`Sender` never even looks at
  `ReceiverOptions`), and the upload itself still completes correctly.

## Compression

`--compress`/`-z` compresses a file's literal delta data with zlib
before it crosses the wire; `--compress-level` controls how hard, and
`--skip-compress` excludes already-compressed file types. All three
match real rsync's own semantics, verified against upstream's actual
source (`token.c`, `RsyncProject/rsync`) and `rsync.1`'s own documented
wording rather than assumed.

### What gets compressed, and what doesn't

Only a delta's literal data - the bytes a `DataOp` carries because they
didn't match anything in the receiver's signature - is ever compressed.
`CopyOp`s (plain block-index references, not data) and every
`FrameSignature` (checksums) are never touched, matching the ticket's
own scope exactly.

Rather than compressing each `DataOp` independently, `Sender` (via
`toWireDeltaOps`, `internal/pipeline/messages.go`) concatenates *all* of
one file's literal data into a single buffer and zlib-compresses that as
one unit per `FrameDelta` message, with a single `Compressed bool`
marker on the message itself; each `DataOp`'s own wire form then carries
only how many of the decompressed stream's bytes are its
(`wireDeltaOp.Length`), so the receiver can re-slice it back apart after
one decompression. This amortizes zlib's fixed ~8-byte header/trailer
overhead across a whole file instead of paying it again for every
separate literal run a scattered-changes file can produce - closer to
real rsync's own `zlibx` compression choice (one persistent per-file
deflate stream with matched data excluded from it) than compressing
op-by-op would have been. If the compressed result isn't actually
smaller than the raw literal data - realistic for a file whose total
changed content is tiny, exactly the case delta transfer exists for, or
for already-incompressible data that slipped past `--skip-compress` -
the file is sent uncompressed instead; `Compressed` is a real, checked
outcome, not just a request.

Compression is entirely a **sending-side** decision. `Receiver` takes no
compression-related options of its own at all: it simply decompresses
whatever each `deltaMessage.Compressed` marker says, on every transport,
which is possible because grsync only ever pushes (see
[Status](#status)) - `Sender` always runs on the local, requesting
process, never remotely, for every path currently wired into the CLI.

### `--compress-level`

Verified against real rsync's own source (`token.c`'s
`init_compression_level`) and `rsync.1`'s own documented wording: for
zlib compression, valid levels are **1 (fastest) to 9 (smallest), with 6
as the default**. `--compress-level=0` explicitly turns compression off
- overriding a `-z` given alongside it - and `--compress-level=-1` means
"use the default." Giving `--compress-level` alone, without `-z`,
implies compression (unless the resulting level is 0), matching real
rsync's own documented "the `--compress` option is implied" rule. An
out-of-range value is silently clamped into `[1, 9]`, matching
`rsync.1`'s own "too-large or too-small value" wording. All of this is
`ClampCompressLevel` (`internal/pipeline/compress.go`) and
`effectiveCompressOptions` (`internal/cli/sync.go`).

### `--skip-compress`

Overrides the built-in list of already-compressed file suffixes
(`gz`, `zip`, `jpg`, `mp3`, `mp4`, and 91 others) that are sent
uncompressed even with `--compress` on, since running zlib over an
already-compressed format wastes CPU for no size benefit. The full
default list is real rsync's own, copied verbatim from `rsync.1`'s own
documented default (`DefaultSkipCompressSuffixes`,
`internal/pipeline/compress.go`) rather than invented. An explicit
`--skip-compress=""` is a meaningful override in its own right ("skip
nothing"), matching real rsync's own documented meaning for it - grsync
tells that apart from "the flag was never given at all" via
`cmd.Flags().Changed`, not `opts.skipCompress`'s zero value, since an
empty string is both.

Matching is a plain, case-insensitive suffix list
(`--skip-compress=gz/jpg/mp3`); real rsync's own `--skip-compress`
grammar additionally supports bracketed character classes inside a
suffix (e.g. `mp[34]`), which grsync's version does not - a deliberate,
disclosed scope reduction, since plain suffixes cover the default list
and the overwhelming majority of real-world uses.

**Worth disclosing**: real rsync's own current documentation (as of this
writing) admits `--skip-compress` "has no effect" in its own latest
implementation, because none of its currently-supported compression
algorithms allow changing level mid-stream - its per-file persistent
deflate context, once opened, keeps compressing everything at the same
level regardless of what the suffix list says. grsync's frame-per-file
design has no such persistent stream to be stuck with: `toWireDeltaOps`
makes a fresh, genuine "compress this file's literal data or don't" call
for every file, so `--skip-compress` actually works here - a real
improvement made possible by the architectural difference, not a silent
divergence from upstream's documented behavior.

### Interaction with `--stats` and `--dry-run`

`--stats`' "Total bytes sent"/"Total bytes received" (see
[Progress and Stats](#progress-and-stats)) already measure genuine wire
traffic via `countingReadWriter`, which wraps the connection itself - so
compressed bytes are reflected automatically, no changes needed for this
ticket. Since `Stats` is computed on the *receiving* side, it's
specifically **"Total bytes received"** that shrinks with compression
for an upload (`Sender` on the far end sends compressed data, this side
receives it) - "Total bytes sent" reflects this side's own small
signature/ack traffic back to the sender, which compression doesn't
touch. `Total file size` is unaffected either way, since it describes
the files themselves, not what crossed the wire.

`--dry-run` and `--compress` compose cleanly: the full signature/delta
exchange - including compressing the delta's literal data - still runs
during a dry run exactly as it would for a real sync (see
[Dry-Run Mode](#dry-run-mode)'s own explanation of why), so itemize
output stays accurate; only the final disk write is skipped, and
compression has nothing to do with that.

### Across transports

- **Local**: `Sender` runs in-process with the `CompressOptions`
  `effectiveCompressOptions` computed from the CLI flags - no different
  from any other in-process call.
- **SSH**: unlike `--dry-run`/`--itemize-changes`/`--verbose`/
  `--progress`/`--stats` (see [Dry-Run Mode](#dry-run-mode)'s own
  "Across transports" section), `--compress` needs **no remote argv
  change at all**: `Sender` runs locally for this transport too, and the
  remote `--server` process's `Receiver` just reacts to each
  `deltaMessage`'s own `Compressed` marker, exactly like every other
  transport. Verified over a real SSH connection to `127.0.0.1` by
  `TestSSHLocalhost_CompressDoesNotBreakTheTransfer` (skipped gracefully
  without a local `sshd`).
- **`rsync://` daemon, upload (`DirectionPut`)**: `Sender` runs on the
  *client* side for this direction (see
  [rsync Daemon Mode](#rsync-daemon-mode)), exactly where `--compress`'s
  decision belongs - no daemon-protocol extension needed, the same way
  SSH needs none. Verified over a real TCP connection by
  `TestDaemon_RealTCP_PutWithCompressUploadsCorrectly`.
- **`rsync://` daemon, download (`DirectionGet`)**: the daemon's own
  `Sender` call for a module download has no CLI wiring at all yet - see
  [Status](#status)'s "push only" scope boundary, already established
  before this ticket - so it always runs with compression disabled
  (`pipeline.CompressOptions{}`), consistent with that existing
  boundary, not a new gap introduced here.

### Hard links

A hard-link group's secondary members never go through the
signature/delta exchange at all - `Sender` and `Receiver` both skip them
outright, recreating the link directly instead (see
[File Attribute Preservation](#file-attribute-preservation)) - so
`--compress` is moot for them by construction, with nothing to compress
in the first place. `TestSenderReceiver_CompressWorksWithHardLinks`
confirms that skip still holds correctly with compression enabled.

## rsync Daemon Mode

`internal/daemon` implements grsync's `--daemon` server mode: a second way
to *reach* a grsync instance, over a plain TCP port instead of SSH,
speaking a real subset of upstream rsync's own daemon protocol rather
than an invented one. Every wire-format detail below (the `rsyncd.conf`
syntax, the `@RSYNCD` greeting, the MD4 authentication algorithm) was
checked against upstream rsync's actual source (`authenticate.c`,
`clientserver.c`) while building this, not reconstructed from memory or
the man page alone.

```sh
grsync --daemon --config rsyncd.conf --port 8730
```

### `rsyncd.conf`

`daemon.ParseConfig` reads real `rsyncd.conf` syntax: `[module]` sections,
`name = value` parameters, `#` comments, blank lines, and trailing-`\`
line continuation. Parameters that appear before any `[module]` header
become that module's starting defaults, matching real rsync's own global
section.

Supported parameters:

| Parameter | Default | Meaning |
|---|---|---|
| `path` | *(required)* | directory the module exposes |
| `comment` | *(empty)* | shown next to the module name in a listing |
| `read only` | `true` | rejects uploads (`put`) to this module |
| `list` | `true` | hides the module from a `#list` request - it is *not* made unreachable; a client who already knows its name can still select it directly, matching real rsync's own documented behavior |
| `exclude` | *(none)* | space-separated patterns hidden from downloads, compiled via the same `sync.CompileRules`/`sync.Included` machinery `--exclude` uses |
| `auth users` | *(none)* | comma/space-separated usernames; a non-empty list means the module requires authentication |
| `secrets file` | *(none)* | path to a `name:password` per-line file |
| `max connections` | `0` (unlimited) | **parsed but not yet enforced** - see Scope boundaries below |

An unrecognized parameter (real rsyncd.conf has dozens grsync doesn't
implement - `uid`, `hosts allow`, `log file`, `timeout`, and more) is
silently accepted, not an error: rejecting an otherwise-valid config file
over one unimplemented option would be worse than ignoring that line. A
line that isn't valid `name = value` or `[section]` syntax at all, or a
recognized parameter with a malformed value, is a hard parse error.

### `rsync://` URLs

`daemon.ParseURL` parses `rsync://[user@]host[:port]/module[/path]`,
including the bare `rsync://host` and `rsync://host/` forms real rsync
uses to mean "list this daemon's modules" rather than selecting one, and
IPv6 literals.

**`rsync://host/module` is a real destination argument to the main
`grsync SRC... DEST` command**: `grsync -a src rsync://host/module`
dials the daemon over plain TCP, runs the real handshake/auth below, then
uploads through the same `pipeline.Sender` every other destination uses.
`internal/cli`'s `isRsyncURL` distinguishes this from a local path or an
SSH `user@host:path` by its `rsync://` prefix, checked before either of
those is - `transport.ParseRemotePath` itself now also refuses anything
containing `"://"`, so an `rsync://` URL is never valid SSH syntax by
construction (a real ambiguity found and fixed while wiring this up: it
used to parse as host `"rsync"`, path `"//host/module"`).

Only pushing to a module (`grsync SRC rsync://host/module`) is supported,
matching the existing SSH-transport restriction rather than introducing a
new asymmetry - pulling *from* an `rsync://` source is rejected with a
clear error, the same as pulling from an SSH source already is. A
destination URL also can't yet target a sub-path within a module
(`rsync://host/module/subdir`) - the daemon protocol itself (see below)
only supports syncing an entire module, and that's explicitly out of
scope to change here; grsync rejects this case with a clear error rather
than silently ignoring the sub-path.

**Credentials**: verified against real rsync's actual documented
behavior (`rsync.1`'s "RSYNC_PASSWORD" and "--password-file" sections)
rather than invented. The username is the URL's own `user@` part, else
`USER`, else `LOGNAME` (`USER` wins if both are set), else `"nobody"` -
real rsync's exact resolution order. The password comes from
`--password-file FILE` (or stdin, if `FILE` is `-`) if given, else the
`RSYNC_PASSWORD` environment variable, else an interactive,
non-echoing terminal prompt - also real rsync's exact precedence.
**There is deliberately no `--password` flag**: real rsync has never had
one either, precisely because a password given directly as a
command-line argument is visible to any other user on the same machine
via the process list (`ps`), and an environment variable can leak the
same way on some systems - which is exactly why `--password-file` exists
and why grsync (matching real rsync) refuses a world-readable
`--password-file` (POSIX only; see `checkPasswordFilePermissions`, split
`_unix.go`/`_windows.go` the same way `internal/sync`'s ownership and
hard-link handling already is).

None of this is resolved eagerly: `daemon.PasswordFunc` is only ever
called if the daemon actually sends `AUTHREQD`, exactly matching real
rsync's own `auth_client()`, which is only ever invoked in response to
that same line. Connecting to a module that turns out not to require
authentication never prompts, never reads `--password-file`, and never
consults `RSYNC_PASSWORD` - `TestDialClient_PasswordFuncNotCalledForAnonymousModule`
in `internal/daemon/client_test.go` proves this directly, not just by
absence of a prompt in a passing test. A multi-source sync against the
same daemon destination resolves (and, if it comes to it, prompts) at
most once for the whole invocation, not once per source, even though
each source still gets its own connection (see `DialClient` below).

### Handshake and authentication

The connection sequence matches real rsync: the daemon speaks first
(`@RSYNCD: 31.0`), the client replies with its own greeting, then sends
either `#list` (module listing) or a module name. An unknown module gets
an `@ERROR` line; a `list = false` module is skipped by `#list` but still
selectable by name.

If the selected module has `auth users` configured, the daemon sends
`@RSYNCD: AUTHREQD <challenge>` with a fresh random challenge; the client
answers `<user> <response>` where
`response = base64_no_padding(MD4(secret + challenge))` - secret hashed
first, then challenge, no seed byte, exactly matching real rsync's
`generate_hash()`. **The password itself never crosses the wire, only
this one-way hash of it** - `TestAuth_NoPlaintextPasswordOnWire` in
`internal/daemon/auth_test.go` proves this by capturing and inspecting
the actual bytes each side sends, not just checking the outcome.
Comparison on the server side uses `crypto/subtle` for constant-time
comparison, and every authentication failure (unknown user, wrong
password, unreadable secrets file) returns the same generic
`@ERROR: auth failed on module <name>`, matching real rsync's refusal to
let a client distinguish those cases.

**Scoped down from real rsync, deliberately:**
- **Classic MD4 only** - real rsync protocol 30+ negotiates MD4 vs MD5
  via a digest list in the greeting line; grsync always uses MD4 and
  ignores any digest list it receives. Digest negotiation is real,
  unimplemented scope, not a bug.
- **Exact-match `auth users` only** - real rsync supports wildcards and
  `@group` entries in this list; grsync matches usernames literally.
- **The secrets file's own permissions are never checked** - real rsync's
  default "strict modes" refuses a world- or group-readable secrets file.
  grsync reads whatever `secrets file` points to regardless of its
  permissions. Given this project's cross-platform (including Windows)
  scope, where POSIX permission bits don't map cleanly, this was left
  out rather than half-implemented; worth a follow-up on POSIX platforms.

### Access control and transfer

Once authenticated, the client sends `get` (download) or `put` (upload).
`put` against a `read only` module is refused - with an `@ERROR` line and
without either side ever committing to the transfer protocol - before any
file ever moves. `get` walks and filters the module's `path` through the
module's `exclude` patterns before sending, exactly like `--exclude`
elsewhere in grsync.

**`exclude` is enforced on downloads only.** That's where the daemon
itself walks and filters the directory it's about to send from, the same
mechanism `--exclude` already uses; `pipeline.Receiver` has no per-entry
filtering hook, so an upload to a non-read-only module is not currently
filtered against the module's `exclude` list. A deliberate, documented
boundary, not an oversight.

After a module is selected (and authenticated, if required), this package
hands the connection straight to `pipeline.Sender`/`pipeline.Receiver` -
the same functions the SSH transport uses. **The handshake and
authentication above are real-rsync-protocol-shaped; the transfer that
follows is not.** Like the SSH transport (see
[End-to-End Sync Pipeline](#end-to-end-sync-pipeline)), it's
`encoding/gob`, not upstream rsync's actual binary wire format - so a
grsync daemon interoperates with another grsync client, not with a real
`rsync` binary, exactly the same boundary that already exists for SSH.

Server and client sides are each a single `net.Conn`-in/`error`-out entry
point: `daemon.ServeConn` (accepted connections, used by `Serve`'s accept
loop) and `daemon.DialClient` (outbound connections, used by
`internal/cli`'s `syncToRsyncDaemon`). `DialClient` exists specifically
because `DialGreeting`/`DialAuth`/`DialModule` are built around this
package's own unexported connection type - before `DialClient`, nothing
outside `internal/daemon` could actually call them, a gap found while
wiring the CLI up to this package rather than one planned in advance.

**Other scope boundaries:**
- **`max connections` is parsed but not enforced** - nothing currently
  caps concurrent connections to a module at that number.
- One connection's panic can't take the daemon process down: `Serve`
  recovers per-connection, so a bug anywhere in the handshake/auth/
  transfer chain stays scoped to that one client instead of killing every
  other in-flight connection.
- Every line read before a transfer begins (greeting, module selection,
  auth response) is capped at 8 KiB, so an unauthenticated client can't
  force unbounded memory growth by sending data with no newline.

Tested with `TestDaemon_RealTCP_*` and `TestDialClient_*` in
`internal/daemon`, and end to end through the real CLI command with
`TestE2E_LocalToRsyncDaemon_*` in `internal/cli/rsync_url_test.go` - all
over an actual loopback TCP connection (listen, dial, full handshake,
auth, and transfer), not just in-memory pipes standing in for a
connection, since (unlike the SSH tests) nothing external is needed to
exercise this end to end. `internal/cli`'s tests drive the real
`NewRootCmd()` command, the same way `TestE2E_LocalToLocal` does for a
local sync, against a real daemon started in-process - not
`internal/pipeline` or `internal/daemon` called directly.

## Architecture

- `cmd/grsync` - CLI entrypoint.
- `internal/cli` - flag/argument parsing (built on cobra) and the real
  sync entry point (`sync.go`): local-to-local runs the pipeline
  in-process over an `io.Pipe`, local-to-remote (SSH) spawns and drives it
  over an SSH `Session`, and local-to-`rsync://` (`rsync_url.go`) dials
  the daemon over TCP and drives it through `internal/daemon`'s
  `DialClient`; `credentials.go` resolves the username/password for the
  latter, matching real rsync's own precedence (see
  [rsync Daemon Mode](#rsync-daemon-mode) above).
- `internal/pipeline` - wires `internal/sync` and `internal/transport`
  together into an actual sync; see
  [End-to-End Sync Pipeline](#end-to-end-sync-pipeline) above.
- `internal/sync` - file-list generation, filter matching, the
  delta-transfer algorithm, and attribute preservation.
- `internal/transport` - remote endpoint parsing, RSH command
  construction, frame protocol, subprocess session management, and the
  `--server` handshake.
- `internal/daemon` - the rsync daemon protocol, both sides: `rsyncd.conf`
  and `rsync://` URL parsing, the `@RSYNCD` greeting/handshake and module
  listing, MD4 challenge-response authentication, and per-module access
  control, handing off to `internal/pipeline` for the actual transfer via
  `ServeConn` (server) and `DialClient` (client). See
  [rsync Daemon Mode](#rsync-daemon-mode) above.

Goal: full feature parity with upstream rsync, including protocol/format
interoperability where specified (e.g. batch mode's file format).

## License

[GNU General Public License v3.0](LICENSE)
