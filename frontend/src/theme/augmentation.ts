// TypeScript module augmentation so our custom palette keys and typography
// variants are first-class citizens of the MUI theme (usable via `sx`, the
// `theme.palette.*` API, and the Typography `variant` prop).
import type * as React from "react";

// Extra surface-container levels MD3 exposes that MUI's default palette lacks.
// MUI gives us background.default + background.paper; these fill in the rest.
export interface SurfacePalette {
  main: string;
  dim: string;
  bright: string;
  container: string;
  containerLow: string;
  containerHigh: string;
  containerHighest: string;
  variant: string;
  tint: string;
  glass: string;
}

// Pass/fail colors for the run-history dot/bar visualizations, kept separate
// from the semantic success/error palette so they can be brightness-matched.
export interface DotPalette {
  pass: string;
  fail: string;
}

declare module "@mui/material/styles" {
  interface Palette {
    surface: SurfacePalette;
    dot: DotPalette;
  }
  interface PaletteOptions {
    surface?: Partial<SurfacePalette>;
    dot?: Partial<DotPalette>;
  }

  interface TypographyVariants {
    headline: React.CSSProperties;
    label: React.CSSProperties;
  }
  interface TypographyVariantsOptions {
    headline?: React.CSSProperties;
    label?: React.CSSProperties;
  }
}

declare module "@mui/material/Typography" {
  interface TypographyPropsVariantOverrides {
    headline: true;
    label: true;
  }
}
