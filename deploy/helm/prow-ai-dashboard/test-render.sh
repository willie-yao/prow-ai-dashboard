#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
chart="$root/deploy/helm/prow-ai-dashboard"
tmp="${TMPDIR:-/tmp}/prow-ai-dashboard-helm-$$"
mkdir -p "$tmp"

cat > "$tmp/values.yaml" <<'VALUES'
image:
  tag: sha-test
project:
  config: |
    id: test
    name: Test
    testgrid:
      dashboard: test
    storage:
      provider: local
      base: /tmp
    branding:
      title: Test
      base_path: /
      site_url: https://example.test
  systemPrompt: test prompt
VALUES

for file in 30-tools.yaml 35-k8s-tools.yaml 36-validate-tool.yaml \
  37-verify-timeline.yaml 38-transient-signatures.yaml 39-recurrence.yaml \
  40-diff-last-passing.yaml 41-required-evidence.yaml; do
  cmp "$root/experimental/orka/manifests/$file" "$chart/files/orka-tools/$file"
done

helm lint "$chart" -f "$tmp/values.yaml"
helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" > "$tmp/inprocess.yaml"
if grep -Fq 'test-prow-ai-dashboard-artifact-tool' "$tmp/inprocess.yaml"; then
  echo 'in-process render unexpectedly created Orka artifact Tool resources' >&2
  exit 1
fi

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.namespace=orka-test \
  --set orka.artifactTool.nodeSelector.agentpool=nodepool1 \
  > "$tmp/owned.yaml"

grep -Fq 'name: test-prow-ai-dashboard-artifact-tool' "$tmp/owned.yaml"
grep -Fq 'namespace: orka-test' "$tmp/owned.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/orka-artifact-tool:sha-test' "$tmp/owned.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/orka-producer:sha-test' "$tmp/owned.yaml"
grep -Fq 'image: ghcr.io/willie-yao/prow-ai-dashboard/orka-ingestor:sha-test' "$tmp/owned.yaml"
grep -Fq 'http://test-prow-ai-dashboard-artifact-tool.orka-test.svc:8080/tool/read_artifact' "$tmp/owned.yaml"
grep -Fq -- '-tool-auth-secret=test-prow-ai-dashboard-artifact-tool-auth' "$tmp/owned.yaml"
grep -Fq 'name: test-prow-ai-dashboard-orka-tools' "$tmp/owned.yaml"
grep -Fq 'agentpool: nodepool1' "$tmp/owned.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.namespace=orka-test \
  --set orka.artifactTool.enabled=false \
  --set orka.artifactTool.baseURL=http://shared-tools.orka-test.svc:9090 \
  --set orka.artifactTool.auth.existingSecret=shared-tool-auth \
  > "$tmp/external.yaml"

if grep -Fq 'name: test-prow-ai-dashboard-artifact-tool' "$tmp/external.yaml"; then
  echo 'external artifact Tool render unexpectedly created release-owned resources' >&2
  exit 1
fi
grep -Fq 'http://shared-tools.orka-test.svc:9090/tool/read_artifact' "$tmp/external.yaml"
grep -Fq -- '-tool-auth-secret=shared-tool-auth' "$tmp/external.yaml"

helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron \
  --set analysis=orka \
  --set orka.baseTools.create=false \
  --set orka.baseTools.existingConfigMap=shared-tools \
  > "$tmp/existing-tools.yaml"
grep -Fq 'name: shared-tools' "$tmp/existing-tools.yaml"

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --set analysis=orka \
  --set orka.artifactTool.enabled=false > "$tmp/invalid-artifact.yaml" 2>&1; then
  echo 'missing external artifact Tool values were accepted' >&2
  exit 1
fi

if helm template test "$chart" -n dashboard-test -f "$tmp/values.yaml" \
  --set mode=cron --set analysis=orka \
  --set orka.baseTools.create=false > "$tmp/invalid-tools.yaml" 2>&1; then
  echo 'missing base Tool ConfigMap values were accepted' >&2
  exit 1
fi

echo 'Helm render checks passed.'
