import { useEffect, useState, type ReactNode } from "react";
import { ManifestContext } from "../hooks/useManifest";
import type { Manifest } from "../types/manifest";

export function ManifestProvider({ children }: { children: ReactNode }) {
  const [manifest, setManifest] = useState<Manifest | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const url = `${import.meta.env.BASE_URL}data/manifest.json`;
    fetch(url)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json() as Promise<Manifest>;
      })
      .then((m) => {
        setManifest(m);
        if (m.branding?.title) {
          document.title = m.branding.title;
        }
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  if (error) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-background text-on-background p-6">
        <div className="max-w-md text-center">
          <h1 className="text-lg font-semibold mb-2">Failed to load dashboard config</h1>
          <p className="text-sm text-on-surface-variant">{error}</p>
          <p className="text-sm text-on-surface-variant mt-2">
            Expected file: <code>data/manifest.json</code>
          </p>
        </div>
      </div>
    );
  }

  if (!manifest) {
    return null;
  }

  return <ManifestContext.Provider value={manifest}>{children}</ManifestContext.Provider>;
}
