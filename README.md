# dirstat

[![CI](https://github.com/phillipod/go-dirstat/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/phillipod/go-dirstat/actions/workflows/ci.yml)
[![Coverage](https://github.com/phillipod/go-dirstat/actions/workflows/coverage.yml/badge.svg?branch=main)](https://github.com/phillipod/go-dirstat/actions/workflows/coverage.yml)

`dirstat` is a terminal disk-usage tool for finding where space has gone and
cleaning it up. It works as a regular command, as a source of records for shell
scripts, or as a full-screen browser.

The default output is a size-sorted tree with bars, percentages, and subtree
counts. Other commands provide extension summaries, largest-file lists,
filesystem status, growth history, and filtered output in TSV, JSONL, or
NUL-delimited form. `dirstat tui` adds navigation, previews, marking, and a
queue for file operations.

Scans run concurrently, using `GOMAXPROCS` workers unless configured otherwise.
They stay on the starting filesystem by default and skip `/proc`, `/sys`,
`/dev`, `/run`, and other kernel filesystems. Hardlinks are counted once when
the platform provides a stable file identity; later links appear as zero-size
`↪` entries. The first path in lexical order owns the size, which keeps results
stable when directory reads finish in a different order.

File changes go through a separate plan/apply path. A plan records the source
identity, size, and modification time. Apply checks them again, keeps paths
under the plan root, refuses destination conflicts by default, and writes an
audit record. `dirstat` uses the caller's privileges; it does not run `sudo` or
use a privileged helper.

## Install

```bash
go install github.com/phillipod/go-dirstat/cmd/dirstat@latest
```

## Build from source

```bash
make            # builds ./bin/dirstat
make install    # copies it into $GOBIN (or ~/.local/bin)
```

Without Make:

```bash
go install ./cmd/dirstat
```

## Automation and releases

CI uses GitHub-hosted runners:

- Pull requests and pushes to `main` run tests and builds on Linux, macOS, and
  Windows, including native Linux and Windows arm64 runners. Vet, the race
  detector, golangci-lint, and actionlint run in separate jobs.
- Coverage runs with Go 1.24 and the current stable Go release. The workflow
  keeps its reports as artifacts and enforces 70% total statement coverage.
- A nightly workflow runs shuffled tests and CLI integration checks on all
  supported hosts.
- Tags matching `v*` build release archives and `SHA256SUMS` for Linux, macOS,
  and Windows on amd64 and arm64.

```bash
git tag -a v1.2.3 -m 'release v1.2.3'
git push origin v1.2.3
```

## Quick start

```bash
# Scan the current directory. Sizes are allocated bytes, like du.
dirstat

# Show two levels, at most 20 entries per directory, using apparent size.
dirstat -d 2 -n 20 -A ~/projects

# Include files as well as directories.
dirstat -a -d 2 ~/projects

# Summarize by extension and list the largest files.
dirstat ext ~/projects
#   (equivalent: dirstat --by-ext ~/projects)

# Full-screen interactive browser
dirstat tui ~/projects

# Check capacity and inode use, then collect host diagnostics.
dirstat status /
dirstat diagnose --format=json /

# Find files of at least 1 GiB and emit JSONL.
dirstat query --format=jsonl --kind=file --min-size=1G /srv

# Inspect before changing anything
dirstat inspect --content /srv/archive/old.log

# Write a deletion plan, check it, then apply it.
dirstat plan delete --root /srv /srv/archive/old.log > cleanup.jsonl
dirstat apply --dry-run cleanup.jsonl
dirstat apply --yes cleanup.jsonl

# Build/version information (the `version` subcommand is equivalent)
dirstat --version
```

## Text mode

The default command prints a tree. Each row contains the size, its share of the
root, file and directory counts, and the name:

```
12.3G  ████████████░░░░  100.0%  1,204f 48d  .
├──  4.5G  ████░░░░░░░░░░░░  37.0%  ...   node_modules/
├──  2.1G  ██░░░░░░░░░░░░░░  17.0%  ...   src/
...
Total:  12.3G  1,204 files  48 dirs  in 312ms   ext4
```

Color is enabled only when stdout is a terminal. Bar colors reflect the share
of the root: red at 50% and above, yellow at 20%, cyan at 5%, and green below
that.

## Scope & filtering

The default scope is intentionally close to `du -x`: stay on one filesystem,
do not follow symlinks, and leave virtual filesystems alone.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-x`, `--one-file-system` | default | stay on one filesystem (`du -x` compatible) |
| `--cross-device` | off | cross filesystem boundaries |
| `--no-virtual-exclude` | off | do **not** skip `/proc /sys /dev /run` + pseudo-fs |
| `-L`, `--follow` | off | follow symlinked directories (with loop protection) |
| `--exclude GLOB` | — | exclude basename/path glob, `du --exclude` style (repeatable) |
| `--exclude-path P` | — | exclude absolute path prefix (repeatable) |
| `--include-path P` | — | scan **only** these path prefixes (repeatable) |
| `--include-fs TYPE` | — | include **only** these filesystem types (Linux/macOS; repeatable) |
| `--exclude-fs TYPE` | — | exclude these filesystem types (Linux/macOS; repeatable) |
| `--no-hidden` | off | skip dotfile entries |
| `--min-size SIZE` | — | skip files with logical size smaller than SIZE (`10M`, `1G`, `1T`, …) |
| `--max-size SIZE` | — | skip files with logical size larger than SIZE |
| `-j, --jobs N` | GOMAXPROCS | concurrent directory workers (maximum 4096) |

Filesystem-type filters use the Linux mount table or macOS `statfs`. Other
platforms return an error when these flags are used.

```bash
# Scan the root filesystem, excluding common project caches.
dirstat -x --exclude '.git' --exclude 'node_modules' /

# Cross mounts, but include only btrfs and ext4.
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

There is no header or summary. Multiple roots are written in argument order,
and paths are prefixed with their cleaned input root. The normal sort, depth,
limit, size-mode, and `--files` options still apply. `--limit` does not add a
synthetic row for omitted entries.

Sizes use compact units (`B`, `K`, `M`, `G`, `T`, and so on). Add `--bytes` for
an integer byte count. Paths use `/` separators. Tabs, newlines, carriage
returns, backslashes, other control bytes, and invalid UTF-8 are escaped so a
filename cannot split a TSV record.

```bash
# Filter on an exact byte count.
dirstat --format=tsv --bytes --files ~/projects |
  awk -F '\t' '$1 >= 1048576 { print $2 }'

# Print the path column.
dirstat --format=tsv ~/projects | cut -f2
```

The two-column format belongs to tree output. It is not available in
`extensions` or `tui`, and it cannot be combined with `--by-ext`.

### Query mode

`dirstat query [path...]` turns a scan into a flat record stream. It can filter
by size, age, owner, group, extension, kind, glob, or regular expression, and
supports more than one sort key. Owner, group, mode, and identity metadata is
read only when a filter or output field needs it.

- `--format=jsonl` writes one record per line.
- `--format=tsv` writes the comma-separated `--fields`, without a header.
- `--format=nul` writes absolute paths separated by NUL bytes.

The default TSV fields are `path,kind,size,size-human,mtime`. `size` is an
integer byte count in the selected size mode; `size-human` is the same value in
compact units. TSV cells use the same escaping rules as tree output.

```bash
# Select files of at least 100 MiB, then apply a second numeric filter in awk.
dirstat query --kind=file --min-size=100M --format=tsv \
  --fields=size,path /var | awk -F '\t' '$1 >= 1073741824 { print $2 }'

# Old log files, largest first.
dirstat query --kind=file --extension=log --older-than=720h \
  --sort=size:desc --format=jsonl /srv

# Pass names safely, including names containing newlines.
dirstat query --kind=file --path-regexp='\.tmp$' --format=nul /srv |
  xargs -0 -n1 printf '%s\n'
```

### Pressure, inspection, and growth

- `status` shows byte and inode use for the filesystem containing each path.
- `diagnose` adds host-specific checks. On Linux it finds deleted files that
  are still open by a process. Unsupported checks are shown as unavailable.
- `inspect` prints metadata without following the final symlink. It can also
  show a size-limited text or hex preview from the beginning or end of a file.
- `history growth` scans the path and compares it with the previous snapshot
  for the same scope, reporting new, grown, shrunk, and removed paths. Up to 20
  snapshots are retained for 30 days.

These commands support JSON output. `query` uses JSONL because it returns a
stream of records.

### File operation plans

`dirstat plan ACTION SOURCE [DESTINATION]` supports `delete`, `copy`, `move`,
`rename`, `mkdir`, `touch`, `truncate`, `chmod`, `chown`, `archive`, and
`extract`. The plan stores paths under a canonical `--root` and captures
metadata for existing sources without following the final symlink.

Plans and results are versioned JSONL. Apply requires `--yes`; use `--dry-run`
to perform the checks without changing anything. Existing destinations are an
error unless `--conflict=overwrite` is set. Directory deletion requires
`--recursive`, and the plan root itself cannot be deleted or moved. Archive
extraction rejects absolute paths and `..` traversal.

When overwrite is enabled, the old destination is kept until its replacement
has completed. If the replacement fails, `dirstat` restores the old one.

```bash
dirstat plan delete --recursive --root /srv /srv/old-release -o cleanup.jsonl
dirstat apply --dry-run cleanup.jsonl
dirstat apply --yes cleanup.jsonl > cleanup-results.jsonl
```

Apply stops on the first error and writes a result for each operation it
attempted. Audit logging is enabled by default and can be disabled with
`--no-audit`.

The path and metadata checks are meant to catch stale plans and accidental
escapes. They are not a sandbox against another process that can rename paths
inside the root while a plan is running. Restrict writes or work from a private
snapshot when the tree is not trusted.

## Agent skills

`dirstat skills` installs portable Agent Skills definitions for Codex and
Claude. The default target is the user's skills directory; use
`--scope=project --project-dir .` for a repository-local installation, or
`--codex-path` / `--claude-path` for an exact `SKILL.md` destination.

```bash
# Print a definition for an agent without changing any files.
dirstat skills view

# Install the read-only analysis skill for both Codex and Claude.
dirstat skills install

# Add the plan-and-dry-run operator skill alongside it.
dirstat skills install --profile=operator

# Print the non-authorizing template. Add built-in guarded-cleanup safeguards if useful.
dirstat skills rules template
dirstat skills rules template --guarded-cleanup

# Install an administrator skill whose permitted actions are copied from policy.md.
dirstat skills install --profile=administrator --rules-file policy.md

# In a terminal, this asks whether to open the same template in tools.editor.
dirstat skills install --profile=administrator

# Check or remove installed definitions. Changed files need --force.
dirstat skills status --profile=all
dirstat skills remove --profile=operator
```

The profiles are deliberately separate: `dirstat` only investigates and
proposes cleanup; `dirstat-operator` may create guarded plans and dry-run them
but still needs authorization to mutate files; `dirstat-administrator` may act
only within the policy embedded when it was installed. Administrator policies
are supplied with repeatable `--rule` flags or `--rules-file`, copied into the
skill, and required every time that profile is installed or updated. When an
administrator install has no policy and both input and output are terminals,
`dirstat` asks whether to open the template with the exact `tools.editor` argv
configured in `config.json`, then whether to include the built-in guarded
cleanup safeguards (plan, dry-run, audit, and symlink protection). The template
grants no authority until edited; unchanged templates are rejected. Scripts
should supply `--rule` or `--rules-file`; `--edit-rules` explicitly requests
the terminal editor.

## TUI

`dirstat tui [path]` opens a full-screen browser.

| Key | Action |
| --- | --- |
| `↑`/`↓` or `k`/`j` | move |
| `Enter` / `l` / `→` | expand or collapse a directory |
| `Space` | mark or unmark a file/directory for a batch action |
| `h` / `←` | collapse, or jump to parent |
| `g` / `G` | top / bottom · `PgUp`/`PgDn` page |
| `/` / `Ctrl+L` | filter visible paths / clear filter |
| `i` / `p` / `F3` | show or hide metadata and the file preview |
| `F4` / `o` / `!` | configured editor / pager / shell |
| `F5` / `F6` | stage copy / move for the selection or marked paths |
| `F7` / `F8` | stage mkdir / recursive delete |
| `a` | review the operation queue, then type `APPLY` to execute |
| `s` | cycle tree sort: size → count → mtime → name; extension sort: size → count → name |
| `m` | toggle on-disk / apparent size |
| `e` / `t` | extensions view / tree view |
| `f` | largest files view (top 100) |
| `Tab` / `Shift+Tab` | cycle analytical views forward / backward |
| `r` | force rescan |
| `c` / `Esc` | stop an active scan and keep the current partial/cached results |
| `?` | help · `q` / `Ctrl+C` quit |

The TUI takes its disk-usage view from tools such as WinDirStat and TreeSize,
with keyboard controls closer to Norton Commander or XTree. On terminals at
least 120 columns wide it opens a metadata and preview pane beside the tree.
Narrower terminals use the same commands without the second pane.

Scans appear as they run rather than behind a loading screen. Top-level
directories show up first and their totals change until the final tree is
ready.

F5 through F8 add operations to a queue. Press `a` to review the queue, then
type `APPLY` to run it. Sources are checked again immediately before each
operation. Successful deletes update the tree, totals, derived views, and cache
immediately. A background reconciliation scan is used when an exact delta is
not available, including copy and move operations, hardlink or followed-symlink
ambiguity, an interrupted in-flight scan, or an audit log inside the scan root.
After a partial apply, failed and unattempted operations remain in the queue.

This is not intended to be a general-purpose file manager. There is no remote
access, privilege escalation, plugin system, or shell interpolation. Growth
comparison is currently available from `dirstat history growth`, not as a TUI
view.

### TUI cache

Snapshots are stored under `<cache dir>/dirstat/`. The cache key includes the
scan root and scope options, so a cache made with different filters is not
reused. The TUI displays cached data immediately, starts a fresh scan in the
background, and replaces the tree when that scan finishes. Use `--no-cache` to
disable this.

### Configuration and audit

Optional configuration is read from `dirstat/config.json` in the platform user
configuration directory. Tool commands are argv arrays rather than shell
strings. The selected path is appended to the editor and pager arguments; the
shell starts in the selected directory. Direct `sudo` commands are rejected.

```json
{
  "tools": {
    "pager": ["less", "-R"],
    "editor": ["vim", "--"],
    "shell": ["bash", "-l"]
  },
  "read_only": false,
  "audit_path": "/var/tmp/dirstat-operations.jsonl"
}
```

By default, operation results are appended to an audit file in the platform
state directory. On Unix the file mode is `0600`. `tui --read-only` disables
mutations and the external editor and shell. Use `--no-audit` to turn audit
logging off.

## Accuracy

Each scan records two sizes:

- **apparent** — logical file length (`du --apparent-size`), and
- **on-disk** — allocated 512-byte blocks (the number plain `du` prints).

Linux and macOS provide allocated block counts and stable device/inode IDs.
Windows provides stable volume/file IDs, so filesystem boundaries, symlink-loop
checks, and hardlink deduplication also work there. Windows does not expose an
allocated block count through the stat API used here, so both size modes show
the logical size.

Filesystem-type filters are available on Linux and macOS. Other platforms
return an error if those flags are used. A permission or stat error on one
entry is attached to that entry; it does not stop the rest of the scan.

## Architecture

The packages are split into wiring, presentation, domain, and foundation
layers. `.boundary.yaml` records that arrangement for architecture checks.

```
cmd/dirstat            entrypoint  — tiny main; hands off to cli
internal/cli           wiring      — cobra commands, flags, the run pipeline
internal/render        presentation — rich text and scriptable TSV output
internal/tui           presentation — Bubble Tea browser and action queue
internal/preview       presentation — size-limited text/hex previews
internal/scan          domain      — concurrent scanner, builds the tree
internal/agg           domain      — extension/top-file breakdowns
internal/index         domain      — snapshot, scope fingerprint, and disk store
internal/skills        domain      — portable agent skill definitions and guarded installation
internal/query         domain      — flat filtering, sorting, JSONL/TSV/NUL
internal/history       domain      — retained snapshots and growth deltas
internal/diagnose      domain      — volume and platform pressure evidence
internal/fsops         domain      — plan checks, file operations, and audit records
internal/config        foundation  — optional user tools and safety defaults
internal/fsinfo        foundation  — on-demand metadata and volume capacity
internal/scope         foundation  — all filtering policy (cross-fs, fstype, …)
internal/format        foundation  — bytes/bars/percent rendering helpers
internal/tree          foundation  — the measured-Node data model (leaf)
internal/version       foundation  — build metadata
```

Some implementation details:

- `scope.Policy` decides whether an entry is counted and whether a directory is
  entered.
- `tree.Node` stores apparent and allocated sizes along with subtree counts, so
  renderers do not need to walk the filesystem again.
- Directory reads and stat calls share the configured worker limit
  (`GOMAXPROCS` by default).
- `index` fingerprints the scan scope before reusing a cached snapshot.
- The CLI and TUI both send versioned plans to `fsops`; neither has a separate
  mutation implementation.
- Detailed metadata and previews are loaded on demand rather than stored on
  every tree node.

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
