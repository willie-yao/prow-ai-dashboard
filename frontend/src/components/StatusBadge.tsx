import { statusColor, statusBg } from "../lib/utils";

interface StatusBadgeProps {
  status: string;
}

export function StatusBadge({ status }: StatusBadgeProps) {
  return (
    <span
      className={`inline-flex items-center rounded-full px-2.5 py-0.5 font-label text-xs font-medium uppercase tracking-wide ${statusColor(status)} ${statusBg(status)}/15`}
    >
      {status}
    </span>
  );
}
