import { styled } from "@mui/material/styles";
import Paper from "@mui/material/Paper";

// Translucent surface for cards, dropdowns and raised panels. Override
// radius and padding via `sx`.
// Cast back to `typeof Paper` so it keeps Paper's polymorphic `component` prop.
export const Panel = styled(Paper)(({ theme }) => ({
  backgroundColor: (theme.vars ?? theme).palette.surface.glass,
  backdropFilter: "blur(12px)",
  WebkitBackdropFilter: "blur(12px)",
  border: `1px solid ${(theme.vars ?? theme).palette.divider}`,
  backgroundImage: "none",
})) as typeof Paper;
