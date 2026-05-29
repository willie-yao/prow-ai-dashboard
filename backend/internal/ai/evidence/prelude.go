// Package evidence provides project-agnostic helpers shared by every AI
// module's curator pipeline. It exposes two pieces:
//
//   - FetchLogTail: fetch a log file via GCS and return the last N lines
//     capped at a byte limit.
//   - BuildPrelude: produce the standard "failure body + stack + build log
//     tail" section that every curator prompt starts with.
//
// Modules layer their project-specific evidence (resource YAMLs, controller
// logs, etc.) on top of the prelude and append whatever analysis
// instructions they need.
//
// Lives outside the per-module packages so the universal agentic pipeline
// (the L.x stages) can share the exact same prelude shape used by curator
// for stable cache keys across modes.
package evidence

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Options controls the size of the build log tail rendered into the prelude.
// Different modules historically used different caps (generic: 200/10000,
// capi: 500/15000). Callers pass their preferred sizes; the zero value of
// either field falls back to defaults that match the generic module's
// pre-extraction behavior.
type Options struct {
	// TailLines is the number of trailing lines of the build log to
	// include. Defaults to 200 when zero.
	TailLines int
	// TailChars is the maximum byte length of the build log tail (applied
	// after line truncation, with "..." appended on overflow). Defaults to
	// 10000 when zero.
	TailChars int
	// StackChars is the maximum byte length of the failure body / stack
	// trace included in the prelude. Defaults to 5000 when zero.
	StackChars int
}

func (o Options) tailLines() int {
	if o.TailLines <= 0 {
		return 200
	}
	return o.TailLines
}

func (o Options) tailChars() int {
	if o.TailChars <= 0 {
		return 10000
	}
	return o.TailChars
}

func (o Options) stackChars() int {
	if o.StackChars <= 0 {
		return 5000
	}
	return o.StackChars
}

// BuildPrelude returns the universal opening section of a curator prompt:
// header, test name, consecutive-failure count, failure message, stack trace
// (truncated), and the build log tail. Returns a string without a trailing
// newline; callers append their own analysis instructions or per-module
// evidence sections.
//
// Errors fetching the build log are logged and ignored; the prelude is
// rendered from whatever is available.
func BuildPrelude(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int, opts Options) string {
	var sb strings.Builder
	sb.WriteString("Investigate this test failure using the data below.\n\n")
	fmt.Fprintf(&sb, "Test: %s\n", tc.Name)
	fmt.Fprintf(&sb, "Failed %d consecutive times\n\n", consecutive)
	fmt.Fprintf(&sb, "Error: %s\n", tc.FailureMessage)

	if tc.FailureBody != "" {
		fmt.Fprintf(&sb, "\nStack trace:\n%s\n", truncate(tc.FailureBody, opts.stackChars()))
	}

	if run != nil && run.BuildLogURL != "" {
		if tail := FetchLogTail(ctx, client, run.BuildLogURL, opts.tailLines(), opts.tailChars()); tail != "" {
			fmt.Fprintf(&sb, "\n=== Build Log (last %d lines) ===\n%s\n", opts.tailLines(), tail)
		}
	}

	return sb.String()
}

// FetchLogTail fetches a log file via GCS and returns the last lastN lines,
// truncated to at most maxChars bytes (with "..." appended on overflow).
// Fetch failures are logged at INFO and the function returns "".
func FetchLogTail(ctx context.Context, client *http.Client, url string, lastN int, maxChars int) string {
	data, err := gcs.FetchRaw(ctx, client, url)
	if err != nil {
		log.Printf("  ⚠ evidence: failed to fetch %s: %v", url, err)
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > lastN {
		lines = lines[len(lines)-lastN:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxChars {
		out = out[:maxChars] + "..."
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
