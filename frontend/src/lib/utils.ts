import type { JobSummary } from "../types/dashboard";
import type { CategoryRule } from "../types/manifest";

export function statusColor(status: string): string {
  switch (status) {
    case "PASSING":
      return "text-secondary";
    case "FAILING":
      return "text-error";
    case "FLAKY":
      return "text-tertiary";
    default:
      return "text-on-surface-variant";
  }
}

export function statusBg(status: string): string {
  switch (status) {
    case "PASSING":
      return "bg-secondary";
    case "FAILING":
      return "bg-error";
    case "FLAKY":
      return "bg-tertiary";
    default:
      return "bg-on-surface-variant";
  }
}

export function dotColor(passed: boolean, result?: string): string {
  if (result === "PENDING") return "bg-tertiary";
  return passed ? "bg-secondary" : "bg-error";
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  const h = Math.floor(seconds / 3600);
  const m = Math.round((seconds % 3600) / 60);
  return `${h}h ${m}m`;
}

export function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const hours = Math.floor(diff / 3600000);
  if (hours < 1) return "just now";
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "1 day ago";
  return `${days} days ago`;
}

export function formatPercent(rate: number): string {
  return `${Math.round(rate * 100)}%`;
}

export function groupByCategory(
  jobs: JobSummary[]
): Record<string, JobSummary[]> {
  const groups: Record<string, JobSummary[]> = {};
  for (const job of jobs) {
    const cat = job.category || "other";
    (groups[cat] ??= []).push(job);
  }
  return groups;
}

export function groupByBranch(
  jobs: JobSummary[]
): Record<string, JobSummary[]> {
  const groups: Record<string, JobSummary[]> = {};
  for (const job of jobs) {
    const branch = job.branch || "unknown";
    (groups[branch] ??= []).push(job);
  }
  return groups;
}

/**
 * Build a category-id -> label map, deduplicating by id. Returns just the
 * implicit "Other" entry when the manifest declares no rules.
 */
export function categoryLabelsFromRules(
  rules: CategoryRule[] | undefined
): Record<string, string> {
  const out: Record<string, string> = { other: "Other" };
  for (const r of rules ?? []) {
    if (r.id && !(r.id in out)) out[r.id] = r.label || r.id;
  }
  return out;
}

/** Build an ordered list of category ids matching the manifest's rule order. */
export function categoryOrderFromRules(
  rules: CategoryRule[] | undefined
): string[] {
  const seen = new Set<string>();
  const order: string[] = [];
  for (const r of rules ?? []) {
    if (r.id && !seen.has(r.id)) {
      seen.add(r.id);
      order.push(r.id);
    }
  }
  order.push("other");
  return order;
}

/**
 * Build an ordered list of category ids for display. When the manifest
 * declares `category_display_order`, those ids come first (in their declared
 * order), followed by any remaining ids in rule-declaration order, with
 * "other" implicitly last.
 */
export function categoryDisplayOrder(
  rules: CategoryRule[] | undefined,
  explicit: string[] | undefined
): string[] {
  const ruleOrder = categoryOrderFromRules(rules);
  if (!explicit || explicit.length === 0) return ruleOrder;
  const seen = new Set<string>();
  const out: string[] = [];
  for (const id of explicit) {
    if (id && !seen.has(id)) {
      seen.add(id);
      out.push(id);
    }
  }
  for (const id of ruleOrder) {
    if (!seen.has(id)) {
      seen.add(id);
      out.push(id);
    }
  }
  return out;
}

/** Split text with inline numbered/bulleted steps into separate lines. */
export function formatSteps(text: string): string {
  // Insert newlines before numbered steps: "2." "3." etc (not "1." at start of text)
  let result = text.replace(/\s+(\d+)\.\s/g, (match, num) => {
    return Number(num) > 1 ? `\n${num}. ` : match;
  });
  // Insert newlines before parenthesized numbers: "(1)" "(2)" etc when preceded by text
  result = result.replace(/([.!?:])?\s+\((\d+)\)\s/g, (_match, punct, num) => {
    return `${punct || ""}\n(${num}) `;
  });
  // Insert newlines before bullet markers
  result = result.replace(/\s+[-•]\s/g, "\n• ");
  return result;
}

/** Strip common verbose prefixes from test names for compact display. */
export function shortTestName(name: string): string {
  let short = name
    .replace(/^\[It\]\s+/, "")
    .replace(/^Workload cluster creation\s+Creating\s+(a\s+)?/, "")
    .replace(/^Running the Cluster API E2E tests\s+/, "CAPI: ")
    .replace(/^Conformance Tests\s+/, "Conformance: ")
    .replace(/^Running\s+/, "");
  // Capitalize first letter
  return short.charAt(0).toUpperCase() + short.slice(1);
}

