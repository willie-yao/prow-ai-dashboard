// Package skillsuggest drafts diagnostic skill recipes for systemic recurring
// patterns and opens draft PRs adding them to the dashboard repo's skills/
// directory. It is opt-in through ai.suggest_skills and idempotent through
// hidden markers and existing-skill checks.
package skillsuggest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Completer is the subset of the AI client this package needs.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// existingSkill is the minimal view of a loaded recipe used for the
// already-covered check and id-collision avoidance.
type existingSkill struct {
	ID          string
	Description string
	Triggers    []string
}

// coveredCheck asks the model whether the pattern is already handled by one of
// the existing skills. triggerMatched lists ids whose regex triggers fired on
// the pattern text. Returns true to skip suggesting.
func coveredCheck(ctx context.Context, c Completer, existing []existingSkill, triggerMatched []string, p models.PatternAnalysis) (bool, string, error) {
	if len(existing) == 0 {
		return false, "", nil
	}
	var sb strings.Builder
	for _, s := range existing {
		fmt.Fprintf(&sb, "- id: %s\n  description: %s\n  triggers: %s\n", s.ID, oneLine(s.Description), strings.Join(s.Triggers, " | "))
	}
	matched := "none"
	if len(triggerMatched) > 0 {
		matched = strings.Join(triggerMatched, ", ")
	}
	user := fmt.Sprintf(`Existing skill recipes:
%s
A recurring failure pattern was found:
- subject: %s
- shared_root_cause: %s
- summary: %s

Skills whose triggers already regex-match this pattern's text: %s

Does any existing skill already cover this failure class (i.e. a new recipe
would be redundant)? Answer with a single line of JSON:
{"covered": true|false, "skill_id": "<id or empty>", "reason": "<short>"}`,
		sb.String(), p.Subject, oneLine(p.SharedRootCause), oneLine(p.Summary), matched)

	out, err := c.Complete(ctx, coveredSystemPrompt, user)
	if err != nil {
		return false, "", err
	}
	var v struct {
		Covered bool   `json:"covered"`
		SkillID string `json:"skill_id"`
		Reason  string `json:"reason"`
	}
	if err := parseJSONObject(out, &v); err != nil {
		return false, "", fmt.Errorf("covered-check response: %w", err)
	}
	return v.Covered, strings.TrimSpace(v.SkillID + " " + v.Reason), nil
}

const coveredSystemPrompt = `You decide whether a newly observed CI failure pattern is already covered by an existing diagnostic "skill" recipe. A recipe covers a pattern when it would fire on the same failure class (same root cause family), even if the wording differs. Answer only with the requested one-line JSON, no prose.`

// generateRecipe drafts a skill recipe YAML for the pattern and validates it
// against the skills schema. Returns the recipe id after collision avoidance
// and the YAML file content. An invalid draft is an error so the caller skips
// it rather than proposing a broken recipe.
func generateRecipe(ctx context.Context, c Completer, p models.PatternAnalysis, existingIDs []string) (string, string, error) {
	user := fmt.Sprintf(`Draft a skill recipe for this recurring failure pattern.

subject: %s
shared_root_cause: %s
suggested_fix: %s
summary: %s
builds_analyzed: %d

Existing recipe ids (pick a NEW, distinct kebab-case id): %s

Output ONLY the YAML recipe, no code fence, no commentary.`,
		p.Subject, oneLine(p.SharedRootCause), oneLine(p.SuggestedFix), oneLine(p.Summary), p.BuildsAnalyzed, strings.Join(existingIDs, ", "))

	out, err := c.Complete(ctx, recipeSystemPrompt, user)
	if err != nil {
		return "", "", err
	}
	yamlContent := extractYAML(out)
	sk, err := skills.ParseAndValidate([]byte(yamlContent))
	if err != nil {
		return "", "", fmt.Errorf("generated recipe failed validation: %w", err)
	}
	// Sanitize the model-chosen id for skills/<id>.yaml and avoid collisions.
	safe := sanitizeID(sk.ID)
	if safe == "" {
		return "", "", fmt.Errorf("generated recipe id %q is not a usable identifier", sk.ID)
	}
	id := uniqueID(safe, existingIDs)
	if id != sk.ID {
		// Re-stamp the id line so the file content matches the chosen id.
		yamlContent = replaceIDLine(yamlContent, id)
	}
	return id, yamlContent, nil
}

// recipeSystemPrompt teaches the schema. Triggers match draft analysis text;
// required_evidence any_of patterns match artifact paths the agent reads.
const recipeSystemPrompt = `You write diagnostic "skill" recipes for a Prow CI failure-analysis engine. A recipe is YAML with this schema:

id: kebab-case-identifier            # required, unique
name: Human readable name            # optional
description: one line for maintainers # optional, not shown to the model
priority: 150                        # optional integer, higher wins on ties
triggers:                            # required: regexes ORed; match the model's draft root-cause TEXT
  - "(?i)some error string"
required_evidence:                   # optional: artifact-PATH regex groups the agent must have read
  - id: group-id
    description: what this evidence is
    any_of:
      - "artifacts/.*/SomeResource/.*\\.ya?ml"
procedure: |                         # optional: numbered steps quoted back to the model
  1. Read X. 2. Check Y. 3. Cite Z verbatim.

Rules:
- triggers match ANALYSIS TEXT (error strings, root-cause phrases), NOT file paths.
- required_evidence any_of match ARTIFACT PATHS (e.g. artifacts/clusters/.../*.yaml, build-log.txt).
- Every regex must be valid Go regexp syntax.
- Keep it specific to this failure class; prefer 2-4 triggers and 1-2 evidence groups.
- The procedure must direct the agent to read the real logs and cite them; never let it rule a failure transient without evidence.
Output only valid YAML.`

var (
	fenceRe    = regexp.MustCompile("(?s)```(?:ya?ml)?\\s*(.*?)```")
	idRe       = regexp.MustCompile(`(?m)^(\s*id:\s*).*$`)
	unsafeIDRe = regexp.MustCompile(`[^a-z0-9]+`)
)

// sanitizeID coerces a model-chosen id into a safe kebab-case identifier so it
// can't produce a nested or escaping skills/<id>.yaml path.
func sanitizeID(id string) string {
	id = unsafeIDRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(id)), "-")
	return strings.Trim(id, "-")
}

// extractYAML strips a surrounding markdown code fence if the model added one.
func extractYAML(s string) string {
	s = strings.TrimSpace(s)
	if m := fenceRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return s
}

// replaceIDLine rewrites the first `id:` line to the chosen id.
func replaceIDLine(yamlContent, newID string) string {
	replaced := false
	return idRe.ReplaceAllStringFunc(yamlContent, func(m string) string {
		if replaced {
			return m
		}
		replaced = true
		sub := idRe.FindStringSubmatch(m)
		return sub[1] + newID
	})
}

// uniqueID returns id unchanged if it doesn't collide with an existing recipe,
// else appends a short disambiguating suffix.
func uniqueID(id string, existing []string) string {
	set := map[string]bool{}
	for _, e := range existing {
		set[e] = true
	}
	if !set[id] {
		return id
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", id, i)
		if !set[cand] {
			return cand
		}
	}
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

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
