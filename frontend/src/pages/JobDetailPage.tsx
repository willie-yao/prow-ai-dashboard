import { useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useJobDetail } from "../hooks/useData";
import {
  formatDuration,
  formatPercent,
  timeAgo,
  statusBg,
} from "../lib/utils";
import type { BuildResult, TestCase } from "../types/dashboard";
import { RunTimeline } from "../components/RunTimeline";
import { TestResultsGrid } from "../components/TestResultsGrid";
import { TestCaseTable } from "../components/TestCaseTable";
import { HiChevronRight } from "react-icons/hi2";

export function JobDetailPage() {
  const { jobName } = useParams<{ jobName: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const [gridOpen, setGridOpen] = useState(false);
  const { data, loading, error } = useJobDetail(jobName);

  const runs = data?.runs ?? [];

  const selectedBuildId =
    searchParams.get("run") ?? runs[0]?.build_id ?? undefined;

  const selectedRun: BuildResult | undefined = useMemo(() => {
    if (!selectedBuildId) return undefined;
    return runs.find((r) => r.build_id === selectedBuildId);
  }, [runs, selectedBuildId]);

  const testCases: TestCase[] = selectedRun?.test_cases ?? [];

  const passRate7d = useMemo(() => {
    if (runs.length === 0) return null;
    const cutoff = Date.now() - 7 * 24 * 60 * 60 * 1000;
    const recent = runs.filter(
      (r) => new Date(r.started).getTime() >= cutoff
    );
    if (recent.length === 0) return null;
    return recent.filter((r) => r.passed).length / recent.length;
  }, [runs]);

  function handleSelectRun(buildId: string) {
    setSearchParams({ run: buildId });
  }

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

  const lastRun = runs[0] ?? null;

  return (
    <div className="space-y-6 sm:space-y-8">
      {/* Breadcrumb */}
      <nav className="font-label flex items-center gap-2 text-sm text-on-surface-variant">
        <Link to="/" className="transition-colors hover:text-primary">
          Dashboard
        </Link>
        <span>›</span>
        <span className="text-on-surface">{jobName}</span>
      </nav>

      {/* Job header */}
      <div>
        <h1 className="font-headline text-xl sm:text-2xl font-bold text-on-surface">
          {jobName}
        </h1>
        <div className="mt-3 flex flex-wrap items-center gap-3">
          {passRate7d !== null && (
            <span
              className={`rounded-full px-2.5 py-0.5 text-xs font-medium ${
                passRate7d >= 0.9
                  ? "bg-secondary/20 text-secondary"
                  : passRate7d >= 0.7
                    ? "bg-tertiary/20 text-tertiary"
                    : "bg-error/20 text-error"
              }`}
            >
              {formatPercent(passRate7d)} pass rate (7d)
            </span>
          )}
          <span className="text-sm text-on-surface-variant">
            {runs.length} total run{runs.length !== 1 && "s"}
          </span>
          {lastRun && (
            <span className="text-sm text-on-surface-variant">
              Last run {timeAgo(lastRun.started)}
            </span>
          )}
        </div>
      </div>

      {/* Run timeline */}
      {runs.length === 0 ? (
        <div className="glass rounded-xl p-8 text-center">
          <p className="text-on-surface-variant">No runs found</p>
        </div>
      ) : (
        <>
          <section>
            <h2 className="font-headline mb-3 text-lg font-semibold text-on-surface">
              Run History
            </h2>
            <RunTimeline
              runs={runs}
              selectedBuildId={selectedBuildId}
              onSelect={handleSelectRun}
            />
          </section>

          {/* Test results grid — collapsible */}
          <section>
            <button
              type="button"
              onClick={() => setGridOpen(!gridOpen)}
              className="flex items-center gap-2 font-headline text-lg font-semibold text-on-surface hover:text-primary transition-colors"
            >
              <span className={`inline-block transition-transform duration-200 ${gridOpen ? "rotate-90" : ""}`}><HiChevronRight className="h-5 w-5" /></span>
              Test Results Grid
            </button>
            <div
              className={`grid transition-[grid-template-rows] duration-300 ease-in-out ${gridOpen ? "grid-rows-[1fr]" : "grid-rows-[0fr]"}`}
            >
              <div className="overflow-hidden">
                <div className="pt-3">
                  <TestResultsGrid runs={runs} jobName={jobName!} />
                </div>
              </div>
            </div>
          </section>

          {/* Selected run details */}
          {selectedRun && (
            <section className="glass rounded-xl p-4 sm:p-6">
              <div className="mb-4 flex items-center gap-3">
                <h3 className="font-headline text-base font-semibold text-on-surface">
                  Run Details
                </h3>
                {selectedRun.result === "PENDING" ? (
                  <span className="rounded-full bg-primary/20 px-2.5 py-0.5 font-label text-xs font-medium text-primary">
                    In Progress
                  </span>
                ) : (
                  <span
                    className={`inline-block h-2.5 w-2.5 rounded-full ${statusBg(selectedRun.passed ? "PASSING" : "FAILING")}`}
                  />
                )}
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
                    Finished
                  </span>
                  <p className="text-on-surface">
                    {selectedRun.result === "PENDING"
                      ? "Still running…"
                      : new Date(selectedRun.finished).toLocaleString()}
                  </p>
                </div>
                <div>
                  <span className="font-label text-xs text-on-surface-variant">
                    Duration
                  </span>
                  <p className="text-on-surface">
                    {selectedRun.result === "PENDING"
                      ? "—"
                      : formatDuration(selectedRun.duration_seconds)}
                  </p>
                </div>
                <div>
                  <span className="font-label text-xs text-on-surface-variant">
                    Commit
                  </span>
                  <p className="font-mono text-on-surface">
                    {selectedRun.commit
                      ? selectedRun.commit.slice(0, 8)
                      : "—"}
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
            </section>
          )}

          {/* Test cases */}
          {selectedRun && testCases.length > 0 && (
            <section>
              <h2 className="font-headline mb-3 text-lg font-semibold text-on-surface">
                Test Cases
              </h2>
              <TestCaseTable testCases={testCases} jobName={jobName} buildId={selectedRun?.build_id} buildLogUrl={selectedRun?.build_log_url} />
            </section>
          )}

          {selectedRun && testCases.length === 0 && (
            <section className="glass rounded-xl p-8 text-center">
              <p className="text-on-surface-variant">
                {selectedRun.result === "PENDING"
                  ? "⏳ This build is still running — test results will appear when it completes."
                  : "No test cases available for this run."}
              </p>
            </section>
          )}
        </>
      )}
    </div>
  );
}
