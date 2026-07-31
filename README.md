# grsync

An rsync-inspired file synchronization tool written in Go.

## Status

**CLI skeleton only — no sync logic yet.** The command-line interface parses
arguments and flags and prints what it received, but no files are actually
read, compared, transferred, or deleted. `internal/sync` and
`internal/transport` are currently empty packages reserved for that work.

## Build

Requires Go (see [go.mod](go.mod) for the version this module targets).

```sh
go build ./cmd/grsync
```

This produces a `grsync` (or `grsync.exe` on Windows) binary in the current
directory. To run without building a binary:

```sh
go run ./cmd/grsync <source>... <destination> [flags]
```

## Usage

```
grsync <source>... <destination> [flags]
```

At least one `source` and exactly one `destination` are required
(the last positional argument is always the destination), matching rsync's
own `SRC... DEST` grammar rather than restricting to a single source.

| Flag | Shorthand | Description |
|---|---|---|
| `--archive` | `-a` | archive mode (equivalent to common rsync defaults) |
| `--verbose` | `-v` | increase output verbosity |
| `--compress` | `-z` | compress file data during transfer |
| `--recursive` | `-r` | recurse into directories |
| `--dry-run` | `-n` | show what would be transferred without transferring |
| `--delete` | | delete extraneous files from destination |
| `--progress` | | show progress during transfer |
| `--exclude PATTERN` | | exclude files matching PATTERN (repeatable) |
| `--include PATTERN` | | include files matching PATTERN (repeatable) |
| `--filter RULE` | | add a file-filtering RULE (repeatable) |

`--exclude`, `--include`, and `--filter` are collected into a single ordered
rule list, not three independent lists — their relative order on the command
line is preserved regardless of which of the three flags produced each rule.
This matches rsync's first-match-wins filter semantics, where rule order is
significant.

At this stage, running the command only echoes the parsed values back — it
does not transfer any files:

```sh
$ go run ./cmd/grsync ./src1 ./src2 ./dst -av --exclude "*.log" --include "keep.log"
sources:     [./src1 ./src2]
destination: ./dst
archive:     true
verbose:     true
...
filters:     exclude:*.log, include:keep.log
```

## Architecture

grsync follows the same conceptual split as upstream
[rsync](https://rsync.samba.org/): a **sync layer** that decides *what* needs
to change (comparing source and destination file trees, applying
include/exclude/filter rules) and a **transport layer** that handles *how*
data actually moves (local copies today, with room for remote transports
later). The codebase mirrors that split:

- `cmd/grsync` — CLI entrypoint (binary wiring only).
- `internal/cli` — argument/flag parsing (built on
  [spf13/cobra](https://github.com/spf13/cobra)); owns no sync or transport
  logic.
- `internal/sync` — (placeholder) will own comparison and change-planning
  logic, analogous to rsync's file-list generation and delta algorithm.
- `internal/transport` — (placeholder) will own how bytes are actually moved
  between source and destination.

grsync's goal is full feature parity with upstream rsync, not just
similarity of CLI flags — including protocol- and format-level
interoperability with the C implementation where specified (for example,
batch mode's `--write-batch`/`--read-batch` file format is required to be
interoperable with the C rsync implementation). None of that is implemented
yet: the wire protocol, delta-transfer algorithm, and batch file format are
still to come in later tickets. This README will be updated as each piece
lands.

## License

[GNU General Public License v3.0](LICENSE)
