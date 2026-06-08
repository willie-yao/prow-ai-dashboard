import type { Components, Theme } from "@mui/material/styles";

// Global component defaults/overrides that encode the dashboard's look once, so
// individual components stay lean. Component-specific looks (e.g. the glass
// panel) live in the shared primitives, not here.
export function buildComponents(): Components<Theme> {
  return {
    MuiCssBaseline: {
      styleOverrides: {
        body: {
          WebkitFontSmoothing: "antialiased",
          MozOsxFontSmoothing: "grayscale",
        },
      },
    },
    MuiButton: {
      defaultProps: { disableElevation: true },
      styleOverrides: {
        root: { textTransform: "none", fontWeight: 600 },
      },
    },
    MuiChip: {
      styleOverrides: {
        label: { fontWeight: 600 },
      },
    },
    MuiTooltip: {
      defaultProps: { arrow: true },
    },
    MuiTypography: {
      defaultProps: {
        variantMapping: {
          headline: "h2",
          label: "span",
        },
      },
    },
  };
}
