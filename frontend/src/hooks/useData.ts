import { useState, useEffect } from "react";
import type { Dashboard, FlakinessReport, JobDetail, ResolvedState, SearchIndex } from "../types/dashboard";
import { jobDataFilename } from "../lib/utils";

const DATA_BASE =
  import.meta.env.VITE_DATA_URL ?? `${import.meta.env.BASE_URL}data`;

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function useJSON<T>(path: string | null) {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(path !== null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setData(null);
    setError(null);
    setLoading(path !== null);
    if (path === null) return;

    fetch(`${DATA_BASE}/${path}`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json() as Promise<T>;
      })
      .then((value) => {
        if (!cancelled) setData(value);
      })
      .catch((error: unknown) => {
        if (!cancelled) setError(errorMessage(error));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [path]);

  return { data, loading, error };
}

export function useDashboard() {
  return useJSON<Dashboard>("dashboard.json");
}

export function useFlakinessReport() {
  return useJSON<FlakinessReport>("flakiness.json");
}

export function useJobDetail(jobName: string | undefined) {
  return useJSON<JobDetail>(jobName ? `jobs/${jobDataFilename(jobName)}` : null);
}

export function useSearchIndex() {
  return useJSON<SearchIndex>("search-index.json");
}

export function useResolved() {
  const [data, setData] = useState<ResolvedState>({ resolved: {} });
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    fetch(`${DATA_BASE}/resolved.json`, { cache: "no-store" })
      // A missing file (static mode, or nothing resolved yet) is normal: treat
      // it as an empty set rather than an error.
      .then((r) => (r.ok ? r.json() : { resolved: {} }))
      .then((d: ResolvedState) => {
        if (!cancelled) setData(d?.resolved ? d : { resolved: {} });
      })
      .catch(() => {
        if (!cancelled) setData({ resolved: {} });
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [nonce]);

  return { data, loading, refetch: () => setNonce((n) => n + 1) };
}
