import Box from "@mui/material/Box";
import Tooltip from "@mui/material/Tooltip";
import { Link as RouterLink } from "react-router-dom";
import type { RunSummary } from "../types/dashboard";
import { dotColorFor } from "../theme";

interface SparklineProps {
  runs: RunSummary[];
  jobID: string;
}

export function Sparkline({ runs, jobID }: SparklineProps) {
  // Show newest on the right: recent_runs is newest-first, so take first 8 and reverse
  const recent = runs.slice(0, 8).reverse();

  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 0.75 }}>
      {recent.map((run) => (
        <Tooltip
          key={run.build_id}
          title={`#${run.build_id} — ${run.passed ? "Passed" : "Failed"}`}
        >
          <Box
            component={RouterLink}
            to={`/job/${encodeURIComponent(jobID)}?run=${run.build_id}`}
            aria-label={`Run ${run.build_id} ${run.passed ? "passed" : "failed"}`}
            onClick={(event) => event.stopPropagation()}
            sx={{
              display: "inline-flex",
              alignItems: "center",
              justifyContent: "center",
              p: 0.5,
              m: -0.5,
              borderRadius: "50%",
              "&:hover > span": { transform: "scale(1.25)" },
            }}
          >
            <Box
              component="span"
              sx={{
                display: "block",
                width: 10,
                height: 10,
                borderRadius: "50%",
                bgcolor: (theme) => dotColorFor(theme, run.passed),
                transition: "transform 140ms ease",
              }}
            />
          </Box>
        </Tooltip>
      ))}
    </Box>
  );
}
