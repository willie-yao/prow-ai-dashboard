import { createAppTheme } from "./createAppTheme";

// Named-theme registry. Adding another design is a one-line entry here; the app
// shell reads `defaultTheme`. Swap or extend this to support a theme picker.
export const themes = {
  default: createAppTheme(),
} as const;

export type ThemeName = keyof typeof themes;

export const defaultTheme = themes.default;
