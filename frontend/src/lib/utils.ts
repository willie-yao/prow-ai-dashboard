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
 * Default category rules used when the manifest does not declare any.
 * Mirrors backend project.DefaultCategories so the frontend renders the
 * same generic labels and order the backend assigned.
 */
const defaultCategoryRules: CategoryRule[] = [
  { match: "conformance", id: "conformance", label: "Conformance" },
  { match: "capi-e2e", id: "capi-e2e", label: "CAPI E2E" },
  { match: "upgrade", id: "upgrade", label: "Upgrade" },
  { match: "coverage", id: "coverage", label: "Coverage" },
  { match: "scalability", id: "scalability", label: "Scalability" },
  { match: "e2e", id: "e2e", label: "E2E" },
];

/** Returns the rules from the manifest, falling back to engine defaults. */
export function effectiveCategoryRules(
  rules: CategoryRule[] | undefined
): CategoryRule[] {
  return rules && rules.length > 0 ? rules : defaultCategoryRules;
}

/** Build a category-id -> label map, deduplicating by id. */
export function categoryLabelsFromRules(
  rules: CategoryRule[] | undefined
): Record<string, string> {
  const out: Record<string, string> = { other: "Other" };
  for (const r of effectiveCategoryRules(rules)) {
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
  for (const r of effectiveCategoryRules(rules)) {
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

/** Strip the leading "periodic-<file_prefix>" off a job name for compact display. */
export function shortJobName(name: string, filePrefix: string): string {
  if (!filePrefix) return name;
  const prefix = `periodic-${filePrefix}`;
  return name.startsWith(prefix) ? name.slice(prefix.length) : name;
}

export interface FileToUrlContext {
  buildLogUrl?: string;
  clusterArtifacts?: { machines?: { logs: Record<string, string> }[] };
  /** Source repo for repo-relative file paths in AI analyses. */
  sourceRepo?: SourceRepoRef;
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
