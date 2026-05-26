import React, { useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useJobDetail } from "../hooks/useData";
import { formatDuration, timeAgo, fileToUrl, fileSortKey, formatSteps } from "../lib/utils";
import { DurationChart } from "../components/DurationChart";
import { RunTimeline } from "../components/RunTimeline";
import type { BuildResult, TestCase } from "../types/dashboard";
import {
  HiSparkles,
  HiClipboardDocumentList,
  HiArchiveBox,
  HiCloud,
  HiServerStack,
  HiMapPin,
  HiChevronRight,
} from "react-icons/hi2";

/** Strip numbers and hex strings to normalize error messages for grouping. */
function normalizeMessage(msg: string): string {
  return msg
    .replace(/0x[0-9a-fA-F]+/g, "…")
    .replace(/[0-9a-f]{8,}/gi, "…")
    .replace(/\d+/g, "…")
    .replace(/…[.…]+/g, "…")
    .trim();
}

/** Highlight Go file:line references in stack traces */
const goFileLineRe = /([a-zA-Z0-9_/.\-@]+\.go:\d+)/g;

function highlightStackTrace(body: string): (string | React.ReactElement)[] {
  const parts: (string | React.ReactElement)[] = [];
  let lastIndex = 0;
  let match: RegExpExecArray | null;
  let key = 0;

  while ((match = goFileLineRe.exec(body)) !== null) {
    if (match.index > lastIndex) {
      parts.push(body.slice(lastIndex, match.index));
    }
    parts.push(
      <span key={key++} className="text-primary">
        {match[1]}
      </span>
    );
    lastIndex = match.index + match[0].length;
  }
  if (lastIndex < body.length) {
    parts.push(body.slice(lastIndex));
  }
  return parts;
}

interface TestOccurrence {
  run: BuildResult;
  testCase: TestCase | null; // null means absent from this run
}

interface FailureGroup {
  normalizedMessage: string;
  sampleMessage: string;
  count: number;
}

