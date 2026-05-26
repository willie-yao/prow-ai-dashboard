import type { JobSummary } from "../types/dashboard";

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

export const categoryLabels: Record<string, string> = {
  "capz-e2e": "CAPZ E2E",
  "aks-e2e": "AKS E2E",
  upgrade: "Upgrade",
  "capi-e2e": "CAPI E2E",
  conformance: "Conformance",
  coverage: "Coverage",
  scalability: "Scalability",
  other: "Other",
};

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

const CAPZ_REPO = "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/";

/** Turn a file path from AI analysis into a URL if possible. */
export function fileToUrl(
  filePath: string,
  context?: { buildLogUrl?: string; clusterArtifacts?: { machines?: { logs: Record<string, string> }[] } }
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

  // Only link repo files that look like they belong to the CAPZ repo
  const capzPrefixes = /^(test|templates|pkg|scripts|api|exp|hack|config|deploy|cloud|cmd|util|feature)\//;
  if (/\.(go|yaml|yml|sh|json|tpl|md)$/.test(clean) && capzPrefixes.test(clean)) {
    return CAPZ_REPO + clean;
  }
  return null;
}

/** Returns sort priority: 0 = GCS artifact, 1 = CAPZ repo, 2 = other/unlinked */
export function fileSortKey(
  filePath: string,
  context?: Parameters<typeof fileToUrl>[1]
): number {
  const url = fileToUrl(filePath, context);
  if (!url) return 2;
  if (url.includes("storage.googleapis.com") || url.includes("gcsweb.k8s.io")) return 0;
  if (url.includes("github.com")) return 1;
  return 2;
}
