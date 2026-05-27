package ai

// ResponseFormatFooter is appended to every system prompt and pins the JSON
// schema the Go code unmarshals. Consumer prompts must not add their own
// schema; if the model returns anything other than this shape, the fetcher
// falls back to a Medium-severity placeholder.
const ResponseFormatFooter = `## Response Format

Always respond with a single JSON object matching this schema:

{
  "summary":        "1-2 sentence headline derived from root_cause",
  "is_transient":   true | false,
  "root_cause":     "the specific error found in evidence, with quoted log lines",
  "severity":       "Critical" | "High" | "Medium" | "Low",
  "suggested_fix":  "exact fix with file paths and changes needed",
  "relevant_files": ["file1.go", "file2.yaml"]
}

Set is_transient=true only when the root cause is a known transient infra
issue (throttling, quota exhaustion, intermittent DNS, image-pull backoff,
disk pressure, etcd leader election) rather than a real bug in the code
under test. When in doubt, set is_transient=false so the failure stays
visible.`
