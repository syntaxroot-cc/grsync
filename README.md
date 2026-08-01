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
out of scope (compression, progress reporting, `--dry-run`, partial/
append transfers, batch mode, full `--delete`, hard links, and device/
special files) - this is real, working sync, not yet full feature parity.

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
| `--perms` | `-p` | preserve permissions (implied by `--archive`) |
| `--times` | `-t` | preserve modification times (implied by `--archive`) |
| `--owner` | `-o` | preserve owner (implied by `--archive`; requires appropriate privileges) |
| `--group` | `-g` | preserve group (implied by `--archive`; requires appropriate privileges) |
| `--links` | `-l` | recreate symlinks as symlinks (implied by `--archive`) |
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
ticket): compression, progress reporting, real `--dry-run` (still the
flag-echoing placeholder, to avoid silently performing a real sync when a
dry run was requested), partial/append transfers, batch mode, pulling
from a remote source (only local-source syncs are supported - push, not
pull), and - carried over from SC-8 - hard links and device/special
files. `sync.DetectHardLinks`/`sync.ApplyHardLinks`/`sync.ApplySpecialFile`
exist and are tested, but nothing in `pipeline.Receiver` calls them yet;
wiring them in is a reasonable, low-risk follow-up (hard links
particularly, since unlike device files it needs no elevated privilege)
but was left out of this already-large integration ticket rather than
expanding its scope further.

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
