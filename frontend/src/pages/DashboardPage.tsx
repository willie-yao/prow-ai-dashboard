import Box from "@mui/material/Box";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import Typography from "@mui/material/Typography";
import type { SxProps, Theme } from "@mui/material/styles";
import { useMemo, useState } from "react";
import { useDashboard } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import {
  timeAgo,
  groupByCategory,
  categoryLabelsFromRules,
  categoryDisplayOrder,
} from "../lib/utils";
import type { JobSummary } from "../types/dashboard";
import { SummaryBar } from "../components/SummaryBar";
import { NeedsAttention } from "../components/NeedsAttention";
import { JobCard } from "../components/JobCard";
import { LoadingState } from "../components/LoadingState";
import { ErrorState } from "../components/ErrorState";

type StatusFilter = "ALL" | "PASSING" | "FLAKY" | "FAILING";

const statusFilters: { label: string; value: StatusFilter }[] = [
  { label: "All", value: "ALL" },
  { label: "Passing", value: "PASSING" },
  { label: "Flaky", value: "FLAKY" },
  { label: "Failing", value: "FAILING" },
];

const toggleGroupSx: SxProps<Theme> = {
  display: "flex",
  flexWrap: "wrap",
  gap: 0.75,
  "& .MuiToggleButtonGroup-grouped": {
    border: 0,
    borderRadius: "999px !important",
    mx: 0,
  },
};

const toggleButtonSx: SxProps<Theme> = {
  px: 1.5,
  py: 0.5,
  minHeight: 0,
  typography: "label",
  textTransform: "none",
  color: "text.secondary",
  bgcolor: (theme) => theme.palette.surface.container,
  transition: "background-color 160ms ease, color 160ms ease",
  "&:hover": {
    bgcolor: (theme) => theme.palette.surface.containerHigh,
  },
  "&.Mui-selected": {
    color: "primary.contrastText",
    bgcolor: "primary.main",
    "&:hover": { bgcolor: "primary.dark" },
  },
};

export function DashboardPage() {
  const { data, loading, error } = useDashboard();
  const manifest = useManifest();
  const categoryLabels = useMemo(
    () => categoryLabelsFromRules(manifest.categories),
    [manifest.categories]
  );
  const categoryOrder = useMemo(
    () => categoryDisplayOrder(manifest.categories, manifest.category_display_order),
    [manifest.categories, manifest.category_display_order]
  );
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("ALL");
  const [branchFilter, setBranchFilter] = useState("ALL");

  const branches = useMemo(() => {
    if (!data) return [];
    const set = new Set(data.jobs.map((j) => j.branch).filter(Boolean));
    return Array.from(set).sort((a, b) => {
      // "main" always first
      if (a === "main") return -1;
      if (b === "main") return 1;
      // Release branches: sort descending (newest first)
      // Extract version numbers for proper numeric comparison
      const aMatch = a.match(/(\d+)\.(\d+)/);
      const bMatch = b.match(/(\d+)\.(\d+)/);
      if (aMatch && bMatch) {
        const aMajor = Number(aMatch[1]),
          aMinor = Number(aMatch[2]);
        const bMajor = Number(bMatch[1]),
          bMinor = Number(bMatch[2]);
        if (aMajor !== bMajor) return bMajor - aMajor;
        return bMinor - aMinor;
      }
      return a.localeCompare(b);
    });
  }, [data]);

  const filtered = useMemo(() => {
    if (!data) return [];
    return data.jobs.filter((j: JobSummary) => {
      if (statusFilter !== "ALL" && j.overall_status !== statusFilter)
        return false;
      if (branchFilter !== "ALL" && j.branch !== branchFilter) return false;
      return true;
    });
  }, [data, statusFilter, branchFilter]);

  const grouped = useMemo(() => groupByCategory(filtered), [filtered]);
  const hasCategories = (manifest.categories?.length ?? 0) > 0;

  if (loading) {
    return <LoadingState />;
  }

  if (error) {
    return <ErrorState message={error} onRetry={() => window.location.reload()} />;
  }

  if (!data) return null;

  const sortedCategories = Object.keys(grouped).sort((a, b) => {
    const ai = categoryOrder.indexOf(a);
    const bi = categoryOrder.indexOf(b);
    return (ai === -1 ? 999 : ai) - (bi === -1 ? 999 : bi);
  });

  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: 4 }}>
      <Box>
        <Typography variant="h4" component="h1">
          Test Health Overview
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
          Last updated: {timeAgo(data.generated_at)}
        </Typography>
      </Box>

      <NeedsAttention />

      <SummaryBar
        jobs={data.jobs}
        onFilterClick={(s) => setStatusFilter(s as StatusFilter)}
        activeFilter={statusFilter}
      />

      <Box sx={{ display: "flex", flexWrap: "wrap", gap: 3 }}>
        <Box
          sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}
        >
          <Typography variant="label" color="text.secondary">
            Status
          </Typography>
          <ToggleButtonGroup
            exclusive
            value={statusFilter}
            onChange={(_, value: StatusFilter | null) => {
              if (value) setStatusFilter(value);
            }}
            aria-label="Status filter"
            sx={toggleGroupSx}
          >
            {statusFilters.map((f) => (
              <ToggleButton key={f.value} value={f.value} sx={toggleButtonSx}>
                {f.label}
              </ToggleButton>
            ))}
          </ToggleButtonGroup>
        </Box>

        <Box
          sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}
        >
          <Typography variant="label" color="text.secondary">
            Branch
          </Typography>
          <ToggleButtonGroup
            exclusive
            value={branchFilter}
            onChange={(_, value: string | null) => {
              if (value) setBranchFilter(value);
            }}
            aria-label="Branch filter"
            sx={toggleGroupSx}
          >
            <ToggleButton value="ALL" sx={toggleButtonSx}>
              All
            </ToggleButton>
            {branches.map((b) => (
              <ToggleButton key={b} value={b} sx={toggleButtonSx}>
                {b}
              </ToggleButton>
            ))}
          </ToggleButtonGroup>
        </Box>
      </Box>

      {filtered.length === 0 ? (
        <Box sx={{ py: 8, textAlign: "center" }}>
          <Typography color="text.secondary">No jobs match filters</Typography>
        </Box>
      ) : !hasCategories ? (
        <Box
          sx={{
            display: "grid",
            gridTemplateColumns: {
              xs: "1fr",
              md: "1fr 1fr",
              lg: "repeat(3, 1fr)",
            },
            gap: 2,
          }}
        >
          {filtered.map((job) => (
            <JobCard key={job.name} job={job} />
          ))}
        </Box>
      ) : (
        sortedCategories.map((category) => (
          <Box key={category} component="section">
            <Typography
              variant="headline"
              component="h2"
              sx={{ mb: 2, fontSize: "1.25rem" }}
            >
              {categoryLabels[category] ??
                category.charAt(0).toUpperCase() + category.slice(1)}
            </Typography>
            <Box
              sx={{
                display: "grid",
                gridTemplateColumns: {
                  xs: "1fr",
                  md: "1fr 1fr",
                  lg: "repeat(3, 1fr)",
                },
                gap: 2,
              }}
            >
              {grouped[category].map((job) => (
                <JobCard key={job.name} job={job} />
              ))}
            </Box>
          </Box>
        ))
      )}
    </Box>
  );
}
