package ai

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// L.4 Step 2: regex-based critique that catches the punt-style
// suggested_fix pattern observed in L.4 Step 1 measurements (Qwen
// punt_rate=80% post-Step-1, having dropped only 9pp from L.3 Step 2's
// 89%). The Step 1 prompt rewrites are necessary but not sufficient
// for weaker models: Qwen reads "do not write investigation tasks"
// then writes one anyway. This file adds a second-pass gate that
// rejects punt-shaped finals and re-prompts the agentic loop with a
// targeted feedback message before caching.
//
// Implementation is intentionally deterministic in v1: the regex
// catches the dominant failure mode (imperative diagnostic verbs in
// suggested_fix) without an extra LLM round-trip. A future v2 may
// add an LLM judge for subtler issues (e.g. root_cause names a
// resource that was never read), but ship the cheap fix first.

// _diagVerbs and _diagGerunds enumerate the diagnostic / information-
// gathering vocabulary the L.4 Step 1 prompts already declare forbidden
// in suggested_fix. Kept as raw alternation strings so the same lists
// can be reused across the bare-imperative pattern and the
// "<subject> should <verb>" pattern without duplication.
const _diagVerbs = `check|verify|investigate|ensure|inspect|examine|confirm|audit|review|determine|monitor|troubleshoot|debug|look\s+into|look\s+at|analyze`
const _diagGerunds = `checking|verifying|investigating|ensuring|inspecting|examining|confirming|auditing|reviewing|determining|monitoring|troubleshooting|debugging|looking\s+into|looking\s+at|analyzing`

// puntPattern is one of the four punt shapes recognized by critique.
// Each pattern runs as its own regex because Go's RE2 has no negative
// lookahead; the bare-imperative and should/need shapes need to be
// filtered against a validation-followup exemption ("verify BY
// rerunning" is fine, "verify cloud-init" is a punt). Patterns 3 and
// 4 (recommend-gerund) don't get the exemption.
//
// Doing this as multiple regexes is also clearer than a giant
// alternation with parenthesized capture groups for branch detection.
type puntPattern struct {
	re                     *regexp.Regexp
	exemptValidationFollow bool
}

// validationFollowRE matches the prepositional phrase the L.4 Step 1
// prompts explicitly allow in composite remediations like "apply the
// fix; verify BY tailing the controller log". Applied to the text
// immediately after a candidate match.
var validationFollowRE = regexp.MustCompile(`^\s+(?:by|via|through|using|that)\b`)

// puntPatterns is the Go port of the validated A/B-harness regex from
// build_ab_l4s1.py, split into four single-purpose patterns to work
// around RE2's lack of negative lookahead.
//  1. Bare diagnostic imperative at sentence/bullet start
//     ("Check X", "1. Verify Y", "- Investigate Z").
//  2. "<subject> should/need-to <diagnostic verb>"
//     ("You should check", "operator needs to verify").
//  3. "<subject> recommend(s) <diagnostic gerund>"
//     ("We recommend reviewing").
//  4. Standalone "recommend <gerund>" at sentence/bullet start.
var puntPatterns = []puntPattern{
	{
		re: regexp.MustCompile(
			`(?im)(?:^|[.!?]\s+|;\s+|^\s*\d+[.)]\s*|^\s*[-*]\s*)` +
				`(?:please\s+)?(?:` + _diagVerbs + `)\b`,
		),
		exemptValidationFollow: true,
	},
	{
		re: regexp.MustCompile(
			`(?i)\b(?:user|operator|developer|engineer|team|you|we|they|one)\s+` +
				`(?:should|must|need\s+to|needs\s+to|ought\s+to|may\s+want\s+to|might\s+want\s+to|could)\s+` +
				`(?:also\s+)?(?:` + _diagVerbs + `)\b`,
		),
		exemptValidationFollow: true,
	},
	{
		re: regexp.MustCompile(
			`(?i)\b(?:i|we|they|operator|team)\s+recommends?\s+(?:` + _diagGerunds + `)\b`,
		),
	},
	{
		re: regexp.MustCompile(
			`(?im)(?:^|[.!?]\s+|;\s+|^\s*\d+[.)]\s*|^\s*[-*]\s*)` +
				`recommends?\s+(?:` + _diagGerunds + `)\b`,
		),
	},
}

