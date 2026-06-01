#!/usr/bin/env python3
"""A/B compare published AI summaries against a captured baseline.

Used to validate Phase L rollouts: collapse `module: capi | generic` into
a single universal agentic pipeline without regressing the quality of the
summaries published on the live dashboards.

Workflow
--------
1. Before rollout: capture per-site baseline JSON (one file per site) in the
   shape documented at files/baseline-ai-summaries/README.md. The required
   top-level keys are `site`, `base_url`, and `jobs[].runs_with_ai_summaries[]
   .test_cases[]` each carrying `{name, ai_summary, ai_analysis}`.
2. Roll out the change to one or more sites.
3. Wait for the next fetcher run to publish new `data/jobs/<name>.json`
   files against the same `base_url`.
4. Run this script with the same baseline paths. It refetches the current
   published data for the same (job, build, test) triples and writes a TSV
   report with per-case before/after deltas.

The report's `regression_flag` column is the single signal to act on:
true means at least one of these happened for that case:
  - the mode regressed from agentic → curator (or → empty);
  - the summary got >50% shorter;
  - relevant_files dropped by >50% AND was non-zero before;
  - the case became AI-unavailable (ai_summary present before, absent now).

Exit code is non-zero if any regression flag is true so CI can gate on it.
"""

from __future__ import annotations

import argparse
import csv
import json
import re
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Iterable


@dataclass
class Case:
    """One published AI analysis tied to a (job, build, test) triple."""

    site: str
    job_name: str
    build_id: str
    test_name: str
    ai_summary: str
    ai_analysis: dict | None

    @property
    def mode(self) -> str:
        if not self.ai_analysis:
            return ""
        return (self.ai_analysis or {}).get("mode", "") or ""

    @property
    def relevant_files(self) -> list[str]:
        if not self.ai_analysis:
            return []
        return list((self.ai_analysis or {}).get("relevant_files") or [])

    @property
    def root_cause(self) -> str:
        if not self.ai_analysis:
            return ""
        return (self.ai_analysis or {}).get("root_cause", "") or ""

    @property
    def severity(self) -> str:
        if not self.ai_analysis:
            return ""
        return (self.ai_analysis or {}).get("severity", "") or ""

    @property
    def is_transient(self) -> bool:
        # is_transient lives on AISummary (top-level field on the published
        # JSON), not on AIAnalysis. Baselines were captured before that
        # field was always present; treat missing as False.
        if not self.ai_analysis:
            return False
        # Some baselines stored is_transient alongside the analysis.
        return bool((self.ai_analysis or {}).get("is_transient", False))


def index_baseline(path: str) -> tuple[str, str, dict[tuple[str, str, str], Case]]:
    """Read a baseline file. Returns (site, base_url, {(job, build, test) -> Case})."""
    with open(path) as f:
        baseline = json.load(f)
    site = baseline.get("site") or ""
    base_url = (baseline.get("base_url") or "").rstrip("/")
    if not site or not base_url:
        raise SystemExit(f"baseline {path}: missing 'site' or 'base_url'")

    out: dict[tuple[str, str, str], Case] = {}
    for job in baseline.get("jobs", []):
        job_name = job.get("job_name", "")
        for run in job.get("runs_with_ai_summaries", []):
            build_id = str(run.get("build_id", ""))
            for tc in run.get("test_cases", []):
                # Baselines captured pre/post-bcca072 sometimes store
                # ai_summary as the AISummary object {generated_at,
                # summary, is_transient} and sometimes as the bare
                # summary string. Normalize to the summary text.
                ai_summary_obj = tc.get("ai_summary") or {}
                if isinstance(ai_summary_obj, dict):
                    summary_text = ai_summary_obj.get("summary", "") or ""
                else:
                    summary_text = str(ai_summary_obj or "")
                key = (job_name, build_id, tc.get("name", ""))
                out[key] = Case(
                    site=site,
                    job_name=job_name,
                    build_id=build_id,
                    test_name=tc.get("name", ""),
                    ai_summary=summary_text,
                    ai_analysis=tc.get("ai_analysis"),
                )
    return site, base_url, out


# job_name → slug used in the published data/jobs/<slug>.json path. The
# fetcher writes one file per job using the job name verbatim with `/`
# replaced by `_` (see backend/internal/output). We also fall back to a
# safer slug if a future migration changes the rule.
def job_to_slug(job_name: str) -> str:
    return job_name.replace("/", "_")


# Cache of fetched jobs files so multiple test cases under the same job
# don't re-download.
_jobs_cache: dict[str, dict | None] = {}


def fetch_current_job(base_url: str, job_name: str) -> dict | None:
    """Fetch the live `data/jobs/<slug>.json` for a job. Returns None if 404."""
    slug = job_to_slug(job_name)
    cache_key = f"{base_url}|{slug}"
    if cache_key in _jobs_cache:
        return _jobs_cache[cache_key]

    url = f"{base_url}/data/jobs/{slug}.json"
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "ab_compare/1.0"})
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        if e.code == 404:
            _jobs_cache[cache_key] = None
            return None
        raise
    _jobs_cache[cache_key] = data
    return data


