# Plan and result format

`dirstat plan` writes a versioned JSON Lines document. `dirstat apply` accepts
at most 64 MiB (`67,108,864` bytes) from a file or standard input. It reads byte
N+1 before parsing, so an oversized document is rejected rather than treated as
a shorter valid plan.

## Plan creation inputs

The original `dirstat plan ACTION SOURCE [DESTINATION]` form remains the
single-operation contract. Batch creation is additive:

- repeat `--source=PATH` for a uniform action;
- use `--files0-from=FILE` for NUL-terminated uniform-action source paths;
- use `--operations-from=FILE` for mixed-action request JSONL;
- use `-` as the selected input file to read stdin.

The three batch input modes are mutually exclusive and cannot be combined with
positional sources. Uniform copy, move, and rename batches require
`--destination-dir`; each destination is that directory plus the source
basename. Archive and extract batches require explicit per-operation
destinations and therefore use request JSONL.

NUL and request-JSONL inputs are each capped at 64 MiB and read through byte
N+1. NUL input must contain no empty records and must terminate its last record
with NUL. It preserves newlines, tabs, and leading dashes. Invalid UTF-8 is
rejected explicitly because JSONL cannot round-trip an arbitrary invalid byte
sequence without changing the path. Request JSONL has one object per physical
line and uses this strict schema:

```jsonl
{"action":"delete","source":"old tree","recursive":true}
{"action":"copy","source":"artifact","destination":"archive/artifact"}
{"action":"truncate","source":"service.log","size":0}
```

Allowed fields are `action`, `source`, `destination`, `mode`, `uid`, `gid`,
`size`, `format`, and `recursive`. IDs, record types, and metadata guards are
not request fields: dirstat assigns deterministic `ACTION-ORDINAL` IDs and
captures fresh source and destination guards after validating the whole
request set. Unknown or duplicate fields, blank records, trailing values, and
action-incompatible fields are errors.

Field types and units are part of the request contract:

- `action` is one of `delete`, `copy`, `move`, `rename`, `mkdir`, `touch`,
  `truncate`, `chmod`, `chown`, `archive`, or `extract`.
- `source` and `destination` are UTF-8 path strings. Relative paths are resolved
  below the plan root; absolute paths must already be confined to it.
- `mode` is a JSON integer from 0 through 4095. It is a POSIX permission and
  special-bit mask written in decimal JSON (`384` is octal `0600`), not a text
  mode or a byte count.
- `uid` and `gid` are non-negative JSON integers.
- `size` is a non-negative JSON integer measured in bytes.
- `format` is a string naming `tar`, `tar.gz`, or `zip`; `tgz` and `gzip` are
  accepted aliases for `tar.gz`. When omitted, archive actions infer the format
  from the relevant filename.
- `recursive` is a JSON boolean. It is valid only for `delete`.

Omission is distinct from a zero value for the numeric fields: for example,
`"size":0`, `"uid":0`, and `"mode":0` are explicit instructions.

Exact requests are deduplicated while preserving first-seen order. If every
request is a delete, a recursive delete of a real directory also subsumes
delete requests for descendants. No descendant reduction occurs in a
mixed-action plan, and ambiguous duplicate destinations or ordered effects are
rejected by complete-plan validation. At most 100,000 operations are accepted.

No plan bytes are emitted, and no dirstat-managed `--output` file is created,
until confinement, normalization, fresh guard capture, and whole-plan
prevalidation all succeed. A shell redirection target is outside this guarantee
because the shell creates or truncates it before starting dirstat. Generated
plan output is capped at 64 MiB so it remains acceptable to `dirstat apply`.
`--summary` writes a separate JSON object to stderr; stdout remains plan JSONL
only. The summary includes input/final operation counts, action counts,
deduplication, and a hardlink-aware allocated-byte reclaim estimate for deletes
with an explicit completeness flag.

The summary is a versioned object with this exact schema:

- `type` is `"plan_summary"` and `version` is `1`;
- `input_operations`, `operations`, and `deduplicated_operations` are integer
  counts before and after normalization;
- `action_counts` maps action names to integer counts;
- `delete_reclaim_estimate_bytes` is a non-negative allocated-byte estimate;
- `reclaim_estimate_complete` says whether every candidate contributed a
  trustworthy estimate; and
- `reclaim_estimate_errors`, when present, is the number of candidates whose
  estimate could not be established.

Consumers must treat the reclaim figure as an estimate, not as a reservation or
an additive apparent-size total.

## Record framing

- The first physical line is one plan-header object.
- Every remaining physical line is one operation object.
- A final newline is optional; CRLF and LF line endings are accepted.
- Blank lines, multi-line objects, multiple values on one line, unknown or
  duplicate fields, and trailing non-whitespace data are errors.
