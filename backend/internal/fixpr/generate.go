package fixpr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Completer is the subset of the AI client this package needs (an interface so
// generation is unit-testable).
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// edit is one anchored search/replace within a single file.
type edit struct {
	File string `json:"file"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// proposedFix is a validated, ready-to-commit change.
type proposedFix struct {
	// files maps repo path to the full new content (only changed files).
	files map[string]string
	// diff is a human-readable rendering for the PR body.
	diff string
	// rationale is the model's short explanation of the change.
	rationale string
}

// readFunc fetches a source file's content by path (bound to a repo).
type readFunc func(ctx context.Context, path string) (content string, found bool, err error)

// generateFix turns a systemic pattern into a validated minimal edit: locate the
// target file(s), fetch their current content, ask for anchored edits, and apply
// them (exact-match-once). Any step that can't be grounded returns an error so
// the caller drops the fix rather than proposing something unsafe.
func generateFix(ctx context.Context, c Completer, read readFunc, p models.PatternAnalysis, maxFiles int) (*proposedFix, error) {
	files, err := locateTargets(ctx, c, p, maxFiles)
	if err != nil {
		return nil, err
	}

	contents := make(map[string]string, len(files))
	for _, f := range files {
		body, found, err := read(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		if !found {
			return nil, fmt.Errorf("model named a file that does not exist in the source repo: %s", f)
		}
		contents[f] = body
	}

	edits, rationale, err := proposeEdits(ctx, c, p, contents)
	if err != nil {
		return nil, err
	}
	changed, err := applyEdits(contents, edits, maxFiles)
	if err != nil {
		return nil, err
	}
	return &proposedFix{
		files:     changed,
		diff:      renderDiff(contents, edits),
		rationale: rationale,
	}, nil
}

const locateSystemPrompt = `You identify which source files a known CI failure fix should touch. Given a recurring failure pattern and its suggested fix, name the smallest set of repository file paths (relative to the repo root) that must change. Prefer configuration, template, or manifest files over broad code changes. Answer only with one-line JSON, no prose.`

// locateTargets asks the model which file path(s) the fix should touch, capped
// at maxFiles.
func locateTargets(ctx context.Context, c Completer, p models.PatternAnalysis, maxFiles int) ([]string, error) {
	user := fmt.Sprintf(`Recurring failure pattern:
- subject: %s
- shared_root_cause: %s
- suggested_fix: %s
- summary: %s

Name the repository file path(s) the fix should change (relative to repo root),
fewest first. Answer with one line of JSON:
{"files": ["path/one", "path/two"]}`,
		p.Subject, oneLine(p.SharedRootCause), oneLine(p.SuggestedFix), oneLine(p.Summary))

	out, err := c.Complete(ctx, locateSystemPrompt, user)
	if err != nil {
		return nil, err
	}
	var v struct {
		Files []string `json:"files"`
	}
	if err := parseJSONObject(out, &v); err != nil {
		return nil, fmt.Errorf("locate response: %w", err)
	}
	files := dedupeNonEmpty(v.Files)
	if len(files) == 0 {
		return nil, fmt.Errorf("model named no target files")
	}
	if len(files) > maxFiles {
		return nil, fmt.Errorf("model named %d files, exceeds max_files %d", len(files), maxFiles)
	}
	return files, nil
}

const editSystemPrompt = `You propose a MINIMAL fix as anchored search/replace edits. You are given the current content of one or more files and a known failure root cause. For each change, return an "old" snippet copied VERBATIM from the file (long enough to be unique within that file) and the "new" snippet to replace it with. Do not reformat unrelated lines. Make the smallest change that fixes the root cause. Answer only with JSON, no prose.`

// proposeEdits asks the model for anchored edits given the real file contents.
func proposeEdits(ctx context.Context, c Completer, p models.PatternAnalysis, contents map[string]string) ([]edit, string, error) {
	var sb strings.Builder
	for _, path := range sortedKeys(contents) {
		fmt.Fprintf(&sb, "=== FILE: %s ===\n%s\n", path, contents[path])
	}
	user := fmt.Sprintf(`Root cause: %s
Suggested fix: %s

Current file content(s):
%s
Return anchored edits as JSON:
{"rationale": "<one sentence>", "edits": [{"file": "<path>", "old": "<verbatim snippet>", "new": "<replacement>"}]}
The "old" snippet for each edit MUST appear exactly once in that file.`,
		oneLine(p.SharedRootCause), oneLine(p.SuggestedFix), sb.String())

	out, err := c.Complete(ctx, editSystemPrompt, user)
	if err != nil {
		return nil, "", err
	}
	var v struct {
		Rationale string `json:"rationale"`
		Edits     []edit `json:"edits"`
	}
	if err := parseJSONObject(out, &v); err != nil {
		return nil, "", fmt.Errorf("edit response: %w", err)
	}
	if len(v.Edits) == 0 {
		return nil, "", fmt.Errorf("model proposed no edits")
	}
	return v.Edits, strings.TrimSpace(v.Rationale), nil
}

// Edit-scope caps keep a "fix" a minimal change: a model can't satisfy
// exact-match-once by anchoring on (and replacing) the whole file or a huge
// block.
const (
	maxEdits             = 20   // anchored edits per fix
	maxEditBytes         = 8192 // per old/new snippet
	maxChangedLinesTotal = 120  // summed old+new lines across all edits
)

// applyEdits applies each anchored edit, requiring its "old" snippet to match
// exactly once in the current file content, and enforces minimal-change scope
// caps. Returns only the changed files.
func applyEdits(orig map[string]string, edits []edit, maxFiles int) (map[string]string, error) {
	if len(edits) > maxEdits {
		return nil, fmt.Errorf("fix proposes %d edits, exceeds the cap of %d", len(edits), maxEdits)
	}
	work := make(map[string]string, len(orig))
	for k, v := range orig {
		work[k] = v
	}
	touched := map[string]bool{}
	changedLines := 0
	for _, e := range edits {
		cur, ok := work[e.File]
		if !ok {
			return nil, fmt.Errorf("edit references a file that was not fetched: %s", e.File)
		}
		if e.Old == e.New {
			continue
		}
		if e.Old == "" {
			return nil, fmt.Errorf("edit for %s has an empty anchor", e.File)
		}
		if len(e.Old) > maxEditBytes || len(e.New) > maxEditBytes {
			return nil, fmt.Errorf("edit for %s is too large (snippet exceeds %d bytes); fixes must be minimal", e.File, maxEditBytes)
		}
		// Reject a whole-file rewrite: the anchor must be a fragment, not the
		// entire file content.
		if strings.TrimSpace(e.Old) == strings.TrimSpace(cur) {
			return nil, fmt.Errorf("edit for %s rewrites the whole file; fixes must be minimal anchored changes", e.File)
		}
		switch strings.Count(cur, e.Old) {
		case 1:
			work[e.File] = strings.Replace(cur, e.Old, e.New, 1)
			touched[e.File] = true
			changedLines += lineCount(e.Old) + lineCount(e.New)
		case 0:
			return nil, fmt.Errorf("edit anchor not found in %s (the model's snippet did not match the file)", e.File)
		default:
			return nil, fmt.Errorf("edit anchor is ambiguous in %s (matches more than once)", e.File)
		}
	}
	if len(touched) == 0 {
		return nil, fmt.Errorf("the proposed edits made no effective change")
	}
	if changedLines > maxChangedLinesTotal {
		return nil, fmt.Errorf("fix changes ~%d lines, exceeds the cap of %d; fixes must be minimal", changedLines, maxChangedLinesTotal)
	}
	if len(touched) > maxFiles {
		return nil, fmt.Errorf("fix touches %d files, exceeds max_files %d", len(touched), maxFiles)
	}
	changed := make(map[string]string, len(touched))
	for f := range touched {
		changed[f] = work[f]
	}
	return changed, nil
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// renderDiff produces a simple, human-readable per-edit diff for the PR body.
func renderDiff(orig map[string]string, edits []edit) string {
	var sb strings.Builder
	for _, e := range edits {
		if e.Old == e.New {
			continue
		}
		fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", e.File, e.File)
		for _, line := range strings.Split(e.Old, "\n") {
			fmt.Fprintf(&sb, "- %s\n", line)
		}
		for _, line := range strings.Split(e.New, "\n") {
			fmt.Fprintf(&sb, "+ %s\n", line)
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// parseJSONObject extracts the first {...} object from s and unmarshals it,
// tolerating prose or code fences around the JSON.
func parseJSONObject(s string, v any) error {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return fmt.Errorf("no JSON object in response")
	}
	return json.Unmarshal([]byte(s[start:end+1]), v)
}

func dedupeNonEmpty(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