def index_current(job_doc: dict, site: str) -> dict[tuple[str, str, str], Case]:
    """Index a published jobs file by (job, build, test_name)."""
    out: dict[tuple[str, str, str], Case] = {}
    job_name = job_doc.get("name") or job_doc.get("job_name") or ""
    # Published JobDetail uses "runs"; older code might use "builds" or
    # "recent_runs". Accept all three so the script works against any
    # past or future shape.
    runs = (
        job_doc.get("runs")
        or job_doc.get("builds")
        or job_doc.get("recent_runs")
        or []
    )
    for build in runs:
        build_id = str(build.get("build_id", ""))
        for tc in build.get("test_cases", []):
            ai_summary_obj = tc.get("ai_summary") or {}
            if isinstance(ai_summary_obj, dict):
                summary_text = ai_summary_obj.get("summary", "") or ""
            else:
                summary_text = str(ai_summary_obj or "")
            key = (job_name, build_id, tc.get("name", ""))
            out[key] = Case(
                site=site,
                job_name=job_name,
                build_id=build_id,
                test_name=tc.get("name", ""),
                ai_summary=summary_text,
                ai_analysis=tc.get("ai_analysis"),
            )
    return out


# Roughly counts inline quotes ("...") in the root_cause text. Used as a
# heuristic for "did the AI cite a specific error line".
_QUOTE_RE = re.compile(r'"[^"]{8,}"')


def has_root_cause_quote(case: Case) -> bool:
    return bool(_QUOTE_RE.search(case.root_cause))


def length_ratio(after: str, before: str) -> float:
    if not before:
        return 1.0 if after else 0.0
    return round(len(after) / len(before), 3)


def compare_row(before: Case, after: Case | None) -> dict[str, object]:
    after_summary = after.ai_summary if after else ""
    after_mode = after.mode if after else ""
    after_files = after.relevant_files if after else []
    after_severity = after.severity if after else ""

    before_files = before.relevant_files
    files_delta = len(after_files) - len(before_files)
    files_drop_ratio = (
        (len(before_files) - len(after_files)) / len(before_files)
        if before_files
        else 0.0
    )

    ai_unavailable_before = not before.ai_summary
    ai_unavailable_after = not after_summary
    became_unavailable = (not ai_unavailable_before) and ai_unavailable_after

    mode_regressed = before.mode == "agentic" and after_mode in ("curator", "")

    lratio = length_ratio(after_summary, before.ai_summary)
    severity_flip = before.severity != after_severity and before.severity != ""

    regression = (
        mode_regressed
        or became_unavailable
        or (lratio < 0.5 and before.ai_summary != "")
        or (files_drop_ratio > 0.5 and len(before_files) > 0)
    )

    return {
        "site": before.site,
        "job_name": before.job_name,
        "build_id": before.build_id,
        "test_name": before.test_name,
        "mode_before": before.mode or "missing",
        "mode_after": after_mode or ("missing" if after else "absent"),
        "length_ratio": lratio,
        "relevant_files_before": len(before_files),
        "relevant_files_after": len(after_files),
        "relevant_files_delta": files_delta,
        "has_root_cause_quote_before": has_root_cause_quote(before),
        "has_root_cause_quote_after": has_root_cause_quote(after) if after else False,
        "severity_before": before.severity or "",
        "severity_after": after_severity or "",
        "severity_flip": severity_flip,
        "ai_unavailable_after": became_unavailable,
        "regression_flag": regression,
    }


# Stable column order for the TSV report. Matches the keys built in
# compare_row() above.
COLUMNS = [
    "site",
    "job_name",
    "build_id",
    "test_name",
    "mode_before",
    "mode_after",
    "length_ratio",
    "relevant_files_before",
    "relevant_files_after",
    "relevant_files_delta",
    "has_root_cause_quote_before",
    "has_root_cause_quote_after",
    "severity_before",
    "severity_after",
    "severity_flip",
    "ai_unavailable_after",
    "regression_flag",
]


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description="A/B compare AI summaries vs baseline")
    ap.add_argument(
        "baselines",
        nargs="+",
        help="One or more baseline JSON files (one per site).",
    )
    ap.add_argument(
        "-o",
        "--output",
        default="-",
        help="TSV output path (default: stdout).",
    )
    args = ap.parse_args(argv)

    rows: list[dict[str, object]] = []
    regression_count = 0
    total = 0

    for baseline_path in args.baselines:
        site, base_url, baseline_idx = index_baseline(baseline_path)
        print(
            f"[{site}] {len(baseline_idx)} baseline cases from {baseline_path}",
            file=sys.stderr,
        )

        # Group baseline cases by job to fetch each published file once.
        jobs_needed = {key[0] for key in baseline_idx}
        per_job_current: dict[str, dict[tuple[str, str, str], Case]] = {}
        for job_name in sorted(jobs_needed):
            doc = fetch_current_job(base_url, job_name)
            if doc is None:
                print(
                    f"[{site}] WARN: published jobs file missing for {job_name}",
                    file=sys.stderr,
                )
                continue
            per_job_current[job_name] = index_current(doc, site)

        for key, before in baseline_idx.items():
            current_for_job = per_job_current.get(key[0], {})
            after = current_for_job.get(key)
            row = compare_row(before, after)
            rows.append(row)
            total += 1
            if row["regression_flag"]:
                regression_count += 1

    # Write the TSV report.
    if args.output == "-":
        writer = csv.DictWriter(
            sys.stdout, fieldnames=COLUMNS, delimiter="\t", extrasaction="ignore"
        )
        writer.writeheader()
        writer.writerows(rows)
    else:
        with open(args.output, "w", newline="") as f:
            writer = csv.DictWriter(
                f, fieldnames=COLUMNS, delimiter="\t", extrasaction="ignore"
            )
            writer.writeheader()
            writer.writerows(rows)
        print(f"Wrote {len(rows)} rows to {args.output}", file=sys.stderr)

    print(
        f"\nSummary: {regression_count} regression(s) across {total} cases",
        file=sys.stderr,
    )
    return 1 if regression_count > 0 else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
