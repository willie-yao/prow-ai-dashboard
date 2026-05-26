import { useState, useEffect, useRef, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import Fuse from "fuse.js";
import { useSearchIndex } from "../hooks/useData";
import { shortTestName } from "../lib/utils";
import { HiMagnifyingGlass } from "react-icons/hi2";
import type { SearchEntry } from "../types/dashboard";

const JOB_PREFIX = "periodic-cluster-api-provider-azure-";

function shortJobName(name: string): string {
  return name.startsWith(JOB_PREFIX) ? name.slice(JOB_PREFIX.length) : name;
}

export function SearchBar() {
  const { data } = useSearchIndex();
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [mobileExpanded, setMobileExpanded] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  const fuse = useMemo(() => {
    if (!data?.entries) return null;
    return new Fuse(data.entries, {
      keys: ["test_name", "job_name", "tab_name"],
      threshold: 0.4,
      includeScore: true,
    });
  }, [data]);

  const results = useMemo(() => {
    if (!fuse || !query.trim()) return [];
    return fuse.search(query, { limit: 20 });
  }, [fuse, query]);

  // Group results by job
  const grouped = useMemo(() => {
    const groups = new Map<string, { item: SearchEntry; score?: number }[]>();
    for (const r of results) {
      const job = r.item.job_name;
      const list = groups.get(job);
      if (list) list.push(r);
      else groups.set(job, [r]);
    }
    return groups;
  }, [results]);

  // Cmd+K / Ctrl+K shortcut
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setMobileExpanded(true);
        inputRef.current?.focus();
      }
    }
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, []);

  // Close on click outside
  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setOpen(false);
        setMobileExpanded(false);
      }
    }
    document.addEventListener("mousedown", handleClick);
    return () => document.removeEventListener("mousedown", handleClick);
  }, []);

  // Close on Escape
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") {
        setOpen(false);
        setMobileExpanded(false);
        inputRef.current?.blur();
      }
    }
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, []);

  function handleSelect(entry: SearchEntry) {
    if (entry.kind === "job") {
      navigate(`/job/${encodeURIComponent(entry.job_name)}`);
    } else {
      navigate(`/job/${encodeURIComponent(entry.job_name)}/test/${encodeURIComponent(entry.test_name)}`);
    }
    setOpen(false);
    setQuery("");
    setMobileExpanded(false);
  }

  return (
    <div ref={containerRef} className="relative flex items-center">
      {/* Mobile: magnifying glass toggle */}
      <button
        type="button"
        className="md:hidden flex items-center justify-center p-1.5 text-on-surface-variant hover:text-on-surface transition-colors"
        onClick={() => {
          setMobileExpanded(true);
          setTimeout(() => inputRef.current?.focus(), 50);
        }}
        aria-label="Search"
      >
        <HiMagnifyingGlass className="h-5 w-5" />
      </button>

      {/* Desktop input (always visible) / Mobile input (expandable) */}
      <div className={`${mobileExpanded ? "fixed inset-x-0 top-0 z-[60] flex items-center gap-2 bg-surface-container px-4 h-16 border-b border-outline-variant" : "hidden md:block"}`}>
        <div className="relative w-full md:w-64 lg:w-80">
          <HiMagnifyingGlass className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 h-4 w-4 text-on-surface-variant" />
          <input
            ref={inputRef}
            type="text"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setOpen(true);
            }}
            onFocus={() => setOpen(true)}
            placeholder="Search tests…"
            className="w-full bg-surface-container border border-outline-variant rounded-lg pl-8 pr-12 py-1.5 text-sm text-on-surface placeholder:text-on-surface-variant/60 focus:outline-none focus:ring-1 focus:ring-primary"
          />
          <kbd className="pointer-events-none absolute right-2.5 top-1/2 -translate-y-1/2 hidden sm:inline-flex items-center gap-0.5 rounded border border-outline-variant bg-surface px-1.5 py-0.5 font-label text-[10px] text-on-surface-variant">
            ⌘K
          </kbd>
        </div>
        {mobileExpanded && (
          <button
            type="button"
            className="md:hidden shrink-0 text-sm text-on-surface-variant hover:text-on-surface"
            onClick={() => {
              setMobileExpanded(false);
              setOpen(false);
              setQuery("");
            }}
          >
            Cancel
          </button>
        )}
      </div>

      {/* Dropdown results */}
      {open && query.trim() && (
        <div className={`absolute top-full mt-2 glass rounded-lg border border-outline-variant shadow-xl overflow-hidden ${mobileExpanded ? "fixed left-4 right-4 top-16 mt-0 z-[60]" : "right-0 md:left-0 w-[28rem]"}`}>
          <div className="max-h-[400px] overflow-y-auto">
            {results.length === 0 ? (
              <div className="px-4 py-6 text-center text-sm text-on-surface-variant">
                No results
              </div>
            ) : (
              Array.from(grouped.entries()).map(([jobName, items]) => (
                <div key={jobName}>
                  <div className="sticky top-0 bg-surface-container px-3 py-1.5 font-label text-xs font-medium text-on-surface-variant border-b border-outline-variant">
                    {shortJobName(jobName)}
                  </div>
                  {items.map((r) => (
                    <button
                      key={`${r.item.kind}:${r.item.job_name}/${r.item.test_name}`}
                      type="button"
                      onClick={() => handleSelect(r.item)}
                      className="flex w-full items-center gap-3 px-3 py-2 text-sm text-on-surface hover:bg-surface-container-high transition-colors text-left"
                    >
                      {r.item.kind === "job" ? (
                        <>
                          <span className="inline-block h-2 w-2 shrink-0 rounded-full bg-primary" />
                          <span className="min-w-0 flex-1 truncate font-medium">
                            {r.item.tab_name || shortJobName(r.item.job_name)}
                          </span>
                          <span className="shrink-0 font-label text-xs text-on-surface-variant">
                            {r.item.branch}
                          </span>
                        </>
                      ) : (
                        <>
                          <span
                            className={`inline-block h-2 w-2 shrink-0 rounded-full ${
                              r.item.status === "passed" ? "bg-secondary" : "bg-error"
                            }`}
                          />
                          <span className="min-w-0 flex-1 truncate">
                            {shortTestName(r.item.test_name)}
                          </span>
                          {r.item.fail_rate > 0 && (
                            <span className="shrink-0 rounded-full bg-error/20 px-2 py-0.5 text-xs font-medium text-error">
                              {Math.round(r.item.fail_rate * 100)}%
                            </span>
                          )}
                        </>
                      )}
                    </button>
                  ))}
                </div>
              ))
            )}
          </div>
        </div>
      )}
    </div>
  );
}
