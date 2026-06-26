import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Divider from "@mui/material/Divider";
import List from "@mui/material/List";
import ListItemButton from "@mui/material/ListItemButton";
import Typography from "@mui/material/Typography";
import ReportProblem from "@mui/icons-material/ReportProblem";
import Insights from "@mui/icons-material/Insights";
import { useMemo } from "react";
import { Link as RouterLink } from "react-router-dom";
import { useFlakinessReport } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { shortJobName, shortTestName } from "../lib/utils";
import { soft } from "../theme";
import { Panel } from "./Panel";
import type { PatternAnalysis, TestFlakiness } from "../types/dashboard";

const MAX_ITEMS = 10;
// Recurring patterns are the highest-signal items (confirmed systemic bugs), so
// they lead the box, but cap them so a noisy fleet can't crowd out the
// test-level regressions below.
const MAX_PATTERNS = 5;

interface ItemGroup {
  label: string;
  items: TestFlakiness[];
}

export function NeedsAttention() {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const { data, loading } = useFlakinessReport();

  // Backend already filters to systemic verdicts and ranks them (confidence,
  // then builds), so just drop any missing a job link and cap for display.
  const recurring = useMemo<PatternAnalysis[]>(
    () => (data?.recurring_patterns ?? []).filter((p) => p.job_id).slice(0, MAX_PATTERNS),
    [data],
  );

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

  if (loading || (recurring.length === 0 && groups.length === 0)) return null;

  const totalItems =
    recurring.length + groups.reduce((sum, g) => sum + g.items.length, 0);

  return (
    <Panel elevation={0} sx={{ borderRadius: "12px", overflow: "hidden" }}>
      <Box
        sx={{
          p: { xs: 2, sm: 2.5 },
          display: "flex",
          alignItems: "center",
          gap: 1,
        }}
      >
        <ReportProblem color="warning" fontSize="small" />
        <Typography variant="headline" component="h2" sx={{ fontSize: "1.25rem" }}>
          Needs Attention ({totalItems})
        </Typography>
      </Box>

      <List
        disablePadding
        sx={{
          maxHeight: "60vh",
          overflowY: "auto",
          px: { xs: 2, sm: 2.5 },
          pb: { xs: 2, sm: 2.5 },
        }}
      >
        {recurring.length > 0 && (
          <Box component="li" sx={{ listStyle: "none" }}>
            <Typography
              variant="label"
              component="p"
              color="text.secondary"
              sx={{ py: 1, textTransform: "uppercase" }}
            >
              Recurring Patterns
            </Typography>

            {recurring.map((pattern) => {
              const confColor = pattern.confidence === "low" ? undefined : "warning";
              return (
                <ListItemButton
                  key={pattern.job_id ?? pattern.subject}
                  component={RouterLink}
                  to={`/job/${encodeURIComponent(pattern.job_id ?? "")}`}
                  sx={{
                    gap: 1.5,
                    px: 1,
                    py: 1,
                    borderRadius: "8px",
                    color: "inherit",
                    textDecoration: "none",
                    "&:hover": {
                      bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                    },
                  }}
                >
                  <Insights
                    sx={{ fontSize: 18, color: "warning.main", flexShrink: 0, mt: "2px" }}
                  />

                  <Box sx={{ minWidth: 0, flex: 1 }}>
                    <Typography variant="caption" color="text.secondary" noWrap>
                      {shortJobName(pattern.subject, filePrefix)}
                    </Typography>
                    <Typography variant="body2" color="text.primary" noWrap>
                      {pattern.shared_root_cause || pattern.summary}
                    </Typography>
                  </Box>

                  <Box
                    sx={{
                      display: "flex",
                      alignItems: "center",
                      gap: 1,
                      flexShrink: 0,
                    }}
                  >
                    <Chip
                      size="small"
                      label={`${pattern.builds_analyzed} builds`}
                      sx={{
                        height: 22,
                        bgcolor: "action.selected",
                        color: "text.secondary",
                        fontWeight: 600,
                        display: { xs: "none", sm: "flex" },
                      }}
                    />
                    <Chip
                      size="small"
                      label={pattern.confidence}
                      sx={{
                        height: 22,
                        fontWeight: 600,
                        ...(confColor
                          ? { bgcolor: (theme) => soft(theme, confColor, 0.15), color: `${confColor}.main` }
                          : { bgcolor: "action.selected", color: "text.secondary" }),
                      }}
                    />
                  </Box>
                </ListItemButton>
              );
            })}
          </Box>
        )}

        {groups.map((group, gi) => (
          <Box key={group.label} component="li" sx={{ listStyle: "none" }}>
            {(gi > 0 || recurring.length > 0) && <Divider sx={{ my: 1 }} />}
            <Typography
              variant="label"
              component="p"
              color="text.secondary"
              sx={{ py: 1, textTransform: "uppercase" }}
            >
              {group.label}
            </Typography>

            {group.items.map((item) => (
              <ListItemButton
                key={`${item.job_id}/${item.test_name}`}
                component={RouterLink}
                to={`/job/${encodeURIComponent(item.job_id)}/test/${encodeURIComponent(item.test_name)}${item.last_failure?.build_id ? `?run=${item.last_failure.build_id}` : ""}`}
                sx={{
                  gap: 1.5,
                  px: 1,
                  py: 1,
                  borderRadius: "8px",
                  color: "inherit",
                  textDecoration: "none",
                  "&:hover": {
                    bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                  },
                }}
              >
                <Box
                  sx={{
                    width: 8,
                    height: 8,
                    borderRadius: "50%",
                    flexShrink: 0,
                    bgcolor:
                      item.classification === "flaky"
                        ? "warning.main"
                        : "error.main",
                  }}
                />

                <Box sx={{ minWidth: 0, flex: 1 }}>
                  <Typography variant="caption" color="text.secondary" noWrap>
                    {shortJobName(item.job_name, filePrefix)}
                  </Typography>
                  <Typography variant="body2" color="text.primary" noWrap>
                    {shortTestName(item.test_name)}
                  </Typography>
                </Box>

                <Box
                  sx={{
                    display: "flex",
                    alignItems: "center",
                    gap: 1,
                    flexShrink: 0,
                    minWidth: 0,
                  }}
                >
                  {item.consecutive_failures > 0 && (
                    <Chip
                      size="small"
                      label={`${item.consecutive_failures}×`}
                      sx={{
                        height: 22,
                        bgcolor: (theme) => soft(theme, "error", 0.15),
                        color: "error.main",
                        fontWeight: 600,
                      }}
                    />
                  )}
                  {item.last_failure?.failure_message && (
                    <Typography
                      variant="caption"
                      color="text.secondary"
                      sx={{
                        display: { xs: "none", sm: "block" },
                        maxWidth: 200,
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                      }}
                    >
                      {item.last_failure.failure_message}
                    </Typography>
                  )}
                </Box>
              </ListItemButton>
            ))}
          </Box>
        ))}
      </List>
    </Panel>
  );
}
