import { alpha, type Theme } from "@mui/material/styles";

/** Semantic palette colors that have CSS-variable channel tokens. */
export type SoftColor = "primary" | "success" | "warning" | "error" | "info";

/**
 * Translucent background helper for the dashboard's many tinted surfaces (e.g.
 * the old `bg-error/10`, `bg-primary/5`). Uses the palette's channel token so it
 * stays correct across light/dark, falling back to `alpha()` if CSS variables
 * are unavailable.
 *
 * @example sx={{ bgcolor: (t) => soft(t, "error", 0.1) }}
 */
export function soft(theme: Theme, color: SoftColor, opacity: number): string {
  const channel = theme.vars?.palette?.[color]?.mainChannel;
  if (channel) return `rgba(${channel} / ${opacity})`;
  return alpha(theme.palette[color].main, opacity);
}

/** Test/job status as reported in the data (case-insensitive). */
export type DashboardStatus = string;

/**
 * Map a dashboard status to the MUI color used by Chip/Alert/etc.
 *   PASSING/passed -> success, FAILING/failed -> error, FLAKY -> warning.
 */
export function statusToMuiColor(
  status: DashboardStatus,
): "success" | "warning" | "error" | "default" {
  switch (status.toUpperCase()) {
    case "PASSING":
    case "PASSED":
      return "success";
    case "FAILING":
    case "FAILED":
      return "error";
    case "FLAKY":
      return "warning";
    default:
      return "default";
  }
}

/**
 * Solid theme color for a pass/fail dot or bar (used by the custom
 * visualizations). Returns a CSS color string from the active theme.
 */
export function dotColorFor(
  theme: Theme,
  passed: boolean,
  result?: string,
): string {
  if (result === "PENDING") return theme.palette.warning.main;
  return passed ? theme.palette.success.main : theme.palette.error.main;
}