export function TestDetailPage() {
  const { jobName, testName: encodedTestName } = useParams<{
    jobName: string;
    testName: string;
  }>();
  const testName = encodedTestName ? decodeURIComponent(encodedTestName) : "";
  const { data, loading, error } = useJobDetail(jobName);
  const [searchParams] = useSearchParams();
  const [selectedBuildId, setSelectedBuildId] = useState<string | null>(
    searchParams.get("run")
  );

  // Build per-run test occurrences (oldest first for timeline)
  const occurrences: TestOccurrence[] = useMemo(() => {
    if (!data) return [];
    const sorted = [...(data.runs ?? [])].sort(
      (a, b) => new Date(a.started).getTime() - new Date(b.started).getTime()
    );
    return sorted.map((run) => {
      const tc =
        (run.test_cases ?? []).find((t) => t.name === testName) ?? null;
      return { run, testCase: tc };
    });
  }, [data, testName]);

  // Most recent occurrence that actually has this test
  const latestOccurrence = useMemo(() => {
    for (let i = occurrences.length - 1; i >= 0; i--) {
      if (occurrences[i].testCase) return occurrences[i];
    }
    return null;
  }, [occurrences]);

  // Failure classification
  const classification = useMemo(() => {
    if (!latestOccurrence) return null;
    // Count consecutive failures from the latest run backwards
    let consecutive = 0;
    for (let i = occurrences.length - 1; i >= 0; i--) {
      const tc = occurrences[i].testCase;
      if (!tc) continue; // skip runs where test wasn't present
      if (tc.status === "failed") consecutive++;
      else break;
    }
    if (consecutive === 0) return null;

    const failedRuns = occurrences.filter(
      (o) => o.testCase?.status === "failed"
    );
    const presentRuns = occurrences.filter((o) => o.testCase !== null);
    const passedRuns = presentRuns.filter(
      (o) => o.testCase!.status === "passed"
    );

    if (consecutive >= 3) return `Persistent (${consecutive}×)`;
    if (failedRuns.length > 1 && passedRuns.length > 0) return "Flaky";
    return "One-off";
  }, [occurrences, latestOccurrence]);

  // Failure pattern grouping
  const failureGroups: FailureGroup[] = useMemo(() => {
    const failures = occurrences.filter(
      (o) => o.testCase?.status === "failed" && o.testCase?.failure_message
    );
    if (failures.length === 0) return [];

    const groups = new Map<string, { sample: string; count: number }>();
    for (const f of failures) {
      const msg = f.testCase!.failure_message!;
      const key = normalizeMessage(msg);
      const existing = groups.get(key);
      if (existing) {
        existing.count++;
      } else {
        groups.set(key, { sample: msg, count: 1 });
      }
    }

    return Array.from(groups.entries())
      .map(([normalized, { sample, count }]) => ({
        normalizedMessage: normalized,
        sampleMessage: sample,
        count,
      }))
      .sort((a, b) => b.count - a.count);
  }, [occurrences]);

  const totalFailures = occurrences.filter(
    (o) => o.testCase?.status === "failed"
  ).length;

  // Selected run
  const effectiveSelectedId =
    selectedBuildId ?? latestOccurrence?.run.build_id ?? null;
  const selectedOccurrence = useMemo(() => {
    if (!effectiveSelectedId) return null;
    return (
      occurrences.find((o) => o.run.build_id === effectiveSelectedId) ?? null
    );
  }, [occurrences, effectiveSelectedId]);

  if (loading) {
    return (
      <div className="flex items-center justify-center py-32">
        <svg
          className="h-8 w-8 animate-spin text-primary"
          xmlns="http://www.w3.org/2000/svg"
          fill="none"
          viewBox="0 0 24 24"
        >
          <circle
            className="opacity-25"
            cx="12"
            cy="12"
            r="10"
            stroke="currentColor"
            strokeWidth="4"
          />
          <path
            className="opacity-75"
            fill="currentColor"
            d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"
          />
        </svg>
      </div>
    );
  }

  if (error) {
    return (
      <div className="flex flex-col items-center justify-center gap-4 py-32 text-center">
        <p className="text-error text-lg">Failed to load job details</p>
        <p className="text-on-surface-variant text-sm">{error}</p>
        <button
          onClick={() => window.location.reload()}
          className="rounded-lg bg-primary px-4 py-2 text-sm font-medium text-on-primary transition-colors hover:bg-primary-dim"
        >
          Retry
        </button>
      </div>
    );
  }

  if (!data) return null;

  const testFound = occurrences.some((o) => o.testCase !== null);
  if (!testFound) {
    return (
      <div className="space-y-8">
        <nav className="font-label flex items-center gap-2 text-sm text-on-surface-variant">
          <Link to="/" className="transition-colors hover:text-primary">
            Dashboard
          </Link>
          <span>›</span>
          <Link
            to={`/job/${encodeURIComponent(jobName ?? "")}`}
            className="transition-colors hover:text-primary"
          >
            {jobName}
          </Link>
          <span>›</span>
          <span className="text-on-surface truncate">{testName}</span>
        </nav>
        <div className="glass rounded-xl p-8 text-center">
          <p className="text-on-surface-variant">
            Test not found in any run of this job.
          </p>
        </div>
      </div>
    );
  }

  const selectedTc = selectedOccurrence?.testCase ?? null;
  const selectedRun = selectedOccurrence?.run ?? null;
  const displayStatus = selectedTc?.status ?? latestOccurrence?.testCase?.status ?? "skipped";

  return (
    <div className="space-y-6 sm:space-y-8">
      {/* Breadcrumb */}
      <nav className="font-label flex items-center gap-2 text-sm text-on-surface-variant">
        <Link to="/" className="transition-colors hover:text-primary">
          Dashboard
        </Link>
        <span>›</span>
        <Link
          to={`/job/${encodeURIComponent(jobName ?? "")}${effectiveSelectedId ? `?run=${effectiveSelectedId}` : ""}`}
          className="transition-colors hover:text-primary"
        >
          {jobName}
        </Link>
        <span>›</span>
        <span className="text-on-surface truncate max-w-md" title={testName}>
          {testName}
        </span>
      </nav>

      {/* Test header */}
      <div>
        <h1 className="font-headline text-xl sm:text-2xl font-bold text-on-surface break-all">
          {testName}
        </h1>
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <span
            className={`rounded-full px-2.5 py-0.5 text-xs font-medium ${
              displayStatus === "passed"
                ? "bg-secondary/20 text-secondary"
                : displayStatus === "failed"
                  ? "bg-error/20 text-error"
                  : "bg-on-surface-variant/20 text-on-surface-variant"
            }`}
          >
            {displayStatus.charAt(0).toUpperCase() + displayStatus.slice(1)}
          </span>
          {classification && (
            <span
              className={`rounded-full px-2.5 py-0.5 text-xs font-medium ${
                classification.startsWith("Persistent")
                  ? "bg-error/20 text-error"
                  : classification === "Flaky"
                    ? "bg-tertiary/20 text-tertiary"
                    : "bg-on-surface-variant/20 text-on-surface-variant"
              }`}
            >
              {classification}
            </span>
          )}
        </div>
      </div>

      {/* Pass/fail history bar */}
      <section>
        <h2 className="font-headline mb-3 text-lg font-semibold text-on-surface">
          History
        </h2>
        <RunTimeline
          runs={data?.runs ?? []}
          selectedBuildId={effectiveSelectedId ?? undefined}
          onSelect={setSelectedBuildId}
          colorFn={(run) => {
            const tc = (run.test_cases ?? []).find((t) => t.name === testName);
            if (!tc) return "bg-on-surface-variant/30";
            return tc.status === "passed"
              ? "bg-secondary"
              : tc.status === "failed"
                ? "bg-error"
                : "bg-on-surface-variant";
          }}
          tooltipFn={(run) => {
            const tc = (run.test_cases ?? []).find((t) => t.name === testName);
            return tc
              ? `#${run.build_id} — ${tc.status.charAt(0).toUpperCase() + tc.status.slice(1)}`
              : `#${run.build_id} — Absent`;
          }}
        />
      </section>

      {/* Failure pattern grouping */}
      {failureGroups.length > 0 && (
        <section>
          <h2 className="font-headline mb-3 text-lg font-semibold text-on-surface">
            Failure Patterns
          </h2>
          <div className="glass rounded-xl p-4 space-y-2">
            {failureGroups.map((group, i) => (
              <div
                key={i}
                className="flex items-start gap-3 text-sm"
              >
                <span className="shrink-0 rounded-full bg-error/20 px-2 py-0.5 font-label text-xs font-medium text-error">
                  {group.count} of {totalFailures}
                </span>
                <p className="min-w-0 truncate text-on-surface-variant" title={group.sampleMessage}>
                  {group.sampleMessage.length > 120
                    ? group.sampleMessage.slice(0, 120) + "…"
                    : group.sampleMessage}
                </p>
              </div>
            ))}
          </div>
        </section>
      )}

      {/* Selected failure detail */}
      {selectedRun && selectedTc && (
        <section className="glass rounded-xl p-4 sm:p-6 space-y-5">
          <div className="flex items-center gap-3">
            <h3 className="font-headline text-base font-semibold text-on-surface">
              Run Detail
            </h3>
            <span
              className={`inline-block h-2.5 w-2.5 rounded-full ${
                selectedTc.status === "passed"
                  ? "bg-secondary"
                  : selectedTc.status === "failed"
                    ? "bg-error"
                    : "bg-on-surface-variant"
              }`}
            />
          </div>

          <div className="grid grid-cols-1 gap-x-8 gap-y-3 text-sm sm:grid-cols-2 lg:grid-cols-3">
            <div>
              <span className="font-label text-xs text-on-surface-variant">
                Build ID
              </span>
              <p className="text-on-surface">{selectedRun.build_id}</p>
            </div>
            <div>
              <span className="font-label text-xs text-on-surface-variant">
                Started
              </span>
              <p className="text-on-surface">
                {new Date(selectedRun.started).toLocaleString()}
              </p>
            </div>
            <div>
              <span className="font-label text-xs text-on-surface-variant">
                Duration
              </span>
              <p className="text-on-surface">
                {formatDuration(selectedTc.duration_seconds)}
              </p>
            </div>
            <div>
              <span className="font-label text-xs text-on-surface-variant">
                Run finished
              </span>
              <p className="text-on-surface">
                {timeAgo(selectedRun.finished)}
              </p>
            </div>
            <div className="flex items-end gap-3">
              {selectedRun.prow_url && (
                <a
                  href={selectedRun.prow_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-primary transition-colors hover:text-primary-dim"
                >
                  View in Prow ↗
                </a>
              )}
              {selectedRun.build_log_url && (
                <a
                  href={selectedRun.build_log_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-primary transition-colors hover:text-primary-dim"
                >
                  Build Log ↗
                </a>
              )}
            </div>
          </div>

          {/* Failure message */}
          {selectedTc.failure_message && (
            <pre className="whitespace-pre-wrap rounded-lg bg-error/5 p-4 font-label text-xs leading-relaxed text-error">
              {selectedTc.failure_message}
            </pre>
          )}

          {/* Full stack trace */}
          {selectedTc.failure_body && (
            <details className="group/trace [&>summary]:list-none [&>summary::-webkit-details-marker]:hidden">
              <summary className="cursor-pointer font-label text-xs text-on-surface-variant transition-colors hover:text-on-surface">
                <HiChevronRight className="h-4 w-4 inline-block transition-transform duration-200 group-open/trace:rotate-90" /> Stack Trace
              </summary>
              <pre className="mt-2 whitespace-pre-wrap font-label text-xs leading-relaxed text-on-surface-variant">
                {highlightStackTrace(selectedTc.failure_body)}
              </pre>
            </details>
          )}

          {/* Source location */}
          {selectedTc.failure_location && (
            <div className="flex items-center gap-2 text-xs">
              <HiMapPin className="h-4 w-4 text-on-surface-variant" />
              {selectedTc.failure_location_url ? (
                <a
                  href={selectedTc.failure_location_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="font-mono text-primary hover:underline"
                >
                  {selectedTc.failure_location}
                </a>
              ) : (
                <span className="font-mono text-on-surface-variant">
                  {selectedTc.failure_location}
                </span>
              )}
            </div>
          )}

          {/* Cluster artifacts */}
          {selectedTc.cluster_artifacts && (
            <div className="rounded-lg border border-outline-variant bg-surface-container p-3 space-y-2">
              <p className="font-label text-sm font-semibold text-on-surface">
                Debug Artifacts — {selectedTc.cluster_artifacts.cluster_name}
              </p>

              <div className="flex flex-wrap gap-x-4 gap-y-1.5 text-xs">
                {selectedTc.cluster_artifacts.azure_activity_log && (
                  <a
                    href={selectedTc.cluster_artifacts.azure_activity_log}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 text-primary hover:underline"
                  >
                    <HiCloud className="h-3.5 w-3.5 shrink-0" /> Azure Activity Log
                  </a>
                )}
                {selectedTc.cluster_artifacts.bootstrap_resources_url && (
                  <a
                    href={selectedTc.cluster_artifacts.bootstrap_resources_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 text-primary hover:underline"
                  >
                    <HiClipboardDocumentList className="h-3.5 w-3.5 shrink-0" /> Cluster Resources
                  </a>
                )}
                {selectedTc.cluster_artifacts.pod_log_dirs && Object.entries(selectedTc.cluster_artifacts.pod_log_dirs).map(([dir, url]) => (
                  <a
                    key={dir}
                    href={url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 text-primary hover:underline"
                  >
                    <HiArchiveBox className="h-3.5 w-3.5 shrink-0" /> {dir}
                  </a>
                ))}
                {selectedRun && (
                  <a
                    href={`https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/logs/${selectedRun.job_name}/${selectedRun.build_id}/artifacts/clusters/bootstrap/logs/`}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 text-primary hover:underline"
                  >
                    <HiServerStack className="h-3.5 w-3.5 shrink-0" /> Controller Logs
                  </a>
                )}
              </div>

              {selectedTc.cluster_artifacts.machines &&
                selectedTc.cluster_artifacts.machines.length > 0 && (
                  <details className="group/machines [&>summary]:list-none [&>summary::-webkit-details-marker]:hidden">
                    <summary className="cursor-pointer font-label text-xs text-on-surface-variant transition-colors hover:text-on-surface inline-flex items-center gap-1">
                      <HiChevronRight className="h-3.5 w-3.5 shrink-0 transition-transform duration-200 group-open/machines:rotate-90" />
                      <HiServerStack className="h-3.5 w-3.5 shrink-0" /> Machine Logs (
                      {selectedTc.cluster_artifacts.machines.length} machines)
                    </summary>
                    <div className="mt-2 space-y-2">
                      {selectedTc.cluster_artifacts.machines.map((m) => (
                        <div key={m.name} className="pl-4">
                          <p className="font-mono text-xs text-on-surface-variant">
                            {m.name}
                          </p>
                          <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
                            {Object.entries(m.logs).map(([logType, url]) => (
                              <a
                                key={logType}
                                href={url}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="font-label text-[11px] text-primary hover:underline"
                              >
                                {logType}
                              </a>
                            ))}
                          </div>
                        </div>
                      ))}
                    </div>
                  </details>
                )}
            </div>
          )}

          {/* AI analysis panel */}
          {selectedTc.ai_analysis && (
            <div className="rounded-lg border border-primary/30 bg-primary/5 p-3 sm:p-5 space-y-4">
              <div className="flex items-center gap-2">
                <HiSparkles className="h-5 w-5 text-primary" />
                <span className="font-label text-sm font-semibold text-primary">
                  AI Analysis
                </span>
                <span className={`rounded-full px-2.5 py-0.5 text-xs font-medium ${
                  selectedTc.ai_analysis.severity === "Critical" || selectedTc.ai_analysis.severity === "High"
                    ? "bg-error/20 text-error"
                    : selectedTc.ai_analysis.severity === "Medium"
                      ? "bg-tertiary/20 text-tertiary"
                      : "bg-on-surface-variant/20 text-on-surface-variant"
                }`}>
                  Severity: {selectedTc.ai_analysis.severity}
                </span>
              </div>
              <div>
                <p className="font-label text-xs font-semibold text-on-surface-variant mb-1">Root Cause</p>
                <p className="text-sm text-on-surface leading-relaxed whitespace-pre-line">
                  {formatSteps(selectedTc.ai_analysis.root_cause)}
                </p>
              </div>
              <div>
                <p className="font-label text-xs font-semibold text-on-surface-variant mb-1">Suggested Fix</p>
                <p className="text-sm text-on-surface leading-relaxed whitespace-pre-line">
                  {formatSteps(selectedTc.ai_analysis.suggested_fix)}
                </p>
              </div>
              {selectedTc.ai_analysis.relevant_files && selectedTc.ai_analysis.relevant_files.length > 0 && (
                <div>
                  <p className="font-label text-xs font-semibold text-on-surface-variant mb-1">Files to Check</p>
                  <ul className="list-disc list-inside text-sm text-on-surface space-y-0.5">
                    {[...selectedTc.ai_analysis.relevant_files]
                      .sort((a, b) => fileSortKey(a, { buildLogUrl: selectedRun?.build_log_url, clusterArtifacts: selectedTc.cluster_artifacts }) - fileSortKey(b, { buildLogUrl: selectedRun?.build_log_url, clusterArtifacts: selectedTc.cluster_artifacts }))
                      .map((f, i) => {
                      const url = fileToUrl(f, { buildLogUrl: selectedRun?.build_log_url, clusterArtifacts: selectedTc.cluster_artifacts });
                      return (
                        <li key={i} className="font-mono text-xs">
                          {url ? (
                            <a href={url} target="_blank" rel="noopener noreferrer" className="text-primary hover:underline">{f}</a>
                          ) : (
                            <span className="text-on-surface-variant">{f}</span>
                          )}
                        </li>
                      );
                    })}
                  </ul>
                </div>
              )}
            </div>
          )}

          {/* AI summary (if no deep analysis) */}
          {selectedTc.ai_summary && !selectedTc.ai_analysis && (
            <div className="flex items-start gap-2 rounded-lg bg-surface-container p-3">
              <HiSparkles className="h-4 w-4 shrink-0 text-primary" />
              <span className={`text-xs ${selectedTc.ai_summary.is_transient ? "text-on-surface-variant" : "text-tertiary"}`}>
                {selectedTc.ai_summary.summary}
              </span>
            </div>
          )}
        </section>
      )}

      {/* When a run is selected but the test wasn't present */}
      {selectedRun && !selectedTc && (
        <section className="glass rounded-xl p-8 text-center">
          <p className="text-on-surface-variant">
            This test was not present in build #{selectedRun.build_id}.
          </p>
        </section>
      )}

      {/* Duration trend chart */}
      {(() => {
        const durationHistory = occurrences
          .filter((o) => o.testCase)
          .map((o) => ({
            build_id: o.run.build_id,
            timestamp: o.run.started,
            duration: o.testCase!.duration_seconds,
            passed: o.testCase!.status === "passed",
          }));
        return durationHistory.length > 0 ? (
          <section>
            <h2 className="font-headline mb-3 text-lg font-semibold text-on-surface">
              Duration Trend
            </h2>
            <div className="glass rounded-xl p-4">
              <DurationChart history={durationHistory} />
            </div>
          </section>
        ) : null;
      })()}
    </div>
  );
}
