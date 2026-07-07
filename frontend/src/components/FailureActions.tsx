import { useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { BugReport, Build } from "@mui/icons-material";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAdminToken } from "../hooks/useAdminToken";

type Action = "create-issue" | "propose-fix";

interface Result {
  url?: string;
  error?: string;
}

// FailureActions renders admin write buttons for one systemic pattern. It shows
// only in server mode when the actions capability is present and the pattern
// has an id. The admin's token authenticates each request and attributes the
// issue/PR to them.
export function FailureActions({ failureID }: { failureID: string }) {
  const { features } = useCapabilities();
  const { token, setToken } = useAdminToken();
  const [busy, setBusy] = useState<Action | null>(null);
  const [result, setResult] = useState<Result | null>(null);

  if (!features.actions || !failureID) {
    return null;
  }

  async function run(action: Action) {
    if (!token) return;
    setBusy(action);
    setResult(null);
    try {
      const res = await fetch(
        `${import.meta.env.BASE_URL}api/failures/${encodeURIComponent(failureID)}/${action}`,
        { method: "POST", headers: { Authorization: `Bearer ${token}` } },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text.trim() || `HTTP ${res.status}`);
      }
      const body = (await res.json()) as { url: string };
      setResult({ url: body.url });
    } catch (e) {
      setResult({ error: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(null);
    }
  }

  return (
    <Box>
      <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<BugReport sx={{ fontSize: 18 }} />}
          disabled={!token || busy !== null}
          onClick={() => run("create-issue")}
        >
          {busy === "create-issue" ? "Filing…" : "File issue"}
        </Button>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<Build sx={{ fontSize: 18 }} />}
          disabled={!token || busy !== null}
          onClick={() => run("propose-fix")}
        >
          {busy === "propose-fix" ? "Drafting…" : "Propose fix"}
        </Button>
        {!token && (
          <TextField
            size="small"
            type="password"
            placeholder="GitHub token"
            onChange={(e) => setToken(e.target.value)}
            sx={{ minWidth: 200 }}
            helperText="Admin PAT, kept in memory only"
          />
        )}
      </Stack>
      {result?.url && (
        <Alert severity="success" sx={{ mt: 1 }}>
          Opened:{" "}
          <Link href={result.url} target="_blank" rel="noopener">
            {result.url}
          </Link>
        </Alert>
      )}
      {result?.error && (
        <Alert severity="error" sx={{ mt: 1 }}>
          <Typography variant="body2">{result.error}</Typography>
        </Alert>
      )}
    </Box>
  );
}
