#!/usr/bin/env bash
set -euo pipefail

tag="${1:?usage: package-release.sh TAG OS ARCH GOOS GOARCH ARCHIVE [OUTPUT_DIR]}"
os_name="${2:?missing display OS}"
arch_name="${3:?missing display architecture}"
goos="${4:?missing GOOS}"
goarch="${5:?missing GOARCH}"
archive="${6:?missing archive format}"
output_dir="${7:-dist}"

bash .github/scripts/validate-release-tag.sh "$tag"
if [[ "$(go env GOVERSION)" != "go1.26.5" ]]; then
  echo "release packaging requires go1.26.5; got $(go env GOVERSION)" >&2
  exit 1
fi
if [[ "$archive" != "tar.gz" && "$archive" != "zip" ]]; then
  echo "unsupported release archive format: $archive" >&2
  exit 1
fi

commit="$(git rev-parse HEAD)"
build_date="$(git show -s --format=%cI HEAD)"
source_epoch="$(git show -s --format=%ct HEAD)"
package_name="dirstat-${tag}-${os_name}-${arch_name}"
binary_name="dirstat"
if [[ "$goos" == "windows" ]]; then
  binary_name="dirstat.exe"
fi

mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
stage_root="$(mktemp -d)"
trap 'rm -rf "$stage_root"' EXIT
package_dir="$stage_root/$package_name"
mkdir -p "$package_dir"

CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
  -ldflags "-s -w -X github.com/phillipod/go-dirstat/internal/version.Version=$tag -X github.com/phillipod/go-dirstat/internal/version.Commit=$commit -X github.com/phillipod/go-dirstat/internal/version.BuildDate=$build_date" \
  -o "$package_dir/$binary_name" ./cmd/dirstat
cp LICENSE README.md "$package_dir/"
cat > "$package_dir/release-metadata.txt" <<EOF
version=$tag
commit=$commit
build_date=$build_date
toolchain=go1.26.5
target=$goos/$goarch
EOF
chmod 0755 "$package_dir" "$package_dir/$binary_name"
chmod 0644 "$package_dir/LICENSE" "$package_dir/README.md" "$package_dir/release-metadata.txt"

build_info="$(go version -m "$package_dir/$binary_name")"
grep -Fq 'go1.26.5' <<< "$build_info"
grep -Fq "GOOS=$goos" <<< "$build_info"
grep -Fq "GOARCH=$goarch" <<< "$build_info"

# Normalize mtimes and archive metadata so rebuilding the same commit is stable.
find "$package_dir" -exec touch -d "@$source_epoch" {} +
if [[ "$archive" == "zip" ]]; then
  (
    cd "$stage_root"
    find "$package_name" -print | LC_ALL=C sort | zip -X -q "$output_dir/$package_name.zip" -@
  )
else
  tar --sort=name --mtime="@$source_epoch" --owner=0 --group=0 --numeric-owner \
    -C "$stage_root" -cf - "$package_name" | gzip -n > "$output_dir/$package_name.tar.gz"
fi
