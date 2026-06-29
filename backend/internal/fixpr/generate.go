package fixpr

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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

// generateFix turns a pattern into a validated minimal edit: pick target
// file(s) from the repo's real tree, fetch them, propose anchored edits, and
// apply them (exact-match-once). Any ungrounded step returns an error so the
// caller drops the fix.
func generateFix(ctx context.Context, c Completer, source sourceReader, owner, repo, ref string, p models.PatternAnalysis, maxFiles int) (*proposedFix, error) {
	tree, err := source.ListTree(ctx, owner, repo, ref)
	if err != nil {
		return nil, fmt.Errorf("listing %s/%s tree: %w", owner, repo, err)
	}
	candidates := rankCandidates(tree, p)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidate files in the repo matched the failure")
	}

	files, err := locateTargets(ctx, c, p, candidates, maxFiles)
	if err != nil {
		return nil, err
	}

	contents := make(map[string]string, len(files))
	for _, f := range files {
		body, found, err := source.FileContent(ctx, owner, repo, ref, f)
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
	if err := validateSyntax(changed); err != nil {
		return nil, err
	}
	return &proposedFix{
		files:     changed,
		diff:      renderDiff(contents, edits),
		rationale: rationale,
	}, nil
}

const locateSystemPrompt = `You identify which source files a known CI failure fix should touch. You are given a candidate list of REAL repository file paths. Choose the smallest set of paths FROM THAT LIST that must change to fix the failure, preferring configuration, template, or manifest files. You MUST only return paths that appear verbatim in the candidate list; if none fit, return an empty list. Answer only with one-line JSON, no prose.`

// locateTargets has the model pick target file(s) from the candidate paths,
// capped at maxFiles. Picks outside the candidate set are rejected.
func locateTargets(ctx context.Context, c Completer, p models.PatternAnalysis, candidates []string, maxFiles int) ([]string, error) {
	user := fmt.Sprintf(`Recurring failure pattern:
- subject: %s
- shared_root_cause: %s
- suggested_fix: %s
- summary: %s

Candidate repository files (choose only from these exact paths):
%s

Choose the file path(s) to change, fewest first, only from the list above.
Answer with one line of JSON:
{"files": ["path/one", "path/two"]}`,
		p.Subject, oneLine(p.SharedRootCause), oneLine(p.SuggestedFix), oneLine(p.Summary),
		strings.Join(candidates, "\n"))

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
		return nil, fmt.Errorf("no candidate file fit the failure")
	}
	if len(files) > maxFiles {
		return nil, fmt.Errorf("model named %d files, exceeds max_files %d", len(files), maxFiles)
	}
	candSet := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candSet[c] = true
	}
	for _, f := range files {
		if !candSet[f] {
			return nil, fmt.Errorf("model named %q, which is not a real repo file", f)
		}
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

// Scope caps that keep a fix minimal (so exact-match-once can't be abused with a
// whole-file or oversized anchor).
const (
	maxEdits             = 20   // anchored edits per fix
	maxEditBytes         = 8192 // per old/new snippet
	maxChangedLinesTotal = 120  // summed old+new lines across all edits
)

// applyEdits applies each anchored edit, requiring its "old" snippet to match
// exactly once, within the scope caps. Returns only the changed files.
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
		// Reject a whole-file rewrite; the anchor must be a fragment.
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

// validateSyntax parse-checks each changed file by extension, so an edit that
// leaves the file syntactically broken is dropped rather than proposed.
// Extensions without a validator are skipped.
func validateSyntax(files map[string]string) error {
	for path, content := range files {
		switch ext(strings.ToLower(path)) {
		case ".go":
			if _, err := parser.ParseFile(token.NewFileSet(), path, content, parser.SkipObjectResolution); err != nil {
				return fmt.Errorf("edited %s is not valid Go: %w", path, err)
			}
		case ".yaml", ".yml":
			dec := yaml.NewDecoder(strings.NewReader(content))
			for {
				var doc any
				err := dec.Decode(&doc)
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("edited %s is not valid YAML: %w", path, err)
				}
			}
		case ".json":
			var doc any
			if err := json.Unmarshal([]byte(content), &doc); err != nil {
				return fmt.Errorf("edited %s is not valid JSON: %w", path, err)
			}
		}
	}
	return nil
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

// maxCandidates caps how many real repo paths are offered to the locate step,
// keeping the prompt bounded on large repos.
const maxCandidates = 200

// candidateStopwords are generic failure-narrative words that shouldn't drive
// path matching.
var candidateStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"failure": true, "test": true, "error": true, "during": true, "when": true,
	"node": true, "pod": true, "cluster": true, "azure": true, "kubernetes": true,
	"fix": true, "ensure": true, "issue": true, "from": true, "into": true, "not": true,
	"are": true, "has": true, "was": true, "due": true, "which": true, "their": true,
}

// preferredDirHints boost config/template/manifest locations a fix usually lives
// in; noisy or generated locations are penalized.
var (
	preferredDirHints = []string{"templates/", "config/", "charts/", "manifests/", "hack/", "scripts/", "test/e2e/", "kustomize"}
	penalizedDirHints = []string{"vendor/", "third_party/", "docs/", "node_modules/", "testdata/", ".github/", "examples/"}
	preferredExts     = map[string]bool{".yaml": true, ".yml": true, ".go": true, ".sh": true, ".tpl": true, ".json": true, ".toml": true, ".cfg": true}
)

// rankCandidates returns the repo paths most relevant to the pattern, scored by
// keyword overlap with the failure text plus directory/extension preferences.
func rankCandidates(tree []string, p models.PatternAnalysis) []string {
	keywords := extractKeywords(p.SharedRootCause + " " + p.SuggestedFix + " " + p.Subject + " " + p.Summary)
	type scored struct {
		path  string
		score int
	}
	var ranked []scored
	for _, path := range tree {
		lp := strings.ToLower(path)
		keywordHits := 0
		for kw := range keywords {
			if strings.Contains(lp, kw) {
				keywordHits++
			}
		}
		dirPreferred := false
		for _, h := range preferredDirHints {
			if strings.Contains(lp, h) {
				dirPreferred = true
				break
			}
		}
		// Require a real signal (keyword or preferred dir); a matching extension
		// alone is not enough.
		if keywordHits == 0 && !dirPreferred {
			continue
		}
		score := 2 * keywordHits
		if dirPreferred {
			score++
		}
		for _, h := range penalizedDirHints {
			if strings.Contains(lp, h) {
				score -= 2
				break
			}
		}
		if preferredExts[ext(lp)] {
			score++
		}
		if score > 0 {
			ranked = append(ranked, scored{path, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].path < ranked[j].path
	})
	out := make([]string, 0, maxCandidates)
	for _, s := range ranked {
		if len(out) >= maxCandidates {
			break
		}
		out = append(out, s.path)
	}
	return out
}

// extractKeywords tokenizes failure text into distinctive lowercase terms.
func extractKeywords(text string) map[string]bool {
	kws := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(tok) < 4 || candidateStopwords[tok] {
			continue
		}
		kws[tok] = true
	}
	return kws
}

func ext(p string) string {
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[i:]
	}
	return ""
}
