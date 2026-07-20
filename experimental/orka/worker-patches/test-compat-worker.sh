#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../../.." && pwd)
script="$script_dir/compat-worker.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

bash -n "$script"
"$script" verify
metadata=$("$script" metadata 0123456789abcdef0123456789abcdef01234567)
grep -Fq 'orka_commit=1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254' <<< "$metadata"
grep -Fq 'patch_sha256=09d1a609b2a45d359464ae17bd6a5c183ece235c7cac0fc27d046bda3b928350' <<< "$metadata"
backtick='`'
grep -Fq "${backtick}1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254${backtick}" "$script_dir/COMPATIBILITY.md"
grep -Fq "${backtick}09d1a609b2a45d359464ae17bd6a5c183ece235c7cac0fc27d046bda3b928350${backtick}" "$script_dir/COMPATIBILITY.md"
workflow="$repo_root/.github/workflows/orka-compat-image.yml"
grep -Fq 'experimental/orka/worker-patches/compat-worker.sh prepare _orka' "$workflow"
grep -Fq 'push: false' "$workflow"
grep -Fq 'push: true' "$workflow"
if [[ $(grep -Fc 'packages: write' "$workflow") -ne 1 ]]; then
  echo 'package write permission must be limited to the publish job' >&2
  exit 1
fi
tag=$(awk -F= '$1 == "image_tag" { print $2 }' <<< "$metadata")
[[ $tag == v1-orka-1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254-dashboard-0123456789abcdef0123456789abcdef01234567 ]]
(( ${#tag} <= 128 ))
if "$script" metadata short > /dev/null 2>&1; then
  echo 'short dashboard commit was accepted' >&2
  exit 1
fi


mkdir -p "$tmp/bin"
cat > "$tmp/bin/docker" <<'EOF_DOCKER'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "buildx imagetools inspect "* ]] || { echo "unexpected docker invocation: $*" >&2; exit 3; }
case ${FAKE_REGISTRY_RESULT:-} in
  exists) echo '{"manifest":"present"}'; exit 0 ;;
  missing) echo 'ERROR: ghcr.io/example/worker:test: not found' >&2; exit 1 ;;
  missing404) echo 'ERROR: unexpected status from HEAD request: 404 Not Found' >&2; exit 1 ;;
  error) echo 'ERROR: unexpected status from HEAD request: 401 Unauthorized' >&2; exit 1 ;;
  *) echo 'missing FAKE_REGISTRY_RESULT' >&2; exit 3 ;;
esac
EOF_DOCKER
chmod +x "$tmp/bin/docker"
FAKE_REGISTRY_RESULT=missing PATH="$tmp/bin:$PATH" "$script" assert-tag-absent ghcr.io/example/worker:test
FAKE_REGISTRY_RESULT=missing404 PATH="$tmp/bin:$PATH" "$script" assert-tag-absent ghcr.io/example/worker:test
if FAKE_REGISTRY_RESULT=exists PATH="$tmp/bin:$PATH" "$script" assert-tag-absent ghcr.io/example/worker:test > /dev/null 2>&1; then
  echo 'existing compatibility tag was accepted' >&2
  exit 1
fi
if FAKE_REGISTRY_RESULT=error PATH="$tmp/bin:$PATH" "$script" assert-tag-absent ghcr.io/example/worker:test > /dev/null 2>&1; then
  echo 'registry inspection error was treated as tag absence' >&2
  exit 1
fi

echo 'Orka compatibility metadata checks passed.'
