# dirstat

[![CI](https://github.com/phillipod/go-dirstat/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/phillipod/go-dirstat/actions/workflows/ci.yml)
[![Coverage](https://github.com/phillipod/go-dirstat/actions/workflows/coverage.yml/badge.svg?branch=main)](https://github.com/phillipod/go-dirstat/actions/workflows/coverage.yml)

Read-only terminal disk-usage exploration for people and shell pipelines.

`dirstat` measures directory trees and reports size, file counts, and
breakdowns through three output surfaces:

- a **rich text listing** (the default) with proportional
  bars, percentages, and file/dir counts; plus a by-extension breakdown and a
  largest-files view;
- stable, headerless **two-column TSV** for `cut`, `awk`, and `sort`; and
- a **full-screen interactive TUI** (`dirstat tui`) — browse the tree, expand
  and collapse directories, inspect extension and largest-file views, cycle
  sort and size mode, and watch a persistent
  cache refresh in the background. The tree **populates progressively**: top-level
  directories appear instantly and their sizes climb live under an explicit
  `scanning…` status, with no blocking loading screen.

Scanning is concurrent (work is bounded to `GOMAXPROCS` by default)
and **safe by default**: where device identity is available it never crosses
filesystem boundaries, and it skips `/proc`, `/sys`, `/dev`, and `/run` (and
kernel pseudo-filesystems) even when you ask it to cross, unless you explicitly
disable that exclusion. On Linux and macOS, **hardlinked files are counted once** (by
inode, `du` semantics):
the lexicographically first included path claims the size and every other link
shows as a zero-size `↪` entry, so totals and ownership stay stable across
concurrent scans.

## Install

```bash
go install github.com/phillipod/go-dirstat/cmd/dirstat@latest
```

## Build from source

```bash
make            # builds ./bin/dirstat
make install    # copies it into $GOBIN (or ~/.local/bin)
```

Or directly:

```bash
go install ./cmd/dirstat
```

## Automation and releases

The repository uses GitHub-hosted public runners exclusively:

- **CI** runs on every pull request and `main` push across Linux, macOS, and
  Windows, with tests, vet, the race detector, and golangci-lint.
- **Coverage** runs on pull requests, `main`, and a nightly schedule under the
  minimum supported Go line and the current stable Go release. Reports are
  retained as workflow artifacts with an 80% statement-coverage floor.
- **Nightly integration** runs shuffled tests and real CLI smoke tests across
  Linux, macOS, and Windows, including followed-symlink loop protection where
  the runner supports symlinks.
- **Releases** are published by pushing a semver tag such as `v1.2.3`.
  The release workflow verifies the tag, tests the source, and publishes
  archives for `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`,
  `windows-amd64`, and `windows-arm64`, together with `SHA256SUMS`.

```bash
git tag -a v1.2.3 -m 'release v1.2.3'
git push origin v1.2.3
```

## Quick start

```bash
# Rich text tree of the current directory (on-disk size, like du)
dirstat

# Limit depth and entries per directory, apparent size
dirstat -d 2 -n 20 -A ~/projects

# Include individual files (du -a); default lists directories only
dirstat -a -d 2 ~/projects

# By-extension breakdown + largest files
dirstat ext ~/projects
#   (equivalent: dirstat --by-ext ~/projects)

# Full-screen interactive browser
dirstat tui ~/projects

# Build/version information (the `version` subcommand is equivalent)
dirstat --version
```

## Text mode

The default command prints a tree. Each line shows size, a proportional bar,
the percentage of the total, subtree counts, and the name:

```
12.3G  ████████████░░░░  100.0%  1,204f 48d  .
├──  4.5G  ████░░░░░░░░░░░░  37.0%  ...   node_modules/
├──  2.1G  ██░░░░░░░░░░░░░░  17.0%  ...   src/
...
Total:  12.3G  1,204 files  48 dirs  in 312ms   ext4
```

Colors are emitted only when stdout is a terminal (auto-disabled when piping);
bars are colored by magnitude (red ≥ 50%, yellow ≥ 20%, cyan ≥ 5%, green
otherwise).

## Scope & filtering

`dirstat` exposes a deliberate set of scope options so you can measure exactly
what you want. Defaults are the safe `du`-like behavior.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-x`, `--one-file-system` | default | explicitly stay on one filesystem (`du -x` compatible) |
| `--cross-device` | off | cross filesystem boundaries |
| `--no-virtual-exclude` | off | do **not** skip `/proc /sys /dev /run` + pseudo-fs |
| `-L`, `--follow` | off | follow symlinked directories (with loop protection) |
| `--exclude GLOB` | — | exclude basename/path glob, `du --exclude` style (repeatable) |
| `--exclude-path P` | — | exclude absolute path prefix (repeatable) |
| `--include-path P` | — | scan **only** these path prefixes (repeatable) |
| `--include-fs TYPE` | — | include **only** these filesystem types (Linux; repeatable) |
| `--exclude-fs TYPE` | — | exclude these filesystem types (Linux; repeatable) |
| `--no-hidden` | off | skip dotfile entries |
| `--min-size SIZE` | — | skip files with logical size smaller than SIZE (`10M`, `1G`, `1T`, …) |
| `--max-size SIZE` | — | skip files with logical size larger than SIZE |
| `-j, --jobs N` | GOMAXPROCS | concurrent directory workers (maximum 4096) |

Filesystem-type filtering reads the mount table (`/proc/self/mountinfo` on
Linux) to resolve each directory's fstype; those two flags fail clearly on
other platforms rather than silently doing nothing. Examples:

```bash
# What's on the root filesystem only, ignoring the project's caches?
dirstat -x --exclude '.git' --exclude 'node_modules' /

# Sum only btrfs and ext4 trees, crossing mounts but never pseudo-fs
dirstat --cross-device --include-fs btrfs --include-fs ext4 /srv
```

## Output shaping

| Flag | Meaning |
| --- | --- |
| `-d, --depth N` | max directory depth to print (0 = unlimited) |
| `-n, --limit N` | max entries per directory (0 = unlimited; extras are summed) |
| `-s, --sort MODE` | `size` (default), `size-asc`, `count`, `mtime`, `name` |
| `-a, --files` | list individual files too (`du -a`); default shows directories only, with files folded into each directory's aggregate |
| `-A, --apparent` | use apparent file size (default: on-disk, like `du`) |
| `--format FORMAT` | `text` (default) or stable, headerless `tsv` |
| `--bytes` | raw byte counts instead of human units |
| `--no-bar` / `--no-color` / `--no-counts` | toggle chrome |
| `-e, --by-ext` | extension breakdown instead of the tree |

### TSV for scripts

`--format=tsv` writes exactly two fields per record:

```text
SIZE<TAB>PATH<LF>
```

There is no header, summary, blank separator, percentage, count, bar, color,
timing, filesystem label, or synthetic `--limit` row. Multiple roots are
concatenated in argument order, and every path is qualified by its cleaned
input root. The selected sort, size mode, depth, per-directory limit, and
directories-only/`--files` behavior still apply.

`SIZE` uses the usual compact human units (`B`, `K`, `M`, `G`, `T`, …), or an
exact base-10 byte count with `--bytes`. `PATH` preserves spaces and printable
Unicode. To keep every filesystem name on one physical line, backslash, tab,
newline, and carriage return are encoded as `\\`, `\t`, `\n`, and `\r`;
remaining C0 controls, DEL, and invalid UTF-8 bytes use uppercase `\xHH`.

```bash
# Exact numeric filtering. awk must use the literal tab field separator.
dirstat --format=tsv --bytes --files ~/projects |
  awk -F '\t' '$1 >= 1048576 { print $2 }'

# Select only the path column (human-readable sizes are the default).
dirstat --format=tsv ~/projects | cut -f2
```

TSV is currently a tree-output contract. The format flag is not offered by the
`extensions` or `tui` commands, and combining it with `--by-ext` is rejected,
so incompatible record shapes cannot be mixed silently.

## TUI

`dirstat tui [path]` opens a full-screen browser.

| Key | Action |
| --- | --- |
| `↑`/`↓` or `k`/`j` | move |
| `space` / `l` / `→` | expand or collapse a directory |
| `h` / `←` | collapse, or jump to parent |
| `g` / `G` | top / bottom · `PgUp`/`PgDn` page |
| `s` | cycle tree sort: size → count → mtime → name; extension sort: size → count → name |
| `m` | toggle on-disk / apparent size |
| `e` / `t` | extensions view / tree view |
| `f` | largest files view (top 100) |
| `Tab` / `Shift+Tab` | cycle analytical views forward / backward |
| `r` | force rescan |
| `c` / `Esc` | stop an active scan and keep the current partial/cached results |
| `?` | help · `q` / `Ctrl+C` quit |

The first-release TUI targets the read-only analysis loop shared by WinDirStat,
TreeSize, XTree, and commander-style file browsers: progressive measurement,
fast keyboard navigation, proportional size cues, alternate analytical views,
stable selection while data changes, and an inspectable detail line. It is not
yet a feature-for-feature replacement. Search/type-to-jump, a true two-pane
tree-and-files layout, terminal treemap, multi-snapshot comparison, and history
are the main remaining product gaps. Destructive file operations are outside
the current scope.

**Cache.** The TUI keeps a snapshot under
`<cache dir>/dirstat/` keyed by the scan root **and** a fingerprint of the
active scope options. On open it renders instantly from the cache (showing a
`cached 3m, refreshing…` badge) while a fresh scan refreshes in the background;
once it lands, the tree is swapped in and the cache is updated. Change a scope
flag and the fingerprint changes, so a full rescan runs automatically. Use
`--no-cache` to bypass it.

## Accuracy

Both size models are measured during the scan:

- **apparent** — logical file length (`du --apparent-size`), and
- **on-disk** — allocated 512-byte blocks (the number plain `du` prints).

Linux and macOS expose allocated blocks and stable device/inode identities to
the scanner. On other targets, including Windows, the current portable fallback
reports logical size for both display modes; filesystem-type filtering is not
available there, and `-x` cannot distinguish device boundaries. Symlink-loop
protection falls back to canonical paths; hardlink deduplication is also limited
to Linux and macOS in this release.

Focused scanner tests cover both size models and hardlink deduplication.
Per-entry errors (e.g. permission denied) are recorded on the node and do not
abort the rest of the scan.

## Architecture

`dirstat` is organized as a one-directional layer cake. The intended layering
is encoded in `.boundary.yaml` and can be checked with `structprojection` so
dependency drift is visible during maintenance.

```
cmd/dirstat            entrypoint  — tiny main; hands off to cli
internal/cli           wiring      — cobra commands, flags, the run pipeline
internal/render        presentation — rich text and scriptable TSV output
internal/tui           presentation — Bubble Tea interactive browser
internal/scan          domain      — concurrent scanner, builds the tree
internal/agg           domain      — extension/top-file breakdowns
internal/index         domain      — snapshot, scope fingerprint, and disk store
internal/scope         foundation  — all filtering policy (cross-fs, fstype, …)
internal/format        foundation  — bytes/bars/percent rendering helpers
internal/tree          foundation  — the measured-Node data model (leaf)
internal/version       foundation  — build metadata
```

Notable design points:

- **`scope.Policy`** owns every "count this? descend into this?" decision, so
  the scanner stays a thin, fast traversal and the policy is independently
  tested.
- **`tree.Node`** carries both apparent and on-disk sizes plus aggregated
  subtree counts, so renderers never re-walk.
- **Concurrency**: recursive directory fan-out, outstanding directory reads,
  and entry `stat` calls are all bounded by the configured process-wide worker
  limit (`GOMAXPROCS` by default).
- **Cache**: `index` fingerprints the scope so a cached snapshot is only reused
  when it was produced under the same options; the TUI loads it synchronously
  and refreshes asynchronously.

### Re-running the architecture review

```bash
structprojection show --repo . --language-family go \
  --projection knowledge_graph --boundary-config .boundary.yaml \
  --build-context default \
  --source-fingerprint-mode strict --force-rerun
structprojection show --repo . --language-family go \
  --projection go/boundary_deep --boundary-config .boundary.yaml \
  --build-context default \
  --source-fingerprint-mode strict --force-rerun
structprojection graph report --repo . --projection knowledge_graph \
  --language-family go --build-context default --relation imports \
  --source-fingerprint-mode strict
structprojection graph surprises --repo . --projection knowledge_graph \
  --language-family go --build-context default --relation imports \
  --source-fingerprint-mode strict
```

## License

Copyright (C) 2026 Phillip O'Donnell. This project is distributed under the
[GNU General Public License, version 2](LICENSE) (`GPL-2.0-only`).