// findPunts runs every punt pattern against text and returns the set
// of matched substrings after applying the validation-followup
// exemption. Trims leading punctuation/whitespace that the
// sentence-start anchor pulled into the match so the feedback message
// quotes only the meaningful phrase.
func findPunts(text string) []string {
	var out []string
	for _, p := range puntPatterns {
		idxs := p.re.FindAllStringIndex(text, -1)
		for _, idx := range idxs {
			start, end := idx[0], idx[1]
			if p.exemptValidationFollow {
				if validationFollowRE.MatchString(text[end:]) {
					continue
				}
			}
			match := strings.TrimLeft(text[start:end], " \t\n.!?;-*0123456789).")
			match = strings.TrimSpace(match)
			if match != "" {
				out = append(out, match)
			}
		}
	}
	return out
}

// currentCritiqueVersion is the schema version of the critique
// contract. Bumped on material strengthening of the gate so cache
// entries from a weaker version are invalidated on read. v1 = L.4
// Step 2 (punt-only). v2 = L.4 Step 2.5 (adds hallucination check on
// artifact citations and import-path heuristic on relevant_files).
const currentCritiqueVersion = 2

// artifactCitationRE matches strings in the model's prose that look
// like Prow artifact filenames. Intentionally narrow on bare basenames
// (only well-known artifact names) but broader on qualified paths
// (artifact-shaped .log/.txt/.json under any directory). Source-file
// extensions on bare basenames (.yaml, .go, .py, .md, generic .json)
// are still excluded because the model legitimately cites those
// without reading them via tools (they live in the source repo).
//
// Captures one or more path segments so that qualified citations
// ("machine-foo/cloud-init-output.log", "artifacts/.../events.json")
// round-trip through normalizeArtifactCitation for both full-path and
// basename matching.
var artifactCitationRE = regexp.MustCompile(
	// Qualified path (has a directory) ending in any of the artifact
	// extensions. The capturing group ensures we keep the leading
	// directory prefix.
	`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+\.(?:log|txt|json|xml)` +
		// OR a well-known bare artifact basename.
		`|(?:` +
		`[A-Za-z0-9._-]+\.log` +
		`|build-log\.txt` +
		`|clone-log\.txt` +
		`|started\.json` +
		`|finished\.json` +
		`|prowjob\.json` +
		// Match junit_runner.xml, junit.e2e_suite.1.xml, junit-style.xml.
		`|junit[._-][A-Za-z0-9._-]+\.xml` +
		`)`,
)

// hallucinatedImportPathRE catches the specific failure mode where the
// model puts Go-import-style prefixes into relevant_files (which is
// supposed to hold repo-relative source paths). Surfaced by L.4 Step 2
// Case 1: Qwen produced `sigs.k8s.io/cluster-api-provider-azure/controllers/azuremachine/actuators.go`
// for a file that doesn't exist; the GOPATH-shaped prefix is a tell
// that the model is fabricating from intuition rather than citing a
// real file it saw.
var hallucinatedImportPathRE = regexp.MustCompile(
	`^(?i)(?:sigs\.k8s\.io|github\.com|k8s\.io|golang\.org|google\.golang\.org)/`,
)

// citationStripRE removes line-number and column suffixes the model
// often appends to artifact citations ("build-log.txt:1720",
// "manager.log#L42-L50") so the basename matches the form the tool
// arg actually had.
var citationStripRE = regexp.MustCompile(`(?::\d+(?:-\d+)?|#L\d+(?:-L?\d+)?)\b`)

