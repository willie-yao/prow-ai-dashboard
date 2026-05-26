import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useFlakinessReport } from "../hooks/useData";
import { shortTestName } from "../lib/utils";
import type { TestFlakiness } from "../types/dashboard";

const JOB_PREFIX = "periodic-cluster-api-provider-azure-";
const MAX_ITEMS = 10;

function shortJobName(name: string): string {
  return name.startsWith(JOB_PREFIX) ? name.slice(JOB_PREFIX.length) : name;
}

interface ItemGroup {
  label: string;
  items: TestFlakiness[];
}

export function NeedsAttention() {
  const { data, loading } = useFlakinessReport();

  const groups = useMemo<ItemGroup[]>(() => {
    if (!data) return [];

    const broken = data.recently_broken ?? [];
    const persistent = data.persistent_failures ?? [];
    const flaky = data.most_flaky ?? [];

    const hasPrimary = broken.length > 0 || persistent.length > 0;

    if (hasPrimary) {
      let remaining = MAX_ITEMS;
      const result: ItemGroup[] = [];

      if (broken.length > 0) {
        const slice = broken.slice(0, remaining);
        result.push({ label: "New Regressions", items: slice });
        remaining -= slice.length;
      }

      if (persistent.length > 0 && remaining > 0) {
        result.push({ label: "Persistent Failures", items: persistent.slice(0, remaining) });
      }

      return result;
    }

    if (flaky.length > 0) {
      return [{ label: "Flaky Tests", items: flaky.slice(0, MAX_ITEMS) }];
    }

    return [];
  }, [data]);

  if (loading || groups.length === 0) return null;

  const totalItems = groups.reduce((sum, g) => sum + g.items.length, 0);

  return (
    <div className="glass rounded-xl border border-outline-variant">
      <div className="p-4 sm:p-5">
        <h2 className="font-headline text-xl font-bold text-on-surface">
          ⚠️ Needs Attention{" "}
          <span className="text-on-surface-variant text-sm font-normal">
            ({totalItems})
          </span>
        </h2>
      </div>

      <div className="max-h-[60vh] overflow-y-auto px-4 pb-4 sm:px-5 sm:pb-5">
        {groups.map((group, gi) => (
          <div key={group.label}>
            {gi > 0 && <div className="border-t border-outline-variant" />}
            <p className="font-label text-xs uppercase tracking-wider text-on-surface-variant py-2">
              {group.label}
            </p>

            <div className="space-y-0.5">
              {group.items.map((item) => (
                <Link
                  key={`${item.job_name}/${item.test_name}`}
                  to={`/job/${encodeURIComponent(item.job_name)}/test/${encodeURIComponent(item.test_name)}${item.last_failure?.build_id ? `?run=${item.last_failure.build_id}` : ""}`}
                  className="flex items-center gap-3 rounded-lg px-2 py-2 transition-colors hover:bg-surface-container-high"
                >
                  {/* Status dot */}
                  <span
                    className={`h-2 w-2 shrink-0 rounded-full ${
                      item.classification === "flaky"
                        ? "bg-tertiary"
                        : "bg-error"
                    }`}
                  />

                  {/* Name block */}
                  <div className="min-w-0 flex-1">
                    <p className="text-xs text-on-surface-variant">
                      {shortJobName(item.job_name)}
                    </p>
                    <p className="truncate text-sm text-on-surface">
                      {shortTestName(item.test_name)}
                    </p>
                  </div>

                  {/* Right side: failure count + message */}
                  <div className="flex shrink-0 items-center gap-2">
                    {item.consecutive_failures > 0 && (
                      <span className="rounded-full bg-error/15 px-2 py-0.5 text-xs font-medium text-error">
                        {item.consecutive_failures}×
                      </span>
                    )}
                    {item.last_failure?.failure_message && (
                      <span className="hidden max-w-[200px] truncate text-xs text-on-surface-variant sm:inline">
                        {item.last_failure.failure_message}
                      </span>
                    )}
                  </div>
                </Link>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
