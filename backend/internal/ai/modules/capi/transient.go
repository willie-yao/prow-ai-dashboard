package capi

import "strings"

// transientPattern defines a CAPI/Azure-specific pattern for known transient
// failures that do not need AI analysis.
type transientPattern struct {
	match  func(string) bool
	reason string
}

var knownTransientPatterns = []transientPattern{
	{
		match:  func(s string) bool { return strings.Contains(s, "429") || strings.Contains(s, "throttling") || strings.Contains(s, "too many requests") },
		reason: "Azure API throttling (HTTP 429)",
	},
	{
		match:  func(s string) bool { return strings.Contains(s, "quota") && (strings.Contains(s, "exceeded") || strings.Contains(s, "limit")) },
		reason: "Azure resource quota exceeded",
	},
	{
		match: func(s string) bool {
			return strings.Contains(s, "context deadline exceeded") && (strings.Contains(s, "cleanup") || strings.Contains(s, "delete"))
		},
		reason: "Context deadline during cleanup",
	},
	{
		match: func(s string) bool {
			return strings.Contains(s, "dns") && (strings.Contains(s, "resolution") || strings.Contains(s, "lookup")) && strings.Contains(s, "failed")
		},
		reason: "DNS resolution failure",
	},
	{
		match:  func(s string) bool { return strings.Contains(s, "imagepullbackoff") },
		reason: "Image pull backoff (transient)",
	},
	{
		match:  func(s string) bool { return strings.Contains(s, "no space left on device") },
		reason: "Disk space exhausted",
	},
}

// IsKnownTransient checks if a failure message matches a known transient
// pattern. Returns the reason if transient, empty string otherwise.
func (m *Module) IsKnownTransient(failureMessage string) string {
	lower := strings.ToLower(failureMessage)
	for _, p := range knownTransientPatterns {
		if p.match(lower) {
			return p.reason
		}
	}
	return ""
}
