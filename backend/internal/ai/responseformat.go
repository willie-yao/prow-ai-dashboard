package ai

// ResponseFormatFooter is appended to every system prompt and pins the JSON
// schema the Go code unmarshals. Consumer prompts must not add their own
// schema; if the model returns anything other than this shape, the fetcher
// falls back to a Medium-severity placeholder.
//
// The wording stays tool-neutral here because both agentic and non-agentic
// (prebuilt-evidence) consumers receive this footer. Agentic-specific
// investigation strategy (drill-down, anti-punt enforcement) lives in
// agToolDocs and is appended only when tools are wired.
const ResponseFormatFooter = `## Response Format

Always respond with a single JSON object matching this schema:

{
  "summary":        "1-2 sentence headline derived from root_cause",
  "is_transient":   true | false,
  "root_cause":     "the specific error found in the available evidence, with quoted log lines and concrete file paths when present. Do NOT describe a symptom and stop; trace the chain back to the underlying cause as far as the available evidence allows.",
  "severity":       "Critical" | "High" | "Medium" | "Low",
  "suggested_fix":  "concrete remediation: the specific code change, config edit, command to run, or operational action that fixes the root_cause. Do not list diagnostic or information-gathering tasks as the fix; those belong in your analysis, not handed back to the user. If the available evidence is insufficient to determine a remediation, state that explicitly here (e.g. 'No remediation possible from available evidence: artifacts show X but not Y; would need Z') rather than disguising the gap as a TODO list.",
  "relevant_files": ["file1.go", "file2.yaml"]
}

Set is_transient=true only when the root cause is a known transient infra
issue (throttling, quota exhaustion, intermittent DNS, image-pull backoff,
disk pressure, etcd leader election) rather than a real bug in the code
under test. When in doubt, set is_transient=false so the failure stays
visible.`
