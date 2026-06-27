package ai

// ResponseFormatFooter is appended to every system prompt and pins the JSON
// schema the Go code unmarshals. Consumer prompts must not add their own
// schema; if the model returns anything other than this shape, the fetcher
// falls back to a Medium-severity placeholder.
//
// The wording stays tool-neutral here. Agentic-specific investigation strategy,
// including drill-down and anti-punt enforcement, lives in agToolDocs and is
// appended only when tools are wired.
const ResponseFormatFooter = `## Response Format

Always respond with a single JSON object matching this schema:

{
  "summary":        "1-2 sentence headline derived from root_cause",
  "is_transient":   true | false,
  "root_cause":     "Full causal chain from observed symptom back to the underlying cause as far as the available evidence allows. At least 3-5 sentences. Quote the exact log line(s) that prove each link in the chain and cite the artifact path each quote came from. Do NOT stop at the first error message you see; trace the chain back to the underlying cause through every layer the evidence supports. If two distinct artifacts independently support the same conclusion, cite both.",
  "severity":       "Critical" | "High" | "Medium" | "Low",
  "suggested_fix":  "Provide a concrete remediation. Name the specific file (with line number where applicable), the exact edit or command, and one verification step the operator can run to confirm the fix worked. Do not list diagnostic or information-gathering tasks as the fix; those belong in your analysis, not handed back to the user. If the available evidence is insufficient to determine a remediation, say so explicitly here, starting with the exact phrase 'No remediation possible from available evidence:' followed by which evidence is missing in your own words, rather than disguising the gap as a TODO list.",
  "relevant_files": ["file1.go", "file2.yaml"]
}

Set is_transient=true when the root cause is a transient infrastructure
issue (throttling, quota exhaustion, intermittent DNS, image-pull backoff,
disk pressure, etcd leader election, API server or node still coming up)
rather than a real bug in the code under test. If the failure matches a
transient class the project-specific knowledge calls out, set
is_transient=true even if you could keep digging for a deeper chain;
infrastructure flake is not a code bug. Reserve is_transient=false for
failures that are a genuine defect or that match no known transient class.`
