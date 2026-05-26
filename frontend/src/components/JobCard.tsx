import { Link } from "react-router-dom";
import type { JobSummary } from "../types/dashboard";
import { formatPercent, timeAgo, formatDuration } from "../lib/utils";
import { StatusBadge } from "./StatusBadge";
import { Sparkline } from "./Sparkline";

interface JobCardProps {
  job: JobSummary;
}

export function JobCard({ job }: JobCardProps) {
  const lastRunTime = job.last_run ? timeAgo(job.last_run.timestamp) : "—";
  const lastDuration =
    job.last_run?.duration_seconds != null
      ? formatDuration(job.last_run.duration_seconds)
      : "—";

  return (
    <Link
      to={`/job/${encodeURIComponent(job.name)}`}
      className="glass group flex flex-col gap-3 rounded-2xl border border-outline-variant p-4 transition-all hover:brightness-125"
    >
      <div className="flex items-start justify-between gap-2">
        <h3 className="font-headline text-sm font-semibold leading-snug text-on-surface group-hover:text-primary">
          {job.tab_name}
        </h3>
        <StatusBadge status={job.overall_status} />
      </div>

      {job.description && (
        <p className="line-clamp-2 text-xs leading-relaxed text-on-surface-variant">
          {job.description}
        </p>
      )}

      <Sparkline runs={job.recent_runs} jobName={job.name} />

      <div className="mt-auto flex items-center gap-4 border-t border-outline-variant pt-3 font-label text-[11px] tracking-wide text-on-surface-variant">
        <span>
          Pass{" "}
          <span className="text-on-surface">
            {formatPercent(job.pass_rate_7d)}
          </span>
        </span>
        <span>
          Last{" "}
          <span className="text-on-surface">{lastRunTime}</span>
        </span>
        <span>
          Dur{" "}
          <span className="text-on-surface">{lastDuration}</span>
        </span>
      </div>
    </Link>
  );
}