// normalizeArtifactCitation cleans up a path-shaped match for
// comparison against the reads set. Slash semantics (not OS), lowercase,
// trims wrapping punctuation/quotes/backticks, strips line-number
// suffixes. Returns the cleaned full path; callers use path.Base for
// basename-only comparison.
func normalizeArtifactCitation(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`'\"(),;:")
	s = strings.ReplaceAll(s, `\`, `/`)
	s = citationStripRE.ReplaceAllString(s, "")
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	return s
}

// findUnreadArtifactCitations extracts artifact-path-shaped tokens
// from text and returns the ones that don't match any path the agent
// actually fetched via read_artifact / tail_artifact / grep_artifact.
//
// Calling convention: pass nil for BOTH readsFull and readsBase to
// disable the check (returns nil). Pass an initialized map (even if
// empty) to enable the check. The agentic loop's state.readArtifactsFull
// / readArtifactsBase are lazy-initialized on first successful read,
// so nil naturally means "the agent has made zero successful reads",
// in which case the check is skipped to avoid false-positives on
// the escape hatch ("the log was truncated" with no read recorded).
// In production this is fine because MinGCSBytes forces at least some
// reads before critique can run; tests that exercise punt-only
// behavior pass nil.
//
// Match rules (rubber-duck #2):
//   - If the citation includes a directory prefix (contains a "/"),
//     require an exact full-path match against readsFull. This catches
//     the cross-machine basename collision where Qwen reads machine-A's
//     boot.log and cites machine-B's boot.log.
//   - If the citation is basename-only, match against readsBase. The
//     model citing a bare "boot.log" without qualification only proves
//     it knows the basename, which matches any cluster/machine's
//     boot.log the agent did read.
//
// Returns the de-duplicated list of unread citations in input order.
// readsFull and readsBase keys are pre-normalized basenames / full paths
// (lowercase, slash semantics).
func findUnreadArtifactCitations(text string, readsFull, readsBase map[string]bool) []string {
	if readsFull == nil && readsBase == nil {
		return nil
	}
	if text == "" {
		return nil
	}
	matches := artifactCitationRE.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range matches {
		norm := normalizeArtifactCitation(raw)
		if norm == "" {
			continue
		}
		key := norm
		if seen[key] {
			continue
		}
		seen[key] = true

		base := path.Base(norm)
		hasDir := strings.Contains(norm, "/")
		if hasDir {
			if readsFull[norm] {
				continue
			}
		}
		if !hasDir {
			if readsBase[base] {
				continue
			}
		}
		out = append(out, norm)
	}
	return out
}

// findHallucinatedImportPaths flags Go-import-style prefixes
// (sigs.k8s.io/foo, github.com/bar). These are GOPATH paths, not
// repo-relative source paths; in observed cases they accompany
// fabricated filenames (Case 1 of the L.4 Step 2 A/B). Scans an
// arbitrary set of strings (relevant_files entries plus root_cause /
// suggested_fix prose) so that a hallucinated GOPATH-shaped citation
// is caught wherever the model puts it.
func findHallucinatedImportPaths(candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		// For relevant_files entries the candidate is the whole string;
		// for prose we tokenize on whitespace and common punctuation so
		// "see sigs.k8s.io/foo/bar.go for X" matches the prefix.
		for _, tok := range tokenizeForImportPath(c) {
			s := strings.Trim(tok, "`'\"(),;:")
			if s == "" {
				continue
			}
			if !hallucinatedImportPathRE.MatchString(s) {
				continue
			}
			key := strings.ToLower(s)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, s)
		}
	}
	return out
}

// tokenizeForImportPath splits a string on whitespace so the import-path
// regex can be applied to each token. relevant_files entries are
// whole strings (one token); prose fields contain many tokens.
func tokenizeForImportPath(s string) []string {
	return strings.Fields(s)
}

// critiqueOutcome is returned by critiqueDraft. Passed=true means the
// draft is accepted as-is; Passed=false means the agent should re-loop
// with Feedback appended as a user-role message.
type critiqueOutcome struct {
	Passed   bool
	Feedback string

	// PuntMatches lists exact substrings that triggered the
	// suggested_fix punt regex. Quoted back in Feedback so the model
	// sees its own offending text. Empty when no punt was detected.
	PuntMatches []string

	// UnreadCitations lists artifact-path tokens the model cited
	// without ever fetching via a read/tail/grep tool. Quoted back in
	// Feedback so the model knows which files to actually read on
	// retry. Empty when no hallucination was detected. (L.4 Step 2.5)
	UnreadCitations []string

	// FabricatedImports lists relevant_files entries that look like
	// Go import paths rather than repo-relative paths. Surfaced in
	// Feedback. Empty when none detected. (L.4 Step 2.5)
	FabricatedImports []string
}

// Matches is the back-compat union of all match categories, for the
// agentic loop's log line and for legacy callers that just want a
// flat list of "things that tripped the gate".
func (o critiqueOutcome) Matches() []string {
	n := len(o.PuntMatches) + len(o.UnreadCitations) + len(o.FabricatedImports)
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	out = append(out, o.PuntMatches...)
	out = append(out, o.UnreadCitations...)
	out = append(out, o.FabricatedImports...)
	return out
}

