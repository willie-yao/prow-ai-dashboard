package ai

import (
	"fmt"
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

// critiqueOutcome is returned by critiqueDraft. Passed=true means the
// draft is accepted as-is; Passed=false means the agent should re-loop
// with Feedback appended as a user-role message.
type critiqueOutcome struct {
	Passed   bool
	Feedback string

	// Matches lists the exact substrings that triggered the punt
	// regex, surfaced in Feedback so the model sees its own offending
	// text quoted back. Empty when Passed=true.
	Matches []string
}

// critiqueDraft inspects a parsed final analysis against the L.4 Step 2
// rules. Currently checks only suggested_fix for the punt pattern; root
// cause and named-resource-not-read checks are planned for v2.
func critiqueDraft(parsed analysisResponse) critiqueOutcome {
	matches := findPunts(parsed.SuggestedFix)
	if len(matches) == 0 {
		return critiqueOutcome{Passed: true}
	}
	return critiqueOutcome{
		Passed:   false,
		Feedback: formatCritiqueFeedback(parsed, matches),
		Matches:  matches,
	}
}

// formatCritiqueFeedback builds the user-role message appended to the
// agentic conversation when a draft fails critique. Quotes the
// offending suggested_fix text, lists the matched phrases so the model
// can see exactly what tripped the gate, and re-states the two
// allowed shapes (concrete remediation OR strict escape hatch) so the
// retry has a clear target.
//
// suggested_fix is truncated to feedbackQuoteLimit characters (with an
// ellipsis) so a pathologically long fix doesn't balloon the
// conversation history on every retry. Matched phrases are listed
// separately and are not truncated.
func formatCritiqueFeedback(parsed analysisResponse, matches []string) string {
	// Trim duplicates while preserving order so a long suggested_fix
	// that triggers the regex five times on the same phrase shows the
	// phrase once.
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

Do NOT re-emit the same draft. A composite "apply the fix, then verify by Y" is allowed; "check X, verify Y, investigate Z" alone is not. If you re-emit a TODO list, your answer will be rejected again.`,
		fix,
		strings.Join(quoted, ", "))
}

// feedbackQuoteLimit caps how much of the model's draft suggested_fix
// is quoted into a critique-retry feedback message. Long enough to be
// useful as a "your own words" reminder, short enough to keep the
// per-retry token cost bounded.
const feedbackQuoteLimit = 600
