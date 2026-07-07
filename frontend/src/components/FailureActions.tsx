import { useEffect, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { BugReport, Build, GitHub, Logout } from "@mui/icons-material";
import { useCapabilities } from "../hooks/useCapabilities";

type Action = "create-issue" | "propose-fix";

interface Result {
  url?: string;
  error?: string;
}

const API_BASE = import.meta.env.BASE_URL;

// FailureActions renders admin write buttons for one systemic pattern. It shows
// only in server mode when the actions capability is present. Auth is handled
// by the server: in oauth mode the admin signs in with GitHub (no token in the
// browser); in proxy mode an upstream SSO proxy authenticates the request.
export function FailureActions({ failureID }: { failureID: string }) {
  const { features, auth } = useCapabilities();
  // login: undefined = unknown/loading, null = signed out, string = signed in.
  const [login, setLogin] = useState<string | null | undefined>(undefined);
  const [busy, setBusy] = useState<Action | null>(null);
  const [result, setResult] = useState<Result | null>(null);

  const oauth = auth?.mode === "oauth";

  useEffect(() => {
    if (!features.actions) return;
    // proxy mode: the proxy authenticates every request, so assume authorized.
    if (!oauth) {
      setLogin("");
      return;
    }
    fetch(`${API_BASE}api/auth/user`, { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : null))
      .then((u: { login: string } | null) => setLogin(u ? u.login : null))
      .catch(() => setLogin(null));
  }, [features.actions, oauth]);

  if (!features.actions || !failureID) {
    return null;
  }

  async function run(action: Action) {
    setBusy(action);
    setResult(null);
    try {
      const res = await fetch(
        `${API_BASE}api/failures/${encodeURIComponent(failureID)}/${action}`,
        { method: "POST", credentials: "same-origin" },
      );
      if (res.status === 401) {
        setLogin(null);
        throw new Error("Please sign in to continue");
      }
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

  // oauth mode, signed out: offer sign-in.
  if (oauth && login === null) {
    return (
      <Button
        size="small"
        variant="outlined"
        startIcon={<GitHub sx={{ fontSize: 18 }} />}
        href={auth?.login_url ?? `${API_BASE}api/auth/login`}
      >
        Sign in to file issues or fixes
      </Button>
    );
  }

  if (login === undefined) {
    return null; // still checking auth state
  }

  async function signOut() {
    await fetch(`${API_BASE}api/auth/logout`, { method: "POST", credentials: "same-origin" });
    setLogin(null);
  }

  return (
    <Box>
      <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<BugReport sx={{ fontSize: 18 }} />}
          disabled={busy !== null}
          onClick={() => run("create-issue")}
        >
          {busy === "create-issue" ? "Filing…" : "File issue"}
        </Button>
        <Button
          size="small"
          variant="outlined"
          color="warning"
          startIcon={<Build sx={{ fontSize: 18 }} />}
          disabled={busy !== null}
          onClick={() => run("propose-fix")}
        >
          {busy === "propose-fix" ? "Drafting…" : "Propose fix"}
        </Button>
        {oauth && login && (
          <Button size="small" color="inherit" startIcon={<Logout sx={{ fontSize: 16 }} />} onClick={signOut}>
            {login}
          </Button>
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