- Operation IDs must be non-empty and unique.

For example:

```jsonl
{"type":"plan","version":2,"root":"/srv","created_at":"2026-07-11T12:00:00Z"}
{"type":"operation","id":"delete-1","action":"delete","source":"/srv/old.log","expected":{"path":"/srv/old.log","name":"old.log","kind":"file","mode":384,"mode_text":"-rw-------","size":4096,"allocated":4096,"modified_at":"2026-07-11T11:00:00Z","identity":{"device":1,"file":2,"valid":true}},"recursive":false}
```

The header fields are `type`, `version`, `root`, and optional `created_at`.
Operation fields are `type`, `id`, `action`, `source`, optional `destination`,
`expected`, `expected_destination`, `mode`, `uid`, `gid`, `size`, `format`, and
`recursive`. Action-specific fields are rejected when attached to an unrelated
action.

## Versions and guards

Readers accept plan versions 1 and 2. Version 1 remains available for existing
conflict-fail plans. Only version 2 can authorize `--conflict=overwrite`, and
every overwrite operation must carry `expected_destination` describing either
the reviewed absence or the reviewed object identity.

`expected` binds an existing source to its path, filesystem identity, size, and
modification time. Version 2 `expected_destination` binds the destination path
and records either `exists:false` or `exists:true` plus entry metadata.

## Complete-plan validation

Before opening the audit log or attempting an operation, apply validates every
record, action, action-specific field, confined source and destination, guard,
and ordered path dependency. It rejects duplicate targets, use after a prior
delete or move, and a pre-plan guard made stale by an earlier operation.
Ordered creation chains remain valid when later operations consume a path
produced earlier in the same plan without claiming pre-plan metadata for it and
the dependent preconditions can be established in advance. In particular,
`archive` followed by `extract` is valid; an arbitrary generated or copied file
cannot feed `extract`, because its archive policy could not be validated before
the prefix mutated.

Dynamic conditions are checked again immediately before each operation. A plan
is not a sandbox against another writer racing inside its root.

On Windows, `chmod`, `chown`, and an explicit `mode` on `mkdir` or `touch` are
rejected during this complete-plan pass. The rejection happens before an audit
file or an earlier valid operation can change the filesystem.

## Publication and cancellation

Directory copies are built in a sibling `.dirstat-copy-*` directory. Regular
files are synced, the staged object set is compared with the captured source,
the source is checked again, and the staging directory is published with an
atomic no-replace rename on Linux, macOS, and Windows. Failure or cancellation
before publication leaves the final destination absent. If staging cleanup
itself fails, the operation is `partial` and the error names the residue.

Recursive deletes capture the no-follow object set and remove it from leaves
to root. Context cancellation and object identity are checked between every
removal. An error before the first removal is `error`; an error after one or
more removals is `partial`, with the removed and captured counts in `error`.
Objects added concurrently are never added to the removal set.

## Results

Apply writes one JSONL result for each attempted operation or the operation
whose complete-plan validation failed. Result versions 1 and 2 remain readable;
new results use version 2. `status` is `ok`, `error`, or `partial`.
`may_have_mutated:true` accompanies `partial` when useful filesystem state was
published but every postcondition could not be completed. Callers must preserve
the error and reconcile filesystem state before retrying a partial operation.

`mutation_completed:true` means the filesystem operation itself completed. It
can accompany `status:"partial"` when the later audit outcome could not be made
durable. `audit_intent_status` describes the pre-operation record and
`audit_status` describes the outcome. Values are `disabled`, `not_attempted`,
`written`, `durable`, or `failed`. `written` is used for a caller-owned
`io.Writer` that has no `Sync` method; dirstat does not claim durability it
cannot establish.

## Audit stream ordering

For each attempted operation, dirstat attempts to append two result-shaped
JSONL records to an owned audit stream:

1. `status:"intent", audit_phase:"intent"` is appended and synced before the
   filesystem operation. A write or sync failure stops without mutation.
2. `audit_phase:"outcome"` carries the operation's `ok`, `error`, or `partial`
   result and is appended and synced after the operation.

When both appends succeed, the stream contains the intent followed by the
outcome. The record written to the file says `audit_status:"written"`: the returned
operation result changes that value to `durable` only after `Sync` succeeds. If
the operation completed but writing or syncing its outcome fails, the returned
result is `partial`, with `mutation_completed:true`,
`may_have_mutated:true`, and `audit_status:"failed"`. Owned-file close errors
are handled the same way rather than being discarded. In that case the durable
stream may contain only the intent; absence of an outcome is itself evidence
that reconciliation is required. The audit path may not be equal to or nested
inside any source or destination tree in the plan.
