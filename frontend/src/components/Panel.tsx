import { styled } from "@mui/material/styles";
import Paper from "@mui/material/Paper";

// Translucent "glass" surface that replaces the old `.glass` utility. Use it for
// cards, dropdowns and any raised panel. Override radius/padding via `sx`.
export const Panel = styled(Paper)(({ theme }) => ({
  backgroundColor: theme.palette.surface.glass,
  backdropFilter: "blur(12px)",
  WebkitBackdropFilter: "blur(12px)",
  border: `1px solid ${theme.palette.divider}`,
  backgroundImage: "none",
}));
