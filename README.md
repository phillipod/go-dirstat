# dirstat

[![CI](https://github.com/phillipod/go-dirstat/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/phillipod/go-dirstat/actions/workflows/ci.yml)
[![Coverage](https://github.com/phillipod/go-dirstat/actions/workflows/coverage.yml/badge.svg?branch=main)](https://github.com/phillipod/go-dirstat/actions/workflows/coverage.yml)

Terminal disk-usage analysis and guarded space management for operators and
shell pipelines.

`dirstat` measures directory trees, explains filesystem pressure, identifies
growth and cleanup candidates, and can apply an explicit, auditable mutation
plan. It exposes four complementary surfaces:

- a **rich text listing** (the default) with proportional
  bars, percentages, and file/dir counts; plus a by-extension breakdown and a
  largest-files view;
- stable, headerless **two-column TSV** for `cut`, `awk`, and `sort`;
- selectable-field **JSONL, TSV, and NUL-delimited query output** for scripts;
  and
- a **full-screen interactive TUI** (`dirstat tui`) — browse the tree, expand
  and collapse directories, inspect extension and largest-file views, cycle
  sort and size mode, and watch a persistent
  metadata and bounded content previews, mark candidates, stage guarded file
  operations, and watch a persistent cache refresh in the background. The tree
  **populates progressively**: top-level
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

Mutations are deliberately separate from measurement. `plan` creates a
versioned JSONL operation file with the selected object's identity, size, and
modification time; `apply` revalidates those facts, confines every path to the
declared root, refuses conflicts by default, and appends a private audit record.
It runs only with the caller's privileges and never invokes `sudo` or a helper
daemon.

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

- **CI** runs on every pull request and `main` push across Linux (including a
  native arm64 runner), macOS, and Windows (including a native arm64 runner),
  with tests, vet, the race detector, and golangci-lint.
- **Coverage** runs on pull requests, `main`, and a nightly schedule under the
  minimum supported Go line and the current stable Go release. Reports are
  retained as workflow artifacts with a 70% cross-package statement-coverage
  floor.
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

# Capacity/inode pressure and bounded host diagnostics
dirstat status /
dirstat diagnose --format=json /

# Rich candidate records for automation
dirstat query --format=jsonl --kind=file --min-size=1G /srv

# Inspect before changing anything
dirstat inspect --content /srv/archive/old.log

# Stage, review, dry-run, then explicitly apply a guarded deletion
dirstat plan delete --root /srv /srv/archive/old.log > cleanup.jsonl
dirstat apply --dry-run cleanup.jsonl
dirstat apply --yes cleanup.jsonl

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
| `--include-fs TYPE` | — | include **only** these filesystem types (Linux/macOS; repeatable) |
| `--exclude-fs TYPE` | — | exclude these filesystem types (Linux/macOS; repeatable) |
| `--no-hidden` | off | skip dotfile entries |
| `--min-size SIZE` | — | skip files with logical size smaller than SIZE (`10M`, `1G`, `1T`, …) |
| `--max-size SIZE` | — | skip files with logical size larger than SIZE |
| `-j, --jobs N` | GOMAXPROCS | concurrent directory workers (maximum 4096) |

Filesystem-type filtering resolves each directory's fstype from Linux's mount
table or macOS `statfs`; those two flags fail clearly on platforms without a
filesystem-type API rather than silently doing nothing. Examples:

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
input root. Paths use `/` separators for stable cross-platform processing. The
selected sort, size mode, depth, per-directory limit, and directories-only/`--files`
behavior still apply.

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

### Candidate queries for automation

`dirstat query [path...]` flattens a completed measurement into candidate
records. It accepts the normal scope and size-mode flags plus size, age, owner,
group, extension, kind, glob, regexp, and multi-key sort filters. Metadata is
loaded only for surviving candidates when `--metadata` or a metadata field is
requested.

- `--format=jsonl` emits one self-describing record per line.
- `--format=tsv` emits only the comma-separated `--fields`; it has no header.
- `--format=nul` emits absolute paths terminated by NUL for direct use with
  `xargs -0` and similarly safe consumers.

The default TSV fields are `path,kind,size,size-human,mtime`. `size` is always
an exact byte integer using the selected on-disk/apparent mode; `size-human`
uses compact `B/K/M/G/T/...` units. Path and metadata cells escape controls so
one record always occupies one physical line.

```bash
# Exact numeric candidates, safe to feed to awk.
dirstat query --kind=file --min-size=100M --format=tsv \
  --fields=size,path /var | awk -F '\t' '$1 >= 1073741824 { print $2 }'

# Structured review of old logs, largest first.
dirstat query --kind=file --extension=log --older-than=720h \
  --sort=size:desc --format=jsonl /srv

# NUL-safe path transport. This still only prints; it does not delete.
dirstat query --kind=file --path-regexp='\.tmp$' --format=nul /srv | xargs -0 -n1 printf '%s\n'
```

### Pressure, inspection, and growth

- `status` reports byte and inode pressure for the volume containing each path.
- `diagnose` adds bounded platform evidence. On Linux this includes unlinked
  files still held open under `/proc`; unsupported probes are reported as
  unavailable rather than as an empty success.
- `inspect` reports no-follow metadata and optionally a bounded text or hex
  head/tail preview.
- `history growth` records a fresh scope-fingerprinted snapshot and compares it
  with the previous retained scan. History keeps at most 20 snapshots for 30
  days and classifies paths as new, grown, shrunk, or removed.

Every one of these commands has JSON output for machine consumers; query uses
JSONL because it is a record stream.

### Guarded plans and apply

`dirstat plan ACTION SOURCE [DESTINATION]` supports `delete`, `copy`, `move`,
`rename`, `mkdir`, `touch`, `truncate`, `chmod`, `chown`, `archive`, and
`extract`. It canonicalizes `--root`, rejects escaping paths and symlinked
parents, validates action-specific flags, and captures existing source metadata
without following the final symlink. Recursive directory deletion must be
requested explicitly with `--recursive`; deleting or relocating the declared
root is always refused.

Plans and results are versioned JSONL so they can be reviewed, checked into an
operations repository, or transported by existing administrative tooling.
`apply` refuses to mutate without `--yes`; `--dry-run` performs the same
confinement, stale-object, parameter, and conflict checks without changes.
Destination conflicts fail unless `--conflict=overwrite` is explicit. Archive
extraction rejects absolute and parent-traversal members.

```bash
dirstat plan delete --recursive --root /srv /srv/old-release -o cleanup.jsonl
dirstat apply --dry-run cleanup.jsonl
dirstat apply --yes cleanup.jsonl > cleanup-results.jsonl
```

Apply stops at the first failed operation and reports every completed result;
it does not claim transactionality that filesystems cannot provide. Operations
run with the current user's permissions only. Audit logging is on by default;
use `--no-audit` only when an external control plane supplies equivalent
records.

The guards protect against stale selections, accidental path escape, and
symlinked parents observed during validation. They are not a sandbox against a
hostile process that can concurrently rename directories inside the managed
root; for adversarial multi-user trees, first restrict write access or operate
on a private snapshot/mount.

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
| `i` / `p` / `F3` | toggle metadata and bounded content context |
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

The TUI combines the disk emphasis of WinDirStat/TreeSize with the keyboard
workflow of Norton Commander and XTree. It keeps the measured tree primary and
adds an adaptive metadata/content pane at widths of 120 columns or more; every
operation remains available in compact terminals through the same keys and
modal queue. Actions are staged from the selected/marked paths, guarded with
captured no-follow metadata, reviewed as a list, and require the exact typed
confirmation `APPLY`. A successful or partial apply triggers a fresh scan, so
the displayed totals do not pretend that a failed batch was atomic.

The TUI is still focused on disk-pressure work, not general file-manager
parity: there is no remote transport, privilege escalation, plugin execution,
or shell interpolation. Terminal treemaps and an interactive history-diff view
remain future opportunities; growth comparison is currently available through
`dirstat history growth`.

**Cache.** The TUI keeps a snapshot under
`<cache dir>/dirstat/` keyed by the scan root **and** a fingerprint of the
active scope options. On open it renders instantly from the cache (showing a
`cached 3m, refreshing…` badge) while a fresh scan refreshes in the background;
once it lands, the tree is swapped in and the cache is updated. Change a scope
flag and the fingerprint changes, so a full rescan runs automatically. Use
`--no-cache` to bypass it.

**Configuration and audit.** Optional configuration lives at the platform user
configuration path under `dirstat/config.json`. External tools are exact argv
arrays; the selected path is appended to editor/pager argv, while a configured
shell is started with its working directory set to the selected directory.
Paths are never interpolated into a shell command, and direct `sudo` execution
is rejected.

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

Without an override, mutation results are appended to a mode-`0600` audit file
in the platform state directory. `tui --read-only` disables mutations and the
external editor/shell. `--no-audit` is available only as an explicit opt-out.

## Accuracy

Both size models are measured during the scan:

- **apparent** — logical file length (`du --apparent-size`), and
- **on-disk** — allocated 512-byte blocks (the number plain `du` prints).

Linux and macOS expose allocated blocks and stable device/inode identities to
the scanner. Windows exposes stable volume/file identities through file
handles, so boundary checks, followed-symlink loop protection, and hardlink
deduplication work there too; Windows currently reports logical size for both
display modes because its portable stat result does not expose allocated
blocks. Filesystem-type include/exclude filtering remains a Linux/macOS
capability and fails clearly elsewhere.

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
internal/tui           presentation — Bubble Tea browser and action queue
internal/preview       presentation — bounded text/hex content previews
internal/scan          domain      — concurrent scanner, builds the tree
internal/agg           domain      — extension/top-file breakdowns
internal/index         domain      — snapshot, scope fingerprint, and disk store
internal/query         domain      — flat filtering, sorting, JSONL/TSV/NUL
internal/history       domain      — retained snapshots and growth deltas
internal/diagnose      domain      — volume and platform pressure evidence
internal/fsops         domain      — guarded plans, mutations, and audit records
internal/config        foundation  — optional user tools and safety defaults
internal/fsinfo        foundation  — on-demand metadata and volume capacity
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
- **Mutations**: `fsops` accepts versioned plans rather than raw UI gestures,
  verifies stale identities and confinement immediately before each operation,
  and records results as JSONL. The CLI and TUI are clients of the same engine.
- **Metadata on demand**: `fsinfo` and `preview` inspect only selected/query
  candidates, keeping the scanner's hot tree representation compact.

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
