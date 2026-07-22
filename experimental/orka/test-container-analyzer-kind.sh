#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/test-container-analyzer-kind.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

stub_dir="$tmp/bin"
mkdir -p "$stub_dir"
for command in docker kubectl helm git curl tar go; do
  cat > "$stub_dir/$command" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
  chmod +x "$stub_dir/$command"
done

cat > "$stub_dir/kind" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "get clusters")
    if [[ ${KIND_EXISTING_CLUSTER:-} != "" ]]; then
      printf '%s\n' "$KIND_EXISTING_CLUSTER"
    fi
    ;;
  "create cluster")
    exit 1
    ;;
  "delete cluster")
    printf 'delete\n' >> "$KIND_DELETE_MARKER"
    ;;
  *)
    exit 1
    ;;
esac
STUB
chmod +x "$stub_dir/kind"

marker="$tmp/deletes"
if PATH="$stub_dir:$PATH" \
  KIND_EXISTING_CLUSTER=existing-cluster \
  KIND_DELETE_MARKER="$marker" \
  ORKA_CONTAINER_CLUSTER=existing-cluster \
  "$script_dir/run-container-analyzer-kind.sh" >"$tmp/existing.out" 2>&1; then
  echo "existing cluster was accepted" >&2
  exit 1
fi
grep -Fq 'already exists' "$tmp/existing.out"
if [[ -e $marker ]]; then
  echo "pre-existing cluster was deleted" >&2
  exit 1
fi

if PATH="$stub_dir:$PATH" \
  KIND_DELETE_MARKER="$marker" \
  ORKA_CONTAINER_CLUSTER=partial-cluster \
  "$script_dir/run-container-analyzer-kind.sh" >"$tmp/partial.out" 2>&1; then
  echo "failed kind create unexpectedly succeeded" >&2
  exit 1
fi
if [[ $(wc -l < "$marker") -ne 1 ]]; then
  echo "partial cluster cleanup was not attempted exactly once" >&2
  exit 1
fi

echo "Container analyzer kind ownership checks passed."
