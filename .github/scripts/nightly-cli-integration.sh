#!/usr/bin/env bash
set -euo pipefail

dirstat="${DIRSTAT:?DIRSTAT must name the built CLI}"
dirstat="$(cd "$(dirname "$dirstat")" && pwd)/$(basename "$dirstat")"
artifact_dir="${NIGHTLY_ARTIFACT_DIR:?NIGHTLY_ARTIFACT_DIR is required}"
work="$artifact_dir/work"
subject="$work/subject"
output="$work/output"
state="$work/state"
rm -rf "$artifact_dir"
mkdir -p "$subject/sub" "$subject/empty" "$output" "$state"

printf 'alpha\n' > "$subject/alpha.txt"
printf 'beta payload\n' > "$subject/sub/payload.bin"

(
  cd "$work"
  "$dirstat" --apparent --format=tsv --bytes --files --sort=name subject > output/tree.tsv
  "$dirstat" extensions --apparent --bytes --no-color --no-bar subject > output/extensions.txt
  "$dirstat" status --format=json subject > output/status.json
  "$dirstat" --apparent query --format=tsv --fields=relative,kind,apparent,files,directories --sort=path subject > output/query.tsv
  "$dirstat" inspect --format=json --content --limit=16 subject/alpha.txt > output/inspect.json
  "$dirstat" history --store state/history growth --format=json subject > output/history-baseline.json
  printf '++' >> subject/alpha.txt
  "$dirstat" history --store state/history growth --format=json subject > output/history-growth.json
  "$dirstat" plan --root subject --output output/touch.plan.jsonl touch generated.txt
  "$dirstat" apply --dry-run --no-audit output/touch.plan.jsonl > output/dry-run.jsonl
  test ! -e subject/generated.txt
  "$dirstat" apply --yes --no-audit output/touch.plan.jsonl > output/applied.jsonl
  test -f subject/generated.txt
)

grep -q '^By extension$' "$output/extensions.txt"
grep -q '^Largest files$' "$output/extensions.txt"
grep -q '"total_bytes"' "$output/status.json"
grep -q '"preview"' "$output/inspect.json"
go run ./.github/scripts/verify-nightly-history.go \
  "$subject" "$output/tree.tsv" "$output/query.tsv" \
  "$output/history-baseline.json" "$output/history-growth.json"

actual_files="$(cd "$subject" && find . -type f -print | LC_ALL=C sort)"
expected_files="$(printf './alpha.txt\n./generated.txt\n./sub/payload.bin\n')"
if [[ "$actual_files" != "$expected_files" ]]; then
  printf 'nightly output or state leaked into the scanned subject:\n%s\n' "$actual_files" >&2
  exit 1
fi
