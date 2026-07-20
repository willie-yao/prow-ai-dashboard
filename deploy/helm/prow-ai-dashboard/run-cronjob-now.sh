#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: run-cronjob-now.sh [--wait] [--timeout <duration>] <namespace> <cronjob>

Create a uniquely named Job from a dashboard CronJob. The command refuses to
start alongside an active scheduled Job or an earlier active manual Job.
USAGE
}

wait_for_job=false
timeout=90m
while [[ $# -gt 0 ]]; do
  case $1 in
    --wait)
      wait_for_job=true
      shift
      ;;
    --timeout)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      timeout=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    -*)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      break
      ;;
  esac
done
if [[ $# -ne 2 ]]; then
  usage >&2
  exit 2
fi

namespace=$1
cronjob=$2
kubectl_bin=${KUBECTL:-kubectl}
active=$($kubectl_bin -n "$namespace" get cronjob "$cronjob" -o jsonpath='{range .status.active[*]}{.name}{"\n"}{end}')
if [[ -n $active ]]; then
  echo "CronJob $namespace/$cronjob already has an active Job: $active" >&2
  exit 1
fi
label_key=prow-ai-dashboard.willieyao.dev/source-cronjob
manual_jobs=$($kubectl_bin -n "$namespace" get jobs -l "$label_key=$cronjob" -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[*]}{.type}{"="}{.status}{","}{end}{"\n"}{end}')
manual_active=""
while IFS=$'\t' read -r name conditions; do
  [[ -z $name ]] && continue
  if [[ $conditions != *"Complete=True"* && $conditions != *"Failed=True"* ]]; then
    manual_active+="${manual_active:+, }$name"
  fi
done <<< "$manual_jobs"
if [[ -n $manual_active ]]; then
  echo "CronJob $namespace/$cronjob already has an active manual Job: $manual_active" >&2
  exit 1
fi

suffix=$(date -u +%Y%m%d%H%M%S)
max_prefix=$((63 - 1 - ${#suffix}))
prefix=${cronjob:0:max_prefix}
prefix=${prefix%-}
job_name="$prefix-$suffix"

job_manifest=$($kubectl_bin -n "$namespace" create job --from="cronjob/$cronjob" "$job_name" --dry-run=client -o yaml)
labeled_manifest=$(printf '%s\n' "$job_manifest" | $kubectl_bin label --local -f - "$label_key=$cronjob" -o yaml)
printf '%s\n' "$labeled_manifest" | $kubectl_bin -n "$namespace" create -f -

echo "Created Job $namespace/$job_name"
echo "Follow: kubectl -n $namespace logs -f job/$job_name --all-containers=true --prefix=true"
echo "Status: kubectl -n $namespace get job/$job_name"

if [[ $wait_for_job == true ]]; then
  if ! $kubectl_bin -n "$namespace" wait --for=condition=complete --timeout="$timeout" "job/$job_name"; then
    $kubectl_bin -n "$namespace" describe "job/$job_name" >&2 || true
    exit 1
  fi
  $kubectl_bin -n "$namespace" logs "job/$job_name" --all-containers=true --prefix=true
fi
