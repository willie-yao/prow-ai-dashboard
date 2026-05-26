import { useMemo, useState } from "react";
import { useDashboard } from "../hooks/useData";
import {
  timeAgo,
  groupByCategory,
  categoryLabels,
} from "../lib/utils";
import type { JobSummary } from "../types/dashboard";
import { SummaryBar } from "../components/SummaryBar";
import { NeedsAttention } from "../components/NeedsAttention";
import { JobCard } from "../components/JobCard";

type StatusFilter = "ALL" | "PASSING" | "FLAKY" | "FAILING";

const statusFilters: { label: string; value: StatusFilter }[] = [
  { label: "All", value: "ALL" },
  { label: "Passing", value: "PASSING" },
  { label: "Flaky", value: "FLAKY" },
  { label: "Failing", value: "FAILING" },
];

export function DashboardPage() {
  const { data, loading, error } = useDashboard();
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("ALL");
  const [branchFilter, setBranchFilter] = useState("ALL");

  const branches = useMemo(() => {
    if (!data) return [];
    const set = new Set(data.jobs.map((j) => j.branch).filter(Boolean));
    return Array.from(set).sort((a, b) => {
      // "main" always first
      if (a === "main") return -1;
      if (b === "main") return 1;
      // Release branches: sort descending (newest first)
      // Extract version numbers for proper numeric comparison
      const aMatch = a.match(/(\d+)\.(\d+)/);
      const bMatch = b.match(/(\d+)\.(\d+)/);
      if (aMatch && bMatch) {
        const aMajor = Number(aMatch[1]), aMinor = Number(aMatch[2]);
        const bMajor = Number(bMatch[1]), bMinor = Number(bMatch[2]);
        if (aMajor !== bMajor) return bMajor - aMajor;
        return bMinor - aMinor;
      }
      return a.localeCompare(b);
    });
  }, [data]);

  const filtered = useMemo(() => {
    if (!data) return [];
    return data.jobs.filter((j: JobSummary) => {
      if (statusFilter !== "ALL" && j.overall_status !== statusFilter)
        return false;
      if (branchFilter !== "ALL" && j.branch !== branchFilter) return false;
      return true;
    });
  }, [data, statusFilter, branchFilter]);

  const grouped = useMemo(() => groupByCategory(filtered), [filtered]);

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
        <p className="text-error text-lg">Failed to load dashboard</p>
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

  const categoryOrder = Object.keys(categoryLabels);
  const sortedCategories = Object.keys(grouped).sort((a, b) => {
    const ai = categoryOrder.indexOf(a);
    const bi = categoryOrder.indexOf(b);
    return (ai === -1 ? 999 : ai) - (bi === -1 ? 999 : bi);
  });

  return (
    <div className="space-y-8">
      {/* Header */}
      <div>
        <h1 className="font-headline text-3xl font-bold text-on-surface">
          Test Health Overview
        </h1>
        <p className="mt-1 text-sm text-on-surface-variant">
          Last updated: {timeAgo(data.generated_at)}
        </p>
      </div>

      {/* Needs attention */}
      <NeedsAttention />

      {/* Summary bar */}
      <SummaryBar jobs={data.jobs} onFilterClick={(s) => setStatusFilter(s as StatusFilter)} activeFilter={statusFilter} />

      {/* Filters */}
      <div className="flex flex-wrap gap-6">
        {/* Status filter */}
        <div className="flex items-center gap-2">
          <span className="font-label text-xs tracking-wide text-on-surface-variant">
            Status
          </span>
          <div className="flex gap-1">
            {statusFilters.map((f) => (
              <button
                key={f.value}
                onClick={() => setStatusFilter(f.value)}
                className={`rounded-full px-3 py-1 text-xs font-medium transition-colors ${
                  statusFilter === f.value
                    ? "bg-primary text-on-primary"
                    : "bg-surface-container text-on-surface-variant hover:bg-surface-container-high"
                }`}
              >
                {f.label}
              </button>
            ))}
          </div>
        </div>

        {/* Branch filter */}
        <div className="flex items-center gap-2">
          <span className="font-label text-xs tracking-wide text-on-surface-variant">
            Branch
          </span>
          <div className="flex gap-1">
            <button
              onClick={() => setBranchFilter("ALL")}
              className={`rounded-full px-3 py-1 text-xs font-medium transition-colors ${
                branchFilter === "ALL"
                  ? "bg-primary text-on-primary"
                  : "bg-surface-container text-on-surface-variant hover:bg-surface-container-high"
              }`}
            >
              All
            </button>
            {branches.map((b) => (
              <button
                key={b}
                onClick={() => setBranchFilter(b)}
                className={`rounded-full px-3 py-1 text-xs font-medium transition-colors ${
                  branchFilter === b
                    ? "bg-primary text-on-primary"
                    : "bg-surface-container text-on-surface-variant hover:bg-surface-container-high"
                }`}
              >
                {b}
              </button>
            ))}
          </div>
        </div>
      </div>

      {/* Job grid grouped by category */}
      {sortedCategories.length === 0 ? (
        <div className="py-16 text-center">
          <p className="text-on-surface-variant">No jobs match filters</p>
        </div>
      ) : (
        sortedCategories.map((category) => (
          <section key={category}>
            <h2 className="font-headline mb-4 text-xl font-semibold text-on-surface">
              {categoryLabels[category] ??
                category.charAt(0).toUpperCase() + category.slice(1)}
            </h2>
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
              {grouped[category].map((job) => (
                <JobCard key={job.name} job={job} />
              ))}
            </div>
          </section>
        ))
      )}
    </div>
  );
}
