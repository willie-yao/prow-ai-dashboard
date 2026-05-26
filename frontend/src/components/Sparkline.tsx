import { useState } from "react";
import { Link } from "react-router-dom";
import type { RunSummary } from "../types/dashboard";
import { dotColor } from "../lib/utils";

interface SparklineProps {
  runs: RunSummary[];
  jobName: string;
}

export function Sparkline({ runs, jobName }: SparklineProps) {
  const [hoveredIdx, setHoveredIdx] = useState<number | null>(null);
  // Show newest on the right: recent_runs is newest-first, so take first 8 and reverse
  const recent = runs.slice(0, 8).reverse();

  return (
    <div className="relative flex items-center gap-1.5">
      {recent.map((run, i) => (
        <Link
          key={run.build_id}
          to={`/job/${encodeURIComponent(jobName)}?run=${run.build_id}`}
          className="group relative"
          onMouseEnter={() => setHoveredIdx(i)}
          onMouseLeave={() => setHoveredIdx(null)}
        >
          <span
            className={`block h-2.5 w-2.5 rounded-full transition-transform group-hover:scale-125 ${dotColor(run.passed)}`}
          />
          {hoveredIdx === i && (
            <span className="absolute bottom-full left-1/2 z-10 mb-1.5 -translate-x-1/2 whitespace-nowrap rounded bg-surface-container-highest px-2 py-1 font-label text-[10px] text-on-surface shadow-lg">
              #{run.build_id} — {run.passed ? "Passed" : "Failed"}
            </span>
          )}
        </Link>
      ))}
    </div>
  );
}
