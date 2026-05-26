import type { JobSummary } from "../types/dashboard";

interface SummaryBarProps {
  jobs: JobSummary[];
  onFilterClick?: (status: string) => void;
  activeFilter?: string;
}

export function SummaryBar({ jobs, onFilterClick, activeFilter }: SummaryBarProps) {
  const passing = jobs.filter((j) => j.overall_status === "PASSING").length;
  const flaky = jobs.filter((j) => j.overall_status === "FLAKY").length;
  const failing = jobs.filter((j) => j.overall_status === "FAILING").length;

  const cards = [
    { label: "Passing", status: "PASSING", count: passing, text: "text-secondary", bg: "bg-secondary/10", ring: "ring-secondary" },
    { label: "Flaky", status: "FLAKY", count: flaky, text: "text-tertiary", bg: "bg-tertiary/10", ring: "ring-tertiary" },
    { label: "Failing", status: "FAILING", count: failing, text: "text-error", bg: "bg-error/10", ring: "ring-error" },
  ] as const;

  return (
    <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
      {cards.map((card) => {
        const isActive = activeFilter === card.status;
        return (
          <button
            key={card.label}
            type="button"
            onClick={() => onFilterClick?.(isActive ? "ALL" : card.status)}
            className={`glass flex flex-col items-center justify-center gap-1 rounded-2xl border border-outline-variant px-4 py-5 transition-all hover:brightness-110 cursor-pointer ${card.bg} ${isActive ? `ring-2 ${card.ring}` : ""}`}
          >
            <span className={`text-3xl font-bold ${card.text}`}>
              {card.count}
            </span>
            <span className="font-label text-xs uppercase tracking-wider text-on-surface-variant">
              {card.label}
            </span>
          </button>
        );
      })}
    </div>
  );
}