export interface SourceRepoRef {
  owner: string;
  name: string;
}

/** Strip the derived short-name prefix off a job name for compact display. */
export function shortJobName(name: string, shortNamePrefix: string): string {
  if (!shortNamePrefix) return name;
  return name.startsWith(shortNamePrefix) ? name.slice(shortNamePrefix.length) : name;
}

export interface FileToUrlContext {
  buildLogUrl?: string;
  clusterArtifacts?: { machines?: { logs: Record<string, string> }[] };
  /** Source repo for repo-relative file paths in AI analyses. */
  sourceRepo?: SourceRepoRef;
  /**
   * Web URL root for the current build (e.g.
   * `https://gcsweb.k8s.io/gcs/<bucket>/logs/<job>/<id>/`). When the
   * cited path does not match a known artifact via other heuristics, we
   * resolve it as `<webUrl>/<path>` (prepending `artifacts/` when the
   * path does not already start with it). Lets agent-cited artifact
   * paths render as clickable links without depending on the
   * `cluster_artifacts.machines` index.
   */
  webUrl?: string;
}

/** Turn a file path from AI analysis into a URL if possible. */
export function fileToUrl(
  filePath: string,
  context?: FileToUrlContext
): string | null {
  const clean = filePath.replace(/\s*\(.*\)$/, "").trim();
  const lower = clean.toLowerCase();

  // Match artifact/log references to actual URLs from context
  if (context) {
    if (/build-log/i.test(lower) && context.buildLogUrl) {
      return context.buildLogUrl;
    }
    const machines = context.clusterArtifacts?.machines;
    if (machines && machines.length > 0) {
      const logs = machines[0].logs;
      if (/cloud-init-output/i.test(lower) && logs["cloud-init-output.log"]) {
        return logs["cloud-init-output.log"];
      }
      if (/cloud-init\.log/i.test(lower) && logs["cloud-init.log"]) {
        return logs["cloud-init.log"];
      }
      if (/boot\.log/i.test(lower) && logs["boot.log"]) {
        return logs["boot.log"];
      }
      if (/kubelet\.log/i.test(lower) && logs["kubelet.log"]) {
        return logs["kubelet.log"];
      }
      if (/kube-apiserver/i.test(lower) && logs["kube-apiserver.log"]) {
        return logs["kube-apiserver.log"];
      }
      if (/journal\.log/i.test(lower) && logs["journal.log"]) {
        return logs["journal.log"];
      }
      if (/containerd/i.test(lower) && logs["containerd.log"]) {
        return logs["containerd.log"];
      }
    }
  }

  // Skip descriptive text that isn't a real path
  if (/\.status\.|portal\.azure|azuremachine.*field|controller.*log/i.test(clean)) {
    return null;
  }

  // Link plausible repo-relative source paths to the configured source repo.
  const repoPathPrefixes = /^(test|templates|pkg|scripts|api|exp|hack|config|deploy|cloud|cmd|util|feature)\//;
  if (/\.(go|yaml|yml|sh|json|tpl|md)$/.test(clean) && repoPathPrefixes.test(clean) && context?.sourceRepo) {
    const { owner, name } = context.sourceRepo;
    return `https://github.com/${owner}/${name}/blob/main/${clean}`;
  }

  // Fallback: resolve plausible GCS artifact paths against the build's
  // web URL. Catches agent-cited paths like
  // "artifacts/clusters/<name>/machines/<vm>/kubelet.log" or
  // "clusters/<name>/resources/Foo/bar.yaml" that the curator-era
  // heuristics above could not map.
  if (context?.webUrl) {
    let path = clean.replace(/^\/+/, "");
    const looksLikeArtifact =
      /\.(log|yaml|yml|json|xml|txt|out|conf)$/i.test(path) && path.includes("/");
    if (looksLikeArtifact) {
      if (!path.startsWith("artifacts/")) {
        path = `artifacts/${path}`;
      }
      const base = context.webUrl.replace(/\/+$/, "");
      return `${base}/${path}`;
    }
  }

  return null;
}

/** Returns sort priority: 0 = GCS artifact, 1 = source repo, 2 = other/unlinked */
export function fileSortKey(
  filePath: string,
  context?: FileToUrlContext
): number {
  const url = fileToUrl(filePath, context);
  if (!url) return 2;
  if (url.includes("storage.googleapis.com") || url.includes("gcsweb.k8s.io")) return 0;
  if (url.includes("github.com")) return 1;
  return 2;
}
