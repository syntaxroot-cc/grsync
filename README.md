# grsync

An rsync-inspired file synchronization tool written in Go.

## Status

CLI parsing and file enumeration are implemented; data transfer is not.
`internal/sync` builds a sorted file list (`sync.Walk`), but nothing calls
it yet — the CLI only echoes parsed flags — and `internal/transport` is
still empty.

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

`--exclude`/`--include`/`--filter` share one ordered rule list — their
relative order on the command line is preserved, matching rsync's
first-match-wins semantics.

## File Enumeration

`internal/sync.Walk` recursively lists a source tree into a sorted
`[]FileEntry` (path, size, mtime, mode, uid/gid, symlink target). Symlinks
are captured via `Lstat`, never followed.

`-r`/`-d` control how far it descends:

| Recursive | Dirs | Result |
|---|---|---|
| off | off | directories skipped entirely |
| off | on | directories listed, not descended into |
| on | any | full recursion |

On Windows, `UID`/`GID` are always `0` — there's no POSIX ownership concept
to read, so `0` means "unavailable," not a real value.

## Architecture

- `cmd/grsync` — CLI entrypoint.
- `internal/cli` — flag/argument parsing (built on cobra).
- `internal/sync` — file-list generation today; comparison/delta logic later.
- `internal/transport` — (placeholder) data movement, local and remote.

Goal: full feature parity with upstream rsync, including protocol/format
interoperability where specified (e.g. batch mode's file format).

## License

[GNU General Public License v3.0](LICENSE)
