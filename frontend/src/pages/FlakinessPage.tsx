import { useState } from "react";
import { Link } from "react-router-dom";
import { useFlakinessReport } from "../hooks/useData";
import { formatPercent, timeAgo } from "../lib/utils";
import { DurationChart } from "../components/DurationChart";
import type { TestFlakiness } from "../types/dashboard";
import { HiFaceSmile, HiChevronRight } from "react-icons/hi2";

type Tab = "most_flaky" | "persistent" | "recently_broken";

const tabs: { label: string; value: Tab; tooltip: string }[] = [
  { label: "Most Flaky", value: "most_flaky", tooltip: "Tests that alternate between passing and failing. Sorted by flip rate — the percentage of runs where the result changed from the previous run." },
  { label: "Persistent Failures", value: "persistent", tooltip: "Tests that have failed 3 or more times in a row with the same error. These are consistently broken, not flaky." },
  { label: "Recently Broken", value: "recently_broken", tooltip: "Tests that started a new failure streak within the last 48 hours. These are likely new regressions." },
];

const JOB_PREFIX = "periodic-cluster-api-provider-azure-";

function shortJobName(name: string): string {
  return name.startsWith(JOB_PREFIX) ? name.slice(JOB_PREFIX.length) : name;
}

function classificationStyle(c: TestFlakiness["classification"]): string {
  switch (c) {
    case "persistent":
      return "bg-error/20 text-error";
    case "flaky":
      return "bg-tertiary/20 text-tertiary";
    case "one-off":
      return "bg-on-surface-variant/20 text-on-surface-variant";
  }
}

function classificationLabel(c: TestFlakiness["classification"]): string {
  return c.charAt(0).toUpperCase() + c.slice(1);
}

function metricValue(tab: Tab, item: TestFlakiness): string {
  switch (tab) {
    case "most_flaky":
      return formatPercent(item.flip_rate);
    case "persistent":
      return `${item.consecutive_failures}×`;
    case "recently_broken":
      return item.first_failed_at ? timeAgo(item.first_failed_at) : "—";
  }
}

function metricLabel(tab: Tab): string {
  switch (tab) {
    case "most_flaky":
      return "Flip Rate";
    case "persistent":
      return "Consecutive";
    case "recently_broken":
      return "Since";
  }
}

