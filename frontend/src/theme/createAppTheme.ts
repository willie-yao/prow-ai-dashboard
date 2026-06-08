import {
  createTheme,
  type Theme,
  type PaletteOptions,
} from "@mui/material/styles";
import "./augmentation";
import { darkTokens, lightTokens, type ColorTokens } from "./tokens";
import { buildComponents } from "./components";

// Map raw MD3 tokens onto MUI palette slots. Semantic mapping:
//   primary (blue)   -> primary
//   secondary (green, PASSING) -> success
//   tertiary (amber, FLAKY)    -> warning
//   error (red)      -> error
// The extra surface-container levels live under the custom `surface` key.
function paletteFromTokens(t: ColorTokens): PaletteOptions {
  return {
    primary: {
      main: t.primary,
      dark: t.primaryDim,
      light: t.primaryContainer,
      contrastText: t.onPrimary,
    },
    success: {
      main: t.secondary,
      dark: t.secondaryDim,
      light: t.secondaryContainer,
      contrastText: t.onSecondary,
    },
    warning: {
      main: t.tertiary,
      light: t.tertiaryContainer,
      contrastText: t.onTertiary,
    },
    error: {
      main: t.error,
      dark: t.errorDim,
      light: t.errorContainer,
      contrastText: t.onError,
    },
    background: {
      default: t.background,
      paper: t.surfaceContainer,
    },
    text: {
      primary: t.onSurface,
      secondary: t.onSurfaceVariant,
    },
    divider: t.outlineVariant,
    surface: {
      main: t.surface,
      dim: t.surfaceDim,
      bright: t.surfaceBright,
      container: t.surfaceContainer,
      containerLow: t.surfaceContainerLow,
      containerHigh: t.surfaceContainerHigh,
      containerHighest: t.surfaceContainerHighest,
      variant: t.surfaceVariant,
      tint: t.surfaceTint,
      glass: t.glass,
    },
  };
}

const typography = {
  fontFamily: '"Inter", system-ui, -apple-system, "Segoe UI", sans-serif',
  // Root font-size is 17px (see index.css); keep MUI's px<->rem math in sync.
  htmlFontSize: 17,
  h1: { fontWeight: 800, letterSpacing: "-0.02em" },
  h2: { fontWeight: 700, letterSpacing: "-0.01em" },
  h3: { fontWeight: 700, letterSpacing: "-0.01em" },
  h4: { fontWeight: 700 },
  h5: { fontWeight: 600 },
  h6: { fontWeight: 600 },
  button: { fontWeight: 600 },
  // Custom variants mirroring the old `font-headline` / `font-label` utilities.
  headline: {
    fontFamily: '"Inter", sans-serif',
    fontWeight: 700,
    letterSpacing: "-0.01em",
  },
  label: {
    fontFamily: '"Space Grotesk", "Inter", sans-serif',
    fontWeight: 500,
    fontSize: "0.75rem",
    letterSpacing: "0.04em",
    lineHeight: 1.4,
  },
};

// Build the dashboard theme. Light + dark color schemes are generated from the
// token sets and switched at runtime via a class selector (see useColorScheme
// in the app shell). To create a different theme, pass different token sets or
// add a new factory and register it in themes.ts.
export function createAppTheme(
  tokens: { light: ColorTokens; dark: ColorTokens } = {
    light: lightTokens,
    dark: darkTokens,
  },
): Theme {
  return createTheme({
    cssVariables: { colorSchemeSelector: "class" },
    defaultColorScheme: "dark",
    colorSchemes: {
      light: { palette: paletteFromTokens(tokens.light) },
      dark: { palette: paletteFromTokens(tokens.dark) },
    },
    shape: { borderRadius: 12 },
    typography,
    components: buildComponents(),
  });
}
