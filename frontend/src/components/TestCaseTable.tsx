import React, { useState } from "react";
import { Link } from "react-router-dom";
import type { TestCase } from "../types/dashboard";
import { formatDuration, fileToUrl, fileSortKey, formatSteps } from "../lib/utils";

interface TestCaseTableProps {
  testCases: TestCase[];
  jobName?: string;
  buildId?: string;
  buildLogUrl?: string;
}

const statusOrder: Record<string, number> = {
  failed: 0,
  passed: 1,
  skipped: 2,
};

import {
  HiCheckCircle,
  HiXCircle,
  HiMinusCircle,
  HiSparkles,
  HiClipboardDocumentList,
  HiArchiveBox,
  HiCloud,
  HiServerStack,
  HiMapPin,
  HiChevronRight,
} from "react-icons/hi2";

function statusIcon(status: string) {
  switch (status) {
    case "passed":
      return <HiCheckCircle className="text-secondary text-lg" />;
    case "failed":
      return <HiXCircle className="text-error text-lg" />;
    default:
      return <HiMinusCircle className="text-on-surface-variant text-lg" />;
  }
}

// Hide Ginkgo setup/teardown entries unless they failed.
const setupPatterns = /synchronizedbeforesuite|synchronizedaftersuite|beforesuite|aftersuite/i;

// Highlight Go file:line references in stack traces
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

