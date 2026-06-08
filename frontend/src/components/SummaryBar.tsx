import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Typography from "@mui/material/Typography";
import { Panel } from "./Panel";
import { soft, type SoftColor } from "../theme";
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

  const cards: {
    label: string;
    status: "PASSING" | "FLAKY" | "FAILING";
    count: number;
    color: Extract<SoftColor, "success" | "warning" | "error">;
  }[] = [
    { label: "Passing", status: "PASSING", count: passing, color: "success" },
    { label: "Flaky", status: "FLAKY", count: flaky, color: "warning" },
    { label: "Failing", status: "FAILING", count: failing, color: "error" },
  ];

  return (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: { xs: "1fr", sm: "repeat(3, 1fr)" },
        gap: 2,
      }}
    >
      {cards.map((card) => {
        const isActive = activeFilter === card.status;
        return (
          <Panel
            key={card.label}
            elevation={0}
            sx={{
              borderRadius: "16px",
              overflow: "hidden",
              bgcolor: (theme) => soft(theme, card.color, 0.1),
              boxShadow: (theme) =>
                isActive ? `0 0 0 2px ${theme.palette[card.color].main}` : "none",
            }}
          >
            <ButtonBase
              onClick={() => onFilterClick?.(isActive ? "ALL" : card.status)}
              disabled={!onFilterClick}
              sx={{
                width: "100%",
                px: 4,
                py: 2.5,
                display: "flex",
                flexDirection: "column",
                alignItems: "center",
                justifyContent: "center",
                gap: 0.5,
                cursor: onFilterClick ? "pointer" : "default",
                transition: "filter 160ms ease, background-color 160ms ease",
                "&:hover": {
                  filter: onFilterClick ? "brightness(1.08)" : "none",
                },
              }}
            >
              <Typography
                variant="h4"
                component="span"
                sx={{ fontWeight: 700, color: `${card.color}.main` }}
              >
                {card.count}
              </Typography>
              <Typography
                variant="label"
                component="span"
                sx={{ textTransform: "uppercase", color: "text.secondary" }}
              >
                {card.label}
              </Typography>
            </ButtonBase>
          </Panel>
        );
      })}
    </Box>
  );
}
