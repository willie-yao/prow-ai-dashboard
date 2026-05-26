import type { BuildResult } from "../types/dashboard";
import { dotColor } from "../lib/utils";

interface RunTimelineProps {
  runs: BuildResult[];
  selectedBuildId?: string;
  onSelect: (buildId: string) => void;
  /** Optional custom color function per run. If not provided, uses pass/fail from run.passed. */
  colorFn?: (run: BuildResult) => string;
  /** Optional custom tooltip per run. */
  tooltipFn?: (run: BuildResult) => string;
}

function shortDate(dateStr: string): string {
  const d = new Date(dateStr);
  return `${d.getMonth() + 1}/${d.getDate()}`;
}

export function RunTimeline({
  runs,
  selectedBuildId,
  onSelect,
  colorFn,
  tooltipFn,
}: RunTimelineProps) {
  // Oldest first so newest is on the right
  const sorted = [...runs].sort(
    (a, b) => new Date(a.started).getTime() - new Date(b.started).getTime()
  );

  return (
    <div className="overflow-x-auto">
      <div className="flex items-start gap-2 p-1">
        {sorted.map((run, i) => {
          const isSelected = run.build_id === selectedBuildId;
          const showDate = sorted.length <= 10 || i % 5 === 0 || i === sorted.length - 1;
          const color = colorFn ? colorFn(run) : dotColor(run.passed, run.result);
          const tooltip = tooltipFn ? tooltipFn(run) : `#${run.build_id} — ${run.passed ? "Passed" : "Failed"}`;

          return (
            <div key={run.build_id} className="flex flex-col items-center">
              <button
                type="button"
                onClick={() => onSelect(run.build_id)}
                className={`h-6 w-10 rounded-sm transition-all ${color} ${
                  isSelected
                    ? "ring-2 ring-primary ring-offset-1 ring-offset-surface"
                    : "hover:brightness-125"
                }`}
                title={tooltip}
              />
              <span className={`mt-1.5 font-label text-[9px] ${showDate ? "text-on-surface-variant" : "invisible"}`}>
                {shortDate(run.started)}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
