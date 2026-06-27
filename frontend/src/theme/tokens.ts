// Single source of truth for raw color values. Change a color once here and it
// propagates through the MUI theme. Each scheme exposes the same Material Design
// 3 token keys. To add a theme, create another ColorTokens object and wire it up
// in themes.ts.

export interface ColorTokens {
  background: string;
  surface: string;
  surfaceDim: string;
  surfaceBright: string;
  surfaceContainer: string;
  surfaceContainerLow: string;
  surfaceContainerHigh: string;
  surfaceContainerHighest: string;
  surfaceVariant: string;

  onSurface: string;
  onSurfaceVariant: string;

  primary: string;
  primaryDim: string;
  primaryContainer: string;
  onPrimary: string;

  // PASSING green maps to MUI success.
  secondary: string;
  secondaryDim: string;
  secondaryContainer: string;
  onSecondary: string;

  // FLAKY amber maps to MUI warning.
  tertiary: string;
  tertiaryContainer: string;
  onTertiary: string;

  error: string;
  errorDim: string;
  errorContainer: string;
  onError: string;

  // Pass/fail dot colors for run visualizations. Equal perceived brightness
  // keeps one dot from appearing larger than the other on dark surfaces.
  dotPass: string;
  dotFail: string;

  outline: string;
  outlineVariant: string;

  surfaceTint: string;

  // Translucent panel background. Stored pre-baked with alpha per scheme so it
  // switches correctly without runtime alpha math on a CSS variable.
  glass: string;
}

// Dark palette from the Material Design 3 values used by the current design.
export const darkTokens: ColorTokens = {
  background: "#0e0e0e",
  surface: "#0e0e0e",
  surfaceDim: "#0e0e0e",
  surfaceBright: "#2c2c2c",
  surfaceContainer: "#1a1919",
  surfaceContainerLow: "#131313",
  surfaceContainerHigh: "#201f1f",
  surfaceContainerHighest: "#262626",
  surfaceVariant: "#262626",

  onSurface: "#ffffff",
  onSurfaceVariant: "#adaaaa",

  primary: "#87adff",
  primaryDim: "#006ff0",
  primaryContainer: "#6f9fff",
  onPrimary: "#002c67",

  secondary: "#69f6b8",
  secondaryDim: "#58e7ab",
  secondaryContainer: "#006c49",
  onSecondary: "#005a3c",

  tertiary: "#ffb148",
  tertiaryContainer: "#f8a010",
  onTertiary: "#573500",

  error: "#ff716c",
  errorDim: "#d7383b",
  errorContainer: "#9f0519",
  onError: "#490006",

  // Brightness-matched against the bright mint pass color so a lone failed dot
  // among passes does not read as smaller or higher.
  dotPass: "#45c78f",
  dotFail: "#ff8e89",

  outline: "#777575",
  outlineVariant: "#494847",

  surfaceTint: "#87adff",

  glass: "rgba(32, 31, 31, 0.8)",
};

// Light palette uses Material Design 3 values derived from the same hues.
export const lightTokens: ColorTokens = {
  background: "#fbfbff",
  surface: "#fbfbff",
  surfaceDim: "#dad9e0",
  surfaceBright: "#fbfbff",
  surfaceContainer: "#f0eff5",
  surfaceContainerLow: "#f5f4fa",
  surfaceContainerHigh: "#eae9f0",
  surfaceContainerHighest: "#e4e3ea",
  surfaceVariant: "#e1e2ec",

  onSurface: "#1a1c1e",
  onSurfaceVariant: "#44474e",

  primary: "#005ac6",
  primaryDim: "#0049a8",
  primaryContainer: "#d8e2ff",
  onPrimary: "#ffffff",

  secondary: "#006c49",
  secondaryDim: "#00583a",
  secondaryContainer: "#8ff8c4",
  onSecondary: "#ffffff",

  tertiary: "#8a5100",
  tertiaryContainer: "#ffddb3",
  onTertiary: "#ffffff",

  error: "#ba1a1a",
  errorDim: "#93000a",
  errorContainer: "#ffdad6",
  onError: "#ffffff",

  // Light scheme renders dark dots on a light surface, so no bloom mismatch;
  // keep the semantic pass/fail hues.
  dotPass: "#006c49",
  dotFail: "#ba1a1a",

  outline: "#74777f",
  outlineVariant: "#c4c6cf",

  surfaceTint: "#005ac6",

  glass: "rgba(240, 239, 245, 0.8)",
};