// critiqueDraft inspects a parsed final analysis against the L.4
// critique contract (Step 2 punt regex + Step 2.5 hallucination /
// import-path checks). Returns Passed=true only when every check
// passes; on any failure, Feedback combines all triggered checks into
// one user-role message so the model fixes everything in a single
// retry rather than playing whack-a-mole across rounds.
//
// readsFull / readsBase are the agent's actually-fetched artifact
// paths (full and basename); pass empty maps to skip the hallucination
// check (e.g. for early-loop critique against a state with no tool
// calls yet). When BOTH maps are empty AND the text cites artifacts,
// the check still fires: zero reads with specific citations is
// definitionally a hallucination.
func critiqueDraft(parsed analysisResponse, readsFull, readsBase map[string]bool) critiqueOutcome {
	puntMatches := findPunts(parsed.SuggestedFix)

	// Scan all prose fields plus each relevant_files entry: the model
	// may cite an unread artifact in any of them.
	var unread []string
	scanned := map[string]bool{}
	scan := func(s string) {
		for _, u := range findUnreadArtifactCitations(s, readsFull, readsBase) {
			if scanned[u] {
				continue
			}
			scanned[u] = true
			unread = append(unread, u)
		}
	}
	scan(parsed.RootCause)
	scan(parsed.Summary)
	scan(parsed.SuggestedFix)
	for _, rf := range parsed.RelevantFiles {
		scan(rf)
	}

	// Rubber-duck #6: scan prose fields for fabricated import paths
	// too, not just relevant_files. The Step 2 Case 1 hallucination
	// embedded sigs.k8s.io/.../actuators.go in root_cause.
	importCandidates := append([]string{parsed.RootCause, parsed.Summary, parsed.SuggestedFix}, parsed.RelevantFiles...)
	fabricated := findHallucinatedImportPaths(importCandidates)

	out := critiqueOutcome{
		PuntMatches:       puntMatches,
		UnreadCitations:   unread,
		FabricatedImports: fabricated,
	}
	if len(puntMatches) == 0 && len(unread) == 0 && len(fabricated) == 0 {
		out.Passed = true
		return out
	}
	out.Feedback = formatCritiqueFeedback(parsed, out)
	return out
}

// formatCritiqueFeedback builds the user-role message appended to the
// agentic conversation when a draft fails critique. Combines feedback
// for whichever of the three checks failed (punt, hallucinated artifact
// citations, fabricated import paths) into a single message so the
// model can address everything in one retry.
//
// suggested_fix is truncated to feedbackQuoteLimit characters (with an
// ellipsis) so a pathologically long fix doesn't balloon the
// conversation history on every retry. Matched phrases / unread
// citations are listed separately and are not truncated.
func formatCritiqueFeedback(parsed analysisResponse, out critiqueOutcome) string {
	var sections []string

	if len(out.PuntMatches) > 0 {
		sections = append(sections, formatPuntSection(parsed, out.PuntMatches))
	}
	if len(out.UnreadCitations) > 0 {
		sections = append(sections, formatUnreadSection(out.UnreadCitations))
	}
	if len(out.FabricatedImports) > 0 {
		sections = append(sections, formatFabricatedImportSection(out.FabricatedImports))
	}

	sections = append(sections, `Re-emit your JSON addressing every issue above. Do NOT re-emit the same draft. If you re-emit the same issues, your answer will be rejected again.`)

	return strings.Join(sections, "\n\n")
}

