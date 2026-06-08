import Chip, { type ChipProps } from "@mui/material/Chip";
import { statusToMuiColor, soft } from "../theme";

interface StatusChipProps extends Omit<ChipProps, "color" | "label"> {
  /** Dashboard status, e.g. "PASSING", "FAILING", "FLAKY", "passed". */
  status: string;
  /** Override the displayed text (defaults to the status itself). */
  label?: string;
}

// Pill showing a test/job status with the themed color. Replaces the old
// StatusBadge + statusColor/statusBg helpers.
export function StatusChip({ status, label, sx, ...rest }: StatusChipProps) {
  const color = statusToMuiColor(status);
  return (
    <Chip
      size="small"
      label={label ?? status}
      sx={[
        {
          textTransform: "uppercase",
          letterSpacing: "0.04em",
          fontWeight: 600,
          ...(color === "default"
            ? { bgcolor: "action.selected", color: "text.secondary" }
            : {
                bgcolor: (t) => soft(t, color, 0.15),
                color: `${color}.main`,
              }),
        },
        ...(Array.isArray(sx) ? sx : [sx]),
      ]}
      {...rest}
    />
  );
}
