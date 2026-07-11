#!/usr/bin/env bash
set -euo pipefail

tag="${1:?usage: validate-release-tag.sh TAG}"
semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
if [[ ! "$tag" =~ $semver_re ]]; then
  echo "release tag must be v-prefixed SemVer, such as v1.2.3 or v1.2.3-rc.1; got: $tag" >&2
  exit 1
fi