// formatPuntSection is the L.4 Step 2 punt-detection feedback,
// extracted so the combined formatter can include it alongside the
// Step 2.5 sections.
func formatPuntSection(parsed analysisResponse, matches []string) string {
	seen := map[string]bool{}
	uniq := make([]string, 0, len(matches))
	for _, m := range matches {
		key := strings.ToLower(strings.TrimSpace(m))
		if seen[key] {
			continue
		}
		seen[key] = true
		uniq = append(uniq, strings.TrimSpace(m))
	}
	var quoted []string
	for _, m := range uniq {
		quoted = append(quoted, fmt.Sprintf("%q", m))
	}

	fix := strings.TrimSpace(parsed.SuggestedFix)
	if len(fix) > feedbackQuoteLimit {
		fix = fix[:feedbackQuoteLimit] + "… [truncated]"
	}

	return fmt.Sprintf(`Your draft suggested_fix is being rejected because it contains diagnostic / information-gathering language that the system prompt forbids:

  %s

(matched: %s)

This is a TODO list for the user, not a remediation. The investigation work belongs to YOU, not the user. Before re-emitting your JSON:

1. For each named resource you mentioned in root_cause (Machine, Pod, controller, namespace, VM, container), use your tools NOW to read that resource's own artifacts. Examples: AzureMachine/<name>.yaml status conditions, the corresponding cloud-init/kubelet/journal log, the controller manager log grepped for <name>. Pick the 1-3 most directly tied to the failure; do not chase incidental mentions.
2. Re-emit your JSON with EITHER:
   (a) a CONCRETE remediation: the specific code change, config edit, command to run, retry, redeploy, rollback, or operational fix that addresses the root_cause, OR
   (b) the strict escape hatch starting with "No remediation possible from available evidence:" and including all THREE required parts: (1) the strongest fact you established, (2) the specific artifacts/logs you consulted, (3) the exact missing evidence that prevents a remediation.

A composite "apply the fix, then verify by Y" is allowed; "check X, verify Y, investigate Z" alone is not.`,
		fix,
		strings.Join(quoted, ", "))
}

// formatUnreadSection is the L.4 Step 2.5 hallucination feedback. The
// model named an artifact in its prose but the tool log shows no
// read_artifact / tail_artifact / grep_artifact call against it. Either
// the model invented the citation or it inferred from a directory
// listing; either way, force it to actually fetch the bytes before
// claiming what they contain. Encourages batching reads in one
// assistant turn so the existing critiqueRetryIters=3 budget suffices.
func formatUnreadSection(unread []string) string {
	var quoted []string
	for _, u := range unread {
		quoted = append(quoted, fmt.Sprintf("  - %s", u))
	}
	return fmt.Sprintf(`Your draft cites the following artifact(s) but the tool log shows no read_artifact / tail_artifact / grep_artifact call against them:

%s

Either you fabricated these citations or you inferred from a directory listing; both are unacceptable. Do NOT infer file contents from filenames or list output. Before re-emitting:

1. In ONE assistant turn, batch read_artifact / tail_artifact / grep_artifact calls for every cited artifact you have not yet fetched. If a file is large, prefer tail_artifact or grep_artifact with wide context over read_artifact.
2. If a file does not exist, the tool will return an error; in that case remove the citation from your draft and re-emit using only evidence the tools actually returned.
3. Claim only facts supported by the bytes the tool actually returned. Do not paraphrase a grep_artifact match into a claim about the rest of the file you did not see.`,
		strings.Join(quoted, "\n"))
}

// formatFabricatedImportSection is the L.4 Step 2.5 import-path
// heuristic feedback. relevant_files is supposed to hold repo-relative
// source paths, but in observed cases (L.4 Step 2 A/B Case 1) the
// model emits GOPATH-shaped prefixes (sigs.k8s.io/foo/bar.go) for
// files that don't exist. Reject those and ask the model to omit
// rather than fabricate.
func formatFabricatedImportSection(fabricated []string) string {
	var quoted []string
	for _, f := range fabricated {
		quoted = append(quoted, fmt.Sprintf("  - %s", f))
	}
	return fmt.Sprintf(`Your draft relevant_files contains paths that use Go-import-style prefixes (sigs.k8s.io/, github.com/, k8s.io/, golang.org/):

%s

relevant_files must contain REPO-RELATIVE source paths (e.g. "controllers/azuremachine_controller.go", "config/webhook/manifests.yaml"), not Go import paths. In observed failure cases the import-path-shaped entries point at files that do not exist. Re-emit with either the correct repo-relative path, or omit the entry entirely if you cannot identify the file precisely.`,
		strings.Join(quoted, "\n"))
}

// feedbackQuoteLimit caps how much of the model's draft suggested_fix
// is quoted into a critique-retry feedback message. Long enough to be
// useful as a "your own words" reminder, short enough to keep the
// per-retry token cost bounded.
const feedbackQuoteLimit = 600
