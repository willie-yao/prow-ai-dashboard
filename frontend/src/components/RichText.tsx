import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import { Fragment, type ReactNode } from "react";
import { formatSteps, fileToUrl, type FileToUrlContext } from "../lib/utils";

interface RichTextProps {
  text: string;
  /**
   * Run non-code segments through formatSteps (insert line breaks before
   * numbered steps and bullets). Use for multi-line analysis prose; leave off
   * for short one-line summaries.
   */
  steps?: boolean;
  /**
   * When provided, code spans and bare file paths in the prose that resolve to
   * a known prow artifact or source file are rendered as links. Resolution goes
   * through fileToUrl, which is deterministic and returns null for anything it
   * can't confidently map (versions, line-number citations, error messages,
   * resource names), so unresolvable tokens stay plain and links are never
   * fabricated.
   */
  fileCtx?: FileToUrlContext;
}

// CodeSpan styling matches markdown inline code: a monospace pill with a subtle
// fill. fontSize is relative so it scales with the surrounding Typography
// variant (body2, caption, ...).
const codeSx = {
  fontFamily: "monospace",
  fontSize: "0.85em",
  px: 0.5,
  py: 0.125,
  borderRadius: "4px",
  bgcolor: "action.selected",
  color: "text.primary",
  wordBreak: "break-word",
} as const;

// A linked code span keeps the pill but reads as a link.
const codeLinkSx = {
  ...codeSx,
  color: "primary.main",
  textDecorationColor: "transparent",
  "&:hover": { textDecorationColor: "inherit" },
} as const;

// A bare path linked inline in prose: monospace + link color, no pill, so dense
// prose with many paths stays readable.
const pathLinkSx = {
  fontFamily: "monospace",
  color: "primary.main",
  wordBreak: "break-word",
  textDecorationColor: "transparent",
  "&:hover": { textDecorationColor: "inherit" },
} as const;

// Inline-code span splitter: capture groups land on odd indices.
const CODE_SPLIT = /`([^`]+)`/g;

// basename shortens a linked file path to its final segment for display, so
// long paths don't clutter the prose. The full path is kept in the link's
// title (hover) and the href is unchanged. A path with no separator is
// returned as-is.
function basename(path: string): string {
  const i = path.lastIndexOf("/");
  return i >= 0 ? path.slice(i + 1) : path;
}

// Candidate file path in free prose: one or more "/"-separated segments ending
// in a known source/artifact extension. Requiring a slash and an extension
// keeps it conservative; fileToUrl makes the final keep/drop decision so no
// broken links are produced. A trailing line ref (":120", ":120-130") is left
// out of the match so the path still resolves.
const PATH_RE =
  /(?:[\w.-]+\/)+[\w.-]+\.(?:go|ya?ml|sh|json|tpl|md|log|txt|xml|out|conf)\b/g;

// Linkify resolvable bare file paths in a prose string. Returns the raw string
// when nothing resolves (keeps the parent's whiteSpace: pre-line intact).
function linkifyPaths(
  text: string,
  fileCtx: FileToUrlContext | undefined,
  keyBase: number,
): ReactNode {
  if (!fileCtx) return text;
  PATH_RE.lastIndex = 0;
  const out: ReactNode[] = [];
  let last = 0;
  let k = 0;
  let m: RegExpExecArray | null;
  while ((m = PATH_RE.exec(text)) !== null) {
    const token = m[0];
    const url = fileToUrl(token, fileCtx);
    if (!url) continue;
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push(
      <Link
        key={`${keyBase}-${k++}`}
        href={url}
        target="_blank"
        rel="noopener noreferrer"
        sx={pathLinkSx}
        title={token}
      >
        {basename(token)}
      </Link>,
    );
    last = m.index + token.length;
  }
  if (out.length === 0) return text;
  if (last < text.length) out.push(text.slice(last));
  return out;
}

/**
 * Render AI analysis text with markdown-style inline `code` spans (filenames,
 * error messages, variable names, etc.) as styled inline code, and link both
 * code spans and bare file paths that resolve to a real prow artifact or source
 * file (when fileCtx is supplied). Everything else renders verbatim, so callers
 * keep whiteSpace: "pre-line" on the container to preserve newlines.
 */
export function RichText({ text, steps = false, fileCtx }: RichTextProps) {
  const parts = text.split(CODE_SPLIT);
  return (
    <>
      {parts.map((part, i) => {
        if (i % 2 === 0) {
          const formatted = steps ? formatSteps(part) : part;
          return <Fragment key={i}>{linkifyPaths(formatted, fileCtx, i)}</Fragment>;
        }
        const url = fileCtx ? fileToUrl(part, fileCtx) : null;
        if (url) {
          return (
            <Link
              key={i}
              href={url}
              target="_blank"
              rel="noopener noreferrer"
              sx={codeLinkSx}
              title={part}
            >
              {basename(part)}
            </Link>
          );
        }
        return (
          <Box component="code" key={i} sx={codeSx}>
            {part}
          </Box>
        );
      })}
    </>
  );
}
