import { useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { BugReport, Build, GitHub } from "@mui/icons-material";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";

type Action = "create-issue" | "propose-fix";

interface Preview {
  token: string;
  kind: "issue" | "fix";
  title: string;
  body: string;
  diff?: string;
}

const API_BASE = import.meta.env.BASE_URL;

// stripComments removes the hidden dedup HTML comment from a body for display;
// the posted content keeps it.
function stripComments(s: string): string {
  return s.replace(/<!--[\s\S]*?-->/g, "").trim();
}

// FailureActions renders admin write buttons for one systemic pattern. It shows
// only in server mode when the actions capability is present. Auth state is
// shared with the navbar via useAuth: in oauth mode a signed-out admin sees a
// contextual sign-in prompt; once signed in (or in proxy mode) the buttons act.
// Each action first previews the exact issue/PR in a dialog so the admin can
// review, optionally refine it with a prompt, and confirm before anything is
// posted.
export function FailureActions({ failureID }: { failureID: string }) {
  const { features } = useCapabilities();
  const { status, signIn } = useAuth();
  const [action, setAction] = useState<Action | null>(null);
  const [busy, setBusy] = useState<"preview" | "refine" | "confirm" | null>(null);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [instruction, setInstruction] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [url, setUrl] = useState<string | null>(null);

  if (!features.actions || !failureID || status === "loading" || status === "unavailable") {
    return null;
  }

  if (status === "anonymous") {
    return (
      <Button
        size="small"
        variant="outlined"
        startIcon={<GitHub sx={{ fontSize: 18 }} />}
        onClick={signIn}
      >
        Sign in to file issues or fixes
      </Button>
    );
  }

  function open(act: Action) {
    setAction(act);
    setPreview(null);
    setInstruction("");
    setError(null);
    setUrl(null);
    void loadPreview(act, false);
  }

  async function loadPreview(act: Action, refine: boolean) {
    setBusy(refine ? "refine" : "preview");
    setError(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(failureID)}/${act}/preview`,
        {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(refine ? { instruction } : {}),
        },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      setPreview((await res.json()) as Preview);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  async function confirm() {
    if (!preview) return;
    setBusy("confirm");
    setError(null);
    try {
      const res = await fetch(`${API_BASE}api/actions/confirm`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token: preview.token }),
      });
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      const body = (await res.json()) as { url: string };
      setUrl(body.url);
      close();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  function close() {
    setAction(null);
    setPreview(null);
    setInstruction("");
    setError(null);
  }

  const isFix = action === "propose-fix";

  return (
    <Box>
      <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<BugReport sx={{ fontSize: 18 }} />}
          disabled={action !== null}
          onClick={() => open("create-issue")}
        >
          File issue
        </Button>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<Build sx={{ fontSize: 18 }} />}
          disabled={action !== null}
          onClick={() => open("propose-fix")}
        >
          Propose fix
        </Button>
      </Stack>

      {url && (
        <Alert severity="success" sx={{ mt: 1 }}>
          Opened:{" "}
          <Link href={url} target="_blank" rel="noopener">
            {url}
          </Link>
        </Alert>
      )}

      <Dialog open={action !== null} onClose={busy ? undefined : close} maxWidth="md" fullWidth>
        <DialogTitle>{isFix ? "Review draft fix PR" : "Review issue"}</DialogTitle>
        <DialogContent dividers>
          {busy === "preview" && (
            <Stack direction="row" spacing={1.5} sx={{ alignItems: "center", py: 3 }}>
              <CircularProgress size={20} />
              <Typography variant="body2" color="text.secondary">
                {isFix
                  ? "Generating a fix from the failure artifacts. This can take a minute…"
                  : "Preparing the issue draft…"}
              </Typography>
            </Stack>
          )}

          {error && (
            <Alert severity="error" sx={{ mb: 2 }}>
              <Typography variant="body2">{error}</Typography>
            </Alert>
          )}

          {preview && busy !== "preview" && (
            <>
              <Typography variant="subtitle2" color="text.secondary">
                Title
              </Typography>
              <Typography variant="body1" sx={{ mb: 2, fontWeight: 600 }}>
                {preview.title}
              </Typography>
              <Typography variant="subtitle2" color="text.secondary">
                {preview.kind === "fix" ? "Description" : "Body"}
              </Typography>
              <Box
                sx={{
                  mb: 2,
                  p: 1.5,
                  bgcolor: "action.hover",
                  borderRadius: 1,
                  whiteSpace: "pre-wrap",
                  wordBreak: "break-word",
                  fontSize: "0.875rem",
                }}
              >
                {stripComments(preview.body) || "(no description)"}
              </Box>
              {preview.diff && (
                <>
                  <Typography variant="subtitle2" color="text.secondary">
                    Proposed diff
                  </Typography>
                  <Box
                    component="pre"
                    sx={{
                      mb: 2,
                      p: 1.5,
                      bgcolor: "action.hover",
                      borderRadius: 1,
                      overflowX: "auto",
                      maxHeight: 320,
                      fontSize: "0.8rem",
                      m: 0,
                    }}
                  >
                    {preview.diff}
                  </Box>
                </>
              )}
              <TextField
                label="Refine this draft with a prompt (optional)"
                placeholder={
                  isFix
                    ? "e.g. patch the kustomize base instead of the rendered template"
                    : "e.g. mention this also affects the IPv6 flavor"
                }
                fullWidth
                multiline
                minRows={2}
                size="small"
                value={instruction}
                disabled={busy !== null}
                onChange={(e) => setInstruction(e.target.value)}
                sx={{ mt: 1 }}
              />
              <Button
                size="small"
                sx={{ mt: 1 }}
                startIcon={busy === "refine" ? <CircularProgress size={14} color="inherit" /> : undefined}
                disabled={busy !== null || instruction.trim() === "" || action === null}
                onClick={() => action && loadPreview(action, true)}
              >
                {busy === "refine" ? "Regenerating…" : "Regenerate with prompt"}
              </Button>
            </>
          )}
        </DialogContent>
        <DialogActions>
          <Button onClick={close} disabled={busy !== null}>
            Cancel
          </Button>
          <Button
            variant="contained"
            color="warning"
            disabled={busy !== null || !preview}
            startIcon={busy === "confirm" ? <CircularProgress size={16} color="inherit" /> : undefined}
            onClick={confirm}
          >
            {isFix ? "Open draft PR" : "File issue"}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
