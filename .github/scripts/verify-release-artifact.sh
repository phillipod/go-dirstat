#!/usr/bin/env bash
set -euo pipefail

tag="${1:?usage: verify-release-artifact.sh TAG OS ARCH GOOS GOARCH ARCHIVE_PATH COMMIT BUILD_DATE [RUN_BINARY]}"
os_name="${2:?missing display OS}"
arch_name="${3:?missing display architecture}"
goos="${4:?missing GOOS}"
goarch="${5:?missing GOARCH}"
archive_path="${6:?missing archive path}"
commit="${7:?missing commit}"
build_date="${8:?missing build date}"
run_binary="${9:-false}"

package_name="dirstat-${tag}-${os_name}-${arch_name}"
binary_name="dirstat"
if [[ "$goos" == "windows" ]]; then
  binary_name="dirstat.exe"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
case "$archive_path" in
  *.zip) unzip -q "$archive_path" -d "$work" ;;
  *.tar.gz) tar -xzf "$archive_path" -C "$work" ;;
  *) echo "unsupported release archive: $archive_path" >&2; exit 1 ;;
esac

package_dir="$work/$package_name"
test -d "$package_dir"
unexpected_entries="$(cd "$package_dir" && find . ! -path . ! -type f -print)"
if [[ -n "$unexpected_entries" ]]; then
  printf 'unexpected non-regular archive entries:\n%s\n' "$unexpected_entries" >&2
  exit 1
fi
actual_files="$(cd "$package_dir" && find . -type f -print | LC_ALL=C sort)"
expected_files="$(printf './LICENSE\n./README.md\n./%s\n./release-metadata.txt' "$binary_name" | LC_ALL=C sort)"
if [[ "$actual_files" != "$expected_files" ]]; then
  printf 'unexpected archive layout:\n%s\n' "$actual_files" >&2
  exit 1
fi

metadata="$package_dir/release-metadata.txt"
grep -Fxq "version=$tag" "$metadata"
grep -Fxq "commit=$commit" "$metadata"
grep -Fxq "build_date=$build_date" "$metadata"
grep -Fxq 'toolchain=go1.26.5' "$metadata"
grep -Fxq "target=$goos/$goarch" "$metadata"

binary="$package_dir/$binary_name"
build_info="$(go version -m "$binary")"
grep -Fq 'go1.26.5' <<< "$build_info"
grep -Fq "GOOS=$goos" <<< "$build_info"
grep -Fq "GOARCH=$goarch" <<< "$build_info"

if [[ "$run_binary" == "true" ]]; then
  version_output="$("$binary" version)"
  expected_version="dirstat $tag ($commit) built $build_date"
  if [[ "$version_output" != "$expected_version" ]]; then
    printf 'version mismatch:\nwant: %s\ngot:  %s\n' "$expected_version" "$version_output" >&2
    exit 1
  fi
  "$binary" --help >/dev/null

  fixture="$work/smoke-subject"
  output="$work/smoke-output"
  mkdir -p "$fixture/sub" "$output"
  printf 'alpha\n' > "$fixture/alpha.txt"
  printf 'beta payload\n' > "$fixture/sub/payload.bin"
  "$binary" --apparent --format=tsv --bytes --files --sort=name "$fixture" > "$output/tree.tsv"
  "$binary" --apparent query --kind=file --format=tsv --fields=relative,apparent --sort=path "$fixture" > "$output/query.tsv"
  printf 'alpha.txt\t6\nsub/payload.bin\t13\n' > "$output/query.expected.tsv"
  cmp "$output/query.expected.tsv" "$output/query.tsv"
fi