function TestRow({ item, tab }: { item: TestFlakiness; tab: Tab }) {
  const [expanded, setExpanded] = useState(false);
  const failPct = Math.round(item.fail_rate * 100);

  return (
    <div className="glass rounded-xl">
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        className="w-full px-3 sm:px-4 py-3 text-left"
      >
        <div className="flex flex-wrap sm:flex-nowrap items-center gap-3 sm:gap-4">
          {/* Test Name */}
          <div className="min-w-0 flex-1">
            <Link
              to={`/job/${encodeURIComponent(item.job_name)}/test/${encodeURIComponent(item.test_name)}${item.last_failure?.build_id ? `?run=${item.last_failure.build_id}` : ""}`}
              onClick={(e) => e.stopPropagation()}
              className="block truncate text-sm font-medium text-on-surface hover:text-primary transition-colors"
              title={item.test_name}
            >
              {item.test_name}
            </Link>
            <Link
              to={`/job/${encodeURIComponent(item.job_name)}`}
              onClick={(e) => e.stopPropagation()}
              className="font-label text-xs text-on-surface-variant hover:text-primary transition-colors"
            >
              {shortJobName(item.job_name)}
            </Link>
          </div>

          {/* Metric */}
          <div className="shrink-0 text-right w-16">
            <p className="font-label text-xs text-on-surface-variant">
              {metricLabel(tab)}
            </p>
            <p className="text-sm font-semibold text-on-surface">
              {metricValue(tab, item)}
            </p>
          </div>

          {/* Fail Rate Bar */}
          <div className="shrink-0 w-24">
            <p className="font-label text-xs text-on-surface-variant mb-1">
              Fail {failPct}%
            </p>
            <div className="h-2 w-full rounded-full bg-on-surface-variant/20">
              <div
                className="h-2 rounded-full bg-error"
                style={{ width: `${failPct}%` }}
              />
            </div>
          </div>

          {/* Classification badge */}
          <span
            className={`shrink-0 rounded-full px-2.5 py-0.5 text-xs font-medium ${classificationStyle(item.classification)}`}
          >
            {classificationLabel(item.classification)}
          </span>

          {/* Expand indicator */}
          <span
            className={`shrink-0 text-on-surface-variant transition-transform duration-200 ${expanded ? "rotate-90" : ""}`}
          >
            <HiChevronRight className="h-4 w-4" />
          </span>
        </div>

        {/* Last Error (truncated) */}
        {item.last_failure?.failure_message && (
          <p className="mt-2 truncate text-xs text-on-surface-variant">
            {item.last_failure.failure_message}
          </p>
        )}
      </button>

      {/* Expanded content */}
      {expanded && (
        <div className="border-t border-outline-variant px-4 py-4 space-y-4">
          {/* Last error full message */}
          {item.last_failure?.failure_message && (
            <div>
              <p className="font-label text-xs text-on-surface-variant mb-1">
                Last Error
              </p>
              <pre className="whitespace-pre-wrap rounded-lg bg-error/5 p-3 font-label text-xs leading-relaxed text-error">
                {item.last_failure.failure_message}
              </pre>
            </div>
          )}

          {/* Error patterns */}
          {item.error_patterns && item.error_patterns.length > 0 && (
            <div>
              <p className="font-label text-xs text-on-surface-variant mb-2">
                Error Patterns
              </p>
              <div className="space-y-2">
                {item.error_patterns.map((pat, i) => (
                  <div key={i} className="flex items-start gap-3 text-sm">
                    <span className="shrink-0 rounded-full bg-error/20 px-2 py-0.5 font-label text-xs font-medium text-error">
                      {pat.count}×
                    </span>
                    <div className="min-w-0">
                      <p className="truncate text-xs text-on-surface-variant" title={pat.normalized_message}>
                        {pat.normalized_message}
                      </p>
                      <p className="mt-0.5 truncate text-xs text-on-surface-variant/60" title={pat.example_message}>
                        e.g. {pat.example_message}
                      </p>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Duration chart */}
          {item.duration_history && item.duration_history.length > 0 && (
            <DurationChart history={item.duration_history} />
          )}
        </div>
      )}
    </div>
  );
}

export function FlakinessPage() {
  const { data, loading, error } = useFlakinessReport();
  const [activeTab, setActiveTab] = useState<Tab>("most_flaky");

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
        <p className="text-error text-lg">Failed to load test analysis</p>
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

  const listMap: Record<Tab, TestFlakiness[]> = {
    most_flaky: data.most_flaky,
    persistent: data.persistent_failures,
    recently_broken: data.recently_broken,
  };

  const items = listMap[activeTab] ?? [];

  return (
    <div className="space-y-8">
      {/* Header */}
      <div>
        <h1 className="font-headline text-3xl font-bold text-on-surface">
          Test Analysis
        </h1>
        <p className="mt-1 text-sm text-on-surface-variant">
          Last updated: {timeAgo(data.generated_at)}
        </p>
      </div>

      {/* Tabs */}
      <div className="flex flex-wrap gap-1">
        {tabs.map((t) => (
          <button
            key={t.value}
            onClick={() => setActiveTab(t.value)}
            title={t.tooltip}
            className={`rounded-full px-3 py-1 text-xs font-medium transition-colors ${
              activeTab === t.value
                ? "bg-primary text-on-primary"
                : "bg-surface-container text-on-surface-variant hover:bg-surface-container-high"
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {/* Tab description */}
      <p className="text-sm text-on-surface-variant -mt-4">
        {tabs.find((t) => t.value === activeTab)?.tooltip}
      </p>

      {/* Content */}
      {items.length === 0 ? (
        <div className="glass rounded-xl py-16 text-center">
          <p className="text-on-surface-variant text-lg flex items-center justify-center gap-2">
            No tests match this category <HiFaceSmile className="h-5 w-5" />
          </p>
        </div>
      ) : (
        <div className="space-y-3">
          {items.map((item) => (
            <TestRow
              key={`${item.job_name}/${item.test_name}`}
              item={item}
              tab={activeTab}
            />
          ))}
        </div>
      )}
    </div>
  );
}