export function TestCaseTable({ testCases, jobName, buildId, buildLogUrl }: TestCaseTableProps) {
  const [expandedRows, setExpandedRows] = useState<Set<number>>(new Set());

  const filtered = testCases.filter(
    (tc) => tc.status !== "skipped" && (tc.status === "failed" || !setupPatterns.test(tc.name))
  );

  const sorted = [...filtered].sort(
    (a, b) => (statusOrder[a.status] ?? 3) - (statusOrder[b.status] ?? 3)
  );

  function toggleRow(idx: number) {
    setExpandedRows((prev) => {
      const next = new Set(prev);
      if (next.has(idx)) next.delete(idx);
      else next.add(idx);
      return next;
    });
  }

  return (
    <div className="overflow-x-auto rounded-xl border border-outline-variant">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-outline-variant bg-surface-container">
            <th className="w-10 px-3 py-2.5 text-left font-label text-xs uppercase tracking-wider text-on-surface-variant">
              &nbsp;
            </th>
            <th className="px-3 py-2.5 text-left font-label text-xs uppercase tracking-wider text-on-surface-variant">
              Test Name
            </th>
            <th className="w-24 px-3 py-2.5 text-right font-label text-xs uppercase tracking-wider text-on-surface-variant hidden sm:table-cell">
              Duration
            </th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((tc, idx) => {
            const isExpanded = expandedRows.has(idx);
            const hasFail = tc.status === "failed" && tc.failure_message;
            const stripe =
              idx % 2 === 0
                ? "bg-surface-container"
                : "bg-surface-container-high";

            return (
              <tr key={idx} className="group">
                <td colSpan={3} className="p-0">
                  <div
                    role={hasFail ? "button" : undefined}
                    tabIndex={hasFail ? 0 : undefined}
                    onClick={() => hasFail && toggleRow(idx)}
                    onKeyDown={(e) => {
                      if (hasFail && (e.key === "Enter" || e.key === " ")) {
                        e.preventDefault();
                        toggleRow(idx);
                      }
                    }}
                    className={`flex items-center ${stripe} ${hasFail ? "cursor-pointer hover:brightness-110" : ""}`}
                  >
                    <span className="w-10 shrink-0 px-2 sm:px-3 py-2">
                      {statusIcon(tc.status)}
                    </span>
                    <span className="min-w-0 flex-1 break-words px-2 sm:px-3 py-2 text-on-surface">
                      {jobName && tc.status === "failed" ? (
                        <Link
                          to={`/job/${encodeURIComponent(jobName)}/test/${encodeURIComponent(tc.name)}${buildId ? `?run=${buildId}` : ""}`}
                          className="hover:text-primary transition-colors"
                          onClick={(e) => e.stopPropagation()}
                        >
                          {tc.name}
                        </Link>
                      ) : (
                        tc.name
                      )}
                      {tc.failure_location_url && (
                        <a
                          href={tc.failure_location_url}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="ml-2 inline-flex text-primary hover:text-primary-container"
                          onClick={(e) => e.stopPropagation()}
                          title="View source on GitHub"
                        >
                          <svg
                            className="h-3.5 w-3.5"
                            viewBox="0 0 24 24"
                            fill="none"
                            stroke="currentColor"
                            strokeWidth="2"
                            strokeLinecap="round"
                            strokeLinejoin="round"
                          >
                            <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
                            <polyline points="15 3 21 3 21 9" />
                            <line x1="10" y1="14" x2="21" y2="3" />
                          </svg>
                        </a>
                      )}
                    </span>
                    <span className="w-24 shrink-0 px-2 sm:px-3 py-2 text-right font-label text-xs text-on-surface-variant hidden sm:block">
                      {formatDuration(tc.duration_seconds)}
                    </span>
                  </div>

                  {/* AI summary — shown inline for failed tests without expanding */}
                  {tc.ai_summary && (
                    <div className={`flex items-start gap-2 pl-10 sm:pl-16 pr-4 sm:pr-6 py-1.5 ${stripe}`}>
                      <HiSparkles className="h-4 w-4 shrink-0 text-primary" />
                      <span className={`text-xs ${tc.ai_summary.is_transient ? "text-on-surface-variant" : "text-tertiary"}`}>
                        {tc.ai_summary.summary}
                        {tc.ai_summary.is_transient && (
                          <span className="ml-1 text-on-surface-variant/60">· Likely transient</span>
                        )}
                      </span>
                    </div>
                  )}

                  {hasFail && isExpanded && (
                    <div className="border-t border-outline-variant bg-error/5 px-6 py-4 space-y-3">
                      {/* Failure message */}
                      <pre className="whitespace-pre-wrap font-label text-xs leading-relaxed text-error">
                        {tc.failure_message}
                      </pre>

                      {/* Full stack trace */}
                      {tc.failure_body && (
                        <details className="group/trace [&>summary]:list-none [&>summary::-webkit-details-marker]:hidden">
                          <summary className="cursor-pointer font-label text-xs text-on-surface-variant hover:text-on-surface transition-colors">
                            <HiChevronRight className="h-4 w-4 inline-block transition-transform duration-200 group-open/trace:rotate-90" /> Stack Trace
                          </summary>
                          <pre className="mt-2 whitespace-pre-wrap font-label text-xs leading-relaxed text-on-surface-variant">
                            {highlightStackTrace(tc.failure_body)}
                          </pre>
                        </details>
                      )}

                      {/* Source location link */}
                      {tc.failure_location && (
                        <div className="flex items-center gap-2 text-xs">
                          <HiMapPin className="h-4 w-4 text-on-surface-variant" />
                          {tc.failure_location_url ? (
                            <a
                              href={tc.failure_location_url}
                              target="_blank"
                              rel="noopener noreferrer"
                              className="font-mono text-primary hover:underline"
                            >
                              {tc.failure_location}
                            </a>
                          ) : (
                            <span className="font-mono text-on-surface-variant">
                              {tc.failure_location}
                            </span>
                          )}
                        </div>
                      )}

                      {/* Cluster artifact links */}
                      {tc.cluster_artifacts && (
                        <div className="rounded-lg border border-outline-variant bg-surface-container p-3 space-y-2">
                          <p className="font-label text-sm font-semibold text-on-surface">
                            Debug Artifacts — {tc.cluster_artifacts.cluster_name}
                          </p>

                          <div className="flex flex-wrap gap-x-4 gap-y-1.5 text-xs">
                            {tc.cluster_artifacts.azure_activity_log && (
                              <a
                                href={tc.cluster_artifacts.azure_activity_log}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="inline-flex items-center gap-1 text-primary hover:underline"
                              >
                                <HiCloud className="h-3.5 w-3.5 shrink-0" /> Azure Activity Log
                              </a>
                            )}
                            {tc.cluster_artifacts.bootstrap_resources_url && (
                              <a
                                href={tc.cluster_artifacts.bootstrap_resources_url}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="inline-flex items-center gap-1 text-primary hover:underline"
                              >
                                <HiClipboardDocumentList className="h-3.5 w-3.5 shrink-0" /> Cluster Resources
                              </a>
                            )}
                            {tc.cluster_artifacts.pod_log_dirs && Object.entries(tc.cluster_artifacts.pod_log_dirs).map(([dir, url]) => (
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
                            {jobName && buildId && (
                              <a
                                href={`https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/logs/${jobName}/${buildId}/artifacts/clusters/bootstrap/logs/`}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="inline-flex items-center gap-1 text-primary hover:underline"
                              >
                                <HiServerStack className="h-3.5 w-3.5 shrink-0" /> Controller Logs
                              </a>
                            )}
                          </div>

                          {tc.cluster_artifacts.machines && tc.cluster_artifacts.machines.length > 0 && (
                            <details className="group/machines [&>summary]:list-none [&>summary::-webkit-details-marker]:hidden">
                              <summary className="cursor-pointer font-label text-xs text-on-surface-variant hover:text-on-surface transition-colors inline-flex items-center gap-1">
                                <HiChevronRight className="h-3.5 w-3.5 shrink-0 transition-transform duration-200 group-open/machines:rotate-90" />
                                <HiServerStack className="h-3.5 w-3.5 shrink-0" /> Machine Logs ({tc.cluster_artifacts.machines.length} machines)
                              </summary>
                              <div className="mt-2 space-y-2">
                                {tc.cluster_artifacts.machines.map((m) => (
                                  <div key={m.name} className="pl-4">
                                    <p className="font-mono text-xs text-on-surface-variant">{m.name}</p>
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

                      {/* AI deep analysis panel */}
                      {tc.ai_analysis && (
                        <div className="rounded-lg border border-primary/30 bg-primary/5 p-5 space-y-4">
                          <div className="flex items-center gap-2">
                            <HiSparkles className="h-5 w-5 text-primary" />
                            <span className="font-label text-sm font-semibold text-primary">
                              AI Analysis
                            </span>
                            <span className={`rounded-full px-2.5 py-0.5 text-xs font-medium ${
                              tc.ai_analysis.severity === "Critical" || tc.ai_analysis.severity === "High"
                                ? "bg-error/20 text-error"
                                : tc.ai_analysis.severity === "Medium"
                                  ? "bg-tertiary/20 text-tertiary"
                                  : "bg-on-surface-variant/20 text-on-surface-variant"
                            }`}>
                              Severity: {tc.ai_analysis.severity}
                            </span>
                          </div>
                          <div>
                            <p className="font-label text-xs font-semibold text-on-surface-variant mb-1">Root Cause</p>
                            <p className="text-sm text-on-surface leading-relaxed whitespace-pre-line">
                              {formatSteps(tc.ai_analysis.root_cause)}
                            </p>
                          </div>
                          <div>
                            <p className="font-label text-xs font-semibold text-on-surface-variant mb-1">Suggested Fix</p>
                            <p className="text-sm text-on-surface leading-relaxed whitespace-pre-line">
                              {formatSteps(tc.ai_analysis.suggested_fix)}
                            </p>
                          </div>
                          {tc.ai_analysis.relevant_files && tc.ai_analysis.relevant_files.length > 0 && (
                            <div>
                              <p className="font-label text-xs font-semibold text-on-surface-variant mb-1">Files to Check</p>
                              <ul className="list-disc list-inside text-sm text-on-surface space-y-0.5">
                                {[...tc.ai_analysis.relevant_files]
                                  .sort((a, b) => fileSortKey(a, { buildLogUrl, clusterArtifacts: tc.cluster_artifacts }) - fileSortKey(b, { buildLogUrl, clusterArtifacts: tc.cluster_artifacts }))
                                  .map((f, i) => {
                                  const url = fileToUrl(f, { buildLogUrl, clusterArtifacts: tc.cluster_artifacts });
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
                    </div>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
