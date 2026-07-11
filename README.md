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
  detector, golangci-lint, actionlint, and release-toolchain `govulncheck` run
  in separate jobs. Lint covers production, test, and fuzz code; pull requests
  also receive a high-severity dependency review.
- A dedicated Ubuntu and macOS lane builds the real CLI and drives resize,
  navigation, help, rescan, and bounded shutdown through a native Unix PTY. It
  retains the ANSI transcript as CI evidence. Windows keeps the portable
  headless program and model lifecycle coverage until a stable ConPTY harness
  is available; it does not claim Unix PTY parity.
- The supported Go floor is 1.25.12 and release builds use Go 1.26.5. Coverage
  runs against both supported branches, retains reports as artifacts, and
  enforces 70% total statement coverage plus a 77% `internal/tui` floor with
  ratchets for program lifecycle, filtering, management, and context rendering.
- The [nightly workflow](https://github.com/phillipod/go-dirstat/actions/workflows/nightly.yml)
  keeps output and state outside its scanned fixture, checks exact tree, query,
  and history records, and proves loop and hardlink behavior natively on Unix
  and Windows. It also runs the race detector, bounded parser fuzzing, and
  retained million-entry query and wide-directory throughput/heap budgets.
- The [release rehearsal](https://github.com/phillipod/go-dirstat/actions/workflows/release-rehearsal.yml)
  is non-publishing. It builds reproducible archives for all six targets,
  unpacks and smokes each one on its matching native runner, generates checksums
  and an SPDX JSON SBOM, and retains the complete evidence bundle.
- Tags matching `v*` publish only after native tests, full lint, race,
  `govulncheck`, artifact rehearsal, checksums, and supply-chain generation all
  pass. Release archives receive cryptographically signed GitHub provenance and
  SBOM attestations. Every third-party workflow action is pinned to an immutable
  commit and Dependabot proposes managed action updates.

```bash
git tag -a v1.2.3 -m 'release v1.2.3'
git push origin v1.2.3
```

After downloading a release bundle, verify its checksums and GitHub attestation:

```bash
sha256sum -c SHA256SUMS
gh attestation verify dirstat-v1.2.3-linux-amd64.tar.gz \
  --repo phillipod/go-dirstat
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

Scope flags are global so they can appear before or after a scan subcommand,
but they are accepted only by the default scan, `extensions`, `query`, `tui`,
and the history commands that consume them. Supplying one to `status`,
`diagnose`, `inspect`, `plan`, `apply`, `state`, `skills`, or `version` is an error;
there is no silently ignored safety flag.

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

With `--follow`, metadata and measured sizes both describe the followed target.
A leading-only dotfile such as `.env` has no extension on both query and
extension-summary surfaces; `.config.json` has extension `.json`. Owner/group
filters, fields, and sorts fail with an explicit capability error on Windows,
where SID lookup is not yet implemented.

- `--format=jsonl` writes one record per line.
- `--format=tsv` writes the comma-separated `--fields`, without a header.
- `--format=nul` writes absolute paths separated by NUL bytes.
- `--limit=N` retains only the best `N` records for the requested sort instead
  of materializing every match; it applies independently to each root.
- `--stream` skips sorting and emits deterministic tree order without retaining
  a record set. It cannot be combined with `--sort` and accepts `--limit`.
- `--index=live|prefer|only|refresh` selects the measured-tree source. `live`
  is the unchanged default and never reads or writes the query index. `prefer`
  uses a complete, fresh matching snapshot and otherwise performs a read-only
  live fallback. `only` fails when that snapshot is unavailable. `refresh`
  performs one live scan and publishes it only when the scan is complete.
- `--index-evidence=text|jsonl` writes source, age, fingerprint, and explicit
  completeness evidence to stderr for non-live modes; stdout record schemas do
  not change. Persisted-only/preferred queries reject live owner/group/mode or
  identity metadata rather than silently mixing indexed and live truth.

The default TSV fields are `path,kind,size,size-human,mtime`. `size` is an
integer byte count in the selected size mode; `size-human` is the same value in
compact units. TSV cells use the same escaping rules as tree output. Repeated
Unix UID/GID name lookups, including unknown identities, are cached for the
life of the process.

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

# Low-memory, unsorted input for a streaming awk pipeline.
dirstat query --stream --kind=file --fields=size,path /srv |
  awk -F '\t' '$1 >= 1073741824 { print $2 }'

# Materialize one complete query snapshot, then require it in automation.
dirstat query --index=refresh --index-evidence=jsonl --kind=file /srv >/dev/null
dirstat query --index=only --index-evidence=jsonl --kind=file /srv
```

### Pressure, inspection, and growth

- `status` shows byte and inode use for the filesystem containing each path.
  It distinguishes physical allocation from caller pressure: reserved blocks
  are unavailable to an ordinary caller even though they are not allocated.
- `diagnose` adds host-specific checks. On Linux it finds deleted files that
  are still open by a process. Unsupported checks are shown as unavailable.
- `inspect` prints metadata without following the final symlink. It can also
  show a size-limited text or hex preview from the beginning or end of a file.
  Text escapes terminal controls by default. `--raw-content` writes only the
  bounded preview bytes for explicit binary-safe use; JSON remains lossless.
  `--tail` requires `--content`.
- `history growth` scans the path and compares it with the previous snapshot
  for the same scope, reporting new, grown, shrunk, and removed paths. Up to 20
  snapshots are retained for 30 days in the platform state directory. The
  history store is excluded from its subject scan automatically; an explicit
  `--store` may be below the scanned path, but it cannot be the scan root or
  contain the scan root. A
  store outside the scanned path does not change the scan fingerprint, so a
  migrated baseline remains visible to the destination's normal `history list`
  and `history growth` commands. Pre-release history formerly kept below the
  cache directory is migrated only by the explicit, previewable `state migrate`
  workflow described below.

History changes include both directory aggregates and their changed
descendants, so the default rows are diagnostic but are not additive: do not
sum a parent and its child. `history growth --leaf-only` suppresses every
changed path with a changed descendant. `--kind=file|directory|all`,
`--depth=N` (root is depth 0, `-1` is unlimited), and `--limit=N` provide
deterministic operational views; JSON delta records expose the same `depth`.

```bash
# Largest changed leaves only, avoiding ancestor double counting.
dirstat history growth --leaf-only --limit=50 --format=json /srv
```

### State lifecycle and policy

`dirstat state` inventories and maintains the expendable query/TUI index and
durable history stores. `status`, `list`, and `size` are non-creating reads;
they report corrupt, foreign, unsafe, or inaccessible boundaries instead of
following symlinks. `prune` applies store-wide TTL and byte quotas, `clear`
removes only safely owned payload entries, and `migrate` explicitly adopts
recognized legacy stores, moves compatible history, and invalidates obsolete
cache formats. Every mutation requires exactly one of `--dry-run` or `--yes`.

State JSON is deterministic. `managed`, `safe`, and `inventory_complete`
distinguish trusted evidence from a failed or foreign boundary. Size output is
explicitly `size_scope=policy_payload`: it counts logical bytes governed by
TTL/quota policy, not ownership markers, lock files, or directory metadata.
`removed` reports a confirmed deletion; `may_have_mutated` is set when a
post-mutation durability error prevents a definite answer.

```bash
# Read-only inventory; these commands do not create a store.
dirstat state --format=json status
dirstat state --kind=history list
dirstat state size

# Preview first, then repeat with explicit confirmation.
dirstat state prune --dry-run
dirstat state migrate --dry-run
dirstat state migrate --yes
```

These commands support JSON output. `status --format=json` always returns one
JSON array, including for a single path; use `--format=jsonl` for one volume per
line. The legacy `used_bytes` and `used_percent` status fields remain aliases
for physical allocation. New consumers should use `physical_used_bytes`,
`physical_used_percent`, `caller_pressure_percent`, and `available_bytes`.
Every capacity and identity field describes `resolved_path`; `path` preserves
the path supplied by the caller. `query` uses JSONL because it returns a stream
of records.

Linux open-deleted results use diagnostics schema version 2. Each
`open_deleted` entry is one unique zero-link regular file, identified by device
and inode, with every observed holder PID and descriptor grouped under it.
`size_bytes` is logical length; `allocated_bytes` and the summary's
`reclaimable_bytes` are unique allocated storage rather than a per-descriptor
sum. The summary includes `/proc` coverage. When coverage is partial, the
reclaimable value is an observed lower bound and is labeled that way in text.
Schema version 1 consumers should replace top-level per-row `pid` and
`descriptor` reads with iteration over each object's `holders`, and should use
the summary's `reclaimable_bytes` instead of summing `open_deleted` rows.
On non-Linux hosts the capability remains explicitly unavailable and the Linux
object list and summary are omitted.

### Monitoring exit contracts

Read-only commands provide opt-in condition exits. Conditions 4 through 6 are
evaluated after the command writes its normal text or valid JSON/JSONL output;
exit 3 rejects the incomplete root before rendering it:

| Exit | Condition |
| --- | --- |
| `3` | a scan was incomplete and `--allow-partial` was not selected |
| `4` | caller byte or inode pressure exceeded an enabled maximum |
| `5` | requested diagnostic or inode-pressure evidence was partial or unavailable |
| `6` | a query violated `--require-match` or `--fail-if-match` |

`status` and `diagnose` accept `--max-byte-pressure=PERCENT` and
`--max-inode-pressure=PERCENT`; `-1` disables a threshold. Byte thresholds use
ordinary-caller pressure, not the lower physical-allocation percentage. When an
inode threshold is enabled but the platform cannot report inode totals, the
command exits 5 instead of treating unavailable evidence as a passing check.
`diagnose` gives incomplete evidence precedence over pressure because a partial
probe cannot prove the complete host state. Query candidate-condition flags are
mutually exclusive and run after every requested root has emitted its records.

```bash
# Alert when ordinary callers have less than the configured pressure margin.
dirstat status --format=json --max-byte-pressure=90 --max-inode-pressure=90 /

# Treat cleanup candidates as a monitoring condition while retaining JSONL.
dirstat query --kind=file --min-size=10G --fail-if-match --format=jsonl /srv
```

### File operation plans

`dirstat plan ACTION SOURCE [DESTINATION]` supports `delete`, `copy`, `move`,
`rename`, `mkdir`, `touch`, `truncate`, `chmod`, `chown`, `archive`, and
`extract`. The plan stores paths under a canonical `--root` and captures
metadata for existing sources without following the final symlink.

The positional form remains the one-operation interface. Scripted batches can
use repeatable `--source`, `--files0-from=FILE` for NUL-terminated paths, or
`--operations-from=FILE` for strict request-only JSONL; `-` reads the selected
input from stdin. Uniform copy, move, and rename batches also require
`--destination-dir` and retain each source basename. Mixed actions and actions
that need distinct destinations use JSONL:

```bash
# Hostile path bytes such as newlines are unambiguous in NUL input.
dirstat query --kind=file --format=nul /srv |
  dirstat plan --root /srv --files0-from=- --recursive \
    --output cleanup.jsonl delete

# Request-only JSONL has no caller-selected IDs or metadata guards.
printf '%s\n' \
  '{"action":"mkdir","source":"staging"}' \
  '{"action":"touch","source":"staging/marker"}' |
  dirstat plan --root /srv --operations-from=- --summary \
    --output rollout.jsonl
```

These input modes are mutually exclusive and limited to 64 MiB, with byte N+1
read before accepting the limit. NUL input must end with NUL. JSONL accepts one
strict operation request per physical line and may mix actions. Invalid UTF-8
paths are rejected because versioned plan JSONL cannot preserve them. Exact
requests are deduplicated; recursive-delete descendants are removed only when
the complete request set contains deletes alone and the covering source is a
real directory. Other duplicate targets and ordering conflicts are errors.
Input order remains stable and IDs are regenerated as `ACTION-ORDINAL`.

The complete normalized batch is confined, guarded, and prevalidated before
any plan bytes are emitted or a dirstat-managed `--output` file is created.
Shell redirection creates its target before dirstat starts, so use `--output`
when that no-file-on-error guarantee matters. Generated output is also capped
at 64 MiB. `--summary` leaves plan stdout untouched and writes one JSON summary
to stderr with operation/action counts, deduplication, and a hardlink-aware
allocated-byte delete-reclaim estimate plus its completeness state.

Plans and results are versioned JSONL. Plan input is limited to 64 MiB and is
decoded strictly: every physical line must contain exactly one object, and
unknown or duplicate fields, trailing JSON, blank records, duplicate operation
IDs, and invalid operation dependencies are rejected. The complete plan is
statically validated before the audit log or filesystem is changed, so a valid
prefix cannot run when a later operation is invalid. See the
[plan and result machine contract](documentation/plan-format.md).

Version 2 plans guard both source and destination state; version 1 plans remain
readable but cannot authorize overwrite. Apply requires `--yes`; use
`--dry-run` to perform the checks without changing anything. Existing
destinations are an error unless `--conflict=overwrite` is set. Directory
deletion requires `--recursive`, and the plan root itself cannot be deleted or
moved. Recursive deletion captures the reviewed tree and removes it from the
leaves upward, checking cancellation and object identity between removals. If
it stops after the first removal, the result is `partial` and states how many
captured objects were removed.

Directory copies are assembled in a hidden sibling staging directory. Files
are synced, the staged tree and unchanged source snapshot are checked, and only
then is the complete tree renamed into place. A failed or cancelled build never
exposes the final destination; cleanup failure is reported as a partial result
with the staging residue identified. Linux, macOS, and Windows use an atomic
no-replace rename, so a destination created during staging is preserved.

Archive dry-runs and extraction use the same complete manifest policy.
Absolute and traversing names, hardlinks, special entries, and entries whose
parent is a symlink or non-directory are rejected. Accepted archives are built
through a private, rooted staging directory before the completed tree is
published.

When overwrite is enabled, the old destination is kept until its replacement
has completed. If the replacement fails, `dirstat` restores the old one. If a
different process creates the destination during replacement, dirstat keeps
that object and retains the reviewed destination under its reported backup
path instead of deleting either one.

```bash
dirstat plan delete --recursive --root /srv /srv/old-release -o cleanup.jsonl
dirstat apply --dry-run cleanup.jsonl
dirstat apply --yes cleanup.jsonl > cleanup-results.jsonl
```

Apply stops on the first error and writes a result for each operation it
attempted. Audit logging is enabled by default and can be disabled with
`--no-audit`. Before each mutation, the owned audit file receives and syncs an
intent record. The outcome is appended and synced afterward. An intent failure
prevents the operation; an outcome write, sync, or close failure after a
completed operation returns `status: "partial"`, `mutation_completed: true`,
and `audit_status: "failed"`. A cross-filesystem move whose destination was
published but whose source cleanup could not finish also returns `partial` with
`may_have_mutated: true`. Callers should reconcile partial results before
retrying.

Windows rejects `chmod`, `chown`, and explicit `--mode` values during
complete-plan validation. Default `mkdir` and `touch` behavior remains
available, but dirstat does not report POSIX ownership or mode changes as
successful on a platform that cannot provide those semantics.

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
| `Space` / `Ctrl+X` | mark or unmark a path / clear all marks |
| `h` / `←` | collapse, or jump to parent |
| `g` / `G` | top / bottom · `PgUp`/`PgDn` page |
| `/` / `Ctrl+L` | filter the active tree, extension, or largest-file view / clear filter |
| `i` / `p` / `F3` | show or hide metadata and the file preview |
| `F4` / `o` / `!` | configured editor / pager / shell |
| `F5` / `F6` | open the confined destination picker for staged copy / move |
| `F7` / `F8` | stage mkdir / recursive delete |
| `a` | open the scrollable operation queue for review, dry-run, export, or apply |
| `s` | cycle tree sort: size → count → mtime → name; extension sort: size → count → name |
| `m` | toggle on-disk / apparent size |
| `e` / `t` | extensions view / tree view |
| `f` | largest files view (top 100) |
| `Y` / `O` | measured growth / unique open-deleted pressure views |
| `T` | set the caller-available reclaim target (raw bytes or `K/M/G/T/P/E`) |
| `Tab` / `Shift+Tab` | cycle tree, extension, and largest-file views |
| `r` | refresh the current scan or analytical view |
| `c` / `Esc` | cancel active analysis, or stop a scan and keep its current results |
| `?` | help · `q` / `Ctrl+C` quit |

The TUI takes its disk-usage view from tools such as WinDirStat and TreeSize,
with keyboard controls closer to Norton Commander or XTree. On terminals at
least 120 columns wide it opens a metadata and preview pane beside the tree.
Narrower terminals use the same commands without the second pane.

Scans appear as they run rather than behind a loading screen. Top-level
directories show up first and their totals change until the final tree is
ready. The header reports ordinary-caller byte pressure, available capacity,
and inode pressure when the platform provides it. A configured or interactive
reclaim target adds a post-queue forecast based on the queue's deduplicated
on-disk reclaim estimate; it does not treat reserved blocks as caller-available.

`Y` compares the complete in-memory scan with snapshots retained by
`history growth`, showing baseline/current timestamps, freshness,
completeness, and allocated/apparent deltas. A mutable refresh records the
current complete baseline. Read-only mode opens history without creating state;
if no baseline exists it reports that explicitly. The history state directory
and its canonical or scan-root alias are excluded before the first scan,
including when state lies below a broad root such as the user's home directory.
`O` displays unique open-deleted objects, holder PIDs/descriptors, coverage, and
an observed lower bound when `/proc` evidence is incomplete. Both analyses are
cancellable and label freshness.

F5 through F8 add operations to a queue. Press `a` to open the queue, where
arrow and paging keys scroll every item, `x` removes the selected operation,
`[`/`]` change its order, `v` runs a non-mutating dry-run, and `e` exports the
complete guarded JSONL plan. The header shows the visible range, reviewed count,
and an on-disk reclaim estimate. Every queue page must be displayed before the
TUI will accept `APPLY`. Sources are checked again immediately before each
operation. Configured operation-count and reclaim-byte caps are checked while
staging and again before confirmation. F5/F6 open a no-follow, root-confined
directory browser: arrows select, Enter/Right opens a child, Left/Backspace
returns toward the scan root, and Tab chooses the current directory. It shows
target availability, pressure, inode use, and a cross-device warning; typing
switches to an exact manual destination. Successful deletes update the tree,
totals, derived views, and cache immediately. A background reconciliation scan
is used when an exact delta is not available, including copy and move operations,
hardlink or followed-symlink
ambiguity, an interrupted in-flight scan, or an audit log inside the scan root.
After a partial apply, unresolved schema-v2 `partial` / `may_have_mutated`
operations plus failed and unattempted operations remain in the queue. An
operation with `mutation_completed: true` is removed so retry cannot repeat an
already completed mutation, even when its audit outcome failed; the audit
failure remains visible and reconciliation still runs. The result view states
that disk may have changed while a reconciliation scan runs. Any failed or
canceled apply triggers reconciliation because an operation may have changed
disk before returning its error. Editor and shell completions also trigger a
rescan regardless of exit status, since either tool may save changes and then
exit nonzero; the process error remains visible while the scan runs.

This is not intended to be a general-purpose file manager. There is no remote
access, privilege escalation, plugin system, or shell interpolation.

### TUI cache

Snapshots are stored under `<cache dir>/dirstat/`. The cache key includes the
scan root and scope options, so a cache made with different filters is not
reused. The TUI displays cached data immediately, starts a fresh scan in the
background, and replaces the tree when that scan finishes. Use `--no-cache` to
disable this. Fingerprint version 2 uses canonical length-prefixed fields and a
full SHA-256 digest. It intentionally invalidates snapshots and matching
  history keys written by pre-release builds. `state migrate` is the explicit
  path for moving recognized legacy history; ordinary scans never guess at or
  delete old state. Effective read-only mode does not open the creating cache
  store.

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
  "tui": {
    "target_available_bytes": 21474836480,
    "queue_max_operations": 1000,
    "queue_max_reclaim_bytes": 107374182400
  },
  "read_only": false,
  "audit_path": "/var/tmp/dirstat-operations.jsonl",
  "history_max": 20,
  "state": {
    "cache_max_bytes": 536870912,
    "cache_ttl_hours": 720,
    "history_max_bytes": 2147483648,
    "history_ttl_days": 30
  }
}
```

TUI configuration byte values are raw, unambiguous bytes. A zero reclaim-byte
cap means unlimited; the operation cap must be greater than zero and defaults
to 1000. The interactive `T` prompt additionally accepts binary `K/M/G/T/P/E`
suffixes for incident response.
State byte limits are global logical-payload budgets across roots. TTLs and
history count retention are enforced on successful writes and by explicit
`state prune`; a failed publication preserves prior durable records.

By default, operation intent and outcome records are appended to an audit file
in the platform state directory. Owned audit files are synced after every
record and close errors are reported. On Unix the file mode is `0600`.
`tui --read-only` disables mutations and the external editor and shell, and
does not create audit, cache, or history state.
For mutable sessions, the audit directory and file are created lazily when the
first operation is applied. An audit path inside a source or destination tree
in the plan is rejected before the file is opened. Use `--no-audit` to turn
audit logging off.
Configuration is decoded strictly: unknown or duplicate fields, trailing JSON
values, non-absolute audit paths, invalid history retention, and blank,
NUL-containing, or `sudo` tool commands are rejected with the config file and
field in the error.

## Accuracy

Each scan records two sizes:

- **apparent** — logical file length (`du --apparent-size`), and
- **on-disk** — allocated 512-byte blocks (the number plain `du` prints).

Linux and macOS provide allocated block counts and stable device/inode IDs.
Completed scans with unreadable or racing entries exit with status `3` before
rendering that root. The default listing, `extensions`, and `query` accept
`--allow-partial` when best-effort output is intentional and print the error
count to stderr. History never records an incomplete scan as a baseline.
Windows provides stable volume/file IDs, so filesystem boundaries, symlink-loop
checks, and hardlink deduplication also work there. Windows does not expose an
allocated block count through the stat API used here, so both size modes show
the logical size. Store ownership uses the current-user owner and DACL rather
than Unix mode bits, and snapshot/marker publication uses write-through Windows
renames. Windows has no portable parent-directory `fsync`; deletion metadata is
therefore crash-durable on a best-effort basis even though returned lifecycle
results remain truthful about the visible mutation.

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
internal/fileclass     foundation  — shared filename extension semantics
internal/storefs       foundation  — rooted, owned cache/history filesystem boundary
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
